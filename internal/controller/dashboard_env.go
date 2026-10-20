/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"maps"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The dashboard's environment from Secrets and ConfigMaps (spec.components.dashboard.envFrom and
// .env, operator#311).
//
// Precedence, lowest to highest: the operator's envFrom (<cr>-config, <cr>-dashboard-secret,
// <cr>-jwt), then dashboard.envFrom in list order, then the operator's explicit env, then
// dashboard.env. The variables in dashboardOwnedEnv are never taken from the user: naming one in env,
// or carrying one in an envFrom source, fails the reconcile instead of being dropped or winning.
// Errors name fields, objects and variables, never a value.

// dashboardEnvHashAnnotation is the pod-template annotation holding a hash of every referenced
// Secret/ConfigMap value, so a change to one rolls the dashboard. It is omitted when the dashboard
// references no object.
const dashboardEnvHashAnnotation = "orama.cloud/dashboard-env-hash"

// dashboardOwnedEnv maps each variable the operator owns on the dashboard to what controls it.
var dashboardOwnedEnv = map[string]string{
	"DATABASE_URL":      "the control DB (spec.components.controlDB)",
	"POSTGRES_HOST":     "the control DB (spec.components.controlDB)",
	"POSTGRES_PORT":     "the control DB (spec.components.controlDB)",
	"POSTGRES_DB":       "the control DB (spec.components.controlDB)",
	"POSTGRES_PASSWORD": "the control DB (spec.components.controlDB)",
	"JWT_PRIVATE_KEY":   "the operator-generated <cr>-jwt Secret",
	"JWT_PUBLIC_KEY":    "the operator-generated <cr>-jwt Secret",
	"JWT_ISSUER":        "the operator's JWT issuer",
	"MASTER_API_KEY":    "spec.masterAPIKeySecretRef",
}

// refusedEnvError is the refusal for an operator-owned variable supplied through field.
func refusedEnvError(field, name string) error {
	return fmt.Errorf("%s sets %s, which the operator owns (it comes from %s); remove it", field, name, dashboardOwnedEnv[name])
}

// validateDashboardEnv checks dashboard.env and the shape of dashboard.envFrom without reading the
// cluster. Every entry is checked, including one a later entry shadows.
func validateDashboardEnv(oc *oramav1alpha1.OramaCloudV1) error {
	d := oc.Spec.Components.Dashboard
	for i, e := range d.Env {
		if d.Payments != nil && slices.Contains(dashboardPaymentsEnvNames, e.Name) {
			continue
		}
		if _, owned := dashboardOwnedEnv[e.Name]; owned {
			return refusedEnvError(fmt.Sprintf("dashboard.env[%d]", i), e.Name)
		}
		if kind, name, key, _, ok := envKeyRef(e); ok && (name == "" || key == "") {
			return fmt.Errorf("dashboard.env[%d] (%s): the %s key reference needs a name and a key", i, e.Name, kind)
		}
	}
	for i, src := range d.EnvFrom {
		switch {
		case (src.SecretRef == nil) == (src.ConfigMapRef == nil):
			return fmt.Errorf("dashboard.envFrom[%d] must set exactly one of secretRef and configMapRef", i)
		case src.SecretRef != nil && src.SecretRef.Name == "", src.ConfigMapRef != nil && src.ConfigMapRef.Name == "":
			return fmt.Errorf("dashboard.envFrom[%d] needs a name", i)
		}
	}
	return nil
}

// dashboardEnvSource is one dashboard.envFrom entry with the data it read.
type dashboardEnvSource struct {
	Kind, Name, Prefix string
	Data               map[string]string // raw bytes as Go strings; nil for an absent optional object
}

// envFromRef unpacks a validated envFrom entry into its kind, name and optional flag.
func envFromRef(src corev1.EnvFromSource) (kind, name string, optional bool) {
	if src.SecretRef != nil {
		return "Secret", src.SecretRef.Name, ptrTrue(src.SecretRef.Optional)
	}
	return "ConfigMap", src.ConfigMapRef.Name, ptrTrue(src.ConfigMapRef.Optional)
}

// envKeyRef unpacks an env entry's Secret or ConfigMap key selector; ok=false for any other entry
// (a literal, fieldRef or resourceFieldRef names no object).
func envKeyRef(e corev1.EnvVar) (kind, name, key string, optional, ok bool) {
	switch {
	case e.ValueFrom == nil:
	case e.ValueFrom.SecretKeyRef != nil:
		ref := e.ValueFrom.SecretKeyRef
		return "Secret", ref.Name, ref.Key, ptrTrue(ref.Optional), true
	case e.ValueFrom.ConfigMapKeyRef != nil:
		ref := e.ValueFrom.ConfigMapKeyRef
		return "ConfigMap", ref.Name, ref.Key, ptrTrue(ref.Optional), true
	}
	return "", "", "", false, false
}

func ptrTrue(b *bool) bool { return b != nil && *b }

// readEnvObject returns the data Kubernetes exposes to environment variables: a Secret's data, or a
// ConfigMap's data (binaryData never reaches the environment). found=false when it does not exist;
// any other error is returned as is, so optional never hides it.
func (r *OramaCloudV1Reconciler) readEnvObject(ctx context.Context, namespace, kind, name string) (map[string]string, bool, error) {
	key := client.ObjectKey{Namespace: namespace, Name: name}
	data := map[string]string{}
	var err error
	if kind == "Secret" {
		s := &corev1.Secret{}
		if err = r.Get(ctx, key, s); err == nil {
			for k, v := range s.Data {
				data[k] = string(v)
			}
		}
	} else {
		cm := &corev1.ConfigMap{}
		if err = r.Get(ctx, key, cm); err == nil {
			maps.Copy(data, cm.Data)
		}
	}
	switch {
	case apierrors.IsNotFound(err):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("read %s %s/%s: %w", kind, namespace, name, err)
	}
	return data, true, nil
}

// dashboardEnvSources reads every dashboard.envFrom source in order. A missing source that is not
// optional, or a source carrying an operator-owned variable after its prefix, is an error naming it.
func (r *OramaCloudV1Reconciler) dashboardEnvSources(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) ([]dashboardEnvSource, error) {
	var out []dashboardEnvSource
	for i, src := range oc.Spec.Components.Dashboard.EnvFrom {
		kind, name, optional := envFromRef(src)
		data, found, err := r.readEnvObject(ctx, oc.Namespace, kind, name)
		if err != nil {
			return nil, fmt.Errorf("dashboard.envFrom[%d]: %w", i, err)
		}
		if !found && !optional {
			return nil, fmt.Errorf("dashboard.envFrom[%d]: %s %s/%s does not exist", i, kind, oc.Namespace, name)
		}
		for _, k := range slices.Sorted(maps.Keys(data)) {
			if _, owned := dashboardOwnedEnv[src.Prefix+k]; owned {
				return nil, refusedEnvError(fmt.Sprintf("dashboard.envFrom[%d] (%s %s)", i, kind, name), src.Prefix+k)
			}
		}
		out = append(out, dashboardEnvSource{Kind: kind, Name: name, Prefix: src.Prefix, Data: data})
	}
	return out, nil
}

// dashboardEnvHash is the hex SHA-256 of the envFrom sources' data and of the value behind every key
// selector in the dashboard env that reaches the container, or "" when neither references an object.
// A non-optional selector whose object or key is missing fails. Every field goes into the hash as
// its raw bytes behind a length prefix (hashFields), with each source's keys sorted, so a Secret
// value that is not valid UTF-8 hashes as itself. The input records kind, name, order, prefix and
// selected key, and whether an optional object or key is present, so an absent optional key and a
// present empty value hash differently.
func (r *OramaCloudV1Reconciler) dashboardEnvHash(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, sources []dashboardEnvSource) (string, error) {
	h := sha256.New()
	keys := 0
	paymentsEnv, err := r.dashboardPaymentsEnv(ctx, oc)
	if err != nil {
		return "", err
	}
	for _, e := range overrideEnv(paymentsEnv, dashboardUserEnv(oc)) {
		kind, name, key, optional, ok := envKeyRef(e)
		if !ok {
			continue
		}
		data, found, err := r.readEnvObject(ctx, oc.Namespace, kind, name)
		if err != nil {
			return "", fmt.Errorf("dashboard.env %s: %w", e.Name, err)
		}
		value, has := data[key]
		if !has && !optional {
			if !found {
				return "", fmt.Errorf("dashboard.env %s: %s %s/%s does not exist", e.Name, kind, oc.Namespace, name)
			}
			return "", fmt.Errorf("dashboard.env %s: %s %s/%s has no key %s", e.Name, kind, oc.Namespace, name, key)
		}
		hashFields(h, "key", kind, name, key, presence(has), value)
		keys++
	}
	if len(sources) == 0 && keys == 0 {
		return "", nil
	}
	for _, s := range sources {
		hashFields(h, "envFrom", s.Kind, s.Name, s.Prefix, presence(s.Data != nil), strconv.Itoa(len(s.Data)))
		for _, k := range slices.Sorted(maps.Keys(s.Data)) {
			hashFields(h, k, s.Data[k])
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashFields writes each field to h as an 8-byte big-endian length and its raw bytes, so no two
// different field sequences write the same bytes.
func hashFields(h hash.Hash, fields ...string) {
	var n [8]byte
	for _, f := range fields {
		binary.BigEndian.PutUint64(n[:], uint64(len(f)))
		h.Write(n[:])
		h.Write([]byte(f))
	}
}

// presence is "1" for a present object or key and "0" for an absent optional one.
func presence(present bool) string {
	if present {
		return "1"
	}
	return "0"
}

// overrideEnv returns base without the names extra sets, followed by extra with only the last entry
// of each name (in their relative order), so each name appears once and extra wins.
func overrideEnv(base, extra []corev1.EnvVar) []corev1.EnvVar {
	last := make(map[string]int, len(extra))
	for i, e := range extra {
		last[e.Name] = i
	}
	out := make([]corev1.EnvVar, 0, len(base)+len(last))
	for _, e := range base {
		if _, replaced := last[e.Name]; !replaced {
			out = append(out, e)
		}
	}
	for i, e := range extra {
		if last[e.Name] == i {
			out = append(out, e)
		}
	}
	return out
}

// dashboardReferences reports whether oc's enabled dashboard takes its environment from the kind/name
// object, through envFrom or a key selector that reaches the container.
func dashboardReferences(oc *oramav1alpha1.OramaCloudV1, kind, name string) bool {
	d := oc.Spec.Components.Dashboard
	if !d.Enabled {
		return false
	}
	if kind == "Secret" && d.Payments != nil && (name == d.Payments.SecretRef.Name || name == oc.Name+"-payments-webhook") {
		return true
	}
	for _, src := range d.EnvFrom {
		if (src.SecretRef == nil) == (src.ConfigMapRef == nil) {
			continue
		}
		if k, n, _ := envFromRef(src); k == kind && n == name {
			return true
		}
	}
	for _, e := range overrideEnv(nil, dashboardUserEnv(oc)) {
		if k, n, _, _, ok := envKeyRef(e); ok && k == kind && n == name {
			return true
		}
	}
	return false
}

// requestsForDashboardEnv maps a Secret or ConfigMap to every OramaCloudV1 in its namespace whose
// dashboard reads it, so a change (or its creation or deletion) re-hashes and rolls the dashboard.
// These objects usually have no owner reference, so Owns() does not see them.
func (r *OramaCloudV1Reconciler) requestsForDashboardEnv(ctx context.Context, obj client.Object) []reconcile.Request {
	var kind string
	switch obj.(type) {
	case *corev1.Secret:
		kind = "Secret"
	case *corev1.ConfigMap:
		kind = "ConfigMap"
	default:
		return nil
	}
	var list oramav1alpha1.OramaCloudV1List
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "list OramaCloudV1 for a dashboard environment object", "kind", kind, "name", obj.GetName())
		return nil
	}
	var reqs []reconcile.Request
	for i := range list.Items {
		if dashboardReferences(&list.Items[i], kind, obj.GetName()) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
	}
	return reqs
}

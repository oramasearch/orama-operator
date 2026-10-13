/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The user-supplied dashboard environment (spec.components.dashboard.envFrom and .env, operator#311).
//
// Precedence, lowest to highest: the operator's envFrom (shared ConfigMap, <cr>-dashboard-secret,
// <cr>-jwt), then dashboard.envFrom in list order, then the operator's explicit env, then
// dashboard.env (which replaces an operator variable of the same name). The variables in
// dashboardOwnedEnv are never taken from the user: naming one in env, or carrying one in an envFrom
// source, fails the reconcile instead of being silently dropped or silently winning.

// dashboardEnvHashAnnotation carries a hash of every referenced Secret/ConfigMap value, so a change to
// one rolls the dashboard. The values themselves are never written anywhere.
const dashboardEnvHashAnnotation = "orama.cloud/dashboard-env-hash"

// dashboardOwnedEnv maps each variable the operator sets for correctness to what controls it.
var dashboardOwnedEnv = map[string]string{
	"DATABASE_URL":      "the control DB (spec.components.controlDB)",
	"POSTGRES_PASSWORD": "the control DB (spec.components.controlDB)",
	"JWT_PRIVATE_KEY":   "the operator-generated <cr>-jwt Secret",
	"JWT_PUBLIC_KEY":    "the operator-generated <cr>-jwt Secret",
	"JWT_ISSUER":        "spec.jwtIssuer",
	"MASTER_API_KEY":    "spec.masterAPIKeySecretRef",
}

// ownedEnvError is the refusal for an operator-owned variable supplied through field.
func ownedEnvError(field, name string) error {
	return fmt.Errorf("%s sets %s, which the operator owns (set it through %s); remove it", field, name, dashboardOwnedEnv[name])
}

// validateDashboardEnv checks dashboard.env and the shape of dashboard.envFrom without reading the cluster.
func validateDashboardEnv(oc *oramav1alpha1.OramaCloudV1) error {
	d := oc.Spec.Components.Dashboard
	for _, e := range d.Env {
		if _, owned := dashboardOwnedEnv[e.Name]; owned {
			return ownedEnvError("dashboard.env", e.Name)
		}
	}
	for i, src := range d.EnvFrom {
		switch {
		case (src.SecretRef == nil) == (src.ConfigMapRef == nil):
			return fmt.Errorf("dashboard.envFrom[%d] must set exactly one of secretRef or configMapRef", i)
		case src.SecretRef != nil && src.SecretRef.Name == "", src.ConfigMapRef != nil && src.ConfigMapRef.Name == "":
			return fmt.Errorf("dashboard.envFrom[%d] needs a name", i)
		}
	}
	return nil
}

// dashboardEnvSource is one dashboard.envFrom entry with the data it read (nil for an absent optional source).
type dashboardEnvSource struct {
	Kind, Name, Prefix string
	Data               map[string]string
}

// envFromRef unpacks an envFrom entry into its kind, name and optional flag.
func envFromRef(src corev1.EnvFromSource) (kind, name string, optional bool) {
	if src.SecretRef != nil {
		return "Secret", src.SecretRef.Name, src.SecretRef.Optional != nil && *src.SecretRef.Optional
	}
	return "ConfigMap", src.ConfigMapRef.Name, src.ConfigMapRef.Optional != nil && *src.ConfigMapRef.Optional
}

// readEnvObject returns a Secret's or ConfigMap's data as strings, or found=false when it does not exist.
func (r *OramaCloudV1Reconciler) readEnvObject(ctx context.Context, namespace, kind, name string) (map[string]string, bool, error) {
	key := client.ObjectKey{Namespace: namespace, Name: name}
	data := map[string]string{}
	var err error
	if kind == "Secret" {
		s := &corev1.Secret{}
		if err = r.Get(ctx, key, s); err == nil {
			maps.Copy(data, s.StringData)
			for k, v := range s.Data {
				data[k] = string(v)
			}
		}
	} else {
		cm := &corev1.ConfigMap{}
		if err = r.Get(ctx, key, cm); err == nil {
			maps.Copy(data, cm.Data)
			for k, v := range cm.BinaryData {
				data[k] = string(v)
			}
		}
	}
	switch {
	case apierrors.IsNotFound(err):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	return data, true, nil
}

// dashboardEnvSources reads every dashboard.envFrom source. A missing source that is not optional, or
// a source carrying an operator-owned variable (after its prefix), is an error naming it.
func (r *OramaCloudV1Reconciler) dashboardEnvSources(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) ([]dashboardEnvSource, error) {
	var out []dashboardEnvSource
	for _, src := range oc.Spec.Components.Dashboard.EnvFrom {
		kind, name, optional := envFromRef(src)
		data, found, err := r.readEnvObject(ctx, oc.Namespace, kind, name)
		if err != nil {
			return nil, err
		}
		if !found && !optional {
			return nil, fmt.Errorf("dashboard.envFrom: %s %q not found in namespace %s", kind, name, oc.Namespace)
		}
		for _, k := range slices.Sorted(maps.Keys(data)) {
			if _, owned := dashboardOwnedEnv[src.Prefix+k]; owned {
				return nil, ownedEnvError(fmt.Sprintf("dashboard.envFrom %s %q", kind, name), src.Prefix+k)
			}
		}
		out = append(out, dashboardEnvSource{Kind: kind, Name: name, Prefix: src.Prefix, Data: data})
	}
	return out, nil
}

// envKeyRef unpacks an env entry's Secret or ConfigMap key reference; ok=false for any other entry.
func envKeyRef(e corev1.EnvVar) (kind, name, key string, optional, ok bool) {
	switch {
	case e.ValueFrom == nil:
		return "", "", "", false, false
	case e.ValueFrom.SecretKeyRef != nil:
		ref := e.ValueFrom.SecretKeyRef
		return "Secret", ref.Name, ref.Key, ref.Optional != nil && *ref.Optional, true
	case e.ValueFrom.ConfigMapKeyRef != nil:
		ref := e.ValueFrom.ConfigMapKeyRef
		return "ConfigMap", ref.Name, ref.Key, ref.Optional != nil && *ref.Optional, true
	}
	return "", "", "", false, false
}

// dashboardEnvHash hashes the data of every envFrom source and the value of every Secret/ConfigMap key
// dashboard.env references, so a content change rolls the pod. "" when the user references nothing,
// which leaves a dashboard without user env unchanged. A missing, non-optional key reference fails.
func (r *OramaCloudV1Reconciler) dashboardEnvHash(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, sources []dashboardEnvSource) (string, error) {
	type keyValue struct{ Kind, Name, Key, Value string }
	var refs []keyValue
	for _, e := range oc.Spec.Components.Dashboard.Env {
		kind, name, key, optional, ok := envKeyRef(e)
		if !ok {
			continue
		}
		data, found, err := r.readEnvObject(ctx, oc.Namespace, kind, name)
		if err != nil {
			return "", err
		}
		value, has := data[key]
		if !has && !optional {
			if !found {
				return "", fmt.Errorf("dashboard.env %s: %s %q not found in namespace %s", e.Name, kind, name, oc.Namespace)
			}
			return "", fmt.Errorf("dashboard.env %s: %s %q has no key %q", e.Name, kind, name, key)
		}
		refs = append(refs, keyValue{kind, name, key, value})
	}
	if len(sources) == 0 && len(refs) == 0 {
		return "", nil
	}
	// json.Marshal writes map keys sorted, so the same data always hashes the same.
	b, err := json.Marshal(struct {
		Sources []dashboardEnvSource
		Refs    []keyValue
	}{sources, refs})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// overrideEnv returns base without the names extra sets, followed by extra, so each name appears once
// and a user entry replaces the operator's.
func overrideEnv(base, extra []corev1.EnvVar) []corev1.EnvVar {
	out := slices.DeleteFunc(slices.Clone(base), func(e corev1.EnvVar) bool {
		return slices.ContainsFunc(extra, func(x corev1.EnvVar) bool { return x.Name == e.Name })
	})
	return append(out, extra...)
}

// dashboardReferences reports whether oc's dashboard env takes anything from the kind/name object.
func dashboardReferences(oc *oramav1alpha1.OramaCloudV1, kind, name string) bool {
	d := oc.Spec.Components.Dashboard
	if !d.Enabled {
		return false
	}
	for _, src := range d.EnvFrom {
		if src.SecretRef == nil && src.ConfigMapRef == nil {
			continue
		}
		if k, n, _ := envFromRef(src); k == kind && n == name {
			return true
		}
	}
	for _, e := range d.Env {
		if k, n, _, _, ok := envKeyRef(e); ok && k == kind && n == name {
			return true
		}
	}
	return false
}

// requestsForDashboardEnv wakes every OramaCloudV1 whose dashboard env reads the changed Secret or
// ConfigMap, so a content change re-hashes and rolls the dashboard.
func (r *OramaCloudV1Reconciler) requestsForDashboardEnv(ctx context.Context, obj client.Object) []reconcile.Request {
	kind := "ConfigMap"
	if _, ok := obj.(*corev1.Secret); ok {
		kind = "Secret"
	}
	var list oramav1alpha1.OramaCloudV1List
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
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

// dashboardCredentialSourceFunc binds dashboardCredentialSource to oc's current env. A source that
// cannot be read is left out here; the dashboard reconcile reports it.
func (r *OramaCloudV1Reconciler) dashboardCredentialSourceFunc(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) func(key string) (credentialCheck, bool, bool) {
	d := oc.Spec.Components.Dashboard
	if !d.Enabled || (len(d.EnvFrom) == 0 && len(d.Env) == 0) {
		return func(string) (credentialCheck, bool, bool) { return credentialCheck{}, false, false }
	}
	operatorEnv, _ := dashboardOperatorEnv(oc)
	var sources []dashboardEnvSource
	for _, src := range d.EnvFrom {
		if src.SecretRef == nil && src.ConfigMapRef == nil {
			continue
		}
		kind, name, _ := envFromRef(src)
		if data, found, err := r.readEnvObject(ctx, oc.Namespace, kind, name); err == nil && found {
			sources = append(sources, dashboardEnvSource{Kind: kind, Name: name, Prefix: src.Prefix, Data: data})
		}
	}
	return func(key string) (credentialCheck, bool, bool) {
		return dashboardCredentialSource(oc, operatorEnv, sources, key)
	}
}

// dashboardCredentialSource says where the dashboard really reads key from, once the user env is
// applied: a Secret key to check instead of <cr>-dashboard-secret's (ok, check=true), a value that is
// not a Secret key (ok, check=false: a literal, a ConfigMap, a field), or ok=false when key still comes
// from <cr>-dashboard-secret. operatorEnv is the operator's explicit dashboard env.
func dashboardCredentialSource(oc *oramav1alpha1.OramaCloudV1, operatorEnv []corev1.EnvVar, sources []dashboardEnvSource, key string) (credentialCheck, bool, bool) {
	env := overrideEnv(operatorEnv, oc.Spec.Components.Dashboard.Env)
	if i := slices.IndexFunc(env, func(e corev1.EnvVar) bool { return e.Name == key }); i >= 0 {
		if kind, name, k, _, ok := envKeyRef(env[i]); ok && kind == "Secret" {
			return credentialCheck{secret: name, key: k}, true, true
		}
		return credentialCheck{}, false, true
	}
	for _, src := range slices.Backward(sources) {
		if k, has := cutPrefixKey(src, key); has {
			if src.Kind == "Secret" {
				return credentialCheck{secret: src.Name, key: k}, true, true
			}
			return credentialCheck{}, false, true
		}
	}
	return credentialCheck{}, false, false
}

// cutPrefixKey is the source key that becomes env var name through src's prefix, if src has it.
func cutPrefixKey(src dashboardEnvSource, name string) (string, bool) {
	k, ok := strings.CutPrefix(name, src.Prefix)
	if !ok {
		return "", false
	}
	_, has := src.Data[k]
	return k, has
}

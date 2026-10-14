/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Stack-wide credentials (#307): the broker admin user, the Redis password and the OramaCore master
// API key. Each lives in one Secret that every client reads, generated once and never overwritten,
// so a value set in the Secret is the value every component connects with.
//
// These Secrets carry the component label "credentials", which no teardown removes: disabling the
// broker or cache keeps its password, so a retained data volume still matches it on re-enable.

const (
	credentialsComponent = "credentials"

	// masterAPIKeyKey is the key of the master API key in <cr>-master-api-key (and the default key of
	// spec.masterAPIKeySecretRef).
	masterAPIKeyKey = "MASTER_API_KEY"

	// defaultBrokerAdminUser is the broker admin user a generated <cr>-broker-admin names.
	defaultBrokerAdminUser = "orama"

	// The variables that carry the broker admin user and password into a client's env, and their
	// placeholders in a config template.
	brokerUserEnv     = "BROKER_USERNAME"
	brokerPasswordEnv = "BROKER_PASSWORD"
	// redisPasswordEnv carries <cr>-cache-auth's password into the cache container and its clients.
	redisPasswordEnv = "REDIS_PASSWORD"

	// seededPrefix marks a dummy value: every seeded component credential and the password the
	// broker admin Secret held before #307 start with it.
	seededPrefix = "dev-"
)

func cacheAuthSecretName(oc *oramav1alpha1.OramaCloudV1) string { return oc.Name + "-cache-auth" }

// masterAPIKeyRef is the Secret and key every component reads MASTER_API_KEY from:
// spec.masterAPIKeySecretRef; else, on a read member with a remote peer, the peer's credentials
// Secret, so the member answers to its primary's key; else <cr>-master-api-key.
func masterAPIKeyRef(oc *oramav1alpha1.OramaCloudV1) (name, key string) {
	if ref := oc.Spec.MasterAPIKeySecretRef; ref != nil && ref.Name != "" {
		key = ref.Key
		if key == "" {
			key = masterAPIKeyKey
		}
		return ref.Name, key
	}
	if p := remotePeer(oc); p != nil {
		return p.CredentialsSecretRef.Name, masterAPIKeyKey
	}
	return generatedMasterAPIKeySecretName(oc), masterAPIKeyKey
}

// generatedMasterAPIKeySecretName is the master API key Secret the operator creates when nothing
// else names one.
func generatedMasterAPIKeySecretName(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-master-api-key"
}

// masterAPIKeyEnv is the MASTER_API_KEY variable of every component that reads it. An explicit env
// entry wins over envFrom, so a key left in an older component Secret is ignored.
func masterAPIKeyEnv(oc *oramav1alpha1.OramaCloudV1) corev1.EnvVar {
	name, key := masterAPIKeyRef(oc)
	return secretKeyEnv(masterAPIKeyKey, name, key)
}

// brokerCredentialEnv carries the broker admin user and password from <cr>-broker-admin.
func brokerCredentialEnv(oc *oramav1alpha1.OramaCloudV1) []corev1.EnvVar {
	return []corev1.EnvVar{
		secretKeyEnv(brokerUserEnv, brokerAdminSecretName(oc), "username"),
		secretKeyEnv(brokerPasswordEnv, brokerAdminSecretName(oc), "password"),
	}
}

// engineBrokerCredentialEnv is the broker account an engine side (writer or reader) authenticates
// to the stream with: the remote reader account from the peer's credentials Secret on a read
// member's readers, the in-cluster broker admin otherwise.
func engineBrokerCredentialEnv(oc *oramav1alpha1.OramaCloudV1, side string) []corev1.EnvVar {
	if p := remotePeer(oc); p != nil && side == "reader" {
		return []corev1.EnvVar{
			secretKeyEnv(brokerUserEnv, p.CredentialsSecretRef.Name, remoteReadBrokerUserKey),
			secretKeyEnv(brokerPasswordEnv, p.CredentialsSecretRef.Name, remoteReadBrokerPasswordKey),
		}
	}
	return brokerCredentialEnv(oc)
}

// redisPasswordEnvVar carries the Redis password from <cr>-cache-auth.
func redisPasswordEnvVar(oc *oramav1alpha1.OramaCloudV1) corev1.EnvVar {
	return secretKeyEnv(redisPasswordEnv, cacheAuthSecretName(oc), "password")
}

// redisURL is the in-cluster Redis URL a client connects with. The password is a $(REDIS_PASSWORD)
// reference that the kubelet expands from an earlier env entry (redisPasswordEnvVar), so the pod
// spec never holds the value. The generated password is hex, which needs no URL escaping.
func redisURL(oc *oramav1alpha1.OramaCloudV1) string {
	return "redis://:$(" + redisPasswordEnv + ")@" + oc.Name + "-cache:6379"
}

// amqpURL is the broker's AMQP URL with the admin credentials as $(VAR) references (brokerCredentialEnv).
func amqpURL(oc *oramav1alpha1.OramaCloudV1) string {
	return "amqp://$(" + brokerUserEnv + "):$(" + brokerPasswordEnv + ")@" + oc.Name + "-broker:5672"
}

// storeInCluster reports whether a data store runs in this cluster for this CR: enabled, not
// external, and not a central store that only the primary region runs.
func storeInCluster(oc *oramav1alpha1.OramaCloudV1, spec oramav1alpha1.DataStoreSpec) bool {
	return spec.Mode != "external" && spec.Enabled && !(spec.Placement == "central" && oc.Spec.Region.Role == "read")
}

// ensureCredentialSecrets creates the stack-wide credential Secrets that are missing:
// <cr>-broker-admin and <cr>-cache-auth with random passwords where the broker and cache run in
// this cluster, and <cr>-master-api-key unless spec.masterAPIKeySecretRef names another Secret. An
// existing Secret is never changed. Elsewhere the Secrets must be provided (CredentialsReady names
// a missing one).
func (r *OramaCloudV1Reconciler) ensureCredentialSecrets(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	lbls := labelsFor(oc, credentialsComponent)
	if storeInCluster(oc, oc.Spec.Components.Broker) {
		pass, err := randHex(24)
		if err != nil {
			return err
		}
		if err := r.createSecretIfAbsent(ctx, oc, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: brokerAdminSecretName(oc), Namespace: oc.Namespace, Labels: lbls},
			StringData: map[string]string{"username": defaultBrokerAdminUser, "password": pass},
		}); err != nil {
			return fmt.Errorf("broker admin secret: %w", err)
		}
	}
	if storeInCluster(oc, oc.Spec.Components.Cache) {
		pass, err := randHex(24)
		if err != nil {
			return err
		}
		if err := r.createSecretIfAbsent(ctx, oc, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: cacheAuthSecretName(oc), Namespace: oc.Namespace, Labels: lbls},
			StringData: map[string]string{"password": pass},
		}); err != nil {
			return fmt.Errorf("cache auth secret: %w", err)
		}
	}
	if name, _ := masterAPIKeyRef(oc); name == generatedMasterAPIKeySecretName(oc) {
		key, err := r.initialMasterAPIKey(ctx, oc)
		if err != nil {
			return err
		}
		if err := r.createSecretIfAbsent(ctx, oc, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: oc.Namespace, Labels: lbls},
			StringData: map[string]string{masterAPIKeyKey: key},
		}); err != nil {
			return fmt.Errorf("master API key secret: %w", err)
		}
	}
	return nil
}

// initialMasterAPIKey is the value a new <cr>-master-api-key starts with. A stack reconciled before
// #307 kept its key in <cr>-engine-secret; carrying that value over keeps the engine, dashboard and
// control DB agreeing through the upgrade. A new stack gets a random key.
func (r *OramaCloudV1Reconciler) initialMasterAPIKey(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (string, error) {
	engine := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: componentSecretName(oc, "engine")}, engine)
	switch {
	case err == nil && secretValue(engine, masterAPIKeyKey) != "":
		return secretValue(engine, masterAPIKeyKey), nil
	case err != nil && !apierrors.IsNotFound(err):
		return "", err
	}
	return randHex(32)
}

// credentialsConditionType is False while any credential the stack uses is a seeded dummy or missing.
const credentialsConditionType = "CredentialsReady"

// credentialCheck is one Secret key the stack reads, with the value it was seeded with ("" for
// generated or user-provided credentials).
type credentialCheck struct{ secret, key, seed string }

// credentialChecks lists every Secret key the stack's running components read credentials from: the
// seeded keys of each component Secret the operator keeps, the broker admin password and the Redis
// password wherever a component uses them, and the master API key.
func (r *OramaCloudV1Reconciler) credentialChecks(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) ([]credentialCheck, error) {
	var checks []credentialCheck
	// Component Secrets exist for the components that run (teardown deletes the rest). They are
	// matched by name, so a Secret recreated by hand without the operator's labels is checked too.
	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, client.InNamespace(oc.Namespace)); err != nil {
		return nil, err
	}
	dashboardSource := r.dashboardCredentialSourceFunc(ctx, oc)
	for _, s := range secrets.Items {
		rest, hasPrefix := strings.CutPrefix(s.Name, oc.Name+"-")
		component, hasSuffix := strings.CutSuffix(rest, "-secret")
		if !hasPrefix || !hasSuffix {
			continue
		}
		for key, seed := range componentSecretData(component) {
			// A dashboard key the user env provides is checked where the dashboard reads it (operator#311).
			if component == "dashboard" {
				if check, mustCheck, overridden := dashboardSource(key); overridden {
					if mustCheck {
						checks = append(checks, check)
					}
					continue
				}
			}
			checks = append(checks, credentialCheck{secret: s.Name, key: key, seed: seed})
		}
	}
	c := oc.Spec.Components
	peer := remotePeer(oc)
	if storeInCluster(oc, c.Broker) || (c.Engine.Enabled && peer == nil) || (c.Ingestion.Enabled && c.Ingestion.Harvest) {
		checks = append(checks, credentialCheck{secret: brokerAdminSecretName(oc), key: "password"})
	}
	if c.Engine.Enabled && peer != nil {
		checks = append(checks, credentialCheck{secret: peer.CredentialsSecretRef.Name, key: remoteReadBrokerPasswordKey})
	}
	if storeInCluster(oc, c.Cache) || c.Ingestion.Enabled {
		checks = append(checks, credentialCheck{secret: cacheAuthSecretName(oc), key: "password"})
	}
	name, key := masterAPIKeyRef(oc)
	return append(checks, credentialCheck{secret: name, key: key}), nil
}

// credentialFindings returns the sorted "Secret/key" names whose value is missing, empty, its seeded
// dummy or another dev-* value. Values are compared here and never returned, logged or recorded.
func (r *OramaCloudV1Reconciler) credentialFindings(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) ([]string, error) {
	checks, err := r.credentialChecks(ctx, oc)
	if err != nil {
		return nil, err
	}
	cache := map[string]*corev1.Secret{}
	var findings []string
	for _, c := range checks {
		s, seen := cache[c.secret]
		if !seen {
			s = &corev1.Secret{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: c.secret}, s); err != nil {
				if !apierrors.IsNotFound(err) {
					return nil, err
				}
				s = nil
			}
			cache[c.secret] = s
		}
		v := secretValue(s, c.key)
		if v == "" || (c.seed != "" && v == c.seed) || strings.HasPrefix(v, seededPrefix) {
			findings = append(findings, c.secret+"/"+c.key)
		}
	}
	slices.Sort(findings)
	return slices.Compact(findings), nil
}

// secretValue is a Secret key's value ("" for a nil Secret). StringData is read too, for a Secret
// not yet normalized by the API server.
func secretValue(s *corev1.Secret, key string) string {
	if s == nil {
		return ""
	}
	if v, ok := s.Data[key]; ok {
		return string(v)
	}
	return s.StringData[key]
}

// credentialsCondition is the CredentialsReady condition: True when every credential the stack reads
// is set and none is a seeded dummy, else False naming each Secret/key to replace.
func credentialsCondition(findings []string) metav1.Condition {
	if len(findings) == 0 {
		return metav1.Condition{Type: credentialsConditionType, Status: metav1.ConditionTrue, Reason: "CredentialsSet",
			Message: "every credential the stack reads is set and none is a seeded dummy"}
	}
	return metav1.Condition{Type: credentialsConditionType, Status: metav1.ConditionFalse, Reason: "SeededOrMissingCredentials",
		Message: "seeded dummy or missing: " + strings.Join(findings, ", ")}
}

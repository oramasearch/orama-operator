/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Stack-wide credentials (#307): the broker admin user and the Redis password, each in one Secret
// that every client reads by secretKeyRef, the analytics key the readers and analytics-processor
// share (#386), and the master API key, which the CR names (spec.masterAPIKeySecretRef). A
// workload's template carries credentialsHashKey, the digest of the credential Secrets it reads, so
// a changed value rolls exactly the workloads that use it.
//
// The generated Secrets carry the component label "credentials", which no teardown removes:
// disabling the broker or cache keeps its password, so a retained data volume still matches it on
// re-enable, and disabling analytics-processor or ClickHouse keeps the analytics key.

const (
	credentialsComponent = "credentials"

	// credentialsHashKey is the pod-template annotation holding secretDataHash of the credential
	// Secrets a workload reads (broker admin, cache auth, analytics key, master API key).
	credentialsHashKey = "orama.cloud/credentials-hash"

	// masterAPIKeyEnvName is the variable every consumer receives the master key in, and the key
	// spec.masterAPIKeySecretRef reads when its key is omitted.
	masterAPIKeyEnvName = "MASTER_API_KEY"

	// brokerAdminUser is the broker admin user a generated <cr>-broker-admin names.
	brokerAdminUser = "orama"

	// The variables that carry the broker admin user and password and the Redis password into a
	// container's env; each also names its __VAR__ placeholder in a config template.
	brokerUserEnv     = "BROKER_USERNAME"
	brokerPasswordEnv = "BROKER_PASSWORD"
	redisPasswordEnv  = "REDIS_PASSWORD"

	// analyticsKeyEnvName is the key of <cr>-analytics-api-key, and the variable that carries it into
	// the readers' and analytics-processor's render-config; it also names the __ANALYTICS_API_KEY__
	// placeholder.
	analyticsKeyEnvName = "ANALYTICS_API_KEY"
)

func cacheAuthSecretName(oc *oramav1alpha1.OramaCloudV1) string { return oc.Name + "-cache-auth" }

// analyticsKeySecretName is the generated Secret holding the key readers check on GET /v1/analytics
// (reader_side.analytics.api_key) and analytics-processor sends. It can read and erase search events
// and nothing else, so the processor holds no master key.
func analyticsKeySecretName(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-analytics-api-key"
}

// credentialKeys are the keys each generated credential Secret must hold, nonempty.
func credentialKeys(oc *oramav1alpha1.OramaCloudV1, secret string) []string {
	switch secret {
	case brokerAdminSecretName(oc):
		return []string{"username", "password"}
	case cacheAuthSecretName(oc):
		return []string{"password"}
	case analyticsKeySecretName(oc):
		return []string{analyticsKeyEnvName}
	}
	return nil
}

// storeLocal reports whether a data store runs in this cluster for this CR: enabled, not external,
// and not a central store that only the primary region runs.
func storeLocal(oc *oramav1alpha1.OramaCloudV1, spec oramav1alpha1.DataStoreSpec) bool {
	return spec.Mode != "external" && spec.Enabled && !(spec.Placement == "central" && oc.Spec.Region.Role == "read")
}

// masterAPIKeyRef is the Secret and key spec.masterAPIKeySecretRef names. The name is empty when
// the reference is unset or has no name; the key defaults to MASTER_API_KEY.
func masterAPIKeyRef(oc *oramav1alpha1.OramaCloudV1) (name, key string) {
	ref := oc.Spec.MasterAPIKeySecretRef
	if ref == nil || ref.Name == "" {
		return "", ""
	}
	if ref.Key == "" {
		return ref.Name, masterAPIKeyEnvName
	}
	return ref.Name, ref.Key
}

// masterAPIKeyEnv is the MASTER_API_KEY entry of every consumer, a required secretKeyRef to the
// referenced Secret. Call it only with a reference whose name is set. An explicit env entry wins
// over envFrom, so a MASTER_API_KEY left in an older component Secret is ignored.
func masterAPIKeyEnv(oc *oramav1alpha1.OramaCloudV1) corev1.EnvVar {
	name, key := masterAPIKeyRef(oc)
	return secretKeyEnv(masterAPIKeyEnvName, name, key)
}

// brokerCredentialEnv carries the broker admin user and password from <cr>-broker-admin.
func brokerCredentialEnv(oc *oramav1alpha1.OramaCloudV1) []corev1.EnvVar {
	return []corev1.EnvVar{
		secretKeyEnv(brokerUserEnv, brokerAdminSecretName(oc), "username"),
		secretKeyEnv(brokerPasswordEnv, brokerAdminSecretName(oc), "password"),
	}
}

// analyticsKeyEnv carries the analytics key from <cr>-analytics-api-key.
func analyticsKeyEnv(oc *oramav1alpha1.OramaCloudV1) corev1.EnvVar {
	return secretKeyEnv(analyticsKeyEnvName, analyticsKeySecretName(oc), analyticsKeyEnvName)
}

// redisPasswordEnvVar carries the Redis password from <cr>-cache-auth.
func redisPasswordEnvVar(oc *oramav1alpha1.OramaCloudV1) corev1.EnvVar {
	return secretKeyEnv(redisPasswordEnv, cacheAuthSecretName(oc), "password")
}

// amqpURL is the broker's AMQP URL. The credentials are $(VAR) references the kubelet expands from
// the brokerCredentialEnv entries, which must come before it, so the pod spec holds no value.
func amqpURL(oc *oramav1alpha1.OramaCloudV1) string {
	return "amqp://$(" + brokerUserEnv + "):$(" + brokerPasswordEnv + ")@" + oc.Name + "-broker:5672"
}

// redisURL is the in-cluster Redis URL, its password a $(REDIS_PASSWORD) reference to an earlier
// redisPasswordEnvVar entry.
func redisURL(oc *oramav1alpha1.OramaCloudV1) string {
	return "redis://:$(" + redisPasswordEnv + ")@" + oc.Name + "-cache:6379"
}

func secretKeyEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key}}}
}

// ensureCredentialSecrets creates <cr>-broker-admin and <cr>-cache-auth, with a random 48-character
// hex password, where the broker and the cache run in this cluster, and <cr>-analytics-api-key, a
// random 48-character hex ANALYTICS_API_KEY, on every stack. An existing Secret, generated or
// supplied, is never changed. Each one is then read back, so a Secret that won a create race is the
// one checked: a missing, empty or unsupported value fails the reconcile, naming the Secret and key.
// Elsewhere a broker or cache client's Secret must come through the set's secrets list.
func (r *OramaCloudV1Reconciler) ensureCredentialSecrets(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	c := oc.Spec.Components
	for _, s := range []struct {
		local  bool
		name   string
		values func(pass string) map[string][]byte
	}{
		{storeLocal(oc, c.Broker), brokerAdminSecretName(oc), func(p string) map[string][]byte {
			return map[string][]byte{"username": []byte(brokerAdminUser), "password": []byte(p)}
		}},
		{storeLocal(oc, c.Cache), cacheAuthSecretName(oc), func(p string) map[string][]byte {
			return map[string][]byte{"password": []byte(p)}
		}},
		// The readers and analytics-processor are its only consumers, but it does not follow either:
		// re-enabling the processor or ClickHouse finds the same key.
		{true, analyticsKeySecretName(oc), func(k string) map[string][]byte {
			return map[string][]byte{analyticsKeyEnvName: []byte(k)}
		}},
	} {
		if !s.local {
			continue
		}
		pass, err := randHex(24)
		if err != nil {
			return fmt.Errorf("generate %s: %w", s.name, err)
		}
		if err := r.createSecretIfAbsent(ctx, oc, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: s.name, Namespace: oc.Namespace, Labels: labelsFor(oc, credentialsComponent)},
			Type:       corev1.SecretTypeOpaque,
			Data:       s.values(pass),
		}); err != nil {
			return fmt.Errorf("credential Secret %s/%s: %w", oc.Namespace, s.name, err)
		}
		stored := &corev1.Secret{}
		if err := r.secretReader().Get(ctx, types.NamespacedName{Namespace: oc.Namespace, Name: s.name}, stored); err != nil {
			return fmt.Errorf("read credential Secret %s/%s: %w", oc.Namespace, s.name, err)
		}
		for _, key := range credentialKeys(oc, s.name) {
			if err := checkCredentialValue(stored, key); err != nil {
				return err
			}
		}
	}
	return nil
}

// secretReader is the reader for credential Secrets: uncached when the manager provides one, since
// this reconcile may have just created the Secret.
func (r *OramaCloudV1Reconciler) secretReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// credentialsHash reads the named Secrets a workload takes credentials from and returns their
// secretDataHash. A Secret that does not exist yet is left out, so the pod waits on its
// secretKeyRef and the template rolls once it appears. An existing broker-admin, cache-auth or
// analytics key Secret must hold its keys, nonempty and in credentialCharset: a broken one fails the reconcile, naming
// the Secret and key, and is left as it is.
func (r *OramaCloudV1Reconciler) credentialsHash(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, names ...string) (string, error) {
	var secrets []*corev1.Secret
	for _, name := range names {
		sec := &corev1.Secret{}
		if err := r.secretReader().Get(ctx, types.NamespacedName{Namespace: oc.Namespace, Name: name}, sec); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", fmt.Errorf("read credential Secret %s/%s: %w", oc.Namespace, name, err)
		}
		for _, key := range credentialKeys(oc, name) {
			if err := checkCredentialValue(sec, key); err != nil {
				return "", err
			}
		}
		secrets = append(secrets, sec)
	}
	return secretDataHash(secrets...), nil
}

// credentialCharset is what a broker, Redis or analytics credential may hold: RFC 3986 unreserved
// characters. Clients receive it inside URLs and rabbitmq.conf/credentials.json, so anything else
// would need escaping the clients do not do. Generated hex values always qualify.
const credentialCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-._~"

func checkCredentialValue(sec *corev1.Secret, key string) error {
	v, ok := sec.Data[key]
	switch {
	case !ok:
		return fmt.Errorf("credential Secret %s/%s has no key %s", sec.Namespace, sec.Name, key)
	case len(v) == 0:
		return fmt.Errorf("credential Secret %s/%s key %s is empty", sec.Namespace, sec.Name, key)
	case strings.Trim(string(v), credentialCharset) != "":
		return fmt.Errorf("credential Secret %s/%s key %s holds characters outside A-Z a-z 0-9 - . _ ~", sec.Namespace, sec.Name, key)
	}
	return nil
}

// masterAPIKeySecret is the master API key Secret a consumer reads, and false while
// spec.masterAPIKeySecretRef has no name. A consumer then leaves its workload as it is (it cannot
// build the env entry), and validateMasterAPIKey reports the missing reference in Ready.
func masterAPIKeySecret(oc *oramav1alpha1.OramaCloudV1) (string, bool) {
	name, _ := masterAPIKeyRef(oc)
	return name, name != ""
}

// masterAPIKeyConsumers reports whether any enabled component reads MASTER_API_KEY.
// analytics-processor does not: it holds the analytics key (#386).
func masterAPIKeyConsumers(oc *oramav1alpha1.OramaCloudV1) bool {
	c := oc.Spec.Components
	return c.Engine.Enabled || c.Ingestion.Enabled || c.Workers.Enabled || c.Dashboard.Enabled
}

// validateMasterAPIKey checks spec.masterAPIKeySecretRef. It returns nil when the reference is
// usable, a Ready=False condition naming the cause when it is not, and an error only when the
// Secret cannot be read. Every consumer needs the reference's name to build its pod; the Secret's
// value is checked only while the engine is enabled, since the engine does not start without a
// real key. Messages name the Secret and key, never a value.
func (r *OramaCloudV1Reconciler) validateMasterAPIKey(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (*metav1.Condition, error) {
	if !masterAPIKeyConsumers(oc) {
		return nil, nil
	}
	notReady := func(reason, msg string) *metav1.Condition {
		return &metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: reason, Message: msg}
	}
	name, key := masterAPIKeyRef(oc)
	if name == "" {
		return notReady("MasterAPIKeyRefMissing", "spec.masterAPIKeySecretRef.name is required"), nil
	}
	if !oc.Spec.Components.Engine.Enabled {
		return nil, nil
	}
	where := fmt.Sprintf("Secret %s/%s key %s", oc.Namespace, name, key)
	sec := &corev1.Secret{}
	if err := r.secretReader().Get(ctx, types.NamespacedName{Namespace: oc.Namespace, Name: name}, sec); err != nil {
		if apierrors.IsNotFound(err) {
			return notReady("MasterAPIKeySecretMissing", fmt.Sprintf("master API key %s: the Secret does not exist", where)), nil
		}
		return nil, fmt.Errorf("read master API key Secret %s/%s: %w", oc.Namespace, name, err)
	}
	v, ok := sec.Data[key]
	switch {
	case !ok:
		return notReady("MasterAPIKeyKeyMissing", fmt.Sprintf("master API key %s: the key does not exist", where)), nil
	case len(v) == 0:
		return notReady("MasterAPIKeyEmpty", fmt.Sprintf("master API key %s is empty", where)), nil
	case strings.HasPrefix(string(v), "dev-"):
		return notReady("MasterAPIKeyPlaceholder", fmt.Sprintf("master API key %s is a dev-* placeholder", where)), nil
	case strings.ContainsAny(string(v), "\r\n"):
		// The render-config script substitutes the key on one line.
		return notReady("MasterAPIKeyInvalid", fmt.Sprintf("master API key %s contains a line break", where)), nil
	}
	return nil, nil
}

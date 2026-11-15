package controller

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Former defaults are recognized only for the one-time upgrade, never seeded into a new Secret.
const (
	legacyBrokerPassword  = "dev-rabbitmq-password"
	legacyDiscoSecret     = "dev-disco-secret"
	legacyPgAdminPassword = "dev-pgadmin-password"
)

type legacyCredentialSource struct {
	component string
	key       string
}

type legacyCredential struct {
	key         string
	placeholder string
	sources     []legacyCredentialSource
}

// legacyCredentialSeed retains a real shared credential, or adopts agreeing real component
// credentials before falling back to a generated value. Conflicts stop bootstrap before any
// consumer changes. Only the exact former seed is a placeholder; other dev-prefixed values survive.
func (r *OramaCloudV1Reconciler) legacyCredentialSeed(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, name string, legacy legacyCredential, generated string) (string, error) {
	stored := &corev1.Secret{}
	err := r.secretReader().Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: name}, stored)
	if err == nil && string(stored.Data[legacy.key]) != legacy.placeholder {
		return generated, nil // The existing shared Secret wins; normal validation checks it.
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("read credential Secret %s/%s: %w", oc.Namespace, name, err)
	}
	var adopted []byte
	var adoptedFrom string
	for _, source := range legacy.sources {
		sec := &corev1.Secret{}
		key := client.ObjectKey{Namespace: oc.Namespace, Name: componentSecretName(oc, source.component)}
		if err := r.secretReader().Get(ctx, key, sec); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", fmt.Errorf("read legacy credential Secret %s: %w", key, err)
		}
		value, found := sec.Data[source.key]
		if !found || string(value) == legacy.placeholder {
			continue
		}
		if err := checkCredentialValue(sec, source.key); err != nil {
			return "", err
		}
		where := fmt.Sprintf("%s key %s", key, source.key)
		if adopted != nil && !bytes.Equal(adopted, value) {
			return "", fmt.Errorf("credential Secret %s/%s: legacy credentials in %s and %s disagree", oc.Namespace, name, adoptedFrom, where)
		}
		adopted, adoptedFrom = value, where
	}
	if adopted != nil {
		return string(adopted), nil
	}
	return generated, nil
}

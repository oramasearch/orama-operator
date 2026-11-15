// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	sealingGenerationKey = "generation"
	sealingActiveKIDKey  = "active-kid"
	sealingPrivatePrefix = "private-"
	sealingPublicPrefix  = "public-"
)

type lakeSealingMaterial struct {
	Cluster    string
	Generation int64
	KID        string
	PublicKey  []byte
	PrivateKey []byte
	Status     *oramav1alpha1.LakeSealingStatus
}

func lakeSealingKeySecret(name string) string {
	return "lake-" + name + "-sealing-key"
}

func lakeSealingGeneration(lake *oramav1alpha1.Lake) int64 {
	if lake.Spec.Sealing.Generation > 0 {
		return lake.Spec.Sealing.Generation
	}
	return 1
}

func lakeClusterID(lake *oramav1alpha1.Lake) string {
	if lake.UID != "" {
		return string(lake.UID)
	}
	return lake.Name
}

func (r *LakeReconciler) reconcileLakeSealingKey(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	namespace string,
	labels map[string]string,
) (lakeSealingMaterial, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lakeSealingKeySecret(lake.Name),
			Namespace: namespace,
		},
	}
	desiredGeneration := lakeSealingGeneration(lake)
	remaining, unresolved, err := r.sealedSecretKeyUse(ctx, lake.Name)
	if err != nil {
		return lakeSealingMaterial{}, err
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if err := updateLakeSealingSecret(secret, desiredGeneration, remaining, unresolved); err != nil {
			return err
		}
		secret.Labels = labels
		return controllerutil.SetControllerReference(lake, secret, r.Scheme)
	})
	if err != nil {
		return lakeSealingMaterial{}, err
	}

	generation, err := strconv.ParseInt(string(secret.Data[sealingGenerationKey]), 10, 64)
	if err != nil {
		return lakeSealingMaterial{}, fmt.Errorf("parse reconciled sealing key generation: %w", err)
	}
	kid := string(secret.Data[sealingActiveKIDKey])
	publicKey := append([]byte(nil), secret.Data[sealingPublicPrefix+kid]...)
	privateKey := append([]byte(nil), secret.Data[sealingPrivatePrefix+kid]...)
	if err := validateLakeSealingKeypair(kid, privateKey, publicKey); err != nil {
		return lakeSealingMaterial{}, err
	}
	previous := lakePreviousKeyStatus(secret, kid, remaining)
	status := &oramav1alpha1.LakeSealingStatus{
		Cluster:           lakeClusterID(lake),
		Generation:        generation,
		KID:               kid,
		PublicKey:         base64.RawURLEncoding.EncodeToString(publicKey),
		PreviousKeys:      previous,
		UnresolvedRecords: unresolved,
	}
	return lakeSealingMaterial{
		Cluster:    status.Cluster,
		Generation: generation,
		KID:        kid,
		PublicKey:  publicKey,
		PrivateKey: privateKey,
		Status:     status,
	}, nil
}

func updateLakeSealingSecret(
	secret *corev1.Secret,
	desiredGeneration int64,
	remaining map[string]int32,
	unresolved int32,
) error {
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	currentGeneration := int64(0)
	if value := secret.Data[sealingGenerationKey]; len(value) > 0 {
		parsed, err := strconv.ParseInt(string(value), 10, 64)
		if err != nil {
			return fmt.Errorf("parse sealing key generation: %w", err)
		}
		currentGeneration = parsed
	}
	if desiredGeneration < currentGeneration {
		return fmt.Errorf(
			"spec.sealing.generation %d cannot decrease from %d",
			desiredGeneration,
			currentGeneration,
		)
	}
	if currentGeneration == 0 || desiredGeneration > currentGeneration {
		private, public, err := newLakeSealingKeypair()
		if err != nil {
			return err
		}
		kid := sealingKID(public)
		secret.Data[sealingGenerationKey] = []byte(strconv.FormatInt(desiredGeneration, 10))
		secret.Data[sealingActiveKIDKey] = []byte(kid)
		secret.Data[sealingPrivatePrefix+kid] = private
		secret.Data[sealingPublicPrefix+kid] = public
	}
	activeKID := string(secret.Data[sealingActiveKIDKey])
	for key := range secret.Data {
		if !strings.HasPrefix(key, sealingPrivatePrefix) {
			continue
		}
		kid := strings.TrimPrefix(key, sealingPrivatePrefix)
		if kid == activeKID || remaining[kid] > 0 || unresolved > 0 {
			continue
		}
		delete(secret.Data, sealingPrivatePrefix+kid)
		delete(secret.Data, sealingPublicPrefix+kid)
	}
	return nil
}

func validateLakeSealingKeypair(kid string, privateKey []byte, publicKey []byte) error {
	if len(publicKey) != 32 || len(privateKey) != 32 {
		return fmt.Errorf("active sealing key %q is incomplete", kid)
	}
	if sealingKID(publicKey) != kid {
		return fmt.Errorf("active sealing public key does not match kid %q", kid)
	}
	private, err := ecdh.X25519().NewPrivateKey(privateKey)
	if err != nil {
		return fmt.Errorf("load active sealing private key: %w", err)
	}
	if string(private.PublicKey().Bytes()) != string(publicKey) {
		return fmt.Errorf("active sealing keypair %q does not match", kid)
	}
	return nil
}

func lakePreviousKeyStatus(
	secret *corev1.Secret,
	activeKID string,
	remaining map[string]int32,
) []oramav1alpha1.LakeSealingPreviousKeyStatus {
	var previous []oramav1alpha1.LakeSealingPreviousKeyStatus
	for key := range secret.Data {
		if !strings.HasPrefix(key, sealingPrivatePrefix) {
			continue
		}
		kid := strings.TrimPrefix(key, sealingPrivatePrefix)
		if kid == activeKID {
			continue
		}
		count := remaining[kid]
		previous = append(previous, oramav1alpha1.LakeSealingPreviousKeyStatus{
			KID:       kid,
			Remaining: count,
		})
	}
	sort.Slice(previous, func(i int, j int) bool {
		return previous[i].KID < previous[j].KID
	})
	return previous
}

func newLakeSealingKeypair() ([]byte, []byte, error) {
	private, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate X25519 sealing key: %w", err)
	}
	return private.Bytes(), private.PublicKey().Bytes(), nil
}

func (r *LakeReconciler) sealedSecretKeyUse(
	ctx context.Context,
	lakeName string,
) (map[string]int32, int32, error) {
	var resources oramav1alpha1.LakeSealedSecretList
	if err := r.List(ctx, &resources); err != nil {
		return nil, 0, fmt.Errorf("list LakeSealedSecrets: %w", err)
	}
	remaining := map[string]int32{}
	unresolved := int32(0)
	for i := range resources.Items {
		resource := &resources.Items[i]
		if resource.Spec.LakeRef.Name != lakeName {
			continue
		}
		if resource.Spec.Source.Type != "literal" {
			if resource.Status.KID != "" {
				remaining[resource.Status.KID]++
			}
			continue
		}
		for _, record := range resource.Spec.Data {
			envelope, err := decodeSealedSecretRecord(record)
			if err != nil {
				unresolved++
				continue
			}
			remaining[envelope.KID]++
		}
	}
	return remaining, unresolved, nil
}

func loadLakeSealingKey(secret *corev1.Secret, kid string) ([]byte, []byte, error) {
	privateKey := secret.Data[sealingPrivatePrefix+kid]
	publicKey := secret.Data[sealingPublicPrefix+kid]
	if len(privateKey) != 32 || len(publicKey) != 32 {
		return nil, nil, fmt.Errorf("unknown sealing kid %q", kid)
	}
	return privateKey, publicKey, nil
}

func getLakeSealingKeySecret(
	ctx context.Context,
	reader client.Reader,
	lake *oramav1alpha1.Lake,
) (*corev1.Secret, error) {
	namespace := lake.Spec.Namespace
	if namespace == "" {
		namespace = "lake-dev"
	}
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: lakeSealingKeySecret(lake.Name)}
	if err := reader.Get(ctx, key, secret); err != nil {
		return nil, err
	}
	return secret, nil
}

/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/ecdh"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"golang.org/x/crypto/chacha20poly1305"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func TestSealedSecretEnvelopeReferenceFixture(t *testing.T) {
	recipientPrivate := sequenceBytes(1, 32)
	ephemeralPrivate := sequenceBytes(33, 32)
	nonce := sequenceBytes(65, chacha20poly1305.NonceSizeX)
	aad := sealedSecretAAD{Cluster: "cluster-fixture", Namespace: "payments", Name: "database"}
	record, recipientPublic := referenceSeal(t, []byte("fixture-secret"), recipientPrivate, ephemeralPrivate, nonce, aad, "")
	const expected = "pmF2AWJjdFgeEHhaBXET+DLyNpH7FrrQcmyA2bKdKakJ+AEZcFR7Y2FhZKNkbmFtZWhkYXRhYmFzZWdjbHVzdGVyb2NsdXN0ZXItZml4dHVyZWluYW1lc3BhY2VocGF5bWVudHNjZXBrWCBYaa/0UFSXMsuq7V5d+bMKbaMcsOV0K61a1KGnaPGme2NraWR2cXFqXzl3TzFDeUtYOVBiaE5RajNKQWVub25jZVgYQUJDREVGR0hJSktMTU5PUFFSU1RVVldY"
	if record != expected {
		t.Fatalf("fixture changed:\n%s", record)
	}
	plaintext, kid, err := unsealSecretRecord(record, recipientPrivate, recipientPublic, aad)
	if err != nil {
		t.Fatalf("unseal reference fixture: %v", err)
	}
	if string(plaintext) != "fixture-secret" || kid != referenceKID(recipientPublic) {
		t.Fatalf("plaintext=%q kid=%q", plaintext, kid)
	}
}

func TestSealedSecretEnvelopeRejectsWrongAAD(t *testing.T) {
	recipientPrivate := sequenceBytes(1, 32)
	aad := sealedSecretAAD{Cluster: "cluster-a", Namespace: "payments", Name: "database"}
	record, recipientPublic := referenceSeal(
		t,
		[]byte("credential"),
		recipientPrivate,
		sequenceBytes(33, 32),
		sequenceBytes(65, chacha20poly1305.NonceSizeX),
		aad,
		"",
	)
	for _, test := range []struct {
		name string
		aad  sealedSecretAAD
	}{
		{name: "cluster", aad: sealedSecretAAD{Cluster: "cluster-b", Namespace: "payments", Name: "database"}},
		{name: "namespace", aad: sealedSecretAAD{Cluster: "cluster-a", Namespace: "other", Name: "database"}},
		{name: "name", aad: sealedSecretAAD{Cluster: "cluster-a", Namespace: "payments", Name: "other"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := unsealSecretRecord(record, recipientPrivate, recipientPublic, test.aad); err == nil {
				t.Fatal("record unsealed with mismatched AAD")
			}
		})
	}
}

func TestLakeSealedSecretLiteralDelivery(t *testing.T) {
	scheme := newSealedSecretScheme(t)
	recipientPrivate := sequenceBytes(1, 32)
	recipient, err := ecdh.X25519().NewPrivateKey(recipientPrivate)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := recipient.PublicKey().Bytes()
	kid := referenceKID(publicKey)
	aad := sealedSecretAAD{Cluster: "cluster-uid", Namespace: "app", Name: "database"}
	record, _ := referenceSeal(
		t,
		[]byte("password-value"),
		recipientPrivate,
		sequenceBytes(33, 32),
		sequenceBytes(65, chacha20poly1305.NonceSizeX),
		aad,
		"",
	)
	lake, resource, keySecret := sealedSecretObjects(kid, publicKey, recipientPrivate)
	resource.Spec.Data = map[string]string{"password": record}
	resource.Spec.Source = oramav1alpha1.LakeSealedSecretSource{Type: "literal"}
	client := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(lake, resource, keySecret).
		WithStatusSubresource(lake, resource).
		Build()
	reconcileSealedSecret(t, client, scheme, resource.Name, nil)

	produced := &corev1.Secret{}
	key := types.NamespacedName{Namespace: "app", Name: "database"}
	if err := client.Get(context.Background(), key, produced); err != nil {
		t.Fatalf("produced Secret: %v", err)
	}
	if string(produced.Data["password"]) != "password-value" {
		t.Fatalf("produced data = %q", produced.Data["password"])
	}
	if produced.Labels[sealedSecretLabel] != "true" {
		t.Fatalf("sealed label = %q", produced.Labels[sealedSecretLabel])
	}
	if len(produced.OwnerReferences) != 1 || produced.OwnerReferences[0].Kind != "LakeSealedSecret" {
		t.Fatalf("owner references = %+v", produced.OwnerReferences)
	}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: resource.Name}, resource); err != nil {
		t.Fatal(err)
	}
	if resource.Status.Phase != "Ready" || resource.Status.KID != kid || resource.Status.UnsealedAt == nil {
		t.Fatalf("status = %+v", resource.Status)
	}
	firstUnsealedAt := resource.Status.UnsealedAt.DeepCopy()
	reconcileSealedSecret(t, client, scheme, resource.Name, nil)
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: resource.Name}, resource); err != nil {
		t.Fatal(err)
	}
	if !resource.Status.UnsealedAt.Equal(firstUnsealedAt) {
		t.Fatalf("steady reconcile changed unsealedAt from %s to %s", firstUnsealedAt, resource.Status.UnsealedAt)
	}
}

func TestLakeSealedSecretRefusesUnknownKID(t *testing.T) {
	scheme := newSealedSecretScheme(t)
	recipientPrivate := sequenceBytes(1, 32)
	recipient, err := ecdh.X25519().NewPrivateKey(recipientPrivate)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := recipient.PublicKey().Bytes()
	kid := referenceKID(publicKey)
	record, _ := referenceSeal(
		t,
		[]byte("password-value"),
		recipientPrivate,
		sequenceBytes(33, 32),
		sequenceBytes(65, chacha20poly1305.NonceSizeX),
		sealedSecretAAD{Cluster: "cluster-uid", Namespace: "app", Name: "database"},
		"missing-kid",
	)
	lake, resource, keySecret := sealedSecretObjects(kid, publicKey, recipientPrivate)
	resource.Spec.Data = map[string]string{"password": record}
	resource.Spec.Source = oramav1alpha1.LakeSealedSecretSource{Type: "literal"}
	client := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(lake, resource, keySecret).
		WithStatusSubresource(lake, resource).
		Build()
	reconcileSealedSecret(t, client, scheme, resource.Name, nil)

	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: "database"}, &corev1.Secret{}); err == nil {
		t.Fatal("Secret created for an unknown kid")
	}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "app", Name: resource.Name}, resource); err != nil {
		t.Fatal(err)
	}
	if resource.Status.Phase != "Error" || conditionReason(resource.Status.Conditions, "Ready") != "UnsealFailed" {
		t.Fatalf("status = %+v", resource.Status)
	}
}

func TestLakeSealingRotationRetainsUsedKey(t *testing.T) {
	scheme := newSealedSecretScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: types.UID("cluster-uid")},
		Spec:       oramav1alpha1.LakeSpec{Image: "oramasearch/lake:test"},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake).WithStatusSubresource(lake).Build()
	reconciler := &LakeReconciler{Client: client, Scheme: scheme}
	first, err := reconciler.reconcileLakeSealingKey(context.Background(), lake, "lake-dev", map[string]string{"app": "lake"})
	if err != nil {
		t.Fatal(err)
	}
	record, _ := referenceSeal(
		t,
		[]byte("old"),
		first.PrivateKey,
		sequenceBytes(33, 32),
		sequenceBytes(65, chacha20poly1305.NonceSizeX),
		sealedSecretAAD{Cluster: "cluster-uid", Namespace: "app", Name: "database"},
		"",
	)
	resource := &oramav1alpha1.LakeSealedSecret{
		ObjectMeta: metav1.ObjectMeta{Name: "database", Namespace: "app"},
		Spec: oramav1alpha1.LakeSealedSecretSpec{
			LakeRef:  corev1.LocalObjectReference{Name: "prod"},
			Template: oramav1alpha1.LakeSealedSecretTemplate{Name: "database"},
			Data:     map[string]string{"password": record},
			Source:   oramav1alpha1.LakeSealedSecretSource{Type: "literal"},
		},
	}
	if err := client.Create(context.Background(), resource); err != nil {
		t.Fatal(err)
	}
	if err := client.Get(context.Background(), clientKey("prod", ""), lake); err != nil {
		t.Fatal(err)
	}
	lake.Spec.Sealing.Generation = 2
	if err := client.Update(context.Background(), lake); err != nil {
		t.Fatal(err)
	}
	second, err := reconciler.reconcileLakeSealingKey(context.Background(), lake, "lake-dev", map[string]string{"app": "lake"})
	if err != nil {
		t.Fatal(err)
	}
	if second.KID == first.KID {
		t.Fatal("rotation did not change the kid")
	}
	if len(second.Status.PreviousKeys) != 1 || second.Status.PreviousKeys[0].KID != first.KID || second.Status.PreviousKeys[0].Remaining != 1 {
		t.Fatalf("rotation status = %+v", second.Status)
	}
	keySecret := &corev1.Secret{}
	if err := client.Get(context.Background(), clientKey(lakeSealingKeySecret("prod"), "lake-dev"), keySecret); err != nil {
		t.Fatal(err)
	}
	if len(keySecret.Data[sealingPrivatePrefix+first.KID]) != 32 {
		t.Fatal("old private key was removed while a record still uses it")
	}
	newRecord, _ := referenceSeal(
		t,
		[]byte("new"),
		second.PrivateKey,
		sequenceBytes(97, 32),
		sequenceBytes(129, chacha20poly1305.NonceSizeX),
		sealedSecretAAD{Cluster: "cluster-uid", Namespace: "app", Name: "database"},
		"",
	)
	if err := client.Get(context.Background(), clientKey("database", "app"), resource); err != nil {
		t.Fatal(err)
	}
	resource.Spec.Data["password"] = newRecord
	if err := client.Update(context.Background(), resource); err != nil {
		t.Fatal(err)
	}
	third, err := reconciler.reconcileLakeSealingKey(context.Background(), lake, "lake-dev", map[string]string{"app": "lake"})
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Status.PreviousKeys) != 0 {
		t.Fatalf("old key still reported after re-seal: %+v", third.Status.PreviousKeys)
	}
	if err := client.Get(context.Background(), clientKey(lakeSealingKeySecret("prod"), "lake-dev"), keySecret); err != nil {
		t.Fatal(err)
	}
	if _, found := keySecret.Data[sealingPrivatePrefix+first.KID]; found {
		t.Fatal("old private key was not removed after every record was re-sealed")
	}
}

func TestLakeSourceMintsSealedValue(t *testing.T) {
	scheme := newSealedSecretScheme(t)
	lake, resource, keySecret := remoteSealedSecretObjects(t, "lake")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		mint := decodeMintRequest(t, request)
		if mint.Attestation.Token != "projected-token" || mint.Source.Lake == nil || mint.Source.Lake.Mount != "production" {
			t.Errorf("mint request = %+v", mint)
		}
		record := referenceSealForMintRequest(t, mint, []byte("lake-value"), byte(calls))
		_ = json.NewEncoder(writer).Encode(lakeMintResponse{OK: true, Value: lakeMintValue{Data: map[string]string{"token": record}}})
	}))
	defer server.Close()
	client := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(lake, resource, keySecret).
		WithStatusSubresource(lake, resource).
		Build()
	reconciler := &LakeSealedSecretReconciler{
		Client:              client,
		Scheme:              scheme,
		LakeURL:             server.URL,
		ServiceAccountToken: "projected-token",
	}
	reconcileSealedSecret(t, client, scheme, resource.Name, reconciler)
	produced := &corev1.Secret{}
	if err := client.Get(context.Background(), clientKey("database", "app"), produced); err != nil {
		t.Fatal(err)
	}
	if string(produced.Data["token"]) != "lake-value" || calls != 1 {
		t.Fatalf("data=%q calls=%d", produced.Data["token"], calls)
	}
	reconcileSealedSecret(t, client, scheme, resource.Name, reconciler)
	if calls != 1 {
		t.Fatalf("unchanged lake source minted %d times", calls)
	}
	produced.Labels[sealedSecretLabel] = "false"
	if err := client.Update(context.Background(), produced); err != nil {
		t.Fatal(err)
	}
	reconcileSealedSecret(t, client, scheme, resource.Name, reconciler)
	if calls != 2 {
		t.Fatalf("drifted lake Secret was not reminted: calls=%d", calls)
	}
}

func TestDynamicSourceRenewsBeforeTTL(t *testing.T) {
	scheme := newSealedSecretScheme(t)
	lake, resource, keySecret := remoteSealedSecretObjects(t, "dynamic")
	current := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		mint := decodeMintRequest(t, request)
		record := referenceSealForMintRequest(t, mint, []byte("dynamic-value"), byte(calls))
		lease := &lakeMintLease{
			ID:        "lease",
			ExpiresAt: current.Add(time.Hour),
		}
		_ = json.NewEncoder(writer).Encode(lakeMintResponse{OK: true, Value: lakeMintValue{
			Data:  map[string]string{"password": record},
			Lease: lease,
		}})
	}))
	defer server.Close()
	client := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(lake, resource, keySecret).
		WithStatusSubresource(lake, resource).
		Build()
	reconciler := &LakeSealedSecretReconciler{
		Client:              client,
		Scheme:              scheme,
		LakeURL:             server.URL,
		ServiceAccountToken: "projected-token",
		Now: func() time.Time {
			return current
		},
	}
	result := reconcileSealedSecret(t, client, scheme, resource.Name, reconciler)
	if calls != 1 || result.RequeueAfter != 40*time.Minute {
		t.Fatalf("calls=%d requeue=%s", calls, result.RequeueAfter)
	}
	current = current.Add(39 * time.Minute)
	reconcileSealedSecret(t, client, scheme, resource.Name, reconciler)
	if calls != 1 {
		t.Fatalf("lease renewed too early: calls=%d", calls)
	}
	current = current.Add(2 * time.Minute)
	reconcileSealedSecret(t, client, scheme, resource.Name, reconciler)
	if calls != 2 {
		t.Fatalf("lease was not renewed before expiry: calls=%d", calls)
	}
}

func newSealedSecretScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, oramav1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func sealedSecretObjects(
	kid string,
	publicKey []byte,
	privateKey []byte,
) (*oramav1alpha1.Lake, *oramav1alpha1.LakeSealedSecret, *corev1.Secret) {
	controller := true
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: types.UID("cluster-uid")},
		Spec:       oramav1alpha1.LakeSpec{Namespace: "lake", Image: "oramasearch/lake:test"},
		Status: oramav1alpha1.LakeStatus{Sealing: &oramav1alpha1.LakeSealingStatus{
			Cluster: "cluster-uid", Generation: 1, KID: kid,
			PublicKey: base64.RawURLEncoding.EncodeToString(publicKey),
		}},
	}
	resource := &oramav1alpha1.LakeSealedSecret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "database-sealed",
			Namespace: "app",
			UID:       types.UID("sealed-uid"),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "orama.cloud/v1alpha1",
				Kind:       "Lake",
				Name:       "prod",
				UID:        lake.UID,
				Controller: &controller,
			}},
		},
		Spec: oramav1alpha1.LakeSealedSecretSpec{
			LakeRef:  corev1.LocalObjectReference{Name: "prod"},
			Template: oramav1alpha1.LakeSealedSecretTemplate{Name: "database"},
		},
	}
	keySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: lakeSealingKeySecret("prod"), Namespace: "lake"},
		Data: map[string][]byte{
			sealingGenerationKey:       []byte("1"),
			sealingActiveKIDKey:        []byte(kid),
			sealingPrivatePrefix + kid: privateKey,
			sealingPublicPrefix + kid:  publicKey,
		},
	}
	return lake, resource, keySecret
}

func remoteSealedSecretObjects(
	t *testing.T,
	typ string,
) (*oramav1alpha1.Lake, *oramav1alpha1.LakeSealedSecret, *corev1.Secret) {
	t.Helper()
	privateKey := sequenceBytes(1, 32)
	private, err := ecdh.X25519().NewPrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := private.PublicKey().Bytes()
	lake, resource, keySecret := sealedSecretObjects(referenceKID(publicKey), publicKey, privateKey)
	if typ == "lake" {
		resource.Spec.Source = oramav1alpha1.LakeSealedSecretSource{
			Type: "lake",
			Lake: &oramav1alpha1.LakeStaticSecretSource{Mount: "production", Name: "database"},
		}
	} else {
		resource.Spec.Source = oramav1alpha1.LakeSealedSecretSource{
			Type: "dynamic",
			Dynamic: &oramav1alpha1.LakeDynamicSecretSource{
				Engine: "postgres", Role: "reader", TTL: metav1.Duration{Duration: time.Hour},
			},
		}
	}
	return lake, resource, keySecret
}

func reconcileSealedSecret(
	t *testing.T,
	kubernetesClient client.Client,
	scheme *runtime.Scheme,
	name string,
	reconciler *LakeSealedSecretReconciler,
) ctrl.Result {
	t.Helper()
	if reconciler == nil {
		reconciler = &LakeSealedSecretReconciler{Client: kubernetesClient, Scheme: scheme}
	}
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "app", Name: name},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return result
}

func decodeMintRequest(t *testing.T, request *http.Request) lakeMintRequest {
	t.Helper()
	if request.Method != http.MethodPost || request.URL.Path != lakeMintSealedSecretRoute {
		t.Fatalf("request = %s %s", request.Method, request.URL.Path)
	}
	mint := lakeMintRequest{}
	if err := json.NewDecoder(request.Body).Decode(&mint); err != nil {
		t.Fatal(err)
	}
	return mint
}

func referenceSealForMintRequest(
	t *testing.T,
	mint lakeMintRequest,
	plaintext []byte,
	seed byte,
) string {
	t.Helper()
	publicKey, err := base64.RawURLEncoding.DecodeString(mint.Recipient.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	record, _ := referenceSealToPublic(
		t,
		plaintext,
		publicKey,
		sequenceBytes(seed+32, 32),
		sequenceBytes(seed+64, chacha20poly1305.NonceSizeX),
		sealedSecretAAD{
			Cluster:   mint.Recipient.Cluster,
			Namespace: mint.Recipient.Namespace,
			Name:      mint.Recipient.Name,
		},
		mint.Recipient.KID,
	)
	return record
}

func referenceSeal(
	t *testing.T,
	plaintext []byte,
	recipientPrivateBytes []byte,
	ephemeralPrivateBytes []byte,
	nonce []byte,
	aad sealedSecretAAD,
	kidOverride string,
) (string, []byte) {
	t.Helper()
	recipientPrivate, err := ecdh.X25519().NewPrivateKey(recipientPrivateBytes)
	if err != nil {
		t.Fatal(err)
	}
	recipientPublic := recipientPrivate.PublicKey().Bytes()
	record, publicKey := referenceSealToPublic(
		t,
		plaintext,
		recipientPublic,
		ephemeralPrivateBytes,
		nonce,
		aad,
		kidOverride,
	)
	return record, publicKey
}

func referenceSealToPublic(
	t *testing.T,
	plaintext []byte,
	recipientPublicBytes []byte,
	ephemeralPrivateBytes []byte,
	nonce []byte,
	aad sealedSecretAAD,
	kidOverride string,
) (string, []byte) {
	t.Helper()
	curve := ecdh.X25519()
	recipientPublic, err := curve.NewPublicKey(recipientPublicBytes)
	if err != nil {
		t.Fatal(err)
	}
	ephemeralPrivate, err := curve.NewPrivateKey(ephemeralPrivateBytes)
	if err != nil {
		t.Fatal(err)
	}
	ephemeralPublic := ephemeralPrivate.PublicKey().Bytes()
	shared, err := ephemeralPrivate.ECDH(recipientPublic)
	if err != nil {
		t.Fatal(err)
	}
	key := referenceDeriveKey(shared, ephemeralPublic, recipientPublicBytes)
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		t.Fatal(err)
	}
	mode, err := cbor.CanonicalEncOptions().EncMode()
	if err != nil {
		t.Fatal(err)
	}
	aadMap := map[string]string{
		"cluster":   aad.Cluster,
		"name":      aad.Name,
		"namespace": aad.Namespace,
	}
	aadBytes, err := mode.Marshal(aadMap)
	if err != nil {
		t.Fatal(err)
	}
	kid := referenceKID(recipientPublicBytes)
	if kidOverride != "" {
		kid = kidOverride
	}
	envelope := map[string]interface{}{
		"v":     uint64(1),
		"kid":   kid,
		"epk":   ephemeralPublic,
		"nonce": nonce,
		"aad":   aadMap,
		"ct":    aead.Seal(nil, nonce, plaintext, aadBytes),
	}
	raw, err := mode.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw), recipientPublicBytes
}

func referenceDeriveKey(shared []byte, ephemeralPublic []byte, recipientPublic []byte) []byte {
	extract := hmac.New(sha256.New, make([]byte, sha256.Size))
	_, _ = extract.Write(shared)
	prk := extract.Sum(nil)
	expand := hmac.New(sha256.New, prk)
	_, _ = expand.Write([]byte(sealingKDFInfo))
	_, _ = expand.Write(ephemeralPublic)
	_, _ = expand.Write(recipientPublic)
	_, _ = expand.Write([]byte{1})
	return expand.Sum(nil)
}

func referenceKID(publicKey []byte) string {
	digest := sha256.Sum256(publicKey)
	return base64.RawURLEncoding.EncodeToString(digest[:16])
}

func sequenceBytes(start byte, length int) []byte {
	value := make([]byte, length)
	for index := range value {
		value[index] = start + byte(index)
	}
	return value
}

func clientKey(name string, namespace string) client.ObjectKey {
	return client.ObjectKey{Name: name, Namespace: namespace}
}

func conditionReason(conditions []metav1.Condition, typ string) string {
	for _, condition := range conditions {
		if condition.Type == typ {
			return condition.Reason
		}
	}
	return ""
}

/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

// Regression tests for the isolation rules a security review of sealed secret delivery relied
// on. Each test
// names the rule it holds: break the rule and the test fails.

import (
	"context"
	"crypto/ecdh"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const isolationPlaintext = "isolation-plaintext-7f3a"

// literalIsolationObjects returns a Lake, its key Secret and a literal LakeSealedSecret in
// namespace whose one record is sealed to the Lake's key with the given AAD.
func literalIsolationObjects(
	t *testing.T,
	namespace string,
	templateName string,
	aad sealedSecretAAD,
) (*oramav1alpha1.Lake, *oramav1alpha1.LakeSealedSecret, *corev1.Secret) {
	t.Helper()
	privateKey := sequenceBytes(1, 32)
	private, err := ecdh.X25519().NewPrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	publicKey := private.PublicKey().Bytes()
	lake, resource, keySecret := sealedSecretObjects(referenceKID(publicKey), publicKey, privateKey)
	record, _ := referenceSeal(
		t,
		[]byte(isolationPlaintext),
		privateKey,
		sequenceBytes(33, 32),
		sequenceBytes(65, chacha20poly1305.NonceSizeX),
		aad,
		"",
	)
	resource.Namespace = namespace
	resource.Spec.Template.Name = templateName
	resource.Spec.Source = oramav1alpha1.LakeSealedSecretSource{Type: "literal"}
	resource.Spec.Data = map[string]string{"password": record}
	return lake, resource, keySecret
}

func reconcileSealedSecretIn(
	t *testing.T,
	reconciler *LakeSealedSecretReconciler,
	namespace string,
	name string,
) (ctrl.Result, error) {
	t.Helper()
	return reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: namespace, Name: name},
	})
}

func assertNoSecret(t *testing.T, kubernetesClient client.Client, namespace string, name string) {
	t.Helper()
	err := kubernetesClient.Get(context.Background(), clientKey(name, namespace), &corev1.Secret{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Secret %s/%s exists or could not be read: %v", namespace, name, err)
	}
}

// assertNoPlaintextIn fails when the plaintext appears anywhere in the object as stored.
func assertNoPlaintextIn(t *testing.T, object any) {
	t.Helper()
	encoded, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), isolationPlaintext) {
		t.Fatalf("the plaintext appears in %T: %s", object, encoded)
	}
}

// Rule: a record sealed for one namespace, cluster or Secret name never opens anywhere else, so
// ciphertext copied out of one workspace's resource cannot be replayed to produce a Secret in
// another, and the refusal writes no Secret anywhere.
func TestSealedSecretRecordIsRefusedOutsideItsBinding(t *testing.T) {
	for _, test := range []struct {
		name      string
		namespace string
		template  string
		aad       sealedSecretAAD
	}{
		{
			name:      "another namespace",
			namespace: "other",
			template:  "database",
			aad:       sealedSecretAAD{Cluster: "cluster-uid", Namespace: "app", Name: "database"},
		},
		{
			name:      "another cluster",
			namespace: "app",
			template:  "database",
			aad:       sealedSecretAAD{Cluster: "cluster-b", Namespace: "app", Name: "database"},
		},
		{
			name:      "another Secret name",
			namespace: "app",
			template:  "payroll",
			aad:       sealedSecretAAD{Cluster: "cluster-uid", Namespace: "app", Name: "database"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			scheme := newSealedSecretScheme(t)
			lake, resource, keySecret := literalIsolationObjects(t, test.namespace, test.template, test.aad)
			kubernetesClient := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(lake, resource, keySecret, lakeAdminTokenSecret()).
				WithStatusSubresource(lake, resource).
				Build()
			reconciler := &LakeSealedSecretReconciler{Client: kubernetesClient, Scheme: scheme}
			if _, err := reconcileSealedSecretIn(t, reconciler, test.namespace, resource.Name); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			stored := &oramav1alpha1.LakeSealedSecret{}
			if err := kubernetesClient.Get(context.Background(), clientKey(resource.Name, test.namespace), stored); err != nil {
				t.Fatal(err)
			}
			if stored.Status.Phase != "Error" || conditionReason(stored.Status.Conditions, "Ready") != "UnsealFailed" {
				t.Fatalf("status = %+v, want an UnsealFailed error", stored.Status)
			}
			assertNoSecret(t, kubernetesClient, test.namespace, test.template)
			assertNoSecret(t, kubernetesClient, test.aad.Namespace, test.aad.Name)
			assertNoPlaintextIn(t, stored)
		})
	}
}

// Rule: the produced Secret is written only in the resource's own namespace, and a Secret of the
// same name that this resource does not control is never overwritten.
func TestLakeSealedSecretNeverOverwritesASecretItDoesNotControl(t *testing.T) {
	scheme := newSealedSecretScheme(t)
	aad := sealedSecretAAD{Cluster: "cluster-uid", Namespace: "app", Name: "database"}
	lake, resource, keySecret := literalIsolationObjects(t, "app", "database", aad)
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "database", Namespace: "app"},
		Data:       map[string][]byte{"password": []byte("foreign-value")},
	}
	kubernetesClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(lake, resource, keySecret, foreign, lakeAdminTokenSecret()).
		WithStatusSubresource(lake, resource).
		Build()
	reconciler := &LakeSealedSecretReconciler{Client: kubernetesClient, Scheme: scheme}
	if _, err := reconcileSealedSecretIn(t, reconciler, "app", resource.Name); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	stored := &oramav1alpha1.LakeSealedSecret{}
	if err := kubernetesClient.Get(context.Background(), clientKey(resource.Name, "app"), stored); err != nil {
		t.Fatal(err)
	}
	if conditionReason(stored.Status.Conditions, "Ready") != "SecretOwnershipConflict" {
		t.Fatalf("status = %+v, want SecretOwnershipConflict", stored.Status)
	}
	kept := &corev1.Secret{}
	if err := kubernetesClient.Get(context.Background(), clientKey("database", "app"), kept); err != nil {
		t.Fatal(err)
	}
	if string(kept.Data["password"]) != "foreign-value" || len(kept.OwnerReferences) != 0 {
		t.Fatalf("the foreign Secret was changed: data=%q owners=%+v", kept.Data["password"], kept.OwnerReferences)
	}
}

// Rule: the operator asks lake to seal only to the resource's own namespace and Secret name and
// to this Lake's cluster key, and refuses an answer lake sealed for any other namespace, so even a
// misbehaving lake cannot have the operator write a value meant for one namespace into another.
func TestLakeSourceSealsOnlyToItsOwnNamespace(t *testing.T) {
	scheme := newSealedSecretScheme(t)
	lake, resource, keySecret := remoteSealedSecretObjects(t, "lake")
	var recipients []lakeMintRecipient
	sealFor := "app"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mint := decodeMintRequest(t, request)
		recipients = append(recipients, mint.Recipient)
		mint.Recipient.Namespace = sealFor
		record := referenceSealForMintRequest(t, mint, []byte(isolationPlaintext), byte(len(recipients)))
		_ = json.NewEncoder(writer).Encode(lakeMintResponse{OK: true, Value: lakeMintValue{Data: map[string]string{"token": record}}})
	}))
	defer server.Close()
	kubernetesClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(lake, resource, keySecret, lakeAdminTokenSecret()).
		WithStatusSubresource(lake, resource).
		Build()
	reconciler := &LakeSealedSecretReconciler{
		Client:  kubernetesClient,
		Scheme:  scheme,
		LakeURL: server.URL,
	}
	if _, err := reconcileSealedSecretIn(t, reconciler, "app", resource.Name); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(recipients) != 1 {
		t.Fatalf("mint calls = %d", len(recipients))
	}
	want := lakeMintRecipient{
		Cluster:   lake.Status.Sealing.Cluster,
		Namespace: "app",
		Name:      "database",
		KID:       lake.Status.Sealing.KID,
		PublicKey: lake.Status.Sealing.PublicKey,
	}
	if recipients[0] != want {
		t.Fatalf("mint recipient = %+v, want %+v", recipients[0], want)
	}
	produced := &corev1.Secret{}
	if err := kubernetesClient.Get(context.Background(), clientKey("database", "app"), produced); err != nil {
		t.Fatal(err)
	}
	assertNoPlaintextIn(t, produced.Annotations)

	// A lake that seals the next delivery for another namespace is refused, and nothing is written
	// in that namespace.
	sealFor = "other"
	stored := &oramav1alpha1.LakeSealedSecret{}
	if err := kubernetesClient.Get(context.Background(), clientKey(resource.Name, "app"), stored); err != nil {
		t.Fatal(err)
	}
	stored.Spec.Source.Lake.Name = "rotated"
	stored.Generation++
	if err := kubernetesClient.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileSealedSecretIn(t, reconciler, "app", resource.Name); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := kubernetesClient.Get(context.Background(), clientKey(resource.Name, "app"), stored); err != nil {
		t.Fatal(err)
	}
	if conditionReason(stored.Status.Conditions, "Ready") != "UnsealFailed" {
		t.Fatalf("status = %+v, want UnsealFailed for a record sealed to another namespace", stored.Status)
	}
	assertNoSecret(t, kubernetesClient, "other", "database")
	assertNoPlaintextIn(t, stored)
}

// Rule: the operator never writes a plaintext outside the produced Secret's data: not in the
// resource's status, not in a condition message, not in the error lake's refusal carries back.
func TestLakeSealedSecretStatusNeverCarriesThePlaintext(t *testing.T) {
	scheme := newSealedSecretScheme(t)
	lake, resource, keySecret := remoteSealedSecretObjects(t, "lake")
	refuse := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mint := decodeMintRequest(t, request)
		if refuse {
			_ = json.NewEncoder(writer).Encode(lakeMintResponse{OK: false, Error: &lakeAPIError{Code: "not_found", Message: "no such secret"}})
			return
		}
		record := referenceSealForMintRequest(t, mint, []byte(isolationPlaintext), 1)
		_ = json.NewEncoder(writer).Encode(lakeMintResponse{OK: true, Value: lakeMintValue{Data: map[string]string{"token": record}}})
	}))
	defer server.Close()
	kubernetesClient := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(lake, resource, keySecret, lakeAdminTokenSecret()).
		WithStatusSubresource(lake, resource).
		Build()
	reconciler := &LakeSealedSecretReconciler{
		Client:  kubernetesClient,
		Scheme:  scheme,
		LakeURL: server.URL,
	}
	if _, err := reconcileSealedSecretIn(t, reconciler, "app", resource.Name); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	stored := &oramav1alpha1.LakeSealedSecret{}
	if err := kubernetesClient.Get(context.Background(), clientKey(resource.Name, "app"), stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.Phase != "Ready" {
		t.Fatalf("status = %+v", stored.Status)
	}
	assertNoPlaintextIn(t, stored)

	refuse = true
	stored.Spec.Source.Lake.Name = "gone"
	stored.Generation++
	if err := kubernetesClient.Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}
	_, err := reconcileSealedSecretIn(t, reconciler, "app", resource.Name)
	if err == nil {
		t.Fatal("a refused mint did not return an error")
	}
	if strings.Contains(err.Error(), isolationPlaintext) {
		t.Fatalf("the reconcile error carries the plaintext: %v", err)
	}
	if err := kubernetesClient.Get(context.Background(), clientKey(resource.Name, "app"), stored); err != nil {
		t.Fatal(err)
	}
	if conditionReason(stored.Status.Conditions, "Ready") != "MintFailed" {
		t.Fatalf("status = %+v, want MintFailed", stored.Status)
	}
	assertNoPlaintextIn(t, stored)
}

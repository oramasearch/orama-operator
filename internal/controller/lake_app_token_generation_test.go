/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Lake raises apps.token_generation to rotate an app's tokens (lake#1070). The operator mints
// every token again, registers the new hashes with token_hashes_generation set to that generation,
// and acknowledges the generation of an existing app whose hashes it already minted.
func TestLakeAppTokenGenerationRotatesAndAcknowledges(t *testing.T) {
	scheme := newLakeAppScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&oramav1alpha1.LakeApp{}).Build()
	h := newAppsSyncHarness(t, c)
	h.converge()
	row := &h.store.rows[0]
	if row.TokenHashes == nil {
		t.Fatal("the app's first tokens were never registered")
	}
	first := row.TokenHashes.DeepCopy()
	tokens := func() *corev1.Secret {
		t.Helper()
		app := h.app()
		var secret corev1.Secret
		if err := c.Get(h.ctx, client.ObjectKey{Namespace: lakeAppNamespace(app), Name: lakeAppTokensName(app)}, &secret); err != nil {
			t.Fatal(err)
		}
		return &secret
	}

	// Lake's schema gains 0112: the existing app's hashes are acknowledged at generation 0, and
	// its tokens are kept.
	row.TokenGenerationKnown = true
	h.converge()
	if row.TokenHashesGeneration == nil || *row.TokenHashesGeneration != 0 {
		t.Fatalf("token_hashes_generation = %v, want 0 acknowledged", row.TokenHashesGeneration)
	}
	if !reflect.DeepEqual(row.TokenHashes, first) {
		t.Fatal("acknowledging the current generation rotated the tokens")
	}

	// Lake asks for a rotation.
	row.TokenGeneration = 1
	h.converge()
	if row.TokenHashesGeneration == nil || *row.TokenHashesGeneration != 1 {
		t.Fatalf("token_hashes_generation = %v, want 1", row.TokenHashesGeneration)
	}
	if row.TokenHashes == nil || row.TokenHashes.Source == first.Source || row.TokenHashes.Events == first.Events || row.TokenHashes.Pull == first.Pull {
		t.Fatalf("the tokens were not all minted again: %+v", row.TokenHashes)
	}
	secret := tokens()
	if lakeAppTokensGeneration(secret) != 1 || !reflect.DeepEqual(lakeAppTokenHashes(secret), row.TokenHashes) {
		t.Fatal("the Secret does not hold the registered generation's tokens")
	}
	if h.app().Spec.Sync.TokenGeneration != 1 {
		t.Fatal("the LakeApp does not carry the generation Lake asked for")
	}
}

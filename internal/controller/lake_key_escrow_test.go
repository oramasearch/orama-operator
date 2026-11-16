/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	"github.com/oramasearch/orama-operator/internal/backup"
	"github.com/oramasearch/orama-operator/internal/escrow"
)

type recordingPut struct{ objects map[string][]byte }

func (p *recordingPut) Put(_ context.Context, key string, body []byte) error {
	p.objects[key] = body
	return nil
}

// The instance key is sealed to the recovery keys and written once; a recovery identity opens it
// and finds the key the fingerprint names; nothing is written again while nothing changed.
func TestLakeInstanceKeyIsEscrowedToTheRecoveryKeys(t *testing.T) {
	ctx := context.Background()
	identity, recipient, err := escrow.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	lake := backupsTestLake()
	lake.Spec.Backups.EscrowRecipients = []string{recipient}
	bucket := &oramav1alpha1.CloudflareR2Bucket{
		ObjectMeta: metav1.ObjectMeta{Name: "lake-prod-backups", Namespace: "lake"},
		Spec:       oramav1alpha1.CloudflareR2BucketSpec{Credentials: &oramav1alpha1.CloudflareR2BucketCredentials{SecretName: "lake-prod-backups-s3"}},
		Status:     oramav1alpha1.CloudflareR2BucketStatus{Ready: true, CredentialsSecret: "lake-prod-backups-s3"},
	}
	credentials := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "lake-prod-backups-s3", Namespace: "lake"}, Data: map[string][]byte{
		"accessKeyId": []byte("k"), "secretAccessKey": []byte("s"), "endpoint": []byte("https://acct.r2.cloudflarestorage.com"), "bucket": []byte("lake-prod-backups"),
	}}
	kek := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: lakeInstanceKekSecret("prod"), Namespace: "lake"}, Data: map[string][]byte{"key": []byte("0123abcd")}}
	scheme := backupTestScheme(t)
	r := &LakeReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(bucket, credentials, kek).Build(), Scheme: scheme}
	put := &recordingPut{objects: map[string][]byte{}}
	previous := lakeKeyEscrowObjectStore
	lakeKeyEscrowObjectStore = func(backup.Bucket) interface {
		Put(ctx context.Context, key string, body []byte) error
	} {
		return put
	}
	t.Cleanup(func() { lakeKeyEscrowObjectStore = previous })

	status, condition := r.reconcileLakeKeyEscrow(ctx, lake, "lake")
	if condition.Status != metav1.ConditionTrue || status == nil || len(put.objects) != 1 {
		t.Fatalf("condition %+v status %+v objects %d", condition, status, len(put.objects))
	}
	var sealed []byte
	for _, body := range put.objects {
		sealed = body
	}
	opened, err := escrow.Decrypt(identity, sealed)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Data        map[string][]byte `json:"data"`
		Fingerprint string            `json:"fingerprint"`
	}
	if err := json.Unmarshal(opened, &document); err != nil || string(document.Data["key"]) != "0123abcd" || document.Fingerprint != status.Fingerprint {
		t.Fatalf("opened %+v, %v", document, err)
	}

	lake.Status.KeyEscrow = status
	put.objects = map[string][]byte{}
	if _, condition := r.reconcileLakeKeyEscrow(ctx, lake, "lake"); condition.Status != metav1.ConditionTrue || len(put.objects) != 0 {
		t.Fatalf("a steady Lake wrote %d objects (%+v)", len(put.objects), condition)
	}
	lake.Spec.Backups.EscrowRecipients = nil
	if _, condition := r.reconcileLakeKeyEscrow(ctx, lake, "lake"); condition.Reason != "NotConfigured" {
		t.Fatalf("without recipients: %+v", condition)
	}
}

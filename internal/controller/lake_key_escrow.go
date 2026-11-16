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
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	"github.com/oramasearch/orama-operator/internal/backup"
	"github.com/oramasearch/orama-operator/internal/escrow"
)

// Escrow of the Lake's instance key (operator#739, docs/lake/key-escrow.md). The Secret
// lake-<name>-instance-kek wraps every sealed workspace secret; losing it loses them all, and it is
// not a datastore a backup can restore. So the operator seals the Secret's data, in the age format,
// to the recovery keys in spec.backups.escrowRecipients (public keys; their private halves are held
// off the cluster by people) and writes the sealed copy to the Lake's Retain backup bucket as
// lake-<name>/instance-kek/<fingerprint>.age. It writes again only when the key or the recipients
// change, so a steady Lake makes no request. The operator never holds a recovery key and cannot open
// what it wrote; a rehearsal is a person opening it with `age -d` and comparing the fingerprint in the
// Lake's status (the runbook).
//
// Decided under Steve's 2026-11-14 delegation: the sealed copy may live in R2, in the backup bucket,
// because it is encrypted to keys that never touch the cluster or R2. Who holds the recovery keys,
// and where, is left to Steve (the runbook's banner).

const lakeKeyEscrowCondition = "InstanceKeyEscrowed"

// lakeKeyEscrowObjectStore opens the backup bucket; injected for tests.
var lakeKeyEscrowObjectStore = func(bucket backup.Bucket) interface {
	Put(ctx context.Context, key string, body []byte) error
} {
	return &backup.S3{Bucket: bucket}
}

func lakeKeyFingerprint(data map[string][]byte) string {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	hash := sha256.New()
	for _, key := range keys {
		fmt.Fprintf(hash, "%s\x00%x\x00", key, data[key])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func recipientsDigest(recipients []string) string {
	sorted := append([]string(nil), recipients...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	return hex.EncodeToString(sum[:])
}

// reconcileLakeKeyEscrow seals the instance key when it or its recipients changed, and returns the
// status and condition to record.
func (r *LakeReconciler) reconcileLakeKeyEscrow(ctx context.Context, lake *oramav1alpha1.Lake, ns string) (*oramav1alpha1.LakeKeyEscrowStatus, metav1.Condition) {
	condition := metav1.Condition{Type: lakeKeyEscrowCondition, Status: metav1.ConditionFalse, ObservedGeneration: lake.Generation}
	previous := lake.Status.KeyEscrow
	if lake.Spec.Backups == nil || len(lake.Spec.Backups.EscrowRecipients) == 0 {
		condition.Reason, condition.Message = "NotConfigured", "spec.backups.escrowRecipients is empty: the instance key has no copy off the cluster (docs/lake/key-escrow.md)"
		return previous, condition
	}
	var key corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: lakeInstanceKekSecret(lake.Name)}, &key); err != nil || len(key.Data["key"]) == 0 {
		condition.Reason, condition.Message = "KeyNotFound", "the instance key Secret does not exist yet"
		return previous, condition
	}
	fingerprint, digest := lakeKeyFingerprint(key.Data), recipientsDigest(lake.Spec.Backups.EscrowRecipients)
	if previous != nil && previous.Fingerprint == fingerprint && previous.RecipientsDigest == digest {
		condition.Status, condition.Reason = metav1.ConditionTrue, "Escrowed"
		condition.Message = fmt.Sprintf("sealed to %d recovery keys at %s", len(lake.Spec.Backups.EscrowRecipients), previous.Object)
		return previous, condition
	}
	bucketData, reason, message, err := resolveBackupBucket(ctx, r.Client, ns, oramav1alpha1.BackupBucketRef{Kind: "CloudflareR2Bucket", Name: lakeBackupBucketName(lake.Name)})
	if err != nil || reason != "" {
		if err != nil {
			message = err.Error()
		}
		condition.Reason, condition.Message = "BucketNotReady", message
		return previous, condition
	}
	bucket, err := backup.BucketFromSecret(bucketData.Data)
	if err != nil {
		condition.Reason, condition.Message = "BucketNotReady", err.Error()
		return previous, condition
	}
	plaintext, err := json.Marshal(map[string]interface{}{
		"lake": lake.Name, "namespace": ns, "secret": key.Name, "data": key.Data,
		"fingerprint": fingerprint, "sealedAt": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		condition.Reason, condition.Message = "SealFailed", err.Error()
		return previous, condition
	}
	sealed, err := escrow.Encrypt(lake.Spec.Backups.EscrowRecipients, plaintext)
	if err != nil {
		condition.Reason, condition.Message = "SealFailed", err.Error()
		return previous, condition
	}
	object := fmt.Sprintf("lake-%s/instance-kek/%s.age", lake.Name, fingerprint[:16])
	if err := lakeKeyEscrowObjectStore(bucket).Put(ctx, object, sealed); err != nil {
		condition.Reason = "WriteFailed"
		condition.Message = "the sealed copy could not be written: " + strings.ReplaceAll(err.Error(), bucket.SecretAccessKey, "[redacted]")
		return previous, condition
	}
	status := &oramav1alpha1.LakeKeyEscrowStatus{
		Object:           fmt.Sprintf("s3://%s/%s", bucket.Name, object),
		Fingerprint:      fingerprint,
		RecipientsDigest: digest,
		EscrowedAt:       metav1.NewTime(time.Now().UTC().Truncate(time.Second)),
	}
	condition.Status, condition.Reason = metav1.ConditionTrue, "Escrowed"
	condition.Message = fmt.Sprintf("sealed to %d recovery keys at %s", len(lake.Spec.Backups.EscrowRecipients), status.Object)
	return status, condition
}

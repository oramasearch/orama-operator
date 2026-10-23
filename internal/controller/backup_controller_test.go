/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	"github.com/oramasearch/orama-operator/internal/backup"
)

const testOperatorImage = "oramasearch/orama-operator:dev-abc123"

func backupTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{oramav1alpha1.AddToScheme, corev1.AddToScheme, batchv1.AddToScheme, networkingv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

// A ready backup bucket in namespace "backups", usable from "ws-1", with its scoped credentials.
func readyBackupBucket() []client.Object {
	return []client.Object{
		&oramav1alpha1.CloudflareR2Bucket{
			ObjectMeta: metav1.ObjectMeta{Name: "lake-backups", Namespace: "backups"},
			Spec: oramav1alpha1.CloudflareR2BucketSpec{
				SecretRef: "cloudflare", Name: "lake-backups", DeletionPolicy: oramav1alpha1.BucketDeletionRetain,
				Credentials: &oramav1alpha1.CloudflareR2BucketCredentials{SecretName: "lake-backups-s3", AllowedNamespaces: []string{"ws-1"}},
			},
			Status: oramav1alpha1.CloudflareR2BucketStatus{Created: true, Ready: true, CredentialsSecret: "lake-backups-s3", CredentialsTokenID: "tok-1"},
		},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "lake-backups-s3", Namespace: "backups"}, Data: map[string][]byte{
			"accessKeyId": []byte("tok-1"), "secretAccessKey": []byte("hashed"), "endpoint": []byte("https://acct.r2.cloudflarestorage.com"),
			"bucket": []byte("lake-backups"), "region": []byte("auto"),
		}},
		// The account token: it must never be copied anywhere.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cloudflare", Namespace: "backups"}, Data: map[string][]byte{"token": []byte("account-token"), "accountID": []byte("acct")}},
	}
}

func testBackup(kind oramav1alpha1.BackupDatastoreKind) *oramav1alpha1.Backup {
	return &oramav1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "events-20261021t030000z", Namespace: "ws-1"},
		Spec: oramav1alpha1.BackupSpec{
			Datastore: oramav1alpha1.BackupDatastoreRef{Kind: kind, Name: "events", ConnectionSecretRef: oramav1alpha1.LocalSecretReference{Name: "events-connection"}},
			Bucket:    oramav1alpha1.BackupBucketRef{Kind: "CloudflareR2Bucket", Name: "lake-backups", Namespace: "backups"},
		},
	}
}

type fakeObjectStore struct{ deleted []string }

func (f *fakeObjectStore) DeletePrefix(_ context.Context, prefix string) (int, error) {
	f.deleted = append(f.deleted, prefix)
	return 3, nil
}

func newBackupReconciler(t *testing.T, objects ...client.Object) (*BackupReconciler, client.Client, *fakeObjectStore) {
	t.Helper()
	scheme := backupTestScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&oramav1alpha1.Backup{}, &oramav1alpha1.CloudflareR2Bucket{}, &batchv1.Job{}, &corev1.Pod{}).
		WithObjects(objects...).Build()
	store := &fakeObjectStore{}
	clock := time.Date(2026, 10, 21, 3, 0, 5, 0, time.UTC)
	return &BackupReconciler{
		Client: kube, Scheme: scheme, Image: testOperatorImage,
		ObjectStore: func(backup.Bucket) BackupObjectStore { return store },
		Now:         func() time.Time { return clock },
	}, kube, store
}

func reconcileBackup(t *testing.T, r *BackupReconciler, b *oramav1alpha1.Backup) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(b)})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(b), b); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return result
}

func TestBackupRefusesADatastoreKindWithNoMechanism(t *testing.T) {
	b := testBackup(oramav1alpha1.BackupDatastorePostgres)
	r, kube, _ := newBackupReconciler(t, append(readyBackupBucket(), b)...)
	reconcileBackup(t, r, b)
	if b.Status.Phase != oramav1alpha1.BackupFailed {
		t.Fatalf("phase = %q, want Failed", b.Status.Phase)
	}
	condition := apimeta.FindStatusCondition(b.Status.Conditions, "Complete")
	if condition == nil || condition.Reason != "MechanismNotAvailable" || !strings.Contains(condition.Message, "Postgres") {
		t.Fatalf("condition = %+v", condition)
	}
	var jobs batchv1.JobList
	if err := kube.List(context.Background(), &jobs, client.InNamespace("ws-1")); err != nil || len(jobs.Items) != 0 {
		t.Fatalf("jobs = %d, %v; a kind with no mechanism must start no Job", len(jobs.Items), err)
	}
}

func TestBackupWaitsUntilTheBucketCanBeUsed(t *testing.T) {
	cases := []struct {
		name   string
		mutate func([]client.Object)
		reason string
	}{
		{"no bucket", func(o []client.Object) { o[0].SetName("other") }, "BucketNotFound"},
		{"no credentials", func(o []client.Object) {
			o[0].(*oramav1alpha1.CloudflareR2Bucket).Spec.Credentials = nil
		}, "BucketHasNoCredentials"},
		{"namespace not allowed", func(o []client.Object) {
			o[0].(*oramav1alpha1.CloudflareR2Bucket).Spec.Credentials.AllowedNamespaces = []string{"ws-2"}
		}, "BucketNotAllowed"},
		{"not ready", func(o []client.Object) {
			o[0].(*oramav1alpha1.CloudflareR2Bucket).Status.Ready = false
		}, "BucketNotReady"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			objects := readyBackupBucket()
			tc.mutate(objects)
			b := testBackup(oramav1alpha1.BackupDatastoreClickHouse)
			r, _, _ := newBackupReconciler(t, append(objects, b)...)
			result := reconcileBackup(t, r, b)
			condition := apimeta.FindStatusCondition(b.Status.Conditions, "Complete")
			if b.Status.Phase != oramav1alpha1.BackupPending || condition == nil || condition.Reason != tc.reason {
				t.Fatalf("phase %q, condition %+v; want Pending/%s", b.Status.Phase, condition, tc.reason)
			}
			if result.RequeueAfter == 0 {
				t.Fatal("a waiting Backup must be retried")
			}
		})
	}
}

func TestBackupRunsAJobFromTheOperatorImageAndRecordsItsReport(t *testing.T) {
	b := testBackup(oramav1alpha1.BackupDatastoreClickHouse)
	r, kube, _ := newBackupReconciler(t, append(readyBackupBucket(), b)...)
	ctx := context.Background()
	reconcileBackup(t, r, b) // finalizer
	reconcileBackup(t, r, b)

	if b.Status.Phase != oramav1alpha1.BackupRunning || b.Status.Location != "s3://lake-backups/ws-1/clickhouse/events/events-20261021t030000z/" {
		t.Fatalf("status = %+v", b.Status)
	}
	var job batchv1.Job
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "ws-1", Name: b.Status.JobName}, &job); err != nil {
		t.Fatal(err)
	}
	pod := job.Spec.Template.Spec
	container := pod.Containers[0]
	if container.Image != testOperatorImage || strings.Join(container.Command, " ") != "/manager backup" {
		t.Fatalf("container runs %s %v; want the operator image's /manager backup", container.Image, container.Command)
	}
	wantArgs := "--datastore ClickHouse --backup events-20261021t030000z --prefix ws-1/clickhouse/events/events-20261021t030000z"
	if !strings.HasPrefix(strings.Join(container.Args, " "), wantArgs) {
		t.Fatalf("args = %v", container.Args)
	}
	// Restricted Pod Security, no Kubernetes credentials.
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot ||
		*container.SecurityContext.AllowPrivilegeEscalation || !*container.SecurityContext.ReadOnlyRootFilesystem ||
		container.SecurityContext.Capabilities.Drop[0] != "ALL" || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("pod is not restricted: %+v %+v", pod.SecurityContext, container.SecurityContext)
	}
	secretsUsed := map[string]bool{}
	for _, env := range container.Env {
		if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("env %s is not from a Secret", env.Name)
		}
		secretsUsed[env.ValueFrom.SecretKeyRef.Name] = true
	}
	copyName := b.Status.JobName + "-bucket"
	if len(secretsUsed) != 2 || !secretsUsed["events-connection"] || !secretsUsed[copyName] {
		t.Fatalf("Job reads Secrets %v; want the connection and the bucket copy only", secretsUsed)
	}
	var copied corev1.Secret
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "ws-1", Name: copyName}, &copied); err != nil {
		t.Fatal(err)
	}
	if _, leaked := copied.Data["token"]; leaked || string(copied.Data["accessKeyId"]) != "tok-1" {
		t.Fatalf("bucket copy = %v; want the scoped credentials and never the account token", copied.Data)
	}
	var policy networkingv1.NetworkPolicy
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "ws-1", Name: b.Status.JobName}, &policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.Spec.Ingress) != 0 || len(policy.Spec.Egress) != 3 {
		t.Fatalf("policy = %+v", policy.Spec)
	}

	// The Job finishes and reports.
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}
	if err := kube.Status().Update(ctx, &job); err != nil {
		t.Fatal(err)
	}
	runPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: b.Status.JobName + "-x", Namespace: "ws-1", Labels: map[string]string{"job-name": b.Status.JobName}}}
	if err := kube.Create(ctx, runPod); err != nil {
		t.Fatal(err)
	}
	runPod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "backup", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		Message: `{"succeeded":true,"sizeBytes":4096,"message":"clickhouse backup e7a1 created"}`,
	}}}}
	if err := kube.Status().Update(ctx, runPod); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r, b)
	condition := apimeta.FindStatusCondition(b.Status.Conditions, "Complete")
	if b.Status.Phase != oramav1alpha1.BackupSucceeded || b.Status.SizeBytes != 4096 || b.Status.CompletionTime == nil || condition.Status != metav1.ConditionTrue {
		t.Fatalf("status = %+v", b.Status)
	}
	// Once done, the run's credentials and policy are removed.
	reconcileBackup(t, r, b)
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "ws-1", Name: copyName}, &copied); !apierrors.IsNotFound(err) {
		t.Fatalf("bucket copy still exists after the run: %v", err)
	}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "ws-1", Name: b.Status.JobName}, &policy); !apierrors.IsNotFound(err) {
		t.Fatalf("network policy still exists after the run: %v", err)
	}
}

func TestBackupFailureIsInTheWordsOfWhatRefused(t *testing.T) {
	b := testBackup(oramav1alpha1.BackupDatastoreClickHouse)
	r, kube, _ := newBackupReconciler(t, append(readyBackupBucket(), b)...)
	ctx := context.Background()
	reconcileBackup(t, r, b)
	reconcileBackup(t, r, b)
	var job batchv1.Job
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "ws-1", Name: b.Status.JobName}, &job); err != nil {
		t.Fatal(err)
	}
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded"}}
	if err := kube.Status().Update(ctx, &job); err != nil {
		t.Fatal(err)
	}
	refusal := "clickhouse refused: 500 Code: 499. DB::Exception: Access Denied"
	runPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ws-1", Labels: map[string]string{"job-name": job.Name}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "backup", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Message: `{"succeeded":false,"message":"` + refusal + `"}`,
		}}}}}}
	if err := kube.Create(ctx, runPod); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r, b)
	if b.Status.Phase != oramav1alpha1.BackupFailed || b.Status.Message != refusal {
		t.Fatalf("status = %+v; want Failed with the datastore's refusal", b.Status)
	}
}

func TestDeletingABackupDeletesItsObjectsUnlessTheBucketIsGone(t *testing.T) {
	ctx := context.Background()
	b := testBackup(oramav1alpha1.BackupDatastoreClickHouse)
	b.Finalizers = []string{backupFinalizer}
	b.Status = oramav1alpha1.BackupStatus{Phase: oramav1alpha1.BackupSucceeded, Location: "s3://lake-backups/ws-1/clickhouse/events/events-20261021t030000z/"}
	r, kube, store := newBackupReconciler(t, append(readyBackupBucket(), b)...)
	if err := kube.Delete(ctx, b); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r, b)
	if len(store.deleted) != 1 || store.deleted[0] != "ws-1/clickhouse/events/events-20261021t030000z/" {
		t.Fatalf("deleted prefixes = %v", store.deleted)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(b), b); !apierrors.IsNotFound(err) {
		t.Fatalf("Backup should be gone: %v", err)
	}

	// With the bucket resource gone, the objects stay (a Retain bucket outlives it) and the
	// Backup is still released.
	kept := testBackup(oramav1alpha1.BackupDatastoreClickHouse)
	kept.Finalizers = []string{backupFinalizer}
	kept.Status = b.Status
	r, kube, store = newBackupReconciler(t, kept)
	if err := kube.Delete(ctx, kept); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r, kept)
	if len(store.deleted) != 0 {
		t.Fatalf("deleted %v with no bucket", store.deleted)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(kept), kept); !apierrors.IsNotFound(err) {
		t.Fatalf("Backup should be released: %v", err)
	}
}

func TestBackupJobNameIsADNSLabelWithRoomForItsSecret(t *testing.T) {
	for _, name := range []string{"short", strings.Repeat("a", 63), strings.Repeat("schedule.name-", 15) + "20261021t030000z"} {
		got := backupJobName(name)
		if len(got+"-bucket") > 63 || dnsLabel(got) != got {
			t.Fatalf("backupJobName(%q) = %q", name, got)
		}
	}
	if backupJobName(strings.Repeat("a", 70)+"1") == backupJobName(strings.Repeat("a", 70)+"2") {
		t.Fatal("long names must not collide")
	}
}

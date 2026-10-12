/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const testBucketCopy = "prod-st-555555555555-credentials"

// mountsFixture is a sqlite service (whose pod the operator builds) with a volume and a bucket of
// its workspace.
func mountsFixture(mounts ...LakeStoreMount) *fakeServiceStore {
	volume := testStore(testVolumeStoreID, testVolumeStoreSlug, storeTypeVolume, storeProviderCluster, `{"size_gi":5}`)
	volume.Name = "scratch"
	bucket := testStore(testBucketStoreID, testBucketStoreSlug, storeTypeObject, "s3", testBucketSpec("https://s3.amazonaws.com"))
	bucket.Name = "daily-reports"
	return &fakeServiceStore{
		services:    []LakeService{testService(testSQLiteID, testSQLiteSlug, serviceKindSQLite, `{"storage_gi":15}`, false)},
		stores:      []LakeStore{volume, bucket},
		storeMounts: mounts,
	}
}

func volumeMount(path string, readOnly bool) LakeStoreMount {
	return LakeStoreMount{StoreID: testVolumeStoreID, ObjectKind: StoreMountKindService, ObjectID: testSQLiteID, MountPath: path, ReadOnly: readOnly}
}

func bucketMount() LakeStoreMount {
	return LakeStoreMount{StoreID: testBucketStoreID, ObjectKind: StoreMountKindService, ObjectID: testSQLiteID}
}

func podEnv(pod corev1.PodSpec) map[string]corev1.EnvVar {
	env := map[string]corev1.EnvVar{}
	for _, variable := range pod.Containers[0].Env {
		env[variable.Name] = variable
	}
	return env
}

func hasStoreVolume(pod corev1.PodSpec, slug string) bool {
	for _, volume := range pod.Volumes {
		if volume.Name == storeVolumeName(slug) {
			return true
		}
	}
	return false
}

func (h *serviceHarness) sqliteTemplate() corev1.PodTemplateSpec {
	h.t.Helper()
	return h.sqliteStatefulSet(testSQLiteName).Spec.Template
}

func TestStoreMountsWrapperServicePods(t *testing.T) {
	store := mountsFixture(volumeMount("/mnt/scratch", true), bucketMount())
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	h.deliverStoreSecret(testBucketSecret)
	h.poll()

	template := h.sqliteTemplate()
	pod := template.Spec
	var claim *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == storeVolumeName(testVolumeStoreSlug) {
			claim = &pod.Volumes[i]
		}
	}
	if claim == nil || claim.PersistentVolumeClaim == nil || claim.PersistentVolumeClaim.ClaimName != testVolumeClaim || !claim.PersistentVolumeClaim.ReadOnly {
		t.Fatalf("volumes = %+v", pod.Volumes)
	}
	found := false
	for _, mount := range pod.Containers[0].VolumeMounts {
		found = found || (mount.Name == storeVolumeName(testVolumeStoreSlug) && mount.MountPath == "/mnt/scratch" && mount.ReadOnly)
	}
	if !found {
		t.Fatalf("volume mounts = %+v", pod.Containers[0].VolumeMounts)
	}
	env := podEnv(pod)
	for name, want := range map[string]string{
		"STORE_DAILY_REPORTS_BUCKET": "reports", "STORE_DAILY_REPORTS_ENDPOINT": "https://s3.amazonaws.com",
		"STORE_DAILY_REPORTS_REGION": "auto", "STORE_DAILY_REPORTS_PREFIX": "daily/",
	} {
		if env[name].Value != want {
			t.Errorf("%s = %q, want %q", name, env[name].Value, want)
		}
	}
	for name, key := range map[string]string{
		"STORE_DAILY_REPORTS_ACCESS_KEY_ID": "access_key_id", "STORE_DAILY_REPORTS_SECRET_ACCESS_KEY": "secret_access_key",
		"STORE_DAILY_REPORTS_SESSION_TOKEN": "session_token", "STORE_DAILY_REPORTS_ACCOUNT_KEY": "account_key",
		"STORE_DAILY_REPORTS_SAS_TOKEN": "sas_token",
	} {
		ref := env[name].ValueFrom
		if ref == nil || ref.SecretKeyRef == nil || ref.SecretKeyRef.Name != testBucketCopy || ref.SecretKeyRef.Key != key ||
			ref.SecretKeyRef.Optional == nil || !*ref.SecretKeyRef.Optional {
			t.Errorf("%s = %+v", name, env[name])
		}
	}
	digest := template.Annotations[storeCredentialsAnnotation]
	if digest == "" {
		t.Fatal("no credentials digest on the pod template")
	}

	// The copy in the workload's namespace: the credential fields only, owned by the Lake.
	copied := &corev1.Secret{}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testServiceNamespace, Name: testBucketCopy}, copied); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(copied, h.lake) || copied.Labels[storeIDLabel] != testBucketStoreID || len(copied.Data) != 2 ||
		string(copied.Data["secret_access_key"]) != "secret" {
		t.Fatalf("copy = %+v", copied)
	}

	// Steady: no update to the copy or the StatefulSet.
	updates := *h.updates
	h.poll()
	if *h.updates != updates {
		t.Fatalf("a steady pass made %d updates", *h.updates-updates)
	}

	// A new credential version: the copy and the template digest follow, so the pods roll.
	delivered := &corev1.Secret{}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testServiceNamespace, Name: testBucketSecret}, delivered); err != nil {
		t.Fatal(err)
	}
	delivered.Data["secret_access_key"] = []byte("rotated")
	if err := h.client.Update(context.Background(), delivered); err != nil {
		t.Fatal(err)
	}
	h.poll()
	if got := h.sqliteTemplate().Annotations[storeCredentialsAnnotation]; got == digest || got == "" {
		t.Fatal("a new credential did not change the pod template")
	}

	// The bucket unmounted: its variables and copy go; the volume stays.
	store.mu.Lock()
	store.storeMounts = []LakeStoreMount{volumeMount("/mnt/scratch", true)}
	store.mu.Unlock()
	h.poll()
	pod = h.sqliteTemplate().Spec
	if _, found := podEnv(pod)["STORE_DAILY_REPORTS_BUCKET"]; found || !hasStoreVolume(pod, testVolumeStoreSlug) {
		t.Fatalf("after unmounting the bucket: env %v, volumes %+v", podEnv(pod), pod.Volumes)
	}
	if _, found := h.sqliteTemplate().Annotations[storeCredentialsAnnotation]; found {
		t.Fatal("digest kept without a bucket")
	}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: testServiceNamespace, Name: testBucketCopy}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("copy kept after the bucket was unmounted: %v", err)
	}
}

func TestStoreMountsWithoutMountsLeaveTheTemplate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		missing bool
	}{{"no mounts", false}, {"schema before 0080", true}} {
		t.Run(tc.name, func(t *testing.T) {
			store := mountsFixture()
			store.storeMountsMissing = tc.missing
			h := newServiceHarness(t, serviceTestLake(), store)
			h.poll()
			template := h.sqliteTemplate()
			if _, found := template.Annotations[storeCredentialsAnnotation]; found || len(template.Spec.Volumes) != 1 ||
				len(template.Spec.Containers[0].VolumeMounts) != 2 {
				t.Fatalf("template changed without mounts: %+v", template)
			}
			for name := range podEnv(template.Spec) {
				if len(name) > 6 && name[:6] == "STORE_" {
					t.Fatalf("store variable %s without a mount", name)
				}
			}
		})
	}
}

func TestStoreMountsReadFailureLeavesTheService(t *testing.T) {
	store := mountsFixture(volumeMount("/mnt/scratch", false))
	store.services = append(store.services, testService(testZettaServiceID, testZettaSlug, serviceKindZetta, `{}`, false))
	h := newServiceHarness(t, serviceTestLake(), store)
	h.poll()
	if !hasStoreVolume(h.sqliteTemplate().Spec, testVolumeStoreSlug) {
		t.Fatal("volume not mounted")
	}
	store.mu.Lock()
	store.storeMounts = nil
	store.storeMountsErr = errors.New("connection reset")
	store.mu.Unlock()
	h.poll()
	if !hasStoreVolume(h.sqliteTemplate().Spec, testVolumeStoreSlug) {
		t.Fatal("a failed mounts read took the volume out of the pods")
	}
	if !h.changing {
		t.Fatal("a service left for the next pass must keep the Lake on the fast poll")
	}
	// Other services carry on.
	h.zettaCluster(testZettaSlug)
}

func TestResolveStoreMountsLeavesOutWhatCannotMount(t *testing.T) {
	lake := serviceTestLake()
	volume := testStore(testVolumeStoreID, testVolumeStoreSlug, storeTypeVolume, storeProviderCluster, `{}`)
	other := testStore("77777777-7777-4777-8777-777777777777", "st-777777777777", storeTypeVolume, storeProviderCluster, `{}`)
	other.WorkspaceID = "99999999-9999-4999-8999-999999999999"
	absent := testStore("88888888-8888-4888-8888-888888888888", "st-888888888888", storeTypeVolume, storeProviderCluster, `{}`)
	absent.Desired = serviceDesiredAbsent
	lakeRun := testStore("66666666-6666-4666-8666-666666666666", "st-666666666666", storeTypeObject, storeProviderZetta, `{}`)
	second := testStore("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "st-aaaaaaaaaaaa", storeTypeVolume, storeProviderCluster, `{}`)
	second.Name = "zz"
	stores := []LakeStore{volume, other, absent, lakeRun, second}
	mount := func(id, path string) LakeStoreMount {
		return LakeStoreMount{StoreID: id, ObjectKind: StoreMountKindService, ObjectID: testSQLiteID, MountPath: path}
	}
	got := withoutReservedPaths(resolveStoreMounts(lake, stores, []LakeStoreMount{
		mount(volume.ID, "/mnt/a"),
		mount(other.ID, "/mnt/b"),
		mount(absent.ID, "/mnt/c"),
		mount(lakeRun.ID, ""),
		mount(second.ID, "/mnt/a"), // the same path as the first
		{StoreID: volume.ID, ObjectKind: StoreMountKindApp, ObjectID: testSQLiteID, MountPath: "/mnt/d"},
		{StoreID: volume.ID, ObjectKind: StoreMountKindService, ObjectID: "someone-else", MountPath: "/mnt/e"},
	}, StoreMountKindService, testSQLiteID, testServiceCustomer, testWorkspaceID), wrapperDataDir)
	if len(got) != 1 || got[0].StoreID != volume.ID || got[0].MountPath != "/mnt/a" {
		t.Fatalf("resolved = %+v", got)
	}
	for _, path := range []string{"", "/", "/a/../b", "relative", wrapperDataDir} {
		resolved := withoutReservedPaths(resolveStoreMounts(lake, []LakeStore{volume}, []LakeStoreMount{mount(volume.ID, path)},
			StoreMountKindService, testSQLiteID, testServiceCustomer, testWorkspaceID), wrapperDataDir)
		if len(resolved) != 0 {
			t.Errorf("path %q mounted", path)
		}
	}
	if got := storeEnvPrefix("daily-reports-2"); got != "STORE_DAILY_REPORTS_2_" {
		t.Fatalf("prefix = %q", got)
	}
}

// fakeVolumeStats answers a node's stats summary.
type fakeVolumeStats struct {
	used  map[types.NamespacedName]int64
	err   error
	calls int
}

func (f *fakeVolumeStats) PVCUsedBytes(_ context.Context, node string) (map[types.NamespacedName]int64, error) {
	f.calls++
	if node != "node-a" {
		return nil, errors.New("unknown node")
	}
	return f.used, f.err
}

func runningPodWithClaim(name, claim string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testServiceNamespace},
		Spec: corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "c", Image: "i"}},
			Volumes: []corev1.Volume{{Name: "v", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning}}
}

func TestStoreUsedBytes(t *testing.T) {
	store := mountsFixture(volumeMount("/mnt/scratch", false))
	claim := types.NamespacedName{Namespace: testServiceNamespace, Name: testVolumeClaim}
	stats := &fakeVolumeStats{used: map[types.NamespacedName]int64{claim: 100 << 20}}
	h := newServiceHarness(t, serviceTestLake(), store, runningPodWithClaim("sqlite-0", testVolumeClaim))
	h.reconciler.VolumeStats = stats
	h.poll()
	if len(store.usedBytesWrites) != 1 || store.usedBytesWrites[0] != (LakeStoreUsageWrite{testVolumeStoreID, 100 << 20}) {
		t.Fatalf("writes = %+v", store.usedBytesWrites)
	}
	// Once a minute: the next pass does not measure.
	calls := stats.calls
	h.poll()
	if stats.calls != calls {
		t.Fatal("measured twice within the interval")
	}

	lake := h.lake
	volume := &store.stores[0]
	now := time.Now()
	measure := func(used int64, at time.Time) {
		t.Helper()
		stats.used[claim] = used
		if err := h.reconciler.reconcileStoreUsage(context.Background(), lake, store, []*LakeStore{volume}, at); err != nil {
			t.Fatal(err)
		}
	}
	// Moved by less than 1 MiB: no write.
	measure(100<<20+512<<10, now)
	if len(store.usedBytesWrites) != 1 {
		t.Fatalf("a small move was written: %+v", store.usedBytesWrites)
	}
	// More than 1 MiB: written.
	measure(102<<20, now)
	if len(store.usedBytesWrites) != 2 || store.usedBytesWrites[1].UsedBytes != 102<<20 {
		t.Fatalf("writes = %+v", store.usedBytesWrites)
	}
	// Unchanged, but 15 minutes on: written again.
	measure(102<<20, now.Add(storeUsageMaxAge+time.Second))
	if len(store.usedBytesWrites) != 3 {
		t.Fatalf("a stale row was not refreshed: %+v", store.usedBytesWrites)
	}
	// Stats unavailable, or no running pod: nothing written, nothing fails.
	stats.err = errors.New("kubelet unreachable")
	measure(500<<20, now.Add(time.Hour))
	stats.err = nil
	if len(store.usedBytesWrites) != 3 {
		t.Fatalf("written without stats: %+v", store.usedBytesWrites)
	}
	if err := h.client.Delete(context.Background(), runningPodWithClaim("sqlite-0", testVolumeClaim)); err != nil {
		t.Fatal(err)
	}
	measure(500<<20, now.Add(time.Hour))
	if len(store.usedBytesWrites) != 3 {
		t.Fatalf("written without a pod mounting the volume: %+v", store.usedBytesWrites)
	}
}

func TestStoreUsedBytesNeedsTheMountsSchema(t *testing.T) {
	store := mountsFixture()
	store.storeMountsMissing = true
	stats := &fakeVolumeStats{used: map[types.NamespacedName]int64{}}
	h := newServiceHarness(t, serviceTestLake(), store, runningPodWithClaim("sqlite-0", testVolumeClaim))
	h.reconciler.VolumeStats = stats
	h.poll()
	if stats.calls != 0 || len(store.usedBytesWrites) != 0 {
		t.Fatalf("measured before 0080: %d calls, %+v", stats.calls, store.usedBytesWrites)
	}
}

func TestParseKubeletVolumeStats(t *testing.T) {
	raw := []byte(`{"node":{"nodeName":"node-a"},"pods":[
		{"podRef":{"name":"a","namespace":"ws"},"volume":[
			{"name":"store-st-444444444444","usedBytes":1048576,"pvcRef":{"name":"prod-st-444444444444","namespace":"ws"}},
			{"name":"tmp","usedBytes":4096}]},
		{"podRef":{"name":"b","namespace":"ws"}}]}`)
	used, err := parseKubeletVolumeStats(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(used) != 1 || used[types.NamespacedName{Namespace: "ws", Name: "prod-st-444444444444"}] != 1<<20 {
		t.Fatalf("used = %v", used)
	}
	if _, err := parseKubeletVolumeStats([]byte("not json")); err == nil {
		t.Fatal("a broken summary parsed")
	}
}

/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	migrationTestImage = "docker.io/oramasearch/cloud-v1-migration@sha256:0000000000000000000000000000000000000000000000000000000000000001"
	// Secret values: none may appear anywhere in a rendered Job.
	migrationTestDSN      = "postgresql://ro:prod1-db-password@aws.connect.psdb.cloud/oramacore"
	migrationTestKey      = "prod1-master-key-value"
	migrationTestAWSID    = "AKIAPROD1SETTINGS"
	migrationTestAWSKey   = "prod1-aws-secret-value"
	migrationTestEvidence = `{"observed_at":"2026-10-19T00:00:00Z"}`
	migrationTestRolePass = "migration-role-password"
	migrationTestStackKey = "stack-master-key-value"
)

// migrationTestStack is a Ready stack with its cluster row and a pinned writer image.
func migrationTestStack(mut func(*oramav1alpha1.OramaCloudV1)) *oramav1alpha1.OramaCloudV1 {
	return newOCV1(func(oc *oramav1alpha1.OramaCloudV1) {
		oc.Name, oc.Namespace = "v1-dev", "v1-dev"
		oc.Spec.Components.ControlDB = oramav1alpha1.ControlDBSpec{DataStoreSpec: oramav1alpha1.DataStoreSpec{Enabled: true}}
		oc.Spec.Components.Engine = oramav1alpha1.EngineSpec{Enabled: true, Envoy: true}
		oc.Spec.Images.Overrides = map[string]string{"oramacore": "oramasearch/oramacore-cloud-v1:v1.2.39-ready-9d8ed373"}
		oc.Spec.MasterAPIKeySecretRef = &oramav1alpha1.MasterAPIKeySecretRef{Name: "v1-dev-master", Key: "key"}
		oc.Status.Phase = "Ready"
		oc.Status.Conditions = []metav1.Condition{{Type: clusterRowReadyCondition, Status: metav1.ConditionTrue, Reason: "RowPresent"}}
		if mut != nil {
			mut(oc)
		}
	})
}

// migrationTestResource is the issue's example migration.
func migrationTestResource(mut func(*oramav1alpha1.OramaCloudV1Migration)) *oramav1alpha1.OramaCloudV1Migration {
	m := &oramav1alpha1.OramaCloudV1Migration{
		ObjectMeta: metav1.ObjectMeta{Name: "team-4e01bc49", Namespace: "v1-dev", UID: types.UID("uid-migration"), Generation: 1},
		Spec: oramav1alpha1.OramaCloudV1MigrationSpec{
			TargetRef:  "v1-dev",
			TeamID:     "4e01bc49-b388-43c2-8986-2395a0b8fe52",
			ProjectIDs: []string{"b0d16f69-1a49-4659-8a60-008886765691", "a0d16f69-1a49-4659-8a60-008886765691"},
			RunID:      "pilot-1",
			Step:       oramav1alpha1.MigrationStepSeed,
			Apply:      true,
			EvidenceSecretRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "team-4e01bc49-evidence"}, Key: "isolation.json"},
			Image:          migrationTestImage,
			PullSecretName: "oramasearch-dockerhub",
			RemoveOnDelete: true,
			Source: oramav1alpha1.OramaCloudV1MigrationSource{
				DatabaseSecretRef: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "v1-nonprod-prod1-db-ro"}, Key: "url"},
				DatabaseHost: "aws.connect.psdb.cloud",
				WriterURL:    "https://writer.prod1.example.com",
				ReaderURL:    "https://reader.prod1.example.com",
				MasterKeySecretRef: corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "v1-nonprod-prod1-master-key"}, Key: "key"},
				DeploymentRevision: "v1.2.39",
				SchemaRevision:     "890a7e97",
				Settings: oramav1alpha1.OramaCloudV1MigrationSettings{
					Region:               "us-east-2",
					InstanceID:           "i-0b9478719a8f91e2b",
					Document:             "OramaMigrationReadIndexInfo",
					DataDir:              "/opt/oramacore/.data/writer",
					CredentialsSecretRef: corev1.LocalObjectReference{Name: "v1-nonprod-prod1-settings-ssm"},
				},
			},
		},
	}
	if mut != nil {
		mut(m)
	}
	return m
}

// migrationTestInputs are the stack's current control DB schemas and every Secret a run reads.
func migrationTestInputs(oc *oramav1alpha1.OramaCloudV1) []client.Object {
	schemas, err := controlDBSchemas(oc)
	if err != nil {
		panic(err)
	}
	var objs []client.Object
	for _, s := range schemas {
		v := schemaVersion(s.sql)
		objs = append(objs, &oramav1alpha1.PostgresSchema{
			ObjectMeta: metav1.ObjectMeta{Name: oc.Name + s.suffix, Namespace: oc.Namespace, Generation: 1},
			Spec:       oramav1alpha1.PostgresSchemaSpec{Version: v},
			Status:     oramav1alpha1.PostgresSchemaStatus{Phase: "Applied", AppliedVersion: v, ObservedGeneration: 1},
		})
	}
	secret := func(name string, data map[string]string) *corev1.Secret {
		s := secretOf(name, data)
		s.Namespace = oc.Namespace
		return s
	}
	return append(objs,
		secret("v1-nonprod-prod1-db-ro", map[string]string{"url": migrationTestDSN}),
		secret("v1-nonprod-prod1-master-key", map[string]string{"key": migrationTestKey}),
		secret("v1-nonprod-prod1-settings-ssm", map[string]string{"access_key_id": migrationTestAWSID, "secret_access_key": migrationTestAWSKey}),
		secret("team-4e01bc49-evidence", map[string]string{"isolation.json": migrationTestEvidence}),
		secret("oramasearch-dockerhub", map[string]string{".dockerconfigjson": "{}"}),
		secret(migrationRoleSecretName(oc), map[string]string{"username": "migration", "password": migrationTestRolePass,
			"uri": "postgresql://migration:" + migrationTestRolePass + "@v1-dev-controldb-rw.v1-dev:5432/oramacore"}),
		secret("v1-dev-master", map[string]string{"key": migrationTestStackKey}),
	)
}

// migrationHarness is a fake cluster holding a migration, its stack and its inputs. It counts
// status writes.
type migrationHarness struct {
	t            *testing.T
	kube         client.Client
	r            *OramaCloudV1MigrationReconciler
	key          client.ObjectKey
	statusWrites int
	// failPods makes listing pods fail, as a transient API error would.
	failPods bool
}

func newMigrationHarness(t *testing.T, m *oramav1alpha1.OramaCloudV1Migration, objs ...client.Object) *migrationHarness {
	t.Helper()
	s := ocv1TestScheme(t)
	h := &migrationHarness{t: t, key: client.ObjectKeyFromObject(m)}
	h.kube = fake.NewClientBuilder().WithScheme(s).
		WithStatusSubresource(&oramav1alpha1.OramaCloudV1Migration{}, &batchv1.Job{}).
		WithObjects(append(objs, m)...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.PodList); ok && h.failPods {
					return errors.New("injected pod list failure")
				}
				return c.List(ctx, list, opts...)
			},
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if _, ok := obj.(*oramav1alpha1.OramaCloudV1Migration); ok {
					h.statusWrites++
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	h.r = &OramaCloudV1MigrationReconciler{Client: h.kube, Scheme: s, APIReader: h.kube}
	return h
}

func (h *migrationHarness) reconcile() ctrl.Result {
	h.t.Helper()
	res, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key})
	if err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (h *migrationHarness) migration() *oramav1alpha1.OramaCloudV1Migration {
	h.t.Helper()
	m := &oramav1alpha1.OramaCloudV1Migration{}
	if err := h.kube.Get(context.Background(), h.key, m); err != nil {
		h.t.Fatal(err)
	}
	return m
}

func (h *migrationHarness) update(mut func(*oramav1alpha1.OramaCloudV1Migration)) {
	h.t.Helper()
	m := h.migration()
	mut(m)
	m.Generation++
	if err := h.kube.Update(context.Background(), m); err != nil {
		h.t.Fatal(err)
	}
}

func (h *migrationHarness) jobs() []string {
	h.t.Helper()
	var jobs batchv1.JobList
	if err := h.kube.List(context.Background(), &jobs, client.InNamespace(h.key.Namespace)); err != nil {
		h.t.Fatal(err)
	}
	var names []string
	for _, j := range jobs.Items {
		names = append(names, j.Name)
	}
	return names
}

func (h *migrationHarness) job(name string) *batchv1.Job {
	h.t.Helper()
	job := &batchv1.Job{}
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: h.key.Namespace, Name: name}, job); err != nil {
		h.t.Fatalf("Job %s: %v", name, err)
	}
	return job
}

// finish ends a Job as the Job controller would after its pod's tool exited with code, writing
// message to the termination log.
func (h *migrationHarness) finish(name string, code int32, message string) {
	h.t.Helper()
	ctx := context.Background()
	job := h.job(name)
	cond := batchv1.JobComplete
	if code != 0 {
		cond = batchv1.JobFailed
		job.Status.Failed = 1
	} else {
		job.Status.Succeeded = 1
		now := metav1.Now()
		job.Status.CompletionTime = &now
	}
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{Type: cond, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()})
	if err := h.kube.Status().Update(ctx, job); err != nil {
		h.t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-pod", Namespace: h.key.Namespace, Labels: map[string]string{batchv1.JobNameLabel: name}},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{{Name: migrationWaitContainer,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: migrationContainer,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Message: message}}}},
		},
	}
	if err := h.kube.Create(ctx, pod); err != nil {
		h.t.Fatal(err)
	}
}

func migrationStack(t *testing.T, mut func(*oramav1alpha1.OramaCloudV1)) []client.Object {
	t.Helper()
	oc := migrationTestStack(mut)
	return append([]client.Object{oc}, migrationTestInputs(oc)...)
}

func TestMigrationJobRendersTheToolContract(t *testing.T) {
	oc := migrationTestStack(nil)
	h := newMigrationHarness(t, migrationTestResource(nil), migrationStack(t, nil)...)
	h.reconcile()
	m := h.migration()
	if m.Status.Phase != oramav1alpha1.MigrationPhaseRunning || m.Status.JobName != "team-4e01bc49-seed-0" {
		t.Fatalf("status = %+v", m.Status)
	}
	job := h.job("team-4e01bc49-seed-0")
	pod := job.Spec.Template.Spec
	c := pod.Containers[0]

	if !reflect.DeepEqual(c.Command, []string{"run-step"}) || !reflect.DeepEqual(c.Args, []string{"seed", "--apply"}) {
		t.Fatalf("command %v args %v", c.Command, c.Args)
	}
	if c.Image != migrationTestImage {
		t.Fatalf("image %s", c.Image)
	}

	// Every variable, in order: a literal, or a secretKeyRef to exactly this Secret and key.
	type want struct{ value, secret, key string }
	wants := []struct {
		name string
		want
	}{
		{"TEAM_ID", want{value: "4e01bc49-b388-43c2-8986-2395a0b8fe52"}},
		{"RUN_ID", want{value: "pilot-1"}},
		{"PROJECT_IDS", want{value: "a0d16f69-1a49-4659-8a60-008886765691,b0d16f69-1a49-4659-8a60-008886765691"}},
		{"RUNS_DIR", want{value: "/runs"}},
		{"EVIDENCE_PATH", want{value: "/evidence/isolation.json"}},
		{"ORAMA_MIGRATION_PROD1_DSN", want{secret: "v1-nonprod-prod1-db-ro", key: "url"}},
		{"ORAMA_MIGRATION_PROD1_WRITER_URL", want{value: "https://writer.prod1.example.com"}},
		{"ORAMA_MIGRATION_PROD1_READER_URL", want{value: "https://reader.prod1.example.com"}},
		{"ORAMA_MIGRATION_PROD1_MASTER_KEY", want{secret: "v1-nonprod-prod1-master-key", key: "key"}},
		{"ORAMA_MIGRATION_PROD1_DEPLOYMENT_REVISION", want{value: "v1.2.39"}},
		{"ORAMA_MIGRATION_PROD1_SCHEMA_REVISION", want{value: "890a7e97"}},
		{"ORAMA_MIGRATION_PROD1_SETTINGS_SSM_REGION", want{value: "us-east-2"}},
		{"ORAMA_MIGRATION_PROD1_SETTINGS_SSM_INSTANCE_ID", want{value: "i-0b9478719a8f91e2b"}},
		{"ORAMA_MIGRATION_PROD1_SETTINGS_SSM_DOCUMENT", want{value: "OramaMigrationReadIndexInfo"}},
		{"ORAMA_MIGRATION_PROD1_SETTINGS_DATA_DIR", want{value: "/opt/oramacore/.data/writer"}},
		{"ORAMA_MIGRATION_PROD1_SETTINGS_AWS_ACCESS_KEY_ID", want{secret: "v1-nonprod-prod1-settings-ssm", key: "access_key_id"}},
		{"ORAMA_MIGRATION_PROD1_SETTINGS_AWS_SECRET_ACCESS_KEY", want{secret: "v1-nonprod-prod1-settings-ssm", key: "secret_access_key"}},
		{"ORAMA_MIGRATION_PROD2_DSN", want{secret: "v1-dev-controldb-migration", key: "uri"}},
		{"ORAMA_MIGRATION_PROD2_WRITER_URL", want{value: "http://v1-dev-writer:8080"}},
		{"ORAMA_MIGRATION_PROD2_READER_URL", want{value: "http://v1-dev-reader"}},
		{"ORAMA_MIGRATION_PROD2_MASTER_KEY", want{secret: "v1-dev-master", key: "key"}},
		{"ORAMA_MIGRATION_PROD2_DEPLOYMENT_REVISION", want{value: "v1.2.39-ready-9d8ed373"}},
		{"ORAMA_MIGRATION_PROD2_SCHEMA_REVISION", want{value: lastTableSchemaVersion(t, oc)}},
	}
	if len(c.Env) != len(wants) {
		t.Fatalf("env has %d variables, want %d: %v", len(c.Env), len(wants), envNames(c.Env))
	}
	for i, w := range wants {
		got := c.Env[i]
		if got.Name != w.name {
			t.Fatalf("env[%d] = %s, want %s", i, got.Name, w.name)
		}
		if w.secret == "" {
			if got.ValueFrom != nil || got.Value != w.value {
				t.Errorf("%s = %q (from %v), want literal %q", w.name, got.Value, got.ValueFrom, w.value)
			}
			continue
		}
		ref := got.ValueFrom
		if got.Value != "" || ref == nil || ref.SecretKeyRef == nil || ref.SecretKeyRef.Name != w.secret || ref.SecretKeyRef.Key != w.key ||
			ref.SecretKeyRef.Optional != nil {
			t.Errorf("%s = %q from %v, want secretKeyRef %s/%s", w.name, got.Value, ref, w.secret, w.key)
		}
	}
	for _, e := range c.Env {
		if e.Name == "ORAMA_MIGRATION_PROD2_CLUSTER_ID" {
			t.Error("the tool reads the target cluster id itself")
		}
	}

	// No Secret value anywhere in the Job: not in args, not in literal env, not in the wait.
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{migrationTestDSN, migrationTestKey, migrationTestAWSID, migrationTestAWSKey, migrationTestEvidence, migrationTestRolePass, migrationTestStackKey} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the Job carries a Secret value %q", secret)
		}
	}

	// Mounts: the run volume, a writable /tmp, the receipt read-only. No SSH anywhere.
	mounts := map[string]corev1.VolumeMount{}
	for _, vm := range c.VolumeMounts {
		mounts[vm.MountPath] = vm
	}
	volumes := map[string]corev1.Volume{}
	for _, v := range pod.Volumes {
		volumes[v.Name] = v
	}
	if len(mounts) != 3 || mounts["/runs"].Name != "runs" || mounts["/tmp"].Name != "tmp" || !mounts["/evidence"].ReadOnly {
		t.Fatalf("mounts = %+v", c.VolumeMounts)
	}
	if v := volumes["runs"].PersistentVolumeClaim; v == nil || v.ClaimName != "team-4e01bc49-runs" {
		t.Fatalf("runs volume = %+v", volumes["runs"])
	}
	if volumes["tmp"].EmptyDir == nil {
		t.Fatalf("tmp volume = %+v", volumes["tmp"])
	}
	ev := volumes["evidence"].Secret
	if ev == nil || ev.SecretName != "team-4e01bc49-evidence" || *ev.DefaultMode != 0o400 ||
		!reflect.DeepEqual(ev.Items, []corev1.KeyToPath{{Key: "isolation.json", Path: "isolation.json"}}) {
		t.Fatalf("evidence volume = %+v", volumes["evidence"])
	}
	if strings.Contains(string(raw), ".ssh") || strings.Contains(string(raw), "SSH") {
		t.Error("the Job mentions SSH: settings are read over SSM")
	}

	// Security: non-root, read-only root, no service account token, no privilege.
	sc := pod.SecurityContext
	if !*sc.RunAsNonRoot || *sc.RunAsUser != 10001 || *sc.RunAsGroup != 10001 || *sc.FSGroup != 10001 {
		t.Fatalf("pod security = %+v", sc)
	}
	if *pod.AutomountServiceAccountToken {
		t.Fatal("service account token mounted")
	}
	for _, ctr := range append(pod.InitContainers, pod.Containers...) {
		s := ctr.SecurityContext
		if s == nil || !*s.ReadOnlyRootFilesystem || *s.AllowPrivilegeEscalation || !reflect.DeepEqual(s.Capabilities.Drop, []corev1.Capability{"ALL"}) {
			t.Fatalf("%s security = %+v", ctr.Name, s)
		}
	}
	if c.TerminationMessagePolicy != corev1.TerminationMessageReadFile {
		t.Fatalf("termination message policy %s", c.TerminationMessagePolicy)
	}
	if !reflect.DeepEqual(pod.ImagePullSecrets, []corev1.LocalObjectReference{{Name: "oramasearch-dockerhub"}}) {
		t.Fatalf("pull secrets %v", pod.ImagePullSecrets)
	}

	// The wait signs in as the migration role through PG* variables, its password a secretKeyRef.
	wait := pod.InitContainers[0]
	if wait.Name != migrationWaitContainer || wait.Image != "postgres:16-alpine" || strings.Contains(strings.Join(wait.Command, " "), "postgresql://") {
		t.Fatalf("wait container = %+v", wait)
	}
	if !reflect.DeepEqual(envNames(wait.Env), []string{"PGHOST", "PGPORT", "PGDATABASE", "PGUSER", "PGPASSWORD", "PGCONNECT_TIMEOUT"}) ||
		wait.Env[3].Value != "migration" || wait.Env[4].ValueFrom.SecretKeyRef.Name != "v1-dev-controldb-migration" || wait.Env[4].ValueFrom.SecretKeyRef.Key != "password" {
		t.Fatalf("wait env = %+v", wait.Env)
	}

	// Labels put the pod in the stack's sandbox as component migration.
	for k, v := range map[string]string{
		"app.kubernetes.io/name": appName, "app.kubernetes.io/instance": "v1-dev", "app.kubernetes.io/component": "migration",
		"app.kubernetes.io/managed-by": "orama-cloud-v1-operator", migrationLabel: "team-4e01bc49",
	} {
		if job.Spec.Template.Labels[k] != v {
			t.Errorf("pod label %s = %q, want %q", k, job.Spec.Template.Labels[k], v)
		}
	}
	if *job.Spec.BackoffLimit != 0 || *job.Spec.ActiveDeadlineSeconds != 6*3600 || *job.Spec.TTLSecondsAfterFinished != 604800 {
		t.Fatalf("job limits = %v %v %v", *job.Spec.BackoffLimit, *job.Spec.ActiveDeadlineSeconds, *job.Spec.TTLSecondsAfterFinished)
	}
	if !metav1.IsControlledBy(job, m) {
		t.Fatal("Job not controlled by the migration")
	}

	// The run volume: owned, ReadWriteOnce, 20Gi by default.
	pvc := &corev1.PersistentVolumeClaim{}
	if err := h.kube.Get(context.Background(), client.ObjectKey{Namespace: "v1-dev", Name: "team-4e01bc49-runs"}, pvc); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(pvc, m) || !reflect.DeepEqual(pvc.Spec.AccessModes, []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}) ||
		pvc.Spec.Resources.Requests.Storage().String() != "20Gi" || pvc.Spec.StorageClassName != nil {
		t.Fatalf("run volume = %+v", pvc.Spec)
	}
	if !meta.IsStatusConditionTrue(m.Status.Conditions, migrationTargetWrittenCondition) {
		t.Fatal("an applied seed did not record TargetWritten")
	}
}

// lastTableSchemaVersion is the version of the last control DB schema that creates tables (0008).
func lastTableSchemaVersion(t *testing.T, oc *oramav1alpha1.OramaCloudV1) string {
	t.Helper()
	return schemaVersion(mustSQL(controlDBSchema0008SQL()))
}

func envNames(env []corev1.EnvVar) []string {
	var names []string
	for _, e := range env {
		names = append(names, e.Name)
	}
	return names
}

// Each step runs its own command, --apply only for seed and catchUp with apply, and the receipt only
// for those two.
func TestMigrationCommandPerStepAndApply(t *testing.T) {
	oc := migrationTestStack(nil)
	target := &migrationTarget{oc: oc, deploymentRevision: "r", schemaRevision: "s"}
	for _, tt := range []struct {
		step     string
		apply    bool
		job      string
		args     []string
		evidence bool
	}{
		{oramav1alpha1.MigrationStepPlan, false, "team-4e01bc49-plan-0", []string{"plan"}, false},
		{oramav1alpha1.MigrationStepPlan, true, "team-4e01bc49-plan-0", []string{"plan"}, false},
		{oramav1alpha1.MigrationStepSeed, false, "team-4e01bc49-seed-dry-run-0", []string{"seed"}, true},
		{oramav1alpha1.MigrationStepSeed, true, "team-4e01bc49-seed-0", []string{"seed", "--apply"}, true},
		{oramav1alpha1.MigrationStepCatchUp, false, "team-4e01bc49-catch-up-dry-run-0", []string{"catch-up"}, true},
		{oramav1alpha1.MigrationStepCatchUp, true, "team-4e01bc49-catch-up-0", []string{"catch-up", "--apply"}, true},
		{oramav1alpha1.MigrationStepVerify, true, "team-4e01bc49-verify-0", []string{"verify"}, false},
	} {
		m := migrationTestResource(func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.Step, m.Spec.Apply = tt.step, tt.apply })
		run := migrationRunFor(m)
		job := migrationJob(m, target, run)
		c := job.Spec.Template.Spec.Containers[0]
		if job.Name != tt.job || !reflect.DeepEqual(c.Args, tt.args) {
			t.Errorf("%s apply=%t: Job %s args %v, want %s %v", tt.step, tt.apply, job.Name, c.Args, tt.job, tt.args)
		}
		hasEvidence := false
		for _, e := range c.Env {
			hasEvidence = hasEvidence || e.Name == "EVIDENCE_PATH"
		}
		for _, vm := range c.VolumeMounts {
			hasEvidence = hasEvidence || vm.MountPath == "/evidence"
		}
		if hasEvidence != tt.evidence {
			t.Errorf("%s: evidence mounted = %t", tt.step, hasEvidence)
		}
	}
	removal := migrationRemovalRun(migrationTestResource(func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.Attempt = 2 }))
	if c := migrationJob(migrationTestResource(nil), target, removal).Spec.Template.Spec.Containers[0]; removal.jobName != "team-4e01bc49-remove-2" ||
		!reflect.DeepEqual(c.Args, []string{"remove-target", "--apply"}) {
		t.Fatalf("removal Job %s args %v", removal.jobName, c.Args)
	}
	if got := migrationJobName(strings.Repeat("a", 60), "-catch-up-dry-run-12"); len(got) > 63 || !strings.HasSuffix(got, "-catch-up-dry-run-12") ||
		got == migrationJobName(strings.Repeat("a", 59)+"b", "-catch-up-dry-run-12") {
		t.Fatalf("long Job name %q", got)
	}
}

// No Job runs until the target exists, is Ready and has its cluster row.
func TestMigrationWaitsForItsTarget(t *testing.T) {
	for name, tt := range map[string]struct {
		objs []client.Object
		want string
	}{
		"no target":        {objs: nil, want: "waiting for OramaCloudV1 v1-dev/v1-dev"},
		"target not Ready": {objs: migrationStack(t, func(oc *oramav1alpha1.OramaCloudV1) { oc.Status.Phase = "Progressing" }), want: `to be Ready (it is "Progressing")`},
		"cluster row not ready": {objs: migrationStack(t, func(oc *oramav1alpha1.OramaCloudV1) {
			oc.Status.Conditions = []metav1.Condition{{Type: clusterRowReadyCondition, Status: metav1.ConditionFalse, Reason: "WaitingForSchema"}}
		}), want: "ClusterRowReady condition (WaitingForSchema)"},
		"a Secret missing": {objs: func() []client.Object {
			var objs []client.Object
			for _, o := range migrationStack(t, nil) {
				if o.GetName() != "v1-nonprod-prod1-settings-ssm" {
					objs = append(objs, o)
				}
			}
			return objs
		}(), want: "waiting for Secret v1-dev/v1-nonprod-prod1-settings-ssm"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newMigrationHarness(t, migrationTestResource(nil), tt.objs...)
			if res := h.reconcile(); res.RequeueAfter != migrationWait {
				t.Fatalf("requeue = %v", res)
			}
			m := h.migration()
			if m.Status.Phase != oramav1alpha1.MigrationPhasePending || !strings.Contains(m.Status.Message, tt.want) {
				t.Fatalf("status = %s %q, want Pending %q", m.Status.Phase, m.Status.Message, tt.want)
			}
			if jobs := h.jobs(); len(jobs) != 0 {
				t.Fatalf("Jobs created while waiting: %v", jobs)
			}
		})
	}
}

// An unsafe target or an out-of-order step is Blocked, and no Job runs.
func TestMigrationRefusesUnsafeOrOutOfOrderSteps(t *testing.T) {
	for name, tt := range map[string]struct {
		stack func(*oramav1alpha1.OramaCloudV1)
		mig   func(*oramav1alpha1.OramaCloudV1Migration)
		want  string
	}{
		"crons not suspended": {
			stack: func(oc *oramav1alpha1.OramaCloudV1) { oc.Spec.Components.Dashboard.Crons.Suspend = boolPtr(false) },
			want:  "spec.components.dashboard.crons.suspend false",
		},
		"analytics import running": {
			stack: func(oc *oramav1alpha1.OramaCloudV1) {
				oc.Spec.Components.AnalyticsProcessor.Import = oramav1alpha1.AnalyticsImportSpec{Enabled: true}
			},
			want: "spec.components.analyticsProcessor.import.suspend false",
		},
		"catchUp before an applied seed": {
			mig:  func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.Step = oramav1alpha1.MigrationStepCatchUp },
			want: "catchUp needs an applied seed",
		},
		"verify before an applied seed": {
			mig:  func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.Step = oramav1alpha1.MigrationStepVerify },
			want: "verify needs an applied seed",
		},
		"seed without a receipt": {
			mig:  func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.EvidenceSecretRef = nil },
			want: "seed needs spec.evidenceSecretRef",
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newMigrationHarness(t, migrationTestResource(tt.mig), migrationStack(t, tt.stack)...)
			h.reconcile()
			m := h.migration()
			if m.Status.Phase != oramav1alpha1.MigrationPhaseBlocked || !strings.Contains(m.Status.Message, tt.want) {
				t.Fatalf("status = %s %q, want Blocked %q", m.Status.Phase, m.Status.Message, tt.want)
			}
			if jobs := h.jobs(); len(jobs) != 0 {
				t.Fatalf("Jobs created while blocked: %v", jobs)
			}
		})
	}

	// A dry run of the seed never writes, so the stack's schedules do not matter to it.
	h := newMigrationHarness(t, migrationTestResource(func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.Apply = false }),
		migrationStack(t, func(oc *oramav1alpha1.OramaCloudV1) { oc.Spec.Components.Dashboard.Crons.Suspend = boolPtr(false) })...)
	h.reconcile()
	if jobs := h.jobs(); !reflect.DeepEqual(jobs, []string{"team-4e01bc49-seed-dry-run-0"}) {
		t.Fatalf("dry run Jobs = %v", jobs)
	}
}

// An applied seed that succeeded records Seeded; a catch-up then runs, and raising attempt runs it
// again as a new Job, keeping the first.
func TestMigrationSeedThenCatchUpReRunsOnAttempt(t *testing.T) {
	h := newMigrationHarness(t, migrationTestResource(nil), migrationStack(t, nil)...)
	h.reconcile()
	h.finish("team-4e01bc49-seed-0", 0, `{"version": 1, "phase": "seeded"}`)
	h.reconcile()
	if m := h.migration(); m.Status.Phase != oramav1alpha1.MigrationPhaseSucceeded || !meta.IsStatusConditionTrue(m.Status.Conditions, migrationSeededCondition) {
		t.Fatalf("after the seed: %+v", m.Status)
	}

	h.update(func(m *oramav1alpha1.OramaCloudV1Migration) {
		m.Spec.Step, m.Spec.Attempt = oramav1alpha1.MigrationStepCatchUp, 1
	})
	h.reconcile()
	h.finish("team-4e01bc49-catch-up-1", 0, `{"version": 1, "phase": "reconciled"}`)
	h.reconcile()
	h.update(func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.Attempt = 2 })
	h.reconcile()
	if jobs := h.jobs(); !reflect.DeepEqual(jobs, []string{"team-4e01bc49-catch-up-1", "team-4e01bc49-catch-up-2", "team-4e01bc49-seed-0"}) {
		t.Fatalf("Jobs = %v", jobs)
	}
	m := h.migration()
	if m.Status.Phase != oramav1alpha1.MigrationPhaseRunning || m.Status.JobName != "team-4e01bc49-catch-up-2" ||
		m.Status.Step != oramav1alpha1.MigrationStepCatchUp || m.Status.Attempt != 2 || !m.Status.Apply || m.Status.Result != nil {
		t.Fatalf("status = %+v", m.Status)
	}
	if !meta.IsStatusConditionTrue(m.Status.Conditions, migrationSeededCondition) {
		t.Fatal("Seeded was cleared")
	}

	// A finished run is not run again, even once its Job has expired.
	h.finish("team-4e01bc49-catch-up-2", 0, `{}`)
	h.reconcile()
	if err := h.kube.Delete(context.Background(), h.job("team-4e01bc49-catch-up-2")); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if jobs := h.jobs(); len(jobs) != 2 {
		t.Fatalf("an expired run ran again: %v", jobs)
	}
}

// A second run waits while another Job of the migration is still running.
func TestMigrationNeverRunsTwoJobsAtOnce(t *testing.T) {
	h := newMigrationHarness(t, migrationTestResource(func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.Apply = false }), migrationStack(t, nil)...)
	h.reconcile()
	h.update(func(m *oramav1alpha1.OramaCloudV1Migration) { m.Spec.Apply = true })
	h.reconcile()
	if m := h.migration(); m.Status.Phase != oramav1alpha1.MigrationPhasePending || !strings.Contains(m.Status.Message, "waiting for Job team-4e01bc49-seed-dry-run-0 to finish") {
		t.Fatalf("status = %s %q", m.Status.Phase, m.Status.Message)
	}
	if jobs := h.jobs(); len(jobs) != 1 {
		t.Fatalf("Jobs = %v", jobs)
	}
}

// The exit code decides the phase; the termination message is copied to status.result verbatim.
func TestMigrationOutcomeFromTheToolsExit(t *testing.T) {
	result := `{"version": 1, "run_id": "pilot-1", "phase": "reconciled", "reconciled": true, "target_write_active": false, "counts": {"projects": 1, "indexes": 1, "documents": 330}, "lag_seconds": 42, "last_command": "catch-up", "last_exit": 0}`
	for _, tt := range []struct {
		code    int32
		message string
		phase   string
		result  bool
		says    string
	}{
		{0, result, oramav1alpha1.MigrationPhaseSucceeded, true, "completed"},
		{3, result, oramav1alpha1.MigrationPhaseBlocked, true, "refused by the tool's guard"},
		{1, result, oramav1alpha1.MigrationPhaseFailed, true, "exit code 1"},
		{4, result, oramav1alpha1.MigrationPhaseFailed, true, "found a difference"},
		{1, "Traceback: not json", oramav1alpha1.MigrationPhaseFailed, false, "left no status --json"},
	} {
		h := newMigrationHarness(t, migrationTestResource(nil), migrationStack(t, nil)...)
		h.reconcile()
		h.finish("team-4e01bc49-seed-0", tt.code, tt.message)
		h.reconcile()
		m := h.migration()
		if m.Status.Phase != tt.phase || !strings.Contains(m.Status.Message, tt.says) || m.Status.CompletedAt == nil {
			t.Errorf("exit %d: status = %s %q", tt.code, m.Status.Phase, m.Status.Message)
		}
		switch {
		case tt.result && (m.Status.Result == nil || !sameJSON(t, m.Status.Result.Raw, []byte(result))):
			t.Errorf("exit %d: result = %v, want the termination message", tt.code, m.Status.Result)
		case !tt.result && m.Status.Result != nil:
			t.Errorf("exit %d: result = %s from a message that is not JSON", tt.code, m.Status.Result.Raw)
		}
		if seeded := meta.IsStatusConditionTrue(m.Status.Conditions, migrationSeededCondition); seeded != (tt.code == 0) {
			t.Errorf("exit %d: Seeded = %t", tt.code, seeded)
		}
	}
}

// A finished Job whose pod cannot be read is retried, never recorded without its exit code.
func TestMigrationOutcomeWaitsForAReadablePod(t *testing.T) {
	h := newMigrationHarness(t, migrationTestResource(nil), migrationStack(t, nil)...)
	h.reconcile()
	h.finish("team-4e01bc49-seed-0", 0, `{"phase": "seeded"}`)
	h.failPods = true
	if _, err := h.r.Reconcile(context.Background(), ctrl.Request{NamespacedName: h.key}); err == nil {
		t.Fatal("an unreadable pod did not fail the reconcile")
	}
	if m := h.migration(); m.Status.Phase != oramav1alpha1.MigrationPhaseRunning {
		t.Fatalf("phase = %s while the pod was unreadable", m.Status.Phase)
	}
	h.failPods = false
	h.reconcile()
	if m := h.migration(); m.Status.Phase != oramav1alpha1.MigrationPhaseSucceeded {
		t.Fatalf("phase = %s once the pod was readable", m.Status.Phase)
	}
}

// A Job stopped at its deadline is Failed with that message, whatever its pod says.
func TestMigrationDeadlineIsFailed(t *testing.T) {
	h := newMigrationHarness(t, migrationTestResource(nil), migrationStack(t, nil)...)
	h.reconcile()
	job := h.job("team-4e01bc49-seed-0")
	job.Status.Failed = 1
	job.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded}}
	if err := h.kube.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	h.reconcile()
	if m := h.migration(); m.Status.Phase != oramav1alpha1.MigrationPhaseFailed || !strings.Contains(m.Status.Message, "6h deadline") {
		t.Fatalf("status = %s %q", m.Status.Phase, m.Status.Message)
	}
}

// A steady migration writes no status: a second reconcile of an unchanged run changes nothing.
func TestMigrationStatusIsWrittenOnlyOnATransition(t *testing.T) {
	h := newMigrationHarness(t, migrationTestResource(nil), migrationStack(t, nil)...)
	h.reconcile()
	writes := h.statusWrites
	h.reconcile()
	h.reconcile()
	if h.statusWrites != writes {
		t.Fatalf("a running migration rewrote its status %d times", h.statusWrites-writes)
	}
	h.finish("team-4e01bc49-seed-0", 0, `{"phase": "seeded"}`)
	h.reconcile()
	if h.statusWrites != writes+1 {
		t.Fatalf("finishing wrote status %d times, want once", h.statusWrites-writes)
	}
	h.reconcile()
	if h.statusWrites != writes+1 {
		t.Fatal("a finished migration rewrote its status")
	}

	// Waiting on the same thing is not a transition either.
	w := newMigrationHarness(t, migrationTestResource(nil))
	w.reconcile()
	writes = w.statusWrites
	w.reconcile()
	if w.statusWrites != writes {
		t.Fatal("a waiting migration rewrote its status")
	}
}

// Deleting with removeOnDelete runs remove-target, then deletes the run volume, then lets go. The
// tool's refusal (exit 3: the team was switched) keeps the resource.
func TestMigrationDeletionRemovesTheTeamThenTheRunVolume(t *testing.T) {
	ctx := context.Background()
	start := func(t *testing.T) *migrationHarness {
		h := newMigrationHarness(t, migrationTestResource(nil), migrationStack(t, nil)...)
		h.reconcile()
		h.finish("team-4e01bc49-seed-0", 0, `{"phase": "seeded"}`)
		h.reconcile()
		if err := h.kube.Delete(ctx, h.migration()); err != nil {
			t.Fatal(err)
		}
		h.reconcile()
		m := h.migration()
		if m.Status.Phase != oramav1alpha1.MigrationPhaseRemoving || m.Status.JobName != "team-4e01bc49-remove-0" {
			t.Fatalf("status = %s %s", m.Status.Phase, m.Status.JobName)
		}
		if args := h.job("team-4e01bc49-remove-0").Spec.Template.Spec.Containers[0].Args; !reflect.DeepEqual(args, []string{"remove-target", "--apply"}) {
			t.Fatalf("removal args %v", args)
		}
		return h
	}
	runVolume := func(h *migrationHarness) bool {
		err := h.kube.Get(ctx, client.ObjectKey{Namespace: "v1-dev", Name: "team-4e01bc49-runs"}, &corev1.PersistentVolumeClaim{})
		if err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		return err == nil
	}

	t.Run("removed", func(t *testing.T) {
		h := start(t)
		h.finish("team-4e01bc49-remove-0", 0, `{"phase": "removed"}`)
		h.reconcile()
		if runVolume(h) {
			t.Fatal("run volume kept after the removal")
		}
		h.reconcile()
		if err := h.kube.Get(ctx, h.key, &oramav1alpha1.OramaCloudV1Migration{}); !apierrors.IsNotFound(err) {
			t.Fatalf("migration kept after its run volume went: %v", err)
		}
	})
	t.Run("switched", func(t *testing.T) {
		h := start(t)
		h.finish("team-4e01bc49-remove-0", 3, `{"phase": "switched", "target_write_active": true}`)
		h.reconcile()
		h.reconcile()
		m := h.migration()
		if m.Status.Phase != oramav1alpha1.MigrationPhaseBlocked || !controllerContainsFinalizer(m) {
			t.Fatalf("a refused removal let go: %s, finalizers %v", m.Status.Phase, m.Finalizers)
		}
		if !runVolume(h) {
			t.Fatal("run volume deleted after a refused removal")
		}
	})
	t.Run("nothing written", func(t *testing.T) {
		h := newMigrationHarness(t, migrationTestResource(func(m *oramav1alpha1.OramaCloudV1Migration) {
			m.Spec.Step = oramav1alpha1.MigrationStepPlan
		}), migrationStack(t, nil)...)
		h.reconcile()
		h.finish("team-4e01bc49-plan-0", 0, `{}`)
		h.reconcile()
		if err := h.kube.Delete(ctx, h.migration()); err != nil {
			t.Fatal(err)
		}
		h.reconcile()
		h.reconcile()
		if jobs := h.jobs(); !reflect.DeepEqual(jobs, []string{"team-4e01bc49-plan-0"}) {
			t.Fatalf("a plan-only migration ran %v on deletion", jobs)
		}
		if err := h.kube.Get(ctx, h.key, &oramav1alpha1.OramaCloudV1Migration{}); !apierrors.IsNotFound(err) {
			t.Fatalf("migration kept: %v", err)
		}
	})
}

// sameJSON reports whether two JSON documents hold the same value.
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatal(err)
	}
	return reflect.DeepEqual(x, y)
}

// The result is stored as the API server would re-encode it, so reading it back changes nothing.
func TestMigrationResultIsStoredAsTheAPIServerEncodesIt(t *testing.T) {
	got := migrationResult(`{"version": 1, "counts": {"documents": 12345678901234567890}, "lag_seconds": 4.50}` + "\n")
	if got == nil || string(got.Raw) != `{"counts":{"documents":12345678901234567890},"lag_seconds":4.50,"version":1}` {
		t.Fatalf("result = %v", got)
	}
	for _, notAnObject := range []string{"", "Traceback", "[1]", "null", `{"a":1} trailing`} {
		if migrationResult(notAnObject) != nil {
			t.Errorf("%q became a result", notAnObject)
		}
	}
}

func controllerContainsFinalizer(m *oramav1alpha1.OramaCloudV1Migration) bool {
	for _, f := range m.Finalizers {
		if f == migrationFinalizer {
			return true
		}
	}
	return false
}

func TestImageRevision(t *testing.T) {
	for ref, want := range map[string]string{
		"oramasearch/oramacore-cloud-v1:v1.2.39-ready-9d8ed373": "v1.2.39-ready-9d8ed373",
		"registry:5000/oramacore:v1.2.39@sha256:abc":            "v1.2.39",
		"registry:5000/oramacore@sha256:abc":                    "sha256:abc",
		"oramasearch/oramacore":                                 "latest",
	} {
		if got := imageRevision(ref); got != want {
			t.Errorf("imageRevision(%q) = %q, want %q", ref, got, want)
		}
	}
}

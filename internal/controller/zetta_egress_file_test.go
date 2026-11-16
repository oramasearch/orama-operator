// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The usage of zetta PR #1510 lists ZETTA_EGRESS_ALLOW_FILE; older images do not.
const (
	testZettaUsageWithFile    = "Environment:\n  ZETTA_EGRESS_ALLOW -> --egress-allow\n  ZETTA_EGRESS_ALLOW_FILE -> --egress-allow-file\n"
	testZettaUsageWithoutFile = "Environment:\n  ZETTA_EGRESS_ALLOW -> --egress-allow\n"
)

// operator#724: whether an image re-reads the allowlist file is what its own usage says. Only a
// clean `zetta --help` that names the variable as a whole word counts; anything else keeps the
// variable, the form every image understands.
func TestZettaImageFeaturesComeFromTheImagesUsage(t *testing.T) {
	for name, check := range map[string]struct {
		log  string
		exit int32
		want bool
	}{
		"lists the file":            {testZettaUsageWithFile, 0, true},
		"an older usage":            {testZettaUsageWithoutFile, 0, false},
		"an unknown option":         {"zetta: unknown option '--help'\n" + testZettaUsageWithFile, 2, false},
		"a longer name":             {"ZETTA_EGRESS_ALLOW_FILES -> --x\n", 0, false},
		"no output":                 {"", 0, false},
		"crashed after printing it": {testZettaUsageWithFile, 139, false},
	} {
		features := parseZettaImageFeatures("oramasearch/zetta:new", check.log, check.exit)
		if features.Image != "oramasearch/zetta:new" || features.EgressAllowFile != check.want {
			t.Errorf("%s: features = %+v, want egressAllowFile %v", name, features, check.want)
		}
	}
}

// An answer counts only for the image it was read from.
func TestZettaEgressFileOnlyForTheImageThatSaidSo(t *testing.T) {
	z := newZettaTestCluster()
	z.Spec.Image = "oramasearch/zetta:new"
	yes := &oramav1alpha1.ZettaImageFeatures{Image: "oramasearch/zetta:new", EgressAllowFile: true}
	if !zettaImageReadsEgressFile(z, yes) {
		t.Fatal("the image said it reads the file")
	}
	for name, features := range map[string]*oramav1alpha1.ZettaImageFeatures{
		"no answer":     nil,
		"another image": {Image: "oramasearch/zetta:old", EgressAllowFile: true},
		"it does not":   {Image: "oramasearch/zetta:new"},
	} {
		if zettaImageReadsEgressFile(z, features) {
			t.Errorf("%s: an image must not lose ZETTA_EGRESS_ALLOW", name)
		}
	}
}

func renderZettaWithEgress(t *testing.T, z *oramav1alpha1.OramaZettaCluster, render zettaEgressRender) *appsv1.StatefulSet {
	t.Helper()
	r := &OramaZettaClusterReconciler{Scheme: zettaTestScheme(t)}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta", Namespace: "orama-system"}}
	if err := r.mutateZettaStatefulSetWithEgress(z, sts, "z0-zetta-headless", "z0-zetta", zettaLabels(z), "", nil, render); err != nil {
		t.Fatalf("mutateZettaStatefulSetWithEgress: %v", err)
	}
	return sts
}

// An image that re-reads the file gets an empty ZETTA_EGRESS_ALLOW and the ConfigMap mounted where
// ZETTA_EGRESS_ALLOW_FILE points, so a new allowlist changes nothing in the pod template: no restart.
func TestAnImageThatReadsTheFileIsNotRolledByARuleChange(t *testing.T) {
	z := newZettaTestCluster()
	z.Spec.EgressAllowConfigMap = "z0-zetta-egress"
	before := renderZettaWithEgress(t, z, zettaEgressRender{configMap: "z0-zetta-egress", allow: "api.example.com:443", file: true})
	after := renderZettaWithEgress(t, z, zettaEgressRender{configMap: "z0-zetta-egress", allow: "api.example.com:443,*:443", file: true})
	if !reflect.DeepEqual(before.Spec.Template, after.Spec.Template) {
		t.Fatal("a rule change rolled a Zetta that re-reads its allowlist file")
	}
	c := before.Spec.Template.Spec.Containers[0]
	if got := findEnv(c.Env, "ZETTA_EGRESS_ALLOW"); got == nil || got.Value != "" {
		t.Fatalf("ZETTA_EGRESS_ALLOW = %+v, want empty: the file is the whole allowlist", got)
	}
	if got := findEnv(c.Env, "ZETTA_EGRESS_ALLOW_FILE"); got == nil || got.Value != "/etc/zetta/egress/allow" {
		t.Fatalf("ZETTA_EGRESS_ALLOW_FILE = %+v", got)
	}
	var mount *corev1.VolumeMount
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == "egress-allow" {
			mount = &c.VolumeMounts[i]
		}
	}
	if mount == nil || !mount.ReadOnly || mount.MountPath != "/etc/zetta/egress" || strings.HasPrefix(mount.MountPath, zettaDataDir) {
		t.Fatalf("egress-allow mount = %+v", mount)
	}
	var volume *corev1.Volume
	for i := range before.Spec.Template.Spec.Volumes {
		if before.Spec.Template.Spec.Volumes[i].Name == "egress-allow" {
			volume = &before.Spec.Template.Spec.Volumes[i]
		}
	}
	if volume == nil || volume.ConfigMap == nil || volume.ConfigMap.Name != "z0-zetta-egress" ||
		len(volume.ConfigMap.Items) != 1 || volume.ConfigMap.Items[0].Key != "allow" || volume.ConfigMap.Items[0].Path != "allow" {
		t.Fatalf("egress-allow volume = %+v", volume)
	}
}

// Any other image (or one not asked yet) keeps the ConfigMap's value as ZETTA_EGRESS_ALLOW, exactly
// as before, so it never loses an allowance; the file is mounted for it too.
func TestAnyOtherImageKeepsTheAllowlistInItsEnvironment(t *testing.T) {
	z := newZettaTestCluster()
	z.Spec.EgressAllowConfigMap = "z0-zetta-egress"
	sts := renderZettaWithEgress(t, z, zettaEgressRender{configMap: "z0-zetta-egress", allow: "api.example.com:443"})
	c := sts.Spec.Template.Spec.Containers[0]
	if got := findEnv(c.Env, "ZETTA_EGRESS_ALLOW"); got == nil || got.Value != "api.example.com:443" {
		t.Fatalf("ZETTA_EGRESS_ALLOW = %+v, want the ConfigMap's value", got)
	}
	if got := findEnv(c.Env, "ZETTA_EGRESS_ALLOW_FILE"); got == nil {
		t.Fatal("the file is mounted and named for every image")
	}
}

// A ZETTA_EGRESS_ALLOW stated in spec.env is kept and appears once; without spec.egressAllowConfigMap
// nothing is mounted.
func TestTheEgressFileKeepsAStatedAllowlistAndIsOptIn(t *testing.T) {
	z := newZettaTestCluster()
	z.Spec.EgressAllowConfigMap = "z0-zetta-egress"
	z.Spec.Env = []corev1.EnvVar{{Name: "ZETTA_EGRESS_ALLOW", Value: "*.example.com:443"}}
	sts := renderZettaWithEgress(t, z, zettaEgressRender{configMap: "z0-zetta-egress", allow: "other:443", file: true})
	count := 0
	for _, variable := range sts.Spec.Template.Spec.Containers[0].Env {
		if variable.Name == "ZETTA_EGRESS_ALLOW" {
			count++
			if variable.Value != "*.example.com:443" {
				t.Errorf("a stated allowlist was replaced: %q", variable.Value)
			}
		}
	}
	if count != 1 {
		t.Fatalf("ZETTA_EGRESS_ALLOW appears %d times", count)
	}

	plain := renderZettaWithEgress(t, newZettaTestCluster(), zettaEgressRender{})
	if got := findEnv(plain.Spec.Template.Spec.Containers[0].Env, "ZETTA_EGRESS_ALLOW_FILE"); got != nil {
		t.Fatalf("a cluster without spec.egressAllowConfigMap got %+v", got)
	}
	for _, volume := range plain.Spec.Template.Spec.Volumes {
		if volume.Name == "egress-allow" {
			t.Fatal("a cluster without spec.egressAllowConfigMap got the egress volume")
		}
	}
	if _, ignored := zettaCustomEnv([]corev1.EnvVar{{Name: "ZETTA_EGRESS_ALLOW_FILE", Value: "/data/x"}}); len(ignored) != 1 {
		t.Fatal("spec.env must not point ZETTA_EGRESS_ALLOW_FILE elsewhere")
	}
}

// Until the named ConfigMap exists the StatefulSet is held, so no node starts with an allowlist it
// was not given.
func TestTheStatefulSetWaitsForTheEgressConfigMap(t *testing.T) {
	scheme := zettaTestScheme(t)
	z := newZettaTestCluster()
	z.Spec.EgressAllowConfigMap = "z0-zetta-egress"
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(z).Build()
	r := &OramaZettaClusterReconciler{Client: cl, Scheme: scheme}
	state, err := r.reconcileZettaEgressFile(context.Background(), z)
	if err != nil || !state.hold {
		t.Fatalf("state = %+v, %v; want held", state, err)
	}
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta-egress", Namespace: z.Namespace},
		Data: map[string]string{"allow": "api.example.com:443"}}
	if err := cl.Create(context.Background(), configMap); err != nil {
		t.Fatal(err)
	}
	state, err = r.reconcileZettaEgressFile(context.Background(), z)
	if err != nil || state.hold || state.render.configMap != "z0-zetta-egress" || state.render.allow != "api.example.com:443" || state.render.file {
		t.Fatalf("state = %+v, %v; want the ConfigMap's value in the variable (no answer from the image)", state, err)
	}
}

// The features Job runs `zetta --help` from spec.image with nothing mounted, its answer is kept per
// image, and a new image is asked again.
func TestTheFeaturesJobAsksEachImageOnce(t *testing.T) {
	ctx := context.Background()
	scheme := zettaTestScheme(t)
	z := newZettaTestCluster()
	z.Spec.Image = "oramasearch/zetta:new"
	z.Spec.PullSecretName = "pull"
	z.Spec.EgressAllowConfigMap = "z0-zetta-egress"
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta-egress", Namespace: z.Namespace},
		Data: map[string]string{"allow": ""}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(z, configMap).Build()
	logs := testZettaUsageWithFile
	r := &OramaZettaClusterReconciler{Client: cl, Scheme: scheme,
		PodLogs: func(context.Context, string, string) (string, error) { return logs, nil }}

	state, err := r.reconcileZettaEgressFile(ctx, z)
	if err != nil || state.render.file || state.requeue == 0 {
		t.Fatalf("before the answer: %+v, %v", state, err)
	}
	var job batchv1.Job
	if err := cl.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: "z0-zetta-features"}, &job); err != nil {
		t.Fatalf("features Job: %v", err)
	}
	pod := job.Spec.Template.Spec
	container := pod.Containers[0]
	if container.Image != "oramasearch/zetta:new" || !reflect.DeepEqual(container.Command, []string{"zetta"}) ||
		!reflect.DeepEqual(container.Args, []string{"--help"}) || len(container.Env) != 0 || len(pod.Volumes) != 0 ||
		pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken ||
		len(pod.ImagePullSecrets) != 1 || pod.ImagePullSecrets[0].Name != "pull" || !metav1.IsControlledBy(&job, z) {
		t.Fatalf("features Job = %+v", job.Spec)
	}

	answer := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "z0-zetta-features-abc", Namespace: z.Namespace,
			Labels: map[string]string{"job-name": job.Name}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "features", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}}},
	}
	controlledByJob(&job, answer)
	if err := cl.Create(ctx, answer); err != nil {
		t.Fatal(err)
	}
	state, err = r.reconcileZettaEgressFile(ctx, z)
	if err != nil || !state.render.file || state.features == nil || state.features.Image != "oramasearch/zetta:new" {
		t.Fatalf("after the answer: %+v, %v", state, err)
	}

	// Kept: the same image is not asked again, whatever its pod would say now.
	z.Status.ImageFeatures = state.features
	logs = testZettaUsageWithoutFile
	if state, _ = r.reconcileZettaEgressFile(ctx, z); !state.render.file {
		t.Fatal("a kept answer for the same image was read again")
	}

	// A new image: the old answer is not used, and the Job for the old image is replaced.
	z.Spec.Image = "oramasearch/zetta:old"
	if state, _ = r.reconcileZettaEgressFile(ctx, z); state.render.file {
		t.Fatal("an answer for another image was used")
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(&job), &job); err == nil && job.Annotations[zettaFeaturesImageAnnotation] == "oramasearch/zetta:new" {
		t.Fatal("the Job for the previous image was kept")
	}
}

// controlledByJob makes pod controlled by job, as the Job controller does.
func controlledByJob(job *batchv1.Job, pod *corev1.Pod) {
	isController := true
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &isController,
	}}
}

// The Lake side: the ConfigMap is written controlled by its cluster, a failed namespace read keeps
// the value the cluster runs with (from the ConfigMap, or from the variable of a cluster not moved
// yet), and a cluster that names no ConfigMap has its own removed.
func TestTheLakeWritesTheEgressConfigMap(t *testing.T) {
	ctx := context.Background()
	scheme := zettaTestScheme(t)
	cluster := newZettaTestCluster()
	cluster.Spec.Env = []corev1.EnvVar{{Name: "ZETTA_EGRESS_ALLOW", Value: "old.example.com:443"}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

	if current, known := currentZettaEgressAllow(ctx, cl, cluster); !known || current != "old.example.com:443" {
		t.Fatalf("a cluster still on the variable: current %q, %v", current, known)
	}
	cluster.Spec.EgressAllowConfigMap = zettaEgressConfigMapName(cluster.Name)
	if err := reconcileZettaEgressConfigMap(ctx, cl, scheme, cluster, "api.example.com:443"); err != nil {
		t.Fatal(err)
	}
	var configMap corev1.ConfigMap
	if err := cl.Get(ctx, client.ObjectKey{Namespace: cluster.Namespace, Name: "z0-zetta-egress"}, &configMap); err != nil {
		t.Fatal(err)
	}
	if configMap.Data["allow"] != "api.example.com:443" || !metav1.IsControlledBy(&configMap, cluster) {
		t.Fatalf("ConfigMap = %+v", configMap)
	}
	if current, known := currentZettaEgressAllow(ctx, cl, cluster); !known || current != "api.example.com:443" {
		t.Fatalf("current %q, %v; want the ConfigMap's value", current, known)
	}

	cluster.Spec.EgressAllowConfigMap = ""
	if err := reconcileZettaEgressConfigMap(ctx, cl, scheme, cluster, ""); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(&configMap), &configMap); err == nil {
		t.Fatal("a cluster that names no ConfigMap kept its egress ConfigMap")
	}
}

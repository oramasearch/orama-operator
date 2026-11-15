/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v3/pkg/chart/loader"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const leafName = "example-nonprod"

// fakeLeaf stands in for a managed cluster. It records what it was asked to do.
type fakeLeaf struct {
	operatorRequests []leafOperatorRequest
	operatorState    leafOperatorState
	operatorErr      error
	removedOperator  []string

	releaseRequests []helmReleaseRequest
	releaseState    helmReleaseState
	releaseErr      error
	removedReleases []string
	kubeconfigs     []string
}

func (f *fakeLeaf) Ensure(_ context.Context, kubeconfig []byte, req leafOperatorRequest) (leafOperatorState, error) {
	f.kubeconfigs = append(f.kubeconfigs, string(kubeconfig))
	f.operatorRequests = append(f.operatorRequests, req)
	return f.operatorState, f.operatorErr
}

func (f *fakeLeaf) Remove(_ context.Context, _ []byte, namespace string) error {
	f.removedOperator = append(f.removedOperator, namespace)
	return nil
}

func (f *fakeLeaf) EnsureRelease(_ context.Context, kubeconfig []byte, req helmReleaseRequest) (helmReleaseState, error) {
	f.kubeconfigs = append(f.kubeconfigs, string(kubeconfig))
	f.releaseRequests = append(f.releaseRequests, req)
	return f.releaseState, f.releaseErr
}

func (f *fakeLeaf) RemoveRelease(_ context.Context, _ []byte, namespace, release string) error {
	f.removedReleases = append(f.removedReleases, namespace+"/"+release)
	return nil
}

func leafTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(clusterTestScheme(t)).WithObjects(objects...).
		WithStatusSubresource(&oramav1alpha1.OramaOperator{}, &oramav1alpha1.ClusterHelmRelease{}, &oramav1alpha1.KubernetesCluster{}).
		Build()
}

// readyLeaf is a KubernetesCluster that is Ready, and its kubeconfig Secret.
func readyLeaf() []client.Object {
	cluster := &oramav1alpha1.KubernetesCluster{
		ObjectMeta: metav1.ObjectMeta{Name: leafName},
		Status: oramav1alpha1.KubernetesClusterStatus{
			Phase:               clusterPhaseReady,
			KubeconfigSecretRef: &oramav1alpha1.ClusterSecretReference{Name: leafName + "-kubeconfig", Namespace: "orama-system"},
		},
	}
	kubeconfig := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: leafName + "-kubeconfig", Namespace: "orama-system"},
		Data:       map[string][]byte{kubeconfigSecretKey: []byte("leaf-kubeconfig")},
	}
	return []client.Object{cluster, kubeconfig}
}

func dockerHubSecret() *corev1.Secret {
	auth := base64.StdEncoding.EncodeToString([]byte("registry-user:hub-token"))
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "oramasearch-dockerhub", Namespace: "orama-system"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{"https://index.docker.io/v1/":{"auth":"` + auth + `"}}}`)},
	}
}

func testOramaOperator(version string) *oramav1alpha1.OramaOperator {
	return &oramav1alpha1.OramaOperator{
		ObjectMeta: metav1.ObjectMeta{Name: leafName},
		Spec: oramav1alpha1.OramaOperatorSpec{
			ClusterRef:         leafName,
			Version:            version,
			ImagePullSecretRef: oramav1alpha1.ClusterSecretReference{Name: "oramasearch-dockerhub", Namespace: "orama-system"},
			Values:             &apiextensionsv1.JSON{Raw: []byte(`{"operator":{"replicas":2},"clusterManagement":{"enabled":true}}`)},
		},
	}
}

func reconcileOperator(t *testing.T, r *OramaOperatorReconciler) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: leafName}})
}

func getOperator(t *testing.T, c client.Client) *oramav1alpha1.OramaOperator {
	t.Helper()
	var operator oramav1alpha1.OramaOperator
	if err := c.Get(context.Background(), types.NamespacedName{Name: leafName}, &operator); err != nil {
		t.Fatal(err)
	}
	return &operator
}

// The operator is not installed onto a cluster that is not Ready, and nothing reaches for it.
func TestOramaOperatorWaitsForItsCluster(t *testing.T) {
	leaf := &fakeLeaf{}
	objects := readyLeaf()
	objects[0].(*oramav1alpha1.KubernetesCluster).Status.Phase = clusterPhaseProvisioning
	c := leafTestClient(t, append(objects, testOramaOperator("dev-83242f9"), dockerHubSecret())...)
	r := &OramaOperatorReconciler{Client: c, Scheme: clusterTestScheme(t), Installer: leaf}

	if _, err := reconcileOperator(t, r); err != nil {
		t.Fatalf("waiting is not an error: %v", err)
	}
	operator := getOperator(t, c)
	if operator.Status.Phase != clusterPhasePending || !strings.Contains(operator.Status.Message, "is Provisioning, not Ready") {
		t.Fatalf("phase %q, message %q", operator.Status.Phase, operator.Status.Message)
	}
	if len(leaf.operatorRequests) != 0 {
		t.Fatal("the cluster was reached before it was Ready")
	}
}

// With the cluster Ready the installer is given the chart published for the version, the values
// for a leaf, the pull secret, and the cluster's kubeconfig; the resource then follows the leaf.
func TestOramaOperatorInstallsTheVersionAndFollowsTheLeaf(t *testing.T) {
	leaf := &fakeLeaf{operatorState: leafOperatorState{
		InstalledVersion: "dev-83242f9", ChartVersion: "0.0.0-dev-83242f9", Revision: 1, ReleaseStatus: "deployed",
		Image: "oramasearch/orama-operator:dev-83242f9", Replicas: 2, ReadyReplicas: 0,
		PodProblem: "container manager ImagePullBackOff: pull access denied",
	}}
	c := leafTestClient(t, append(readyLeaf(), testOramaOperator("dev-83242f9"), dockerHubSecret())...)
	r := &OramaOperatorReconciler{Client: c, Scheme: clusterTestScheme(t), Installer: leaf}

	result, err := reconcileOperator(t, r)
	if err != nil || result.RequeueAfter != leafOperatorPollInterval {
		t.Fatalf("an operator that is starting is looked at every %s: got %v, %v", leafOperatorPollInterval, result, err)
	}
	req := leaf.operatorRequests[0]
	if leaf.kubeconfigs[0] != "leaf-kubeconfig" {
		t.Fatalf("reached the cluster with %q", leaf.kubeconfigs[0])
	}
	if req.Namespace != "orama-system" || req.Chart.Repository != defaultLeafChartRepository || req.Chart.Version != "0.0.0-dev-83242f9" {
		t.Fatalf("request: %+v", req)
	}
	if req.Chart.User != "registry-user" || req.Chart.Pass != "hub-token" || req.PullSecret.Name != "oramasearch-dockerhub" {
		t.Fatalf("registry credentials not taken from the pull secret: user %q", req.Chart.User)
	}
	operatorValues := req.Values["operator"].(map[string]any)
	image := operatorValues["image"].(map[string]any)
	if image["repository"] != "oramasearch/orama-operator" || image["tag"] != "dev-83242f9" || operatorValues["replicas"] != float64(2) {
		t.Fatalf("values: %+v", req.Values)
	}
	if req.Values["clusterManagement"].(map[string]any)["enabled"] != true {
		t.Fatal("spec.values must pass through untouched apart from the image and pull secret: the operator is the same on every cluster")
	}

	operator := getOperator(t, c)
	if operator.Status.Phase != operatorPhaseInstalling || operator.Status.InstalledVersion != "dev-83242f9" || operator.Status.Revision != 1 {
		t.Fatalf("status: %+v", operator.Status)
	}
	if want := "0 of 2 operator pods are ready on the cluster: container manager ImagePullBackOff: pull access denied"; operator.Status.Message != want {
		t.Fatalf("the pod's own error is not reported:\n got %q\nwant %q", operator.Status.Message, want)
	}

	leaf.operatorState.ReadyReplicas, leaf.operatorState.PodProblem = 2, ""
	if result, err = reconcileOperator(t, r); err != nil || result.RequeueAfter != leafOperatorSteadyRecheck {
		t.Fatalf("a ready operator is rechecked every %s: got %v, %v", leafOperatorSteadyRecheck, result, err)
	}
	operator = getOperator(t, c)
	if operator.Status.Phase != clusterPhaseReady || operator.Status.Message != "" || !apimeta.IsStatusConditionTrue(operator.Status.Conditions, clusterConditionReady) {
		t.Fatalf("status when ready: %+v", operator.Status)
	}
}

// An operator is not installed onto a cluster that lacks what it needs to start. The resource
// waits and names what is missing.
func TestOramaOperatorIsNotInstalledWithoutItsPrerequisites(t *testing.T) {
	leaf := &fakeLeaf{operatorState: leafOperatorState{MissingCRDs: []string{"clusters.postgresql.cnpg.io", "scheduledbackups.postgresql.cnpg.io"}}}
	c := leafTestClient(t, append(readyLeaf(), testOramaOperator("v0.2.0"), dockerHubSecret())...)
	r := &OramaOperatorReconciler{Client: c, Scheme: clusterTestScheme(t), Installer: leaf}

	result, err := reconcileOperator(t, r)
	if err != nil || result.RequeueAfter != leafOperatorPollInterval {
		t.Fatalf("waiting for prerequisites is not an error and is rechecked: got %v, %v", result, err)
	}
	if leaf.operatorRequests[0].Chart.Version != "0.2.0" {
		t.Fatalf("a release version v0.2.0 has the chart 0.2.0, asked for %q", leaf.operatorRequests[0].Chart.Version)
	}
	operator := getOperator(t, c)
	ready := apimeta.FindStatusCondition(operator.Status.Conditions, clusterConditionReady)
	if operator.Status.Phase != clusterPhasePending || ready.Reason != "WaitingForPrerequisites" ||
		!strings.Contains(operator.Status.Message, "clusters.postgresql.cnpg.io, scheduledbackups.postgresql.cnpg.io") ||
		!strings.Contains(operator.Status.Message, "not installed") {
		t.Fatalf("phase %q reason %q message %q", operator.Status.Phase, ready.Reason, operator.Status.Message)
	}
}

// What the leaf, Helm or the registry refuses is recorded and returned as an error.
func TestOramaOperatorReportsAndReturnsAnInstallError(t *testing.T) {
	refused := errors.New("the chart registry-1.docker.io/oramasearch/orama-operator-chart:0.0.0-dev-1 cannot be pulled: not found")
	leaf := &fakeLeaf{operatorErr: refused}
	c := leafTestClient(t, append(readyLeaf(), testOramaOperator("dev-1"), dockerHubSecret())...)
	r := &OramaOperatorReconciler{Client: c, Scheme: clusterTestScheme(t), Installer: leaf}

	if _, err := reconcileOperator(t, r); !errors.Is(err, refused) {
		t.Fatalf("want the installer's error returned, got %v", err)
	}
	operator := getOperator(t, c)
	if operator.Status.Phase != clusterPhaseDegraded || operator.Status.Message != refused.Error() {
		t.Fatalf("phase %q message %q", operator.Status.Phase, operator.Status.Message)
	}

	// A pull secret that is absent is named, before the cluster is touched.
	c = leafTestClient(t, append(readyLeaf(), testOramaOperator("dev-1"))...)
	leaf = &fakeLeaf{}
	r = &OramaOperatorReconciler{Client: c, Scheme: clusterTestScheme(t), Installer: leaf}
	if _, err := reconcileOperator(t, r); err == nil || !strings.Contains(err.Error(), "orama-system/oramasearch-dockerhub does not exist") {
		t.Fatalf("want the missing pull secret named, got %v", err)
	}
	if len(leaf.operatorRequests) != 0 {
		t.Fatal("the cluster was reached without a pull secret")
	}
}

// Deleting the resource uninstalls the operator from a cluster that is still there, and simply
// lets go of one that is gone.
func TestOramaOperatorDeletionUninstallsFromAClusterThatStillExists(t *testing.T) {
	for _, clusterExists := range []bool{true, false} {
		leaf := &fakeLeaf{operatorState: leafOperatorState{Replicas: 1, ReadyReplicas: 1, Image: "oramasearch/orama-operator:dev-1"}}
		objects := []client.Object{testOramaOperator("dev-1"), dockerHubSecret()}
		objects = append(objects, readyLeaf()...)
		c := leafTestClient(t, objects...)
		r := &OramaOperatorReconciler{Client: c, Scheme: clusterTestScheme(t), Installer: leaf}
		if _, err := reconcileOperator(t, r); err != nil {
			t.Fatal(err)
		}
		if !clusterExists {
			if err := c.Delete(context.Background(), &oramav1alpha1.KubernetesCluster{ObjectMeta: metav1.ObjectMeta{Name: leafName}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := c.Delete(context.Background(), getOperator(t, c)); err != nil {
			t.Fatal(err)
		}
		if _, err := reconcileOperator(t, r); err != nil {
			t.Fatal(err)
		}
		var gone oramav1alpha1.OramaOperator
		if err := c.Get(context.Background(), types.NamespacedName{Name: leafName}, &gone); !apierrors.IsNotFound(err) {
			t.Fatalf("cluster exists %t: the resource was not released: %v", clusterExists, err)
		}
		if uninstalled := len(leaf.removedOperator) == 1; uninstalled != clusterExists {
			t.Fatalf("cluster exists %t: uninstalled %t", clusterExists, uninstalled)
		}
	}
}

func testRelease(name string, dependsOn ...string) *oramav1alpha1.ClusterHelmRelease {
	return &oramav1alpha1.ClusterHelmRelease{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: oramav1alpha1.ClusterHelmReleaseSpec{
			ClusterRef: leafName, ReleaseName: strings.TrimPrefix(name, "nonprod-"), Namespace: name + "-system",
			Chart:     oramav1alpha1.ClusterHelmChart{Repository: "https://cloudnative-pg.github.io/charts", Name: "cloudnative-pg", Version: "0.29.0"},
			Values:    &apiextensionsv1.JSON{Raw: []byte(`{"replicaCount":1}`)},
			DependsOn: dependsOn,
		},
	}
}

func reconcileRelease(t *testing.T, r *ClusterHelmReleaseReconciler, name string) (ctrl.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
}

func getRelease(t *testing.T, c client.Client, name string) *oramav1alpha1.ClusterHelmRelease {
	t.Helper()
	var rel oramav1alpha1.ClusterHelmRelease
	if err := c.Get(context.Background(), types.NamespacedName{Name: name}, &rel); err != nil {
		t.Fatal(err)
	}
	return &rel
}

// A release is installed at its pinned version, reports each workload that is not rolled out
// with what its pods say, and becomes Ready when they all are.
func TestClusterHelmReleaseInstallsAndReportsItsWorkloads(t *testing.T) {
	leaf := &fakeLeaf{releaseState: helmReleaseState{
		ChartVersion: "0.29.0", AppVersion: "1.30.0", Revision: 1, Status: "deployed", Workloads: 1,
		NotReady: []string{"Deployment cnpg-system/cnpg-cloudnative-pg has 0 of 1 ready (pod x Unschedulable: 0/1 nodes are available)"},
	}}
	c := leafTestClient(t, append(readyLeaf(), testRelease("nonprod-cnpg"))...)
	r := &ClusterHelmReleaseReconciler{Client: c, Scheme: clusterTestScheme(t), Installer: leaf}

	result, err := reconcileRelease(t, r, "nonprod-cnpg")
	if err != nil || result.RequeueAfter != leafOperatorPollInterval {
		t.Fatalf("got %v, %v", result, err)
	}
	req := leaf.releaseRequests[0]
	if req.Release != "cnpg" || req.Namespace != "nonprod-cnpg-system" || req.Chart.Version != "0.29.0" || req.Chart.Name != "cloudnative-pg" || req.Values["replicaCount"] != float64(1) {
		t.Fatalf("request: %+v", req)
	}
	// A release without imagePullSecretRef installs exactly as before: no Secret is written.
	if req.PullSecret != nil {
		t.Fatalf("a release without imagePullSecretRef carried a pull Secret: %+v", req.PullSecret)
	}
	rel := getRelease(t, c, "nonprod-cnpg")
	if rel.Status.Phase != operatorPhaseInstalling || !strings.Contains(rel.Status.Message, "0 of 1 ready (pod x Unschedulable") || rel.Status.AppVersion != "1.30.0" {
		t.Fatalf("status: %+v", rel.Status)
	}

	leaf.releaseState.NotReady, leaf.releaseState.ReadyWorkloads = nil, 1
	if result, err = reconcileRelease(t, r, "nonprod-cnpg"); err != nil || result.RequeueAfter != leafOperatorSteadyRecheck {
		t.Fatalf("got %v, %v", result, err)
	}
	if rel = getRelease(t, c, "nonprod-cnpg"); rel.Status.Phase != clusterPhaseReady || rel.Status.ReadyWorkloads != 1 || rel.Status.Message != "" {
		t.Fatalf("status when ready: %+v", rel.Status)
	}

	leaf.releaseErr = errors.New("helm upgrade of cnpg on the cluster failed: admission webhook denied the request")
	if _, err = reconcileRelease(t, r, "nonprod-cnpg"); !errors.Is(err, leaf.releaseErr) {
		t.Fatalf("want Helm's error returned, got %v", err)
	}
	if rel = getRelease(t, c, "nonprod-cnpg"); rel.Status.Phase != clusterPhaseDegraded || rel.Status.Message != leaf.releaseErr.Error() {
		t.Fatalf("status after an error: %+v", rel.Status)
	}
}

// A release waits for what it depends on, and what is depended on cannot be uninstalled first.
func TestClusterHelmReleaseOrdersInstallAndUninstallByDependsOn(t *testing.T) {
	leaf := &fakeLeaf{releaseState: helmReleaseState{ChartVersion: "0.29.0", Status: "deployed"}}
	c := leafTestClient(t, append(readyLeaf(), testRelease("nonprod-cert-manager"), testRelease("nonprod-k8ssandra", "nonprod-cert-manager"))...)
	r := &ClusterHelmReleaseReconciler{Client: c, Scheme: clusterTestScheme(t), Installer: leaf}

	if _, err := reconcileRelease(t, r, "nonprod-k8ssandra"); err != nil {
		t.Fatalf("waiting on a dependency is not an error: %v", err)
	}
	if rel := getRelease(t, c, "nonprod-k8ssandra"); rel.Status.Phase != clusterPhasePending || !strings.Contains(rel.Status.Message, "nonprod-cert-manager, which this depends on, is Pending, not Ready") {
		t.Fatalf("status: %+v", rel.Status)
	}
	if len(leaf.releaseRequests) != 0 {
		t.Fatal("installed before its dependency")
	}

	for _, name := range []string{"nonprod-cert-manager", "nonprod-k8ssandra"} {
		if _, err := reconcileRelease(t, r, name); err != nil {
			t.Fatal(err)
		}
		if rel := getRelease(t, c, name); rel.Status.Phase != clusterPhaseReady {
			t.Fatalf("%s: %+v", name, rel.Status)
		}
	}

	// Deleting the dependency first is held, with the reason.
	if err := c.Delete(context.Background(), getRelease(t, c, "nonprod-cert-manager")); err != nil {
		t.Fatal(err)
	}
	if _, err := reconcileRelease(t, r, "nonprod-cert-manager"); err != nil {
		t.Fatal(err)
	}
	held := getRelease(t, c, "nonprod-cert-manager")
	if held.Status.Phase != clusterPhaseDeleting || !strings.Contains(held.Status.Message, "nonprod-k8ssandra") || len(leaf.removedReleases) != 0 {
		t.Fatalf("a release others depend on was uninstalled: %+v, removed %v", held.Status, leaf.removedReleases)
	}
	if err := c.Delete(context.Background(), getRelease(t, c, "nonprod-k8ssandra")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"nonprod-k8ssandra", "nonprod-cert-manager"} {
		if _, err := reconcileRelease(t, r, name); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(leaf.removedReleases, ","); got != "nonprod-k8ssandra-system/k8ssandra,nonprod-cert-manager-system/cert-manager" {
		t.Fatalf("uninstall order: %s", got)
	}
}

func TestLeafChartVersionFollowsTheImageTag(t *testing.T) {
	for tag, want := range map[string]string{
		"v0.1.0": "0.1.0", "v1.2.3-rc.1": "1.2.3-rc.1", "dev-83242f9": "0.0.0-dev-83242f9", "main-4b6a37d": "0.0.0-main-4b6a37d", "feature_x.1": "0.0.0-feature-x-1",
	} {
		if got := leafChartVersion(tag); got != want {
			t.Errorf("%s: got %s, want %s", tag, got, want)
		}
	}
}

func TestValuesAreComparedAsHelmStoresThem(t *testing.T) {
	if !sameValues(map[string]any{"a": 1, "b": map[string]any{"c": []any{"x"}}}, map[string]any{"a": float64(1), "b": map[string]any{"c": []any{"x"}}}) {
		t.Fatal("an int and the float64 Helm returns for it are the same value")
	}
	if !sameValues(nil, map[string]any{}) || sameValues(map[string]any{"a": 1}, map[string]any{"a": 2}) {
		t.Fatal("empty equals nil, and different values differ")
	}
}

func TestRegistryCredentialsAreReadFromAPullSecret(t *testing.T) {
	user, pass, err := registryCredentials(dockerHubSecret(), "registry-1.docker.io")
	if err != nil || user != "registry-user" || pass != "hub-token" {
		t.Fatalf("Docker Hub is known by index.docker.io too: %q %q %v", user, pass, err)
	}
	if _, _, err := registryCredentials(dockerHubSecret(), "ghcr.io"); err == nil || !strings.Contains(err.Error(), "no credentials for registry ghcr.io") {
		t.Fatalf("want the registry named, got %v", err)
	}
	if _, _, err := registryCredentials(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "y"}}, "ghcr.io"); err == nil || !strings.Contains(err.Error(), "kubernetes.io/dockerconfigjson") {
		t.Fatalf("want the expected Secret type named, got %v", err)
	}
}

func TestPodProblemSaysWhyPodsAreNotReady(t *testing.T) {
	pods := []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "orama-operator-1"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "0/1 nodes are available"}},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "manager", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff", Message: "back-off 40s"}},
					LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}},
				{Name: "starting", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}}},
			},
		},
	}}
	got := podProblem(pods)
	for _, want := range []string{"pod orama-operator-1 Unschedulable: 0/1 nodes are available", "container manager CrashLoopBackOff: back-off 40s (last exit 1 Error"} {
		if !strings.Contains(got, want) {
			t.Fatalf("lacks %q: %s", want, got)
		}
	}
	if strings.Contains(got, "ContainerCreating") {
		t.Fatalf("a container that is simply starting is not a problem: %s", got)
	}
}

// The chart declares what this operator version needs on a cluster before it can start, and the
// declaration covers CloudNativePG's ScheduledBackup, the kind the Lake controller watches: a
// manager on a cluster without that CRD does not start.
func TestTheChartDeclaresTheCRDsTheManagerNeedsToStart(t *testing.T) {
	chrt, err := loader.LoadDir("../../chart")
	if err != nil {
		t.Fatal(err)
	}
	required := chartRequiredCRDs(chrt)
	has := func(name string) bool {
		for _, r := range required {
			if r == name {
				return true
			}
		}
		return false
	}
	for _, gvk := range []struct{ plural, group string }{
		{"scheduledbackups", cnpgScheduledBackupGVK.Group}, {"clusters", cnpgClusterGVK.Group}, {"backups", cnpgBackupGVK.Group},
		{"certificates", "cert-manager.io"}, {"issuers", "cert-manager.io"},
	} {
		if name := gvk.plural + "." + gvk.group; !has(name) {
			t.Errorf("chart/Chart.yaml annotation %s does not list %s", requiredCRDsAnnotation, name)
		}
	}
	// The chart carries the management kinds: the operator installed on a leaf is the same one,
	// and that cluster can manage others.
	shipped := map[string]bool{}
	for _, crd := range chrt.CRDObjects() {
		shipped[crd.Name] = true
	}
	for _, name := range []string{"crds/orama.cloud_kubernetesclusters.yaml", "crds/orama.cloud_oramaoperators.yaml", "crds/orama.cloud_clusterhelmreleases.yaml"} {
		if !shipped[name] {
			t.Errorf("the chart does not ship %s; have %v", name, shipped)
		}
	}
}

// Helm puts a resource whose manifest names no namespace in the kubeconfig's default namespace,
// so the release's namespace must be that default. K8ssandra 1.33.0's Deployments landed in
// `default` on the first leaf because it was not.
func TestTheReleaseNamespaceIsTheKubeconfigsDefault(t *testing.T) {
	kubeconfig := []byte(`apiVersion: v1
kind: Config
current-context: leaf
clusters: [{name: leaf, cluster: {server: "https://leaf.invalid"}}]
users: [{name: admin, user: {token: t}}]
contexts: [{name: leaf, context: {cluster: leaf, user: admin}}]
`)
	getter, _, err := leafClients(kubeconfig, "k8ssandra-operator")
	if err != nil {
		t.Fatal(err)
	}
	namespace, _, err := getter.ToRawKubeConfigLoader().Namespace()
	if err != nil || namespace != "k8ssandra-operator" {
		t.Fatalf("Helm would place un-namespaced resources in %q (%v), not the release's namespace", namespace, err)
	}
}

// A leaf client knows Orama's kinds as well as Kubernetes': the observability agent reads the hub's
// PixieCloud and writes a PixieVizier on leaf clusters.
func TestLeafClientsKnowOramaKinds(t *testing.T) {
	kubeconfig := []byte(`apiVersion: v1
kind: Config
current-context: leaf
clusters: [{name: leaf, cluster: {server: "https://leaf.invalid"}}]
users: [{name: admin, user: {token: t}}]
contexts: [{name: leaf, context: {cluster: leaf, user: admin}}]
`)
	_, c, err := leafClients(kubeconfig, "pl")
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range []client.Object{&oramav1alpha1.PixieCloud{}, &oramav1alpha1.PixieVizier{}, &corev1.Secret{}} {
		if _, err := c.GroupVersionKindFor(obj); err != nil {
			t.Errorf("a leaf client cannot handle %T: %v", obj, err)
		}
	}
}

// A CRD manifest shipped apart from a chart is applied only if it holds nothing but CRDs.
func TestCRDManifestsHoldOnlyCRDs(t *testing.T) {
	c := leafTestClient(t)
	err := applyCRDDocuments(context.Background(), c, "https://example.invalid/crd.yaml",
		[]byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: sneaky\n"))
	if err == nil || !strings.Contains(err.Error(), "only CustomResourceDefinitions are applied") {
		t.Fatalf("want a non-CRD refused, got %v", err)
	}
	if err := applyCRDDocuments(context.Background(), c, "https://example.invalid/empty.yaml", []byte("# nothing\n")); err == nil {
		t.Fatal("a manifest with no CRD is an error")
	}
}

// A chart's hooks get minutes, not the API timeout: a hook Job on a new cluster pulls its image
// alongside everything else. With 30 seconds, the ClickHouse operator's release failed 26 times
// in a row on example-production-us.
func TestHelmWaitsLongEnoughForAChartsHooks(t *testing.T) {
	if leafHookTimeout < 5*time.Minute {
		t.Fatalf("hooks get %s; a hook's image pull on a new cluster needs minutes", leafHookTimeout)
	}
	if leafHookTimeout == leafAPITimeout {
		t.Fatal("the hook timeout is the API timeout again")
	}
}

// A release whose images come from a private registry gets its cluster's pull Secret in its
// namespace (operator#15); a production Secret never reaches a cluster that is not production.
func TestClusterHelmReleaseCopiesItsPullSecretUnderTheProductionRule(t *testing.T) {
	leaf := &fakeLeaf{releaseState: helmReleaseState{Status: "deployed"}}
	rel := testRelease("nonprod-kube-prometheus-stack")
	rel.Spec.ImagePullSecretRef = &oramav1alpha1.ClusterSecretReference{Name: "oramasearch-dockerhub", Namespace: "orama-system"}
	rel.Spec.ImagePullSecretAs = "platform-pull"
	c := leafTestClient(t, append(readyLeaf(), rel, dockerHubSecret())...)
	r := &ClusterHelmReleaseReconciler{Client: c, Scheme: clusterTestScheme(t), Installer: leaf}
	if _, err := reconcileRelease(t, r, rel.Name); err != nil {
		t.Fatal(err)
	}
	if len(leaf.releaseRequests) != 1 || leaf.releaseRequests[0].PullSecret == nil || leaf.releaseRequests[0].PullSecret.Name != "platform-pull" ||
		string(leaf.releaseRequests[0].PullSecret.Data[corev1.DockerConfigJsonKey]) == "" {
		t.Fatalf("the pull Secret was not passed with the release: %+v", leaf.releaseRequests)
	}

	// The same Secret labelled for production is refused for this nonprod-or-untagged cluster.
	production := dockerHubSecret()
	production.Labels = map[string]string{secretPurposeLabel: productionPurpose}
	if err := c.Update(context.Background(), production); err != nil {
		t.Fatal(err)
	}
	leaf.releaseRequests = nil
	if _, err := reconcileRelease(t, r, rel.Name); err != nil {
		t.Fatal(err)
	}
	if len(leaf.releaseRequests) != 0 {
		t.Fatal("a release was installed with a production pull Secret on a cluster that is not production")
	}
	if got := getRelease(t, c, rel.Name); got.Status.Phase != clusterPhaseDegraded || !strings.Contains(got.Status.Message, "never copied") {
		t.Fatalf("status %+v", got.Status)
	}
}

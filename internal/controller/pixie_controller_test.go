/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// applyAsUpsert stands in for server-side apply, which controller-runtime 0.18's fake client does
// not implement: an apply creates the object or replaces it, keeping its status subresource.
var applyAsUpsert = interceptor.Funcs{
	Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		if patch.Type() != types.ApplyPatchType {
			return c.Patch(ctx, obj, patch, opts...)
		}
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())
		err := c.Get(ctx, client.ObjectKeyFromObject(obj), existing)
		if apierrors.IsNotFound(err) {
			obj.SetResourceVersion("")
			return c.Create(ctx, obj)
		}
		if err != nil {
			return err
		}
		obj.SetResourceVersion(existing.GetResourceVersion())
		// An apply never carries status: keep what the cluster reported, as the API server does.
		if u, ok := obj.(*unstructured.Unstructured); ok {
			if status, found := existing.Object["status"]; found {
				u.Object["status"] = status
			}
		}
		return c.Update(ctx, obj)
	},
}

// kubernetesAPIObjects are the default/kubernetes Service and Endpoints a sandbox's API allowance
// is built from.
func kubernetesAPIObjects() []client.Object {
	return []client.Object{
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
			Spec: corev1.ServiceSpec{ClusterIP: "10.0.0.1", Ports: []corev1.ServicePort{{Port: 443}}}},
		&corev1.Endpoints{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes", Namespace: "default"},
			Subsets: []corev1.EndpointSubset{{Addresses: []corev1.EndpointAddress{{IP: "20.1.2.3"}}, Ports: []corev1.EndpointPort{{Port: 443}}}}},
	}
}

// newPixieScheme is the test scheme with RBAC, which the vendored Pixie bundles contain.
func newPixieScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := newLakeScheme(t)
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func newPixieTestClient(t *testing.T, scheme *runtime.Scheme, funcs interceptor.Funcs, objects ...client.Object) client.Client {
	t.Helper()
	if funcs.Patch == nil {
		funcs.Patch = applyAsUpsert.Patch
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(kubernetesAPIObjects(), objects...)...).
		WithStatusSubresource(&oramav1alpha1.PixieCloud{}, &oramav1alpha1.PixieVizier{}, &oramav1alpha1.NetworkSandbox{}).
		WithInterceptorFuncs(funcs).Build()
}

func reconcileTwice(t *testing.T, r interface {
	Reconcile(context.Context, ctrl.Request) (ctrl.Result, error)
}, namespace, name string) {
	t.Helper()
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}); err != nil {
			t.Fatalf("reconcile %s/%s: %v", namespace, name, err)
		}
	}
}

func vizierSecrets() []client.Object {
	return []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pixie-pull", Namespace: "pl"}, Type: corev1.SecretTypeDockerConfigJson,
			Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{}`)}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: vizierDeploySecret, Namespace: "pl"}, Data: map[string][]byte{"deploy-key": []byte("px-dep-x")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pixie-cloud-ca", Namespace: "pl"}, Data: map[string][]byte{"ca.crt": []byte("hub-ca")}},
	}
}

// Without its deploy key and the hub's CA, Vizier waits and runs nothing.
func TestPixieVizierWaitsForItsOwnCredentials(t *testing.T) {
	scheme := newPixieScheme(t)
	vz := testPixieVizier()
	vz.Name = pixieConcreteName
	kube := newPixieTestClient(t, scheme, interceptor.Funcs{}, vz, vizierSecrets()[0])
	r := &PixieVizierReconciler{Client: kube, Scheme: scheme}
	reconcileTwice(t, r, "pl", pixieConcreteName)
	var got oramav1alpha1.PixieVizier
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(vz), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != oramav1alpha1.ObservabilityPhasePending || !strings.Contains(got.Status.Message, "deploy key") {
		t.Fatalf("status %+v", got.Status)
	}
	var daemonSets appsv1.DaemonSetList
	if err := kube.List(context.Background(), &daemonSets); err != nil || len(daemonSets.Items) != 0 {
		t.Fatalf("Vizier ran before its credentials arrived: %v", daemonSets.Items)
	}
}

func TestPixieVizierReconcileIsDeterministicAndKeepsRegistration(t *testing.T) {
	ctx := context.Background()
	scheme := newPixieScheme(t)
	vz := testPixieVizier()
	vz.Name = pixieConcreteName
	kube := newPixieTestClient(t, scheme, interceptor.Funcs{}, append(vizierSecrets(), vz)...)
	r := &PixieVizierReconciler{Client: kube, Scheme: scheme}
	reconcileTwice(t, r, "pl", pixieConcreteName)

	var pem appsv1.DaemonSet
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "pl", Name: "vizier-pem"}, &pem); err != nil {
		t.Fatalf("the node agents were not applied: %v", err)
	}
	if !metav1.IsControlledBy(&pem, vz) && pem.OwnerReferences[0].Name != pixieConcreteName {
		t.Errorf("vizier-pem is not controlled by its PixieVizier")
	}
	var certs corev1.Secret
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "pl", Name: vizierServiceTLSSecret}, &certs); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"server.crt", "server.key", "client.crt", "client.key", "ca.crt"} {
		if len(certs.Data[key]) == 0 {
			t.Errorf("service-tls-certs has no %s", key)
		}
	}
	var sandbox oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "pl", Name: networkSandboxName}, &sandbox); err != nil {
		t.Fatal(err)
	}
	if len(sandbox.Spec.CiliumPolicies) != 1 || sandbox.Spec.CiliumPolicies[0].Name != "allow-node-agents" {
		t.Errorf("the host-network node agents have no allowance: %+v", sandbox.Spec.CiliumPolicies)
	}
	for _, policy := range sandbox.Spec.Policies {
		if policy.Name == "allow-hub-egress" {
			t.Error("a Vizier reaching its hub through a PeerConnection also has a direct path to a hub")
		}
	}

	// The cloud connector registers and writes the cluster's id back; the operator keeps it.
	var cluster corev1.Secret
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "pl", Name: vizierClusterSecret}, &cluster); err != nil {
		t.Fatal(err)
	}
	signingKey := string(cluster.Data["jwt-signing-key"])
	cluster.Data["cluster-id"] = []byte("c0ffee00-0000-0000-0000-000000000001")
	if err := kube.Update(ctx, &cluster); err != nil {
		t.Fatal(err)
	}
	reconcileTwice(t, r, "pl", pixieConcreteName)
	if err := kube.Get(ctx, client.ObjectKeyFromObject(&cluster), &cluster); err != nil {
		t.Fatal(err)
	}
	if string(cluster.Data["cluster-id"]) == "" || string(cluster.Data["jwt-signing-key"]) != signingKey {
		t.Fatal("the operator changed the registration or the signing key")
	}
	var got oramav1alpha1.PixieVizier
	if err := kube.Get(ctx, client.ObjectKeyFromObject(vz), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.ClusterID != "c0ffee00-0000-0000-0000-000000000001" {
		t.Errorf("status.clusterID %q", got.Status.ClusterID)
	}
	if got.Status.Phase != oramav1alpha1.ObservabilityPhaseProgressing || got.Status.Ready == "" {
		t.Errorf("status %+v", got.Status)
	}

	// A steady Vizier writes no status, and its certificates stay.
	statusVersion, certsVersion := got.ResourceVersion, certs.ResourceVersion
	reconcileTwice(t, r, "pl", pixieConcreteName)
	if err := kube.Get(ctx, client.ObjectKeyFromObject(vz), &got); err != nil {
		t.Fatal(err)
	}
	if err := kube.Get(ctx, client.ObjectKeyFromObject(&certs), &certs); err != nil {
		t.Fatal(err)
	}
	if got.ResourceVersion != statusVersion || certs.ResourceVersion != certsVersion {
		t.Error("a steady Vizier rewrote its status or reissued its certificates")
	}
}

// Before the provider's release exists (#455), the cluster's alert routing and rules are written.
func TestPixieVizierWritesAlertRoutingBeforeItsRelease(t *testing.T) {
	ctx := context.Background()
	scheme := newPixieScheme(t)
	vz := testPixieVizier()
	vz.Name = pixieConcreteName
	vz.Spec.Release = nil
	vz.Spec.AlertRoutes = []oramav1alpha1.PixieAlertRoute{{Name: "oncall", Kind: "slack", Severities: []string{"critical"}, SecretName: "alert-oncall"}}
	kube := newPixieTestClient(t, scheme, interceptor.Funcs{}, vz)
	reconcileTwice(t, &PixieVizierReconciler{Client: kube, Scheme: scheme}, "pl", pixieConcreteName)
	routing := &unstructured.Unstructured{}
	routing.SetGroupVersionKind(alertmanagerConfigGVK)
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "pl", Name: observabilityRouting}, routing); err != nil {
		t.Fatalf("no alert routing before the release: %v", err)
	}
	if routing.GetLabels()[alertingLabel] != "true" {
		t.Error("the routing is not selected by the platform's Alertmanager")
	}
	var got oramav1alpha1.PixieVizier
	if err := kube.Get(ctx, client.ObjectKeyFromObject(vz), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != oramav1alpha1.ObservabilityPhasePending || !strings.Contains(got.Status.Message, "#455") {
		t.Fatalf("status %+v", got.Status)
	}
	var daemonSets appsv1.DaemonSetList
	if err := kube.List(ctx, &daemonSets); err != nil || len(daemonSets.Items) != 0 {
		t.Fatal("Vizier ran without a release")
	}
}

// Without the platform layer's monitoring CRDs, Vizier still runs, and says alerting is not set up.
func TestPixieVizierToleratesMissingMonitoringCRDs(t *testing.T) {
	scheme := newPixieScheme(t)
	vz := testPixieVizier()
	vz.Name = pixieConcreteName
	noMatch := func(obj client.Object) error {
		if gvk := obj.GetObjectKind().GroupVersionKind(); gvk.Group == "monitoring.coreos.com" {
			return &apimeta.NoKindMatchError{GroupKind: schema.GroupKind{Group: gvk.Group, Kind: gvk.Kind}}
		}
		return nil
	}
	noMonitoring := interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := noMatch(obj); err != nil {
				return err
			}
			return c.Get(ctx, key, obj, opts...)
		},
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := noMatch(obj); err != nil {
				return err
			}
			return c.Create(ctx, obj, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			if err := noMatch(obj); err != nil {
				return err
			}
			return c.Delete(ctx, obj, opts...)
		},
	}
	kube := newPixieTestClient(t, scheme, noMonitoring, append(vizierSecrets(), vz)...)
	reconcileTwice(t, &PixieVizierReconciler{Client: kube, Scheme: scheme}, "pl", pixieConcreteName)
	var got oramav1alpha1.PixieVizier
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(vz), &got); err != nil {
		t.Fatal(err)
	}
	alerting := apimeta.FindStatusCondition(got.Status.Conditions, pixieAlertingCondition)
	if alerting == nil || alerting.Status != metav1.ConditionFalse || got.Status.Phase == oramav1alpha1.ObservabilityPhaseDegraded {
		t.Fatalf("status %+v", got.Status)
	}
}

// fakeHubBackend stands in for Pixie Cloud's API and the bootstrap's services.
type fakeHubBackend struct {
	ready     bool
	index     string
	admin     [2]string
	org       string
	keys      map[string]string // id -> key, as Pixie holds them
	created   int
	deleted   []string
	connected map[string]bool
}

func (f *fakeHubBackend) Ready(context.Context) (bool, string, error) {
	return f.ready, "migrations running", nil
}
func (f *fakeHubBackend) EnsureIndex(_ context.Context, name string) error {
	f.index = name
	return nil
}
func (f *fakeHubBackend) EnsureAdmin(_ context.Context, email, password string) (string, error) {
	f.admin = [2]string{email, password}
	return "identity-1", nil
}
func (f *fakeHubBackend) EnsureOrg(_ context.Context, name, email, identity string) (pixieIdentity, error) {
	f.org = name + "/" + identity
	return pixieIdentity{OrgID: "org-1", UserID: "user-1", Email: email}, nil
}
func (f *fakeHubBackend) DeployKeyIDs(_ context.Context, as pixieIdentity) (map[string]bool, error) {
	if as.OrgID != "org-1" {
		return nil, apierrors.NewBadRequest("wrong identity")
	}
	ids := map[string]bool{}
	for id := range f.keys {
		ids[id] = true
	}
	return ids, nil
}
func (f *fakeHubBackend) CreateDeployKey(context.Context, pixieIdentity) (pixieDeployKey, error) {
	f.created++
	key := pixieDeployKey{ID: fmt.Sprintf("id-%d", f.created), Key: fmt.Sprintf("px-dep-%d", f.created)}
	f.keys[key.ID] = key.Key
	return key, nil
}
func (f *fakeHubBackend) DeleteDeployKey(_ context.Context, _ pixieIdentity, id string) error {
	f.deleted = append(f.deleted, id)
	delete(f.keys, id)
	return nil
}
func (f *fakeHubBackend) Clusters(context.Context, pixieIdentity) (map[string]pixieAgentState, error) {
	states := map[string]pixieAgentState{}
	for cluster, connected := range f.connected {
		states[cluster] = pixieAgentState{Registered: true, Connected: connected}
	}
	return states, nil
}
func (f *fakeHubBackend) Close() {}

// markPixieWorkloadsReady makes every workload and the database report ready, as a cluster would.
func markPixieWorkloadsReady(t *testing.T, kube client.Client, namespace string) {
	t.Helper()
	ctx := context.Background()
	var deployments appsv1.DeploymentList
	if err := kube.List(ctx, &deployments, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	for i := range deployments.Items {
		d := &deployments.Items[i]
		replicas := int32(1)
		if d.Spec.Replicas != nil {
			replicas = *d.Spec.Replicas
		}
		d.Status = appsv1.DeploymentStatus{ObservedGeneration: d.Generation, Replicas: replicas, UpdatedReplicas: replicas, AvailableReplicas: replicas, ReadyReplicas: replicas}
		if err := kube.Status().Update(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	var statefulSets appsv1.StatefulSetList
	if err := kube.List(ctx, &statefulSets, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	for i := range statefulSets.Items {
		s := &statefulSets.Items[i]
		replicas := int32(1)
		if s.Spec.Replicas != nil {
			replicas = *s.Spec.Replicas
		}
		s.Status = appsv1.StatefulSetStatus{ObservedGeneration: s.Generation, Replicas: replicas, ReadyReplicas: replicas, UpdatedReplicas: replicas}
		if err := kube.Status().Update(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	db := &unstructured.Unstructured{}
	db.SetGroupVersionKind(cnpgClusterGVK)
	if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: pixieDBCluster}, db); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(db.Object, int64(pixieDBInstances), "status", "readyInstances"); err != nil {
		t.Fatal(err)
	}
	if err := kube.Update(ctx, db); err != nil {
		t.Fatal(err)
	}
}

func TestPixieCloudBootstrapIssuesAndRevokesPerClusterKeys(t *testing.T) {
	ctx := context.Background()
	scheme := newPixieScheme(t)
	cloud := testPixieCloud()
	cloud.Name = pixieConcreteName
	cloud.Spec.Agents = []oramav1alpha1.PixieCloudAgent{{Name: "prod2", Cluster: "orama-production-cluster-02"}, {Name: "prod3", Cluster: "orama-production-cluster-03"}}
	// A PeerConnection's allowance in the sandbox survives the hub's own rendering.
	peerAllowance := oramav1alpha1.NetworkSandboxPolicy{Name: "peer-pixie-agent-prod2-gateway-ingress", Comment: "peer",
		Spec: networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}}}
	kube := newPixieTestClient(t, scheme, interceptor.Funcs{},
		cloud,
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pixie-pull", Namespace: "plc"}, Data: map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{}`)}},
		&oramav1alpha1.PeerConnection{ObjectMeta: metav1.ObjectMeta{Name: pixieAgentPeer("prod2"), Namespace: "plc"}, Spec: oramav1alpha1.PeerConnectionSpec{PeerID: testPeerID, Desired: "present"}},
	)
	backend := &fakeHubBackend{keys: map[string]string{}, connected: map[string]bool{"orama-production-cluster-02": true}}
	r := &PixieCloudReconciler{Client: kube, Scheme: scheme, OperatorNamespace: "orama-system",
		Backend: func(pixieHubCredentials) (pixieHubBackend, error) { return backend, nil }}
	reconcileTwice(t, r, "plc", pixieConcreteName)

	// Before the hub is ready, keys are minted but not registered.
	if backend.index != "" || len(backend.keys) != 0 {
		t.Fatal("the bootstrap ran before the hub was ready")
	}
	var sandbox oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "plc", Name: networkSandboxName}, &sandbox); err != nil {
		t.Fatal(err)
	}
	sandbox.Spec.Policies = append(sandbox.Spec.Policies, peerAllowance)
	if err := kube.Update(ctx, &sandbox); err != nil {
		t.Fatal(err)
	}
	markPixieWorkloadsReady(t, kube, "plc")
	reconcileTwice(t, r, "plc", pixieConcreteName)
	backend.ready = true
	reconcileTwice(t, r, "plc", pixieConcreteName)

	var admin corev1.Secret
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "plc", Name: pixieAdminSecret}, &admin); err != nil {
		t.Fatal(err)
	}
	if backend.index != pixieMetadataIndex || backend.admin[0] != "ops@support.example" || backend.admin[1] != string(admin.Data["password"]) || len(admin.Data["password"]) < 32 {
		var status oramav1alpha1.PixieCloud
		_ = kube.Get(ctx, client.ObjectKeyFromObject(cloud), &status)
		t.Fatalf("bootstrap: index %q admin %q; hub %s: %s", backend.index, backend.admin[0], status.Status.Phase, status.Status.Message)
	}
	if backend.org != "orama/identity-1" {
		t.Fatalf("organisation bootstrap %q", backend.org)
	}
	ids := map[string]string{}
	for _, agent := range []string{"prod2", "prod3"} {
		var secret corev1.Secret
		if err := kube.Get(ctx, client.ObjectKey{Namespace: "plc", Name: pixieDeployKeyPrefix + agent}, &secret); err != nil {
			t.Fatal(err)
		}
		id := string(secret.Data["id"])
		if backend.keys[id] == "" || backend.keys[id] != string(secret.Data["deploy-key"]) {
			t.Fatalf("agent %s holds key %q (id %q), which Pixie did not issue", agent, secret.Data["deploy-key"], id)
		}
		ids[agent] = id
	}
	if ids["prod2"] == ids["prod3"] || backend.created != 2 {
		t.Fatalf("keys created %d, ids %v: want one per cluster", backend.created, ids)
	}
	var got oramav1alpha1.PixieCloud
	if err := kube.Get(ctx, client.ObjectKeyFromObject(cloud), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != oramav1alpha1.ObservabilityPhaseReady || got.Status.CACertificate == "" || !got.Status.Agents[0].Connected || got.Status.Agents[1].Connected {
		t.Fatalf("status %+v", got.Status)
	}
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "plc", Name: networkSandboxName}, &sandbox); err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, policy := range sandbox.Spec.Policies {
		kept = kept || policy.Name == peerAllowance.Name
	}
	if !kept {
		t.Error("the hub's rendering dropped its PeerConnection's allowance")
	}

	// Steady: no status write, and every key stays the same.
	version := got.ResourceVersion
	reconcileTwice(t, r, "plc", pixieConcreteName)
	if err := kube.Get(ctx, client.ObjectKeyFromObject(cloud), &got); err != nil {
		t.Fatal(err)
	}
	if got.ResourceVersion != version {
		t.Error("a steady hub rewrote its status")
	}

	if backend.created != 2 {
		t.Errorf("a steady hub created %d more keys", backend.created-2)
	}

	// A rebuilt hub forgets prod2's key: Pixie issues a new one and the Secret takes it.
	delete(backend.keys, ids["prod2"])
	reconcileTwice(t, r, "plc", pixieConcreteName)
	var reissued corev1.Secret
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "plc", Name: pixieDeployKeyPrefix + "prod2"}, &reissued); err != nil {
		t.Fatal(err)
	}
	if string(reissued.Data["id"]) == ids["prod2"] || backend.keys[string(reissued.Data["id"])] == "" {
		t.Fatalf("prod2's key was not reissued: %v", reissued.Data)
	}
	ids["prod2"] = string(reissued.Data["id"])
	if err := kube.Get(ctx, client.ObjectKeyFromObject(cloud), &got); err != nil {
		t.Fatal(err)
	}

	// The managing cluster stops listing prod3: its key is revoked and its Secret deleted.
	got.Spec.Agents = got.Spec.Agents[:1]
	if err := kube.Update(ctx, &got); err != nil {
		t.Fatal(err)
	}
	reconcileTwice(t, r, "plc", pixieConcreteName)
	if len(backend.deleted) != 1 || backend.deleted[0] != ids["prod3"] {
		t.Fatalf("revoked %v, want prod3's key %s", backend.deleted, ids["prod3"])
	}
	var revoked corev1.Secret
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "plc", Name: pixieDeployKeyPrefix + "prod3"}, &revoked); !apierrors.IsNotFound(err) {
		t.Error("the revoked cluster's key Secret remains")
	}
	if backend.keys[ids["prod2"]] == "" {
		t.Error("revoking prod3 touched prod2's key")
	}
}

// A component whose image is not in the registry yet (a mirror or #455 build missing) says so,
// instead of "rolling out" for ever.
func TestPixieVizierNamesAnImageItCannotPull(t *testing.T) {
	ctx := context.Background()
	scheme := newPixieScheme(t)
	vz := testPixieVizier()
	vz.Name = pixieConcreteName
	image := string(vz.Spec.Release.Images["vizier-kelvin"])
	stuck := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "kelvin-0", Namespace: "pl", Labels: map[string]string{pixieComponentLabel: "kelvin"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Image: image,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}}}},
	}
	kube := newPixieTestClient(t, scheme, interceptor.Funcs{}, append(vizierSecrets(), vz, stuck)...)
	reconcileTwice(t, &PixieVizierReconciler{Client: kube, Scheme: scheme}, "pl", pixieConcreteName)
	var got oramav1alpha1.PixieVizier
	if err := kube.Get(ctx, client.ObjectKeyFromObject(vz), &got); err != nil {
		t.Fatal(err)
	}
	for _, component := range got.Status.Components {
		if component.Name == "kelvin" {
			if !strings.Contains(component.Message, "cannot pull image "+image) {
				t.Fatalf("kelvin says %q", component.Message)
			}
			return
		}
	}
	t.Fatal("no kelvin component")
}

// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const k0sDeliveryContract = "cc922054ad10fd1991ab1077bb8a2c3bf3eeeae2"

type environmentAdmissionFixture struct {
	ctx                context.Context
	admin              client.Client
	operator           client.Client
	tenant             client.Client
	workspace          client.Client
	clientset          kubernetes.Interface
	scheme             *runtime.Scheme
	reconciler         *LakeEnvironmentReconciler
	environment        *oramav1alpha1.LakeEnvironment
	namespace          string
	workspaceNamespace string
}

func TestLakeEnvironmentSealedDeliveryAdmission(t *testing.T) {
	f := newEnvironmentAdmissionFixture(t)
	f.assertNamespaceSelection(t)
	source, secret := f.createSource(t, "database", "application-database", "first")
	f.environment.Spec.SecretRefs = []string{"secret://database"}
	f.updateEnvironment(t)
	f.reconcile(t)
	copy, owner := f.assertDelivery(t, source, "first")
	pod := f.admitPod(t, "application", "application-database")
	t.Logf("after Secret: ADMITTED as system:serviceaccount:orama-system:orama-operator; sealed=%q owner=LakeSealedSecret %s/%s UID=%s", copy.Labels[sealedSecretLabel], owner.Namespace, owner.Name, owner.UID)
	t.Logf("after Pod: ADMITTED; sealed-secret-names=%q envFrom=%s", pod.Annotations[environmentSealedNamesAnnotation], pod.Spec.Containers[0].EnvFrom[0].SecretRef.Name)
	f.assertTenantDenials(t, copy)
	f.assertPolicyWriteDenials(t)
	f.assertSteadyDelivery(t, copy, owner)

	secret.Data["DATABASE_URL"] = []byte("rotated")
	if err := f.operator.Update(f.ctx, secret); err != nil {
		t.Fatal(err)
	}
	f.reconcile(t)
	rotated, rotatedOwner := f.assertDelivery(t, source, "rotated")
	if rotated.UID != copy.UID || rotatedOwner.UID != owner.UID {
		t.Fatal("rotation replaced a delivery identity")
	}
	f.admitPod(t, "rotated-application", "application-database")
	t.Log("rotation: Secret UPDATE admitted; local owner UID retained; consuming Pod admitted")

	f.assertReferenceChanges(t, source)
}

func newEnvironmentAdmissionFixture(t *testing.T) *environmentAdmissionFixture {
	t.Helper()
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for native admission envtest")
	}
	server := &envtest.Environment{
		BinaryAssetsDirectory: assets,
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	server.ControlPlane.GetAPIServer().Configure().
		Set("authorization-mode", "RBAC").
		Set("enable-admission-plugins", "ValidatingAdmissionPolicy")
	config, err := server.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})
	f := &environmentAdmissionFixture{ctx: context.Background(), scheme: newLakeEnvironmentScheme(t)}
	if err := nodev1.AddToScheme(f.scheme); err != nil {
		t.Fatal(err)
	}
	f.admin = admissionClient(t, config, f.scheme)
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	f.clientset = clientset
	f.installOperatorIdentity(t, config)
	f.createEnvironment(t)
	f.initializeAbsentControllers(t)
	f.installAdmissionContract(t, server, config)
	return f
}

func (f *environmentAdmissionFixture) installOperatorIdentity(t *testing.T, config *rest.Config) {
	t.Helper()
	f.create(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orama-system"}})
	f.create(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "lake"}})
	chart, err := exec.Command("helm", "template", "orama-operator", "../../chart", "--namespace", "orama-system", "--show-only", "templates/rbac.yaml").Output()
	if err != nil {
		t.Fatalf("render operator RBAC: %v", err)
	}
	f.install(t, string(chart), nil)
	token, err := f.clientset.CoreV1().ServiceAccounts("orama-system").CreateToken(f.ctx, "orama-operator", &authenticationv1.TokenRequest{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	operatorConfig := rest.AnonymousClientConfig(config)
	operatorConfig.BearerToken = token.Status.Token
	f.operator = admissionClient(t, operatorConfig, f.scheme)
	f.reconciler = &LakeEnvironmentReconciler{Client: f.operator, Scheme: f.scheme}
}

func (f *environmentAdmissionFixture) createEnvironment(t *testing.T) {
	t.Helper()
	f.create(t, &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod"},
		Spec:       oramav1alpha1.LakeSpec{Namespace: "lake", Image: "oramasearch/lake:test"},
	})
	f.environment = &oramav1alpha1.LakeEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: "production", Namespace: "lake"},
		Spec: oramav1alpha1.LakeEnvironmentSpec{
			LakeRef: "prod", Repository: "repo", Environment: "production",
			Kind: oramav1alpha1.LakeEnvironmentKindNamed, Change: "release",
			Image: testEnvironmentImage, Port: 8080, Replicas: 1,
		},
	}
	f.create(t, f.environment)
	f.namespace = lakeEnvironmentNamespace(f.environment)
	f.reconcile(t)
}

func (f *environmentAdmissionFixture) initializeAbsentControllers(t *testing.T) {
	t.Helper()
	// envtest has no quota or service-account controllers; complete their fixture state.
	var quota corev1.ResourceQuota
	if err := f.admin.Get(f.ctx, client.ObjectKey{Namespace: f.namespace, Name: environmentProfileName}, &quota); err != nil {
		t.Fatal(err)
	}
	quota.Status.Hard = quota.Spec.Hard.DeepCopy()
	quota.Status.Used = corev1.ResourceList{}
	for key := range quota.Spec.Hard {
		quota.Status.Used[key] = resource.MustParse("0")
	}
	if err := f.admin.Status().Update(f.ctx, &quota); err != nil {
		t.Fatal(err)
	}
	f.create(t, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: f.namespace}})
	f.workspaceNamespace = "prod-ws-policy-proof"
	f.create(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: f.workspaceNamespace}})
	noToken := false
	f.create(t, &corev1.ServiceAccount{
		ObjectMeta:                   metav1.ObjectMeta{Name: "default", Namespace: f.workspaceNamespace},
		AutomountServiceAccountToken: &noToken,
	})
	f.create(t, &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: "untrusted"}, Handler: "untrusted"})
}

func (f *environmentAdmissionFixture) installAdmissionContract(t *testing.T, server *envtest.Environment, config *rest.Config) {
	t.Helper()
	policies, err := os.ReadFile("testdata/k0s/environment-policies.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if count := f.install(t, string(policies), nil); count != 4 {
		t.Fatalf("installed %d policy objects, want both policies and bindings", count)
	}
	profile, err := os.ReadFile("testdata/k0s/tenant-profile.yaml")
	if err != nil {
		t.Fatal(err)
	}
	count := f.install(t, strings.ReplaceAll(string(profile), "{{ .Namespace }}", f.namespace), func(kind string) bool {
		return kind == "Role" || kind == "RoleBinding"
	})
	if count != 2 {
		t.Fatalf("installed %d tenant RBAC objects, want 2", count)
	}
	tenant, err := server.AddUser(envtest.User{Name: "tenant-proof", Groups: []string{"liquid:tenant:" + f.namespace}}, config)
	if err != nil {
		t.Fatal(err)
	}
	f.tenant = admissionClient(t, tenant.Config(), f.scheme)
	workspace, err := server.AddUser(envtest.User{
		Name: "system:serviceaccount:" + f.workspaceNamespace + ":default",
		Groups: []string{
			"system:serviceaccounts",
			"system:serviceaccounts:" + f.workspaceNamespace,
			"system:authenticated",
		},
	}, config)
	if err != nil {
		t.Fatal(err)
	}
	f.workspace = admissionClient(t, workspace.Config(), f.scheme)
	t.Logf("installed both exact k0s policies, Deny/Audit bindings and tenant RBAC from %s", k0sDeliveryContract)
}

func admissionClient(t *testing.T, config *rest.Config, scheme *runtime.Scheme) client.Client {
	t.Helper()
	kube, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return kube
}

func (f *environmentAdmissionFixture) create(t *testing.T, object client.Object) {
	t.Helper()
	if err := f.admin.Create(f.ctx, object); err != nil {
		t.Fatalf("create %T: %v", object, err)
	}
}

func (f *environmentAdmissionFixture) install(t *testing.T, data string, selectKind func(string) bool) int {
	t.Helper()
	decoder := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(data), 4096)
	count := 0
	for {
		object := &unstructured.Unstructured{}
		if err := decoder.Decode(&object.Object); err != nil {
			if err == io.EOF {
				return count
			}
			t.Fatal(err)
		}
		if object.GetKind() == "" || selectKind != nil && !selectKind(object.GetKind()) {
			continue
		}
		f.create(t, object)
		count++
	}
}

func (f *environmentAdmissionFixture) reconcile(t *testing.T) {
	t.Helper()
	if _, err := f.reconciler.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(f.environment)}); err != nil {
		t.Fatal(err)
	}
	if err := f.admin.Get(f.ctx, client.ObjectKeyFromObject(f.environment), f.environment); err != nil {
		t.Fatal(err)
	}
	if f.environment.Status.Phase == oramav1alpha1.LakeEnvironmentPhaseDegraded {
		t.Fatalf("environment degraded: %+v", f.environment.Status)
	}
}

func (f *environmentAdmissionFixture) updateEnvironment(t *testing.T) {
	t.Helper()
	if err := f.operator.Update(f.ctx, f.environment); err != nil {
		t.Fatal(err)
	}
}

func (f *environmentAdmissionFixture) createSource(t *testing.T, name, secretName, value string) (*oramav1alpha1.LakeSealedSecret, *corev1.Secret) {
	t.Helper()
	source := &oramav1alpha1.LakeSealedSecret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "lake"},
		Spec: oramav1alpha1.LakeSealedSecretSpec{
			LakeRef:  corev1.LocalObjectReference{Name: "prod"},
			Template: oramav1alpha1.LakeSealedSecretTemplate{Name: secretName},
			Source:   oramav1alpha1.LakeSealedSecretSource{Type: "literal"},
			Data:     map[string]string{"DATABASE_URL": "synthetic-source-ciphertext"},
		},
	}
	f.create(t, source)
	source.Status.Phase = "Ready"
	source.Status.ObservedGeneration = source.Generation
	source.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Delivered", ObservedGeneration: source.Generation, LastTransitionTime: metav1.Now()}}
	if err := f.operator.Status().Update(f.ctx, source); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "lake", Labels: map[string]string{sealedSecretLabel: "true"}},
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"DATABASE_URL": []byte(value)},
	}
	if err := controllerutil.SetControllerReference(source, secret, f.scheme); err != nil {
		t.Fatal(err)
	}
	if err := f.operator.Create(f.ctx, secret); err != nil {
		t.Fatal(err)
	}
	return source, secret
}

func (f *environmentAdmissionFixture) renderedPod(t *testing.T, name string) *corev1.Pod {
	t.Helper()
	var deployment appsv1.Deployment
	if err := f.admin.Get(f.ctx, client.ObjectKey{Name: "environment", Namespace: f.namespace}, &deployment); err != nil {
		t.Fatal(err)
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: f.namespace,
		Labels: deployment.Spec.Template.Labels, Annotations: deployment.Spec.Template.Annotations,
	}, Spec: *deployment.Spec.Template.Spec.DeepCopy()}
}

func (f *environmentAdmissionFixture) admitPod(t *testing.T, name, expectedNames string) *corev1.Pod {
	t.Helper()
	pod := f.renderedPod(t, name)
	names := make([]string, 0)
	for _, from := range pod.Spec.Containers[0].EnvFrom {
		names = append(names, from.SecretRef.Name)
	}
	if got := pod.Annotations[environmentSealedNamesAnnotation]; got != expectedNames || got != strings.Join(names, ",") {
		t.Fatalf("annotation=%q envFrom=%v, want %q", got, names, expectedNames)
	}
	// Deployments create Pods through a controller. Submit the actual template using
	// the tenant's workload grant; the operator chart intentionally cannot create Pods.
	if err := f.tenant.Create(f.ctx, pod); err != nil {
		t.Fatalf("admit rendered Pod: %v", err)
	}
	return pod
}

func (f *environmentAdmissionFixture) assertDelivery(t *testing.T, source *oramav1alpha1.LakeSealedSecret, value string) (*corev1.Secret, *oramav1alpha1.LakeSealedSecret) {
	t.Helper()
	copy := &corev1.Secret{}
	if err := f.operator.Get(f.ctx, client.ObjectKey{Name: source.Spec.Template.Name, Namespace: f.namespace}, copy); err != nil {
		t.Fatal(err)
	}
	owner := &oramav1alpha1.LakeSealedSecret{}
	if err := f.operator.Get(f.ctx, client.ObjectKey{Name: source.Spec.Template.Name, Namespace: f.namespace}, owner); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(copy, owner) || owner.UID == source.UID || owner.UID == "" || owner.Status.Phase != "Ready" || copy.Labels[sealedSecretLabel] != "true" || string(copy.Data["DATABASE_URL"]) != value {
		t.Fatal("delivered Secret must have current data, sealed label, and a Ready same-namespace owner with its own UID")
	}
	return copy, owner
}

func (f *environmentAdmissionFixture) assertNamespaceSelection(t *testing.T) {
	t.Helper()
	var namespace corev1.Namespace
	if err := f.operator.Get(f.ctx, client.ObjectKey{Name: f.namespace}, &namespace); err != nil {
		t.Fatal(err)
	}
	if namespace.Labels[environmentAdmissionLabel] != "true" {
		t.Fatalf("operator-created namespace %s lacks %s=true", namespace.Name, environmentAdmissionLabel)
	}
	secretRequest := func() error {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "plain", Namespace: f.namespace}}
		return f.operator.Create(f.ctx, secret, client.DryRunAll)
	}
	waitAdmissionDenial(t, secretRequest, "Secrets in a liquid environment must be owned by a LakeSealedSecret")
	pod := f.renderedPod(t, "plain-pod")
	pod.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "database"}}}}
	waitAdmissionDenial(t, func() error {
		return f.tenant.Create(f.ctx, pod.DeepCopy(), client.DryRunAll)
	}, "Container Secret environment values must name a LakeSealedSecret delivery")
	t.Logf("namespace assertion: operator-created %s has %s=true; both policies REFUSE missing delivery metadata", namespace.Name, environmentAdmissionLabel)
	// Reproduce the selector gap only on this disposable API server, then exercise
	// reconciliation of drift. Never remove a production namespace's profile label.
	delete(namespace.Labels, environmentAdmissionLabel)
	if err := f.admin.Update(f.ctx, &namespace); err != nil {
		t.Fatal(err)
	}
	if err := secretRequest(); err != nil {
		t.Fatalf("unlabelled namespace control request: %v", err)
	}
	f.reconcile(t)
	waitAdmissionDenial(t, secretRequest, "Secrets in a liquid environment must be owned by a LakeSealedSecret")
	t.Log("namespace negative control: missing label bypasses admission; reconciliation restores label and refusal")
}

func waitAdmissionDenial(t *testing.T, request func() error, message string) {
	t.Helper()
	var last error
	err := wait.PollUntilContextTimeout(context.Background(), 100*time.Millisecond, 30*time.Second, true, func(context.Context) (bool, error) {
		last = request()
		var status apierrors.APIStatus
		return errors.As(last, &status) && strings.Contains(last.Error(), "ValidatingAdmissionPolicy") && strings.Contains(last.Error(), message), nil
	})
	if err != nil {
		t.Fatalf("wait for admission denial: %v; last response: %v", err, last)
	}
	t.Logf("admission REFUSED: %v", last)
}

func (f *environmentAdmissionFixture) assertTenantDenials(t *testing.T, delivered *corev1.Secret) {
	t.Helper()
	for _, forged := range []bool{false, true} {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tenant-forgery", Namespace: f.namespace}}
		if forged {
			secret.Labels = delivered.Labels
			secret.OwnerReferences = []metav1.OwnerReference{{APIVersion: oramav1alpha1.GroupVersion.String(), Kind: "LakeSealedSecret", Name: "does-not-exist", UID: "nonexistent-uid", Controller: boolPtr(true)}}
		}
		err := f.tenant.Create(f.ctx, secret)
		if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "tenant-proof") || !strings.Contains(err.Error(), "cannot create resource") {
			t.Fatalf("tenant forged=%v must be refused by RBAC: %v", forged, err)
		}
		t.Logf("tenant forged=%v REFUSED by RBAC: %v", forged, err)
	}
}

func (f *environmentAdmissionFixture) assertPolicyWriteDenials(t *testing.T) {
	t.Helper()
	allowAll := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-allow-all", Namespace: f.workspaceNamespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{}},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{}},
		},
	}
	err := f.workspace.Create(f.ctx, allowAll)
	if !apierrors.IsForbidden(err) || !strings.Contains(err.Error(), "cannot create resource \"networkpolicies\"") {
		t.Fatalf("workspace service account must be refused when creating allow-all NetworkPolicy: %v", err)
	}
	t.Logf("workspace service account allow-all NetworkPolicy REFUSED by RBAC: %v", err)

	identities := []struct {
		name      string
		user      string
		groups    []string
		namespace string
	}{
		{
			name:      "workspace service account",
			user:      "system:serviceaccount:" + f.workspaceNamespace + ":default",
			groups:    []string{"system:serviceaccounts", "system:serviceaccounts:" + f.workspaceNamespace, "system:authenticated"},
			namespace: f.workspaceNamespace,
		},
		{
			name:      "tenant group",
			user:      "tenant-proof",
			groups:    []string{"liquid:tenant:" + f.namespace, "system:authenticated"},
			namespace: f.namespace,
		},
	}
	resources := []struct {
		group    string
		resource string
	}{
		{group: "networking.k8s.io", resource: "networkpolicies"},
		{group: "cilium.io", resource: "ciliumnetworkpolicies"},
		{group: oramav1alpha1.GroupVersion.Group, resource: "networksandboxes"},
	}
	for _, identity := range identities {
		for _, resource := range resources {
			for _, verb := range []string{"create", "update", "patch", "delete"} {
				review, reviewErr := f.clientset.AuthorizationV1().SubjectAccessReviews().Create(f.ctx,
					&authorizationv1.SubjectAccessReview{Spec: authorizationv1.SubjectAccessReviewSpec{
						User: identity.user, Groups: identity.groups,
						ResourceAttributes: &authorizationv1.ResourceAttributes{
							Namespace: identity.namespace, Verb: verb, Group: resource.group, Resource: resource.resource,
						},
					}}, metav1.CreateOptions{})
				if reviewErr != nil {
					t.Fatalf("review %s %s %s: %v", identity.name, verb, resource.resource, reviewErr)
				}
				if review.Status.Allowed {
					t.Errorf("%s may %s %s/%s in %s", identity.name, verb, resource.group, resource.resource, identity.namespace)
				}
			}
		}
	}
}

func (f *environmentAdmissionFixture) assertSteadyDelivery(t *testing.T, secret *corev1.Secret, owner *oramav1alpha1.LakeSealedSecret) {
	t.Helper()
	sealed := &LakeSealedSecretReconciler{Client: &namespaceReadGuard{Client: f.operator}, Scheme: f.scheme, APIReader: f.operator}
	if _, err := sealed.Reconcile(f.ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(owner)}); err != nil {
		t.Fatal(err)
	}
	var currentSecret corev1.Secret
	var currentOwner oramav1alpha1.LakeSealedSecret
	if err := f.operator.Get(f.ctx, client.ObjectKeyFromObject(secret), &currentSecret); err != nil {
		t.Fatal(err)
	}
	if err := f.operator.Get(f.ctx, client.ObjectKeyFromObject(owner), &currentOwner); err != nil {
		t.Fatal(err)
	}
	if secret.ResourceVersion != currentSecret.ResourceVersion || owner.ResourceVersion != currentOwner.ResourceVersion {
		t.Fatal("steady delivery changed Secret or owner status")
	}
}

func (f *environmentAdmissionFixture) assertReferenceChanges(t *testing.T, original *oramav1alpha1.LakeSealedSecret) {
	t.Helper()
	second, _ := f.createSource(t, "cache", "application-cache", "cache-value")
	f.environment.Spec.SecretRefs = []string{"database", "secret://cache"}
	f.updateEnvironment(t)
	f.reconcile(t)
	f.assertDelivery(t, second, "cache-value")
	f.admitPod(t, "two-deliveries", "application-database,application-cache")
	f.environment.Spec.SecretRefs = []string{"cache"}
	f.updateEnvironment(t)
	f.reconcile(t)
	f.assertRemoved(t, original)
	f.admitPod(t, "one-delivery", "application-cache")
	f.environment.Spec.SecretRefs = nil
	f.updateEnvironment(t)
	f.reconcile(t)
	f.assertRemoved(t, second)
	pod := f.admitPod(t, "no-deliveries", "")
	if _, exists := pod.Annotations[environmentSealedNamesAnnotation]; exists {
		t.Fatal("annotation remains after removing all references")
	}
	t.Log("reference changes/removal: annotation matches both resolved names, then remaining name, then is absent; copied Secrets and local owners deleted")
}

func (f *environmentAdmissionFixture) assertRemoved(t *testing.T, source *oramav1alpha1.LakeSealedSecret) {
	t.Helper()
	for _, object := range []client.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: source.Spec.Template.Name, Namespace: f.namespace}},
		&oramav1alpha1.LakeSealedSecret{ObjectMeta: metav1.ObjectMeta{Name: source.Spec.Template.Name, Namespace: f.namespace}},
	} {
		if err := f.operator.Get(f.ctx, client.ObjectKeyFromObject(object), object); !apierrors.IsNotFound(err) {
			t.Fatalf("stale %T remains: %v", object, err)
		}
	}
	if err := f.operator.Get(f.ctx, client.ObjectKeyFromObject(source), &oramav1alpha1.LakeSealedSecret{}); err != nil {
		t.Fatalf("source changed during cleanup: %v", err)
	}
}

// The production cached client cannot start a Namespace informer with get-only RBAC.
type namespaceReadGuard struct {
	client.Client
}

func (g *namespaceReadGuard) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, isNamespace := object.(*corev1.Namespace); isNamespace {
		return errors.New("Namespace lookup must use the uncached APIReader")
	}
	return g.Client.Get(ctx, key, object, options...)
}

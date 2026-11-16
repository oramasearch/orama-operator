/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The isolation rules a tenant's app runs under, which a security review of tenant apps relies
// on. Each test pins one rule so that it
// cannot silently stop holding.

const isolationTestWorkspace = "prod-ws-0123456789ab"

// isolationTestApps is one synchronized app of each shape a tenant can ask lake for: runtime and
// image mode, with service bindings, a mounted volume and a bucket, and a stopped one.
func isolationTestApps(lake *oramav1alpha1.Lake) map[string]*oramav1alpha1.LakeApp {
	base := func(name string) *oramav1alpha1.LakeApp {
		return &oramav1alpha1.LakeApp{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: lakeNamespace(lake)},
			Spec: oramav1alpha1.LakeAppSpec{
				LakeRef: lake.Name,
				Mode:    oramav1alpha1.LakeAppModeRuntime,
				Port:    8080,
				Source: &oramav1alpha1.LakeAppSource{
					Repository: "acme/site", Revision: "blake3:00", Framework: "fastapi",
				},
				Sync: &oramav1alpha1.LakeAppSync{
					ID:                 "4578260e-adb0-49b8-b044-1a06513e89bb",
					CustomerID:         "6bb777ba-498a-4939-9daf-53e37729fe23",
					WorkspaceID:        "6bb777ba-498a-4939-9daf-53e37729fe23",
					WorkspaceNamespace: isolationTestWorkspace,
					Generation:         1,
				},
			},
		}
	}
	runtime := base("runtime")
	image := base("image")
	image.Spec.Mode = oramav1alpha1.LakeAppModeImage
	image.Spec.Source = nil
	image.Spec.Image = "registry.example/acme/site@sha256:" + strings.Repeat("a", 64)
	bound := base("bound")
	bound.Spec.Bindings = &oramav1alpha1.LakeAppBindings{
		Zetta:        &oramav1alpha1.LakeAppZettaBinding{CustomerID: bound.Spec.Sync.CustomerID, Scopes: []string{"read"}},
		Services:     []string{"5103dc69-b44c-4f3e-9d1a-0c4d2b1e6f70"},
		ServiceNames: map[string]string{"5103dc69-b44c-4f3e-9d1a-0c4d2b1e6f70": "orders"},
	}
	stores := base("stores")
	stores.Spec.Stores = []oramav1alpha1.LakeAppStoreMount{
		{ID: "store-volume", Name: "data", Slug: "data", Type: storeTypeVolume, ClaimName: "store-data", MountPath: "/data"},
		{ID: "store-bucket", Name: "files", Slug: "files", Type: storeTypeObject, Bucket: "files", Endpoint: "https://s3.example", SecretName: "files-credentials"},
	}
	stopped := base("stopped")
	zero := int32(0)
	stopped.Spec.Replicas = &zero
	return map[string]*oramav1alpha1.LakeApp{
		"runtime": runtime, "image": image, "bound": bound, "stores": stores, "stopped": stopped,
	}
}

func isolationTestSecrets(app *oramav1alpha1.LakeApp) lakeAppSecrets {
	return lakeAppSecrets{
		EnvFromNames: []string{lakeAppEnvSecretName(app, "env")},
		BindingsName: lakeAppBindingsName(app),
		PullName:     lakeAppPullName(app),
		StoreSecrets: map[string]string{"store-bucket": lakeAppStoreSecretName(app, "files")},
	}
}

// A tenant's app pod passes the Pod Security "restricted" profile its workspace namespace enforces,
// and holds no Kubernetes credential: no service account token, no host namespace, no host path,
// no capability, a read-only root, RuntimeDefault seccomp and bounded resources.
func TestATenantAppPodHoldsNoKubernetesCredentialAndMeetsRestrictedPodSecurity(t *testing.T) {
	lake := testWorkspaceLake()
	for name, app := range isolationTestApps(lake) {
		pod := lakeAppPodSpec(lake, app, isolationTestSecrets(app))
		if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
			t.Errorf("%s: the app pod mounts a service account token", name)
		}
		if pod.ServiceAccountName != "" && pod.ServiceAccountName != "default" {
			t.Errorf("%s: the app pod runs as service account %q", name, pod.ServiceAccountName)
		}
		if pod.EnableServiceLinks == nil || *pod.EnableServiceLinks {
			t.Errorf("%s: the app pod gets every Service of its namespace as environment", name)
		}
		if pod.HostNetwork || pod.HostPID || pod.HostIPC {
			t.Errorf("%s: the app pod shares a host namespace", name)
		}
		security := pod.SecurityContext
		if security == nil || security.RunAsNonRoot == nil || !*security.RunAsNonRoot ||
			security.RunAsUser == nil || *security.RunAsUser == 0 ||
			security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("%s: the pod security context is not restricted: %+v", name, security)
		}
		for _, volume := range pod.Volumes {
			if volume.EmptyDir == nil && volume.Secret == nil && volume.PersistentVolumeClaim == nil {
				t.Errorf("%s: volume %q is neither an emptyDir, a Secret nor a claim: %+v", name, volume.Name, volume.VolumeSource)
			}
		}
		if len(pod.InitContainers) != 0 || len(pod.EphemeralContainers) != 0 || len(pod.Containers) != 1 {
			t.Errorf("%s: the app pod runs containers beyond its app container", name)
		}
		for _, container := range pod.Containers {
			assertRestrictedAppContainer(t, name, container)
		}
	}
}

func assertRestrictedAppContainer(t *testing.T, app string, container corev1.Container) {
	t.Helper()
	security := container.SecurityContext
	if security == nil || security.AllowPrivilegeEscalation == nil || *security.AllowPrivilegeEscalation ||
		security.ReadOnlyRootFilesystem == nil || !*security.ReadOnlyRootFilesystem ||
		(security.Privileged != nil && *security.Privileged) || security.Capabilities == nil ||
		len(security.Capabilities.Add) != 0 || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != "ALL" {
		t.Errorf("%s: container %q is not restricted: %+v", app, container.Name, security)
	}
	for _, port := range container.Ports {
		if port.HostPort != 0 {
			t.Errorf("%s: container %q binds host port %d", app, container.Name, port.HostPort)
		}
	}
	if container.Resources.Limits.Cpu().IsZero() || container.Resources.Limits.Memory().IsZero() {
		t.Errorf("%s: container %q has no CPU or memory limit: %+v", app, container.Name, container.Resources)
	}
}

// A tenant's app reaches, on its own policy, only cluster DNS, its Lake's API port and data
// services of its own workspace: never an address block (cloud metadata, nodes, the Kubernetes API,
// the internet), another namespace, or the Lake's other ports.
func TestATenantAppEgressReachesOnlyDNSItsLakeAPIAndItsOwnWorkspace(t *testing.T) {
	lake := testWorkspaceLake()
	allowed := map[string]bool{"kube-system": true, lakeNamespace(lake): true, isolationTestWorkspace: true}
	for name, app := range isolationTestApps(lake) {
		for _, active := range []bool{false, true} {
			rules := lakeAppEgressRules(lake, app, active)
			for _, rule := range rules {
				if len(rule.To) == 0 {
					t.Fatalf("%s (connections active %v): an egress rule allows every destination: %+v", name, active, rule)
				}
				for _, peer := range rule.To {
					if peer.IPBlock != nil {
						t.Fatalf("%s: egress names an address block: %+v", name, peer.IPBlock)
					}
					if peer.NamespaceSelector == nil || peer.PodSelector == nil ||
						len(peer.NamespaceSelector.MatchExpressions) != 0 || len(peer.PodSelector.MatchLabels) == 0 {
						t.Fatalf("%s: egress peer is not one namespace and labelled pods: %+v", name, peer)
					}
					namespace := peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]
					if len(peer.NamespaceSelector.MatchLabels) != 1 || !allowed[namespace] {
						t.Fatalf("%s: egress reaches namespace %q", name, namespace)
					}
					switch namespace {
					case "kube-system":
						if len(rule.Ports) != 2 || peer.PodSelector.MatchLabels["k8s-app"] != "kube-dns" {
							t.Fatalf("%s: egress to kube-system is not cluster DNS: %+v", name, rule)
						}
						for _, port := range rule.Ports {
							if port.Port == nil || port.Port.IntValue() != 53 {
								t.Fatalf("%s: egress to cluster DNS opens port %v", name, port.Port)
							}
						}
					case lakeNamespace(lake):
						if peer.PodSelector.MatchLabels["app.kubernetes.io/name"] != "lake" ||
							peer.PodSelector.MatchLabels["app.kubernetes.io/instance"] != lake.Name {
							t.Fatalf("%s: egress reaches a pod of the Lake namespace other than the Lake: %+v", name, peer.PodSelector)
						}
						if got := policyPorts(t, rule.Ports); len(got) != 1 || got[0] != int32(lakeContainerPort(lake)) {
							t.Fatalf("%s: egress to the Lake is not its API port only: %v", name, got)
						}
					}
				}
			}
		}
	}
}

// Only ingress-nginx and the owning Lake may open a connection to a tenant's app, on its port.
// Another tenant's app, the workspace's own services and every other pod cannot.
func TestATenantAppAdmitsOnlyIngressNginxAndItsLakeOnItsPort(t *testing.T) {
	lake := testWorkspaceLake()
	for name, app := range isolationTestApps(lake) {
		rules := lakeAppIngressRules(lake, app)
		if len(rules) != 2 {
			t.Fatalf("%s: %d ingress rules, want ingress-nginx and the Lake", name, len(rules))
		}
		for _, rule := range rules {
			if got := policyPorts(t, rule.Ports); len(got) != 1 || got[0] != app.Spec.Port {
				t.Fatalf("%s: ingress is not the app port only: %v", name, got)
			}
			if len(rule.From) != 1 || rule.From[0].IPBlock != nil || rule.From[0].NamespaceSelector == nil {
				t.Fatalf("%s: ingress source is not one namespace: %+v", name, rule.From)
			}
		}
		if rules[0].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "ingress-nginx" || rules[0].From[0].PodSelector != nil {
			t.Fatalf("%s: the first ingress source is not the ingress-nginx namespace: %+v", name, rules[0].From[0])
		}
		assertOnlyOwningLakePeer(t, lake, rules[1].From[0])
	}
}

// The workspace-wide allowances that reach the Kubernetes API, or that the operator's own
// datastore ingress uses, select by labels only the operator puts on datastore pods: never an app.
func TestNoWorkspaceAPIServerOrOperatorAllowanceSelectsATenantApp(t *testing.T) {
	lake := testWorkspaceLake()
	for name, app := range isolationTestApps(lake) {
		labels := lakeAppLabels(app)
		for _, client := range workspaceAPIServerClients {
			if selectorMatches(t, client.pods, labels) {
				t.Fatalf("%s: Kubernetes API allowance %s selects the app", name, client.name)
			}
		}
		for _, allowance := range workspaceOperatorIngresses {
			policy := workspaceOperatorIngressPolicy(allowance, isolationTestWorkspace, nil)
			if selectorMatches(t, policy.Spec.PodSelector, labels) {
				t.Fatalf("%s: operator ingress allowance %s selects the app", name, policy.Name)
			}
		}
		operatorPostgres := workspaceOperatorPostgresPolicy("orama-system", isolationTestWorkspace, nil)
		if selectorMatches(t, operatorPostgres.Spec.PodSelector, labels) {
			t.Fatalf("%s: the operator's Postgres allowance selects the app", name)
		}
	}
}

// A Lake-protected app's Ingress names X-Orama-User and X-Orama-Space as auth response headers, so
// ingress-nginx overwrites whatever a client sent under those names with Lake's answer. A public app
// has no forward auth at all (see the report for what that means for the headers).
func TestALakeProtectedAppIngressReplacesTheClientsIdentityHeaders(t *testing.T) {
	lake := testWorkspaceLake()
	lake.Spec.Host = "lake.example"
	lake.Spec.IngressEnabled = true
	app := isolationTestApps(lake)["runtime"]
	for _, auth := range []string{"", oramav1alpha1.LakeAppAuthLake} {
		app.Spec.Auth = auth
		annotations := lakeAppIngressAnnotations(lake, app)
		headers := map[string]bool{}
		for _, header := range strings.Split(annotations["nginx.ingress.kubernetes.io/auth-response-headers"], ",") {
			headers[strings.TrimSpace(header)] = true
		}
		if !headers["X-Orama-User"] || !headers["X-Orama-Space"] {
			t.Fatalf("auth %q: identity headers are not set from Lake's answer: %v", auth, annotations)
		}
		if !strings.HasPrefix(annotations["nginx.ingress.kubernetes.io/auth-url"], "http://lake-"+lake.Name+"."+lakeNamespace(lake)+".svc.cluster.local:") {
			t.Fatalf("auth %q: forward auth does not go to the owning Lake: %v", auth, annotations)
		}
	}
	app.Spec.Auth = oramav1alpha1.LakeAppAuthPublic
	annotations := lakeAppIngressAnnotations(lake, app)
	if _, found := annotations["nginx.ingress.kubernetes.io/auth-url"]; found {
		t.Fatalf("a public app has forward auth: %v", annotations)
	}
}

// Each app's access policy selects that app's pods only, so one app's allowances never widen
// another's.
func TestATenantAppPolicySelectsOnlyThatApp(t *testing.T) {
	lake := testWorkspaceLake()
	apps := isolationTestApps(lake)
	for name, app := range apps {
		selector := lakeAppLabels(app)
		for other, otherApp := range apps {
			if other == name {
				continue
			}
			if selectorMatches(t, metav1.LabelSelector{MatchLabels: selector}, lakeAppLabels(otherApp)) {
				t.Fatalf("%s's policy selects app %s", name, other)
			}
		}
	}
}

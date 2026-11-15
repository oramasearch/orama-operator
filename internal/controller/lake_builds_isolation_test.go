/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"net/netip"
	"reflect"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The isolation rules the tenant-build security review relies on. Each test names the rule it holds,
// so a change that weakens one fails here instead of silently widening what tenant source can do.

// Rule: a build step reaches no private, link-local or cloud platform address on its public
// 80/443 rule. Azure's 168.63.129.16 is a public address that serves node provisioning data.
func TestBuildPublicEgressNeverReachesPlatformOrPrivateAddresses(t *testing.T) {
	lake := buildTestLake()
	var except []netip.Prefix
	publicRules := 0
	for _, rule := range buildEgressRules(lake) {
		for _, peer := range rule.To {
			if peer.IPBlock == nil {
				continue
			}
			publicRules++
			if peer.IPBlock.CIDR != "0.0.0.0/0" {
				t.Fatalf("public rule CIDR = %q", peer.IPBlock.CIDR)
			}
			for _, cidr := range peer.IPBlock.Except {
				prefix, err := netip.ParsePrefix(cidr)
				if err != nil {
					t.Fatalf("except %q: %v", cidr, err)
				}
				except = append(except, prefix)
			}
			for _, port := range rule.Ports {
				if value := port.Port.IntValue(); value != 80 && value != 443 {
					t.Fatalf("public rule opens port %d", value)
				}
			}
		}
	}
	if publicRules != 1 {
		t.Fatalf("public egress rules = %d, want 1", publicRules)
	}
	blocked := func(address string) bool {
		ip := netip.MustParseAddr(address)
		for _, prefix := range except {
			if prefix.Contains(ip) {
				return true
			}
		}
		return false
	}
	for _, address := range []string{
		"168.63.129.16",   // Azure WireServer and host agent
		"169.254.169.254", // instance metadata (Azure, AWS, GCP)
		"10.0.0.1", "172.16.0.1", "192.168.1.1", "100.64.0.1", "127.0.0.1", "0.0.0.1",
	} {
		if !blocked(address) {
			t.Errorf("a build step can reach %s on 80/443", address)
		}
	}
	for _, address := range []string{"104.16.0.1", "151.101.0.223"} { // package indexes stay reachable
		if blocked(address) {
			t.Errorf("public address %s is blocked", address)
		}
	}
}

// Rule: the only in-cluster peers a build reaches are cluster DNS and its own Lake's pods.
func TestBuildEgressReachesOnlyClusterDNSAndItsLake(t *testing.T) {
	lake := buildTestLake()
	for _, rule := range buildEgressRules(lake) {
		for _, peer := range rule.To {
			if peer.IPBlock != nil {
				continue
			}
			if peer.NamespaceSelector == nil || peer.PodSelector == nil {
				t.Fatalf("in-cluster egress peer without both selectors: %+v", peer)
			}
			namespace := peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]
			switch {
			case namespace == "kube-system" && peer.PodSelector.MatchLabels["k8s-app"] == "kube-dns":
			case namespace == lakeNamespace(lake) &&
				peer.PodSelector.MatchLabels["app.kubernetes.io/name"] == "lake" &&
				peer.PodSelector.MatchLabels["app.kubernetes.io/instance"] == lake.Name:
			default:
				t.Fatalf("build egress reaches an unexpected peer: %+v", peer)
			}
		}
	}
}

// Rule: the builder pod gets no host access, no API credential and no capability beyond the two
// rootless BuildKit needs; the only Secrets it reads are its own build's tokens and the base
// image pull credential.
func TestBuilderPodHasNoHostAccessOrExtraPrivilege(t *testing.T) {
	lake := buildTestLake()
	build := testBuild(testBuildID, testBuildTarget)
	target, err := parseBuildTarget(build.Target)
	if err != nil {
		t.Fatal(err)
	}
	pod := buildPod(lake, &build, target, buildLabels(lake, &build, target))
	if pod.HostNetwork || pod.HostPID || pod.HostIPC || pod.ShareProcessNamespace != nil {
		t.Fatalf("builder pod shares a host or pod namespace: %+v", pod)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken || pod.ServiceAccountName != "" {
		t.Fatalf("builder pod can reach the Kubernetes API: automount=%v serviceAccount=%q",
			pod.AutomountServiceAccountToken, pod.ServiceAccountName)
	}
	if len(pod.InitContainers) != 0 || len(pod.EphemeralContainers) != 0 || len(pod.Containers) != 1 {
		t.Fatalf("builder pod runs more than its one builder container")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot ||
		pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser == 0 {
		t.Fatalf("builder pod may run as root: %+v", pod.SecurityContext)
	}
	for _, volume := range pod.Volumes {
		switch {
		case volume.EmptyDir != nil:
			if volume.EmptyDir.SizeLimit == nil {
				t.Fatalf("emptyDir %s has no size limit", volume.Name)
			}
		case volume.Secret != nil && volume.Name == buildBaseAuthVolume && volume.Secret.SecretName == buildBasePullSecretName:
		default:
			t.Fatalf("builder pod mounts an unexpected volume: %+v", volume)
		}
	}
	container := pod.Containers[0]
	security := container.SecurityContext
	if security == nil || (security.Privileged != nil && *security.Privileged) {
		t.Fatalf("builder container is privileged: %+v", security)
	}
	if security.ProcMount != nil && *security.ProcMount != corev1.DefaultProcMount {
		t.Fatalf("builder container unmasks /proc without a user-namespaced pod: %v", *security.ProcMount)
	}
	added := []string{}
	for _, capability := range security.Capabilities.Add {
		added = append(added, string(capability))
	}
	if strings.Join(added, ",") != "SETUID,SETGID" || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("builder capabilities = add %v drop %v", security.Capabilities.Add, security.Capabilities.Drop)
	}
	for _, port := range container.Ports {
		if port.HostPort != 0 {
			t.Fatalf("builder container binds host port %d", port.HostPort)
		}
	}
	if len(container.EnvFrom) != 0 {
		t.Fatalf("builder container imports a whole Secret or ConfigMap: %+v", container.EnvFrom)
	}
	for _, env := range container.Env {
		if env.ValueFrom == nil {
			continue
		}
		ref := env.ValueFrom.SecretKeyRef
		if ref == nil || ref.Name != buildTokenSecretName(build.ID) {
			t.Fatalf("builder env %s reads a value other than this build's tokens: %+v", env.Name, env.ValueFrom)
		}
	}
}

// Rule: one namespace per (Lake, space), so two customers' builds never share a namespace, their
// NetworkPolicies, or their token Secrets; and each space's registry cache stays under that space.
func TestBuildNamespaceAndCacheArePerSpace(t *testing.T) {
	lake := buildTestLake()
	one, err := parseBuildTarget("lake-registry.lake.svc:80/space-one/app:v1")
	if err != nil {
		t.Fatal(err)
	}
	two, err := parseBuildTarget("lake-registry.lake.svc:80/space-two/app:v1")
	if err != nil {
		t.Fatal(err)
	}
	if buildNamespace(lake.Name, one.Space) == buildNamespace(lake.Name, two.Space) {
		t.Fatal("two spaces share a build namespace")
	}
	build := testBuild(testBuildID, testBuildTarget)
	for _, target := range []buildTarget{one, two} {
		cache := buildCacheRef(&build, target, true)
		if !strings.HasPrefix(cache, target.Registry+"/"+target.Space+"/cache/") {
			t.Fatalf("cache %q is outside space %q", cache, target.Space)
		}
	}
	for _, target := range []string{
		"lake-registry.lake.svc:80/space-one/../space-two/app:v1",
		"lake-registry.lake.svc:80/space-one/cache/app:v1",
		"lake-registry.lake.svc:80/Space-One/app:v1",
	} {
		if _, err := parseBuildTarget(target); err == nil {
			t.Fatalf("target %q parsed; a build may name only <registry>/<space>/<name>:<tag>", target)
		}
	}
}

// Rule: the operator never adopts a namespace that belongs to another Lake or another space (a
// hash collision or a namespace made by someone else), so it never puts one tenant's build in
// another's namespace.
func TestBuildNamespaceOwnedByAnotherSpaceIsNotAdopted(t *testing.T) {
	lake := buildTestLake()
	target, err := parseBuildTarget(testBuildTarget)
	if err != nil {
		t.Fatal(err)
	}
	name := buildNamespace(lake.Name, target.Space)
	squatter := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		Annotations: map[string]string{buildOwnerAnnotation: lake.Name, buildSpaceAnnotation: "space-other"},
	}}
	store := &fakeServiceStore{builds: []LakeBuild{testBuild(testBuildID, testBuildTarget)}}
	h := newBuildHarness(t, lake, store, squatter)
	if _, err := h.reconciler.reconcileBuildNamespace(context.Background(), lake, target); err == nil {
		t.Fatal("a namespace annotated for another space was adopted")
	}
	h.poll()
	var jobs batchv1.JobList
	if err := h.client.List(context.Background(), &jobs, client.InNamespace(name)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatalf("a build Job was created in another space's namespace: %d", len(jobs.Items))
	}
}

// Rule: every pod in a build namespace starts from deny-all in both directions; nothing reaches a
// builder, and a builder reaches only what build-network allows.
func TestBuildNamespaceDefaultDenyCoversEveryPod(t *testing.T) {
	lake := buildTestLake()
	store := &fakeServiceStore{builds: []LakeBuild{testBuild(testBuildID, testBuildTarget)}}
	h := newBuildHarness(t, lake, store)
	h.poll()
	target, err := parseBuildTarget(testBuildTarget)
	if err != nil {
		t.Fatal(err)
	}
	namespace := buildNamespace(lake.Name, target.Space)
	deny := &networkingv1.NetworkPolicy{}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: buildDefaultDeny}, deny); err != nil {
		t.Fatal(err)
	}
	spec := deny.Spec
	if len(spec.PodSelector.MatchLabels) != 0 || len(spec.PodSelector.MatchExpressions) != 0 ||
		len(spec.Ingress) != 0 || len(spec.Egress) != 0 || len(spec.PolicyTypes) != 2 {
		t.Fatalf("default-deny does not cover every pod in both directions: %+v", spec)
	}
	access := &networkingv1.NetworkPolicy{}
	if err := h.client.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: buildNetworkPolicy}, access); err != nil {
		t.Fatal(err)
	}
	if len(access.Spec.Ingress) != 0 {
		t.Fatalf("build-network admits ingress: %+v", access.Spec.Ingress)
	}
}

// Rule: every build namespace carries the mandatory Cilium deny of the cloud metadata and Azure
// host-service addresses, selecting every pod, before any build Job exists. On AKS-managed Cilium
// the public rule's ipBlock exclusions alone did not stop metadata traffic.
func TestBuildNamespaceDeniesCloudMetadataWithCilium(t *testing.T) {
	lake := buildTestLake()
	store := &fakeServiceStore{builds: []LakeBuild{testBuild(testBuildID, testBuildTarget)}}
	h := newBuildHarness(t, lake, store)
	h.poll()
	target, err := parseBuildTarget(testBuildTarget)
	if err != nil {
		t.Fatal(err)
	}
	namespace := buildNamespace(lake.Name, target.Space)
	policy := metadataDenyForTest(namespace)
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(policy), policy); err != nil {
		t.Fatalf("build namespace has no metadata deny: %v", err)
	}
	if !metav1.IsControlledBy(policy, lake) {
		t.Fatal("metadata deny is not controlled by its Lake")
	}
	want := map[string]interface{}{
		"endpointSelector": map[string]interface{}{},
		"egressDeny": []interface{}{map[string]interface{}{
			"toCIDR": []interface{}{"169.254.169.254/32", "168.63.129.16/32", "fd00:ec2::254/128"},
		}},
	}
	if !reflect.DeepEqual(policy.Object["spec"], want) {
		t.Fatalf("metadata deny spec = %#v", policy.Object["spec"])
	}
}

// Rule: a metadata deny controlled by someone else is never adopted or rewritten, and no build Job
// starts in a namespace whose deny the operator does not own.
func TestBuildDoesNotStartWhenItsMetadataDenyIsForeign(t *testing.T) {
	lake := buildTestLake()
	target, err := parseBuildTarget(testBuildTarget)
	if err != nil {
		t.Fatal(err)
	}
	namespace := buildNamespace(lake.Name, target.Space)
	foreign := metadataDenyForTest(namespace)
	foreign.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: oramav1alpha1.GroupVersion.String(), Kind: "Lake", Name: "another-lake", UID: "another-uid", Controller: ptrTo(true),
	}})
	foreign.Object["spec"] = map[string]interface{}{"endpointSelector": map[string]interface{}{}}
	store := &fakeServiceStore{builds: []LakeBuild{testBuild(testBuildID, testBuildTarget)}}
	h := newBuildHarness(t, lake, store, foreign)
	h.poll()
	var jobs batchv1.JobList
	if err := h.client.List(context.Background(), &jobs, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 0 {
		t.Fatal("a build Job started behind a metadata deny the operator does not own")
	}
	policy := metadataDenyForTest(namespace)
	if err := h.client.Get(context.Background(), client.ObjectKeyFromObject(policy), policy); err != nil {
		t.Fatal(err)
	}
	if _, rewritten := policy.Object["spec"].(map[string]interface{})["egressDeny"]; rewritten {
		t.Fatal("a foreign metadata deny was rewritten")
	}
}

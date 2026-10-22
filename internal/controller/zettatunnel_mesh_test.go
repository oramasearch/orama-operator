/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	"github.com/oramasearch/orama-operator/internal/meshprojection"
)

func meshPodFor(name, node, ip string, labels map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testTunnelNS, UID: types.UID("uid-" + name), Labels: labels},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "c"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, PodIP: ip},
	}
}

func meshSandbox(enrolled bool) *oramav1alpha1.NetworkSandbox {
	app := map[string]string{lakeAppIdentityLabel: "web"}
	db := map[string]string{serviceIDLabel: testConnectionTargetID}
	connection := &oramav1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: lakeConnectionName(testConnectionID)}}
	allowances := connectionAllowances(connection, metav1.LabelSelector{MatchLabels: app}, metav1.LabelSelector{MatchLabels: db}, tcpPorts(5432))
	allowances = append(allowances, sandboxAllowance("DNS for every workload.", workspaceDNSPolicy(testTunnelNS, nil)))
	sandbox := &oramav1alpha1.NetworkSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: networkSandboxName, Namespace: testTunnelNS, UID: types.UID("sandbox"), Generation: 1},
		Spec:       oramav1alpha1.NetworkSandboxSpec{PodSelector: metav1.LabelSelector{}, Policies: allowances},
	}
	if enrolled {
		sandbox.Spec.Mesh = &oramav1alpha1.NetworkSandboxMesh{Enrolled: true}
	}
	return sandbox
}

func markRendered(t *testing.T, kube client.Client, sandbox *oramav1alpha1.NetworkSandbox) {
	t.Helper()
	var current oramav1alpha1.NetworkSandbox
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(sandbox), &current); err != nil {
		t.Fatal(err)
	}
	current.Status.ObservedGeneration = current.Generation
	apimeta.SetStatusCondition(&current.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Rendered", ObservedGeneration: current.Generation})
	if err := kube.Status().Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
}

func readMeshTemplate(t *testing.T, kube client.Client, node string) (meshprojection.Template, map[string][]byte) {
	t.Helper()
	var shard corev1.Secret
	if err := kube.Get(context.Background(), client.ObjectKey{Namespace: zettaTunnelMeshNamespace, Name: meshNodeSecret(node)}, &shard); err != nil {
		t.Fatal(err)
	}
	var template meshprojection.Template
	data, ok := shard.Data[meshprojection.TemplateFile(node)]
	if !ok {
		return template, shard.Data
	}
	if err := json.Unmarshal(data, &template); err != nil {
		t.Fatal(err)
	}
	return template, shard.Data
}

func TestMeshProjectsOnlyDeclaredPathsOfEnrolledPodsAfterTheirPoliciesAreRendered(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	app := map[string]string{lakeAppIdentityLabel: "web"}
	db := map[string]string{serviceIDLabel: testConnectionTargetID}
	sandbox := meshSandbox(true)
	objects := []client.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testTunnelNS}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orama-system"}},
		sandbox,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}},
		// Two replicas of the app on one node: two identities, never one.
		meshPodFor("web-1", "node-a", "10.244.0.10", app),
		meshPodFor("web-2", "node-a", "10.244.0.11", app),
		meshPodFor("db-0", "node-b", "10.244.1.20", db),
		// Not running, IPv6 only, or not scheduled: never projected.
		func() *corev1.Pod {
			pod := meshPodFor("done", "node-a", "10.244.0.12", app)
			pod.Status.Phase = corev1.PodSucceeded
			return pod
		}(),
		meshPodFor("v6", "node-a", "fd00::1", app),
		meshPodFor("pending", "", "", app),
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&oramav1alpha1.NetworkSandbox{}, &appsv1.DaemonSet{}).
		WithObjects(objects...).Build()
	r := &zettaTunnelMeshReconciler{Client: kube, APIReader: kube, Scheme: scheme, OperatorNamespace: "orama-system",
		ProjectorImage: "oramasearch/orama-operator:test", Now: func() time.Time { return time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC) }}
	reconcile := func() ctrl.Result {
		t.Helper()
		result, err := r.Reconcile(ctx, zettaTunnelMeshRequest)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	// Before the sandbox's mesh-only policies are rendered, nothing is projected.
	if result := reconcile(); result.RequeueAfter != zettaTunnelMeshEnrolledPoll {
		t.Fatalf("an enrolled namespace is not polled: %+v", result)
	}
	if template, _ := readMeshTemplate(t, kube, "node-a"); len(template.Workloads) != 0 {
		t.Fatalf("pods were projected before their policies were rendered: %+v", template)
	}
	markRendered(t, kube, sandbox)
	reconcile()

	nodeA, files := readMeshTemplate(t, kube, "node-a")
	if len(nodeA.Workloads) != 2 || nodeA.Workloads[0].Identity != "web-1.workspace.mesh.orama.internal" ||
		nodeA.Workloads[1].Identity != "web-2.workspace.mesh.orama.internal" || nodeA.Workloads[0].PodUID != "uid-web-1" {
		t.Fatalf("node-a workloads = %+v", nodeA.Workloads)
	}
	web := nodeA.Workloads[0]
	if len(web.Routes) != 1 || web.Routes[0] != (meshprojection.Route{
		Destination: "10.244.1.20:5432", Mesh: "10.244.1.20:15008", Identity: "db-0.workspace.mesh.orama.internal",
	}) || len(web.Ports) != 0 {
		t.Fatalf("web routes = %+v ports = %+v", web.Routes, web.Ports)
	}
	nodeB, _ := readMeshTemplate(t, kube, "node-b")
	if len(nodeB.Workloads) != 1 || len(nodeB.Workloads[0].Routes) != 0 || len(nodeB.Workloads[0].Ports) != 1 ||
		nodeB.Workloads[0].Ports[0].Port != 5432 || nodeB.Workloads[0].Ports[0].Target != "127.0.0.1:5432" ||
		strings.Join(nodeB.Workloads[0].Ports[0].Sources, ",") != "web-1.workspace.mesh.orama.internal,web-2.workspace.mesh.orama.internal" {
		t.Fatalf("db workload = %+v", nodeB.Workloads)
	}
	cert := parseTestCertificate(t, files[web.TLS.Certificate])
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != web.Identity {
		t.Fatalf("workload certificate names %v", cert.DNSNames)
	}
	var pki corev1.Secret
	if err := kube.Get(ctx, client.ObjectKey{Namespace: "orama-system", Name: zettaTunnelMeshPKISecret}, &pki); err != nil {
		t.Fatal(err)
	}
	if err := cert.CheckSignatureFrom(parseTestCertificate(t, files[zettaTunnelMeshCAFile])); err != nil {
		t.Fatal("the workload certificate is not signed by the projected CA")
	}
	for _, node := range []string{"node-a", "node-b"} {
		var shard corev1.Secret
		if err := kube.Get(ctx, client.ObjectKey{Namespace: zettaTunnelMeshNamespace, Name: meshNodeSecret(node)}, &shard); err != nil {
			t.Fatalf("node %s's Secret is missing, so its agent's mount would wait: %v", node, err)
		}
		for key, value := range shard.Data {
			if strings.Contains(string(value), "PRIVATE KEY") && strings.HasSuffix(key, ".key") && strings.Contains(string(value), string(pki.Data["ca.key"])) {
				t.Fatalf("the CA key leaked into node %s's Secret", node)
			}
		}
	}

	// Stable: a second pass keeps the same certificates.
	reconcile()
	_, again := readMeshTemplate(t, kube, "node-a")
	if string(again[web.TLS.Certificate]) != string(files[web.TLS.Certificate]) {
		t.Fatal("a steady pass reissued a workload certificate")
	}

	// A pod that goes leaves the projection; a node with no enrolled pod has no template.
	if err := kube.Delete(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-0", Namespace: testTunnelNS}}); err != nil {
		t.Fatal(err)
	}
	reconcile()
	if template, _ := readMeshTemplate(t, kube, "node-b"); len(template.Workloads) != 0 {
		t.Fatalf("node-b still projects %+v", template.Workloads)
	}
	if template, _ := readMeshTemplate(t, kube, "node-a"); len(template.Workloads[0].Routes) != 0 {
		t.Fatalf("a route to a gone pod remains: %+v", template.Workloads[0].Routes)
	}

	// Unenrolling stops projecting the namespace and stops polling it.
	var current oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, client.ObjectKeyFromObject(sandbox), &current); err != nil {
		t.Fatal(err)
	}
	current.Spec.Mesh = nil
	if err := kube.Update(ctx, &current); err != nil {
		t.Fatal(err)
	}
	if result := reconcile(); result.RequeueAfter == zettaTunnelMeshEnrolledPoll {
		t.Fatal("an unenrolled cluster is still polled")
	}
	if template, _ := readMeshTemplate(t, kube, "node-a"); len(template.Workloads) != 0 {
		t.Fatalf("an unenrolled namespace is still projected: %+v", template.Workloads)
	}
}

func TestMeshAgentIsPrivilegedInItsOwnNamespaceAndTheProjectorIsNot(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	kube := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "orama-system"}}, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}).Build()
	r := &zettaTunnelMeshReconciler{Client: kube, Scheme: scheme, OperatorNamespace: "orama-system", ProjectorImage: "oramasearch/orama-operator:test"}
	if _, err := r.Reconcile(ctx, zettaTunnelMeshRequest); err != nil {
		t.Fatal(err)
	}
	var namespace corev1.Namespace
	if err := kube.Get(ctx, client.ObjectKey{Name: zettaTunnelMeshNamespace}, &namespace); err != nil ||
		namespace.Labels["pod-security.kubernetes.io/enforce"] != "privileged" {
		t.Fatalf("mesh namespace = %+v (%v)", namespace.Labels, err)
	}
	var daemonSet appsv1.DaemonSet
	if err := kube.Get(ctx, client.ObjectKey{Namespace: zettaTunnelMeshNamespace, Name: meshNodeDaemonSet("node-a")}, &daemonSet); err != nil {
		t.Fatal(err)
	}
	pod := daemonSet.Spec.Template.Spec
	agent, projector := pod.Containers[0], pod.Containers[1]
	if !pod.HostNetwork || *pod.AutomountServiceAccountToken || len(pod.Tolerations) != 1 || pod.Tolerations[0].Operator != corev1.TolerationOpExists {
		t.Fatalf("agent pod = %+v", pod)
	}
	if !*agent.SecurityContext.Privileged || *agent.SecurityContext.RunAsUser != 0 ||
		strings.Join(agent.Args, " ") != "mesh --configuration /run/zetta-mesh/mesh.json --health-listen 127.0.0.1:15020 --audit -" ||
		agent.ReadinessProbe.HTTPGet.Host != "127.0.0.1" || agent.Image != DefaultZettaTunnelImage {
		t.Fatalf("agent = %+v", agent)
	}
	for _, mount := range agent.VolumeMounts {
		if !mount.ReadOnly {
			t.Fatalf("the agent mounts %s writable", mount.Name)
		}
	}
	if projector.SecurityContext.Privileged != nil || *projector.SecurityContext.AllowPrivilegeEscalation || !*projector.SecurityContext.RunAsNonRoot ||
		projector.Command[0] != "/mesh-projector" || projector.Image != "oramasearch/orama-operator:test" {
		t.Fatalf("projector = %+v", projector)
	}
	var secrets []string
	for _, volume := range pod.Volumes {
		if volume.Secret != nil {
			secrets = append(secrets, volume.Secret.SecretName)
		}
		if volume.HostPath != nil && volume.HostPath.Path != "/proc" {
			t.Fatalf("the agent mounts host path %s", volume.HostPath.Path)
		}
	}
	if len(secrets) != 1 || secrets[0] != meshNodeSecret("node-a") {
		t.Fatalf("node-a's agent mounts Secrets %v, want only %s", secrets, meshNodeSecret("node-a"))
	}
	if got := daemonSetNodes(&daemonSet, []string{"node-a", "node-b"}); len(got) != 1 || got[0] != "node-a" {
		t.Fatalf("node-a's agent can run on %v", got)
	}
	mounted := false
	for _, mount := range projector.VolumeMounts {
		if mount.Name == "template" && mount.MountPath == "/etc/zetta-mesh-templates/"+strconv.Itoa(meshprojection.Shard("node-a")) && mount.ReadOnly {
			mounted = true
		}
	}
	if !mounted {
		t.Fatalf("the projector does not read node-a's Secret where it looks for its template: %+v", projector.VolumeMounts)
	}
}

func TestEnrolledSandboxAllowsDeclaredConnectionsOnlyOnTheMeshPort(t *testing.T) {
	for _, enrolled := range []bool{false, true} {
		ctx := context.Background()
		scheme := newLakeScheme(t)
		sandbox := meshSandbox(enrolled)
		kube := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&oramav1alpha1.NetworkSandbox{}).WithObjects(sandbox).Build()
		r := &NetworkSandboxReconciler{Client: kube, Scheme: scheme}
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sandbox)}); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{lakeConnectionName(testConnectionID) + "-egress", lakeConnectionName(testConnectionID) + "-ingress"} {
			var policy networkingv1.NetworkPolicy
			if err := kube.Get(ctx, client.ObjectKey{Namespace: testTunnelNS, Name: name}, &policy); err != nil {
				t.Fatal(err)
			}
			var ports []networkingv1.NetworkPolicyPort
			for _, rule := range policy.Spec.Egress {
				ports = append(ports, rule.Ports...)
			}
			for _, rule := range policy.Spec.Ingress {
				ports = append(ports, rule.Ports...)
			}
			want := int32(5432)
			if enrolled {
				want = zettaTunnelMeshInboundPort
			}
			if len(ports) != 1 || ports[0].Port.IntVal != want {
				t.Fatalf("enrolled=%v %s ports = %+v, want only %d", enrolled, name, ports, want)
			}
		}
		var dns networkingv1.NetworkPolicy
		if err := kube.Get(ctx, client.ObjectKey{Namespace: testTunnelNS, Name: workspaceDNSName}, &dns); err != nil {
			t.Fatal(err)
		}
		if len(dns.Spec.Egress[0].Ports) != 2 {
			t.Fatalf("enrolment changed a non-Connection allowance: %+v", dns.Spec)
		}
	}
}

func TestLakeWorkspaceOverrideEnrolsItsSandbox(t *testing.T) {
	ctx := context.Background()
	scheme := newLakeScheme(t)
	lake := &oramav1alpha1.Lake{
		ObjectMeta: metav1.ObjectMeta{Name: "prod", UID: types.UID("lake-uid")},
		Spec: oramav1alpha1.LakeSpec{Namespace: "lake-system", Workspaces: oramav1alpha1.LakeWorkspacesSpec{
			Overrides: map[string]oramav1alpha1.LakeWorkspaceOverrideSpec{testWorkspaceID: {Mesh: true}},
		}},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(lake,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: testTunnelNS}}).Build()
	r := &LakeWorkspacesReconciler{Client: kube, Scheme: scheme}
	labels := map[string]string{workspaceLabel: testWorkspaceID, workspaceLakeLabel: lake.Name}
	_ = r.reconcileWorkspaceNetworkPolicies(ctx, lake, testTunnelNS, labels, false)
	var sandbox oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, client.ObjectKey{Namespace: testTunnelNS, Name: networkSandboxName}, &sandbox); err != nil {
		t.Fatal(err)
	}
	if !sandboxMeshEnrolled(&sandbox) {
		t.Fatal("the workspace override did not enrol the sandbox")
	}
	lake.Spec.Workspaces.Overrides = nil
	_ = r.reconcileWorkspaceNetworkPolicies(ctx, lake, testTunnelNS, labels, false)
	if err := kube.Get(ctx, client.ObjectKey{Namespace: testTunnelNS, Name: networkSandboxName}, &sandbox); err != nil {
		t.Fatal(err)
	}
	if sandboxMeshEnrolled(&sandbox) {
		t.Fatal("removing the override left the sandbox enrolled")
	}
}

func TestMeshEnvtestRendersMeshOnlyPoliciesAndAValidAgent(t *testing.T) {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		t.Skip("KUBEBUILDER_ASSETS is required for the mesh envtest")
	}
	environment := &envtest.Environment{BinaryAssetsDirectory: assets, CRDDirectoryPaths: []string{"../../config/crd/bases"},
		CRDs: networkSandboxCiliumTestCRDs(), ErrorIfCRDPathMissing: true}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = environment.Stop() })
	scheme := newLakeScheme(t)
	kube, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, name := range []string{testTunnelNS, "orama-system"} {
		if err := kube.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
			t.Fatal(err)
		}
	}
	sandbox := meshSandbox(true)
	sandbox.UID, sandbox.Generation = "", 0
	if err := kube.Create(ctx, sandbox); err != nil {
		t.Fatal(err)
	}
	sandboxes := &NetworkSandboxReconciler{Client: kube, Scheme: scheme}
	if _, err := sandboxes.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(sandbox)}); err != nil {
		t.Fatal(err)
	}
	var policy networkingv1.NetworkPolicy
	if err := kube.Get(ctx, client.ObjectKey{Namespace: testTunnelNS, Name: lakeConnectionName(testConnectionID) + "-ingress"}, &policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.Spec.Ingress[0].Ports) != 1 || policy.Spec.Ingress[0].Ports[0].Port.IntVal != zettaTunnelMeshInboundPort {
		t.Fatalf("the API server holds %+v", policy.Spec.Ingress)
	}
	if err := kube.Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}); err != nil {
		t.Fatal(err)
	}
	mesh := &zettaTunnelMeshReconciler{Client: kube, APIReader: kube, Scheme: scheme, OperatorNamespace: "orama-system", ProjectorImage: "oramasearch/orama-operator:test"}
	if _, err := mesh.Reconcile(ctx, zettaTunnelMeshRequest); err != nil {
		t.Fatalf("the API server refused the mesh objects: %v", err)
	}
	var daemonSet appsv1.DaemonSet
	if err := kube.Get(ctx, client.ObjectKey{Namespace: zettaTunnelMeshNamespace, Name: meshNodeDaemonSet("node-a")}, &daemonSet); err != nil {
		t.Fatal(err)
	}
	if len(daemonSet.Spec.Template.Spec.Containers) != 2 {
		t.Fatalf("DaemonSet = %+v", daemonSet.Spec.Template.Spec)
	}
}

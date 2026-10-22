/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/netip"
	"os"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	"github.com/oramasearch/orama-operator/internal/meshprojection"
)

// zetta-tunnel's mesh (decision D7): a privileged node agent on every node of every cluster, in its
// own system namespace, and the operator's projection of which pods it captures.
const (
	zettaTunnelMeshNamespace     = "zetta-tunnel-mesh"
	zettaTunnelMeshName          = "zetta-tunnel-mesh"
	zettaTunnelMeshPKISecret     = "zetta-tunnel-mesh-pki"
	zettaTunnelMeshInboundPort   = 15008
	zettaTunnelMeshProbePort     = 15020
	zettaTunnelMeshNodePrefix    = "mesh-node-"
	zettaTunnelMeshCAFile        = "workload-ca.pem"
	zettaTunnelMeshIdentitySuff  = ".mesh.orama.internal"
	zettaTunnelMeshMaxWorkloads  = 128
	zettaTunnelMeshMaxRoutes     = 256
	zettaTunnelMeshEnrolledPoll  = 3 * time.Second
	zettaTunnelMeshProjectionDir = "/run/zetta-mesh"
	zettaTunnelMeshTemplatesDir  = "/etc/zetta-mesh-templates"
)

func zettaTunnelMeshLabels() map[string]string {
	return map[string]string{zettaTunnelNameKey: zettaTunnelAppName, zettaTunnelComponentKey: "mesh"}
}

func zettaTunnelMeshObjectLabels() map[string]string {
	return withLabels(zettaTunnelMeshLabels(), map[string]string{workspaceManagedByLabel: zettaTunnelManagedBy})
}

// sandboxRendered reports whether the sandbox's current spec has been rendered into policies.
func sandboxRendered(sandbox *oramav1alpha1.NetworkSandbox) bool {
	if sandbox.Status.ObservedGeneration != sandbox.Generation {
		return false
	}
	for _, condition := range sandbox.Status.Conditions {
		if condition.Type == "Ready" {
			return condition.Status == metav1.ConditionTrue && condition.ObservedGeneration == sandbox.Generation
		}
	}
	return false
}

func sandboxMeshEnrolled(sandbox *oramav1alpha1.NetworkSandbox) bool {
	return sandbox.Spec.Mesh != nil && sandbox.Spec.Mesh.Enrolled
}

// meshOnlyPorts narrows every rule of a declared Connection to the mesh's mutual-TLS port: an
// enrolled pod reaches another only through the agents, never on the application port.
func meshOnlyPorts(spec *networkingv1.NetworkPolicySpec) {
	ports := tcpPorts(zettaTunnelMeshInboundPort)
	for i := range spec.Ingress {
		spec.Ingress[i].Ports = append([]networkingv1.NetworkPolicyPort(nil), ports...)
	}
	for i := range spec.Egress {
		spec.Egress[i].Ports = append([]networkingv1.NetworkPolicyPort(nil), ports...)
	}
}

// meshIdentity is a pod's identity in the mesh. It is per pod because zetta-tunnel refuses a node
// projection with two workloads of the same identity, and replicas share a node.
func meshIdentity(pod *corev1.Pod) string {
	return pod.Name + "." + pod.Namespace + zettaTunnelMeshIdentitySuff
}

// zettaTunnelMeshReconciler keeps the node agent running on every node and writes, per node, the
// template the node projector turns into the agent's projection.
type zettaTunnelMeshReconciler struct {
	client.Client
	APIReader         client.Reader
	Scheme            *runtime.Scheme
	Image             string
	OperatorNamespace string
	// ProjectorImage is the image of the node projector (the operator's own image). Empty means the
	// manager pod's own image.
	ProjectorImage string
	Now            func() time.Time
}

// +kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

var zettaTunnelMeshRequest = reconcile.Request{NamespacedName: client.ObjectKey{Namespace: zettaTunnelMeshNamespace, Name: zettaTunnelMeshName}}

func (r *zettaTunnelMeshReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *zettaTunnelMeshReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *zettaTunnelMeshReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx).WithName("zetta-tunnel-mesh")
	if _, err := ensureOperatorNamespace(ctx, r.reader(), r.Client, zettaTunnelMeshNamespace, map[string]string{
		workspaceManagedByLabel:              zettaTunnelManagedBy,
		"pod-security.kubernetes.io/enforce": "privileged",
		"pod-security.kubernetes.io/audit":   "privileged",
		"pod-security.kubernetes.io/warn":    "privileged",
	}); err != nil {
		return ctrl.Result{}, err
	}
	pullSecrets, err := copyOperatorPullSecrets(ctx, r.Client, r.APIReader, r.OperatorNamespace, zettaTunnelMeshNamespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	ca, err := r.ensureCA(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	templates, enrolled, err := r.templates(ctx, ca)
	if err != nil {
		return ctrl.Result{}, err
	}
	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList); err != nil {
		return ctrl.Result{}, err
	}
	nodes := make([]string, 0, len(nodeList.Items))
	for i := range nodeList.Items {
		nodes = append(nodes, nodeList.Items[i].Name)
	}
	sort.Strings(nodes)
	if err := r.reconcileNodeSecrets(ctx, nodes, templates); err != nil {
		return ctrl.Result{}, err
	}
	// The single DaemonSet and the shard Secrets every node mounted (before #788) go first: the new
	// agents bind the same host ports.
	if err := r.removeSharedTemplates(ctx); err != nil {
		return ctrl.Result{}, err
	}
	projectorImage, err := r.projectorImage(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	if projectorImage == "" {
		log.Info("the node agent waits for the projector image: the manager is not running in a pod")
	} else {
		for _, node := range nodes {
			if err := r.reconcileDaemonSet(ctx, pullSecrets, projectorImage, node); err != nil {
				return ctrl.Result{}, err
			}
		}
	}
	if err := r.removeGoneNodes(ctx, nodes); err != nil {
		return ctrl.Result{}, err
	}
	if enrolled {
		// Pods are read without an informer (the operator holds none cluster-wide), so a namespace in
		// the mesh is polled.
		return ctrl.Result{RequeueAfter: zettaTunnelMeshEnrolledPoll}, nil
	}
	return ctrl.Result{RequeueAfter: 10 * time.Minute}, nil
}

// ensureCA keeps the mesh's workload CA in the operator's namespace, where the node projector
// cannot read it.
func (r *zettaTunnelMeshReconciler) ensureCA(ctx context.Context) (tunnelCA, error) {
	namespace := r.OperatorNamespace
	if namespace == "" {
		namespace = zettaTunnelMeshNamespace
	}
	key := client.ObjectKey{Namespace: namespace, Name: zettaTunnelMeshPKISecret}
	var secret corev1.Secret
	err := r.Get(ctx, key, &secret)
	if apierrors.IsNotFound(err) {
		ca, err := newTunnelCA("zetta-tunnel mesh workload CA", r.now())
		if err != nil {
			return tunnelCA{}, err
		}
		secret = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelMeshPKISecret, Namespace: namespace, Labels: zettaTunnelMeshObjectLabels()},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"ca.crt": ca.CertPEM, "ca.key": ca.KeyPEM},
		}
		if err := r.Create(ctx, &secret); err != nil && !apierrors.IsAlreadyExists(err) {
			return tunnelCA{}, err
		}
		if err := r.Get(ctx, key, &secret); err != nil {
			return tunnelCA{}, err
		}
	} else if err != nil {
		return tunnelCA{}, err
	}
	ca := tunnelCA{CertPEM: secret.Data["ca.crt"], KeyPEM: secret.Data["ca.key"]}
	if _, _, err := parseTunnelCA(ca); err != nil {
		return tunnelCA{}, fmt.Errorf("Secret %s/%s: %w", namespace, zettaTunnelMeshPKISecret, err)
	}
	return ca, nil
}

// meshPod is an enrolled pod the agent can capture.
type meshPod struct {
	pod      *corev1.Pod
	identity string
	ip       netip.Addr
}

// nodeTemplate is one node's template and the files beside it.
type nodeTemplate struct {
	template meshprojection.Template
	files    map[string][]byte
}

// templates builds every node's template from the enrolled sandboxes, their running pods and their
// declared Connections.
func (r *zettaTunnelMeshReconciler) templates(ctx context.Context, ca tunnelCA) (map[string]*nodeTemplate, bool, error) {
	var sandboxes oramav1alpha1.NetworkSandboxList
	if err := r.List(ctx, &sandboxes); err != nil {
		return nil, false, err
	}
	nodes := map[string]*nodeTemplate{}
	enrolled := false
	existing, err := r.existingShards(ctx)
	if err != nil {
		return nil, false, err
	}
	for i := range sandboxes.Items {
		sandbox := &sandboxes.Items[i]
		if !sandboxMeshEnrolled(sandbox) || !sandbox.DeletionTimestamp.IsZero() {
			continue
		}
		enrolled = true
		// Step 1 of zetta-tunnel's enrolment order: the mesh-only policies are rendered before any
		// pod of the sandbox is projected.
		if !sandboxRendered(sandbox) {
			continue
		}
		pods, err := r.enrolledPods(ctx, sandbox)
		if err != nil {
			return nil, true, err
		}
		routes, ports := meshPaths(sandbox, pods)
		for _, member := range pods {
			node := nodes[member.pod.Spec.NodeName]
			if node == nil {
				node = &nodeTemplate{
					template: meshprojection.Template{Node: member.pod.Spec.NodeName},
					files:    map[string][]byte{zettaTunnelMeshCAFile: append([]byte(nil), ca.CertPEM...)},
				}
				nodes[member.pod.Spec.NodeName] = node
			}
			if len(node.template.Workloads) >= zettaTunnelMeshMaxWorkloads {
				// Left out: no capture, and Cilium still refuses its application ports.
				continue
			}
			certName, keyName := member.identity+".crt", member.identity+".key"
			request := tunnelLeafRequest{CommonName: member.identity, DNSNames: []string{member.identity}, Server: true, Client: true}
			cert, key := existing[certName], existing[keyName]
			if !tunnelLeafCurrent(cert, key, ca.CertPEM, request, r.now()) {
				cert, key, err = issueTunnelLeaf(ca, request, r.now())
				if err != nil {
					return nil, true, err
				}
			}
			node.files[certName], node.files[keyName] = cert, key
			workloadRoutes := routes[member.identity]
			if len(workloadRoutes) > zettaTunnelMeshMaxRoutes {
				workloadRoutes = workloadRoutes[:zettaTunnelMeshMaxRoutes]
			}
			node.template.Workloads = append(node.template.Workloads, meshprojection.TemplateWorkload{
				PodUID:   string(member.pod.UID),
				Identity: member.identity,
				TLS:      meshprojection.TLS{Certificate: certName, Key: keyName, CA: zettaTunnelMeshCAFile},
				Routes:   workloadRoutes,
				Ports:    ports[member.identity],
			})
		}
	}
	for _, node := range nodes {
		sort.Slice(node.template.Workloads, func(i, j int) bool { return node.template.Workloads[i].Identity < node.template.Workloads[j].Identity })
	}
	return nodes, enrolled, nil
}

// enrolledPods are the sandbox's running pods with an IPv4 address on a node.
func (r *zettaTunnelMeshReconciler) enrolledPods(ctx context.Context, sandbox *oramav1alpha1.NetworkSandbox) ([]meshPod, error) {
	selector, err := metav1.LabelSelectorAsSelector(&sandbox.Spec.PodSelector)
	if err != nil {
		return nil, err
	}
	var pods corev1.PodList
	if err := r.reader().List(ctx, &pods, client.InNamespace(sandbox.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}
	var out []meshPod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName == "" || pod.Spec.HostNetwork || !pod.DeletionTimestamp.IsZero() ||
			pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		ip, err := netip.ParseAddr(pod.Status.PodIP)
		if err != nil || !ip.Is4() {
			continue
		}
		out = append(out, meshPod{pod: pod, identity: meshIdentity(pod), ip: ip})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].identity < out[j].identity })
	return out, nil
}

// meshPaths turns the sandbox's declared Connection allowances into each pod's routes (where it may
// connect) and ports (who may connect to it), keyed by identity. The allowances are read from the
// sandbox spec, which keeps the application ports; only the rendered policies are narrowed to the
// mesh port.
func meshPaths(sandbox *oramav1alpha1.NetworkSandbox, pods []meshPod) (map[string][]meshprojection.Route, map[string][]meshprojection.Port) {
	routes := map[string][]meshprojection.Route{}
	sources := map[string]map[int32]map[string]bool{}
	seenRoute := map[string]bool{}
	for _, allowance := range sandbox.Spec.Policies {
		if !isConnectionAllowanceName(allowance.Name) || !strings.HasSuffix(allowance.Name, "-egress") {
			continue
		}
		sourceSelector, err := metav1.LabelSelectorAsSelector(&allowance.Spec.PodSelector)
		if err != nil {
			continue
		}
		for _, rule := range allowance.Spec.Egress {
			for _, peer := range rule.To {
				if peer.PodSelector == nil || peer.NamespaceSelector != nil || peer.IPBlock != nil {
					continue
				}
				targetSelector, err := metav1.LabelSelectorAsSelector(peer.PodSelector)
				if err != nil {
					continue
				}
				for _, source := range pods {
					if !sourceSelector.Matches(labels.Set(source.pod.Labels)) {
						continue
					}
					for _, target := range pods {
						if target.identity == source.identity || !targetSelector.Matches(labels.Set(target.pod.Labels)) {
							continue
						}
						for _, policyPort := range rule.Ports {
							port, ok := meshPolicyPort(policyPort, target.pod)
							if !ok {
								continue
							}
							destination := fmt.Sprintf("%s:%d", target.ip, port)
							key := source.identity + " " + destination
							if !seenRoute[key] {
								seenRoute[key] = true
								routes[source.identity] = append(routes[source.identity], meshprojection.Route{
									Destination: destination,
									Mesh:        fmt.Sprintf("%s:%d", target.ip, zettaTunnelMeshInboundPort),
									Identity:    target.identity,
								})
							}
							if sources[target.identity] == nil {
								sources[target.identity] = map[int32]map[string]bool{}
							}
							if sources[target.identity][port] == nil {
								sources[target.identity][port] = map[string]bool{}
							}
							sources[target.identity][port][source.identity] = true
						}
					}
				}
			}
		}
	}
	ports := map[string][]meshprojection.Port{}
	for identity, byPort := range sources {
		numbers := make([]int, 0, len(byPort))
		for port := range byPort {
			numbers = append(numbers, int(port))
		}
		sort.Ints(numbers)
		for _, number := range numbers {
			var names []string
			for name := range byPort[int32(number)] {
				names = append(names, name)
			}
			sort.Strings(names)
			ports[identity] = append(ports[identity], meshprojection.Port{
				Port: int32(number), Target: fmt.Sprintf("127.0.0.1:%d", number), Sources: names,
			})
		}
	}
	for identity := range routes {
		sort.Slice(routes[identity], func(i, j int) bool { return routes[identity][i].Destination < routes[identity][j].Destination })
	}
	return routes, ports
}

// meshPolicyPort is a TCP NetworkPolicy port as a number on target, resolving a named port.
func meshPolicyPort(port networkingv1.NetworkPolicyPort, target *corev1.Pod) (int32, bool) {
	if port.Protocol != nil && *port.Protocol != corev1.ProtocolTCP || port.Port == nil {
		return 0, false
	}
	if port.Port.Type == intstr.Int {
		return port.Port.IntVal, port.Port.IntVal > 0
	}
	for _, container := range target.Spec.Containers {
		for _, containerPort := range container.Ports {
			if containerPort.Name == port.Port.StrVal && (containerPort.Protocol == "" || containerPort.Protocol == corev1.ProtocolTCP) {
				return containerPort.ContainerPort, true
			}
		}
	}
	return 0, false
}

// Each node's template, and the workload certificates and private keys it names, are in that
// node's own Secret, mounted only by that node's agent (orama-operator#788, LSR-039). The kubelet
// delivers a Secret to a node only for a pod bound there that mounts it, so root on one node reads
// only the keys of the workloads on that node, never another node's.

// meshNodeKey is a fixed-length name for a node: FNV-1a 64 of its name, in hex. A node name can be
// longer than an object name allows.
func meshNodeKey(node string) string {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(node))
	return fmt.Sprintf("%016x", hash.Sum64())
}

// meshNodeSecret is the name of a node's template Secret.
func meshNodeSecret(node string) string { return zettaTunnelMeshNodePrefix + meshNodeKey(node) }

// meshNodeDaemonSet is the name of a node's agent DaemonSet.
func meshNodeDaemonSet(node string) string { return zettaTunnelMeshName + "-" + meshNodeKey(node) }

const (
	// meshNodeLabel carries meshNodeKey on a node's Secret, DaemonSet and pods.
	meshNodeLabel = "orama.cloud/mesh-node"
	// meshNodeNameAnnotation carries the node's full name.
	meshNodeNameAnnotation = "orama.cloud/mesh-node-name"
)

func meshNodeLabels(node string) map[string]string {
	return withLabels(zettaTunnelMeshLabels(), map[string]string{meshNodeLabel: meshNodeKey(node)})
}

func meshNodeObjectLabels(node string) map[string]string {
	return withLabels(meshNodeLabels(node), map[string]string{workspaceManagedByLabel: zettaTunnelManagedBy})
}

// existingShards is every workload certificate and key already issued, so a steady pass reissues
// none.
func (r *zettaTunnelMeshReconciler) existingShards(ctx context.Context) (map[string][]byte, error) {
	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, client.InNamespace(zettaTunnelMeshNamespace), client.HasLabels{meshNodeLabel}); err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for i := range secrets.Items {
		if secrets.Items[i].Labels[workspaceManagedByLabel] != zettaTunnelManagedBy {
			continue
		}
		for key, value := range secrets.Items[i].Data {
			out[key] = value
		}
	}
	return out, nil
}

// reconcileNodeSecrets writes each node's template, and only the files it names, into that node's
// Secret. Every node has one, empty or not, so its agent's mount never waits.
func (r *zettaTunnelMeshReconciler) reconcileNodeSecrets(ctx context.Context, nodes []string, templates map[string]*nodeTemplate) error {
	for _, node := range nodes {
		data := map[string][]byte{}
		if entry := templates[node]; entry != nil {
			encoded, err := json.Marshal(entry.template)
			if err != nil {
				return err
			}
			data[meshprojection.TemplateFile(node)] = encoded
			for name, value := range entry.files {
				data[name] = value
			}
		}
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: meshNodeSecret(node), Namespace: zettaTunnelMeshNamespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
			if secret.ResourceVersion != "" && secret.Labels[workspaceManagedByLabel] != zettaTunnelManagedBy {
				return fmt.Errorf("Secret %s/%s is not the operator's", zettaTunnelMeshNamespace, secret.Name)
			}
			secret.Labels = meshNodeObjectLabels(node)
			secret.Annotations = map[string]string{meshNodeNameAnnotation: node}
			secret.Type = corev1.SecretTypeOpaque
			secret.Data = data
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// removeSharedTemplates deletes the operator's single DaemonSet and the shard Secrets it mounted on
// every node, which held every node's keys.
func (r *zettaTunnelMeshReconciler) removeSharedTemplates(ctx context.Context) error {
	legacy := []client.Object{&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: zettaTunnelMeshName, Namespace: zettaTunnelMeshNamespace}}}
	for i := 0; i < meshprojection.Shards; i++ {
		legacy = append(legacy, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: meshprojection.ShardSecret(i), Namespace: zettaTunnelMeshNamespace}})
	}
	for _, object := range legacy {
		if err := r.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if object.GetLabels()[workspaceManagedByLabel] != zettaTunnelManagedBy {
			continue
		}
		if err := r.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// removeGoneNodes deletes the DaemonSet and Secret of every node that is no longer in the cluster.
func (r *zettaTunnelMeshReconciler) removeGoneNodes(ctx context.Context, nodes []string) error {
	current := map[string]bool{}
	for _, node := range nodes {
		current[meshNodeKey(node)] = true
	}
	var daemonSets appsv1.DaemonSetList
	if err := r.List(ctx, &daemonSets, client.InNamespace(zettaTunnelMeshNamespace), client.HasLabels{meshNodeLabel}); err != nil {
		return err
	}
	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, client.InNamespace(zettaTunnelMeshNamespace), client.HasLabels{meshNodeLabel}); err != nil {
		return err
	}
	var stale []client.Object
	for i := range daemonSets.Items {
		stale = append(stale, &daemonSets.Items[i])
	}
	for i := range secrets.Items {
		stale = append(stale, &secrets.Items[i])
	}
	for _, object := range stale {
		labels := object.GetLabels()
		if current[labels[meshNodeLabel]] || labels[workspaceManagedByLabel] != zettaTunnelManagedBy {
			continue
		}
		if err := r.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// projectorImage is the operator's own image, read from the manager pod.
func (r *zettaTunnelMeshReconciler) projectorImage(ctx context.Context) (string, error) {
	if r.ProjectorImage != "" {
		return r.ProjectorImage, nil
	}
	podName := os.Getenv("HOSTNAME")
	if r.APIReader == nil || r.OperatorNamespace == "" || podName == "" {
		return "", nil
	}
	var pod corev1.Pod
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: r.OperatorNamespace, Name: podName}, &pod); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	for _, container := range pod.Spec.Containers {
		if container.Name == "manager" {
			return container.Image, nil
		}
	}
	return "", nil
}

func (r *zettaTunnelMeshReconciler) image() string {
	if r.Image != "" {
		return r.Image
	}
	return DefaultZettaTunnelImage
}

// reconcileDaemonSet keeps one node's agent: a DaemonSet whose required node affinity names only
// that node, mounting only that node's Secret. The Secret is mounted where the projector reads its
// node's template (<templates>/<Shard(node)>), so the projector is unchanged.
func (r *zettaTunnelMeshReconciler) reconcileDaemonSet(ctx context.Context, pullSecrets []string, projectorImage, node string) error {
	var references []corev1.LocalObjectReference
	for _, name := range pullSecrets {
		references = append(references, corev1.LocalObjectReference{Name: name})
	}
	hostPathDirectory := corev1.HostPathDirectory
	templateMount := corev1.VolumeMount{Name: "template", MountPath: fmt.Sprintf("%s/%d", zettaTunnelMeshTemplatesDir, meshprojection.Shard(node)), ReadOnly: true}
	templateVolume := corev1.Volume{Name: "template", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName: meshNodeSecret(node), DefaultMode: int32Ptr(0o400), Optional: boolPtr(true),
	}}}
	name := meshNodeDaemonSet(node)
	daemonSet := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: zettaTunnelMeshNamespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, daemonSet, func() error {
		if daemonSet.ResourceVersion != "" && daemonSet.Labels[workspaceManagedByLabel] != zettaTunnelManagedBy {
			return fmt.Errorf("DaemonSet %s/%s is not the operator's", zettaTunnelMeshNamespace, name)
		}
		daemonSet.Labels = meshNodeObjectLabels(node)
		daemonSet.Annotations = map[string]string{meshNodeNameAnnotation: node}
		daemonSet.Spec.Selector = &metav1.LabelSelector{MatchLabels: meshNodeLabels(node)}
		daemonSet.Spec.UpdateStrategy = appsv1.DaemonSetUpdateStrategy{
			Type:          appsv1.RollingUpdateDaemonSetStrategyType,
			RollingUpdate: &appsv1.RollingUpdateDaemonSet{MaxUnavailable: intstrPtr(intstr.FromInt32(1))},
		}
		daemonSet.Spec.Template.Labels = meshNodeObjectLabels(node)
		daemonSet.Spec.Template.Spec = corev1.PodSpec{
			// Only this node: its Secret holds this node's workload keys and no other's.
			Affinity: &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
				RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchFields: []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}},
				}}},
			}},
			AutomountServiceAccountToken:  boolPtr(false),
			ImagePullSecrets:              references,
			HostNetwork:                   true,
			DNSPolicy:                     corev1.DNSClusterFirstWithHostNet,
			PriorityClassName:             "system-node-critical",
			Tolerations:                   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			TerminationGracePeriodSeconds: int64Ptr(30),
			Containers: []corev1.Container{
				{
					Name:            "agent",
					Image:           r.image(),
					ImagePullPolicy: corev1.PullIfNotPresent,
					Args: []string{
						"mesh",
						"--configuration", zettaTunnelMeshProjectionDir + "/" + meshprojection.ProjectionFile,
						"--health-listen", fmt.Sprintf("127.0.0.1:%d", zettaTunnelMeshProbePort),
						"--audit", "-",
					},
					// Entering pod network namespaces and installing capture needs a privileged root
					// process (zetta-tunnel docs/mesh.md). Only this agent has it; workloads stay restricted.
					SecurityContext: &corev1.SecurityContext{
						Privileged:   boolPtr(true),
						RunAsUser:    int64Ptr(0),
						RunAsNonRoot: boolPtr(false),
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Host: "127.0.0.1", Path: "/readyz", Port: intstr.FromInt32(zettaTunnelMeshProbePort)}},
						PeriodSeconds: 5, TimeoutSeconds: 3, FailureThreshold: 3,
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Host: "127.0.0.1", Path: "/healthz", Port: intstr.FromInt32(zettaTunnelMeshProbePort)}},
						PeriodSeconds: 10, TimeoutSeconds: 3, FailureThreshold: 3,
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
						Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("512Mi")},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "host-proc", MountPath: "/host/proc", ReadOnly: true},
						{Name: "projection", MountPath: zettaTunnelMeshProjectionDir, ReadOnly: true},
					},
				},
				{
					Name:            "projector",
					Image:           projectorImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Command:         []string{"/mesh-projector"},
					Args: []string{
						"--templates", zettaTunnelMeshTemplatesDir,
						"--proc", "/host/proc",
						"--output", zettaTunnelMeshProjectionDir,
					},
					Env: []corev1.EnvVar{{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}}},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: boolPtr(false),
						ReadOnlyRootFilesystem:   boolPtr(true),
						RunAsNonRoot:             boolPtr(true),
						RunAsUser:                int64Ptr(65532),
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
						Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: resource.MustParse("128Mi")},
					},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "host-proc", MountPath: "/host/proc", ReadOnly: true},
						{Name: "projection", MountPath: zettaTunnelMeshProjectionDir},
						templateMount,
					},
				},
			},
			Volumes: []corev1.Volume{
				{Name: "host-proc", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/proc", Type: &hostPathDirectory}}},
				{Name: "projection", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
				templateVolume,
			},
		}
		return nil
	})
	return err
}

func (r *zettaTunnelMeshReconciler) request(context.Context, client.Object) []reconcile.Request {
	return []reconcile.Request{zettaTunnelMeshRequest}
}

type zettaTunnelMeshStartup struct{ r *zettaTunnelMeshReconciler }

func (s zettaTunnelMeshStartup) Start(ctx context.Context) error {
	if _, err := s.r.Reconcile(ctx, zettaTunnelMeshRequest); err != nil {
		logf.FromContext(ctx).Error(err, "first mesh reconcile failed; the watches retry it")
	}
	return nil
}

func (s zettaTunnelMeshStartup) NeedLeaderElection() bool { return true }

func (r *zettaTunnelMeshReconciler) SetupWithManager(mgr ctrl.Manager) error {
	all := handler.EnqueueRequestsFromMapFunc(r.request)
	own := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, object client.Object) []reconcile.Request {
		if object.GetNamespace() != zettaTunnelMeshNamespace {
			return nil
		}
		return r.request(ctx, object)
	})
	if err := ctrl.NewControllerManagedBy(mgr).
		Named("zettatunnel-mesh").
		Watches(&oramav1alpha1.NetworkSandbox{}, all).
		Watches(&oramav1alpha1.Connection{}, all).
		Watches(&corev1.Node{}, all).
		Watches(&appsv1.DaemonSet{}, own).
		Watches(&corev1.Secret{}, own).
		Complete(r); err != nil {
		return err
	}
	return mgr.Add(zettaTunnelMeshStartup{r: r})
}

/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The workspace egress gateway (operator#470, decision D3): zetta-tunnel's `egress` role, one
// Deployment per workspace that has a DNS-name EgressRule, configured exactly as zetta-tunnel's
// docs/egress.md specifies. The operator publishes the rule snapshot as a ConfigMap (kubelet
// replaces the mounted file atomically) and annotates the gateway pods with its version so the
// kubelet refreshes it promptly. The gateway is removed when the workspace's last name rule goes.

const (
	egressGatewayName            = "zetta-tunnel-egress"
	egressGatewayComponentLabel  = "orama.cloud/component"
	egressGatewayComponent       = "egress-gateway"
	egressGatewayPort            = 7445
	egressGatewayHealthPort      = 8080
	egressGatewayRulesKey        = "rules.json"
	egressGatewayRulesDir        = "/etc/zetta-tunnel/egress"
	egressGatewayRulesVersionKey = "orama.cloud/egress-rules-version"
	// egressGatewayImage is zetta-tunnel's private image. Moving it is an operator change, like
	// every other image the operator runs; the Lake's pull Secret pulls it.
	egressGatewayImage = "oramasearch/zetta-tunnel:dev-933d6fc"
	// The gateway's own limits (docs/egress.md).
	egressGatewayMaxRules     = 10000
	egressGatewayMaxSourceIPs = 256
)

// egressGatewayRule is one entry of the projected snapshot.
type egressGatewayRule struct {
	ID        string   `json:"id"`
	Workspace string   `json:"workspace"`
	SourceIPs []string `json:"source_ips"`
	DNSName   string   `json:"dns_name"`
	Ports     []int32  `json:"ports"`
}

type egressGatewaySnapshot struct {
	Rules []egressGatewayRule `json:"rules"`
}

func egressGatewayLabels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       egressGatewayName,
		"app.kubernetes.io/managed-by": "orama-operator",
		egressGatewayComponentLabel:    egressGatewayComponent,
	}
}

// podIPs is the numeric addresses of the running pods the selector chooses, the gateway excluded.
func podIPs(ctx context.Context, reader client.Reader, namespace string, selector metav1.LabelSelector) ([]string, error) {
	parsed, err := metav1.LabelSelectorAsSelector(&selector)
	if err != nil {
		return nil, err
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: parsed}); err != nil {
		return nil, err
	}
	gateway := labels.SelectorFromSet(map[string]string{egressGatewayComponentLabel: egressGatewayComponent})
	var ips []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed ||
			gateway.Matches(labels.Set(pod.Labels)) {
			continue
		}
		addresses := pod.Status.PodIPs
		if len(addresses) == 0 && pod.Status.PodIP != "" {
			addresses = []corev1.PodIP{{IP: pod.Status.PodIP}}
		}
		for _, address := range addresses {
			if parsed, err := netip.ParseAddr(address.IP); err == nil {
				ips = append(ips, parsed.String())
			}
		}
	}
	sort.Strings(ips)
	return sortedUnique(ips), nil
}

// egressGatewayEntry is a DNS-name rule with its current source addresses, before overlaps are
// resolved.
type egressGatewayEntry struct {
	rule *oramav1alpha1.EgressRule
	ips  []string
}

// buildEgressGatewaySnapshot turns the namespace's present name rules into the gateway snapshot.
// The gateway refuses a snapshot in which two rules authorize the same source, name and port, so
// a later rule (by id) gives up a source address that an earlier rule already authorizes for the
// same name on every one of its ports. A partial overlap cannot be expressed and is reported for
// the later rule, which then authorizes nothing for that address. A rule with no running source
// pods is omitted (it authorizes nobody either way). Problems are keyed by rule name.
func buildEgressGatewaySnapshot(namespace string, entries []egressGatewayEntry) (egressGatewaySnapshot, map[string]string) {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i].rule, entries[j].rule
		if (a.Spec.SourceKind == "") != (b.Spec.SourceKind == "") {
			return a.Spec.SourceKind == ""
		}
		return a.Spec.RuleID < b.Spec.RuleID
	})
	problems := map[string]string{}
	claimed := map[string]string{}
	snapshot := egressGatewaySnapshot{Rules: []egressGatewayRule{}}
	for _, entry := range entries {
		rule := entry.rule
		ports := sortedUniqueInt32(sortedInt32(append([]int32(nil), rule.Spec.Ports...)))
		var kept []string
		for _, ip := range entry.ips {
			covered, overlapping := 0, ""
			for _, port := range ports {
				if owner, found := claimed[ip+"|"+rule.Spec.DNSName+"|"+strconv.Itoa(int(port))]; found {
					covered++
					overlapping = owner
				}
			}
			switch {
			case covered == 0:
				kept = append(kept, ip)
			case covered < len(ports):
				problems[rule.Name] = fmt.Sprintf(
					"overlaps EgressRule %s for %s on some ports; overlapping rules for one name must list the same ports",
					overlapping, rule.Spec.DNSName)
			}
		}
		if len(kept) > egressGatewayMaxSourceIPs {
			problems[rule.Name] = fmt.Sprintf("selects %d source pods; the egress gateway accepts at most %d per rule",
				len(kept), egressGatewayMaxSourceIPs)
			continue
		}
		if len(kept) == 0 {
			continue
		}
		if len(snapshot.Rules) == egressGatewayMaxRules {
			problems[rule.Name] = fmt.Sprintf("the workspace has more than %d name rules", egressGatewayMaxRules)
			continue
		}
		for _, ip := range kept {
			for _, port := range ports {
				claimed[ip+"|"+rule.Spec.DNSName+"|"+strconv.Itoa(int(port))] = rule.Name
			}
		}
		snapshot.Rules = append(snapshot.Rules, egressGatewayRule{
			ID:        rule.Spec.RuleID,
			Workspace: namespace,
			SourceIPs: kept,
			DNSName:   rule.Spec.DNSName,
			Ports:     ports,
		})
	}
	return snapshot, problems
}

// encodeEgressGatewaySnapshot is the exact file the gateway reads: {"rules": [...]}, never null.
func encodeEgressGatewaySnapshot(snapshot egressGatewaySnapshot) ([]byte, error) {
	if snapshot.Rules == nil {
		snapshot.Rules = []egressGatewayRule{}
	}
	return json.Marshal(snapshot)
}

// egressGatewayState is what the gateway reconcile observed, for the rules' status.
type egressGatewayState struct {
	Ready    bool
	Problems map[string]string
}

// reconcileEgressGateway renders or removes the namespace's gateway from its present name rules.
func reconcileEgressGateway(
	ctx context.Context,
	kube client.Client,
	lake *oramav1alpha1.Lake,
	namespace string,
) (egressGatewayState, error) {
	var rules oramav1alpha1.EgressRuleList
	if err := kube.List(ctx, &rules, client.InNamespace(namespace)); err != nil {
		return egressGatewayState{}, err
	}
	var entries []egressGatewayEntry
	for i := range rules.Items {
		rule := &rules.Items[i]
		if rule.Spec.DNSName == "" || rule.Spec.RuleID == "" || rule.Spec.Desired != oramav1alpha1.EgressRuleDesiredPresent ||
			!rule.DeletionTimestamp.IsZero() || !metav1.IsControlledBy(rule, lake) || validateEgressRuleSpec(&rule.Spec) != nil {
			continue
		}
		selector := metav1.LabelSelector{}
		if rule.Spec.SourceKind != "" {
			var err error
			selector, err = lakeWorkloadSelector(ctx, kube, rule.Spec.LakeRef, namespace, rule.Spec.SourceKind, rule.Spec.SourceID)
			if err != nil {
				// A source not there yet has no addresses to authorize.
				continue
			}
		}
		ips, err := podIPs(ctx, kube, namespace, selector)
		if err != nil {
			return egressGatewayState{}, err
		}
		entries = append(entries, egressGatewayEntry{rule: rule, ips: ips})
	}
	if len(entries) == 0 {
		return egressGatewayState{}, deleteEgressGateway(ctx, kube, lake, namespace)
	}
	snapshot, problems := buildEgressGatewaySnapshot(namespace, entries)
	encoded, err := encodeEgressGatewaySnapshot(snapshot)
	if err != nil {
		return egressGatewayState{}, err
	}
	gatewayLabels := egressGatewayLabels()
	gatewayLabels[serviceLakeLabel] = lake.Name

	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: egressGatewayName, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, kube, config, func() error {
		if config.ResourceVersion != "" && !metav1.IsControlledBy(config, lake) {
			return fmt.Errorf("ConfigMap %s/%s is not controlled by Lake %q", namespace, egressGatewayName, lake.Name)
		}
		config.Labels = withLabels(config.Labels, gatewayLabels)
		config.Data = map[string]string{egressGatewayRulesKey: string(encoded)}
		return controllerutil.SetControllerReference(lake, config, kube.Scheme())
	}); err != nil {
		return egressGatewayState{}, err
	}

	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: egressGatewayName, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, kube, service, func() error {
		if service.ResourceVersion != "" && !metav1.IsControlledBy(service, lake) {
			return fmt.Errorf("Service %s/%s is not controlled by Lake %q", namespace, egressGatewayName, lake.Name)
		}
		service.Labels = withLabels(service.Labels, gatewayLabels)
		service.Spec.Selector = egressGatewayLabels()
		// 443 carries TLS with the destination's SNI; 7445 the explicit ZTEG1 preface.
		service.Spec.Ports = []corev1.ServicePort{
			{Name: "tls", Protocol: corev1.ProtocolTCP, Port: 443, TargetPort: intstr.FromInt32(egressGatewayPort)},
			{Name: "explicit", Protocol: corev1.ProtocolTCP, Port: egressGatewayPort, TargetPort: intstr.FromInt32(egressGatewayPort)},
		}
		return controllerutil.SetControllerReference(lake, service, kube.Scheme())
	}); err != nil {
		return egressGatewayState{}, err
	}

	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: egressGatewayName, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, kube, deployment, func() error {
		if deployment.ResourceVersion != "" && !metav1.IsControlledBy(deployment, lake) {
			return fmt.Errorf("Deployment %s/%s is not controlled by Lake %q", namespace, egressGatewayName, lake.Name)
		}
		deployment.Labels = withLabels(deployment.Labels, gatewayLabels)
		deployment.Spec = egressGatewayDeploymentSpec(lake)
		return controllerutil.SetControllerReference(lake, deployment, kube.Scheme())
	}); err != nil {
		return egressGatewayState{}, err
	}
	refreshEgressGatewayPods(ctx, kube, namespace, config.ResourceVersion)
	return egressGatewayState{Ready: deployment.Status.ReadyReplicas > 0, Problems: problems}, nil
}

func egressGatewayDeploymentSpec(lake *oramav1alpha1.Lake) appsv1.DeploymentSpec {
	replicas := int32(1)
	falseValue := false
	trueValue := true
	user := int64(65532)
	var pullSecrets []corev1.LocalObjectReference
	if lake.Spec.PullSecretName != "" {
		pullSecrets = []corev1.LocalObjectReference{{Name: lake.Spec.PullSecretName}}
	}
	probe := func(path string) *corev1.Probe {
		return &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: path, Port: intstr.FromInt32(egressGatewayHealthPort),
			}},
			PeriodSeconds:  5,
			TimeoutSeconds: 5,
		}
	}
	return appsv1.DeploymentSpec{
		Replicas: &replicas,
		Selector: &metav1.LabelSelector{MatchLabels: egressGatewayLabels()},
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: egressGatewayLabels()},
			Spec: corev1.PodSpec{
				AutomountServiceAccountToken: &falseValue,
				ImagePullSecrets:             pullSecrets,
				SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot:   &trueValue,
					RunAsUser:      &user,
					RunAsGroup:     &user,
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
				Containers: []corev1.Container{{
					Name:  "egress",
					Image: egressGatewayImage,
					// The exact contract of zetta-tunnel's egress role: no environment, every
					// listener explicit. The probe listener is reachable only by the kubelet: no
					// NetworkPolicy admits port 8080.
					Args: []string{
						"egress",
						"--rules", egressGatewayRulesDir + "/" + egressGatewayRulesKey,
						"--listen", fmt.Sprintf("0.0.0.0:%d", egressGatewayPort),
						"--health-listen", fmt.Sprintf("0.0.0.0:%d", egressGatewayHealthPort),
						"--sni-port", "443",
						"--audit", "-",
					},
					Ports: []corev1.ContainerPort{
						{Name: "egress", ContainerPort: egressGatewayPort, Protocol: corev1.ProtocolTCP},
						{Name: "health", ContainerPort: egressGatewayHealthPort, Protocol: corev1.ProtocolTCP},
					},
					ReadinessProbe: probe("/readyz"),
					LivenessProbe:  probe("/healthz"),
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("50m"),
							corev1.ResourceMemory: resource.MustParse("32Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: &falseValue,
						ReadOnlyRootFilesystem:   &trueValue,
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					},
					VolumeMounts: []corev1.VolumeMount{{Name: "rules", MountPath: egressGatewayRulesDir, ReadOnly: true}},
				}},
				Volumes: []corev1.Volume{{
					Name: "rules",
					VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: egressGatewayName},
					}},
				}},
			},
		},
	}
}

// refreshEgressGatewayPods annotates the gateway pods with the snapshot version: the kubelet then
// syncs the pod and replaces the mounted file without waiting for its periodic resync, so a
// removed rule or a new source address takes effect in seconds. Best effort.
func refreshEgressGatewayPods(ctx context.Context, kube client.Client, namespace, version string) {
	var pods corev1.PodList
	if err := kube.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels(egressGatewayLabels())); err != nil {
		return
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil || pod.Annotations[egressGatewayRulesVersionKey] == version {
			continue
		}
		before := pod.DeepCopy()
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[egressGatewayRulesVersionKey] = version
		_ = kube.Patch(ctx, pod, client.MergeFrom(before))
	}
}

func deleteEgressGateway(ctx context.Context, kube client.Client, lake *oramav1alpha1.Lake, namespace string) error {
	for _, object := range []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: egressGatewayName, Namespace: namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: egressGatewayName, Namespace: namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: egressGatewayName, Namespace: namespace}},
	} {
		if err := kube.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(object, lake) {
			continue
		}
		if err := kube.Delete(ctx, object); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// egressGatewayAddress is what a workload uses to reach a named destination.
func egressGatewayAddress(namespace string) string {
	return strings.Join([]string{egressGatewayName, namespace, "svc", "cluster", "local"}, ".")
}

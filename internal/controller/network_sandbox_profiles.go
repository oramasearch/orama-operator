/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"net/netip"
	"sort"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

func readKubernetesAPIServerTarget(ctx context.Context, reader client.Reader) (workspaceAPIServerTarget, error) {
	var kubernetes corev1.Service
	if err := reader.Get(ctx, client.ObjectKey{Namespace: "default", Name: "kubernetes"}, &kubernetes); err != nil {
		return workspaceAPIServerTarget{}, fmt.Errorf("read the Kubernetes API Service: %w", err)
	}
	var endpoints corev1.Endpoints
	if err := reader.Get(ctx, client.ObjectKey{Namespace: "default", Name: "kubernetes"}, &endpoints); err != nil {
		return workspaceAPIServerTarget{}, fmt.Errorf("read the Kubernetes API endpoints: %w", err)
	}
	return workspaceAPIServerTargetFrom(&kubernetes, &endpoints)
}

func publicInternetPeers(ctx context.Context, reader client.Reader) ([]networkingv1.NetworkPolicyPeer, error) {
	exceptions := append([]string(nil), workspacePublicIPExceptions...)
	var kubernetes corev1.Service
	if err := reader.Get(ctx, client.ObjectKey{Namespace: "default", Name: "kubernetes"}, &kubernetes); err != nil {
		return nil, fmt.Errorf("read the Kubernetes API Service before allowing internet egress: %w", err)
	}
	for _, ip := range append([]string{kubernetes.Spec.ClusterIP}, kubernetes.Spec.ClusterIPs...) {
		exceptions = appendAddressException(exceptions, ip)
	}
	var endpoints corev1.Endpoints
	if err := reader.Get(ctx, client.ObjectKey{Namespace: "default", Name: "kubernetes"}, &endpoints); err != nil {
		return nil, fmt.Errorf("read the Kubernetes API endpoints before allowing internet egress: %w", err)
	}
	for _, subset := range endpoints.Subsets {
		for _, address := range append(subset.Addresses, subset.NotReadyAddresses...) {
			exceptions = appendAddressException(exceptions, address.IP)
		}
	}
	var nodes corev1.NodeList
	if err := reader.List(ctx, &nodes); err != nil {
		return nil, fmt.Errorf("list node addresses before allowing internet egress: %w", err)
	}
	for i := range nodes.Items {
		for _, cidr := range append([]string{nodes.Items[i].Spec.PodCIDR}, nodes.Items[i].Spec.PodCIDRs...) {
			if prefix, err := netip.ParsePrefix(cidr); err == nil {
				exceptions = append(exceptions, prefix.Masked().String())
			}
		}
		for _, address := range nodes.Items[i].Status.Addresses {
			exceptions = appendAddressException(exceptions, address.Address)
		}
	}
	sort.Strings(exceptions)
	exceptions = sortedUnique(exceptions)
	var ipv4, ipv6 []string
	for _, value := range exceptions {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			return nil, fmt.Errorf("invalid public internet exclusion %q: %w", value, err)
		}
		if prefix.Addr().Is4() {
			ipv4 = append(ipv4, value)
		} else {
			ipv6 = append(ipv6, value)
		}
	}
	return []networkingv1.NetworkPolicyPeer{
		{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: ipv4}},
		{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: ipv6}},
	}, nil
}

func sandboxAllowance(comment string, policy *networkingv1.NetworkPolicy) oramav1alpha1.NetworkSandboxPolicy {
	return oramav1alpha1.NetworkSandboxPolicy{
		Name:    policy.Name,
		Comment: comment,
		Spec:    *policy.Spec.DeepCopy(),
	}
}

func existingSandboxAllowances(
	ctx context.Context,
	kube client.Client,
	namespace string,
	names ...string,
) ([]oramav1alpha1.NetworkSandboxPolicy, error) {
	var sandbox oramav1alpha1.NetworkSandbox
	if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: networkSandboxName}, &sandbox); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, nil
		}
		return nil, fmt.Errorf("read existing NetworkSandbox %s/%s: %w", namespace, networkSandboxName, err)
	}
	byName := make(map[string]oramav1alpha1.NetworkSandboxPolicy, len(sandbox.Spec.Policies))
	for _, policy := range sandbox.Spec.Policies {
		byName[policy.Name] = policy
	}
	allowances := make([]oramav1alpha1.NetworkSandboxPolicy, 0, len(names))
	for _, name := range names {
		if policy, ok := byName[name]; ok {
			allowances = append(allowances, policy)
		}
	}
	return allowances, nil
}

func reconcileOwnedNetworkSandbox(
	ctx context.Context,
	kube client.Client,
	scheme *runtime.Scheme,
	owner client.Object,
	namespace string,
	labels map[string]string,
	podSelector metav1.LabelSelector,
	policies []oramav1alpha1.NetworkSandboxPolicy,
) (*oramav1alpha1.NetworkSandbox, error) {
	sandbox := &oramav1alpha1.NetworkSandbox{
		ObjectMeta: metav1.ObjectMeta{Name: networkSandboxName, Namespace: namespace},
	}
	_, err := controllerutil.CreateOrUpdate(ctx, kube, sandbox, func() error {
		if current := metav1.GetControllerOf(sandbox); current != nil && !metav1.IsControlledBy(sandbox, owner) {
			return fmt.Errorf(
				"NetworkSandbox %s/%s is controlled by %s %s",
				namespace,
				networkSandboxName,
				current.Kind,
				current.Name,
			)
		}
		sandbox.Spec = oramav1alpha1.NetworkSandboxSpec{
			PodSelector:  *podSelector.DeepCopy(),
			PolicyLabels: copyStringMap(labels),
			Policies:     append([]oramav1alpha1.NetworkSandboxPolicy(nil), policies...),
		}
		return controllerutil.SetControllerReference(owner, sandbox, scheme)
	})
	if err != nil {
		return nil, fmt.Errorf("reconcile NetworkSandbox %s/%s: %w", namespace, networkSandboxName, err)
	}
	if _, _, err := reconcileNetworkSandboxPolicies(ctx, kube, scheme, sandbox); err != nil {
		return nil, err
	}
	return sandbox, nil
}

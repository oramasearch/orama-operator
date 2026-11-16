// SPDX-License-Identifier: MIT

package controller

import corev1 "k8s.io/api/core/v1"

// lakePodNdots is the resolver's ndots on lake's, agents' and apps' pods.
//
// The kubelet writes ndots:5, so a name with fewer than five dots is first tried under every
// search domain, A and AAAA each, before it is tried as given. A Service's full name
// (<svc>.<ns>.svc.cluster.local) has four dots, so every lookup of it walked the search list,
// including the node's cloud host domain, which CoreDNS forwards off the cluster: about 18 ms per
// lookup on dev against 2–7 ms for the absolute name (operator#276). With ndots:2 a name with two
// or more dots is tried as given first, so a Service's full name resolves on the first query and an
// external name such as api.openai.com no longer walks the search list. Short names (<svc> and
// <svc>.<ns>) keep using the search list as before.
//
// The operator therefore writes every in-cluster URL these pods dial as the Service's full name,
// never the <svc>.<ns>.svc form: that has two dots, would be tried as given first and forwarded off
// the cluster before the search list found it.
const lakePodNdots = "2"

// lakePodDNSConfig is the pod DNSConfig that sets lakePodNdots. It keeps the ClusterFirst
// nameservers and search list; the kubelet merges the option over its own ndots:5.
func lakePodDNSConfig() *corev1.PodDNSConfig {
	ndots := lakePodNdots
	return &corev1.PodDNSConfig{Options: []corev1.PodDNSConfigOption{{Name: "ndots", Value: &ndots}}}
}

// serviceHost is a Service's full in-cluster DNS name.
func serviceHost(name, namespace string) string {
	return name + "." + namespace + ".svc.cluster.local"
}

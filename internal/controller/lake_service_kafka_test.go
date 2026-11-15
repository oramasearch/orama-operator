// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// workspaceKafkaService is a Kafka service of the test Lake in the test workspace namespace.
func workspaceKafkaService() catalogService {
	return catalogService{
		lake:      testWorkspaceLake(),
		namespace: testWorkspaceName,
		name:      "prod-svc-666666666666",
		spec:      lakeServiceSpec{Instances: 1, StorageGi: 10},
	}
}

// builtKafkaObjects renders what build writes, as the objects the API server would hold.
func builtKafkaObjects(t *testing.T, in catalogService) map[string]*unstructured.Unstructured {
	t.Helper()
	out := map[string]*unstructured.Unstructured{}
	for _, resource := range (kafkaServiceKind{}).build(in, "") {
		obj := newServiceObject(resource.gvk, in.namespace, resource.name)
		for _, field := range resource.fields {
			if err := unstructured.SetNestedField(obj.Object, runtime.DeepCopyJSONValue(field.value), field.path...); err != nil {
				t.Fatalf("%s %v: %v", resource.gvk.Kind, field.path, err)
			}
		}
		out[resource.gvk.Kind] = obj
	}
	return out
}

// strimziListenerPolicy is the NetworkPolicy Strimzi 1.2.0 writes for a Kafka's listeners,
// `<cluster>-network-policy-kafka` (KafkaCluster.generateNetworkPolicy): one ingress rule per
// listener port, from the listener's networkPolicyPeers, or from every source when it has none.
// Strimzi's rules for its own ports (9090, 9091, 8443) admit only Strimzi pods and are left out.
func strimziListenerPolicy(t *testing.T, kafka *unstructured.Unstructured) networkingv1.NetworkPolicy {
	t.Helper()
	listeners, _, err := unstructured.NestedSlice(kafka.Object, "spec", "kafka", "listeners")
	if err != nil {
		t.Fatal(err)
	}
	policy := networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: kafka.GetName() + "-network-policy-kafka", Namespace: kafka.GetNamespace()},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				strimziClusterLabel: kafka.GetName(), strimziKindLabel: "Kafka", "strimzi.io/name": kafka.GetName() + "-kafka",
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		},
	}
	for _, raw := range listeners {
		listener := raw.(map[string]interface{})
		rule := networkingv1.NetworkPolicyIngressRule{Ports: []networkingv1.NetworkPolicyPort{{
			Protocol: protocolPtr(corev1.ProtocolTCP),
			Port:     intstrPtr(intstr.FromInt32(int32(listener["port"].(int64)))),
		}}}
		if peers, found := listener["networkPolicyPeers"]; found {
			encoded, err := json.Marshal(peers)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &rule.From); err != nil {
				t.Fatalf("networkPolicyPeers %s: %v", encoded, err)
			}
		}
		policy.Spec.Ingress = append(policy.Spec.Ingress, rule)
	}
	return policy
}

// The listener is TLS with SCRAM-SHA-512, the cluster authorizes, and Strimzi's own policy admits
// the listener only from the owning Lake's pods (operator#770).
func TestServicesKafkaListenerIsTLSAuthorizedAndPeerScoped(t *testing.T) {
	in := workspaceKafkaService()
	objects := builtKafkaObjects(t, in)
	kafka := objects["Kafka"]

	listeners, _, _ := unstructured.NestedSlice(kafka.Object, "spec", "kafka", "listeners")
	if len(listeners) != 1 {
		t.Fatalf("listeners = %v", listeners)
	}
	listener := listeners[0].(map[string]interface{})
	if listener["tls"] != true || listener["type"] != "internal" || listener["port"] != int64(kafkaListenerPort) {
		t.Fatalf("listener is not an internal TLS listener on %d: %v", kafkaListenerPort, listener)
	}
	if auth, _, _ := unstructured.NestedString(listener, "authentication", "type"); auth != "scram-sha-512" {
		t.Fatalf("listener authentication = %q", auth)
	}
	peers, _ := listener["networkPolicyPeers"].([]interface{})
	if len(peers) != 1 {
		t.Fatalf("networkPolicyPeers = %v, want only the owning Lake", listener["networkPolicyPeers"])
	}
	want := map[string]interface{}{
		"namespaceSelector": map[string]interface{}{"matchLabels": map[string]interface{}{"kubernetes.io/metadata.name": "lake"}},
		"podSelector": map[string]interface{}{"matchLabels": map[string]interface{}{
			"app.kubernetes.io/name": "lake", "app.kubernetes.io/instance": "prod",
		}},
	}
	if !reflect.DeepEqual(peers[0], want) {
		t.Fatalf("networkPolicyPeers[0] = %v, want %v", peers[0], want)
	}

	if authorization, _, _ := unstructured.NestedString(kafka.Object, "spec", "kafka", "authorization", "type"); authorization != "simple" {
		t.Fatalf("spec.kafka.authorization.type = %q, want simple", authorization)
	}

	user := objects["KafkaUser"]
	if kind, _, _ := unstructured.NestedString(user.Object, "spec", "authorization", "type"); kind != "simple" {
		t.Fatalf("KafkaUser authorization = %v", user.Object["spec"])
	}
	acls, _, _ := unstructured.NestedSlice(user.Object, "spec", "authorization", "acls")
	if len(acls) == 0 {
		t.Fatal("the KafkaUser has no ACLs")
	}
	// Never a cluster-wide administrator: no ACL management, broker configuration or inter-broker
	// operations, and nothing on Kafka's own internal topics.
	var deniesInternal bool
	for _, raw := range acls {
		acl := raw.(map[string]interface{})
		resource := acl["resource"].(map[string]interface{})
		operations := acl["operations"].([]interface{})
		if acl["type"] == "deny" {
			if resource["type"] == "topic" && resource["name"] == "__" && resource["patternType"] == "prefix" &&
				reflect.DeepEqual(operations, []interface{}{"All"}) {
				deniesInternal = true
			}
			continue
		}
		for _, op := range operations {
			if op == "All" {
				t.Errorf("ACL %v allows All", acl)
			}
			if resource["type"] == "cluster" && op != "IdempotentWrite" {
				t.Errorf("ACL %v allows %v on the cluster", acl, op)
			}
		}
	}
	if !deniesInternal {
		t.Errorf("no ACL denies Kafka's internal topics: %v", acls)
	}
}

// No NetworkPolicy in a workspace namespace, ours or the one Strimzi writes from the listener,
// admits the Kafka listener from outside the namespace, except the owning Lake's pods
// (operator#770).
func TestWorkspaceKafkaListenerAdmitsOnlyItsWorkspaceAndLake(t *testing.T) {
	ctx := context.Background()
	lake := testWorkspaceLake()
	reconciler, kube := newWorkspaceHarness(t, lake)
	store := &fakeWorkspacesStore{rows: []LakeWorkspaceRow{testWorkspaceRow("running")}}
	reconciler.syncLakeWorkspaces(ctx, lake, store, store.rows)

	var policies networkingv1.NetworkPolicyList
	if err := kube.List(ctx, &policies, client.InNamespace(testWorkspaceName)); err != nil {
		t.Fatal(err)
	}
	in := workspaceKafkaService()
	all := append(policies.Items, strimziListenerPolicy(t, builtKafkaObjects(t, in)["Kafka"]))

	broker := map[string]string{
		strimziClusterLabel: in.name, strimziKindLabel: "Kafka", "strimzi.io/name": in.name + "-kafka",
		"strimzi.io/component-type": "kafka", "strimzi.io/pool-name": "pool",
	}
	lakePods := lakePodSelector(lake).MatchLabels
	otherPod := map[string]string{"app.kubernetes.io/name": "ingress-nginx"}
	admitting := 0
	for _, policy := range all {
		if !selects(t, policy.Spec.PodSelector, broker) {
			continue
		}
		for _, rule := range policy.Spec.Ingress {
			if !rulePortAdmits(rule.Ports, kafkaListenerPort) {
				continue
			}
			admitting++
			if len(rule.From) == 0 {
				t.Errorf("%s admits %d from every source", policy.Name, kafkaListenerPort)
				continue
			}
			for _, peer := range rule.From {
				switch {
				case peer.IPBlock != nil:
					t.Errorf("%s admits %d from an address block %v", policy.Name, kafkaListenerPort, peer.IPBlock)
				case peer.NamespaceSelector == nil && peer.PodSelector != nil:
					// Same-namespace pods: the workspace's own, narrowed by its Connection paths.
				case peer.NamespaceSelector != nil &&
					reflect.DeepEqual(peer.NamespaceSelector, namespaceNameSelector(lakeNamespace(lake))) &&
					peer.PodSelector != nil && selects(t, *peer.PodSelector, lakePods) && !selects(t, *peer.PodSelector, otherPod):
					// The owning Lake.
				default:
					t.Errorf("%s admits %d from outside the workspace: %+v", policy.Name, kafkaListenerPort, peer)
				}
			}
		}
	}
	if admitting == 0 {
		t.Fatal("no policy admits the Kafka listener at all; the owning Lake must reach it")
	}
}

func rulePortAdmits(ports []networkingv1.NetworkPolicyPort, port int32) bool {
	if len(ports) == 0 {
		return true
	}
	for _, p := range ports {
		if p.Port == nil {
			return true
		}
		if p.Port.Type == intstr.Int && p.Port.IntVal <= port && (p.EndPort == nil && p.Port.IntVal == port || p.EndPort != nil && *p.EndPort >= port) {
			return true
		}
		if p.Port.Type == intstr.String && strings.HasPrefix(p.Port.StrVal, "tcp-") {
			return true
		}
	}
	return false
}

// The credential is SASL_SSL with the Kafka's cluster CA, read only from the CA Secret the Kafka
// owns (operator#770).
func TestServicesKafkaCredentialCarriesTheClusterCA(t *testing.T) {
	ctx := context.Background()
	in := workspaceKafkaService()
	kafka := newServiceObject(strimziKafkaGVK, in.namespace, in.name)
	kafka.SetUID(types.UID("kafka-uid"))
	user := newServiceObject(strimziKafkaUserGVK, in.namespace, in.name+kafkaUserSuffix)
	user.SetUID(types.UID("user-uid"))
	found := map[string]*unstructured.Unstructured{strimziKafkaGVK.Kind: kafka, strimziKafkaUserGVK.Kind: user}
	password := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: user.GetName(), Namespace: in.namespace,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "kafka.strimzi.io/v1", Kind: "KafkaUser", Name: user.GetName(), UID: user.GetUID()}}},
		Data: map[string][]byte{"password": []byte("scram-secret")},
	}
	const pem = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	ca := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: in.name + "-cluster-ca-cert", Namespace: in.namespace,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "kafka.strimzi.io/v1", Kind: "Kafka", Name: kafka.GetName(), UID: kafka.GetUID()}}},
		Data: map[string][]byte{"ca.crt": []byte(pem)},
	}
	scheme := newLakeScheme(t)

	// Without the CA Secret the credential waits.
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(password).Build()
	credential, refusal, err := kafkaServiceKind{}.credential(ctx, c, in, found, "")
	if err != nil || refusal != "" || credential != nil {
		t.Fatalf("without the CA: %s %q %v", credential, refusal, err)
	}

	// A CA Secret the Kafka does not own fails the service.
	foreign := ca.DeepCopy()
	foreign.OwnerReferences = nil
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(password, foreign).Build()
	if _, refusal, err = (kafkaServiceKind{}).credential(ctx, c, in, found, ""); err != nil ||
		refusal != "Secret prod-ws-123e4567e89b/prod-svc-666666666666-cluster-ca-cert exists and is not this Lake's service" {
		t.Fatalf("a foreign CA Secret: %q %v", refusal, err)
	}

	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(password, ca).Build()
	credential, refusal, err = kafkaServiceKind{}.credential(ctx, c, in, found, "")
	if err != nil || refusal != "" {
		t.Fatalf("credential: %q %v", refusal, err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(credential, &got); err != nil {
		t.Fatal(err)
	}
	if got["security_protocol"] != "SASL_SSL" || got["sasl_mechanism"] != "SCRAM-SHA-512" || got["ca_certificate"] != pem ||
		got["bootstrap"] != "prod-svc-666666666666-kafka-bootstrap.prod-ws-123e4567e89b.svc.cluster.local:9092" ||
		got["username"] != user.GetName() || got["password"] != "scram-secret" {
		t.Fatalf("credential = %v", got)
	}
}

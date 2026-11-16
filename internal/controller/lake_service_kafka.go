// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Kafka services run on Strimzi 1.2.0, whose only API is kafka.strimzi.io/v1: KRaft is the only
// mode and brokers live in KafkaNodePools. Field names follow the 1.2.0 chart's CRDs.
var (
	strimziKafkaGVK     = schema.GroupVersionKind{Group: "kafka.strimzi.io", Version: "v1", Kind: "Kafka"}
	strimziNodePoolGVK  = schema.GroupVersionKind{Group: "kafka.strimzi.io", Version: "v1", Kind: "KafkaNodePool"}
	strimziKafkaUserGVK = schema.GroupVersionKind{Group: "kafka.strimzi.io", Version: "v1", Kind: "KafkaUser"}
)

const (
	// strimziClusterLabel ties a KafkaNodePool, a KafkaUser and every PVC to their Kafka.
	strimziClusterLabel = "strimzi.io/cluster"
	strimziKindLabel    = "strimzi.io/kind"
	kafkaListenerName   = "scram"
	kafkaListenerPort   = 9092
	// kafkaNodePoolName is the pool's name suffix; its pods are <kafka>-pool-<id>.
	kafkaNodePoolSuffix = "-pool"
	// kafkaUserSuffix names the KafkaUser, which is also its SCRAM username. A bare "app" would
	// collide between two Kafka services of one namespace.
	kafkaUserSuffix    = "-app"
	kafkaSASLMechanism = "SCRAM-SHA-512"
	// The client listener is TLS with SCRAM-SHA-512 (operator#770), so clients speak SASL_SSL and
	// trust the Kafka's cluster CA, which the credential carries.
	kafkaSecurityProtocol = "SASL_SSL"
	// kafkaClusterCASuffix names the Secret in which Strimzi keeps the cluster CA certificate
	// (key ca.crt). Strimzi sets the Kafka as its owner (clusterCa.generateSecretOwnerReference
	// defaults to true), which is how the credential trusts it.
	kafkaClusterCASuffix = "-cluster-ca-cert"
	kafkaClusterCAKey    = "ca.crt"
)

type kafkaServiceKind struct{}

func (kafkaServiceKind) title() string { return "Kafka" }

func (kafkaServiceKind) objects(in catalogService) []catalogObject {
	return []catalogObject{
		{gvk: strimziKafkaGVK, name: in.name},
		{gvk: strimziNodePoolGVK, name: in.name + kafkaNodePoolSuffix},
		{gvk: strimziKafkaUserGVK, name: in.name + kafkaUserSuffix},
	}
}

// kafkaMinInSync keeps writes available with one broker down: 1 for one or two brokers, 2 for
// three.
func kafkaMinInSync(instances int32) int64 {
	if instances <= 2 {
		return 1
	}
	return int64(instances) - 1
}

func (k kafkaServiceKind) build(in catalogService, _ string) []catalogResource {
	objects := k.objects(in)
	brokers := int64(in.spec.Instances)
	minInSync := kafkaMinInSync(in.spec.Instances)
	cluster := map[string]string{strimziClusterLabel: in.name}
	return []catalogResource{
		{catalogObject: objects[0], fields: []serviceField{
			// The client listener (operator#770): TLS with Strimzi's cluster CA, SCRAM-SHA-512, and
			// networkPolicyPeers. Strimzi writes its own <name>-network-policy-kafka from these peers;
			// with none it admits the listener from every source, which undoes the workspace's
			// default-deny. The only peer is the owning Lake: the workspace's own pods reach the
			// listener through the workspace's NetworkPolicies (same-namespace traffic until its
			// Connections are enforced, then each Connection's path), like every other kind.
			{path: []string{"spec", "kafka", "listeners"}, value: []interface{}{map[string]interface{}{
				"name": kafkaListenerName, "port": int64(kafkaListenerPort), "type": "internal", "tls": true,
				"authentication":     map[string]interface{}{"type": "scram-sha-512"},
				"networkPolicyPeers": kafkaListenerPeers(in),
			}}},
			// Kafka's internal topics default to three replicas; with fewer brokers consumer
			// groups and transactions never become available.
			{path: []string{"spec", "kafka", "config"}, value: map[string]interface{}{
				"offsets.topic.replication.factor":         brokers,
				"transaction.state.log.replication.factor": brokers,
				"transaction.state.log.min.isr":            minInSync,
				"default.replication.factor":               brokers,
				"min.insync.replicas":                      minInSync,
			}},
			// Kafka's own authorizer: a user may do only what its ACLs allow. Strimzi makes its own
			// components (brokers, the entity operator) super users.
			{path: []string{"spec", "kafka", "authorization"}, value: map[string]interface{}{"type": "simple"}},
			// The entity operator's pod meets the `restricted` Pod Security Standard by what is
			// written here, not by how Strimzi happens to be installed on the cluster.
			{path: []string{"spec", "entityOperator"}, value: map[string]interface{}{
				"topicOperator": map[string]interface{}{"resources": kafkaOperatorResources()},
				"userOperator":  map[string]interface{}{"resources": kafkaOperatorResources()},
				"template": map[string]interface{}{
					"pod":                    map[string]interface{}{"securityContext": restrictedPodSecurityContext()},
					"topicOperatorContainer": map[string]interface{}{"securityContext": restrictedContainerSecurityContext()},
					"userOperatorContainer":  map[string]interface{}{"securityContext": restrictedContainerSecurityContext()},
				},
			}},
		}},
		{catalogObject: objects[1], labels: cluster, fields: []serviceField{
			{path: []string{"spec", "replicas"}, value: brokers},
			{path: []string{"spec", "roles"}, value: []interface{}{"controller", "broker"}},
			{path: []string{"spec", "resources"}, value: kafkaBrokerResources()},
			// A workspace namespace enforces the `restricted` Pod Security Standard, and the API
			// server refuses a broker pod that does not meet it (operator#428). The security
			// context is part of the resource this operator writes, so a Kafka service starts
			// on any cluster, whatever Strimzi's own defaults are there.
			{path: []string{"spec", "template"}, value: map[string]interface{}{
				"pod":            map[string]interface{}{"securityContext": restrictedPodSecurityContext()},
				"kafkaContainer": map[string]interface{}{"securityContext": restrictedContainerSecurityContext()},
			}},
			// Storage cannot be updated (the CRD says so); deleteClaim stays false and deletion
			// removes the claims itself.
			{path: []string{"spec", "storage"}, createOnly: true, value: map[string]interface{}{
				"type": "persistent-claim", "size": serviceStorage(in.spec), "deleteClaim": false,
			}},
		}},
		{catalogObject: objects[2], labels: cluster, fields: []serviceField{
			{path: []string{"spec", "authentication"}, value: map[string]interface{}{"type": "scram-sha-512"}},
			{path: []string{"spec", "authorization"}, value: map[string]interface{}{"type": "simple", "acls": kafkaUserACLs()}},
		}},
	}
}

// kafkaListenerPeers is the one peer the listener names: the owning Lake's pods in its namespace.
func kafkaListenerPeers(in catalogService) []interface{} {
	return []interface{}{map[string]interface{}{
		"namespaceSelector": map[string]interface{}{"matchLabels": map[string]interface{}{
			"kubernetes.io/metadata.name": lakeNamespace(in.lake),
		}},
		"podSelector": map[string]interface{}{"matchLabels": stringMapToInterface(lakePodSelector(in.lake).MatchLabels)},
	}}
}

func stringMapToInterface(in map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// kafkaUserACLs scope the workspace's credential to its own topics (operator#770, operator#771).
// The Kafka is this one service's, so its topics are the workspace's: every topic but Kafka's
// internal ones (prefix "__", denied), its consumer groups and transactional ids. The user may
// produce, consume, create, delete and configure its topics, and write idempotently. It holds
// nothing else on the cluster: no ACL management (Alter), no broker configuration
// (AlterConfigs), no inter-broker operations (ClusterAction), so it cannot raise its own rights.
func kafkaUserACLs() []interface{} {
	acl := func(kind, resourceType, name, pattern string, operations ...string) map[string]interface{} {
		ops := make([]interface{}, 0, len(operations))
		for _, op := range operations {
			ops = append(ops, op)
		}
		resource := map[string]interface{}{"type": resourceType}
		// The cluster resource has no name: Kafka has one, and Strimzi names it itself.
		if name != "" {
			resource["name"] = name
			resource["patternType"] = pattern
		}
		return map[string]interface{}{"type": kind, "resource": resource, "operations": ops}
	}
	return []interface{}{
		acl("allow", "topic", "*", "literal",
			"Read", "Write", "Create", "Delete", "Describe", "DescribeConfigs", "AlterConfigs", "Alter"),
		acl("deny", "topic", "__", "prefix", "All"),
		acl("allow", "group", "*", "literal", "Read", "Describe", "Delete"),
		acl("allow", "transactionalId", "*", "literal", "Write", "Describe"),
		acl("allow", "cluster", "", "", "IdempotentWrite"),
	}
}

func (kafkaServiceKind) passwordSecret(string) (string, string) { return "", "" }

func kafkaBootstrap(in catalogService) string {
	return fmt.Sprintf("%s-kafka-bootstrap.%s.svc.cluster.local:%d", in.name, in.namespace, kafkaListenerPort)
}

func (kafkaServiceKind) endpoint(in catalogService) string { return kafkaBootstrap(in) }

// ready: the Kafka's Ready condition. The KafkaUser Secret is the credential's half of the rule.
func (kafkaServiceKind) ready(_ catalogService, found map[string]*unstructured.Unstructured) (bool, string) {
	kafka := found[strimziKafkaGVK.Kind]
	if kafka == nil || !conditionTrue(kafka.Object, "Ready") {
		return false, "waiting for the Kafka cluster"
	}
	return true, ""
}

// A Kafka service is three containers: the broker and the entity operator's topic and user
// operators. Left without resources each takes the workspace namespace's LimitRange default, one
// CPU of limit apiece, so a one-broker Kafka cost three CPUs of a workspace's eight and the entity
// operator's pod was refused by the quota (operator#428). They are stated here instead, so that a
// one-broker Kafka costs one CPU of limits in all, what every other single-instance service costs:
// 500m for the broker and 250m for each operator.
func kafkaBrokerResources() map[string]interface{} {
	return map[string]interface{}{
		"requests": map[string]interface{}{"cpu": "100m", "memory": "512Mi"},
		"limits":   map[string]interface{}{"cpu": "500m", "memory": "1Gi"},
	}
}

func kafkaOperatorResources() map[string]interface{} {
	return map[string]interface{}{
		"requests": map[string]interface{}{"cpu": "50m", "memory": "128Mi"},
		"limits":   map[string]interface{}{"cpu": "250m", "memory": "256Mi"},
	}
}

// restrictedPodSecurityContext and restrictedContainerSecurityContext are what the `restricted`
// Pod Security Standard requires, as the unstructured values an upstream resource's pod template
// takes.
func restrictedPodSecurityContext() map[string]interface{} {
	return map[string]interface{}{
		"runAsNonRoot":   true,
		"seccompProfile": map[string]interface{}{"type": "RuntimeDefault"},
	}
}

func restrictedContainerSecurityContext() map[string]interface{} {
	return map[string]interface{}{
		"allowPrivilegeEscalation": false,
		"runAsNonRoot":             true,
		"capabilities":             map[string]interface{}{"drop": []interface{}{"ALL"}},
		"seccompProfile":           map[string]interface{}{"type": "RuntimeDefault"},
	}
}

// strimziPodSetListGVK is the resource through which Strimzi creates a Kafka's pods. Its status is
// where Strimzi records a pod the API server refused.
var strimziPodSetListGVK = schema.GroupVersionKind{Group: "core.strimzi.io", Version: "v1", Kind: "StrimziPodSetList"}

// waitingReason says what Strimzi is stuck on: first a pod set's own failure (the API server
// refusing the broker pod, with its reason), else the Kafka's NotReady message. On 2026-10-13 a
// Kafka in a workspace namespace waited forever on a pod that violated the namespace's Pod
// Security level, and the service said only "waiting for the Kafka cluster" (operator#428).
func (kafkaServiceKind) waitingReason(ctx context.Context, c client.Reader, in catalogService, found map[string]*unstructured.Unstructured) (string, error) {
	sets := &unstructured.UnstructuredList{}
	sets.SetGroupVersionKind(strimziPodSetListGVK)
	err := c.List(ctx, sets, client.InNamespace(in.namespace), client.MatchingLabels{strimziClusterLabel: in.name})
	if err != nil && !apimeta.IsNoMatchError(err) && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("list the Kafka's pod sets: %w", err)
	}
	for _, set := range sets.Items {
		conditions, _, _ := unstructured.NestedSlice(set.Object, "status", "conditions")
		for _, raw := range conditions {
			condition, ok := raw.(map[string]interface{})
			if !ok || condition["status"] != string(metav1.ConditionTrue) {
				continue
			}
			if message, _ := condition["message"].(string); message != "" {
				return boundedReason(message), nil
			}
		}
	}
	// The entity operator is a Deployment. A pod of it the API server refuses (the workspace's
	// quota, for one) is recorded on the Deployment's ReplicaFailure condition, while the Kafka
	// says only that it timed out waiting.
	var deployments appsv1.DeploymentList
	if err := c.List(ctx, &deployments, client.InNamespace(in.namespace), client.MatchingLabels{strimziClusterLabel: in.name}); err != nil {
		return "", fmt.Errorf("list the Kafka's Deployments: %w", err)
	}
	for _, deployment := range deployments.Items {
		for _, condition := range deployment.Status.Conditions {
			if condition.Type == appsv1.DeploymentReplicaFailure && condition.Status == corev1.ConditionTrue && condition.Message != "" {
				return boundedReason(condition.Message), nil
			}
		}
	}
	if kafka := found[strimziKafkaGVK.Kind]; kafka != nil {
		return boundedReason(conditionMessage(kafka.Object, "NotReady")), nil
	}
	return "", nil
}

// credential reads the Secret the User Operator writes for the KafkaUser: named by the user's
// status.secret (its own name until then), key password. The Secret must be the KafkaUser's own.
// It also carries the cluster CA certificate the listener's TLS chains to, from
// <name>-cluster-ca-cert, which must be the Kafka's own.
func (kafkaServiceKind) credential(ctx context.Context, c client.Reader, in catalogService, found map[string]*unstructured.Unstructured, _ string) ([]byte, string, error) {
	user := found[strimziKafkaUserGVK.Kind]
	if user == nil {
		return nil, "", nil
	}
	secretName, _, _ := unstructured.NestedString(user.Object, "status", "secret")
	if secretName == "" {
		secretName = user.GetName()
	}
	username, _, _ := unstructured.NestedString(user.Object, "status", "username")
	if username == "" {
		username = user.GetName()
	}
	secret, err := readServiceSecret(ctx, c, in.namespace, secretName)
	if err != nil || secret == nil {
		return nil, "", err
	}
	if !hasOwnerUID(secret, user) {
		return nil, foreignServiceMessage("Secret", in.namespace, secretName), nil
	}
	password := string(secret.Data["password"])
	if password == "" {
		return nil, "", nil
	}
	ca, refusal, err := kafkaClusterCA(ctx, c, in, found[strimziKafkaGVK.Kind])
	if err != nil || refusal != "" || ca == "" {
		return nil, refusal, err
	}
	credential, err := json.Marshal(struct {
		Kind             string `json:"kind"`
		Endpoint         string `json:"endpoint"`
		Bootstrap        string `json:"bootstrap"`
		Username         string `json:"username"`
		Password         string `json:"password"`
		SASLMechanism    string `json:"sasl_mechanism"`
		SecurityProtocol string `json:"security_protocol"`
		CACertificate    string `json:"ca_certificate"`
	}{serviceKindKafka, kafkaBootstrap(in), kafkaBootstrap(in), username, password, kafkaSASLMechanism, kafkaSecurityProtocol, ca})
	return credential, "", err
}

// kafkaClusterCA reads the PEM cluster CA certificate of the Kafka, "" while it is not there yet.
// During a CA renewal Strimzi keeps ca.crt the current certificate and the broker certificates are
// re-signed in the same rolling update, so the credential follows on the next poll.
func kafkaClusterCA(ctx context.Context, c client.Reader, in catalogService, kafka *unstructured.Unstructured) (string, string, error) {
	if kafka == nil {
		return "", "", nil
	}
	name := in.name + kafkaClusterCASuffix
	secret, err := readServiceSecret(ctx, c, in.namespace, name)
	if err != nil || secret == nil {
		return "", "", err
	}
	if !hasOwnerUID(secret, kafka) {
		return "", foreignServiceMessage("Secret", in.namespace, name), nil
	}
	return string(secret.Data[kafkaClusterCAKey]), "", nil
}

// podSelector: the Kafka's broker and controller pods.
func (kafkaServiceKind) podSelector(in catalogService) map[string]string {
	return map[string]string{strimziClusterLabel: in.name, strimziKindLabel: "Kafka"}
}

// claimSelectors: Strimzi labels every broker and controller claim with the Kafka's name.
func (kafkaServiceKind) claimSelectors(in catalogService) []client.MatchingLabels {
	return []client.MatchingLabels{{strimziClusterLabel: in.name, strimziKindLabel: "Kafka"}}
}

// nameOverhead: "-kafka-bootstrap", the longest Service name Strimzi derives.
func (kafkaServiceKind) nameOverhead() int { return len("-kafka-bootstrap") }

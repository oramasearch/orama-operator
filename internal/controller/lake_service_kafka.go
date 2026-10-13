/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"

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
	kafkaUserSuffix       = "-app"
	kafkaSASLMechanism    = "SCRAM-SHA-512"
	kafkaSecurityProtocol = "SASL_PLAINTEXT"
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
			{path: []string{"spec", "kafka", "listeners"}, value: []interface{}{map[string]interface{}{
				"name": kafkaListenerName, "port": int64(kafkaListenerPort), "type": "internal", "tls": false,
				"authentication": map[string]interface{}{"type": "scram-sha-512"},
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
			// The entity operator's pod meets the `restricted` Pod Security Standard by what is
			// written here, not by how Strimzi happens to be installed on the cluster.
			{path: []string{"spec", "entityOperator"}, value: map[string]interface{}{
				"topicOperator": map[string]interface{}{}, "userOperator": map[string]interface{}{},
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
		}},
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
	if kafka := found[strimziKafkaGVK.Kind]; kafka != nil {
		return boundedReason(conditionMessage(kafka.Object, "NotReady")), nil
	}
	return "", nil
}

// credential reads the Secret the User Operator writes for the KafkaUser: named by the user's
// status.secret (its own name until then), key password. The Secret must be the KafkaUser's own.
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
	credential, err := json.Marshal(struct {
		Kind             string `json:"kind"`
		Endpoint         string `json:"endpoint"`
		Bootstrap        string `json:"bootstrap"`
		Username         string `json:"username"`
		Password         string `json:"password"`
		SASLMechanism    string `json:"sasl_mechanism"`
		SecurityProtocol string `json:"security_protocol"`
	}{serviceKindKafka, kafkaBootstrap(in), kafkaBootstrap(in), username, password, kafkaSASLMechanism, kafkaSecurityProtocol})
	return credential, "", err
}

// claimSelectors: Strimzi labels every broker and controller claim with the Kafka's name.
func (kafkaServiceKind) claimSelectors(in catalogService) []client.MatchingLabels {
	return []client.MatchingLabels{{strimziClusterLabel: in.name, strimziKindLabel: "Kafka"}}
}

// nameOverhead: "-kafka-bootstrap", the longest Service name Strimzi derives.
func (kafkaServiceKind) nameOverhead() int { return len("-kafka-bootstrap") }

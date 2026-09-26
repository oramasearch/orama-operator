/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"

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
			{path: []string{"spec", "entityOperator"}, value: map[string]interface{}{
				"topicOperator": map[string]interface{}{}, "userOperator": map[string]interface{}{},
			}},
		}},
		{catalogObject: objects[1], labels: cluster, fields: []serviceField{
			{path: []string{"spec", "replicas"}, value: brokers},
			{path: []string{"spec", "roles"}, value: []interface{}{"controller", "broker"}},
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

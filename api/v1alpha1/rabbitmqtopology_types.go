/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// RabbitMQTopologySpec is the desired topology of the RabbitMQ broker — its virtual hosts,
// per-service users, scoped permissions, and streams — reconciled against the broker's
// management HTTP API. This is the operator's OWN topology CRD (principle #3): the bitnami
// RabbitMQ Cluster/Messaging-Topology operators were dropped (their images 404'd), so the
// broker runs as a plain StatefulSet and the operator models its topology here (#41), the same
// "external resource as an owned CRD" shape as CloudflareDNSRecord. It supersedes #36's
// imperative broker settings and app-boot side effects: the topology is declared, owned, and
// reconciled instead of created implicitly by OramaCore on connect.
type RabbitMQTopologySpec struct {
	// BrokerService is the broker Service DNS name whose management API the operator reconciles
	// against (e.g. <cr>-broker). The management API is reached at
	// http://<BrokerService>.<namespace>.svc.cluster.local:<ManagementPort>.
	BrokerService string `json:"brokerService"`

	// ManagementPort is the RabbitMQ management HTTP API port.
	// +kubebuilder:default=15672
	// +optional
	ManagementPort int32 `json:"managementPort,omitempty"`

	// AdminSecretRef names the Secret holding the broker admin credentials for the management API
	// (keys `username` + `password`). Nil-safe: until the Secret exists the reconciler is a safe
	// no-op (it waits, like the Cloudflare reconcilers wait on their token Secret).
	AdminSecretRef string `json:"adminSecretRef"`

	// VHosts are the virtual hosts to ensure exist (idempotent PUT). "/" always exists in a fresh
	// RabbitMQ, but listing it is harmless and documents intent.
	// +optional
	VHosts []string `json:"vhosts,omitempty"`

	// Users are the per-service accounts to ensure (least-privilege, principle #4). Each user's
	// password is sourced from a referenced Secret so no credential ever lives in spec. Service
	// accounts should stay untagged (no management/administrator access).
	// +optional
	// +listType=map
	// +listMapKey=name
	Users []RabbitMQUser `json:"users,omitempty"`

	// Permissions scope each user's configure/write/read regexes on a vhost. Empty regex ("^$")
	// grants nothing; scope to the exact stream name (e.g. "^oramacore-operations$") for
	// least-privilege.
	// +optional
	Permissions []RabbitMQPermission `json:"permissions,omitempty"`

	// Streams are the stream queues (x-queue-type=stream) to declare — notably the
	// `oramacore-operations` writer→reader replication stream (#41). NOTE: RabbitMQ compares stream
	// properties on (re-)declaration, so a stream a native stream-protocol client (OramaCore) also
	// creates on connect must be declared with MATCHING arguments — retention set here that the
	// client does not also request will make the client's Create fail. Leave retention unset unless
	// you know the client's exact create arguments.
	// +optional
	// +listType=map
	// +listMapKey=name
	Streams []RabbitMQStream `json:"streams,omitempty"`
}

// RabbitMQUser is a broker account whose password is sourced from a Secret.
type RabbitMQUser struct {
	// Name is the RabbitMQ username (e.g. oramacore-writer).
	Name string `json:"name"`

	// PasswordSecretRef names the Secret + key holding this user's password.
	PasswordSecretRef RabbitMQSecretKeyRef `json:"passwordSecretRef"`

	// Tags is the RabbitMQ user tag list (comma-separated), e.g. "" (a plain service account) or
	// "management"/"administrator". Least-privilege service accounts stay untagged.
	// +optional
	Tags string `json:"tags,omitempty"`
}

// RabbitMQSecretKeyRef references a single key within a Secret in the CR's namespace.
type RabbitMQSecretKeyRef struct {
	// Name of the Secret.
	Name string `json:"name"`
	// Key within the Secret holding the value.
	Key string `json:"key"`
}

// RabbitMQPermission scopes one user's access on one vhost (the standard RabbitMQ
// configure/write/read regex model). A producer needs `write` on the stream; a consumer needs
// `read`; a client that declares the stream itself needs `configure`.
type RabbitMQPermission struct {
	// User is the RabbitMQ username this permission applies to.
	User string `json:"user"`

	// VHost the permission applies to.
	// +kubebuilder:default=/
	// +optional
	VHost string `json:"vhost,omitempty"`

	// Configure regex (resources the user may declare/delete). "^$" = none.
	// +kubebuilder:default="^$"
	// +optional
	Configure string `json:"configure,omitempty"`

	// Write regex (resources the user may publish to). "^$" = none.
	// +kubebuilder:default="^$"
	// +optional
	Write string `json:"write,omitempty"`

	// Read regex (resources the user may consume from). "^$" = none.
	// +kubebuilder:default="^$"
	// +optional
	Read string `json:"read,omitempty"`
}

// RabbitMQStream is a stream queue (x-queue-type=stream) declared via the management API.
type RabbitMQStream struct {
	// Name of the stream (e.g. oramacore-operations).
	Name string `json:"name"`

	// VHost the stream lives in.
	// +kubebuilder:default=/
	// +optional
	VHost string `json:"vhost,omitempty"`

	// MaxAge bounds retention by time using RabbitMQ's stream duration syntax (e.g. "7D", "12h").
	// Empty = unbounded. Set only at declaration; RabbitMQ cannot change it later via policy.
	// +optional
	MaxAge string `json:"maxAge,omitempty"`

	// MaxLengthBytes bounds retention by total stream size in bytes. 0 = unbounded.
	// +optional
	MaxLengthBytes int64 `json:"maxLengthBytes,omitempty"`

	// MaxSegmentSizeBytes sets the stream's segment size in bytes. 0 = broker default (500 MB).
	// +optional
	MaxSegmentSizeBytes int64 `json:"maxSegmentSizeBytes,omitempty"`
}

// RabbitMQTopologyStatus is the observed topology. Written to the status subresource ONLY on a
// real state transition (deep-compare first) — a steady topology produces zero control-plane
// traffic (anti-flap, principle #7).
type RabbitMQTopologyStatus struct {
	// ObservedGeneration is the spec generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// VHosts is the number of vhosts reconciled.
	// +optional
	VHosts int `json:"vhosts,omitempty"`

	// Users is the number of users reconciled.
	// +optional
	Users int `json:"users,omitempty"`

	// Permissions is the number of permission grants reconciled.
	// +optional
	Permissions int `json:"permissions,omitempty"`

	// Streams reflects each managed stream and whether it is present on the broker.
	// +optional
	// +listType=map
	// +listMapKey=name
	Streams []RabbitMQStreamStatus `json:"streams,omitempty"`

	// Message is a human-readable summary of the current state (or the last error).
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions carry the standard `Ready` condition. Reason distinguishes
	// WaitingForCredentials, BrokerUnavailable, ReconcileError, and Reconciled.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RabbitMQStreamStatus is the observed state of one managed stream.
type RabbitMQStreamStatus struct {
	// Name is the stream's spec name.
	Name string `json:"name"`
	// Present is true once the stream exists on the broker.
	// +optional
	Present bool `json:"present,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:shortName=rmqtopo
//+kubebuilder:printcolumn:name="Broker",type=string,JSONPath=`.spec.brokerService`
//+kubebuilder:printcolumn:name="VHosts",type=integer,JSONPath=`.status.vhosts`
//+kubebuilder:printcolumn:name="Users",type=integer,JSONPath=`.status.users`
//+kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`

// RabbitMQTopology is the broker's declared topology (vhosts / users / permissions / streams),
// owned by an OramaCloudV1 CR and reconciled against the RabbitMQ management HTTP API. A
// finalizer removes the declared users and streams from the broker on delete, so tearing down the
// owning CR cascades to real broker cleanup. Until the broker (and its admin Secret) is up the
// reconciler is a safe no-op that surfaces a clear `Ready=False` condition rather than erroring —
// ordered readiness gating (the topology settles once the broker is reachable).
type RabbitMQTopology struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RabbitMQTopologySpec   `json:"spec,omitempty"`
	Status RabbitMQTopologyStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// RabbitMQTopologyList contains a list of RabbitMQTopology.
type RabbitMQTopologyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RabbitMQTopology `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RabbitMQTopology{}, &RabbitMQTopologyList{})
}

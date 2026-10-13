/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// OramaOperatorSpec says which cluster gets the Orama operator, and at which version.
type OramaOperatorSpec struct {
	// ClusterRef names the KubernetesCluster the operator is installed onto. The cluster is
	// reached through the admin kubeconfig Secret that KubernetesCluster writes.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterRef cannot be changed; create another OramaOperator for another cluster"
	ClusterRef string `json:"clusterRef"`

	// Version is the operator version the cluster runs: the image tag, and the chart published
	// with it (a release such as v0.1.0, or a build such as dev-83242f9). Changing it upgrades
	// the cluster's operator, CRDs first.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`
	Version string `json:"version"`

	// Namespace is where the operator is installed on the cluster.
	// +kubebuilder:default=orama-system
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="namespace cannot be changed"
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// ImagePullSecretRef names a registry credentials Secret (kubernetes.io/dockerconfigjson) on
	// this cluster. It is copied to the target cluster so its nodes can pull the private operator
	// image, and it authenticates the pull of the chart.
	ImagePullSecretRef ClusterSecretReference `json:"imagePullSecretRef"`

	// ImageRepository is the operator image without a tag.
	// +kubebuilder:default="oramasearch/orama-operator"
	// +optional
	ImageRepository string `json:"imageRepository,omitempty"`

	// ChartRepository is the OCI repository holding the operator's chart, one tag per version.
	// +kubebuilder:default="registry-1.docker.io/oramasearch/orama-operator-chart"
	// +optional
	ChartRepository string `json:"chartRepository,omitempty"`

	// Values are chart values for the cluster's operator. The image and its pull secret are set
	// by this controller and win over anything here.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Values *apiextensionsv1.JSON `json:"values,omitempty"`
}

// OramaOperatorStatus is what the target cluster's operator is observed to be.
type OramaOperatorStatus struct {
	// Phase is Pending (waiting for the cluster), Installing, Ready, Degraded or Deleting.
	// +optional
	Phase string `json:"phase,omitempty"`
	// InstalledVersion is the operator version of the Helm release on the cluster.
	// +optional
	InstalledVersion string `json:"installedVersion,omitempty"`
	// ChartVersion is the chart version of that release.
	// +optional
	ChartVersion string `json:"chartVersion,omitempty"`
	// Revision is the Helm release's revision on the cluster.
	// +optional
	Revision int32 `json:"revision,omitempty"`
	// ReleaseStatus is Helm's own word for the release: deployed, failed, pending-upgrade.
	// +optional
	ReleaseStatus string `json:"releaseStatus,omitempty"`
	// Image is the image the operator's Deployment on the cluster runs.
	// +optional
	Image string `json:"image,omitempty"`
	// Replicas and ReadyReplicas are the operator Deployment's, on the cluster.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// Message says why the operator is not Ready, in the words of whatever refused: the
	// registry, Helm, the cluster's API server, or the operator's own pod.
	// +optional
	Message string `json:"message,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Conditions: Ready.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Cluster,shortName=ooperator
//+kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef`
//+kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
//+kubebuilder:printcolumn:name="Installed",type=string,JSONPath=`.status.installedVersion`
//+kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`

// OramaOperator installs the Orama operator onto a cluster this one manages and keeps it at the
// version named. Every cluster runs the same operator with the same kinds; which cluster manages
// which is decided by where these resources are created, not by how an operator is installed.
type OramaOperator struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   OramaOperatorSpec   `json:"spec,omitempty"`
	Status OramaOperatorStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// OramaOperatorList is a list of OramaOperator.
type OramaOperatorList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []OramaOperator `json:"items"`
}

func init() {
	SchemeBuilder.Register(&OramaOperator{}, &OramaOperatorList{})
}

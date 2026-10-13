/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterHelmChart says which chart, from where, at which version.
type ClusterHelmChart struct {
	// Repository is a chart repository (https://cloudnative-pg.github.io/charts), with Name the
	// chart in it, or an OCI repository holding the chart (oci://registry/path), with Name unset.
	// +kubebuilder:validation:MinLength=1
	Repository string `json:"repository"`
	// Name is the chart's name in a chart repository. Unused for an OCI repository.
	// +optional
	Name string `json:"name,omitempty"`
	// Version is the chart version. It is required: a release on a managed cluster is always
	// pinned, and changing this is how it is upgraded.
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`
	// CredentialsSecretRef names a Secret on this cluster for a private chart: either a
	// kubernetes.io/dockerconfigjson Secret with an entry for the repository's host, or one with
	// `username` and `password` keys.
	// +optional
	CredentialsSecretRef *ClusterSecretReference `json:"credentialsSecretRef,omitempty"`
}

// ClusterHelmReleaseSpec is one chart release on a cluster this one manages.
// +kubebuilder:validation:XValidation:rule="self.chart.repository.startsWith('https://') ? has(self.chart.name) && self.chart.name != ” : true",message="chart.name is required for a chart repository (https://)"
type ClusterHelmReleaseSpec struct {
	// ClusterRef names the KubernetesCluster the chart is installed onto.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="clusterRef cannot be changed; create another ClusterHelmRelease for another cluster"
	ClusterRef string `json:"clusterRef"`

	// ReleaseName is the Helm release's name on the cluster.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=53
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="releaseName cannot be changed"
	ReleaseName string `json:"releaseName"`

	// Namespace is the release's namespace on the cluster. It is created when absent.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="namespace cannot be changed"
	Namespace string `json:"namespace"`

	Chart ClusterHelmChart `json:"chart"`

	// Values are the chart's values.
	// +kubebuilder:pruning:PreserveUnknownFields
	// +optional
	Values *apiextensionsv1.JSON `json:"values,omitempty"`

	// DependsOn names other ClusterHelmRelease resources, on the same cluster, that must be
	// Ready before this one is installed (cert-manager before a chart whose webhooks need it).
	// +optional
	// +listType=set
	DependsOn []string `json:"dependsOn,omitempty"`

	// RequiredCRDs are CRD names that must exist on the cluster before this chart is installed
	// or upgraded, for a chart whose CRDs are shipped apart from it.
	// +optional
	// +listType=set
	RequiredCRDs []string `json:"requiredCRDs,omitempty"`
}

// ClusterHelmReleaseStatus is what the cluster has.
type ClusterHelmReleaseStatus struct {
	// Phase is Pending (waiting for the cluster or a dependency), Installing, Ready, Degraded or
	// Deleting.
	// +optional
	Phase string `json:"phase,omitempty"`
	// ChartVersion and AppVersion are those of the release on the cluster.
	// +optional
	ChartVersion string `json:"chartVersion,omitempty"`
	// +optional
	AppVersion string `json:"appVersion,omitempty"`
	// Revision is the Helm release's revision.
	// +optional
	Revision int32 `json:"revision,omitempty"`
	// ReleaseStatus is Helm's own word for the release: deployed, failed, pending-upgrade.
	// +optional
	ReleaseStatus string `json:"releaseStatus,omitempty"`
	// Workloads counts the release's Deployments, StatefulSets and DaemonSets; ReadyWorkloads
	// those fully rolled out.
	// +optional
	Workloads int32 `json:"workloads,omitempty"`
	// +optional
	ReadyWorkloads int32 `json:"readyWorkloads,omitempty"`
	// Message says why the release is not Ready, in the words of whatever refused: the chart
	// repository, Helm, the cluster's API server, or the release's own pods.
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
//+kubebuilder:resource:scope=Cluster,shortName=chr
//+kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterRef`
//+kubebuilder:printcolumn:name="Release",type=string,JSONPath=`.spec.releaseName`
//+kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.chart.version`
//+kubebuilder:printcolumn:name="Installed",type=string,JSONPath=`.status.chartVersion`
//+kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
//+kubebuilder:printcolumn:name="Message",type=string,JSONPath=`.status.message`

// ClusterHelmRelease is one Helm chart release on a cluster this one manages, at a pinned
// version: the cluster-scoped software a leaf needs before and around its operator
// (CloudNativePG, an ingress controller, the service operators). Reconciled by the management
// cluster's operator only.
type ClusterHelmRelease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterHelmReleaseSpec   `json:"spec,omitempty"`
	Status ClusterHelmReleaseStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// ClusterHelmReleaseList is a list of ClusterHelmRelease.
type ClusterHelmReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterHelmRelease `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterHelmRelease{}, &ClusterHelmReleaseList{})
}

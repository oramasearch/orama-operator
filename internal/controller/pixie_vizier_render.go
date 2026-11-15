/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Vizier, rendered for one PixieVizier (operator#15, #456).

const (
	// vizierServiceTLSSecret holds the Vizier services' CA, server and client certificates, in the
	// layout Pixie's cert provisioner writes (server.crt, server.key, client.crt, client.key, ca.crt).
	vizierServiceTLSSecret = "service-tls-certs"
	// vizierClusterSecret holds jwt-signing-key (minted once) and, after registration, the
	// cluster-id and cluster-name the cloud connector writes back. The operator never overwrites
	// those two.
	vizierClusterSecret  = "pl-cluster-secrets"
	vizierDeploySecret   = "pl-deploy-secrets"
	vizierCloudConfig    = "pl-cloud-config"
	vizierClusterConfig  = "pl-cluster-config"
	vizierCloudCAPath    = "/etc/pixie-cloud-ca"
	pixieCertsAnnotation = "orama.cloud/pixie-certs"
	// pixieConfigAnnotation is a fingerprint of the ConfigMaps a pod reads (stampPixieConfigHash).
	pixieConfigAnnotation = "orama.cloud/pixie-config"
)

// vizierObjects is exactly the Vizier the operator applies.
var vizierObjects = []pixieObjectKey{
	{"ServiceAccount", "cloud-conn-service-account"},
	{"ServiceAccount", "metadata-service-account"},
	{"ServiceAccount", "query-broker-service-account"},
	{"Role", "pl-cloud-connector-ns-role"},
	{"Role", "pl-vizier-metadata-role"},
	{"Role", "pl-vizier-query-broker-role"},
	{"ClusterRole", "pl-cloud-connector-role"},
	{"ClusterRole", "pl-node-view"},
	{"ClusterRole", "pl-vizier-metadata"},
	{"RoleBinding", "pl-cloud-connector-binding"},
	{"RoleBinding", "pl-vizier-metadata-binding"},
	{"RoleBinding", "pl-vizier-query-broker-binding"},
	{"ClusterRoleBinding", "pl-cloud-connector-cluster-binding"},
	{"ClusterRoleBinding", "pl-node-view-cluster-binding"},
	{"ClusterRoleBinding", "pl-vizier-metadata-cluster-binding"},
	{"ClusterRoleBinding", "pl-vizier-metadata-node-view-cluster-binding"},
	{"ConfigMap", "pl-cloud-connector-tls-config"},
	{"ConfigMap", "pl-tls-config"},
	{"Service", "kelvin-service"},
	{"Service", "vizier-cloud-connector-svc"},
	{"Service", "vizier-metadata-svc"},
	{"Service", "vizier-query-broker-svc"},
	{"PersistentVolumeClaim", "metadata-pv-claim"},
	{"Deployment", "kelvin"},
	{"Deployment", "vizier-cloud-connector"},
	{"Deployment", "vizier-query-broker"},
	{"StatefulSet", "vizier-metadata"},
	{"DaemonSet", "vizier-pem"},
	{"ConfigMap", "nats-config"},
	{"Service", "pl-nats"},
	{"Service", "pl-nats-mgmt"},
	{"StatefulSet", "pl-nats"},
}

// vizierDropped is what the vendored Vizier holds and the operator never applies: the updater,
// which would change versions on its own; the cert provisioner Job, whose certificates the operator
// mints; and the RBAC of Pixie's own Vizier custom resource, which does not exist without Pixie's
// operator (the cloud connector then reads its address from pl-cloud-config).
var vizierDropped = []pixieObjectKey{
	{"ServiceAccount", "pl-cert-provisioner-service-account"},
	{"ServiceAccount", "pl-updater-service-account"},
	{"Role", "pl-cert-provisioner-role"},
	{"Role", "pl-updater-role"},
	{"Role", "pl-vizier-crd-role"},
	{"ClusterRole", "pl-updater-cluster-role"},
	{"RoleBinding", "pl-cert-provisioner-binding"},
	{"RoleBinding", "pl-updater-binding"},
	{"RoleBinding", "pl-vizier-crd-binding"},
	{"RoleBinding", "pl-vizier-crd-metadata-binding"},
	{"RoleBinding", "pl-vizier-query-broker-crd-binding"},
	{"ClusterRoleBinding", "pl-updater-cluster-binding"},
	{"Job", "cert-provisioner-job"},
}

// vizierBudgets caps every Vizier container. The node agent's is set from spec.nodeMemory instead.
var vizierBudgets = map[string]pixieBudget{
	"kelvin/cc-wait":                       pixieWaitBudget,
	"kelvin/qb-wait":                       pixieWaitBudget,
	"kelvin/app":                           {CPURequest: "200m", CPULimit: "2", MemoryRequest: "1Gi", MemoryLimit: "2Gi"},
	"vizier-cloud-connector/nats-wait":     pixieWaitBudget,
	"vizier-cloud-connector/app":           {CPURequest: "50m", CPULimit: "500m", MemoryRequest: "128Mi", MemoryLimit: "256Mi", GoRuntime: true},
	"vizier-query-broker/cc-wait":          pixieWaitBudget,
	"vizier-query-broker/mds-wait":         pixieWaitBudget,
	"vizier-query-broker/app":              {CPURequest: "100m", CPULimit: "1", MemoryRequest: "256Mi", MemoryLimit: "512Mi", GoRuntime: true},
	"vizier-metadata/nats-wait":            pixieWaitBudget,
	"vizier-metadata/app":                  {CPURequest: "100m", CPULimit: "1", MemoryRequest: "512Mi", MemoryLimit: "1Gi", GoRuntime: true},
	"vizier-pem/qb-wait":                   pixieWaitBudget,
	"pl-nats/pl-nats":                      {CPURequest: "50m", CPULimit: "500m", MemoryRequest: "128Mi", MemoryLimit: "256Mi", GoRuntime: true},
	"vizier-pem/" + vizierPEMContainerName: {}, // from spec.nodeMemory
}

const vizierPEMContainerName = "pem"

// pixieWaitBudget is the budget of an init container that only waits for a dependency with curl.
var pixieWaitBudget = pixieBudget{CPURequest: "10m", CPULimit: "100m", MemoryRequest: "16Mi", MemoryLimit: "32Mi"}

// pemBudget is the node agent's budget: its memory request and limit are spec.nodeMemory, and its
// table store (PL_TABLE_STORE_DATA_LIMIT_MB) is 60% of it, the ratio of Pixie's own default (2 GiB
// with 1280 MiB of data).
func pemBudget(nodeMemory resource.Quantity) (corev1.ResourceRequirements, string) {
	memory := nodeMemory.DeepCopy()
	requirements := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m"), corev1.ResourceMemory: memory},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: memory},
	}
	tableStoreMiB := memory.Value() / (1024 * 1024) * 6 / 10
	return requirements, fmt.Sprintf("%d", tableStoreMiB)
}

// vizierRender is everything rendering needs beyond the spec.
type vizierRender struct {
	Vizier *oramav1alpha1.PixieVizier
	// CertsHash changes whenever the service certificates or the hub's CA change, and rolls every
	// pod that reads them.
	CertsHash string
}

// renderVizier is the Vizier for one PixieVizier, in apply order: RBAC and config first, the
// workloads last.
func renderVizier(input vizierRender) ([]client.Object, error) {
	vz := input.Vizier
	if problems := pixieReleaseProblems(vz.Spec.Release); len(problems) > 0 {
		return nil, fmt.Errorf("%s", problems[0])
	}
	if err := checkVizierCloudAddress(vz.Spec.CloudAddress); err != nil {
		return nil, err
	}
	if vz.Spec.NodeMemory.Value() < 1<<30 {
		return nil, fmt.Errorf("nodeMemory %s is below 1Gi, which leaves the node agent no room for data", vz.Spec.NodeMemory.String())
	}
	objects, err := pixieBundle([]string{"vizier.yaml", "vizier-nats.yaml"}, vizierObjects, vizierDropped)
	if err != nil {
		return nil, err
	}
	objects = append(objects, vizierConfigMaps(vz)...)
	for _, object := range objects {
		stampPixieObject(object, pixieManagedVizier, oramav1alpha1.PixieVizierNamespace, vz.Spec.PullSecretName)
		bindingsToNamespace(object, oramav1alpha1.PixieVizierNamespace)
		if err := setReleaseImages(object, vz.Spec.Release.Images); err != nil {
			return nil, err
		}
		if err := applyBudgets(object, vizierBudgets); err != nil {
			return nil, err
		}
		if err := transformVizierObject(object, input); err != nil {
			return nil, err
		}
	}
	stampPixieConfigHash(objects)
	return objects, nil
}

func vizierConfigMaps(vz *oramav1alpha1.PixieVizier) []client.Object {
	_, tableStore := pemBudget(vz.Spec.NodeMemory)
	return []client.Object{
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: vizierCloudConfig},
			Data: map[string]string{
				"PL_CLOUD_ADDR":        vz.Spec.CloudAddress,
				"PL_CLUSTER_NAME":      vz.Spec.ClusterName,
				"PL_UPDATE_CLOUD_ADDR": "",
			},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: vizierClusterConfig},
			Data: map[string]string{
				"PL_CUSTOM_ANNOTATIONS":        "",
				"PL_CUSTOM_LABELS":             "",
				"PL_DISABLE_AUTO_UPDATE":       "true",
				"PL_ETCD_OPERATOR_ENABLED":     "false",
				"PL_MD_ETCD_SERVER":            "",
				"PX_MEMORY_LIMIT":              vz.Spec.NodeMemory.String(),
				"PX_MEMORY_REQUEST":            vz.Spec.NodeMemory.String(),
				"PL_TABLE_STORE_DATA_LIMIT_MB": tableStore,
			},
		},
	}
}

func transformVizierObject(object client.Object, input vizierRender) error {
	vz := input.Vizier
	spec, _ := podSpecOf(object)
	if spec == nil {
		if pvc, ok := object.(*corev1.PersistentVolumeClaim); ok && pvc.Name == "metadata-pv-claim" {
			// 16Gi upstream; the metadata store holds Kubernetes metadata only.
			pvc.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("16Gi")}
		}
		return nil
	}
	pruneUnmountedVolumes(spec)
	for i := range spec.Containers {
		c := &spec.Containers[i]
		// No error reporting to Sentry: nothing leaves the cluster (#456).
		removeEnv(c, "PL_SENTRY_DSN")
	}
	template := podTemplateAnnotations(object)
	template[pixieCertsAnnotation] = input.CertsHash
	switch object.GetName() {
	case "vizier-cloud-connector":
		app := containerNamed(spec, "app")
		if app == nil {
			return fmt.Errorf("vizier-cloud-connector has no app container")
		}
		setEnvVar(app, corev1.EnvVar{Name: "PL_CLOUD_ADDR", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: vizierCloudConfig}, Key: "PL_CLOUD_ADDR",
		}}})
		setEnvVar(app, corev1.EnvVar{Name: "PL_CLUSTER_NAME", ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: vizierCloudConfig}, Key: "PL_CLUSTER_NAME",
		}}})
		setEnv(app, "PL_DISABLE_AUTO_UPDATE", "true")
		setEnvVar(app, secretEnv("PL_DEPLOY_KEY", vz.Spec.DeployKeySecretName, "deploy-key"))
		// Pixie verifies the cloud's certificate against the system roots and has no flag for a
		// private CA, so the hub's CA is given to Go as its root file. Pixie skips verification
		// entirely for an address under .svc.cluster.local; the operator never uses one.
		setEnv(app, "SSL_CERT_FILE", vizierCloudCAPath+"/ca.crt")
		app.VolumeMounts = append(app.VolumeMounts, corev1.VolumeMount{Name: "cloud-ca", MountPath: vizierCloudCAPath, ReadOnly: true})
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: "cloud-ca", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: vz.Spec.CloudCASecretName, Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}},
		}})
	case "vizier-pem":
		pem := containerNamed(spec, vizierPEMContainerName)
		if pem == nil {
			return fmt.Errorf("vizier-pem has no pem container")
		}
		requirements, tableStore := pemBudget(vz.Spec.NodeMemory)
		pem.Resources = requirements
		setEnv(pem, "PL_TABLE_STORE_DATA_LIMIT_MB", tableStore)
	}
	return nil
}

// podTemplateAnnotations is the (created if absent) annotation map of a workload's pod template.
func podTemplateAnnotations(object client.Object) map[string]string {
	var meta *map[string]string
	switch o := object.(type) {
	case *appsv1.Deployment:
		meta = &o.Spec.Template.Annotations
	case *appsv1.StatefulSet:
		meta = &o.Spec.Template.Annotations
	case *appsv1.DaemonSet:
		meta = &o.Spec.Template.Annotations
	default:
		return map[string]string{}
	}
	if *meta == nil {
		*meta = map[string]string{}
	}
	return *meta
}

// pruneUnmountedVolumes drops volumes no container mounts. The query broker declares an envoy
// ConfigMap volume it never mounts, and the kubelet would wait for that ConfigMap forever.
func pruneUnmountedVolumes(spec *corev1.PodSpec) {
	mounted := map[string]bool{}
	for _, containers := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for _, c := range containers {
			for _, mount := range c.VolumeMounts {
				mounted[mount.Name] = true
			}
		}
	}
	kept := spec.Volumes[:0]
	for _, volume := range spec.Volumes {
		if mounted[volume.Name] {
			kept = append(kept, volume)
		}
	}
	spec.Volumes = kept
}

// vizierCloudAddresses are the only hub addresses a Vizier dials: the hub's vzconn on its own
// cluster, or the PeerConnection import in Vizier's namespace. Both are names on the hub's proxy
// certificate (pixieProxyDNSNames). Pixie's cloud connector dials gRPC, which sets the TLS server
// name to the address's host (grpc v1.43.0 credentials/tls.go), and verifies it against Go's root
// file, which the operator sets to the hub's CA; it skips verification for any address containing
// .svc.cluster.local, so no such address is ever rendered.
var vizierCloudAddresses = map[string]bool{
	fmt.Sprintf("%s.%s.svc:%d", pixieVzconnService, oramav1alpha1.PixieCloudNamespace, pixieVzconnPort):       true,
	fmt.Sprintf("%s.%s.svc:%d", pixieAgentImportService, oramav1alpha1.PixieVizierNamespace, pixieVzconnPort): true,
}

func checkVizierCloudAddress(address string) error {
	if strings.Contains(address, ".svc.cluster.local") {
		return fmt.Errorf("cloudAddress %q is under .svc.cluster.local, for which Pixie skips certificate verification", address)
	}
	if !vizierCloudAddresses[address] {
		return fmt.Errorf("cloudAddress %q is not one of the hub addresses on its certificate", address)
	}
	return nil
}

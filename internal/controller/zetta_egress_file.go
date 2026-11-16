// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// A Zetta's egress allowlist as a file it re-reads (operator#724, zetta#1332, zetta PR #1510).
//
// The Lake reconcilers render each workspace Zetta's and managed Zetta service's allowlist from the
// namespace's NetworkSandbox (lake_zetta_egress.go). Until now the value was ZETTA_EGRESS_ALLOW in
// the pod template, so every EgressRule or Connection change restarted the Zetta. The value now
// lives in ConfigMap <cluster>-zetta-egress (key allow), named by spec.egressAllowConfigMap, and is
// mounted on every node at /etc/zetta/egress/allow with ZETTA_EGRESS_ALLOW_FILE pointing at it.
//
// Whether the image reads that file is what the image says, never a flag: Job
// <cluster>-zetta-features runs `zetta --help` from spec.image once per image, for every cluster
// (the identity and enrollment settings of zetta_identity.go follow the same answer, operator#1174),
// and the image re-reads the file when its usage lists ZETTA_EGRESS_ALLOW_FILE
// (status.imageFeatures). Then:
//
//   - an image that reads the file gets ZETTA_EGRESS_ALLOW empty (the static part denies all) and
//     the file is its whole allowlist, so a ConfigMap change reaches the running node with no restart;
//   - any other image, and any image whose answer is not known yet, gets the ConfigMap's value as
//     ZETTA_EGRESS_ALLOW, exactly as before: a change rolls the StatefulSet. The file is mounted for
//     it too, which an image that knows it reads on top of the same value, so the policy is the same.
//
// So the operator rolls first and the Zetta image that reads the file follows; the switch is one roll
// of the StatefulSet, when the answer for the new image arrives.

const (
	// zettaEgressConfigMapKey is the ConfigMap key holding the allowlist.
	zettaEgressConfigMapKey = "allow"
	// zettaEgressConfigMapSuffix names a cluster's ConfigMap: <cluster>-zetta-egress.
	zettaEgressConfigMapSuffix = "-zetta-egress"
	zettaEgressVolume          = "egress-allow"
	zettaEgressMountPath       = "/etc/zetta/egress"
	zettaEgressAllowFilePath   = zettaEgressMountPath + "/" + zettaEgressConfigMapKey
	// zettaEgressAllowFileEnv is the engine's file variable, and what its usage lists when it has it.
	zettaEgressAllowFileEnv = "ZETTA_EGRESS_ALLOW_FILE"
	// zettaEgressFileMode: the file is policy, not a secret, but nothing in the pod writes it.
	zettaEgressFileMode int32 = 0o444

	// zettaFeaturesImageAnnotation records, on the features Job, the image it reads.
	zettaFeaturesImageAnnotation = "orama.cloud/zetta-image"
	// zettaFeaturesContainer is the features Job's only container.
	zettaFeaturesContainer = "features"
	// zettaFeaturesLogLimit bounds the pod log read: the usage text is a few KB.
	zettaFeaturesLogLimit = int64(256 << 10)
	// zettaFeaturesPoll is how often a cluster waiting for its image's answer is looked at again.
	zettaFeaturesPoll = 10 * time.Second
	// zettaFeaturesRetry is how long a features Job that gave no answer stays before it is replaced.
	zettaFeaturesRetry = 2 * time.Minute
)

// zettaEgressConfigMapName is the ConfigMap holding a cluster's egress allowlist.
func zettaEgressConfigMapName(clusterName string) string {
	return clusterName + zettaEgressConfigMapSuffix
}

// zettaFeaturesJobName names a cluster's features Job.
func zettaFeaturesJobName(z *oramav1alpha1.OramaZettaCluster) string {
	return z.Name + "-zetta-features"
}

// zettaImage is the image the cluster's nodes run.
func zettaImage(z *oramav1alpha1.OramaZettaCluster) string {
	if z.Spec.Image == "" {
		return "oramasearch/zetta:headless-v1-sandbox"
	}
	return z.Spec.Image
}

// zettaEgressRender is what the StatefulSet carries for spec.egressAllowConfigMap.
type zettaEgressRender struct {
	// configMap is the mounted ConfigMap; empty renders nothing.
	configMap string
	// allow is the ConfigMap's allowlist, ZETTA_EGRESS_ALLOW for an image that does not read the file.
	allow string
	// file is true when the image re-reads the file: ZETTA_EGRESS_ALLOW is then empty.
	file bool
	// gateway is the ConfigMap's gateway and routes (operator#1179), set as ZETTA_EGRESS_GATEWAY and
	// ZETTA_EGRESS_GATEWAY_ROUTES when gatewayEnv says the image reads them.
	gateway    zettaEgressGateway
	gatewayEnv bool
}

// zettaEgressState is what one reconcile knows about the cluster's egress allowlist file.
type zettaEgressState struct {
	render zettaEgressRender
	// hold is true while the named ConfigMap does not exist: the StatefulSet is left as it is, so no
	// node starts with an allowlist it was not given.
	hold bool
	// features is what status.imageFeatures holds after this reconcile.
	features *oramav1alpha1.ZettaImageFeatures
	// requeue is when to look again while the image's answer is being read.
	requeue time.Duration
}

// reconcileZettaEgressFile reads the image's answer and spec.egressAllowConfigMap. Without the field
// nothing is mounted.
func (r *OramaZettaClusterReconciler) reconcileZettaEgressFile(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) (zettaEgressState, error) {
	features, requeue, err := r.reconcileZettaImageFeatures(ctx, z)
	if err != nil {
		return zettaEgressState{}, err
	}
	return r.reconcileZettaEgressFileWith(ctx, z, features, requeue)
}

// reconcileZettaEgressFileWith reads spec.egressAllowConfigMap, given the image's answer (which every
// cluster reads, operator#1174: the identity and enrollment settings follow it too).
func (r *OramaZettaClusterReconciler) reconcileZettaEgressFileWith(
	ctx context.Context,
	z *oramav1alpha1.OramaZettaCluster,
	features *oramav1alpha1.ZettaImageFeatures,
	requeue time.Duration,
) (zettaEgressState, error) {
	name := z.Spec.EgressAllowConfigMap
	if name == "" {
		return zettaEgressState{features: features, requeue: requeue}, nil
	}
	var configMap corev1.ConfigMap
	if err := r.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: name}, &configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return zettaEgressState{hold: true, features: features, requeue: zettaFeaturesPoll}, nil
		}
		return zettaEgressState{}, err
	}
	return zettaEgressState{
		render: zettaEgressRender{
			configMap: name,
			allow:     configMap.Data[zettaEgressConfigMapKey],
			file:      zettaImageReadsEgressFile(z, features),
			gateway: zettaEgressGateway{
				address: configMap.Data[zettaEgressGatewayKey],
				routes:  configMap.Data[zettaEgressGatewayRoutesKey],
			},
			gatewayEnv: zettaImageRoutesThroughGateway(z, features),
		},
		features: features,
		requeue:  requeue,
	}, nil
}

// zettaImageRoutesThroughGateway reports whether the answer is for spec.image and says the image
// reads ZETTA_EGRESS_GATEWAY and ZETTA_EGRESS_GATEWAY_ROUTES (zetta#1331).
func zettaImageRoutesThroughGateway(z *oramav1alpha1.OramaZettaCluster, features *oramav1alpha1.ZettaImageFeatures) bool {
	return features != nil && features.Image == zettaImage(z) && features.EgressGateway
}

// zettaImageReadsClusterTokenFile reports whether the answer is for spec.image and says the image
// reads ZETTA_CLUSTER_TOKEN_FILE (zetta#770). An answer for another image, or none, is false, so a
// node keeps ZETTA_CLUSTER_TOKEN until its own image is known to read the file (operator#721).
func zettaImageReadsClusterTokenFile(z *oramav1alpha1.OramaZettaCluster, features *oramav1alpha1.ZettaImageFeatures) bool {
	return features != nil && features.Image == zettaImage(z) && features.ClusterTokenFile
}

// zettaImageReadsEgressFile reports whether the answer is for spec.image and says the image re-reads
// ZETTA_EGRESS_ALLOW_FILE. An answer for another image, or none, is false.
func zettaImageReadsEgressFile(z *oramav1alpha1.OramaZettaCluster, features *oramav1alpha1.ZettaImageFeatures) bool {
	return features != nil && features.Image == zettaImage(z) && features.EgressAllowFile
}

// zettaPodLogsConfigured reports whether this reconciler can read a pod's log. Without one (unit
// tests that do not exercise it) no features Job is made, and every image gets the variable.
func (r *OramaZettaClusterReconciler) zettaPodLogsConfigured() bool {
	return r.PodLogs != nil || r.Clientset != nil
}

// reconcileZettaImageFeatures returns the answer for spec.image, reading it once with a Job. While
// it is read, the previous answer is returned (it is for another image, so nothing acts on it).
func (r *OramaZettaClusterReconciler) reconcileZettaImageFeatures(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) (*oramav1alpha1.ZettaImageFeatures, time.Duration, error) {
	cached := z.Status.ImageFeatures
	image := zettaImage(z)
	if cached != nil && cached.Image == image {
		return cached, 0, nil
	}
	if !r.zettaPodLogsConfigured() {
		return cached, 0, nil
	}
	job, err := r.ensureZettaFeaturesJob(ctx, z, image)
	if err != nil || job == nil {
		return cached, zettaFeaturesPoll, err
	}
	log, exitCode, found, err := r.readZettaFeaturesPod(ctx, job)
	if err != nil {
		return cached, zettaFeaturesPoll, nil
	}
	if found {
		return parseZettaImageFeatures(image, log, exitCode), 0, nil
	}
	if (job.Status.Succeeded > 0 || jobFailed(job)) && lakeManifestJobFinishedBefore(job, time.Now().Add(-zettaFeaturesRetry)) {
		// No answer (a pull or scheduling failure): try again with a fresh Job.
		policy := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy}); client.IgnoreNotFound(err) != nil {
			return cached, zettaFeaturesPoll, err
		}
	}
	return cached, zettaFeaturesPoll, nil
}

// parseZettaImageFeatures reads `zetta --help`. An image that re-reads the allowlist file lists
// ZETTA_EGRESS_ALLOW_FILE in its usage and exits 0; one with identity root rotation lists
// ZETTA_IDENTITY_MIN_EPOCH; one with CTL-2 V2 enrollment lists ZETTA_CLUSTER_CA and
// ZETTA_CLUSTER_CA_SEED_FILE (operator#1174); one that reads the join token from its file lists
// ZETTA_CLUSTER_TOKEN_FILE (zetta#770, operator#721). Every other answer (an older usage, an unknown option,
// a crash) is an image that has none of them, which renders it as before: the safe form for any image.
func parseZettaImageFeatures(image, log string, exitCode int32) *oramav1alpha1.ZettaImageFeatures {
	clean := exitCode == 0
	return &oramav1alpha1.ZettaImageFeatures{
		Image:                image,
		EgressAllowFile:      clean && zettaUsageListsEnv(log, zettaEgressAllowFileEnv),
		IdentityRootRotation: clean && zettaUsageListsEnv(log, zettaIdentityMinEpochEnv),
		ClusterCA:            clean && zettaUsageListsEnv(log, zettaClusterCAEnv),
		ClusterCASeedFile:    clean && zettaUsageListsEnv(log, zettaClusterCASeedFileEnv),
		EgressGateway: clean && zettaUsageListsEnv(log, zettaEgressGatewayEnv) &&
			zettaUsageListsEnv(log, zettaEgressGatewayRoutesEnv),
		ClusterTokenFile: clean && zettaUsageListsEnv(log, zettaClusterTokenFileEnv),
	}
}

// zettaUsageListsEnv reports whether a usage text names variable as a whole word.
func zettaUsageListsEnv(log, variable string) bool {
	for _, field := range strings.FieldsFunc(log, func(c rune) bool {
		return (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_'
	}) {
		if field == variable {
			return true
		}
	}
	return false
}

// ensureZettaFeaturesJob returns the Job reading image's usage, creating it when missing. A Job for
// another image is deleted first; nil is returned while it goes.
func (r *OramaZettaClusterReconciler) ensureZettaFeaturesJob(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, image string) (*batchv1.Job, error) {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: zettaFeaturesJobName(z), Namespace: z.Namespace}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(job), job); err == nil {
		if !metav1.IsControlledBy(job, z) {
			return nil, fmt.Errorf("features Job %s is not controlled by OramaZettaCluster %s", job.Name, z.Name)
		}
		if !job.DeletionTimestamp.IsZero() {
			return nil, nil
		}
		if job.Annotations[zettaFeaturesImageAnnotation] != image {
			policy := metav1.DeletePropagationBackground
			err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy,
				Preconditions: &metav1.Preconditions{UID: &job.UID}})
			return nil, client.IgnoreNotFound(err)
		}
		return job, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	job.Labels = zettaFeaturesJobLabels(z)
	job.Annotations = map[string]string{zettaFeaturesImageAnnotation: image}
	job.Spec = zettaFeaturesJobSpec(z, image)
	if err := controllerutil.SetControllerReference(z, job, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil, nil
		}
		return nil, err
	}
	return job, nil
}

// zettaFeaturesJobLabels never match the cluster's Services (no app.kubernetes.io/name) and carry
// no workload identity: `zetta --help` needs no network and no credential.
func zettaFeaturesJobLabels(z *oramav1alpha1.OramaZettaCluster) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "orama-operator",
		"orama.cloud/zetta-features":   z.Name,
	}
}

// zettaFeaturesJobSpec runs `zetta --help` from image as the zetta user: no environment, no volume,
// no service account token. One attempt: the usage is printed at once.
func zettaFeaturesJobSpec(z *oramav1alpha1.OramaZettaCluster, image string) batchv1.JobSpec {
	securityContext := hardenedSecurityContext()
	securityContext.RunAsNonRoot = boolPtr(true)
	securityContext.ReadOnlyRootFilesystem = boolPtr(true)
	deadline := int64(600) // covers pulling the image
	ttl := int32(3600)
	spec := batchv1.JobSpec{
		BackoffLimit:            int32Ptr(0),
		ActiveDeadlineSeconds:   &deadline,
		TTLSecondsAfterFinished: &ttl,
		Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: zettaFeaturesJobLabels(z)},
			Spec: corev1.PodSpec{
				RestartPolicy:                corev1.RestartPolicyNever,
				AutomountServiceAccountToken: boolPtr(false),
				EnableServiceLinks:           boolPtr(false),
				SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot: boolPtr(true),
					RunAsUser:    int64Ptr(10001),
					RunAsGroup:   int64Ptr(10001),
				},
				Containers: []corev1.Container{{
					Name:            zettaFeaturesContainer,
					Image:           image,
					Command:         []string{"zetta"},
					Args:            []string{"--help"},
					Resources:       *resourceReqs("32Mi", "10m", "256Mi"),
					SecurityContext: securityContext,
				}},
			},
		},
	}
	if z.Spec.PullSecretName != "" {
		spec.Template.Spec.ImagePullSecrets = []corev1.LocalObjectReference{{Name: z.Spec.PullSecretName}}
	}
	return spec
}

// readZettaFeaturesPod returns the log and exit code of the newest terminated pod of this Job.
func (r *OramaZettaClusterReconciler) readZettaFeaturesPod(ctx context.Context, job *batchv1.Job) (string, int32, bool, error) {
	var pods corev1.PodList
	if err := r.reader().List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return "", 0, false, err
	}
	pods.Items = slices.DeleteFunc(pods.Items, func(pod corev1.Pod) bool { return !metav1.IsControlledBy(&pod, job) })
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[j].CreationTimestamp.Before(&pods.Items[i].CreationTimestamp)
	})
	for i := range pods.Items {
		pod := &pods.Items[i]
		for _, container := range pod.Status.ContainerStatuses {
			if container.Name != zettaFeaturesContainer || container.State.Terminated == nil {
				continue
			}
			log, err := r.zettaFeaturesLog(ctx, job.Namespace, pod.Name)
			if err != nil {
				return "", 0, false, err
			}
			return log, container.State.Terminated.ExitCode, true, nil
		}
	}
	return "", 0, false, nil
}

func (r *OramaZettaClusterReconciler) zettaFeaturesLog(ctx context.Context, namespace, podName string) (string, error) {
	if r.PodLogs != nil {
		return r.PodLogs(ctx, namespace, podName)
	}
	limit := zettaFeaturesLogLimit
	raw, err := r.Clientset.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: zettaFeaturesContainer, LimitBytes: &limit,
	}).DoRaw(ctx)
	return string(raw), err
}

// applyZettaEgressRender mounts the allowlist file on the Zetta container and sets the two
// variables: ZETTA_EGRESS_ALLOW_FILE always, and ZETTA_EGRESS_ALLOW empty for an image that reads
// the file or the ConfigMap's value for any other, unless spec.env names ZETTA_EGRESS_ALLOW.
func applyZettaEgressRender(pod *corev1.PodSpec, render zettaEgressRender, specEnv []corev1.EnvVar) {
	if render.configMap == "" {
		return
	}
	pod.Volumes = append(pod.Volumes, corev1.Volume{
		Name: zettaEgressVolume,
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: render.configMap},
			Items:                []corev1.KeyToPath{{Key: zettaEgressConfigMapKey, Path: zettaEgressConfigMapKey}},
			DefaultMode:          int32Ptr(zettaEgressFileMode),
			// Present before the StatefulSet changes (the reconcile holds until it is); a key removed
			// later leaves the engine's last good policy in force rather than a pod that cannot start.
			Optional: boolPtr(true),
		}},
	})
	container := &pod.Containers[0]
	container.VolumeMounts = append(container.VolumeMounts,
		corev1.VolumeMount{Name: zettaEgressVolume, MountPath: zettaEgressMountPath, ReadOnly: true})
	container.Env = append(container.Env, corev1.EnvVar{Name: zettaEgressAllowFileEnv, Value: zettaEgressAllowFilePath})
	// Named egress through the workspace gateway (operator#1179): only for an image that reads both
	// variables, and only while the namespace has a name rule for this Zetta. Zetta reads the routes
	// from its environment, so a change rolls the nodes.
	if render.gatewayEnv && render.gateway.routes != "" && render.gateway.address != "" {
		container.Env = append(container.Env,
			corev1.EnvVar{Name: zettaEgressGatewayEnv, Value: render.gateway.address},
			corev1.EnvVar{Name: zettaEgressGatewayRoutesEnv, Value: render.gateway.routes})
	}
	if findEnvVar(specEnv, zettaEgressAllowEnv) != nil {
		return
	}
	static := render.allow
	if render.file {
		static = zettaEgressDeny
	}
	container.Env = append(container.Env, corev1.EnvVar{Name: zettaEgressAllowEnv, Value: static})
}

// lakeZettaEgressConfigMapRef is spec.egressAllowConfigMap for a Lake-owned workspace Zetta or Zetta
// service: its ConfigMap, or "" when the Lake's spec.services.zettaEnv states ZETTA_EGRESS_ALLOW,
// which is then kept as the whole allowlist.
func lakeZettaEgressConfigMapRef(lake *oramav1alpha1.Lake, clusterName string) string {
	if findEnvVar(lake.Spec.Services.ZettaEnv, zettaEgressAllowEnv) != nil {
		return ""
	}
	return zettaEgressConfigMapName(clusterName)
}

// currentZettaEgressAllow is the allowlist the cluster runs with now: its ConfigMap's value, else
// (a cluster still on the variable) ZETTA_EGRESS_ALLOW in its spec.env. It is what a failed read of
// the namespace keeps, so a failed read never changes what the Zetta may reach.
func currentZettaEgressAllow(ctx context.Context, reader client.Reader, cluster *oramav1alpha1.OramaZettaCluster) (string, bool) {
	var configMap corev1.ConfigMap
	key := client.ObjectKey{Namespace: cluster.Namespace, Name: zettaEgressConfigMapName(cluster.Name)}
	if err := reader.Get(ctx, key, &configMap); err == nil {
		if value, found := configMap.Data[zettaEgressConfigMapKey]; found {
			return value, true
		}
	}
	if variable := findEnvVar(cluster.Spec.Env, zettaEgressAllowEnv); variable != nil {
		return variable.Value, true
	}
	return "", false
}

// reconcileZettaEgressConfigMap writes the cluster's allowlist ConfigMap, controlled by the cluster
// so it goes with it and a change requeues it. A cluster with no spec.egressAllowConfigMap gets none,
// and a ConfigMap left from before is deleted.
func reconcileZettaEgressConfigMap(ctx context.Context, c client.Client, scheme *runtime.Scheme, cluster *oramav1alpha1.OramaZettaCluster, allow string) error {
	return reconcileZettaEgressConfigMapWithGateway(ctx, c, scheme, cluster, allow, zettaEgressGateway{})
}

// reconcileZettaEgressConfigMapWithGateway also writes the namespace's egress gateway and the routes
// through it (keys gateway and gatewayRoutes, operator#1179), or neither when there are no routes.
func reconcileZettaEgressConfigMapWithGateway(ctx context.Context, c client.Client, scheme *runtime.Scheme, cluster *oramav1alpha1.OramaZettaCluster, allow string, gateway zettaEgressGateway) error {
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: zettaEgressConfigMapName(cluster.Name), Namespace: cluster.Namespace}}
	if cluster.Spec.EgressAllowConfigMap == "" {
		if err := c.Get(ctx, client.ObjectKeyFromObject(configMap), configMap); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !metav1.IsControlledBy(configMap, cluster) {
			return nil
		}
		return client.IgnoreNotFound(c.Delete(ctx, configMap))
	}
	_, err := controllerutil.CreateOrUpdate(ctx, c, configMap, func() error {
		if configMap.ResourceVersion != "" && !metav1.IsControlledBy(configMap, cluster) {
			return fmt.Errorf("ConfigMap %s/%s is not controlled by OramaZettaCluster %s", configMap.Namespace, configMap.Name, cluster.Name)
		}
		configMap.Labels = zettaLabels(cluster)
		configMap.Data = map[string]string{zettaEgressConfigMapKey: allow}
		if gateway.routes != "" && gateway.address != "" {
			configMap.Data[zettaEgressGatewayKey] = gateway.address
			configMap.Data[zettaEgressGatewayRoutesKey] = gateway.routes
		}
		return controllerutil.SetControllerReference(cluster, configMap, scheme)
	})
	if err != nil {
		return fmt.Errorf("write the Zetta egress allowlist ConfigMap: %w", err)
	}
	return nil
}

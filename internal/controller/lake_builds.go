/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const (
	BuildPhaseQueued    = "Queued"
	BuildPhaseRunning   = "Running"
	BuildPhaseSucceeded = "Succeeded"
	BuildPhaseFailed    = "Failed"
	BuildPhaseCancelled = "Cancelled"

	buildDesiredQueued    = "queued"
	buildDesiredCancelled = "cancelled"

	buildIDLabel          = "orama.cloud/build-id"
	buildLakeLabel        = "orama.cloud/build-lake"
	buildSpaceLabel       = "orama.cloud/build-space"
	buildOwnerAnnotation  = "orama.cloud/build-owner"
	buildSpaceAnnotation  = "orama.cloud/build-space"
	buildEventAnnotation  = "orama.cloud/build-event-"
	buildTokenSourceKey   = "SOURCE_TOKEN"
	buildTokenPushKey     = "PUSH_TOKEN"
	buildTokenEventsKey   = "EVENTS_TOKEN"
	buildArgAllow         = "PYTHON_VERSION,APP_DEPS,APP_RUNTIME_IMAGE"
	buildDefaultPerLake   = int32(4)
	buildDefaultPerSpace  = int32(2)
	buildDeadlineSeconds  = int64(3600)
	buildJobTTLSeconds    = int32(3600)
	buildRootlessUser     = int64(1000)
	buildRootlessGroup    = int64(1000)
	buildNetworkPolicy    = "build-network"
	buildDefaultDeny      = "build-default-deny"
	buildDockerConfigPath = "/home/user/.docker"
)

var (
	errBuildsTableMissing = errors.New("the builds table does not exist yet")
	buildSpacePattern     = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	buildRegistryPattern  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::[0-9]{1,5})?$`)
	buildImagePattern     = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	buildDigestPattern    = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// LakeBuild is one row of lake's builds table.
type LakeBuild struct {
	ID          string
	CustomerID  string
	Repository  string
	Revision    string
	Context     string
	Dockerfile  string
	Target      string
	BuildArgs   string
	Desired     string
	Phase       string
	Digest      string
	Size        int64
	Message     string
	TokenHashes string
}

// LakeBuildStatus is the operator-owned observation written back to a builds row.
type LakeBuildStatus struct {
	BuildID     string
	Phase       string
	Digest      string
	Size        int64
	Message     string
	TokenHashes string
}

func (s LakeBuildStatus) equalBuild(build *LakeBuild) bool {
	return s.Phase == build.Phase && s.Digest == build.Digest && s.Size == build.Size &&
		s.Message == build.Message && canonicalJSON(s.TokenHashes) == canonicalJSON(build.TokenHashes)
}

type buildTarget struct {
	Registry string
	Space    string
	Name     string
}

type buildResult struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

type buildEvent struct {
	BuildID string   `json:"build_id"`
	Type    string   `json:"type"`
	Phase   string   `json:"phase,omitempty"`
	Lines   []string `json:"lines,omitempty"`
	Digest  string   `json:"digest,omitempty"`
	Size    int64    `json:"size,omitempty"`
	Message string   `json:"message,omitempty"`
}

func lakeBuildsEnabled(lake *oramav1alpha1.Lake) bool {
	return lake.Spec.Builds.Enabled && lakePostgresEnabled(lake)
}

func lakeControlRowsEnabled(lake *oramav1alpha1.Lake) bool {
	return lakeServicesEnabled(lake) || lakeBuildsEnabled(lake)
}

func buildCacheEnabled(lake *oramav1alpha1.Lake) bool {
	return lake.Spec.Builds.CacheEnabled == nil || *lake.Spec.Builds.CacheEnabled
}

func buildConcurrency(lake *oramav1alpha1.Lake) (int32, int32) {
	perLake := lake.Spec.Builds.Concurrency.PerLake
	if perLake == 0 {
		perLake = buildDefaultPerLake
	}
	perSpace := lake.Spec.Builds.Concurrency.PerSpace
	if perSpace == 0 {
		perSpace = buildDefaultPerSpace
	}
	return perLake, perSpace
}

func parseBuildTarget(target string) (buildTarget, error) {
	parts := strings.Split(target, "/")
	if len(parts) != 3 || !buildRegistryPattern.MatchString(parts[0]) || !buildSpacePattern.MatchString(parts[1]) ||
		!buildImagePattern.MatchString(parts[2]) {
		return buildTarget{}, fmt.Errorf("target must be <registry-host>/<space>/<name>:<tag>")
	}
	return buildTarget{Registry: parts[0], Space: parts[1], Name: strings.SplitN(parts[2], ":", 2)[0]}, nil
}

func buildHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

func buildNamespace(lake, space string) string {
	suffix := "-builds-" + buildHash(space)
	prefix := "lake-"
	maxLake := 63 - len(prefix) - len(suffix)
	if len(lake) > maxLake {
		lake = strings.TrimRight(lake[:maxLake], "-")
	}
	return prefix + lake + suffix
}

func buildJobName(id string) string {
	return "build-" + buildHash(id)
}

func buildTokenSecretName(id string) string {
	return buildJobName(id) + "-tokens"
}

func buildLabels(lake *oramav1alpha1.Lake, build *LakeBuild, target buildTarget) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "lake-builder",
		"app.kubernetes.io/instance":   buildJobName(build.ID),
		"app.kubernetes.io/managed-by": "orama-operator",
		buildIDLabel:                   build.ID,
		buildLakeLabel:                 lake.Name,
		buildSpaceLabel:                buildHash(target.Space),
	}
}

func buildNamespaceLabels(lake *oramav1alpha1.Lake, space string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":                     "lake-builds",
		"app.kubernetes.io/managed-by":               "orama-operator",
		buildLakeLabel:                               lake.Name,
		buildSpaceLabel:                              buildHash(space),
		"pod-security.kubernetes.io/enforce":         "privileged",
		"pod-security.kubernetes.io/enforce-version": "latest",
		"pod-security.kubernetes.io/audit":           "restricted",
		"pod-security.kubernetes.io/audit-version":   "latest",
		"pod-security.kubernetes.io/warn":            "restricted",
		"pod-security.kubernetes.io/warn-version":    "latest",
	}
}

func buildPolicyLabels(lake *oramav1alpha1.Lake, space string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       "lake-builds",
		"app.kubernetes.io/managed-by": "orama-operator",
		buildLakeLabel:                 lake.Name,
		buildSpaceLabel:                buildHash(space),
	}
}

func buildNamespaceAnnotations(lake *oramav1alpha1.Lake, space string) map[string]string {
	return map[string]string{
		buildOwnerAnnotation: lake.Name,
		buildSpaceAnnotation: space,
	}
}

func (r *LakeReconciler) reconcileBuildNamespace(ctx context.Context, lake *oramav1alpha1.Lake, target buildTarget) (string, error) {
	name := buildNamespace(lake.Name, target.Space)
	desired := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name, Labels: buildNamespaceLabels(lake, target.Space), Annotations: buildNamespaceAnnotations(lake, target.Space),
	}}
	if err := r.Create(ctx, desired); err == nil {
		return name, nil
	} else if !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create build namespace %s: %w", name, err)
	}
	existing := &corev1.Namespace{}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, client.ObjectKey{Name: name}, existing); err != nil {
		return "", fmt.Errorf("read build namespace %s: %w", name, err)
	}
	if existing.Annotations[buildOwnerAnnotation] != lake.Name || existing.Annotations[buildSpaceAnnotation] != target.Space {
		return "", fmt.Errorf("namespace %s exists and is not this Lake's build space", name)
	}
	changed := mergeStringMap(&existing.Labels, buildNamespaceLabels(lake, target.Space))
	changed = mergeStringMap(&existing.Annotations, buildNamespaceAnnotations(lake, target.Space)) || changed
	if changed {
		if err := r.Update(ctx, existing); err != nil {
			return "", fmt.Errorf("update build namespace %s: %w", name, err)
		}
	}
	return name, nil
}

func mergeStringMap(destination *map[string]string, wanted map[string]string) bool {
	if *destination == nil {
		*destination = map[string]string{}
	}
	changed := false
	for key, value := range wanted {
		if (*destination)[key] != value {
			(*destination)[key] = value
			changed = true
		}
	}
	return changed
}

func (r *LakeReconciler) reconcileBuildNetworkPolicies(ctx context.Context, lake *oramav1alpha1.Lake, namespace, space string) error {
	labels := buildPolicyLabels(lake, space)
	deny := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: buildDefaultDeny, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, deny, func() error {
		deny.Labels = labels
		deny.Spec.PodSelector = metav1.LabelSelector{}
		deny.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}
		deny.Spec.Ingress = nil
		deny.Spec.Egress = nil
		return controllerutil.SetControllerReference(lake, deny, r.Scheme)
	}); err != nil {
		return fmt.Errorf("reconcile build default-deny NetworkPolicy: %w", err)
	}
	access := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: buildNetworkPolicy, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, access, func() error {
		access.Labels = labels
		access.Spec.PodSelector = metav1.LabelSelector{MatchLabels: map[string]string{buildLakeLabel: lake.Name}}
		access.Spec.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}
		access.Spec.Ingress = nil
		access.Spec.Egress = buildEgressRules(lake)
		return controllerutil.SetControllerReference(lake, access, r.Scheme)
	}); err != nil {
		return fmt.Errorf("reconcile build access NetworkPolicy: %w", err)
	}
	return nil
}

func buildEgressRules(lake *oramav1alpha1.Lake) []networkingv1.NetworkPolicyEgressRule {
	tcp := corev1.ProtocolTCP
	udp := corev1.ProtocolUDP
	lakeNamespace := lakeNamespace(lake)
	return []networkingv1.NetworkPolicyEgressRule{
		{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &udp, Port: buildPort(53)},
				{Protocol: &tcp, Port: buildPort(53)},
			},
		},
		{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": lakeNamespace}},
				PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					"app.kubernetes.io/name": "lake", "app.kubernetes.io/instance": lake.Name,
				}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: buildPort(lakeServicePort)}},
		},
		{
			To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{
				CIDR: "0.0.0.0/0",
				Except: []string{
					"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
					"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15",
					"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
				},
			}}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &tcp, Port: buildPort(80)},
				{Protocol: &tcp, Port: buildPort(443)},
			},
		},
	}
}

func buildPort(port int) *intstr.IntOrString {
	value := intstr.FromInt(port)
	return &value
}

func buildResources(lake *oramav1alpha1.Lake) corev1.ResourceRequirements {
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("500m"),
			corev1.ResourceMemory:           resource.MustParse("1Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:              resource.MustParse("2"),
			corev1.ResourceMemory:           resource.MustParse("4Gi"),
			corev1.ResourceEphemeralStorage: resource.MustParse("20Gi"),
		},
	}
	if lake.Spec.Builds.Resources == nil {
		return resources
	}
	for name, quantity := range lake.Spec.Builds.Resources.Requests {
		resources.Requests[name] = quantity.DeepCopy()
	}
	for name, quantity := range lake.Spec.Builds.Resources.Limits {
		resources.Limits[name] = quantity.DeepCopy()
	}
	return resources
}

func buildCacheRef(build *LakeBuild, target buildTarget, enabled bool) string {
	if !enabled {
		return ""
	}
	return target.Registry + "/" + target.Space + "/cache/" + target.Name
}

func buildEnv(lake *oramav1alpha1.Lake, build *LakeBuild, target buildTarget) []corev1.EnvVar {
	secret := buildTokenSecretName(build.ID)
	return []corev1.EnvVar{
		{Name: "LAKE_URL", Value: lakeInternalURL(lake, lakeNamespace(lake))},
		{Name: "BUILD_ID", Value: build.ID},
		{Name: "REPOSITORY", Value: build.Repository},
		{Name: "REVISION", Value: build.Revision},
		{Name: "CONTEXT", Value: build.Context},
		{Name: "DOCKERFILE", Value: build.Dockerfile},
		{Name: "TARGET", Value: build.Target},
		{Name: "BUILD_ARGS", Value: build.BuildArgs},
		{Name: "BUILD_ARG_ALLOW", Value: buildArgAllow},
		secretEnv("SOURCE_TOKEN", secret, buildTokenSourceKey),
		secretEnv("PUSH_TOKEN", secret, buildTokenPushKey),
		secretEnv("EVENTS_TOKEN", secret, buildTokenEventsKey),
		{Name: "CACHE_REF", Value: buildCacheRef(build, target, buildCacheEnabled(lake))},
		{Name: "DOCKER_CONFIG", Value: buildDockerConfigPath},
	}
}

func buildPod(lake *oramav1alpha1.Lake, build *LakeBuild, target buildTarget, labels map[string]string) corev1.PodSpec {
	no := false
	yes := true
	user := buildRootlessUser
	group := buildRootlessGroup
	unconfinedSeccomp := &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined}
	unconfinedAppArmor := &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined}
	pod := corev1.PodSpec{
		AutomountServiceAccountToken: &no,
		RestartPolicy:                corev1.RestartPolicyNever,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: &yes,
			RunAsUser:    &user,
			RunAsGroup:   &group,
			FSGroup:      &group,
		},
		Containers: []corev1.Container{{
			Name:      "builder",
			Image:     lake.Spec.Builds.BuilderImage,
			Env:       buildEnv(lake, build, target),
			Resources: buildResources(lake),
			SecurityContext: &corev1.SecurityContext{
				RunAsNonRoot:             &yes,
				RunAsUser:                &user,
				RunAsGroup:               &group,
				AllowPrivilegeEscalation: &no,
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				SeccompProfile:           unconfinedSeccomp,
				AppArmorProfile:          unconfinedAppArmor,
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "workspace", MountPath: "/workspace"},
				{Name: "buildkit", MountPath: "/home/user/.local/share/buildkit"},
				{Name: "tmp", MountPath: "/tmp"},
				{Name: "docker-config", MountPath: buildDockerConfigPath},
			},
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		}},
		Volumes: []corev1.Volume{
			buildEmptyDir("workspace", "10Gi"),
			buildEmptyDir("buildkit", "8Gi"),
			buildEmptyDir("tmp", "1Gi"),
			buildEmptyDir("docker-config", "16Mi"),
		},
	}
	if lake.Spec.PullSecretName != "" {
		pod.ImagePullSecrets = []corev1.LocalObjectReference{{Name: lake.Spec.PullSecretName}}
	}
	return pod
}

func buildEmptyDir(name, size string) corev1.Volume {
	quantity := resource.MustParse(size)
	return corev1.Volume{
		Name: name,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
			SizeLimit: &quantity,
		}},
	}
}

func (r *LakeReconciler) reconcileBuildTokens(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, build *LakeBuild, labels map[string]string) (string, error) {
	name := buildTokenSecretName(build.ID)
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := r.Get(ctx, key, secret); err == nil {
		if secret.Labels[buildIDLabel] != build.ID || !metav1.IsControlledBy(secret, lake) {
			return "", fmt.Errorf("Secret %s/%s exists and is not this build's credential", namespace, name)
		}
		for _, token := range []string{buildTokenSourceKey, buildTokenPushKey, buildTokenEventsKey} {
			if len(secret.Data[token]) == 0 {
				return "", fmt.Errorf("build token Secret %s/%s is missing %s", namespace, name, token)
			}
		}
		return buildTokenHashes(secret.Data), nil
	} else if !apierrors.IsNotFound(err) {
		return "", err
	}
	data := map[string][]byte{}
	for _, token := range []string{buildTokenSourceKey, buildTokenPushKey, buildTokenEventsKey} {
		value, err := randomHex(32)
		if err != nil {
			return "", err
		}
		data[token] = []byte(value)
	}
	secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}, Data: data}
	if err := controllerutil.SetControllerReference(lake, secret, r.Scheme); err != nil {
		return "", err
	}
	if err := r.Create(ctx, secret); err != nil {
		return "", fmt.Errorf("create build token Secret %s/%s: %w", namespace, name, err)
	}
	return buildTokenHashes(data), nil
}

func buildTokenHashes(data map[string][]byte) string {
	hashes := map[string]string{}
	for name, key := range map[string]string{
		"source": buildTokenSourceKey,
		"push":   buildTokenPushKey,
		"events": buildTokenEventsKey,
	} {
		sum := sha256.Sum256(data[key])
		hashes[name] = hex.EncodeToString(sum[:])
	}
	encoded, _ := json.Marshal(hashes)
	return string(encoded)
}

func canonicalJSON(value string) string {
	if strings.TrimSpace(value) == "" {
		return "{}"
	}
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return value
	}
	encoded, _ := json.Marshal(decoded)
	return string(encoded)
}

func (r *LakeReconciler) createBuildJob(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, build *LakeBuild, target buildTarget, labels map[string]string) error {
	backoff := int32(0)
	deadline := buildDeadlineSeconds
	ttl := buildJobTTLSeconds
	suspended := true
	name := buildJobName(build.ID)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Suspend:                 &suspended,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       buildPod(lake, build, target, labels),
			},
		},
	}
	if err := controllerutil.SetControllerReference(lake, job, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, job); err != nil {
		return fmt.Errorf("create build Job %s/%s: %w", namespace, name, err)
	}
	return nil
}

func (r *LakeReconciler) activeBuildCounts(ctx context.Context, lake *oramav1alpha1.Lake, space string) (int32, int32, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var jobs batchv1.JobList
	if err := reader.List(ctx, &jobs, client.MatchingLabels{buildLakeLabel: lake.Name}); err != nil {
		return 0, 0, err
	}
	var perLake, perSpace int32
	spaceHash := buildHash(space)
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if !metav1.IsControlledBy(job, lake) || !job.DeletionTimestamp.IsZero() ||
			job.Status.Succeeded > 0 || job.Status.Failed > 0 {
			continue
		}
		perLake++
		if job.Labels[buildSpaceLabel] == spaceHash {
			perSpace++
		}
	}
	return perLake, perSpace, nil
}

func validateBuild(build *LakeBuild) error {
	if build.ID == "" || build.Repository == "" || build.Revision == "" {
		return fmt.Errorf("id, repository, and revision are required")
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(build.BuildArgs), &args); err != nil {
		return fmt.Errorf("build_args_json must be an object of string values: %w", err)
	}
	return nil
}

func (r *LakeReconciler) reconcileLakeBuild(ctx context.Context, lake *oramav1alpha1.Lake, build *LakeBuild) (LakeBuildStatus, bool, bool, error) {
	status := LakeBuildStatus{BuildID: build.ID, Phase: BuildPhaseQueued, TokenHashes: canonicalJSON(build.TokenHashes)}
	if status.TokenHashes == "" {
		status.TokenHashes = "{}"
	}
	if err := validateBuild(build); err != nil {
		status.Phase, status.Message, status.TokenHashes = BuildPhaseFailed, err.Error(), "{}"
		return status, false, true, nil
	}
	target, err := parseBuildTarget(build.Target)
	if err != nil {
		status.Phase, status.Message, status.TokenHashes = BuildPhaseFailed, err.Error(), "{}"
		return status, false, true, nil
	}
	if lake.Spec.Builds.BuilderImage == "" {
		status.Phase, status.Message, status.TokenHashes = BuildPhaseFailed, "builds are enabled but spec.builds.builderImage is empty", "{}"
		return status, false, true, nil
	}
	namespace, err := r.reconcileBuildNamespace(ctx, lake, target)
	if err != nil {
		return status, true, false, err
	}
	labels := buildLabels(lake, build, target)
	if err := r.reconcileBuildNetworkPolicies(ctx, lake, namespace, target.Space); err != nil {
		return status, true, false, err
	}
	job := &batchv1.Job{}
	jobKey := client.ObjectKey{Namespace: namespace, Name: buildJobName(build.ID)}
	err = r.Get(ctx, jobKey, job)
	if err == nil {
		if job.Labels[buildIDLabel] != build.ID || !metav1.IsControlledBy(job, lake) {
			return status, false, false, fmt.Errorf("Job %s exists and is not this build", jobKey)
		}
		return r.observeBuildJob(ctx, lake, namespace, build, job)
	}
	if !apierrors.IsNotFound(err) {
		return status, true, false, err
	}
	if build.Phase == BuildPhaseRunning {
		status.Phase, status.Message, status.TokenHashes = BuildPhaseFailed, "build Job disappeared before its outcome was recorded", "{}"
		return status, false, true, nil
	}
	secret := &corev1.Secret{}
	secretKey := client.ObjectKey{Namespace: namespace, Name: buildTokenSecretName(build.ID)}
	if err := r.Get(ctx, secretKey, secret); err == nil {
		if secret.Labels[buildIDLabel] != build.ID || !metav1.IsControlledBy(secret, lake) {
			return status, false, false, fmt.Errorf("Secret %s exists and is not this build's credential", secretKey)
		}
		status.Phase, status.Message, status.TokenHashes = BuildPhaseFailed, "build Job disappeared before its outcome was recorded", "{}"
		return status, false, true, nil
	} else if !apierrors.IsNotFound(err) {
		return status, true, false, err
	}
	perLake, perSpace, err := r.activeBuildCounts(ctx, lake, target.Space)
	if err != nil {
		return status, true, false, err
	}
	lakeLimit, spaceLimit := buildConcurrency(lake)
	if perLake >= lakeLimit {
		status.Message = fmt.Sprintf("waiting for Lake build concurrency (%d)", lakeLimit)
		return status, true, false, nil
	}
	if perSpace >= spaceLimit {
		status.Message = fmt.Sprintf("waiting for space build concurrency (%d)", spaceLimit)
		return status, true, false, nil
	}
	hashes, err := r.reconcileBuildTokens(ctx, lake, namespace, build, labels)
	if err != nil {
		return status, true, false, err
	}
	status.TokenHashes = hashes
	if err := r.createBuildJob(ctx, lake, namespace, build, target, labels); err != nil {
		return status, true, false, err
	}
	status.Message = "build Job created"
	return status, true, false, nil
}

func (r *LakeReconciler) observeBuildJob(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, build *LakeBuild, job *batchv1.Job) (LakeBuildStatus, bool, bool, error) {
	status := LakeBuildStatus{BuildID: build.ID, Phase: BuildPhaseQueued, Message: "waiting for builder pod", TokenHashes: canonicalJSON(build.TokenHashes)}
	if job.Spec.Suspend != nil && *job.Spec.Suspend {
		target, err := parseBuildTarget(build.Target)
		if err != nil {
			return status, false, false, err
		}
		labels := buildLabels(lake, build, target)
		hashes, err := r.reconcileBuildTokens(ctx, lake, namespace, build, labels)
		if err != nil {
			return status, true, false, err
		}
		status.TokenHashes = hashes
		if canonicalJSON(build.TokenHashes) != hashes {
			status.Message = "build credentials ready"
			return status, true, false, nil
		}
		released := false
		job.Spec.Suspend = &released
		if err := r.Update(ctx, job); err != nil {
			return status, true, false, err
		}
		status.Message = "build Job released"
		return status, true, false, nil
	}
	r.publishBuildPodEvents(ctx, lake, namespace, build, job)
	switch {
	case job.Status.Succeeded > 0:
		logs, err := r.buildLogs(ctx, namespace, job.Name)
		if err != nil {
			status.Phase, status.Message = BuildPhaseRunning, "collecting the build result: "+err.Error()
			return status, true, false, nil
		}
		result, err := parseBuildResult(logs)
		if err != nil {
			status.Phase, status.Message = BuildPhaseRunning, "collecting the build result: "+err.Error()
			return status, true, false, nil
		}
		status.Phase, status.Message = BuildPhaseSucceeded, "image pushed"
		status.Digest, status.Size, status.TokenHashes = result.Digest, result.Size, "{}"
		return status, false, true, nil
	case job.Status.Failed > 0:
		status.Phase, status.Message, status.TokenHashes = BuildPhaseFailed, r.buildFailureMessage(ctx, namespace, job), "{}"
		return status, false, true, nil
	case job.Status.Active > 0:
		status.Phase, status.Message = BuildPhaseRunning, "builder running"
		return status, true, false, nil
	default:
		return status, true, false, nil
	}
}

func parseBuildResult(logs string) (buildResult, error) {
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		var result buildResult
		if err := json.Unmarshal([]byte(line), &result); err != nil {
			return buildResult{}, fmt.Errorf("final stdout line is not result JSON")
		}
		if !buildDigestPattern.MatchString(result.Digest) || result.Size < 0 {
			return buildResult{}, fmt.Errorf("final stdout line has an invalid digest or size")
		}
		return result, nil
	}
	return buildResult{}, fmt.Errorf("builder stdout is empty")
}

func (r *LakeReconciler) buildLogs(ctx context.Context, namespace, jobName string) (string, error) {
	if r.BuildLogs != nil {
		return r.BuildLogs(ctx, namespace, jobName)
	}
	if r.Clientset == nil {
		return "", fmt.Errorf("pod log client is unavailable")
	}
	pods, err := r.Clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + jobName})
	if err != nil {
		return "", err
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("builder pod is not readable yet")
	}
	sort.Slice(pods.Items, func(i, j int) bool {
		return pods.Items[i].CreationTimestamp.After(pods.Items[j].CreationTimestamp.Time)
	})
	tail := int64(20)
	raw, err := r.Clientset.CoreV1().Pods(namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{TailLines: &tail}).DoRaw(ctx)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (r *LakeReconciler) buildFailureMessage(ctx context.Context, namespace string, job *batchv1.Job) string {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed {
			if condition.Message != "" {
				return condition.Message
			}
			if condition.Reason != "" {
				return condition.Reason
			}
		}
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"job-name": job.Name}); err == nil {
		for _, pod := range pods.Items {
			for _, container := range pod.Status.ContainerStatuses {
				if terminated := container.State.Terminated; terminated != nil {
					if terminated.Message != "" {
						return terminated.Message
					}
					if terminated.Reason != "" {
						return terminated.Reason
					}
				}
			}
		}
	}
	return "build Job failed"
}

func (r *LakeReconciler) cancelLakeBuild(ctx context.Context, lake *oramav1alpha1.Lake, build *LakeBuild) (LakeBuildStatus, bool, error) {
	status := LakeBuildStatus{BuildID: build.ID, Phase: BuildPhaseCancelled, Message: "cancelled", TokenHashes: "{}"}
	target, err := parseBuildTarget(build.Target)
	if err != nil {
		return status, false, nil
	}
	namespace := buildNamespace(lake.Name, target.Space)
	objects := []client.Object{
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: buildJobName(build.ID), Namespace: namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: buildTokenSecretName(build.ID), Namespace: namespace}},
	}
	deleting := false
	foreground := metav1.DeletePropagationForeground
	for _, object := range objects {
		if err := r.Get(ctx, client.ObjectKeyFromObject(object), object); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return status, true, err
		}
		if object.GetLabels()[buildIDLabel] != build.ID || !metav1.IsControlledBy(object, lake) {
			return status, false, fmt.Errorf("%s %s is not owned by this build", object.GetObjectKind().GroupVersionKind().Kind, client.ObjectKeyFromObject(object))
		}
		if object.GetDeletionTimestamp().IsZero() {
			if err := r.Delete(ctx, object, &client.DeleteOptions{PropagationPolicy: &foreground}); err != nil && !apierrors.IsNotFound(err) {
				return status, true, err
			}
		}
		deleting = true
	}
	return status, deleting, nil
}

func (r *LakeReconciler) cleanupBuildTokens(ctx context.Context, lake *oramav1alpha1.Lake, build *LakeBuild) error {
	target, err := parseBuildTarget(build.Target)
	if err != nil {
		return nil
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: buildTokenSecretName(build.ID), Namespace: buildNamespace(lake.Name, target.Space),
	}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(secret), secret); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if secret.Labels[buildIDLabel] != build.ID || !metav1.IsControlledBy(secret, lake) {
		return fmt.Errorf("Secret %s is not owned by this build", client.ObjectKeyFromObject(secret))
	}
	if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// reconcileLakeBuilds applies one ordered read of the builds table and reports whether any build
// still needs close polling.
func (r *LakeReconciler) reconcileLakeBuilds(ctx context.Context, lake *oramav1alpha1.Lake, store LakeServiceStore, builds []LakeBuild) bool {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	changing := false
	for i := range builds {
		build := &builds[i]
		var status LakeBuildStatus
		var still, cleanup bool
		var err error
		switch build.Desired {
		case buildDesiredQueued:
			status, still, cleanup, err = r.reconcileLakeBuild(ctx, lake, build)
		case buildDesiredCancelled:
			status, still, err = r.cancelLakeBuild(ctx, lake, build)
		default:
			status = LakeBuildStatus{BuildID: build.ID, Phase: BuildPhaseFailed, Message: "desired must be queued or cancelled", TokenHashes: "{}"}
			cleanup = true
		}
		changing = changing || still
		if err != nil {
			log.Error(err, "build deferred", "build", build.ID)
			changing = true
			continue
		}
		if cleanup {
			if err := r.cleanupBuildTokens(ctx, lake, build); err != nil {
				log.Error(err, "build token cleanup deferred", "build", build.ID)
				changing = true
				continue
			}
		}
		if status.equalBuild(build) {
			continue
		}
		if err := store.UpdateBuildStatus(ctx, status); err != nil {
			log.Error(err, "build status write deferred", "build", build.ID)
			changing = true
			continue
		}
		build.Phase, build.Digest, build.Size = status.Phase, status.Digest, status.Size
		build.Message, build.TokenHashes = status.Message, status.TokenHashes
	}
	return changing
}

func (r *LakeReconciler) publishBuildPodEvents(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, build *LakeBuild, job *batchv1.Job) {
	events := r.buildPodEvents(ctx, namespace, build.ID, job)
	if len(events) == 0 {
		return
	}
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	pending := make([]buildEvent, 0, len(events))
	for key, event := range events {
		if job.Annotations[buildEventAnnotation+key] == "true" {
			continue
		}
		job.Annotations[buildEventAnnotation+key] = "true"
		pending = append(pending, event)
	}
	if len(pending) == 0 {
		return
	}
	if err := r.Update(ctx, job); err != nil {
		return
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: buildTokenSecretName(build.ID)}, secret); err != nil {
		return
	}
	token := string(secret.Data[buildTokenEventsKey])
	for _, event := range pending {
		r.postBuildEvent(ctx, lake, token, event)
	}
}

func (r *LakeReconciler) buildPodEvents(ctx context.Context, namespace, buildID string, job *batchv1.Job) map[string]buildEvent {
	events := map[string]buildEvent{}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"job-name": job.Name}); err == nil {
		for _, pod := range pods.Items {
			for _, condition := range pod.Status.Conditions {
				if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionTrue {
					events["scheduled"] = buildEvent{BuildID: buildID, Type: "phase", Phase: "queued", Message: "builder pod scheduled"}
				}
			}
			for _, container := range pod.Status.ContainerStatuses {
				if terminated := container.State.Terminated; terminated != nil && terminated.Reason == "OOMKilled" {
					events["oom-killed"] = buildEvent{BuildID: buildID, Type: "error", Message: "builder pod was OOMKilled"}
				}
			}
		}
	}
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Reason == "DeadlineExceeded" {
			events["deadline"] = buildEvent{BuildID: buildID, Type: "error", Message: "build deadline exceeded"}
		}
	}
	return events
}

func (r *LakeReconciler) postBuildEvent(ctx context.Context, lake *oramav1alpha1.Lake, token string, event buildEvent) {
	if token == "" {
		return
	}
	body, err := json.Marshal(event)
	if err != nil {
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost,
		lakeInternalURL(lake, lakeNamespace(lake))+"/v1/publish_build_event", bytes.NewReader(body))
	if err != nil {
		return
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	httpClient := r.BuildEventClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 3 * time.Second}
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
}

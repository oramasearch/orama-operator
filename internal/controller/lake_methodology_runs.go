/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
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
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Runs of a workspace's own methodology (lake#655, orama-operator#291; lake migration
// 0078_methodology_runs; this repo's docs/lake/methodology-runs.md). A workspace methodology is
// Python code in one of the workspace's repositories, so it runs as untrusted code. Lake writes one
// methodology_runs row per run; the operator waits for the methodology's image build, then runs
// three pods, one after another, in the Lake's methodology-runs namespace:
//
//   - stage: the pristine runtime image with the run's credential, lmr.<run id>.<secret>, egress
//     only to lake's pods and DNS. It pages the data in from lake onto the run's work volume.
//   - run: the built image, the code that is not trusted. No credential, no service account
//     token, no service links, no network at all unless the person granted it (then the public
//     internet only), non-root with a read-only root filesystem, requests equal to limits, a
//     deadline.
//   - collect: the runtime image with the credential again, egress to lake only. It posts the
//     result and the artifact.
//
// NetworkPolicy is per pod, so the code that is not trusted never shares a network namespace with
// a credential: that is why these are three pods sharing one volume, not three containers of one.
// The operator generates the credential and writes only its SHA-256 into methodology_run_status;
// it also pulls the built image from lake's registry (Basic user methodology-run).
const (
	MethodologyPhaseBuilding   = "Building"
	MethodologyPhaseQueued     = "Queued"
	MethodologyPhaseStaging    = "Staging"
	MethodologyPhaseRunning    = "Running"
	MethodologyPhaseCollecting = "Collecting"
	MethodologyPhaseSucceeded  = "Succeeded"
	MethodologyPhaseFailed     = "Failed"
	MethodologyPhaseStopped    = "Stopped"

	methodologyRunIDLabel      = "orama.cloud/methodology-run-id"
	methodologyRunLakeLabel    = "orama.cloud/methodology-run-lake"
	methodologyRunRoleLabel    = "orama.cloud/methodology-run-role"
	methodologyRunNetworkLabel = "orama.cloud/methodology-run-network"
	methodologyOwnerAnnotation = "orama.cloud/methodology-runs-owner"

	methodologyRoleStage   = "stage"
	methodologyRoleSandbox = "sandbox"
	methodologyRoleCollect = "collect"

	methodologyNetworkGranted = "granted"
	methodologyNetworkDenied  = "denied"

	methodologyModeValidate = "validate"
	methodologyModeTrain    = "train"

	methodologyDefaultDeny     = "methodology-default-deny"
	methodologyTransferPolicy  = "methodology-transfer"
	methodologySandboxInternet = "methodology-sandbox-internet"

	methodologyCLI      = "/opt/orama/bin/methodology"
	methodologyWorkPath = "/work"
	// methodologySafeWorkingDir is where a pod that holds the run credential starts: its own
	// emptyDir, never the work volume the sandbox can write.
	methodologySafeWorkingDir   = "/tmp"
	methodologyCredentialDir    = "/var/run/orama"
	methodologyCredentialFile   = "credential"
	methodologyPullUser         = "methodology-run"
	methodologyRunAsUser        = int64(10001)
	methodologyTransferDeadline = int64(1800)
	methodologyJobTTLSeconds    = int32(3600)
	methodologyDefaultWorkSize  = "10Gi"
	methodologyLakePublicHost   = "LAKE_PUBLIC_HOST"
	// methodologyDefaultConcurrentRuns is how many of one workspace's runs may be staging,
	// running or collecting at once when spec.methodologies.maxConcurrentRunsPerWorkspace is unset.
	methodologyDefaultConcurrentRuns = int32(2)
)

var (
	methodologyRunSlugPattern = regexp.MustCompile(`^mr-[0-9a-f]{12}$`)
	methodologyImagePattern   = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

	// errMethodologyRunsTableMissing: lake's schema has not reached 0078_methodology_runs yet.
	errMethodologyRunsTableMissing = errors.New("the methodology_runs table does not exist yet (lake schema before 0078_methodology_runs)")
)

// LakeMethodologyRun is one methodology_runs row joined to its image build and its status row.
type LakeMethodologyRun struct {
	ID             string
	CustomerID     string
	WorkspaceID    string
	MethodologyID  string
	Mode           string
	BuildID        string
	Image          string
	RuntimeImage   string
	CPUMillis      int32
	MemoryBytes    int64
	GPUs           int32
	GPUClass       string
	TimeoutSeconds int32
	Network        bool
	Slug           string
	Desired        string
	Generation     int64
	// Build is the joined builds row; nil when the run names no build or the row is gone.
	Build  *LakeMethodologyRunBuild
	Status *LakeMethodologyRunStatus
}

// LakeMethodologyRunBuild is what a run needs of its image build.
type LakeMethodologyRunBuild struct {
	Desired string
	Phase   string
	Digest  string
	Target  string
	Message string
}

// LakeMethodologyRunStatus is one methodology_run_status row, written only by the operator.
// StartedAt and FinishedAt are the sandbox pod's: what lake charges the run's resources for.
type LakeMethodologyRunStatus struct {
	RunID              string
	Phase              string
	Message            string
	ExitCode           *int32
	CredentialSHA256   string
	ObservedGeneration int64
	StartedAt          *time.Time
	FinishedAt         *time.Time
}

func sameTime(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}

func (s LakeMethodologyRunStatus) equal(other LakeMethodologyRunStatus) bool {
	sameExit := (s.ExitCode == nil && other.ExitCode == nil) ||
		(s.ExitCode != nil && other.ExitCode != nil && *s.ExitCode == *other.ExitCode)
	return s.RunID == other.RunID && s.Phase == other.Phase && s.Message == other.Message && sameExit &&
		s.CredentialSHA256 == other.CredentialSHA256 && s.ObservedGeneration == other.ObservedGeneration &&
		sameTime(s.StartedAt, other.StartedAt) && sameTime(s.FinishedAt, other.FinishedAt)
}

func methodologyTerminal(phase string) bool {
	return phase == MethodologyPhaseSucceeded || phase == MethodologyPhaseFailed || phase == MethodologyPhaseStopped
}

// methodologyRunsNamespace is the one namespace of a Lake's methodology runs. It is not a
// workspace namespace: those carry allow policies selecting every pod (lake, DNS, the internet,
// the workspace's own services), and NetworkPolicies only ever add what they allow.
func methodologyRunsNamespace(lake string) string {
	const prefix, suffix = "lake-", "-methodology-runs"
	maxLake := 63 - len(prefix) - len(suffix)
	if len(lake) > maxLake {
		lake = strings.TrimRight(lake[:maxLake], "-")
	}
	return prefix + lake + suffix
}

func methodologyStageJob(slug string) string         { return slug + "-stage" }
func methodologySandboxJob(slug string) string       { return slug + "-run" }
func methodologyCollectJob(slug string) string       { return slug + "-collect" }
func methodologyWorkClaim(slug string) string        { return slug + "-work" }
func methodologyCredentialSecret(slug string) string { return slug + "-credential" }
func methodologyPullSecret(slug string) string       { return slug + "-pull" }

// lakePublicHost is the host lake's registry is reached at from outside the cluster, which is
// where kubelet pulls a built image from: LAKE_PUBLIC_HOST in spec.env (what lake's build_json
// uses), else spec.host.
func lakePublicHost(lake *oramav1alpha1.Lake) string {
	for _, variable := range lake.Spec.Env {
		if variable.Name == methodologyLakePublicHost && variable.Value != "" {
			return variable.Value
		}
	}
	return lake.Spec.Host
}

// methodologyRunImage resolves a succeeded build to its pull reference the way lake's build_json
// does: <public host>/<space>/<name>@<digest>.
func methodologyRunImage(lake *oramav1alpha1.Lake, build *LakeMethodologyRunBuild) (string, error) {
	target, err := parseBuildTarget(build.Target)
	if err != nil {
		return "", fmt.Errorf("the build's target %q: %w", build.Target, err)
	}
	if !buildDigestPattern.MatchString(build.Digest) {
		return "", fmt.Errorf("the build reported no image digest")
	}
	host := lakePublicHost(lake)
	if host == "" {
		return "", fmt.Errorf("the Lake has no public host (spec.host or LAKE_PUBLIC_HOST) to pull the built image from")
	}
	return host + "/" + target.Space + "/" + target.Name + "@" + build.Digest, nil
}

func methodologyLabels(lake *oramav1alpha1.Lake, run *LakeMethodologyRun, role string) map[string]string {
	labels := map[string]string{
		"app.kubernetes.io/name":       "methodology-run",
		"app.kubernetes.io/instance":   run.Slug,
		"app.kubernetes.io/managed-by": "orama-operator",
		methodologyRunIDLabel:          run.ID,
		methodologyRunLakeLabel:        lake.Name,
	}
	if role != "" {
		labels[methodologyRunRoleLabel] = role
	}
	if role == methodologyRoleSandbox {
		labels[methodologyRunNetworkLabel] = methodologyNetworkDenied
		if run.Network {
			labels[methodologyRunNetworkLabel] = methodologyNetworkGranted
		}
	}
	return labels
}

func methodologyNamespaceLabels(lake *oramav1alpha1.Lake) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":                     "lake-methodology-runs",
		"app.kubernetes.io/managed-by":               "orama-operator",
		methodologyRunLakeLabel:                      lake.Name,
		"pod-security.kubernetes.io/enforce":         "restricted",
		"pod-security.kubernetes.io/enforce-version": "latest",
		"pod-security.kubernetes.io/audit":           "restricted",
		"pod-security.kubernetes.io/audit-version":   "latest",
		"pod-security.kubernetes.io/warn":            "restricted",
		"pod-security.kubernetes.io/warn-version":    "latest",
	}
}

// reconcileMethodologyNamespace creates the Lake's methodology-runs namespace (Pod Security
// restricted) and its four policies: deny everything to every pod, let the stage and collect
// pods reach DNS and lake's pods, let a sandbox (or a served model's serve pod) granted network
// reach DNS and the public internet, and let lake's pods, and nothing else, reach a serve pod's
// port (lake#690).
func (r *LakeReconciler) reconcileMethodologyNamespace(ctx context.Context, lake *oramav1alpha1.Lake) (string, error) {
	name := methodologyRunsNamespace(lake.Name)
	desired := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: methodologyNamespaceLabels(lake),
		Annotations: map[string]string{methodologyOwnerAnnotation: lake.Name}}}
	if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create methodology-runs namespace %s: %w", name, err)
	} else if err != nil {
		existing := &corev1.Namespace{}
		if err := r.reader().Get(ctx, client.ObjectKey{Name: name}, existing); err != nil {
			return "", fmt.Errorf("read methodology-runs namespace %s: %w", name, err)
		}
		if existing.Annotations[methodologyOwnerAnnotation] != lake.Name {
			return "", fmt.Errorf("namespace %s exists and is not this Lake's methodology-runs namespace", name)
		}
		if mergeStringMap(&existing.Labels, methodologyNamespaceLabels(lake)) {
			if err := r.Update(ctx, existing); err != nil {
				return "", fmt.Errorf("update methodology-runs namespace %s: %w", name, err)
			}
		}
	}
	labels := map[string]string{
		"app.kubernetes.io/name":       "lake-methodology-runs",
		"app.kubernetes.io/managed-by": "orama-operator",
		methodologyRunLakeLabel:        lake.Name,
	}
	policies := []*networkingv1.NetworkPolicy{
		methodologyDenyPolicy(name),
		methodologyTransferNetworkPolicy(lake, name),
		methodologySandboxInternetPolicy(lake, name),
		methodologyServeIngressPolicy(lake, name),
	}
	for _, wanted := range policies {
		policy := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: wanted.Name, Namespace: name}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
			policy.Labels = labels
			policy.Spec = wanted.Spec
			return controllerutil.SetControllerReference(lake, policy, r.Scheme)
		}); err != nil {
			return "", fmt.Errorf("reconcile NetworkPolicy %s/%s: %w", name, wanted.Name, err)
		}
	}
	return name, nil
}

func methodologyDenyPolicy(namespace string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: methodologyDefaultDeny, Namespace: namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		},
	}
}

// methodologyDNSRule is egress to the cluster's DNS.
func methodologyDNSRule(lake *oramav1alpha1.Lake) networkingv1.NetworkPolicyEgressRule {
	return buildEgressRules(lake)[0]
}

// methodologyTransferPods selects a Lake's stage and collect pods, a run's or a served model's: the
// only pods in the methodology-runs namespace that hold a credential and talk to lake.
func methodologyTransferPods(lake *oramav1alpha1.Lake) *metav1.LabelSelector {
	return &metav1.LabelSelector{
		MatchLabels: map[string]string{methodologyRunLakeLabel: lake.Name},
		MatchExpressions: []metav1.LabelSelectorRequirement{{Key: methodologyRunRoleLabel,
			Operator: metav1.LabelSelectorOpIn, Values: []string{methodologyRoleStage, methodologyRoleCollect}}},
	}
}

// methodologyTransferNetworkPolicy is the stage and collect pods' egress. Lake's pods are isolated
// by the Lake's NetworkSandbox, so the destination half is its allow-methodology-ingress-to-lake
// (lake#802): both ends need a rule.
func methodologyTransferNetworkPolicy(lake *oramav1alpha1.Lake, namespace string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: methodologyTransferPolicy, Namespace: namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: *methodologyTransferPods(lake),
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			// DNS and lake's own pods, nothing else: buildEgressRules' first two rules.
			Egress: buildEgressRules(lake)[:2],
		},
	}
}

// methodologyPublicCIDRExcept is everything that is not the public internet: private, shared,
// loopback, link-local (cloud metadata) and reserved ranges, so a granted sandbox reaches neither
// the cluster, its nodes nor the metadata service.
var methodologyPublicCIDRExcept = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15",
	"198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
}

func methodologySandboxInternetPolicy(lake *oramav1alpha1.Lake, namespace string) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: methodologySandboxInternet, Namespace: namespace},
		Spec: networkingv1.NetworkPolicySpec{
			// A run's sandbox, or a served model's serve pod (lake#690), whose person granted it the
			// internet.
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					methodologyRunLakeLabel:    lake.Name,
					methodologyRunNetworkLabel: methodologyNetworkGranted,
				},
				MatchExpressions: []metav1.LabelSelectorRequirement{{Key: methodologyRunRoleLabel,
					Operator: metav1.LabelSelectorOpIn, Values: []string{methodologyRoleSandbox, methodologyRoleServe}}},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				methodologyDNSRule(lake),
				{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{
					CIDR: "0.0.0.0/0", Except: methodologyPublicCIDRExcept}}}},
			},
		},
	}
}

// methodologyRunResources: requests equal limits, from the row.
func methodologyRunResources(run *LakeMethodologyRun) corev1.ResourceRequirements {
	list := corev1.ResourceList{
		corev1.ResourceCPU:    *resource.NewMilliQuantity(int64(run.CPUMillis), resource.DecimalSI),
		corev1.ResourceMemory: *resource.NewQuantity(run.MemoryBytes, resource.BinarySI),
	}
	if run.GPUs > 0 {
		list[resourceNvidiaGPU] = *resource.NewQuantity(int64(run.GPUs), resource.DecimalSI)
	}
	return corev1.ResourceRequirements{Requests: list, Limits: list.DeepCopy()}
}

// methodologyTransferResources are the stage and collect pods' bounded resources.
func methodologyTransferResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
	}
}

// methodologyPod is one of a run's pods: non-root (uid 10001, which owns the work volume through
// fsGroup), a read-only root filesystem with a writable /tmp, all capabilities dropped, the runtime
// default seccomp profile, no privilege escalation, no service account token, no service links.
func methodologyPod(image string, command []string, env []corev1.EnvVar, resources corev1.ResourceRequirements,
	claim, pullSecret, credentialSecret string) corev1.PodSpec {
	noToken, noLinks, nonRoot, noEscalation, readOnlyRoot := false, false, true, false, true
	user := methodologyRunAsUser
	tempSize := resource.MustParse("1Gi")
	mounts := []corev1.VolumeMount{{Name: "tmp", MountPath: "/tmp"}, {Name: "work", MountPath: methodologyWorkPath}}
	volumes := []corev1.Volume{
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tempSize}}},
		{Name: "work", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}},
	}
	if credentialSecret != "" {
		mode := int32(0o440)
		mounts = append(mounts, corev1.VolumeMount{Name: "credential", MountPath: methodologyCredentialDir, ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: "credential", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: credentialSecret, DefaultMode: &mode,
			Items: []corev1.KeyToPath{{Key: agentCredentialKey, Path: methodologyCredentialFile}},
		}}})
	}
	var pullSecrets []corev1.LocalObjectReference
	if pullSecret != "" {
		pullSecrets = []corev1.LocalObjectReference{{Name: pullSecret}}
	}
	workingDir := ""
	if credentialSecret != "" {
		// A pod that holds the credential (stage, collect, model stage) must never import code the
		// sandbox wrote. The runtime starts with `python3 -m`, which puts the working directory
		// first on the module path, and the image's working directory is /work, the volume the
		// sandbox writes: a planted /work/orama_methodology or /work/json.py would run with the
		// credential and the transfer pod's network (orama-operator#714). Start in this pod's own
		// /tmp and tell Python not to add the working directory at all.
		workingDir = methodologySafeWorkingDir
		env = append(append([]corev1.EnvVar(nil), env...), corev1.EnvVar{Name: "PYTHONSAFEPATH", Value: "1"})
	}
	return corev1.PodSpec{
		RestartPolicy:                corev1.RestartPolicyNever,
		ImagePullSecrets:             pullSecrets,
		AutomountServiceAccountToken: &noToken,
		EnableServiceLinks:           &noLinks,
		DNSConfig:                    lakePodDNSConfig(),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   &nonRoot,
			RunAsUser:      &user,
			RunAsGroup:     &user,
			FSGroup:        &user,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{{
			Name:            "methodology",
			Image:           image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         command,
			Env:             env,
			WorkingDir:      workingDir,
			Resources:       resources,
			VolumeMounts:    mounts,
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: &noEscalation,
				ReadOnlyRootFilesystem:   &readOnlyRoot,
				RunAsNonRoot:             &nonRoot,
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		}},
		Volumes: volumes,
	}
}

// methodologyTransferEnv is what the stage and collect pods are told: lake's in-cluster address
// and the run. The credential is a file, never an environment value.
func methodologyTransferEnv(lake *oramav1alpha1.Lake, run *LakeMethodologyRun) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "LAKE_URL", Value: lakeInternalURL(lake, lakeNamespace(lake))},
		{Name: "RUN_ID", Value: run.ID},
		{Name: "HOME", Value: "/tmp"},
	}
}

// methodologyStagePod is the run's first pod, so the work volume's first consumer: with a
// WaitForFirstConsumer storage class the volume is created in the zone the stage pod lands in. For
// a GPU run, zones are the zones where nodes of its GPU class are (methodologyRunPlacement), so its
// sandbox can be scheduled next to its data; empty leaves the stage pod free to go anywhere.
func methodologyStagePod(lake *oramav1alpha1.Lake, run *LakeMethodologyRun, zones []string) corev1.PodSpec {
	pod := methodologyPod(run.RuntimeImage,
		[]string{methodologyCLI, "stage", "--lake", "$(LAKE_URL)", "--run", "$(RUN_ID)",
			"--credential-file", methodologyCredentialDir + "/" + methodologyCredentialFile, "--out", methodologyWorkPath + "/in"},
		methodologyTransferEnv(lake, run), methodologyTransferResources(),
		methodologyWorkClaim(run.Slug), methodologyPullSecret(run.Slug), methodologyCredentialSecret(run.Slug))
	if len(zones) > 0 {
		pod.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: corev1.LabelTopologyZone, Operator: corev1.NodeSelectorOpIn, Values: zones,
				}}}},
			},
		}}
	}
	return pod
}

// methodologyPlacement is where a run's work volume may go: the zones its stage pod is pinned to
// (none: anywhere), or why the run must wait.
type methodologyPlacement struct {
	Zones []string
	Wait  string
}

// methodologyRunPlacement decides where a run's work volume goes (orama-operator#293). A run
// without GPUs goes anywhere. A GPU run needs nodes that can run its sandbox: nodes labelled with
// its GPU class (the sandbox's nodeSelector), or, without a class, nodes that offer GPUs. With none
// it waits, and says so. Otherwise its volume goes in one of their zones
// (topology.kubernetes.io/zone); nodes without a zone label add no zone, and when none has one (a
// cluster without zones) the volume is not constrained. Unschedulable nodes do not count.
func methodologyRunPlacement(nodes []corev1.Node, run *LakeMethodologyRun) methodologyPlacement {
	if run.GPUs <= 0 {
		return methodologyPlacement{}
	}
	class := validGPUClass(run.GPUClass)
	zones := map[string]bool{}
	matched := 0
	for i := range nodes {
		node := &nodes[i]
		if node.Spec.Unschedulable {
			continue
		}
		if class != "" {
			if validGPUClass(node.Labels[gpuClassLabel]) != class {
				continue
			}
		} else {
			gpus := node.Status.Allocatable[resourceNvidiaGPU]
			if gpus.IsZero() {
				gpus = node.Status.Capacity[resourceNvidiaGPU]
			}
			if gpus.IsZero() {
				continue
			}
		}
		matched++
		if zone := node.Labels[corev1.LabelTopologyZone]; zone != "" {
			zones[zone] = true
		}
	}
	if matched == 0 {
		if class != "" {
			return methodologyPlacement{Wait: fmt.Sprintf("Queued: no node of GPU class %s is in the cluster yet; the run starts when one is.", class)}
		}
		return methodologyPlacement{Wait: "Queued: no node with GPUs is in the cluster yet; the run starts when one is."}
	}
	placement := methodologyPlacement{}
	for zone := range zones {
		placement.Zones = append(placement.Zones, zone)
	}
	sort.Strings(placement.Zones)
	return placement
}

// methodologyActive is a phase in which a run holds one of its workspace's places.
func methodologyActive(phase string) bool {
	return phase == MethodologyPhaseStaging || phase == MethodologyPhaseRunning || phase == MethodologyPhaseCollecting
}

// methodologyConcurrentRuns is the Lake's cap on one workspace's active runs.
func methodologyConcurrentRuns(lake *oramav1alpha1.Lake) int32 {
	if limit := lake.Spec.Methodologies.MaxConcurrentRunsPerWorkspace; limit > 0 {
		return limit
	}
	return methodologyDefaultConcurrentRuns
}

// methodologyQueue admits a workspace's runs up to the Lake's cap, oldest first (orama-operator#293).
// Runs already staging, running or collecting hold their places; one pass admits the others in
// the order lake created them, while places are free.
type methodologyQueue struct {
	limit   int32
	active  map[string]int32
	waiting map[string]int32
}

func newMethodologyQueue(lake *oramav1alpha1.Lake, runs []LakeMethodologyRun) *methodologyQueue {
	queue := &methodologyQueue{limit: methodologyConcurrentRuns(lake), active: map[string]int32{}, waiting: map[string]int32{}}
	for i := range runs {
		if runs[i].Desired == serviceDesiredPresent && runs[i].Status != nil && methodologyActive(runs[i].Status.Phase) {
			queue.active[runs[i].WorkspaceID]++
		}
	}
	return queue
}

// admit reports whether run may start staging now, taking a place when it does; otherwise the
// message saying why it waits and where it is in line.
func (q *methodologyQueue) admit(run *LakeMethodologyRun) (bool, string) {
	if run.Status != nil && methodologyActive(run.Status.Phase) {
		return true, ""
	}
	if q.active[run.WorkspaceID] < q.limit {
		q.active[run.WorkspaceID]++
		return true, ""
	}
	q.waiting[run.WorkspaceID]++
	ahead := q.waiting[run.WorkspaceID] - 1
	place := "this run is next"
	if ahead == 1 {
		place = "1 run is ahead of it"
	} else if ahead > 1 {
		place = fmt.Sprintf("%d runs are ahead of it", ahead)
	}
	return false, fmt.Sprintf("Queued: %d of this workspace's methodology runs are running (the limit is %d); %s.",
		q.active[run.WorkspaceID], q.limit, place)
}

// methodologyCollectPod reports the result; exitCode is the sandbox's, passed as --run-exit-code
// when known, so the runtime reports a failed run as failed.
func methodologyCollectPod(lake *oramav1alpha1.Lake, run *LakeMethodologyRun, exitCode *int32) corev1.PodSpec {
	command := []string{methodologyCLI, "collect", "--lake", "$(LAKE_URL)", "--run", "$(RUN_ID)",
		"--credential-file", methodologyCredentialDir + "/" + methodologyCredentialFile, "--in", methodologyWorkPath + "/out"}
	if exitCode != nil {
		command = append(command, "--run-exit-code", fmt.Sprint(*exitCode))
	}
	return methodologyPod(run.RuntimeImage, command,
		methodologyTransferEnv(lake, run), methodologyTransferResources(),
		methodologyWorkClaim(run.Slug), methodologyPullSecret(run.Slug), methodologyCredentialSecret(run.Slug))
}

// methodologySandboxPod runs the code that is not trusted. It names no Secret volume and no Secret
// environment, so nothing in it can read the run's credential; only kubelet uses the pull Secret.
func methodologySandboxPod(run *LakeMethodologyRun, image string) corev1.PodSpec {
	pod := methodologyPod(image,
		[]string{methodologyCLI, "run", "--in", methodologyWorkPath + "/in", "--out", methodologyWorkPath + "/out", "--source", "/methodology"},
		[]corev1.EnvVar{{Name: "HOME", Value: "/tmp"}},
		methodologyRunResources(run), methodologyWorkClaim(run.Slug), methodologyPullSecret(run.Slug), "")
	if run.GPUs > 0 && validGPUClass(run.GPUClass) != "" {
		pod.NodeSelector = map[string]string{gpuClassLabel: validGPUClass(run.GPUClass)}
	}
	return pod
}

func (r *LakeReconciler) reconcileMethodologyPullSecret(ctx context.Context, lake *oramav1alpha1.Lake, namespace string,
	run *LakeMethodologyRun, credential string, labels map[string]string) error {
	return r.reconcileMethodologyPullSecretFor(ctx, lake, namespace, run.Slug, methodologyPullUser, credential, labels)
}

// reconcileMethodologyPullSecretFor writes `<slug>-pull`: the Lake's pull Secret entries plus lake's
// public host with Basic `user` and the credential (methodology-run for a run,
// methodology-deployment for a served model).
func (r *LakeReconciler) reconcileMethodologyPullSecretFor(ctx context.Context, lake *oramav1alpha1.Lake, namespace,
	slug, user, credential string, labels map[string]string) error {
	var source *corev1.Secret
	if lake.Spec.PullSecretName != "" {
		source = &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: lakeNamespace(lake), Name: lake.Spec.PullSecretName}, source); err != nil {
			return fmt.Errorf("read the Lake's pull Secret: %w", err)
		}
	}
	config, ok := mergeRegistryDockerConfig(source, lakePublicHost(lake), user, []byte(credential))
	if !ok {
		return fmt.Errorf("the Lake's pull Secret %q has invalid dockerconfigjson data", lake.Spec.PullSecretName)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: methodologyPullSecret(slug), Namespace: namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = labels
		secret.Type = corev1.SecretTypeDockerConfigJson
		secret.Data = map[string][]byte{corev1.DockerConfigJsonKey: config}
		return controllerutil.SetControllerReference(lake, secret, r.Scheme)
	})
	return err
}

func (r *LakeReconciler) reconcileMethodologyWorkClaim(ctx context.Context, lake *oramav1alpha1.Lake, namespace string,
	run *LakeMethodologyRun, labels map[string]string) error {
	return r.reconcileMethodologyClaim(ctx, lake, namespace, methodologyWorkClaim(run.Slug), labels)
}

// reconcileMethodologyClaim creates a run's work volume or a served model's model volume
// (spec.methodologies.workVolumeSize and storageClassName), ReadWriteOnce.
func (r *LakeReconciler) reconcileMethodologyClaim(ctx context.Context, lake *oramav1alpha1.Lake, namespace, name string,
	labels map[string]string) error {
	claim := &corev1.PersistentVolumeClaim{}
	key := client.ObjectKey{Namespace: namespace, Name: name}
	if err := r.Get(ctx, key, claim); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	size := resource.MustParse(methodologyDefaultWorkSize)
	if lake.Spec.Methodologies.WorkVolumeSize != nil {
		size = *lake.Spec.Methodologies.WorkVolumeSize
	}
	claim = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: namespace, Labels: labels},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources:   corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: size}},
		},
	}
	if lake.Spec.Methodologies.StorageClassName != "" {
		class := lake.Spec.Methodologies.StorageClassName
		claim.Spec.StorageClassName = &class
	}
	if err := controllerutil.SetControllerReference(lake, claim, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, claim); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create work volume %s: %w", key.Name, err)
	}
	return nil
}

// ensureMethodologyJob returns the Job named `name`, creating it from pod when it does not exist.
func (r *LakeReconciler) ensureMethodologyJob(ctx context.Context, lake *oramav1alpha1.Lake, namespace, name string,
	labels map[string]string, pod corev1.PodSpec, deadline int64) (*batchv1.Job, bool, error) {
	job := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, job); err == nil {
		if job.Labels[methodologyRunIDLabel] != labels[methodologyRunIDLabel] || !metav1.IsControlledBy(job, lake) {
			return nil, false, fmt.Errorf("Job %s/%s exists and is not this run's", namespace, name)
		}
		return job, false, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, false, err
	}
	backoff, ttl := int32(0), methodologyJobTTLSeconds
	job = &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template:                corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: pod},
		},
	}
	if err := controllerutil.SetControllerReference(lake, job, r.Scheme); err != nil {
		return nil, false, err
	}
	if err := r.Create(ctx, job); err != nil {
		return nil, false, fmt.Errorf("create Job %s: %w", name, err)
	}
	return job, true, nil
}

func jobFailureMessage(job *batchv1.Job) string {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			if condition.Message != "" {
				return condition.Message
			}
			return condition.Reason
		}
	}
	return ""
}

// methodologyPodMessageLimit bounds what a failed pod's own words add to a status message.
const methodologyPodMessageLimit = 1024

// methodologyJobFailure is why a stage or collect Job failed, in the pod's own words when it left
// any (lake#802), else the Job's own condition ("Job has reached the specified backoff limit"). It
// is read before the run's objects are removed, since the pod goes with them.
func (r *LakeReconciler) methodologyJobFailure(ctx context.Context, job *batchv1.Job) string {
	if words := failedPodWords(ctx, r.reader(), job.Namespace, job.Name); words != "" {
		return words
	}
	return jobFailureMessage(job)
}

// failedPodWords is what a Job's failed pod said (lake#802): its container's termination message,
// which is the tail of its log when it wrote none (TerminationMessageFallbackToLogsOnError), such
// as "stage failed: methodology_run_input could not reach Lake: timed out"; else how it ended
// (OOMKilled, exited with N). Whitespace is collapsed and at most methodologyPodMessageLimit bytes
// of its end are kept. Empty when no pod of the Job failed or none can be read.
func failedPodWords(ctx context.Context, reader client.Reader, namespace, jobName string) string {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"job-name": jobName}); err != nil {
		return ""
	}
	for i := range pods.Items {
		for _, container := range pods.Items[i].Status.ContainerStatuses {
			terminated := container.State.Terminated
			if terminated == nil || terminated.ExitCode == 0 {
				continue
			}
			words := strings.TrimSpace(terminated.Message)
			if words == "" {
				words = terminated.Reason // OOMKilled, Error, ...
				if words == "" || words == "Error" {
					words = fmt.Sprintf("exited with %d", terminated.ExitCode)
				}
			}
			words = strings.Join(strings.Fields(words), " ")
			if len(words) > methodologyPodMessageLimit {
				// The end says why; cut at a byte boundary and drop a split character.
				words = "…" + strings.ToValidUTF8(words[len(words)-methodologyPodMessageLimit:], "")
			}
			return words
		}
	}
	return ""
}

func jobDeadlineExceeded(job *batchv1.Job) bool {
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue && condition.Reason == "DeadlineExceeded" {
			return true
		}
	}
	return false
}

// sandboxObservation is what the sandbox pod showed: when its code started and finished, and its
// exit code once it has one.
type sandboxObservation struct {
	startedAt  *time.Time
	finishedAt *time.Time
	exitCode   *int32
}

func (r *LakeReconciler) observeSandbox(ctx context.Context, namespace, job string) (sandboxObservation, error) {
	var observed sandboxObservation
	var pods corev1.PodList
	if err := r.reader().List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"job-name": job}); err != nil {
		return observed, err
	}
	for i := range pods.Items {
		for _, container := range pods.Items[i].Status.ContainerStatuses {
			if running := container.State.Running; running != nil && !running.StartedAt.IsZero() {
				at := running.StartedAt.UTC().Truncate(time.Second)
				observed.startedAt = &at
			}
			if terminated := container.State.Terminated; terminated != nil {
				code := terminated.ExitCode
				observed.exitCode = &code
				if !terminated.StartedAt.IsZero() {
					at := terminated.StartedAt.UTC().Truncate(time.Second)
					observed.startedAt = &at
				}
				if !terminated.FinishedAt.IsZero() {
					at := terminated.FinishedAt.UTC().Truncate(time.Second)
					observed.finishedAt = &at
				}
			}
		}
	}
	return observed, nil
}

func nowSecond() *time.Time {
	at := time.Now().UTC().Truncate(time.Second)
	return &at
}

// methodologyRunWorkload is the run's workload_resources row (pods 0): the CPU, memory, GPUs, GPU
// class and VRAM of the sandbox pod the operator schedules for it, from that pod's spec, so it does
// not depend on the pod still existing.
func methodologyRunWorkload(spec corev1.PodSpec, id string) WorkloadResources {
	pod := &corev1.Pod{Spec: spec}
	resources := podSpecResources(&pod.Spec, containerDefaults{})
	var gpu gpuInfo
	if resources.gpus > 0 {
		gpu = podGPUInfo(pod, nil)
	}
	totals := &workloadTotals{}
	totals.add(resources, gpu)
	return totals.row(workloadKey{WorkloadKindMethodologyRun, id}, 0)
}

// recordMethodologyResources writes the run's workload_resources row before its sandbox Job is
// created (lake#655, as lake#658 does for builds and agent jobs): lake charges a finished run's
// minutes at these numbers once, whether or not the operator ever saw its pod, so a run whose pod
// and Job are gone before a workload pass is still charged. A row that cannot be written yet holds
// the code back: no run starts unmetered. The workload pass replaces pods with the live count while
// the pod runs and keeps these numbers after it finishes.
func (r *LakeReconciler) recordMethodologyResources(ctx context.Context, store LakeServiceStore, run *LakeMethodologyRun, spec corev1.PodSpec) error {
	if err := store.UpsertWorkloadResources(ctx, methodologyRunWorkload(spec, run.ID)); err != nil {
		if errors.Is(err, errWorkloadResourcesTableMissing) {
			return nil // a schema before 0075 meters nothing at all
		}
		return fmt.Errorf("record the run's resources: %w", err)
	}
	return nil
}

// reconcileMethodologyRun advances one present run and reports what the operator observed, and
// whether it is still changing.
func (r *LakeReconciler) reconcileMethodologyRun(ctx context.Context, lake *oramav1alpha1.Lake, store LakeServiceStore,
	run *LakeMethodologyRun, namespace func() (string, error), queue *methodologyQueue,
	nodes func() ([]corev1.Node, error)) (LakeMethodologyRunStatus, bool, error) {
	status := LakeMethodologyRunStatus{RunID: run.ID, ObservedGeneration: run.Generation}
	prior := run.Status
	if prior != nil {
		status.StartedAt, status.FinishedAt, status.ExitCode = prior.StartedAt, prior.FinishedAt, prior.ExitCode
		if methodologyTerminal(prior.Phase) {
			// A finished run keeps its outcome; its objects are removed once it has reported.
			done := *prior
			done.CredentialSHA256 = ""
			done.ObservedGeneration = run.Generation
			if err := r.removeMethodologyRun(ctx, lake, run); err != nil {
				return done, true, err
			}
			return done, false, nil
		}
	}
	fail := func(message string) (LakeMethodologyRunStatus, bool, error) {
		status.Phase, status.Message, status.CredentialSHA256 = MethodologyPhaseFailed, message, ""
		if status.StartedAt != nil && status.FinishedAt == nil {
			status.FinishedAt = nowSecond()
		}
		return status, true, r.removeMethodologyRun(ctx, lake, run)
	}
	switch {
	case !methodologyRunSlugPattern.MatchString(run.Slug):
		status.Phase, status.Message = MethodologyPhaseFailed, "slug must match "+methodologyRunSlugPattern.String()
		return status, false, nil
	case run.Mode != methodologyModeValidate && run.Mode != methodologyModeTrain:
		status.Phase, status.Message = MethodologyPhaseFailed, "mode must be validate or train"
		return status, false, nil
	case run.RuntimeImage == "":
		status.Phase, status.Message = MethodologyPhaseFailed, "the run names no runtime image"
		return status, false, nil
	case run.CPUMillis <= 0 || run.MemoryBytes <= 0 || run.TimeoutSeconds <= 0 || run.GPUs < 0:
		status.Phase, status.Message = MethodologyPhaseFailed, "the run's CPU, memory and time limit must be positive"
		return status, false, nil
	}

	// The image: lake's, or the build's once it has succeeded.
	image := run.Image
	if image == "" {
		build := run.Build
		switch {
		case build == nil:
			return fail("the run names no image and no image build")
		case build.Desired == buildDesiredCancelled || build.Phase == BuildPhaseCancelled:
			return fail("the methodology's image build was cancelled")
		case build.Phase == BuildPhaseFailed:
			message := "the methodology's image build failed"
			if build.Message != "" {
				message += ": " + build.Message
			}
			return fail(message)
		case build.Phase != BuildPhaseSucceeded:
			status.Phase, status.Message = MethodologyPhaseBuilding, "waiting for the methodology's image build"
			return status, true, nil
		}
		resolved, err := methodologyRunImage(lake, build)
		if err != nil {
			return fail(err.Error())
		}
		image = resolved
	}
	if !methodologyImagePattern.MatchString(image) {
		return fail("the image must be pinned by digest (name@sha256:<64 hex>)")
	}

	// Where its volume goes, and whether it may start now: a GPU run waits for a node of its class,
	// and every run waits for a place among its workspace's runs (orama-operator#293).
	var placement methodologyPlacement
	if run.GPUs > 0 {
		listed, err := nodes()
		if err != nil {
			return status, true, err
		}
		placement = methodologyRunPlacement(listed, run)
	}
	started := prior != nil && methodologyActive(prior.Phase)
	if placement.Wait != "" && !started {
		status.Phase, status.Message = MethodologyPhaseQueued, placement.Wait
		return status, true, nil
	}
	if admitted, why := queue.admit(run); !admitted {
		// A stage Job that already exists (a status write lost after it was created) is under way.
		existing := &batchv1.Job{}
		err := r.Get(ctx, client.ObjectKey{Namespace: methodologyRunsNamespace(lake.Name), Name: methodologyStageJob(run.Slug)}, existing)
		switch {
		case err == nil && existing.Labels[methodologyRunIDLabel] == run.ID:
			queue.active[run.WorkspaceID]++
			queue.waiting[run.WorkspaceID]--
		case err == nil || apierrors.IsNotFound(err):
			status.Phase, status.Message = MethodologyPhaseQueued, why
			return status, true, nil
		default:
			return status, true, err
		}
	}

	ns, err := namespace()
	if err != nil {
		return status, true, err
	}
	labels := methodologyLabels(lake, run, "")
	credential, err := r.reconcileAgentCredential(ctx, lake, ns, labels, methodologyCredentialSecret(run.Slug),
		methodologyRunIDLabel, run.ID, "lmr.")
	if err != nil {
		return status, true, err
	}
	status.CredentialSHA256 = credentialDigest(credential)
	if err := r.reconcileMethodologyPullSecret(ctx, lake, ns, run, credential, labels); err != nil {
		return status, true, err
	}
	if err := r.reconcileMethodologyWorkClaim(ctx, lake, ns, run, labels); err != nil {
		return status, true, err
	}

	// 1. stage
	stage, _, err := r.ensureMethodologyJob(ctx, lake, ns, methodologyStageJob(run.Slug),
		methodologyLabels(lake, run, methodologyRoleStage), methodologyStagePod(lake, run, placement.Zones), methodologyTransferDeadline)
	if err != nil {
		return status, true, err
	}
	switch {
	case stage.Status.Failed > 0:
		message := "could not bring the data in"
		if detail := r.methodologyJobFailure(ctx, stage); detail != "" {
			message += ": " + detail
		}
		return fail(message)
	case stage.Status.Succeeded == 0:
		status.Phase, status.Message = MethodologyPhaseStaging, "bringing the data in"
		return status, true, nil
	}

	// 2. run: the code that is not trusted. Its resources are recorded before its Job exists.
	sandboxSpec := methodologySandboxPod(run, image)
	existing := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: methodologySandboxJob(run.Slug)}, existing); apierrors.IsNotFound(err) {
		if err := r.recordMethodologyResources(ctx, store, run, sandboxSpec); err != nil {
			return status, true, err
		}
	} else if err != nil {
		return status, true, err
	}
	sandbox, _, err := r.ensureMethodologyJob(ctx, lake, ns, methodologySandboxJob(run.Slug),
		methodologyLabels(lake, run, methodologyRoleSandbox), sandboxSpec, int64(run.TimeoutSeconds))
	if err != nil {
		return status, true, err
	}
	observed, err := r.observeSandbox(ctx, ns, sandbox.Name)
	if err != nil {
		return status, true, err
	}
	if observed.startedAt != nil {
		status.StartedAt = observed.startedAt
	}
	if !jobFinished(sandbox) {
		status.Phase, status.Message = MethodologyPhaseRunning, "running the methodology's code"
		if status.StartedAt == nil {
			status.Message = "starting the methodology's code"
		}
		return status, true, nil
	}
	if status.FinishedAt == nil {
		status.FinishedAt = observed.finishedAt
		if status.FinishedAt == nil {
			// The pod is gone or never reported (a deadline kills it): the Job's own end.
			status.FinishedAt = nowSecond()
			for _, condition := range sandbox.Status.Conditions {
				if (condition.Type == batchv1.JobFailed || condition.Type == batchv1.JobComplete) && !condition.LastTransitionTime.IsZero() {
					at := condition.LastTransitionTime.UTC().Truncate(time.Second)
					status.FinishedAt = &at
				}
			}
		}
		if status.StartedAt == nil {
			status.StartedAt = status.FinishedAt
		}
		// Written again at its end, from the Job it ran as, in case the row was removed.
		if err := r.recordMethodologyResources(ctx, store, run, sandbox.Spec.Template.Spec); err != nil {
			return status, true, err
		}
	}
	if observed.exitCode != nil {
		status.ExitCode = observed.exitCode
	} else if status.ExitCode == nil && sandbox.Status.Succeeded > 0 {
		zero := int32(0)
		status.ExitCode = &zero
	}
	codeMessage := ""
	switch {
	case sandbox.Status.Failed > 0 && jobDeadlineExceeded(sandbox):
		codeMessage = fmt.Sprintf("the methodology's code ran past its time limit (%d seconds)", run.TimeoutSeconds)
	case sandbox.Status.Failed > 0 && status.ExitCode != nil:
		codeMessage = fmt.Sprintf("the methodology's code exited with %d", *status.ExitCode)
	case sandbox.Status.Failed > 0:
		codeMessage = "the methodology's code did not finish"
		if detail := jobFailureMessage(sandbox); detail != "" {
			codeMessage += ": " + detail
		}
	}

	// 3. collect: report the result, whether the code succeeded or not.
	collect, _, err := r.ensureMethodologyJob(ctx, lake, ns, methodologyCollectJob(run.Slug),
		methodologyLabels(lake, run, methodologyRoleCollect), methodologyCollectPod(lake, run, status.ExitCode), methodologyTransferDeadline)
	if err != nil {
		return status, true, err
	}
	switch {
	case collect.Status.Failed > 0:
		message := "could not report the result to Lake"
		if detail := r.methodologyJobFailure(ctx, collect); detail != "" {
			message += ": " + detail
		}
		if codeMessage != "" {
			message = codeMessage + "; " + message
		}
		return fail(message)
	case collect.Status.Succeeded == 0:
		status.Phase, status.Message = MethodologyPhaseCollecting, "reporting the result"
		return status, true, nil
	}
	if codeMessage != "" {
		return fail(codeMessage)
	}
	status.Phase, status.Message, status.CredentialSHA256 = MethodologyPhaseSucceeded, "finished", ""
	return status, true, r.removeMethodologyRun(ctx, lake, run)
}

// removeMethodologyRun deletes a run's Jobs (and their pods), credential, pull Secret and work
// volume. Only objects carrying the run's id and controlled by the Lake are touched.
func (r *LakeReconciler) removeMethodologyRun(ctx context.Context, lake *oramav1alpha1.Lake, run *LakeMethodologyRun) error {
	if !methodologyRunSlugPattern.MatchString(run.Slug) {
		return nil
	}
	namespace := methodologyRunsNamespace(lake.Name)
	background := metav1.DeletePropagationBackground
	objects := []client.Object{
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: methodologyStageJob(run.Slug), Namespace: namespace}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: methodologySandboxJob(run.Slug), Namespace: namespace}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: methodologyCollectJob(run.Slug), Namespace: namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: methodologyCredentialSecret(run.Slug), Namespace: namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: methodologyPullSecret(run.Slug), Namespace: namespace}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: methodologyWorkClaim(run.Slug), Namespace: namespace}},
	}
	for _, object := range objects {
		if err := r.Get(ctx, client.ObjectKeyFromObject(object), object); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return err
		}
		if object.GetLabels()[methodologyRunIDLabel] != run.ID || !metav1.IsControlledBy(object, lake) {
			continue
		}
		if err := r.Delete(ctx, object, &client.DeleteOptions{PropagationPolicy: &background}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// stopMethodologyRun ends an absent run: whichever pod is running stops, its objects go, and a
// sandbox that had started is charged up to now.
func (r *LakeReconciler) stopMethodologyRun(ctx context.Context, lake *oramav1alpha1.Lake, store LakeServiceStore, run *LakeMethodologyRun) (LakeMethodologyRunStatus, error) {
	status := LakeMethodologyRunStatus{RunID: run.ID, Phase: MethodologyPhaseStopped, Message: "stopped", ObservedGeneration: run.Generation}
	if prior := run.Status; prior != nil {
		status.StartedAt, status.FinishedAt, status.ExitCode = prior.StartedAt, prior.FinishedAt, prior.ExitCode
		if prior.Phase == MethodologyPhaseSucceeded || prior.Phase == MethodologyPhaseFailed {
			status.Phase, status.Message = prior.Phase, prior.Message
		}
	}
	if methodologyRunSlugPattern.MatchString(run.Slug) {
		namespace := methodologyRunsNamespace(lake.Name)
		sandbox := &batchv1.Job{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: methodologySandboxJob(run.Slug)}, sandbox); err == nil &&
			sandbox.Labels[methodologyRunIDLabel] == run.ID && metav1.IsControlledBy(sandbox, lake) {
			observed, err := r.observeSandbox(ctx, namespace, sandbox.Name)
			if err != nil {
				return status, err
			}
			if status.StartedAt == nil {
				status.StartedAt = observed.startedAt
			}
			if status.StartedAt != nil && status.FinishedAt == nil {
				status.FinishedAt = observed.finishedAt
				if status.FinishedAt == nil {
					status.FinishedAt = nowSecond()
				}
				if err := r.recordMethodologyResources(ctx, store, run, sandbox.Spec.Template.Spec); err != nil {
					return status, err
				}
			}
		} else if err != nil && !apierrors.IsNotFound(err) {
			return status, err
		}
	}
	return status, r.removeMethodologyRun(ctx, lake, run)
}

// reconcileLakeMethodologyRuns applies one read of methodology_runs and reports whether any run is
// still changing. The namespace and its policies are reconciled once per pass, when a run first
// needs them.
func (r *LakeReconciler) reconcileLakeMethodologyRuns(ctx context.Context, lake *oramav1alpha1.Lake, store LakeServiceStore, runs []LakeMethodologyRun) bool {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	changing := false
	var namespaceName string
	var namespaceErr error
	namespaceDone := false
	namespace := func() (string, error) {
		if !namespaceDone {
			namespaceName, namespaceErr = r.reconcileMethodologyNamespace(ctx, lake)
			namespaceDone = true
		}
		return namespaceName, namespaceErr
	}
	queue := newMethodologyQueue(lake, runs)
	var nodeList []corev1.Node
	var nodesErr error
	nodesDone := false
	nodes := func() ([]corev1.Node, error) {
		if !nodesDone {
			var listed corev1.NodeList
			nodesErr = r.reader().List(ctx, &listed)
			nodeList, nodesDone = listed.Items, true
			if nodesErr != nil {
				nodesErr = fmt.Errorf("list nodes to place a GPU run: %w", nodesErr)
			}
		}
		return nodeList, nodesErr
	}
	for i := range runs {
		run := &runs[i]
		var status LakeMethodologyRunStatus
		var err error
		switch run.Desired {
		case serviceDesiredAbsent:
			if run.Status != nil && run.Status.ObservedGeneration == run.Generation && methodologyTerminal(run.Status.Phase) &&
				run.Status.CredentialSHA256 == "" {
				continue
			}
			status, err = r.stopMethodologyRun(ctx, lake, store, run)
		case serviceDesiredPresent:
			var still bool
			status, still, err = r.reconcileMethodologyRun(ctx, lake, store, run, namespace, queue, nodes)
			changing = changing || still
		default:
			status = LakeMethodologyRunStatus{RunID: run.ID, Phase: MethodologyPhaseFailed,
				Message: "desired must be present or absent", ObservedGeneration: run.Generation}
		}
		if err != nil {
			log.Error(err, "methodology run deferred", "run", run.ID, "slug", run.Slug)
			changing = true
			if status.Phase == "" {
				continue
			}
		}
		if run.Status != nil && run.Status.equal(status) {
			continue
		}
		if err := store.UpsertMethodologyRunStatus(ctx, status); err != nil {
			log.Error(err, "methodology run status write deferred", "run", run.ID)
			changing = true
			continue
		}
		copied := status
		run.Status = &copied
	}
	return changing
}

// jobFinished reports whether a Job's one pod has ended, succeeded or failed (backoffLimit 0).
func jobFinished(job *batchv1.Job) bool {
	return job.Status.Succeeded > 0 || job.Status.Failed > 0
}

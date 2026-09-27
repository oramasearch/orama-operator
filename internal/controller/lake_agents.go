/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// lake's agents (lake#228; the contract is lake's docs/api/agent-launches.md and this repo's
// docs/lake/agents.md). Two halves:
//
//   - The Agent Library's specialized agents (Ask Orama, the Project Agent, Auto-Suggest,
//     AgentCreator): one Deployment and Service each, from spec.agents.library, told lake's
//     in-cluster address and the service token lake presents to them. lake learns their
//     addresses through LAKE_AGENT_ENDPOINTS.
//   - Launches of user-created Python agents: rows of lake's agent_launches table (migration
//     0030), read on the services poll. A job launch is a Job, a service launch a Deployment and
//     Service, always from spec.agents.runtimeImage. Each gets a credential the operator
//     generates, lal.<launch id>.<secret>, in a Secret the pod reads; the operator writes only its
//     SHA-256 into agent_launch_status, and lake resolves the credential by comparing digests.
const (
	agentPort            = 8080
	agentPortName        = "http"
	agentHealthPath      = "/healthz"
	agentRunAsUser       = int64(10001)
	agentDefaultModel    = "gpt-5.5"
	agentDefaultDeadline = int64(3600)
	agentJobTTLSeconds   = int32(3600)
	agentTempVolume      = "tmp"

	agentLaunchIDLabel   = "orama.cloud/agent-launch-id"
	libraryAgentLabel    = "orama.cloud/library-agent"
	agentCredentialKey   = "token"
	agentServiceTokenKey = "token"
	openAIKeyName        = "OPENAI_API_KEY"

	agentLaunchModeJob     = "job"
	agentLaunchModeService = "service"

	AgentPhasePending   = "Pending"
	AgentPhaseRunning   = "Running"
	AgentPhaseReady     = "Ready"
	AgentPhaseSucceeded = "Succeeded"
	AgentPhaseFailed    = "Failed"
	AgentPhaseStopped   = "Stopped"
)

var agentLaunchSlugPattern = regexp.MustCompile(`^al-[0-9a-f]{12}$`)

// errAgentLaunchesTableMissing: lake's schema has not reached 0030_agent_launches yet.
var errAgentLaunchesTableMissing = errors.New("the agent_launches table does not exist yet (lake schema before 0030_agent_launches)")

// LakeAgentLaunch is one row of lake's agent_launches table joined to its status row.
type LakeAgentLaunch struct {
	ID         string
	CustomerID string
	AgentID    string
	Mode       string
	// Input is the raw jsonb input a job is asked to do.
	Input      string
	Slug       string
	Desired    string
	Generation int64
	Status     *LakeAgentLaunchStatus
}

// LakeAgentLaunchStatus is one agent_launch_status row, written only by the operator.
type LakeAgentLaunchStatus struct {
	LaunchID           string
	Phase              string
	Message            string
	Endpoint           string
	ExitCode           *int32
	Result             string
	CredentialSHA256   string
	ObservedGeneration int64
}

func (s LakeAgentLaunchStatus) equal(other LakeAgentLaunchStatus) bool {
	sameExit := (s.ExitCode == nil && other.ExitCode == nil) ||
		(s.ExitCode != nil && other.ExitCode != nil && *s.ExitCode == *other.ExitCode)
	return s.LaunchID == other.LaunchID && s.Phase == other.Phase && s.Message == other.Message &&
		s.Endpoint == other.Endpoint && sameExit && s.Result == other.Result &&
		s.CredentialSHA256 == other.CredentialSHA256 && s.ObservedGeneration == other.ObservedGeneration
}

func agentsEnabled(lake *oramav1alpha1.Lake) bool {
	return lake.Spec.Agents.RuntimeImage != "" || len(lake.Spec.Agents.Library) > 0
}

func agentModel(lake *oramav1alpha1.Lake) string {
	if lake.Spec.Agents.Model != "" {
		return lake.Spec.Agents.Model
	}
	return agentDefaultModel
}

func lakeAgentServiceTokenSecret(lake string) string { return "lake-" + lake + "-agent-service-token" }

func libraryAgentName(lake, key string) string { return lake + "-" + key }

func agentLaunchName(lake, slug string) string { return lake + "-" + slug }

func agentLaunchCredentialSecret(lake, slug string) string {
	return agentLaunchName(lake, slug) + "-credential"
}

// lakeInternalURL is lake's in-cluster address: its Service, on the Service port.
func lakeInternalURL(lake *oramav1alpha1.Lake, namespace string) string {
	return fmt.Sprintf("http://lake-%s.%s.svc:%d", lake.Name, namespace, lakeServicePort)
}

func agentServiceURL(name, namespace string) string {
	return fmt.Sprintf("http://%s.%s.svc:%d", name, namespace, agentPort)
}

// libraryAgentEndpoints is LAKE_AGENT_ENDPOINTS: key=url for every library agent, sorted.
func libraryAgentEndpoints(lake *oramav1alpha1.Lake, namespace string) string {
	pairs := make([]string, 0, len(lake.Spec.Agents.Library))
	for _, agent := range lake.Spec.Agents.Library {
		pairs = append(pairs, agent.Key+"="+agentServiceURL(libraryAgentName(lake.Name, agent.Key), namespace))
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

// lakeAgentEnv is what the lake container learns about its agents.
func lakeAgentEnv(lake *oramav1alpha1.Lake, namespace string) []corev1.EnvVar {
	if !agentsEnabled(lake) {
		return nil
	}
	return []corev1.EnvVar{
		{Name: "LAKE_AGENT_ENDPOINTS", Value: libraryAgentEndpoints(lake, namespace)},
		{Name: "LAKE_INTERNAL_URL", Value: lakeInternalURL(lake, namespace)},
		{Name: "LAKE_AGENT_MODEL", Value: agentModel(lake)},
		{Name: "LAKE_AGENT_SERVICE_TOKEN", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: lakeAgentServiceTokenSecret(lake.Name)},
			Key:                  agentServiceTokenKey,
		}}},
	}
}

func randomHex(bytes int) (string, error) {
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

// reconcileLakeAgentServiceToken mints the token lake presents to agent services, once.
func (r *LakeReconciler) reconcileLakeAgentServiceToken(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, labels map[string]string) error {
	if !agentsEnabled(lake) {
		return nil
	}
	name := lakeAgentServiceTokenSecret(lake.Name)
	existing := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, existing); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	token, err := randomHex(32)
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Data:       map[string][]byte{agentServiceTokenKey: []byte(token)},
	}
	if err := controllerutil.SetControllerReference(lake, secret, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create agent service token: %w", err)
	}
	return nil
}

func secretEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key}}}
}

// agentBaseEnv is what every agent pod is told: lake's address, the general OpenAI key, the model
// and where traces go.
func agentBaseEnv(lake *oramav1alpha1.Lake, namespace string) []corev1.EnvVar {
	model := agentModel(lake)
	env := []corev1.EnvVar{
		{Name: "LAKE_URL", Value: lakeInternalURL(lake, namespace)},
		{Name: "PORT", Value: fmt.Sprint(agentPort)},
		{Name: "TRACE_DIR", Value: "/tmp/traces"},
		{Name: "HOME", Value: "/tmp"},
		{Name: "AGENT_MODEL", Value: model},
		{Name: "ASK_ORAMA_MODEL", Value: model},
		{Name: "PROJECT_MODEL", Value: model},
		{Name: "AUTOSUGGEST_MODEL", Value: model},
	}
	if lake.Spec.Agents.OpenAISecretName != "" {
		env = append(env, secretEnv(openAIKeyName, lake.Spec.Agents.OpenAISecretName, openAIKeyName))
	}
	return env
}

// agentPodSpec is an agent pod: non-root with a read-only root filesystem and a writable /tmp, the
// Lake's pull Secret, no Kubernetes token.
func agentPodSpec(lake *oramav1alpha1.Lake, image string, env []corev1.EnvVar, service bool) corev1.PodSpec {
	var pullSecrets []corev1.LocalObjectReference
	if lake.Spec.PullSecretName != "" {
		pullSecrets = []corev1.LocalObjectReference{{Name: lake.Spec.PullSecretName}}
	}
	noToken, noLinks, nonRoot, noEscalation, readOnlyRoot := false, false, true, false, true
	user := agentRunAsUser
	tempSize := resource.MustParse("1Gi")
	container := corev1.Container{
		Name:            "agent",
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Env:             env,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
			Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: agentTempVolume, MountPath: "/tmp"}},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &noEscalation,
			ReadOnlyRootFilesystem:   &readOnlyRoot,
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		},
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
	}
	if service {
		health := corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: agentHealthPath, Port: intstr.FromString(agentPortName), Scheme: corev1.URISchemeHTTP}}
		container.Ports = []corev1.ContainerPort{{Name: agentPortName, ContainerPort: agentPort, Protocol: corev1.ProtocolTCP}}
		container.StartupProbe = &corev1.Probe{ProbeHandler: health, PeriodSeconds: 5, TimeoutSeconds: 3, FailureThreshold: 60}
		container.ReadinessProbe = &corev1.Probe{ProbeHandler: health, PeriodSeconds: 10, TimeoutSeconds: 3, FailureThreshold: 3}
		container.LivenessProbe = &corev1.Probe{ProbeHandler: health, PeriodSeconds: 20, TimeoutSeconds: 3, FailureThreshold: 3}
	}
	return corev1.PodSpec{
		ImagePullSecrets:             pullSecrets,
		AutomountServiceAccountToken: &noToken,
		EnableServiceLinks:           &noLinks,
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot:   &nonRoot,
			RunAsUser:      &user,
			RunAsGroup:     &user,
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		Containers: []corev1.Container{container},
		Volumes: []corev1.Volume{{Name: agentTempVolume, VolumeSource: corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &tempSize}}}},
	}
}

// agentLabels are an agent's own labels: the Lake's, with the name and instance replaced. An agent
// pod must never carry the Lake's app.kubernetes.io/name and instance, which are the selector of
// lake's Service and PodDisruptionBudget: a job pod carrying them received lake's traffic.
func agentLabels(labels map[string]string, name, instance string, extra map[string]string) map[string]string {
	out := withLabels(labels, extra)
	out["app.kubernetes.io/name"] = name
	out["app.kubernetes.io/instance"] = instance
	return out
}

func withLabels(labels map[string]string, extra map[string]string) map[string]string {
	out := make(map[string]string, len(labels)+len(extra))
	for key, value := range labels {
		out[key] = value
	}
	for key, value := range extra {
		out[key] = value
	}
	return out
}

// reconcileAgentService makes a Deployment and a Service named `name` run `pod`, and reports the
// Deployment's available replicas.
func (r *LakeReconciler) reconcileAgentService(ctx context.Context, lake *oramav1alpha1.Lake, namespace, name string, labels, selector map[string]string, pod corev1.PodSpec) (int32, error) {
	replicas := int32(1)
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		deployment.Labels = labels
		deployment.Spec.Replicas = &replicas
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
		deployment.Spec.Template.Labels = withLabels(labels, selector)
		deployment.Spec.Template.Spec = pod
		return controllerutil.SetControllerReference(lake, deployment, r.Scheme)
	}); err != nil {
		return 0, fmt.Errorf("reconcile agent deployment %s: %w", name, err)
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec.Selector = selector
		svc.Spec.Ports = []corev1.ServicePort{{Name: agentPortName, Port: agentPort, TargetPort: intstr.FromString(agentPortName)}}
		return controllerutil.SetControllerReference(lake, svc, r.Scheme)
	}); err != nil {
		return 0, fmt.Errorf("reconcile agent service %s: %w", name, err)
	}
	return deployment.Status.AvailableReplicas, nil
}

// reconcileLakeLibraryAgents runs spec.agents.library and removes library agents no longer listed.
func (r *LakeReconciler) reconcileLakeLibraryAgents(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, labels map[string]string) error {
	wanted := map[string]bool{}
	for _, agent := range lake.Spec.Agents.Library {
		name := libraryAgentName(lake.Name, agent.Key)
		wanted[name] = true
		selector := map[string]string{"app.kubernetes.io/name": agent.Key, "app.kubernetes.io/instance": name}
		env := append(agentBaseEnv(lake, namespace),
			secretEnv("AGENT_SERVICE_TOKEN", lakeAgentServiceTokenSecret(lake.Name), agentServiceTokenKey))
		pod := agentPodSpec(lake, agent.Image, env, true)
		libraryLabels := agentLabels(labels, agent.Key, name, map[string]string{libraryAgentLabel: agent.Key})
		if _, err := r.reconcileAgentService(ctx, lake, namespace, name, libraryLabels, selector, pod); err != nil {
			return err
		}
	}
	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments, client.InNamespace(namespace), client.HasLabels{libraryAgentLabel}); err != nil {
		return err
	}
	for i := range deployments.Items {
		deployment := &deployments.Items[i]
		if wanted[deployment.Name] || !metav1.IsControlledBy(deployment, lake) {
			continue
		}
		if err := r.Delete(ctx, deployment); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: deployment.Name, Namespace: namespace}}
		if err := r.Delete(ctx, svc); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// reconcileAgentLaunchCredential returns the launch's credential, generating it once.
func (r *LakeReconciler) reconcileAgentLaunchCredential(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, labels map[string]string, launch *LakeAgentLaunch) (string, error) {
	name := agentLaunchCredentialSecret(lake.Name, launch.Slug)
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err == nil {
		if secret.Labels[agentLaunchIDLabel] != launch.ID || !metav1.IsControlledBy(secret, lake) {
			return "", fmt.Errorf("Secret %s/%s exists and is not this launch's credential", namespace, name)
		}
		return string(secret.Data[agentCredentialKey]), nil
	} else if !apierrors.IsNotFound(err) {
		return "", err
	}
	random, err := randomHex(16)
	if err != nil {
		return "", err
	}
	credential := "lal." + launch.ID + "." + random
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace,
			Labels: withLabels(labels, map[string]string{agentLaunchIDLabel: launch.ID})},
		Data: map[string][]byte{agentCredentialKey: []byte(credential)},
	}
	if err := controllerutil.SetControllerReference(lake, secret, r.Scheme); err != nil {
		return "", err
	}
	if err := r.Create(ctx, secret); err != nil {
		return "", fmt.Errorf("create launch credential %s: %w", name, err)
	}
	return credential, nil
}

func credentialDigest(credential string) string {
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:])
}

func (launch *LakeAgentLaunch) env(lake *oramav1alpha1.Lake, namespace string) []corev1.EnvVar {
	env := append(agentBaseEnv(lake, namespace),
		secretEnv("LAKE_TOKEN", agentLaunchCredentialSecret(lake.Name, launch.Slug), agentCredentialKey),
		corev1.EnvVar{Name: "CUSTOMER_ID", Value: launch.CustomerID},
		corev1.EnvVar{Name: "AGENT_ID", Value: launch.AgentID},
		corev1.EnvVar{Name: "AGENT_MODE", Value: launch.Mode},
	)
	if launch.Mode == agentLaunchModeJob {
		input := launch.Input
		if strings.TrimSpace(input) == "" {
			input = "null"
		}
		env = append(env, corev1.EnvVar{Name: "AGENT_INPUT", Value: input})
	} else {
		env = append(env, secretEnv("AGENT_SERVICE_TOKEN", lakeAgentServiceTokenSecret(lake.Name), agentServiceTokenKey))
	}
	return env
}

// jobOutcome reads a finished job's pod: its exit code and the runtime's termination summary.
func (r *LakeReconciler) jobOutcome(ctx context.Context, namespace, job string) (*int32, string) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{"job-name": job}); err != nil {
		return nil, "null"
	}
	for _, pod := range pods.Items {
		for _, container := range pod.Status.ContainerStatuses {
			if terminated := container.State.Terminated; terminated != nil {
				code := terminated.ExitCode
				result := strings.TrimSpace(terminated.Message)
				if !json.Valid([]byte(result)) || result == "" {
					encoded, _ := json.Marshal(map[string]string{"error": result})
					result = string(encoded)
				}
				return &code, result
			}
		}
	}
	return nil, "null"
}

// reconcileAgentLaunch makes one present launch run and reports what the operator observed.
func (r *LakeReconciler) reconcileAgentLaunch(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, labels map[string]string, launch *LakeAgentLaunch) (LakeAgentLaunchStatus, bool, error) {
	status := LakeAgentLaunchStatus{LaunchID: launch.ID, Result: "null", ObservedGeneration: launch.Generation}
	switch {
	case !agentLaunchSlugPattern.MatchString(launch.Slug):
		status.Phase, status.Message = AgentPhaseFailed, "slug must match "+agentLaunchSlugPattern.String()
		return status, false, nil
	case launch.Mode != agentLaunchModeJob && launch.Mode != agentLaunchModeService:
		status.Phase, status.Message = AgentPhaseFailed, "mode must be job or service"
		return status, false, nil
	case lake.Spec.Agents.RuntimeImage == "":
		status.Phase, status.Message = AgentPhaseFailed, "agents are not enabled on this Lake (spec.agents.runtimeImage)"
		return status, false, nil
	}
	// A job that already finished keeps its outcome; its pod may be gone.
	if prior := launch.Status; prior != nil && (prior.Phase == AgentPhaseSucceeded || prior.Phase == AgentPhaseFailed) {
		return *prior, false, nil
	}
	credential, err := r.reconcileAgentLaunchCredential(ctx, lake, namespace, labels, launch)
	if err != nil {
		return status, true, err
	}
	status.CredentialSHA256 = credentialDigest(credential)
	name := agentLaunchName(lake.Name, launch.Slug)
	launchLabels := agentLabels(labels, "agent-runtime", name, map[string]string{agentLaunchIDLabel: launch.ID})
	if launch.Mode == agentLaunchModeService {
		selector := map[string]string{"app.kubernetes.io/name": "agent-runtime", "app.kubernetes.io/instance": name}
		available, err := r.reconcileAgentService(ctx, lake, namespace, name, launchLabels, selector,
			agentPodSpec(lake, lake.Spec.Agents.RuntimeImage, launch.env(lake, namespace), true))
		if err != nil {
			return status, true, err
		}
		if available > 0 {
			status.Phase, status.Message, status.Endpoint = AgentPhaseReady, "serving", agentServiceURL(name, namespace)
			return status, false, nil
		}
		status.Phase, status.Message = AgentPhasePending, "starting the agent"
		return status, true, nil
	}

	job := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, job); apierrors.IsNotFound(err) {
		deadline := agentDefaultDeadline
		if lake.Spec.Agents.JobDeadlineSeconds != nil {
			deadline = *lake.Spec.Agents.JobDeadlineSeconds
		}
		backoff, ttl := int32(0), agentJobTTLSeconds
		pod := agentPodSpec(lake, lake.Spec.Agents.RuntimeImage, launch.env(lake, namespace), false)
		pod.RestartPolicy = corev1.RestartPolicyNever
		job = &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: launchLabels},
			Spec: batchv1.JobSpec{
				BackoffLimit:            &backoff,
				ActiveDeadlineSeconds:   &deadline,
				TTLSecondsAfterFinished: &ttl,
				Template:                corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: launchLabels}, Spec: pod},
			},
		}
		if err := controllerutil.SetControllerReference(lake, job, r.Scheme); err != nil {
			return status, true, err
		}
		if err := r.Create(ctx, job); err != nil {
			return status, true, fmt.Errorf("create agent job %s: %w", name, err)
		}
		status.Phase, status.Message = AgentPhasePending, "starting the agent"
		return status, true, nil
	} else if err != nil {
		return status, true, err
	}
	switch {
	case job.Status.Succeeded > 0:
		status.ExitCode, status.Result = r.jobOutcome(ctx, namespace, name)
		if status.ExitCode == nil {
			// The Job reports success before its pod's termination state is readable. Wait for
			// it rather than freezing an empty outcome, since a finished run is never revisited.
			status.Phase, status.Message = AgentPhaseRunning, "collecting the result"
			return status, true, nil
		}
		status.Phase, status.Message = AgentPhaseSucceeded, "finished"
		// The run is over: its credential stops resolving in lake.
		status.CredentialSHA256 = ""
		return status, false, nil
	case job.Status.Failed > 0:
		status.ExitCode, status.Result = r.jobOutcome(ctx, namespace, name)
		status.Phase, status.Message = AgentPhaseFailed, "the agent did not finish"
		for _, condition := range job.Status.Conditions {
			if condition.Type == batchv1.JobFailed && condition.Message != "" {
				status.Message = condition.Message
			}
		}
		status.CredentialSHA256 = ""
		return status, false, nil
	case job.Status.Active > 0:
		status.Phase, status.Message = AgentPhaseRunning, "running"
		return status, true, nil
	}
	status.Phase, status.Message = AgentPhasePending, "starting the agent"
	return status, true, nil
}

// stopAgentLaunch removes an absent launch's Job or Deployment, Service and credential.
func (r *LakeReconciler) stopAgentLaunch(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, launch *LakeAgentLaunch) (LakeAgentLaunchStatus, error) {
	status := LakeAgentLaunchStatus{LaunchID: launch.ID, Phase: AgentPhaseStopped, Message: "stopped", Result: "null", ObservedGeneration: launch.Generation}
	if launch.Status != nil {
		status.ExitCode, status.Result = launch.Status.ExitCode, launch.Status.Result
		if launch.Status.Phase == AgentPhaseSucceeded || launch.Status.Phase == AgentPhaseFailed {
			status.Phase, status.Message = launch.Status.Phase, launch.Status.Message
		}
	}
	if !agentLaunchSlugPattern.MatchString(launch.Slug) {
		return status, nil
	}
	name := agentLaunchName(lake.Name, launch.Slug)
	background := metav1.DeletePropagationBackground
	objects := []client.Object{
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: agentLaunchCredentialSecret(lake.Name, launch.Slug), Namespace: namespace}},
	}
	for _, object := range objects {
		if err := r.Get(ctx, client.ObjectKeyFromObject(object), object); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return status, err
		}
		if object.GetLabels()[agentLaunchIDLabel] != launch.ID || !metav1.IsControlledBy(object, lake) {
			continue
		}
		if err := r.Delete(ctx, object, &client.DeleteOptions{PropagationPolicy: &background}); err != nil && !apierrors.IsNotFound(err) {
			return status, err
		}
	}
	return status, nil
}

// reconcileLakeAgentLaunches applies one read of agent_launches and reports whether any launch
// is still changing.
func (r *LakeReconciler) reconcileLakeAgentLaunches(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, labels map[string]string, store LakeServiceStore, launches []LakeAgentLaunch) bool {
	log := logf.FromContext(ctx).WithValues("lake", lake.Name)
	changing := false
	for i := range launches {
		launch := &launches[i]
		var status LakeAgentLaunchStatus
		var err error
		switch launch.Desired {
		case serviceDesiredAbsent:
			if launch.Status != nil && launch.Status.ObservedGeneration == launch.Generation &&
				(launch.Status.Phase == AgentPhaseStopped || launch.Status.Phase == AgentPhaseSucceeded || launch.Status.Phase == AgentPhaseFailed) &&
				launch.Status.CredentialSHA256 == "" {
				continue
			}
			status, err = r.stopAgentLaunch(ctx, lake, namespace, launch)
		case serviceDesiredPresent:
			var still bool
			status, still, err = r.reconcileAgentLaunch(ctx, lake, namespace, labels, launch)
			changing = changing || still
		default:
			status = LakeAgentLaunchStatus{LaunchID: launch.ID, Phase: AgentPhaseFailed, Result: "null",
				Message: "desired must be present or absent", ObservedGeneration: launch.Generation}
		}
		if err != nil {
			log.Error(err, "agent launch deferred", "launch", launch.ID, "slug", launch.Slug)
			changing = true
			continue
		}
		if launch.Status != nil && launch.Status.equal(status) {
			continue
		}
		if err := store.UpsertAgentLaunchStatus(ctx, status); err != nil {
			log.Error(err, "agent launch status write deferred", "launch", launch.ID)
			continue
		}
		copied := status
		launch.Status = &copied
	}
	return changing
}

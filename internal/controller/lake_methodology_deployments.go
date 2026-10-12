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
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Served models of a workspace's own methodologies (lake#690; lake migration
// 0080_methodology_deployments; this repo's docs/lake/methodology-runs.md#serving-a-model). Lake
// writes one methodology_deployments row when a person publishes such a model in Ship → Models. The
// operator serves it in the Lake's methodology-runs namespace, with the same split as a run:
//
//   - stage: a Job of the pristine runtime image with the deployment's credential,
//     lmd.<deployment id>.<secret>, egress only to lake's pods and DNS. It pages the model's artifact
//     onto the deployment's model volume (`methodology stage-model`).
//   - serve: a Deployment of the methodology's own image (`methodology serve`), the code that is not
//     trusted. No credential, no service account token, no service links, no network of its own
//     unless the person granted it, requests equal to limits, the model volume read-only. One
//     NetworkPolicy admits connections to it from lake's pods only, on its port; a Service names it,
//     and lake forwards each predict call there.
//
// The credential stays live while the deployment is present: kubelet pulls the methodology's image
// with it (Basic user methodology-deployment) whenever a serve pod restarts. The operator writes only
// its SHA-256, and the Service's in-cluster URL, into methodology_deployment_status.
const (
	DeploymentPhaseBuilding = "Building"
	DeploymentPhaseQueued   = "Queued"
	DeploymentPhaseStaging  = "Staging"
	DeploymentPhaseStarting = "Starting"
	DeploymentPhaseReady    = "Ready"
	DeploymentPhaseFailed   = "Failed"
	DeploymentPhaseStopped  = "Stopped"

	// WorkloadKindMethodologyDeployment is a served methodology model's serve pods (lake#690).
	WorkloadKindMethodologyDeployment = "methodology_deployment"

	methodologyDeploymentIDLabel = "orama.cloud/methodology-deployment-id"
	methodologyRoleServe         = "serve"
	methodologyServeIngress      = "methodology-serve-ingress"
	methodologyServePort         = int32(8080)
	methodologyServeServicePort  = int32(80)
	methodologyDeploymentPull    = "methodology-deployment"
	methodologyModelPath         = methodologyWorkPath + "/model"
	// methodologyServeRestartLimit is how many times a serve pod may exit before the deployment is
	// failed with what it said: a model that cannot load will not load on a fifth try.
	methodologyServeRestartLimit = int32(3)
)

var (
	methodologyDeploymentSlugPattern = regexp.MustCompile(`^md-[0-9a-f]{12}$`)

	// errMethodologyDeploymentsTableMissing: lake's schema has not reached 0080 yet.
	errMethodologyDeploymentsTableMissing = errors.New("the methodology_deployments table does not exist yet (lake schema before 0080_methodology_deployments)")
)

// LakeMethodologyDeployment is one methodology_deployments row joined to its image build and its
// status row.
type LakeMethodologyDeployment struct {
	ID            string
	CustomerID    string
	WorkspaceID   string
	MethodologyID string
	ModelID       string
	BuildID       string
	Image         string
	RuntimeImage  string
	CPUMillis     int32
	MemoryBytes   int64
	GPUs          int32
	GPUClass      string
	Network       bool
	Slug          string
	Desired       string
	Generation    int64
	Build         *LakeMethodologyRunBuild
	Status        *LakeMethodologyDeploymentStatus
}

// LakeMethodologyDeploymentStatus is one methodology_deployment_status row, written only by the
// operator. Endpoint is the serve Service's in-cluster URL, set once the Service exists.
type LakeMethodologyDeploymentStatus struct {
	DeploymentID       string
	Phase              string
	Message            string
	CredentialSHA256   string
	Endpoint           string
	ObservedGeneration int64
	ReadyAt            *time.Time
	StoppedAt          *time.Time
}

func (s LakeMethodologyDeploymentStatus) equal(other LakeMethodologyDeploymentStatus) bool {
	return s.DeploymentID == other.DeploymentID && s.Phase == other.Phase && s.Message == other.Message &&
		s.CredentialSHA256 == other.CredentialSHA256 && s.Endpoint == other.Endpoint &&
		s.ObservedGeneration == other.ObservedGeneration && sameTime(s.ReadyAt, other.ReadyAt) &&
		sameTime(s.StoppedAt, other.StoppedAt)
}

func deploymentTerminal(phase string) bool {
	return phase == DeploymentPhaseFailed || phase == DeploymentPhaseStopped
}

func methodologyDeploymentStageJob(slug string) string { return slug + "-stage" }
func methodologyModelClaim(slug string) string         { return slug + "-model" }

// methodologyServeEndpoint is the serve Service's in-cluster URL, the only kind lake accepts.
func methodologyServeEndpoint(slug, namespace string) string {
	return "http://" + slug + "." + namespace + ".svc.cluster.local"
}

// asRun gives a deployment's sizing to the helpers runs and deployments share.
func (d *LakeMethodologyDeployment) asRun() *LakeMethodologyRun {
	return &LakeMethodologyRun{ID: d.ID, CustomerID: d.CustomerID, WorkspaceID: d.WorkspaceID,
		MethodologyID: d.MethodologyID, RuntimeImage: d.RuntimeImage, CPUMillis: d.CPUMillis,
		MemoryBytes: d.MemoryBytes, GPUs: d.GPUs, GPUClass: d.GPUClass, Network: d.Network, Slug: d.Slug}
}

func methodologyDeploymentLabels(lake *oramav1alpha1.Lake, deployment *LakeMethodologyDeployment, role string) map[string]string {
	labels := map[string]string{
		"app.kubernetes.io/name":       "methodology-deployment",
		"app.kubernetes.io/instance":   deployment.Slug,
		"app.kubernetes.io/managed-by": "orama-operator",
		methodologyDeploymentIDLabel:   deployment.ID,
		methodologyRunLakeLabel:        lake.Name,
	}
	if role != "" {
		labels[methodologyRunRoleLabel] = role
	}
	if role == methodologyRoleServe {
		labels[methodologyRunNetworkLabel] = methodologyNetworkDenied
		if deployment.Network {
			labels[methodologyRunNetworkLabel] = methodologyNetworkGranted
		}
	}
	return labels
}

// methodologyServeIngressPolicy admits lake's pods, and nothing else, to a serve pod's port.
func methodologyServeIngressPolicy(lake *oramav1alpha1.Lake, namespace string) *networkingv1.NetworkPolicy {
	tcp := corev1.ProtocolTCP
	port := intstr.FromInt32(methodologyServePort)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: methodologyServeIngress, Namespace: namespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				methodologyRunLakeLabel: lake.Name,
				methodologyRunRoleLabel: methodologyRoleServe,
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": lakeNamespace(lake)}},
					PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
						"app.kubernetes.io/name": "lake", "app.kubernetes.io/instance": lake.Name,
					}},
				}},
				Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
			}},
		},
	}
}

// methodologyDeploymentStagePod pages the model in. For a GPU model its zones are where nodes of
// its GPU class are (the model volume's first consumer decides its zone), as for a run.
func methodologyDeploymentStagePod(lake *oramav1alpha1.Lake, deployment *LakeMethodologyDeployment, zones []string) corev1.PodSpec {
	pod := methodologyPod(deployment.RuntimeImage,
		[]string{methodologyCLI, "stage-model", "--lake", "$(LAKE_URL)", "--deployment", "$(DEPLOYMENT_ID)",
			"--credential-file", methodologyCredentialDir + "/" + methodologyCredentialFile, "--out", methodologyModelPath},
		[]corev1.EnvVar{
			{Name: "LAKE_URL", Value: lakeInternalURL(lake, lakeNamespace(lake))},
			{Name: "DEPLOYMENT_ID", Value: deployment.ID},
			{Name: "HOME", Value: "/tmp"},
		},
		methodologyTransferResources(), methodologyModelClaim(deployment.Slug),
		methodologyPullSecret(deployment.Slug), methodologyCredentialSecret(deployment.Slug))
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

// methodologyServePod serves the model: the methodology's own image with nothing that could reach
// a credential, the model volume read-only, a readiness probe on /healthz.
func methodologyServePod(deployment *LakeMethodologyDeployment, image string) corev1.PodSpec {
	pod := methodologyPod(image,
		[]string{methodologyCLI, "serve", "--model", methodologyModelPath, "--source", "/methodology",
			"--port", fmt.Sprint(methodologyServePort)},
		[]corev1.EnvVar{{Name: "HOME", Value: "/tmp"}},
		methodologyRunResources(deployment.asRun()), methodologyModelClaim(deployment.Slug),
		methodologyPullSecret(deployment.Slug), "")
	pod.RestartPolicy = corev1.RestartPolicyAlways
	for i := range pod.Volumes {
		if pod.Volumes[i].PersistentVolumeClaim != nil {
			pod.Volumes[i].PersistentVolumeClaim.ReadOnly = true
		}
	}
	container := &pod.Containers[0]
	for i := range container.VolumeMounts {
		if container.VolumeMounts[i].Name == "work" {
			container.VolumeMounts[i].ReadOnly = true
		}
	}
	container.Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: methodologyServePort, Protocol: corev1.ProtocolTCP}}
	probe := corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromString("http")}}
	container.ReadinessProbe = &corev1.Probe{ProbeHandler: probe, PeriodSeconds: 5, TimeoutSeconds: 3, FailureThreshold: 3}
	container.LivenessProbe = &corev1.Probe{ProbeHandler: probe, InitialDelaySeconds: 30, PeriodSeconds: 20,
		TimeoutSeconds: 5, FailureThreshold: 6}
	if deployment.GPUs > 0 && validGPUClass(deployment.GPUClass) != "" {
		pod.NodeSelector = map[string]string{gpuClassLabel: validGPUClass(deployment.GPUClass)}
	}
	return pod
}

// reconcileMethodologyServe creates or corrects the serve Deployment and its Service.
func (r *LakeReconciler) reconcileMethodologyServe(ctx context.Context, lake *oramav1alpha1.Lake, namespace string,
	deployment *LakeMethodologyDeployment, image string) (*appsv1.Deployment, error) {
	labels := methodologyDeploymentLabels(lake, deployment, methodologyRoleServe)
	selector := map[string]string{methodologyDeploymentIDLabel: deployment.ID, methodologyRunRoleLabel: methodologyRoleServe}
	replicas := int32(1)
	served := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: deployment.Slug, Namespace: namespace}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(served), served); err == nil &&
		(served.Labels[methodologyDeploymentIDLabel] != deployment.ID || !metav1.IsControlledBy(served, lake)) {
		return nil, fmt.Errorf("Deployment %s/%s exists and is not this deployment's", namespace, deployment.Slug)
	} else if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, served, func() error {
		served.Labels = labels
		served.Spec.Replicas = &replicas
		served.Spec.Selector = &metav1.LabelSelector{MatchLabels: selector}
		// One pod at a time: the model volume is ReadWriteOnce.
		served.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		served.Spec.Template = corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
			Spec: methodologyServePod(deployment, image)}
		return controllerutil.SetControllerReference(lake, served, r.Scheme)
	}); err != nil {
		return nil, fmt.Errorf("reconcile serve Deployment %s: %w", deployment.Slug, err)
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: deployment.Slug, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		service.Labels = methodologyDeploymentLabels(lake, deployment, "")
		service.Spec.Type = corev1.ServiceTypeClusterIP
		service.Spec.Selector = selector
		service.Spec.Ports = []corev1.ServicePort{{Name: "http", Port: methodologyServeServicePort,
			TargetPort: intstr.FromString("http"), Protocol: corev1.ProtocolTCP}}
		return controllerutil.SetControllerReference(lake, service, r.Scheme)
	}); err != nil {
		return nil, fmt.Errorf("reconcile serve Service %s: %w", deployment.Slug, err)
	}
	return served, nil
}

// serveFailure is what a serve pod that keeps exiting said, once it has exited more than
// methodologyServeRestartLimit times; empty while it has not.
func (r *LakeReconciler) serveFailure(ctx context.Context, namespace string, deployment *LakeMethodologyDeployment) (string, error) {
	var pods corev1.PodList
	if err := r.reader().List(ctx, &pods, client.InNamespace(namespace),
		client.MatchingLabels{methodologyDeploymentIDLabel: deployment.ID, methodologyRunRoleLabel: methodologyRoleServe}); err != nil {
		return "", err
	}
	for i := range pods.Items {
		for _, container := range pods.Items[i].Status.ContainerStatuses {
			if container.RestartCount < methodologyServeRestartLimit {
				continue
			}
			message := "the model's server stopped " + fmt.Sprint(container.RestartCount) + " times"
			if terminated := container.LastTerminationState.Terminated; terminated != nil {
				message = fmt.Sprintf("the model's server exited with %d", terminated.ExitCode)
				if detail := strings.TrimSpace(terminated.Message); detail != "" {
					if len(detail) > 1000 {
						detail = detail[len(detail)-1000:]
					}
					message += ": " + detail
				}
			}
			return message, nil
		}
	}
	return "", nil
}

// reconcileMethodologyDeployment advances one present deployment and reports what the operator
// observed, and whether it is still changing.
func (r *LakeReconciler) reconcileMethodologyDeployment(ctx context.Context, lake *oramav1alpha1.Lake,
	deployment *LakeMethodologyDeployment, namespace func() (string, error),
	nodes func() ([]corev1.Node, error)) (LakeMethodologyDeploymentStatus, bool, error) {
	status := LakeMethodologyDeploymentStatus{DeploymentID: deployment.ID, ObservedGeneration: deployment.Generation}
	prior := deployment.Status
	if prior != nil {
		status.ReadyAt = prior.ReadyAt
		if deploymentTerminal(prior.Phase) && prior.ObservedGeneration == deployment.Generation {
			done := *prior
			done.CredentialSHA256, done.Endpoint = "", ""
			return done, false, r.removeMethodologyDeployment(ctx, lake, deployment)
		}
	}
	fail := func(message string) (LakeMethodologyDeploymentStatus, bool, error) {
		status.Phase, status.Message, status.CredentialSHA256, status.Endpoint = DeploymentPhaseFailed, message, "", ""
		status.StoppedAt = nowSecond()
		return status, true, r.removeMethodologyDeployment(ctx, lake, deployment)
	}
	switch {
	case !methodologyDeploymentSlugPattern.MatchString(deployment.Slug):
		status.Phase, status.Message = DeploymentPhaseFailed, "slug must match "+methodologyDeploymentSlugPattern.String()
		return status, false, nil
	case !methodologyRunsEnabled(lake):
		status.Phase, status.Message = DeploymentPhaseFailed, "workspace methodologies are not enabled on this Lake (spec.methodologies.runtimeImage)"
		return status, false, nil
	case deployment.RuntimeImage == "":
		status.Phase, status.Message = DeploymentPhaseFailed, "the deployment names no runtime image"
		return status, false, nil
	case deployment.CPUMillis <= 0 || deployment.MemoryBytes <= 0 || deployment.GPUs < 0:
		status.Phase, status.Message = DeploymentPhaseFailed, "the deployment's CPU and memory must be positive"
		return status, false, nil
	}

	image := deployment.Image
	if image == "" {
		build := deployment.Build
		switch {
		case build == nil:
			return fail("the model names no image and no image build")
		case build.Desired == buildDesiredCancelled || build.Phase == BuildPhaseCancelled:
			return fail("the methodology's image build was cancelled")
		case build.Phase == BuildPhaseFailed:
			return fail("the methodology's image build failed")
		case build.Phase != BuildPhaseSucceeded:
			status.Phase, status.Message = DeploymentPhaseBuilding, "waiting for the methodology's image build"
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

	var placement methodologyPlacement
	if deployment.GPUs > 0 {
		listed, err := nodes()
		if err != nil {
			return status, true, err
		}
		placement = methodologyRunPlacement(listed, deployment.asRun())
		placement.Wait = strings.Replace(placement.Wait, "the run starts", "the model is served", 1)
	}
	started := prior != nil && (prior.Phase == DeploymentPhaseStaging || prior.Phase == DeploymentPhaseStarting ||
		prior.Phase == DeploymentPhaseReady)
	if placement.Wait != "" && !started {
		status.Phase, status.Message = DeploymentPhaseQueued, placement.Wait
		return status, true, nil
	}

	ns, err := namespace()
	if err != nil {
		return status, true, err
	}
	labels := methodologyDeploymentLabels(lake, deployment, "")
	credential, err := r.reconcileAgentCredential(ctx, lake, ns, labels, methodologyCredentialSecret(deployment.Slug),
		methodologyDeploymentIDLabel, deployment.ID, "lmd.")
	if err != nil {
		return status, true, err
	}
	status.CredentialSHA256 = credentialDigest(credential)
	if err := r.reconcileMethodologyPullSecretFor(ctx, lake, ns, deployment.Slug, methodologyDeploymentPull, credential, labels); err != nil {
		return status, true, err
	}
	if err := r.reconcileMethodologyClaim(ctx, lake, ns, methodologyModelClaim(deployment.Slug), labels); err != nil {
		return status, true, err
	}

	// 1. stage the model, once.
	stage, err := r.ensureMethodologyDeploymentStage(ctx, lake, ns, deployment, placement.Zones)
	if err != nil {
		return status, true, err
	}
	stageDone := stage == nil || stage.Status.Succeeded > 0
	if stage != nil && stage.Status.Failed > 0 {
		message := "could not bring the model in"
		if detail := jobFailureMessage(stage); detail != "" {
			message += ": " + detail
		}
		return fail(message)
	}
	if !stageDone {
		status.Phase, status.Message = DeploymentPhaseStaging, "bringing the model in"
		return status, true, nil
	}

	// 2. serve it. Its resources are recorded by the workload pass while its pods run.
	served, err := r.reconcileMethodologyServe(ctx, lake, ns, deployment, image)
	if err != nil {
		return status, true, err
	}
	status.Endpoint = methodologyServeEndpoint(deployment.Slug, ns)
	if served.Status.ReadyReplicas > 0 {
		status.Phase, status.Message = DeploymentPhaseReady, "serving"
		if status.ReadyAt == nil {
			status.ReadyAt = nowSecond()
		}
		return status, false, nil
	}
	failure, err := r.serveFailure(ctx, ns, deployment)
	if err != nil {
		return status, true, err
	}
	if failure != "" {
		return fail(failure)
	}
	status.Phase, status.Message = DeploymentPhaseStarting, "starting the model's server"
	return status, true, nil
}

// ensureMethodologyDeploymentStage returns the stage Job, creating it unless the model is already
// on the volume (the serve Deployment exists: the stage ran and its Job may be gone by its TTL).
func (r *LakeReconciler) ensureMethodologyDeploymentStage(ctx context.Context, lake *oramav1alpha1.Lake, namespace string,
	deployment *LakeMethodologyDeployment, zones []string) (*batchv1.Job, error) {
	name := methodologyDeploymentStageJob(deployment.Slug)
	job := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, job); err == nil {
		if job.Labels[methodologyDeploymentIDLabel] != deployment.ID || !metav1.IsControlledBy(job, lake) {
			return nil, fmt.Errorf("Job %s/%s exists and is not this deployment's", namespace, name)
		}
		return job, nil
	} else if !apierrors.IsNotFound(err) {
		return nil, err
	}
	served := &appsv1.Deployment{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: deployment.Slug}, served); err == nil &&
		served.Labels[methodologyDeploymentIDLabel] == deployment.ID {
		return nil, nil
	} else if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	labels := methodologyDeploymentLabels(lake, deployment, methodologyRoleStage)
	backoff, ttl, deadline := int32(0), methodologyJobTTLSeconds, methodologyTransferDeadline
	job = &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: methodologyDeploymentStagePod(lake, deployment, zones)},
		},
	}
	if err := controllerutil.SetControllerReference(lake, job, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, job); err != nil {
		return nil, fmt.Errorf("create Job %s: %w", name, err)
	}
	return job, nil
}

// removeMethodologyDeployment deletes a deployment's stage Job, serve Deployment and Service,
// credential, pull Secret and model volume. Only objects carrying its id and controlled by the
// Lake are touched.
func (r *LakeReconciler) removeMethodologyDeployment(ctx context.Context, lake *oramav1alpha1.Lake, deployment *LakeMethodologyDeployment) error {
	if !methodologyDeploymentSlugPattern.MatchString(deployment.Slug) {
		return nil
	}
	namespace := methodologyRunsNamespace(lake.Name)
	background := metav1.DeletePropagationBackground
	objects := []client.Object{
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: methodologyDeploymentStageJob(deployment.Slug), Namespace: namespace}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: deployment.Slug, Namespace: namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: deployment.Slug, Namespace: namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: methodologyCredentialSecret(deployment.Slug), Namespace: namespace}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: methodologyPullSecret(deployment.Slug), Namespace: namespace}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: methodologyModelClaim(deployment.Slug), Namespace: namespace}},
	}
	for _, object := range objects {
		if err := r.Get(ctx, client.ObjectKeyFromObject(object), object); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return err
		}
		if object.GetLabels()[methodologyDeploymentIDLabel] != deployment.ID || !metav1.IsControlledBy(object, lake) {
			continue
		}
		if err := r.Delete(ctx, object, &client.DeleteOptions{PropagationPolicy: &background}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// reconcileLakeMethodologyDeployments applies one read of methodology_deployments and reports
// whether any is still changing.
func (r *LakeReconciler) reconcileLakeMethodologyDeployments(ctx context.Context, lake *oramav1alpha1.Lake,
	store LakeServiceStore, deployments []LakeMethodologyDeployment) bool {
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
	var nodeList []corev1.Node
	var nodesErr error
	nodesDone := false
	nodes := func() ([]corev1.Node, error) {
		if !nodesDone {
			var listed corev1.NodeList
			nodesErr = r.reader().List(ctx, &listed)
			nodeList, nodesDone = listed.Items, true
			if nodesErr != nil {
				nodesErr = fmt.Errorf("list nodes to place a GPU model: %w", nodesErr)
			}
		}
		return nodeList, nodesErr
	}
	for i := range deployments {
		deployment := &deployments[i]
		var status LakeMethodologyDeploymentStatus
		var err error
		switch deployment.Desired {
		case serviceDesiredAbsent:
			if deployment.Status != nil && deployment.Status.ObservedGeneration == deployment.Generation &&
				deploymentTerminal(deployment.Status.Phase) && deployment.Status.CredentialSHA256 == "" {
				continue
			}
			status = LakeMethodologyDeploymentStatus{DeploymentID: deployment.ID, Phase: DeploymentPhaseStopped,
				Message: "not served", ObservedGeneration: deployment.Generation, StoppedAt: nowSecond()}
			if prior := deployment.Status; prior != nil {
				status.ReadyAt = prior.ReadyAt
				if prior.Phase == DeploymentPhaseFailed {
					status.Phase, status.Message = prior.Phase, prior.Message
				}
				if prior.StoppedAt != nil {
					status.StoppedAt = prior.StoppedAt
				}
			}
			err = r.removeMethodologyDeployment(ctx, lake, deployment)
		case serviceDesiredPresent:
			var still bool
			status, still, err = r.reconcileMethodologyDeployment(ctx, lake, deployment, namespace, nodes)
			changing = changing || still
		default:
			status = LakeMethodologyDeploymentStatus{DeploymentID: deployment.ID, Phase: DeploymentPhaseFailed,
				Message: "desired must be present or absent", ObservedGeneration: deployment.Generation}
		}
		if err != nil {
			log.Error(err, "methodology deployment deferred", "deployment", deployment.ID, "slug", deployment.Slug)
			changing = true
			if status.Phase == "" {
				continue
			}
		}
		if deployment.Status != nil && deployment.Status.equal(status) {
			continue
		}
		if err := store.UpsertMethodologyDeploymentStatus(ctx, status); err != nil {
			log.Error(err, "methodology deployment status write deferred", "deployment", deployment.ID)
			changing = true
			continue
		}
		copied := status
		deployment.Status = &copied
	}
	return changing
}

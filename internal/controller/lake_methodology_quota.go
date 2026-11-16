// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Methodology pods count against their workspace's quota (orama-operator#799).
//
// A workspace methodology's stage, run, collect and serve pods run in the Lake's shared
// methodology-runs namespace, not in the workspace's namespace: a workspace namespace's policies
// allow every pod lake, DNS, the internet and the workspace's services, and NetworkPolicies only add
// what they allow, so the code that is not trusted could not be kept off the network there
// (methodologyRunsNamespace). Kubernetes counts a pod only against the ResourceQuota of its own
// namespace, so without more a workspace's methodologies would use compute beyond its quota.
//
// Before a run or a served model starts, the operator therefore creates a reservation in the
// workspace's own namespace: a pod that is never scheduled (a scheduling gate holds it), carrying
// the largest resources any of the run's or the served model's pods will use. The API server admits
// it exactly as it admits the workspace's apps and services: the workspace's LimitRange applies to
// it and its ResourceQuota counts it, and a pod that does not fit is refused. Refused, the run or
// model waits as Queued and Lake says why; admitted, its pods run in the methodology-runs namespace,
// and the reservation keeps the room taken until they are gone. A gated pod uses no node, pulls no
// image and is never metered (it is never scheduled, and methodology pods count only in the
// methodology-runs namespace). One ResourceQuota on the methodology-runs namespace is the ceiling on
// every workspace's methodologies together.

const (
	// methodologyRoleReservation is the role label of a reservation pod.
	methodologyRoleReservation = "quota-reservation"
	// methodologyReservationGate holds a reservation pod unscheduled for its whole life.
	methodologyReservationGate = "orama.cloud/methodology-quota-reservation"
	// methodologyReservationImage is never pulled: a gated pod is never scheduled.
	methodologyReservationImage = "registry.k8s.io/pause:3.10"
	// methodologyCeilingQuota names the ResourceQuota of the methodology-runs namespace.
	methodologyCeilingQuota = "methodology-ceiling"
)

// methodologyReservationName is `<slug>-quota`, in the workspace's namespace.
func methodologyReservationName(slug string) string { return slug + "-quota" }

// defaultMethodologyNamespaceQuota is the ceiling when spec.methodologies.namespaceQuota is empty.
func defaultMethodologyNamespaceQuota() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceRequestsCPU:                           resource.MustParse("64"),
		corev1.ResourceRequestsMemory:                        resource.MustParse("256Gi"),
		corev1.ResourceLimitsMemory:                          resource.MustParse("256Gi"),
		corev1.ResourceName("requests." + resourceNvidiaGPU): resource.MustParse("8"),
		corev1.ResourcePods:                                  resource.MustParse("500"),
		corev1.ResourcePersistentVolumeClaims:                resource.MustParse("200"),
	}
}

// methodologyNamespaceQuota is the methodology-runs namespace's ceiling.
func methodologyNamespaceQuota(lake *oramav1alpha1.Lake) corev1.ResourceList {
	if len(lake.Spec.Methodologies.NamespaceQuota) > 0 {
		return lake.Spec.Methodologies.NamespaceQuota.DeepCopy()
	}
	return defaultMethodologyNamespaceQuota()
}

// methodologyReservationResources is, per resource, the largest request and the largest limit of
// pods that run one after another: what the workspace must have room for while they run.
func methodologyReservationResources(pods ...corev1.ResourceRequirements) corev1.ResourceRequirements {
	largest := func(into corev1.ResourceList, from corev1.ResourceList) {
		for name, quantity := range from {
			if current, ok := into[name]; !ok || quantity.Cmp(current) > 0 {
				into[name] = quantity.DeepCopy()
			}
		}
	}
	reserved := corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}
	for _, pod := range pods {
		largest(reserved.Requests, pod.Requests)
		largest(reserved.Limits, pod.Limits)
	}
	return reserved
}

// methodologyRunReservation is what a run reserves: its stage and collect pods' resources and its
// sandbox's, whichever is larger, since they run one after another.
func methodologyRunReservation(run *LakeMethodologyRun) corev1.ResourceRequirements {
	return methodologyReservationResources(methodologyTransferResources(), methodologyRunResources(run))
}

// methodologyDeploymentReservation is what a served model reserves: its stage pod's resources and
// its serve pod's, whichever is larger.
func methodologyDeploymentReservation(deployment *LakeMethodologyDeployment) corev1.ResourceRequirements {
	return methodologyReservationResources(methodologyTransferResources(), methodologyRunResources(deployment.asRun()))
}

// methodologyReservationPod is the reservation: restricted Pod Security like every workspace pod,
// no service account token, gated so it is never scheduled.
func methodologyReservationPod(namespace, name string, labels map[string]string, resources corev1.ResourceRequirements) *corev1.Pod {
	noToken, noLinks, nonRoot, noEscalation, readOnlyRoot := false, false, true, false, true
	user := methodologyRunAsUser
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &noToken,
			EnableServiceLinks:           &noLinks,
			SchedulingGates:              []corev1.PodSchedulingGate{{Name: methodologyReservationGate}},
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &nonRoot,
				RunAsUser:      &user,
				RunAsGroup:     &user,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            "reservation",
				Image:           methodologyReservationImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Resources:       resources,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &noEscalation,
					ReadOnlyRootFilesystem:   &readOnlyRoot,
					RunAsNonRoot:             &nonRoot,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
					SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
				},
			}},
		},
	}
}

// methodologyReservation is one run's or served model's reservation request.
type methodologyReservation struct {
	// Namespace is the workspace's namespace.
	Namespace string
	Slug      string
	// IDLabel and ID name the run or deployment: the reservation carries them like its other pods.
	IDLabel   string
	ID        string
	Labels    map[string]string
	Resources corev1.ResourceRequirements
	// What is "run" or "model", for the messages.
	What string
}

// methodologyReservationOutcome: Wait is why it must wait (Queued); Fail is why it can never start.
type methodologyReservationOutcome struct {
	Wait string
	Fail string
}

// reserveMethodologyQuota makes sure the reservation exists in the workspace's namespace. A
// reservation that exists is left as it is: pod resources cannot change, and a run's or a model's
// sizing does not change either. A refusal by the workspace's ResourceQuota or LimitRange is an
// outcome, not an error: a request larger than the quota's whole hard limit fails, anything else
// waits for room.
func (r *LakeReconciler) reserveMethodologyQuota(ctx context.Context, lake *oramav1alpha1.Lake,
	want methodologyReservation) (methodologyReservationOutcome, error) {
	if want.Namespace == "" {
		return methodologyReservationOutcome{Wait: fmt.Sprintf(
			"Queued: the workspace has no namespace yet; the %s starts when it does.", want.What)}, nil
	}
	if len(validation.IsDNS1123Label(want.Namespace)) != 0 {
		return methodologyReservationOutcome{Fail: fmt.Sprintf("the workspace's namespace %q is not a DNS label", want.Namespace)}, nil
	}
	name := methodologyReservationName(want.Slug)
	existing := &corev1.Pod{}
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: want.Namespace, Name: name}, existing); err == nil {
		if existing.Labels[want.IDLabel] != want.ID || !metav1.IsControlledBy(existing, lake) {
			return methodologyReservationOutcome{}, fmt.Errorf("pod %s/%s exists and is not this %s's reservation", want.Namespace, name, want.What)
		}
		if existing.DeletionTimestamp.IsZero() {
			return methodologyReservationOutcome{}, nil
		}
		return methodologyReservationOutcome{Wait: fmt.Sprintf(
			"Queued: the %s's previous quota reservation is still being removed.", want.What)}, nil
	} else if !apierrors.IsNotFound(err) {
		return methodologyReservationOutcome{}, err
	}
	labels := map[string]string{}
	for key, value := range want.Labels {
		labels[key] = value
	}
	labels[methodologyRunRoleLabel] = methodologyRoleReservation
	pod := methodologyReservationPod(want.Namespace, name, labels, want.Resources)
	if err := controllerutil.SetControllerReference(lake, pod, r.Scheme); err != nil {
		return methodologyReservationOutcome{}, err
	}
	err := r.Create(ctx, pod)
	switch {
	case err == nil || apierrors.IsAlreadyExists(err):
		return methodologyReservationOutcome{}, nil
	case apierrors.IsNotFound(err):
		return methodologyReservationOutcome{Wait: fmt.Sprintf(
			"Queued: the workspace's namespace %s does not exist yet; the %s starts when it does.", want.Namespace, want.What)}, nil
	case !apierrors.IsForbidden(err):
		return methodologyReservationOutcome{}, fmt.Errorf("reserve the %s's resources in %s: %w", want.What, want.Namespace, err)
	}
	refusal := apiRefusalMessage(err)
	if strings.Contains(refusal, "cannot create resource") {
		// The operator's own RBAC, not the workspace: an error to retry, never the run's failure.
		return methodologyReservationOutcome{}, fmt.Errorf("reserve the %s's resources in %s: %w", want.What, want.Namespace, err)
	}
	quota := &corev1.ResourceQuota{}
	if qerr := r.reader().Get(ctx, client.ObjectKey{Namespace: want.Namespace, Name: workspaceProfileName}, quota); qerr == nil {
		if over := quotaNeverFits(quota.Spec.Hard, want.Resources); over != "" {
			return methodologyReservationOutcome{Fail: fmt.Sprintf(
				"the %s asks for more than the workspace's whole quota allows (%s)", want.What, over)}, nil
		}
	} else if !apierrors.IsNotFound(qerr) {
		return methodologyReservationOutcome{}, qerr
	}
	if strings.Contains(refusal, "exceeded quota") {
		return methodologyReservationOutcome{Wait: fmt.Sprintf(
			"Queued: the workspace's resource quota has no room for this %s yet (%s); it starts when the workspace's other work frees enough.",
			want.What, refusal)}, nil
	}
	// The LimitRange, or another admission rule of the workspace, refuses the pod outright.
	return methodologyReservationOutcome{Fail: fmt.Sprintf("the workspace's limits refuse this %s: %s", want.What, refusal)}, nil
}

// apiRefusalMessage is the API server's own words for a refusal.
func apiRefusalMessage(err error) string {
	if status, ok := err.(apierrors.APIStatus); ok && status.Status().Message != "" {
		return status.Status().Message
	}
	return err.Error()
}

// quotaNeverFits names the resources for which the reservation alone is above the quota's hard
// limit ("requests.cpu 16 > 4"), or "" when it could fit in an otherwise empty workspace.
func quotaNeverFits(hard corev1.ResourceList, resources corev1.ResourceRequirements) string {
	asked := func(name corev1.ResourceName) (resource.Quantity, bool) {
		text := string(name)
		switch {
		case name == corev1.ResourcePods:
			return resource.MustParse("1"), true
		case strings.HasPrefix(text, "requests."):
			quantity, ok := resources.Requests[corev1.ResourceName(strings.TrimPrefix(text, "requests."))]
			return quantity, ok
		case strings.HasPrefix(text, "limits."):
			quantity, ok := resources.Limits[corev1.ResourceName(strings.TrimPrefix(text, "limits."))]
			return quantity, ok
		case name == corev1.ResourceCPU || name == corev1.ResourceMemory:
			quantity, ok := resources.Requests[name]
			return quantity, ok
		}
		return resource.Quantity{}, false
	}
	var over []string
	for name, limit := range hard {
		quantity, ok := asked(name)
		if ok && quantity.Cmp(limit) > 0 {
			over = append(over, fmt.Sprintf("%s %s > %s", name, quantity.String(), limit.String()))
		}
	}
	sort.Strings(over)
	return strings.Join(over, ", ")
}

// releaseMethodologyQuota deletes a run's or served model's reservation. A namespace that is not a
// DNS label (or empty: the workspace row is gone) has none to delete here; sweepMethodologyReservations
// finds it.
func (r *LakeReconciler) releaseMethodologyQuota(ctx context.Context, lake *oramav1alpha1.Lake, namespace, slug, idLabel, id string) error {
	if namespace == "" || len(validation.IsDNS1123Label(namespace)) != 0 {
		return nil
	}
	pod := &corev1.Pod{}
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: methodologyReservationName(slug)}, pod); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if pod.Labels[idLabel] != id || !metav1.IsControlledBy(pod, lake) || !pod.DeletionTimestamp.IsZero() {
		return nil
	}
	if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// sweepMethodologyReservations deletes this Lake's reservations, of runs (idLabel
// methodologyRunIDLabel) or of served models, whose id is not in the pass's read. Every present
// run or model, and every one still to settle, is in the read; a reservation of any other is left
// over (its workspace row went first, or its release failed before it settled).
func (r *LakeReconciler) sweepMethodologyReservations(ctx context.Context, lake *oramav1alpha1.Lake, idLabel string, read map[string]bool) error {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.MatchingLabels{methodologyRunLakeLabel: lake.Name, methodologyRunRoleLabel: methodologyRoleReservation}); err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		id, ok := pod.Labels[idLabel]
		if !ok || read[id] || !metav1.IsControlledBy(pod, lake) || !pod.DeletionTimestamp.IsZero() {
			continue
		}
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// reconcileMethodologyCeiling keeps the methodology-runs namespace's ResourceQuota.
func (r *LakeReconciler) reconcileMethodologyCeiling(ctx context.Context, lake *oramav1alpha1.Lake, namespace string, labels map[string]string) error {
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: methodologyCeilingQuota, Namespace: namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, quota, func() error {
		quota.Labels = labels
		quota.Spec.Hard = methodologyNamespaceQuota(lake)
		return controllerutil.SetControllerReference(lake, quota, r.Scheme)
	}); err != nil {
		return fmt.Errorf("reconcile ResourceQuota %s/%s: %w", namespace, methodologyCeilingQuota, err)
	}
	return nil
}

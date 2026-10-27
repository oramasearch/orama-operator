/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Applying and pruning what a PixieCloud or PixieVizier renders. Every object is server-side
// applied with the operator's field manager (a no-op write when nothing changed). Namespaced objects
// are controlled by the concrete resource, so deleting it removes them; cluster-scoped RBAC carries
// the pixie label and is removed by the resource's finalizer. An object the operator rendered before
// and no longer renders (the plugin loader Job of an earlier release) is deleted.

// pixiePrunedKinds is every kind a pixie render may contain, which pruning lists by label.
var pixiePrunedKinds = []schema.GroupVersionKind{
	{Group: "apps", Version: "v1", Kind: "Deployment"},
	{Group: "apps", Version: "v1", Kind: "StatefulSet"},
	{Group: "apps", Version: "v1", Kind: "DaemonSet"},
	{Group: "batch", Version: "v1", Kind: "Job"},
	{Group: "", Version: "v1", Kind: "ConfigMap"},
	{Group: "", Version: "v1", Kind: "Service"},
	{Group: "", Version: "v1", Kind: "ServiceAccount"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"},
	{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"},
	{Group: "policy", Version: "v1", Kind: "PodDisruptionBudget"},
}

func pixieClusterScoped(kind string) bool {
	return kind == "ClusterRole" || kind == "ClusterRoleBinding"
}

// toApplyObject converts a typed or unstructured object into the server-side apply body: its
// apiVersion and kind set, its status and creation timestamp dropped.
func toApplyObject(object client.Object, scheme *runtime.Scheme) (*unstructured.Unstructured, error) {
	if u, ok := object.(*unstructured.Unstructured); ok {
		copy := u.DeepCopy()
		delete(copy.Object, "status")
		return copy, nil
	}
	gvk, err := apiutil.GVKForObject(object, scheme)
	if err != nil {
		return nil, err
	}
	data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: data}
	u.SetGroupVersionKind(gvk)
	delete(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	return u, nil
}

// applyPixieObjects applies objects in order and prunes what the previous render had and this one
// does not. owner controls every namespaced object.
func applyPixieObjects(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, managed string, objects []client.Object) error {
	desired := map[string]bool{}
	for _, object := range objects {
		if object.GetNamespace() != "" {
			if err := controllerutil.SetControllerReference(owner, object, scheme); err != nil {
				return err
			}
		}
		body, err := toApplyObject(object, scheme)
		if err != nil {
			return err
		}
		labels := body.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[pixieManagedLabel] = managed
		body.SetLabels(labels)
		desired[pixieObjectID(body.GetKind(), body.GetNamespace(), body.GetName())] = true
		if err := c.Patch(ctx, body, client.Apply, client.FieldOwner(pixieFieldOwner), client.ForceOwnership); err != nil {
			return fmt.Errorf("apply %s %s: %w", body.GetKind(), body.GetName(), err)
		}
	}
	return prunePixieObjects(ctx, c, owner.GetNamespace(), managed, desired)
}

func pixieObjectID(kind, namespace, name string) string { return kind + "/" + namespace + "/" + name }

// prunePixieObjects deletes every object labelled for managed, in namespace or cluster-scoped,
// that is not in keep. A nil keep deletes them all (teardown).
func prunePixieObjects(ctx context.Context, c client.Client, namespace, managed string, keep map[string]bool) error {
	var errs []error
	for _, gvk := range pixiePrunedKinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		options := []client.ListOption{client.MatchingLabels{pixieManagedLabel: managed}}
		if !pixieClusterScoped(gvk.Kind) {
			options = append(options, client.InNamespace(namespace))
		}
		if err := c.List(ctx, list, options...); err != nil {
			if apimeta.IsNoMatchError(err) {
				continue
			}
			errs = append(errs, fmt.Errorf("list %s: %w", gvk.Kind, err))
			continue
		}
		for i := range list.Items {
			item := &list.Items[i]
			if keep[pixieObjectID(gvk.Kind, item.GetNamespace(), item.GetName())] {
				continue
			}
			propagation := metav1.DeletePropagationBackground
			if err := c.Delete(ctx, item, &client.DeleteOptions{PropagationPolicy: &propagation}); err != nil && !apierrors.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("delete %s %s: %w", gvk.Kind, item.GetName(), err))
			}
		}
	}
	return errors.Join(errs...)
}

// pixieWorkloadStatus reads the readiness of each workload a render holds, one entry per
// component, sorted by name.
// pods reads a component's pods, uncached and only for a component that is not ready, to say
// what blocks it.
func pixieWorkloadStatus(ctx context.Context, c client.Reader, pods client.Reader, namespace string, objects []client.Object) ([]oramav1alpha1.ObservabilityComponentStatus, error) {
	var components []oramav1alpha1.ObservabilityComponentStatus
	for _, object := range objects {
		var ready bool
		var message string
		switch object.(type) {
		case *appsv1.Deployment:
			var live appsv1.Deployment
			found, err := getIfExists(ctx, c, namespace, object.GetName(), &live)
			if err != nil {
				return nil, err
			}
			ready, message = deploymentReady(found, &live)
		case *appsv1.StatefulSet:
			var live appsv1.StatefulSet
			found, err := getIfExists(ctx, c, namespace, object.GetName(), &live)
			if err != nil {
				return nil, err
			}
			ready, message = statefulSetReady(found, &live)
		case *appsv1.DaemonSet:
			var live appsv1.DaemonSet
			found, err := getIfExists(ctx, c, namespace, object.GetName(), &live)
			if err != nil {
				return nil, err
			}
			ready, message = daemonSetReady(found, &live)
		default:
			continue
		}
		if !ready && pods != nil {
			blocked, err := podImageProblem(ctx, pods, namespace, object.GetName())
			if err != nil {
				return nil, err
			}
			message = firstNonEmpty(blocked, message)
		}
		components = append(components, oramav1alpha1.ObservabilityComponentStatus{Name: object.GetName(), Ready: ready, Message: message})
	}
	sort.Slice(components, func(i, j int) bool { return components[i].Name < components[j].Name })
	return components, nil
}

func getIfExists(ctx context.Context, c client.Reader, namespace, name string, into client.Object) (bool, error) {
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, into); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// The readiness messages are coarse on purpose: they change on a state transition, not on every
// replica count, so status does not flap (principle 7).
func deploymentReady(found bool, d *appsv1.Deployment) (bool, string) {
	if !found {
		return false, "not created yet"
	}
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	if d.Status.ObservedGeneration < d.Generation || d.Status.UpdatedReplicas < want || d.Status.AvailableReplicas < want {
		return false, "rolling out"
	}
	return true, ""
}

func statefulSetReady(found bool, s *appsv1.StatefulSet) (bool, string) {
	if !found {
		return false, "not created yet"
	}
	want := int32(1)
	if s.Spec.Replicas != nil {
		want = *s.Spec.Replicas
	}
	if s.Status.ObservedGeneration < s.Generation || s.Status.ReadyReplicas < want || s.Status.UpdatedReplicas < want {
		return false, "rolling out"
	}
	return true, ""
}

func daemonSetReady(found bool, d *appsv1.DaemonSet) (bool, string) {
	if !found {
		return false, "not created yet"
	}
	if d.Status.ObservedGeneration < d.Generation || d.Status.DesiredNumberScheduled == 0 ||
		d.Status.NumberAvailable < d.Status.DesiredNumberScheduled || d.Status.UpdatedNumberScheduled < d.Status.DesiredNumberScheduled {
		return false, "not running on every node"
	}
	return true, ""
}

// podImageProblem names an image a component's pods cannot pull, or "". ErrImagePull and
// ImagePullBackOff alternate while a pull is retried; both say the same thing here, so the status
// does not flap. The likeliest cause is a mirror or build that does not exist yet.
func podImageProblem(ctx context.Context, reader client.Reader, namespace, component string) (string, error) {
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{pixieComponentLabel: component}); err != nil {
		return "", err
	}
	var images []string
	for _, pod := range pods.Items {
		for _, status := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
			if waiting := status.State.Waiting; waiting != nil {
				switch waiting.Reason {
				case "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
					images = append(images, status.Image)
				}
			}
		}
	}
	if len(images) == 0 {
		return "", nil
	}
	sort.Strings(images)
	return "cannot pull image " + images[0] + ": it is not in the registry or the pull Secret cannot read it (hack/observability-images.yaml)", nil
}

// summarizeComponents is the phase and "n/total" of a component list, and why it is not Ready.
func summarizeComponents(components []oramav1alpha1.ObservabilityComponentStatus) (phase, ready, message string) {
	n := 0
	var waiting []string
	for _, component := range components {
		if component.Ready {
			n++
		} else {
			waiting = append(waiting, component.Name)
		}
	}
	ready = fmt.Sprintf("%d/%d", n, len(components))
	switch {
	case len(components) > 0 && n == len(components):
		return oramav1alpha1.ObservabilityPhaseReady, ready, ""
	default:
		return oramav1alpha1.ObservabilityPhaseProgressing, ready, "waiting for " + strings.Join(waiting, ", ")
	}
}

// pixieSecretRef is a convenience for a Secret volume.
func pixieSecretVolume(name, secret string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secret}}}
}

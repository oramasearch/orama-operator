/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	jsonpatch "github.com/evanphx/json-patch/v5"
	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
	"github.com/oramasearch/orama-operator/internal/imagepolicy"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func validateWorkloadImages(object client.Object) error {
	fields, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		return err
	}
	// Typed client objects often have no TypeMeta until they reach the API encoder.
	switch object.(type) {
	case *appsv1.Deployment:
		fields["kind"] = "Deployment"
	case *appsv1.StatefulSet:
		fields["kind"] = "StatefulSet"
	case *appsv1.DaemonSet:
		fields["kind"] = "DaemonSet"
	case *batchv1.Job:
		fields["kind"] = "Job"
	case *batchv1.CronJob:
		fields["kind"] = "CronJob"
	case *corev1.Pod:
		fields["kind"] = "Pod"
	case *oramav1alpha1.PostgresSchema:
		fields["kind"] = "PostgresSchema"
	}
	if paths := imagepolicy.MutableImages(fields); len(paths) > 0 {
		return fmt.Errorf("%s/%s: immutable image required at %s", object.GetNamespace(), object.GetName(), strings.Join(paths, ", "))
	}
	return nil
}

// Check the fully rendered object immediately before each write. New init containers, sidecars,
// CronJobs and child-worker image env cannot bypass enforcement by choosing another renderer.
type imageEnforcingClient struct {
	client.Client
	violations *[]string
}

func (c imageEnforcingClient) validate(object client.Object) error {
	err := validateWorkloadImages(object)
	if err != nil && c.violations != nil {
		*c.violations = append(*c.violations, err.Error())
	}
	return err
}

func (c imageEnforcingClient) Create(ctx context.Context, object client.Object, opts ...client.CreateOption) error {
	if err := c.validate(object); err != nil {
		return err
	}
	return c.Client.Create(ctx, object, opts...)
}
func (c imageEnforcingClient) Update(ctx context.Context, object client.Object, opts ...client.UpdateOption) error {
	if err := c.validate(object); err != nil {
		return err
	}
	return c.Client.Update(ctx, object, opts...)
}
func (c imageEnforcingClient) Patch(ctx context.Context, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
	// Validate the result of the patch, including fields absent from the supplied object.
	current := object.DeepCopyObject().(client.Object)
	if err := c.Client.Get(ctx, client.ObjectKeyFromObject(object), current); err != nil {
		return err
	}
	before, err := json.Marshal(current)
	if err != nil {
		return err
	}
	data, err := patch.Data(object)
	if err != nil {
		return err
	}
	var after []byte
	switch patch.Type() {
	case types.MergePatchType:
		var fields map[string]interface{}
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		if fields == nil {
			return fmt.Errorf("image enforcement requires an object merge patch")
		}
		metadata, _ := fields["metadata"].(map[string]interface{})
		if metadata == nil {
			metadata = map[string]interface{}{}
			fields["metadata"] = metadata
		}
		if version, _ := metadata["resourceVersion"].(string); version == "" {
			metadata["resourceVersion"] = current.GetResourceVersion()
		}
		data, err = json.Marshal(fields)
		if err != nil {
			return err
		}
		after, err = jsonpatch.MergePatch(before, data)
	case types.JSONPatchType:
		var entries []map[string]interface{}
		if err := json.Unmarshal(data, &entries); err != nil {
			return err
		}
		entries = append([]map[string]interface{}{{"op": "test", "path": "/metadata/resourceVersion", "value": current.GetResourceVersion()}}, entries...)
		data, err = json.Marshal(entries)
		if err != nil {
			return err
		}
		var operations jsonpatch.Patch
		operations, err = jsonpatch.DecodePatch(data)
		if err == nil {
			after, err = operations.Apply(before)
		}
	default:
		return fmt.Errorf("image enforcement does not support patch type %s", patch.Type())
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(after, current); err != nil {
		return err
	}
	if err := c.validate(current); err != nil {
		return err
	}

	// The API must apply the patch to exactly the version validated above.
	return c.Client.Patch(ctx, object, client.RawPatch(patch.Type(), data), opts...)
}

// Report actual owned workloads, including existing immutable Jobs. Warnings do not block the
// live production stack; enforcement also rejects an unchanged, previously-created tagged object.
func (r *OramaCloudV1Reconciler) reportCloudV1Images(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, attempts ...string) error {
	lists := []client.ObjectList{&appsv1.DeploymentList{}, &appsv1.StatefulSetList{}, &batchv1.JobList{}, &batchv1.CronJobList{}, &oramav1alpha1.PostgresSchemaList{}}
	clusters := &unstructured.UnstructuredList{}
	clusters.SetGroupVersionKind(cnpgClusterGVK.GroupVersion().WithKind("ClusterList"))
	lists = append(lists, clusters)
	failures := append([]string(nil), attempts...)
	for _, list := range lists {
		if err := r.List(ctx, list, client.InNamespace(oc.Namespace)); err != nil {
			return err
		}
		objects, err := meta.ExtractList(list)
		if err != nil {
			return err
		}
		for _, object := range objects {
			child, ok := object.(client.Object)
			if !ok {
				continue
			}
			owner := metav1.GetControllerOf(child)
			if owner == nil || owner.Kind != "OramaCloudV1" || owner.Name != oc.Name || owner.UID != oc.UID {
				continue
			}
			if err := validateWorkloadImages(child); err != nil {
				failures = append(failures, err.Error())
			}
		}
	}
	sort.Strings(failures)
	condition := metav1.Condition{Type: "ImagesPinned", Status: metav1.ConditionTrue, Reason: "ImagesPinned", Message: "all owned executable image selections use SHA-256 digests"}
	if len(failures) > 0 {
		condition.Status, condition.Reason = metav1.ConditionFalse, "ImagesNotPinned"
		condition.Message = strings.Join(failures, "; ")
	}
	previous := meta.FindStatusCondition(oc.Status.Conditions, condition.Type)
	changed := previous == nil || previous.Status != condition.Status || previous.Message != condition.Message
	r.setStatus(ctx, oc, oc.Status.Phase, oc.Status.ComponentsReady, oc.Status.Components, nil, condition)
	if changed && condition.Status == metav1.ConditionFalse && r.Recorder != nil {
		r.Recorder.Event(oc, corev1.EventTypeWarning, condition.Reason, condition.Message)
	}
	if len(failures) > 0 && oc.Spec.Images.RequireDigests {
		return fmt.Errorf("ImagesNotPinned: %s", condition.Message)
	}
	return nil
}

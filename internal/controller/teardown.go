/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// teardownByComponent deletes the operator-owned resources for the given component label(s) when a
// component's toggle flips off — disable ≠ leave running (#23). Owner-GC only fires on CR deletion,
// so a disabled component's children would otherwise linger (often CrashLoopBackOff). Idempotent —
// missing resources are ignored, so enable→disable→enable round-trips cleanly. StatefulSet PVCs are
// intentionally left behind (data preserved); only the workloads + their Services / ConfigMaps /
// Secrets / PDBs / Ingresses / Jobs / HPAs are removed.
func (r *OramaCloudV1Reconciler) teardownByComponent(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, components ...string) error {
	for _, comp := range components {
		sel := client.MatchingLabels{
			"app.kubernetes.io/managed-by": "orama-cloud-v1-operator",
			"app.kubernetes.io/instance":   oc.Name,
			"app.kubernetes.io/component":  comp,
		}
		ns := client.InNamespace(oc.Namespace)
		// List+Delete per kind (not DeleteAllOf — core Services don't support DeleteCollection).
		for _, list := range []client.ObjectList{
			&appsv1.DeploymentList{}, &appsv1.StatefulSetList{}, &corev1.ServiceList{},
			&corev1.ConfigMapList{}, &corev1.SecretList{}, &policyv1.PodDisruptionBudgetList{},
			&networkingv1.IngressList{}, &batchv1.JobList{}, &batchv1.CronJobList{}, &autoscalingv2.HorizontalPodAutoscalerList{},
		} {
			if err := r.List(ctx, list, ns, sel); err != nil {
				return fmt.Errorf("teardown %s: list: %w", comp, err)
			}
			items, err := apimeta.ExtractList(list)
			if err != nil {
				return err
			}
			for _, it := range items {
				obj := it.(client.Object)
				if err := r.deleteCloudV1Owned(ctx, oc, obj); err != nil {
					return fmt.Errorf("teardown %s/%s: %w", comp, obj.GetName(), err)
				}
			}
		}
	}
	return nil
}

// teardownControlDB removes the CNPG Cluster and its ScheduledBackup when the control DB is
// disabled. CNPG's own reclaim policy governs the underlying PVC; the operator just removes the
// Cluster CR + the component's owned resources. Under spec.deletionProtection the Cluster and
// ScheduledBackup are kept (and released from the CR's ownership, so a later CR deletion can't
// garbage-collect them either); re-enabling the control DB adopts the same Cluster again.
func (r *OramaCloudV1Reconciler) teardownControlDB(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	cluster := &unstructured.Unstructured{}
	cluster.SetGroupVersionKind(cnpgClusterGVK)
	cluster.SetName(controlDBClusterName(oc))
	cluster.SetNamespace(oc.Namespace)
	scheduledBackup := newControlDBScheduledBackup(controlDBScheduledBackupName(oc), oc.Namespace)
	for _, obj := range []*unstructured.Unstructured{scheduledBackup, cluster} {
		if oc.Spec.DeletionProtection {
			if err := r.releaseFromCR(ctx, oc, obj); err != nil {
				return fmt.Errorf("retain controldb %s: %w", obj.GetKind(), err)
			}
			continue
		}
		if err := r.deleteControlDBOwned(ctx, oc, obj); err != nil {
			return fmt.Errorf("teardown controldb %s: %w", obj.GetKind(), err)
		}
	}
	return r.teardownByComponent(ctx, oc, "controldb")
}

// releaseFromCR removes the CR's owner reference from an existing object (no-op when it is absent
// or already unowned).
func (r *OramaCloudV1Reconciler) releaseFromCR(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, obj *unstructured.Unstructured) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	before := obj.DeepCopy()
	if err := r.ownUnlessProtected(oc, obj); err != nil {
		return err
	}
	if equality.Semantic.DeepEqual(obj.GetOwnerReferences(), before.GetOwnerReferences()) &&
		equality.Semantic.DeepEqual(obj.GetAnnotations(), before.GetAnnotations()) {
		return nil
	}
	return r.Update(ctx, obj)
}

func (r *OramaCloudV1Reconciler) deleteControlDBOwned(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, obj client.Object) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		return client.IgnoreNotFound(err)
	}
	if err := checkControlDBOwnership(oc, obj); err != nil {
		return err
	}
	uid, version := obj.GetUID(), obj.GetResourceVersion()
	return client.IgnoreNotFound(r.Delete(ctx, obj, client.Preconditions{UID: &uid, ResourceVersion: &version}))
}

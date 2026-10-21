/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// Operator-owned Karpenter install — the second slice of operator#69 (extends the NodePool work in
// operator#72). When spec.capacity.karpenter.install is set, the operator OWNS the Karpenter controller
// install itself (its Deployment + ServiceAccount + RBAC + the Karpenter CRDs), rather than assuming
// Karpenter is already present. It server-side-applies the pinned UPSTREAM Helm-rendered manifests as
// `unstructured` objects (embedded via go:embed) so it stays decoupled from Karpenter's Go types/version
// — the same decoupling the NodePool reconcile uses. Cloud-gated: a clean no-op on bare-metal / k0s (no
// cloud provider ⇒ no VM API to drive) and on AKS (Node Auto Provisioning already manages Karpenter);
// the self-install path targets EKS. See docs/research/karpenter-manual.md + docs/components.md.

//go:embed assets/karpenter/v1.13.0/crds.yaml
var karpenterCRDManifests []byte

//go:embed assets/karpenter/v1.13.0/controller.yaml
var karpenterControllerManifests []byte

const (
	// karpenterInstallVersion is the pinned upstream Karpenter version whose manifests are vendored under
	// assets/karpenter/ and re-vendored by hack/fetch-karpenter-manifests.sh. Keep in lockstep with the
	// go:embed paths above (both the CRD chart and the controller chart pin to this).
	karpenterInstallVersion = "1.13.0"
	// karpenterNamespace is where the upstream chart installs the controller (kube-system, so the
	// controller runs on non-Karpenter capacity — the manual's rule that Karpenter must not depend on
	// capacity it provisions).
	karpenterNamespace = "kube-system"

	// installConditionType is the CR status condition reporting the operator-owned install state.
	installConditionType = "KarpenterInstalled"
	// installFinalizer guards deletion of the CLUSTER-SCOPED install resources (CRDs, ClusterRoles,
	// ClusterRoleBindings) a namespaced CR cannot own via ownerReferences — same reason as
	// capacityFinalizer. Added while install is on, released once the shared controller is released.
	installFinalizer = "orama.cloud/karpenter-install"
	// karpenterInstallManagedLabel marks every object the operator applies for the Karpenter install, so
	// the install is identifiable independently of the (Helm-shaped) labels the upstream manifests carry.
	karpenterInstallManagedLabel = "orama.cloud/karpenter-install"
	// karpenterFieldOwner is the server-side-apply field manager for the install objects.
	karpenterFieldOwner = "orama-cloud-v1-operator-karpenter"

	// The placeholders, the expected object sets and the renderer that sets the two CR values on the
	// parsed bundle live in zetta_capacity_install_render.go (operator#79).
)

// capacityInstallRequested reports whether the operator should own the Karpenter install for this CR.
func capacityInstallRequested(z *oramav1alpha1.OramaZettaCluster) bool {
	return z.Spec.Capacity != nil && z.Spec.Capacity.Karpenter != nil && z.Spec.Capacity.Karpenter.Install
}

// reconcileKarpenterInstall owns the (cloud-gated) Karpenter controller install. It returns the
// KarpenterInstalled condition to fold into the CR status (nil ⇒ install is off, drop the condition).
//
// It also owns the install finalizer: added while install is on (so the cluster-scoped CRDs/ClusterRoles
// are cleaned up), released once no CR still requests the shared install. Deletion is handled by the
// caller (finalizeKarpenterInstall) before the CR's owned children are GC'd.
func (r *OramaZettaClusterReconciler) reconcileKarpenterInstall(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) (*metav1.Condition, error) {
	log := logf.FromContext(ctx)

	if !capacityInstallRequested(z) {
		// Install off (or cleared). Only ever touched the cluster while the finalizer is present, so a CR
		// that never used install does ZERO install API calls here. If the finalizer IS present, install
		// was on: release it (ref-counted — the shared controller is kept if another CR still wants it).
		if controllerutil.ContainsFinalizer(z, installFinalizer) {
			if err := r.teardownKarpenterInstall(ctx, z); err != nil {
				return nil, err
			}
			controllerutil.RemoveFinalizer(z, installFinalizer)
			if err := r.Update(ctx, z); err != nil {
				return nil, err
			}
		}
		return nil, nil // no condition — a cluster that never installs keeps a clean status
	}

	// Refuse a malformed cluster name or role ARN before anything else, on every cloud. The CRD pattern
	// rejects them at admission, but a CR stored before the pattern existed (or written past validation)
	// still reaches here (operator#79).
	if err := validateKarpenterInstallValues(z.Spec.Capacity.Karpenter.InstallClusterName, z.Spec.Capacity.Karpenter.ControllerRoleARN); err != nil {
		log.Info("Karpenter install refused: invalid spec", "reason", err.Error())
		return installBlocked("InvalidInstallSpec", "operator-owned Karpenter install refused, nothing applied: "+err.Error()), nil
	}

	// The Karpenter controller is one per cluster, and its IRSA role decides which cloud account it
	// acts in. Only a CR in the operator's own namespace may set it; a tenant's CR, in a workspace
	// namespace, could otherwise point it at a role in the tenant's account (operator#280).
	if !r.installNamespaceAllowed(z) {
		log.Info("Karpenter install refused: CR is not in the operator's namespace", "namespace", z.Namespace)
		return installBlocked("InstallNamespaceNotAllowed", fmt.Sprintf(
			"operator-owned Karpenter install refused, nothing applied: only an OramaZettaCluster in the operator's namespace %q may install the cluster-wide Karpenter controller; this one is in %q",
			r.OperatorNamespace, z.Namespace)), nil
	}
	// Several such CRs must agree: the first (oldest) one's values hold and a conflicting one is
	// refused, never last-writer-wins.
	if owner, err := r.conflictingInstallOwner(ctx, z); err != nil {
		return nil, err
	} else if owner != nil {
		log.Info("Karpenter install refused: conflicts with another CR", "owner", owner.Namespace+"/"+owner.Name)
		return installBlocked("ConflictingInstallSpec", fmt.Sprintf(
			"operator-owned Karpenter install refused, nothing applied: %s/%s already installs Karpenter with a different installClusterName or controllerRoleARN; make them equal or clear install on one of them",
			owner.Namespace, owner.Name)), nil
	}

	// Cloud gate: only self-install on a cloud that both has a VM API and isn't already managing Karpenter.
	cloud, err := r.detectCloudProvider(ctx)
	if err != nil {
		return nil, err
	}
	switch cloud {
	case "":
		log.Info("Karpenter install requested but no cloud provider detected — no-op (bare-metal/k0s)")
		return installBlocked("BareMetalNoCloud",
			"operator-owned Karpenter install skipped — no cloud provider detected (bare-metal / k0s has no VM API for Karpenter to drive); spec.replicas still packs pods onto fixed hosts"), nil
	case "azure":
		return installBlocked("ManagedByAKSNAP",
			"operator-owned Karpenter install skipped on AKS — enable Node Auto Provisioning (NAP), which deploys and manages Karpenter for you, then set spec.capacity.karpenter.enabled (not install)"), nil
	case "aws":
		// proceed — EKS is the self-managed install path.
	default:
		return installBlocked("UnsupportedCloud",
			fmt.Sprintf("operator-owned Karpenter install supports EKS; detected cloud %q is unsupported — install Karpenter out-of-band and use spec.capacity.karpenter.enabled", cloud)), nil
	}

	kc := z.Spec.Capacity.Karpenter

	// Cloud IAM prerequisite: the controller identity the operator cannot mint (a cloud-account prereq,
	// like the image pull secret). Surface a clear condition and DON'T deploy the controller (which would
	// crash-loop without it) — see docs/karpenter-cloud-iam-prerequisite.md.
	if kc.ControllerRoleARN == "" {
		return installBlocked("CloudIdentityMissing",
			"spec.capacity.karpenter.controllerRoleARN is required on EKS — Karpenter's controller needs an IAM role (IRSA) permitted to create/terminate EC2 instances; the operator cannot mint cloud IAM (a cloud-account prerequisite, like the image pull secret). See docs/karpenter-cloud-iam-prerequisite.md"), nil
	}
	if kc.InstallClusterName == "" {
		return installBlocked("InstallClusterNameRequired",
			"spec.capacity.karpenter.installClusterName is required on EKS — Karpenter's controller uses it (settings.clusterName) to discover the cluster's tagged subnets/security-groups/interruption queue"), nil
	}

	// Ensure the finalizer BEFORE creating the cluster-scoped CRDs/ClusterRoles, so a delete that races
	// the create still cleans them up.
	if !controllerutil.ContainsFinalizer(z, installFinalizer) {
		controllerutil.AddFinalizer(z, installFinalizer)
		if err := r.Update(ctx, z); err != nil {
			return nil, err
		}
	}

	// Render both bundles before touching the cluster, so a bundle that fails its checks applies nothing.
	crds, err := karpenterCRDObjects()
	if err != nil {
		return nil, fmt.Errorf("decode Karpenter CRDs: %w", err)
	}
	bundle, err := renderKarpenterController(kc.InstallClusterName, kc.ControllerRoleARN)
	if err != nil {
		return nil, fmt.Errorf("render Karpenter controller: %w", err)
	}

	// 1. CRDs (create/update; never GC'd on teardown — deleting a Karpenter CRD cascades to every
	//    NodePool/NodeClaim cluster-wide, and upstream keeps CRDs out of normal chart GC too).
	if err := r.applyKarpenterObjects(ctx, crds); err != nil {
		return nil, fmt.Errorf("apply Karpenter CRDs: %w", err)
	}
	// 2. Controller bundle (ServiceAccount + RBAC + Deployment + Service + PDB), with the two
	//    cluster-specific values set on the parsed objects.
	if err := r.applyKarpenterObjects(ctx, bundle); err != nil {
		return nil, fmt.Errorf("apply Karpenter controller: %w", err)
	}

	ready, err := r.karpenterControllerReady(ctx)
	if err != nil {
		return nil, err
	}
	if ready {
		return &metav1.Condition{
			Type: installConditionType, Status: metav1.ConditionTrue, Reason: "Installed",
			Message: fmt.Sprintf("operator-managed Karpenter %s installed and available (%s/karpenter)", karpenterInstallVersion, karpenterNamespace),
		}, nil
	}
	return &metav1.Condition{
		Type: installConditionType, Status: metav1.ConditionFalse, Reason: "InstallProgressing",
		Message: fmt.Sprintf("operator-managed Karpenter %s applied; controller Deployment not yet Available", karpenterInstallVersion),
	}, nil
}

// installBlocked builds a KarpenterInstalled=False condition for a gated / prerequisite-missing state.
func installBlocked(reason, msg string) *metav1.Condition {
	return &metav1.Condition{Type: installConditionType, Status: metav1.ConditionFalse, Reason: reason, Message: msg}
}

// detectCloudProvider classifies the cluster's cloud from a node's providerID: "aws" (EKS), "azure"
// (AKS), "gce"/other for a recognised-but-unsupported cloud, or "" for bare-metal / k0s (no cloud
// providerID). Read via the uncached APIReader so the operator holds no cluster-wide Node informer.
func (r *OramaZettaClusterReconciler) detectCloudProvider(ctx context.Context) (string, error) {
	var nodes corev1.NodeList
	if err := r.reader().List(ctx, &nodes); err != nil {
		return "", err
	}
	for i := range nodes.Items {
		pid := nodes.Items[i].Spec.ProviderID
		switch {
		case strings.HasPrefix(pid, "aws://"):
			return "aws", nil
		case strings.HasPrefix(pid, "azure://"):
			return "azure", nil
		case strings.HasPrefix(pid, "gce://"):
			return "gce", nil
		case pid != "":
			// A providerID that isn't one of the known clouds (e.g. a cloud we don't self-install for).
			if scheme, _, ok := strings.Cut(pid, "://"); ok && scheme != "" {
				return scheme, nil
			}
		}
	}
	return "", nil // no node carries a cloud providerID ⇒ bare-metal / k0s
}

// applyKarpenterObjects server-side-applies each object, stamping the operator's management labels.
// Force-ownership so the operator takes over any fields the upstream Helm release previously managed;
// the write is a no-op when nothing changed. Callers pass only checked bundles (karpenterCRDObjects,
// renderKarpenterController).
func (r *OramaZettaClusterReconciler) applyKarpenterObjects(ctx context.Context, objs []*unstructured.Unstructured) error {
	for _, obj := range objs {
		labels := obj.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		// We are the manager now (the upstream manifests carry app.kubernetes.io/managed-by: Helm).
		labels["app.kubernetes.io/managed-by"] = "orama-cloud-v1-operator"
		labels[karpenterInstallManagedLabel] = "true"
		obj.SetLabels(labels)

		if err := r.Patch(ctx, obj, client.Apply, client.FieldOwner(karpenterFieldOwner), client.ForceOwnership); err != nil {
			return fmt.Errorf("apply %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
		}
	}
	return nil
}

// isEmptyManifestDoc reports whether a decoded YAML document should be skipped: an empty doc (a bare
// `---` or comment-only chunk) or one that isn't a Kubernetes object (no apiVersion/kind). The latter
// guards against non-manifest lines a manifest source might carry (e.g. Helm's OCI `Pulled:`/`Digest:`
// preamble) reaching a Patch/Delete call.
func isEmptyManifestDoc(obj *unstructured.Unstructured) bool {
	return len(obj.Object) == 0 || obj.GetAPIVersion() == "" || obj.GetKind() == ""
}

// karpenterControllerReady reports whether the installed Karpenter controller Deployment is Available.
func (r *OramaZettaClusterReconciler) karpenterControllerReady(ctx context.Context) (bool, error) {
	var dep appsv1.Deployment
	if err := r.reader().Get(ctx, client.ObjectKey{Namespace: karpenterNamespace, Name: "karpenter"}, &dep); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return dep.Status.AvailableReplicas > 0, nil
}

// teardownKarpenterInstall removes the operator-managed Karpenter controller resources (ServiceAccount,
// RBAC, Deployment, Service, PDB) when NO OramaZettaCluster still requests the install — the controller
// is a cluster singleton shared by every CR that opts in, so it's ref-counted. The Karpenter CRDs are
// deliberately NOT deleted (deleting a CRD cascades to every NodePool/NodeClaim in the cluster; upstream
// likewise keeps CRDs out of normal chart GC). Tolerant of a missing CRD / already-gone object.
func (r *OramaZettaClusterReconciler) teardownKarpenterInstall(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) error {
	log := logf.FromContext(ctx)
	others, err := r.otherClustersRequestInstall(ctx, z)
	if err != nil {
		return err
	}
	if others {
		log.Info("Karpenter install still requested by another OramaZettaCluster — leaving the shared controller in place")
		return nil
	}
	return r.deleteKarpenterController(ctx)
}

// deleteKarpenterController deletes each object of the controller bundle (by GVK + name + namespace),
// tolerating a missing CRD (NoMatch) or an already-absent object. The unrendered template is enough:
// deletion uses only the identity fields, which hold no CR value.
func (r *OramaZettaClusterReconciler) deleteKarpenterController(ctx context.Context) error {
	objs, err := karpenterControllerTemplate()
	if err != nil {
		return err
	}
	for _, obj := range objs {
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) && !apimeta.IsNoMatchError(err) {
			return fmt.Errorf("delete %s %s/%s: %w", obj.GetKind(), obj.GetNamespace(), obj.GetName(), err)
		}
	}
	return nil
}

// otherClustersRequestInstall reports whether any OTHER (non-deleting) OramaZettaCluster still requests
// the operator-owned Karpenter install — the ref-count that keeps the shared controller alive while any
// consumer remains.
func (r *OramaZettaClusterReconciler) otherClustersRequestInstall(ctx context.Context, self *oramav1alpha1.OramaZettaCluster) (bool, error) {
	var list oramav1alpha1.OramaZettaClusterList
	if err := r.reader().List(ctx, &list); err != nil {
		return false, err
	}
	for i := range list.Items {
		c := &list.Items[i]
		if c.UID == self.UID || !c.DeletionTimestamp.IsZero() {
			continue
		}
		if capacityInstallRequested(c) && r.installNamespaceAllowed(c) {
			return true, nil
		}
	}
	return false, nil
}

// installNamespaceAllowed reports whether z may own the operator-managed Karpenter install: it is in
// the operator's own namespace. Without a known operator namespace no CR may (fail closed).
func (r *OramaZettaClusterReconciler) installNamespaceAllowed(z *oramav1alpha1.OramaZettaCluster) bool {
	return r.OperatorNamespace != "" && z.Namespace == r.OperatorNamespace
}

// conflictingInstallOwner returns the oldest other (non-deleting) allowed CR requesting the install
// with a different cluster name or role ARN than z, when that CR is older than z (creation time,
// then namespace/name). nil when z's values hold.
func (r *OramaZettaClusterReconciler) conflictingInstallOwner(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) (*oramav1alpha1.OramaZettaCluster, error) {
	var list oramav1alpha1.OramaZettaClusterList
	if err := r.reader().List(ctx, &list); err != nil {
		return nil, err
	}
	older := func(a, b *oramav1alpha1.OramaZettaCluster) bool {
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	}
	mine := z.Spec.Capacity.Karpenter
	var owner *oramav1alpha1.OramaZettaCluster
	for i := range list.Items {
		c := &list.Items[i]
		if c.UID == z.UID || !c.DeletionTimestamp.IsZero() || !capacityInstallRequested(c) || !r.installNamespaceAllowed(c) {
			continue
		}
		theirs := c.Spec.Capacity.Karpenter
		if theirs.InstallClusterName == mine.InstallClusterName && theirs.ControllerRoleARN == mine.ControllerRoleARN {
			continue
		}
		if older(c, z) && (owner == nil || older(c, owner)) {
			owner = c
		}
	}
	return owner, nil
}

// finalizeKarpenterInstall is the delete-path cleanup: release the shared Karpenter install (if this was
// the last consumer) then drop the finalizer so the CR (and its owner-GC'd children) can be removed.
func (r *OramaZettaClusterReconciler) finalizeKarpenterInstall(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) error {
	if !controllerutil.ContainsFinalizer(z, installFinalizer) {
		return nil
	}
	if err := r.teardownKarpenterInstall(ctx, z); err != nil {
		return err
	}
	controllerutil.RemoveFinalizer(z, installFinalizer)
	return r.Update(ctx, z)
}

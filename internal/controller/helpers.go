/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/rand"
	"encoding/hex"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

const appName = "orama-cloud-v1"

// labelsFor returns the standard label set for a child object of a given component.
func labelsFor(oc *oramav1alpha1.OramaCloudV1, component string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":       appName,
		"app.kubernetes.io/instance":   oc.Name,
		"app.kubernetes.io/component":  component,
		"app.kubernetes.io/managed-by": "orama-cloud-v1-operator",
		"orama.cloud/region":           oc.Spec.Region.Name,
	}
}

func int32Ptr(i int32) *int32 { return &i }
func int64Ptr(i int64) *int64 { return &i }
func boolPtr(b bool) *bool    { return &b }

// hardenedSecurityContext is the conservative CONTAINER securityContext the operator applies to
// EVERY workload it creates (issue #18): no privilege escalation, drop ALL Linux capabilities, and
// the RuntimeDefault seccomp profile. This baseline is safe for essentially all modern images (it
// doesn't force a non-root user or a read-only root filesystem). The stronger `runAsNonRoot` /
// `readOnlyRootFilesystem` knobs break images that expect root or write to `/`, so they're rolled
// out per-component with testing — NOT applied blanket here. Also satisfies the cluster's Gatekeeper
// / Pod Security "baseline" expectations out of the box.
func hardenedSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// hardenedSecurityContextNetBind is the baseline PLUS CAP_NET_BIND_SERVICE — for a container that
// binds a PRIVILEGED port (<1024, e.g. Envoy / pgAdmin on :80). Dropping ALL capabilities strips
// NET_BIND_SERVICE even from a root process, so a privileged-port bind fails ("permission denied")
// unless it's added back. Adding just this one cap keeps the least-privilege posture (issue #18 —
// exactly the "a blanket securityContext can break a healthy stack" case the audit warns about).
func hardenedSecurityContextNetBind() *corev1.SecurityContext {
	sc := hardenedSecurityContext()
	sc.Capabilities.Add = []corev1.Capability{"NET_BIND_SERVICE"}
	return sc
}

// containerSecurityContext picks the right baseline for a container by its bind port: the
// NET_BIND_SERVICE variant for privileged ports (<1024), the plain baseline otherwise.
func containerSecurityContext(port int32) *corev1.SecurityContext {
	if port > 0 && port < 1024 {
		return hardenedSecurityContextNetBind()
	}
	return hardenedSecurityContext()
}

// dataStoreSecurityContext is the hardening baseline for a STATEFUL store whose image runs a ROOT
// entrypoint that chown's the mounted data volume and then drops to a non-root service user — redis,
// rabbitmq, clickhouse, and mongo all do this. Plain drop-ALL strips CAP_CHOWN / CAP_SETUID and the
// entrypoint crash-loops ("chown: Operation not permitted") — this was caught by the #18 on-cluster
// smoke test. So we drop ALL and add back ONLY the file-ownership + setuid capabilities the
// entrypoint needs, while still dropping the dangerous ones (NET_RAW/NET_ADMIN/SYS_*) and keeping
// allowPrivilegeEscalation=false + seccomp RuntimeDefault. (gosu/su-exec drops privileges via
// SETUID/SETGID; dropping privileges is not blocked by allowPrivilegeEscalation=false.)
func dataStoreSecurityContext() *corev1.SecurityContext {
	sc := hardenedSecurityContext()
	sc.Capabilities.Add = []corev1.Capability{"CHOWN", "DAC_OVERRIDE", "FOWNER", "SETGID", "SETUID"}
	return sc
}

// harden sets the conservative securityContext on a container in place (unless the caller already
// set one). Used for both the main containers and the operator's init/render/wait containers so the
// whole pod meets the baseline.
func harden(c *corev1.Container) {
	if c.SecurityContext == nil {
		c.SecurityContext = hardenedSecurityContext()
	}
}

// hardenAll applies harden() to every container in the slice (in place).
func hardenAll(cs []corev1.Container) {
	for i := range cs {
		harden(&cs[i])
	}
}

// spreadConstraints keeps a replicated Deployment's pods off the same node so one node failing
// doesn't take out every replica. WhenUnsatisfiable=ScheduleAnyway makes it a SOFT preference: a
// single-node sandbox still schedules all replicas, while a real multi-node cluster honors the
// spread. Applied only to replicated (>1) workloads (issue #18).
func spreadConstraints(lbls map[string]string) []corev1.TopologySpreadConstraint {
	return []corev1.TopologySpreadConstraint{{
		MaxSkew:           1,
		TopologyKey:       "kubernetes.io/hostname",
		WhenUnsatisfiable: corev1.ScheduleAnyway,
		LabelSelector:     &metav1.LabelSelector{MatchLabels: lbls},
	}}
}

// resourcesOr returns the CR-provided resource override when set, else the operator's sensible
// per-component default. Lets every component's requests/limits be tuned via the CR (like the engine
// writer/reader already are) while staying right-sized out of the box (issue #18, principle #8).
func resourcesOr(override, def *corev1.ResourceRequirements) *corev1.ResourceRequirements {
	if override != nil {
		return override
	}
	return def
}

// imagePullSecrets returns the pull secret (if any) for pods, so private-registry images
// (e.g. the private Docker Hub repos built by the bridge's build/images tooling) can be
// pulled. nil when spec.images.pullSecretName is unset (public images need nothing).
func imagePullSecrets(oc *oramav1alpha1.OramaCloudV1) []corev1.LocalObjectReference {
	if oc.Spec.Images.PullSecretName == "" {
		return nil
	}
	return []corev1.LocalObjectReference{{Name: oc.Spec.Images.PullSecretName}}
}

// applyPVCRetention sets Retain/Retain on a StatefulSet's PVC retention policy when the CR has
// spec.deletionProtection, so neither CR deletion nor a scale-down ever deletes its volumes even on
// a cluster whose default differs. Without protection the field is left as the API server has it.
func applyPVCRetention(oc *oramav1alpha1.OramaCloudV1, sts *appsv1.StatefulSet) {
	if !oc.Spec.DeletionProtection {
		return
	}
	sts.Spec.PersistentVolumeClaimRetentionPolicy = &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
		WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
	}
}

// componentImage resolves the image for a component: an explicit override wins,
// otherwise registry/tag defaults (published oramasearch/* images).
func componentImage(oc *oramav1alpha1.OramaCloudV1, key, defaultRepo string) string {
	if o := oc.Spec.Images.Overrides[key]; o != "" {
		return o
	}
	reg := oc.Spec.Images.Registry
	if reg == "" {
		reg = "oramasearch"
	}
	tag := oc.Spec.Images.Tag
	if tag == "" {
		tag = "latest"
	}
	return reg + "/" + defaultRepo + ":" + tag
}

// embeddingsImage resolves the OramaCore engine ai-server (embeddings, gRPC :50051) sidecar. This
// is the PUBLISHED oramacore-ai-server image (the engine's own ai-server) — the CUDA build for a
// GPU writer, else the base build (which auto-falls back to CPUExecutionProvider on a CPU node).
// It is NOT embeddings-server-py: that's the separate HPES / hosted-search (index-manager) path.
// A reused published image, so it uses :latest, not the global spec.images.tag. Override wins.
func embeddingsImage(oc *oramav1alpha1.OramaCloudV1) string {
	if o := oc.Spec.Images.Overrides["embeddings"]; o != "" {
		return o
	}
	reg := oc.Spec.Images.Registry
	if reg == "" {
		reg = "oramasearch"
	}
	if oc.Spec.Components.Engine.Writer.GPU {
		return reg + "/oramacore-ai-server-cuda:latest"
	}
	return reg + "/oramacore-ai-server:latest"
}

// oramacoreImage resolves the OramaCore engine image. OramaCore is a REUSED published image, so it
// defaults to :latest (its published tag) rather than the global spec.images.tag — that tag is for
// the operator's OWN built images (which don't include oramacore). An explicit
// spec.images.overrides["oramacore"] wins; pin a version there in production.
func oramacoreImage(oc *oramav1alpha1.OramaCloudV1) string {
	if o := oc.Spec.Images.Overrides["oramacore"]; o != "" {
		return o
	}
	reg := oc.Spec.Images.Registry
	if reg == "" {
		reg = "oramasearch"
	}
	return reg + "/oramacore:latest"
}

func strPtr(s string) *string { return &s }

// randHex returns a cryptographically-random hex string of 2*nbytes characters (e.g. randHex(32) →
// a 64-char token). Used to mint bearer/secret material the operator provisions.
func randHex(nbytes int) (string, error) {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// deleteIfExists deletes an object, treating an already-absent object as success — for tearing down
// an optional child (e.g. an Ingress/DNS record) when its enabling spec field is cleared.
func deleteIfExists(ctx context.Context, c client.Client, obj client.Object) error {
	if err := c.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// ingressClassName is the IngressClass the operator binds its Ingresses to — the
// helmfile-installed ingress-nginx (which fronts the cluster LoadBalancer). TODO: make
// configurable via a spec field if a cluster uses a different controller.
func ingressClassName(_ *oramav1alpha1.OramaCloudV1) string { return "nginx" }

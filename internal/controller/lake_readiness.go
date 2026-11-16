// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

type lakeComponentReadiness struct {
	phase   string
	reason  string
	message string
}

// Match LakeEnvironment's generation and replica checks, and surface terminal rollout failures.
func lakeDeploymentReadiness(deployment *appsv1.Deployment, desired int32) lakeComponentReadiness {
	if deployment.Status.ObservedGeneration < deployment.Generation {
		return lakeComponentReadiness{
			"Progressing", "DeploymentProgressing", "waiting for the Deployment controller to observe the latest change",
		}
	}
	for _, condition := range deployment.Status.Conditions {
		if condition.Type == appsv1.DeploymentReplicaFailure && condition.Status == corev1.ConditionTrue {
			return lakeComponentReadiness{"Degraded", "ReplicaFailure", "Deployment replica failure: " + condition.Message}
		}
		if condition.Type == appsv1.DeploymentProgressing && condition.Status == corev1.ConditionFalse &&
			condition.Reason == "ProgressDeadlineExceeded" {
			return lakeComponentReadiness{"Degraded", "ProgressDeadlineExceeded", "Deployment progress deadline exceeded: " + condition.Message}
		}
	}
	if desired > 0 && deploymentRolledOut(deployment, desired) {
		return lakeComponentReadiness{phase: "Ready"}
	}
	return lakeComponentReadiness{
		"Progressing", "DeploymentProgressing",
		fmt.Sprintf("waiting for %d Deployment replicas: %d ready, %d updated, %d available",
			desired, deployment.Status.ReadyReplicas, deployment.Status.UpdatedReplicas, deployment.Status.AvailableReplicas),
	}
}

// Mirror the schema's reported status while checking that it applies to the current desired schema.
// desiredVersion is the version the Lake's image needs (lakeDesiredSchemaVersion); "" while the
// image's manifest is being read, when the PostgresSchema's own version counts.
func (r *LakeReconciler) lakeSchemaStatus(
	ctx context.Context,
	lake *oramav1alpha1.Lake,
	namespace string,
	desiredVersion string,
) (string, string, lakeComponentReadiness) {
	if !lakePostgresEnabled(lake) {
		return "", "", lakeComponentReadiness{phase: "Ready"}
	}
	var ps oramav1alpha1.PostgresSchema
	key := client.ObjectKey{Namespace: namespace, Name: lakeControlDB(lake.Name) + "-schema"}
	if err := r.Get(ctx, key, &ps); err != nil {
		return "Pending", "", lakeComponentReadiness{"Progressing", "SchemaPending", "waiting for the owned PostgresSchema status"}
	}
	phase := ps.Status.Phase
	if phase == "" {
		phase = "Pending"
	}
	state := lakeComponentReadiness{"Progressing", "SchemaPending", "waiting for PostgresSchema " + ps.Name + " to be Applied"}
	switch {
	case ps.Status.ObservedGeneration != ps.Generation:
		state.reason = "SchemaNotCurrent"
		state.message = "waiting for the schema controller to observe the latest PostgresSchema generation"
	case phase == "Failed":
		state = lakeComponentReadiness{"Degraded", "SchemaFailed", "PostgresSchema " + ps.Name + " failed; inspect its migration Job"}
		if ps.Status.Message != "" {
			state.message += ": " + ps.Status.Message
		}
	case phase == "Applied" && ((desiredVersion != "" && ps.Status.AppliedVersion != desiredVersion) ||
		ps.Status.AppliedVersion != ps.Spec.Version ||
		!apimeta.IsStatusConditionTrue(ps.Status.Conditions, "Applied")):
		state.reason = "SchemaNotCurrent"
		state.message = "waiting for PostgresSchema " + ps.Name + " to apply the current schema version"
	case phase == "Applied":
		state = lakeComponentReadiness{phase: "Ready"}
	}
	return phase, ps.Status.AppliedVersion, state
}

// Stable input order breaks ties; any failed component takes precedence over pending work.
func lakeAggregateReadiness(lake *oramav1alpha1.Lake, components []lakeComponentReadiness) (string, metav1.Condition) {
	state := lakeComponentReadiness{"Ready", "LakeReady", "all required Lake components are ready"}
	for _, component := range components {
		if component.phase == "Degraded" {
			state = component
			break
		}
		if state.phase == "Ready" && component.phase != "Ready" {
			state = component
		}
	}
	status := metav1.ConditionFalse
	if state.phase == "Ready" {
		status = metav1.ConditionTrue
	}
	return state.phase, metav1.Condition{
		Type:               "Ready",
		Status:             status,
		ObservedGeneration: lake.Generation,
		Reason:             state.reason,
		Message:            state.message,
	}
}

// deploymentRolledOut is whether a Deployment's rollout to its current template is complete, as
// kubectl rollout status decides: desired replicas of the new template, every one available, and no
// replica of an old template left. A surge rollout keeps the old pod serving while the new one
// starts; ready, updated and available counts all reach desired then, before the new version
// answers (orama-operator#1049: an App read Ready and its page reloaded onto the old version).
func deploymentRolledOut(deployment *appsv1.Deployment, desired int32) bool {
	status := deployment.Status
	return status.UpdatedReplicas >= desired &&
		status.ReadyReplicas >= desired &&
		status.AvailableReplicas >= desired &&
		status.AvailableReplicas >= status.UpdatedReplicas &&
		status.Replicas <= status.UpdatedReplicas
}

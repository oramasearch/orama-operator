/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// The control DB's oramacore_clusters row (operator#443). The dashboard places a new project on the
// row ORAMACORE_PLACEMENT_CLUSTER_ID names (else the newest), creates its collection on that row's
// writer_url with its master_api_key, and prefixes the read key with its cluster_prefix. A control DB that has the
// schema but no row cannot create a project, so once the schema is applied the operator runs one Job
// that inserts this stack's row when the table is empty. It never updates or deletes a row: a
// control DB with any row (a copy of Prod 1's, or the one an earlier run inserted) is left as it is.

// controlDBClusterPrefix is the inserted row's cluster_prefix. The Cloudflare worker routes reads by
// the read key's prefix and knows only c1.
const controlDBClusterPrefix = "c1"

// clusterRowReadyCondition reports the cluster-row Job on the OramaCloudV1 status.
const clusterRowReadyCondition = "ClusterRowReady"

func controlDBClusterRowJobName(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-controldb-cluster-row"
}

// controlDBClusterRowApplies reports whether this stack inserts its own cluster row: an in-cluster
// control DB the operator applies the schema to (not external, not a read-tier replica) in a stack
// that runs the engine the row points at.
func controlDBClusterRowApplies(oc *oramav1alpha1.OramaCloudV1) bool {
	return controlDBInCluster(oc) && oc.Spec.Region.Role != "read" && oc.Spec.Components.Engine.Enabled
}

// controlDBClusterRowEnv is the row's non-secret columns, derived from the spec, as the Job's env.
// URLs use the public hosts and fall back to the in-cluster Services when a host is unset. The
// replica count and regions are NOT NULL columns the dashboard does not read; they describe the stack.
func controlDBClusterRowEnv(oc *oramav1alpha1.OramaCloudV1) []corev1.EnvVar {
	engine := oc.Spec.Components.Engine
	writerURL := "http://" + oc.Name + "-writer:8080"
	if engine.Writer.Host != "" {
		writerURL = "https://" + engine.Writer.Host
	}
	readerURL := "http://" + oc.Name + "-reader"
	if engine.Reader.Host != "" {
		readerURL = "https://" + engine.Reader.Host
	}
	replicas := engine.Reader.Replicas
	if replicas == 0 {
		replicas = 3 // the reader StatefulSet's default
	}
	regions, _ := json.Marshal([]string{oc.Spec.Region.Name}) // a []string always marshals
	return []corev1.EnvVar{
		{Name: "CLUSTER_NAME", Value: oc.Name},
		{Name: "CLUSTER_PREFIX", Value: controlDBClusterPrefix},
		{Name: "CLUSTER_WRITER_URL", Value: writerURL},
		{Name: "CLUSTER_READER_URL", Value: readerURL},
		{Name: "CLUSTER_READER_REPLICAS", Value: strconv.Itoa(int(replicas))},
		{Name: "CLUSTER_REGIONS", Value: string(regions)},
	}
}

// controlDBClusterRowScript inserts the row only into an empty oramacore_clusters, under a table lock
// so a concurrent writer cannot add a row between the check and the insert. psql reads every value
// from the environment through a backtick \set, so no value is in a process's argv or in the output.
func controlDBClusterRowScript() string {
	vars := []struct{ name, env string }{
		{"name", "CLUSTER_NAME"}, {"prefix", "CLUSTER_PREFIX"}, {"writer_url", "CLUSTER_WRITER_URL"},
		{"reader_url", "CLUSTER_READER_URL"}, {"reader_replicas", "CLUSTER_READER_REPLICAS"},
		{"regions", "CLUSTER_REGIONS"}, {"master_api_key", "MASTER_API_KEY"},
	}
	var sets strings.Builder
	for _, v := range vars {
		fmt.Fprintf(&sets, "\\set %s `printf %%s \"$%s\"`\n", v.name, v.env)
	}
	return `set -eu
echo "cluster-row: waiting for the database..."
until psql "$DATABASE_URL" -c 'SELECT 1' >/dev/null 2>&1; do sleep 3; done
inserted=$(psql "$DATABASE_URL" -X -q -tA -v ON_ERROR_STOP=1 <<'SQL'
` + sets.String() + `BEGIN;
LOCK TABLE oramacore_clusters IN SHARE ROW EXCLUSIVE MODE;
WITH inserted AS (
  INSERT INTO oramacore_clusters (name, writer_url, reader_url, cluster_reader_replica_count,
    cluster_writer_count, cluster_reader_regions, cluster_writer_regions, master_api_key, cluster_prefix)
  SELECT :'name', :'writer_url', :'reader_url', :'reader_replicas'::integer,
    1, :'regions'::jsonb, :'regions'::jsonb, :'master_api_key', :'prefix'
  WHERE NOT EXISTS (SELECT 1 FROM oramacore_clusters)
  RETURNING 1
) SELECT count(*) FROM inserted;
COMMIT;
SQL
)
case "$inserted" in
  1) echo "cluster-row: inserted the $CLUSTER_PREFIX oramacore_clusters row (writer $CLUSTER_WRITER_URL)" ;;
  0) echo "cluster-row: oramacore_clusters already has a row; left unchanged" ;;
  *) echo "cluster-row: unexpected insert count: $inserted"; exit 1 ;;
esac`
}

// controlDBClusterRowJob is the one-shot Job that inserts the row. It reads the connection from the
// CNPG owner Secret and the key from the stack's master API key Secret.
func controlDBClusterRowJob(oc *oramav1alpha1.OramaCloudV1) *batchv1.Job {
	lbls := labelsFor(oc, "controldb")
	nodeSelector, tolerations := podPlacement(oc)
	env := append(controlDBClusterRowEnv(oc),
		secretKeyEnv("DATABASE_URL", controlDBAppSecret(oc), "uri"),
		masterAPIKeyEnv(oc))
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: controlDBClusterRowJobName(oc), Namespace: oc.Namespace, Labels: lbls},
		Spec: batchv1.JobSpec{
			BackoffLimit: int32Ptr(20), // retry while the database comes up
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: lbls},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeSelector:  nodeSelector,
					Tolerations:   tolerations,
					Containers: []corev1.Container{{
						Name:            "cluster-row",
						Image:           "postgres:16-alpine", // ships psql, as the schema Job
						Command:         []string{"sh", "-c", controlDBClusterRowScript()},
						Env:             env,
						Resources:       *resourceReqs("64Mi", "25m", "256Mi"),
						SecurityContext: hardenedSecurityContext(),
					}},
				},
			},
		},
	}
}

// reconcileControlDBClusterRow creates the cluster-row Job once the control DB schema is applied.
// The Job is created once and never replaced: re-running it would change nothing, because the
// insert only fills an empty table.
func (r *OramaCloudV1Reconciler) reconcileControlDBClusterRow(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if !controlDBClusterRowApplies(oc) {
		return nil
	}
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: controlDBClusterRowJobName(oc)}, job)
	switch {
	case err == nil:
		if !metav1.IsControlledBy(job, oc) {
			return fmt.Errorf("cluster-row Job %s is not controlled by %s", job.Name, oc.Name)
		}
		return nil
	case !apierrors.IsNotFound(err):
		return err
	}
	ps := &oramav1alpha1.PostgresSchema{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + "-controldb-schema"}, ps); err != nil {
		return client.IgnoreNotFound(err)
	}
	if ps.Status.Phase != "Applied" {
		return nil // the owned PostgresSchema's status change requeues this CR
	}
	job = controlDBClusterRowJob(oc)
	if err := controllerutil.SetControllerReference(oc, job, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, job)
}

// controlDBClusterRowCondition is the ClusterRowReady condition: True once the Job succeeded (the
// control DB has a row, inserted by the Job or already there; its log says which), False while it
// waits for the schema, runs, or failed. nil when the stack inserts no row, so the condition is dropped.
func (r *OramaCloudV1Reconciler) controlDBClusterRowCondition(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (*metav1.Condition, error) {
	if !controlDBClusterRowApplies(oc) {
		return nil, nil
	}
	name := controlDBClusterRowJobName(oc)
	cond := metav1.Condition{Type: clusterRowReadyCondition, Status: metav1.ConditionFalse}
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: name}, job)
	switch {
	case apierrors.IsNotFound(err):
		cond.Reason, cond.Message = "WaitingForSchema", "waiting for the control DB schema before inserting the "+controlDBClusterPrefix+" oramacore_clusters row"
	case err != nil:
		return nil, err
	case job.Status.Succeeded > 0:
		cond.Status, cond.Reason = metav1.ConditionTrue, "RowPresent"
		cond.Message = "the control DB has an oramacore_clusters row; Job " + name + " inserts the " + controlDBClusterPrefix + " row only into an empty table (its log says which)"
	case jobFailed(job):
		cond.Reason, cond.Message = "JobFailed", "Job "+name+" failed; inspect its pod logs"
	default:
		cond.Reason, cond.Message = "Inserting", "Job "+name+" is running"
	}
	return &cond, nil
}

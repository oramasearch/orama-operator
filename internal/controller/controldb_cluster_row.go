/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

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
// writer_url with its master_api_key, mints the writer's JWT audience from writer_url, and prefixes
// the read key with its cluster_prefix. A control DB that has the schema but no row cannot create a
// project. Cluster rows are never copied from Prod 1, so the operator's c1 row is the only one a
// stack has. Once the schema is applied the operator runs a Job
// that inserts the row when the table is empty. It never updates or deletes a row: a control DB
// with any row is left as it is.

// controlDBClusterPrefix is the inserted row's cluster_prefix. The Cloudflare worker routes reads by
// the read key's prefix and knows only c1, so customers' read keys keep working.
const controlDBClusterPrefix = "c1"

// clusterRowReadyCondition reports the cluster-row Job on the OramaCloudV1 status.
const clusterRowReadyCondition = "ClusterRowReady"

// clusterRowHashKey is the Job annotation holding the hash of its pod template. A Job that has not
// succeeded and was built from another template (a new image override, writer host or key Secret)
// is replaced at once.
const clusterRowHashKey = "orama.cloud/cluster-row-hash"

// clusterRowIDKey is the succeeded Job's annotation holding the id of the control DB's live c1 row,
// or clusterRowNone when it has none (operator#572). The Job's script reads the id back after the
// insert and writes it to its termination message (clusterRowTerminationPath); the operator copies it
// onto the Job, the record it keeps, and gives it to the dashboard as ORAMACORE_PLACEMENT_CLUSTER_ID,
// so the dashboard places new projects on the row the Job wrote without the operator reading the
// database itself.
const (
	clusterRowIDKey           = "orama.cloud/cluster-row-id"
	clusterRowNone            = "none"
	clusterRowTerminationPath = "/dev/termination-log"
)

// clusterRowIDPattern is a Postgres uuid as text, lower-case.
var clusterRowIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// controlDBClientLabel names a control DB client pod that is neither an application component nor a
// PostgresSchema migration, so the network sandbox can admit it to control Postgres (operator#648).
// The cluster-row Job's pod carries it with the value controlDBClusterRowClient.
const (
	controlDBClientLabel      = "orama.cloud/controldb-client"
	controlDBClusterRowClient = "cluster-row"
)

// clusterRowRetryDelay is how long the operator waits after the Job failed before it replaces it.
// Inside one Job the pod is retried clusterRowBackoffLimit times with the Job controller's
// exponential back-off (10s doubling, capped at 6m); a Job that still fails is replaced this long
// after its failure, so a broken database is retried at most about every quarter hour, and never
// needs the Job deleted by hand.
const clusterRowRetryDelay = 5 * time.Minute

const (
	clusterRowBackoffLimit   = 6
	clusterRowActiveDeadline = 30 * 60 // seconds; a pod stuck pulling or waiting fails the Job, which is then retried
)

func controlDBClusterRowJobName(oc *oramav1alpha1.OramaCloudV1) string {
	return oc.Name + "-controldb-cluster-row"
}

// controlDBClusterRowApplies reports whether this stack inserts its own cluster row: an enabled
// in-cluster control DB on a region that is not a read tier, in a stack that runs the engine the
// row points at.
func controlDBClusterRowApplies(oc *oramav1alpha1.OramaCloudV1) bool {
	db := oc.Spec.Components.ControlDB
	return db.Enabled && db.Mode != "external" && oc.Spec.Region.Role != "read" && oc.Spec.Components.Engine.Enabled
}

// controlDBClusterRowMissingHosts names the public hosts the row needs and the stack does not
// publish, or "" when both are published. The row is written once and never updated, so it waits for
// both instead of recording an in-cluster URL that would stay in the control DB: the dashboard mints
// the writer's JWT audience from writer_url, and an in-cluster one is never an audience the writer
// accepts.
func controlDBClusterRowMissingHosts(oc *oramav1alpha1.OramaCloudV1) string {
	var missing []string
	if writerPublicHost(oc) == "" {
		missing = append(missing, "spec.components.engine.writer.host")
	}
	if readerPublicHost(oc) == "" {
		missing = append(missing, "spec.components.engine.reader.host")
	}
	return strings.Join(missing, " and ")
}

// controlDBClusterRowEnv is the row's non-secret columns, derived from the spec, as the Job's env.
// URLs are the public hosts (writerPublicHost, readerPublicHost); the Job is created only once both
// are published (controlDBClusterRowMissingHosts). writer_url is https://<engine.writer.host>, the
// audience the writer already requires in engine.writer.jwt.audiences (validateWriterJWT). The
// replica count and regions are NOT NULL columns the dashboard does not read; they describe the stack.
func controlDBClusterRowEnv(oc *oramav1alpha1.OramaCloudV1) []corev1.EnvVar {
	writerURL := "https://" + writerPublicHost(oc)
	readerURL := "https://" + readerPublicHost(oc)
	replicas := oc.Spec.Components.Engine.Reader.Replicas
	if replicas == 0 {
		replicas = 3 // the CRD default
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

// controlDBClusterRowScript inserts the row only into an empty oramacore_clusters, soft-deleted rows
// included, under a table lock so a concurrent writer cannot add a row between the check and the
// insert. It is safe to run again: a second run finds the row and changes nothing, and a run cut off
// mid-transaction (a database restart) rolls back whole. It then reads back the id of the newest live
// row with the c1 prefix, inserted now or found, and writes it, or clusterRowNone, to its termination
// message (#572). psql reads every value from the environment
// through a backtick \set, so no value is in a process's argv or in the output.
func controlDBClusterRowScript() string {
	vars := []struct{ name, env string }{
		{"name", "CLUSTER_NAME"}, {"prefix", "CLUSTER_PREFIX"}, {"writer_url", "CLUSTER_WRITER_URL"},
		{"reader_url", "CLUSTER_READER_URL"}, {"reader_replicas", "CLUSTER_READER_REPLICAS"},
		{"regions", "CLUSTER_REGIONS"}, {"master_api_key", masterAPIKeyEnvName},
	}
	var sets strings.Builder
	for _, v := range vars {
		fmt.Fprintf(&sets, "\\set %s `printf %%s \"$%s\"`\n", v.name, v.env)
	}
	return `set -eu
echo "cluster-row: waiting for the database..."
until psql "$DATABASE_URL" -c 'SELECT 1' >/dev/null 2>&1; do sleep 3; done
out=$(psql "$DATABASE_URL" -X -q -tA -v ON_ERROR_STOP=1 <<'SQL'
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
SELECT coalesce((SELECT id::text FROM oramacore_clusters WHERE cluster_prefix = :'prefix' AND deleted_at IS NULL
  ORDER BY created_at DESC LIMIT 1), 'none');
SQL
)
inserted=$(printf '%s\n' "$out" | sed -n 1p)
row_id=$(printf '%s\n' "$out" | sed -n 2p)
case "$inserted" in
  1) echo "cluster-row: inserted the $CLUSTER_PREFIX oramacore_clusters row (writer $CLUSTER_WRITER_URL)" ;;
  0) echo "cluster-row: oramacore_clusters already has a row; left unchanged" ;;
  *) echo "cluster-row: unexpected insert count: $inserted"; exit 1 ;;
esac
case "$row_id" in
  none) echo "cluster-row: no live $CLUSTER_PREFIX row; the dashboard places new projects on the newest row" ;;
  ????????-????-????-????-????????????) echo "cluster-row: the $CLUSTER_PREFIX row is $row_id" ;;
  *) echo "cluster-row: unexpected row id: $row_id"; exit 1 ;;
esac
printf '%s' "$row_id" > ` + clusterRowTerminationPath
}

// controlDBClusterRowJob is the Job that inserts the row, annotated with the hash of its pod
// template. It reads the connection from the CNPG owner Secret and the key from the Secret
// spec.masterAPIKeySecretRef names, and runs the postgres-client image. Its pod keeps the controldb
// component label, which teardown deletes it by, and adds controlDBClientLabel, which the network
// sandbox admits to control Postgres.
func controlDBClusterRowJob(oc *oramav1alpha1.OramaCloudV1) *batchv1.Job {
	lbls := labelsFor(oc, "controldb")
	lbls[controlDBClientLabel] = controlDBClusterRowClient
	env := append(controlDBClusterRowEnv(oc), pgURIEnv(oc, "DATABASE_URL"), masterAPIKeyEnv(oc))
	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: lbls},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: boolPtr(false),
			ImagePullSecrets:             imagePullSecrets(oc),
			Containers: []corev1.Container{{
				Name:            "cluster-row",
				Image:           postgresClientImage(oc), // ships psql
				Command:         []string{"sh", "-c", controlDBClusterRowScript()},
				Env:             env,
				Resources:       *resourceReqs("64Mi", "25m", "256Mi"),
				SecurityContext: hardenedSecurityContext(),
				// The row id the script writes (#572); the kubelet's default path, set explicitly.
				TerminationMessagePath:   clusterRowTerminationPath,
				TerminationMessagePolicy: corev1.TerminationMessageReadFile,
			}},
		},
	}
	boundV1PodStorage(&template.Spec)
	raw, _ := json.Marshal(template) // a PodTemplateSpec always marshals
	sum := sha256.Sum256(raw)
	// Identity changes apply to new Jobs without replaying existing one-shot database writes.
	cloudV1PodIdentity(&template.Spec, controlDBClusterRowJobName(oc))
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: controlDBClusterRowJobName(oc), Namespace: oc.Namespace, Labels: lbls,
			Annotations: map[string]string{clusterRowHashKey: hex.EncodeToString(sum[:])}},
		Spec: batchv1.JobSpec{
			BackoffLimit:          int32Ptr(clusterRowBackoffLimit),
			ActiveDeadlineSeconds: int64Ptr(clusterRowActiveDeadline),
			Template:              template,
		},
	}
}

// clusterRowRetryAt is when the operator replaces a failed cluster-row Job: clusterRowRetryDelay
// after its Failed condition. A Failed condition without a time is retried at once.
func clusterRowRetryAt(job *batchv1.Job) time.Time {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime.Add(clusterRowRetryDelay)
		}
	}
	return time.Time{}
}

// reconcileControlDBClusterRow owns the cluster-row Job. It creates the Job once the control DB
// schema is applied and the master key is valid (never with a missing or placeholder key, since the
// row is written once). A succeeded Job is kept as the record and never re-run. A Job that has not
// succeeded and was built from another template is replaced at once; a failed Job is replaced
// clusterRowRetryDelay after its failure. Both deletions are foreground with preconditions, so two
// runs never overlap, and the next reconcile creates the new Job; the 20-second readiness requeue
// keeps reconciling while the condition is not True.
func (r *OramaCloudV1Reconciler) reconcileControlDBClusterRow(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) error {
	if !controlDBClusterRowApplies(oc) {
		return nil
	}
	want := controlDBClusterRowJob(oc)
	if err := ensureCloudV1ServiceAccount(ctx, r.Client, r.Scheme, oc, want.Name, want.Labels); err != nil {
		return err
	}
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKeyFromObject(want), job)
	switch {
	case apierrors.IsNotFound(err):
		ready, err := r.controlDBClusterRowPrerequisites(ctx, oc)
		if err != nil || !ready {
			return err
		}
		if err := controllerutil.SetControllerReference(oc, want, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, want)
	case err != nil:
		return err
	case !metav1.IsControlledBy(job, oc):
		return fmt.Errorf("cluster-row Job %s is not controlled by OramaCloudV1 %s", job.Name, oc.Name)
	case !job.DeletionTimestamp.IsZero():
		return nil
	case job.Status.Succeeded > 0:
		return r.recordClusterRowID(ctx, job)
	case job.Annotations[clusterRowHashKey] != want.Annotations[clusterRowHashKey],
		jobFailed(job) && !time.Now().Before(clusterRowRetryAt(job)):
		policy := metav1.DeletePropagationForeground
		err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy,
			Preconditions: &metav1.Preconditions{UID: &job.UID, ResourceVersion: &job.ResourceVersion}})
		return client.IgnoreNotFound(err)
	}
	return nil
}

// recordClusterRowID copies the row id a succeeded Job's pod reported onto the Job (clusterRowIDKey),
// once. A succeeded Job whose pod reported no id (one built before #572, or whose pod is gone) is
// replaced, foreground and with preconditions, so the next reconcile runs the script again: on a
// control DB that has its row the run inserts nothing and only reports the id.
func (r *OramaCloudV1Reconciler) recordClusterRowID(ctx context.Context, job *batchv1.Job) error {
	if _, ok := job.Annotations[clusterRowIDKey]; ok {
		return nil
	}
	id, err := r.clusterRowIDFromPods(ctx, job)
	if err != nil {
		return err
	}
	if id == "" {
		policy := metav1.DeletePropagationForeground
		err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy,
			Preconditions: &metav1.Preconditions{UID: &job.UID, ResourceVersion: &job.ResourceVersion}})
		return client.IgnoreNotFound(err)
	}
	patch := client.MergeFrom(job.DeepCopy())
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Annotations[clusterRowIDKey] = id
	return r.Patch(ctx, job, patch)
}

// clusterRowIDFromPods is the row id, or clusterRowNone, that a succeeded pod of job wrote to its
// termination message, or "" when no pod of the Job reported a valid one. Pods are listed uncached.
func (r *OramaCloudV1Reconciler) clusterRowIDFromPods(ctx context.Context, job *batchv1.Job) (string, error) {
	var pods corev1.PodList
	if err := r.secretReader().List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return "", fmt.Errorf("list the pods of Job %s: %w", job.Name, err)
	}
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodSucceeded || !slices.ContainsFunc(p.OwnerReferences, func(o metav1.OwnerReference) bool { return o.UID == job.UID }) {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			t := cs.State.Terminated
			if cs.Name != "cluster-row" || t == nil || t.ExitCode != 0 {
				continue
			}
			if msg := strings.TrimSpace(t.Message); msg == clusterRowNone || clusterRowIDPattern.MatchString(msg) {
				return msg, nil
			}
		}
	}
	return "", nil
}

// clusterRowID is the id of the stack's c1 row the cluster-row Job recorded (clusterRowIDKey), or ""
// when the stack inserts no row, the Job has not recorded one yet, or the control DB has no live c1
// row. The dashboard then gets no ORAMACORE_PLACEMENT_CLUSTER_ID and places new projects on the newest
// row, which is the c1 row on a stack whose only row it is.
func (r *OramaCloudV1Reconciler) clusterRowID(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (string, error) {
	if !controlDBClusterRowApplies(oc) {
		return "", nil
	}
	job := &batchv1.Job{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: controlDBClusterRowJobName(oc)}, job); err != nil {
		return "", client.IgnoreNotFound(err)
	}
	if id := job.Annotations[clusterRowIDKey]; job.Status.Succeeded > 0 && clusterRowIDPattern.MatchString(id) {
		return id, nil
	}
	return "", nil
}

// controlDBClusterRowPrerequisites reports whether the Job may be created: both public hosts are
// published, every owned PostgresSchema of the control DB (0000-0007, 0008, the reference rows, the
// migration role's grants) is current, so the row goes into the dashboard's final schema (their
// status changes requeue this CR), and the master key is valid.
func (r *OramaCloudV1Reconciler) controlDBClusterRowPrerequisites(ctx context.Context, oc *oramav1alpha1.OramaCloudV1) (bool, error) {
	if controlDBClusterRowMissingHosts(oc) != "" {
		return false, nil
	}
	schemas, err := controlDBSchemas(oc)
	if err != nil {
		return false, err
	}
	for _, s := range schemas {
		ps := &oramav1alpha1.PostgresSchema{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: oc.Name + s.suffix}, ps); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if !postgresSchemaCurrent(ps) {
			return false, nil
		}
	}
	blocked, err := r.validateMasterAPIKey(ctx, oc)
	return blocked == nil, err
}

// controlDBClusterRowCondition is the ClusterRowReady condition: True once the Job succeeded (the
// control DB had a row when it ran, inserted by the Job or already there; its log says which), False
// while it waits for the public hosts, the schema or the master key, runs, or failed and waits to be
// retried. nil when the stack inserts no row, so the condition is dropped. blocked is
// validateMasterAPIKey's result.
//
// True reports the Job, not a live read of the table: a succeeded Job is kept and never re-run, so a
// control DB emptied afterwards (a restore) still shows True with no row. The row also keeps the
// master_api_key it was inserted with; rotating spec.masterAPIKeySecretRef does not update it. The
// message says both.
func (r *OramaCloudV1Reconciler) controlDBClusterRowCondition(ctx context.Context, oc *oramav1alpha1.OramaCloudV1, blocked *metav1.Condition) (*metav1.Condition, error) {
	if !controlDBClusterRowApplies(oc) {
		return nil, nil
	}
	name := controlDBClusterRowJobName(oc)
	row := "the " + controlDBClusterPrefix + " oramacore_clusters row"
	cond := metav1.Condition{Type: clusterRowReadyCondition, Status: metav1.ConditionFalse}
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: oc.Namespace, Name: name}, job)
	missingHosts := controlDBClusterRowMissingHosts(oc)
	switch {
	case apierrors.IsNotFound(err) && missingHosts != "":
		cond.Reason, cond.Message = "WaitingForPublicHosts", "waiting for "+missingHosts+" before inserting "+row+
			": the row is written once and never updated, so it records only the public URLs"
	case apierrors.IsNotFound(err) && blocked != nil:
		cond.Reason, cond.Message = "WaitingForMasterAPIKey", "waiting for a valid master API key before inserting "+row
	case apierrors.IsNotFound(err):
		cond.Reason, cond.Message = "WaitingForSchema", "waiting for the control DB schemas (0000 to 0008, the reference rows and the migration role's grants) before inserting "+row
	case err != nil:
		return nil, err
	case job.Status.Succeeded > 0:
		cond.Status, cond.Reason = metav1.ConditionTrue, "RowPresent"
		cond.Message = "Job " + name + " succeeded: the control DB had an oramacore_clusters row when it ran (it inserts " + row +
			" only into an empty table; its log says which). This reports the Job, not the table: it is not re-run," +
			" so a control DB emptied later (a restore) still shows True, and the row keeps the master key it was" +
			" inserted with; a rotated spec.masterAPIKeySecretRef does not update it"
	case jobFailed(job):
		cond.Reason = "JobFailed"
		cond.Message = "Job " + name + " failed; inspect its pod logs. The operator replaces it at " +
			clusterRowRetryAt(job).UTC().Format(time.RFC3339)
	default:
		cond.Reason, cond.Message = "Inserting", "Job "+name+" is running"
	}
	return &cond, nil
}

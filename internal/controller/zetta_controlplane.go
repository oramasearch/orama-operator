// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-operator/api/v1alpha1"
)

// zettaSchemaStmt is one DDL statement + the database it applies to (empty db = a CREATE DATABASE).
type zettaSchemaStmt struct{ db, sql string }

const zettaSchemaVersionAnnotation = "orama.cloud/operator-schema-version"

// operatorZettaSchema is the operator's OWN control-plane + data-plane schema, applied to the Zetta
// node that backs the operator (One Orama symbiosis v0 — Zetta as the operator's datastore). Zetta SQL
// is a Postgres subset with **no `IF NOT EXISTS`** and one statement per call, so each is applied
// individually and an "already exists" reply is treated as success. Evolve the schema by editing this
// list. A content hash on the immutable Job makes the reconciler delete and recreate it when this
// schema changes, so an existing cluster applies new statements without an out-of-band step.
var operatorZettaSchema = []zettaSchemaStmt{
	// Control plane — the operator's own coordination state.
	{"", "CREATE DATABASE orama_control"},
	{"orama_control", "CREATE TABLE components (name TEXT, kind TEXT, image TEXT, status TEXT, node TEXT, updated_at TEXT)"},
	{"orama_control", "CREATE TABLE reconcile_events (ts TEXT, cluster TEXT, action TEXT, detail TEXT)"},
	// Cross-cluster Zetta topology (operator#105) — one append-only row per wan-accessible node, keyed by
	// operator + global cluster name. Each operator WRITES its slice's wan endpoints here; Zetta CRDT-syncs
	// the table across every cluster; each operator READS it back (latest-wins by updated_at) to DERIVE the
	// `--peer` set of sibling slices — replacing hand-copied spec.externalPeers. Append-only + no
	// IF NOT EXISTS (zetta#735): a departed node is expressed with a `status='gone'` marker row, never a
	// DELETE. See zetta_topology.go + docs/zetta-datastore.md.
	{"orama_control", "CREATE TABLE topology (operator TEXT, cluster TEXT, node TEXT, address TEXT, reachability TEXT, node_type TEXT, status TEXT, updated_at TEXT)"},
	// Desired deployments and their observed transitions (operator#124), in lake's row shape
	// (operator#560): lake's columns from include/lake/control/zetta_control.hpp, keyed by
	// lake = LAKE_PUBLIC_HOST, then the operator's workload columns. Zetta is append-only, so the
	// deployment contract writes record_type='desired' rows and the operator appends
	// record_type='status' rows. Zetta stores rows as schemaless JSON and has no ALTER TABLE, so
	// this list only documents the columns: on a Zetta where the table exists the statement
	// answers "already exists" and changes nothing, and every row the operator writes carries
	// every column itself. change_id is the column before operator#560 and is no longer written.
	{"orama_control", "CREATE TABLE deployments (operator TEXT, lake TEXT, repository TEXT, environment TEXT, kind TEXT, change TEXT, image TEXT, deployment_id TEXT, requested_by TEXT, requested_at TEXT, phase TEXT, namespace TEXT, url TEXT, message TEXT, updated_at TEXT, ordering_token TEXT, record_type TEXT, replicas TEXT, port TEXT, env_json TEXT, secret_refs_json TEXT, host TEXT, withdrawn TEXT, change_closed TEXT, change_id TEXT)"},
	// The public deployment contract preserves response/history fields separately from the desired and
	// status event stream. A separate table upgrades existing clusters without ALTER TABLE support.
	{"orama_control", "CREATE TABLE deployment_records (deployment_id TEXT, lake TEXT, repository TEXT, environment TEXT, change_id TEXT, image TEXT, result_id TEXT, rollback_of TEXT, secret_refs_json TEXT, created_at TEXT)"},
	// lake's platform requests (operator#557, lake docs/control-plane/platform.md): lake appends an
	// environment's plain values and its custom domains here, keyed by lake = LAKE_PUBLIC_HOST, and
	// its platform sync reads both tables with deployments on every pass. A table that does not
	// exist fails that read, which leaves lake's platform degraded, so the tables are created with
	// the columns of lake's include/lake/control/zetta_control.hpp. The operator applies both to the
	// environment's LakeEnvironment and appends each domain's status (operator#560).
	{"orama_control", "CREATE TABLE environment_env (lake TEXT, repository TEXT, environment TEXT, key TEXT, value TEXT, deleted TEXT, set_by TEXT, updated_at TEXT, ordering_token TEXT)"},
	{"orama_control", "CREATE TABLE domains (lake TEXT, repository TEXT, environment TEXT, host TEXT, tls TEXT, status TEXT, message TEXT, deleted TEXT, requested_by TEXT, updated_at TEXT, ordering_token TEXT)"},
	// Data plane — the application-data landing zone (destined to replace v1's Typesense/ClickHouse/PG).
	{"", "CREATE DATABASE orama_data"},
	{"orama_data", "CREATE TABLE documents (id TEXT, collection TEXT, body TEXT, updated_at TEXT)"},
}

// reconcileZettaControlPlane provisions the operator's databases + tables on this Zetta cluster via an
// in-cluster Job that POSTs each DDL statement to the node's /v1/sql. The operator runs locally (and
// can't reach the ClusterIP), so a Job — like the control-DB migration Job (controldb.go) — is the
// portable mechanism: it waits for the node, applies the schema idempotently, and completes.
func (r *OramaZettaClusterReconciler) reconcileZettaControlPlane(ctx context.Context, z *oramav1alpha1.OramaZettaCluster, clientSvc string) error {
	name := z.Name + "-zetta-schema"
	lbls := zettaPodLabels(z)

	// Render the apply script from the schema list. Each statement → POST /v1/sql; "ok" or
	// "already exists" is success; a flap (503 / empty) retries; anything else fails the Job.
	var b strings.Builder
	b.WriteString("set -u\n")
	b.WriteString(fmt.Sprintf("BASE=\"http://%s:%d\"\n", clientSvc, zettaAPIPort))
	// The engine's HTTP/SQL gate is always on (zetta#799): authenticate every request with the seeded admin
	// bearer, injected as ZETTA_ADMIN_TOKEN from Secret <name>-zetta-admin (below). An unauthenticated call
	// would 401. The value never lands in the pod log (it's only ever in the header, not echoed).
	b.WriteString("AUTH=\"Authorization: Bearer ${ZETTA_ADMIN_TOKEN:-}\"\n")
	b.WriteString("echo 'zetta control-plane: waiting for the node...'\n")
	b.WriteString("until curl -sf -o /dev/null -H \"$AUTH\" \"$BASE/v1/server_info\"; do sleep 2; done\n")
	// GET /v1/sql?statement=...&database=... (the sql capability is GET-bound; a browser query works).
	// The node's HTTP server (zetta#731) drops ~2/3 of back-to-back requests (curl exit → empty body),
	// so retry hard on empty/503, succeed on ok / "already exists", and FAST-FAIL a real engine error.
	b.WriteString(`apply() {
  for try in $(seq 1 60); do
    if [ -n "$1" ]; then
      resp=$(curl -sG -H "$AUTH" "$BASE/v1/sql" --data-urlencode "database=$1" --data-urlencode "statement=$2")
    else
      resp=$(curl -sG -H "$AUTH" "$BASE/v1/sql" --data-urlencode "statement=$2")
    fi
    case "$resp" in
      *'"ok":true'*)      echo "  ok: $2";     return 0 ;;
      *'already exists'*) echo "  exists: $2"; return 0 ;;
      *'"ok":false'*)     echo "  FAIL: $2 -> $resp"; return 1 ;;
      *)                  sleep 2 ;;
    esac
  done
  echo "  FAIL (retries exhausted): $2"; return 1
}
`)
	for _, s := range operatorZettaSchema {
		b.WriteString(fmt.Sprintf("apply %q %q || exit 1\n", s.db, s.sql))
	}
	b.WriteString("echo 'zetta control-plane: schema applied'\n")
	script := b.String()
	schemaSum := sha256.Sum256([]byte(script + "\n" + pinnedCurlImage))
	schemaVersion := fmt.Sprintf("sha256:%x", schemaSum[:6])

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = lbls
		cm.Annotations = map[string]string{zettaSchemaVersionAnnotation: schemaVersion}
		cm.Data = map[string]string{"apply.sh": script}
		return controllerutil.SetControllerReference(z, cm, r.Scheme)
	}); err != nil {
		return err
	}

	var current batchv1.Job
	if err := r.Get(ctx, client.ObjectKey{Namespace: z.Namespace, Name: name}, &current); err == nil {
		if current.Annotations[zettaSchemaVersionAnnotation] != schemaVersion {
			return r.Delete(ctx, &current)
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}

	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, job, func() error {
		job.Labels = lbls
		job.Annotations = map[string]string{zettaSchemaVersionAnnotation: schemaVersion}
		if job.CreationTimestamp.IsZero() { // Jobs are immutable — set the spec once; delete to re-apply
			job.Spec = batchv1.JobSpec{
				BackoffLimit: int32Ptr(20), // retry while the (flaky) node comes up
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: lbls},
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers: []corev1.Container{{
							Name:    "schema",
							Image:   pinnedCurlImage,
							Command: []string{"sh", "-c", "sh /schema/apply.sh"},
							// The seeded admin bearer — auth is mandatory (zetta#799), so the schema curls must
							// authenticate. Read from Secret <name>-zetta-admin (always provisioned before this Job).
							Env: []corev1.EnvVar{{Name: "ZETTA_ADMIN_TOKEN", ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: z.Name + "-zetta-admin"}, Key: "token"}}}},
							VolumeMounts: []corev1.VolumeMount{{Name: "schema", MountPath: "/schema", ReadOnly: true}},
						}},
						Volumes: []corev1.Volume{{
							Name: "schema",
							VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: name}}},
						}},
					},
				},
			}
		}
		return controllerutil.SetControllerReference(z, job, r.Scheme)
	})
	return err
}

// teardownZettaControlPlane removes the schema Job + ConfigMap (it does NOT drop the databases — that
// would destroy data; dropping is a deliberate, separate action).
func (r *OramaZettaClusterReconciler) teardownZettaControlPlane(ctx context.Context, z *oramav1alpha1.OramaZettaCluster) error {
	name := z.Name + "-zetta-schema"
	_ = deleteIfExists(ctx, r.Client, &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace}})
	_ = deleteIfExists(ctx, r.Client, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace}})
	return nil
}

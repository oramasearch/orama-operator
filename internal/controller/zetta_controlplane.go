/*
Copyright 2026 OramaSearch.
Licensed under the Apache License, Version 2.0.
*/

package controller

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	oramav1alpha1 "github.com/oramasearch/orama-cloud-v1-operator/api/v1alpha1"
)

// zettaSchemaStmt is one DDL statement + the database it applies to (empty db = a CREATE DATABASE).
type zettaSchemaStmt struct{ db, sql string }

// operatorZettaSchema is the operator's OWN control-plane + data-plane schema, applied to the Zetta
// node that backs the operator (One Orama symbiosis v0 — Zetta as the operator's datastore). Zetta SQL
// is a Postgres subset with **no `IF NOT EXISTS`** and one statement per call, so each is applied
// individually and an "already exists" reply is treated as success. Evolve the schema by editing this
// list, then delete the `<name>-zetta-schema` Job to re-apply (Jobs are immutable).
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
	lbls := zettaLabels(z)

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

	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Labels = lbls
		cm.Data = map[string]string{"apply.sh": script}
		return controllerutil.SetControllerReference(z, cm, r.Scheme)
	}); err != nil {
		return err
	}

	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: z.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, job, func() error {
		if job.CreationTimestamp.IsZero() { // Jobs are immutable — set the spec once; delete to re-apply
			job.Labels = lbls
			job.Spec = batchv1.JobSpec{
				BackoffLimit: int32Ptr(20), // retry while the (flaky) node comes up
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: lbls},
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers: []corev1.Container{{
							Name:    "schema",
							Image:   "curlimages/curl:8.10.1",
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

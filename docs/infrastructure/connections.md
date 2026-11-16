# Declared workspace connections

[`Connection`](../crd-reference.md#connection-separate-crd-namespaced--one-declared-workload-path)
implements stage 2 of the [secure-networking design](secure-networking.md) for
[operator#468](https://github.com/oramasearch/orama-operator/issues/468). One resource permits one
Lake workload to reach one service or app in the same workspace. Everything else remains denied by
the workspace's `NetworkSandbox`.

## Desired state and ownership

Lake owns desired state in its control Postgres. Migration `0084_network` creates
`network_connections`, `network_connection_status` and `network_guard`; Lake writes desired rows and
the operator writes only status rows. The operator checks for `network_guard` before reading either
network table. Until the guard exists it creates no `Connection`, writes no network status and keeps
`allow-workspace-internal` unchanged. The vendored Lake schema does not include migration 0084 until
that migration merges upstream.

The normal Lake control-row poll reads the complete connection set over pgx. `LISTEN lake_network`
wakes the same reconciler promptly, while the poll recovers a missed notification or restart. Each
row becomes a Lake-controlled `Connection/connection-<row UUID>` in the reconciled workspace
namespace. A hard-removed row is recovered by deleting its orphan CR; the normal removal path is
`desired = 'absent'`.

The `Connection` controller never creates an independent policy. It adds two named allowances to
the workspace's existing `NetworkSandbox`:

- egress selecting only the source pods and only the target pods and ports;
- ingress selecting only the target pods and only the source pods and ports.

The `NetworkSandbox` renders and owns both `NetworkPolicy` objects, so its drift repair, ownership
checks and mandatory metadata deny still apply. Setting the row absent removes both allowances and
renders the sandbox in the same Connection reconcile before phase `Deleted` is reported.

## Workload identity and ports

Lake IDs are mapped to labels the operator owns:

| Kind | Selector |
|---|---|
| app | the existing `orama.cloud/lake-app-id` workload identity resolved from the Lake-owned `LakeApp` carrying `orama.cloud/app-id=<source_id>` |
| service | the labels the service's own operator puts on its serving pods, found from its primary resource (below) |
| agent | `orama.cloud/agent-id=<id>` |
| tunnel | `orama.cloud/tunnel-grant-id=<id>`: the grant's zetta-tunnel endpoint or `cloudflared` connector. The operator generates `Connection/connection-tunnel-<grant id>` itself, controlled by the `ZettaTunnel` or `CloudflareTunnel` of the same Lake and grant ([tunnels](tunnels.md)) |

Targets are services or apps. A peer with only a pod selector is namespace-local, so even a bad ID
cannot select a workload in another workspace. Explicit `ports` are TCP destination ports. An empty
list is narrowed to the target's Kubernetes `Service` target ports, including named ports; it never
opens every pod port. A target with no discoverable Service stays `Pending`.

### How a service target is found

A service's pods and Kubernetes Services do not carry `orama.cloud/service-id`. Upstream operators
(CloudNativePG, TiDB Operator, Strimzi, ...) label them with their own keys, and copying ours onto
the pods would roll every database pod. Only the primary resource the operator writes carries the
id and the kind: the CloudNativePG `Cluster`, the `TidbCluster`, the `OramaZettaCluster`, a wrapper
kind's StatefulSet. So `resolveServiceTarget` (`internal/controller/service_targets.go`):

1. finds the primary resource labelled `orama.cloud/service-id=<id>` in the workspace namespace;
2. takes the serving pods' selector from its kind: each catalog kind's `podSelector`, `cnpg.io/cluster`
   for Postgres, `app.kubernetes.io/component=zetta-node` with the cluster's instance for Zetta. TiDB
   selects the TiDB servers only, never PD or TiKV;
3. takes ports from the Services whose `spec.selector` holds every label of that selector, plus any
   Service labelled with it. Headless Services are skipped: they carry a datastore's own node-to-node
   traffic (Zetta's 7777), which no client connection needs.

If no primary resource carries the id, pods labelled `orama.cloud/service-id=<id>` are the target.
Tunnels (`resolveTunnelTarget`), egress rules with a service source, and the credential's kind lookup
use the same resolution. Found on a development Lake: before this, every connection to a service
stayed `Pending` waiting for the target's Service ports.

A row with non-null `credential_scope` also gets a credential of its own: see
[Credentials on connections](#credentials-on-connections).

## Credentials on connections

[operator#469](https://github.com/oramasearch/orama-operator/issues/469) implements decision D4 of
the [design](secure-networking.md#credentials-on-connections-decision-d4) for Postgres services.
A connection from an app to a service whose `credential_scope` is set gets a datastore role used by
that connection only. The service's owner credential (`app`) and its superuser credential are never
handed to the app.

**The scope.** `credential_scope` is `{"access": "read" | "write", "schema": "<name>"?}`. It is
copied to `Connection.spec.credentialScope`. Unknown fields, another access value, a schema that is
not a plain identifier, or a scope on anything but app to service make the row `Failed`; the
operator never guesses a wider or narrower grant.

| Scope | Grants to role `lake_conn_<connection id without dashes>` |
|---|---|
| `read` | `pg_read_all_data`: `SELECT` on every table, view and sequence of every schema |
| `write` | `pg_read_all_data` and `pg_write_all_data`: also `INSERT`, `UPDATE`, `DELETE` |
| `read` with `schema` | `USAGE` on that schema, `SELECT` on its tables and sequences, and the owner's default privileges for tables created later |
| `write` with `schema` | as above, plus `INSERT`, `UPDATE`, `DELETE` and sequence `UPDATE` |

No scope grants `CREATE` or any DDL. The role is `LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE
NOREPLICATION NOBYPASSRLS`. Each reconcile replaces its grants in one transaction (`DROP OWNED BY`,
revoke the two predefined roles, grant the scope), so a scope change is atomic for other sessions.
A schema that does not exist yet leaves the connection `Pending` until it does.

**How the operator reaches the role.** Every Lake service Postgres cluster sets CloudNativePG's
`enableSuperuserAccess: true`, so CloudNativePG writes `<cluster>-superuser` in the workspace
namespace. Only the operator reads it. Each workspace `NetworkSandbox` has the allowance
`allow-operator-to-postgres`: ingress from the operator's pods to CloudNativePG instance pods on
5432. The operator connects to `<cluster>-rw` and sets the password as a SCRAM-SHA-256 verifier it
computes itself, so the plaintext password never reaches the server, its logs or
`pg_stat_activity`.

**The superuser session verifies the server** (orama-operator#775). The operator connects to
`<cluster>-rw.<namespace>.svc`, the name CloudNativePG's server certificate carries, with
`sslmode=verify-full` against the cluster's own CA (Secret `<cluster>-ca`, key `ca.crt`), and never
falls back to an unverified or plaintext session. Until that Secret exists the Connection waits
("waiting for the target's Postgres CA Secret") and the superuser password is never sent.

**The superuser session trusts nothing in the tenant's database**
([operator#800](https://github.com/oramasearch/orama-operator/issues/800)). The database
belongs to the tenant: its owner may set the database's default `search_path` and create objects in
its own schemas, so a name resolved through that path could be the owner's look-alike of a catalog
view, function or operator, run with superuser rights. Every session the operator opens as the
superuser sends `search_path=pg_catalog, pg_temp` in its startup message, which is in force before
the first statement and overrides the database's default; every transaction sets it again with
`SET LOCAL search_path = pg_catalog, pg_temp`; and every query names its catalog objects,
functions and operators by schema (`pg_catalog.pg_roles`, `pg_catalog.pg_get_userbyid`,
`OPERATOR(pg_catalog.=)`). Roles and schemas are named as quoted identifiers, never resolved through
the path.

**Delivery (the two levels of secrets).** The credential is a user-level secret. The operator writes
it with Lake's `put_secret`, as the operator's admin identity, into the workspace mount
`network-credentials` (created with `create_secret_mount` on first use), under the name
`connection-<connection id>`. It is sealed in the workspace Zetta. The fields are `kind`
(`postgres`), `host`, `port`, `database`, `user`, `password` and `uri`, the same shape as a
service's own credential, and the custom metadata names the connection, generation, role, access
and schema. `network_connection_status.credential_secret` and `Connection.status.credentialSecret`
carry only the reference `lake-secret://<mount id>/connection-<connection id>`. A Kubernetes Secret
exists only when the app mounts it: an app whose secret environment names
`lake-secret://<mount id>/connection-<id>#uri` (or another field) gets the existing `LakeSealedSecret`
projection, and each new version reaches the app at the next delivery (every five minutes) and rolls
its pods.

**Rotation.** Lake's `rotate_network_connection_credential` advances the row's generation. A
`Connection` whose generation is above `status.credentialGeneration` gets a new password: the role
is changed first and the workspace secret second, under the same name, so the reference stays the
same. A failure between the two is repaired by the next reconcile issuing again. The old password
stops working at once; the app receives the new one through its projection. A role that vanished
(for example a recreated cluster) is issued again.

**Revocation.** Setting the row `absent`, deleting the `Connection` or removing the scope ends the
credential in the same reconcile: the network allowances go first, then the role is made `NOLOGIN`,
its sessions are terminated, its grants dropped (`DROP OWNED BY`) and the role dropped. Only then is
`Deleted` reported, with an empty `credential_secret`. The secret's versions stay in the user's
store as history; the credential they hold no longer signs in. A target whose cluster is gone has
no role left and does not block revocation.

**Other datastores.** A credential scope on a Kafka, Valkey, ClickHouse or other service is `Failed`
with a message naming the kind. Each follows in its own issue (Strimzi users and ACLs, Valkey ACL
users, ClickHouse users).

## Rollout and recovery

Lake migration 0084 seeds every app-to-service path in a workspace before exposing the guard. The
operator still keeps `allow-workspace-internal` until every initial row for that workspace has a
current `Ready` policy, including both rendered sides. It then marks the workspace connection mode
active, wakes its synchronized `LakeApp` resources, removes legacy binding egress from their access
policies and removes `allow-workspace-internal`. A workspace with no rows can switch immediately.

The marker is monotonic. A later Postgres outage, missing guard or failed list never restores broad
access. New rows start denied until their policies are ready; absent rows close their path. The
controller finalizer also removes the allowances before a `Connection` can disappear.

`network_connection_status` receives only real transitions: `Pending`, `Ready`, `Failed` or
`Deleted`, with the Lake generation acknowledged in `observed_generation`. `credential_secret`
carries the workspace secret reference of a credential-scoped connection and is empty otherwise. The CR status uses the same phase, message and observed generation and is also
comparison-gated.

## Verification boundary

Credential tests: fake-backed controller tests cover issuance, the steady state, rotation under
the same reference, revocation in one reconcile, deletion, removal of the scope, a vanished role, a
missing schema, the superuser Secret wait, a non-Postgres target, that neither the owner nor the
superuser credential reaches the issued secret, and the SCRAM verifier against an independent
implementation. The Lake secret writer is tested against a fake Lake API: it creates the mount
once, puts a version per issue with the operator's bearer, and turns a refusal into an error. `TestConnectionRolesAgainstPostgres` (opt-in, `CONNECTION_ROLES_TEST_POSTGRES=1`)
starts a throwaway PostgreSQL with `initdb` and SCRAM authentication and proves the acceptance on a
real server: a read role reads and is refused `INSERT`, `UPDATE` and `CREATE`; a write role writes
and is refused DDL; a schema scope reads its schema, including a table created afterwards, and is
refused another; the rotated-out password is refused; revocation ends an open session and refuses a
new sign-in.
`TestConnectionRolesIgnoreTheOwnersSearchPath` (same opt-in) has the database owner put its own
schema ahead of `pg_catalog` in the database's `search_path` and fill it with look-alikes of every
catalog view, function and operator the operator uses, each recording its caller and trying to make
the owner a superuser; issuing, rescoping, checking and revoking a role must run none of them.
`TestConnectionPostgresSessionPinsSearchPath` checks the startup parameter and
`TestConnectionRoleSQLQualifiesEveryCatalogObject` that no query names a catalog object without its
schema; both run without a server.

Native tests cover app-to-service selector mapping, paired ingress and egress, target Service port
resolution, guarded rollout, removal in one reconcile, absence before `network_guard`, legacy app
egress retirement and non-flapping status writes. The opt-in pgx test creates the 0084 connection
tables itself because that migration is not yet vendored.

The named acceptance probe on `example-nonprod` is deferred to rollout: an app with one
connection reaches only its selected service, the same app without the connection is refused, and
setting the row absent refuses the path after one reconcile.

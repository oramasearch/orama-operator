# Lake apps

`LakeApp` is the Kubernetes desired-state record for one app belonging to a cluster-scoped
`Lake`. The resource lives in the Lake namespace. With `spec.apps.enabled`, the operator reads
lake's desired `apps` rows from control Postgres, reconciles Lake-owned `LakeApp` resources,
registers token hashes and writes observed status back. Lake owns the schema and API in lake#293.

## Desired-state contract

Enable it on the Lake:

```yaml
spec:
  apps:
    enabled: true
    wildcard: apps.oramadev.net
    ingressClass: nginx
    runtimeImage: oramasearch/app-runtime:<immutable-tag>
  dnsSecretRef:
    name: cloudflare-api-token
```

Postgres must be enabled and the Lake schema applied. The independent `lake-apps-sync` controller
uses the Services DSN Secret, `lake-<lake>-controldb-app` key `uri`, one pgx connection per pass,
and the same 30-second pass bound. A leader-only listener uses `LISTEN lake_apps`; lake sends the
app ID as the notification payload. Each notification wakes a full list, so a payload cannot select
another tenant. A 60-second full list recovers lost notifications; Pending/Deploying apps and failed
writes use a two-second retry. LakeApp, dependency Secret, schema and ingress changes also wake
the sync. An unavailable database, missing table or incomplete list preserves existing resources.
Disabling apps stops intake and removes its wildcard record, but retains apps and credentials.

The table includes the required workspace identity:

```text
id, customer_id, workspace_id, name, mode, source_repository, source_revision, source_path,
image, framework, port, replicas, host, auth, bindings_json, env_json,
secret_env_refs, desired, generation, phase, url, ready, message,
observed_generation, updated_at
```

The agreed additions are `source_entrypoint` (optional text) and `token_hashes_json` (jsonb).
`id`, `customer_id` and `workspace_id` are UUIDs; `generation` is a positive, monotonically
increasing integer. The query joins `workspace_id` to the operator-written
`workspaces.namespace`; there is no Lake-namespace fallback.
Lake writes desired fields and increments generation on edits. The operator writes only status
and token hashes. Status and hash writes match ID, customer, generation and desired value, so an
old read cannot acknowledge a newer row or recreate a removed row. Unchanged observations write
nothing; `updated_at` changes on status transitions.

`env_json` is a string-to-string JSON object. `secret_env_refs` is a JSON object whose keys are
environment variable names and whose string values select Secrets in the Lake namespace. A value
is `<secret-name>` to use the environment name as the Secret key, or `<secret-name>/<key>` to select
a different key:

```json
{"API_KEY":"svc-creds/token","DB_URL":"db-secret"}
```

This renders `API_KEY` from key `token` in `svc-creds` and `DB_URL` from key `DB_URL` in
`db-secret`. `{}`, SQL `NULL`, JSON `null`, and an empty decoded value request no secret
environment variables. Environment names must pass Kubernetes `IsEnvVarName`, Secret names must
be DNS-1123 subdomains, and keys must be valid Kubernetes Secret keys. Invalid input makes the row
`Failed` with a stable validation message and creates no invalid `LakeApp` update.

For one compatibility release, the operator also accepts the former JSON array of Secret names.
Those whole Secrets retain their `envFrom` behavior through `spec.secretRefs`; new writers must use
the object contract. Lake must authorize these references and detect secret env values before
writing them; plaintext credentials belong in Secrets, never these JSON columns. `bindings_json` is:

```json
{"zetta":{"scopes":["read"]},"services":["22222222-2222-4222-8222-222222222222"]}
```

`zetta` can be null or absent. The sync checks each service ID against a present `services` row
in the same workspace and resolves its name. Duplicate or unsafe filenames fail validation.
The Service reconciler publishes an ID key alongside its existing `<slug>.json` key in the
Lake-owned `lake-<lake>-services` Secret; both contain the same connection document. Only requested
ID keys are copied to `app-bindings` in the app namespace. The pod mounts them as
`/var/run/orama/bindings/services/<service name>`. Withdrawn services lose both keys; an app whose
connection is unavailable stops its workload and reports binding progress.

CR names are stable `app-<8-hex-Lake-hash>-<app UUID>`, independent of display-name or source
changes. They carry the Lake owner reference and `orama.cloud/lake` / `orama.cloud/app-id` labels.
An occupied name belonging to a different owner or customer fails without adoption. The row's
`name` is a DNS label. An empty row host becomes `<name>.<wildcard>`; a supplied host must be one
label beneath that suffix. Lake is responsible for reserving unique app hosts.

### Lifecycle and returned status

| Desired state | Reconciliation and reported state |
|---|---|
| `running` | Create/update the CR from the row. `Pending` until its current generation is observed; `Deploying` during rollout; `Running` with `ready=true` only after current Deployment readiness; `Degraded` for workload failures. Invalid desired input or foreign ownership is `Failed`. |
| `stopped` | Keep the CR and all resources, setting replicas to zero. Report `Stopped`, `ready=false` only after the Deployment observes zero running replicas. Starting restores the row's replica count. |
| `deleted` | Delete the owned CR with UID/resource-version preconditions. Its finalizer removes only that app's resources, credentials, policy and individual DNS record; the shared workspace namespace remains. While cleanup runs report `Deploying`, `ready=false`; after the CR is absent revoke token hashes and write `Deleted`. Lake then removes the row. |
| Row absent | A successful complete list deletes only orphan CRs controlled by this Lake with a sync identity. Standalone and foreign CRs remain untouched. Failed reads never trigger orphan cleanup. |

Every status write includes `phase`, `url`, `ready`, `message`, `observed_generation`, `updated_at`.
Allowed database phases are `Pending`, `Deploying`, `Running`, `Degraded`, `Stopped`, `Failed`,
`Deleted`; these are a projection of the Kubernetes status described below.

### Generated credentials

The app controller generates independent 32-byte random tokens, encoded as hex, once in Secret
`app-<first 12 app UUID hex>-tokens` in the workspace namespace. Its ownership includes the LakeApp UID, so a Secret
from another app incarnation cannot be adopted. `source` and `events` always exist; `zetta` exists
only with a workspace binding. The sync writes their SHA-256 hex digests as:

```json
{"source":"<sha256 hex>","zetta":null,"events":"<sha256 hex>"}
```

When bound, `zetta` is its SHA-256 hex digest instead of null. A workload starts only after the
sync successfully registers these hashes and acknowledges them in `spec.sync.tokenHashes`.
Retries retain the existing tokens. Removing the app's token Secret requests rotation: the controller
generates replacements, pauses the workload, registers new hashes, and rolls the new credentials
into the pod. A failed registration keeps the workload paused. Old tokens stop authenticating when
lake sees the new hashes. Unbinding removes the Zetta token and registers null without rotating
the other purposes. Deletion clears `token_hashes_json` after resource teardown.

Lake hashes each bearer and enforces its purpose against the current row:

- `source`: `repository_tree` / `repository_file` reads for this repository and revision only;
- `zetta`: `workspace_query` / read-only `workspace_sql` for this customer and its declared scopes;
- `events`: `publish_app_event` for this app only, publishing to `app-events.<app id>`.

Runtime pods receive `APP_TOKEN` from the app token Secret's `source` key. The Zetta token is copied to the local
binding Secret and mounted at `/var/run/orama/bindings/zetta`. `EVENTS_TOKEN` comes from
the token Secret's `events` key, `APP_ID` is the row ID, and `EVENTS_URL` is Lake's in-cluster
`/v1/publish_app_event` endpoint. The publisher reads that same Secret. Tokens never appear in
Postgres, specs, literal env values, annotations, status or logs. SQL/client errors are reduced to
stable messages before logging so connection strings and driver details cannot expose credentials.

## API contract

Runtime mode loads a pinned Lake repository revision with the Lake's
`spec.apps.runtimeImage`. Image mode runs an OCI image pinned by SHA-256 digest.

```yaml
apiVersion: orama.cloud/v1alpha1
kind: LakeApp
metadata:
  name: sales-dashboard
  namespace: lake
spec:
  lakeRef: prod
  mode: runtime
  source:
    repository: sales-analytics
    revision: 8f1d6dfdbde090ba5b293a7439f975f5564b026a
    path: apps/dashboard
    framework: fastapi
    entrypoint: main:app
    tokenSecretRef:
      name: sales-dashboard-source
      key: token
  port: 8080
  replicas: 1
  auth: lake
  secretEnv:
    - name: API_KEY
      secretKeyRef:
        name: svc-creds
        key: token
  bindings:
    secretRef:
      name: sales-dashboard-bindings
    zetta:
      customerId: 50d440ef-d5e3-44df-8381-340ad2c3a0fb
      scopes: [datasets:read]
    services:
      - 8831658d-5691-4d40-b2cd-d52c651a8087
  eventsSecretRef:
    name: sales-dashboard-events
```

The Lake-level settings are:

- `spec.apps.enabled`: opt-in synchronization from the Lake control Postgres;
- `spec.apps.runtimeImage`: the private `oramasearch/app-runtime` image used in runtime mode;
- `spec.apps.wildcard`: the DNS suffix used when the app does not set `spec.host`; when apps are
  enabled, the operator also injects it into lake as `LAKE_APPS_WILDCARD` so lake can validate and
  create app hosts;
- `spec.apps.ingressClass`: the app Ingress class, defaulting to `nginx`.

An image-mode app sets `spec.image` to an immutable `name@sha256:<64 hex>` reference. Its optional
`spec.pullSecretName` overrides the Lake's pull Secret. The source pull Secret always remains in the
Lake namespace; the controller copies it into the workspace namespace for synchronized apps or
the private app namespace for standalone CRs.

The apps sync sets `spec.sync` on its Lake-owned CRs: the immutable app `id`, `customerId` and
`workspaceId`, the operator-written `workspaceNamespace`, the desired row `generation`, and
`tokenHashes` acknowledging credentials registered in Postgres.
Only SHA-256 hashes appear here. `bindings.serviceNames` records the service IDs' resolved names
for mount filenames. Standalone CRs omit `sync` and keep their explicit Secret references.
There is no `spec.apps.buildkit`; build Jobs belong to #195 and `spec.builds`.

An explicit `LAKE_APPS_WILDCARD` entry in `spec.env` takes precedence over the managed value. The
operator renders exactly one entry, which preserves the production workaround while allowing it to
be removed after rollout. Lake also reads the optional `LAKE_APPS_PER_CUSTOMER` and
`LAKE_APP_REPLICAS_PER_CUSTOMER` limits directly from `spec.env`; the Lake CR has no corresponding
fields.

`source.entrypoint` is optional. An empty value omits `APP_ENTRYPOINT` so app-runtime reads
the entrypoint from the app's own `app.toml`; an explicit value overrides that file.

## Standalone credential inputs

Credentials never appear in `LakeApp.spec`, Deployment environment values, annotations, status, or
logs. A standalone CR without `spec.sync` uses explicit source Secrets in the Lake namespace.

`spec.source.tokenSecretRef` selects the Lake-minted repository read token used as `APP_TOKEN`.
Runtime mode requires its Secret name and key. The workload copy contains only the selected value,
even when the source Secret holds other keys.

`spec.bindings.secretRef` points at a Secret with these data keys:

- `zetta`: the Lake-minted scoped workspace token when `bindings.zetta` is present;
- one key equal to each requested service identifier: that service's connection document.

The controller copies only those named keys. `spec.secretEnv` maps each environment name to a
Lake-namespace `secretKeyRef`; only selected keys are copied into the isolated namespace and the
Deployment references the local copies through `env[].valueFrom.secretKeyRef`. Operator-managed
environment names take precedence, followed by secret-backed values and then literal `spec.env`
values. `spec.secretRefs` is the one-release legacy whole-Secret form: those Secrets are copied and
exposed through `envFrom`. `spec.eventsSecretRef` names a Secret with `url` and `token` keys for the
app's `app-events.<app>` topic. A selected source Secret change changes the pod-template credential
digest, so Kubernetes performs a rollout without putting secret data in the template.

## Resources and isolation

Synchronized apps run in the owning `<environment>-ws-<workspace id>` namespace and use
`app-<first 12 app UUID hex>` object names so multiple apps cannot collide. The controller verifies
the namespace's full workspace label and never adopts or deletes that shared namespace. Standalone
CRs retain their stable private `lake-<lake>-app-<10-hex-hash>` namespace, derived from Lake and
`LakeApp` identity, with an ownership annotation checked before deletion. The controller creates:

- for standalone apps, `ResourceQuota tenant-defaults`, `LimitRange tenant-defaults` and a default-deny policy;
- a per-app access policy;
- a uniquely named Deployment and Service;
- a uniquely named Ingress when a host is available;
- a proxied `CloudflareDNSRecord` for a custom host when Lake DNS credentials are configured.

The workspace's baseline policies admit DNS and Lake. The per-app policy admits the nginx ingress
controller only to that app's labelled pod and port. Synchronized apps can reach the Lake
Service for source, workspace reads and event publication; their scoped Zetta bearer is validated
by Lake, not sent directly to Zetta. Each service binding gets a rule selecting that service ID.
Standalone apps retain their direct workspace/bus Zetta policies. No general internet egress opens.

The pod runs as uid/gid 10001, drops all capabilities, prevents privilege escalation, uses runtime
default seccomp, has a read-only root filesystem, disables its Kubernetes token and service links,
and gets a size-limited `emptyDir` at `/tmp`. Private pull credentials are copied into the workload
namespace. Runtime env includes `WORKSPACE_ID`, and `LAKE_URL`, auth and event URLs use Lake's
`/w/<workspace id>` route. The selected binding values appear as files at:

```text
/var/run/orama/bindings/zetta
/var/run/orama/bindings/services/<service name>
```

Runtime mode sets both the original app-runtime source variables and the expanded contract:
`LAKE_URL`, `APP_SOURCE=<repository>@<revision>`, `APP_REPOSITORY`, `APP_REVISION`, `APP_PATH`,
optional `APP_ENTRYPOINT`, `APP_FRAMEWORK`, `APP_TOKEN`, and `APP_PORT`. Standalone apps default
service filenames to the ID and obtain `EVENTS_URL` and `EVENTS_TOKEN` from their source Secret.

## Ingress and authentication

An empty app host derives `<app>.<Lake spec.apps.wildcard>`. That address uses the one shared
wildcard record maintained by the apps sync. A standalone custom host gets its own proxied record after nginx
reports a load-balancer target. The ingress class is `spec.apps.ingressClass`, default `nginx`.

With apps enabled, a wildcard and `dnsSecretRef`, the sync maintains one Lake-owned proxied
`CloudflareDNSRecord` in the Lake namespace. It uses the Lake ingress address when its class
matches, otherwise an owned app ingress of the configured class, selected deterministically.
No record is created until a target is reported. Suffix changes retire old owned records, and
disabling apps or removing the suffix/credentials removes the wildcard. Foreign records are
never changed or deleted. This path is independent of `previewWildcard` and supports apps even
when the Lake itself has no public ingress.

`auth: lake` is the default. It adds nginx's `auth-url` annotation pointing at
`http://lake-<lake>.<namespace>.svc.cluster.local:80[/w/<workspace id>]/v1/apps/auth`. The fully
qualified Service name is required by ingress-nginx's resolver. Lake verifies the browser's session
and space membership before the request reaches the app. `auth: public` omits the annotation.
It also sets `auth-always-set-cookie: "true"`, so the app-scoped session cookie Lake returns from
the auth call reaches the browser. When the Lake has public ingress (`ingressEnabled` and `host`),
`auth-signin` sends a browser without that cookie to
`https://<lake host>/apps/authorize?app=<app id>&rd=https://$host$escaped_request_uri`. The return URL
is always https because TLS ends at the edge proxy. The handoff and cookie are specified in lake#407.

## Status and events

Status is deep-compared before every write. `Ready` requires the Deployment controller to have
observed the Deployment's own `metadata.generation` and every desired replica to be updated and
available; the independent `LakeApp` generation is not a rollout marker. Pod status takes priority
over generic rollout progress: `ImagePullBackOff`/`ErrImagePull`,
`CrashLoopBackOff`, and `OOMKilled` produce stable `Degraded` messages. `status.observedRevision`
is the source revision in runtime mode or image digest in image mode.

The readiness read after a workload update bypasses the manager cache, preventing a previously
ready Deployment from acknowledging a new revision before the API's current generation is observed.

Only a real status transition publishes an event. Synchronized apps POST to
`{LAKE_URL}/v1/publish_app_event` with `Authorization: Bearer <events token>` and JSON
`{"app_id":"<id>","event":{"phase":"…","ready":false,"revision":"…","message":"…","url":"…"}}`.
The event carries the Kubernetes phase. Standalone apps retain `publish_event` to Zetta.
Production publication runs
asynchronously with a short timeout; a missing credential, unreachable bus, or rejected event is
logged and never fails or delays resource reconciliation. The same event Secret is delivered to the
app so app-runtime can publish its own progress. Redirects are refused for Lake-issued bearers.

Deletion records `TornDown`, publishes that transition best-effort, removes any individual DNS
record and the app's named resources, and only then releases the finalizer. Standalone deletion
also verifies and deletes its private namespace; synchronized deletion preserves the workspace.

## Verification

The native API-server tests use a fake Lake store, as the Services tests do. They exercise row to
CR to workload, hashes before startup, FQDN ingress authentication, status return, no steady writes,
source rollouts whose app and Deployment generations differ, return to `Running` and ready,
stop/start, credential rotation with a failed DB write, namespace finalization and Deleted
acknowledgement.
Unit cases cover failed-list orphan safety, foreign ownership/bindings, DNS changes and ingress
class selection, listener enable/disable, the schema gate and fallback interval, and HTTP events.
The original standalone lifecycle suite retains its create, rollback and pod-failure coverage:

```sh
export KUBEBUILDER_ASSETS="$(setup-envtest use -p path 1.30.x)"
go test -count=1 -v ./internal/controller -run 'TestLakeApp|TestPublishSyncedLakeAppEvent'
```

The repository proof also runs `go build ./...`, `go vet ./...`, `go test ./...`, and
`helm lint chart`. Optional native SQL verification uses `LAKE_APPS_TEST_POSTGRES_URI` and
`TestLakeAppsPgxStoreAgainstPostgres`: a temporary contract table in a rolled-back transaction,
covering nullable columns, hash/status writes, concurrent generations and row removal. This test
fixture does not alter the byte-identical vendored schema. Re-vendoring lake#293's actual migration
and live app-runtime/Cloudflare validation remain deployment prerequisites outside this proof.

The regression files are `lake_apps_test.go` (row lifecycle, rollback, isolation and no steady
writes), `lake_apps_pgx_test.go` (SQL and concurrent row changes), `lake_apps_dns_test.go`
(wildcard ownership and ingress class), `lake_apps_notify_test.go` (listener and polling gates),
and `lakeapp_tokens_test.go` (credential ownership, unbinding, DNS finalization and event HTTP).
The Services/Stores shared-Secret regression additionally verifies ID aliases and their withdrawal.

The field-by-field reference is in [`../crd-reference.md`](../crd-reference.md#lakeapp-separate-crd-namespaced--one-deployed-app).

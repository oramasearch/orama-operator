# Lake apps

`LakeApp` is the Kubernetes desired-state record for one app belonging to a cluster-scoped
`Lake`. The resource lives in the Lake namespace. On every Lake ([operator#539](https://github.com/oramasearch/orama-operator/issues/539)), the operator reads
lake's desired `apps` rows from control Postgres, reconciles Lake-owned `LakeApp` resources,
registers token hashes and writes observed status back. Lake owns the schema and API in lake#293.

## Desired-state contract

Every Lake runs it; there is no switch. Every field is optional:

```yaml
spec:
  apps:
    wildcard: apps.oramadev.net    # empty: the Lake's spec.host
    ingressClass: nginx
    runtimeImage: oramasearch/app-runtime:<immutable-tag>   # empty: the operator's default
  dnsSecretRef:
    name: cloudflare-api-token
```

Postgres must be enabled (otherwise the Lake's `FeaturesReady` condition says apps cannot run) and the Lake schema applied. The independent `lake-apps-sync` controller
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

### Restart

Lake's `restart_app` writes `restart_generation` (lake#1228, Lake migration `0104_app_restarts`): the
generation of that restart, 0 for an app never restarted. Before, a restart only bumped
`generation`, which the operator does not put on the pod template, so the template stayed the same
and Kubernetes replaced no pod. The apps sync reads the column as
`COALESCE((to_jsonb(a)->>'restart_generation')::bigint, 0)`, so a Lake schema older than `0104`
reads 0 and this operator can roll before the Lake image that writes it. It is copied to
`spec.sync.restartGeneration`, and when it is above 0 the Deployment's pod template carries
`orama.cloud/lake-app-restart: "<restart generation>"`: each restart is a new template and the
Deployment rolls its pods. No other edit changes it, so scaling or an access change restarts
nothing, and an app never restarted has no annotation, so upgrading the operator rolls no pod.
Verify: `TestARestartChangesThePodTemplateAndNothingElseDoes` and, with
`LAKE_APPS_TEST_POSTGRES_URI`, `TestLakeAppsPgxStoreAgainstPostgres` (the column absent, then read).

`env_json` is a string-to-string JSON object. `secret_env_refs` is a JSON object whose keys are
environment variable names and whose values are **lake secret references only**
([below](#lake-secret-references-lake625)). `{}`, SQL `NULL`, JSON `null`, and an empty decoded value
request no secret environment variables. Environment names must pass Kubernetes `IsEnvVarName`.
Invalid input makes the row `Failed` with a stable validation message and creates no invalid
`LakeApp` update.

**Kubernetes Secret names are refused** (operator#492). A value that names a Secret
(`<secret-name>` or `<secret-name>/<key>`), and the former JSON array of Secret names, make the row
`Failed` with a message naming operator#492, and nothing reaches the app. The LakeApp reads its
Secrets from the Lake instance namespace, so a row that named one directly could have delivered any
Secret there into a tenant's app: the instance key `lake-prod-instance-kek`, the control database
credentials, the payment keys. Lake's API never wrote either form (it accepts only `lake-secret://`
references in the app's own workspace), so only a row written some other way could hold one. A
`LakeApp` written by hand from a committed file may still use `spec.secretEnv[].secretKeyRef` and
`spec.secretRefs`; only rows synchronized from Lake are restricted.

### Lake secret references (lake#625)

A user's secret's source of truth is lake's secret store, sealed in its workspace Zetta; a
Kubernetes Secret is only a projection. Lake now writes every new secret reference as
`lake-secret://<mount id>/<secret name>#<field>` (the name may contain `/` and `#`; the field follows
the last `#` and must be a Secret key), and refuses Kubernetes Secret names:

```json
{"DB_PASSWORD":"lake-secret://6f1c2a4e-0d3b-4c55-9a7e-1b2c3d4e5f60/payments/db#password"}
```

The row becomes a `spec.secretEnv[]` entry with `lakeSecret: {mount, name, field}` and a
`secretKeyRef` naming `app-<first 12 app UUID hex>-lake-<12 hex of mount and name>` in the Lake
namespace. For each lake secret the app reads, the LakeApp controller owns a `LakeSealedSecret`
there (labelled `lake.orama.cloud/app-lake-secret`, owner annotation and a plain owner reference to
the LakeApp; the Lake stays its controller) with `source: {type: lake, lake: {mount, name}}`. Lake's
`mint_sealed_secret` answers the secret's fields sealed to the cluster's delivery key, the
`LakeSealedSecret` controller writes the produced Secret, and the environment copy below carries the
selected field into the workspace namespace. Until the first delivery the app is `Progressing` with
reason `SecretDeliveryPending` and no workload is created; a failed delivery says why in the same
reason. An unpinned lake source is delivered again every five minutes ([sealed
secrets](sealed-secrets.md#rotation-of-a-lake-source)), so a new version changes the copy and the pod
template's credential digest and the pods roll. Removing the reference deletes the
`LakeSealedSecret`, its produced Secret and the copy. When lake answers that the secret is gone (`not_found`: deleted,
destroyed, or its mount removed), the `LakeSealedSecret` reports `Revoked` and deletes its produced
Secret, and the LakeApp deletes its copy in the workspace namespace and takes the workload down with
`SecretDeliveryPending`, so no pod keeps the withdrawn value
([orama-operator#777](https://github.com/oramasearch/orama-operator/issues/777); see
[sealed secrets](sealed-secrets.md#when-lake-stops-delivering-a-secret-orama-operator777)).

Lake must authorize these references and detect secret env values before writing them; plaintext
credentials belong in lake's secret store, never these JSON columns. `bindings_json` is:

```json
{"zetta":{"scopes":["read"]},"services":["22222222-2222-4222-8222-222222222222"]}
```

`zetta` can be null or absent. The sync checks each service ID against a present `services` row
in the same workspace and resolves its name. Duplicate or unsafe filenames fail validation.
The Service reconciler publishes an ID key alongside its existing `<slug>.json` key in the
Lake-owned `lake-<lake>-services` Secret; both contain the same connection document, and an
annotation `service-workspace.orama.cloud/<id>` records the service's workspace. Only requested
ID keys **whose annotation names the app's own workspace** are copied to `app-bindings` in the app
namespace (lake#945): a `LakeApp` naming another workspace's service reports
`ServiceInAnotherWorkspace` and gets nothing, whatever its bindings say. The pod mounts them as
`/var/run/orama/bindings/services/<service name>`. Withdrawn services lose both keys; an app whose
connection is unavailable stops its workload and reports binding progress.

CR names are stable `app-<8-hex-Lake-hash>-<app UUID>`, independent of display-name or source
changes. They carry the Lake owner reference and `orama.cloud/lake` / `orama.cloud/app-id` labels.
An occupied name belonging to a different owner or customer fails without adoption. The row's
`name` is a DNS label. An empty row host becomes `<name>.<wildcard>`; a supplied host must be one
label beneath that suffix. Lake is responsible for reserving unique app hosts.

The operator also refuses, on its own and whatever lake accepted
([#713](https://github.com/oramasearch/orama-operator/issues/713)), and marks the row `Failed`:

- a host that is the Lake's own `spec.host`, its `LAKE_PUBLIC_HOST`, or its preview wildcard's base;
- a host whose label is one of lake's built-in reserved names (`lake`, `www`, `api`, `app`, `apps`,
  `admin`, `mcp`, `docs`, `status`, `mail`, `smtp`, `auth`, `login`, `sso`, `id`, `cdn`, `static`,
  `assets`, `grafana`, `prometheus`, `argocd`, `registry`, `git`, `dev`, `staging`, `prod`, `test`)
  or starts with `lake-` or `orama`. Lake checks these against the app's name only
  ([lake#947](https://github.com/oramasearch/lake/issues/947)); a name in `LAKE_APPS_RESERVED_NAMES`
  is lake's to refuse;
- more than 100 replicas (lake's `app_replicas_check`);
- a Zetta binding whose scopes are anything but `["read"]`.

A synchronized app is placed only in a namespace labelled `orama.cloud/workspace=<row workspace>` and
`orama.cloud/customer=<row customer>` by the workspaces reconciler, never in the Lake namespace;
otherwise the LakeApp is `Degraded` with `NamespaceConflict`.

### Store mounts (lake#666)

The sync reads lake's `store_mounts` rows for the app (with the `stores` they name) and writes them
to `spec.stores[]`: a volume of the app's workspace as `{claimName, mountPath, readOnly}`, a bucket
as `{bucket, endpoint, region, prefix, secretName}`. The LakeApp controller mounts a volume's claim
at its path (and gives the pod `fsGroup` 10001 so the app's user can write it), and gives a bucket's
connection as `STORE_<NAME>_*` variables whose credential fields read a copy of the store's
delivered Secret, made the same way as a lake secret reference's copy; a new credential version
rolls the pods through the credential hash. The app waits with `StoreCredentialPending` until
every mounted bucket's credential is delivered. Only a synchronized app may carry `spec.stores`
(`StoresRequireSync`). Reading a bucket needs the workspace's internet egress. The whole of it is
[stores.md](stores.md#mounts-lake666).

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
from another app incarnation cannot be adopted. `source`, `events` and `pull` always exist; `zetta` exists
only with a workspace binding. The sync writes their SHA-256 hex digests as:

```json
{"source":"<sha256 hex>","zetta":null,"events":"<sha256 hex>","pull":"<sha256 hex>"}
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
- `events`: `publish_app_event` for this app only, publishing to `app-events.<app id>`;
- `pull`: Basic-auth registry reads with username `app`, limited to manifests and blobs in
  `<space>/<app name>`; it cannot read another app or cache repository and cannot push.

Runtime pods receive `APP_TOKEN` from the app token Secret's `source` key. The Zetta token goes into
the local binding Secret's `zetta` key, mounted at `/var/run/orama/bindings/zetta`, as the one JSON
object app-runtime reads (its `docs/data-apps/bindings.md`):

```json
{"lake_url":"http://lake-<lake>.<lake namespace>.svc.cluster.local:80/w/<workspace id>","customer_id":"<customer id>","token":"<zetta token>"}
```

`lake_url` is the same value as `LAKE_URL`, and `customer_id` is `spec.bindings.zetta.customerId`.
app-runtime calls `<lake_url>/workspaces/<customer_id>/zetta/v1/<capability>` with the token as its
bearer. The bare token is never written to that file: it is not JSON, and app-runtime stopped every
app with workspace data at startup with *"binding is not valid JSON"* while it was (lake#1205). The
bytes are a contract: `internal/controller/testdata/app-binding-zetta.json` is checked byte for byte
by `lakeapp_binding_contract_test.go`, and app-runtime parses the same file in its own test. `EVENTS_TOKEN` comes from
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

- `spec.apps.runtimeImage`: the private `oramasearch/app-runtime` image used in runtime mode;
  empty uses the operator's default (`internal/controller/lake_defaults.go`);
- the operator passes that image to lake as `LAKE_APPS_RUNTIME_IMAGE`, so
  lake's generated build Dockerfile uses the configured private base image rather than an implicit
  Docker Hub library image;
- `spec.apps.wildcard`: the DNS suffix used when the app does not set `spec.host`; empty uses the
  Lake's own `spec.host`, so an app is `<app>.<lake host>` (`myapp.lake-dev.oramadev.net`). The
  operator injects it into lake as `LAKE_APPS_WILDCARD` so lake can validate and create app hosts.
  HTTPS on these two-level names needs a certificate per zone ([#515](https://github.com/oramasearch/orama-operator/issues/515));
- `spec.apps.ingressClass`: the app Ingress class, defaulting to `nginx`.

An image-mode app sets `spec.image` to an immutable `name@sha256:<64 hex>` reference. Its optional
`spec.pullSecretName` overrides the Lake's pull Secret. For synchronized apps, the controller
creates a `kubernetes.io/dockerconfigjson` Secret whose `auths` preserve the selected source
Secret entries and add `spec.host` with username `app` and the generated `pull` token as password.
The Deployment names this merged Secret in `imagePullSecrets`; deleting the app token Secret
rotates both the registered pull-token hash and the password in the merged Secret. The source pull
Secret always remains in the Lake namespace. Standalone CRs retain the original copy behavior into
their private namespace because they do not have operator-generated synchronized-app tokens.

The apps sync sets `spec.sync` on its Lake-owned CRs: the immutable app `id`, `customerId` and
`workspaceId`, the operator-written `workspaceNamespace`, the desired row `generation`, the
`restartGeneration` of its last restart ([restart](#restart)), and `tokenHashes` acknowledging
credentials registered in Postgres.
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

- `zetta`: the Lake-minted scoped workspace token when `bindings.zetta` is present. The controller
  delivers it inside the JSON binding document above, with Lake's in-cluster URL as `lake_url`;
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
controller and, in a separate rule, only the owning Lake pod in its exact namespace to that app's
labelled pod and declared port. The Lake rule pairs with its sandbox's exact-backend egress for
proxying; port changes update both ends. Neither rule adds general operator access or selects
another app. The policy carries a component justification annotation. Synchronized apps can reach the Lake
pod, on its API (container) port only (operator#711), for source, workspace reads and event publication; their scoped Zetta bearer is validated
by Lake, not sent directly to Zetta. Before the workspace activates declared connections, each
service binding keeps its compatibility rule selecting that service ID. Afterwards those binding
rules are removed and paired `Connection` allowances are the only app-to-workload paths; see
[`../infrastructure/connections.md`](../infrastructure/connections.md).
Standalone apps retain their direct workspace/bus Zetta policies. No general internet egress opens.

The pod runs as uid/gid 10001, drops all capabilities, prevents privilege escalation, uses runtime
default seccomp, has a read-only root filesystem, disables its Kubernetes token and service links,
gets a size-limited `emptyDir` at `/tmp`, and resolves with `ndots:2` so the full Service names it dials resolve on the first query ([DNS](README.md#dns-ndots-2-on-lake-agent-and-app-pods)). Synchronized-app pull credentials exist only in the
merged workload-namespace Docker config Secret; the public Lake host password is never copied into
the CR, pod template, status or logs. Runtime env includes `WORKSPACE_ID`, and `LAKE_URL`, auth and event URLs use Lake's
`/w/<workspace id>` route. The selected binding values appear as files at:

```text
/var/run/orama/bindings/zetta
/var/run/orama/bindings/services/<service name>
```

Runtime mode sets both the original app-runtime source variables and the expanded contract:
`LAKE_URL`, `APP_SOURCE=<repository>@<revision>`, `APP_REPOSITORY`, `APP_REVISION`, `APP_PATH`,
optional `APP_ENTRYPOINT`, `APP_FRAMEWORK`, `APP_TOKEN`, and `APP_PORT`. Standalone apps default
service filenames to the ID and obtain `EVENTS_URL` and `EVENTS_TOKEN` from their source Secret.

Every app, in either mode, also gets `APP_AUTH`: `lake` when the Ingress puts Lake's forward auth in
front of it (below), and `public` otherwise (any `spec.auth` other than empty or `lake` is reported
as `public`). It is a managed name, so neither the app's `env` nor its `secretEnv` can set it, and an
explicit `env` entry wins over `envFrom`. app-runtime reads Lake's identity headers only when it is
`lake` ([app-runtime#11](https://github.com/oramasearch/app-runtime/issues/11)).

## Ingress and authentication

An empty app host derives `<app>.<Lake spec.apps.wildcard>`, and an empty wildcard is the Lake's `spec.host`. That address uses the one shared
wildcard record maintained by the apps sync. A standalone custom host gets its own proxied record after nginx
reports a load-balancer target. The ingress class is `spec.apps.ingressClass`, default `nginx`.

With a wildcard (or a Lake host) and `dnsSecretRef`, the sync maintains one Lake-owned proxied
`CloudflareDNSRecord` in the Lake namespace. It uses the Lake ingress address when its class
matches, otherwise an owned app ingress of the configured class, selected deterministically.
No record is created until a target is reported. Suffix changes retire old owned records, and
removing the suffix and host, or the credentials, removes the wildcard. Foreign records are
never changed or deleted. This path is independent of `previewWildcard` and supports apps even
when the Lake itself has no public ingress.

`auth: lake` is the default. It adds nginx's `auth-url` annotation pointing at
`http://lake-<lake>.<namespace>.svc.cluster.local:80[/w/<workspace id>]/v1/apps/auth`. The fully
qualified Service name is required by ingress-nginx's resolver. Lake verifies the browser's session
and space membership before the request reaches the app. `auth: public` omits the annotation.
Lake's `apps` table stores public access as `none`; the row mapping reads it as `public`.
It also sets `auth-always-set-cookie: "true"`, so the app-scoped session cookie Lake returns from
the auth call reaches the browser. When the Lake has public ingress (`ingressEnabled` and `host`),
`auth-signin` sends a browser without that cookie to
`https://<lake host>/apps/authorize?app=<app id>&rd=https://$host$escaped_request_uri`. The return URL
is always https because TLS ends at the edge proxy. The handoff and cookie are specified in lake#407.

**Control-plane credentials (LSR-208).** The shared ingress-nginx platform configuration clears
`Authorization` before proxying to every tenant app, including public apps, runtime and image apps,
and custom hosts. It drops the entire `Cookie` header if it contains `lake_session`,
`__Host-lake_session` or `__Secure-lake_session` (case-insensitive, including repeated cookie
headers). Otherwise it preserves application cookies, including `lake_app_session`. This fails
closed for mixed cookie headers: the app also loses its own cookies on that request. Clients must
use an app-scoped session; an app cannot use the incoming `Authorization` header for its own auth.
Lake's auth subrequest still receives the original cookies so it can verify app access and return
the app session cookie.

The maps select nginx's operator-generated `$ingress_name`, never a client header or host:
standalone apps use `app`, synchronized apps `app-<12 hex>`. These names are reserved for tenant
apps. Lake's shared API/console Ingress uses `lake-<name>`, its wildcard handlers add
`-apps-wildcard` or `-preview-wildcard`, environments use `environment`, Cloud v1 adds component
suffixes (`-dashboard`, `-docs`, `-writer`, `-reader` and `-metrics`), and Zetta adds `-zetta` or
`-zetta-landing`. None can match the app pattern, even when its CR is named `app`. Those locations
preserve `Authorization` and cookies. The wildcard handlers proxy to Lake itself; a concrete app
Ingress takes precedence when the app is deployed. Keep this naming contract and the platform maps
in sync when adding an Ingress producer. Both `helmfile.yaml` and every cluster's `platform.yaml`
carry the same maps and header ConfigMap values. Per-Ingress snippet annotations remain disabled.

This operator change covers ingress credential forwarding. Lake's `/apps/authorize` must separately
require a browser session, and its scripted scenario probes must separately obtain only an
app-scoped session for app requests; those changes belong to the Lake repository.

**Identity headers.** With `auth: lake` the Ingress names `X-Orama-User,X-Orama-Space` as
`auth-response-headers`. ingress-nginx then sends the app each of those headers set from Lake's
answer to the auth call, and a copy the client sent never reaches the app: it is replaced, or dropped
when Lake's answer has none (an empty `proxy_set_header` value sends no header). The app's access
policy admits only ingress-nginx and its Lake, so nothing reaches the pod around the Ingress. No other
header is replaced, so `X-Orama-User-Id`, `X-Auth-Request-*` and the like arrive as the client wrote
them. With `auth: public` there is no auth call and **every** identity header, `X-Orama-User`
included, is the client's: an app must not treat any of them as identity, and app-runtime returns no
signed-in user there (`APP_AUTH=public`). The Ingress does not strip them on a public app because
that would need an nginx snippet; app-runtime removes them instead before app code sees the request.
Lake apps stay on ingress-nginx and are not served by the zetta-tunnel gateway
(`ingressRefusal`), which makes no auth subrequest.

The bytes an app's Ingress carries are metered: the operator reads ingress-nginx's per-Ingress byte
counters and reports them by app and hour in `app_transfer` ([app-transfer.md](app-transfer.md),
lake#661).

## Status and events

The operator sets only its own owner annotation on the app Deployment and keeps every other
key. The Deployment controller owns `deployment.kubernetes.io/revision`, and a Deployment bumps
`metadata.generation` on annotation changes, so replacing the map made the two rewrite each other
forever and the rollout never reported observed (#230).

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
Unit cases cover failed-list orphan safety, foreign ownership/bindings, merged pull-Secret contents
and malformed-input sanitization, DNS changes and ingress class selection, listener enable/disable,
the schema gate and fallback interval, and HTTP events.
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
`lakeapp_binding_contract_test.go` pins the Zetta binding file app-runtime reads: the exact bytes of
`testdata/app-binding-zetta.json`, and a synchronized app's binding carrying the registered token
inside that document (lake#1205).
`lakeapp_isolation_test.go` pins the isolation rules a tenant's app runs under: restricted pod
security and no Kubernetes token, egress only to cluster DNS, the Lake's API port and the app's own
workspace, ingress only from ingress-nginx and the owning Lake, no workspace Kubernetes API or
operator allowance selecting an app, and Lake-protected identity headers set from Lake's answer.
The security review behind them is [`../security/deep-search-711-tenant-app.md`](../security/deep-search-711-tenant-app.md).
The Services/Stores shared-Secret regression additionally verifies ID aliases and their withdrawal.

The field-by-field reference is in [`../crd-reference.md`](../crd-reference.md#lakeapp-separate-crd-namespaced--one-deployed-app).

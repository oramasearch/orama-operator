# Lake apps

`LakeApp` is the Kubernetes desired-state record for one app belonging to a cluster-scoped
`Lake`. The resource lives in the Lake namespace. The Lake apps-table sync is separate from this
CRD: it creates and updates `LakeApp` resources and the source Secrets that hold credentials.

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
- `spec.apps.wildcard`: the DNS suffix used when the app does not set `spec.host`;
- `spec.apps.ingressClass`: the app Ingress class, defaulting to `nginx`.

An image-mode app sets `spec.image` to an immutable `name@sha256:<64 hex>` reference. Its optional
`spec.pullSecretName` overrides the Lake's pull Secret. The source pull Secret always remains in the
Lake namespace; the controller copies it into the isolated app namespace.

## Credential inputs

Credentials never appear in `LakeApp.spec`, Deployment environment values, annotations, status, or
logs. The Lake-side sync writes source Secrets in the Lake namespace and puts only their names in the
CR.

`spec.source.tokenSecretRef` selects the Lake-minted repository read token used as `APP_TOKEN`.
Runtime mode requires its Secret name and key. The workload copy contains only the selected value,
even when the source Secret holds other keys.

`spec.bindings.secretRef` points at a Secret with these data keys:

- `zetta`: the Lake-minted scoped workspace token when `bindings.zetta` is present;
- one key equal to each requested service identifier: that service's connection document.

The controller copies only those named keys. Generic `spec.secretRefs` name Lake-namespace Secrets
that are copied and exposed through `envFrom`. `spec.eventsSecretRef` names a Secret with `url` and
`token` keys for the app's `app-events.<app>` topic. A source Secret change changes the pod-template
credential digest, so Kubernetes performs a rollout without putting secret data in the template.

## Resources and isolation

The resource's stable workload namespace is `lake-<lake>-app-<10-hex-hash>`, derived from Lake and
`LakeApp` identity. It carries Pod Security `restricted` labels and an ownership annotation that the
finalizer verifies before deletion. The controller creates:

- `ResourceQuota tenant-defaults` and `LimitRange tenant-defaults`;
- a default-deny ingress/egress `NetworkPolicy` and an `app-access` policy;
- Deployment and Service `app`;
- Ingress `app` when a host is available;
- a proxied `CloudflareDNSRecord` for a custom host when Lake DNS credentials are configured.

The access policy admits the nginx ingress namespace and DNS. Runtime mode can reach the Lake
Service to load source. A Zetta binding can reach the customer's Zetta service pods, each requested
service binding gets a rule selecting only that service identifier, and event credentials allow the
Lake bus Zetta pods. No general internet egress is opened.

The pod runs as uid/gid 10001, drops all capabilities, prevents privilege escalation, uses runtime
default seccomp, has a read-only root filesystem, disables its Kubernetes token and service links,
and gets a size-limited `emptyDir` at `/tmp`. Private pull credentials are copied into the workload
namespace. The selected binding values appear as files at:

```text
/var/run/orama/bindings/zetta
/var/run/orama/bindings/services/<service-id>
```

Runtime mode sets both the original app-runtime source variables and the expanded contract:
`LAKE_URL`, `APP_SOURCE=<repository>@<revision>`, `APP_REPOSITORY`, `APP_REVISION`, `APP_PATH`,
`APP_ENTRYPOINT`, `APP_FRAMEWORK`, `APP_TOKEN`, and `APP_PORT`. Event credentials become
`EVENTS_URL` and `EVENTS_TOKEN` through Secret key references.

## Ingress and authentication

An empty app host derives `<app>.<Lake spec.apps.wildcard>`. That address uses the one shared
wildcard record maintained by the apps sync. A custom host gets its own proxied record after nginx
reports a load-balancer target. The ingress class is `spec.apps.ingressClass`, default `nginx`.

`auth: lake` is the default. It adds nginx's `auth-url` annotation pointing at Lake's in-cluster
`/v1/apps/auth` endpoint, which verifies the browser's Lake session and space membership before the
request reaches the app. `auth: public` omits the annotation.

## Status and events

Status is deep-compared before every write. `Ready` requires the Deployment controller to have
observed the current generation and every desired replica to be updated and available. Pod status
takes priority over generic rollout progress: `ImagePullBackOff`/`ErrImagePull`,
`CrashLoopBackOff`, and `OOMKilled` produce stable `Degraded` messages. `status.observedRevision`
is the source revision in runtime mode or image digest in image mode.

Only a real status transition publishes an event. The controller reads the scoped event Secret and
POSTs `publish_event` to Zetta with JSON payload on `app-events.<app>`. Production publication runs
asynchronously with a short timeout; a missing credential, unreachable bus, or rejected event is
logged and never fails or delays resource reconciliation. The same event Secret is delivered to the
app so app-runtime can publish its own progress.

Deletion records `TornDown`, publishes that transition best-effort, removes any individual DNS
record, verifies namespace ownership, deletes the namespace, waits until it is absent, and only then
releases the finalizer.

## Verification

The native API-server test exercises create, source update, rollback, pod-failure status, binding
delivery, exact NetworkPolicy targets, credential rotation, and deletion:

```sh
export KUBEBUILDER_ASSETS="$(setup-envtest use -p path 1.30.x)"
go test -count=1 -v ./internal/controller -run '^TestLakeAppEnvtestLifecycle$'
```

The repository proof also runs `go build ./...`, `go vet ./...`, `go test ./...`, and
`helm lint chart`. A dev-cluster e2e remains deferred until the Lake apps-table sync and app-runtime
image are available.

The field-by-field reference is in [`../crd-reference.md`](../crd-reference.md#lakeapp-separate-crd-namespaced--one-deployed-app).

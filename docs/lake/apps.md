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

`spec.bindings.secretRef` points at a Secret with these data keys:

- `zetta`: the Lake-minted scoped workspace token when `bindings.zetta` is present;
- one key equal to each requested service identifier: that service's connection document.

The controller copies only those named keys. Generic `spec.secretRefs` name Lake-namespace Secrets
that are copied and exposed through `envFrom`. `spec.eventsSecretRef` names a Secret with `url` and
`token` keys for the app's `app-events.<app>` topic. A source Secret change changes the pod-template
credential digest, so Kubernetes performs a rollout without putting secret data in the template.

The field-by-field reference is in [`../crd-reference.md`](../crd-reference.md#lakeapp-separate-crd-namespaced--one-deployed-app).

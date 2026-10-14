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
| service | `orama.cloud/service-id=<id>` |
| agent | `orama.cloud/agent-id=<id>` |
| tunnel | `orama.cloud/tunnel-grant-id=<id>`: the grant's zetta-tunnel endpoint or `cloudflared` connector. The operator generates `Connection/connection-tunnel-<grant id>` itself, controlled by the `ZettaTunnel` or `CloudflareTunnel` of the same Lake and grant ([tunnels](tunnels.md)) |

Targets are services or apps. A peer with only a pod selector is namespace-local, so even a bad ID
cannot select a workload in another workspace. Explicit `ports` are TCP destination ports. An empty
list is narrowed to the target's Kubernetes `Service` target ports, including named ports; it never
opens every pod port. A target with no discoverable Service stays `Pending`.

Credential issuance is deliberately not part of this change. A row with non-null
`credential_scope` is `Failed` with a reference to #469 rather than silently granting unscoped
access.

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
stays empty in #468. The CR status uses the same phase, message and observed generation and is also
comparison-gated.

## Verification boundary

Native tests cover app-to-service selector mapping, paired ingress and egress, target Service port
resolution, guarded rollout, removal in one reconcile, absence before `network_guard`, legacy app
egress retirement and non-flapping status writes. The opt-in pgx test creates the 0084 connection
tables itself because that migration is not yet vendored.

The named acceptance probe on `orama-nonprod-cluster-01` is deferred to rollout: an app with one
connection reaches only its selected service, the same app without the connection is refused, and
setting the row absent refuses the path after one reconcile.

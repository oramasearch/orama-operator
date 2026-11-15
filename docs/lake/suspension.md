# Suspending a Lake

A Lake with `spec.suspended: true` runs no compute and keeps all of its data
([operator#862](https://github.com/oramasearch/orama-operator/issues/862)). A `LakeOrderSource` sets it
on an ordered Lake while lake's `cloud_order_desired_state` says the order is not paid for
([orders.md](../infrastructure/orders.md), lake#972). Setting it on any other Lake has the same effect.
The code is `internal/controller/lake_suspension.go`.

## What stops, and when

| When | What | How |
|---|---|---|
| At once | The lake process | The lake Deployment runs `0` replicas, whatever `spec.replicas` says. |
| At once | Control rows | No service, store, connection, egress rule, tunnel, peer, agent launch, schedule, build, methodology run or served model row is acted on, and no workspace row: nothing new starts and nothing missing is made again. |
| At once | Library agents, launched agents, served models and every other Deployment the Lake controls | Scaled to `0`. The replica count it had is kept in the annotation `orama.cloud/suspended-replicas`. |
| At once | Unfinished Jobs the Lake controls (builds, methodology runs, agent launches) and its CronJobs (agent schedules) | `spec.suspend: true`, so Kubernetes stops their pods and keeps the Job. The `spec.suspend` each had is kept in `orama.cloud/suspended-from`. A finished Job is left alone. |
| At once | Every `LakeApp` and `LakeEnvironment` of the Lake | Their own reconcilers run their Deployments at `0` and report `LakeSuspended`. |
| After the grace | Every `OramaZettaCluster` the Lake controls: its own, its workspaces' and its Zetta services' | `spec.suspended: true`: the StatefulSet runs `0` nodes, and its claims, Services, Secrets and node identities stay. The phase is `Suspended`. |

The **grace is 72 hours** from `status.suspendedSince`, the time the operator first saw
`spec.suspended`. It is a constant, the same on every cluster, with no setting. lake's own entitlement
already has its own grace before an order becomes `suspended`; this one keeps the Zettas readable for a
renewal that comes soon after, so their data is warm when the Lake starts again. The Lake is looked at
again when the grace ends.

The Lake reports `Ready=False` with reason `Suspended` and a message that says whether its Zettas are
still running and until when.

## What is kept

Nothing is deleted. The namespace, the control Postgres (CNPG), the store claim, every volume and claim,
every Secret and Service, the managed services (they keep running: each kind stops differently, see
below), the host, the DNS records and the names all stay. The Lake's status keeps its last values.

## Ending a suspension

When `spec.suspended` is `false` again, `status.suspendedSince` is cleared and the next pass:

1. gives every Deployment with `orama.cloud/suspended-replicas` its count back and removes the mark;
2. gives every Job and CronJob with `orama.cloud/suspended-from` its own `spec.suspend` back (a CronJob
   its owner had suspended stays suspended) and removes the mark;
3. sets `spec.suspended: false` on every Zetta the Lake controls;
4. acts on the control rows again, so anything that changed while the Lake was suspended is reconciled.

The lake Deployment, the apps and the environments follow their own specs again.

## Not stopped

- **Managed services** (TiDB, Cassandra, Valkey, Kafka, ClickHouse, Postgres services) keep their pods.
  Each upstream operator stops differently (CNPG hibernation, K8ssandra `stopped`, a StatefulSet at
  zero for the others), so each needs its own step.
- **A dedicated cluster's node pools** are not changed. Autoscaled pools shrink by themselves to their
  `minCount` once the compute has stopped. A fixed-count pool keeps its nodes: the AKS provider reports
  pool drift and does not apply pool changes to an existing cluster (`aksDrift`), so changing a pool for
  a suspension needs that first.

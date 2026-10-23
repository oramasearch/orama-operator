# The operator's memory

[operator#677](https://github.com/oramasearch/orama-operator/issues/677). The chart gives the manager
`requests: {memory: 128Mi}` and `limits: {memory: 256Mi}` (`chart/values.yaml`, `operator.resources`).
The values are the same on every cluster: management and leaf clusters are installed from the same
chart with the same values (principle 11), so never set them for one cluster. Two settings keep the
operator under that limit, and both are in `cmd/memory.go`.

## The Go runtime knows the limit

The chart gives the manager container `OPERATOR_MEMORY_LIMIT`, its own `limits.memory` in bytes,
through a `resourceFieldRef`. At start the operator sets the Go runtime's soft memory limit to 85% of it
(`debug.SetMemoryLimit`; 218 MiB under 256Mi). The other 15% is for goroutine stacks, memory outside
the Go heap and fragmentation. A `GOMEMLIMIT` in the environment overrides it, and a local `make run`
has no limit. This is principle 8 (b): the binary takes the limit from the same number as its
container.

Without it, Go collects garbage only when the heap doubles since the last collection (GOGC=100). So a
live heap of about 130 MiB can grow past 256Mi before the next collection, and the kernel kills the
container.

## The cache leaves out what no controller reads

The manager's cache, also used for `WATCH_NAMESPACE` runs:

- **Holds every Secret except Helm's release revisions** (label `owner=helm`). Helm stores every
  revision of every release as a Secret with the whole chart in it. The operator never reads those
  through its cache: Helm reads a leaf's releases through its own client. A `Get` of such a Secret
  through the manager's client answers NotFound; read one with the API reader if it is ever needed.
- **Strips `managedFields`** from every cached object (`cache.TransformStripManagedFields`). No
  controller reads them, and server-side apply keeps them on the server.

## What caused the OOM, measured on the managing cluster

On 2026-10-20 the v0.7.1 operator was OOMKilled 20 seconds after it started on
`orama-production-cluster-01`. It restarted once and then ran at 95–140Mi. The pieces were measured
on 2026-10-21 with read-only list, watch and Helm `Get` calls from a probe that used the operator's
own libraries:

| What | Measured |
|---|---|
| Secrets on the managing cluster | 224, about 11.8 MiB of JSON. 57 Helm release Secrets are 96% of it (10.7 MiB) |
| Syncing the Secret cache, all Secrets | 18 MiB retained, 48 MiB allocated |
| The same without Helm release Secrets | 2 MiB retained, 4 MiB allocated |
| Stripping `managedFields` from Secrets | Negligible for Secrets (88 KiB). It saves more on Pods, ReplicaSets and Deployments |
| One Helm `Get` of a platform chart (cert-manager, cnpg, strimzi, clickhouse, k8ssandra) | 13–18 MiB allocated, peak 6–11 MiB above baseline. `Get` decodes every stored revision |
| One Helm `Get` of `orama-operator` (10 revisions) | 43 MiB allocated, peak 19 MiB above baseline |

At start-up, eight `ClusterHelmRelease` workers (`clusterHelmReleaseWorkers`) each run a Helm `Get`
against a leaf. Every `OramaOperator` runs one for the operator's own release while the caches fill.
Together with the full Secret cache, the heap was live at well over 100 MiB. With no memory limit
known to Go, the heap goal (twice the live heap) passed 256Mi.

## Checking it on a cluster

Read only: `kubectl -n orama-system get pod -l app.kubernetes.io/name=orama-operator` (restarts, and
`lastState.terminated.reason`), `kubectl -n orama-system top pod`, and the manager's start-up log line
`Go memory limit set from the container's`.

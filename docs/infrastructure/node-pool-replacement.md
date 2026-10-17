# Applying node-pool sysctls to an existing AKS cluster (design, not implemented)

Status: **design awaiting Steve's approval**. Nothing here is implemented. Tracked in
[operator#579](https://github.com/oramasearch/orama-operator/issues/579), from
[#454](https://github.com/oramasearch/orama-operator/issues/454). It changes the `AksCluster`
controller's "create, observe and delete only" principle, which is why it needs approval first.

## Why the pool cannot be changed in place

AKS takes `linuxOSConfig` only when a cluster or a node pool is created:

- Microsoft's [custom node configuration guide](https://learn.microsoft.com/en-us/azure/aks/custom-node-configuration)
  documents it at two points only: *"When you create an AKS cluster or add a node pool to your
  cluster, you can customize a subset of commonly used OS and kubelet settings"*, with
  `az aks create --linux-os-config` and `az aks nodepool add --linux-os-config`. It has no
  update procedure.
- The [`az aks nodepool` reference](https://learn.microsoft.com/en-us/cli/azure/aks/nodepool?view=azure-cli-latest)
  lists `--linux-os-config` and `--kubelet-config` on `az aks nodepool add`, and on neither
  variant of `az aks nodepool update` (core or `aks-preview`).
- An agent-pool update that changes the setting is refused by the resource provider with
  `CustomKubeletConfigOrCustomLinuxOSConfigCanNotBeChanged` ("CustomKubeletConfig or
  CustomLinuxOSConfig can not be changed for this operation."), as reported against the
  provider through Terraform ([HashiCorp discussion 58557](https://discuss.hashicorp.com/t/unable-to-upgrade-default-node-pool-of-aks-cluster-using-terraform/58557)).

The same guide says node image upgrades reapply the configuration and scale-out preserves it,
so once a pool carries it, the setting survives the cluster's `NodeImage` upgrade channel and
the autoscaler.

So an existing pool gets the setting only through a pool created with it. Today the
`AksCluster` controller never adds a pool to an existing cluster, never drains a node and never
deletes a pool: an edit to `sysctls` on an existing cluster reports `InSync=False` and nothing
else.

## What blocks a plain replace-and-drain

#454 sketched it as: add a replacement pool with the settings and drain the old one. On our
clusters the drain cannot finish without downtime:

- The OramaCloudV1 controller gives the writer and every data store a PodDisruptionBudget with
  `maxUnavailable: 0` (`internal/controller/engine.go` `upsertPDB(..., "writer", 0)`,
  `internal/controller/datastore.go`). An eviction of those pods is refused for as long as the
  budget stands, by design.
- Readers have no node placement (no `nodeSelector` or affinity on the reader StatefulSet), so
  readers, the writer and the data stores share `agentpool` and `userpool` on
  `orama-nonprod-cluster-01`, `orama-production-cluster-02` and `-03`. Every pool readers can
  use also holds zero-budget pods.

A drain that respects the budgets therefore stops on the writer and the data stores of every
stack, and a drain that overrides them is the outage the budgets exist to prevent.

## Option A: replace the pool and drain it (the #454 sketch)

For one pool at a time, when the pool's declared `sysctls` differ from what Azure reports:

1. **Add.** `PUT .../agentPools/<new>` with the old pool's mode, VM size, counts, zones, labels
   and taints, plus `linuxOSConfig`. AKS names are fixed per pool, so the new pool takes the
   spec name with a one-character suffix (spec names are limited to 11 characters to leave
   room), alternating back on the next replacement; status maps the spec name to the Azure name.
   Same zones, so zonal disks of readers and data stores still have nodes to attach to.
2. **Cordon** every node with `kubernetes.azure.com/agentpool=<old>`, through the leaf's admin
   kubeconfig the controller already writes (`orama-system/<cluster>-kubeconfig`).
3. **Drain** with the Eviction API only, so every PodDisruptionBudget is honoured; DaemonSet
   and mirror pods are left. A refused eviction is retried on each reconcile; after a fixed wait
   the resource reports `DrainBlocked` naming the pod and its budget. The operator never deletes
   a pod directly and never edits a budget.
4. **Delete** `agentPools/<old>` once it has no evictable pods left, and wait for Azure to report
   it gone. A system pool is deleted only after its replacement, also `System`, is `Succeeded`.

Progress lives in status, one entry per pool being replaced:
`{name, oldAzureName, newAzureName, step: Adding|Draining|Deleting, startedAt, message}`. Every
step is idempotent (the same `PUT`, cordon, eviction or `DELETE` again), so an operator restart
resumes from the recorded step. One pool per cluster is replaced at a time.

Failure and recovery:

- The add fails (quota, capacity, a refused value): the step stays `Adding` and Azure's error is
  in the message; a failed new pool is deleted and retried. The old pool is untouched.
- The drain is blocked: `DrainBlocked`, as above. With Option A alone this is the normal outcome
  for any pool with a writer or data store on it.
- Reverting the `sysctls` edit before `Deleting` aborts: the old nodes are uncordoned and the new
  pool is deleted. After the old pool is gone, going back is another replacement.

What it changes: `AksCluster` would add, cordon, drain and delete pools on an existing cluster.
Production clusters are `deletionPolicy: Retain`, and the replacement starts only from a
committed edit to their `cluster.yaml`, applied with Steve's consent.

## Option B: add a reader pool and place the readers on it (recommended)

Only the readers need the setting (#454). Instead of replacing shared pools:

1. Declare a new pool, for example `readers` (`role: user`, the same machine size and zones,
   `sysctls: {vm.max_map_count: 4194304}`, label `orama.cloud/pool: readers`), in each cluster's
   `cluster.yaml`. The `AksCluster` controller learns to **add** a declared pool that is missing
   (`PUT .../agentPools/<name>`). It still never changes or deletes an existing pool.
2. The reader StatefulSet gets a required node placement on that label. The StatefulSet's rolling
   update moves readers one at a time under the reader budget (`maxUnavailable: 1`), the same way
   an image roll does. The writer and the data stores do not move.

No pool is drained, no zero-budget pod is evicted, and the existing pools keep running as they
are. It needs a way to say where readers run: either a fixed label the reader controller always
requires (every cluster then needs the pool before readers schedule), or a field on the set
member. The fixed label is the smaller change and keeps every cluster the same.

Failure and recovery: a failed add is reported with Azure's error and retried; readers stay where
they are until the pool exists, because the placement is applied only once the pool reports
`Succeeded`. Undo is removing the placement (readers roll back onto the shared pools); the pool
can stay.

## Option C: a node DaemonSet that writes the setting (not recommended)

Microsoft's guide points to a DaemonSet for settings outside the supported subset. A privileged
DaemonSet could write `vm.max_map_count` on every node of an existing pool with no new pool. It
was not chosen because Steve's review of #406 placed the setting on the node pool, and a
privileged host-writing container is what that review moved away from.

## Recommendation

Option B. It gets readers onto nodes with the setting without draining a writer or data store,
and it widens the `AksCluster` principle by one verb (add a declared pool) instead of three
(add, drain, delete). Option A stays available if a shared pool ever needs a creation-only
setting itself.

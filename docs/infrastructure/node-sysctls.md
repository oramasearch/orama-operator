# Node-pool sysctls

A pool's Linux kernel settings belong in
`KubernetesCluster.spec.nodePools[].sysctls`. The abstract controller copies them to its
owned `AksCluster`; the AKS controller validates them before making any Azure request,
then sends them as `agentPoolProfiles[].linuxOSConfig.sysctls` when creating the cluster.

For example, on a user pool intended for OramaCore readers:

```yaml
nodePools:
  - name: readers
    role: user
    machineSize: Standard_D8as_v7
    count: 2
    labels:
      workload: oramacore-reader
    sysctls:
      vm.max_map_count: 4194304
      vm.swappiness: 0
      net.ipv4.tcp_tw_reuse: 1
      net.ipv4.ip_local_port_range: "20000 60000"
```

This fragment supplements the resource's required system pool. Configure every pool where
readers can schedule, or constrain the readers to configured pools with workload placement.
Adding a label alone does not constrain a workload.

## Supported settings and values

The map uses Linux kernel names. Integer settings require YAML integers; Boolean kernel
settings use integer `0` or `1`; port ranges use a string with two space-separated ports.
Numeric strings and unsupported names are refused with a message naming the pool and setting.
The abstract resource becomes `Degraded` and creates no concrete resource for invalid settings;
a directly authored `AksCluster` becomes `Degraded` with reason `InvalidSysctls`. Both report
`InSync=False` for invalid sysctls, including after a previously valid configuration.

The supported subset is the 28 fields in Microsoft's
[AKS API 2025-05-01 SysctlConfig](https://github.com/Azure/azure-rest-api-specs/blob/main/specification/containerservice/resource-manager/Microsoft.ContainerService/aks/stable/2025-05-01/managedClusters.json):

- `fs.aio-max-nr`, `fs.file-max`, `fs.inotify.max_user_watches`, `fs.nr_open`
- `kernel.threads-max`
- `net.core.netdev_max_backlog`, `net.core.optmem_max`, `net.core.rmem_default`,
  `net.core.rmem_max`, `net.core.somaxconn`, `net.core.wmem_default`, `net.core.wmem_max`
- `net.ipv4.ip_local_port_range`, `net.ipv4.neigh.default.gc_thresh1`,
  `net.ipv4.neigh.default.gc_thresh2`, `net.ipv4.neigh.default.gc_thresh3`,
  `net.ipv4.tcp_fin_timeout`, `net.ipv4.tcp_keepalive_intvl`,
  `net.ipv4.tcp_keepalive_probes`, `net.ipv4.tcp_keepalive_time`,
  `net.ipv4.tcp_max_syn_backlog`, `net.ipv4.tcp_max_tw_buckets`, `net.ipv4.tcp_tw_reuse`
- `net.netfilter.nf_conntrack_buckets`, `net.netfilter.nf_conntrack_max`
- `vm.max_map_count`, `vm.swappiness`, `vm.vfs_cache_pressure`

Values are checked against Microsoft's
[custom node configuration reference](https://learn.microsoft.com/en-us/azure/aks/custom-node-configuration-reference).
Integer fields are limited to signed 32-bit values by this REST API, even though the kernel's
`fs.file-max` range is wider. Settings without a documented interval accept positive int32
values. The API represents `tcp_tw_reuse` as a Boolean, so kernel value `2` is unsupported here.
Port ranges require first port `1024..60999`, last port `32768..65535`, and first < last.

## Drift and existing pools

`InSync=False` names the pool and configured sysctl whose reported value differs. An
unreported value is unknown and is also drift. Omitted settings are unmanaged: the operator
neither compares defaults nor resets them when removed from the resource. It compares Azure's
reported pool configuration, not a direct measurement of the running kernel. A provisioned
cluster remains `Ready` while drift is reported, and steady drift produces no status writes.

The controller creates, observes and deletes clusters; it does not update existing clusters,
add pools to existing clusters, drain nodes or replace pools. Editing these settings on an
existing resource therefore reports drift without applying them.

An invalid edit (an unsupported name, a numeric string, a value out of range) on an existing
`AksCluster` stops observation until it is corrected: the controller sets `Degraded` and
`InSync=False` with reason `InvalidSysctls` and returns before reading the cluster from Azure,
so the provisioning state, node-pool status, Azure's latest operation and the kubeconfig Secret
are not refreshed meanwhile. The Azure cluster itself is untouched. Fix or revert the edit and
the next reconcile observes the cluster again. The same edit on a `KubernetesCluster` marks it
`Degraded` and is not copied to its `AksCluster`, which keeps its last valid spec and goes on
being observed.

## AKS cannot change an existing pool's settings

AKS takes `linuxOSConfig` only when a cluster or pool is created. Microsoft's
[custom node configuration guide](https://learn.microsoft.com/en-us/azure/aks/custom-node-configuration)
documents it only at `az aks create` and `az aks nodepool add`; the
[`az aks nodepool` reference](https://learn.microsoft.com/en-us/cli/azure/aks/nodepool?view=azure-cli-latest)
has `--linux-os-config` on `add` and not on `update` (core or `aks-preview`); and the provider
refuses such an update with `CustomKubeletConfigOrCustomLinuxOSConfigCanNotBeChanged`. The same
guide says node image upgrades reapply the configuration and scale-out preserves it.

An existing cluster therefore gets the setting only through a pool created with it. How the
operator does that is designed in [#579](https://github.com/oramasearch/orama-operator/issues/579), awaiting Steve's approval, in
[node-pool replacement](node-pool-replacement.md): replacing and draining the shared pools
cannot finish because the writer and data stores have `maxUnavailable: 0` budgets, so the
recommendation is to add a reader pool with the setting and place the readers on it. Until that
is approved and implemented, the setting reaches only clusters created after it is declared.

## Which pools

Readers have no node placement today, so they can run on any pool: `agentpool` and `userpool`
on `orama-nonprod-cluster-01`, `orama-production-cluster-02` and `orama-production-cluster-03`.
Declaring `vm.max_map_count: 4194304` on both pools of each cluster is the change that would
cover them without placement. It is not committed: applied today it would only turn `InSync`
False, and a file that differs from what is applied is state the repository misstates. With the
recommended design, the setting goes on a new `readers` pool instead.

## Reader value and rollout gates

Issue #454 requests `vm.max_map_count: 4194304` based on #406's account of readers retaining
about 50,000 mappings per day. These are reported inputs, not a fresh Prod 1 measurement.
At that reported rate, 1,048,576 mappings provide about 21 days from zero, and 4,194,304 provide
about 84 days; existing mappings reduce that headroom. A higher limit postpones exhaustion
and does not resolve unbounded mapping growth.

Prod 1's value is read on the US reader (`i-0dd41c106a2236d3d`, us-east-2) over SSM, read-only,
by whoever holds Prod 1 access: `/proc/sys/vm/max_map_count`, the reader process's start time
and its line count in `/proc/<pid>/maps`, twice at least an hour apart. Keep customer paths and
command-line secrets out of what is recorded.

Roll out through the resources in `config/clusters/`: `orama-nonprod-cluster-01` first, then,
with production approval, `orama-production-cluster-02` and `orama-production-cluster-03`.
Verify the node kernel value (`/proc/sys/vm/max_map_count` from a pod on the pool), that readers
run on the configured pool, and `InSync=True`. No cluster resource or live setting was changed
for this implementation.

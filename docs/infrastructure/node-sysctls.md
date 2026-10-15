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

Update semantics were reviewed against Microsoft's
[Agent Pools Create Or Update API 2025-05-01](https://learn.microsoft.com/en-us/rest/api/aks/agent-pools/create-or-update?view=rest-aks-2025-05-01)
and [custom node configuration guide](https://learn.microsoft.com/en-us/azure/aks/custom-node-configuration).
The API includes `linuxOSConfig` in the request model but does not document whether changing
it on an existing pool is accepted or how running nodes are updated. The guide documents
cluster creation and adding new pools; that alone does not prove in-place update support.
Existing-pool updates remain unverified, rather than inferred from the endpoint's name.

Before applying this to existing pools, obtain provider confirmation or qualify an approved
nonprod update. If replacement is required, design it in the operator with persistent progress,
capacity/quota checks, workload placement, disruption budgets, drain failure recovery, rollback
and safe retirement of the old pool. Neither path is implemented by this change.

## Reader value and rollout gates

Issue #454 requests `vm.max_map_count: 4194304` based on #406's account of readers retaining
about 50,000 mappings per day. These are reported inputs, not a fresh Prod 1 measurement.
At that reported rate, 1,048,576 mappings provide about 21 days from zero, and 4,194,304 provide
about 84 days; existing mappings reduce that headroom. A higher limit postpones exhaustion
and does not resolve unbounded mapping growth.

Record the actual Prod 1 reader node limit, the reader process's current mapping count,
process start time and at least two timestamped counts for the same process before claiming
the value has live evidence. Read `/proc/sys/vm/max_map_count` and count `/proc/<pid>/maps` via
an approved read-only host access path; keep customer paths and command-line secrets out of
receipts. No approved source host or metrics access path was available for a fresh measurement.

Roll out through the resources in `config/clusters/`: qualify
`orama-nonprod-cluster-01` first, then ask for production approval before
`orama-production-cluster-02` and `orama-production-cluster-03`. Existing-pool lifecycle support
is a prerequisite. Verify the actual node kernel value, reader scheduling, and that the
setting survives node replacement/scale-out; verify `InSync` against Azure as well.
Code and stub tests do not qualify a live rollout. No cluster resource or live setting was
changed for this implementation.

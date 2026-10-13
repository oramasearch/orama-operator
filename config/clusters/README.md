# The clusters, as applied

The resources that describe every managed cluster and what runs on it, exactly as they are applied
to the managing cluster (`kubectl` context `orama-production-cluster-01`). **These files are the
record of what is deployed.** Change a file here, apply it, and commit both in the same change; a
`kubectl edit` or `kubectl patch` that is not followed by an update here leaves the record wrong.

How to work with them: [`docs/infrastructure/operations.md`](../../docs/infrastructure/operations.md).
Why they are shaped this way: [`docs/infrastructure/README.md`](../../docs/infrastructure/README.md).

| File | What it holds |
|---|---|
| [`provider.yaml`](provider.yaml) | `KubernetesClusterProvider/azure-orama`: the Azure subscription and defaults every cluster is created with. |
| `<cluster>/cluster.yaml` | The `KubernetesCluster`: region, version, node pools, `purpose` tag, deletion policy. |
| `<cluster>/platform.yaml` | One `ClusterHelmRelease` per chart the cluster runs, in install order. |
| `<cluster>/operator.yaml` | The `OramaOperator`: the operator version the cluster runs. |
| `sets/lake-<purpose>.yaml` | A `LakeSet`: the Lake instances of that purpose. |
| `sets/v1-<purpose>.yaml` | An `OramaCloudV1Set`: the Orama Cloud v1 instances of that purpose. |

| Cluster | Region | Purpose | Deletion policy |
|---|---|---|---|
| `orama-nonprod-cluster-01` | `westus2` | `nonprod` | `Delete` |
| `orama-production-cluster-02` | `swedencentral` | `production` | `Retain` |

The managing cluster, `orama-production-cluster-01`, was created by hand before these kinds
existed and has no `KubernetesCluster` of its own.

No file here holds a secret. Secrets are named by reference and live on the managing cluster.

To check that the files match the cluster:

```sh
kubectl --context orama-production-cluster-01 diff -f config/clusters/ -R
```

Empty output means they match.

# Example: a management cluster's resources

An example of the resources a management cluster holds to run Lake on its leaf clusters, in the shape
of a real deployment. **The values are placeholders**: every cluster, host, account id, address and
Secret name here is made up. A real deployment keeps its own files in its own repository and applies
them to its management cluster.

| File | What it is |
|---|---|
| `provider.yaml` | A `KubernetesClusterProvider`: one cloud account the clusters are created in |
| `<cluster>/cluster.yaml` | A `KubernetesCluster`: the cluster, its region, node pools and `purpose` tag |
| `<cluster>/platform.yaml` | The `ClusterHelmRelease`s that install the platform charts on it. They must match [`helmfile.yaml`](../../helmfile.yaml), which installs the management cluster |
| `<cluster>/operator.yaml` | The `OramaOperator`: the operator version the cluster runs |
| `sets/lake-nonprod.yaml` | A `LakeSet` of nonprod environments (dev, testing, staging) on the nonprod cluster |
| `sets/lake-prod.yaml` | A `LakeSet` of production Lakes, one per region, with restore rehearsals |
| `status/status-page.yaml` | A public status page and the bucket it is served from |

The three clusters are `example-nonprod`, `example-production-eu` and `example-production-us`, managed
from `example-management`. Hosts are under `example.com`.

## The tests read these files

The tests that check a deployment's resources read this folder: the helmfile and every leaf install
the same platform, every leaf runs the same operator, no Secret is shared between nonprod and
production, the sets are admitted by their CRDs, the status page checks every region, and others.

To run them against a real deployment's files, point them at its folder with an absolute path:

```sh
ORAMA_CLUSTERS_DIR=/path/to/your/config/clusters go test ./cmd/ ./internal/controller/
```

The helper is `clustersDir()` in `cmd/clusters_dir_test.go` and
`internal/controller/clusters_dir_test.go`.

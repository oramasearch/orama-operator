# Operating the clusters

How to create a cluster, put its platform layer and operator on it, and run Lake and Orama Cloud v1
instances on it, by editing resources on the managing cluster. The design and the reasons are in
[`README.md`](README.md); every field is in [`../crd-reference.md`](../crd-reference.md). The
`orama-clusters` Claude skill in the umbrella bridge
(https://github.com/oramasearch/orama-bridge/blob/main/.claude/skills/orama-clusters/SKILL.md) is the
short form of this page.

## Where things are

| What | Where |
|---|---|
| The managing cluster | `kubectl` context `orama-production-cluster-01` |
| What is applied to it | [`config/clusters/`](../../config/clusters/README.md), one folder per managed cluster and one for the sets |
| A managed cluster's admin kubeconfig | Secret `orama-system/<cluster>-kubeconfig` on the managing cluster, key `kubeconfig` |
| The operator's image and chart | `oramasearch/orama-operator:<tag>` and `oramasearch/orama-operator-chart:<chart version>`, both private |

To use a managed cluster directly (read-only checks; changes go through the managing cluster):

```sh
kubectl --context orama-production-cluster-01 -n orama-system \
  get secret orama-nonprod-cluster-01-kubeconfig -o jsonpath='{.data.kubeconfig}' | base64 -d > /tmp/nonprod.kubeconfig
chmod 600 /tmp/nonprod.kubeconfig
kubectl --kubeconfig /tmp/nonprod.kubeconfig get nodes
```

## The kinds, in the order they are used

| Kind | Short | One sentence |
|---|---|---|
| `KubernetesClusterProvider` | `kcp` | A cloud account and the defaults clusters are created with. |
| `KubernetesCluster` | `kcluster` | A cluster: region, version, node pools, `purpose`. Creates an `AksCluster` (`aks`). |
| `ClusterHelmRelease` | `chr` | One pinned chart release on a managed cluster. |
| `OramaOperator` | `ooperator` | The managed cluster's own operator, at a version. |
| `LakeSet` | `lset` | Lake instances on managed clusters, from one template. |
| `OramaCloudV1Set` | `ocv1set` | Orama Cloud v1 instances on managed clusters, from one template. |

One command shows everything:

```sh
kubectl --context orama-production-cluster-01 get kcp,kcluster,aks,chr,ooperator,lset,ocv1set
```

Every one of them has a `Message` that says, in the words of whatever refused, why it is not
`Ready`. Read it before looking anywhere else.

## Create a cluster

1. **Choose the region.** Check it offers the node size in three zones and the Kubernetes version,
   and compare prices (Azure's retail price API; `Standard_D8as_v7` is $0.363 an hour in the US
   regions, $0.388 in Sweden Central and $0.439 in West Europe and Germany West Central). The
   subscription's quota is 65 cores per region, which is eight `Standard_D8as_v7` nodes; more is a
   request to Azure.
2. **Copy a cluster folder** in `config/clusters/` and edit `cluster.yaml`: the name
   (`orama-<purpose>-cluster-NN`), `region`, and the `purpose` tag (`nonprod` or `production`).
   **The `purpose` tag is required in practice**: a set refuses a cluster without one.
   Use `deletionPolicy: Retain` for production, so deleting the resource never deletes the cluster.
3. **Edit `platform.yaml` and `operator.yaml`**: every `clusterRef`, and each resource's name
   prefix. The `OramaOperator`'s `version` is the version the other clusters run.
4. **Apply the whole folder at once.** Nothing has to be sequenced by hand:

   ```sh
   kubectl --context orama-production-cluster-01 apply --dry-run=server -f config/clusters/<cluster>/
   kubectl --context orama-production-cluster-01 apply -f config/clusters/<cluster>/
   ```

   The releases and the operator stay `Pending` while Azure creates the cluster, then install by
   themselves: CloudNativePG first, and the operator only once the CRDs it needs exist.
5. **Watch.** Azure takes about 15 minutes; `kubectl get aks` shows its progress in the
   `Operation` column and its error in `Message`. Then:

   ```sh
   kubectl --context orama-production-cluster-01 wait kcluster/<cluster> --for=jsonpath='{.status.phase}'=Ready --timeout=30m
   kubectl --context orama-production-cluster-01 get chr,ooperator
   ```
6. **Check the cluster itself**: one node `Ready`, every pod `Running`, the operator's log with no
   error lines and all its controllers started.
7. Commit the folder and add the cluster to the table in `config/clusters/README.md`.

## Upgrade the operator

Every cluster runs the same operator version (rule 11 in [`CLAUDE.md`](../../CLAUDE.md)).

1. Merge the change into `dev`. Build, publish the chart and roll the managing cluster:
   `~/dev/oramasearch/codex-work/build/scripts/build-operator.sh` does all three from the
   operator checkout's current commit and prints the tag, `dev-<sha>`. It publishes the chart for the same tag, without which a managed cluster cannot
   install that version.
2. Set that version on every `OramaOperator`, in `config/clusters/<cluster>/operator.yaml`, and apply.
   Each managed cluster's operator is upgraded, CRDs first.
3. Confirm every cluster is on it:

   ```sh
   kubectl --context orama-production-cluster-01 get ooperator
   ```

## Add or upgrade a platform chart

Edit the `ClusterHelmRelease` in `<cluster>/platform.yaml` (`spec.chart.version`, `spec.values`) and
apply. Read the chart's release notes for CRD changes first. `dependsOn` orders installs
(cert-manager before K8ssandra); `crdURLs` names CRDs a chart ships in a separate file (TiDB).

## Run instances: sets

A set is one template and a list of members; a member is one instance on one cluster. Members are
independent of each other.

**Create an environment** (a new nonprod Lake, a dedicated production instance): add a member to
the set in `config/clusters/sets/`, then apply.

```yaml
  clusters:
    - clusterRef: orama-nonprod-cluster-01
      name: qa                    # the instance's name; its namespace is lake-qa
      spec:                       # only what differs from the template
        host: lake-qa.oramadev.net
        args: ["--listen", "0.0.0.0:4900", "--node", "qa"]
        env:
          - {name: LAKE_LOG_MODE, value: air-gapped}
          - {name: LAKE_PUBLIC_HOST, value: lake-qa.oramadev.net}
```

**Change a version or a setting for every member**: edit `spec.template` and apply. It reaches all
members at once, except a member whose own `spec` sets that field.

**Change one member only**, or pin it: put the field in that member's `spec`. A list (`env`, `args`)
in a member's `spec` replaces the template's list whole, so state all of it.

**Give one member its own credentials**: add `secrets` to the member, with `as` naming the Secret it
replaces:

```yaml
      secrets:
        - {name: customer-1-openai, namespace: lake, as: lake-prod-openai}
```

**Remove a member**: delete it from the list. With the default `deletionPolicy: Retain` the
instance keeps running and is no longer managed. Only `deletionPolicy: Delete` deletes it.

**What a set will refuse**, with the reason in its `Message`:

- members on clusters of different purposes, or a cluster with no `purpose` tag: a set is all
  nonprod or all production;
- two members on one cluster with the same name or namespace;
- a `LakeSet` whose template or member `spec` sets `namespace` (use the member's `namespace` field);
- an `OramaCloudV1Set` member whose `spec` sets a `region.name` other than its cluster's region;
- an instance of the same name on the cluster that the set did not create.

**Check a set**:

```sh
kubectl --context orama-production-cluster-01 get lset,ocv1set
kubectl --context orama-production-cluster-01 get lset <name> \
  -o jsonpath='{range .status.memberStatuses[*]}{.name} {.namespace} {.phase} {.version} | {.message}{"\n"}{end}'
```

## Things that have gone wrong, and what they looked like

| Symptom | Cause | What to do |
|---|---|---|
| A Lake member stays `Progressing` with "PersistentVolumeClaim … is not bound" | A Lake's store defaults to `ReadWriteMany`, which a disk-backed storage class cannot provide | State `store.accessModes: [ReadWriteOnce]` in the template. A claim's access mode cannot be changed, so the claim that never bound has to be deleted for the cluster's operator to recreate it; on production, ask first |
| `OramaOperator` is `Pending`: "the cluster lacks CRDs this operator version needs" | CloudNativePG is not on the cluster yet | Apply its `ClusterHelmRelease`; the operator installs by itself afterwards |
| `OramaOperator` is `Degraded`: the chart cannot be pulled | The chart for that version was not published | `go run ./hack/pushchart -version <tag>` from the commit the image was built from |
| A set member is `Pending`: "does not have the Lake kind" | The cluster's operator is not installed or not `Ready` | Fix the `OramaOperator` first |
| A cluster sits in `Creating` for many minutes with no error | Normal: a node takes about ten minutes to join | Read `kubectl get aks`'s `Operation` column; an error would be in `Message` |
| The API server refuses a CRD: a validation rule does not compile | gofmt rewrote two single quotes in a `// +kubebuilder` marker into a typographic quote | Write `size(x) > 0` instead of comparing with an empty string |

## Rules that are not negotiable

- **No differences between clusters' operators**: same chart, image, CRDs, controllers and RBAC
  everywhere ([`CLAUDE.md`](../../CLAUDE.md) rule 11).
- **A set never combines nonprod and production.**
- **Per-instance keys are never copied** between clusters; only the Secrets a set lists travel.
- **On a production cluster, ask before anything destructive**: deleting an instance, a claim, a
  namespace, a release or the cluster.
- **A hostname that production traffic already uses is never pointed at a new cluster** without an
  explicit decision. Instances created so far use names in `oramadev.net`.
- **Pull requests target `dev`.**

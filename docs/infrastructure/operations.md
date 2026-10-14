# Operating the clusters

How to create a cluster, put its platform layer and operator on it, and run Lake and Orama Cloud v1
instances on it, by editing resources on the managing cluster. The design and the reasons are in
[`README.md`](README.md); every field is in [`../crd-reference.md`](../crd-reference.md). The
`orama-clusters` Claude skill in the umbrella bridge
(https://github.com/oramasearch/orama-bridge/blob/main/.claude/skills/orama-clusters/SKILL.md) is the
short form of this page.

## The rule before any procedure: nothing is changed by hand

Steve, 2026-10-13: *"the operator is how we affect change (or changing the CRs for the lakeset or
v1 set)"*; *"that's state changes outside of our code, and therefore not reproducible"*.

Every procedure on this page changes a cluster in one of two ways: a change to the operator, or a
change to a resource file in `config/clusters/` that is then applied. **Nothing else is done to any
cluster without Steve's explicit consent for that action**: no `helm` against a cluster, no
`kubectl patch`, `edit`, `delete`, `scale` or `rollout restart`, no deleting an object so that it is
recreated, on nonprod or production ([`CLAUDE.md`](../../CLAUDE.md), *Never change a cluster by
hand*). Reading is always allowed. Where this page reaches a step that can only be done by hand
today, it says so, and that step is a question for Steve and a gap to close in the operator.

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

1. Merge the change into `dev`. **Ask Steve before rolling the managing cluster**: it is not managed
   by a resource yet, so its operator is rolled by a script that runs `helm upgrade` against it.
   With his consent, `~/dev/oramasearch/codex-work/build/scripts/build-operator.sh` builds the
   image, publishes the chart and rolls it from the operator checkout's current commit, and prints
   the tag, `dev-<sha>`. It publishes the chart for the same tag, without which a managed cluster cannot
   install that version.
2. Set that version on every `OramaOperator`, in `config/clusters/<cluster>/operator.yaml`, and apply.
   Each managed cluster's operator is upgraded, CRDs first.
   The management cluster (step 1) and the leaves (this step) must end on the same version: they
   are two installation paths for the same operator.
3. Confirm every cluster is on it:

   ```sh
   kubectl --context orama-production-cluster-01 get ooperator
   ```

## Add or upgrade a platform chart

**A platform chart is described in two places, and both change together**
([README, *Two installation paths, one result*](README.md#two-installation-paths-one-result)):

1. `helmfile.yaml`, which installs the **management** cluster;
2. the `ClusterHelmRelease` in **every** `config/clusters/*/platform.yaml`, which the management
   cluster's operator installs on each **leaf**.

Make the same change in both (`version`, values, a new or removed chart), in one pull request.
`go test ./cmd/ -run Helmfile` fails while they differ. Then:

- apply each `platform.yaml` to the managing cluster: the operator upgrades the leaves;
- apply the helmfile to the management cluster, **with Steve's consent**
  (`helmfile -l name=<release> sync`), and check `helm list -A` there against the file.

Edit the `ClusterHelmRelease` (`spec.chart.version`, `spec.values`) and
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
- an `OramaCloudV1Set` template or member that states `remoteReaders` or `peers` (the set writes
  both), and an `OramaCloudV1Set` with read members and not exactly one primary;
- an instance of the same name on the cluster that the set did not create.

**Check a set**:

```sh
kubectl --context orama-production-cluster-01 get lset,ocv1set
kubectl --context orama-production-cluster-01 get lset <name> \
  -o jsonpath='{range .status.memberStatuses[*]}{.name} {.namespace} {.phase} {.version} | {.message}{"\n"}{end}'
```

## Rehearse a cross-cluster read member

A `read` member serves its region from a primary in another cluster
([README](README.md#read-members-follow-a-primary-in-another-cluster), DESIGN.md decision 69). It
has not run anywhere yet, so it is rehearsed on nonprod first: a second `v1-nonprod` member in
another region, as `read`, which must pass the read checks below before anything changes in
`v1-prod`. Every step is a change to a file in `config/clusters/` applied to the managing cluster,
or a read.

**Before you start**

- Every cluster runs an operator version that has the read path (#396): the managing cluster's
  roll needs Steve's consent ([Upgrade the operator](#upgrade-the-operator)).
- The provider's Azure identity can read public IP addresses in each cluster's node resource group
  (`MC_...`). On a Ready cluster, `kubectl get aks <cluster> -o jsonpath='{.status.egressIPs}'` lists
  its egress addresses and the `EgressIPsKnown` condition is `True`; `ReadFailed` there means the
  identity lacks that read.
- The set's engine runs side-only images: `images.overrides` sets `oramacore-writer` and
  `oramacore-reader`. A read member refuses the combined image, which starts a writer in every reader.

**1. Create the second nonprod cluster.** Follow [Create a cluster](#create-a-cluster) for
`orama-nonprod-cluster-02` in a region other than `orama-nonprod-cluster-01`'s `westus2`, with the
`nonprod` purpose tag. Wait until it is `Ready` and its `status.egressIPs` is filled:

```sh
kubectl --context orama-production-cluster-01 get kcluster orama-nonprod-cluster-02 \
  -o jsonpath='{.status.phase} {.status.egressIPs}{"\n"}'
```

**2. Add the read member to `config/clusters/sets/v1-nonprod.yaml`**, then apply and commit:

```yaml
  - clusterRef: orama-nonprod-cluster-02
    name: v1-dev-read
    namespace: v1-dev-read
    spec:
      region: {role: read}
      components:
        controlDB: {replicate: true}
        broker: {placement: central}     # the readers use the primary's broker
        ingestion: {enabled: false}      # the write path stays on the primary
        dashboard: {enabled: false}
        docs: {enabled: false}
        analyticsProcessor: {enabled: false}
        searchWorkers: {enabled: false}
        pgAdmin: {enabled: false}
```

The set then opens `v1-dev` to the new cluster's egress IPs (`v1-dev` restarts its broker once, to
add the TLS listener), waits for `v1-dev` to publish, and applies `v1-dev-read` with its peer and a
copy of `v1-dev-remote-read`.

**3. Watch it connect.** On the managing cluster:

```sh
kubectl --context orama-production-cluster-01 get ocv1set v1-nonprod \
  -o jsonpath='{range .status.memberStatuses[*]}{.name} {.phase} | {.message}{"\n"}{end}'
```

`v1-dev-read` goes `Pending` ("waiting for primary v1-dev ... to publish its replication stream"),
then `Progressing`, then `Ready`. On `orama-nonprod-cluster-01` (a kubeconfig as in
[Where things are](#where-things-are)):

```sh
kubectl -n v1-dev get ocv1 v1-dev -o jsonpath='{.status.remoteRead}{"\n"}'
kubectl -n v1-dev get svc v1-dev-broker-remote v1-dev-controldb-remote \
  -o custom-columns=NAME:.metadata.name,ADDRESS:.status.loadBalancer.ingress[0].ip,ALLOWED:.spec.loadBalancerSourceRanges
kubectl -n v1-dev get ocv1 v1-dev -o jsonpath='{.status.conditions[?(@.type=="RemoteReadPublished")].message}{"\n"}'
```

`ALLOWED` must be exactly the new cluster's egress IPs as `/32`. On `orama-nonprod-cluster-02`:

```sh
kubectl -n v1-dev-read get ocv1 v1-dev-read -o jsonpath='{.status.phase} {.status.conditions[?(@.type=="PeerReady")].message}{"\n"}'
kubectl -n v1-dev-read logs v1-dev-read-reader-0 -c oramacore | grep -i 'rabbitmq consumer'
kubectl -n v1-dev-read get clusters.postgresql.cnpg.io v1-dev-read-controldb
```

The reader logs `Created rabbitmq consumer oramacore-operations`. The broker on
`orama-nonprod-cluster-01` lists it as a consumer from the new cluster's address:

```sh
kubectl -n v1-dev exec v1-dev-broker-0 -- rabbitmq-streams list_stream_connections user peer_host
kubectl -n v1-dev exec v1-dev-broker-0 -- rabbitmq-streams list_stream_consumers stream offset_lag
```

Replica lag, on the primary and on the replica (read-only queries; the instance is `-1` unless CNPG
has failed over, which `kubectl get clusters.postgresql.cnpg.io` shows):

```sh
kubectl -n v1-dev exec v1-dev-controldb-1 -c postgres -- psql -Atc \
  "select application_name, client_addr, state, replay_lag from pg_stat_replication"
kubectl -n v1-dev-read exec v1-dev-read-controldb-1 -c postgres -- psql -Atc \
  "select pg_is_in_recovery(), now() - pg_last_xact_replay_timestamp()"
```

**4. Run the read checks.** All must hold before `v1-prod` changes:

- **Follows writes.** Write a document to a throwaway index through `v1-dev`'s writer; it is found
  by a search on `v1-dev-read`'s readers within seconds, with the same key `v1-dev` accepts.
  Delete it on `v1-dev`; it disappears from `v1-dev-read`.
- **Serves what was there before.** A search on an existing index returns the same hits on both.
- **Survives restarts.** When a reader or `v1-dev`'s broker restarts (an operator rollout, a node
  upgrade), the reader resumes from its stored offset (no replay from offset 0 in its log) and
  catches up. Restarting a pod by hand needs Steve's consent, like any other change.
- **Control DB matches.** A read-only count of `oramacore_clusters` and `projects` on the replica
  equals the primary's, and `pg_is_in_recovery()` is true.
- **Closed to everyone else.** From an address not in `ALLOWED` (a laptop), `nc -vz -w 5 <address> 5551`
  and `5432` time out. `5552` is not exposed at all.
- **TLS is verified.** From a pod in `orama-nonprod-cluster-02`,
  `openssl s_client -connect <address>:5551 -CAfile <broker-ca.crt from v1-dev-remote-read>` ends with
  `Verify return code: 0 (ok)`; with any other CA file it fails.

**5. Only then, and only with Steve's consent, `v1-prod`.** Switching `v1-us` to `read` puts the
production broker stream and control Postgres on public addresses (TLS, password or client
certificate, source-restricted): Steve confirms it first. An existing primary cannot be switched in
place: its control DB was created as a primary, and the operator refuses to turn it into a replica.
So the change to `config/clusters/sets/v1-prod.yaml` is a new member on
`orama-production-cluster-03` with the spec from step 2 (its own name and namespace), next to the
existing `v1-us`, applied with Steve's consent. Once it passes the same checks against `v1-eu`, US
traffic moves to it in Cloudflare, and what happens to the old `v1-us` primary and its data is a
separate decision with Steve.

## Things that have gone wrong, and what they looked like

| Symptom | Cause | What to do |
|---|---|---|
| A Lake member stays `Progressing` with "PersistentVolumeClaim … is not bound" | A Lake's store defaults to `ReadWriteMany`, which a disk-backed storage class cannot provide | State `store.accessModes: [ReadWriteOnce]` in the template. A claim's access mode cannot be changed, so the claim that never bound has to be replaced, and the operator does not do that yet: **ask Steve before deleting it**, on any cluster |
| `OramaOperator` is `Pending`: "the cluster lacks CRDs this operator version needs" | CloudNativePG is not on the cluster yet | Apply its `ClusterHelmRelease`; the operator installs by itself afterwards |
| `OramaOperator` is `Degraded`: the chart cannot be pulled | The chart for that version was not published | `go run ./hack/pushchart -version <tag>` from the commit the image was built from |
| A set member is `Pending`: "does not have the Lake kind" | The cluster's operator is not installed or not `Ready` | Fix the `OramaOperator` first |
| A `ClusterHelmRelease` is `Degraded` with "pre-upgrade hooks failed … timed out", and its Helm revision climbs | A chart's hook (the ClickHouse operator's CRD-install Job) outlasted Helm's timeout while the new cluster was still pulling images. Fixed 2026-10-12: hooks get ten minutes, where they had 30 seconds | Nothing, on a current operator. It recovers once the hook's image is on the node; check `helm history` on the cluster shows a final `deployed` |
| A Kafka service stays `Provisioning`; its message ends "is forbidden: violates PodSecurity" | Strimzi on that cluster is not running in restricted mode, so the workspace namespace refuses the broker pod | The cluster's Strimzi `ClusterHelmRelease` must set `extraEnvs: STRIMZI_POD_SECURITY_PROVIDER_CLASS=restricted`; the Kafka starts on Strimzi's next pass |
| A cluster sits in `Creating` for many minutes with no error | Normal: a node takes about ten minutes to join | Read `kubectl get aks`'s `Operation` column; an error would be in `Message` |
| The API server refuses a CRD: a validation rule does not compile | gofmt rewrote two single quotes in a `// +kubebuilder` marker into a typographic quote | Write `size(x) > 0` instead of comparing with an empty string |

## Rules that are not negotiable

- **No differences between clusters' operators**: same chart, image, CRDs, controllers and RBAC
  everywhere ([`CLAUDE.md`](../../CLAUDE.md) rule 11).
- **A set never combines nonprod and production.**
- **Per-instance keys are never copied** between clusters; only the Secrets a set lists travel.
- **Nothing is changed by hand on any cluster without Steve's explicit consent** (the section at
  the top of this page). Change the operator, or change a resource file and apply it.
- **A hostname that production traffic already uses is never pointed at a new cluster** without an
  explicit decision. Instances created so far use names in `oramadev.net`.
- **Pull requests target `dev`.**

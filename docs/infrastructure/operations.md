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
| `LakeOrderSource` | `lorders` | Provisions the paid lake.live orders a `LakeSet`'s Lakes record ([orders](orders.md)). |

One command shows everything:

```sh
kubectl --context orama-production-cluster-01 get kcp,kcluster,aks,chr,ooperator,lset,ocv1set,lorders
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
   Each managed cluster's operator is upgraded with its CRDs widened first and applied exactly once
   the new version runs alone (both paths do this the same way:
   [`../operations/crd-rollout.md`](../operations/crd-rollout.md#removing-or-changing-a-field-crds-in-two-steps-around-the-roll)).
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
instance keeps running and is no longer managed. Only `deletionPolicy: Delete` deletes it. A
member's migrations go with it, and are deleted first.

**Move a v1 member's volumes to another class, or shrink them**, when their data may be lost (a member
created before its storage settings were, [#915](https://github.com/oramasearch/orama-operator/issues/915)):
declare the storage you want and approve the deletion on each volume, in the member's `spec`, then apply.
The leaf's operator must already have the field.

```yaml
      spec:
        components:
          engine:
            writer:
              storage:
                size: 512Gi
                storageClassName: managed-csi-premium
                approveDataDeletion: true
```

Watch `status.conditions[StorageApplied]` on the member: `ReplacingStorage` while the old volumes go, then
`Applied` once the new ones match. Then set `approveDataDeletion` back to `false` (or remove it) in a later
commit, so a future change to that volume's storage cannot erase its data.

**Copy a Prod 1 team into a v1 member**: add an entry to the set's `spec.migrations`, and change its
`step` one commit at a time; see
[`config/clusters/README.md`](../../config/clusters/README.md#copying-a-prod-1-team-into-a-member).

**What a set will refuse**, with the reason in its `Message`:

- members on clusters of different purposes, or a cluster with no `purpose` tag: a set is all
  nonprod or all production;
- two members on one cluster with the same name or namespace;
- a `LakeSet` whose template or member `spec` sets `namespace` (use the member's `namespace` field);
- an `OramaCloudV1Set` member whose `spec` sets a `region.name` other than its cluster's region;
- a migration that names no member, or a name two members share (the API server refuses it), whose
  `spec.targetRef` is not its member, that would move to another member, or whose Secret the stack
  already gets or another migration copies from another source (on the migration, as `Blocked`);
- an instance of the same name on the cluster that the set did not create.

**Check a set**:

```sh
kubectl --context orama-production-cluster-01 get lset,ocv1set
kubectl --context orama-production-cluster-01 get lset <name> \
  -o jsonpath='{range .status.memberStatuses[*]}{.name} {.namespace} {.phase} {.version} | {.message}{"\n"}{end}'
```

## Preparing the production v1 dashboard

[#885](https://github.com/oramasearch/orama-operator/issues/885) keeps the live sandbox-shaped
`v1-prod` set unchanged until the production file is ready. Its members remain `Ready=False` at
the production dashboard authentication gate and their existing dashboards keep serving. A source
Secret update alone does not fix that: the live set does not copy the production dashboard or LLM
Secrets. The prepared [`v1-prod.yaml`](../../config/clusters/sets/v1-prod.yaml) gives only `v1-eu`
the restricted Stripe reference and disables `v1-us`'s dashboard. Applying it needs Steve's approval
after every blocker in its header is resolved.

Source checkpoint, 2026-10-23: the restricted Stripe source exists and matches protected staging;
cron/webhook authentication, Google, dedicated Groq and a dedicated Prod 2 `PRIVATE_API_KEY` are
prepared. The 64-character hex private key was generated locally, stored in the migration registry
and source Secret, and read back equal to staging; every other source value was preserved.
Juan explicitly deferred
Anthropic and chose to retain its existing three-byte non-working value. Preserve it unchanged:
the original all-four-real-provider-keys acceptance is unmet, and the rejected Prod 1 candidate
must not replace it. This exception does not approve the production set apply.

Other dashboard inputs still require preparation: the source retains placeholders for
Resend, Slack, `S3_BUCKET_PREFIX` and `DATA_SOURCE_REQUEST_URL`, and has no Clerk
keys. Validate scoped source candidates and record their ownership before updating the source.
Check the rendered references before treating an old `envFrom` value as a missing input: the
operator supplies the Redis HTTP URL/token and dashboard origin, and reconciles an uploads R2
bucket with its own endpoint, bucket, region and access keys. Verify that bucket's readiness;
copying Prod 1 AWS credentials does not establish it. Image qualification, Clerk cutover and
the header's other prerequisites remain independent gates.

`PRIVATE_API_KEY` authenticates the dashboard's machine routes, including import metadata/status
and file-upload URL requests. The target import consumer reads the dashboard's same source key
and calls its in-cluster Service. Its prepared key is dedicated to Prod 2; the current live sandbox
set does not consume that source. Resend invitations send real
email and Slack signup/payment notifications post to their configured channels. Suspended dashboard
crons and analytics import do not suspend those request-driven effects, so review notification
isolation before installing carried credentials. Source inspection alone does not qualify delivery.

Prepare the inputs in this order, using values from secure files rather than command arguments:

1. Create `orama-system/v1-prod-dashboard-stripe` on the managing cluster with key
   `STRIPE_SECRET_KEY`, a live restricted key with exactly the permissions in
   [the dashboard's Stripe key](../components.md#the-dashboards-stripe-key-a-restricted-key-of-its-own-802).
   Check the permission metadata in Stripe; the operator checks only the `rk_live_` prefix.
2. Preserve the prepared OpenAI, Google and Groq values in `orama-system/v1-prod-llm-keys`, and
   retain Anthropic unchanged under Juan's deferred exception. Check lengths only in output;
   the original requirement that all four exceed 20 bytes remains unmet. Record
   any carried Prod 1 credential as a shared dependency before Prod 1 retirement revokes it.
3. With the later production apply, remove `STRIPE_SECRET_KEY` from `v1-prod-dashboard-env`.
   Leave it in place until then; the restricted-key reference refuses a second key in `envFrom`.
   Verify that `CRON_SECRET` and `STRIPE_WEBHOOK_SECRET` contain real values too: the latter must
   match the existing `app.orama.com` Stripe endpoint. Merely finding these key names is insufficient.
4. Apply the merged production set file, with Steve's consent, only after its remaining blockers
   are resolved. Keep EU dashboard crons and the analytics import suspended through the migration.

After applying, read the set and both members' conditions and confirm none reports a dashboard
error. On EU, inspect the dashboard Deployment's references: `envFrom` names
`v1-prod-dashboard-env` (with real `CRON_SECRET` and `STRIPE_WEBHOOK_SECRET`) and the explicit
`STRIPE_SECRET_KEY` reference names `v1-prod-dashboard-stripe`. On US, confirm the dashboard
Deployment, Service and scheduled jobs are absent and downstream components still reconcile.
Check Secret lengths or equality within the checking process, never by printing their contents.
Record each credential change and the eventual apply in the production change and compliance records,
with approver, timestamp and sanitized evidence.

## Paid orders: `LakeOrderSource`

A private warehouse or a dedicated cluster bought on lake.live is provisioned by the management
cluster's operator from lake's `cloud_orders` table, with nothing done by hand
([operator#634](https://github.com/oramasearch/orama-operator/issues/634); the design is
[`orders.md`](orders.md)). One `LakeOrderSource` names the set whose Lakes take orders; the operator
reads each member's orders through the member's cluster, claims each `paid` one, makes the resources
for it on the management cluster and writes the order's state back (`provisioning`, then `ready`
with the host, or `failed` with the reason).

**Turn it on for an environment**: commit a `LakeOrderSource` (the example is in
[`orders.md`](orders.md#the-kind)) and apply it to the management cluster, with Steve's consent, after
lake's `cloud_orders` migration (lake#831) is on that set's Lakes. Nonprod first: a source for
`lake-nonprod`, a test-mode Stripe purchase, and the order reaching `ready`, before one for `lake-prod`.

**Ordered resources are not in `config/clusters/`.** Their source of truth is the order's row, so they
are made by the operator, not written in a file, and `kubectl diff -R -f config/clusters/` does not
show them. Find them by their label:

```sh
kubectl --context orama-production-cluster-01 get lorders
kubectl --context orama-production-cluster-01 get lset,kcluster,chr,ooperator -l orama.cloud/order-id
kubectl --context orama-production-cluster-01 get lorders lake-live \
  -o jsonpath='{range .status.orders[*]}{.id} {.offer} {.region} {.lakeSet} | {.message}{"\n"}{end}'
```

**Never edit or delete an ordered resource by hand.** The operator brings it back to the described
spec on its next pass, and removing one is a change of the order's state, which is not built yet
([`orders.md`](orders.md#open-questions-for-steve)). A dedicated cluster copies the template cluster's
platform and operator, so it is upgraded by upgrading the template cluster's files, like every leaf.

**What a source reports**: `Pending` while the set or lake's table is missing, `Degraded` when a
member's orders cannot be read (the member and the reason are in `status.members`) or `spec.member`
sets a field the operator sets per order, `Progressing` while orders are provisioning.

## Move a cluster's hosts from ingress-nginx to zetta-tunnel

Change the cluster's set members' ingress class in `config/clusters/sets/`, after cert-manager has
issued the hosts' certificates; the steps and checks are in [Ingress](ingress.md#cutover-per-cluster).
Removing ingress-nginx is a platform change made in `helmfile.yaml` and every `platform.yaml` together,
only after every cluster is cut over.

## Things that have gone wrong, and what they looked like

| Symptom | Cause | What to do |
|---|---|---|
| A Lake member stays `Progressing` with "PersistentVolumeClaim … is not bound" | A Lake's store defaults to `ReadWriteMany`, which a disk-backed storage class cannot provide | State `store.accessModes: [ReadWriteOnce]` in the template. A claim's access mode cannot be changed, so the claim that never bound has to be replaced, and the operator does not do that yet: **ask Steve before deleting it**, on any cluster |
| `OramaOperator` is `Pending`: "the cluster lacks CRDs this operator version needs" | CloudNativePG is not on the cluster yet | Apply its `ClusterHelmRelease`; the operator installs by itself afterwards |
| `OramaOperator` is `Degraded`, reason `ProductionCredential`: "… is a production credential and is never copied to a nonprod cluster" | Its `imagePullSecretRef` names a Secret production uses (a production set lists it, a production `OramaOperator` names it, or it is labelled `orama.cloud/purpose` for another purpose); nothing was copied to the cluster (orama-operator#832) | Name the cluster's own Secret in `config/clusters/<cluster>/operator.yaml`, copied under the old name with `imagePullSecretAs`, and apply the file. Creating that Secret on the managing cluster is Steve's |
| `OramaOperator` is `Degraded`: the chart cannot be pulled | The chart for that version was not published | `go run ./hack/pushchart -version <tag>` from the commit the image was built from |
| A set member is `Pending`: "does not have the Lake kind" | The cluster's operator is not installed or not `Ready` | Fix the `OramaOperator` first |
| A `ClusterHelmRelease` is `Degraded` with "pre-upgrade hooks failed … timed out", and its Helm revision climbs | A chart's hook (the ClickHouse operator's CRD-install Job) outlasted Helm's timeout while the new cluster was still pulling images. Fixed 2026-10-12: hooks get ten minutes, where they had 30 seconds | Nothing, on a current operator. It recovers once the hook's image is on the node; check `helm history` on the cluster shows a final `deployed` |
| A Kafka service stays `Provisioning`; its message ends "is forbidden: violates PodSecurity" | Strimzi on that cluster is not running in restricted mode, so the workspace namespace refuses the broker pod | The cluster's Strimzi `ClusterHelmRelease` must set `extraEnvs: STRIMZI_POD_SECURITY_PROVIDER_CLASS=restricted`; the Kafka starts on Strimzi's next pass |
| A cluster sits in `Creating` for many minutes with no error | Normal: a node takes about ten minutes to join | Read `kubectl get aks`'s `Operation` column; an error would be in `Message` |
| The operator is `OOMKilled` seconds after starting, then runs well under its limit | Before operator#677: its cache held every Helm release Secret, and the Go runtime did not know the container's limit while start-up Helm reads filled the heap | Nothing, on an operator with operator#677: it sets its Go limit from the container's and leaves Helm release Secrets out of its cache. Never raise the limit for one cluster; see [`../operations/memory.md`](../operations/memory.md) |
| The API server refuses a CRD: a validation rule does not compile | gofmt rewrote two single quotes in a `// +kubebuilder` marker into a typographic quote | Write `size(x) > 0` instead of comparing with an empty string |

## Rules that are not negotiable

- **No differences between clusters' operators**: same chart, image, CRDs, controllers and RBAC
  everywhere ([`CLAUDE.md`](../../CLAUDE.md) rule 11).
- **A set never combines nonprod and production.**
- **Nonprod never gets a production credential.** A Secret a production set lists, one labelled
  `orama.cloud/purpose: production`, or a migration's Prod 1 source credential is never copied into a
  nonprod member; the operator refuses it
  ([the rule](../crd-reference.md#production-credentials-never-reach-a-nonprod-member)).
- **Per-instance keys are never copied** between clusters; only the Secrets a set lists travel.
- **Nothing is changed by hand on any cluster without Steve's explicit consent** (the section at
  the top of this page). Change the operator, or change a resource file and apply it.
- **A hostname that production traffic already uses is never pointed at a new cluster** without an
  explicit decision. Instances created so far use names in `oramadev.net`.
- **Pull requests target `dev`.**

## Configure node-pool sysctls

Put kernel settings in `KubernetesCluster.spec.nodePools[].sysctls` before creating a
cluster. Follow [node sysctls](node-sysctls.md) for supported names, evidence and rollout gates.
For an existing cluster, a changed sysctl is reported by `InSync=False`; it is not applied,
because AKS sets it only when a pool is created ([node-pool replacement](node-pool-replacement.md)).
Adding a pool to an existing resource is also drift-only in this controller version.
A supported update or a recoverable replacement/drain lifecycle needs a separate operator
design before existing pools can be rolled out. No cluster rollout is qualified by unit tests.

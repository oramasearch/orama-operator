# The clusters, as applied

The resources that describe every managed cluster and what runs on it, exactly as they are applied
to the managing cluster (`kubectl` context `orama-production-cluster-01`). **These files are the
record of what is deployed.** Change a file here, apply it, and commit both in the same change; a
`kubectl edit` or `kubectl patch` that is not followed by an update here leaves the record wrong.

**These files and the operator are the only way a cluster is changed.** Nothing is done to a cluster
by hand without Steve's explicit consent ([`CLAUDE.md`](../../CLAUDE.md), *Never change a cluster by
hand*): a change made by hand is state outside the code, and is not reproducible.

How to work with them: [`docs/infrastructure/operations.md`](../../docs/infrastructure/operations.md).
Why they are shaped this way: [`docs/infrastructure/README.md`](../../docs/infrastructure/README.md).

**These files are one of two installation paths, and they mirror the other.** The leaf clusters
here are installed by the management cluster's operator from these files. The management cluster
itself is installed from [`helmfile.yaml`](../../helmfile.yaml). Both must install the same charts,
versions, namespaces and values: **a change to a `platform.yaml` is not finished until
`helmfile.yaml` and every other cluster's `platform.yaml` have it too**
([`docs/infrastructure/README.md`](../../docs/infrastructure/README.md#two-installation-paths-one-result);
`cmd/platform_sync_test.go` fails when they differ).

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
| `orama-production-cluster-03` | `eastus` | `production` | `Retain` |

The managing cluster, `orama-production-cluster-01`, was created by hand before these kinds
existed and has no `KubernetesCluster` of its own.

`sets/v1-prod.yaml` states the source of each component storage size next to the setting. The
operator only grows volumes, so a size raised in a set file stays raised there.

No file here holds a secret. Secrets are named by reference and live on the managing cluster.
A set's `secrets` list copies them into each member's namespace before the instance is applied;
the copy keeps its name, or takes the one `as` gives. A v1 member's engine master key arrives this
way and is named by `template.masterAPIKeySecretRef` (or a member's `spec`); the operator never
generates it, and without it the member's writer and readers stay at zero replicas. Changing a
copied Secret's source, or its data on the managing cluster, rolls the member's consumers of it.

An `OramaCloudV1Set` copies `cloudflare-api-token` to each member's namespace the same way;
besides DNS, it is the DNS-01 credential of every stack's Let's Encrypt `Issuer`,
so a v1 member with a host fails to reconcile without it
([origin TLS](../../docs/components.md#origin-tls-ingress_tlsgo-457)). Every host in a v1 set,
`extraHosts` included, must be a name in a Cloudflare zone that token can edit, or its certificate is
never issued. An extra host gets no DNS record from the operator
([extra hosts](../../docs/crd-reference.md#extra-hosts-extrahosts)).
The DNS records the operator makes for a member's dashboard, writer and reader hosts all go into
the zone of the Secret's `zoneID`, so every host a member publishes must be in that one zone
([public hosts](../../docs/crd-reference.md#public-writer-and-reader-hosts-enginewriterhost-enginereaderhost)).

A set's `spec.secrets` list copies each named Secret from there into every member's namespace (for
example the dashboard's environment Secret, which the set's template references in
`components.dashboard.envFrom`); see
[the dashboard's environment](../../docs/components.md#the-dashboards-environment). The e2e canary's
Secret (`components.dashboard.crons.e2eCanary.secretRef`) arrives the same way. Creating or
changing a source Secret is a write on the managing cluster and needs Steve's consent.

The dashboard crons and the e2e canary are created suspended. Their `suspend` fields change only in
a set file, committed and applied to the managing cluster; production unsuspends them in the cutover
step that stops Prod 1's crons (see
[the dashboard's scheduled jobs](../../docs/components.md#the-dashboards-scheduled-jobs)). Never
edit, patch or create the CronJobs themselves.

A v1 member imports its readers' search events into ClickHouse only with
`components.analyticsProcessor.import.enabled: true`, which needs `engine`, `analytics` and
`controlDB` enabled. `import.suspend: true` keeps the schedule in place but starts no import, so
turning it on is a one-line change to the set file, applied from that file; the readers keep the
events logged meanwhile, and the first import takes them
([`docs/components.md`](../../docs/components.md#analytics-import)).

[`sets/v1-prod.yaml`](sets/v1-prod.yaml) is the file production runs from. Its template holds only
what `v1-eu` and `v1-us` share, because a member's `spec` is merged over the template and cannot
remove a field from it; each member carries the rest. `v1-eu` has the production values and `v1-us`
keeps its sandbox values. The file's header classifies every key `v1-eu` needs as generated by the
operator, derived, or copied, and lists what still blocks the cutover. `v1-eu`'s copied Secrets are in
the member's own `secrets` list: a member's list adds Secrets for that member only, and replaces a
set entry with the same name on the member's cluster. Nothing is created directly in a member's
namespace.

To check that the files match the cluster:

```sh
kubectl --context orama-production-cluster-01 diff -f config/clusters/ -R
```

Empty output means they match.

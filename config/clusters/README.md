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
never issued. So each member names its own `oramadev.net` dashboard and docs hosts (`app-dev`/`docs-dev`,
`app-eu`/`docs-eu`, `app-us`/`docs-us`); no enabled component in a v1 set has a `.local` host. An extra
host gets no DNS record from the operator
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
a set file, committed and applied to the managing cluster; production unsuspends them at cutover
step 5, after Prod 1's crons are off and `app.orama.com` points at Prod 2 (see
[the dashboard's scheduled jobs](../../docs/components.md#the-dashboards-scheduled-jobs)). Never
edit, patch or create the CronJobs themselves.

An Orama Cloud v1 stack's `controlDB.stripePrices` holds that stack's Stripe price IDs for the
reference rows the operator seeds into its control database
([`docs/components.md`](../../docs/components.md#the-control-dbs-schema)). `v1-nonprod` leaves it
empty: no test-mode prices exist yet, so its plans carry no Stripe prices and it has no add-on
prices. `v1-prod` needs its live-mode IDs before billing works on it.

To check that the files match the cluster:

```sh
kubectl --context orama-production-cluster-01 diff -f config/clusters/ -R
```

Empty output means they match.

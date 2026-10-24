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
| `<cluster>/operator.yaml` | The `OramaOperator`: the operator version the cluster runs, and the pull Secret copied to it. A nonprod cluster names its own (`orama-system/oramasearch-dockerhub-nonprod`, copied as `oramasearch-dockerhub`); a Secret production uses is refused for it (orama-operator#832). |
| `sets/lake-<purpose>.yaml` | A `LakeSet`: the Lake instances of that purpose, and the end-to-end runs against them (`spec.e2eTests`). |
| `sets/v1-<purpose>.yaml` | An `OramaCloudV1Set`: the Orama Cloud v1 instances of that purpose, and the Prod 1 teams being copied into them. |

| Cluster | Region | Purpose | Deletion policy |
|---|---|---|---|
| `orama-nonprod-cluster-01` | `westus2` | `nonprod` | `Delete` |
| `orama-production-cluster-02` | `swedencentral` | `production` | `Retain` |
| `orama-production-cluster-03` | `eastus` | `production` | `Retain` |

**Paid lake.live orders are the exception, on purpose.** A private warehouse or dedicated cluster a
customer buys is made by the operator from lake's `cloud_orders` row (operator#634): its `LakeSet`,
and for a dedicated cluster its `KubernetesCluster`, `ClusterHelmRelease`s and `OramaOperator`, are
not in these files, and carry the label `orama.cloud/order-id`. The order's row is their source of
truth; the `LakeOrderSource` that turns orders into them is a file here once it is applied. See
[`docs/infrastructure/orders.md`](../../docs/infrastructure/orders.md).

The managing cluster, `orama-production-cluster-01`, was created by hand before these kinds
existed and has no `KubernetesCluster` of its own.

`sets/v1-prod.yaml` states the source of each component storage size next to the setting. The
operator only grows volumes, so a size raised in a set file stays raised there.

No file here holds a secret. Secrets are named by reference and live on the managing cluster.
A set's `secrets` list copies them into each member's namespace before the instance is applied;
the copy keeps its name, or takes the one `as` gives. A v1 member that must carry an existing
engine master key (production carries Prod 1's) receives it this way and names it in
`template.masterAPIKeySecretRef` (or a member's `spec`). A member that names none gets a generated
`<cr>-master-api-key` (#615), which is right for a fresh stack such as `v1-dev`. Changing a
copied Secret's source, or its data on the managing cluster, rolls the member's consumers of it.

**Nonprod and production never share a Secret** (orama-operator#787, #789). A nonprod set lists its
own Secrets, copied under the names its stack mounts with `as`; the operator refuses to copy into a
nonprod member a Secret any production set lists, one labelled `orama.cloud/purpose: production`, or
a migration's Prod 1 source credential. The same holds for each `<cluster>/operator.yaml` pull Secret
(orama-operator#832): a nonprod cluster names its own, and the operator refuses to copy to it a
Secret production uses. `TestCommittedSetsAndOperatorsNeverShareASecretAcrossPurposes` fails on a set
or operator file that shares one
([the rule](../../docs/crd-reference.md#production-credentials-never-reach-a-nonprod-member)).

An `OramaCloudV1Set` copies a Cloudflare token to each member's namespace as `cloudflare-api-token`
the same way (`v1-prod` the production token, `v1-nonprod` its own `cloudflare-api-token-oramadev`);
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

`lake-nonprod` uses `billing.enabled: true` and `payments.mode: test`, with each member copying
its own sandbox Secret as `lake-prod-stripe`: `lake/stripe-dev`, `lake/stripe-testing` and
`lake/stripe-staging`. Each source has exactly `accountID`, `operatorKey` and `appKey`; both keys
must belong to that member's sandbox. Keeping the existing target name replaces the retained
production-source data in place when the set is applied. The operator creates a separate
`https://lake-<member>.oramadev.net/billing/stripe/webhook` endpoint and keeps its signing Secret
in that member's namespace. Stripe generates a unique signing secret per endpoint, so one shared
legacy `webhook_secret` cannot verify deliveries to all three hosts
([operator#884](https://github.com/oramasearch/orama-operator/issues/884),
[Stripe signature verification](https://docs.stripe.com/webhooks#verify-webhook-signatures-with-official-libraries)).
The common sources remain `lake/oramasearch-dockerhub-nonprod` (a nonprod-only, read-only pull
token) and `lake/lake-nonprod-openai`. On 2026-10-23 Juan explicitly chose to reuse the existing
Prod1 OpenAI key in that separate source ([#884 decision](https://github.com/oramasearch/orama-operator/issues/884#issuecomment-6374737150));
Azure was cancelled. This is shared credential ownership, not key isolation. Preserve this
consumer before Prod1 key revocation, and review its usage limits together with the shared budget.
Create and verify every source credential before applying the set file; the references alone do
not establish credential isolation. See [Lake payments](../../docs/lake/payments.md) for key
permissions, generated signing Secrets and the `PaymentsReady` condition.

The dashboard crons and the e2e canary are created suspended. Their `suspend` fields change only in
a set file, committed and applied to the managing cluster; production unsuspends them at cutover
step 5, after Prod 1's crons are off and `app.orama.com` points at Prod 2 (see
[the dashboard's scheduled jobs](../../docs/components.md#the-dashboards-scheduled-jobs)). Never
edit, patch or create the CronJobs themselves.

A v1 member imports its readers' search events into ClickHouse only with
`components.analyticsProcessor.import.enabled: true`, which needs `engine`, `analytics` and
`controlDB` enabled. `import.suspend: true` keeps the schedule in place but starts no import, so
turning it on is a one-line change to the set file, applied from that file; the readers keep the
events logged meanwhile, and the first import takes them
([`docs/components.md`](../../docs/components.md#analytics-import)).
`sets/v1-nonprod.yaml` sets `v1-dev` to two readers with the import enabled and suspended, for the
analytics import's proof (#421). It reaches the cluster only when Steve consents to apply it.

An Orama Cloud v1 stack's `controlDB.stripePrices` holds that stack's Stripe price IDs for the
reference rows the operator seeds into its control database
([`docs/components.md`](../../docs/components.md#the-control-dbs-schema)). `v1-nonprod` leaves it
empty: no test-mode prices exist yet, so its plans carry no Stripe prices and it has no add-on
prices. `v1-prod` needs its live-mode IDs before billing works on it.

A Lake's `spec.payments` ([`docs/lake/payments.md`](../../docs/lake/payments.md)) names a Stripe
Secret with keys `accountID`, `operatorKey` and `appKey`, one per Stripe environment
([orama-bridge#201](https://github.com/oramasearch/orama-bridge/issues/201)). Restricted keys can only
be made in the Stripe Dashboard, so a person creates these Secrets on the managing cluster, with
Steve's consent, and a set copies a member's Secret into its namespace. Verify source existence
and key provenance before applying a set that names it.

| Secret | Namespace | Stripe environment |
|---|---|---|
| `stripe-dev` | `lake` | the `dev` sandbox |
| `stripe-testing` | `lake` | the `testing` sandbox |
| `stripe-staging` | `lake` | the `staging` sandbox |
| `stripe-live` | `lake` | live mode, for `lake-prod` |
| `stripe-v1-dev` | `orama-system` | the `dev` sandbox, for `v1-dev` |

Both production members declare `billing.enabled: true` and live `payments`, using
`lake/stripe-live` copied as `stripe`, with a separate webhook/signing Secret per region.
EU passed its live gate before US moved; the common `lake-prod-stripe` copy is now removed
from the file because neither member reads it. Retained Secret objects are not deleted.
The [sets index](sets/README.md) describes the staged file state. Verify the live source and obtain
the production go-ahead before applying; this declaration alone does not prove active payments.

### Moving a member to `spec.billing`

The `lake-nonprod` set declares `billing.enabled: true`; its migration is pending a reviewed
apply after all source credentials exist ([#884](https://github.com/oramasearch/orama-operator/issues/884)).
`lake-prod` members still bill through a `spec.env` entry, `LAKE_BILLING: "on"`. A Lake's
`spec.billing` ([`docs/lake/payments.md`](../../docs/lake/payments.md#billing-specbilling),
[operator#662](https://github.com/oramasearch/orama-operator/issues/662)) is the typed switch that
replaces it, and `spec.payments` requires `billing: {enabled: true}`. While a member has no
`spec.billing` its `spec.env` entry still passes through, so nothing changes until its file moves.
Each production member moves with its own Stripe roll
([#643](https://github.com/oramasearch/orama-operator/issues/643)), one member per pull request,
applied with Steve's consent:

```yaml
# a member's spec, before
env:
- name: LAKE_BILLING
  value: "on"
# after: the env entry is removed
billing: {enabled: true}
```

Once `spec.billing` is set, a leftover `spec.env` `LAKE_BILLING` is ignored and the operator logs that
it is, so remove it in the same change. An on-prem or enterprise Lake leaves `spec.billing` unset.
`Lake/prod` on the managing cluster bills with the manual provider and keeps its `spec.env` entry
until it is retired (#536).

Both v1 sets run the combined OramaCore engine from the private
`oramasearch/oramacore-cloud-v1`, set by tag in each template's `images.overrides.oramacore`: v1.2.39
plus oramacore#340 and #341, which serve `GET /health/ready`, the readers' readiness probe (#597).
The published `oramasearch/oramacore` image has no such route, so its readers never turn Ready.

[`sets/v1-prod.yaml`](sets/v1-prod.yaml) is the file production runs from. Its template holds only
what `v1-eu` and `v1-us` share, because a member's `spec` is merged over the template and cannot
remove a field from it; each member carries the rest. `v1-eu` has the production values and `v1-us`
keeps its sandbox values with its dashboard disabled ([#885](https://github.com/oramasearch/orama-operator/issues/885)).
The live set remains unchanged until the production file's blockers are resolved and Steve approves
the apply; its old dashboards still serve with `Ready=False`. The file's header classifies every key `v1-eu` needs as generated by the
operator, derived, or copied, and lists what still blocks the cutover. The restricted Stripe source
is ready; Anthropic remains non-working under Juan's explicit deferred exception. Other dashboard
placeholders, Clerk configuration and image qualification still need preparation; see the
[production dashboard procedure](../../docs/infrastructure/operations.md#preparing-the-production-v1-dashboard).
Each member's copied Secrets (`v1-eu`'s production keys, including its restricted Stripe key;
`v1-us` copies none) are in that member's `secrets`
list: a member's list adds Secrets for that member only, and replaces a set entry with the same name
on the member's cluster. Nothing is created directly in a member's namespace.

## Copying a Prod 1 team into a member

A v1 set file also says which Prod 1 teams are being copied into its members: one entry per team
under `spec.migrations`
([operator#654](https://github.com/oramasearch/orama-operator/issues/654)). The managing operator
applies each entry as an `OramaCloudV1Migration` named `name` in the member's namespace on the
member's cluster, with `targetRef` set to the member, and the member's operator runs the copy
([`docs/cloud-v1-migration.md`](../../docs/cloud-v1-migration.md)).
[`sets/v1-nonprod.yaml`](sets/v1-nonprod.yaml) carries the pilot's entry (orama-bridge#206).

```yaml
  migrations:
  - name: team-4e01bc49          # the OramaCloudV1Migration's name
    member: v1-dev               # a clusters[].name of this set
    secrets:                     # the Prod 1 credentials and the evidence receipt, from orama-system
    - {name: v1-nonprod-prod1-db-ro, namespace: orama-system}
    spec: {...}                  # an OramaCloudV1Migration's spec, without targetRef
```

Each change to an entry is a change to the set file, applied to the managing cluster with Steve's
consent like any other set change, and committed with it. A step is one change to `spec`:

1. `step: plan`.
2. `step: seed`, without `apply`: the tool's dry run.
3. `apply: true`: the seed writes to the stack.
4. `step: catchUp`. Raise `attempt` for each further catch-up before the cutover.
5. `step: verify`.

The stack must keep `components.dashboard.crons.suspend: true`, and `analyticsProcessor.import.suspend:
true` while the import is enabled, or a writing step is `Blocked`. Watch progress in the set's
`Migrations` column (`1/2 Succeeded`) and `status.migrations`, and on the member's cluster with
`kubectl get ocv1m -n <namespace>`. A `Blocked` migration does not change the set's phase.

- **Secrets.** A migration's `secrets` are copied into the member's namespace before it is applied,
  as a member's are, and deleted once it is gone. A name the stack already gets from the set or the
  member is refused (copy it under another name with `as`), and so is one name copied from two
  sources into one member. Two migrations may list the same Secret from the same source. The Prod 1
  Secrets must exist in `orama-system` on the managing cluster first; creating them is a write there
  and needs Steve's consent.
- **What cannot change.** `member` and the migration's `teamID`, `projectIDs`, `runID` and `source`
  are fixed once applied. The set reports a change to them on the migration and in its own message,
  and leaves the applied migration as it is. To change one, remove the entry and add a new one under
  a new name.
- **Removing an entry** deletes the migration on its cluster. With `removeOnDelete: true` and data
  already written, that runs the tool's `remove-target` on the stack; a team already switched to
  the stack is refused, and the migration stays `Blocked` in `status.migrations` until a person
  decides. To let a switched team go without removing it, first set `removeOnDelete: false`, apply,
  then remove the entry. A removal holds only that migration; the member and the other entries keep
  reconciling.
- **Removing a member, or deleting the set,** deletes its migrations too: a member's entries go
  with it, since the API server refuses an entry that names no member. Under `deletionPolicy:
  Delete` a member's stack is deleted only once its migrations are gone, and a set being deleted
  keeps its finalizer until all of them are, under either policy. A migration stuck in `Blocked`
  holds that deletion, with its message on the set.

## Running the Lake end-to-end gate from a set file

A `LakeE2ETest` runs on the cluster of the Lake it tests, but these files are applied to the managing
cluster. So the testing -> staging gate is an entry under `spec.e2eTests` of
[`sets/lake-nonprod.yaml`](sets/lake-nonprod.yaml), and the managing operator applies it as a
`LakeE2ETest` on the member's cluster with `spec.member` filled in
([operator#589](https://github.com/oramasearch/orama-operator/issues/589),
[`docs/lake/README.md#service-e2e`](../../docs/lake/README.md#service-e2e)). The image is named by
digest. Applying the file starts a run, and changing `runID` to the next release starts the next;
both are a set change, applied with Steve's consent and committed with it. The result is in the
set's `E2E` column and `status.e2eTests`. The entry is in the file, commented out, until
`oramasearch/lake-e2e` has an image and the managing cluster runs an operator that knows the field.

To check that the files match the cluster:

```sh
kubectl --context orama-production-cluster-01 diff -f config/clusters/ -R
```

Empty output means they match.

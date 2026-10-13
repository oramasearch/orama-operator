# Infrastructure: management and leaf clusters

How the operator manages the clusters Orama's products run on. This page records the
architecture as decided so far. It is early: the first kinds exist, most of it is not built, and
this is the design the work follows. The running discussion and the open questions are in
[#351](https://github.com/oramasearch/orama-operator/issues/351).

**Status, 2026-10-12:** decided in outline. The first three kinds are implemented and tested
against a fake Azure API ([#354](https://github.com/oramasearch/orama-operator/issues/354)); no
real cluster has been created with them yet. The first step is one non-production AKS cluster
([#355](https://github.com/oramasearch/orama-operator/issues/355)).

## The model

There are two kinds of cluster, and **every cluster runs an instance of this operator**.

| | Management cluster | Leaf cluster |
|---|---|---|
| **What it is for** | Getting leaf clusters provisioned and operational, and keeping their cluster-level things right | Running the actual workloads, such as Lake instances |
| **What its operator reconciles** | Kubernetes clusters, to start, and probably nothing else | The workloads on that cluster |
| **How many** | One | Many |

The same operator plays both roles. Which role it plays follows from what it is asked to
reconcile: `KubernetesCluster` resources on the management cluster, workload resources (`Lake`,
`OramaZettaCluster` and the rest) on a leaf.

```
                    management cluster
                 ┌──────────────────────┐
                 │   orama-operator     │
                 │   KubernetesCluster  │──── holds each leaf's credential, permanently
                 │    └─ AksCluster      │
                 │   bridges            │
                 └──────────┬───────────┘
        provisions, installs│the platform layer and the leaf's operator,
        sets versions       │
              ┌─────────────┴─────────────┐
              ▼                           ▼
      leaf cluster                  leaf cluster
   ┌────────────────────┐        ┌────────────────────┐
   │ orama-operator     │        │ orama-operator     │
   │ Karpenter, Istio,  │◀──────▶│ Karpenter, Istio,  │
   │ platform layer     │ bridge │ platform layer     │
   │ Lake, Zetta, ...   │        │ Lake, Zetta, ...   │
   └────────────────────┘        └────────────────────┘
```

## Who owns what

**The dividing line is the scope of the resource.**

- **Cluster resources belong to the management cluster's operator.** These are the things
  traditionally installed by [`helmfile.yaml`](../../helmfile.yaml): other operators, such as the
  Postgres operator, and the rest of the platform layer.
- **Namespace resources belong to the leaf's own operator.** These are the workloads: a Lake
  deployment, a Zetta deployment.

| Concern | Owner |
|---|---|
| Provisioning a leaf cluster | Management |
| Destroying a leaf cluster | Management |
| A leaf's Kubernetes version | Management |
| A leaf's cluster resources: the platform layer and the other operators | Management |
| Installing a leaf's operator, and its version | Management |
| Possibly the versions of workloads such as Lake | Management (not settled) |
| Connections between clusters | Management |
| Autoscaling inside a leaf, for example through Karpenter | Leaf |
| A leaf's namespace resources: Lake, Zetta and other workloads, day to day | Leaf |

Four rules follow from this.

1. **The management cluster is never truly done with a leaf.** There is no hand-off point. It gets
   a leaf provisioned and operational, and it keeps managing the cluster-level things above.
2. **A leaf is as self-contained as possible.** It has the knowledge to manage itself for the most
   part.
3. **A leaf's operator does not manage the cluster it sits on**, and above all not its
   provisioning. Autoscaling, yes. Cluster-level changes, almost never.
4. **The management operator holds each leaf's credential permanently.** That is how it acts
   inside a leaf.

## Clusters are ephemeral

A cluster can be created and destroyed without a person in the loop. Everything needed to make
one operational, including what [`helmfile.yaml`](../../helmfile.yaml) and
[`install.md`](../install.md) do by hand today, has to run from inside the management cluster's
operator.

That is a change from how the operator is installed now. Today a person runs `helmfile` to
install the platform operators and then the operator's chart. In this model the management
operator does both for each leaf.

## A leaf's platform layer

Each leaf has its own: Karpenter, Istio, and the operators its workloads depend on.

The starting point is what `helmfile.yaml` installs today:

| Component | Why it is there |
|---|---|
| CloudNativePG | Postgres for control planes |
| ingress-nginx | External access, with the settings AKS needs |
| cert-manager | Certificates for the Cassandra operator's webhooks |
| Strimzi, Altinity ClickHouse, TiDB, K8ssandra, Valkey operators | Lake's managed service kinds |
| orama-operator | The leaf's own operator |

### Installing it: `ClusterHelmRelease` and `OramaOperator`

Steve, 2026-10-12: *"we need to create an operator CR in the management cluster that will install
and control the version of the leaf operator"*, and, of installing it alone: *"you can't install
something without its prerequisites"*.

The operator is one item in an ordered list, and the list is the helmfile. Two management kinds
install it from inside the management cluster's operator, which reaches the leaf with the
kubeconfig Secret its `KubernetesCluster` wrote:

| Kind | What it is |
|---|---|
| `ClusterHelmRelease` | One Helm chart release on a managed cluster, at a pinned version: chart repository or OCI, values, `dependsOn` other releases, `requiredCRDs`, `crdURLs`. One per helmfile entry. |
| `OramaOperator` | The cluster's own Orama operator at a named version. Changing `spec.version` upgrades the leaf, CRDs first. |

What the install requires, and who makes it true:

| Requirement ([`install.md`](../install.md)) | Met by |
|---|---|
| CloudNativePG before the operator. The manager watches CNPG kinds and a missing CRD is fatal by design ([`crd-rollout.md`](../operations/crd-rollout.md)) | A `ClusterHelmRelease` for CloudNativePG. The operator's chart lists the CRDs it needs in its `orama.cloud/required-crds` annotation, and `OramaOperator` **does not install** onto a cluster that lacks one: it stays `Pending` and names them. |
| The namespace, then the pull secret in it, before the release | `OramaOperator` creates the namespace and copies `spec.imagePullSecretRef` from the management cluster, on every reconcile, so a rotated secret reaches the leaf. |
| A chart whose CRDs and RBAC match the image | The chart is published with every image to the private repository `oramasearch/orama-operator-chart` (`hack/pushchart`), and `OramaOperator` pulls the one for `spec.version`. A release `v0.1.0` has the chart `0.1.0`; a build `dev-83242f9` has `0.0.0-dev-83242f9`. |
| CRDs applied on every upgrade (Helm applies `crds/` only on first install) | Both kinds server-side apply the chart's CRDs before every install and upgrade. |
| A default StorageClass and the volume snapshot CRDs | AKS provides both. |
| ingress-nginx, cert-manager and the service operators | A `ClusterHelmRelease` each, when the leaf needs them. cert-manager before K8ssandra is `dependsOn`. |

The chart, its CRDs and its values are the same for every cluster; `OramaOperator` sets only the
image and its pull secret.

Both kinds report what the leaf says. A release that is not ready lists each Deployment,
StatefulSet or DaemonSet that is not rolled out and what its pods report (an image that cannot be
pulled, a container that keeps exiting, a pod that cannot be scheduled). A refusal by the chart
repository, Helm or the leaf's API server is recorded on the resource and returned as an error.

Deleting either resource uninstalls its release from a cluster that still exists. CRDs stay, so
removing an operator never deletes what it was running. A `ClusterHelmRelease` that others depend
on is not uninstalled until they are gone.

A chart whose CRDs are shipped apart from it (TiDB's `crd.yaml`, which the helmfile applies with a
presync hook) names them in `spec.crdURLs`. Each is applied server-side before the chart, on every
install and upgrade, and may hold nothing but CustomResourceDefinitions.

A release is installed with its namespace as the default for resources whose manifest names none.
Helm otherwise puts them wherever the kubeconfig says: K8ssandra's Deployments landed in `default`
on the first leaf before this was fixed.

[`config/samples/leaf-nonprod-operator.yaml`](../../config/samples/leaf-nonprod-operator.yaml) is
the pair for `orama-nonprod-cluster-01`.

Two components named for a leaf are **not** in the helmfile today and are not on the existing
cluster: **Karpenter** and **Istio**. Karpenter can already be installed by the operator from an
`OramaZettaCluster` ([`karpenter-cloud-iam-prerequisite.md`](../karpenter-cloud-iam-prerequisite.md));
under this model that install is a cluster resource and moves to the management side. Istio is new
to the stack.

## Bridges between clusters

Most clusters are standalone and need none. A set of clusters that serves one system across
regions does: the example is a production system in three regions, with a workload that has to
talk between them.

- **A bridge starts as an Istio link** between two clusters, plus whatever Azure, AWS or Google
  Cloud networking it needs underneath.
- **Istio gives the clusters a shared, multi-cluster address space.** Applications use it; they do
  not build their own.
- **Zetta and OramaCore are applications like any other here.** Each may span more than one
  cluster, and each takes advantage of the bridges.
- **A bridge is a resource on the management cluster**, reconciled by its operator.

The rest is to be worked out.

## Abstract kinds and concrete providers

A general rule for every kind in this work, and for the operator from here on.

- **What a person asks for is an abstract kind**, named for the thing and not for the vendor:
  `KubernetesCluster`, `DnsRecord`, `ServiceMesh`.
- **An abstract kind has a type, and the type provisions a concrete kind** for one provider:
  `AksCluster`, `CloudflareDnsRecord`, `IstioServiceMesh`.
- **The reason is that a provider can be swapped.** When an enterprise client has a hard
  requirement for one provider over another, a different concrete kind goes behind the same
  abstract one, and whatever asked for the abstract kind does not change.

| Abstract kind | First concrete kind | Others it leaves room for |
|---|---|---|
| `KubernetesCluster` | `AksCluster` | EKS, GKE, the liquid k0s distribution |
| `DnsRecord` | `CloudflareDnsRecord` | Another DNS provider |
| `ServiceMesh` | `IstioServiceMesh` | Another mesh |

### A provider is its own resource

The abstract kind does not carry the vendor's settings. It points at a **provider resource**,
which says which vendor, which account and how to authenticate. Proposed by Steve, 2026-10-12:
*"maybe we can create a DnsProvider CRD for example then the abstract can point to that
provider."*

| Abstract kind | Points at | Which provisions |
|---|---|---|
| `DnsRecord` | a `DnsProvider` (for example of type Cloudflare) | `CloudflareDnsRecord` |
| `KubernetesCluster` | a `KubernetesClusterProvider` (of type AKS) | `AksCluster` |
| `ServiceMesh` | a mesh provider (of type Istio) | `IstioServiceMesh` |

```
KubernetesCluster ──providerRef──▶ KubernetesClusterProvider (type: aks)
        │  provider-neutral                 vendor, account, credentials, defaults
        │ provisions and owns
        ▼
AksCluster                          one provider's cluster; the only kind that calls Azure
```

The vendor, the account and the credentials are written once, on the provider, instead of on
every record or cluster. Swapping a provider for an enterprise client means pointing at a
different provider resource; what asked for the abstract kind does not change.

The name `KubernetesClusterProvider` is a proposal, by analogy with `DnsProvider`.

**The kinds the operator has today are concrete only.** `CloudflareDNSRecord`,
`CloudflareLoadBalancer`, `CloudflareKVNamespace`, `CloudflareR2Bucket` and `ClerkApplication`
each name their vendor and have no abstract kind above them, and `Lake` and `OramaZettaCluster`
create `CloudflareDNSRecord` resources directly. Whether and when they gain abstract kinds is not
decided; new kinds follow the rule from the start.

## The `KubernetesCluster` kind

A `KubernetesCluster` describes one cluster in provider-neutral terms and names the provider it
wants. The management cluster's operator reconciles it by provisioning the concrete kind that
provider's type names, keeps the cluster as described, and reports what is true about it. The
first provider type is AKS, which provisions an `AksCluster`.

The three kinds exist ([#354](https://github.com/oramasearch/orama-operator/issues/354)); their
fields are in [`crd-reference.md`](../crd-reference.md#kubernetescluster-separate-crd-cluster-scoped--a-cluster-the-operator-provisions).

| Kind | Short name | What it is |
|---|---|---|
| `KubernetesCluster` | `kcluster` | The cluster that is wanted: region, version, node pools, deletion policy, and the provider to use |
| `KubernetesClusterProvider` | `kcp` | A vendor, an account and its credentials, plus the settings with no neutral equivalent |
| `AksCluster` | `aks` | One AKS managed cluster; created and owned by a `KubernetesCluster` |

What they do today: **create, observe and delete.** A `KubernetesCluster` becomes an `AksCluster`,
which creates the resource group and the managed cluster in Azure, waits for it, and writes the
admin kubeconfig to a Secret. Deleting the `KubernetesCluster` deletes the cluster when its
`deletionPolicy` is `Delete`. They do not yet change a cluster after it exists, and they install
nothing on it.

### Seeing what the provider is doing, and what it refused

Steve, 2026-10-12, watching the first leaf's nodes not come up: *"the operator should talk to the
API and report any errors from the azure api so they can be corrected"*.

On every reconcile the `AksCluster` controller reads Azure's latest operation for the cluster
(`managedClusters/<name>/operations/latest`) along with the cluster itself, and reports both:

| Where | What it shows |
|---|---|
| `kubectl get aks` | `Operation`: Azure's progress line while it works (`CreatingAgentPools: 0/1 nodes completed`), then `Succeeded` or `Failed` |
| `kubectl get kcluster`, `kubectl get aks -o wide` | `Message`: the provisioning state, the progress, and every error Azure reports |
| `status.operation.error`, `status.provisioningError`, `status.nodePools[].error` | Each error where Azure put it: on the operation (with the causes nested under it, such as a VM extension's exit code), on the cluster, on a pool |
| The operator's log | An error line when Azure fails an operation, once per distinct failure |

A failure to read the operation is an error like any other: it is recorded and returned. Creating
`orama-nonprod-cluster-01` took 14 minutes, 11 of them with the system node's virtual machine
running and not yet joined; during that time Azure reported no error and only the progress line
said what it was waiting for.

Not read yet: the node resource group's activity log and the scale set's instance view. Reading
them needs `Microsoft.Compute` and `Microsoft.Insights` read on the provisioner role. Azure
copies a node's bootstrap failure onto the operation's error, which is reported.

### One operator, on every cluster

Steve, 2026-10-12: *"install the same operator everywhere... all crds"*, *"no differences between
management and leaf operator"*, *"any leaf can be a mgnt cluster too... no differences"*.

The operator installed on a leaf is the operator installed on the management cluster: the same
chart, the same CRDs (the cluster-management kinds included, in `chart/crds`), the same controllers
running. There is no flag and no chart value that turns cluster management on or off. **Which
cluster manages which is decided only by where the resources are created**: a cluster holding
`KubernetesCluster` resources is managing those clusters, and a cluster holding none reconciles
nothing of the kind.

What a cluster that does manage others needs is a way to call the cloud, which is configuration,
not a different operator:

```sh
helm upgrade orama-operator chart/ -n orama-system -f <stored values> \
  --set clusterManagement.azureWorkloadIdentityClientID=<managed identity client ID>
```

or a service principal secret named by its `KubernetesClusterProvider`.

This is rule 11 in [`CLAUDE.md`](../../CLAUDE.md), and it covers every kind, the e2e test kinds
included: no flag, chart value or CRD directory makes one cluster's operator differ from another's.
`cmd/crd_coverage_test.go` fails if a CRD leaves `chart/crds`, if a controller is registered
inside a condition, or if a flag or chart switch for controllers comes back.

### What lives in Azure for this (created 2026-10-12)

| | |
|---|---|
| Custom role | `Orama Cluster Provisioner`: read the subscription; read, write and delete resource groups; everything under `Microsoft.ContainerService/managedClusters`; read `Microsoft.ContainerService/locations`. Assigned on the subscription, because creating a resource group is a subscription-level action. |
| Managed identity | `orama-operator-cluster-management`, in `orama-production-cluster-01_group` |
| Federated credential | The existing cluster's OIDC issuer, subject `system:serviceaccount:orama-system:orama-operator` |

The role cannot touch machines, networks, storage, identities or role assignments directly. It can
act on any AKS cluster in the subscription, the existing one included.

### Reaching Azure

The operator needs an Azure identity that may create resource groups and managed clusters in the
subscription. This is a cloud-account prerequisite the operator cannot create for itself, the same
class as an image pull secret.

- **Workload identity (preferred):** a user-assigned managed identity with a federated credential
  for the operator's ServiceAccount (`system:serviceaccount:orama-system:orama-operator`, issued
  by the management cluster's OIDC issuer) and a role assignment on the subscription. The
  credentials Secret holds only `tenantID` and `clientID`.
- **Service principal:** the same Secret with a `clientSecret` as well.

## The path from here

1. **The existing cluster, `orama-production-cluster-01`, becomes the management cluster**, slowly
   and intentionally. Today it also runs workloads, which is a leaf's job, so the conversion
   finishes only when those have somewhere else to live.
2. **The first leaf is a non-production cluster**: a second AKS cluster in the western US, for
   staging, dev and testing environments.
3. **Then an actual production cluster**, possibly in the EU.

Cluster names keep the existing convention:

| Cluster | Role | For |
|---|---|---|
| `orama-production-cluster-01` | Becoming the management cluster | Today: the development environment, despite its name |
| `orama-nonprod-cluster-01` | Leaf, first to be created | Staging, dev and testing |
| `orama-production-cluster-02` | Leaf, after | Production |

## Not decided

Kept in [#351](https://github.com/oramasearch/orama-operator/issues/351), in its comments. Among
them: where provider-specific settings go when the abstract kind is neutral; where a leaf's
credential is kept and what it allows; how a leaf's set of cluster resources
is described; how the management operator upgrades a leaf's operator safely; which Istio
multi-cluster model a bridge uses; what changes in Zetta's own cross-cluster peering once a bridge
provides the address space; and how the existing cluster's workloads move.

## Where the decisions came from

Every statement on this page is from Steve, 2026-10-12, recorded verbatim in
[#351](https://github.com/oramasearch/orama-operator/issues/351). `DESIGN.md` carries it as
decision 46.

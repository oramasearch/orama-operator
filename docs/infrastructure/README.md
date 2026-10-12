# Infrastructure: management and leaf clusters

How the operator manages the clusters Orama's products run on. This page records the
architecture as decided so far. It is early: none of it is built, and this is the design the work
follows. The running discussion and the open questions are in
[#351](https://github.com/oramasearch/orama-operator/issues/351).

**Status, 2026-10-12:** decided in outline; nothing is implemented. The first step is one
non-production AKS cluster.

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

## The `KubernetesCluster` kind

A `KubernetesCluster` describes one cluster. The management cluster's operator creates it, keeps
it as described, and reports what is true about it. AKS is the first provider.

The kind is not designed yet. Its reference will be in
[`crd-reference.md`](../crd-reference.md) when it exists.

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
them: where a leaf's credential is kept and what it allows; how a leaf's set of cluster resources
is described; how the management operator upgrades a leaf's operator safely; which Istio
multi-cluster model a bridge uses; what changes in Zetta's own cross-cluster peering once a bridge
provides the address space; and how the existing cluster's workloads move.

## Where the decisions came from

Every statement on this page is from Steve, 2026-10-12, recorded verbatim in
[#351](https://github.com/oramasearch/orama-operator/issues/351). `DESIGN.md` carries it as
decision 46.

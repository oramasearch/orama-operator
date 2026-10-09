# Karpenter cloud-IAM prerequisite (operator-owned install)

When you set `OramaZettaCluster.spec.capacity.karpenter.install: true`, the operator **owns the Karpenter
install** — it server-side-applies the pinned upstream Karpenter controller (Deployment + ServiceAccount
+ RBAC) and the Karpenter CRDs (`NodePool` / `NodeClaim` / `EC2NodeClass`) as owned resources
([operator#75](https://github.com/oramasearch/orama-operator/issues/75), extending the NodePool
work in [operator#72](https://github.com/oramasearch/orama-operator/pull/72)).

There is **one thing the operator cannot do for you**: mint Karpenter's **cloud identity**. Karpenter's
controller must be permitted to *create and terminate VMs* in your cloud account, and the nodes it
launches must be permitted to *join the cluster*. Those are **cloud-account IAM objects** — the same
class of out-of-band prerequisite as the **image pull secret**: an in-cluster operator has no cloud
credentials with which to create them, and handing it such credentials would defeat least-privilege.

> **Boundary, restated:** the operator owns everything *inside* the cluster (controller workload, RBAC,
> CRDs, the `NodePool`). The cloud IAM identity (AWS instance profile / IAM role, Azure managed identity +
> role assignment) is **yours to provision**, once, per cluster. The operator **detects when it is
> missing and reports a clear `.status` condition instead of crash-looping.**

## Why the operator can't mint it

Creating an IAM role/instance profile (AWS) or a role assignment (Azure) is a **control-plane cloud API
call**, authenticated by **cloud** credentials — not Kubernetes RBAC. The operator only holds a
Kubernetes ServiceAccount. Even the Karpenter controller itself doesn't create its *own* permission to
call EC2; that identity is established out-of-band and then *assumed* by the controller pod. So the chain
has to start in the cloud account, before anything in-cluster can provision VMs.

## What you must provision, per cloud

### AWS / EKS (the self-install path)

Two identities, both created once per cluster (CloudFormation/`eksctl`/Terraform — see the upstream
[Karpenter getting-started guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)):

| Identity | Who assumes it | What it permits | Wired via |
|---|---|---|---|
| **Karpenter controller role** (`KarpenterControllerRole-<cluster>`) | the controller pod, via **IRSA** (or EKS Pod Identity) | describe/create/terminate EC2 instances & fleets, read pricing/offerings, manage the instance profile, read the interruption queue | `spec.capacity.karpenter.controllerRoleARN` → annotated onto the controller ServiceAccount (`eks.amazonaws.com/role-arn`) |
| **Karpenter node role / instance profile** (`KarpenterNodeRole-<cluster>`) | the launched EC2 nodes | join the EKS cluster (`AmazonEKSWorkerNodePolicy`, ECR pull, CNI) | referenced by the **platform-owned** `EC2NodeClass` (`spec.role` / `spec.instanceProfile`), which you reference from `spec.capacity.karpenter.nodeClassRef.name` |

Also required for the controller to resolve *what* to launch into:
`spec.capacity.karpenter.installClusterName` (the AWS `settings.clusterName` — Karpenter discovers the
cluster's tagged subnets / security groups / interruption queue from it).

The **node role/instance profile lives on the `EC2NodeClass`, not on the operator** — the NodeClass
carries AMI/subnet/security-group/IAM specifics the operator cannot invent, so it stays platform-owned
and is *referenced* (`nodeClassRef`), exactly as in operator#72.

### Azure / AKS

**The operator does not self-install on AKS.** Azure **Node Auto Provisioning (NAP)** deploys and manages
the Karpenter components for you, including the controller's Azure managed identity + role assignment. So:

- Enable **NAP** on the AKS cluster (that provisions the cloud identity as part of the managed feature).
- Set `spec.capacity.karpenter.enabled: true` (own the `NodePool`), and reference an `AKSNodeClass`
  (`nodeClassRef.group: karpenter.azure.com`, `kind: AKSNodeClass`).
- Leave `install: false` — the operator reports `KarpenterInstalled=False, Reason=ManagedByAKSNAP` if you
  set it, to make the boundary explicit.

### Bare-metal / k0s

No cloud provider ⇒ no VM API for Karpenter to drive. `install: true` is a clean no-op
(`KarpenterInstalled=False, Reason=BareMetalNoCloud`); `spec.replicas` still packs more Zetta pods onto
the fixed host(s) until full.

## How the operator surfaces a missing identity (no crash-loop)

The install reconcile is **cloud-gated and prerequisite-guarded**. It detects the cloud from a node's
`providerID`, then — on EKS — checks the identity/cluster-name inputs **before** deploying the controller
(a controller with no cloud identity would otherwise `CrashLoopBackOff`). It writes a single
`KarpenterInstalled` condition:

| `.status` condition | Reason | Meaning / action |
|---|---|---|
| `KarpenterInstalled=False` | `InvalidInstallSpec` | `installClusterName` or `controllerRoleARN` is not a valid EKS cluster name / IAM role ARN (the CRD refuses these at admission; this catches a CR stored before the patterns existed, [operator#79](https://github.com/oramasearch/orama-operator/issues/79)). Nothing is applied; fix the field. |
| `KarpenterInstalled=False` | `BareMetalNoCloud` | No cloud provider — nothing to install. |
| `KarpenterInstalled=False` | `ManagedByAKSNAP` | On AKS: enable NAP + use `enabled` (not `install`). |
| `KarpenterInstalled=False` | `UnsupportedCloud` | Cloud detected but not an EKS self-install target — install Karpenter out-of-band and use `enabled`. |
| `KarpenterInstalled=False` | **`CloudIdentityMissing`** | **`controllerRoleARN` is empty** — provision the controller IAM role (IRSA) and set the field. The controller is **not** deployed until it's present. |
| `KarpenterInstalled=False` | `InstallClusterNameRequired` | `installClusterName` is empty — set the AWS cluster name. |
| `KarpenterInstalled=False` | `InstallProgressing` | Manifests applied; controller Deployment not yet `Available`. |
| `KarpenterInstalled=True` | `Installed` | Controller installed and available in `kube-system`. |

```
kubectl get ozc <name> -o jsonpath='{.status.conditions[?(@.type=="KarpenterInstalled")].reason}'
```

## Teardown / safety notes

- The controller install is a **cluster singleton**, ref-counted across every `OramaZettaCluster` that
  opts in. Disabling `install` (or deleting the last consumer CR) removes the operator-managed controller
  ServiceAccount/RBAC/Deployment/Service/PDB via a finalizer (the resources are cluster-scoped, so they
  can't be owner-GC'd by a namespaced CR).
- **The Karpenter CRDs are applied but never deleted on teardown.** Deleting a Karpenter CRD cascades to
  **every** `NodePool`/`NodeClaim` in the cluster (terminating their nodes) — far outside this CR's blast
  radius — and upstream likewise keeps CRDs out of normal chart GC. Remove them deliberately with `kubectl
  delete crd` if you truly intend to uninstall Karpenter cluster-wide.
- Not live-tested yet — validating the self-install end-to-end needs an **EKS** (or AKS-NAP) cluster with
  the cloud IAM prerequisites above. The vendored manifests, the structured rendering of the two values
  and the injection refusals are validated offline by `go test ./internal/controller/...`; with
  `KUBEBUILDER_ASSETS` set, `TestKarpenterInstallEnvtest` also proves the CRD schema and the name-scoped
  RBAC against a real API server ([operator#79](https://github.com/oramasearch/orama-operator/issues/79)).

## Pinned version

The operator vendors Karpenter **v1.13.0** (`internal/controller/assets/karpenter/v1.13.0/`), regenerated
reproducibly by [`hack/fetch-karpenter-manifests.sh`](../hack/fetch-karpenter-manifests.sh). Bump the
version there + the `karpenterInstallVersion` constant to upgrade.

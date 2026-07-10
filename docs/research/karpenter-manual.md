# Kubernetes Karpenter Manual: Practitioner Guide for Node Autoscaling and Provisioning

**Current as of:** 2026-07-10  
**Primary API level:** `karpenter.sh/v1` and `karpenter.k8s.aws/v1`  
**Latest stable release verified:** `v1.13.0`, published 2026-06-10 in the AWS provider release stream ([Karpenter AWS provider release v1.13.0](https://github.com/aws/karpenter-provider-aws/releases/tag/v1.13.0)). The official Karpenter getting-started documentation also uses `KARPENTER_VERSION="1.13.0"` in its current install flow ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)).

## Table of contents

1. [Executive overview](#executive-overview)
2. [Introduction and core concepts](#introduction-and-core-concepts)
3. [Architecture and control loops](#architecture-and-control-loops)
4. [Karpenter versus Cluster Autoscaler](#karpenter-versus-cluster-autoscaler)
5. [Installation and setup](#installation-and-setup)
6. [Versioning, API maturity, and v1 migration](#versioning-api-maturity-and-v1-migration)
7. [Core custom resources and API reference](#core-custom-resources-and-api-reference)
8. [Configuration and usage patterns](#configuration-and-usage-patterns)
9. [Advanced topics](#advanced-topics)
10. [Operations: monitoring, logging, upgrades, and troubleshooting](#operations-monitoring-logging-upgrades-and-troubleshooting)
11. [Best practices](#best-practices)
12. [Cloud provider support](#cloud-provider-support)
13. [CLI and kubectl command reference](#cli-and-kubectl-command-reference)
14. [Real-world recipes](#real-world-recipes)
15. [Appendix: selected settings and metrics](#appendix-selected-settings-and-metrics)

## Executive overview

Karpenter is an open-source Kubernetes node autoscaler and node lifecycle manager that provisions compute just in time for unschedulable Pods and removes or replaces nodes when they are empty, underutilized, expired, drifted, unhealthy, or interrupted, depending on configuration and provider capabilities ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/), [Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)). AWS originally created and open sourced Karpenter in 2021, and AWS later contributed the vendor-neutral core to the Cloud Native Computing Foundation through Kubernetes SIG Autoscaling; the CNCF blog describes Karpenter as open-source node lifecycle management for optimizing Kubernetes workload efficiency and cost ([CNCF Karpenter v1.0.0 blog](https://www.cncf.io/blog/2024/11/06/karpenter-v1-0-0-beta/)).

Karpenter is most useful when workloads are diverse, dynamic, or cost-sensitive because it can choose instance types, zones, architectures, and capacity types directly from pending Pod requirements rather than merely changing the desired size of pre-created node groups ([AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html)). On AWS, Karpenter is configured with a generic `NodePool`, an AWS-specific `EC2NodeClass`, and Karpenter-managed immutable `NodeClaim` resources that represent the actual requested compute capacity ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/), [Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/), [Karpenter NodeClaims documentation](https://karpenter.sh/docs/concepts/nodeclaims/)).

A production Karpenter deployment should pin Karpenter and AMI versions, run the controller on non-Karpenter capacity such as an EKS managed node group or Fargate, define bounded and mutually exclusive NodePools, enable interruption handling for Spot usage, configure disruption budgets for business-hour safety, monitor metrics and events, and test API and CRD upgrades before rolling them out ([AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html), [Karpenter Upgrade Guide](https://karpenter.sh/docs/upgrading/upgrade-guide/)).

## Introduction and core concepts

### What Karpenter is

Karpenter is a Kubernetes operator that watches for Pods the scheduler cannot place and then asks the underlying cloud provider to create suitable nodes for those Pods ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)). The official project repository summarizes Karpenter as a Kubernetes node autoscaler built for flexibility, performance, and simplicity ([Karpenter AWS provider repository](https://github.com/aws/karpenter-provider-aws)).

Karpenter’s central design goal is to make infrastructure follow Kubernetes scheduling demand rather than forcing Kubernetes workloads into a pre-designed node group shape. It combines Pod resource requests, Pod scheduling constraints, NodePool constraints, provider-specific offerings, and disruption policy into node lifecycle decisions ([Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/), [Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)).

### The problem Karpenter solves

Traditional Kubernetes node scaling often requires operators to predefine node groups for each major workload shape, capacity type, availability zone, architecture, or accelerator class. That model can work, but it creates operational friction: too many node groups, capacity stranded in the wrong group, slower response to spiky workloads, and difficult migrations when AMIs, instance types, or topology requirements change ([AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html)).

Karpenter addresses these issues by provisioning nodes based on unschedulable Pod requirements and current provider offerings. On AWS, this means Karpenter can consider many EC2 instance types, zones, and capacity types in a single NodePool, then request an EC2 instance that satisfies Kubernetes scheduling constraints and provider-side capacity availability ([Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)).

### Core mental model

Karpenter can be understood as a three-resource model:

| Resource | Scope | Who creates it | What it means |
|---|---:|---|---|
| `NodePool` | Generic Karpenter API | Operator/platform team | A reusable template and policy boundary for Karpenter-created nodes, including labels, taints, requirements, limits, weight, and disruption behavior ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). |
| `EC2NodeClass` | AWS provider API | Operator/platform team | AWS-specific launch configuration such as AMI selection, subnets, security groups, IAM role or instance profile, block devices, kubelet settings, user data, and tags ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| `NodeClaim` | Generic Karpenter API | Karpenter controller | An immutable request for capacity and the lifecycle object that maps a Karpenter decision to an actual node and cloud instance ([Karpenter NodeClaims documentation](https://karpenter.sh/docs/concepts/nodeclaims/)). |

A typical AWS production configuration has several `NodePool` objects, a smaller number of shared `EC2NodeClass` objects, and many short-lived `NodeClaim` objects created and deleted by Karpenter as workload demand changes ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/), [Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)).

### Just-in-time node provisioning

Karpenter provisions nodes only when the Kubernetes scheduler cannot place Pods on existing capacity. It observes unschedulable Pods, computes a compatible node shape, creates a `NodeClaim`, asks the provider to launch capacity, waits for the node to register, and marks it initialized once it is ready, expected resources are present, and startup taints have been removed ([Karpenter NodeClaims documentation](https://karpenter.sh/docs/concepts/nodeclaims/)).

This just-in-time model means Pod resource requests and scheduling constraints are the source of truth. If a workload omits CPU or memory requests, Karpenter can underestimate the capacity needed; if a workload has overly narrow node affinity or topology constraints, Karpenter may be unable to find a matching offering ([Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/), [AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html)).

### Consolidation, disruption, expiration, and drift

Karpenter does more than add nodes. Its disruption controller can remove empty nodes, consolidate workloads onto fewer or cheaper nodes, replace drifted nodes after configuration changes, and handle cloud-provider interruptions when configured ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)).

The key disruption concepts are:

| Concept | Meaning | Practical implication |
|---|---|---|
| Consolidation | Karpenter identifies empty or underutilized nodes that can be deleted or replaced with lower-priced capacity ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)). | Enables continuous cost optimization, but can move Pods unless budgets, PDBs, and `do-not-disrupt` annotations constrain it. |
| Drift | Karpenter detects when existing NodeClaims no longer match the desired NodePool or NodeClass specification ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)). | Enables node refresh after AMI, subnet, security group, or requirement changes. |
| Expiration | `expireAfter` sets a maximum node lifetime; the default is 720 hours unless changed ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). | Useful for periodic node refresh, but it is a forceful lifecycle trigger rather than a minimum lifetime guarantee. |
| Interruption handling | Karpenter can consume interruption events such as Spot interruptions, scheduled maintenance, stopping/terminating events, and status-check failures ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)). | Essential for Spot-heavy fleets; requires SQS/EventBridge setup for full AWS interruption handling. |
| Budgets | NodePool disruption budgets rate-limit voluntary disruption by percentage, absolute count, reason, and optional schedule ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)). | Protects availability during business hours and controls rollout speed for drift and consolidation. |

## Architecture and control loops

### High-level architecture

Karpenter runs as a controller inside the Kubernetes cluster and interacts with both the Kubernetes API server and the provider API. The official documentation states that it observes cluster events and sends commands to the underlying cloud provider to provision new nodes for unschedulable Pods ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)).

```text
+---------------------+        watches         +---------------------+
| Kubernetes API      | <--------------------> | Karpenter controller|
| Pods, Nodes, CRDs   |                        | provision/disrupt   |
+----------+----------+                        +----------+----------+
           ^                                              |
           | registers                                    | launches/terminates
           |                                              v
+----------+----------+                        +----------+----------+
| kubelet on nodes    |                        | Cloud provider API  |
| EC2 VM / AKS VM     |                        | EC2, VMSS, etc.     |
+---------------------+                        +---------------------+
```

The provider-neutral Karpenter core separates Kubernetes-aware scheduling, batching, and disruption logic from provider-specific APIs, while provider implementations such as AWS and Azure handle the actual compute launch and termination calls ([CNCF Karpenter v1.0.0 blog](https://www.cncf.io/blog/2024/11/06/karpenter-v1-0-0-beta/), [Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)).

### Provisioning loop

A simplified provisioning loop is:

1. Pods become pending because the default scheduler cannot place them.
2. Karpenter batches pending Pods and evaluates NodePools whose taints, labels, and requirements are compatible with the Pods.
3. Karpenter combines Pod constraints, NodePool constraints, provider offerings, resource requests, and daemon overhead to compute one or more feasible NodeClaims.
4. Karpenter creates immutable NodeClaims.
5. The provider launches nodes.
6. Nodes register with Kubernetes.
7. Nodes initialize after readiness, expected resources, and startup-taint removal conditions are satisfied ([Karpenter NodeClaims documentation](https://karpenter.sh/docs/concepts/nodeclaims/), [Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)).

### Disruption loop

The disruption controller discovers nodes eligible for automated graceful disruption and runs one automated method at a time, with drift evaluated before consolidation ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)). Karpenter checks PodDisruptionBudgets, `karpenter.sh/do-not-disrupt` controls, and NodePool disruption budgets, performs a scheduling simulation, taints the node with `karpenter.sh/disrupted:NoSchedule`, pre-spins replacements if required, then deletes the node so the termination controller can drain and terminate the underlying NodeClaim ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)).

### Termination controller

Karpenter places finalizers on nodes and NodeClaims it owns so a node deletion triggers graceful drain and cloud-instance cleanup before the Kubernetes object disappears. During termination, Karpenter adds the disrupted taint, evicts drainable Pods with the Kubernetes Eviction API, waits for related `VolumeAttachment` resources, terminates the cloud provider NodeClaim, and then removes the finalizer ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)).

## Karpenter versus Cluster Autoscaler

Cluster Autoscaler and Karpenter both address node autoscaling, but they use different abstractions. Cluster Autoscaler typically changes desired capacity on existing node groups such as Auto Scaling Groups, while Karpenter provisions individual nodes directly from workload requirements and provider offerings ([AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html), [Karpenter migration guide](https://karpenter.sh/docs/getting-started/migrating-from-cas/)).

| Dimension | Cluster Autoscaler | Karpenter |
|---|---|---|
| Primary abstraction | Existing node groups / ASGs | NodePools plus provider NodeClasses and NodeClaims |
| Capacity shape | Predefined node group instance types and zones | Chosen dynamically from NodePool requirements, Pod constraints, and provider offerings |
| Operational burden | More node groups may be needed for diverse workloads | Fewer, broader NodePools can cover many workload shapes |
| Provider coupling | Often interacts with node group APIs | Provider-specific integrations launch and terminate compute directly |
| Downscaling | Scales node groups down and drains nodes | Consolidation can delete empty nodes, delete underutilized nodes, or replace nodes with cheaper variants ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)). |
| Best fit | Stable fleets, highly standardized workloads, or platforms already invested in node groups | Dynamic fleets, heterogeneous workloads, Spot diversification, GPU/architecture specialization, frequent data-plane refresh |

Karpenter is not always a full replacement for all node capacity. AWS recommends running the Karpenter controller itself on a small EKS managed node group or Fargate profile, because Karpenter should not depend on capacity that it is responsible for provisioning and terminating ([AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html)).

## Installation and setup

### Prerequisites

For AWS EKS, the official getting-started guide uses the AWS CLI, `kubectl`, `eksctl` version 0.202.0 or newer, and Helm, and it states that Karpenter is installed with a Helm chart ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)). The same guide states that AWS IAM Roles for Service Accounts should be used so the in-cluster controller can make privileged AWS requests via its Kubernetes ServiceAccount ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)).

Minimum production prerequisites for EKS:

| Area | Requirement |
|---|---|
| Kubernetes | A supported EKS cluster version and Kubernetes version compatible with the Karpenter release ([Karpenter compatibility documentation](https://karpenter.sh/docs/upgrading/compatibility/)). |
| Controller capacity | Small managed node group or Fargate profile for `kube-system` and Karpenter so the controller is not scheduled only on Karpenter-managed nodes ([AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html)). |
| IAM | Controller role through IRSA or Pod Identity, plus node IAM role or instance profile for launched nodes ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)). |
| Networking | Subnets and security groups discoverable by `EC2NodeClass` selector terms ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| Interruption handling | SQS queue and EventBridge rules if Spot and interruption handling are used ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)). |
| CRDs | `NodePool`, `NodeClaim`, provider `NodeClass`, and related CRDs installed before the controller ([Karpenter Upgrade Guide](https://karpenter.sh/docs/upgrading/upgrade-guide/)). |

### Install with Helm on EKS

The official Karpenter installation path uses Helm and the public ECR OCI chart repository; it recommends IRSA for AWS permissions and uses version `1.13.0` in the current example ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)). The Karpenter upgrade guide recommends managing CRDs with the independent `karpenter-crd` chart, because Helm does not upgrade CRDs placed in a normal chart’s `crds/` directory during ordinary chart upgrades ([Karpenter Upgrade Guide](https://karpenter.sh/docs/upgrading/upgrade-guide/)).

```bash
export KARPENTER_NAMESPACE="kube-system"
export KARPENTER_VERSION="1.13.0"
export CLUSTER_NAME="my-eks-cluster"
export AWS_DEFAULT_REGION="us-west-2"
export AWS_ACCOUNT_ID="$(aws sts get-caller-identity --query Account --output text)"
export KARPENTER_IAM_ROLE_ARN="arn:aws:iam::${AWS_ACCOUNT_ID}:role/KarpenterControllerRole-${CLUSTER_NAME}"

# Install or upgrade CRDs first.
helm upgrade --install karpenter-crd \
  oci://public.ecr.aws/karpenter/karpenter-crd \
  --version "${KARPENTER_VERSION}" \
  --namespace "${KARPENTER_NAMESPACE}" \
  --create-namespace

# Install or upgrade the controller.
helm upgrade --install karpenter \
  oci://public.ecr.aws/karpenter/karpenter \
  --version "${KARPENTER_VERSION}" \
  --namespace "${KARPENTER_NAMESPACE}" \
  --create-namespace \
  --set "serviceAccount.annotations.eks\.amazonaws\.com/role-arn=${KARPENTER_IAM_ROLE_ARN}" \
  --set "settings.clusterName=${CLUSTER_NAME}" \
  --set "settings.interruptionQueue=${CLUSTER_NAME}" \
  --set controller.resources.requests.cpu=1 \
  --set controller.resources.requests.memory=1Gi \
  --set controller.resources.limits.cpu=1 \
  --set controller.resources.limits.memory=1Gi \
  --wait
```

If cluster DNS is not available before Karpenter starts, the official guide documents setting `dnsPolicy=Default`, and the troubleshooting guide explains this commonly occurs when CoreDNS is itself waiting for Karpenter-managed capacity ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/), [Karpenter Troubleshooting Guide](https://karpenter.sh/docs/troubleshooting/)).

```bash
helm upgrade --install karpenter oci://public.ecr.aws/karpenter/karpenter \
  --version "${KARPENTER_VERSION}" \
  --namespace kube-system \
  --set dnsPolicy=Default \
  --set "settings.clusterName=${CLUSTER_NAME}"
```

### Controller permissions on AWS

Karpenter needs permission to discover instance types, subnets, security groups, AMIs, pricing, and capacity offerings; create and delete EC2 instances or fleets; manage instance profiles when `spec.role` is used; read interruption queues when configured; and tag launched resources. The official getting-started path uses CloudFormation and `eksctl` to create Karpenter IAM roles, associate the controller role through IRSA, add the node role to `aws-auth`, and create a Spot service-linked role ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/), [Karpenter CloudFormation reference](https://karpenter.sh/docs/reference/cloudformation/)).

For private clusters, Karpenter documents an `isolatedVPC` setting and required VPC endpoints, including EC2, ECR, S3, STS, SSM, SQS if interruption handling is enabled, and EKS; because IAM does not have a VPC endpoint, private clusters should use `spec.instanceProfile` rather than letting Karpenter manage an instance profile from `spec.role` ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)).

```bash
helm upgrade --install karpenter oci://public.ecr.aws/karpenter/karpenter \
  --version "${KARPENTER_VERSION}" \
  --namespace kube-system \
  --set "settings.clusterName=${CLUSTER_NAME}" \
  --set settings.isolatedVPC=true
```

### Verification after installation

Use these checks after installation:

```bash
kubectl -n kube-system rollout status deployment/karpenter
kubectl -n kube-system get pods -l app.kubernetes.io/name=karpenter -o wide
kubectl get crd | grep -E 'nodepools|nodeclaims|ec2nodeclasses'
kubectl get nodepools
kubectl get ec2nodeclasses
kubectl logs -n kube-system -l app.kubernetes.io/name=karpenter --tail=100
```

The first functional test should create a simple `EC2NodeClass`, a broad `NodePool`, and a Deployment that cannot fit on existing nodes; Karpenter should then create a NodeClaim and a Kubernetes Node ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/), [Karpenter NodeClaims documentation](https://karpenter.sh/docs/concepts/nodeclaims/)).

## Versioning, API maturity, and v1 migration

### Current version

The latest stable AWS provider release verified for this manual is `v1.13.0`, published 2026-06-10, and the official installation guide uses `KARPENTER_VERSION="1.13.0"` ([Karpenter AWS provider release v1.13.0](https://github.com/aws/karpenter-provider-aws/releases/tag/v1.13.0), [Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)). Treat Karpenter and its CRDs as tightly coupled: install the CRD chart version that matches the controller chart version unless official release notes state otherwise ([Karpenter Upgrade Guide](https://karpenter.sh/docs/upgrading/upgrade-guide/)).

### v1beta1 to v1 migration context

Karpenter v1.0 introduced stable `v1` APIs and conversion webhooks so clusters using existing `v1beta1` APIs could migrate; Karpenter v1.1 dropped support for `v1beta1`, so clusters still using `v1beta1` must complete the migration before moving to v1.1 or later ([Karpenter Upgrade Guide](https://karpenter.sh/docs/upgrading/upgrade-guide/)). Karpenter versions v0.36 and below are not supported by Karpenter v1.0 and later, so older installations need a planned staged upgrade and migration path ([Karpenter Upgrade Guide](https://karpenter.sh/docs/upgrading/upgrade-guide/)).

Common v1 migration changes practitioners notice:

| Area | v1-era practice |
|---|---|
| API versions | Use `apiVersion: karpenter.sh/v1` for `NodePool` and `NodeClaim`, and `apiVersion: karpenter.k8s.aws/v1` for `EC2NodeClass` ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/), [Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| NodeClass references | Include `group`, `kind`, and `name` in `spec.template.spec.nodeClassRef`; the docs note group/kind are required in the current v1 flow ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). |
| Kubelet fields | Kubelet-related settings moved from `NodePool` into provider-specific `EC2NodeClass` so non-AWS providers are not forced to support AWS kubelet-specific configuration ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). |
| CRD upgrades | Prefer the standalone `karpenter-crd` chart and upgrade CRDs before the controller ([Karpenter Upgrade Guide](https://karpenter.sh/docs/upgrading/upgrade-guide/)). |
| Launch templates | AWS best practices state that custom launch template support is not available with v1 APIs; use `EC2NodeClass` fields such as AMI selectors, user data, block devices, metadata options, and network settings instead ([AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html), [Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |

## Core custom resources and API reference

### Resource relationship model

On AWS, the normal reference chain is `NodePool -> EC2NodeClass -> NodeClaim -> Node -> EC2 instance`. The `NodePool` owns NodeClaims, the NodeClaims map to Kubernetes nodes and provider capacity, and the `EC2NodeClass` supplies the AWS launch configuration used by NodeClaims ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/), [Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/), [Karpenter NodeClaims documentation](https://karpenter.sh/docs/concepts/nodeclaims/)).

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: default
---
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata:
  name: default
spec:
  amiSelectorTerms:
    - alias: al2023@v20250620
  role: KarpenterNodeRole-my-eks-cluster
  subnetSelectorTerms:
    - tags:
        karpenter.sh/discovery: my-eks-cluster
  securityGroupSelectorTerms:
    - tags:
        karpenter.sh/discovery: my-eks-cluster
```

### NodePool field reference

A `NodePool` defines reusable constraints and behavior for nodes Karpenter may create. Karpenter does nothing unless at least one NodePool exists, and if multiple NodePools match a Pod Karpenter uses the highest `weight`, so official docs recommend making NodePools mutually exclusive where possible ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)).

| Field | Type / examples | Description and notes |
|---|---|---|
| `apiVersion` | `karpenter.sh/v1` | Stable Karpenter v1 API for NodePools ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). |
| `kind` | `NodePool` | Declares the generic Karpenter node pool resource. |
| `metadata.name` | DNS label | Logical pool name; Karpenter also exposes it through the well-known `karpenter.sh/nodepool` label ([Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)). |
| `metadata.annotations` | map | Metadata for the NodePool object itself; not the same as node-template annotations. |
| `spec.template.metadata.labels` | map | Labels applied to all created nodes; labels are also propagated into NodeClaim requirements and count with requirements against the combined 100-item limit ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). |
| `spec.template.metadata.annotations` | map | Annotations applied to all created nodes ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). |
| `spec.template.spec.nodeClassRef.group` | `karpenter.k8s.aws` | API group of the provider-specific NodeClass; current docs include this field explicitly ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). |
| `spec.template.spec.nodeClassRef.kind` | `EC2NodeClass`, `AKSNodeClass` | Provider NodeClass kind. |
| `spec.template.spec.nodeClassRef.name` | name | Name of the NodeClass referenced by the NodePool. |
| `spec.template.spec.taints` | list of Kubernetes taints | Permanent taints placed on provisioned nodes; Pods must tolerate them for Karpenter to consider the NodePool ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). |
| `spec.template.spec.startupTaints` | list of taints | Temporary taints applied at node startup; Pods do not need tolerations, and another component must remove the taints after initialization ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). |
| `spec.template.spec.requirements` | list of selector requirements | Constraints over labels such as instance type, zone, architecture, OS, capacity type, or provider-specific labels; Karpenter combines these with Pod constraints ([Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)). |
| `requirements[].key` | label key | Well-known or custom label key. Unknown custom labels must usually be introduced on the NodePool as requirements or labels so Karpenter can reason about them ([Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)). |
| `requirements[].operator` | `In`, `NotIn`, `Exists`, `DoesNotExist`, `Gt`, `Lt`, `Gte`, `Lte` | Operators supported by Karpenter requirements ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). |
| `requirements[].values` | list of strings | Values used by operators such as `In`, `NotIn`, `Gt`, and `Lt`. |
| `requirements[].minValues` | integer | Alpha diversity control used with the `MIN_VALUES_POLICY` setting; useful for Spot diversification when enabled ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/), [Karpenter Settings reference](https://karpenter.sh/docs/reference/settings/)). |
| `spec.template.spec.expireAfter` | duration or `Never` | Maximum node lifetime; default is 720 hours; changing it causes drift in NodeClaims ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). |
| `spec.template.spec.terminationGracePeriod` | duration | Maximum time a node may drain before Karpenter forcibly deletes remaining Pods; changing it causes drift ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). |
| `spec.limits` | resource map | Caps aggregate resources Karpenter may provision through the NodePool; CPU uses DecimalSI and memory uses BinarySI, and enforcement is eventually consistent during rapid scale-out ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). |
| `spec.limits.cpu` | e.g. `1000` | Maximum CPU capacity across the pool. |
| `spec.limits.memory` | e.g. `1000Gi` | Maximum memory capacity across the pool. |
| `spec.limits.nodes` | integer | Node-count limit; the only limit allowed in static `replicas` mode ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). |
| `spec.weight` | integer | Preference when multiple NodePools match; higher weight is preferred and default is 0 ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). |
| `spec.replicas` | integer | Static capacity mode; cannot be combined with `weight`, cannot be switched back to dynamic mode, and uses `limits.nodes` only ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). |
| `spec.disruption.consolidationPolicy` | `WhenEmptyOrUnderutilized`, `WhenEmpty` | Controls whether Karpenter consolidates empty nodes only or also underutilized nodes ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)). |
| `spec.disruption.consolidateAfter` | duration or `Never` | Delay after a node becomes empty or underutilized before consolidation starts; default behavior is documented in the disruption defaults ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)). |
| `spec.disruption.budgets` | list | Rate limits voluntary disruption by node count, percentage, reason, and optional time window; if undefined, Karpenter defaults to `nodes: 10%` ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)). |
| `status.conditions` | conditions list | Includes readiness and validation conditions for the NodePool. |
| `status.resources` | resource map | Reports current aggregate provisioned resources for the NodePool ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). |

#### Complete NodePool example

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: general-purpose
  annotations:
    platform.example.com/owner: platform-team
spec:
  weight: 50
  limits:
    cpu: "2000"
    memory: 4Ti
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 5m
    budgets:
      - nodes: 10%
      - nodes: "0"
        reasons: ["Underutilized"]
        schedule: "0 14 * * mon-fri"   # 14:00 UTC; adjust for local business hours.
        duration: 10h
  template:
    metadata:
      labels:
        nodepool.example.com/workload-class: general
        billing.example.com/team: platform
      annotations:
        example.com/provisioned-by: karpenter
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: default
      expireAfter: 720h
      terminationGracePeriod: 1h
      startupTaints:
        - key: ebs.csi.aws.com/agent-not-ready
          effect: NoExecute
      requirements:
        - key: kubernetes.io/os
          operator: In
          values: ["linux"]
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64", "arm64"]
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["reserved", "spot", "on-demand"]
        - key: topology.kubernetes.io/zone
          operator: In
          values: ["us-west-2a", "us-west-2b", "us-west-2c"]
        - key: karpenter.k8s.aws/instance-category
          operator: In
          values: ["c", "m", "r"]
        - key: karpenter.k8s.aws/instance-generation
          operator: Gt
          values: ["4"]
        - key: karpenter.k8s.aws/instance-family
          operator: Exists
          minValues: 5
```

### EC2NodeClass field reference

`EC2NodeClass` contains AWS-specific settings and must be referenced by each AWS NodePool. Multiple NodePools may point to the same EC2NodeClass, which is useful when the fleet shares the same networking, IAM, and AMI policy but has different scheduling constraints or disruption rules ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)).

| Field | Type / examples | Description and notes |
|---|---|---|
| `apiVersion` | `karpenter.k8s.aws/v1` | Stable AWS provider v1 API for EC2NodeClass ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| `kind` | `EC2NodeClass` | AWS-specific NodeClass kind. |
| `metadata.name` | name | Referenced by `NodePool.spec.template.spec.nodeClassRef.name`. |
| `spec.kubelet` | object | Subset of kubelet configuration moved from NodePool into EC2NodeClass in v1-era APIs ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/), [Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| `spec.kubelet.podsPerCore` | integer | Limits Pods per CPU core. |
| `spec.kubelet.maxPods` | integer | Caps maximum Pods per node; interacts with ENI/IP limits and VPC CNI prefix delegation ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| `spec.kubelet.systemReserved` | resource map | Reserves resources for system daemons. |
| `spec.kubelet.kubeReserved` | resource map | Reserves resources for Kubernetes daemons. |
| `spec.kubelet.evictionHard` | map | Hard kubelet eviction thresholds. |
| `spec.kubelet.evictionSoft` | map | Soft kubelet eviction thresholds. |
| `spec.kubelet.evictionSoftGracePeriod` | map | Grace periods for soft thresholds. |
| `spec.kubelet.evictionMaxPodGracePeriod` | seconds | Maximum Pod grace period during eviction. |
| `spec.kubelet.imageGCHighThresholdPercent` | percentage | Image garbage collection high threshold. |
| `spec.kubelet.imageGCLowThresholdPercent` | percentage | Image garbage collection low threshold. |
| `spec.kubelet.cpuCFSQuota` | boolean | Controls kubelet CPU CFS quota behavior. |
| `spec.kubelet.clusterDNS` | list of IPs | Overrides cluster DNS IPs. |
| `spec.amiFamily` | `AL2`, `AL2023`, `Bottlerocket`, `Windows2019`, `Windows2022`, `Windows2025`, `Custom` | Controls generated user data and default block devices; may be omitted when using an alias AMI selector term ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| `spec.amiSelectorTerms` | list | Required AMI discovery terms by alias, tags, name, ID, or SSM parameter; `alias` supports family-at-version such as `al2023@v20250620` ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| `spec.amiSelectorTerms[].alias` | e.g. `al2023@v20250620` | Recommended production style is to pin a tested AMI version; `latest` is supported but not recommended for production because it can drift nodes when new AMIs appear ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/), [AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html)). |
| `spec.subnetSelectorTerms` | list | Required subnet discovery terms; terms are ORed, while conditions inside a term are ANDed ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| `spec.securityGroupSelectorTerms` | list | Required security group discovery terms; terms are ORed and conditions within a term are ANDed ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| `spec.role` | IAM role name | Lets Karpenter manage a generated instance profile for node launches; use either `role` or `instanceProfile`, not both ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| `spec.instanceProfile` | instance profile name | Required in private clusters without IAM endpoint access because Karpenter cannot call IAM to manage generated instance profiles ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/), [Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| `spec.tags` | map | Additional EC2 tags on launched resources; restricted domains cannot be overridden ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| `spec.metadataOptions` | object | EC2 instance metadata service options; defaults include HTTP endpoint enabled, tokens required, and hop limit 1 ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| `spec.blockDeviceMappings` | list | EBS root and data volume configuration. |
| `spec.instanceStorePolicy` | `RAID0` | Configures instance store volumes as RAID0 for supported AMI families and exposes total instance-store size as allocatable ephemeral storage ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| `spec.userData` | string | User data merged according to AMI family; with `Custom`, Karpenter does not merge bootstrap logic and the user data must fully bootstrap the node ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| `spec.detailedMonitoring` | boolean | Enables EC2 detailed monitoring. |
| `spec.associatePublicIPAddress` | boolean | Controls whether launched instances receive public IP addresses. |
| `spec.capacityReservationSelectorTerms` | list | Selects On-Demand Capacity Reservations for reserved capacity use cases ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/), [Karpenter ODCR task guide](https://karpenter.sh/docs/tasks/odcrs/)). |
| `spec.placementGroupSelector` | object | Selects EC2 placement groups for cluster, partition, or spread placement requirements ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| `spec.networkInterfaces` | list | Advanced network interface, ENA, and EFA-related configuration. |
| `spec.ipPrefixCount` | integer | Configures prefix delegation behavior for pod IP density use cases. |
| `spec.connectionTracking` | object | Configures Nitro connection tracking timeouts where supported ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| `status.conditions` | conditions list | Includes readiness conditions such as subnet, security group, instance profile, AMI, and overall readiness ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| `status.subnets`, `status.securityGroups`, `status.amis` | lists | Shows resolved provider resources from selector terms. |
| `status.instanceProfile` | string | Shows resolved/generated instance profile when applicable. |

#### Complete EC2NodeClass example

```yaml
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata:
  name: default
spec:
  amiFamily: AL2023
  amiSelectorTerms:
    - alias: al2023@v20250620
  role: KarpenterNodeRole-my-eks-cluster
  subnetSelectorTerms:
    - tags:
        karpenter.sh/discovery: my-eks-cluster
        environment: production
  securityGroupSelectorTerms:
    - tags:
        karpenter.sh/discovery: my-eks-cluster
  tags:
    karpenter.sh/discovery: my-eks-cluster
    cost-center: platform
  kubelet:
    maxPods: 58
    podsPerCore: 10
    systemReserved:
      cpu: 200m
      memory: 256Mi
      ephemeral-storage: 1Gi
    kubeReserved:
      cpu: 200m
      memory: 256Mi
      ephemeral-storage: 1Gi
    evictionHard:
      memory.available: 5%
      nodefs.available: 10%
      nodefs.inodesFree: 10%
  metadataOptions:
    httpEndpoint: enabled
    httpProtocolIPv6: disabled
    httpPutResponseHopLimit: 1
    httpTokens: required
  blockDeviceMappings:
    - deviceName: /dev/xvda
      ebs:
        volumeSize: 80Gi
        volumeType: gp3
        encrypted: true
        deleteOnTermination: true
  detailedMonitoring: true
  userData: |
    #!/bin/bash
    echo "custom pre-bootstrap setup" >/etc/karpenter-user-data-marker
```

### NodeClaim field reference

`NodeClaim` resources are created by Karpenter, not by application teams. The official docs describe NodeClaims as immutable resources that represent a request for capacity and manage the lifecycle of a single node; Karpenter creates NodeClaims based on Pod demand and deletes them during disruption or termination ([Karpenter NodeClaims documentation](https://karpenter.sh/docs/concepts/nodeclaims/)).

| Field | Description |
|---|---|
| `apiVersion: karpenter.sh/v1` | Stable Karpenter NodeClaim API ([Karpenter NodeClaims documentation](https://karpenter.sh/docs/concepts/nodeclaims/)). |
| `kind: NodeClaim` | Karpenter-managed capacity request. |
| `metadata.ownerReferences` | Ties the NodeClaim to the NodePool that created it. |
| `metadata.labels` | Includes labels such as `karpenter.sh/nodepool` and provider-derived labels. |
| `spec.nodeClassRef` | Provider NodeClass used to launch the node. |
| `spec.requirements` | Final requirements resolved from Pod and NodePool constraints. |
| `spec.resources.requests` | Capacity Karpenter requested for Pods and overhead. |
| `spec.taints` and `spec.startupTaints` | Taints applied from the NodePool template. |
| `spec.expireAfter` | Maximum lifetime copied from NodePool template at creation. |
| `spec.terminationGracePeriod` | Drain grace policy copied from NodePool template. |
| `status.providerID` | Provider instance identifier after launch. |
| `status.nodeName` | Kubernetes Node name after registration. |
| `status.imageID` | AMI or image resolved for the node. |
| `status.capacity` and `status.allocatable` | Node resource capacity/allocatable observed by Karpenter. |
| `status.conditions` | Lifecycle conditions such as `Launched`, `Registered`, `Initialized`, `Ready`, and `Consolidatable`; drift-related conditions may appear when drift is detected ([Karpenter NodeClaims documentation](https://karpenter.sh/docs/concepts/nodeclaims/), [Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)). |

NodeClaims are especially useful for troubleshooting because they preserve the decision Karpenter made. Inspect them with:

```bash
kubectl get nodeclaims
kubectl describe nodeclaim <nodeclaim-name>
kubectl get nodeclaim <nodeclaim-name> -o yaml
kubectl get nodes -l karpenter.sh/nodepool=<nodepool-name>
```

### Provider equivalents

Azure AKS Node Auto Provisioning uses Karpenter concepts and exposes `NodePool`, `AKSNodeClass`, and `NodeClaim` resources, while AKS automatically deploys, configures, and manages the Karpenter components for AKS clusters using NAP ([AKS node auto provisioning documentation](https://learn.microsoft.com/en-us/azure/aks/node-autoprovision)). AWS uses the self-managed Helm installation path and `EC2NodeClass` provider API described throughout this manual ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)).

## Configuration and usage patterns

### Defining requirements

Karpenter combines cloud-provider constraints, NodePool constraints, and Pod constraints. The scheduling documentation explicitly describes these as layered constraints and states that Pod constraints must fall within NodePool constraints for Karpenter to provision a node ([Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)).

Important well-known labels include:

| Label | Use |
|---|---|
| `kubernetes.io/os` | Restrict Linux or Windows nodes. |
| `kubernetes.io/arch` | Restrict CPU architecture such as `amd64` or `arm64`. |
| `topology.kubernetes.io/zone` | Restrict availability zones. |
| `node.kubernetes.io/instance-type` | Restrict exact instance types. |
| `karpenter.sh/capacity-type` | Select `reserved`, `spot`, or `on-demand`; Karpenter prioritizes reserved, then Spot, then On-Demand when multiple are allowed ([Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)). |
| `karpenter.sh/nodepool` | Target a specific NodePool. |
| `karpenter.k8s.aws/instance-category` | Restrict AWS instance categories such as `c`, `m`, or `r`. |
| `karpenter.k8s.aws/instance-generation` | Restrict AWS instance generations. |
| `karpenter.k8s.aws/instance-family` | Restrict AWS instance families. |
| `karpenter.k8s.aws/instance-cpu` | Restrict vCPU count. |
| `karpenter.k8s.aws/instance-memory` | Restrict memory. |
| `karpenter.k8s.aws/instance-gpu-*` | Restrict GPU capabilities. |
| `karpenter.k8s.aws/instance-local-nvme` | Restrict local NVMe instance-store capacity. |

A broad requirements strategy improves availability and Spot placement:

```yaml
requirements:
  - key: kubernetes.io/os
    operator: In
    values: ["linux"]
  - key: kubernetes.io/arch
    operator: In
    values: ["amd64", "arm64"]
  - key: karpenter.sh/capacity-type
    operator: In
    values: ["spot", "on-demand"]
  - key: karpenter.k8s.aws/instance-category
    operator: In
    values: ["c", "m", "r"]
  - key: karpenter.k8s.aws/instance-generation
    operator: Gt
    values: ["4"]
```

### Capacity types: reserved, Spot, and On-Demand

Karpenter supports `reserved`, `spot`, and `on-demand` as capacity types, and the scheduling docs state that if several capacity types are allowed Karpenter prioritizes reserved capacity, then Spot, then On-Demand ([Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)). Karpenter caches EC2 insufficient-capacity responses for three minutes across NodePools, which helps it fall back quickly when a Spot or reserved offering is unavailable ([Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)).

Production guidance:

- Use `reserved` where On-Demand Capacity Reservations or Capacity Blocks exist and should be consumed first.
- Use broad `spot` NodePools for interruptible workloads with PDBs, checkpointing, or replicas.
- Use `on-demand` for critical singleton workloads, stateful services without resilient failover, and baseline capacity.
- Allow many instance families, sizes, and zones for Spot; overconstraining Spot reduces placement success and can increase interruption risk ([AWS Spot with Karpenter blog](https://aws.amazon.com/blogs/containers/using-amazon-ec2-spot-instances-with-karpenter/), [AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html)).

### Labels, taints, and startup taints

Use labels for selection, cost allocation, observability, and topology; use taints to prevent Pods from landing on nodes unless they explicitly tolerate the taint. Karpenter will not use a NodePool for a Pod if the NodePool has a taint the Pod does not tolerate ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)).

Startup taints are different: Karpenter applies them to new nodes, but Pods do not need to tolerate them because Karpenter assumes another component such as a DaemonSet will remove them after initialization. Misconfigured startup taints can cause repeated provisioning if no component removes them, so they should be used only when a known node-initialization component will clear them ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)).

Example GPU taint and toleration:

```yaml
# NodePool template
spec:
  template:
    spec:
      taints:
        - key: nvidia.com/gpu
          value: "true"
          effect: NoSchedule
```

```yaml
# Pod spec
spec:
  tolerations:
    - key: nvidia.com/gpu
      operator: Equal
      value: "true"
      effect: NoSchedule
  nodeSelector:
    karpenter.sh/nodepool: gpu
```

### Resource limits

`spec.limits` caps aggregate resources Karpenter may provision for a NodePool, but the official docs warn that limit enforcement is eventually consistent and may temporarily overrun during rapid scale-out ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)). Use limits as a safety guardrail, not as the only cost-control mechanism.

```yaml
spec:
  limits:
    cpu: "1000"
    memory: 2Ti
```

Recommended guardrails include NodePool limits, AWS Budgets or Cost Anomaly Detection, quotas on expensive instance families, workload resource quotas, and explicit team-owned NodePools for high-cost hardware ([AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html)).

### Weights and multiple NodePools

If multiple NodePools match a Pod, Karpenter uses the matching NodePool with the highest weight. The official docs recommend mutually exclusive NodePools so no Pod matches multiple pools unless weighted fallback behavior is deliberate ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)).

Common designs:

| Design | Pattern |
|---|---|
| Preferred Spot with On-Demand fallback | Spot NodePool weight 100, On-Demand NodePool weight 10, both match the same general workloads. |
| Team isolation | Team NodePools have unique labels/taints and namespace policies require matching tolerations. |
| Hardware specialization | GPU, Graviton, high-memory, or local-NVMe pools use labels and taints. |
| Static baseline plus dynamic burst | Static `replicas` NodePool provides warm baseline; dynamic NodePool handles bursts. |

### Disruption settings

Karpenter’s default disruption configuration uses `consolidationPolicy` and `consolidateAfter`, and disruption budgets control how quickly voluntary disruption can begin ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)). A practical production starting point is to allow empty-node consolidation quickly, delay underutilized consolidation, and block underutilized consolidation during business hours.

```yaml
spec:
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 10m
    budgets:
      - nodes: 10%
      - nodes: "0"
        reasons: ["Underutilized"]
        schedule: "0 14 * * mon-fri"
        duration: 10h
```

Budgets support absolute counts and percentages; Karpenter evaluates multiple budgets and uses the most restrictive allowed disruption count, with percentage calculations accounting for deleting and NotReady nodes ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)). Budgets can be scoped by reason, with documented reasons including `Drifted`, `Underutilized`, and `Empty` ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)).

### Static NodePools

Setting `spec.replicas` enables static capacity mode. The official NodePool docs state that static NodePools cannot use `weight`, only support `limits.nodes`, are not considered for consolidation, and cannot be switched back to dynamic mode by removing `replicas` ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)).

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: static-baseline
spec:
  replicas: 3
  limits:
    nodes: 5
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: default
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
```

Scale static NodePools with:

```bash
kubectl scale nodepool static-baseline --replicas=5
```

## Advanced topics

### Spot instance handling and interruption

Karpenter can handle interruption events including Spot interruption warnings, scheduled maintenance, instance stopping or terminating events, and status-check failures. For AWS Spot interruptions, EC2 provides a two-minute warning, and Karpenter can start replacement capacity and drain the node in parallel when interruption handling is configured ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/), [AWS Spot with Karpenter blog](https://aws.amazon.com/blogs/containers/using-amazon-ec2-spot-instances-with-karpenter/)).

Full AWS interruption handling requires an SQS queue and EventBridge rules plus the `settings.interruptionQueue` Helm setting. Karpenter also detects instance status issues through EC2 APIs, but the disruption docs distinguish this from queue-based interruption events ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)).

Spot recommendations:

- Enable interruption handling before placing important workloads on Spot.
- Use many instance families, sizes, and zones; avoid single-instance-type Spot NodePools.
- Use PodDisruptionBudgets, multiple replicas, topology spread, and graceful shutdown hooks.
- Do not run AWS Node Termination Handler for the same behavior Karpenter handles, because duplicate drain logic can cause unnecessary churn ([AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html)).

### Spot-to-Spot consolidation

Karpenter supports Spot deletion consolidation by default and supports Spot-to-Spot replacement when the `SpotToSpotConsolidation` feature gate is enabled. The disruption docs state that Karpenter requires a minimum instance-type flexibility of 15 for single-node Spot-to-Spot replacement to avoid replacing a node with a cheaper but too-constrained offering that may be interrupted more often ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)).

### Drift detection

Drift occurs when a NodeClaim no longer matches the desired state implied by its NodePool and NodeClass. Karpenter treats changes to the NodePool template or resolved EC2NodeClass fields such as AMIs, subnets, and security groups as potential drift, while behavioral fields such as `spec.weight`, `spec.limits`, and `spec.disruption` do not drift existing nodes ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)).

Common drift triggers:

- AMI selector resolves to a newer AMI.
- NodePool requirement changes make an existing NodeClaim incompatible.
- Security group or subnet selector resolves to a different set.
- `expireAfter` or `terminationGracePeriod` changes in the NodePool template.

Use pinned AMI aliases for production to make drift intentional:

```yaml
spec:
  amiSelectorTerms:
    - alias: al2023@v20250620
```

The EC2NodeClass docs support `latest`, but warn that production users should avoid it because newly released AMIs can drift nodes unexpectedly ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). AWS best practices similarly recommend pinning tested AMIs and testing new AMIs before production rollout ([AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html)).

### Scheduling constraints and preferences

Karpenter understands standard Kubernetes scheduling controls such as node selectors, node affinity, pod affinity/anti-affinity, topology spread constraints, taints, and tolerations. It treats preferred scheduling constraints as preferences that it may try to satisfy, then relax, which can produce more nodes than expected if preferences are too strict ([Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)).

Supported topology spread keys include `topology.kubernetes.io/zone`, `kubernetes.io/hostname`, and `karpenter.sh/capacity-type`, which makes it possible to spread replicas across zones, nodes, and capacity types ([Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)).

Example spreading across zones and capacity types:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
spec:
  replicas: 6
  selector:
    matchLabels:
      app: api
  template:
    metadata:
      labels:
        app: api
    spec:
      topologySpreadConstraints:
        - maxSkew: 1
          topologyKey: topology.kubernetes.io/zone
          whenUnsatisfiable: DoNotSchedule
          labelSelector:
            matchLabels:
              app: api
        - maxSkew: 1
          topologyKey: karpenter.sh/capacity-type
          whenUnsatisfiable: ScheduleAnyway
          labelSelector:
            matchLabels:
              app: api
      containers:
        - name: api
          image: public.ecr.aws/nginx/nginx:latest
          resources:
            requests:
              cpu: "500m"
              memory: "512Mi"
```

### Pod density and VPC CNI constraints

Pod density is controlled by a combination of kubelet `maxPods`, `podsPerCore`, ENI limits, IP availability, CNI behavior, and subnet capacity. EC2NodeClass exposes kubelet pod-density fields, and the Karpenter settings reference includes `RESERVED_ENIS`, which should be set to `1` when using VPC CNI custom networking or Security Groups for Pods so Karpenter accounts for reserved network interfaces ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/), [Karpenter Settings reference](https://karpenter.sh/docs/reference/settings/)).

Operational symptoms of pod-density mismatch include nodes that are CPU/memory-light but cannot accept more Pods, CNI IP allocation failures, or Karpenter selecting instance types that appear large enough by CPU but are too small by Pod/IP capacity. Use `maxPods`, prefix delegation, larger subnets, and `RESERVED_ENIS` to align Karpenter’s scheduling simulation with actual networking capacity ([Karpenter Troubleshooting Guide](https://karpenter.sh/docs/troubleshooting/)).

### Custom user data, AMIs, and launch templates

With v1 APIs, AWS best practices state that custom launch template support is unavailable; use EC2NodeClass fields instead ([AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html)). EC2NodeClass supports AMI selection, user data, block devices, metadata options, detailed monitoring, instance-store policy, tags, network interfaces, and other launch-related settings ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)).

For standard AMI families, Karpenter merges user data with generated bootstrap data. For `Custom` AMI family, Karpenter does not merge bootstrap logic, so the user data must fully join the cluster and handle the `karpenter.sh/unregistered:NoExecute` startup taint requirement described in the NodeClass docs ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)).

### Instance store volumes

Set `instanceStorePolicy: RAID0` in EC2NodeClass to configure local instance-store volumes as RAID0 where supported. The EC2NodeClass documentation states that this causes allocatable ephemeral storage to account for total instance-store size when supported by the AMI family and Kubernetes version ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)).

```yaml
spec:
  instanceStorePolicy: RAID0
```

### On-Demand Capacity Reservations and reserved capacity

Karpenter supports reserved capacity workflows through `reserved` capacity type and EC2NodeClass `capacityReservationSelectorTerms`. The ODCR task guide describes using Karpenter with On-Demand Capacity Reservations so workloads can target reserved EC2 capacity before other capacity types ([Karpenter ODCR task guide](https://karpenter.sh/docs/tasks/odcrs/), [Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)).

### Windows, Bottlerocket, AL2, and AL2023

EC2NodeClass supports AMI families including AL2, AL2023, Bottlerocket, Windows 2019, Windows 2022, Windows 2025, and Custom. The NodeClass docs note that AL2 AMI support ends with newer Kubernetes releases and that Ubuntu automatic AMI support was removed in v1, requiring custom AMI handling for Ubuntu-like workflows ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)).

## Operations: monitoring, logging, upgrades, and troubleshooting

### Monitoring and Prometheus metrics

Karpenter exposes Prometheus metrics through a metrics endpoint, and the metrics reference lists metrics for build info, NodeClaims, Nodes, Pods, voluntary disruption, scheduling, NodePools, interruption handling, cloud-provider calls, controller runtime, workqueues, client-go, and AWS SDK behavior ([Karpenter Metrics reference](https://karpenter.sh/docs/reference/metrics/)). The getting-started guide includes optional Prometheus and Grafana installation examples with Karpenter dashboards ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)).

Important metrics to monitor:

| Metric | Why it matters |
|---|---|
| `karpenter_nodeclaims_created_total` | New capacity creation rate. |
| `karpenter_nodeclaims_disrupted_total` | Disruption volume by reason. |
| `karpenter_nodes_terminated_total` | Node termination volume. |
| `karpenter_pods_state` | Pending/bound/drained Pod state observed by Karpenter. |
| `karpenter_scheduler_unschedulable_pods_count` | Backlog of Pods Karpenter must handle. |
| `karpenter_scheduler_queue_depth` | Provisioning queue pressure. |
| `karpenter_nodepools_usage` and `karpenter_nodepools_limit` | NodePool usage against configured limits. |
| `karpenter_nodepools_allowed_disruptions` | Current disruption capacity under budgets. |
| `karpenter_cloudprovider_errors_total` | Provider API failures. |
| `karpenter_interruption_received_messages_total` | SQS interruption event volume. |
| `karpenter_cluster_utilization_percent` | Cluster utilization signal for efficiency. |
| `karpenter_build_info` | Running version and build metadata. |

Example ServiceMonitor:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: karpenter
  namespace: kube-system
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: karpenter
  namespaceSelector:
    matchNames:
      - kube-system
  endpoints:
    - port: http-metrics
      path: /metrics
      interval: 30s
```

### Logging

Use controller logs for provisioning, scheduling, disruption, and provider-call diagnosis:

```bash
kubectl logs -n kube-system -l app.kubernetes.io/name=karpenter -f
kubectl logs -n kube-system -l app.kubernetes.io/name=karpenter --since=1h | grep -i error
```

The settings reference documents a `LOG_LEVEL` setting, so debug logging can be enabled through Helm values or environment configuration when deeper diagnosis is needed ([Karpenter Settings reference](https://karpenter.sh/docs/reference/settings/)).

### Events

Karpenter emits Kubernetes events on NodePools, NodeClaims, Nodes, and Pods. Watch events while testing:

```bash
kubectl get events -A --sort-by=.lastTimestamp | grep -i karpenter
kubectl get events -A --field-selector source=karpenter -w
kubectl describe nodepool general-purpose
kubectl describe nodeclaim <nodeclaim-name>
```

AKS NAP documentation also recommends watching events with `kubectl get events -A --field-selector source=karpenter -w` for Azure-managed Karpenter workflows ([AKS node auto provisioning documentation](https://learn.microsoft.com/en-us/azure/aks/node-autoprovision)).

### Upgrading Karpenter

A safe upgrade sequence:

1. Read release notes for the target Karpenter version.
2. Confirm Kubernetes compatibility.
3. Back up `NodePool`, `EC2NodeClass`, disruption budgets, and Helm values.
4. Install or upgrade CRDs using `karpenter-crd` at the target version.
5. Upgrade the controller Helm release.
6. Watch controller rollout, logs, events, NodePool conditions, and NodeClaim creation.
7. Test provisioning and disruption in staging before production.

The upgrade guide recommends the independent CRD chart and warns that Helm does not manage CRD upgrades in the normal chart path ([Karpenter Upgrade Guide](https://karpenter.sh/docs/upgrading/upgrade-guide/)). The same guide states that v1.1 and later no longer support v1beta1 APIs, so API migration must be completed before upgrading old clusters past v1.0 ([Karpenter Upgrade Guide](https://karpenter.sh/docs/upgrading/upgrade-guide/)).

### Troubleshooting playbook

| Symptom | Likely causes | Commands / fixes |
|---|---|---|
| Pending Pods, no NodeClaims | No matching NodePool, taint not tolerated, requirements too narrow, missing resource requests, NodePool not ready | `kubectl describe pod`, `kubectl describe nodepool`, `kubectl logs -n kube-system -l app.kubernetes.io/name=karpenter` |
| NodeClaims created, nodes not Ready | Node IAM role not mapped, subnet/security group issue, bootstrap/user data failure, DNS/STI/SSM private endpoint issue | `kubectl describe nodeclaim`, `aws ec2 describe-instances`, SSM into node, verify `aws-auth` or access entries |
| `no instance type met scheduling requirements` | Requirements, Pod selectors, topology, resource requests, or daemon overhead exclude all offerings | Broaden instance families, sizes, zones, and capacity types; inspect Pod affinity and topology |
| Spot capacity failures | Too few Spot pools or AZs, high interruption market, insufficient capacity cache active | Add instance families/sizes, zones, or On-Demand fallback ([Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)). |
| Nodes not consolidating | PDB blocks eviction, `do-not-disrupt`, consolidation disabled, node not initialized, topology constraints prevent rescheduling | `kubectl describe node`, check PDBs, check `karpenter.sh/do-not-disrupt`, inspect disruption logs ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)). |
| Unexpected node churn | AMI alias `latest`, aggressive consolidation, short `expireAfter`, duplicate Node Termination Handler | Pin AMIs, increase `consolidateAfter`, add budgets, disable duplicate interruption handling ([AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html)). |
| Webhook/unknown field errors | CRDs older than controller or manifests using newer fields | Upgrade CRDs first with `karpenter-crd` ([Karpenter Upgrade Guide](https://karpenter.sh/docs/upgrading/upgrade-guide/)). |
| DNS errors in controller startup | Karpenter depends on CoreDNS capacity that is not running yet | Run controller on MNG/Fargate or set `dnsPolicy=Default` ([Karpenter Troubleshooting Guide](https://karpenter.sh/docs/troubleshooting/)). |
| Private cluster cannot create instance profile | IAM has no private VPC endpoint | Use `spec.instanceProfile` instead of `spec.role` ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)). |
| Spot service-linked role missing | New AWS account has no Spot role | Create `AWSServiceRoleForEC2Spot` with AWS CLI as documented in setup flows ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)). |

## Best practices

### Cost optimization

- Prefer broad NodePools over many narrowly scoped pools so Karpenter can choose the cheapest compatible capacity and has enough Spot diversity ([AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html)).
- Use `reserved`, `spot`, and `on-demand` deliberately; Karpenter prioritizes reserved, then Spot, then On-Demand when all are compatible ([Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)).
- Enable consolidation, but protect workloads with PDBs, budgets, and careful topology policies ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)).
- Set NodePool `limits` and cloud-side budgets for high-cost hardware and runaway scale scenarios ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/), [AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html)).
- Set accurate CPU, memory, and non-CPU resource requests; Karpenter provisions from requests, not actual usage ([Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)).

### Reliability

- Run Karpenter on non-Karpenter capacity such as a managed node group or Fargate ([AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html)).
- Use PDBs for replicated services and avoid overly strict anti-affinity that prevents consolidation or replacement ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/), [Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)).
- Enable interruption handling before using Spot for production workloads ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)).
- Use `terminationGracePeriod` to bound drain duration for nodes with long-lived Pods ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)).
- Pin AMIs and roll them forward intentionally through drift-controlled maintenance windows ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/), [AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html)).

### Multi-tenancy

- Use team-specific labels, taints, namespaces, quotas, and admission policy to prevent teams from accidentally using expensive or sensitive NodePools.
- Create mutually exclusive NodePools when possible, because Karpenter chooses the highest weighted matching NodePool if several match ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)).
- Apply NodePool `limits` per tenant or hardware class to cap blast radius ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)).
- Use separate EC2NodeClasses when teams need different subnets, security groups, IAM roles, AMIs, block devices, or user data ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)).

### Recommended production defaults

| Setting | Starting recommendation |
|---|---|
| Controller placement | EKS managed node group or Fargate, not Karpenter-only nodes ([AWS EKS Karpenter best practices](https://docs.aws.amazon.com/eks/latest/best-practices/karpenter.html)). |
| AMI | Pin an AL2023 or Bottlerocket alias version tested in staging; avoid `latest` in production ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)). |
| Consolidation | `WhenEmptyOrUnderutilized` with `consolidateAfter` of 5–15 minutes for general workloads; longer for stateful or latency-sensitive workloads. |
| Budgets | Default plus explicit business-hour budget blocking underutilized disruption. |
| Spot | Broad families, sizes, and zones; interruption handling enabled. |
| Limits | CPU/memory caps per NodePool plus separate caps for GPU and local-NVMe pools. |
| Requests | Enforce default requests with LimitRanges and resource quotas. |
| Upgrades | Upgrade CRDs first, then controller; test drift and provisioning before production ([Karpenter Upgrade Guide](https://karpenter.sh/docs/upgrading/upgrade-guide/)). |

## Cloud provider support

### AWS

AWS EKS remains the primary and most mature provider path in the official Karpenter documentation. The official getting-started guide focuses on the AWS provider for EKS, uses the `EC2NodeClass` provider API, and points to AWS-specific CloudFormation, IAM, networking, and interruption setup ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/), [Karpenter AWS provider repository](https://github.com/aws/karpenter-provider-aws)).

### Azure

Azure AKS Node Auto Provisioning is based on open-source Karpenter and AKS Karpenter provider technology; AKS automatically deploys, configures, and manages Karpenter for NAP-enabled AKS clusters ([AKS node auto provisioning documentation](https://learn.microsoft.com/en-us/azure/aks/node-autoprovision)). AKS NAP exposes `NodePool`, `AKSNodeClass`, and `NodeClaim`, but Azure documents provider-specific limitations and enablement requirements such as Azure CLI 2.76.0 or later and incompatibility with clusters that already have cluster autoscaler enabled on node pools ([AKS node auto provisioning documentation](https://learn.microsoft.com/en-us/azure/aks/node-autoprovision)).

Enable NAP on a new AKS cluster:

```bash
az aks create \
  --name my-aks \
  --resource-group my-rg \
  --node-provisioning-mode Auto \
  --network-plugin azure \
  --network-plugin-mode overlay \
  --network-dataplane cilium \
  --generate-ssh-keys
```

Enable NAP on an existing AKS cluster:

```bash
az aks update \
  --name my-aks \
  --resource-group my-rg \
  --node-provisioning-mode Auto
```

Disable NAP:

```bash
az aks update \
  --name my-aks \
  --resource-group my-rg \
  --node-provisioning-mode Manual
```

Microsoft’s NAP disruption documentation uses the same general Karpenter concepts of consolidation policy, consolidation delay, expiration, and budgets, but Azure provider field placement and behavior should be checked against the current AKS documentation before copying AWS manifests directly ([AKS NAP disruption policies](https://learn.microsoft.com/en-us/azure/aks/node-autoprovision-disruption)).

### Other providers

The CNCF v1.0.0 blog explains that Karpenter separates provider-neutral Kubernetes scheduling and bin-packing logic from provider-specific compute APIs, allowing providers such as AWS, Azure, GCP, and others to build integrations ([CNCF Karpenter v1.0.0 blog](https://www.cncf.io/blog/2024/11/06/karpenter-v1-0-0-beta/)). For production support, verify that a provider has an actively maintained provider repository, documented NodeClass API, tested cloud permissions model, upgrade path, and compatibility matrix before relying on it.

## CLI and kubectl command reference

### Discovery and status

```bash
kubectl get nodepools
kubectl get nodepools -o wide
kubectl describe nodepool <name>
kubectl get ec2nodeclasses
kubectl describe ec2nodeclass <name>
kubectl get nodeclaims
kubectl get nodeclaims -o wide
kubectl describe nodeclaim <name>
kubectl get nodes -l karpenter.sh/nodepool
kubectl get pods -A --field-selector=status.phase=Pending
```

### Logs and events

```bash
kubectl logs -n kube-system -l app.kubernetes.io/name=karpenter -f
kubectl logs -n kube-system -l app.kubernetes.io/name=karpenter --since=30m
kubectl get events -A --sort-by=.lastTimestamp | grep -i karpenter
kubectl get events -A --field-selector source=karpenter -w
```

### Manual disruption

The disruption docs state that deleting a Karpenter node or its NodeClaim cascades to the other because Karpenter nodes are owned by NodeClaims, and the Karpenter finalizer handles graceful shutdown ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/)).

```bash
kubectl delete nodeclaim <nodeclaim-name>
kubectl delete node <node-name>
kubectl delete nodeclaims --all
kubectl delete nodes -l karpenter.sh/nodepool
kubectl delete nodeclaims -l karpenter.sh/nodepool=<nodepool-name>
```

### Static NodePool scaling

```bash
kubectl scale nodepool static-baseline --replicas=5
kubectl get nodepool static-baseline -o yaml
```

### Patch disruption settings

```bash
kubectl patch nodepool general-purpose --type merge -p '{
  "spec": {
    "disruption": {
      "consolidationPolicy": "WhenEmptyOrUnderutilized",
      "consolidateAfter": "10m"
    }
  }
}'
```

### Check AMI, subnet, and security group resolution

```bash
kubectl get ec2nodeclass default -o jsonpath='{.status.amis}'
echo
kubectl get ec2nodeclass default -o jsonpath='{.status.subnets}'
echo
kubectl get ec2nodeclass default -o jsonpath='{.status.securityGroups}'
echo
```

### Inspect a scheduling failure

```bash
POD=<pending-pod>
NS=<namespace>
kubectl describe pod -n "${NS}" "${POD}"
kubectl get pod -n "${NS}" "${POD}" -o jsonpath='{.spec.nodeSelector}'
echo
kubectl get pod -n "${NS}" "${POD}" -o jsonpath='{.spec.affinity}'
echo
kubectl get pod -n "${NS}" "${POD}" -o jsonpath='{.spec.tolerations}'
echo
kubectl logs -n kube-system -l app.kubernetes.io/name=karpenter --since=15m | grep -E "${POD}|no instance|incompatible|provision"
```

## Real-world recipes

### Recipe 1: Minimal production-ready AWS general-purpose pool

This recipe uses a pinned AL2023 AMI alias, a broad set of instance categories, Spot plus On-Demand capacity, disruption budgets, and Karpenter discovery tags. It follows the official AWS `EC2NodeClass` and `NodePool` model ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/), [Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)).

```yaml
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata:
  name: default
spec:
  amiFamily: AL2023
  amiSelectorTerms:
    - alias: al2023@v20250620
  role: KarpenterNodeRole-my-eks-cluster
  subnetSelectorTerms:
    - tags:
        karpenter.sh/discovery: my-eks-cluster
  securityGroupSelectorTerms:
    - tags:
        karpenter.sh/discovery: my-eks-cluster
  tags:
    karpenter.sh/discovery: my-eks-cluster
    environment: production
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: general
spec:
  weight: 50
  limits:
    cpu: "1000"
    memory: 2Ti
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 10m
    budgets:
      - nodes: 10%
  template:
    metadata:
      labels:
        workload.example.com/class: general
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: default
      expireAfter: 720h
      terminationGracePeriod: 1h
      requirements:
        - key: kubernetes.io/os
          operator: In
          values: ["linux"]
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64", "arm64"]
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["spot", "on-demand"]
        - key: karpenter.k8s.aws/instance-category
          operator: In
          values: ["c", "m", "r"]
        - key: karpenter.k8s.aws/instance-generation
          operator: Gt
          values: ["4"]
```

### Recipe 2: Spot-optimized flexible pool with business-hour protection

This recipe uses broad instance diversity and blocks underutilized consolidation during a UTC business-hour window while still allowing empty-node cleanup. Karpenter’s disruption docs explain that budgets can be scoped by reasons and schedules, and the scheduling docs recommend broad capacity choices for placement flexibility ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/), [Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)).

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: spot-flex
spec:
  weight: 100
  limits:
    cpu: "3000"
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 15m
    budgets:
      - nodes: 20%
      - nodes: "0"
        reasons: ["Underutilized"]
        schedule: "0 14 * * mon-fri"
        duration: 10h
  template:
    metadata:
      labels:
        capacity.example.com/preference: spot
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: default
      terminationGracePeriod: 45m
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["spot"]
        - key: topology.kubernetes.io/zone
          operator: In
          values: ["us-west-2a", "us-west-2b", "us-west-2c"]
        - key: karpenter.k8s.aws/instance-category
          operator: In
          values: ["c", "m", "r"]
        - key: karpenter.k8s.aws/instance-family
          operator: Exists
          minValues: 8
        - key: karpenter.k8s.aws/instance-generation
          operator: Gt
          values: ["4"]
```

### Recipe 3: On-Demand fallback pool

A lower-weight On-Demand pool can provide fallback for workloads that also match Spot. Karpenter uses the highest matching weight, so a Spot pool with higher weight is tried first when both pools match ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)).

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: on-demand-fallback
spec:
  weight: 10
  limits:
    cpu: "1000"
  disruption:
    consolidationPolicy: WhenEmpty
    consolidateAfter: 5m
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: default
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
        - key: karpenter.k8s.aws/instance-category
          operator: In
          values: ["c", "m", "r"]
```

### Recipe 4: GPU isolated pool

Use taints to reserve GPU nodes for GPU workloads and NodePool limits to cap expensive capacity. Karpenter supports GPU-related AWS labels for scheduling constraints ([Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)).

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: gpu
spec:
  limits:
    cpu: "512"
  disruption:
    consolidationPolicy: WhenEmpty
    consolidateAfter: 30m
  template:
    metadata:
      labels:
        accelerator.example.com/type: nvidia
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: gpu
      taints:
        - key: nvidia.com/gpu
          value: "true"
          effect: NoSchedule
      requirements:
        - key: kubernetes.io/arch
          operator: In
          values: ["amd64"]
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
        - key: karpenter.k8s.aws/instance-gpu-count
          operator: Gt
          values: ["0"]
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-worker
spec:
  replicas: 2
  selector:
    matchLabels:
      app: gpu-worker
  template:
    metadata:
      labels:
        app: gpu-worker
    spec:
      tolerations:
        - key: nvidia.com/gpu
          operator: Equal
          value: "true"
          effect: NoSchedule
      containers:
        - name: worker
          image: public.ecr.aws/docker/library/ubuntu:24.04
          command: ["sleep", "3600"]
          resources:
            limits:
              nvidia.com/gpu: 1
            requests:
              cpu: "2"
              memory: 8Gi
              nvidia.com/gpu: 1
```

### Recipe 5: Private EKS cluster with instance profile

Private clusters should set `isolatedVPC=true` and use `spec.instanceProfile` because IAM has no VPC endpoint for Karpenter to manage instance profiles from a role ([Karpenter Getting Started Guide](https://karpenter.sh/docs/getting-started/getting-started-with-karpenter/)).

```yaml
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata:
  name: private
spec:
  amiFamily: AL2023
  amiSelectorTerms:
    - alias: al2023@v20250620
  instanceProfile: KarpenterNodeInstanceProfile-my-private-cluster
  subnetSelectorTerms:
    - tags:
        karpenter.sh/discovery: my-private-cluster
        network: private
  securityGroupSelectorTerms:
    - tags:
        karpenter.sh/discovery: my-private-cluster
  associatePublicIPAddress: false
```

### Recipe 6: Static baseline NodePool plus dynamic burst

Use static capacity when a team needs always-on warm nodes and dynamic capacity for bursts. Static mode has special constraints documented in the NodePool reference ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)).

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: baseline
spec:
  replicas: 3
  limits:
    nodes: 6
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: default
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["on-demand"]
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: burst
spec:
  weight: 20
  limits:
    cpu: "1000"
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 5m
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: default
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["spot", "on-demand"]
```

### Recipe 7: Multi-tenant team pool

Use taints, labels, and quotas so one tenant cannot silently consume another tenant’s specialized capacity. Karpenter’s NodePool taints determine whether a Pod can use that NodePool ([Karpenter NodePools documentation](https://karpenter.sh/docs/concepts/nodepools/)).

```yaml
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: team-analytics
spec:
  limits:
    cpu: "500"
    memory: 1Ti
  template:
    metadata:
      labels:
        tenant.example.com/team: analytics
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: analytics
      taints:
        - key: tenant.example.com/team
          value: analytics
          effect: NoSchedule
      requirements:
        - key: karpenter.sh/capacity-type
          operator: In
          values: ["spot", "on-demand"]
---
apiVersion: v1
kind: ResourceQuota
metadata:
  name: analytics-compute
  namespace: analytics
spec:
  hard:
    requests.cpu: "400"
    requests.memory: 800Gi
```

### Recipe 8: Local NVMe / instance store workloads

Use instance-store labels and `instanceStorePolicy: RAID0` when workloads need high ephemeral I/O and can tolerate node loss. EC2NodeClass supports RAID0 configuration for instance stores, and scheduling labels include local NVMe capacity ([Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/), [Karpenter Scheduling documentation](https://karpenter.sh/docs/concepts/scheduling/)).

```yaml
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata:
  name: nvme
spec:
  amiFamily: AL2023
  amiSelectorTerms:
    - alias: al2023@v20250620
  role: KarpenterNodeRole-my-eks-cluster
  instanceStorePolicy: RAID0
  subnetSelectorTerms:
    - tags:
        karpenter.sh/discovery: my-eks-cluster
  securityGroupSelectorTerms:
    - tags:
        karpenter.sh/discovery: my-eks-cluster
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: nvme
spec:
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: nvme
      requirements:
        - key: karpenter.k8s.aws/instance-local-nvme
          operator: Gt
          values: ["100"]
```

### Recipe 9: AMI rollout by drift with budget

Pin a new AMI alias after staging validation, then let drift replace nodes under a limited budget. Karpenter detects drift from resolved AMI changes and uses disruption budgets for graceful automated replacement ([Karpenter Disruption documentation](https://karpenter.sh/docs/concepts/disruption/), [Karpenter EC2NodeClass documentation](https://karpenter.sh/docs/concepts/nodeclasses/)).

```yaml
apiVersion: karpenter.k8s.aws/v1
kind: EC2NodeClass
metadata:
  name: default
spec:
  amiFamily: AL2023
  amiSelectorTerms:
    - alias: al2023@v20250701
  role: KarpenterNodeRole-my-eks-cluster
  subnetSelectorTerms:
    - tags:
        karpenter.sh/discovery: my-eks-cluster
  securityGroupSelectorTerms:
    - tags:
        karpenter.sh/discovery: my-eks-cluster
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: general
spec:
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
    consolidateAfter: 10m
    budgets:
      - nodes: 5%
        reasons: ["Drifted"]
      - nodes: "0"
        reasons: ["Drifted"]
        schedule: "0 14 * * mon-fri"
        duration: 10h
  template:
    spec:
      nodeClassRef:
        group: karpenter.k8s.aws
        kind: EC2NodeClass
        name: default
```

### Recipe 10: AKS Node Auto Provisioning basics

AKS NAP is managed by AKS and enabled through Azure CLI rather than installing the AWS Helm chart; Microsoft documents that NAP uses Karpenter concepts and exposes `NodePool`, `AKSNodeClass`, and `NodeClaim` resources ([AKS node auto provisioning documentation](https://learn.microsoft.com/en-us/azure/aks/node-autoprovision)).

```bash
az aks update \
  --resource-group my-rg \
  --name my-aks \
  --node-provisioning-mode Auto

kubectl get nodepools
kubectl get aksnodeclasses
kubectl get nodeclaims
kubectl get events -A --field-selector source=karpenter -w
```

## Appendix: selected settings and metrics

### Selected controller settings

Karpenter settings are exposed as controller configuration, environment variables, or Helm values depending on deployment method. The settings reference is the authoritative source for the complete list ([Karpenter Settings reference](https://karpenter.sh/docs/reference/settings/)).

| Setting | Purpose |
|---|---|
| `CLUSTER_NAME` | Required cluster name used by the controller. |
| `INTERRUPTION_QUEUE` | SQS queue name for interruption handling. |
| `FEATURE_GATES` | Enables or disables feature gates such as Spot-to-Spot consolidation or NodeRepair when available. |
| `METRICS_PORT` | Metrics endpoint port. |
| `HEALTH_PROBE_PORT` | Health probe port. |
| `LOG_LEVEL` | Log verbosity. |
| `BATCH_IDLE_DURATION` and `BATCH_MAX_DURATION` | Provisioning batch timing controls. |
| `RESERVED_ENIS` | Number of ENIs reserved from pod-density calculations; relevant for VPC CNI custom networking and Security Groups for Pods. |
| `ISOLATED_VPC` | Configures behavior for private VPC environments without public AWS service access. |
| `MIN_VALUES_POLICY` | Controls behavior for alpha `minValues` requirements. |

### Selected metrics reference

The metrics reference is extensive; start with the following operational groups and expand dashboards as production experience grows ([Karpenter Metrics reference](https://karpenter.sh/docs/reference/metrics/)).

| Group | Example metrics | Use |
|---|---|---|
| Build/version | `karpenter_build_info` | Verify running version and release metadata. |
| Scheduling | `karpenter_scheduler_unschedulable_pods_count`, `karpenter_scheduler_queue_depth`, `karpenter_scheduler_scheduling_duration_seconds` | Detect provisioning backlog and scheduling latency. |
| NodeClaims | `karpenter_nodeclaims_created_total`, `karpenter_nodeclaims_terminated_total`, `karpenter_nodeclaims_disrupted_total` | Track lifecycle decisions and churn. |
| Nodes | `karpenter_nodes_created_total`, `karpenter_nodes_terminated_total`, `karpenter_nodes_allocatable`, `karpenter_nodes_current_lifetime_seconds` | Monitor fleet creation, termination, capacity, and age. |
| Pods | `karpenter_pods_state`, `karpenter_pods_startup_duration_seconds`, `karpenter_pods_bound_duration_seconds` | Understand workload scheduling and startup behavior. |
| NodePools | `karpenter_nodepools_usage`, `karpenter_nodepools_limit`, `karpenter_nodepools_allowed_disruptions`, `karpenter_nodepools_cost_total` | Track pool utilization, limits, disruption room, and cost signal. |
| Disruption | `karpenter_voluntary_disruption_decisions_total`, `karpenter_voluntary_disruption_eligible_nodes` | Understand consolidation and drift decision-making. |
| Cloud provider | `karpenter_cloudprovider_errors_total`, `karpenter_cloudprovider_duration_seconds` | Detect AWS/provider API errors and latency. |
| Interruption | `karpenter_interruption_received_messages_total`, `karpenter_interruption_message_queue_duration_seconds` | Verify interruption handling pipeline. |

### Final production readiness checklist

- Karpenter controller is running on non-Karpenter capacity.
- CRDs are installed and upgraded independently from the controller chart.
- At least one ready NodePool and one ready EC2NodeClass exist.
- AMI aliases are pinned to tested versions.
- NodePool requirements are broad enough for availability and Spot diversity.
- NodePool limits cap worst-case cost.
- Disruption budgets protect business hours and critical workloads.
- PDBs and graceful termination are defined for production workloads.
- Interruption handling is configured before using Spot.
- Metrics, logs, events, and cost dashboards are monitored.
- Upgrade and rollback procedures have been tested in staging.

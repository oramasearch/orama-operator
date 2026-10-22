# Manager permissions

The chart requires Kubernetes 1.30 or later. Every cluster installs the same RBAC and native
ValidatingAdmissionPolicies. The manager keeps cluster-wide reads for its controller caches,
including Secrets; Secret confidentiality against a stolen manager credential is not provided.
Secret and core workload writes are granted through the `orama-operator-workloads` ClusterRole,
bound only inside opted-in namespaces. This role is never cluster-bound. Enumerating the verbs
also removes the old ServiceAccount wildcard's [impersonation authority](https://kubernetes.io/docs/reference/access-authn-authz/authorization/).

A workload namespace opts in with `app.kubernetes.io/managed-by: orama-operator`. The dedicated
namespace chosen for the manager installation is also trusted by the chart. Install the manager
in `orama-system`, outside the reserved namespaces. The manager labels newly
created namespaces and synchronously repairs its fixed RoleBinding before local controller writes.
It cannot add this label to an existing, unrelated namespace. Namespace reads and binding checks
use the uncached client; the next write repairs a deleted binding. Admission checks the current
namespace label on every mutation, including requests made directly with a stolen token and
requests made after an administrator removes the label. Native admission observes namespace
updates through the API server's informer, so direct requests have the normal Kubernetes cache
propagation delay; the manager's uncached preflight rejects a revoked namespace immediately.
Status updates on Orama CRs remain allowed
so a missing label can be reported. Lake and Cloud v1 report `Degraded` with
`NamespaceAccessRequired` and keep existing workloads running until the declaration is applied.

The RoleBinding may grant only the fixed workload ClusterRole, to the installed manager ServiceAccount.
Admission denies broadening its subjects, changing its role, or placing it in an unlabelled or
reserved namespace. The manager cannot mutate `default` or any `kube-*` namespace, apart from the existing
Karpenter install in `kube-system` described below. It cannot change the
admission policies or their bindings. Generated `config/rbac/role.yaml` also drops workload writes;
the install chart supplies the separate workload role and the policies together.

## Upgrade order

Existing namespaces need the label before this version starts reconciling. The committed
[namespace declarations](../../config/namespace-ownership/) include the management credential
namespaces and every member in the committed Lake and Cloud v1 sets. Each file names its target
cluster in its filename and first comment. Apply each file **only to that cluster**, with Steve's
explicit consent. These target-cluster resources stay outside `config/clusters/`, whose recursive
diff against the management cluster must remain empty. Then upgrade the operator through the
existing management and leaf install paths. No namespace is renamed, recreated or deleted. Additional existing installations must add
the same namespace declaration to their committed deployment resources before upgrading.

New set members receive the label when their namespace is created. Existing namespaces preserve
their labels; leaf bootstrap never silently adopts an unrelated namespace. Dynamic workspace,
application, build, rehearsal and tunnel namespaces receive the label at creation through the same
local manager client. Workload grants are installed on first mutation and repaired on later writes.

## Karpenter and remaining authority

Karpenter remains in `kube-system` to preserve the existing autoscaler installation. The manager
can apply only the named Deployment, ServiceAccount, Service, disruption budget, Roles and
RoleBindings from the vendored bundle. Its name-scoped ClusterRole writes, bind and escalate grants
remain necessary to install the autoscaler; admission restricts every rule, aggregation label and binding subject to
the vendored Karpenter definitions. The manager cannot substitute wildcard rules, attach an
aggregation rule, or bind a Karpenter role to itself. Its Deployment must retain the pinned image,
executable, environment and pod identity, preventing replacement with a token-exfiltrating pod.
Re-vendoring requires regenerating `chart/templates/manager-admission.yaml` with
`python3 hack/generate-manager-admission.py` (PyYAML) and running the authorization envtests.
Moving the autoscaler to a dedicated namespace is a separate migration.

The manager still has `nodes/proxy: get`: volume usage from the kubelet summary feeds billing,
and removing it without a replacement would stop metering. A follow-up must replace this with
direct, certificate-verified kubelet HTTPS and `nodes/stats` on clusters with fine-grained kubelet
authorization, then remove `nodes/proxy`. This change therefore hardens the finding's workload and
RBAC blast radius but does not eliminate all authority identified in LSR-220.

The manager container uses `RuntimeDefault` seccomp and a read-only root filesystem. A bounded
256 MiB `emptyDir` at `/tmp`, writable by group 65532, supports Helm's temporary chart and kubeconfig files. These process
settings supplement API authorization; they do not narrow the ServiceAccount's permissions.

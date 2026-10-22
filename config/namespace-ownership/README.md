# Namespace ownership declarations

These files opt existing namespaces into the manager's Secret and core workload write authority
with `app.kubernetes.io/managed-by: orama-operator`. Each filename and first comment identify its
**target cluster**; apply a file only there, with Steve's explicit consent, before upgrading the
operator. Then roll the manager through the normal management and leaf install paths.

| Target cluster | Existing namespaces |
|---|---|
| `orama-production-cluster-01` | `lake`, `orama-system` |
| `orama-production-cluster-02` | `lake-shared`, `orama-system`, `v1-eu` |
| `orama-production-cluster-03` | `lake-shared-us`, `orama-system`, `v1-us` |
| `orama-nonprod-cluster-01` | `lake-dev`, `lake-staging`, `lake-testing`, `orama-system`, `v1-dev` |

These are the credential namespaces on management and the members declared in
[`config/clusters/sets`](../clusters/sets/). Additional existing installations must commit their
namespace declarations here before upgrading. Newly created namespaces receive the label from the
operator; existing, unrelated namespaces are never silently adopted.

They live outside `config/clusters/` because that directory contains resources applied to the
management cluster, whose recursive diff must stay empty. Namespace ownership is cluster
configuration; every cluster still installs identical operator RBAC and admission policies.

See [manager permissions and rollout order](../../docs/operations/manager-permissions.md).

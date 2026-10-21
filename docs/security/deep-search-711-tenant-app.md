# Deep search: a tenant's custom app as a foothold into Kubernetes

Issue: [operator#711](https://github.com/oramasearch/orama-operator/issues/711), one of the silo-breach
deep searches of the multitenancy security review
[lake#873](https://github.com/oramasearch/lake/issues/873). Reviewed on 2026-10-21 against `dev` at
`66d838f`, with read-only reads of the managing cluster `orama-production-cluster-01`.

**The question.** A tenant's app (`LakeApp`, synchronized from a row of lake's `apps` table) runs the
tenant's own code, in the tenant's workspace namespace `<environment>-ws-<id>`, next to other
tenants' apps, services and workspace Zettas and on the same nodes as Lake. What can that code reach,
and what would it take to leave its silo?

**Verdict.** As a *container*, the app is well confined: restricted Pod Security, no Kubernetes
credential, no capability, a read-only root, RuntimeDefault seccomp, a deny-by-default network with
the metadata address explicitly denied, and egress only to cluster DNS, its Lake's API and its own
workspace. Nothing found lets an app reach another workspace, lake's control Postgres, the lake Zetta,
the Kubernetes API or the cloud metadata service through the network as rendered. The real defects
are around the container, not in it:

| # | Defect | Severity | Issue |
|---|---|---|---|
| 1 | Tenant apps are served under the Lake's own host (`<app>.eu.lake.live`; `<app>.oramadev.net` on the managing cluster), so an app can set cookies for Lake and for other tenants' apps | p2 | [operator#762](https://github.com/oramasearch/orama-operator/issues/762) |
| 2 | Lake's session cookies have no `__Host-` prefix, so a subdomain can shadow them (session fixation) | p2 | [lake#910](https://github.com/oramasearch/lake/issues/910) |
| 3 | app-runtime's `current_user()` trusts client-sent identity headers on public apps, and fallback headers no proxy overwrites | p2 | [app-runtime#11](https://github.com/oramasearch/app-runtime/issues/11) |
| 4 | App pods share nodes, kernel and one UID with Lake and every other tenant: no dedicated pool, no user namespaces, no sandbox runtime | p3 | [operator#763](https://github.com/oramasearch/orama-operator/issues/763) |
| 5 | Outside-destination exceptions miss the pod and Service ranges on AKS overlay (`spec.podCIDR` is empty) | p4 | [operator#764](https://github.com/oramasearch/orama-operator/issues/764) |
| 6 | The default app-runtime image, which runs every runtime-mode app, is a mutable tag | p4 | [operator#765](https://github.com/oramasearch/orama-operator/issues/765) |

Fixed in the same pull request as this report: an app's egress to its Lake is limited to the Lake's
API port (it allowed every port of the Lake pod), and `lakeapp_isolation_test.go` pins every isolation
rule this review relies on (below).

## How it was checked

- **Code read** at `dev` `66d838f`: `internal/controller/lakeapp_controller.go`, `lake_apps.go`,
  `lake_workspaces.go`, `lake_network_sandbox.go`, `lake_workspace_datastore_policies.go`,
  `lake_workspace_service_policies.go`, `network_sandbox_profiles.go`, `egress_rules.go`,
  `networksandbox_controller.go`, `lake_defaults.go`; lake `db/schema.sql` (the `apps` table),
  `src/api/web_app_capabilities.cpp`, `src/api/http_server.cpp` (forward auth, cookies, cross-site
  check); app-runtime `src/orama_app/user.py`. Line numbers below are at this branch.
- **Tests written**: `internal/controller/lakeapp_isolation_test.go` (six tests, below). The egress
  test fails on `dev` without the port fix (`image: egress to the Lake is not its API port only: []`).
- **Live, read-only** on `orama-production-cluster-01` (`kubectl get`/`describe` only): namespaces and
  their Pod Security labels, the three workspace namespaces' NetworkPolicies, `deny-cloud-metadata`
  CiliumNetworkPolicies, `NetworkSandbox` status, ResourceQuota and LimitRange, the default
  ServiceAccount, both live app pods' spec and image digest, the app access policy as rendered, nodes,
  RuntimeClasses, kube-system DaemonSets (the CNI), Ingress hosts, and the `Lake/prod` apps spec.
- **Not read**: `orama-nonprod-cluster-01`. Reading it needs its kubeconfig taken out of
  `orama-system/orama-nonprod-cluster-01-kubeconfig` on the managing cluster, which this session's
  permissions refused. The same code renders it; the reads below are to repeat there.
- **Not run**: the live probe. It is a write to a cluster and needs Steve's consent; it is written out
  ready to run in [The live probe](#the-live-probe-needs-steves-consent).

## 1. Network: what the app pod can reach

A synchronized app's pod is selected by these policies in its workspace namespace (NetworkPolicy is
a union of allows; the namespace has a default deny):

- `default-deny` and the mandatory `deny-cloud-metadata` CiliumNetworkPolicy of the workspace
  `NetworkSandbox` (`networksandbox_controller.go:33`, `:274`), endpoint selector `{}`;
- `allow-dns-egress` (`lake_workspaces.go:1158`), `allow-lake-egress` to the Lake pod on its port
  (`lake_workspaces.go:1182`), `allow-lake-ingress` from the Lake and the operator on 4800/8080
  (`lake_workspaces.go:1095`);
- `allow-workspace-internal` (every pod to every pod of the same namespace) until the workspace's
  connections are active (`lake_workspaces.go:842`); then only declared `Connection` paths;
- `allow-internet-https` and `EgressRule` allowances if the tenant turned them on;
- the app's own access policy `app-<id>-access` (`lakeapp_controller.go:427`): ingress from
  ingress-nginx and the owning Lake on the app port (`:465`), egress to cluster DNS and the Lake
  (`:489`, `:546`).

| Destination | Result | Evidence |
|---|---|---|
| Another workspace's Zetta, services, apps | **Pass** | No policy in a workspace names another namespace: the app rules name only `kube-system` (DNS), the Lake namespace (Lake pod) and the app's own workspace (`lakeapp_controller.go:489-556`); workspace-wide rules use `podSelector`s of the same namespace (`lake_workspaces.go:1082-1093`). Pinned by `TestATenantAppEgressReachesOnlyDNSItsLakeAPIAndItsOwnWorkspace`. Peers and connections that do cross workspaces are explicit, Lake-declared paths (`docs/infrastructure/connections.md`, `peer-connections.md`), outside this vector. |
| Lake's control Postgres | **Pass** | Only the Lake pod and the schema Job may reach it (`lakeInternalIngressAllowances`, `lake_network_sandbox.go:383-410`; live `allow-lake-clients-to-controldb`, `allow-schema-to-controldb`). The app's Lake rule selects `app.kubernetes.io/name=lake` only. |
| The lake Zetta (the bus) | **Pass** | Ingress to `lake-<lake>-zetta` admits only the Lake pod and the operator (`lake_network_sandbox.go:142-152`, `:397`; live `allow-lake-ingress-to-zetta`, `allow-operator-to-zetta`). The legacy events rule to it applies only to standalone apps an administrator writes, never to synchronized ones (`lakeapp_controller.go:535-540`). |
| Lake's own pod | **Pass, tightened** | Reachable by design for source, reads and events. Lake admits workspace namespaces only on its API port (`lake_network_sandbox.go:191`, live `allow-workspace-ingress`), but the app's own rule allowed every port; it now names the Lake container port only (`lakeapp_controller.go:546`). The Lake pod's other ports, if any are added, are no longer reachable from apps. |
| Library agents, builds, methodology pods in the Lake namespace | **Pass** | The app's only Lake-namespace peer is the Lake pod. |
| The Kubernetes API | **Pass** | The pod has no token (§2). No egress names the API: the workspace API allowances select CNPG, Strimzi and TiDB pods by labels only the operator sets (`lake_workspace_datastore_policies.go:186-212`), and `TestNoWorkspaceAPIServerOrOperatorAllowanceSelectsATenantApp` pins that none selects an app. The internet allowance excepts the API Service and endpoint addresses (`network_sandbox_profiles.go:65-81`). |
| Cloud metadata `169.254.169.254`, `168.63.129.16`, `fd00:ec2::254` | **Pass** | Denied by the mandatory `deny-cloud-metadata` `egressDeny`, which overrides every allow (`networksandbox_controller.go:274`; `docs/infrastructure/network-sandbox.md` *Mandatory cloud metadata deny*). Live: present and `VALID True` in all three workspace namespaces. Every outside rule also excepts `169.254.0.0/16` (`lake_workspaces.go:75`, `egress_rules.go:41`). |
| Node addresses and node ports (kubelet 10250, NodePorts) | **Pass by code, unknown live** | No allow names a node; the internet and CIDR rules except every node address (`network_sandbox_profiles.go:86-95`). Whether AKS-managed Cilium enforces `ipBlock.except` for node addresses is not proven: the 2026-10-14 proof found it did *not* for the metadata address. The probe checks it. |
| Other pods by IP via a tenant CIDR egress rule | **Pass on Cilium, fragile** | Node `spec.podCIDR` is empty on AKS overlay (live), so pod and Service ranges are never excepted; safe only because Cilium CIDR rules do not select cluster endpoints. Filed as [operator#764](https://github.com/oramasearch/orama-operator/issues/764). |
| The public internet | **Tenant's choice** | Closed unless the workspace turns on `internetEgress` or an `EgressRule` (port 443 to public addresses; `lake_workspaces.go:974-1021`). Live: `prod-ws-6bb777ba498a` has `allow-internet-https`. |
| The cluster enforces the policies | **Pass on the managing cluster** | The CNI is Cilium (`kube-system/cilium` DaemonSet, Azure CNI powered by Cilium), the app pods have Cilium endpoints in state `ready`, and the workspace `NetworkSandbox`es report `Ready=True`. Enforcement on every cluster is [operator#699](https://github.com/oramasearch/orama-operator/issues/699) (PR [#717](https://github.com/oramasearch/orama-operator/pull/717)). |

**DNS** (*can an app resolve and reach `*.svc.cluster.local` names of other tenants?*): it can
**resolve** them, since cluster DNS answers every name for any pod, which discloses namespace names
(a workspace ID prefix) and service names. It cannot **reach** them (above). Accepted: the names carry
no data, and per-namespace DNS views are not something CoreDNS offers without a policy plugin.

## 2. Pod security

| Check | Result | Evidence |
|---|---|---|
| `restricted` Pod Security on workspace namespaces | **Pass** | `lake_workspaces.go:674` (enforce, audit and warn `restricted`, `latest`); standalone app namespaces `lakeapp_controller.go:314-326`. Live: all three `prod-ws-*` namespaces `enforce=restricted`. |
| `automountServiceAccountToken: false` | **Pass** | Pod `lakeapp_controller.go:1357`; the workspace `default` ServiceAccount too (`lake_workspaces.go:805`). Live: both app pods `false`, the SA `false`. |
| No privileged, no capabilities, no escalation | **Pass** | `lakeapp_controller.go:1292-1297`: `allowPrivilegeEscalation: false`, `capabilities.drop: [ALL]`, nothing added. Live: as rendered. |
| Read-only root | **Pass** | `lakeapp_controller.go:1295`, with a 1 GiB `emptyDir` at `/tmp`. Live: as rendered. |
| seccomp | **Pass** | `RuntimeDefault`, `lakeapp_controller.go:1364`. Live: as rendered. |
| Non-root | **Pass** | `runAsNonRoot`, UID/GID 10001 (`lakeapp_controller.go:58`, `:1359-1363`). The same UID for every tenant is part of [operator#763](https://github.com/oramasearch/orama-operator/issues/763). |
| No hostPath, host namespaces or host ports | **Pass** | Volumes are only `emptyDir`, the app's Secret projections and its workspace's claims (`lakeapp_controller.go:1309-1355`); no `hostNetwork`/`hostPID`/`hostIPC`. Live: one `emptyDir` volume. |
| Service links | **Pass** | `enableServiceLinks: false` (`:1358`): the namespace's Services are not injected as environment. |
| Resource limits | **Pass** | Requests 100m/128Mi, limits 1 CPU/1 GiB for every synchronized app (`lakeapp_controller.go:1191-1205`, set by `lakeAppRowSpec`, `lake_apps.go:373-374`). Live: as rendered. |
| Quotas | **Pass** | Workspace `ResourceQuota` and `LimitRange` `workspace-defaults` (`lake_workspaces.go:773-794`). Live: present in all three workspaces. |
| Containers beyond the app | **Pass** | One container, no init or ephemeral containers. |

All of this is pinned by `TestATenantAppPodHoldsNoKubernetesCredentialAndMeetsRestrictedPodSecurity`
for every app shape (runtime, image, bindings, a volume and a bucket, stopped).

## 3. Node isolation

| Check | Result | Evidence |
|---|---|---|
| Sandbox runtime (gVisor, Kata) | **Fail (known, [lake#725](https://github.com/oramasearch/lake/issues/725))** | No `runtimeClassName` (`lakeAppPodSpec`). Live: no pod in the cluster sets one, although a `kata-vm-isolation` RuntimeClass exists. Research: `docs/research/secure-runtimes/`. |
| Separation from Lake on the node | **Fail** | No node selector or toleration. Live: `lake-prod` and both app pods of `prod-ws-6bb777ba498a` ran on `aks-agentpool-33345110-vmss000005`. |
| What an escape exposes | **Recorded** | Root on the node gives Lake's environment and memory (Stripe and model-provider keys, instance key material), every Secret mounted by any pod there, other tenants' attached volumes, the kubelet's credentials and the node identity through IMDS. |
| User namespaces | **Fail** | `hostUsers` unset: UID 10001 is the same host UID for every tenant. |

Filed together as [operator#763](https://github.com/oramasearch/orama-operator/issues/763)
(dedicated tainted pool for tenant code, `hostUsers: false`, then a sandbox runtime).

## 4. Ingress

| Check | Result | Evidence |
|---|---|---|
| Cookies across apps and into Lake | **Fail** | The apps wildcard defaults to the Lake host (`lake_defaults.go:113`); no LakeSet sets another, and the managing cluster's `Lake/prod` uses `oramadev.net`, the whole nonprod zone (live). An app can set `Domain=<lake host>` cookies, shadowing `lake_session` (no `__Host-` prefix, lake `src/api/http_server.cpp:2320`) and other apps' `lake_app_session`. [operator#762](https://github.com/oramasearch/orama-operator/issues/762), [lake#910](https://github.com/oramasearch/lake/issues/910). Lake's cross-site check (`Sec-Fetch-Site: same-origin`, lake `http_server.cpp:13600`) stops cross-origin *requests* from an app page, so this is a cookie channel, not CSRF. |
| Lake reads an app's cookies | **Pass** | `lake_session` and `lake_app_session` are host-only, so a browser does not send Lake's session to an app host, and `lake_app_session` is bound to its app and host when verified (lake `http_server.cpp:9292-9300`). |
| Forged `X-Orama-User` on a Lake-protected app | **Pass** | The Ingress names `X-Orama-User,X-Orama-Space` as `auth-response-headers` (`lakeAppIngressAnnotations`, `lakeapp_controller.go:1537`), so ingress-nginx sets both from Lake's answer and replaces the client's; only ingress-nginx and the Lake can reach the pod (`lakeapp_controller.go:465-487`). Pinned by `TestALakeProtectedAppIngressReplacesTheClientsIdentityHeaders` and `TestATenantAppAdmitsOnlyIngressNginxAndItsLakeOnItsPort`. |
| Forged identity on a public app | **Fail** | No forward auth on a public app, and app-runtime's `current_user()` reads `X-Orama-User` and fallbacks (`x-orama-user-id`, `x-auth-request-*`) from the request. [app-runtime#11](https://github.com/oramasearch/app-runtime/issues/11). |
| Custom domains a tenant claims | **Pass (none possible)** | A synchronized app's host must be exactly one label beneath the apps wildcard (`lake_apps.go:358-364`); lake keeps hosts unique per Lake (`apps_host_idx`, lake `db/schema.sql:1498`) and refuses reserved names (lake `web_app_capabilities.cpp:135`). On `Lake/prod`'s `oramadev.net` wildcard the reserved list does not cover Cloud v1 names such as `app-dev`, which is part of [operator#762](https://github.com/oramasearch/orama-operator/issues/762). Domains as a whole are [lake#888](https://github.com/oramasearch/lake/issues/888). |
| DNS records | **Pass** | Wildcard hosts get no record of their own; a custom (standalone) host gets one proxied record owned by its LakeApp (`lakeapp_controller.go:1514-1530`). |

## 5. Secrets mounted into the app

| Check | Result | Evidence |
|---|---|---|
| Only that app's projections | **Pass** | The pod references only Secrets the operator names for that app: tokens, bindings, environment copies, store credentials, pull (`lakeAppPodSpec`; `lakeAppSecretNameIsManaged`, `lakeapp_controller.go:1062`). Copies carry the app's owner annotation and refuse a Secret owned by another app (`copyLakeAppSecret`). Live: the app pod's environment is `LAKE_URL`, `APP_*`, `WORKSPACE_ID`, `EVENTS_URL`, `EVENTS_TOKEN`, `APP_TOKEN`, with no `envFrom`. |
| Arbitrary Lake-namespace Secrets through `secret_env_refs` | **Pass through lake; operator backstop pending** | Lake's API accepts only `lake-secret://` references (lake `web_app_capabilities.cpp:266-345`). On `dev` the operator still honours a raw Secret name in a row (`lake_apps.go:521`, `lakeAppSourceSecret` reads the Lake namespace, `lakeapp_controller.go:1070`): a row written outside the API could copy any Lake-namespace Secret into a tenant's pod. [operator#492](https://github.com/oramasearch/orama-operator/issues/492) is closed with PR [#709](https://github.com/oramasearch/orama-operator/pull/709), still open; until it merges the backstop is not on `dev`. The operator's trust in rows as a whole is [operator#713](https://github.com/oramasearch/orama-operator/issues/713). |

## 6. Image pinning and admission

| Check | Result | Evidence |
|---|---|---|
| Tenant images | **Pass** | Image mode requires `@sha256:<64 hex>` (`lake_apps.go:411-415`). |
| The runtime image | **Fail (p4)** | `oramasearch/app-runtime:976e6a8`, a tag (`lake_defaults.go:28`); live digest `sha256:ee7222fb…46b3`. [operator#765](https://github.com/oramasearch/orama-operator/issues/765). |
| Admission | **Pass** | Pod Security admission `restricted` (above). Gatekeeper pods also run on the managing cluster (`gatekeeper-system`); its constraints were not reviewed. |

## Overlaps with the other deep searches

- [operator#712](https://github.com/oramasearch/orama-operator/issues/712) (builds): the build
  namespace `lake-prod-builds-*` enforces `privileged` Pod Security (live), and the app-runtime image
  of [operator#765](https://github.com/oramasearch/orama-operator/issues/765) is also the build base.
  A dedicated tenant node pool ([operator#763](https://github.com/oramasearch/orama-operator/issues/763))
  should take build pods too. Not reviewed here.
- [operator#713](https://github.com/oramasearch/orama-operator/issues/713): the `secret_env_refs`
  backstop above.
- [lake#885](https://github.com/oramasearch/lake/issues/885) and
  [lake#888](https://github.com/oramasearch/lake/issues/888): the cookie and domain findings.
- Ingress is moving from ingress-nginx to zetta-tunnel's gateway (`docs/infrastructure/ingress.md`);
  the forward-auth header replacement this review relies on has to hold there too.

## Regression tests

`internal/controller/lakeapp_isolation_test.go`, one test per rule:

| Test | Rule |
|---|---|
| `TestATenantAppPodHoldsNoKubernetesCredentialAndMeetsRestrictedPodSecurity` | §2, every app shape |
| `TestATenantAppEgressReachesOnlyDNSItsLakeAPIAndItsOwnWorkspace` | §1: no address block, only DNS, the Lake pod on its API port, the app's own workspace |
| `TestATenantAppAdmitsOnlyIngressNginxAndItsLakeOnItsPort` | §1 and §4: who may connect to an app |
| `TestNoWorkspaceAPIServerOrOperatorAllowanceSelectsATenantApp` | §1: no workspace-wide API or operator allowance selects an app |
| `TestALakeProtectedAppIngressReplacesTheClientsIdentityHeaders` | §4: forward-auth identity headers |
| `TestATenantAppPolicySelectsOnlyThatApp` | one app's policy never widens another's |

Existing tests already pin the rest: workspace namespace Pod Security and the default ServiceAccount
(`lake_workspaces_test.go`), the public-internet exceptions including `169.254.0.0/16`
(`lake_workspaces_test.go`, `network_sandbox_profiles_test.go`), CIDR rules refusing the metadata
range (`egressrule_controller_test.go`), and the metadata deny (`networksandbox_metadata_test.go`).

## Live reads to repeat on `orama-nonprod-cluster-01`

Read-only, with the leaf's kubeconfig (`lake-devops` skill):

```sh
kubectl get ns --show-labels | grep -- '-ws-'                      # enforce=restricted
kubectl get lakeapps -A
for ns in $(kubectl get ns -o name | grep -- '-ws-' | cut -d/ -f2); do
  kubectl -n "$ns" get networkpolicy,cnp,networksandbox,resourcequota,limitrange
  kubectl -n "$ns" get sa default -o jsonpath='{.automountServiceAccountToken}{"\n"}'
  kubectl -n "$ns" get pods -l app.kubernetes.io/name=lake-app \
    -o jsonpath='{range .items[*]}{.metadata.name} {.spec.automountServiceAccountToken} {.spec.securityContext} {.spec.containers[0].securityContext} {.spec.nodeName}{"\n"}{end}'
done
kubectl -n kube-system get ds cilium
kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name} {.spec.podCIDR}{"\n"}{end}'
```

## The live probe (needs Steve's consent)

**Not run.** It creates a workload on `lake-testing` and so is a change to a cluster. It needs Steve's
explicit yes for this action. Two ways, the first preferred:

**A. Through lake (no hand-made object):** deploy an image-mode app on `lake-testing` through lake's
API, with the image below pushed to the Lake's registry and its digest pinned. The operator then
renders exactly the policies a tenant gets. Read its log with `app_logs`.

**B. By hand (only if Steve chooses it):** the pod below in one `lake-testing` workspace namespace,
carrying the labels of an existing app in that namespace so the app access policy selects it, then
deleted.

The probe image is any small image with `sh`, `nc`, `nslookup` and `wget` (for example
`busybox:1.36` pinned by digest), running this script:

```sh
#!/bin/sh
# Probe what a tenant app can reach. Each line prints OPEN or closed; nothing is written anywhere.
# Fill in from read-only kubectl reads first:
#   OTHER_WS     another lake-testing workspace namespace
#   LAKE_NS      the Lake namespace (lake)          LAKE         lake-testing
#   NODE_IP      this pod's node InternalIP         API_IP       the kubernetes endpoint address
t() { if nc -z -w 3 "$1" "$2" 2>/dev/null; then echo "OPEN   $1:$2 $3"; else echo "closed $1:$2 $3"; fi; }
echo "token: $(ls /var/run/secrets/kubernetes.io/serviceaccount 2>&1)"
grep -E '^(Cap(Prm|Eff|Bnd)|NoNewPrivs|Seccomp):' /proc/self/status
id; touch /probe 2>&1 | head -1
nslookup "${OTHER_WS}-zetta.${OTHER_WS}.svc.cluster.local" || true        # resolves (accepted)
t "${OTHER_WS}-zetta.${OTHER_WS}.svc.cluster.local" 4800 "other workspace Zetta"
t "lake-${LAKE}-controldb-rw.${LAKE_NS}.svc.cluster.local" 5432 "lake control Postgres"
t "lake-${LAKE}-zetta-zetta.${LAKE_NS}.svc.cluster.local" 4800 "lake Zetta (bus)"
t "lake-${LAKE}.${LAKE_NS}.svc.cluster.local" 80 "Lake API (expected OPEN)"
t kubernetes.default.svc 443 "Kubernetes API Service"
t "${API_IP}" 443 "Kubernetes API endpoint"
t 169.254.169.254 80 "cloud metadata"
t 168.63.129.16 80 "Azure host service"
t "${NODE_IP}" 10250 "kubelet"
t "${NODE_IP}" 22 "node ssh"
t "${NODE_IP}" 30000 "a NodePort"
t 1.1.1.1 443 "public internet (OPEN only with internetEgress)"
wget -q -T 3 -O- http://169.254.169.254/metadata/instance?api-version=2021-02-01 --header Metadata:true && echo "IMDS ANSWERED"
```

The manifest for way B (the labels are copied from a real app's pod, read-only, before applying):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: probe-711
  namespace: <lake-testing workspace namespace>
  labels: { <the four labels of an existing app pod in that namespace> }
spec:
  automountServiceAccountToken: false
  enableServiceLinks: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 10001
    runAsGroup: 10001
    seccompProfile: { type: RuntimeDefault }
  restartPolicy: Never
  containers:
  - name: probe
    image: busybox:1.36@sha256:<digest>
    command: ["sh", "-c", "<the script above>"]
    env: [{name: OTHER_WS, value: "..."}, {name: LAKE_NS, value: lake}, {name: LAKE, value: testing},
          {name: NODE_IP, valueFrom: {fieldRef: {fieldPath: status.hostIP}}}, {name: API_IP, value: "..."}]
    securityContext:
      allowPrivilegeEscalation: false
      readOnlyRootFilesystem: true
      capabilities: { drop: [ALL] }
    resources: { limits: { cpu: 100m, memory: 64Mi } }
```

Expected: no token; `CapEff` all zero; `NoNewPrivs 1`; `Seccomp 2`; `/probe` refused; every line
`closed` except the Lake API (and the public address only where `internetEgress` is on). Any other
`OPEN` line, or `IMDS ANSWERED`, is a defect to file against this report.

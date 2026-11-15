# Network sandboxes

`NetworkSandbox` is the namespace network boundary implemented for
[#465](https://github.com/oramasearch/orama-operator/issues/465), narrowed for Cloud v1 engine
egress in [#466](https://github.com/oramasearch/orama-operator/issues/466), and protected from
tenant-authored allowances in [#467](https://github.com/oramasearch/orama-operator/issues/467).
The operator creates one named
`network-sandbox` in every Lake instance namespace, every Lake workspace namespace and every
`OramaCloudV1` stack namespace. Its controller always renders `NetworkPolicy/default-deny` for
both ingress and egress and `CiliumNetworkPolicy/deny-cloud-metadata`, then renders the named
allowances in the resource.

Every sandbox has a required `spec.podSelector`. Default-deny selects only these owner pods;
each allowance's selector is intersected with it, never substituted for it. Conflicting label
requirements select no pod. Cilium endpoint selectors are constrained the same way, using the
`k8s:` label source; node selectors are refused. Empty `{}` is intentional only for workspace
namespaces, which are entirely owned by the workspace. A stack in a shared namespace such as
`orama-system` must not isolate the Orama operator or unrelated pods.

Cloud v1 uses `app.kubernetes.io/instance=<CR name>` and
`app.kubernetes.io/managed-by=orama-cloud-v1-operator`. Existing application, engine, datastore
and demo-seed pod templates already carry these labels. The control CNPG Cluster now explicitly
passes `labelsFor(controldb)` through `spec.inheritedMetadata.labels`, covering database pods and
bootstrap Jobs. PostgresSchema Jobs now inherit the instance label from their parent resource.

Lake uses a dedicated `orama.cloud/lake-instance=<Lake name>` label on its Deployment pod template,
owned Zetta pods, control CNPG instances/bootstrap Jobs, library agents and schema Jobs. This label
is propagated through the Zetta CR and CNPG inherited metadata. Existing immutable workload
selectors are unchanged. Database and schema pods do not inherit Lake's application Service
selector, so application requests cannot be sent to them. The stage-1 ownership migration leaves
workspace workload labels and pre-existing policy specs unchanged. Stage 2 deliberately retires
only `allow-workspace-internal` after the guarded Connection rollout; the additional client-ingress
policies approved during review are listed below.

An existing immutable PostgresSchema Job missing its scope labels is replaced with foreground deletion
before the correctly labelled Job is created. The existing guarded, idempotent schema script is
retained; no two Postgres migration Jobs overlap. Stable pod templates do not roll repeatedly.

Zetta's schema Job keeps its legacy ownership and replacement behavior: only a schema-version
change replaces it, including an existing unowned stale Job, with the original deletion options.
New Job templates carry the selection labels; missing labels do not trigger replacement.

The parent reconciler creates the sandbox and renders its policies synchronously before it creates
or updates a workload. The `NetworkSandbox` controller then owns ongoing repair, adoption and
pruning. A deleted sandbox requeues its parent; a deleted rendered policy requeues the sandbox.
Existing unowned policies with a declared name are adopted, but a policy owned by another resource
is refused. Status changes only when the observed generation, counts or Ready condition changes.

Every allowance has a required `comment`, copied to the rendered policy's
`orama.cloud/network-policy-comment` annotation. An empty comment is rejected by the CRD. No
allowance has an empty ingress `from` or egress `to`, which would mean every peer. Ordinary
allowances use Kubernetes NetworkPolicy; `spec.ciliumPolicies` can declare additional paths that
NetworkPolicy cannot represent. The mandatory metadata deny is generated independently of that
field and cannot be removed or overridden by a declaration.

## Policy authorship and tenant RBAC

Only the operator manager may create, update, patch or delete `NetworkPolicy`,
`CiliumNetworkPolicy` and `NetworkSandbox`. Lake, apps, agents, builds and workspace services do not
receive those grants. The workspace default ServiceAccount is tokenless and has no RoleBinding;
app, agent, build, operator-owned wrapper-service and methodology pod templates also disable token
mounting. The pinned k0s tenant Role grants ordinary workload objects but omits every network-policy
and secure-networking resource.

`TestOnlyOperatorRBACCanWriteNetworkPolicy` scans every checked-in Role, ClusterRole, RoleBinding and
ClusterRoleBinding manifest, including the rendered chart source, generated RBAC, the operator-owned
Karpenter bundle and pinned tenant fixture. Wildcard API groups, resources and verbs count as grants.
The only accepted writers are the generated manager ClusterRole and its chart equivalent; the chart
binding must name only the operator ServiceAccount. Bindings to the built-in `cluster-admin`, `admin`
or `edit` roles are rejected.

The native admission envtest installs the chart RBAC and the exact k0s tenant Role, enables RBAC
authorization, impersonates a workspace default ServiceAccount and submits an ingress-and-egress
allow-all `NetworkPolicy`. The API server must return `Forbidden`. Subject access reviews also prove
that the workspace ServiceAccount and tenant group cannot create, update, patch or delete Kubernetes
policies, Cilium policies or `NetworkSandbox` resources. This is an authorization proof; the separate
policy-rendering tests cover dataplane intent.

## Mandatory cloud metadata deny

The live #466 proof on `example-management` on **2026-10-14** found that a pod carrying
the reader's labels reached `http://169.254.169.254/` and received HTTP **400**, even though
`allow-engine-public-internet` excluded `169.254.0.0/16` and no other policy allowed the address.
On AKS-managed Cilium, NetworkPolicy `ipBlock` exclusions alone did not police this metadata
traffic. An explicit CiliumNetworkPolicy `egressDeny` with `toCIDR: [169.254.169.254/32]` blocked
the same probe, while public HTTPS still worked. This observation was supplied by the live
operator proof; it was not a cluster mutation performed by this change.

Every sandbox profile now gets one mandatory namespaced `deny-cloud-metadata` CiliumNetworkPolicy:

```yaml
spec:
  endpointSelector: # sandbox spec.podSelector, with Kubernetes label-source prefixes
    matchLabels:
      k8s:app.kubernetes.io/instance: v1
      k8s:app.kubernetes.io/managed-by: orama-cloud-v1-operator
  egressDeny:
    - toCIDR:
        - 169.254.169.254/32
        - 168.63.129.16/32
        - fd00:ec2::254/128
```

These are the cloud metadata address, Azure's host-service address and the IPv6 metadata
counterpart. The deny covers all ports and protocols. It takes precedence over every allow,
including another Kubernetes or Cilium policy; see [Cilium deny policy semantics](https://docs.cilium.io/en/stable/security/policy/deny/).
It neither grants public access nor changes the existing DNS, internal-service or public-internet
allowances. The existing `ipBlock` exclusions remain in place for all other restricted ranges.

The selector is exactly the sandbox owner's scope, including match expressions and the `k8s:`
label source. Shared Lake and Cloud v1 namespaces exclude unrelated pods and the operator.
A workspace uses `{}` because it owns its entire namespace; the namespaced Cilium policy cannot
select another workspace. There are no cluster-specific flags or opt-outs. Cilium v2 policy API
and dataplane support are required: the controller watches CiliumNetworkPolicies unconditionally,
and a render failure reports `Ready=False` rather than skipping this protection. Parent reconcilers
do not advance workload reconciliation when policy rendering fails.

Rendering installs default-deny, then the mandatory Cilium deny, before any declared allowances.
`deny-cloud-metadata` is a reserved Cilium policy name. The controller adopts an unowned policy of
that name, rejects foreign ownership, repairs selector/rule/metadata drift and recreates deleted
policies through its owner watch. Each policy has a controller owner reference to the sandbox,
so Kubernetes garbage collection deletes it with the sandbox. Terminating sandboxes do not
recreate policies. `status.ciliumNetworkPolicies` includes this baseline (one for each current
profile); unchanged policy and status objects are not rewritten.

Native tests cover all three profiles, exact deny CIDRs and owner selectors, reserved-name and
foreign-owner rejection, creation, updates, deletion recovery, deletion ownership, stable status
and failure before allowances when the Cilium API is absent. The opt-in envtest covers actual
API persistence and owner-watch-driven drift/deletion repair. Its minimal external Cilium CRD
fixture is also installed by the workspace envtests; it is not a dataplane test or upstream schema
validation. Neither fake clients nor envtest run Kubernetes garbage collection, so deletion tests
check the exact owner reference and that terminating/deleted sandboxes do not recreate policies.

## Address discovery and recovery

Kubernetes API rules include both the `default/kubernetes` Service addresses and all ready and
not-ready endpoint addresses. Public rules start with IPv4 and IPv6 internet CIDRs, then exclude
private, loopback, link-local, metadata, multicast and reserved ranges plus the API Service,
API endpoints, every node address and every node pod CIDR.

The exclusions are applied to the resolved destination address by the cluster dataplane. A public
name that resolves to a private, node or pod address is therefore refused even though OramaCore's
JavaScript runtime accepted the original host name. The mandatory Cilium deny above separately
blocks the three metadata/host-service addresses, where `ipBlock` alone is insufficient on AKS.

Those values come from live cluster discovery. On a transient discovery failure an existing
sandbox retains its last successfully discovered API and public policies and the reconcile returns
an error for retry. A new namespace still receives default-deny and all static narrow rules; it
does not receive a guessed API or public route. Turning a workspace's `internetEgress` off removes
its public rule deliberately.

## Lake workspace namespace

The ownership migration preserves every pre-existing policy name and spec byte-for-byte. A
regression test still compares every original name and `NetworkPolicySpec` against the former
constructors. Review additionally authorized six separate Lake-only client-ingress policies to
pair with the Lake egress rules. Independent tests check their complete selectors, sources and
ports, reject other Lakes, operator and unlabelled source pods, and check stable reconciliation.

| Policy | Allowed path and reason |
|---|---|
| `allow-workspace-internal` | Compatibility only before Lake's `network_guard` exists and while every seeded initial `Connection` becomes Ready. It is then retired permanently; declared paths are documented in [`connections.md`](connections.md). |
| `allow-lake-ingress` | The owning Lake and operator reach workspace Zetta and agent HTTP ports 4800 and 8080. |
| `allow-lake-service-postgres-ingress` | Only the owning Lake in its exact namespace reaches CNPG-labelled Postgres pods on 5432 for Data / Connections and connection checks. |
| `allow-lake-service-kafka-ingress` | Only the owning Lake reaches Strimzi Kafka client pods on 9092 using its projected service credentials. |
| `allow-lake-service-clickhouse-ingress` | Only the owning Lake reaches ClickHouse client pods on 8123/9000 for HTTP/native connections. Keeper is not selected. |
| `allow-lake-service-tidb-ingress` | Only the owning Lake reaches TiDB SQL pods on 4000. PD, TiKV and management ports are not selected. |
| `allow-lake-service-cassandra-ingress` | Only the owning Lake reaches Cassandra-labelled pods on CQL 9042 for service connections. |
| `allow-lake-service-valkey-ingress` | Only the owning Lake reaches Valkey-operator cluster pods on 6379 for service connections. |
| `allow-dns-egress` | Workspace pods reach CoreDNS in `kube-system` over UDP/TCP 53. |
| `allow-lake-egress` | Workspace pods reach only the owning Lake pods on the configured Lake port. |
| `allow-cnpg-operator-ingress` | CloudNativePG reaches instance-manager 8000 and Postgres 5432. |
| `allow-operator-to-postgres` | The Orama operator reaches CloudNativePG instances on 5432 to keep the roles of credential-scoped Connections (#469). |
| `allow-orama-operator-tidb-sql` | The Orama operator reaches TiDB SQL 4000 as root to keep the tenant user (#771). |
| `allow-orama-operator-cassandra-cql` | The Orama operator reaches Cassandra CQL 9042 as the superuser to keep the tenant role (#771). |
| `allow-clickhouse-operator-ingress` | Altinity reaches ClickHouse HTTP 8123 for readiness and schema work. |
| `allow-clickhouse-keeper-operator-ingress` | Altinity reaches Keeper 2181 to create replicated paths. |
| `allow-tidb-pd-operator-ingress` | TiDB operator reaches PD 2379 for members, health and stores. |
| `allow-tidb-tikv-operator-ingress` | TiDB operator reaches TiKV status 20180 during upgrades. |
| `allow-tidb-operator-ingress` | TiDB operator reaches SQL 4000 and status 10080 for password setup and health. |
| `allow-cassandra-operator-ingress` | K8ssandra reaches Cassandra management 8080 to start, drain and manage nodes. |
| `allow-valkey-operator-ingress` | Valkey operator reaches 6379 to form and observe the cluster. |
| `allow-cnpg-apiserver-egress` | CNPG instances and bootstrap jobs reach only the discovered Kubernetes API addresses and ports. |
| `allow-strimzi-apiserver-egress` | Strimzi entity operators reach only the API for topics, users and credential Secrets. |
| `allow-tidb-discovery-apiserver-egress` | TiDB discovery reaches only the API to read its `TidbCluster`. |
| `allow-internet-https` | Only when the workspace opts in, all workspace pods reach public TCP 443 with the cluster/private exclusions above. Rendered by the workspace's `EgressRule/internet-https` (#470). |
| `egress-<id>` (and, for a DNS name, `egress-<id>-gateway-ingress`, `egress-<id>-gateway-egress`) | A Lake-declared `EgressRule`: a CIDR rule's source reaches its range; a name rule's source reaches only the workspace egress gateway, which alone reaches public addresses on the rule's ports ([egress rules](egress-rules.md), #470). |

App-specific access policies remain owned by `LakeApp` and select only that app's identity labels.
Their ingress-nginx, DNS and owning-Lake rules remain. Binding-derived egress is kept during rollout
and removed after declared connections activate. One additional ingress rule
admits only the owning Lake pod in its exact namespace on `LakeApp.spec.port`, for backend
proxying. It applies to synchronized and standalone apps, follows port changes, carries a
component comment, and is tested against the matching Lake egress rule. It grants no new
operator ingress or access to other apps.

## Lake instance namespace

| Policy | Allowed path and reason |
|---|---|
| `allow-dns-egress` | Only Lake-owned workloads reach CoreDNS over UDP/TCP 53. |
| `allow-lake-to-controldb`, `allow-schema-to-controldb` | Lake and its schema migration job reach the owned CNPG cluster on 5432. |
| `allow-lake-to-zetta`, `allow-zetta-internal` | Lake reaches its owned Zetta API on 4800; Zetta peers use 7777 and 4800 among the selected Zetta pods. |
| `allow-controldb-internal` | CNPG members use 5432 and 8000 for replication and health. |
| `allow-lake-to-agents`, `allow-agents-to-lake` | Lake and its library agents call one another on their HTTP ports. |
| `allow-workspace-ingress`, `allow-lake-to-workspaces` | Namespaces labelled for this Lake reach the Lake API; Lake reaches only their Zetta and agent ports. |
| `allow-operator-to-lake`, `allow-operator-to-controldb`, `allow-operator-to-zetta` | The operator calls Lake APIs, reads Lake control rows in Postgres and provisions Zetta over its API. |
| `allow-lake-apiserver-egress`, `allow-cnpg-apiserver-egress`, `allow-cnpg-operator-ingress` | Lake and selected CNPG pods/jobs reach only the discovered API; CNPG's operator reaches its operand on 8000/5432. |
| `allow-lake-public-https` | Lake reaches Clerk, Cloudflare, Stripe and configured model providers over public 443. |
| `allow-agents-public-https` | Library agents reach configured model providers over public 443. |
| `allow-controldb-backup-public-https` | CNPG reaches configured R2/S3 backup endpoints over public 443. |
| `allow-ingress-nginx` | ingress-nginx reaches only Lake's application port for its own Ingress and LakeApp auth subrequests. |
| `allow-lake-clients-to-controldb`, `allow-lake-ingress-to-zetta`, `allow-lake-ingress-to-agents`, `allow-agent-ingress-to-lake` | Destination-side ingress matching the Lake, schema and library-agent egress rules above. Both isolated ends need a rule. |
| `allow-build-ingress-to-lake` | Only this Lake's builder pods in its labelled build namespaces reach the Lake application port for source, registry and build events. |
| `allow-methodology-ingress-to-lake` | Only this Lake's methodology stage and collect pods (a run's, or a served model's stage), in its own `lake-<lake>-methodology-runs` namespace, reach the Lake application port to page data in and report results. The destination half of that namespace's `methodology-transfer` egress; never a sandbox or serve pod ([methodology runs](../lake/methodology-runs.md), lake#802). |
| `allow-lake-to-methodology-serve` | Lake reaches only its served models' serve pods in that namespace on 8080 for predictions: the source half of `methodology-serve-ingress`. |
| `allow-e2e-ingress-to-lake` | Only `LakeE2ETest` runner pods (label `orama.cloud/lake-e2e`) in the Lake's own namespace reach the Lake application port, for the suite's REST, MCP and liquid steps ([#589](https://github.com/oramasearch/orama-operator/issues/589)). The runner is not one of the Lake's pods, so its own egress (the public host, Clerk) is not sandboxed. |
| `allow-rehearsal-ingress-to-controldb` | Only `LakeRestoreRehearsal` verifier pods of this Lake (labels `app.kubernetes.io/name=lake-postgres-rehearsal`, `orama.cloud/rehearsal-lake=<lake>`), in the Lake's namespace (failover) or in a namespace the operator labelled `orama.cloud/rehearsal-lake=<lake>` (the rehearsal's scratch namespace), reach the control database on 5432. They log in with the read-only `lake_rehearsal` role. Without it every Postgres rehearsal of a sandboxed Lake failed at *read-only rehearsal login is unavailable* ([#127](https://github.com/oramasearch/orama-operator/issues/127), [restore rehearsals](../lake/restore-rehearsals.md)). |
| `allow-lake-to-workspace-postgres`, `allow-lake-to-workspace-kafka`, `allow-lake-to-workspace-clickhouse`, `allow-lake-to-workspace-tidb`, `allow-lake-to-workspace-cassandra`, `allow-lake-to-workspace-valkey` | Lake reaches vendor-labelled pods only in its workspace namespaces on 5432, 9092, 8123/9000, 4000, 9042 and 6379 respectively. |
| `allow-lake-to-apps` | Lake egress to each of its declared LakeApp backend namespaces, exact app pod labels and configured container ports. App changes and deletion requeue Lake to update or remove these rules. |

The traffic audit covered `lake_controller.go`, `lake_apps*.go`, `lake_builds.go`,
`lake_services*.go` and the service-kind renderers. `LAKE_SERVICE_DIR` contains Zetta, Postgres
and catalog connection credentials; the Data / Connections and `service_connection` consumers
are **inferred** from that runtime contract (their implementation is outside this repository).
SQLite, DuckDB and Polars wrappers already use 8080, covered by `allow-lake-to-workspaces`;
Zetta uses 4800. Catalog egress uses only the ports emitted in the credentials.

BuildKit runs inside each build Job. Source fetches, registry pushes and events flow from the
builder to Lake (`buildEnv` and `buildEgressRules`); Lake does not connect to a BuildKit daemon. Build namespaces are not
sandboxes, but they get the same `deny-cloud-metadata` CiliumNetworkPolicy, selecting every pod
(`reconcileBuildMetadataDeny`, [#761](https://github.com/oramasearch/orama-operator/issues/761)).
The operator reads build logs via the Kubernetes API, not via Lake pod egress. App-proxy egress
is **inferred** from the declared LakeApp backend Service and port; the generated public app URL
is also reachable through Lake's existing public HTTPS allowance.

The six additional workspace client-ingress policies and LakeApp backend rule provide the
destination half of those paths. The original workspace 4800/8080 rule remains unchanged,
including operator access for Zetta and HTTP reconciliation. No direct Orama operator calls to
catalog database ports or app backends were found: `lake_services.go` and the catalog renderers
use Kubernetes resources, status and Secrets; `lake_services_pgx.go` and the notification loop
connect to Lake's control Postgres, not workspace service databases; app readiness uses Kubernetes
Deployment status. App events/tokens and build events call Lake or the declared Zetta event API.
Thus no new operator-to-catalog or operator-to-app allowance is added. Datastore operators retain
their existing operand-management rules unchanged. The client paths inferred above stay limited
to the owning Lake, vendor/app identities and declared ports.

The backup endpoint path is **inferred** from Lake's CNPG backup contract: the resource supplies an
object-store endpoint, but the reconciled CNPG object does not expose a stable destination CIDR.

## Orama Cloud v1 stack namespace

Every target selector includes the stack instance and manager labels. Component allowances add
their component or database/schema identity. A policy for a disabled component selects no pod
and grants nothing. Operator-labelled and unlabelled pods in `orama-system` match none of them.

| Policy | Allowed path and reason |
|---|---|
| `allow-dns-egress` | Stack workloads reach CoreDNS over UDP/TCP 53. |
| `allow-engine-egress`, `allow-broker-ingress`, `allow-embeddings-ingress` | Writer/readers reach their RabbitMQ replication stream on 5552 and the engine-owned embeddings gRPC service on 50051, which preserves writes, replication and vector searches; broker also accepts workers/HARVEST AMQP 5672; embeddings also accepts DISCO. |
| `allow-dashboard-egress` | Dashboard reaches control DB 5432, cache 6379, the redis-http shim 80, ClickHouse 8123, Typesense 8108, writer 8080, readers 80/8080 and Keycloak 8080. |
| `allow-redis-http-egress`, `allow-redis-http-ingress` | The serverless-redis-http shim reaches only the cache on 6379 (and `allow-cache-ingress` admits it), and admits only the dashboard on 80 ([#562](https://github.com/oramasearch/orama-operator/issues/562)). |
| `allow-workers-egress` | Bundled workers reach broker 5672, cache 6379 and OramaCore writer/readers. |
| `allow-analytics-processor-egress` | Analytics processor reaches ClickHouse 8123, writer 8080, every reader 8080 (it pulls their search events, #419 and #421) and control DB 5432. |
| `allow-analytics-import-egress`, `allow-analytics-processor-ingress` | The analytics import CronJob (#421) calls the analytics processor on 8080, and the processor admits only it there. |
| `allow-analytics-retention-egress` | The analytics retention Job (#421) reaches ClickHouse's native port 9000 for `clickhouse-client`; `allow-analytics-ingress` admits it on 9000 (#616). |
| `allow-index-builder-egress` | Index builder reaches dashboard 3000, cache 6379, control DB 5432, HPES 8787, DISCO 8080/50051, Typesense 8108, decrypter 8080 and HARVEST 8080. |
| `allow-hpes-ingress` | HPES admits only index-builder, on 8787; it has no egress allowance beyond DNS and no public internet ([#574](https://github.com/oramasearch/orama-operator/issues/574)). |
| `allow-harvest-egress` | HARVEST reaches Mongo 27017, broker 5672, cache 6379 and Selenium 4444. |
| `allow-disco-egress`, `allow-pgadmin-egress`, `allow-search-workers-egress` | DISCO reaches embeddings; pgAdmin reaches control DB; hosted-search workers reach Typesense, writer/readers and ClickHouse. |
| `allow-controldb-ingress`, `allow-postgres-schema-egress`, `allow-controldb-cluster-row-egress`, `allow-controldb-internal` | Selected clients, every control DB `PostgresSchema` migration Job and the cluster-row Job (its pod's `orama.cloud/controldb-client=cluster-row`, [#648](https://github.com/oramasearch/orama-operator/issues/648)) reach control DB 5432; CNPG members use 5432/8000 internally. |
| `allow-migration-egress`, `allow-controldb-migration-ingress`, `allow-writer-migration-ingress`, `allow-reader-migration-ingress` | On every stack: an `OramaCloudV1Migration` Job's pod (component `migration`) reaches control DB 5432 as the `migration` role, the writer on 8080 and the readers on 80 with Envoy or 8080 without, and each admits it ([#653](https://github.com/oramasearch/orama-operator/issues/653)). DNS is `allow-dns-egress`. |
| `allow-e2e-egress`, `allow-writer-e2e-ingress`, `allow-reader-e2e-ingress`, `allow-dashboard-e2e-ingress`, `allow-workers-e2e-ingress` | On every stack: an `OramaCloudV1E2ETest` runner's pod (component `e2e`) reaches the writer on 8080, the readers on 80 with Envoy or 8080 without, the dashboard on 3000, and the workers bundle and the four hosted-search workers on 8080, and each admits it ([#61](https://github.com/oramasearch/orama-operator/issues/61), [#63](https://github.com/oramasearch/orama-operator/issues/63)). Nothing outside the namespace. DNS is `allow-dns-egress`. A stack runs a suite only when a test resource is created for it; production creates none. |
| `allow-migration-prod1-egress` | Only while an `OramaCloudV1Migration` in the namespace targets the stack: migration pods reach public addresses on TCP 443 and 5432, for Prod 1's database host on 5432 and its engine origins and `ssm.<region>.amazonaws.com` on 443, which the comment names (the union over the stack's migrations). Enforced by port and public range, not by name: Cilium FQDN policy needs AKS ACNS, which is off; enforcement by name is a follow-up. Private and cluster ranges stay excluded. See `cloud-v1-migration.md`. |
| `allow-cache-ingress`, `allow-analytics-ingress`, `allow-search-ingress` | Each store admits only its listed client component selectors on 6379, 8123 or 8108. |
| `allow-writer-ingress`, `allow-reader-ingress` | OramaCore admits only the dashboard, workers, analytics processor and named hosted-search clients on its service ports. |
| `allow-dashboard-internal-ingress`, `allow-disco-ingress`, `allow-openai-decrypter-ingress`, `allow-harvest-master-ingress` | Index builder reaches each orchestration API on its declared port. |
| `allow-import-consumer-egress`, `allow-dashboard-import-consumer-ingress` | The import consumer ([#446](https://github.com/oramasearch/orama-operator/issues/446)) reaches the dashboard on 3000 for get-source and the status callbacks, and the dashboard admits it; nothing else in the stack. |
| `allow-writer-jwks-egress`, `allow-dashboard-jwks-ingress` | Only while `engine.writer.jwt` is set and its JWKS URL's host is this stack's dashboard Service (`<cr>-dashboard`, alone or qualified with the namespace, `.svc` and the cluster domain): the writer reaches dashboard 3000 for `/api/.well-known/jwks.json`. Readers get neither rule. An external JWKS URL renders neither and goes out through `allow-engine-public-internet`. |
| `allow-harvest-mongo-ingress`, `allow-selenium-ingress`, `allow-keycloak-ingress` | HARVEST reaches its Mongo and browser; dashboard reaches Keycloak. |
| `allow-cnpg-operator-ingress`, `allow-cnpg-apiserver-egress` | CloudNativePG manages control DB through 8000/5432 and its selected pods/jobs reach the API. |
| `allow-operator-to-broker-management` | The Orama operator's `RabbitMQTopology` reconciler reaches management port 15672. |
| `allow-dashboard-ingress-nginx`, `allow-docs-ingress-nginx` | ingress-nginx reaches only dashboard/docs port 3000. |
| `allow-writer-ingress-nginx`, `allow-reader-ingress-nginx` | Only while `engine.writer.host` (primary region) or `engine.reader.host` is published: ingress-nginx alone (never the zetta-tunnel gateway, of 2026-10-17) reaches this stack's writer pods on 8080, or its reader pods on 80 with Envoy and 8080 without ([#385](https://github.com/oramasearch/orama-operator/issues/385)). |
| `allow-engine-public-internet` | Writer/readers reach public IPv4 and IPv6 addresses on any TCP port for customer JavaScript hook URLs. Private, link-local, metadata, carrier-grade NAT, API-server, node-address and pod-CIDR destinations are excluded after DNS resolution. |
| `allow-dashboard-public-https`, `allow-workers-public-https`, `allow-index-builder-public-https`, `allow-harvest-public-https`, `allow-embeddings-public-https`, `allow-search-workers-public-https` | Selected application components reach public HTTPS for their configured providers and downloads. |
| `allow-selenium-public-internet` | Selenium reaches public TCP 80/443 for customer-requested crawls. |
| `allow-import-consumer-public-https` | The import consumer reaches public TCP 443 for the stack's R2 uploads bucket and the engine's public writer host, which get-source names as `writer_url` ([#446](https://github.com/oramasearch/orama-operator/issues/446)). |
| `allow-controldb-primary-replication` | A read-region CNPG cluster reaches its configured primary on public TCP 5432. |

Workers' public APIs, embeddings and hosted-search model/provider downloads, dashboard vendor
list and public primary-Postgres route are **inferred** from component
contracts, configuration fields and the Secret-held Postgres URI. They are documented as such
because the reconciled workload code does not contain a complete stable destination list. Every
inferred public rule still excludes private and cluster ranges.

Native tests enumerate every writer egress policy, prove that unrelated stack services such as
control Postgres are not selected, retain RabbitMQ and the engine-owned embeddings path, check
TCP and UDP DNS, and assert the static and discovered IPv4/IPv6 exclusions. The existing functional
suite remains the write, replication, full-text, vector and hybrid-search proof.

## Deferred work

- The [#468](https://github.com/oramasearch/orama-operator/issues/468) named nonprod probe remains:
  prove a connected app reaches one service, an unconnected app is refused, and an absent row closes
  the path after one reconcile.

No production cluster mutation or new live connectivity probe is part of this change. The native
controller and render suites cover resource creation, migration equivalence, fail-closed rules and
recovery behavior. The live observation above validates the Azure metadata deny mechanism;
rollout verification of the generated policy across all three profiles, including the host-service
and IPv6 destinations, remains an operational follow-up.

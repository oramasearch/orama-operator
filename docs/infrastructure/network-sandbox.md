# Network sandboxes

`NetworkSandbox` is the namespace network boundary implemented for
[#465](https://github.com/oramasearch/orama-operator/issues/465). The operator creates one named
`network-sandbox` in every Lake instance namespace, every Lake workspace namespace and every
`OramaCloudV1` stack namespace. Its controller always renders `NetworkPolicy/default-deny` for
both ingress and egress, then renders only the named allowances in the resource.

The parent reconciler creates the sandbox and renders its policies synchronously before it creates
or updates a workload. The `NetworkSandbox` controller then owns ongoing repair, adoption and
pruning. A deleted sandbox requeues its parent; a deleted rendered policy requeues the sandbox.
Existing unowned policies with a declared name are adopted, but a policy owned by another resource
is refused. Status changes only when the observed generation, counts or Ready condition changes.

Every allowance has a required `comment`, copied to the rendered policy's
`orama.cloud/network-policy-comment` annotation. An empty comment is rejected by the CRD. No
allowance has an empty ingress `from` or egress `to`, which would mean every peer. Kubernetes
NetworkPolicy expresses every path in this stage, so the generated sandboxes currently have no
`CiliumNetworkPolicy`. `spec.ciliumPolicies` is reserved for a later path that NetworkPolicy cannot
represent.

## Address discovery and recovery

Kubernetes API rules include both the `default/kubernetes` Service addresses and all ready and
not-ready endpoint addresses. Public rules start with IPv4 and IPv6 internet CIDRs, then exclude
private, loopback, link-local, metadata, multicast and reserved ranges plus the API Service,
API endpoints, every node address and every node pod CIDR.

Those values come from live cluster discovery. On a transient discovery failure an existing
sandbox retains its last successfully discovered API and public policies and the reconcile returns
an error for retry. A new namespace still receives default-deny and all static narrow rules; it
does not receive a guessed API or public route. Turning a workspace's `internetEgress` off removes
its public rule deliberately.

## Lake workspace namespace

This is a compatibility migration: the generated policy names and specs are byte-for-byte equal to
the policies produced before `NetworkSandbox`. A regression test compares every name and
`NetworkPolicySpec` against the former constructors.

| Policy | Allowed path and reason |
|---|---|
| `allow-workspace-internal` | Pods in the same workspace communicate for existing Zetta, service and app behavior. Issue #468 replaces this broad in-workspace rule with declared connections. |
| `allow-lake-ingress` | The owning Lake and operator reach workspace Zetta and agent HTTP ports 4800 and 8080. |
| `allow-dns-egress` | Workspace pods reach CoreDNS in `kube-system` over UDP/TCP 53. |
| `allow-lake-egress` | Workspace pods reach only the owning Lake pods on the configured Lake port. |
| `allow-cnpg-operator-ingress` | CloudNativePG reaches instance-manager 8000 and Postgres 5432. |
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
| `allow-internet-https` | Only when the workspace opts in, all workspace pods reach public TCP 443 with the cluster/private exclusions above. |

App-specific access policies remain owned by `LakeApp`; they already select one app and its
declared ports. This stage does not change those specs.

## Lake instance namespace

| Policy | Allowed path and reason |
|---|---|
| `allow-dns-egress` | All Lake namespace workloads reach CoreDNS over UDP/TCP 53. |
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

The backup endpoint path is **inferred** from Lake's CNPG backup contract: the resource supplies an
object-store endpoint, but the reconciled CNPG object does not expose a stable destination CIDR.

## Orama Cloud v1 stack namespace

All selectors include the stack name and component label. A policy for a disabled component
selects no pod and grants nothing.

| Policy | Allowed path and reason |
|---|---|
| `allow-dns-egress` | Stack workloads reach CoreDNS over UDP/TCP 53. |
| `allow-engine-egress`, `allow-broker-ingress`, `allow-embeddings-ingress` | Writer/readers reach RabbitMQ stream 5552 and embeddings gRPC 50051; broker also accepts workers/HARVEST AMQP 5672; embeddings also accepts DISCO. |
| `allow-dashboard-egress` | Dashboard reaches control DB 5432, cache 6379, ClickHouse 8123, Typesense 8108, writer 8080, readers 80/8080, Keycloak 8080 and Airbyte 8001. |
| `allow-workers-egress` | Bundled workers reach broker 5672, cache 6379 and OramaCore writer/readers. |
| `allow-analytics-processor-egress` | Analytics processor reaches ClickHouse 8123, writer 8080 and control DB 5432. |
| `allow-index-builder-egress` | Index builder reaches dashboard 3000, cache 6379, control DB 5432, DISCO 8080/50051, Typesense 8108, decrypter 8080, HARVEST 8080 and Airbyte 8001. |
| `allow-harvest-egress` | HARVEST reaches Mongo 27017, broker 5672, cache 6379 and Selenium 4444. |
| `allow-disco-egress`, `allow-pgadmin-egress`, `allow-search-workers-egress` | DISCO reaches embeddings; pgAdmin reaches control DB; hosted-search workers reach Typesense, writer/readers and ClickHouse. |
| `allow-controldb-ingress`, `allow-postgres-schema-egress`, `allow-controldb-internal` | Selected clients and the schema job reach control DB 5432; CNPG members use 5432/8000 internally. |
| `allow-cache-ingress`, `allow-analytics-ingress`, `allow-search-ingress` | Each store admits only its listed client component selectors on 6379, 8123 or 8108. |
| `allow-writer-ingress`, `allow-reader-ingress` | OramaCore admits only the dashboard, workers, analytics processor and named hosted-search clients on its service ports. |
| `allow-dashboard-internal-ingress`, `allow-disco-ingress`, `allow-openai-decrypter-ingress`, `allow-harvest-master-ingress` | Index builder reaches each orchestration API on its declared port. |
| `allow-harvest-mongo-ingress`, `allow-selenium-ingress`, `allow-keycloak-ingress` | HARVEST reaches its Mongo and browser; dashboard reaches Keycloak. |
| `allow-cnpg-operator-ingress`, `allow-cnpg-apiserver-egress` | CloudNativePG manages control DB through 8000/5432 and its selected pods/jobs reach the API. |
| `allow-operator-to-broker-management` | The Orama operator's `RabbitMQTopology` reconciler reaches management port 15672. |
| `allow-dashboard-ingress-nginx`, `allow-docs-ingress-nginx` | ingress-nginx reaches only dashboard/docs port 3000. |
| `allow-engine-public-internet` | Writer/readers retain public TCP 80/443. This is intentionally broad until #466 audits and narrows engine destinations. |
| `allow-dashboard-public-https`, `allow-workers-public-https`, `allow-index-builder-public-https`, `allow-harvest-public-https`, `allow-embeddings-public-https`, `allow-search-workers-public-https` | Selected application components reach public HTTPS for their configured providers and downloads. |
| `allow-selenium-public-internet` | Selenium reaches public TCP 80/443 for customer-requested crawls. |
| `allow-controldb-primary-replication` | A read-region CNPG cluster reaches its configured primary on public TCP 5432. |
| `allow-control-clients-external-postgres` | In external control-DB mode, dashboard, index builder, analytics processor and pgAdmin reach public TCP 5432. |

The Airbyte namespace/port, workers' public APIs, embeddings and hosted-search model/provider
downloads, dashboard vendor list, public primary-Postgres route, and external control-DB address
are **inferred** from component contracts, configuration fields and the Secret-held Postgres URI.
They are documented as such because the reconciled workload code does not contain a complete stable
destination list. Every inferred public rule still excludes private and cluster ranges.

## Deferred work

- [#466](https://github.com/oramasearch/orama-operator/issues/466) narrows engine public egress.
- [#467](https://github.com/oramasearch/orama-operator/issues/467) prevents tenants from writing
  policy resources.
- [#468](https://github.com/oramasearch/orama-operator/issues/468) replaces workspace-wide internal
  reachability with declared `Connection` resources.

No production cluster mutation or live connectivity probe is part of this change. The native
controller and render suites prove resource creation, migration equivalence, fail-closed rules and
recovery behavior; live rollout remains an operational follow-up.

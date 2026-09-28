# orama-operator — docs

Deep docs for the operator. Kept in lockstep with the source (see the repo
[`CLAUDE.md`](../CLAUDE.md) → *Docs discipline*).

- [`../README.md`](../README.md) — the front door: what the operator is, status, build and test, and pointers into these docs.
- [`../DESIGN.md`](../DESIGN.md) — architecture + the decision log from the design grill.
- [`install.md`](install.md) — **install the operator + deploy a v1 stack** (prereqs, helm/helmfile, the CR, multi-region, uninstall).
- [`install/credentials.md`](install/credentials.md) — **private-image bootstrap**: manager versus application pull-secret settings, render proof, and startup diagnosis.
- [`install/k0s.md`](install/k0s.md) — **install a private Lake on the liquid k0s distribution**, including the fresh-machine, single-node storage, pull-secret, Ready, and air-gapped paths.
- [`development.md`](development.md) — build, codegen, run, and the AKS smoke test.
- [`crd-reference.md`](crd-reference.md) — the `OramaCloudV1` field reference.
- [`components.md`](components.md) — what each component reconciler creates, and its status.
- [`datastore-crds.md`](datastore-crds.md) — **datastore state as declarative, owned CRDs** ([#40](https://github.com/oramasearch/orama-operator/issues/40)): the reconciled-CRD pattern and the `RabbitMQTopology` broker topology (vhosts/users/permissions/streams via the management API).
- [`zetta-datastore.md`](zetta-datastore.md) — **Zetta as the operator's datastore** (One Orama symbiosis v0): the `orama_control` / `orama_data` schema, how it's provisioned over `/v1/sql`, and the desired-vs-observed source-of-truth boundary.
- [`zetta-cluster-mtls.md`](zetta-cluster-mtls.md) — **the shared cluster join token**: how the operator mints one token per `OramaZettaCluster` so every node forms **one** mTLS cluster (the CA is derived from the token), why peer traffic is mTLS-only (no plaintext mode), and the engine dependencies for multi-node convergence ([operator#92](https://github.com/oramasearch/orama-operator/issues/92)).
- [`control-plane/README.md`](control-plane/README.md) — **liquid deployment control plane**: authenticated REST `/v1` and MCP operations, Zetta history/audit storage, `LakeEnvironment` materialization, rollback, and OpenAPI drift verification.
- [`lake/README.md`](lake/README.md) — **the `Lake` CR**: what it reconciles (namespace, credentials, Deployment, Service, CNPG control-plane Postgres, the schema via `PostgresSchema`, Ingress + DNS), status, RBAC, how to deploy the operator with Lake support, how to run a Lake, the current production instance.
- [`lake/schema.md`](lake/schema.md) — **Lake schema provenance and rollout ordering**: pinned byte verification, migration `0021`, automatic Job replacement, re-vendoring, and native envtest/Postgres proof.
- [`lake/backups.md`](lake/backups.md) — **Lake control-plane backups**: R2 credentials, scheduled CNPG Barman backups, verification, configuration changes, and recovery into a new Cluster.
- [`lake/environments.md`](lake/environments.md) — **named and per-change Lake environments**: Zetta deployment events, `LakeEnvironment` materialization, the k0s namespace profile, ingress/DNS, anti-flap status and teardown.
- [`lake/apps.md`](lake/apps.md) — **deployed Lake apps**: the `LakeApp` runtime/image contract, isolated namespace, ingress/auth, data-binding Secret delivery, rollout status and live events.
- [`lake/lake-sqlite.md`](lake/lake-sqlite.md) — **lake-sqlite**: the web service behind the `sqlite` service kind (one SQLite database, `modernc.org/sqlite`, BSD-3-Clause): its API, configuration, build and local run.
- [`lake/lake-duckdb.md`](lake/lake-duckdb.md) — **lake-duckdb**, powered by DuckDB: where the web service behind the `duckdb` service kind lives (its own private repository and image) and how this operator runs it.
- [`lake/lake-polars.md`](lake/lake-polars.md) — **lake-polars**, powered by Polars: where the web service behind the `polars` service kind lives (its own private repository and image) and how this operator runs it.
- [`lake/services.md`](lake/services.md) — **Lake managed services**: the `services` / `service_status` contract in lake's control Postgres, one Lake-owned `OramaZettaCluster`, CloudNativePG `Cluster`, or catalog resource (Kafka, ClickHouse, TiDB, Cassandra, Valkey through their upstream operators; SQLite, DuckDB and Polars through lake-sqlite, lake-duckdb and lake-polars, which the operator runs itself) per service, adoption of workspace clusters, the `lake-<lake>-services` credentials Secret, deletion and quota.
- [`lake/stores.md`](lake/stores.md) — **Lake customer stores** (lake#196): the `stores` / `store_status` contract on the services poll; phase 1's cluster volume (one Lake-owned PersistentVolumeClaim per store, its connection in `lake-<lake>-services`), the other pairs `Failed`, deletion.
- [`lake/workspaces.md`](lake/workspaces.md) — **per-customer Zetta workspaces (superseded by services)**: what decision 45 built and how it maps onto managed services.
- [`lake/admission-fixtures.md`](lake/admission-fixtures.md) — pinned k0s admission policies and tenant RBAC used by the native environment delivery proof.
- [`lake/data-services/README.md`](lake/data-services/README.md) — **Lake data-service labels**: per-operation evidence, the managed threshold, and the customer responsibility for explicitly unmanaged services.
- [`lake/data-services/postgres.md`](lake/data-services/postgres.md) — **Lake Postgres rehearsals**: automated backup recovery, scratch upgrade, replicated failover, verification, cleanup, and status recording.
- [`lake/sealed-secrets.md`](lake/sealed-secrets.md) — **one-way secret delivery**: the cluster sealing key, canonical CBOR envelope, AAD binding, rotation, source types, and verification.
- [`connecting.md`](connecting.md) — **connect to a deployed `OramaZettaCluster`** across every surface: REST/SQL (`/v1/*`), MCP (`POST /mcp`, for Claude/agents), the self-describing `mcp.md` / `llms.txt` pages, and the web portal (`/`); addressing (port-forward / Ingress `spec.host`), and MCP/admin-token auth.
- [`operations/crd-rollout.md`](operations/crd-rollout.md) — **CRDs and rollout**: the three
  hand-maintained artifacts per kind, why one missing CRD crash-loops the whole manager, the
  checks that hold the invariant, and why fail-fast is deliberate.
- [`operations/rename-cutover.md`](operations/rename-cutover.md) — **one-time AKS rename cutover**: install the `orama-operator` Helm release, preserve CRDs and the production leader-election Lease, move control-plane callers, and retire the former release.
- [`karpenter-cloud-iam-prerequisite.md`](karpenter-cloud-iam-prerequisite.md) — **operator-owned Karpenter install** ([#75](https://github.com/oramasearch/orama-operator/issues/75)): the cloud IAM identity the operator can't mint (controller IRSA role / node instance profile / Azure role assignment), how it's cloud-gated (EKS self-install, no-op on AKS-NAP / bare-metal), and the `.status` conditions.

The production system this operator models is documented separately in
[`techops-docs/docs/orama-cloud-v1/`](https://github.com/oramasearch/techops-docs/tree/main/docs/orama-cloud-v1).

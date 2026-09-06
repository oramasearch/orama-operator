# orama-cloud-v1-operator — docs

Deep docs for the operator. Kept in lockstep with the source (see the repo
[`CLAUDE.md`](../CLAUDE.md) → *Docs discipline*).

- [`../README.md`](../README.md) — overview + quickstart.
- [`../DESIGN.md`](../DESIGN.md) — architecture + the decision log from the design grill.
- [`install.md`](install.md) — **install the operator + deploy a v1 stack** (prereqs, helm/helmfile, the CR, multi-region, uninstall).
- [`development.md`](development.md) — build, codegen, run, and the AKS smoke test.
- [`crd-reference.md`](crd-reference.md) — the `OramaCloudV1` field reference.
- [`components.md`](components.md) — what each component reconciler creates, and its status.
- [`datastore-crds.md`](datastore-crds.md) — **datastore state as declarative, owned CRDs** ([#40](https://github.com/oramasearch/orama-cloud-v1-operator/issues/40)): the reconciled-CRD pattern and the `RabbitMQTopology` broker topology (vhosts/users/permissions/streams via the management API).
- [`zetta-datastore.md`](zetta-datastore.md) — **Zetta as the operator's datastore** (One Orama symbiosis v0): the `orama_control` / `orama_data` schema, how it's provisioned over `/v1/sql`, and the desired-vs-observed source-of-truth boundary.
- [`zetta-cluster-mtls.md`](zetta-cluster-mtls.md) — **the shared cluster join token**: how the operator mints one token per `OramaZettaCluster` so every node forms **one** mTLS cluster (the CA is derived from the token), why peer traffic is mTLS-only (no plaintext mode), and the engine dependencies for multi-node convergence ([operator#92](https://github.com/oramasearch/orama-cloud-v1-operator/issues/92)).
- [`control-plane/README.md`](control-plane/README.md) — **liquid deployment control plane**: authenticated REST `/v1` and MCP operations, Zetta history/audit storage, `LakeEnvironment` materialization, rollback, and OpenAPI drift verification.
- [`lake/README.md`](lake/README.md) — **the `Lake` CR**: what it reconciles (namespace, credentials, Deployment, Service, CNPG control-plane Postgres, the schema via `PostgresSchema`, Ingress + DNS), status, RBAC, how to deploy the operator with Lake support, how to run a Lake, the current production instance.
- [`lake/backups.md`](lake/backups.md) — **Lake control-plane backups**: R2 credentials, scheduled CNPG Barman backups, verification, configuration changes, and recovery into a new Cluster.
- [`lake/environments.md`](lake/environments.md) — **named and per-change Lake environments**: Zetta deployment events, `LakeEnvironment` materialization, the k0s namespace profile, ingress/DNS, anti-flap status and teardown.
- [`lake/data-services/README.md`](lake/data-services/README.md) — **Lake data-service labels**: per-operation evidence, the managed threshold, and the customer responsibility for explicitly unmanaged services.
- [`lake/data-services/postgres.md`](lake/data-services/postgres.md) — **Lake Postgres rehearsals**: automated backup recovery, scratch upgrade, replicated failover, verification, cleanup, and status recording.
- [`lake/sealed-secrets.md`](lake/sealed-secrets.md) — **one-way secret delivery**: the cluster sealing key, canonical CBOR envelope, AAD binding, rotation, source types, and verification.
- [`connecting.md`](connecting.md) — **connect to a deployed `OramaZettaCluster`** across every surface: REST/SQL (`/v1/*`), MCP (`POST /mcp`, for Claude/agents), the self-describing `mcp.md` / `llms.txt` pages, and the web portal (`/`); addressing (port-forward / Ingress `spec.host`), and MCP/admin-token auth.
- [`karpenter-cloud-iam-prerequisite.md`](karpenter-cloud-iam-prerequisite.md) — **operator-owned Karpenter install** ([#75](https://github.com/oramasearch/orama-cloud-v1-operator/issues/75)): the cloud IAM identity the operator can't mint (controller IRSA role / node instance profile / Azure role assignment), how it's cloud-gated (EKS self-install, no-op on AKS-NAP / bare-metal), and the `.status` conditions.

The production system this operator models is documented separately in
[`techops-docs/docs/orama-cloud-v1/`](https://github.com/oramasearch/techops-docs/tree/main/docs/orama-cloud-v1).

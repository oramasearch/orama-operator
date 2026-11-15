# Releases

One page per tagged release, newest first. A page states what the release includes, what it
does not, what is knowingly still broken, and how it was verified.

- [v0.11.12](v0.11.12.md) — 2026-10-31. A patch: the production Pixie hub takes the managing cluster and nonprod clusters too,
  agents may name their own alert receivers, and the bootstrap writes each user's settings rows.
- [v0.11.11](v0.11.11.md) — 2026-10-31. A patch: Envoy's CORS allows the hub's domain, so the UI runs scripts; every Pixie
  pod template carries a fingerprint of the ConfigMaps it reads.
- [v0.11.10](v0.11.10.md) — 2026-10-31. A patch: the UI fetches the PxL script bundle from its own origin and the API server
  from the bundle server's Service.
- [v0.11.9](v0.11.9.md) — 2026-10-31. A patch: the hub Prometheus sets a `scrapeConfigSelector`, so prometheus-operator writes
  its configuration and the federation jobs and hub rules load.
- [v0.11.8](v0.11.8.md) — 2026-10-31. A patch: people can sign in to the Pixie hubs. The organisation gets the
  administrator's email domain, and `ObservabilityHub.spec.users` gives each person a generated password.
- [v0.11.7](v0.11.7.md) — 2026-10-31. A patch: Pixie's metadata index is created once OpenSearch is ready, before the hub
  is, so the indexer starts and the hubs can become Ready; how to reach the monitoring UIs.
- [v0.11.6](v0.11.6.md) — 2026-10-31. A patch: the hub Prometheus owns its volume (`fsGroup`), a Pixie Postgres image must
  carry its tag; the Pixie prune spares objects another controller owns, and `pl-sentry-dsn-config` is rendered.
- [v0.11.5](v0.11.5.md) — 2026-10-31. A patch: Pixie's release, `config/clusters/observability-provider.yaml` naming
  all 27 images by digest, and Pixie's build patches they were made with. No operator code or CRD changes.
- [v0.11.4](v0.11.4.md) — 2026-10-30. A patch: kube-prometheus-stack's `KubeCPUQuotaOvercommit` and `KubeMemoryQuotaOvercommit`
  are off on every cluster, because Lake's workspace quotas are ceilings meant to exceed the cluster. Platform values only.
- [v0.11.3](v0.11.3.md) — 2026-10-30. A patch: kube-prometheus-stack's `KubeCPUOvercommit` and `KubeMemoryOvercommit` are off
  on every cluster, for clusters that autoscale instead of holding a spare node. Platform values only; no code or CRD changes.
- [v0.11.2](v0.11.2.md) — 2026-10-30. A patch: observability agents write their alert routing before Pixie's release
  exists; leaf clients know Orama's kinds, and the agent creates the hub's `plc` namespace. No CRD changes.
- [v0.11.1](v0.11.1.md) — 2026-10-29. A patch: a production observability hub runs on the management cluster (a hub with
  no `clusterRef`), and the `prod-support` cluster purpose is removed. No Lake or Cloud v1 workload changes on the roll.
- [v0.11.0](v0.11.0.md) — 2026-10-29. A minor release: five observability kinds and a self-hosted Pixie, zone-level
  authenticated origin pulls with one CA per zone, and four Cloud v1 fixes. Every combined-image Cloud v1 stack's readers
  restart once.
- [v0.10.6](v0.10.6.md) — 2026-10-27. A patch: an App or environment is Ready only when its rollout is complete.
- [v0.10.5](v0.10.5.md) — 2026-10-27. A patch: App hosts stop flapping to Cloudflare 526; the App's Secret pass
  keeps its Ingress's certificate copy and edge client CA.
- [v0.10.4](v0.10.4.md) — 2026-10-26. A patch: agents default to `gpt-6.1-sol`, and a Lake restart replaces
  the App's pods through `LakeApp.spec.sync.restartGeneration`. `LakeApp` gains one optional field;
  no App restarts on the roll.
- [v0.10.3](v0.10.3.md) — 2026-10-25. A patch: the Cloud v1 writer embeds 64 texts per batch, not 50,000, so a
  CPU writer's memory stays bounded during a copy. Each Cloud v1 stack's writer and readers roll once.
- [v0.10.2](v0.10.2.md) — 2026-10-25. A patch: an app's Zetta binding is the JSON document app-runtime reads,
  and Cloud v1 stacks generate their DISCO and pgAdmin credentials and replace the placeholder
  RabbitMQ admin password. Each bound app and the affected Cloud v1 consumers roll once.
- [v0.10.1](v0.10.1.md) — 2026-10-25. A patch: versions as columns on `LakeSet` (`Versions`) and `Lake`
  (`Image`, `Version`, `Revision`), for `kubectl get` and Lens. No workload restarts.
- [v0.10.0](v0.10.0.md) — 2026-10-24. The operator side of lake v0.7.0: a lake version is only its
  image (`lake manifest`, `lake migrate`), a Lake on a customer-owned domain, a status page, store
  restore rehearsals, the Stripe auto top-up permission check, and generated Cloud v1
  stack-internal credentials. New kind `StatusPage`. **Roll it before lake v0.7.0.**
- [v0.9.5](v0.9.5.md) — 2026-10-24. A patch for lake v0.6.0: its schema through `0099` and the
  prepaid balance's Stripe events. **Roll it before the lake v0.6.0 image.**
- [v0.9.4](v0.9.4.md) — 2026-10-24. A patch: a Cloud v1 docs host gets its DNS record.
- [v0.9.3](v0.9.3.md) — 2026-10-24. A patch: Cloud v1 storage can approve replacing a volume it cannot
  change in place (`approveDataDeletion`), for moving to a premium storage class.
- [v0.9.2](v0.9.2.md) — 2026-10-24. A patch: the lake Deployment gets its own `ZETTA_EGRESS_ALLOW`, so lake
  reaches its Zettas again. **Roll it before the lake build that needs it.**
- [v0.9.1](v0.9.1.md) — 2026-10-24. A patch for lake v0.5.0: its schema through `0098`, the Clerk
  secret key, and the Clerk session and Stripe subscription webhook events. **Roll it before the lake
  v0.5.0 image.**
- [v0.9.0](v0.9.0.md) — 2026-10-23. The operator side of zetta v0.5.0 (approved connector
  destinations on every workspace Zetta), nonprod Stripe payments per environment, Cloud v1 production
  dashboard credentials, the qualified HPES image and an optional Ably reference.
  **Roll it before zetta v0.5.0.**
- [v0.8.0](v0.8.0.md) — 2026-10-22. The security hardening release, lake v0.4.0's schema, backups, edge host policies and
  certificates, and the Cloud v1 stack's own users and keys. Read its *Before it rolls*.
- [v0.7.3](v0.7.3.md) — 2026-10-20. A patch releasing the committed cluster files: every Lake on
  lake v0.3.1, the leaves on the operator, and a Cloud v1 migration declared (not applied). No code
  or CRD change. 43 resource kinds.
- [v0.7.2](v0.7.2.md) — 2026-10-20. A patch: Lake's schema through `0090_billing_contacts`,
  so lake v0.3.1 can roll. No CRD change. 43 resource kinds.
- [v0.7.1](v0.7.1.md) — 2026-10-20. A patch: `OramaCloudV1Migration` copies one team's data into a
  Cloud v1 stack, declared on `OramaCloudV1Set`, and every Cloud v1 stack gets a `migration` role that
  can sign in only during a migration; `Lake.spec.payments` gives each Lake its own Stripe endpoint,
  catalog and keys; a Stripe webhook endpoint's deletion always finishes. Nothing is used on a
  cluster until a resource names it. 43 resource kinds.
- [v0.7.0](v0.7.0.md) — 2026-10-20. The payment kinds: the operator keeps an app's Stripe webhook
  endpoint and catalog from committed resources (`PaymentProvider`, `PaymentWebhookEndpoint`,
  `StripeWebhookEndpoint`, `PaymentCatalog`, `StripeCatalog`; a `cloud_v1` catalog on a live key
  refused); paid orders provisioned from lake's control plane (`LakeOrderSource`); the Cloud v1
  cluster-row Job through the network sandbox; Lake's schema at lake v0.3.0. 42 resource kinds.
- [v0.6.2](v0.6.2.md) — 2026-10-19. A patch: the Cloud v1 network sandbox admits every control
  DB migration Job, so `controldb-schema-0008` and `reference-data` reach the database.
  Also carries origin TLS on every Lake Ingress with the Lake's Cloudflare zone secured by the
  operator (`EdgeProvider`, `EdgeZonePolicy`, `CloudflareZonePolicy`) and the Cloud v1 engine image
  serving `/health/ready`. 36 resource kinds.
- [v0.6.1](v0.6.1.md) — 2026-10-18. A patch: the Cloud v1 network sandbox allows the analytics
  retention Job (to ClickHouse), the analytics import CronJob (to the processor) and the processor's
  pulls from the readers, which v0.6.0 needed; a stack naming no master key gets a generated
  one. No CRD change. 33 resource kinds.
- [v0.6.0](v0.6.0.md) — 2026-10-17. The Cloud v1 engine reachable and kept for the migration:
  the writer verifies the dashboard's write tokens, public writer and reader hosts with extra names
  and `/metrics` closed, readers as a StatefulSet with a disk each, a dedicated reader analytics
  key, a scheduled analytics import with 30 days kept, and the `c1` cluster row on a new control
  database. `engine.reader.hpa` removed. 33 resource kinds.
- [v0.5.0](v0.5.0.md) — 2026-10-17. The Orama Cloud v1 stack in its hosted shape: an in-cluster
  control database with the dashboard's migration 0008 and reference rows, side-only engine images,
  LLM providers from the spec, the hooks deny list, startup budgets, reader readiness on
  `/health/ready`, generated broker and Redis passwords with one required master key, the
  dashboard's environment and crons, origin TLS, and storage the operator grows. Create-only
  Cloudflare DNS, node-pool sysctls, and `LakeE2ETest` aimed at a set member. 33 resource kinds.
- [v0.4.0](v0.4.0.md) — 2026-10-17. Every Lake runs every feature, with the operator defaulting
  every image (the library agents and agent-runtime at their `v0.1.0` releases); a Lake's Clerk
  instance kept configured from its secret key; a Lake's own Zetta as its deployment control plane
  in lake's row format; CRDs applied widened before a roll and exactly after. 33 resource kinds.
- [v0.3.0](v0.3.0.md) — 2026-10-17. Secure networking (network sandboxes, declared connections,
  egress, tunnels, peers, the zetta-tunnel ingress gateway and mesh) and regions (one Lake per
  region with a region picker at an apex host). The nonprod set gains `testing`. 33 resource kinds.
- [v0.2.0](v0.2.0.md) — 2026-10-14. The operator manages clusters: AKS clusters, their platform
  layer and their own operator, and Lake and Orama Cloud v1 instances on them from sets, with one
  identical operator on every cluster. Every Lake service kind starts in a workspace. 24 resource
  kinds.
- [v0.1.0](v0.1.0.md) — 2026-10-12. The first tagged release, so the page is the full inventory:
  all 17 resource kinds and what each reconciles, the deployment control plane, and how the
  operator is installed.

## Conventions

- **A release promotes `dev` to `main`.** Work lands on `dev`; `main` moves only on a release and
  equals the last tag in between.
- **A release is three tags with one name**: the Git tag on the merge commit, the GitHub
  release, and the same tag on the image, naming the digest that is published.
- **A first release, or one after a long gap, lists everything in it**, not a summary: every
  resource kind in `api/v1alpha1/`, checked against `chart/crds/`.
- **Say what has been proven.** Implemented, validated on a cluster, and live-tested
  against a real external account are different claims;
  [`docs/components.md`](../components.md) holds the status of each reconciler.
- **Every number is measured** and says what it was measured on.
- **Known issues are part of the release**, stated in the notes.

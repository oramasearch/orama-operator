# Releases

One page per tagged release, newest first. A page states what the release includes, what it
does not, what is knowingly still broken, and how it was verified.

- [v0.7.2](v0.7.2.md) — 2026-10-20. A patch: Lake's schema through `0090_billing_contacts`
  (#656), so lake v0.3.1 can roll. No CRD change. 43 resource kinds.
- [v0.7.1](v0.7.1.md) — 2026-10-20. A patch: `OramaCloudV1Migration` runs one Prod 1 team's copy
  into a Cloud v1 stack (#653), declared on `OramaCloudV1Set` (#654), and every Cloud v1 stack gets a
  `migration` role that can sign in only during a migration; `Lake.spec.payments` gives each Lake its
  own Stripe endpoint, catalog and keys (#639); a Stripe webhook endpoint's deletion always finishes
  (#663). Nothing is used on a cluster until a resource names it. 43 resource kinds.
- [v0.7.0](v0.7.0.md) — 2026-10-20. The payment kinds: the operator keeps an app's Stripe webhook
  endpoint and catalog from committed resources (#637, #638: `PaymentProvider`,
  `PaymentWebhookEndpoint`, `StripeWebhookEndpoint`, `PaymentCatalog`, `StripeCatalog`; a `cloud_v1`
  catalog on a live key refused); paid
  `lake.live` orders provisioned from lake's control plane (#634: `LakeOrderSource`); the Cloud v1
  cluster-row Job through the network sandbox (#648); Lake's schema at lake v0.3.0 (#656). None of
  the new kinds is used on a cluster yet. 42 resource kinds. Image `main-deea3c8` = `v0.7.0`
  (`sha256:527a65f8…`), rolled to every cluster on 2026-10-20.
- [v0.6.2](v0.6.2.md) — 2026-10-19. A patch: the Cloud v1 network sandbox admits every control
  DB migration Job, so `controldb-schema-0008` and `reference-data` reach the database (#625, #630).
  Also carries origin TLS on every Lake Ingress with the Lake's Cloudflare zone secured by the
  operator (#631: `EdgeProvider`, `EdgeZonePolicy`, `CloudflareZonePolicy`), the Cloud v1 engine image
  serving `/health/ready` (#626) and `lake-prod` on `lake.live` (#624). 36 resource kinds.
- [v0.6.1](v0.6.1.md) — 2026-10-18. A patch: the Cloud v1 network sandbox allows the analytics
  retention Job (to ClickHouse), the analytics import CronJob (to the processor) and the processor's
  pulls from the readers, which v0.6.0 needed (#616); a stack naming no master key gets a generated
  one (#615). No CRD change. 33 resource kinds.
- [v0.6.0](v0.6.0.md) — 2026-10-17. The Cloud v1 engine reachable and kept for the migration:
  the writer verifies the dashboard's write tokens, public writer and reader hosts with extra names
  and `/metrics` closed, readers as a StatefulSet with a disk each, a dedicated reader analytics
  key, a scheduled analytics import with 30 days kept, and the `c1` cluster row on a new control
  database. `engine.reader.hpa` removed. 33 resource kinds.
- [v0.5.0](v0.5.0.md) — 2026-10-17. The Orama Cloud v1 stack in Prod 1's shape for the
  migration: an in-cluster control database with the dashboard's migration 0008 and reference rows,
  side-only engine images, LLM providers from the spec, the hooks deny list, Prod 1's startup
  budgets, reader readiness on `/health/ready`, generated broker and Redis passwords with one
  required master key, the dashboard's environment and crons, origin TLS, and storage the operator
  grows. Create-only Cloudflare DNS, node-pool sysctls, and `LakeE2ETest` aimed at a set member.
  33 resource kinds.
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
  equals the last tag in between. The steps are in the root `CLAUDE.md`, under *Cutting a release*.
- **A release is three tags with one name**: the Git tag on the merge commit, the GitHub
  release, and the same tag on the image in Docker Hub, naming the digest that is deployed.
- **A first release, or one after a long gap, lists everything in it**, not a summary: every
  resource kind in `api/v1alpha1/`, checked against `chart/crds/`.
- **Say where each thing has run.** Implemented, validated on the cluster, and live-tested
  against a real external account are different claims;
  [`docs/components.md`](../components.md) holds the status of each reconciler.
- **Every number is measured** and says what it was measured on.
- **Known issues are part of the release**, linked to their tracking issue.
- Cross-repository references are full URLs; issues in this repository are linked by number.

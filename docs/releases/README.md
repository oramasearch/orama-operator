# Releases

One page per tagged release, newest first. A page states what the release includes, what it
does not, what is knowingly still broken, and how it was verified.

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

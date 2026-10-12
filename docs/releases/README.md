# Releases

One page per tagged release, newest first. A page states what the release includes, what it
does not, what is knowingly still broken, and how it was verified.

- [v0.1.0](v0.1.0.md) — 2026-10-12. The first tagged release, so the page is the full inventory:
  all 17 resource kinds and what each reconciles, the deployment control plane, and how the
  operator is installed.

## Conventions

- **Cut a release from `main`**, which is what the cluster runs. The steps are in the root
  `CLAUDE.md`, under *Cutting a release*.
- **A release is three tags with one name**: the Git tag on the merge commit, the GitHub
  release, and the same tag on the image in Docker Hub, naming the digest that is deployed.
- **A first release, or one after a long gap, lists everything in it**, not a summary: every
  resource kind in `api/v1alpha1/`, checked against `chart/crds/` and `chart/e2e-crds/`.
- **Say where each thing has run.** Implemented, validated on the cluster, and live-tested
  against a real external account are different claims;
  [`docs/components.md`](../components.md) holds the status of each reconciler.
- **Every number is measured** and says what it was measured on.
- **Known issues are part of the release**, linked to their tracking issue.
- Cross-repository references are full URLs; issues in this repository are linked by number.

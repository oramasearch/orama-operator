# Repository agent rules

Read [`CLAUDE.md`](CLAUDE.md) before changing this repository; its production-migration and
documentation rules apply in full.

- Keep matching documentation in the same commit as every behavior change.
- Do not add AI attribution to commits or pull requests.
- Do not include time estimates.
- Changes ship through pull requests; never fast-forward `main`.
- This operator runs in production on AKS: preserve deterministic reconciliation, recovery, and
  accurate, non-flapping status.

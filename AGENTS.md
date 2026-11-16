# Repository agent rules

Read [`CLAUDE.md`](CLAUDE.md) before changing this repository; its principles, build and test
commands and documentation rules apply in full.

- Keep matching documentation in the same commit as every behavior change.
- Do not add AI attribution to commits or pull requests.
- Do not include time estimates.
- Never write the word "seam"; name the actual mechanism (interface, extension point, boundary).
- Pull requests target `dev`; never push to `dev` or `main` directly.
- Preserve deterministic reconciliation, recovery, and accurate, non-flapping status.
- Do not change a cluster by hand; change the operator or a committed resource it reconciles.

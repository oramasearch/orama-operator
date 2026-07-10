# orama-cloud-v1-operator — docs

Deep docs for the operator. Kept in lockstep with the source (see the repo
[`CLAUDE.md`](../CLAUDE.md) → *Docs discipline*).

- [`../README.md`](../README.md) — overview + quickstart.
- [`../DESIGN.md`](../DESIGN.md) — architecture + the decision log from the design grill.
- [`install.md`](install.md) — **install the operator + deploy a v1 stack** (prereqs, helm/helmfile, the CR, multi-region, uninstall).
- [`development.md`](development.md) — build, codegen, run, and the AKS smoke test.
- [`crd-reference.md`](crd-reference.md) — the `OramaCloudV1` field reference.
- [`components.md`](components.md) — what each component reconciler creates, and its status.
- [`zetta-datastore.md`](zetta-datastore.md) — **Zetta as the operator's datastore** (One Orama symbiosis v0): the `orama_control` / `orama_data` schema, how it's provisioned over `/v1/sql`, and the desired-vs-observed source-of-truth boundary.

The production system this operator models is documented separately in
[`techops-docs/docs/orama-cloud-v1/`](https://github.com/oramasearch/techops-docs/tree/main/docs/orama-cloud-v1).

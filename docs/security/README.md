# Security reviews

Findings of security reviews of what the operator renders and runs, one page per review. Each page
says what was checked, how (the code read, the tests written, the live read-only reads) and what was
found, with file and line references, and links the issue filed for every defect.

- [`deep-search-711-tenant-app.md`](deep-search-711-tenant-app.md) — **a tenant's custom app as a
  foothold into Kubernetes** ([#711](https://github.com/oramasearch/orama-operator/issues/711), part of
  [lake#873](https://github.com/oramasearch/lake/issues/873)): the app pod's network reach, Pod Security,
  node isolation, ingress and identity headers, mounted Secrets and image pinning, the regression tests
  that pin each rule, and a ready-to-run live probe that needs Steve's consent.

- [`deep-search-712-tenant-builds.md`](deep-search-712-tenant-builds.md) — **in-cluster builds of
  tenant source** ([#712](https://github.com/oramasearch/orama-operator/issues/712)): the build
  namespace and Pod Security, rootless BuildKit without its process sandbox, node sharing, network
  reach (the Cilium metadata deny added to build namespaces), cache isolation, registry
  credentials (Orama's Docker Hub credential reaching builds, #758), image push and pinning, and
  cross-tenant source, logs and tokens.

# Security reviews

Findings of security reviews of what the operator renders and runs, one page per review. Each page
says what was checked, how (the code read, the tests written, the live read-only reads) and what was
found, with file and line references, and links the issue filed for every defect.

- [`deep-search-711-tenant-app.md`](deep-search-711-tenant-app.md) — **a tenant's custom app as a
  foothold into Kubernetes** ([#711](https://github.com/oramasearch/orama-operator/issues/711), part of
  [lake#873](https://github.com/oramasearch/lake/issues/873)): the app pod's network reach, Pod Security,
  node isolation, ingress and identity headers, mounted Secrets and image pinning, the regression tests
  that pin each rule, and a ready-to-run live probe that needs Steve's consent.

# CRDs and rollout

The manager registers a watch for **every** kind unconditionally. A kind whose CRD is not on the
cluster is therefore not a degraded controller — the cache never syncs, `problem running manager`
is fatal, and the pod crash-loops with every other controller down with it.

That is not a theoretical risk. On 2026-09-09 the operator was down for roughly twelve hours
([#166](https://github.com/oramasearch/orama-operator/issues/166)) because two CRDs had quietly
accumulated un-applied: `lakee2etests`, then — after the first was fixed — `clerkapplications`.
`Lake/prod` kept serving throughout, so nothing paged; the control plane simply stopped
reconciling, and the only symptom was a pod nobody was looking at.

## Why they were missing

**Helm applies `chart/crds` on install only. Never on upgrade.** That is Helm's documented
behaviour, not a bug here. Both rollout paths also pass `--skip-crds`.

So a CRD added after a cluster's first install lands nowhere until someone applies it by hand, and
until this document existed nothing checked that they had. A runbook line is what failed: the step
was written down and skipped, twice, without anything noticing.

## The invariant, and the two checks that hold it

> Every kind the manager watches has a CRD in the chart, and every CRD in the chart is on the
> cluster before the manager rolls.

Each half is checked rather than remembered:

`LakeAppsReconciler` is a second controller of the existing `Lake` kind. The registration check
maps that controller to `Lake`; it watches existing `LakeApp`, Secret, Ingress, DNS and schema
resources and introduces no additional CRD or RBAC resource. Apply the updated Lake/LakeApp CRD
schemas before rolling out apps sync, so `apps.enabled` and `LakeApp.spec.sync` are retained.

There are **three** hand-maintained artifacts per kind, and forgetting any one of them produces the
same crash with the same blast radius: the CRD in `chart/crds`, the CRD on the cluster, and the
grant in the chart ClusterRole. A kind the manager watches without RBAC to list and watch it fails
its cache sync exactly like a missing CRD does.

| What could be forgotten | Check | When |
|---|---|---|
| A controller ships without its CRD in the chart | [`cmd/crd_coverage_test.go`](../../cmd/crd_coverage_test.go) | `go test ./cmd/...` — CI, no cluster |
| A chart CRD ships without an RBAC grant | same test | `go test ./cmd/...` — CI, no cluster |
| A chart CRD is absent from the cluster | [`hack/check-cluster-crds.sh`](../../hack/check-cluster-crds.sh) | before **and** after every `helm upgrade` |
| An e2e CRD leaks into the install chart | [`cmd/crd_coverage_test.go`](../../cmd/crd_coverage_test.go) | `go test ./cmd/...` — CI, no cluster |

The test also fails on the reverse — a chart CRD nothing reconciles — which catches a controller
dropped without its CRD.

```sh
make check-crds CONTEXT=orama-production-cluster-01     # report, exit 1 if any are missing
./hack/check-cluster-crds.sh orama-production-cluster-01 --apply   # apply what is missing
```

Run it **after** the upgrade as well as before. The chart can gain a CRD in the same change that
gains the controller, and only the post-check sees that.

## Fail-fast is deliberate

A missing CRD could be non-fatal: start the other controllers, mark that one degraded, keep
reconciling. That is explicitly **not** what this operator does, and the reason is that a silently
degraded control plane is worse than a loud one. A crash-looping pod is visible in one `kubectl get
pods`; a manager that started twenty controllers and quietly skipped the twenty-first looks healthy
while the resource it owns drifts.

The cost of fail-fast is exactly the incident above — all-or-nothing availability. That cost is
only acceptable because the two checks make the failure impossible to ship, which is why they are
tests and scripts rather than a paragraph.

If this is ever revisited, revisit it here, with the reasoning — do not let it change by accident of
`SetupWithManager` ordering.

## Every cluster gets every CRD

Steve, 2026-10-12: *"install the same operator everywhere... all crds"*, *"no differences"*.

There is one chart and one CRD directory, `chart/crds`, and every cluster gets all of it: the
cluster-management kinds (`KubernetesCluster`, `KubernetesClusterProvider`, `AksCluster`,
`ClusterHelmRelease`, `OramaOperator`) and the e2e test kinds (`LakeE2ETest`,
`ZettaClusterE2ETest`, `OramaCloudV1E2ETest`) included. `chart/e2e-crds/` (2026-09-09) and
`chart/management-crds/` (2026-10-12) each held CRDs applied only to some clusters; both are gone,
because a second directory is how clusters come to differ.

- The cluster-management controllers run in every cluster's operator, with no flag. A cluster
  manages others by holding the resources
  ([`infrastructure/README.md`](../infrastructure/README.md#one-operator-on-every-cluster)).
- The e2e **controllers** still run only behind `--enable-e2e-controllers` (chart `e2e.enabled`,
  off by default, which also renders their RBAC). Production runs no e2e suites, decided
  2026-09-09 after `LakeE2ETest` crash-looped the manager for twelve hours
  ([#166](https://github.com/oramasearch/orama-operator/issues/166)). That outage was a controller
  watching a kind whose CRD was missing; with every CRD on every cluster it cannot recur either way.

`cmd/crd_coverage_test.go` holds this: it fails if the chart grows a second CRD directory, if a
kind has no CRD in `chart/crds`, or if an e2e controller is registered outside its flag.

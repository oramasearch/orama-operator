# CRDs and rollout

The manager registers a watch for **every** kind unconditionally. A kind whose CRD is not on the
cluster is therefore not a degraded controller — the cache never syncs, `problem running manager`
is fatal, and the pod crash-loops with every other controller down with it.

That is not a theoretical risk. On 2026-09-09 the operator was down for roughly twelve hours
([#166](https://github.com/oramasearch/orama-operator/issues/166)) because two CRDs had quietly
accumulated un-applied: `lakee2etests`, then — after the first was fixed — `clerkapplications`.
a development Lake kept serving throughout, so nothing paged; the control plane simply stopped
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
schemas before rolling out apps sync, so `LakeApp.spec.sync` is retained. (`apps.enabled` was removed by operator#539: every Lake syncs apps.)

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
make check-crds CONTEXT=example-management     # report, exit 1 if any are missing
./hack/check-cluster-crds.sh example-management --apply   # apply what is missing
```

Run it **after** the upgrade as well as before. The chart can gain a CRD in the same change that
gains the controller, and only the post-check sees that.

## Removing or changing a field: CRDs in two steps around the roll

The API server prunes every field a CRD's schema does not declare, on write **and when it reads a
stored object**. So a CRD that drops a field changes what the *running* operator sees the moment it
is applied. Applying `chart/crds` before the new operator starts, as both install paths did, made
every removal visible to the old operator first.

That happened on 2026-10-17 ([#543](https://github.com/oramasearch/orama-operator/issues/543)):
rolling #542, which removed the Lake feature switches (`spec.apps.enabled`,
`spec.services.enabled`, ...), the old operator (`dev-cb98230`) read a development Lake with the switches
pruned, took that as "apps and services off", and for about 30 seconds rendered a Deployment
without `LAKE_APPS_WILDCARD`, `LAKE_APPS_RUNTIME_IMAGE`, `LAKE_SERVICE_DIR` or the services
volume. The pod restarted twice. Nothing was deleted only because the old controllers treated
"disabled" as "stop reconciling", not "delete".

Applying the CRDs **after** the roll instead would break the opposite case: a new operator needs a
field it reads to be in the CRD before it starts, or the API server prunes it from what the
operator reads and writes.

**So both install paths apply the CRDs twice:**

| Step | What is applied | Why it is safe |
|---|---|---|
| 1. Before the new operator starts | The chart's CRDs **widened** by everything the cluster's copies still declare: each property only the old schema has (at any depth), each enum value only it allows; a field only the new schema requires becomes optional; a version only the cluster has stays served, never as storage | It only adds. The old operator still reads every field it read; the new one already finds every field it reads |
| 2. The new operator rolls | `helm upgrade` | Old and new both see a schema that holds what they read |
| 3. Only the new operator runs | The chart's CRDs **exactly** | The dropped fields are pruned now, and nothing running reads them |

The widening is [`internal/crdrollout`](../../internal/crdrollout/crdrollout.go) (`Union`), one
implementation for both paths:

- **Managing cluster** (`build-operator.sh`, outside this repository in
  build scripts): `kubectl get crd -o json`, then
  [`go run ./hack/crdunion`](../../hack/crdunion/main.go) writes the widened CRDs, which are
  applied; then `helm upgrade` and `kubectl rollout status`; then `kubectl apply --server-side -f
  chart/crds/`. If the rollout fails the script stops with the widened CRDs in place, which is a
  state both versions run on.

  ```sh
  kubectl --context $KCTX get crd -o json > crds-before.json
  go run ./hack/crdunion -cluster crds-before.json -chart chart/crds -out crds-widened
  kubectl --context $KCTX apply --server-side --force-conflicts -f crds-widened/
  helm --kube-context $KCTX upgrade orama-operator chart/ -n orama-system -f values.yaml --set operator.image.tag=<tag>
  kubectl --context $KCTX -n orama-system rollout status deploy/orama-operator --timeout=300s
  kubectl --context $KCTX apply --server-side --force-conflicts -f chart/crds/
  ```

- **Leaf** (`OramaOperator`, `internal/controller/leaf_operator_installer.go`): before the
  install or upgrade it applies each chart CRD as `crdrollout.Union` of the chart's and the
  leaf's copy. On every later pass, once the operator Deployment runs only the requested version
  (every pod of the current template, all ready, the image tag equal to `spec.version`), it applies
  exactly each chart CRD whose leaf copy is still wider. A leaf already equal to the chart is not
  written. Third-party charts installed by `ClusterHelmRelease` keep their CRDs applied exactly
  before the upgrade: their projects version their APIs themselves.

Both steps use the same field manager (`kubectl` on the managing cluster, `orama-operator-management`
on a leaf), so the exact apply removes what the widened one added.

`TestRemovingACRDFieldIsInvisibleToTheOperatorStillRunning` (envtest, a real API server) proves
it: a stored object keeps the dropped field under the widened CRD, a new field is accepted at once,
and only the exact apply prunes the old one.

### What this does not make safe

Widening keeps a field **visible**; it cannot make two operators agree on what it **means**.

- **A type change** (`string` to `object`) cannot be widened: the new type wins and stored values
  of the old type fail. Never change a field's type; add a field with a new name, stop reading the
  old one, and remove it in a later change.
- **A change of meaning** under the same name (a default that flips, a value whose effect changes)
  is read both ways during the roll. Use a new name.
- **A field a committed resource still sets.** After step 3, `kubectl apply` of a file under
  `config/clusters/` that sets a removed field silently loses it, or fails under strict validation.
  Remove the field from every committed resource in the same pull request.
- **A controller that deletes on disable.** If a removed field turned something off, its controller
  must not have deleted on "off" in the release before; the widening keeps that release reading
  the field, but a by-hand `kubectl apply -f chart/crds/` would not.

### The check: `TestNoCRDFieldThePreviousReleaseReadsDisappearsUnnoticed`

[`cmd/crd_field_removal_test.go`](../../cmd/crd_field_removal_test.go) compares `chart/crds` with the
CRDs of the newest release tag behind `HEAD` (`git describe --tags --match 'v[0-9]*'`). Every
field the release declares that the chart no longer does, every field whose type changed, every
removed version and every removed CRD fails the test until it is named in
`acknowledgedCRDChanges`, with why it is safe by the rules above. A type change should never be
acknowledged. When a release is tagged, the entries no longer differ from it and the test asks
for them to be removed, so the list holds only what is in flight.

The test compares with the last release, not with what a cluster runs: the development clusters
run `dev-<sha>` between releases. The roll order above is what protects a `dev` to `dev` roll;
the test makes every removal deliberate and reviewed.

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



There is one chart and one CRD directory, `chart/crds`, and every cluster gets all of it: the
cluster-management kinds (`KubernetesCluster`, `KubernetesClusterProvider`, `AksCluster`,
`ClusterHelmRelease`, `OramaOperator`) and the e2e test kinds (`LakeE2ETest`,
`ZettaClusterE2ETest`, `OramaCloudV1E2ETest`) included. `chart/e2e-crds/` (2026-09-09) and
`chart/management-crds/` (2026-10-12) each held CRDs applied only to some clusters; both are gone,
because a second directory is how clusters come to differ.

- The cluster-management controllers run in every cluster's operator, with no flag. A cluster
  manages others by holding the resources
  ([`infrastructure/README.md`](../infrastructure/README.md#one-operator-on-every-cluster)).
- The e2e controllers run in every cluster's operator too, with no flag. A cluster runs an e2e
  suite only when someone creates one of those resources on it; production runs none because none
  is created there. The earlier gate (`--enable-e2e-controllers`, chart `e2e.enabled`, decided
  2026-09-09 after `LakeE2ETest` crash-looped the manager for twelve hours,
  [#166](https://github.com/oramasearch/orama-operator/issues/166)) guarded against a controller
  watching a kind whose CRD was missing. With every CRD on every cluster that cannot happen, and
  the gate was a way for clusters to differ, so it is gone.

`cmd/crd_coverage_test.go` holds this (`TestNoClusterDiffersFromAnother`,
`TestTheChartHasOneCRDDirectoryWithEveryKind`): it fails if the chart grows a second CRD directory,
if a controller is registered inside a condition, if a flag turns controllers on or off, or if the
chart's RBAC or manager arguments render conditionally.
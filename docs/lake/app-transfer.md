# App transfer

lake meters the bytes each app receives and sends through the ingress as `transfer`
([lake#661](https://github.com/oramasearch/lake/issues/661), part of the metering epic
[lake#658](https://github.com/oramasearch/lake/issues/658)). That traffic never passes through
lake: it goes from the ingress controller to the app's pods. The operator is the only writer of the
numbers: rows in the Lake's control Postgres, table `app_transfer` (lake migration
`0081_app_transfer`, guard `app_transfer_guard`). lake's owner loop charges each closed hour once in
the app's workspace Zetta (lake `docs/api/metering.md#app-traffic`).

Code: `internal/controller/lake_app_transfer.go` (the pass) and `lake_app_transfer_pgx.go` (the SQL).

## Where the bytes come from

The clusters run **ingress-nginx** (chart `ingress-nginx-4.15.1`, controller `v1.15.1` on the dev
AKS cluster). It counts every request per Ingress in two Prometheus histograms on its metrics port,
`10254`:

| Metric | Labels used | Counts |
|---|---|---|
| `nginx_ingress_controller_request_size_sum` | `namespace`, `ingress` | `$request_length`: what the client sent (request line, headers, body) → `in_bytes` |
| `nginx_ingress_controller_response_size_sum` | `namespace`, `ingress` | `$bytes_sent`: what was sent back (headers, body) → `out_bytes` |

Every series of an Ingress (its hosts, paths, methods, statuses) is summed. These metrics are **off
by default** (`--enable-metrics` defaults to false; the chart's `controller.metrics.enabled` is
false). The dev cluster's controller had them off when this was written (it answered only Go and
process metrics), so turning them on is a deploy step, done once per cluster:

```sh
helm upgrade ingress-nginx ingress-nginx/ingress-nginx -n ingress-nginx --version 4.15.1 \
  --reuse-values --set controller.metrics.enabled=true
```

This is the least invasive source: it changes one flag on the controller that already carries the
traffic. No sidecar, log shipping or proxy goes in front of the apps. Until it is on, the pass logs
`no ingress controller reports per-Ingress byte counters` and writes nothing.

## The pass

On the Lake's control-rows poll, with the workload resources pass (at most once every 30 seconds per
Lake, which every Lake with its control Postgres runs):

1. List the Lake's `LakeApp`s with a `spec.sync` (the apps sync's). Each app's Ingress is
   `lakeAppWorkloadObjectName(app)` in `lakeAppNamespace(app)`, as `reconcileLakeAppIngress` creates
   it; the map gives the app id (`spec.sync.id`) and its workspace (`spec.sync.workspaceId`).
2. List the ingress-nginx controller pods in every namespace (`app.kubernetes.io/name=ingress-nginx`,
   `app.kubernetes.io/component=controller`) and read `http://<pod IP>:10254/metrics` from each
   running one (5-second timeout, 32 MiB limit). No new RBAC: pods get/list is already granted.
3. For each app Ingress on each controller pod, take the increase since the last pass. The counters
   belong to the controller pod and start from zero when it restarts, so the operator keeps the last
   value per pod and Ingress in memory:
   - a value seen before: the difference, or the whole value if it went down (a restart);
   - a pod or Ingress not seen before: the whole value if the pod started after the operator began
     reading, or if the pod was already known (a new Ingress); otherwise it is the **baseline** (what
     the controller counted before the operator started is not this pass's traffic).
4. Add each app's increase to its row for the current UTC hour, all apps in **one statement**
   (`addAppTransferSQL`, `INSERT … SELECT FROM unnest(…) ON CONFLICT (app_id, hour) DO UPDATE` adding
   to the row), and delete rows older than eight days (`pruneAppTransferSQL`; lake charges an hour
   within seven).
5. Only after the write succeeds do the remembered counters advance. A failed write or an unreadable
   controller pod keeps the previous values, so the next pass counts those bytes then, once.

A schema older than `0081_app_transfer` skips the write and logs it, and the counters advance as the
baseline.

## What it does not count

- Bytes moved between the operator's last read before it restarts and its first read after: at most
  one poll, since that first read is the baseline.
- The Lake sign-in handoff (`auth-url`) of a protected app is a subrequest to Lake.
- Ingresses that are not an app of this Lake (Lake's own Ingress, other Lakes' apps).

## Proof

- `go test ./internal/controller/ -run 'AppTransfer|ParseIngressBytes'`: parsing an ingress-nginx
  answer (two series of one Ingress summed, another Ingress apart, metrics off detected); the
  baseline, each pass's increase into the app's hour, a counter restart, an unreadable controller
  counted on the next pass, a failed write retried without double counting, retention; a controller
  pod that started after the operator counted whole, and a schema before `0081` skipped.
- `LAKE_SERVICES_TEST_POSTGRES=1 go test ./internal/controller/ -run TestServicesPgxStoreAgainstPostgres`:
  the SQL against the vendored schema (`checkAppTransferPgx`: passes add up, old rows pruned, the
  missing-table error).
- lake's `OperatorContract.EveryStatementTheOperatorRunsPreparesAgainstTheSchema` prepares both
  statements against lake's schema.

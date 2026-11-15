# Status pages: external checks, alerts and a public page

A `StatusPage` checks public endpoints from the operator that holds it, sends an alert each time a
component changes state, and writes a static page to a bucket that Cloudflare serves. It was built
for lake.live ([liquid-bridge#14](https://github.com/oramasearch/liquid-bridge/issues/14)); the
objectives and the incident process it serves are techops-docs'
[`docs/lake/slos.md`](https://github.com/oramasearch/techops-docs/blob/main/docs/lake/slos.md) and
[`docs/lake/status-page.md`](https://github.com/oramasearch/techops-docs/blob/main/docs/lake/status-page.md).
Field reference: [`crd-reference.md`](../crd-reference.md#statuspage-separate-crd-namespaced--external-checks-alerts-and-a-public-status-page).
Why it is shaped this way: [`DESIGN.md`](../../DESIGN.md) decision 103.

## How it works

```
managing cluster (eastus)                          Cloudflare
┌────────────────────────────┐   HTTPS, like a    ┌──────────────────────────────┐
│ orama-operator             │   customer         │ eu.lake.live → prod-02 (EU)  │
│  StatusPage/lake-live ─────┼───────────────────►│ us.lake.live → prod-03 (US)  │
│   every 60 s: 15 checks    │                    │ lake.live    → prod-02       │
│   change of state ─► email │                    │                              │
│   status.json, index.html ─┼──── S3 PUT ───────►│ R2 lake-live-status          │
└────────────────────────────┘                    │   served on status.lake.live │
                                                  └──────────────────────────────┘
```

- **Checks** run from the operator's own pod, all at once, every `intervalSeconds`. A check fails
  `failureThreshold` runs in a row (3) before it counts as failing; one pass clears it.
- **Components** group checks. Every check that ran passing: `Operational`. Every one failing:
  `Outage`. Some: `Degraded`. None could run: `Unknown`. A check whose Secret or key is missing
  does not run and is listed under `uncheckedChecks`; it never fails a component.
- **Alerts** go out on each change of a component's state (Operational → Outage, Outage →
  Operational, …), never on the first state after creation. An alert that cannot be delivered is
  retried every run and `AlertsDelivered` turns False with the reason.
- **The page** is `index.html` (rewritten when the spec changes) and `status.json` (rewritten every
  run). The browser reads `status.json` once a minute. When its `generated` time is older than
  three intervals plus a minute, the page says **Status unknown: the checks have not reported
  since …**: a stopped operator never leaves a green page behind. `status.json` holds titles,
  states, dates and the 90-day history only, never a URL or a failure message.
- **Status writes** happen only on a change of state, a new UTC day in a component's history, or a
  condition change. A steady page makes no API-server writes.

Why the checks run on the managing cluster: they must not share the failure domain of what they
check. From there they leave Azure's production clusters and come back through Cloudflare, so they
see DNS, the edge, TLS, the ingress and the pod as a customer does.

## The lake.live page

[`config/clusters/status/lake-live.yaml`](../../config/clusters/status/lake-live.yaml), applied to
the managing cluster in `orama-system`:

| Component | Checks | Passing answer |
|---|---|---|
| Europe API, United States API | `/readyz`; `/v1/server_info`; `/v1/workspace_engine_info` on the probe workspace | `ok: true`, certificate valid ≥ 14 days; `ok: true` and `value.region.name` = `swedencentral` / `eastus`; `ok: true` (one real call through to a workspace Zetta, lake#1097) |
| Europe Agents, United States Agents | `/v1/receive_agent_tasks?agent=…&wait_ms=0` with the agent's bus credential | `ok: true` (reads the inbox, claims nothing) |
| Europe Web app, United States Web app | `/` and `/sign-in` with `Accept: text/html` | body has `<title>Lake Console</title>` |
| Order sheet (lake.live) | `https://lake.live/` | body has `<title>Lake — Get Lake`, certificate valid ≥ 14 days |

These are the health checks of the umbrella's gate (`/readyz`, `server_info`, a workspace Zetta
call, the agent bus), run every minute on production.

## What Steve applies

Nothing here is applied by an agent. Every step is a change to the managing cluster and needs
Steve's consent.

1. **The operator version with `StatusPage`.** Merge the pull request, cut the operator release, and
   roll it to the managing cluster the usual way (`make check-crds CONTEXT=orama-production-cluster-01`,
   then the helmfile roll), and to the leaves through each `config/clusters/<cluster>/operator.yaml`.
   Every cluster gets the kind and the controller; only the managing cluster gets a `StatusPage`.
   Check the CRD is there: `kubectl --context orama-production-cluster-01 get crd statuspages.orama.cloud`.
2. **The Secrets**, in `orama-system` on the managing cluster. None is in a file.
   - `lake-live-probe-eu` and `lake-live-probe-us`: `token` (a Lake API key of a probe account that
     is a member of one test workspace in that region; it needs nothing more), `workspace_id` (that
     workspace), `agent` (a library agent's name), `agent_credential` (that agent's bus credential).
     Creating the probe account and workspace on lake.live is a production write Steve makes.
   - `lake-live-status-email`: `host` `smtp.resend.com`, `port` `465`, `username` `resend`,
     `password` (Orama's Resend key, the one in `lake/lake-prod-resend`, key `api_key`; `lake.live`
     is verified in Resend), `from` `lake.live status <status@lake.live>`, `to` (comma-separated).
   - Optional, `lake-live-status-webhook` with `url`, and `alerts.webhook.secretRef` added in the file,
     once the paging channel is chosen (decision 66, [liquid-bridge#42](https://github.com/oramasearch/liquid-bridge/issues/42)).

   ```sh
   kubectl --context orama-production-cluster-01 -n orama-system create secret generic lake-live-status-email \
     --from-literal=host=smtp.resend.com --from-literal=port=465 --from-literal=username=resend \
     --from-literal=password="$RESEND_KEY" --from-literal=from='lake.live status <status@lake.live>' \
     --from-literal=to='<address>'
   kubectl --context orama-production-cluster-01 -n orama-system create secret generic lake-live-probe-eu \
     --from-literal=token="$EU_PROBE_TOKEN" --from-literal=workspace_id=<eu workspace> \
     --from-literal=agent=<agent> --from-literal=agent_credential="$EU_AGENT_CREDENTIAL"
   # and lake-live-probe-us the same way with the US values
   ```

   The page can be applied before the probe Secrets exist: those checks show as not checked and
   the rest run.
3. **The file**: `kubectl --context orama-production-cluster-01 apply -f config/clusters/status/lake-live.yaml`
   (a server dry-run first: `--dry-run=server`). The bucket token in
   `orama-system/cloudflare-api-token` already creates R2 buckets there; attaching the custom domain
   also needs it to be allowed to add an R2 custom domain in the `lake.live` zone. If it is not, the
   bucket's `status.message` says so, and the token's permissions are Steve's to change.

## How to read it

```sh
kubectl --context orama-production-cluster-01 -n orama-system get statuspage lake-live
kubectl --context orama-production-cluster-01 -n orama-system get statuspage lake-live \
  -o jsonpath='{range .status.components[*]}{.name}{"\t"}{.state}{"\t"}{.since}{"\t"}{.message}{"\n"}{end}'
kubectl --context orama-production-cluster-01 -n orama-system get statuspage lake-live \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
kubectl --context orama-production-cluster-01 -n orama-system get cfr2 lake-live-status \
  -o jsonpath='{.status.ready} {.status.customDomain} {.status.customDomainStatus} {.status.message}{"\n"}'
curl -sS https://status.lake.live/status.json | jq '{generated, phase, components: [.components[] | {name, state}]}'
```

| You see | It means |
|---|---|
| `PHASE Operational`, `OPERATIONAL 9/9` | Every component passes |
| A component `Outage` with `message` `…readyz: GET https://eu.lake.live/readyz: status 503, want 200 (error store_unavailable)` | That region's lake cannot open its store: techops-docs `docs/lake/runbooks/lake-pod-restarts.md` |
| `Outage` on every component of one region with Cloudflare `52x` statuses | That region's origin: `docs/lake/runbooks/cloudflare-edge.md` |
| Every component of both regions failing at once | DNS or Cloudflare: `docs/lake/runbooks/dns.md` |
| `workspace-zetta` failing while `readyz` passes | lake answers but cannot reach its workspace Zettas (lake#1097's failure) |
| `uncheckedChecks` and `ChecksConfigured=False` | A probe Secret or key is missing; nothing failed |
| `Published=False` | The bucket refused the write (its message); the checks and alerts keep running |
| `AlertsDelivered=False DeliveryFailed` | The SMTP server or webhook refused; the alert is retried every run |
| The page says *Status unknown: the checks have not reported since …* | The operator on the managing cluster stopped running the checks: look at its pod and logs |

To prove an alert end to end, change one check's expectation in a copy of the resource under
another name (for example `equals: wrong`), apply it, and wait three intervals: the component turns
`Outage` and the alert arrives. Delete the copy afterwards. A copy has no `publish`, so it writes no
page.

## Removing it

Delete the `StatusPage`: the checks and alerts stop and the page turns Unknown after three minutes.
Deleting the `CloudflareR2Bucket` detaches `status.lake.live` and revokes the bucket token; the
bucket and its objects stay (`deletionPolicy: Retain`).

## Limits and what is not built

- **One vantage point**, the managing cluster, in Azure `eastus`: the same Azure region as the US
  production cluster (`orama-production-cluster-03`). An `eastus` regional outage can take both the
  US region and its checks, and then the page goes Unknown rather than Outage. A second vantage,
  outside Azure and Cloudflare, is a follow-up ([#955](https://github.com/oramasearch/orama-operator/issues/955)).
- **The page shares Cloudflare with lake.live.** When Cloudflare itself is down, both are; the
  fallback channel is in techops-docs `docs/lake/status-page.md`.
- **No incident posts or subscriber feed yet.** The page shows live state and 90 days of history.
  Incident files and an Atom feed (techops-docs' proposal) are a follow-up ([#955](https://github.com/oramasearch/orama-operator/issues/955)).
- **The agent check holds a library agent's bus credential**, because `receive_agent_tasks` answers
  only that agent. A credential-free health read is asked of lake in
  [lake#1164](https://github.com/oramasearch/lake/issues/1164); until then Steve may leave
  `agent`/`agent_credential` out and the agent components show as not checked.
- **No server-side SLIs.** Latency and request-level availability need `/metrics` scraped
  ([operator#15](https://github.com/oramasearch/orama-operator/issues/15)).

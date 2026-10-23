# The Cloud v1 uploads bucket and import consumer

[orama-bridge#240](https://github.com/oramasearch/orama-bridge/issues/240) moves uploads and crawl
exports to a Cloudflare R2 bucket provisioned by the operator. The dashboard signs upload URLs
([#238](https://github.com/oramasearch/orama-bridge/issues/238)); the consumer discovers objects by
listing the bucket ([#239](https://github.com/oramasearch/orama-bridge/issues/239)).

## The stack's bucket

With Cloudflare and the dashboard enabled, every `OramaCloudV1` owns a `CloudflareR2Bucket` named
`<cr>-uploads`. Its account token comes from the stack's Cloudflare Secret (`token`, `accountID`).
The bucket controller mints a token scoped to this one bucket, with permission to read, write, list
and delete objects, and writes `<cr>-uploads-r2` with keys `endpoint`, `bucket`, `region` (`auto`),
`accessKeyId` and `secretAccessKey`. The dashboard, crawler and consumer share this bucket token.
The account token remains with the bucket controller.

The bucket permits browser `PUT` requests from `https://<dashboard.host>` and every
`https://<dashboard.extraHosts>` origin, with any request header and a 3600-second preflight cache.
The browser and customer scripts upload directly to `https://<accountID>.r2.cloudflarestorage.com`.
These requests bypass the stack's ingress and Cloudflare proxy request-size limit; a single R2 PUT
supports up to 5 GiB. The dashboard's upload limit remains 100 MiB.

Objects are import staging. The consumer deletes them after import; an `orama-expire` lifecycle rule
expires remaining objects after seven days, including uploads that are never imported. The bucket's
`deletionPolicy` is `Delete`, so it goes with the stack. `<cr>-legacy-indexes` is unchanged; backup
buckets and archives have their own policies.

The bucket controller reads CORS and lifecycle settings before writing and writes only on a change.
A 404 means the configuration has not been set yet, so the controller writes the desired settings.
CORS is the whole desired configuration; lifecycle updates preserve rules other than `orama-expire`.
Removing either optional field leaves that provider configuration alone. Existing bucket resources
without these fields make no new CORS or lifecycle API calls. The account token needs the same
Workers R2 Storage edit permission as bucket creation; a refused configuration call makes the bucket
not Ready and records the Cloudflare error.

## Credential wiring

Every variable below uses a `secretKeyRef` into `<cr>-uploads-r2`.

| Secret key | Dashboard | Crawler worker (boto3) | Import consumer |
|---|---|---|---|
| `endpoint` | `S3_ENDPOINT` | `AWS_ENDPOINT_URL_S3` | `S3_ENDPOINT` |
| `bucket` | `S3_BUCKET` | `S3_BUCKET_NAME` | `S3_BUCKET` |
| `region` | `S3_BUCKET_REGION` | `AWS_DEFAULT_REGION` | `AWS_REGION` |
| `accessKeyId` | `AWS_ACCESS_KEY_VALUE` | `AWS_ACCESS_KEY_ID` | `AWS_ACCESS_KEY_ID` |
| `secretAccessKey` | `AWS_SECRET_ACCESS_KEY_VALUE` | `AWS_SECRET_ACCESS_KEY` | `AWS_SECRET_ACCESS_KEY` |

The dashboard's explicit `env` entries override placeholders in `dashboard.envFrom` when the bucket
Secret is present. Its five references are optional, so a missing bucket Secret leaves the envFrom
values available and does not prevent the dashboard from starting. The bucket Secret joins the
dashboard and consumer credential hashes, so rotating it rolls their pods. The crawler worker is not spawned yet ([operator#12](https://github.com/oramasearch/orama-operator/issues/12));
its Job template should use `uploadsBucketEnv(oc, crawlerUploadsEnv)`. Boto3 reads the endpoint
variable directly and uses its default addressing.

## The bucket worker

`spec.components.importConsumer` runs `<cr>-import-consumer`, a Deployment using the configured
private `oramasearch/airbyte-fargate-consumer` image, with `deno run -A src/bucket-worker.ts`.
It lists `<cr>-uploads` every ten seconds and runs the per-object import for new files and crawl
exports. A curl upload starts an import when the object appears, without an `upload-complete` call.
No AWS queue, event rule or IAM user is needed for this stack.

The worker has exactly one replica and uses the `Recreate` deployment strategy, so a rollout stops
the old worker before starting its replacement. Duplicate-import protection lives within one
process. `concurrency` (default two, range 1–16) controls simultaneous imports within that process.
Each child process gets a V8 heap cap derived from the container memory limit less the worker's
128 MiB share: three quarters of the remainder divided by concurrency, at least 64 MiB.

The other environment variables are:

- `DASHBOARD_URL=http://<cr>-dashboard:3000`, the stack's own Service.
- `PRIVATE_API_KEY`, from the same Secret key the dashboard reads through `dashboard.env` or
  `dashboard.envFrom`, sent as `x-api-key` on get-source and status callbacks.
- `WORKER_CONCURRENCY`, `IMPORT_MAX_OLD_SPACE_MB`, `POLL_INTERVAL_SECONDS=10`, `PORT=8080`, and
  `NODE_ENV=production`.

The worker asks get-source for the destination, writes documents with the project's write key and
reports file or crawl status to the dashboard. TCP probes use the health listener on 8080. A
600-second termination grace period lets imports finish; interrupted objects remain for discovery
by the next worker. Public HTTPS egress already covers R2 and the engine's public writer host.

The consumer is off by default and requires an image, Cloudflare and the dashboard enabled. Both
admission and reconciliation enforce these dependencies. Reconciliation waits for all five bucket
Secret keys and a Secret-backed dashboard API key. Configuration errors report `Ready=False`
(`ReconcileError`) in the last component step, so other components can still reconcile.

## Cutover

Prod 1's Fargate rule keeps importing Prod 1's AWS bucket for Prod 1. The two systems use separate
buckets. Once `app.orama.com` serves Prod 2, the dashboard signs links for `<cr>-uploads`, so new
uploads reach R2 and its bucket consumer. No new dashboard uploads reach Prod 1's bucket after that
switch; its existing staging objects were already imported and need no migration.

Release, image selection, set changes and live upload verification happen after the code is merged.
Field reference: [crd-reference.md](crd-reference.md#speccomponentsimportconsumer). Network paths:
[network-sandbox.md](infrastructure/network-sandbox.md#orama-cloud-v1-stack-namespace).

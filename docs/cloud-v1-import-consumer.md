# The Cloud v1 import consumer in the cluster

[operator#446](https://github.com/oramasearch/orama-operator/issues/446). Decision: Juan, 2026-10-13,
on [orama-bridge#160](https://github.com/oramasearch/orama-bridge/issues/160): *"After the cutover the
consumer moves into the cluster and keeps reading the same bucket."* Epic:
[orama-bridge#137](https://github.com/oramasearch/orama-bridge/issues/137).

Status: implemented in the operator and in the consumer's queue-worker mode, tested with fakes. Not
running anywhere: no image is published, and the AWS resources below do not exist yet.

## What it replaces

On Prod 1, crawls and file uploads land as objects in the S3 bucket `airbyte-exports-prod`
(account `007522357987`, `us-east-1`). An EventBridge rule on object creation starts one ECS Fargate
task of `airbyte-fargate-consumer` per object, with `S3_BUCKET` and `S3_KEY` set. The task:

1. asks the dashboard where the object goes: `GET <DASHBOARD_URL>/api/private/v1/get-source?sourceId=<index>`;
2. writes the documents to the engine writer that answer names (`cluster.writer_url`), with the
   project's write key (`Authorization: Bearer <write_api_key>`), through a temporary index and
   `replace-index` for a full refresh;
3. reports progress back: `update-crawl-status` for crawls, `update-file-status` (`indexing`, then
   `synced` or `failed`) for uploads;
4. sends `PRIVATE_API_KEY` as `x-api-key` on every dashboard call.

The source is [`oramasearch/airbyte-fargate-consumer`](https://github.com/oramasearch/airbyte-fargate-consumer)
(private; default branch `develop`).

## What runs instead

`spec.components.importConsumer` on an `OramaCloudV1` runs one Deployment, `<cr>-import-consumer`,
from the same image in its queue-worker mode (`deno run -A src/sqs-worker.ts`, added in
[airbyte-fargate-consumer#13](https://github.com/oramasearch/airbyte-fargate-consumer/pull/13)). It
long-polls an SQS queue that receives the bucket's object-created events and, for each object, runs
the unchanged `src/main.ts` in its own process with `S3_BUCKET` and `S3_KEY` set. Steps 1 to 4 above
are the same code; only the trigger changes. Two differences, both deliberate:

- `DASHBOARD_URL` is the stack's own dashboard Service, `http://<cr>-dashboard:3000`, not
  `https://app.orama.com`, so the consumer cannot reach Prod 1 while DNS moves.
- `PRIVATE_API_KEY` is read from the same Secret key the dashboard takes it from
  (`dashboard.env` or `dashboard.envFrom`, resolved as for `CRON_SECRET`), so the key the consumer
  sends is always the key the dashboard checks. There is no second reference to keep in step.

Queue handling, in the consumer (`src/lib/sqs-worker.ts`): a message is deleted once its object has
been imported, whether the import succeeded or failed (the Fargate path runs each object once and
never retries, and a failed import has already reported `failed`); its visibility is extended while
the import runs; an import killed because the pod is stopping leaves the message for redelivery; a
message that is not an object-created event for the configured bucket is logged and deleted.

Field reference: [crd-reference.md](crd-reference.md#speccomponentsimportconsumer). Network paths:
[network-sandbox.md](infrastructure/network-sandbox.md#orama-cloud-v1-stack-namespace).

## What must exist in AWS first — not created by the operator, and not created by this change

Prod 1's AWS account is read-only for the migration (Juan, 2026-10-07): nothing below has been
created or changed. Each item needs its owner's explicit approval, and is recorded in the bridge's
`docs/projects/orama-cloud-v1/production-changes.md` when done. Owner: **Juan** (Prod 1 AWS,
`oramacore-aws` / `terraform-aws`), with **Steve** approving the cost and the new IAM user.

| # | Resource | Exact shape | Why |
|---|---|---|---|
| 1 | SQS queue, e.g. `orama-prod2-import` in `us-east-1`, account `007522357987` | Standard queue (not FIFO). Visibility timeout 300 s (the worker extends it while an import runs). Message retention 4 days or more. Receive wait time 20 s. SSE with the SQS-owned key. | The consumer's input. Its URL is `spec.components.importConsumer.queueURL`. |
| 2 | Dead-letter queue, e.g. `orama-prod2-import-dlq`, and a redrive policy on queue 1 | `maxReceiveCount` 5. | A message the worker cannot even delete (a crash loop) stops coming back and stays visible for inspection. The worker deletes failed imports itself, so in normal operation the DLQ stays empty. |
| 3 | The bucket's events delivered to queue 1 | **Either** (a) a new EventBridge rule on the default bus with the same event pattern as the existing Fargate rule (source `aws.s3`, detail-type `Object Created`, `detail.bucket.name` = `airbyte-exports-prod`, and the same key filter if it has one), targeting queue 1 with the event as the message body; **or** (b) an S3 event notification on `airbyte-exports-prod` for `s3:ObjectCreated:*` (same prefix filter as the Fargate rule) to queue 1. (a) is preferred: EventBridge delivery is already on for the bucket, it adds a rule without touching the bucket's configuration, and it delivers the key exactly as the Fargate task receives it. The worker reads both message shapes. | Without it the queue stays empty. |
| 4 | Queue policy on queue 1 | Allow `sqs:SendMessage` from principal `events.amazonaws.com` (for (a)) or `s3.amazonaws.com` (for (b)), with `aws:SourceArn` = the rule's ARN (a) or `arn:aws:s3:::airbyte-exports-prod` (b) and `aws:SourceAccount` = `007522357987`. | SQS refuses the delivery otherwise. |
| 5 | IAM user, e.g. `orama-prod2-import-consumer`, with one access key | Inline policy, nothing else: `sqs:ReceiveMessage`, `sqs:DeleteMessage`, `sqs:ChangeMessageVisibility`, `sqs:GetQueueAttributes` on queue 1's ARN; `s3:GetObject` on `arn:aws:s3:::airbyte-exports-prod/*`. Add `s3:ListBucket` on the bucket and `s3:DeleteObject` on its objects only if Airbyte sources are kept (they are being removed, bridge decisions of 2026-10-16, 12a). | The consumer's identity. An access key, because the AKS clusters have no AWS workload identity. |
| 6 | The key, stored | The key pair goes into the migration credential file on Steve's host (`~/.config/orama-migration/prod2/secrets.json`, entry e.g. `prod2-import-consumer-aws`, fields `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`), with its export in `env.sh` and its row in the ledger, per the umbrella `CLAUDE.md`; never in 1Password, never in a repository. From there it reaches the cluster as Secret `v1-prod-import-consumer-aws` through the set's copied Secrets, like the other `v1-prod-*` Secrets. | `spec.components.importConsumer.awsCredentialsSecretRef`. |
| 7 | The image | `oramasearch/airbyte-fargate-consumer` on Docker Hub, **created private before the first push** (org rule), built from the consumer's merged queue-worker change and tagged; `spec.components.importConsumer.image` names it. No such Docker Hub repository exists today (checked 2026-10-21). | The operator has no default image for the consumer. |

The Fargate rule's exact event pattern and key filter are in `terraform-aws`
`environments/prod/main.tf`; read them from there (or from the live rule, read-only) when writing
item 3, so both paths match the same objects.

**A warning about the consumer repository.** `airbyte-fargate-consumer` has an enabled GitHub
Actions workflow (`.github/workflows/deploy.yml`) that, on every push to `develop` or `main`, builds
the image, pushes it to ECR in account `007522357987` and deploys a new task definition to the ECS
service. Merging the queue-worker change into `develop` therefore changes Prod 1's ECS deployment,
even though the Fargate command is unchanged. Merge it only with Juan's approval for that Prod 1
change, or after the workflow question is settled (the org has no CI; disabling it is Steve's call).

## The switch

The Fargate path and this consumer must never both import the same objects: each object would be
imported twice, written to the engine twice and reported to the dashboard twice. So:

1. Items 1 to 7 exist; the Secret and the image are in place. The consumer stays `enabled: false`.
2. Stop the Fargate path's trigger for the bucket (disable the EventBridge rule's ECS target), and in
   the same change start delivery to queue 1 (item 3). Prod 1 AWS change: Juan's approval.
3. Set `enabled: true` in the set file (`config/clusters/sets/v1-prod.yaml`, member `v1-eu`), apply
   that file, and commit it.
4. Check: `<cr>-import-consumer` is `Ready`; a test upload reaches `synced` in the dashboard; the
   queue's `ApproximateNumberOfMessagesVisible` returns to zero; the DLQ is empty.

Objects written between steps 2 and 3 wait in the queue and are imported when the consumer starts.
Rolling back is the reverse: `enabled: false`, then the Fargate target back on.

This is after the cutover (the issue's own condition). Until then the Fargate task keeps running and
follows `app.orama.com` to Prod 2.

## Open question for Juan and Steve

Decision 12a of 2026-10-16 (Steve's rule: only Clerk, Cloudflare and Resend stay outside the
cluster; "data and processing: none stay outside") came after Juan's decision of 2026-10-13 that the
consumer keeps reading the S3 bucket. This design keeps S3 and SQS in AWS, as #446 says. If 12a means
the bucket itself must move (for example to an R2 bucket the operator already provisions, or an
in-cluster object store), the consumer's queue reading is what changes; the Deployment, the key
handling and the network paths stay. That is a decision for Juan and Steve, not made here.

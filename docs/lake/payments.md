# Payments for a Lake (`spec.payments`)

[operator#639](https://github.com/oramasearch/orama-operator/issues/639), part of the Stripe epic
([orama-bridge#199](https://github.com/oramasearch/orama-bridge/issues/199)). lake takes payments
through Stripe when it has `LAKE_STRIPE_SECRET_KEY` and `LAKE_STRIPE_WEBHOOK_SECRET`, and through its
manual provider when it has neither (lake `src/api/billing_capabilities.cpp`,
`environment_payment_providers`). With `spec.payments`, the operator keeps the Lake's webhook
endpoint and Lake's products in a Stripe account and gives the Lake both variables. No key or
signing secret is copied by hand.

Before this field every Lake read both variables from one shared Secret, `lake-prod-stripe`, through
`spec.env`. One webhook signing secret can match only one endpoint, one key cannot be a test key for
nonprod and a live key for production, and the endpoint was made by hand.

```yaml
spec:
  host: testing.lake.example        # required with spec.payments
  billing: {enabled: true}          # required with spec.payments
  payments:
    mode: test                      # test or live
    secretRef: {name: stripe}       # a Secret in spec.namespace
```

The API server refuses `spec.payments` without `spec.host`, because the webhook endpoint is at
`https://<host>/billing/stripe/webhook`.
It also refuses `spec.payments` unless `spec.billing.enabled` is true (below): a Lake with billing
off ignores its Stripe keys and answers the webhook 404, so the endpoint would fail every delivery.

## Billing (`spec.billing`)

[operator#662](https://github.com/oramasearch/orama-operator/issues/662). Whether a Lake bills at
all is lake's `LAKE_BILLING` (lake `src/main.cpp`, `docs/api/billing.md`): on only when it is `on`,
`true` or `1`. Off, lake registers no billing capabilities, builds no payment provider (Stripe keys
are ignored), answers `/billing/stripe/webhook` 404 and shows no Billing page.

```yaml
spec:
  billing:
    enabled: true                   # LAKE_BILLING=on
```

| `spec.billing` | The Lake gets | `spec.env`'s `LAKE_BILLING` |
|---|---|---|
| `enabled: true` | `LAKE_BILLING=on` | Ignored, and the operator logs that it is |
| `enabled: false` | No `LAKE_BILLING`: billing off | Ignored, and logged |
| Unset | Nothing from the operator | Passed through unchanged |

- **On-prem and enterprise installs leave it unset** (or `enabled: false`), and bill nothing.
- **Billing does not need payments.** A Lake can bill through lake's manual provider with no
  `spec.payments`; `Lake/prod` on the managing cluster does.
- **Payments need billing**: the CEL rule above.
- **Unset passes `spec.env` through** so nothing changes before each set file moves: a member that
  bills through a `spec.env` entry keeps billing until its file says `billing: {enabled: true}`
  ([`config/clusters/README.md`](../../config/clusters/README.md#moving-a-member-to-specbilling)).

Billing is the same field on every Lake; it is not a feature switch in the sense of
[every Lake runs every feature](README.md#every-lake-runs-every-feature). It says whether this
install charges anyone, which is a property of the install, like `ingressEnabled`, and each Lake
states its value in its own spec or set member.

The code is `internal/controller/lake_billing.go`.

The code is `internal/controller/lake_payments.go`. The payment kinds it uses are described in
[`../infrastructure/payments.md`](../infrastructure/payments.md).

## The Secret

`spec.payments.secretRef` names a Secret in the Lake's namespace with three keys:

| Key | Value |
|---|---|
| `accountID` | The Stripe account or sandbox, `acct_…` |
| `operatorKey` | The environment's **operator key**: a restricted key that writes products, prices and webhook endpoints and reads the account. The `PaymentProvider` uses it |
| `appKey` | The environment's **Lake key**: a restricted key for customers, invoices, invoice items and Checkout sessions, which also reads products and prices. The Lake gets it as `LAKE_STRIPE_SECRET_KEY` |

Both keys' prefixes must agree with `spec.payments.mode`: `sk_live_` or `rk_live_` for `live`,
`sk_test_` or `rk_test_` for `test`. The mode is written in the committed set file, so a live
Secret given to a nonprod Lake, or a test Secret given to a production one, is refused instead of
used. Restricted keys can only be made in the Stripe Dashboard; the operator only reads them, and no
key value appears in a status, a log or an event.

Each environment has its own Secret, created by a person on the managing cluster
([orama-bridge#201](https://github.com/oramasearch/orama-bridge/issues/201)); see
[`config/clusters/README.md`](../../config/clusters/README.md). A set copies the
member's Secret into its namespace with `spec.secrets` or `clusters[].secrets`.

## What the operator keeps

Everything below is owned by the Lake, in its namespace, and carries its labels.
`kubectl get payprov,paywh,paycat -n <namespace>` shows all three.

| Object | Name | What it is |
|---|---|---|
| `PaymentProvider` | `lake-<name>-payments` | `type: stripe`, `accountId` from the Secret, `livemode` from `spec.payments.mode`, the key `operatorKey` of the Secret |
| `PaymentWebhookEndpoint` | `lake-<name>-payments` | `https://<host>/billing/stripe/webhook`, the events below, `apiVersion: 2024-06-20` (the `Stripe-Version` lake pins, lake `include/lake/api/stripe.hpp`), `app: lake`. Its signing secret goes to Secret `lake-<name>-payments-webhook`, key `signingSecret` |
| `PaymentCatalog` | `lake-<name>-catalog` | `app: lake`, Lake's three products |

The webhook path is Lake's route. Each Lake has its own endpoint, with its own signing secret,
because two Lakes can bill in one account (`shared` and `shared-us` in live mode).

### The webhook events

The endpoint subscribes to the events lake's Stripe handler takes (lake `src/api/stripe.cpp`):

- `invoice.paid`, `invoice.payment_failed` and `invoice.voided`, which lake handles today;
- `checkout.session.completed`, `checkout.session.async_payment_succeeded`,
  `checkout.session.async_payment_failed` and `checkout.session.expired`, for the order sheet of
  [lake#833](https://github.com/oramasearch/lake/issues/833).

lake answers 2xx to an event type it does not handle, so subscribing to the Checkout events before
lake handles them is harmless. Stripe cannot filter by metadata, so the endpoint also receives Cloud
v1's events of these types; lake's handler acknowledges them and changes nothing. The list is
`lakeStripeWebhookEvents` in `lake_payments.go`; change it together with lake's handler.

### Lake's products

| ID | Name | Prices |
|---|---|---|
| `lake_usage` | Lake usage | none: amounts come from Lake's price list |
| `lake_private_warehouse` | Lake Private Warehouse | none: amounts come from Lake's order sheet (lake#833) |
| `lake_dedicated_cluster` | Lake Dedicated Cluster | none |

**These IDs are a contract with lake** ([lake#855](https://github.com/oramasearch/lake/issues/855)):
lake's invoice and Checkout lines name the product and carry an inline price from Lake's own price
list or order sheet. Renaming or removing one breaks lake's billing. The products are
`lakePaymentProducts` in `lake_payments.go`, with `lakePaymentCatalogVersion`, which is bumped on
every change ([Two catalogs, one product](../infrastructure/payments.md#two-catalogs-one-product)).
Deleting the catalog leaves the products in Stripe.

## The check of Lake's key

Once the catalog is `Ready`, the operator reads `GET /v1/products/lake_usage` with `appKey`. The
catalog made that product with `operatorKey`, so:

- 200 with `livemode` matching the mode: the two keys belong to the same account or sandbox.
- 404: `appKey` belongs to another account or sandbox. `PaymentsReady=False`, reason
  `AppKeyMismatch`.
- 200 with the other mode: `ModeMismatch`.
- any other answer, or none: `StripeAPIError`, with the HTTP status. A 401 means Stripe does not
  accept the key; a 403 means it lacks Products read.

The check runs again every hour, a minute after `StripeAPIError`, and at once when the key, the
account or the mode changes. Between checks a steady Lake sends Stripe nothing. The operator keeps
the last result in memory, so a restarted operator checks every Lake once.

## When the Lake gets its variables

The first time, the Lake gets `LAKE_STRIPE_SECRET_KEY` and `LAKE_STRIPE_WEBHOOK_SECRET` only once
everything is in place:

1. the provider's `Credentials` are `True`;
2. the webhook endpoint is `Ready`, and Secret `lake-<name>-payments-webhook` holds `signingSecret`;
3. the catalog is `Ready`;
4. the check of Lake's key passed.

Until then the Lake has neither and takes payments through its manual provider. A `secretKeyRef` to
a Secret that does not exist yet would stop the pod from starting (`CreateContainerConfigError`),
and a single-replica Lake rolls with the `Recreate` strategy, so it would go down. Requiring the
endpoint to be `Ready`, besides its Secret, means a Secret of that name made by hand is never given
to the Lake.

```yaml
- name: LAKE_STRIPE_SECRET_KEY
  valueFrom: {secretKeyRef: {name: <spec.payments.secretRef.name>, key: appKey}}
- name: LAKE_STRIPE_WEBHOOK_SECRET
  valueFrom: {secretKeyRef: {name: lake-<name>-payments-webhook, key: signingSecret}}
```

Both names are reserved while `spec.payments` is set: entries in `spec.env` with either name are
dropped.

### What keeps the variables, and what withdraws them

Once the Lake has the variables, a failure on Stripe's side is reported in `PaymentsReady` and the
Lake keeps them. A Stripe outage never takes the Lake back to manual payments. They are kept when:

- the provider, the endpoint or the catalog is not `Ready` (a failed hourly check, a Stripe error);
- the check of Lake's key fails, with `StripeAPIError` or `AppKeyMismatch`;
- the signing Secret is briefly missing while the endpoint is replaced.

Two failures withdraw them, and the Lake rolls to manual payments:

- `SecretMissing`: the Secret, or one of its keys, is gone. A `secretKeyRef` to it would stop the
  next pod from starting.
- `ModeMismatch`: a key of the wrong mode must never reach the pod.

### Rolling the Lake when a value changes

The pod template carries `orama.cloud/payments-secret-hash`, the SHA-256 of `appKey` and
`signingSecret`, present only while the Lake has the variables. A replaced webhook endpoint has a
new signing secret and a rotated `appKey` has a new value, both under unchanged names, so the hash
is what rolls the Lake. The Lake controller watches the payments Secret and the signing Secret
(which belongs to the webhook endpoint, not to the Lake), so either change is seen at once.

The hash moves only when everything is in place again. While a failure keeps the variables, the
annotation keeps its previous value: a rotated `appKey` that fails its check, or a signing Secret
being replaced, does not roll the Lake. A pod that restarts for another reason in that window reads
the Secrets as they are.

## Status

Condition `PaymentsReady` exists only on a Lake with `spec.payments`, or while what it made is being
removed. A payments failure never changes the Lake's `Ready` condition or phase.

| Status | Reason | Meaning |
|---|---|---|
| `True` | `Configured` | Everything is in place and the Lake has its Stripe variables. The message names the mode, the account and the webhook endpoint |
| `False` | `SecretMissing` | The Secret does not exist, lacks a key, or its `accountID` is not `acct_…`. Nothing is created or changed |
| `False` | `ModeMismatch` | A key's prefix, or what `appKey` sees, disagrees with `spec.payments.mode`. Nothing is created or changed |
| `False` | `ProviderNotReady` | The provider has not checked `operatorKey`, or its `Credentials` are not `True`; the message carries its reason |
| `False` | `Progressing` | Waiting for the endpoint, the catalog or the signing Secret |
| `False` | `AppKeyMismatch` | `appKey` cannot see `lake_usage`: it belongs to another account or sandbox |
| `False` | `StripeAPIError` | Stripe gave no usable answer to the check of `appKey` |
| `False` | the child's reason | The endpoint or the catalog is not `Ready`: `URLTaken`, `SigningSecretTaken`, `ProductTaken`, `PriceTaken`, `InvalidSpec`, … ([`payments.md`](../infrastructure/payments.md)) |
| `False` | `Removing` | `spec.payments` was removed and the endpoint is still being deleted |

While a failure keeps the variables, the message ends with "the Lake keeps the Stripe keys it was
given". Status is written only when the condition changes.

## Removing `spec.payments`, or deleting the Lake

Removing the field withdraws the variables at once (entries in `spec.env` with those names apply
again), then:

1. deletes the `PaymentCatalog`; Stripe's products stay;
2. deletes the `PaymentWebhookEndpoint` in the foreground, so it is gone only once its
   `StripeWebhookEndpoint`'s finalizer finishes cleanup;
3. deletes the `PaymentProvider` once the endpoint is gone.

Deleting the Lake runs the same steps before the Lake's finalizer is released. Lake deletion always
finishes regardless of Stripe's response: endpoint cleanup retries for five minutes from the
`StripeWebhookEndpoint`'s deletion timestamp, then releases it. A Stripe 401 or 403 releases it
immediately. A provider whose `Credentials` are not `True` waits only for that grace window.

When cleanup cannot finish, Warning event `EndpointLeftBehind` names the Stripe endpoint ID and
the reason. A person deletes that endpoint in the Stripe Dashboard. The provider stays until the
endpoint's finalizer releases it; Kubernetes API errors still retry. See
[Deleting an endpoint](../infrastructure/payments.md#deleting-an-endpoint) for the cleanup rules.

## Moving a member from `spec.env` to `spec.payments`

The set files are changed by [operator#641](https://github.com/oramasearch/orama-operator/issues/641)
and [operator#643](https://github.com/oramasearch/orama-operator/issues/643), not here. For each
member:

1. The environment's Secret exists on the managing cluster, namespace `lake`
   ([`config/clusters/README.md`](../../config/clusters/README.md)).
2. In the set file, copy it into the member's namespace under one name and add the `payments` block
   and `billing: {enabled: true}`, which `spec.payments` requires; remove the member's
   `LAKE_STRIPE_SECRET_KEY`, `LAKE_STRIPE_WEBHOOK_SECRET` and `LAKE_BILLING` entries, which
   `spec.payments` and `spec.billing` would drop anyway:

   ```yaml
   # config/clusters/sets/lake-nonprod.yaml (excerpt)
   spec:
     template:
       billing: {enabled: true}
       payments:
         mode: test
         secretRef: {name: stripe}
     clusters:
     - clusterRef: orama-nonprod-cluster-01
       name: testing
       secrets:
       - {name: stripe-testing, namespace: lake, as: stripe}
   ```

   `lake-prod` uses `mode: live` and `stripe-live`.
3. Apply the file, with Steve's consent, and watch `kubectl get lake <name> -o
   jsonpath='{.status.conditions[?(@.type=="PaymentsReady")]}'`.

The member drops the old variables as soon as `spec.payments` is applied, so it restarts on manual
payments, and restarts again with the new variables once `PaymentsReady` is `True`. A member whose
`PaymentsReady` stays `False` keeps running on manual payments; the reason says what to fix.

`Lake/prod` on the managing cluster (`lake.oramadev.net`, temporary) gets no Stripe.

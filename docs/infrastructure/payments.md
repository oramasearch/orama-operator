# Payments: `PaymentProvider` and the payment kinds

Cloud v1 and Lake bill through one shared Stripe account
([orama-bridge#199](https://github.com/oramasearch/orama-bridge/issues/199)). The operator manages
what they need in that account from committed resources, in the shape of principle 10 in
[`CLAUDE.md`](../../CLAUDE.md): a provider resource, an abstract kind and a concrete kind, as the
edge kinds do ([`README.md`](README.md#abstract-kinds-and-concrete-providers)).

| Kind | What it is | State |
|---|---|---|
| `PaymentProvider` | The vendor, the account or sandbox, and the operator's key. Checks the key; changes nothing in Stripe. | Built |
| `PaymentWebhookEndpoint` | The provider-neutral request "deliver these payment events to this URL". | Built ([operator#637](https://github.com/oramasearch/orama-operator/issues/637)) |
| `StripeWebhookEndpoint` | Created and owned by a `PaymentWebhookEndpoint` of a stripe provider; the only kind that calls Stripe's webhook API. | Built (operator#637) |
| The catalog kinds | Cloud v1's sandbox catalog. | [operator#638](https://github.com/oramasearch/orama-operator/issues/638) |

The code is `internal/controller/paymentprovider_controller.go`,
`paymentwebhookendpoint_controller.go` and `stripewebhookendpoint_controller.go`, and the Stripe
client the payment kinds share, `internal/controller/stripe_api.go`. The fields are in
[`crd-reference.md`](../crd-reference.md#paymentprovider-separate-crd-namespaced--the-vendor-account-and-key-for-payment-resources).

## A provider and its Secret

```yaml
apiVersion: orama.cloud/v1alpha1
kind: PaymentProvider
metadata:
  name: stripe
  namespace: lake-testing
spec:
  type: stripe
  stripe:
    accountId: acct_1Example        # the account or sandbox
    livemode: false                 # live mode is production only
    apiKeySecretRef:
      name: stripe-operator
      key: apiKey
```

The Secret holds the environment's **operator key**: a restricted key allowed to write products,
prices and webhook endpoints, and to read the account. Webhook endpoints need its Webhook Endpoints
write permission; listing them, which the endpoints do on every pass, needs no more. Each environment (live mode, and the `dev`,
`testing` and `staging` sandboxes) has its own. Restricted keys can only be made in the Stripe
Dashboard, so a person creates the key there; the operator only reads it. Its value never appears in
a repository, an issue, a log or a resource's status. The Lake key and the Cloud v1 key belong to
the apps, not to the provider.

## The credential check

The provider reports the result in condition `Credentials`. `kubectl get payprov -A` shows its status
and reason.

1. **The Secret.** A missing Secret, or a Secret without the key, is `False`, reason `SecretMissing`.
   Nothing is sent to Stripe. The provider looks again in a minute, and at once when the Secret is
   created or changed.
2. **The mode.** The key's prefix must agree with `spec.stripe.livemode`: `sk_live_` or `rk_live_` for
   live mode, `sk_test_` or `rk_test_` for a sandbox. A key that disagrees, or has neither prefix, is
   `False`, reason `ModeMismatch`, and is not sent to Stripe.
3. **The account.** `GET /v1/account`. When its `id` is `spec.stripe.accountId`, the condition is
   `True`, reason `Verified`. Another account is `False`, reason `AccountMismatch`, naming both.
4. **A key that may not read the account.** When Stripe answers `GET /v1/account` with 403 (or a
   `permission_error`), the key is a restricted key without that permission. The provider then lists
   one webhook endpoint, `GET /v1/webhook_endpoints?limit=1`. A 200 proves the key works; an endpoint
   in the list must have the provider's mode, or the condition is `ModeMismatch`. Otherwise it is
   `True`, reason `ModeVerified`. **On this path the account ID is not checked**: a key of another
   account in the same mode passes. Whether a restricted key can be allowed to read the account, and
   under which Dashboard permission, is recorded by
   [orama-bridge#200](https://github.com/oramasearch/orama-bridge/issues/200) §2; a key given that
   permission is checked fully and reports `Verified`.
5. **A Stripe error.** Any other failure is `False`, reason `StripeAPIError`, with the HTTP status and
   Stripe's error type and code. HTTP 401 means Stripe does not accept the key: it is revoked, expired
   or mistyped.

The check runs again every hour, so a key revoked or narrowed in the Dashboard is reported, and a
minute after a `StripeAPIError`. Between checks a provider makes no request. A changed key, account
or mode is checked at once. The provider remembers its last check in memory, so a restarted operator
checks every provider once. Status is written only when the condition changes: a steady provider
makes one request to Stripe an hour and no write.

## How the operator calls Stripe

Every payment kind calls Stripe through `stripe_api.go`:

- **Form encoding.** Stripe's REST API takes form bodies. Nested fields are bracketed keys
  (`metadata[orama_app]`, `price_data[product]`) and a list repeats its key (`enabled_events[]`).
  Keys are sorted, so the same request always has the same body.
- **API version.** Every request carries `Stripe-Version`. The operator's own calls (account, products,
  prices) use one pinned version, `2024-06-20`. A webhook endpoint's calls use that endpoint's
  `apiVersion`.
- **Idempotency keys.** Stripe replays its saved answer to an `Idempotency-Key` it has seen in the
  last 24 hours, a saved 500 included, and refuses a key reused with other parameters. So a key names
  one request exactly: `orama-<object UID>-<operation>-<target>-<first 16 hex of the body's sha256>`,
  where the target is `new` for a create and the Stripe object's ID for an update. A key Stripe has
  answered is never sent again for a new request: the object's status counts the writes Stripe has
  answered, and that count ends the key (`-1`, `-2`, ...; none while it is 0). See
  [Idempotency keys](#idempotency-keys-a-key-stripe-answered-is-spent). These keys are what Stripe's
  request log shows for the operator's writes. GET and DELETE carry no key.
- **Repeats.** A request that gets no answer (a timeout, a dropped connection) is sent again, up to
  three times, with the same key, so Stripe carries it out once. A request that got any answer is not
  repeated in the same reconcile.
- **Errors never carry a key.** Stripe's error message can echo the key it was sent. The client keeps
  the HTTP status, `error.type` and `error.code`, drops the message on 401 and 403, and otherwise
  replaces the key, and anything shaped like a Stripe secret key or signing secret, with `<redacted>`.
  Conditions and logs use only what it returns. Lake does the same (`src/api/stripe.cpp`).

## Webhook endpoints

Each Lake and each Cloud v1 dashboard host has its own Stripe webhook endpoint, because each
endpoint has its own signing secret. A `PaymentWebhookEndpoint` asks for one:

```yaml
apiVersion: orama.cloud/v1alpha1
kind: PaymentWebhookEndpoint
metadata:
  name: lake
  namespace: lake-testing
spec:
  providerRef:
    name: stripe
  url: https://testing.lake.example/v1/billing/stripe/webhook   # https only
  events: [customer.subscription.updated, invoice.paid]         # only what the app handles
  apiVersion: "2024-06-20"                                      # the version the app parses
  app: lake                                                     # metadata[orama_app]
  signingSecretName: lake-stripe-webhook                        # gets key signingSecret
```

For a `stripe` provider it creates a `StripeWebhookEndpoint` of the same name, which it owns, with
the same spec and the provider's key reference, and copies back that object's `Ready` condition and
endpoint ID. `kubectl get paywh -A` and `kubectl get stripewh -A` show both. Stripe cannot filter
deliveries by metadata, so an endpoint also receives the other app's events of the types it
subscribes to; the app's handler answers 2xx to those and changes nothing.

### What the endpoint controller does

Each pass of a `StripeWebhookEndpoint`:

1. **Waits for the provider.** The provider's `Credentials` must be `True` (`Verified` or
   `ModeVerified`) for its current spec, and its key Secret must exist. Otherwise `Ready=False`,
   reason `ProviderNotReady`, and nothing is sent to Stripe.
2. **Lists every endpoint** of the account (`GET /v1/webhook_endpoints?limit=100`, page by page).
   Ours is the one with `metadata[orama_managed]=true` and
   `metadata[orama_owner]=<namespace>/<name>` at the spec's URL, or, when the spec's URL changed, the
   endpoint with those tags whose ID the status records. The owner tag names no cluster, so a cluster
   rebuilt from the committed resources has the same namespaces and names and recognises its own
   endpoints.
3. **`URLTaken`.** An endpoint at the spec's URL without our tags (made by hand, or another object's)
   is never adopted, changed or deleted. `Ready=False`, reason `URLTaken`, and the message names its
   ID. A person decides what to do with it, in the Stripe Dashboard; the controller looks again
   every minute.
4. **None of ours: create.** `POST /v1/webhook_endpoints` with the URL, the sorted events, the
   `api_version`, the description `orama-operator: <app> <namespace>/<name>` and the three metadata
   tags. The signing secret (`whsec_...`) is written at once to Secret `spec.signingSecretName`, key
   `signingSecret`, owned by the `StripeWebhookEndpoint`.
5. **Ours: repair.** When its URL, its sorted events or its status (it must be `enabled`) differ from
   the spec, one `POST /v1/webhook_endpoints/{id}` sends only what differs.
6. **Ours, but it must be replaced.** Its signing Secret is gone or has no `signingSecret` (a
   recreated namespace, say), or its `api_version` is not the spec's (Stripe cannot change it). The
   controller deletes the endpoint and creates it again as in step 4, under a key whose target is
   `recreate-<old endpoint ID>`, and records an event: Warning `SigningSecretLost`, or Normal
   `APIVersionChanged`. The app must then read the new secret.
7. `Ready=True`, reason `Configured`.

A signing Secret another controller owns is never written: `Ready=False`, reason
`SigningSecretTaken`, and nothing is sent to Stripe.

A pass runs every hour, a minute after a failure, and at once when the spec, the key or the
presence of the signing Secret changes. Between passes nothing is sent to Stripe. Status is written
only when it changes, so a steady endpoint makes one list an hour and no write.

### The signing secret is written once

Stripe returns an endpoint's signing secret only in the answer to its create. The operator writes it
to the Secret straight away and never reads it back from Stripe, because it cannot. A lost Secret is
therefore repaired by replacing the endpoint (step 6), which gives it a new signing secret. The
Secret's value never appears in status, a log or an event.

### Idempotency keys: a key Stripe answered is spent

A key is `orama-<object UID>-<operation>-<target>-<16 hex of the body's sha256>`, ended by the
object's `status.writes` when that is not 0. `status.writes` counts the creates and updates Stripe
has answered for the object, 2xx or not, and is saved with the status after each one. So:

- A request that got no answer (a timeout, a dropped connection) is sent again under the same key,
  and Stripe carries it out once. When every attempt loses its answer the pass reports
  `StripeAPIError`; if Stripe did create the endpoint, the next pass finds it without its signing
  secret and replaces it (step 6), so nothing is left behind.
- After a 5xx, which Stripe saves against the key, the next attempt has a new key and is carried out.
- The same request needed again later gets a new key too. Stripe would answer a key it has seen in
  the last 24 hours with the saved answer and do nothing: an endpoint disabled in the Dashboard a
  second time would stay disabled, and an endpoint deleted in the Dashboard soon after its create
  would be "created" again as the deleted one.

### Deleting an endpoint

The finalizer `orama.cloud/payment-webhook-cleanup` deletes our Stripe endpoint, found as in step 2,
before the `StripeWebhookEndpoint` goes; an endpoint Stripe no longer has (404) counts as deleted.
Deleting the `PaymentWebhookEndpoint` deletes its `StripeWebhookEndpoint` through the owner
reference.

The finalizer needs the provider's key. When the provider or its key Secret is already gone (deleted
together with the endpoint, or with the namespace), it does not block: it records a Warning event,
`EndpointLeftBehind`, naming the Stripe endpoint it left, changes nothing in Stripe and lets the
object go. **A person then deletes that endpoint in the Stripe Dashboard.** While the provider exists
but its `Credentials` are not `True`, deletion waits for them. Whoever deletes a provider and its
endpoints together deletes the endpoints first, and the provider once they are gone.

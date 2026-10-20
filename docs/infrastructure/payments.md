# Payments: `PaymentProvider` and the payment kinds

Cloud v1 and Lake bill through one shared Stripe account
([orama-bridge#199](https://github.com/oramasearch/orama-bridge/issues/199)). The operator manages
what they need in that account from committed resources, in the shape of principle 10 in
[`CLAUDE.md`](../../CLAUDE.md): a provider resource, an abstract kind and a concrete kind, as the
edge kinds do ([`README.md`](README.md#abstract-kinds-and-concrete-providers)).

| Kind | What it is | State |
|---|---|---|
| `PaymentProvider` | The vendor, the account or sandbox, and the operator's key. Checks the key; changes nothing in Stripe. | Built |
| `PaymentWebhookEndpoint` | The provider-neutral request "deliver these payment events to this URL". | Next ([operator#637](https://github.com/oramasearch/orama-operator/issues/637)) |
| `StripeWebhookEndpoint` | Created and owned by a `PaymentWebhookEndpoint` of a stripe provider; the only kind that calls Stripe's webhook API. | Next (operator#637) |
| The catalog kinds | Cloud v1's sandbox catalog. | [operator#638](https://github.com/oramasearch/orama-operator/issues/638) |

The code is `internal/controller/paymentprovider_controller.go` and the Stripe client the payment
kinds share, `internal/controller/stripe_api.go`. The fields are in
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
prices and webhook endpoints, and to read the account. Each environment (live mode, and the `dev`,
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
  where the target is `new` for a create and the Stripe object's ID for an update. After a 5xx answer,
  the next attempt adds the attempt number (`-1`, `-2`, ...), or it would get the saved 5xx back.
  These keys are what Stripe's request log shows for the operator's writes. GET and DELETE carry no
  key.
- **Repeats.** A request that gets no answer (a timeout, a dropped connection) is sent again, up to
  three times, with the same key, so Stripe carries it out once. A request that got any answer is not
  repeated in the same reconcile.
- **Errors never carry a key.** Stripe's error message can echo the key it was sent. The client keeps
  the HTTP status, `error.type` and `error.code`, drops the message on 401 and 403, and otherwise
  replaces the key, and anything shaped like a Stripe secret key or signing secret, with `<redacted>`.
  Conditions and logs use only what it returns. Lake does the same (`src/api/stripe.cpp`).

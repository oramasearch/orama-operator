# Clerk sign-in for a Lake (`spec.clerk`)

[operator#541](https://github.com/oramasearch/orama-operator/issues/541). lake signs people in
through [Clerk](https://clerk.com) when it has a publishable key (lake `docs/api/clerk.md`). With
`spec.clerk`, the operator keeps that Clerk instance configured for lake and gives the Lake its
keys. No key is copied by hand.

## Two Clerk applications, four environments

Steve, 2026-10-17: *"two clerk apps, four environments"*.

| Clerk application | Development instance | Production instance |
|---|---|---|
| **`lake`** (`app_3JpNNpVUwgRgnkItIPbzzRWa2BP`, the existing one) | **dev** (`ins_3JpNNvZd4jnehvW6XsKWcMq6MKx`, `above-seahorse-7535`) | **testing** (domain `lake-testing.oramadev.net`) |
| **`lake-live`** | **staging** | **prod** (domain `lake.live`, serving `eu.lake.live` and `us.lake.live`) |

**One secret key belongs to one instance.** A Lake names the Secret holding its instance's key.
Both production Lakes (`shared` and `shared-us`) hold the same key, so they share the instance:
one person can sign in on both regions. Each region still keeps its own Lake account.

## What the operator does

On each pass the operator reads the current state first and changes only what differs. The pass
runs:

- when the Lake changes;
- when the secret key changes;
- every hour, so a setting changed in the dashboard is put back;
- one minute after a failure.

Every call is to Clerk's Backend API (spec version 2026-08-20,
https://clerk.com/docs/reference/spec/bapi/latest.json), with the instance's secret key:

| What | Backend API | Behaviour |
|---|---|---|
| Instance type and allowed origins | `GET /v1/instance` | Read |
| Frontend API host and DNS targets | `GET /v1/domains` (the primary, non-satellite domain) | Read |
| The `lake` JWT template: `{"aud":"lake","email":"{{user.primary_email_address}}","name":"{{user.full_name}}"}`, lifetime 300 s | `GET` then `POST /v1/jwt_templates`, or `PATCH /v1/jwt_templates/{id}` | Created when missing; updated when the claims or lifetime differ |
| Allowed origins: `https://<host>`, and `https://<apexHost>` when set | `PATCH /v1/instance` (`allowed_origins`) | **Added, never removed.** Several Lakes may share the instance |
| Webhook endpoint `https://<host>/identity/clerk-webhook` for `user.created`, `user.updated`, `user.deleted`, `session.revoked`, `session.removed` and `session.ended` (the session events since lake v0.5.0, lake#948) | `POST /v1/webhooks/svix_url` (or `POST /v1/webhooks/svix` the first time), then Svix's Endpoint API with the App Portal token the link carries | An existing endpoint at the same URL is adopted, and its events are corrected if they differ. Its signing secret (`whsec_…`) is stored |

Endpoint management is Svix's API, not Clerk's, because Clerk delivers webhooks through
[Svix](https://clerk.com/docs/guides/development/webhooks/overview). Clerk's Backend API only
returns an App Portal link for the instance's Svix application. That link carries a token scoped
to that one application, the same one Clerk's dashboard uses
([Svix App Portal](https://docs.svix.com/app-portal)). The Svix calls are:

- `GET /api/v1/app/{app}/endpoint`
- `POST /api/v1/app/{app}/endpoint`
- `PATCH /api/v1/app/{app}/endpoint/{id}`
- `GET /api/v1/app/{app}/endpoint/{id}/secret`

The Svix region comes from the token's suffix, as the Svix SDKs do it.

### What the Lake receives

The operator writes a Secret owned by the Lake, `lake-<name>-clerk` in `spec.namespace`, with
these keys:

- `publishableKey`: derived from the Frontend API host. It is `pk_live_` for a production instance
  and `pk_test_` for a development one, followed by base64 of `<host>$`. lake parses it the same
  way. If the secret-key Secret holds `publishableKey`, that value is used instead.
- `issuer`
- `webhookSecret`

The Lake receives three variables. All are **reserved** while `spec.clerk` is set, so `spec.env`
cannot set them:

- `LAKE_CLERK_PUBLISHABLE_KEY`, as a value.
- `LAKE_CLERK_SECRET_KEY`, as a `secretKeyRef` to `secretKey` of the Lake's own
  `spec.clerk.secretKeySecretRef`, given with the publishable key. Since lake v0.5.0 lake checks every
  Clerk exchange live with it, and Clerk sign-in fails closed without it (lake#948).
- `LAKE_CLERK_WEBHOOK_SECRET`, as a `secretKeyRef` to `webhookSecret`. lake's handler for it is
  [lake#765](https://github.com/oramasearch/lake/issues/765).

Each variable is added once the operator has its value. Until then lake runs with Clerk off.

### A production instance's DNS

Each target in the domain's `dns_targets` becomes a CloudflareDNSRecord owned by the Lake. If
`dns_targets` is absent, the legacy `cname_targets` are used instead. The records are:

- **CNAME or TXT**: `clerk.`, `accounts.`, and the email records.
- **Not proxied.** Clerk has to see its own targets.
- **Written with `spec.dnsSecretRef`.** That Secret's zone must be the domain's zone:
  - the `lake.live` zone for prod;
  - the `oramadev.net` zone for testing.
- **Set to `deletionPolicy: Retain`.** Every Lake sharing the instance writes the same records, so
  deleting one Lake must not take them from the others.
- **Owned by the instance, with `sharedOwner: clerk/<instance ID>`.** The first Lake's resource
  creates each record with the comment `orama-operator:clerk/<instance ID>`, and every other
  Lake's resource recognises that comment as the operator's own. A record made by hand at one
  of these names is never taken over: the resource reports the conflict and stays not ready.

A target Clerk marks `manual_resolution_required` is not written. The pass reports it in
`ClerkReady`.

### Status

`ClerkReady` only exists on a Lake with `spec.clerk`:

| Status | Reason | Message |
|---|---|---|
| `True` | `Configured` | For example *"production instance on lake.live, webhook ep_…, 3 DNS record(s)"* |
| `False` | `SecretKeyMissing` | |
| `False` | `ClerkAPIError` | Clerk's error |

A Clerk failure never fails the Lake. The Lake keeps the keys it already had.

### A Lake without `spec.clerk`

Nothing changes:

- no request goes to Clerk;
- no Secret, DNS record or condition is added;
- the Deployment's environment is what it was, so `spec.env` keeps any `LAKE_CLERK_PUBLISHABLE_KEY`
  it sets.

`TestALakeWithoutClerkIsUntouched` holds this.

**Removing `spec.clerk`:**
- deletes `lake-<name>-clerk` and the Clerk DNS resources (their Cloudflare records stay, as `Retain`);
- removes `ClerkReady`;
- gives `spec.env` back its variables.

## What the Backend API cannot do: one-time dashboard steps

Clerk's Platform API is a private beta, and the Orama workspace has no access to it. That API is
where applications, instances and the sign-up settings live. So these steps are done once, by a
person, in the [Clerk dashboard](https://dashboard.clerk.com). The operator does not check them.

1. **Create the application `lake-live`** with a development instance (staging). Application
   `lake` already exists.
2. **Create the production instances**:
   - `lake` on `lake-testing.oramadev.net`;
   - `lake-live` on `lake.live`.

   Clone the settings from the development instance. The operator writes the DNS records Clerk asks
   for once the Lake has the instance's key.
3. **On each of the four instances**, under *User & authentication*:
   - **Email**: *Sign-up with email* on, *Require email address* on, *Verify at sign-up* on (email
     verification code), *Sign in with email* on. Clerk's config key: `auth_email.used_for_sign_up`,
     `used_for_sign_in`, `verify_at_sign_up`.
   - **Restrictions**: access mode **Invite-only**. Clerk's config key:
     `auth_access_control.sign_up_mode = restricted`.
4. **Copy each instance's secret key** (*API keys*, `sk_test_…` or `sk_live_…`) into the Secrets
   below.

## The Secrets Steve creates

These go on the managing cluster (`orama-production-cluster-01`), in namespace **`lake`**. There is
one per environment, each with key **`secretKey`**:

| Secret (namespace `lake`) | Key | Value |
|---|---|---|
| `clerk-lake-dev` | `secretKey` | the `lake` app's development instance key (`sk_test_…`) |
| `clerk-lake-testing` | `secretKey` | the `lake` app's production instance key (`sk_live_…`) |
| `clerk-lake-staging` | `secretKey` | the `lake-live` app's development instance key (`sk_test_…`) |
| `clerk-lake-prod` | `secretKey` | the `lake-live` app's production instance key (`sk_live_…`) |

```sh
kubectl --context orama-production-cluster-01 -n lake create secret generic clerk-lake-dev \
  --from-literal=secretKey='sk_test_…'
```

A `publishableKey` key may be added beside `secretKey`. It is only needed if Clerk's derived key
ever differs from the dashboard's.

## The set files

A set copies Secrets into each member's namespace (`spec.secrets` for every member,
`clusters[].secrets` for one), and `clusters[].spec` is merged over `spec.template`. Each member
copies its own environment's Secret under the one name `clerk-secret-key` (`as:`), so the `clerk`
block is the same everywhere and can live in the template.

```yaml
# config/clusters/sets/lake-nonprod.yaml (excerpt)
spec:
  template:
    clerk:
      secretKeySecretRef: {name: clerk-secret-key}
  clusters:
  - clusterRef: orama-nonprod-cluster-01
    name: dev
    secrets:
    - {name: clerk-lake-dev, namespace: lake, as: clerk-secret-key}
  - clusterRef: orama-nonprod-cluster-01
    name: testing
    secrets:
    - {name: clerk-lake-testing, namespace: lake, as: clerk-secret-key}
    spec:
      dnsSecretRef: {name: <the oramadev.net zone Secret testing already uses>}
  - clusterRef: orama-nonprod-cluster-01
    name: staging
    secrets:
    - {name: clerk-lake-staging, namespace: lake, as: clerk-secret-key}
```

```yaml
# config/clusters/sets/lake-prod.yaml (excerpt): both regions share the production instance
spec:
  secrets:
  - {name: clerk-lake-prod, namespace: lake, as: clerk-secret-key}
  template:
    clerk:
      secretKeySecretRef: {name: clerk-secret-key}
    dnsSecretRef: {name: <the lake.live zone Secret>}
```

The `clerk` block is the same in a template and in a member's `spec`. A member's `dnsSecretRef`
must be the zone of its instance's domain: testing's is `oramadev.net`, prod's is `lake.live`.
The set files are changed and applied separately from this operator change.

## Rolling it out

Steve, 2026-10-17. The pipeline runs **dev, then testing, then staging, then prod.**

On `dev`, the gate is: an invited user signs in through Clerk and lands on `/`.

`Lake/prod` (`lake.oramadev.net`) keeps the hand-set `LAKE_CLERK_PUBLISHABLE_KEY` in `spec.env`
until it is given `spec.clerk`.

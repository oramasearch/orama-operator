# Clerk API Knowledge Base: A Developer's Reference

*A comprehensive, practical reference for building against Clerk's APIs. All facts below are sourced from Clerk's official documentation ([clerk.com/docs](https://clerk.com/docs)), API references, OpenAPI specs, official SDKs, changelog, and the [clerk/skills](https://github.com/clerk/skills) and [clerk](https://github.com/clerk) GitHub organizations. Last verified July 2026.*

---

## Table of Contents

1. [Overview & Mental Model](#1-overview--mental-model)
2. [Authentication & Keys](#2-authentication--keys)
3. [Platform API (Deep Dive)](#3-platform-api-deep-dive)
4. [Backend API (Deep Dive)](#4-backend-api-deep-dive)
5. [Frontend API & Client-Side](#5-frontend-api--client-side)
6. [Webhooks](#6-webhooks)
7. [SDKs & Tooling](#7-sdks--tooling)
8. [Rate Limits, Pagination, Errors](#8-rate-limits-pagination-errors)
9. [Common Recipes / Playbooks](#9-common-recipes--playbooks)
10. [Gotchas & Best Practices](#10-gotchas--best-practices)

---

## 1. Overview & Mental Model

Clerk exposes **three distinct HTTP APIs**. Understanding which one to use — and when — is the single most important mental model for working with Clerk programmatically. Clerk's own reference hub states this explicitly: "Clerk offers a variety of guides to help you build and work with Clerk... Dig into our SDK reference documentation" ([Welcome to Clerk Docs](https://clerk.com/docs)), and the dedicated API overview page lays out the split between Frontend and Backend APIs, with the Platform API documented as a distinct third surface ([API Reference | Clerk Docs](https://clerk.com/docs/reference/api/overview)).

### The Three APIs

| API | Audience | Purpose | Base URL | Auth |
|---|---|---|---|---|
| **Frontend API** | Browsers, native/mobile clients | Sign-in/sign-up UI flows, session management, consumed by ClerkJS and all frontend SDKs | Instance-specific subdomain (e.g. `https://your-app.clerk.accounts.dev` in dev, or your custom domain in prod) | Publishable Key (client-side) + session cookie/token |
| **Backend API** | Your backend servers | Manage resources *inside* one Clerk application (users, orgs, sessions, webhooks, billing, etc.) | `https://api.clerk.com/v1` | Secret Key as Bearer token |
| **Platform API** | Your backend servers / automation / agents | Manage the *workspace* itself — create/list/administer whole Clerk **applications**, instances, domains, transfers | `https://api.clerk.com/v1/platform/...` | Platform API access token as Bearer token |

Per Clerk's official description: "The Clerk Frontend API is meant to be accessed from a browser or native clients. This is what the Clerk SDK's utilize. Use this API if you are building client-side functionality." "The Clerk Backend API is meant to be accessed by backend servers. Use this API if you need to update data inside of Clerk's systems outside the concept of a session, like coordinating data sync operations with third-parties or fetching and updating configuration settings." "The Clerk Platform API is meant to be accessed by backend servers. Use this API if you need to manage resources of a workspace such as your Clerk applications, domains, and application transfers." ([API Reference | Clerk Docs](https://clerk.com/docs/reference/api/overview))

### Mental Model Diagram

```
                        ┌─────────────────────────────┐
                        │   Your Clerk Workspace       │
                        │  (organization-level account)│
                        └──────────────┬───────────────┘
                                       │
                     Platform API (api.clerk.com/v1/platform/*)
                     Auth: Platform API access token
                     Manages: Applications, Instances, Domains, Transfers
                                       │
              ┌────────────────────────┼────────────────────────┐
              ▼                        ▼                        ▼
     ┌─────────────────┐     ┌─────────────────┐      ┌─────────────────┐
     │  Application A   │     │  Application B   │      │  Application C   │
     │ (e.g. "Acme App")│     │ (e.g. "Beta App")│      │  ...             │
     │                  │     │                  │      │                  │
     │ ┌──────────────┐ │     │ ┌──────────────┐ │      │                  │
     │ │ Dev Instance │ │     │ │ Dev Instance │ │      │                  │
     │ │ pk_test/sk_..│ │     │ │ pk_test/sk_..│ │      │                  │
     │ └──────────────┘ │     │ └──────────────┘ │      │                  │
     │ ┌──────────────┐ │     │ ┌──────────────┐ │      │                  │
     │ │ Prod Instance│ │     │ │ Prod Instance│ │      │                  │
     │ │ pk_live/sk_..│ │     │ │ pk_live/sk_..│ │      │                  │
     │ └──────────────┘ │     │ └──────────────┘ │      │                  │
     └─────────┬────────┘     └──────────────────┘      └──────────────────┘
               │
    Backend API (api.clerk.com/v1)          Frontend API (per-instance subdomain)
    Auth: Secret Key (sk_test_/sk_live_)     Auth: Publishable Key + session token
    Manages: Users, Orgs, Sessions,          Powers: <SignIn/>, <SignUp/>, useAuth(),
    Webhooks, Billing, JWT templates,        session refresh, client-side user state
    SAML/SSO, Waitlist, M2M, API keys, etc.
```

### When to Use Each

- **Use the Platform API** when you are building tooling, an internal admin panel, or an *agentic workflow* that needs to spin up new Clerk applications programmatically — e.g., a SaaS platform provisioning a dedicated Clerk app per customer, or the Clerk CLI itself ([Clerk CLI docs](https://clerk.com/docs/cli); [Clerk for Platforms](https://clerk.com/platform)). This is exactly the pattern that powers Clerk's own CLI (`clerk init`, `clerk apps create`) and the March 2026 Stripe Projects integration, where the Stripe CLI command `stripe projects add clerk` provisions a full Clerk application (dev + prod instances) from the terminal ([Clerk is now available in Stripe Projects](https://clerk.com/changelog/2026-03-26-clerk-stripe-projects)).
- **Use the Backend API** for essentially all day-to-day server-side work inside a single application: creating/updating users, managing organizations, verifying sessions, sending webhooks, configuring billing plans, etc. This is what `clerkClient` (from `@clerk/backend`, `@clerk/nextjs/server`, etc.) wraps ([clerkClient | Clerk Docs](https://clerk.com/docs/reference/backend/overview)).
- **Use the Frontend API** indirectly — you almost never call it directly. It's what ClerkJS and every frontend SDK (`@clerk/clerk-react`, `@clerk/nextjs` client components, etc.) talk to under the hood to run sign-in/sign-up flows and manage the client-side session.

### A Note on Terminology Collision

Clerk uses the phrase "API keys" for **two completely different things**, which is a common source of confusion:

1. **Your application's Publishable/Secret Key pair** — the credentials *you* use to talk to Clerk's Backend/Frontend APIs. Found on the dashboard's "API keys" page ([Clerk environment variables](https://clerk.com/docs/guides/development/clerk-environment-variables)).
2. **The end-user-facing "API keys" feature** — a machine-authentication feature that lets *your application's own users* generate long-lived API keys to call *your* application's API on their behalf, verified via Clerk SDKs on your backend ([Using API keys - Machine authentication](https://clerk.com/docs/guides/development/machine-auth/api-keys)). This is entirely separate from your Secret Key.

Section 2 disambiguates all of these key types in detail.

---

## 2. Authentication & Keys

Clerk's docs are explicit: "Clerk's authentication service implements a dual-key system using publishable and secret keys... Developers typically store API keys as environment variables" ([API Key - Clerk Glossary](https://clerk.com/glossary/api-key)). Beyond that base pair, Clerk has expanded into several additional token families for machine-to-machine and platform-automation use cases. Here is the full landscape.

### 2.1 Publishable Key

- **Format/prefix:** `pk_test_...` (development) or `pk_live_...` (production) ([Clerk environment variables](https://clerk.com/docs/guides/development/clerk-environment-variables)).
- **Where used:** Client-side / frontend code. Initializes ClerkJS and all frontend SDKs.
- **Safety:** Safe to expose publicly — it is designed to be embedded in frontend bundles. "Safe to expose on the frontend. You don't need to rotate it if it's committed to a repository or otherwise made public." ([Rotate your Clerk API keys](https://clerk.com/docs/guides/secure/rotate-api-keys))
- **Common env var names:** `NEXT_PUBLIC_CLERK_PUBLISHABLE_KEY`, `PUBLIC_CLERK_PUBLISHABLE_KEY`, `EXPO_PUBLIC_CLERK_PUBLISHABLE_KEY`, `VITE_CLERK_PUBLISHABLE_KEY`, `NUXT_PUBLIC_CLERK_PUBLISHABLE_KEY` ([Clerk environment variables](https://clerk.com/docs/guides/development/clerk-environment-variables)).

### 2.2 Secret Key

- **Format/prefix:** `sk_test_...` (development) or `sk_live_...` (production) ([Clerk environment variables](https://clerk.com/docs/guides/development/clerk-environment-variables)).
- **Where used:** Backend/server code only. This is the Bearer token for the **Backend API** (`https://api.clerk.com/v1`).
- **Safety:** **Must be kept private.** "Do not expose this on the frontend with a public environment variable." Never commit it, never send it to a browser.
- **Common env var name:** `CLERK_SECRET_KEY`.
- **Where to find it:** Clerk Dashboard → select app/instance → **Configure → Developers → API Keys** ([Clerk - How do I link my account? - Nango Docs](https://nango.dev/docs/integrations/all/clerk/connect)).
- **Rotation:** Go to the **API keys** page in the Dashboard, under **Secret keys** select **+ Add new key**, give it a descriptive name, copy the value, update `CLERK_SECRET_KEY` everywhere (local `.env`, hosting provider, CI/CD), then redeploy and verify. ([Rotate your Clerk API keys](https://clerk.com/docs/guides/secure/rotate-api-keys))

### 2.3 Dev vs. Prod Instances

Every Clerk **application** typically has (at minimum) a **development instance** and a **production instance**, each with its own key pair and user pool:

| Environment | Publishable Key prefix | Secret Key prefix | Notes |
|---|---|---|---|
| Development | `pk_test_` | `sk_test_` | Free plan includes all paid functionality for staging/testing purposes — e.g., you can test enterprise SSO for every customer before production cutover without paying per connection ([How to add SSO and SAML to my SaaS Product](https://clerk.com/articles/how-to-add-sso-and-saml-to-my-saas-product)) |
| Production | `pk_live_` | `sk_live_` | Requires a verified custom domain; billed per Monthly Active User beyond free tier |

A guard-rail worth building into CI: reject a live publishable key paired with a test secret key (or vice versa) — see the Gotchas section for a validation snippet.

### 2.4 Platform API Access Token

- **Purpose:** Authenticates calls to the **Platform API** (`https://api.clerk.com/v1/platform/...`), which manages your entire Clerk **workspace** — creating/listing applications, managing instances/domains, transferring applications between workspaces.
- **Usage in requests:** `Authorization: Bearer YOUR_SECRET_TOKEN` as shown in Clerk's own Platform API reference example:
  ```bash
  curl https://api.clerk.com/v1/platform/applications \
    --header 'Authorization: Bearer YOUR_SECRET_TOKEN'
  ```
  ([Clerk Platform API Reference Documentation](https://clerk.com/docs/reference/platform-api/tag/applications))
- **How the CLI gets it:** The Clerk CLI (`clerk init`, `clerk apps create`) authenticates you (`clerk auth login`) and then uses a Platform API token under the hood to provision applications on your behalf — this is exactly the mechanism referenced in Clerk's own `clerk-setup` skill: `clerk init` "creates the Clerk app via PLAPI, links the project..." ([clerk/skills — clerk-setup SKILL.md](https://github.com/clerk/skills/blob/main/skills/core/clerk-setup/SKILL.md)). "PLAPI" is Clerk-internal shorthand for the Platform API.
- **Distinct from Secret Key:** A Secret Key scopes you to *one instance inside one application*. A Platform API token scopes you to your *entire workspace*, letting you enumerate and create applications themselves.

### 2.5 The End-User-Facing "API Keys" Feature (Machine Auth)

This is **not** your Secret Key. It's an opt-in feature you enable for your *own users* (or organizations) so they can generate long-lived, revocable tokens to call *your* application's API. Clerk's machine-authentication overview describes three distinct token types, of which this is one:

> "Clerk currently supports three types of machine authentication tokens: OAuth access tokens, M2M (machine-to-machine) tokens, API keys." ([Machine authentication | Clerk Docs](https://clerk.com/docs/guides/development/machine-auth/overview))

**API keys**: "intended for the case where your application's users can create API keys that can delegate access to your application's API on their behalf." Capabilities: let users create keys via the `<UserProfile />` component or a custom UI, verify them server-side with Clerk SDKs, and revoke them instantly ([Machine authentication overview](https://clerk.com/docs/guides/development/machine-auth/overview)).

Setup and usage, per the official guide ([Using API keys - Machine authentication](https://clerk.com/docs/guides/development/machine-auth/api-keys)):

1. Enable in Dashboard: **API keys** page → **Enable API keys** → choose **Enable User API keys**, **Enable Organization API keys**, or both.
2. **Create** an API key (server-side):
   ```ts
   // The secret is only returned ONCE, on creation — store it immediately.
   const apiKey = await clerkClient.apiKeys.create({
     name: 'Production API Key',
     subject: userId, // user_xxx or org_xxx
     secondsUntilExpiration: 60 * 60 * 24 * 365, // optional; keys never expire by default
   });
   console.log(apiKey.secret); // only available here
   ```
3. **Verify** a caller's API key on your backend:
   ```ts
   const result = await clerkClient.apiKeys.verifySecret(secretFromRequest);
   ```
4. **Revoke** instantly if compromised:
   ```ts
   await clerkClient.apiKeys.revoke({ apiKeyId: 'ak_...' });
   ```
5. Enable it as an accepted auth type at a specific route:
   ```ts
   const authReq = await clerkClient.authenticateRequest(req, {
     acceptsToken: ['session_token', 'api_key'],
   });
   ```
   ([Introducing API Keys: Machine-to-Machine Auth with Clerk](https://www.youtube.com/watch?v=jFJTbLIVOzA))

### 2.6 M2M (Machine-to-Machine) Tokens

M2M tokens are for **authenticating your own backend services to each other** — not for your end-users. "If the goal is to authenticate requests made between several different services within your own infrastructure, this is the intended use case for Clerk's M2M tokens feature. With this feature, you can: create machines, dictate which machines are allowed to communicate with each other, create tokens that can be used to authenticate requests between these machines." ([Machine authentication overview](https://clerk.com/docs/guides/development/machine-auth/overview))

M2M tokens went to public beta in August 2025: "M2M tokens are designed specifically for authenticating requests between different machines within your backend infrastructure." ([M2M Tokens Public Beta - Clerk](https://clerk.com/changelog/2025-08-15-m2m-beta))

Each "machine" you register gets its own **machine secret key** (distinct format from `sk_*`, e.g. referenced as `CLERK_MACHINE_SECRET_KEY` in examples), which is used to mint and verify M2M tokens:

```ts
// Node — @clerk/backend
import { createClerkClient } from '@clerk/backend';

const clerkClient = createClerkClient({
  machineSecretKey: process.env.CLERK_MACHINE_SECRET_KEY,
});

// Create an M2M token
const token = await clerkClient.m2mTokens.create({
  secondsUntilExpiration: 3600,
});

// Verify it on the receiving machine
const result = await clerkClient.m2mTokens.verifySecret({
  secret: 'mt_secret_xxxxx',
});

// Revoke it
await clerkClient.m2mTokens.revoke({
  m2mTokenId: 'mt_xxxxx',
  revocationReason: 'Revoked by user',
});
```
([clerk/javascript @clerk/backend@2.7.0 release notes](https://newreleases.io/project/github/clerk/javascript/release/@clerk%2Fbackend@2.7.0); [clerk/m2m-example](https://github.com/clerk/m2m-example))

To accept M2M tokens in a request handler:
```ts
const authReq = await clerkClient.authenticateRequest(request, {
  acceptsToken: 'm2m_token', // previously "machine_token"
});
if (authReq.isAuthenticated) { /* ... */ }
```

**Rotating a machine's secret key** (Backend API):
```bash
curl -X POST "https://api.clerk.com/v1/machines/{machine_id}/secret_key/rotate" \
  -H "Authorization: Bearer $CLERK_SECRET_KEY" \
  -H "Content-Type: application/json" \
  -d '{"previous_token_ttl": 3600}'
```
The response returns `{"object": "machine_secret_key", "secret": "sk_machine_..."}`. Maximum `previous_token_ttl` is 28,800 seconds (8 hours); set to `0` to expire the old key immediately ([`rotateSecretKey()` - Machines | Clerk Docs](https://clerk.com/docs/reference/backend/machines/rotate-secret-key)).

### 2.7 OAuth Access Tokens (Machine Auth)

The third machine-auth token type: a standard OAuth **authorization code flow** token that lets a machine act *on behalf of a user* against Clerk's API. Clerk does **not** yet support the OAuth **client credentials** flow (that gap is filled by M2M tokens for service-to-service auth) — "Clerk is aiming to add support for the OAuth client credentials flow in the near future." ([Machine authentication overview](https://clerk.com/docs/guides/development/machine-auth/overview))

### 2.8 Summary Table: All Key/Token Types

| Token type | Prefix/format | Issued to | Scope | Where obtained |
|---|---|---|---|---|
| Publishable Key | `pk_test_` / `pk_live_` | Frontend code | One instance | Dashboard → API keys |
| Secret Key | `sk_test_` / `sk_live_` | Your backend | One instance (Backend API) | Dashboard → API keys |
| Platform API access token | Bearer token (opaque) | Automation / CLI / agents | Entire workspace (Platform API) | Generated via CLI auth / dashboard workspace settings |
| End-user API key | `ak_...`-style secret, returned once | Your app's end users/orgs | Scoped to a user or org, revocable | Created via `<UserProfile/>` or `apiKeys.create()` |
| Machine secret key | Machine-scoped secret | A registered "machine" (service) | That machine's M2M tokens | Dashboard → Machine config, or rotated via API |
| M2M token | `mt_secret_...` | A machine, short-lived | Calls between allow-listed machines | `m2mTokens.create()` |
| OAuth access token | Standard OAuth token | A machine acting for a user | Authorization-code-flow scope | OAuth authorization flow |
| Session token (JWT) | Standard JWT | An end-user's browser session | That user's session | Issued by Frontend API on sign-in |

### 2.9 Security Best Practices

- **Never** expose the Secret Key, machine secret keys, or end-user API key secrets client-side. Only the Publishable Key is safe in browser code.
- Store secrets in a secrets manager or platform environment variables — not hardcoded, not committed to source control.
- Validate that publishable/secret key "liveness" match at startup (both `_test_` or both `_live_`) to prevent cross-environment key mismatches — a common production incident pattern ([clerk-multi-env-setup agent skill](https://agentskills.codes/skills/clerk-multi-env-setup)).
- Rotate the Secret Key via the Dashboard's "add new key" + redeploy pattern, keeping the old key valid only during the transition window ([Rotate your Clerk API keys](https://clerk.com/docs/guides/secure/rotate-api-keys)).
- For machine secret key rotation, use the `previous_token_ttl` grace period so in-flight deploys don't break: `clerk api --platform POST /v1/platform/applications/<app_id>/rotate_secret_keys -d '{"delay_old_secrets_expiration_hours": 24, "reason": "scheduled rotation"}'` ([clerk/skills clerk-setup SKILL.md](https://github.com/clerk/skills/blob/main/skills/core/clerk-setup/SKILL.md)).

---

## 3. Platform API (Deep Dive)

### 3.1 Purpose and Positioning

The Platform API is for managing your **workspace-level** resources — the applications themselves, not what's inside them. Clerk's docs state plainly: "Use this API if you need to manage resources of a workspace such as your Clerk applications, domains, and application transfers." ([API Reference | Clerk Docs](https://clerk.com/docs/reference/api/overview)) It underpins Clerk's own CLI and its "Clerk for Platforms" white-label offering, which lets other platforms (e.g., Replit, and via the Stripe Projects integration, Stripe) provision Clerk applications programmatically on behalf of their own end users:

> "Clerk provides the APIs and infrastructure to provision, manage, and customize authentication for your platform's customers... Programmatic provisioning: Spin up applications for your users with Clerk's APIs. Configure branding, sign-in methods, and SSO providers for each tenant." ([Clerk for Platforms](https://clerk.com/platform))

Real-world validation: Replit's auth integration is described as "Built by Replit, auth by Clerk... When users ask for auth, Replit uses Clerk for Platforms to provision and configure Clerk on their behalf. Fully white-labeled." ([Clerk for Platforms](https://clerk.com/platform)). And in March 2026, Stripe shipped a CLI plugin that calls the same mechanism: "We partnered with Stripe on Stripe Projects to bring authentication into the CLI-first workflow. Provision Clerk from your terminal with just: `stripe projects add clerk`" ([Clerk on X](https://x.com/clerk/status/2037254533462872172)); "Using the Stripe CLI, you can: Connect an existing Clerk account or have one created for you; Provision a new Clerk application with both development and production credentials; Manage authentication keys, rotate secrets, and access your Clerk dashboard — all from Stripe." ([Clerk is now available in Stripe Projects](https://clerk.com/changelog/2026-03-26-clerk-stripe-projects))

### 3.2 Base URL & Auth

- **Base path:** `https://api.clerk.com/v1/platform/`
- **Auth:** `Authorization: Bearer YOUR_SECRET_TOKEN` — a Platform API access token (distinct from any single application's Secret Key) ([Clerk Platform API Reference](https://clerk.com/docs/reference/platform-api/tag/applications)).

### 3.3 Applications Resource

#### List applications

```bash
curl https://api.clerk.com/v1/platform/applications \
  --header 'Authorization: Bearer YOUR_SECRET_TOKEN'
```

Optional query parameter: `include_secret_keys` — whether to include secret keys in the response.

Response:
```json
[
  {
    "application_id": "string",
    "name": "string",
    "instances": [
      {
        "instance_id": "ins_2abc123def456",
        "environment_type": "production",
        "secret_key": "sk_live_abc123...",
        "publishable_key": "pk_live_abc123..."
      }
    ]
  }
]
```
([Clerk Platform API Reference Documentation](https://clerk.com/docs/reference/platform-api/tag/applications))

#### Create a new application

```bash
curl https://api.clerk.com/v1/platform/applications \
  --request POST \
  --header 'Content-Type: application/json' \
  --header 'Authorization: Bearer YOUR_SECRET_TOKEN' \
  --data '{
    "name": "My New App",
    "domain": "",
    "proxy_path": "/__clerk",
    "environment_types": ["development", "production"],
    "template": "b2b-saas"
  }'
```

Request body fields:

| Field | Type | Notes |
|---|---|---|
| `name` | string | Application display name |
| `domain` | string, optional | Domain for the application |
| `proxy_path` | string, optional | Custom proxy path for provider domains; must start with `/` and be a single path segment; defaults to `/__clerk` |
| `environment_types` | array, optional | `"development"` and/or `"production"`; defaults to `["development"]` |
| `template` | string, optional | e.g. `"b2b-saas"`, `"b2c-saas"`, `"waitlist"` |

Response mirrors the list-applications shape — an application object with its `instances[]` array, each instance carrying its own `secret_key` and `publishable_key` ([Clerk Platform API Reference](https://clerk.com/docs/reference/platform-api/tag/applications)).

**Application object schema:** "An object representing a Clerk application. Each application can have multiple instances, typically one for development and one for production, each with distinct user pools." Fields: `application_id`, `name`, `instances[]`.

### 3.4 Instances and Domains

Instances are nested under applications rather than exposed as a fully independent top-level resource in the public docs snapshot, but the object shape is well-documented: each instance has `instance_id`, `environment_type` (`"development"` | `"production"`), `secret_key`, and `publishable_key` ([Clerk Platform API Reference](https://clerk.com/docs/reference/platform-api/tag/applications)).

- **Development instances** get a Clerk-provided domain automatically.
- **Production instances** require additional domain configuration (you point a custom domain at Clerk and verify it).

### 3.5 Instance Config (Including Billing)

Beyond the endpoints in the public reference snippet, the official `clerk/skills` billing skill documents a **PATCH-based instance config API** for programmatically editing billing plans/features — useful for agents seeding config across environments:

```bash
clerk api --platform PATCH /v1/platform/applications/<app_id>/instances/<ins_id>/config \
  -d '{"billing":{"plans":[{"slug":"pro","name":"Pro","amount":2000,"currency":"usd","payer_type":"user","is_recurring":true}],"features":[{"slug":"export","name":"Export"}]}}'
```

This is explicitly flagged as **experimental**: "Billing APIs are still experimental. Pin your `@clerk/nextjs` and `clerk-js` package versions." ([clerk-billing skill](https://skills.re/skills/clerk/skills/clerk-billing))

The Clerk CLI wraps this config surface with friendlier commands:
```bash
clerk enable billing                 # enable for both user + org, auto-creates free plans
clerk enable billing --for org       # org-scoped only
clerk config pull --keys billing > billing.json   # pull current config as JSON
clerk config patch --file billing.json --dry-run  # preview diff
clerk config patch --file billing.json            # apply
```
([clerk-billing skill](https://skills.re/skills/clerk/skills/clerk-billing))

### 3.6 Secret Key Rotation via Platform API

```bash
clerk api --platform POST /v1/platform/applications/<app_id>/rotate_secret_keys \
  -d '{"delay_old_secrets_expiration_hours": 24, "reason": "scheduled rotation"}'
```
"PLAPI exposes secret-key rotation directly. Use raw `clerk api` until the friendly wrapper ships." ([clerk/skills clerk-setup SKILL.md](https://github.com/clerk/skills/blob/main/skills/core/clerk-setup/SKILL.md))

### 3.7 How the Clerk CLI Uses the Platform API

The CLI's `init` flow is a thin, ergonomic wrapper over the Platform API:

> "`clerk init` creates the Clerk app via PLAPI, links the project, writes the framework-specific publishable + secret keys to the right env file (e.g. `.env.local` for Next.js, `.env` for Vite-based projects), and installs the SDK package." ([clerk/skills clerk-setup SKILL.md](https://github.com/clerk/skills/blob/main/skills/core/clerk-setup/SKILL.md))

Other CLI commands and their API relationship, per Clerk's own CLI docs: "`clerk config` — Manage your application's settings directly from the command line... `clerk api` is an authenticated client for Clerk's Backend API. Run `clerk api ls` to explore available endpoints... `clerk apps list` and `clerk apps create` let you view and create Clerk applications from the terminal." ([Clerk CLI - Guides](https://clerk.com/docs/cli)) Note that `clerk apps create`/`clerk apps list` map to the **Platform API**, while `clerk api` maps to the **Backend API** — the CLI cleanly separates the two surfaces even though both live under `api.clerk.com`.

### 3.8 Worked Example: Provision a New App End-to-End and Retrieve Its Keys

**Via raw curl (Platform API):**

```bash
# 1. Create the application with both dev and prod instances
curl https://api.clerk.com/v1/platform/applications \
  --request POST \
  --header 'Content-Type: application/json' \
  --header "Authorization: Bearer $CLERK_PLATFORM_TOKEN" \
  --data '{
    "name": "acme-tenant-042",
    "environment_types": ["development", "production"],
    "template": "b2c-saas"
  }' | tee new_app.json

# 2. Extract the dev-instance secret + publishable keys
python3 -c "
import json
app = json.load(open('new_app.json'))
for inst in app['instances']:
    print(inst['environment_type'], inst['publishable_key'], inst['secret_key'])
"

# 3. Immediately use the new instance's Secret Key against the Backend API
#    to seed the first admin user (see Section 9a for the full recipe)
curl https://api.clerk.com/v1/users \
  --request POST \
  --header "Authorization: Bearer sk_test_NEW_INSTANCE_SECRET" \
  --header 'Content-Type: application/json' \
  --data '{"email_address": ["admin@acme-tenant-042.com"], "password": "TempPassw0rd!23"}'
```

**Via Clerk CLI (equivalent, higher-level):**

```bash
clerk auth login
clerk apps create --name "acme-tenant-042" --template b2c-saas
clerk link                      # links the current directory to the new app
clerk api GET /users            # confirm the Backend API works with the linked app's key
```
([Clerk CLI docs](https://clerk.com/docs/cli); [Introducing Clerk CLI](https://clerk.com/blog/introducing-clerk-cli))

---

## 4. Backend API (Deep Dive)

### 4.1 Basics

- **Base URL:** `https://api.clerk.com/v1` ([Clerk Backend API Reference](https://clerk.com/docs/reference/backend-api); confirmed across [clerk/skills clerk-backend-api SKILL.md](https://github.com/clerk/skills/blob/main/skills/core/clerk-backend-api/SKILL.md) and the Python/Dart/C# SDK docs).
- **Auth:** `Authorization: Bearer $CLERK_SECRET_KEY` on every request.
- **Versioning:** "When the API changes in a way that isn't compatible with older versions, a new version is released. Each version is identified by its release date, e.g. `2021-02-05`." ([Clerk Backend API Reference](https://clerk.com/docs/reference/backend-api)) Version can be pinned via the `Clerk-API-Version` header or by pinning your SDK version.
- **OpenAPI spec:** Downloadable directly from the reference page ("Download OpenAPI specification: Download"), and the raw specs live in the public [clerk/openapi-specs](https://raw.githubusercontent.com/clerk/openapi-specs/main/bapi/) GitHub repo, versioned by date (e.g. `2025-03-12.yml`, `2025-11-10.yml`).

### 4.2 Resource Groups Overview

| Resource group | Representative endpoints | Notes |
|---|---|---|
| Users | `GET/POST /users`, `GET/PATCH/DELETE /users/{id}`, ban/unban/lock/unlock, metadata | Core CRUD + admin actions |
| Organizations | `GET/POST /organizations`, memberships, invitations (single + bulk) | B2B multi-tenancy primitives |
| Sessions | `GET /sessions`, revoke, verify (deprecated), `create_token_from_template` | JWT issuance/verification |
| Sign-in tokens | `POST /sign_in_tokens` | Passwordless / magic-link-style sign-in |
| Actor tokens (impersonation) | `POST /actor_tokens`, revoke | "Sign in as user" |
| Allowlist / Blocklist | `GET/POST/DELETE /allowlist_identifiers`, `/blocklist_identifiers` | Access restriction by email/phone |
| JWT templates | `create_token_from_template` on sessions | Custom claims for third-party JWTs |
| Webhooks | Configured in Dashboard; delivered via Svix | Event notifications |
| Enterprise connections (SAML/OIDC/SSO) | `POST/GET/PATCH/DELETE /enterprise_connections` (unified March 2026) | SSO for orgs |
| Waitlist | `GET/POST /waitlist_entries` | Pre-launch gating |
| Billing | `/billing/plans`, `/billing/prices`, `/billing/subscription_items`, `/billing/statements` | Subscriptions, plans, credits |
| Machines / M2M | `/machines`, `/m2m_tokens` | Service-to-service auth |
| API Keys | `/api_keys` | End-user-facing M2M feature |
| OAuth applications | `/oauth_applications` | Clerk-as-OAuth-provider |
| JWKS | `GET /jwks` | Public keys for token verification, **no rate limit** |

### 4.3 Users

#### List users

```
GET /v1/users
```
Query params: `limit` (max 500, default 10), `offset`, `order_by` (`+/-created_at`, `+/-updated_at`, `+/-email_address`, `+/-web3wallet`, `+/-first_name`, `+/-last_name`, `+/-phone_number`, `+/-username`, `+/-last_active_at`, `+/-last_sign_in_at`), `email_address[]`, `phone_number[]`, `username[]`, `web3wallet[]`, `user_id[]`, `query`, `created_at` (range filter, `gt:TIMESTAMP` / `lt:TIMESTAMP` in Unix ms) ([clerk/skills clerk-backend-api SKILL.md](https://github.com/clerk/skills/blob/main/skills/core/clerk-backend-api/SKILL.md)).

**curl:**
```bash
curl -s "https://api.clerk.com/v1/users?limit=100&offset=0&order_by=-created_at" \
  -H "Authorization: Bearer $CLERK_SECRET_KEY"
```

**Node (`@clerk/backend`):**
```ts
import { createClerkClient } from '@clerk/backend';
const clerkClient = createClerkClient({ secretKey: process.env.CLERK_SECRET_KEY });

const { data: users, totalCount } = await clerkClient.users.getUserList({
  limit: 100,
  orderBy: '-created_at',
});
```

**Python (`clerk-backend-api`):**
```python
from clerk_backend_api import Clerk

with Clerk(bearer_auth="sk_test_...") as clerk:
    res = clerk.users.list(limit=100, order_by="-created_at")
    for user in res:
        print(user.id, user.email_addresses)
```
([clerk-sdk-python users README](https://github.com/clerk/clerk-sdk-python/blob/main/docs/sdks/users/README.md))

#### Create a user

```bash
curl -X POST "https://api.clerk.com/v1/users" \
  -H "Authorization: Bearer $CLERK_SECRET_KEY" \
  -H "Content-Type: application/json" \
  -d '{"email_address": ["new@example.com"], "password": "S3cur3Pass!"}'
```

#### Update user / metadata

```
PATCH /v1/users/{user_id}
```
Body (snake_case): `{ public_metadata, private_metadata, unsafe_metadata, first_name, last_name, username, ... }`

```bash
curl -s -X PATCH "https://api.clerk.com/v1/users/${USER_ID}" \
  -H "Authorization: Bearer $CLERK_SECRET_KEY" \
  -H "Content-Type: application/json" \
  -d '{"public_metadata": {"plan": "pro", "onboarded": true}}'
```
**Important:** metadata updates via `updateUser()` **replace** the entire metadata object rather than merging it — see Gotchas §10.3. There is a dedicated *merge* endpoint (`update_metadata` / `PATCH /users/{id}/metadata`) that performs a deep merge instead: "Update a user's metadata attributes by merging existing values with the provided parameters... a deep merge will be performed. Deep means that any nested JSON objects will be merged as well. You can remove metadata keys at any level by setting their value to `null`." ([clerk-sdk-python users README](https://github.com/clerk/clerk-sdk-python/blob/main/docs/sdks/users/README.md))

#### Ban / Unban / Lock / Unlock

```bash
curl -X POST "https://api.clerk.com/v1/users/${USER_ID}/ban" \
  -H "Authorization: Bearer $CLERK_SECRET_KEY"
```
"Marks the given user as banned, which means that all their sessions are revoked and they are not allowed to sign in again." The `lock` variant instead temporarily blocks sign-in until a configurable lock duration expires ([clerk-sdk-python users README](https://github.com/clerk/clerk-sdk-python/blob/main/docs/sdks/users/README.md)).

#### Delete a user (irreversible)

```bash
curl -X DELETE "https://api.clerk.com/v1/users/${USER_ID}" \
  -H "Authorization: Bearer $CLERK_SECRET_KEY"
```
Destroys the user record, all sessions, all memberships, and all associated data. Returns `{ id, object, deleted: true }` ([clerk/skills clerk-backend-api SKILL.md](https://github.com/clerk/skills/blob/main/skills/core/clerk-backend-api/SKILL.md)).

#### Impersonation / Actor Tokens

```
POST /v1/actor_tokens
```
"Create an actor token that can be used to impersonate the given user. The `actor` parameter needs to include at least a `sub` key whose value is the ID of the actor (impersonating) user."

Request:
```json
{
  "user_id": "user_2OEpKhcCN1Lat9NQ0G6puh7q5Rb",
  "actor": { "sub": "user_ADMIN_ID" },
  "expires_in_seconds": 3600,
  "session_max_duration_in_seconds": 1800
}
```
Response includes an `object: "actor_token"`, `status: "pending"`, and a `url` you redirect to in order to consume the token: "Once you've successfully created an actor token, you can use the `url` attribute of the response to consume the token and impersonate a user... The Frontend API will redirect you to the `/sign-in` page of your application, where the flow will continue by consuming the `__clerk_ticket` parameter." ([Clerk Backend API Reference](https://clerk.com/docs/reference/backend-api); [User impersonation](https://clerk.com/docs/guides/users/impersonation))

To revoke: `POST /v1/actor_tokens/{actor_token_id}/revoke`.

On the client, `useAuth()` exposes an `actor` object when the current session is an impersonation session, so you can render an "impersonating" banner.

### 4.4 Organizations, Memberships, Invitations

#### Create organization

```
POST /v1/organizations
```
Body: `{ name, created_by (user_id), public_metadata?, private_metadata?, max_allowed_memberships? }`

```bash
ORG=$(curl -s -X POST "https://api.clerk.com/v1/organizations" \
  -H "Authorization: Bearer $CLERK_SECRET_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"name\": \"Acme Corp\", \"created_by\": \"$USER_ID\"}")
```
```ts
// Node
const org = await clerkClient.organizations.createOrganization({
  name: 'Acme Corp',
  createdBy: userId,
});
```
([clerk/skills clerk-backend-api SKILL.md](https://github.com/clerk/skills/blob/main/skills/core/clerk-backend-api/SKILL.md))

`max_allowed_memberships` sets a per-plan membership cap for the organization ([Reddit thread citing Clerk docs on max_allowed_memberships](https://www.reddit.com/r/nextjs/comments/1gj4eg3/any_examples_using_clerk_for_a_saas_with_control/)).

#### List organizations
```
GET /v1/organizations
```
Query: `limit`, `offset`, `query`, `order_by`.

#### Create a single invitation
```
POST /v1/organizations/{organization_id}/invitations
```
Rate limited to **250 requests per hour** per application instance. Body: `{ email_address, role: "org:admin" | "org:member", public_metadata?, private_metadata? }` ([Send and manage Organization invitations via Clerk](https://clerk.com/docs/guides/organizations/add-members/invitations)).

```bash
curl -s -X POST "https://api.clerk.com/v1/organizations/${ORG_ID}/invitations" \
  -H "Authorization: Bearer $CLERK_SECRET_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"email_address\": \"user@example.com\", \"role\": \"org:admin\"}"
```

#### Bulk invitations
```
POST /v1/organizations/{organization_id}/invitations/bulk
```
"Limited to a maximum of 10 invitations per API call... rate limited to 50 requests per hour per application instance." ([Send and manage Organization invitations via Clerk](https://clerk.com/docs/guides/organizations/add-members/invitations))

#### Revoke an invitation
```
POST /v1/organizations/{organization_id}/invitations/{invitation_id}/revoke
```

#### Invitation metadata propagation
"Once the invited user signs up using the invitation link, Clerk stores the invitation metadata (`OrganizationInvitation.publicMetadata`) in the Organization membership's metadata (`OrganizationMembership.publicMetadata`)." ([Send and manage Organization invitations via Clerk](https://clerk.com/docs/guides/organizations/add-members/invitations))

#### Memberships
```
POST   /v1/organizations/{organization_id}/memberships          # create
GET    /v1/organizations/{organization_id}/memberships          # list
PATCH  /v1/organizations/{organization_id}/memberships/{user_id} # update (role, metadata)
DELETE /v1/organizations/{organization_id}/memberships/{user_id} # remove
```
([createOrganizationMembership() - Organizations](https://clerk.com/docs/reference/backend/organization/create-organization-membership); [clerk-sdk-python organizations README](https://pypi.org/project/clerk-backend-api/1.2.0/))

Roles always use the `org:` prefix, e.g. `"org:admin"`, `"org:member"` ([clerk/skills clerk-backend-api SKILL.md](https://github.com/clerk/skills/blob/main/skills/core/clerk-backend-api/SKILL.md)).

### 4.5 Sessions & JWT Verification

#### List / get / revoke sessions
```
GET /v1/sessions
GET /v1/sessions/{session_id}
POST /v1/sessions/{session_id}/revoke
```

#### Verify a session token (recommended path)

For most frameworks, use the framework's built-in middleware (`clerkMiddleware()` for Next.js/Express, etc.) rather than manual verification — it handles the handshake, clock skew, and JWKS caching for you.

For lower-level cases, `@clerk/backend` exposes `authenticateRequest()` (recommended) and `verifyToken()` (advanced/manual):

```ts
import { createClerkClient } from '@clerk/backend';
const clerkClient = createClerkClient({ secretKey: process.env.CLERK_SECRET_KEY });

// Recommended — full authentication of a request object
const requestState = await clerkClient.authenticateRequest(request, {
  authorizedParties: ['https://your-app.com'],
});

// Lower-level — verify a raw token
const claims = await clerkClient.verifyToken(sessionToken, {
  jwtKey: process.env.CLERK_JWT_KEY, // enables networkless verification
});
```
"Verifies a Clerk-generated token signature. Networkless if the `jwtKey` is provided. Otherwise, performs a network call to retrieve the JWKS from the Backend API." Default `apiUrl` is `https://api.clerk.com` ([`verifyToken()` - clerkClient](https://clerk.com/docs/reference/backend/verify-token)).

Retrieve the JWKS directly (useful for other languages/manual verification):
```
GET /v1/jwks
```
This endpoint has **no rate limit** ([Rate limits - How Clerk works](https://clerk.com/docs/guides/how-clerk-works/system-limits)).

#### JWT Templates

Session tokens can be transformed into custom-shaped JWTs (for third-party services like Supabase, Hasura, or Convex) via JWT Templates configured in the Dashboard, then minted with:
```
POST /v1/sessions/{session_id}/tokens/{template_name}
```
(SDK: `createSessionTokenFromTemplate` / `create_token_from_template`) ([clerk-backend-api PyPI sessions listing](https://pypi.org/project/clerk-backend-api/1.2.0/)).

### 4.6 Sign-In Tokens

```
POST /v1/sign_in_tokens
```
"Creates a new sign-in token and associates it with the given user. By default, sign-in tokens expire in 30 days. You can optionally supply a different duration in seconds using the `expires_in_seconds` property." Default: `2592000` seconds (30 days) ([`createSignInToken()` - Sign-in tokens](https://clerk.com/docs/reference/backend/sign-in-tokens/create-sign-in-token)).

```bash
curl -X POST "https://api.clerk.com/v1/sign_in_tokens" \
  -H "Authorization: Bearer $CLERK_SECRET_KEY" \
  -H "Content-Type: application/json" \
  -d '{"user_id": "user_abc123", "expires_in_seconds": 3600}'
```
This is the standard mechanism for "magic link"-style or invite-triggered sign-in flows where you want to hand a pre-authenticated URL to a specific user.

### 4.7 Allowlist / Blocklist

"Allow-lists and Block-lists allow you to control who can sign up or sign in to your application, by restricting access based on the user's email address or phone number." ([Allow-list / Block-list - Clerk Backend API](https://clerk.com/docs/reference/backend-api/tag/Allow-list-Block-list))

```
GET    /v1/allowlist_identifiers
POST   /v1/allowlist_identifiers
DELETE /v1/allowlist_identifiers/{identifier_id}
GET    /v1/blocklist_identifiers
POST   /v1/blocklist_identifiers
DELETE /v1/blocklist_identifiers/{identifier_id}
```
([Clerk Backend API - OpenAPI.city](https://openapi.city/providers/clerk))

```bash
curl -X POST "https://api.clerk.com/v1/allowlist_identifiers" \
  -H "Authorization: Bearer $CLERK_SECRET_KEY" \
  -H "Content-Type: application/json" \
  -d '{"identifier": "trusted@example.com"}'
```

### 4.8 Webhooks (configuration)

Webhooks themselves are configured via the Dashboard (endpoint URL + subscribed events), then delivered via Svix. See full Section 6 below for signature verification and sync patterns.

### 4.9 SAML / SSO / Enterprise Connections

As of **March 9, 2026**, Clerk unified its previously separate `/saml_connections` and `/oidc_connections` endpoints into a single `/enterprise_connections` endpoint:

> "You can now fully manage both SAML and OIDC enterprise connections via the Clerk Backend API... This unified API allows you to manage both SAML and OIDC connections, and will serve as the primary interface moving forward. Support for the legacy SAML endpoint may be phased out in the future." ([Create and manage enterprise connections through Clerk's API](https://clerk.com/changelog/2026-03-09-bapi-enterprise-connections))

| Method | Path | Description |
|---|---|---|
| `POST` | `/v1/enterprise_connections` | Create a connection. Accepts `provider`, `domains`, `name`, `organization_id` |
| `GET` | `/v1/enterprise_connections` | List connections. Query: `organization_id` (optional), pagination |
| `GET` | `/v1/enterprise_connections/{enterpriseConnectionID}` | Get one connection |
| `PATCH` | `/v1/enterprise_connections/{enterpriseConnectionID}` | Update a connection |

The SDK's typed namespace is `enterpriseConnections`, exposing `createEnterpriseConnection`, `getEnterpriseConnection`, `getEnterpriseConnectionList`, `updateEnterpriseConnection`, `deleteEnterpriseConnection` — "it replaces the deprecated `samlConnections` namespace." ([Federated identity for enterprise SaaS: SAML, OIDC, and SCIM](https://clerk.com/articles/federated-identity-for-enterprise-saas-saml-oidc-and-scim-2))

```bash
curl -X POST "https://api.clerk.com/v1/enterprise_connections" \
  -H "Authorization: Bearer $CLERK_SECRET_KEY" \
  -H "Content-Type: application/json" \
  -d '{"provider": "saml_okta", "name": "Acme SSO", "organization_id": "org_123"}'
```

Directory Sync (SCIM) is layered on top: enabling it for a connection generates a SCIM token scoped to that organization, which you paste into the IdP's provisioning config (Okta, Entra ID, etc.) to enable automatic user provisioning/deprovisioning ([Federated identity for enterprise SaaS: SAML, OIDC, and SCIM](https://clerk.com/articles/federated-identity-for-enterprise-saas-saml-oidc-and-scim-2)). Directory Sync reached GA on April 16, 2026, with group-to-role and custom attribute mapping reaching GA on May 21, 2026 ([OIDC vs SAML for Enterprise SSO: A 2026 Decision Guide](https://clerk.com/articles/oidc-vs-saml-for-enterprise-sso-a-2026-decision-guide)).

### 4.10 Waitlist

```
GET  /v1/waitlist_entries
POST /v1/waitlist_entries
```
([`list()` - Waitlist entries](https://clerk.com/docs/reference/backend/waitlist-entries/list); [`create()` - Waitlist Entries](https://clerk.com/docs/reference/backend/waitlist-entries/create))

```bash
curl -X POST "https://api.clerk.com/v1/waitlist_entries" \
  -H "Authorization: Bearer $CLERK_SECRET_KEY" \
  -H "Content-Type: application/json" \
  -d '{"email_address": "prospect@example.com"}'
```

### 4.11 Billing (Plans, Subscriptions, Prices, Credits)

Billing is explicitly noted as an evolving/experimental surface, and its endpoints moved from a `/commerce/*` prefix to `/billing/*` in the **2025-11-10** API version:

| Old endpoint | New endpoint |
|---|---|
| `GET /v1/commerce/plans` | `GET /v1/billing/plans` |
| `GET /v1/commerce/statements` | `GET /v1/billing/statements` |
| `GET /v1/commerce/statements/{statementID}` | `GET /v1/billing/statements/{statementID}` |
| `GET /v1/commerce/statements/{statementID}/payment_attempts` | `GET /v1/billing/statements/{statementID}/payment_attempts` |
| `GET /v1/commerce/subscription_items` | `GET /v1/billing/subscription_items` |
| `DELETE /v1/commerce/subscription_items/{subscriptionItemID}` | `DELETE /v1/billing/subscription_items/{subscriptionItemID}` |
| `POST /v1/commerce/subscription_items/{subscriptionItemID}/extend_free_trial` | `POST /v1/billing/subscription_items/{subscriptionItemID}/extend_free_trial` |

([Upgrade to API version 2025-11-10](https://clerk.com/docs/guides/development/upgrading/upgrade-guides/2025-11-10))

Full endpoint set (per the Go and Dart SDKs, which mirror BAPI 1:1):

| Endpoint | Method | Description |
|---|---|---|
| `/billing/plans` | GET | List all billing plans |
| `/billing/prices` | GET | List all billing prices |
| `/billing/prices` | POST | Create a custom billing price |
| `/billing/subscription_items` | GET | List all subscription items |
| `/billing/subscription_items/{id}` | DELETE | Cancel a subscription item |
| `/billing/subscription_items/{id}/extend_free_trial` | POST | Extend free trial |
| `/billing/subscription_items/{id}/price_transition` | POST | Transition to a different price/plan |
| `/billing/statements` | GET | List billing statements |
| `/billing/statements/{id}` | GET | Retrieve a statement |
| `/billing/statements/{id}/payment_attempts` | GET | List payment attempts for a statement |
| `/organizations/{organization_id}/billing/subscription` | GET | Get an org's subscription |
| `/users/{user_id}/billing/subscription` | GET | Get a user's subscription |

([pkg.go.dev clerk-sdk-go/v2/billing](https://pkg.go.dev/github.com/clerk/clerk-sdk-go/v2/billing); [pub.dev clerk_backend_api Dart docs](https://pub.dev/documentation/clerk_backend_api/latest/))

**List plans (curl):**
```bash
curl "https://api.clerk.com/v1/billing/plans" \
  -H "Authorization: Bearer $CLERK_SECRET_KEY"
```

**Transition a subscription item to a new price (curl):**
```bash
curl -X POST "https://api.clerk.com/v1/billing/subscription_items/subi_123/price_transition" \
  -H "Authorization: Bearer $CLERK_SECRET_KEY" \
  -H "Content-Type: application/json" \
  -d '{"from_price_id": "cprice_123", "to_price_id": "cprice_456"}'
```
([Custom Plans and prices - Billing management](https://clerk.com/docs/guides/billing/custom-plans))

**Go SDK:**
```go
import "github.com/clerk/clerk-sdk-go/v2/billing"

plans, err := billing.ListPlans(ctx, &billing.ListPlansParams{})
```

Front-end usage is typically via the `<PricingTable />` and `<Protect feature="..." />` components, gated server-side with `has({ plan })` / `has({ feature })` from `auth()` — see the recipe in Section 9f-adjacent billing notes. Note the important separation: **Plans live in Clerk, not synced to Stripe** unless you bring your own Stripe account; dev instances can use Clerk's shared development payment gateway with no Stripe account required, while production requires Stripe for payment processing ([clerk-billing skill](https://skills.re/skills/clerk/skills/clerk-billing)).

---

## 5. Frontend API & Client-Side

### 5.1 What It's For

"The Clerk Frontend API is meant to be accessed from a browser or native clients. This is what the Clerk SDK's utilize. Use this API if you are building client-side functionality." ([API Reference | Clerk Docs](https://clerk.com/docs/reference/api/overview)) You almost never call it directly — ClerkJS (`@clerk/clerk-js`) and every frontend SDK wrap it for you.

### 5.2 ClerkJS

ClerkJS is the underlying client-side library that all frontend framework SDKs (`@clerk/nextjs`, `@clerk/clerk-react`, `@clerk/vue`, mobile SDKs, etc.) are built on. It handles:
- Initializing the client with your Publishable Key
- Rendering prebuilt UI components (`<SignIn/>`, `<SignUp/>`, `<UserProfile/>`, `<OrganizationSwitcher/>`, `<PricingTable/>`)
- Managing the client-side session object and reactive auth state
- Talking to the Frontend API for sign-in/sign-up/session-refresh operations

### 5.3 How Session Tokens Work

A session is established by the Frontend API and represented as a signed JWT (the session token) containing cryptographically signed claims about the user's identity and auth state: "A request is considered authenticated when the backend can securely identify the user and device that is making the request. This is done by passing the user's session token to the backend. The session token contains cryptographically signed claims about the user's identity and authentication state." ([Making authenticated requests](https://clerk.com/docs/backend-requests/overview))

**Required headers** for the backend to authenticate a request: `Authorization` (Bearer session token), `Accept`, `Host`, `Origin`, `Referer`, `Sec-Fetch-Dest`, `User-Agent`, `X-Forwarded-Host`, `X-Forwarded-Proto` (or `CloudFront-Forwarded-Proto`) ([Making authenticated requests](https://clerk.com/docs/backend-requests/overview)).

### 5.4 Same-Origin vs. Cross-Origin Requests

- **Same-origin** (e.g., calling `foo.com/api` from JS on `foo.com`): the session token is automatically included via cookie. "All requests to same-origin endpoints are authenticated by default; simply make the request as you normally would."
- **Cross-origin** (e.g., calling `api.foo.com` from JS on `foo.com`): you must manually retrieve the token and set the header:
  ```ts
  const { getToken } = useAuth();
  const token = await getToken();
  fetch('https://api.foo.com/data', {
    headers: { Authorization: `Bearer ${token}` },
  });
  ```
  ([Making authenticated requests](https://clerk.com/docs/backend-requests/overview))

### 5.5 How Frontend and Backend Interact

1. User completes sign-in via ClerkJS talking to the **Frontend API**.
2. Frontend API issues a session and session token (JWT), stored client-side (cookie for same-origin, retrievable via `getToken()` otherwise).
3. Client includes that token on requests to your backend.
4. Your backend, using the **Backend API** / `@clerk/backend` SDK, verifies the token (via `authenticateRequest()`/`verifyToken()`), optionally hitting `GET /v1/jwks` (no rate limit) unless networkless verification is configured with a cached JWT key.
5. Once authenticated, your backend can go on to call any other Backend API resource (users, orgs, billing, etc.) using its Secret Key.

### 5.6 Rate Limits Specific to the Frontend API

| Name | Type | Limit |
|---|---|---|
| Create SignIn | `/v1/sign_ins` | 5 requests / 10 seconds |
| Create SignUp | `/v1/sign_ups` | 5 requests / 10 seconds |
| Attempt SignIn | `/v1/sign_ins/attempt_(first\|second)_factor` | 3 requests / 10 seconds |
| Attempt SignUp | `/v1/sign_ups/attempt_verification` | 3 requests / 10 seconds |

Rate-limited per user, identified by IP address ([Rate limits - How Clerk works](https://clerk.com/docs/guides/how-clerk-works/system-limits)).

---

## 6. Webhooks

### 6.1 Delivery Mechanism

Clerk delivers webhooks via **Svix**: "Clerk uses Svix to send our webhooks." ([Webhooks overview | Clerk Docs](https://clerk.com/docs/guides/development/webhooks/overview)) Configure endpoints and subscribed events in the Dashboard's **Webhooks** page, where you'll also find each endpoint's **signing secret** (`whsec_...`).

### 6.2 Common Event Types

Frequently used events (non-exhaustive; full list is in the Dashboard's event picker): `user.created`, `user.updated`, `user.deleted`, `session.created`, `session.ended`, `organization.created`, `organizationMembership.created`, `organization.subscription.*` (billing lifecycle) — see practical usage in real handler code below and in [Gate features by plan tier using Clerk billing subscriptions](https://www.joinnextdev.com/a/clerk/gate-features-by-plan-tier-using-clerk-billing-subscriptions).

### 6.3 Signature Verification (Svix / Standard Webhooks)

Three headers are sent on every webhook delivery: `svix-id` (unique message ID, used for dedup), `svix-timestamp` (Unix seconds, for replay protection), `svix-signature` (space-delimited list of base64 HMAC signatures, for authenticity) ([How to Verify Webhooks with the Svix Libraries](https://docs.svix.com/receiving/verifying-payloads/how); [Webhooks and Data Synchronization | DeepWiki](https://deepwiki.com/clerk/clerk-docs/7.3-webhooks-and-data-synchronization)).

**Verification algorithm** (if rolling your own): concatenate `{svix-id}.{svix-timestamp}.{raw_body}`, HMAC-SHA256 it using the base64-decoded portion of the secret *after* the `whsec_` prefix, base64-encode the result, and compare (constant-time) against each `v1,<sig>` entry in `svix-signature` — accepting if **any** match (multiple entries appear during secret rotation) ([Verifying Webhooks Manually - Svix Docs](https://docs.svix.com/receiving/verifying-payloads/how-manual)).

**Recommended: use Clerk's built-in helper** rather than hand-rolling verification:

```ts
// Next.js — app/api/webhooks/clerk/route.ts
import { verifyWebhook } from '@clerk/nextjs/webhook';

export async function POST(req: Request) {
  try {
    const evt = await verifyWebhook(req); // reads CLERK_WEBHOOK_SIGNING_SECRET automatically
    switch (evt.type) {
      case 'user.created':
        // sync to your DB
        break;
      case 'user.updated':
        // sync to your DB
        break;
      case 'user.deleted':
        // remove from your DB
        break;
    }
    return new Response('OK', { status: 200 });
  } catch (err) {
    console.error('Webhook verification failed:', err);
    return new Response('Invalid signature', { status: 400 });
  }
}
```
([skills/skills/features/clerk-webhooks/SKILL.md](https://github.com/clerk/skills/blob/main/skills/features/clerk-webhooks/SKILL.md))

**Express (manual Svix, if not using the framework helper) — must use the raw body, not JSON-parsed:**

```ts
import express from 'express';
import { Webhook } from 'svix';

const app = express();

// CRITICAL: use express.raw(), NOT express.json(), for webhook routes
app.post('/api/webhooks/clerk',
  express.raw({ type: 'application/json' }),
  (req, res) => {
    const wh = new Webhook(process.env.CLERK_WEBHOOK_SECRET!);
    try {
      const evt = wh.verify(req.body, {
        'svix-id': req.headers['svix-id'] as string,
        'svix-timestamp': req.headers['svix-timestamp'] as string,
        'svix-signature': req.headers['svix-signature'] as string,
      });
      // handle evt...
      res.status(200).json({ received: true });
    } catch (err) {
      console.error('Webhook verification failed:', err);
      res.status(400).json({ error: 'Invalid signature' });
    }
  }
);
```
([clerk-webhooks-events skill via Tessl](https://tessl.io/registry/skills/github/jeremylongshore/claude-code-plugins-plus-skills/clerk-webhooks-events))

**Python (manual, using `svix`):**
```python
from svix.webhooks import Webhook, WebhookVerificationError

wh = Webhook(WEBHOOK_SECRET)  # WEBHOOK_SECRET = "whsec_..."

try:
    evt = wh.verify(raw_body, {
        "svix-id": headers["svix-id"],
        "svix-timestamp": headers["svix-timestamp"],
        "svix-signature": headers["svix-signature"],
    })
except WebhookVerificationError:
    return Response(status_code=400)
```

### 6.4 Common Pattern: Sync Users to Your DB

```ts
// app/api/webhooks/clerk/route.ts (Next.js)
import { verifyWebhook } from '@clerk/nextjs/webhook';
import { db } from '@/lib/db';

export async function POST(req: Request) {
  const evt = await verifyWebhook(req);

  switch (evt.type) {
    case 'user.created': {
      const { id, email_addresses, image_url, first_name, last_name, username } = evt.data;
      await db.user.create({
        data: {
          externalUserId: id,
          email: email_addresses[0].email_address,
          username,
          firstName: first_name,
          lastName: last_name,
          photo: image_url,
        },
      });
      break;
    }
    case 'user.updated': {
      const { id, first_name, last_name, username, image_url } = evt.data;
      await db.user.update({
        where: { externalUserId: id },
        data: { firstName: first_name, lastName: last_name, username, photo: image_url },
      });
      break;
    }
    case 'user.deleted': {
      const { id } = evt.data;
      await db.user.delete({ where: { externalUserId: id! } });
      break;
    }
  }
  return new Response('OK', { status: 200 });
}
```
([Stack Overflow — Clerk webhook message is failing, showing the canonical sync pattern](https://stackoverflow.com/questions/77902422/clerk-webhook-message-is-failing))

Best practices layered on top of this pattern:
- **Always** verify the signature, even for "notification only" handlers — "Skipping verification, even for notification-only handlers, exposes the endpoint to spoofed events." ([clerk-webhooks skill](https://github.com/clerk/skills/blob/main/skills/features/clerk-webhooks/SKILL.md))
- **Idempotency:** use `svix-id` as a dedup key since Clerk/Svix retries failed deliveries.
- **Replay protection:** reject events where `now - svix_timestamp > 300` seconds.
- Route webhook endpoints so your auth middleware does **not** require a session token to hit them (they're server-to-server), but you still gate them by signature.
- Offload slow DB work to a background queue so the handler responds quickly (Svix retries on slow/failed responses).

---

## 7. SDKs & Tooling

### 7.1 Official Frontend SDKs

| SDK | Description |
|---|---|
| Next.js (`@clerk/nextjs`) | "Easily add secure, beautiful, and fast authentication to Next.js with Clerk." |
| React (`@clerk/clerk-react`) | Install/initialize Clerk in a React + Vite app |
| Expo | Authenticate users in React Native apps |
| TanStack React Start | SSR-friendly auth |
| React Router | Edge- and SSR-friendly auth |
| Android / iOS | Native mobile SDKs (v1 as of Feb 2026, unified `.auth` entry point — [iOS and Android SDKs v1](https://clerk.com/changelog/2026-02-10-ios-android-sdk-v1)) |
| Astro | SSR-friendly auth |
| Chrome Extension | Plasmo or plain TypeScript variants |
| JavaScript (`@clerk/clerk-js`) | The base ClerkJS library; prebuilt components + helpers |
| Nuxt | Vue meta-framework support |
| Vue | Install/initialize Clerk in Vue + Vite |

([SDK References | Clerk Docs](https://clerk.com/docs/reference/overview))

### 7.2 Official Backend SDKs

| SDK | Description |
|---|---|
| Express (`@clerk/express`) | Add auth/user management to Express apps |
| Fastify (`@clerk/fastify`) | Secure auth flows for Fastify |
| Node (`@clerk/backend`, historically `@clerk/clerk-sdk-node`) | "Access the Clerk Backend API from Node.js" ([clerk/clerk-sdk-node](https://github.com/clerk/clerk-sdk-node)) — this is the foundational package that `@clerk/nextjs/server`, `@clerk/express`, etc. all build on |
| Go (`clerk-sdk-go`) | "A wrapper around the Backend API written in Golang" |
| Python (`clerk-backend-api`, alias `clerk_backend_sdk`) | "A wrapper around the Backend API written in Python" — released as beta October 2024 ([Python Backend SDK](https://clerk.com/changelog/2024-10-08-python-backend-sdk-beta)) |
| Ruby on Rails (`clerk-sdk-ruby`) | "Access the Clerk Backend API from Ruby" |
| C# | Wrapper around the Backend API |
| Java | Wrapper around the Backend API |
| PHP | Wrapper around the Backend API (community-adjacent, listed alongside official backend SDKs) |

([SDK References | Clerk Docs](https://clerk.com/docs/reference/overview))

### 7.3 Community-Maintained SDKs

Angular, Elysia, Hono, Koa, Rust, SolidJS, Svelte(Kit), Tauri — all listed on the official SDK reference hub as community-maintained ([SDK References | Clerk Docs](https://clerk.com/docs/reference/overview)).

### 7.4 Node/Backend SDK Usage Pattern

```ts
import { createClerkClient } from '@clerk/backend';

const clerkClient = createClerkClient({
  secretKey: process.env.CLERK_SECRET_KEY,
  // machineSecretKey: process.env.CLERK_MACHINE_SECRET_KEY, // if using M2M
});

// Users
const user = await clerkClient.users.getUser(userId);
await clerkClient.users.updateUser(userId, { publicMetadata: { plan: 'pro' } });

// Organizations
const org = await clerkClient.organizations.createOrganization({ name: 'Acme', createdBy: userId });

// Request authentication
const requestState = await clerkClient.authenticateRequest(request, {
  authorizedParties: ['https://example.com'],
});
```
Framework-specific packages (`@clerk/nextjs/server`, `@clerk/express`) re-export a pre-configured `clerkClient` singleton so you rarely call `createClerkClient` yourself in those environments:
```ts
import { clerkClient } from '@clerk/nextjs/server';
```

### 7.5 Python SDK Usage Pattern

```python
from clerk_backend_api import Clerk

with Clerk(bearer_auth="sk_test_...") as clerk:
    res = clerk.users.list(limit=20)
    print(res)

# Async
import asyncio
from clerk_backend_api import Clerk

async def main():
    async with Clerk(bearer_auth="sk_test_...") as clerk:
        res = await clerk.users.list_async(limit=20)
        print(res)

asyncio.run(main())
```
Install via pip, Poetry, or uv: `pip install clerk-backend-api` / `poetry add clerk-backend-api` ([clerk-backend-api PyPI](https://pypi.org/project/clerk-backend-api/0.5.0a8/); [clerk/clerk-sdk-python USAGE.md](https://github.com/clerk/clerk-sdk-python/blob/main/USAGE.md)).

### 7.6 Django REST Framework Integration Pattern

A common community pattern is a custom DRF authentication backend that calls `clerk_backend_api` to verify the incoming bearer token and hydrate `request.user` — install `clerk-backend-api` via `uv add clerk-backend-api` (or pip), store `CLERK_SECRET_KEY` in Django settings, and register the custom class under `REST_FRAMEWORK["DEFAULT_AUTHENTICATION_CLASSES"]` ([Integrate Clerk Authentication With a React Frontend and Django API](https://www.youtube.com/watch?v=Z8kkyk98w54)).

### 7.7 The Clerk CLI

Open source, installable via npm, Homebrew, bun, pnpm, or yarn:
```bash
npm install -g clerk
```
("The Homebrew tap lives at github.com/clerk/homebrew-stable and pulls the matching prebuilt binary from the CLI's GitHub releases." — [Add Clerk authentication to a React app with the Clerk CLI](https://clerk.com/articles/add-clerk-authentication-to-a-react-app-with-the-clerk-cli))

Core commands:

| Command | Purpose |
|---|---|
| `clerk init` | Detects framework, scaffolds Clerk into the project (SDK install, middleware, auth pages), provisions a new app via the Platform API if not already linked |
| `clerk config` | Pull/patch instance settings (sign-in methods, redirects, session policy, billing config) as JSON, with dry-run diffs |
| `clerk api` | Authenticated client for the Backend API — `clerk api ls` lists endpoints; run with no args for an interactive request builder |
| `clerk api --platform` | Same client pointed at the Platform API for workspace-level operations |
| `clerk apps list` / `clerk apps create` | View/create Clerk applications from the terminal (Platform API) |
| `clerk doctor` | Validates your project's Clerk integration, flags common issues |
| `clerk open` | Opens the Clerk Dashboard in your browser |
| `clerk deploy` | Part of the three-command flow (`init`, `config`, `deploy`) advertised on the CLI's landing page |

("The Clerk CLI is a command-line tool for you and your agents to set up and manage Clerk authentication directly from the terminal. It supports 10+ frameworks." — [Clerk CLI - Guides](https://clerk.com/docs/cli); [Introducing Clerk CLI](https://clerk.com/blog/introducing-clerk-cli); [Clerk CLI product page](https://clerk.com/cli))

The CLI is explicitly designed for **agentic workflows** as well as humans: "One CLI for agents and developers. Purpose-built for developers using AI to ship fast and securely." ([Clerk CLI](https://clerk.com/cli)) It is open source ([Clerk CLI changelog entry](https://clerk.com/changelog/2026-04-22-clerk-cli)).

### 7.8 The Official `clerk/skills` Repository

Clerk publishes a set of structured "agent skills" (Markdown playbooks with embedded scripts) at [github.com/clerk/skills](https://github.com/clerk/skills), designed to be loaded by AI coding agents. Notable skill files referenced throughout this document:

- `skills/core/clerk-setup/SKILL.md` — framework detection, quickstart fetch, key acquisition (Keyless vs. manual)
- `skills/core/clerk-backend-api/SKILL.md` — full Backend API explorer/executor with a "FAST PATH" of pre-baked common operations, mandatory safety checks before writes, and rate-limit tables
- `skills/features/clerk-orgs/SKILL.md` — organization management patterns
- `skills/features/clerk-webhooks/SKILL.md` — webhook verification and sync patterns
- `skills/clerk-billing` — billing setup and PLAPI-based config management

These are a uniquely authoritative, code-first source for exact endpoint shapes, rate limits, and safe operational patterns (e.g., mandatory confirmation before DELETE requests).

---

## 8. Rate Limits, Pagination, Errors

### 8.1 Rate Limit Behavior

On exceeding a limit, Clerk returns **HTTP 429** and a `Retry-After` response header containing the number of seconds until the block expires: "If you receive a `429` response code, you have been rate limited. All subsequent requests to that specific endpoint will be blocked for a given amount of time... Requests that have been rate limited will receive the `Retry-After` response header, which contains the number of seconds after which the block expires." ([Rate limits - How Clerk works | Clerk Docs](https://clerk.com/docs/guides/how-clerk-works/system-limits))

**Backend API limits** (identified per application instance via the Secret Key):

| Scope | Limit |
|---|---|
| Production instances (general) | 1,000 requests / 10 seconds |
| Development instances (general) | 100 requests / 10 seconds |
| `GET /v1/jwks` | **No rate limit** |
| `POST /v1/invitations` | 100 requests / hour |
| `POST /v1/invitations/bulk` | 25 requests / hour |
| `POST /v1/organizations/{id}/invitations` | 250 requests / hour |
| `POST /v1/organizations/{id}/invitations/bulk` | 50 requests / hour |
| `PATCH /v1/users/{id}`, `.../metadata` | 10 requests / 10 seconds per user |
| `PATCH /v1/organizations/{id}`, `.../metadata` | 10 requests / 10 seconds per organization |
| `PATCH /v1/organizations/{id}/memberships/{user_id}`, `.../metadata` | 10 requests / 10 seconds per membership |

**Frontend API limits** (identified per user/IP):

| Scope | Limit |
|---|---|
| `POST /v1/sign_ins` (create) | 5 requests / 10 seconds |
| `POST /v1/sign_ups` (create) | 5 requests / 10 seconds |
| `/v1/sign_ins/attempt_(first\|second)_factor` | 3 requests / 10 seconds |
| `/v1/sign_ups/attempt_verification` | 3 requests / 10 seconds |

([Rate limits - How Clerk works | Clerk Docs](https://clerk.com/docs/guides/how-clerk-works/system-limits); cross-referenced with [clerk/skills clerk-backend-api SKILL.md](https://github.com/clerk/skills/blob/main/skills/core/clerk-backend-api/SKILL.md))

**Important gotcha:** `currentUser()` (Next.js helper) internally calls `GET /v1/users/me`, so it counts against the general rate limit. Prefer `auth()`, which reads claims straight from the already-verified session token with **no** network call, when you only need session claims rather than the full user object ([clerk/skills clerk-backend-api SKILL.md](https://github.com/clerk/skills/blob/main/skills/core/clerk-backend-api/SKILL.md); [Rate limits - How Clerk works](https://clerk.com/docs/guides/how-clerk-works/system-limits)).

**Handling 429s (Node example):**
```ts
async function callWithBackoff(fn: () => Promise<any>) {
  try {
    return await fn();
  } catch (err: any) {
    if (err.status === 429) {
      const retryAfter = Number(err.retryAfter ?? err.response?.headers?.get('Retry-After') ?? 1);
      await new Promise((r) => setTimeout(r, retryAfter * 1000));
      return callWithBackoff(fn);
    }
    throw err;
  }
}
```

### 8.2 Pagination

List endpoints use `limit` + `offset` query parameters:

- Default `limit` is typically `10`; **max `limit` for `/users` is 500** ([clerk/skills clerk-backend-api SKILL.md](https://github.com/clerk/skills/blob/main/skills/core/clerk-backend-api/SKILL.md)).
- SDK responses are wrapped in a `PaginatedResourceResponse` shape: `{ data: T[], totalCount: number }` on the JS/TS SDK side ([PaginatedResourceResponse - Types](https://clerk.com/docs/reference/backend/types/paginated-resource-response)).

```ts
const { data, totalCount } = await clerkClient.users.getUserList({ limit: 100, offset: 0 });
```
```bash
curl "https://api.clerk.com/v1/users?limit=100&offset=100" \
  -H "Authorization: Bearer $CLERK_SECRET_KEY"
```
For full enumeration, loop incrementing `offset` by `limit` until the returned page is shorter than `limit` or `offset >= totalCount`.

### 8.3 Error Response Format

Every error response follows a consistent shape:

```json
{
  "errors": [
    {
      "message": "is invalid",
      "long_message": "http://localhost:3000/ does not match one of the allowed values for parameter redirect_url",
      "code": "form_param_value_invalid",
      "meta": { "param_name": "redirect_url" }
    }
  ],
  "clerk_trace_id": "c17d3948ee86decbf95d11b41c8ddfa4"
}
```
([Stack Overflow — Clerk with Next.js local development error](https://stackoverflow.com/questions/78794275/clerk-with-next-js-local-development-error/78840653))

Fields:
- **`errors`** — array of error objects, each with `message` (short), `long_message` (detailed, human-readable), `code` (machine-readable error code, e.g. `form_param_unknown`, `authorization_invalid`), and `meta` (contextual data, e.g. which `param_name` was invalid).
- **`clerk_trace_id`** — "A unique identifier for tracing the specific request, useful for debugging" — include this when contacting Clerk support ([ClerkErrorResponse - Clerk Android docs](https://clerk-android.clerkstage.dev/source/api/com.clerk.api.network.model.error/-clerk-error-response/index.html)).

**Common status codes:**

| Code | Meaning | Example cause |
|---|---|---|
| 400 | Request was not successful | Malformed request |
| 401 | Authentication invalid | Missing/invalid Secret Key or token |
| 402 | Payment required | Feature gated behind a paid plan |
| 403 | Authorization invalid | Valid credentials, insufficient permission |
| 404 | Resource not found | Invalid ID in path |
| 422 | Invalid request parameters | e.g., unknown/misnamed field (`email_address` instead of `email_addresses`) |
| 429 | Rate limited | See §8.1 |

Example 422 in the wild:
```json
{
  "errors": [
    {
      "message": "is unknown",
      "long_message": "email_address is not a valid parameter for this request.",
      "code": "form_param_unknown",
      "meta": { "param_name": "email_address" }
    }
  ],
  "clerk_trace_id": "95axxxxxxxxxxx"
}
```
The fix here: the `CreateUser` endpoint expects the **plural, array-typed** field `email_addresses`, not `email_address` ([clerk-import-export-users ERROR.md](https://github.com/jacksonkasi1/clerk-import-export-users/blob/main/ERROR.md)).

SDK-level error types wrap this shape: JS SDKs expose `ClerkAPIResponseError` (`clerkError: true`, `status`, `message`, `clerkTraceId`); the Go SDK exposes `APIErrorResponse{ Errors []Error, HTTPStatusCode int, TraceID string }`; Python raises `ClerkErrors` (domain errors) or `SDKError` (generic) ([ClerkAPIResponseError - Astro Types](https://clerk.com/docs/astro/reference/types/clerk-api-response-error); [pkg.go.dev clerk-sdk-go/v3](https://pkg.go.dev/github.com/clerk/clerk-sdk-go/v3); [Users API | clerk-sdk-python | DeepWiki](https://deepwiki.com/clerk/clerk-sdk-python/5.1-users-api)).

### 8.4 Idempotency

Clerk's Backend API does not document a dedicated `Idempotency-Key` header mechanism in its public reference. For idempotent behavior on your own side (e.g., webhook processing, retried creates), use natural keys instead: rely on `svix-id` for webhook dedup (§6.4), and rely on Clerk's own uniqueness constraints (e.g., duplicate email on `CreateUser` returns a `422`) to avoid double-creating resources on retry. When retrying failed requests against write endpoints, check first via a `GET`/list call keyed on your natural identifier (email, `external_id`) before re-issuing a `POST`.

---

## 9. Common Recipes / Playbooks

### 9a. Provision a New Clerk App via Platform API, Then Seed Users/Orgs

```bash
#!/usr/bin/env bash
set -euo pipefail

# 1. Create the application (Platform API)
APP_JSON=$(curl -s https://api.clerk.com/v1/platform/applications \
  --request POST \
  --header 'Content-Type: application/json' \
  --header "Authorization: Bearer $CLERK_PLATFORM_TOKEN" \
  --data '{
    "name": "tenant-acme",
    "environment_types": ["development", "production"],
    "template": "b2b-saas"
  }')

DEV_SECRET=$(echo "$APP_JSON" | python3 -c "
import sys, json
app = json.load(sys.stdin)
dev = next(i for i in app['instances'] if i['environment_type'] == 'development')
print(dev['secret_key'])
")

# 2. Seed an admin user (Backend API, using the NEW instance's secret key)
USER_JSON=$(curl -s -X POST "https://api.clerk.com/v1/users" \
  -H "Authorization: Bearer $DEV_SECRET" \
  -H "Content-Type: application/json" \
  -d '{"email_address": ["admin@acme.com"], "password": "TempPassw0rd!23"}')

ADMIN_ID=$(echo "$USER_JSON" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")

# 3. Seed the first organization
ORG_JSON=$(curl -s -X POST "https://api.clerk.com/v1/organizations" \
  -H "Authorization: Bearer $DEV_SECRET" \
  -H "Content-Type: application/json" \
  -d "{\"name\": \"Acme Corp\", \"created_by\": \"$ADMIN_ID\"}")

echo "Provisioned app + admin user + org:"
echo "$ORG_JSON" | python3 -m json.tool
```
This composes the exact patterns shown in Clerk's own [Platform API reference](https://clerk.com/docs/reference/platform-api/tag/applications) and [clerk-backend-api SKILL.md fast path](https://github.com/clerk/skills/blob/main/skills/core/clerk-backend-api/SKILL.md).

### 9b. Bulk-Import Users

Clerk maintains an open-source migration tool that accepts a JSON or CSV file (including exports straight from the Clerk Dashboard), calls the `CreateUser` Backend API endpoint per row, respects rate limits, and handles errors gracefully: "Clerk provides an open-source tool for basic migrations... The tool creates a user in Clerk using the Backend API... The tool respects backend rate limits. The tool gracefully handles errors." ([Migrating your data | Clerk Docs](https://clerk.com/docs/guides/development/migrating/overview))

Key considerations documented by Clerk:
- You must supply `password_hasher` (the hashing algorithm used by the source system) so Clerk can accept pre-hashed passwords directly rather than forcing a reset; Clerk can transparently upgrade hashes to bcrypt over time.
- You **cannot** migrate users from a Development instance to a Production instance directly — migrate into the correct instance from the start.
- Consider mapping legacy IDs into `external_id` and surfacing it via a custom session-token claim, so downstream systems can continue joining on the old ID:
  ```json
  { "external_id": "{{user.external_id}}" }
  ```
  configured under Dashboard → **Sessions** → **Customize session token** → **Claims**.

**Manual bulk-import loop (Node), respecting rate limits with basic backoff:**
```ts
import { createClerkClient } from '@clerk/backend';
import legacyUsers from './export.json';

const clerkClient = createClerkClient({ secretKey: process.env.CLERK_SECRET_KEY });

for (const u of legacyUsers) {
  try {
    await clerkClient.users.createUser({
      emailAddress: [u.email],
      passwordDigest: u.passwordHash,
      passwordHasher: 'bcrypt', // must match source system's algorithm
      externalId: u.legacyId,
      firstName: u.firstName,
      lastName: u.lastName,
    });
  } catch (err: any) {
    if (err.status === 429) {
      await new Promise((r) => setTimeout(r, 1000));
      // retry logic here
    } else {
      console.error(`Failed to import ${u.email}:`, err);
    }
  }
}
```
([Migrating your data | Clerk Docs](https://clerk.com/docs/guides/development/migrating/overview))

Two overall migration strategies documented by Clerk:
1. **Basic export/import** — snapshot-then-load; simplest but risks data being briefly out of sync and ends users' existing sessions from the old system.
2. **Trickle migration** — run both systems in parallel, migrating users to Clerk as they authenticate, with a scheduled cutover once inactive-user tail is small enough to batch-import. Note that Clerk bills by Monthly Active/Resolved Users, not total user-table size, making parallel-running comparatively cheap: "Clerk only charges by [MAU]... never based on the total number of users in the user table. Free plans have a limit of 50,000 MRUs per application." ([Migrating your data | Clerk Docs](https://clerk.com/docs/guides/development/migrating/overview))

### 9c. Sync Users to Your Own DB via Webhooks

See full pattern in Section 6.4. Summary checklist:
1. Configure a webhook endpoint in the Dashboard subscribed to `user.created`, `user.updated`, `user.deleted` (and `organization.*` / `organizationMembership.*` if syncing orgs too).
2. Verify every request with `verifyWebhook()` (framework helper) or the raw Svix library — **never skip verification**.
3. Use the raw request body for verification (frameworks that eagerly JSON-parse will break signature checks).
4. Key your local DB rows on Clerk's `id` (store as `externalUserId`/`clerkId`), not on mutable fields like email.
5. Make handlers idempotent using `svix-id`; offload slow work to a queue.

### 9d. Implement M2M Auth with the API Keys Feature

Use this when **your application's own users** need to call your API programmatically (not for your internal services — that's M2M tokens, §9-equivalent below).

1. Enable in Dashboard: **API keys** page → **Enable API keys** → choose User and/or Organization scope.
2. Let users self-serve via `<UserProfile />`, or build custom UI with hooks.
3. Server-side creation (e.g., an admin-issued key):
   ```ts
   const apiKey = await clerkClient.apiKeys.create({
     name: 'Integration Key',
     subject: userId,
     scopes: ['read:orders'], // optional; restrict included permissions
     secondsUntilExpiration: 60 * 60 * 24 * 90,
   });
   // apiKey.secret is shown ONLY here — persist it to the caller now
   ```
4. Verify on every protected request:
   ```ts
   const authReq = await clerkClient.authenticateRequest(request, {
     acceptsToken: ['session_token', 'api_key'],
   });
   if (authReq.isAuthenticated) {
     const { userId, orgId } = authReq.toAuth(); // scoped to whichever Clerk populated
   }
   ```
5. Revoke immediately if compromised:
   ```ts
   await clerkClient.apiKeys.revoke({ apiKeyId: apiKey.id });
   ```
("API keys are scoped to either a user or an org, never both — pass through whichever Clerk populated." — [API keys & M2M - eve](https://clerk.com/docs/guides/ai/eve/api-keys-and-m2m))

### 9e. Impersonation / Actor Tokens

See full endpoint detail in Section 4.3. End-to-end recipe:

```bash
# 1. Create the actor token (admin_user_id is the impersonator)
RESP=$(curl -s -X POST "https://api.clerk.com/v1/actor_tokens" \
  -H "Authorization: Bearer $CLERK_SECRET_KEY" \
  -H "Content-Type: application/json" \
  -d "{
    \"user_id\": \"$TARGET_USER_ID\",
    \"actor\": {\"sub\": \"$ADMIN_USER_ID\"},
    \"expires_in_seconds\": 3600,
    \"session_max_duration_in_seconds\": 1800
  }")

# 2. Redirect the admin's browser to the returned `url` to consume it
IMPERSONATION_URL=$(echo "$RESP" | python3 -c "import sys,json; print(json.load(sys.stdin)['url'])")
echo "Redirect admin to: $IMPERSONATION_URL"
```
On the client, detect and surface the impersonation state:
```ts
const { userId, actor } = useAuth();
if (actor) {
  // Render a "You are impersonating <userId> as <actor.sub>" banner
}
```
To end early: `POST /v1/actor_tokens/{actor_token_id}/revoke`. The simplest path for ad-hoc support use is the Dashboard's built-in **Impersonate user** action on the Users page, which performs the same flow without any code ([User impersonation](https://clerk.com/docs/guides/users/impersonation)).

### 9f. Organization-Based Multi-Tenancy

Baseline pattern for B2B apps:

1. **Create an org per tenant** (server-side, at signup/onboarding time):
   ```ts
   const org = await clerkClient.organizations.createOrganization({
     name: tenantName,
     createdBy: founderUserId,
     maxAllowedMemberships: 50, // optional cap, e.g. tied to their plan
   });
   ```
2. **Invite teammates** with role assignment (`org:admin` / `org:member`, always `org:`-prefixed):
   ```ts
   await clerkClient.organizations.createOrganizationInvitation({
     organizationId: org.id,
     emailAddress: teammateEmail,
     role: 'org:member',
   });
   ```
3. **Protect routes** by reading `orgId`/`orgRole` from `auth()` and gating with `has({ role })` or `has({ permission })` — this is the pattern the official [`clerk-orgs` skill](https://github.com/clerk/skills/blob/main/skills/features/clerk-orgs/SKILL.md) documents.
4. **Scope billing per tenant** using organization-level plans (`<PricingTable for="organization" />`) and `getOrganizationBillingSubscription()` / `GET /organizations/{organization_id}/billing/subscription` to gate features by the org's plan tier server-side ([Gate features by plan tier using Clerk billing subscriptions](https://www.joinnextdev.com/a/clerk/gate-features-by-plan-tier-using-clerk-billing-subscriptions)).
5. **Enterprise SSO per tenant** using the unified `/enterprise_connections` endpoint scoped by `organization_id` (Section 4.9) — lets each customer bring their own IdP (Okta, Entra ID, Google Workspace) without affecting other tenants.
6. **Sync subscription state** by listening for `organization.subscription.*` webhook events and invalidating any cached feature-gate state immediately, rather than trusting client-supplied plan data.

---

## 10. Gotchas & Best Practices

### 10.1 Experimental / Beta Features

- **Billing APIs are explicitly experimental.** "Billing APIs are still experimental. Pin your `@clerk/nextjs` and `clerk-js` package versions." ([clerk-billing skill](https://skills.re/skills/clerk/skills/clerk-billing)) Expect breaking changes; the `/commerce/*` → `/billing/*` endpoint rename in the 2025-11-10 API version is a concrete example of churn already occurring ([Upgrade to API version 2025-11-10](https://clerk.com/docs/guides/development/upgrading/upgrade-guides/2025-11-10)).
- **M2M tokens were in public beta as of August 2025** ([M2M Tokens Public Beta](https://clerk.com/changelog/2025-08-15-m2m-beta)) — verify current GA status before depending on it for critical production paths.
- **OAuth client credentials flow is not yet supported** — only the authorization code flow is, for OAuth-based machine auth ([Machine authentication overview](https://clerk.com/docs/guides/development/machine-auth/overview)).
- **Enterprise connections API was unified in March 2026** — the legacy `/saml_connections` endpoint is deprecated and "support... may be phased out in the future" ([Create and manage enterprise connections through Clerk's API](https://clerk.com/changelog/2026-03-09-bapi-enterprise-connections)). Build new integrations against `/enterprise_connections`.

### 10.2 Versioning

Pin the Backend API version you depend on, either via your SDK's pinned release or an explicit `Clerk-API-Version` header, since breaking changes ship as new dated versions ([Clerk Backend API Reference](https://clerk.com/docs/reference/backend-api)). Always review the specific upgrade guide (e.g., [Upgrade to API version 2025-11-10](https://clerk.com/docs/guides/development/upgrading/upgrade-guides/2025-11-10)) before bumping.

### 10.3 Metadata Overwrites, Not Merges

This is one of the most common production bugs: `updateUser({ publicMetadata: {...} })` **replaces** the entire metadata object — it does not merge:

```ts
// WRONG — deletes all other publicMetadata fields
await clerkClient.users.updateUser(userId, { publicMetadata: { newField: 'value' } });

// RIGHT — read first, spread, then write
const user = await clerkClient.users.getUser(userId);
await clerkClient.users.updateUser(userId, {
  publicMetadata: { ...user.publicMetadata, newField: 'value' },
});
```
([clerk/skills clerk-backend-api SKILL.md](https://github.com/clerk/skills/blob/main/skills/core/clerk-backend-api/SKILL.md))

If you need an atomic merge instead, use the dedicated metadata-merge endpoint (`update_metadata`/`PATCH /users/{id}/metadata`), which performs a true deep merge and lets you delete individual keys by setting them to `null` ([clerk-sdk-python users README](https://github.com/clerk/clerk-sdk-python/blob/main/docs/sdks/users/README.md)).

### 10.4 The Three Metadata Types

| Type | Field | Readable by | Writable by | Use for |
|---|---|---|---|---|
| Public | `public_metadata` | Client + Server | Server only | Plan tier, roles, feature flags the frontend reads |
| Private | `private_metadata` | Server only | Server only | Stripe customer IDs, compliance flags, internal identifiers |
| Unsafe | `unsafe_metadata` | Client + Server | Client + Server | Ephemeral UI/onboarding state — **avoid sensitive data**, since the client can write it directly |

([clerk/skills clerk-backend-api SKILL.md](https://github.com/clerk/skills/blob/main/skills/core/clerk-backend-api/SKILL.md))

### 10.5 What CANNOT Be Done via API

- **Cannot migrate users from a Development instance to a Production instance** directly ([Migrating your data | Clerk Docs](https://clerk.com/docs/guides/development/migrating/overview)) — you must import into the correct target instance from the start.
- **`GET /v1/sessions/verify` is deprecated** in favor of `verifyToken()`/`authenticateRequest()` — don't build new integrations against it ([clerk-backend-api PyPI listing](https://pypi.org/project/clerk-backend-api/1.2.0/)).
- **Retrieved API-key/M2M-token secrets are shown exactly once**, at creation time; there is no "reveal secret" endpoint afterward — if lost, revoke and recreate.
- The CLI itself is explicit about scope: "The CLI handles project setup, configuration, and API access. For user management, analytics, and other advanced features, use the Clerk Dashboard directly." ([Clerk CLI - Guides](https://clerk.com/docs/cli))

### 10.6 Security Pitfalls

- **Never expose the Secret Key, machine secret keys, or end-user API-key secrets client-side.** Only the Publishable Key is designed for browser exposure ([Rotate your Clerk API keys](https://clerk.com/docs/guides/secure/rotate-api-keys)).
- **Never skip webhook signature verification**, even on "notification-only" handlers ([clerk-webhooks skill](https://github.com/clerk/skills/blob/main/skills/features/clerk-webhooks/SKILL.md)).
- **Always use the raw request body** for webhook verification — frameworks that JSON-parse before your handler sees the body will silently break signature checks ([How to Verify Webhooks with the Svix Libraries](https://docs.svix.com/receiving/verifying-payloads/how)).
- **Validate publishable/secret key liveness match at startup** (`pk_live_*` should always pair with `sk_live_*`, never `sk_test_*`) to catch environment misconfigurations before they reach production:
  ```ts
  const pkIsLive = process.env.NEXT_PUBLIC_CLERK_PUBLISHABLE_KEY!.startsWith('pk_live_');
  const skIsLive = process.env.CLERK_SECRET_KEY!.startsWith('sk_live_');
  if (pkIsLive !== skIsLive) {
    throw new Error('Clerk key mismatch: publishable and secret keys must be from the same environment');
  }
  ```
  ([clerk-multi-env-setup agent skill](https://agentskills.codes/skills/clerk-multi-env-setup))
- **Check `CLERK_BAPI_SCOPES` (or your own scope model) before performing write operations programmatically** — Clerk's own backend-API skill enforces this: "For write operations (POST/PUT/PATCH/DELETE), check `CLERK_BAPI_SCOPES` before attempting the request. If missing or insufficient, ask the user upfront." ([clerk/skills clerk-backend-api SKILL.md](https://github.com/clerk/skills/blob/main/skills/core/clerk-backend-api/SKILL.md))
- **Treat DELETE as irreversible** — deleting a user destroys the user record, all sessions, and all memberships permanently, with no undo.

### 10.7 Testing with Dev Instances

- Development instances (`pk_test_`/`sk_test_`) include **all paid functionality for free**, so you can fully stage features like enterprise SSO, billing plans, or waitlist gating before committing to a production billing plan ([How to add SSO and SAML to my SaaS Product](https://clerk.com/articles/how-to-add-sso-and-saml-to-my-saas-product)).
- Dev instances get a Clerk-provided domain automatically; production instances require you to configure and verify a custom domain.
- For Billing specifically, dev instances can use Clerk's shared development payment gateway with **no Stripe account required**; production requires a connected Stripe account purely for payment processing ([clerk-billing skill](https://skills.re/skills/clerk/skills/clerk-billing)).
- E2E/CI test suites should use dedicated dev-instance keys checked into CI secrets (never live keys) — see Clerk's own Playwright starter, which ships a `.env.local.example` demonstrating exactly this pattern with `pk_test_XXXX` / `sk_test_XXXX` placeholders ([clerk-playwright-nextjs .env.local.example](https://github.com/clerk/clerk-playwright-nextjs/blob/main/.env.local.example)).
- Use **Testing Tokens** (a dedicated Backend API resource group) to bypass bot-detection/CAPTCHA in automated end-to-end tests without weakening production protections.

### 10.8 Performance Tip: `auth()` vs. `currentUser()`

Prefer `auth()` (reads already-verified JWT claims, zero network calls, not rate-limited) over `currentUser()` (calls `GET /v1/users/me`, subject to standard Backend API rate limits) whenever you only need session claims like `userId`/`orgId`/`sessionId` rather than the full user profile ([clerk/skills clerk-backend-api SKILL.md](https://github.com/clerk/skills/blob/main/skills/core/clerk-backend-api/SKILL.md); [Rate limits - How Clerk works](https://clerk.com/docs/guides/how-clerk-works/system-limits)).

---

*End of knowledge base. All facts above are drawn from Clerk's official documentation, API references, changelog, blog, and the `clerk` and `clerk/skills` GitHub organizations, current as of July 2026. Endpoint paths, rate limits, and key formats were cross-checked against Clerk's live docs (`clerk.com/docs`), the public OpenAPI specs at [github.com/clerk/openapi-specs](https://raw.githubusercontent.com/clerk/openapi-specs/main/bapi/), and multiple official SDK repositories (Node, Python, Go, Ruby, C#, Dart) to confirm consistency across languages.*

# A Lake on the customer's own domain (white label)

[operator#947](https://github.com/oramasearch/orama-operator/issues/947), part of
[lake#1129](https://github.com/oramasearch/lake/issues/1129). Private Warehouse, Dedicated Cluster
and On-Prem are sold with "your own domain, white-labelled". This page is how the operator serves a
Lake on a host in a zone the **customer** owns, for example `data.example.com`, with TLS that renews
itself. Sign-in on that host is lake's side: lake's
[`docs/deployment/white-label-sign-in.md`](https://github.com/oramasearch/lake/blob/dev/docs/deployment/white-label-sign-in.md).

Code: `internal/controller/lake_custom_domain.go`. Tests: `internal/controller/lake_custom_domain_test.go`
(`TestCustomDomainLifecycleEnvtest` walks pending, verified, routed and removal against a real API
server; `TestCustomDomainValidationEnvtest` the CEL rules).

## What it is

```yaml
apiVersion: orama.cloud/v1alpha1
kind: Lake
spec:
  host: acme.lake.live            # the Orama host: stays, as an alias
  ingressEnabled: true
  dnsSecretRef: { name: cloudflare-lake-live }
  customDomain:
    host: data.example.com        # the customer's host
```

In a `LakeSet` it goes in the member's `spec` (or the template), like any other Lake field; the set
merges it into the member's Lake. The operator never writes the customer's DNS and holds no
credential for their zone.

## How it goes, step by step

| Phase | What the operator has done | What the customer sees |
|---|---|---|
| `Pending` | Published the records below in `status.customDomain.records` and written the origin record in the Lake's zone. Looks DNS up every minute | Nothing on their host. `acme.lake.live` works as before |
| `Verified` | Both records were found once. Created the host's own Ingress and a Let's Encrypt `Certificate`, solved with HTTP-01. Clerk allows `https://data.example.com` | Their host answers once the certificate is issued |
| `Routed` | The certificate is `Ready`. The Lake's environment moves to the customer's host (below) and the pods roll once | Every link, sign-in, callback and page is on `data.example.com`. Pages opened on `acme.lake.live` are redirected there; the API, MCP, git and the registry keep answering on it |

Condition `CustomDomainReady` says the same: `RecordsNotFound` or `WaitingForLoadBalancer`, then
`CertificatePending`, then `Routed` (True). The Lake's own `Ready` does not depend on it: the Orama
host keeps working while the customer's records are missing.

### The records the customer creates

Read them from the Lake (`kubectl get lake <name> -o jsonpath='{.status.customDomain.records}'` on
its cluster) and hand them over:

| Purpose | Type | Name | Value |
|---|---|---|---|
| `routing` | `CNAME` | `data.example.com` | `acme.origin.lake.live` (with a DNS credential), else the ingress load balancer's name, or an `A`/`AAAA` record for its address |
| `ownership` | `TXT` | `_orama-verification.data.example.com` | `orama-verification=<token>` |
| `clerk` | as Clerk asks | under the customer's zone | Only for a Clerk production instance on the customer's domain (below) |

- **The origin record.** With a `dnsSecretRef`, the operator keeps a DNS-only (not proxied)
  `CloudflareDNSRecord` `lake-<name>-custom-domain-origin` in the Lake's own zone:
  `<the Lake's host without its zone, dots as dashes>.origin.<zone>` (`acme.origin.lake.live`),
  pointing at the ingress load balancer. The customer's CNAME points at it, so their record never
  changes when the load balancer does. It is not proxied: the customer's users must reach the origin
  directly, where the customer's certificate is served.
- **The token** is derived from the Lake's UID and the host, so it differs per Lake and per host and
  is stable across reconciles. A host that someone else controls cannot be verified by a Lake that
  does not hold their zone.
- **The routing check** passes when the host's canonical name is the target, or when it resolves to
  an address the target resolves to. So a provider that flattens CNAMEs at the apex, or a proxy in
  front, still passes when it ends at the same origin.
- **Verification is kept.** Once both were found, `status.customDomain.verifiedTime` is set and a later
  failed lookup does not unroute the host; `records[].found` shows what the latest lookup sees.
  A routed host is looked up every ten minutes, only to keep `found` current. Changing the host starts
  again from `Pending`.

### TLS

- The host gets a cert-manager `Certificate` `<host>-tls` in the Lake namespace, owned by the Lake,
  from the Lake's `Issuer` (`lake-<name>-letsencrypt`). The Issuer has an HTTP-01 solver selected
  for exactly that name (`selector.dnsNames`), placed first. cert-manager prefers a `dnsNames` match
  over the zone's DNS-01 selector, so the customer's host is solved with HTTP-01 whether or not the
  Lake's zone has been read ([`origin-tls.md`](origin-tls.md)). cert-manager renews it before
  `status.customDomain.certificateNotAfter`.
- HTTP-01 needs `http://data.example.com/.well-known/acme-challenge/` to reach this cluster's ingress
  controller on port 80. The customer's CNAME to the origin record does that. If the customer proxies
  the name through their own CDN, the challenge path must reach the origin over plain HTTP until the
  first certificate is issued (see *HTTP-01: what must reach the origin* in `origin-tls.md`).
- The host has **its own Ingress**, `lake-<name>-custom-domain`, with HTTPS, the redirect from HTTP,
  and none of the Lake Ingress's Cloudflare edge settings. The Lake's own Ingress requires
  Cloudflare's client certificate (authenticated origin pulls); the customer's users do not come
  through Orama's zone, so that requirement would refuse every one of them.

### What the Lake is told (operator and lake variables)

Only once `Routed`. Before, and without `spec.customDomain`, the Lake's environment is exactly what it
was, so adding this field to the operator restarts no existing Lake.

| Variable | Value once routed | What lake does with it |
|---|---|---|
| `LAKE_PUBLIC_HOST` | `data.example.com` | Every link, OAuth/OIDC/SAML callback, passkey relying party, the console's origin, image references |
| `LAKE_HOST_ALIASES` | `acme.lake.live` (`spec.host`) | Requests there are the Lake itself: browser pages are redirected to the public host, everything else is served as on it ([lake#1136](https://github.com/oramasearch/lake/issues/1136)) |
| `LAKE_CONTROL_HOST` | `acme.lake.live` | The Lake's name on its `orama_control` rows. The operator keys those rows on `spec.host` too (`lakeRowHost`), so deployments, environments and domains keep matching when the public host moves |

The CEL rules refuse `spec.customDomain` without `host` and `ingressEnabled`, a custom host equal to
`host` or `apexHost`, and any of these three variables in `spec.env` alongside it. A lake image older
than lake#1136 ignores `LAKE_HOST_ALIASES` and `LAKE_CONTROL_HOST` and would name its control rows by
the customer's host, so **lake is rolled first** (below).

The operator itself keeps using `spec.host` for what is internal: the image pull host of methodology
runs, the Clerk webhook URL (`https://acme.lake.live/identity/clerk-webhook`), and the control rows.
Both hosts route to the same Lake.

### Clerk

With `spec.clerk`, the instance's allowed origins gain `https://data.example.com` once the records
are verified, so sign-in works the moment the host is routed. A white-labelled Lake normally has its
**own** Clerk production instance whose Frontend API is `clerk.data.example.com` (its own
`spec.clerk` Secret): the DNS records Clerk asks for in the customer's zone are not written (no Orama
token can), they are published in `status.customDomain.records` with purpose `clerk`, and the
`ClerkReady` message counts them. Records in Orama's zone are still written as before
([`clerk.md`](clerk.md)).

### Removal

Removing `spec.customDomain` deletes the host's Ingress, its `Certificate` and the certificate's
Secret (cert-manager writes it with the `orama.cloud/custom-domain` label from the Certificate's
`secretTemplate`), and the origin record; `status.customDomain` and the condition go, and the Lake
rolls back to `spec.host`. Changing the host deletes the old host's Ingress, certificate and key.
Everything is owned by the Lake, so deleting the Lake deletes it too.

## Same on every cluster

Nothing here depends on the cluster: no flag, no chart value. What differs between two Lakes is
their own `spec.customDomain` and `dnsSecretRef`. A Lake without a `dnsSecretRef` (an install that
brings its own DNS) gets a routing record pointing at the load balancer itself.

## Roll order

1. **lake** with lake#1136 (`LAKE_HOST_ALIASES`, `LAKE_CONTROL_HOST`, `LAKE_SIGN_IN_SSO_PROVIDER`) to
   every environment, through the gates. It changes nothing on a Lake that does not set them.
2. **The operator** with this change: the CRDs widened first, then the roll, then the CRDs exact
   ([`../operations/crd-rollout.md`](../operations/crd-rollout.md)). No Lake restarts: none has
   `spec.customDomain`.
3. **The Lake's resource**: add `spec.customDomain` to the member in `config/clusters/` and apply
   that file. Hand the records in its status to the customer.

## Not covered yet

- **Apps and previews under the customer's name.** `spec.apps.wildcard` and `spec.previewWildcard`
  need wildcard certificates, which need DNS-01 in the customer's zone (an `_acme-challenge` CNAME
  delegated to a zone Orama controls). Until then a white-labelled Lake's apps stay under the Orama
  apps wildcard ([operator#962](https://github.com/oramasearch/orama-operator/issues/962)).
- **The managing cluster's view.** A `LakeSet` member reports the Lake's phase, not its customer
  records; read them on the member's cluster.

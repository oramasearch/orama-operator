# Lake origin TLS and the zone's edge settings

[operator#578](https://github.com/oramasearch/orama-operator/issues/578). Every Lake Ingress serves
HTTPS with a publicly trusted certificate, and the Cloudflare zone in front of it is secured by the
operator, never by hand. Cloud v1 has had origin TLS since
[#457](https://github.com/oramasearch/orama-operator/issues/457) (`ingress_tls.go`); this page is the
Lake form of it, plus the zone settings and the check that only Cloudflare reaches the origin.

Code: `internal/controller/lake_origin_tls.go` (Lake, apps, environments),
`internal/controller/edgezonepolicy_controller.go` (`EdgeProvider`, `EdgeZonePolicy`),
`internal/controller/cloudflarezonepolicy_controller.go` (`CloudflareZonePolicy`, the only code that
changes a zone's settings), `internal/controller/oramazettacluster_controller.go` (public Zettas).

## The path, end to end

```
browser ──HTTPS (edge certificate)──▶ Cloudflare ──HTTPS, Full (strict), client certificate──▶ ingress-nginx ──▶ Lake / app / environment
                                                     origin certificate: Let's Encrypt, DNS-01
```

| Leg | What secures it | Where it comes from |
|---|---|---|
| Browser to Cloudflare | The edge certificate (Universal SSL covers the zone apex and one level below it) and **Always Use HTTPS** | The zone; Always Use HTTPS set by `CloudflareZonePolicy` |
| Cloudflare to the origin | **Full (strict)**: TLS, with the origin's certificate verified | The zone mode, set by `CloudflareZonePolicy` once every origin certificate is valid |
| The origin's certificate | Let's Encrypt, issued and renewed by cert-manager, DNS-01 through the Lake's `dnsSecretRef` | The Lake's `Issuer`; one certificate per host, or a wildcard |
| Only Cloudflare reaches the origin | **Authenticated origin pulls**: Cloudflare presents its client certificate, and every Lake Ingress requires it | The zone setting `tls_client_auth`, set by `CloudflareZonePolicy`; the requirement on each Ingress, set by the Lake once the zone has it |
| Plain HTTP | Redirected to HTTPS by nginx on every Lake host, and by Cloudflare zone-wide once every host has an edge certificate | The Ingresses, once the zone's mode is Full or stricter |

## Certificates

Each Lake with a `dnsSecretRef` owns a namespaced cert-manager **Issuer**
`lake-<name>-letsencrypt` in its namespace: Let's Encrypt production, one DNS-01 solver through the
Secret's `token` key. Each environment therefore uses its own zone's token: `lake.live` Lakes the
`lake.live` token, nonprod Lakes the `oramadev.net` one. cert-manager writes the ACME account key
into `lake-<name>-letsencrypt-account`. Wildcards need DNS-01, which is why there is no HTTP-01
solver.

| Ingress | Hosts | Certificate |
|---|---|---|
| `lake-<name>` (Lake namespace) | `host`, `apexHost` | One per host through ingress-shim (`applyIngressTLS`), Secret `<host>-tls`. A hand-supplied `spec.tls.secretName` replaces them |
| `lake-<name>-apps-wildcard` (Lake namespace) | `*.<apps wildcard>` (default `*.<host>`) | The apps wildcard certificate, Secret `wildcard.<suffix>-tls` |
| `lake-<name>-preview-wildcard` (Lake namespace) | `*.<previewWildcard>`, when set | The preview wildcard certificate |
| A `LakeApp`'s Ingress (workspace namespace) | The app's host | Under the apps wildcard (one label): a copy of the wildcard certificate. Any other host: its own `Certificate` `<host>-tls` in the Lake namespace, owned by the `LakeApp` |
| A `LakeEnvironment`'s Ingress (workspace namespace) | The environment's host and its domains | A preview host: a copy of the preview wildcard certificate. A named host and each domain: its own `Certificate` in the Lake namespace, owned by the `LakeEnvironment` |
| A public `OramaZettaCluster`'s Ingress | `spec.host` | Its own `Issuer` `<name>-letsencrypt` from its `dnsSecretRef`, through ingress-shim. Zettas a Lake creates have no Ingress |

- **Why certificates are issued in the Lake namespace and copied.** An Ingress can name only a Secret
  in its own namespace, and the Cloudflare token must never be put in a workspace namespace, where
  users' workloads run. So every certificate is issued where the credential is, and its Secret
  (`kubernetes.io/tls`, labelled `orama.cloud/tls-copy`) is copied next to the Ingress that serves it,
  named `<ingress>-tls-<12 hex of the host's SHA-256>`. A renewal is copied again: the app and
  environment controllers watch the Lake namespace's `*-tls` Secrets. A certificate not issued yet is
  not copied; ingress-nginx then serves no certificate and no redirect for that host until it is.
- **Why wildcards.** Let's Encrypt allows 50 new certificates per registered domain per week, and
  5 per identical set of names. One wildcard per Lake for apps and one for previews keeps a busy week
  of new apps and previews within both.
- **The wildcard Ingresses** make the origin present the wildcard certificate for any name under it,
  so the zone check below passes for the wildcard DNS record. A name with no app or environment is
  answered with a permanent redirect to `https://<host>/`; it never reaches a workload.
- **No HTTP-only fallback.** A Lake with an Ingress but neither `dnsSecretRef` nor `tls.secretName`
  fails its reconcile and says so; no Ingress is written. A public Zetta without `dnsSecretRef` fails
  the same way. **Known gap:** apps and environments of a Lake without a `dnsSecretRef` (an install
  that brings its own DNS) have no certificate source and stay on HTTP until HTTP-01 issuance is built.
- **Hosts outside the Lake's zone** (a custom domain in another zone) cannot pass DNS-01 through the
  Lake's token: the host's `Certificate` stays not ready and nginx serves it without a certificate,
  as before. The DNS record for such a domain is not in the Lake's zone either.

## The zone: `EdgeProvider`, `EdgeZonePolicy`, `CloudflareZonePolicy`

Principle 10 (abstract kind, provider resource, concrete kind). Each Lake with a `dnsSecretRef`
owns both of these, named `lake-<name>-edge`:

- an **`EdgeProvider`** of type `cloudflare` naming the `dnsSecretRef` Secret's `token` (and its
  `accountID`);
- an **`EdgeZonePolicy`** naming that provider and the Secret's `zoneID`.

The `EdgeZonePolicy` controller creates an owned **`CloudflareZonePolicy`**, the only kind that calls
Cloudflare for zone settings, and copies its status back. Every ten minutes it reads the zone's
settings and its proxied A, AAAA and CNAME records, probes every proxied host, and turns each setting
on only when it is safe, in this order:

| Step | Setting | Turned on when |
|---|---|---|
| 1 | `ssl` = `full` (from `off` or `flexible`) | Every proxied host's origin completes a TLS handshake |
| 2 | `ssl` = `strict` | Every proxied host's origin presents a certificate valid for that host |
| 3 | `tls_client_auth` = `on` (authenticated origin pulls, Cloudflare's shared certificate) | The mode is `full` or `strict` |
| 4 | `always_use_https` = `on` | Every proxied host has a valid edge certificate |

- **The checks are probes, not bookkeeping.** For each record the operator dials the record's
  target on 443 with the record's name as SNI and verifies the certificate against the public roots
  (the origin check), and dials the name itself through public DNS (the edge check). A wildcard
  record `*.x` is checked as `orama-edge-probe.x`. So a zone shared by several clusters, or with hosts
  the operator does not manage, is judged by what every origin actually serves, not by what one
  cluster knows.
- **Nothing is turned off.** A host that later fails is listed in `status.hosts` and in the
  conditions; the zone is not downgraded. Deleting a policy changes nothing in Cloudflare. Several
  policies may name one zone (one per Lake, on several clusters); they ask for the same settings.
- **Automatic SSL/TLS.** A zone in Cloudflare's automatic mode (`ssl_automatic_mode` = `auto`) is
  switched to `custom` before the mode is set, so the mode the operator sets holds.
- **Status.** `kubectl get edgezp -A` shows `Origin` (the mode), `AOP`, `HTTPS` and `Phase`
  (`Ready`, `Progressing` while a step waits on a check, `Failed` when the zone cannot be read or
  changed). Conditions: `Credentials`, `OriginCertificates`, `EdgeCertificates`, `OriginTLSStrict`,
  `AuthenticatedOriginPulls`, `AlwaysUseHTTPS`. `status.hosts` lists up to 50 failing hosts with the
  reason for each leg. A refused read keeps the last observed settings, so a Cloudflare outage does
  not make the Lakes drop their redirect or client-certificate check.

### The token's permissions

On the zone, the API token in `dnsSecretRef` needs:

| Permission (as the dashboard names it) | For |
|---|---|
| Zone > DNS > Edit | DNS records (already required) and DNS-01 challenges; includes DNS Read, which lists the proxied records |
| **Zone > Zone Settings > Edit** | Reading and setting `ssl`, `ssl_automatic_mode`, `tls_client_auth`, `always_use_https` |

A token without Zone Settings is reported by name: the policy goes `Failed` with the message
*"the API token may not read the zone's settings: add Zone > Zone Settings > Read (Edit includes it)
for this zone"*, or the Edit form when only the change is refused. The Lakes then keep serving HTTPS
with no redirect and no client-certificate check, which is safe in any mode.

## What each Lake Ingress does with the zone's state

The Lake reads its `EdgeZonePolicy` status and sets, on every Lake Ingress (its own, the wildcards,
its apps' and its environments'):

| Zone state | `ssl-redirect` | Client certificate |
|---|---|---|
| Not observed yet, no policy, `off` or `flexible` | `"false"`: under Flexible, Cloudflare reaches the origin over HTTP and a redirect would loop | Not required |
| `full` or `strict` | `"true"`, unless a host of the Ingress is in the policy's `status.hosts` without an edge certificate (itself or its wildcard), then `"false"`: a visitor sent to HTTPS could not follow | Not required |
| `full` or `strict`, authenticated origin pulls on | `"true"` | **Required**: `auth-tls-secret` names a Secret `<ingress>-edge-client-ca` (`ca.crt` = Cloudflare's origin-pull CA, from the policy's `status.originClientCAPEM`), `auth-tls-verify-client: "on"`, `auth-tls-verify-depth: "1"` |

`"false"` is written rather than left out because ingress-nginx redirects every host that has a
certificate by default.

## Only Cloudflare reaches the origin: why authenticated origin pulls

The choice was between restricting the ingress-nginx load balancer to Cloudflare's published IP
ranges (`loadBalancerSourceRanges` through the platform) and authenticated origin pulls. Authenticated
origin pulls were chosen:

- **Scoped to Lake.** The requirement is per Ingress, so Cloud v1's Ingresses on the same controller,
  the `/metrics` companions and anything else on it are unchanged. A source-range list applies to the
  whole controller on every cluster, management included.
- **Nothing to keep current.** Cloudflare's IP ranges change; a list in the platform values would go
  stale and silently cut traffic. The origin-pull CA is valid until 2029-11-01 and is in the operator.
- **Nothing in-cluster breaks.** Lake's in-cluster callers (the apps' auth subrequest, the e2e Job)
  use the Service, not the load balancer; public-host callers resolve to Cloudflare. Cloud load
  balancer health probes are TCP to the node ports. The operator's own origin probes still see the
  certificate: nginx completes the handshake and answers 400 without a client certificate.
- **The order is safe.** The zone presents the certificate before any Ingress requires it.

Its limit, stated plainly: the global origin-pull certificate is Cloudflare's, shared by every
Cloudflare account, so it proves "from Cloudflare", not "from our zone". The stronger form is
zone-level authenticated origin pulls with a certificate the operator generates and uploads (Zone >
SSL and Certificates > Edit). Plain HTTP on port 80 is not covered by a client certificate; with the
redirect on it serves only redirects.

## Rolling it out

What a release containing this does on a cluster, in order (no step is done by hand):

1. The operator rolls with the three new CRDs (`edgeproviders`, `edgezonepolicies`,
   `cloudflarezonepolicies`; `make check-crds` first).
2. Each Lake writes its `Issuer`, `EdgeProvider` and `EdgeZonePolicy`, and adds TLS to its Ingresses
   with `ssl-redirect: "false"`. cert-manager issues the certificates through DNS-01 (a minute or
   two each); the app and environment controllers copy them next to their Ingresses.
3. The zone policy probes. Once every proxied origin presents a valid certificate it sets Full
   (strict); from Full it turns on authenticated origin pulls. A zone with a proxied host the
   operator does not serve (or whose certificate is not valid) stays where it is and lists the host.
4. The Lakes see the zone's state and turn on the redirect and the client-certificate requirement.
5. Always Use HTTPS waits until every proxied host has an edge certificate. App hosts two levels
   below the zone apex (`<app>.eu.lake.live`, `<app>.lake-dev.oramadev.net`) are not covered by
   Universal SSL, so in those zones this step waits until an edge certificate covers them (an
   Advanced Certificate for `*.eu.lake.live` and `*.us.lake.live`, ordered in Cloudflare) or the apps
   wildcard moves one level below the apex. Meanwhile nginx redirects every Lake host the edge has
   a certificate for, and leaves the Ingresses of uncovered hosts (those apps, the apps wildcard) on
   HTTP and HTTPS as they were.

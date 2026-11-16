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
                                                     origin certificate: Let's Encrypt, DNS-01 in the Lake's zone, HTTP-01 elsewhere
```

| Leg | What secures it | Where it comes from |
|---|---|---|
| Browser to Cloudflare | The edge certificate (Universal SSL covers the zone apex and one level below it) and **Always Use HTTPS** | The zone; Always Use HTTPS set by `CloudflareZonePolicy` |
| Cloudflare to the origin | **Full (strict)**: TLS, with the origin's certificate verified | The zone mode, set by `CloudflareZonePolicy` once every origin certificate is valid |
| The origin's certificate | Let's Encrypt, issued and renewed by cert-manager: DNS-01 through the Lake's `dnsSecretRef` for names in its zone, HTTP-01 for any other name | The Lake's `Issuer`; one certificate per host, or a wildcard (DNS-01 only) |
| Only our zone reaches the origin | **Zone-level authenticated origin pulls**: Cloudflare presents the zone's own client certificate, signed by a CA the operator generated, and every Lake Ingress requires it | The certificate, uploaded and turned on by `CloudflareZonePolicy` ([operator#633](https://github.com/oramasearch/orama-operator/issues/633)); before that, global pulls (`tls_client_auth`) with Cloudflare's shared certificate. The requirement on each Ingress, set by the Lake once the zone has it |
| Plain HTTP | Redirected to HTTPS by nginx on every Lake host, and by Cloudflare zone-wide once every host has an edge certificate | The Ingresses, once the zone's mode is Full or stricter |

## Certificates

Each Lake owns a namespaced cert-manager **Issuer** `lake-<name>-letsencrypt` in its namespace:
Let's Encrypt production. cert-manager writes the ACME account key into
`lake-<name>-letsencrypt-account`.

### The solver is chosen per host ([operator#632](https://github.com/oramasearch/orama-operator/issues/632))

A host whose zone has a DNS-01 credential is always solved with DNS-01; any other host with
HTTP-01. The choice is made per host, from the Lake's `dnsSecretRef` and the zone its
`EdgeZonePolicy` reads, never per cluster: every cluster runs the same code, and two Lakes on one
cluster can differ only by their own `dnsSecretRef`.

| The Lake | The Issuer's solvers | So |
|---|---|---|
| Has a `spec.customDomain` (any of the rows below too) | First, HTTP-01 with the selector `dnsNames: [<customer host>]` | The customer's host is always HTTP-01: a `dnsNames` match wins over the zone's `dnsZones` DNS-01 selector and over a solver with no selector ([`custom-domains.md`](custom-domains.md)) |
| Has a `dnsSecretRef`, and its zone's name is known (`EdgeZonePolicy` `status.zone`) | DNS-01 through the Secret's `token`, with the selector `dnsZones: [<zone>]`; then HTTP-01 with no selector | The zone and every name under it (the Lake's host, apex, wildcards, apps and named environments in the zone) use DNS-01: cert-manager always prefers a solver whose `dnsZones` matches over one with no selector. A custom domain in another zone uses HTTP-01 |
| Has a `dnsSecretRef`, zone not read yet (or never readable) | DNS-01 alone, no selector | Every name uses DNS-01, as before HTTP-01 existed. A name outside the zone cannot be issued until the zone is read |
| Has no `dnsSecretRef` (an install that brings its own DNS) | HTTP-01 alone | Every host is issued through HTTP-01. The Lake renders no wildcard Ingress and asks for no wildcard certificate: wildcards need DNS-01 |

Each environment therefore uses its own zone's token: `lake.example.com` Lakes the `lake.example.com` token,
nonprod Lakes the `nonprod.example.com` one. The zone's name is read by `CloudflareZonePolicy` with
`GET /zones/<zoneID>`, which needs Zone > Zone > Read; cert-manager's Cloudflare DNS-01 solver
already needs the same permission to find a name's zone, so a token that issues certificates today
has it. A token without it is named in the policy's `ZoneName` condition, and the Lake keeps DNS-01
for every name.

### HTTP-01: what must reach the origin

Let's Encrypt fetches `http://<host>/.well-known/acme-challenge/<token>` on port 80. cert-manager
answers it with a temporary Ingress of the Lake's ingress class (`spec.ingressClassName`, default
`nginx`) in the Lake namespace, for that host and path only.

- **On the origin nothing is needed.** ingress-nginx never redirects the challenge path to HTTPS
  (its default `no-tls-redirect-locations` is `/.well-known/acme-challenge`), and the client
  certificate of authenticated origin pulls is checked on HTTPS only, so a Lake Ingress with the
  redirect and the requirement on does not block the challenge on port 80.
- **The host's DNS must lead to this cluster's ingress controller on port 80.** That is the case
  for a host that points at the load balancer directly (DNS only), which is how an install that
  brings its own DNS and a custom domain in someone else's zone are normally set up.
- **Behind a Cloudflare proxy it is not reliable, which is why no host in a zone the Lake has a
  credential for ever uses it.** Cloudflare does not exempt the challenge path from Always Use
  HTTPS; Let's Encrypt follows the redirect to HTTPS, and under Full (strict) Cloudflare then needs
  a valid origin certificate for the host, which is the certificate being issued, and the origin
  needs Cloudflare's client certificate once authenticated origin pulls are on. Every host in our
  Cloudflare zones has the zone's credential and stays on DNS-01. A custom domain that its owner
  proxies through their own Cloudflare zone must keep Always Use HTTPS off for it, or use their
  zone's SSL mode Full (not strict), until its first certificate is issued; or point it at the
  origin unproxied.
- **Previews are not issued through HTTP-01.** Let's Encrypt allows 50 new certificates per
  registered domain per week, so a certificate per preview would soon also block the Lake's other
  hosts. A preview host that no preview wildcard certificate covers (a Lake without a
  `dnsSecretRef`) stays on HTTP; its custom domains are still issued.

| Ingress | Hosts | Certificate |
|---|---|---|
| `lake-<name>` (Lake namespace) | `host`, `apexHost` | One per host through ingress-shim (`applyIngressTLS`), Secret `<host>-tls`. A hand-supplied `spec.tls.secretName` replaces them |
| `lake-<name>-apps-wildcard` (Lake namespace) | `*.<apps wildcard>` (default `*.<host>`) | The apps wildcard certificate, Secret `wildcard.<suffix>-tls` |
| `lake-<name>-custom-domain` (Lake namespace) | `spec.customDomain.host`, once verified | Its own `Certificate` `<host>-tls`, HTTP-01 through a solver selected for that name alone; none of the edge settings below, because the customer's users do not come through Orama's zone ([`custom-domains.md`](custom-domains.md)) |
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
- **A certificate is never shared across owners**
  ([#776](https://github.com/oramasearch/orama-operator/issues/776)). When `<host>-tls` already belongs to
  another `LakeApp` or `LakeEnvironment`, a second owner naming the same host gets no copy of it, and a
  copy made for it before is deleted: the private key of one tenant's host is never copied into another
  tenant's namespace. The domain intake refuses such a claim earlier (`environments.md`, *The first
  claim keeps a host*); this is the check behind it.
- **Why wildcards.** Let's Encrypt allows 50 new certificates per registered domain per week, and
  5 per identical set of names. One wildcard per Lake for apps and one for previews keeps a busy week
  of new apps and previews within both.
- **The wildcard Ingresses** make the origin present the wildcard certificate for any name under it,
  so the zone check below passes for the wildcard DNS record. A name with no app or environment is
  answered with a permanent redirect to `https://<host>/`; it never reaches a workload.
- **Every host has a certificate source.** A Lake with an Ingress and no `tls.secretName` has its
  hosts issued by its Issuer, through DNS-01 or HTTP-01 as above; a Lake without a `dnsSecretRef` is
  no longer refused. Until a certificate is issued ingress-nginx serves the host with its default
  certificate and no redirect, as for any certificate not issued yet. A public Zetta without
  `dnsSecretRef` still fails its reconcile (its Ingress has no HTTP-01 path yet).
- **Hosts outside the Lake's zone** (a custom domain in another zone) are issued through HTTP-01
  once the zone's name is known. The DNS record for such a domain is not in the Lake's zone; its
  owner points it at the cluster.
- **A Lake without a `dnsSecretRef` has no zone policy**, so its Ingresses get neither the redirect
  nor the client-certificate requirement: the operator does not know what is in front of it.

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
| 3b | Zone-level authenticated origin pulls: the zone's own certificate uploaded (`POST /zones/{zone}/origin_tls_client_auth`) and turned on (`PUT .../origin_tls_client_auth/settings` `{"enabled": true}`) | The mode is `full` or `strict`, and every proxied origin that asks for a client certificate accepts the zone's CA, made once by the managing cluster and copied by the set to every cluster serving the zone ([below](#zone-level-authenticated-origin-pulls-the-operators-own-certificate)) |
| 4 | An Advanced Certificate ordered for each proxied host more than one level below the apex (lake#798) | The host has no valid edge certificate and no live certificate pack in the zone covers it |
| 5 | `always_use_https` = `on` | Every proxied host has a valid edge certificate |

- **The checks are probes, not bookkeeping.** For each record the operator dials the record's
  target on 443 with the record's name as SNI and verifies the certificate against the public roots
  (the origin check), and dials the name itself through public DNS (the edge check). A wildcard
  record `*.x` is checked as `orama-edge-probe.x`. So a zone shared by several clusters, or with hosts
  the operator does not manage, is judged by what every origin actually serves, not by what one
  cluster knows.
- **Nothing is turned off.** A host that later fails is listed in `status.hosts` and in the
  conditions; the zone is not downgraded. Deleting a policy changes nothing in Cloudflare. Several
  policies may name one zone (one per Lake, on several clusters); they ask for the same settings.
- **Edge certificates for app hosts (lake#798).** Universal SSL covers the apex and one level below
  it (`nonprod.example.com`, `*.nonprod.example.com`), so a Lake's apps wildcard (`*.lake-testing.nonprod.example.com`,
  `*.eu.lake.example.com`) has no edge certificate and the edge refuses the handshake (alert 40). For each
  proxied host two or more levels below the apex whose edge check fails, the policy reads the zone's
  certificate packs (`status=all`) and, when no live pack lists the host (or, for a plain name, the
  wildcard one level above it), orders one: `type: advanced`, hosts *apex, the host, and for a
  wildcard its base name*, TXT validation (Cloudflare adds the records itself on a zone it serves),
  Let's Encrypt, 90 days, renewed by Cloudflare. A pending pack is not ordered again; a deleted,
  expired, failed or timed-out one is, and a pack ordered earlier in the same pass counts as live:
  hosts are taken in sorted order, so the apps wildcard's pack covers the app hosts under it and one
  reconcile orders one pack for them, not one per app. The operator never deletes a pack. The zone must have
  **Advanced Certificate Manager** (a paid Cloudflare subscription per zone); without it, or without
  the permission below, the `EdgeCertificateOrders` condition says so and the other steps go on. A
  zone whose hosts all have edge certificates makes no certificate API calls.
- **Automatic SSL/TLS.** A zone in Cloudflare's automatic mode (`ssl_automatic_mode` = `auto`) is
  switched to `custom` before the mode is set, so the mode the operator sets holds.
- **Status.** `kubectl get edgezp -A` shows `Origin` (the mode), `AOP`, `ZoneAOP`, `HTTPS` and `Phase`
  (`Ready`, `Progressing` while a step waits on a check, `Failed` when the zone cannot be read or
  changed). Conditions: `Credentials`, `OriginCertificates`, `EdgeCertificates`, `OriginTLSStrict`,
  `AuthenticatedOriginPulls`, `ZoneOriginPulls`, `EdgeCertificateOrders`, `AlwaysUseHTTPS`. `Ready` needs zone-level pulls on. `status.hosts` lists up to 50 failing hosts with the
  reason for each leg. A refused read keeps the last observed settings, so a Cloudflare outage does
  not make the Lakes drop their redirect or client-certificate check.

### The token's permissions

On the zone, the API token in `dnsSecretRef` needs:

| Permission (as the dashboard names it) | For |
|---|---|
| Zone > DNS > Edit | DNS records (already required) and DNS-01 challenges; includes DNS Read, which lists the proxied records |
| **Zone > Zone Settings > Edit** | Reading and setting `ssl`, `ssl_automatic_mode`, `tls_client_auth`, `always_use_https` |
| Zone > Zone > Read | Reading the zone's name, so names outside it are issued through HTTP-01 ([operator#632](https://github.com/oramasearch/orama-operator/issues/632)); cert-manager's DNS-01 solver already needs it |
| **Zone > SSL and Certificates > Edit** | Listing certificate packs and ordering Advanced Certificates for hosts Universal SSL does not cover (step 4); listing, uploading and deleting the zone-level origin-pull certificates and turning zone-level pulls on (step 3b). Without it the `EdgeCertificateOrders` and `ZoneOriginPulls` conditions name it |

A token without Zone Settings is reported by name: the policy goes `Failed` with the message
*"the API token may not read the zone's settings: add Zone > Zone Settings > Read (Edit includes it)
for this zone"*, or the Edit form when only the change is refused. The Lakes then keep serving HTTPS
with no redirect and no client-certificate check, which is safe in any mode.

A token without SSL and Certificates is named in the `ZoneOriginPulls` condition (*"add Zone > SSL and
Certificates > Read (Edit includes it)"*); the policy stays `Progressing` on global pulls, and fails
only when an upload or the switch itself is refused.

## What each Lake Ingress does with the zone's state

The Lake reads its `EdgeZonePolicy` status and sets, on every Lake Ingress (its own, the wildcards,
its apps' and its environments'):

| Zone state | `ssl-redirect` | Client certificate |
|---|---|---|
| Not observed yet, no policy, `off` or `flexible` | `"false"`: under Flexible, Cloudflare reaches the origin over HTTP and a redirect would loop | Not required |
| `full` or `strict` | `"true"`, unless a host of the Ingress is in the policy's `status.hosts` without an edge certificate (itself or its wildcard), then `"false"`: a visitor sent to HTTPS could not follow | Not required |
| `full` or `strict`, authenticated origin pulls on | `"true"` | **Required**: `auth-tls-secret` names a Secret `<ingress>-edge-client-ca` (`ca.crt` = the policy's `status.originClientCAPEM`: Cloudflare's origin-pull CA and the zone's CA until the zone presents its own certificate, then the zone's CA alone), `auth-tls-verify-client: "on"`, `auth-tls-verify-depth: "1"` |

`"false"` is written rather than left out because ingress-nginx redirects every host that has a
certificate by default.

A host that requires the client certificate is never served by the zetta-tunnel ingress gateway,
which verifies no client certificate: the projection refuses it on every Ingress that names it
([`infrastructure/ingress.md`](../infrastructure/ingress.md#what-it-serves),
[#798](https://github.com/oramasearch/orama-operator/issues/798)). Without that, the gateway's public
load balancer would be a second origin without the requirement.

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

Global pulls alone have a limit: the global origin-pull certificate is Cloudflare's, shared by every
Cloudflare account, so it proves "from Cloudflare", not "from our zone". Someone with their own
Cloudflare zone pointed at our load balancer would pass. So the operator goes on to zone-level pulls
with a certificate it owns (next section). Plain HTTP on port 80 is not covered by a client
certificate; with the redirect on it serves only redirects.

## Zone-level authenticated origin pulls: the operator's own certificate

[operator#633](https://github.com/oramasearch/orama-operator/issues/633). Code:
`internal/controller/cloudflarezonepolicy_originpull.go` (the zone policy) and
`internal/controller/managedset_origin_pull.go` (the LakeSet's copy).

- **One CA per zone, made by the managing cluster**. Cloudflare
  presents one client certificate per zone, and a zone may be served by several clusters
  (for example `lake.example.com` by `example-production-eu` and `example-production-us`, and a
  nonprod zone by a Lake on the managing cluster and by `example-nonprod`). So every cluster serving a zone must present and
  trust the same CA. The managing cluster's operator keeps it: the Secret
  `orama-edge-origin-pull-<zone ID>` in its namespace (`ca.crt`, `ca.key`, `tls.crt`, `tls.key`).
- **How it reaches every cluster serving the zone.** A `LakeSet` reads each member's zone from the
  `zoneID` of the member's `dnsSecretRef` (a Secret the set copies), creates the zone's Secret once if
  it is missing, renews its client certificate when due, and copies the CA certificate and the client
  certificate and key (**never `ca.key`**) into the member's namespace, under the same name and
  labelled `orama.cloud/edge-origin-pull: distributed`, together with the Secrets the set names and
  before the Lake. The set does this on its own, not through its `secrets:` list: the CA must exist
  before anything could name it, its name depends on the zone, it must reach exactly the members
  serving the zone, and nobody creates or copies it by hand. The first set to create a zone's CA
  labels it with its member's purpose (`orama.cloud/purpose`), so a production zone's CA is never
  copied into a nonprod member. Every set pass (at most five minutes apart) keeps the copies in
  step.
- **Precedence on a cluster.** A Lake that a set manages (it carries `orama.cloud/managed-by-set`)
  writes `spec.originPullSecretRef` on its `EdgeZonePolicy`, naming the copied Secret. A zone policy
  with `originPullSecretRef` uses **that Secret only**: it never creates a CA or a client certificate
  for the zone, and never writes the Secret. While the Secret is missing (or holds no valid client
  certificate) the `ZoneOriginPulls` condition says it waits for the managing cluster
  (`WaitingForDistributedCA`) and the zone stays on global pulls; while its CA cannot be read the
  policy fails. A zone policy without `originPullSecretRef` (a Lake no set manages, such as a Lake on the
  managing cluster) uses the operator's own Secret in its namespace, creating it once.
  On the managing cluster that is the very Secret the sets copy, so a development Lake and the
  `lake-nonprod` members present and trust the same `nonprod.example.com` CA.
- **The CA and the leaf.** The CA (RSA 2048, valid 10 years, a subject unique to it) is created once
  and never replaced; a Secret whose CA cannot be read fails rather than making a new CA, which would
  break every origin that trusts the old one. The client leaf (RSA 2048, `clientAuth`, valid one year)
  is replaced 45 days before it expires (Cloudflare's own alerts start at 30), by the managing
  cluster only. Every policy for the zone shares it, so they all upload the same leaf.
- **Upload and switch.** The leaf and its key are uploaded (`POST /zones/{zone}/origin_tls_client_auth`)
  and zone-level pulls turned on (`PUT .../settings` `{"enabled": true}`). Zone-level certificates take
  precedence over the global one, and of several the most recently deployed is presented. The newest
  live leaf of the zone's CA is the zone's current one, whoever uploaded it: a cluster uploads its
  leaf only when it is newer, and once the current one is `active` deletes only older leaves of the CA
  (and second copies of the current one). So a cluster whose copy of a renewed leaf has not arrived
  yet never uploads the old one again or deletes the new one. A certificate of any other CA is never
  touched.
- **What the origins trust.** `status.originClientCAPEM` is Cloudflare's global CA and the zone's CA
  together until the zone presents a leaf of the zone's CA, then the zone's CA alone, which is what
  shuts out other Cloudflare zones. `status.zoneOriginPulls` says the zone presents it. While a new
  leaf deploys, the bundle stays the zone's CA alone (both leaves verify).
- **The switch is gated by a probe.** Once the zone presents the leaf, an origin that requires a
  client certificate and does not trust the zone's CA refuses Cloudflare, and a zone may have origins
  the operator does not run. So the operator reads, for every proxied record, the origin's TLS
  CertificateRequest (the CAs it accepts, by subject). It uploads a leaf that would change what the
  zone presents, and turns zone-level pulls on, only when every origin that asks for a client
  certificate lists the zone's CA and every origin answered. Otherwise the `ZoneOriginPulls` condition
  names up to five origins it waits for. With the CA distributed, every cluster's origins accept the
  same CA, so the probes of every cluster serving the zone pass. Renewing a leaf of the CA the zone
  already presents needs no gate.
- **Never an outage from the other side.** If the zone may present a certificate of another CA (one
  uploaded by hand, or by a cluster that made its own CA before it was given the zone's), the policy
  sets `authenticatedOriginPulls` false, so its Lakes stop requiring a client certificate rather than
  refuse the edge, and says so in the condition.
- **A refused read keeps the last bundle**, so a Cloudflare outage, or a missing or unreadable Secret,
  never sends the origins back to Cloudflare's global CA while the zone presents its own.
- **Known gaps.** The CA is not rotated before its ten years run out. If zone-level pulls are turned
  off by hand after the switch, origins refuse the edge until the next check (at most ten minutes)
  turns them back on. A Lake that a set manages but whose `dnsSecretRef` Secret the set does not copy
  gets no CA, and its policy waits (`WaitingForDistributedCA`): the set can only read the zone from a
  Secret it copies.

## Rolling it out

What a release containing this does on a cluster, in order (no step is done by hand):

1. The operator rolls with the three new CRDs (`edgeproviders`, `edgezonepolicies`,
   `cloudflarezonepolicies`; `make check-crds` first).
2. Each Lake writes its `Issuer`, `EdgeProvider` and `EdgeZonePolicy`, and adds TLS to its Ingresses
   with `ssl-redirect: "false"`. cert-manager issues the certificates through DNS-01 (a minute or
   two each); the app and environment controllers copy them next to their Ingresses.
3. The zone policy probes. Once every proxied origin presents a valid certificate it sets Full
   (strict); from Full it turns on authenticated origin pulls. The zone's origin-pull CA is already
   there: the managing cluster's set created it and copied it before the Lake (a Lake no set manages
   uses the operator's own Secret, created now). Zone-level pulls follow once every origin that asks
   for a client certificate accepts that CA. A zone with a proxied host the
   operator does not serve (or whose certificate is not valid) stays where it is and lists the host.
4. The Lakes see the zone's state and turn on the redirect and the client-certificate requirement.
5. Always Use HTTPS waits until every proxied host has an edge certificate. App hosts two levels
   below the zone apex (`<app>.eu.lake.example.com`, `<app>.lake-dev.nonprod.example.com`) are not covered by
   Universal SSL, so the zone policy orders an Advanced Certificate for the apps wildcard, which
   covers them (step 4 of the policy, lake#798), and this step waits until Cloudflare has validated and deployed them. That
   needs Advanced Certificate Manager on the zone and Zone > SSL and Certificates > Edit on the
   token; until both are in place the `EdgeCertificateOrders` condition says which is missing.
   Meanwhile nginx redirects every Lake host the edge has
   a certificate for, and leaves the Ingresses of uncovered hosts (those apps, the apps wildcard) on
   HTTP and HTTPS as they were.

# Ingress through zetta-tunnel

Stage 6 of the [secure-networking design](secure-networking.md), decision D8, for
[operator#473](https://github.com/oramasearch/orama-operator/issues/473): traffic enters every
cluster through [zetta-tunnel](https://github.com/oramasearch/zetta-tunnel)'s ingress gateway
(`docs/ingress.md`), replacing ingress-nginx, which is retired upstream. ingress-nginx stays in the
platform layer until every cluster has been cut over.

## How it is installed

The gateway is part of the operator, so it is on every cluster and the same on every cluster:

| | Management cluster | Leaf cluster |
|---|---|---|
| Installed by | The operator chart from `helmfile.yaml` (release `orama-operator`) | The operator chart from `config/clusters/<cluster>/operator.yaml` (`OramaOperator`) |
| Image | `networking.zettaTunnelImage` in `chart/values.yaml`, the same value on both paths |

The operator, not a separate chart, renders the gateway, because what the gateway serves is
generated from the Ingresses the operator writes, and a separate chart would need its own private
chart repository and a pull secret in its namespace that the platform layer cannot provide for a
third-party chart. ingress-nginx stays a platform chart in `helmfile.yaml` and every
`config/clusters/*/platform.yaml`, unchanged, until the cutover below is finished on every cluster;
then it is removed from all of them in one pull request (`cmd/platform_sync_test.go`).

What the operator renders, on every cluster, in the namespace `zetta-tunnel-ingress` (restricted
Pod Security, its own `NetworkSandbox`):

| Object | What |
|---|---|
| `IngressClass/zetta-tunnel` | Controller `orama.cloud/zetta-tunnel-ingress`. Not the default class. |
| Service `zetta-tunnel-ingress` | `LoadBalancer`, TCP 443 to the gateway's 8443, `externalTrafficPolicy: Local` (as ingress-nginx needs on AKS). Exists always, so its address is known before any host moves. |
| Deployment `zetta-tunnel-ingress` | `zetta-tunnel ingress --configuration /etc/zetta-ingress/routes.json --listen 0.0.0.0:8443 --health-listen 0.0.0.0:8080 --audit -`, two replicas spread over nodes (zero while there is nothing to serve), restricted, non-root, read-only root, PodDisruptionBudget of one. Its pull secrets are copies of the manager pod's own. |
| Secret `zetta-tunnel-ingress-routes` | `routes.json` (zetta-tunnel's exact projection) and, beside it, `<host>.crt` and `<host>.key` for every served host, copied from each Ingress's TLS Secret. One Secret, mounted without `subPath`, so a complete generation reaches the gateway at once; it re-reads every two seconds. |
| ConfigMap `zetta-tunnel-ingress-report` | `served` (hosts), `skipped` (`namespace/ingress host: reason`), `on-nginx` (operator Ingresses still on class `nginx`). |
| NetworkSandbox | The public reaches only 8443; the gateway reaches only the served backends' pods on their target ports, and DNS. |

Every backend that admitted ingress-nginx now admits the gateway beside it: the Lake API
(`allow-ingress-nginx`), Orama Cloud v1's dashboard and docs, and Lake environments. Lake apps do not
(below).

## What it serves

Every Ingress the operator writes (an `orama.cloud` resource controls it), **whatever its class**:
Lake, Lake environments, Orama Cloud v1 and Zetta. So before a host moves, the gateway already serves
it; moving it only moves DNS. For each Ingress rule and path: the exact host, `/` or the path,
`Prefix` (or `Exact`), the target `http://<service>.<namespace>.svc.cluster.local:<port>`, and the
body limit from nginx's `proxy-body-size` (the Lake API's `64m`), nginx's default 1 MiB when absent.
Requests and responses stream, as `proxy-buffering: off` did.

It refuses, and reports, rather than serve something differently:

| Refused | Why |
|---|---|
| A host with no TLS Secret, or a Secret with no certificate yet | The gateway serves HTTPS only, with the host's own certificate. Certificates come from cert-manager ([#457](https://github.com/oramasearch/orama-operator/issues/457)); until it fills an Ingress's TLS Secret, that host is not served here. |
| Lake app Ingresses | An app's authentication is nginx's `auth-url` subrequest to Lake, and app transfer is metered from nginx's per-Ingress byte counters (`lake_app_transfer.go`). The gateway does neither. Apps stay on ingress-nginx (`spec.apps.ingressClass` stays `nginx`) until zetta-tunnel can, so ingress-nginx cannot be removed before then. |
| `auth-url`, `auth-signin`, rewrites, snippets, a non-HTTP backend protocol | Not implemented by the gateway; serving without them would change behaviour. |
| Every host of an Ingress with `denylist-source-range`, on every Ingress that serves it | The gateway cannot refuse a source. Orama Cloud v1's `/metrics` deny companions ([#314](https://github.com/oramasearch/orama-operator/issues/314)) cover its public writer and reader hosts, and the main Ingress's Prefix `/` would forward `/metrics` too, so those hosts stay on ingress-nginx. |
| Wildcard hosts, a host already served with another certificate, a path already routed, a missing backend Service or port | The projection would be invalid or ambiguous. |

## How a host moves: the Ingress class

DNS follows an Ingress's load-balancer address (`CloudflareDNSRecord` is written from it). The
gateway gives an Ingress of class `zetta-tunnel` its own address **only while it serves every host
of that Ingress and has an available replica**; otherwise it clears it. ingress-nginx gives
addresses only to class `nginx`. So the class decides where DNS points, and a host the gateway cannot
serve is never pointed at it.

The class is set on the resource that owns the host, so it is per cluster through the set member
that runs on that cluster:

| Resource | Field |
|---|---|
| `Lake` | `spec.ingressClassName` (its Ingress and its environments') |
| `OramaCloudV1` | `spec.components.ingressClassName` |
| `OramaZettaCluster` | `spec.ingressClassName` |

Each is `nginx` (the default) or `zetta-tunnel`. No operator flag or chart value changes, so no
cluster's operator differs from another's.

## Cutover, per cluster

Each step is a reviewed pull request that changes committed files, applied the usual way; nothing is
done by hand ([operations](operations.md)).

1. **Certificates first.** cert-manager ([#457](https://github.com/oramasearch/orama-operator/issues/457))
   issues every public host's certificate into its Ingress's TLS Secret. Cloud v1 Ingresses request
   theirs from the stack's `Issuer`, one Secret per host ([components](../components.md#origin-tls-ingress_tlsgo-457));
   Lake and Zetta Ingresses do not request one yet ([#578](https://github.com/oramasearch/orama-operator/issues/578)). Read
   `kubectl -n zetta-tunnel-ingress get configmap zetta-tunnel-ingress-report -o yaml`: the host is in
   `served`, not in `skipped`.
2. **Check the gateway serves it** before DNS moves, with the cluster's gateway address:
   `curl --resolve <host>:443:<gateway address> https://<host>/` must answer as the host does through
   nginx, with its certificate.
3. **Move the cluster's hosts**: in `config/clusters/sets/*.yaml`, set the member for that cluster to
   `ingressClassName: zetta-tunnel` (Lake, Zetta) or `components.ingressClassName: zetta-tunnel`
   (Cloud v1), and apply. The Ingress gets the gateway's address and its DNS record follows. The
   Cloudflare zone must use SSL mode **Full (strict)** for these hosts: the gateway has no plain HTTP
   listener. Rollback is the same change back to `nginx`.
4. **Confirm**: `kubectl get ingress -A` shows the gateway's address for every moved Ingress, the
   report's `skipped` is empty, and every public host answers through the gateway with its
   certificate.
5. **Remove ingress-nginx** only when, on **every** cluster, the report's `on-nginx` is empty and no
   Ingress of class `nginx` is left (Lake apps included, which needs the gateway to support their
   authentication and metering first): remove the `ingress-nginx` release from `helmfile.yaml` and
   every `config/clusters/*/platform.yaml` in one pull request, and the `allow-ingress-nginx`
   allowances with it.

## Records and metrics

The gateway runs with `--audit -`: one `ingress_request` JSON line per authorized request (host,
route, source, decision). Its Prometheus metrics (`zetta_ingress_*`) are on the private probe port
8080, aggregate only. Delivery into the workspace Zetta is
[zetta#1281](https://github.com/oramasearch/zetta/issues/1281).

## Verify

Native tests (`go test ./internal/controller/ -run 'IngressGateway|IngressBody|BackendsAdmit'`):
`TestIngressGatewayServesEveryOperatorHostItCanAndRefusesTheRest` decodes `routes.json` strictly as
zetta-tunnel's projection and checks the served hosts, targets, body limits and certificate files;
the report's refusals (no certificate, Lake app, a second certificate for a host) and `on-nginx`;
the gateway Deployment, its copied pull secret and the IngressClass; the gateway's egress to only
the served backends; that only a served `zetta-tunnel` Ingress gets the address, and only after the
gateway is available; and that losing a certificate takes the address away.
`TestIngressGatewayIsScaledToZeroWithNothingToServe` covers an empty cluster.

Live acceptance per cluster (the cutover steps above): every public host answers through the gateway
with its certificate, and nothing reaches ingress-nginx.

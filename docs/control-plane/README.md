# Deployment control plane

The operator owns liquid's deployment contract. The authoritative OpenAPI document is
[`openapi.json`](openapi.json), served byte-for-byte at `GET /v1/openapi.json`. The same four
operations are available as MCP tools at `POST /mcp`:

- `create_deployment`
- `list_deployments`
- `rollback_deployment`
- `deployment_status`

The chart exposes the manager on the ClusterIP Service
`orama-operator-control-plane:8090` over HTTPS only. `--control-plane-bind-address` changes the
listener; an empty value disables it for local development. The chart does not create public routing.
Every cluster installs its own cert-manager-backed private CA, serving Certificate, and ingress
NetworkPolicy from the same chart. The manager waits for the serving Secret, requires TLS 1.2 or newer,
and reloads the projected certificate on each handshake so renewal needs no restart. Missing or
invalid TLS credentials fail closed. Plain HTTP clients must migrate to HTTPS.

In-cluster callers must have pod label `orama.cloud/deployment-client: "true"` **and** run in a
namespace labelled `orama.cloud/deployment-clients: "true"`. Declare these labels in the caller's
versioned resources; namespace label administration is part of granting deployment access. Unlabelled
pods, including pods in the manager namespace, cannot reach the deployment port. The policy preserves
metrics on 8080 and health probes on 8081; neither port may be selected as the deployment port. There
are no current in-cluster callers in the operator, Lake, or bridge configuration. Administrators can
still use Kubernetes-authenticated port-forwarding without caller labels.

## Authentication and scope

Every REST operation and every MCP request requires `Authorization: Bearer <token>`. The token is the
mint-once deployment credential stored in `Secret/lake-<lake>-deploy`, key `token`, in the Lake
namespace. The Lake reconciler generates 32 random bytes with a `lake_deploy_` prefix independently
of the instance-admin token. Matching is constant-time. The credential scopes all reads and writes
to that Lake; the repository and environment in a request cannot select a different Lake.

Lake never receives the deploy Secret: `LAKE_ADMIN_TOKEN` still references `lake-<lake>-admin`, so
the deploy credential grants no Lake instance-admin authority. The control plane rejects the Lake
admin bearer, including while the deploy Secret is missing or empty. There is no migration fallback.
Existing automation must switch credentials when this version rolls out. Grant automation read
access only to its Lake's deploy Secret and distribute the public CA certificate separately.

The Lake must have a public host (`spec.host`, or a literal `LAKE_PUBLIC_HOST` in `spec.env`): every
row is keyed by it, as lake reads them. A Lake without one answers `unavailable`, naming the fix.

The namespace must contain exactly one `OramaZettaCluster` with
`spec.provisionOperatorSchema` enabled. The operator uses that cluster's private seeded admin token for
SQL; it never forwards the caller's credential to Zetta. Missing, ambiguous, or unready control-plane
state returns an `unavailable` envelope.

For an administrative port-forward, retrieve the scoped credential without putting it in shell
history, and save the public CA certificate. These reads require Kubernetes authorization; do not
grant automation access to the CA private key or Lake admin Secret.

```sh
set +x
TOKEN="$(kubectl get secret lake-prod-deploy -n lake -o jsonpath='{.data.token}' | base64 -d)"
kubectl get secret orama-operator-control-plane-tls -n orama-system \
  -o jsonpath='{.data.ca\.crt}' | base64 -d > control-plane-ca.crt
# Run this in another terminal:
kubectl port-forward -n orama-system service/orama-operator-control-plane 8090:8090
# Keep the Service hostname for certificate verification through the local forward:
curl --cacert control-plane-ca.crt \
  --resolve orama-operator-control-plane.orama-system.svc.cluster.local:8090:127.0.0.1 \
  --config - <<EOF
header = "Authorization: Bearer $TOKEN"
url = "https://orama-operator-control-plane.orama-system.svc.cluster.local:8090/v1/list_deployments?repository=<repository-id>"
EOF
unset TOKEN
```

An admitted in-cluster client uses that HTTPS URL directly with the public CA bundle. Never disable
certificate verification. The private CA certificate lasts ten years and renews a year before expiry;
clients must refresh their trusted CA bundle during CA rollover. Serving certificates last 90 days
and renew 15 days before expiry. Preserve the CA Secret in disaster recovery to retain client trust.

## Storage and reconciliation

Zetta is the deployment intake and audit store. A create or
rollback appends two records:

1. a full immutable response/history row in `orama_control.deployment_records`; and
2. a desired row in `orama_control.deployments`, in lake's row shape (operator#560: `lake` is the
   Lake's public host, `phase` is `Pending`, `requested_by` is `lake-deploy-token/<lake>` because the
   caller holds the Lake's deploy token, and every column lake selects is present), consumed by the
   `LakeEnvironment` materializer and shown by lake's Ship → Deployments. See
   [the row contract](../lake/environments.md#the-row-contract-lakes-format-the-operator-conforms).

The history table carries `result_id`, `rollback_of`, and secret references, which lake does not read.
Times in both are lake's: UTC with six fractional digits. A create ID is the SHA-256 action identity of Lake,
repository, environment, change, and image. Repeating that action returns its existing record and does
not append another desired event. Lists reduce duplicate append retries by ID and return the repository's
records in reverse creation order.

One Lake/repository/environment identity maps to one stable `LakeEnvironment` and workload Namespace.
A later deployment updates that resource's image and change. Rollback looks up a record previously
deployed to the same repository and environment, creates a new history record with `rollback_of`, and
updates the same environment through a new desired event. Until the new Deployment generation is
observed, status remains `Progressing`; stale Ready status is never returned for a new deployment ID.

The contract does not carry runtime topology. Contract-created environments use one replica, container
port `8080`, no host, and `kind: named`. An environment whose name exactly equals its change digest is a
preview. Environment values and custom domains are lake's (`set_environment_env`, `create_domain`), as
rows the operator applies to the same environment.

## Sealed-secret references

`secrets` entries accept a `LakeSealedSecret` name or `secret://<name>`. Create and rollback verify that
every reference exists in the authenticated Lake namespace, belongs to that Lake, reports Ready for its
current generation, and owns its delivered Secret. The operation is refused with a path such as
`/secrets/0` otherwise. The `LakeEnvironment` controller copies the delivered Secrets into its isolated
Namespace and imports them through `envFrom`; see [Lake deployment environments](../lake/environments.md).

## Audit

Every authenticated operation appends an `orama_control.reconcile_events` row named
`deployment.<operation>`. Its JSON detail records repository, environment, deployment ID, outcome, and
failure reason without recording either bearer. The API returns `unavailable` if the audit append cannot
be confirmed. Rejected credentials are written to the manager's structured audit log because no Lake or
Zetta store is authorized yet.

## MCP

The endpoint implements JSON-RPC initialization, `tools/list`, and `tools/call`. Tool names and input
schemas match the REST operations. Successful calls return both text containing the ordinary
`{ok,value}` envelope and `structuredContent`; capability failures return an error envelope with
`isError: true`.

```json
{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}
```

## Contract drift

`TestDeploymentOpenAPIContractMatchesLiquidVendoredCopy` compares the served bytes with this file and
with liquid's `contracts/control-plane/openapi.json` when `LIQUID_SOURCE_DIR` or the sibling development
checkout is present. It always checks liquid's recorded SHA-256 digest so CI failure output includes both
digests. Current digest:

```text
sha256:96dee72f3085489ef586a8c3108fa4922d14e61a9f869c9e851580236d2e7b2e
```

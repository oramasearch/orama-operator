# Deployment control plane

The operator owns liquid's deployment contract. The authoritative OpenAPI document is
[`openapi.json`](openapi.json), served byte-for-byte at `GET /v1/openapi.json`. The same four
operations are available as MCP tools at `POST /mcp`:

- `create_deployment`
- `list_deployments`
- `rollback_deployment`
- `deployment_status`

The chart exposes the manager on the ClusterIP Service
`orama-operator-control-plane:8090`. `--control-plane-bind-address` changes the listener;
an empty value disables it. The chart does not create public routing for this Service.

## Authentication and scope

Every REST operation and every MCP request requires `Authorization: Bearer <token>`. The token is the
mint-once Lake environment credential stored in `Secret/lake-<lake>-admin`, key `token`, in the Lake
namespace. Matching is constant-time. The credential scopes all reads and writes to that Lake; the
repository and environment in a request cannot select a different Lake.

The namespace must contain exactly one `OramaZettaCluster` with
`spec.provisionOperatorSchema` enabled. The operator uses that cluster's private seeded admin token for
SQL; it never forwards the caller's credential to Zetta. Missing, ambiguous, or unready control-plane
state returns an `unavailable` envelope.

Retrieve the environment credential without printing it into a shell history:

```sh
TOKEN="$(kubectl get secret lake-prod-admin -n lake -o jsonpath='{.data.token}' | base64 -d)"
curl -s -H "Authorization: Bearer $TOKEN" \
  http://orama-operator-control-plane.orama-system.svc.cluster.local:8090/v1/list_deployments?repository=<repository-id>
```

## Storage and reconciliation

Zetta remains the deployment intake and audit store from liquid-bridge decisions 22 and 34. A create or
rollback appends two records:

1. a full immutable response/history row in `orama_control.deployment_records`; and
2. a desired event in `orama_control.deployments`, consumed by the existing `LakeEnvironment`
   materializer.

The separate history table adds `result_id`, `rollback_of`, and secret references on existing clusters
without requiring unsupported `ALTER TABLE`. A create ID is the SHA-256 action identity of Lake,
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
preview. Public domains and routing are configured separately.

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

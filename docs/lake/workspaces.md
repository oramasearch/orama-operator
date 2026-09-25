# Lake workspaces (superseded by managed services)

**Superseded by [managed services](services.md) (liquid-bridge decision 46).** A customer's
workspace — the Zetta cluster behind lake's Design, Train and Ship pages — is now that customer's
**default Zetta service**. This page records what decision 45 built and what happens to it.

## What decision 45 did

Zetta has no tenancy, so each lake customer workspace got its own `OramaZettaCluster`. lake
appended `desired` and `withdrawn` rows to `orama_control.workspaces` on its own Zetta data plane
(which ran with `provisionOperatorSchema: true` for that purpose); the operator's
`OramaZettaCluster` poll created `<lake>-<workspace_id>` per valid `workspace_id`
(`^ws-[0-9a-f]{12}$`), appended `status` rows on transition, deleted a withdrawn workspace's data
PVCs, cluster and credential ([#173](https://github.com/oramasearch/orama-operator/pull/173)),
capped the count with `spec.workspaces.maxWorkspaces`, and handed lake the admin tokens in Secret
`lake-<lake>-workspaces` mounted at `/var/run/lake/workspaces`, with `LAKE_WORKSPACE_DIR` and
`LAKE_WORKSPACE_URL_TEMPLATE`.

## What changed

| Decision 45 | Now |
|---|---|
| requests in `orama_control.workspaces` on the Zetta data plane | the `services` table in lake's control Postgres, read with pgx |
| status rows appended to the same Zetta table | `service_status` rows upserted on change |
| the data plane provisions the operator schema | the data plane is a plain data plane again (`provisionOperatorSchema: false`); the `workspaces` table is no longer in the operator schema |
| Secret `lake-<lake>-workspaces`, key `<workspace_id>` = token | Secret `lake-<lake>-services`, key `<slug>.json` = connection JSON; the old Secret is deleted |
| `/var/run/lake/workspaces`, `LAKE_WORKSPACE_DIR`, `LAKE_WORKSPACE_URL_TEMPLATE` | `/var/run/lake/services`, `LAKE_SERVICE_DIR`; endpoints come from `service_status` |
| `withdrawn` row | `desired = 'absent'`; the same PVC-then-cluster order and ownership rule |
| `workspace quota reached (<n>)` | `service quota reached (<n>)` |

## Existing workspace clusters

An existing `<lake>-ws-<12 hex>` cluster keeps its data: the customer's default Zetta service with
that slug **adopts** it (see [adoption](services.md#adoption-of-workspace-clusters)). Until lake
writes that service row, the cluster is left alone and counts against the quota.

## `spec.workspaces`

The field remains as a deprecated alias; see [turning services on](services.md#turning-it-on):
`enabled` turns services on, `image` and `maxWorkspaces` are fallbacks for `spec.services`, and
`resources` and `storage.storageClassName` still shape Zetta services' nodes.

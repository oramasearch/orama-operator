# Lake data services

Every data service associated with a `Lake` is listed in `status.dataServices`. An operation is
`tested` only after its `LakeRestoreRehearsal` succeeds. `untested` means the procedure exists but
has no successful result, while `none` means the current configuration cannot provide it.
`managed` becomes true only when every operation required by that configuration is tested.

| Service | Initial label | Customer responsibility | What managed requires |
|---|---|---|---|
| Control-plane Postgres | Unmanaged until rehearsed | Enable object-store backups and run the restore and upgrade rehearsals. For two or more instances, also run failover. | Successful backup/restore and upgrade rehearsals; successful failover for a replicated cluster. A single instance reports `failover: none` explicitly. |
| Zetta data plane | Unmanaged until rehearsed | The Lake owns provisioning and ordered teardown; backup, restore, upgrade, and failover procedures still need rehearsal support. | Tested backup, restore, upgrade, and failover procedures for the owned cluster. |

Postgres rehearsals are cluster-scoped `LakeRestoreRehearsal` objects. Restore and upgrade require
enabled backups and create an isolated, deterministically named namespace. The controller copies
only the CNPG application credentials and object-store credentials required by the rehearsal,
recovers the selected backup, compares every guard-table row count, the migrations missing from
`schema_migrations` and the newest `schema_migrations` version with the live control DB, fails when
either database lacks a migration the vendored lake schema records, records the result, and deletes the scratch
Cluster, Secrets, Job, and Namespace. Upgrade changes `spec.imageName` on that recovered Cluster
before verification. Failover requires two or more instances, requests a CNPG switchover to a
healthy replica, then connects through the existing `lake-<name>-controldb-app` Secret and `-rw`
Service and checks that every vendored migration is recorded before recording success.

Examples and the operational procedure are in [Postgres data-service rehearsals](postgres.md).

The `Data` column from `kubectl get lake` reports the managed and unmanaged counts. Inspect the
individual evidence and notes with:

```sh
kubectl get lake <name> -o jsonpath='{.status.dataServices}' | jq
```

Future data services must be added to this table and to `status.dataServices`; silence does not
count as managed.

# Lake data services

Every data service associated with a `Lake` is listed in `status.dataServices`. An operation is
`tested` only after its `LakeRestoreRehearsal` succeeds. `untested` means the procedure exists but
has no successful result, while `none` means the current configuration cannot provide it.
`managed` becomes true only when every operation required by that configuration is tested.

| Service | Initial label | Customer responsibility | What managed requires |
|---|---|---|---|
| Control-plane Postgres | Unmanaged until rehearsed | Enable object-store backups and run the restore and upgrade rehearsals. For two or more instances, also run failover. | Successful backup/restore and upgrade rehearsals; successful failover for a replicated cluster. A single instance reports `failover: none` explicitly. |
| Zetta data plane | Unmanaged | Provision, protect, recover, upgrade, and fail over Zetta outside this `Lake` until #125 and its lifecycle procedures land. | Operator-owned provisioning plus tested backup, restore, upgrade, and failover procedures. |

The `Data` column from `kubectl get lake` reports the managed and unmanaged counts. Inspect the
individual evidence and notes with:

```sh
kubectl get lake <name> -o jsonpath='{.status.dataServices}' | jq
```

Future data services must be added to this table and to `status.dataServices`; silence does not
count as managed.

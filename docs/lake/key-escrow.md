# Escrow of the Lake instance key

> **Human action: open.** Steve decides who holds the recovery keys and where they are kept
> (never 1Password, per the org rule), each holder makes a key pair off the cluster, and the public
> keys go into `spec.backups.escrowRecipients` of each Lake's set file, applied with Steve's consent.
> Until then no Lake's instance key has a copy off the cluster: every Lake reports
> `InstanceKeyEscrowed=False`, `NotConfigured`. Tracked in
> [operator#739](https://github.com/oramasearch/orama-operator/issues/739).

## Why

`lake-<name>-instance-kek` (on the dev cluster `lake-prod-instance-kek`) is the root wrapping key of
lake's persisted identity and per-customer keys. Losing it loses every sealed workspace secret, and
it is not in any backup: it is a Kubernetes Secret, not a datastore.

## What the operator does

With `spec.backups` and at least one recipient, the Lake reconciler
(`internal/controller/lake_key_escrow.go`):

1. reads the Secret's data and computes its **fingerprint** (SHA-256 of the data, which says nothing
   about the key itself);
2. seals a JSON document with the Lake's name, namespace, Secret name, data and fingerprint, in the
   **age** format (`age-encryption.org/v1`, X25519 recipients; `internal/escrow`), to every
   recipient: any one recipient's private key opens it;
3. writes it to the Lake's backup bucket (`lake-<name>-backups`, `deletionPolicy: Retain`) as
   `lake-<name>/instance-kek/<first 16 hex of the fingerprint>.age`;
4. records `status.keyEscrow` (`object`, `fingerprint`, `recipientsDigest`, `escrowedAt`) and the
   condition `InstanceKeyEscrowed=True`, `Escrowed`.

It writes again only when the key or the recipients change. The operator never holds a recovery key
and cannot open what it wrote.

**Decided under Steve's 2026-11-14 delegation:** the sealed copy may live in R2, in the backup
bucket, because it is encrypted to keys that never touch the cluster or R2. A reader of the bucket
learns only that the key exists and its fingerprint.

Condition reasons: `NotConfigured`, `KeyNotFound`, `BucketNotReady`, `SealFailed`, `WriteFailed`,
`Escrowed`.

## Steps for the holders (human)

1. **Make a key pair, off the cluster**, on a machine you trust, with the standard
   [age](https://age-encryption.org) tool: `age-keygen -o recovery.key`. It prints the public key,
   `age1...`. Keep `recovery.key` where Steve decided (offline media in a safe, a hardware-backed
   store); never in a repository, a cluster, 1Password or a chat.
2. **Send only the public key** (`age1...`) to whoever edits the set file. Two or more holders give
   two or more recipients, so the loss of one key is survivable.
3. **Add the public keys** to each Lake in its set file (now in `orama-infra`):
   ```yaml
   spec:
     backups:
       cloudflareSecretRef: {name: cloudflare-r2}
       escrowRecipients:
         - age1...   # holder one
         - age1...   # holder two
   ```
   Applying it is a cluster change: Steve's consent.

## Check (read-only)

```sh
kubectl get lake <name> -o jsonpath='{.status.keyEscrow}{"\n"}'
kubectl get lake <name> -o jsonpath='{.status.conditions[?(@.type=="InstanceKeyEscrowed")]}{"\n"}'
```

## Rehearse a recovery (human, at least once a quarter)

1. Download the object named in `status.keyEscrow.object` with an S3 client and the bucket's
   read credentials.
2. Open it: `age -d -i recovery.key kek.age > kek.json`. Keep `kek.json` on the trusted machine
   only, and delete it after the check.
3. Compare: the `fingerprint` in `kek.json` equals `status.keyEscrow.fingerprint`, and the
   SHA-256 the operator computes (`sha256` over each key name, a NUL byte, the value's hex, a NUL
   byte, in key order) gives the same value.
4. Record the date and the result on [operator#739](https://github.com/oramasearch/orama-operator/issues/739)
   or its successor.

## Recover the key after a loss (human, with Steve's consent)

Recreate the Secret from `kek.json`'s `data` in the Lake's namespace **before** the Lake reconciler
runs again (it mints a new key only when the Secret is absent or empty), then restart nothing: lake
reads it at start. Because this writes a Secret on a cluster by hand, it is a one-off that needs
Steve's explicit consent at that time.

# One-way sealed secret delivery

`LakeSealedSecret` carries ciphertext or a lake reference and produces a namespaced Kubernetes
`Secret`. Plaintext exists only in the authoring process before sealing, inside lake while it reads
or mints a value, and in the target cluster's produced Secret. The authored custom resource never
contains a live value.

The format is lake-native. It does not implement Bitnami Sealed Secrets or `kubeseal`.

## Cluster recipient

Each cluster-scoped `Lake` owns `Secret/lake-<lake>-sealing-key` in `spec.namespace`. The Secret
contains raw X25519 private and public key bytes indexed by `kid`; it is never mounted into a
workload. The operator publishes only this recipient information:

```yaml
status:
  sealing:
    cluster: 8cc182a1-5a84-4c65-92c7-742481ec88d4
    generation: 1
    kid: 8_y8fQ8vVY8KxkXIXgP0Pg
    publicKey: "<unpadded-base64url raw X25519 public key>"
```

`cluster` is the Kubernetes UID of the `Lake` object. It is stable for that Lake's lifetime and
differs when an equivalent manifest is created in another cluster. The lake Deployment receives
`LAKE_SEALING_PUBLIC_KEY` and `LAKE_SEALING_KID`; it never receives the private key.

## Envelope version 1

The value under each `spec.data` key is standard padded base64 of one deterministic canonical-CBOR
map. The decoded map is:

```text
{
  "v":     1,                         # unsigned integer
  "kid":   text,                      # recipient key identifier
  "epk":   bytes[32],                 # ephemeral X25519 public key
  "nonce": bytes[24],                 # XChaCha20-Poly1305 nonce
  "aad": {
    "cluster":   text,
    "namespace": text,
    "name":      text
  },
  "ct":    bytes                      # ciphertext followed by the 16-byte tag
}
```

Encoding follows RFC 8949 deterministic rules: definite lengths, preferred integer encoding, and
map keys ordered by their encoded bytes. A decoder rejects duplicate keys, tags, indefinite
lengths, unknown fields, and any representation whose canonical re-encoding differs.

The cryptographic steps are:

1. Generate an ephemeral X25519 keypair and calculate `shared = X25519(ephemeral_private,
   recipient_public)`.
2. Calculate the `kid` as unpadded base64url of the first 16 bytes of
   `SHA-256(recipient_public)`.
3. Apply HKDF-SHA-256. Extract uses a 32-byte all-zero salt and `shared` as input keying material.
   Expand produces 32 bytes with `info = "orama lake sealed secret v1" || epk ||
   recipient_public`.
4. Canonically encode the nested `aad` map by itself. Encrypt with XChaCha20-Poly1305 using the
   derived key, the 24-byte nonce, and those exact encoded bytes as associated data.
5. Canonically encode the complete record and standard-base64 it for the CR field.

The operator compares all three AAD fields before attempting authenticated decryption. `cluster`
prevents cross-cluster replay; `namespace` and `name` prevent moving a record to another namespace
or produced Secret. `name` is `spec.template.name`, not the `LakeSealedSecret` resource name.

The independent reference fixture is generated in
`internal/controller/lakesealedsecret_controller_test.go` from fixed recipient, ephemeral, nonce,
AAD and plaintext inputs. Its standard-base64 envelope is:

```text
pmF2AWJjdFgeEHhaBXET+DLyNpH7FrrQcmyA2bKdKakJ+AEZcFR7Y2FhZKNkbmFtZWhkYXRhYmFzZWdjbHVzdGVyb2NsdXN0ZXItZml4dHVyZWluYW1lc3BhY2VocGF5bWVudHNjZXBrWCBYaa/0UFSXMsuq7V5d+bMKbaMcsOV0K61a1KGnaPGme2NraWR2cXFqXzl3TzFDeUtYOVBiaE5RajNKQWVub25jZVgYQUJDREVGR0hJSktMTU5PUFFSU1RVVldY
```

The liquid `secret seal` leaf and lake `seal_secret`/`mint_sealed_secret` leaf must import this
fixture unchanged as an interoperability test.

## Sources

### Literal

`source.type: literal` requires `spec.data`. Each map value is an envelope. All entries must use
one `kid`, and all bind to the same cluster, namespace, and produced Secret name.

```yaml
spec:
  lakeRef: {name: prod}
  template: {name: application-database, type: Opaque}
  source: {type: literal}
  data:
    username: "<base64 canonical-CBOR envelope>"
    password: "<base64 canonical-CBOR envelope>"
```

An author can read the public recipient and `kid`, the target metadata, ciphertext sizes, and the
source reference. The author cannot derive the cluster private key or read back plaintext from the
custom resource. Kubernetes principals that can read the produced Secret can read the value, so
Secret RBAC remains security-critical.

### Lake static secret

`source.type: lake` carries only `{mount, name, version?}`. The operator sends the source, target
AAD, current recipient, and its projected Kubernetes service-account token to
`POST /v1/mint_sealed_secret` on the referenced Lake. Lake exchanges the attestation, reads the
authorized version, and returns only a sealed record to the operator.

### Dynamic secret

`source.type: dynamic` carries `{engine, role, ttl}`. The same capability mints a scoped credential
and returns sealed data plus `{id, expires_at, renew_at?}`. The operator renews at `renew_at`; when lake
omits it, the operator renews one third of the requested TTL before expiry. It updates the existing
Secret in place and records only lease metadata in status.

The lake capability implementations are separate lake work. Until lake supplies
`mint_sealed_secret`, only literal sources complete against a real Lake deployment; the operator's
HTTP and renewal behavior is covered by an `httptest` lake double.

The lake leaf's action input is the following JSON. `source` is the CRD source object unchanged.
The capability must exchange and authorize the attestation before reading or minting a value.

```json
{
  "attestation": {
    "type": "kubernetes",
    "token": "<projected service-account token>",
    "audiences": ["lake"]
  },
  "recipient": {
    "cluster": "<Lake UID>",
    "namespace": "application",
    "name": "application-database",
    "kid": "<active kid>",
    "public_key": "<unpadded-base64url X25519 public key>"
  },
  "source": {
    "type": "lake",
    "lake": {"mount": "production", "name": "database", "version": 3}
  }
}
```

It uses lake's normal `{ok, value | error, duration_ms}` response envelope. `value.data` is a map
of Secret keys to version-1 records. A dynamic response also has `value.lease` with `id`,
`expires_at`, and optional `renew_at`. Plaintext and unsealed dynamic credentials must not appear in
the response.

## Reconciliation and ownership

The referenced cluster-scoped `Lake` becomes the controller owner of the `LakeSealedSecret`. The
`LakeSealedSecret` becomes the controller owner of exactly one produced Secret in its namespace.
Changing `template.name` removes the formerly owned Secret. Every produced Secret has:

```yaml
metadata:
  labels:
    lake.orama.cloud/sealed: "true"
```

The remote-source Secret also carries a private-keyed content MAC annotation. That lets the
operator detect and repair manual Secret drift without publishing a plaintext confirmation hash.
Status is written only on transition and reports `phase`, `unsealedAt`, `kid`, an optional dynamic
`lease`, and a standard `Ready` condition.

The install ClusterRole grants create, update and delete for Secrets across namespaces. Kubernetes
RBAC cannot scope Secret names in a ClusterRole, so this grant is namespace-wide wherever the
operator runs. Controller ownership checks prevent adoption or overwrite of an unrelated Secret.

## Rotation

Generation 1 is minted once when `spec.sealing.generation` is omitted. Increase the integer to
rotate:

```sh
kubectl patch lake prod --type merge -p '{"spec":{"sealing":{"generation":2}}}'
kubectl get lake prod -o jsonpath='{.status.sealing}'
```

The operator creates a new X25519 keypair and immediately publishes it. It retains each old private
key while any literal envelope or remote-source status names that `kid`.
`status.sealing.previousKeys[]` reports the old `kid` and exact remaining record count. A malformed
record whose `kid` cannot be read increments `unresolvedRecords` and conservatively retains every
old key. Re-seal all
literal values with the active recipient; lake and dynamic sources re-mint after observing the new
Lake status. The next Lake reconcile deletes an old private key only after its remaining count is
zero. Decreasing the generation is refused.

Deleting the sealing-key Secret is not rotation: recovery of ciphertext under a deleted key is
impossible. Back up this Secret with the same care as other cluster recovery material.

## Verify

```sh
kubectl get lake prod -o jsonpath='{.status.sealing}'
kubectl get lakesealedsecret -A
kubectl get secret application-database -n application \
  -o jsonpath='{.metadata.labels.lake\.orama\.cloud/sealed}'
```

Repository verification covers the reference fixture, each AAD mismatch, unknown `kid`, key
retention and cleanup, Lake-source minting, dynamic renewal, labels, and ownership:

```sh
go test ./internal/controller -run 'Test(SealedSecret|LakeSealedSecret|LakeSealing|LakeSource|DynamicSource)'
```

# AKS operator rename cutover

This is the one-time production handoff from the former Helm release to `orama-operator` in the
`orama-system` namespace. Run it only during a scheduled AKS change. It does not replace or recreate
the `orama.cloud` CRDs, and it does not mutate the `Lake`, `OramaCloudV1`, or
`OramaZettaCluster` resources.

The chart now renders the Deployment, control-plane Service, ServiceAccount, and RBAC names from
the `orama-operator` chart name. Helm cannot rename an existing release in place, so the handoff
briefly installs both releases. Both manager binaries use the preserved leader-election ID
`orama-cloud-v1-operator.orama.cloud`; only one manager can reconcile while both Deployments exist.
The Lease is created by controller-runtime, not by Helm, so retiring the former release does not
delete it.

The `app.kubernetes.io/managed-by=orama-cloud-v1-operator` labels on existing reconciled workloads
and the `orama-cloud-v1-operator-karpenter` server-side-apply field owner are compatibility
identifiers, not Helm release names. They remain unchanged so status selection, disabled-component
teardown, and Karpenter field ownership continue across the handoff.

## Inputs and hard gates

Run from a clean checkout of the reviewed operator revision with `helm`, `kubectl`, and the
production values available. Replace `<immutable-tag>` with the published image for that revision.

```sh
export CUTOVER_CONTEXT=orama-production-cluster-01
export CUTOVER_NAMESPACE=orama-system
export FORMER_RELEASE=orama-cloud-v1-operator
export CURRENT_RELEASE=orama-operator
export OPERATOR_IMAGE_TAG=<immutable-tag>
export CUTOVER_RECORDS="$(mktemp -d)"
```

Confirm the context explicitly. Stop if it is not the intended AKS cluster.

```sh
kubectl config current-context
kubectl --context "$CUTOVER_CONTEXT" cluster-info
helm status "$FORMER_RELEASE" -n "$CUTOVER_NAMESPACE" --kube-context "$CUTOVER_CONTEXT"
```

Capture the former revision, its user-supplied values, the CRD identities, and the Lease identity.
The values file must not contain credentials; if it does, keep the temporary directory protected
and remove it after the change record has been stored securely.

```sh
helm list -n "$CUTOVER_NAMESPACE" --kube-context "$CUTOVER_CONTEXT" --filter "^${FORMER_RELEASE}$" -o json > "$CUTOVER_RECORDS/former-release.json"
helm get values "$FORMER_RELEASE" -n "$CUTOVER_NAMESPACE" --kube-context "$CUTOVER_CONTEXT" -o yaml > "$CUTOVER_RECORDS/operator-values.yaml"
kubectl --context "$CUTOVER_CONTEXT" get crd -o custom-columns=NAME:.metadata.name,UID:.metadata.uid --no-headers | sort > "$CUTOVER_RECORDS/crds-before.txt"
kubectl --context "$CUTOVER_CONTEXT" get lease orama-cloud-v1-operator.orama.cloud -n "$CUTOVER_NAMESPACE" -o jsonpath='{.metadata.uid}{"\n"}' > "$CUTOVER_RECORDS/lease-before.txt"
kubectl --context "$CUTOVER_CONTEXT" get lakes,oramacloudv1s,oramazettaclusters -A
```

Stop if the former release is not deployed, the Lease is absent, any managed CR is already
degraded, or the saved values are not understood. Do not delete or reapply CRDs as part of this
rename.

## Install the renamed release

Review `operator-values.yaml`. It contains only explicit overrides from the former release. The
command below forces the renamed image repository, immutable tag, and ServiceAccount even if an
old name was explicitly saved.

```sh
helm upgrade --install "$CURRENT_RELEASE" ./chart \
  -n "$CUTOVER_NAMESPACE" \
  --kube-context "$CUTOVER_CONTEXT" \
  --create-namespace \
  --skip-crds \
  -f "$CUTOVER_RECORDS/operator-values.yaml" \
  --set operator.image.repository=oramasearch/orama-operator \
  --set operator.image.tag="$OPERATOR_IMAGE_TAG" \
  --set serviceAccount.name=orama-operator
kubectl --context "$CUTOVER_CONTEXT" rollout status deployment/orama-operator -n "$CUTOVER_NAMESPACE"
kubectl --context "$CUTOVER_CONTEXT" get lease orama-cloud-v1-operator.orama.cloud -n "$CUTOVER_NAMESPACE" -o wide
kubectl --context "$CUTOVER_CONTEXT" get endpointslice -n "$CUTOVER_NAMESPACE" -l kubernetes.io/service-name=orama-operator-control-plane
kubectl --context "$CUTOVER_CONTEXT" get lakes,oramacloudv1s,oramazettaclusters -A
```

At this point the former manager normally still holds the shared Lease and the renamed manager is
waiting. Confirm that there is exactly one Lease holder and that no CR status regressed.

Move every in-cluster control-plane caller from
`orama-cloud-v1-operator-control-plane.orama-system.svc.cluster.local:8090` to
`orama-operator-control-plane.orama-system.svc.cluster.local:8090`. Verify an authenticated
`list_deployments` request through the new Service before retiring the former Service.

## Hand off leadership

Keep the former release history so it remains available for rollback, then remove its rendered
objects. Its manager relinquishes the shared Lease and the renamed manager acquires it.

```sh
helm uninstall "$FORMER_RELEASE" -n "$CUTOVER_NAMESPACE" --kube-context "$CUTOVER_CONTEXT" --keep-history
kubectl --context "$CUTOVER_CONTEXT" rollout status deployment/orama-operator -n "$CUTOVER_NAMESPACE"
kubectl --context "$CUTOVER_CONTEXT" get lease orama-cloud-v1-operator.orama.cloud -n "$CUTOVER_NAMESPACE" -o wide
kubectl --context "$CUTOVER_CONTEXT" get lakes,oramacloudv1s,oramazettaclusters -A
```

Verify that the CRD and Lease UIDs are unchanged and that only the renamed operator objects remain.

```sh
kubectl --context "$CUTOVER_CONTEXT" get crd -o custom-columns=NAME:.metadata.name,UID:.metadata.uid --no-headers | sort > "$CUTOVER_RECORDS/crds-after.txt"
kubectl --context "$CUTOVER_CONTEXT" get lease orama-cloud-v1-operator.orama.cloud -n "$CUTOVER_NAMESPACE" -o jsonpath='{.metadata.uid}{"\n"}' > "$CUTOVER_RECORDS/lease-after.txt"
diff -u "$CUTOVER_RECORDS/crds-before.txt" "$CUTOVER_RECORDS/crds-after.txt"
diff -u "$CUTOVER_RECORDS/lease-before.txt" "$CUTOVER_RECORDS/lease-after.txt"
kubectl --context "$CUTOVER_CONTEXT" get deployment,service,serviceaccount -n "$CUTOVER_NAMESPACE" -l app.kubernetes.io/name=orama-operator
kubectl --context "$CUTOVER_CONTEXT" get deployment orama-cloud-v1-operator -n "$CUTOVER_NAMESPACE"
helm list -n "$CUTOVER_NAMESPACE" --kube-context "$CUTOVER_CONTEXT" --all
```

The old Deployment lookup must return `NotFound`; both `diff` commands must be empty. Record the
renamed release revision, image digest, Lease holder, CR statuses, and authenticated control-plane
check in the production change record.

## Rollback

Before the former release is retired, rollback is `helm uninstall orama-operator -n
orama-system --kube-context orama-production-cluster-01`; the former manager remains leader.

After the handoff, read the former revision from `former-release.json`, restore it from Helm's kept
history, move control-plane callers back to the former Service, and then retire the renamed release.
The shared Lease continues to prevent concurrent reconciliation.

```sh
export FORMER_REVISION=<revision-from-former-release.json>
helm rollback "$FORMER_RELEASE" "$FORMER_REVISION" -n "$CUTOVER_NAMESPACE" --kube-context "$CUTOVER_CONTEXT" --wait
kubectl --context "$CUTOVER_CONTEXT" rollout status deployment/orama-cloud-v1-operator -n "$CUTOVER_NAMESPACE"
kubectl --context "$CUTOVER_CONTEXT" get lease orama-cloud-v1-operator.orama.cloud -n "$CUTOVER_NAMESPACE" -o wide
helm uninstall "$CURRENT_RELEASE" -n "$CUTOVER_NAMESPACE" --kube-context "$CUTOVER_CONTEXT"
kubectl --context "$CUTOVER_CONTEXT" get lakes,oramacloudv1s,oramazettaclusters -A
```

Stop and investigate if Helm cannot restore the former revision, leadership is absent, more than
one Lease is active, or any managed CR becomes degraded. Do not delete CRs, CRDs, persistent
volumes, or the preserved Lease to force the rollback.

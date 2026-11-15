#!/usr/bin/env bash
# SPDX-License-Identifier: MIT

# Re-vendor the pinned upstream Karpenter manifests the operator server-side-applies when
# spec.capacity.karpenter.install is set (operator#75 — operator-owned Karpenter install).
#
# The operator OWNS the Karpenter install but does NOT vendor Karpenter's Go types; instead it embeds
# the upstream Helm-rendered manifests (via go:embed) and applies each object as `unstructured`. This
# script reproducibly regenerates those embedded assets from the pinned upstream charts. Re-run it to
# bump the pinned version, then rebuild + commit the regenerated assets.
#
#   ./hack/fetch-karpenter-manifests.sh                 # uses KARPENTER_VERSION below
#   KARPENTER_VERSION=1.14.0 ./hack/fetch-karpenter-manifests.sh
#
# Requires: helm (v3.8+, OCI support).
set -euo pipefail

KARPENTER_VERSION="${KARPENTER_VERSION:-1.13.0}"
NAMESPACE="${KARPENTER_NAMESPACE:-kube-system}"
OUT_DIR="$(cd "$(dirname "$0")/.." && pwd)/internal/controller/assets/karpenter/v${KARPENTER_VERSION}"

# Placeholders for the two per-CR values. The operator never replaces them as text: it parses the bundle,
# then sets the CR's values on the ServiceAccount annotation and the controller's CLUSTER_NAME env var,
# each of which must hold its placeholder (zetta_capacity_install_render.go, operator#79). Keep these
# EXACTLY in sync with that file. If a new version adds, removes or renames an object, update
# karpenterControllerObjects / karpenterCRDNames there AND the resourceNames in chart/templates/rbac.yaml.
CLUSTER_NAME_PLACEHOLDER="__CLUSTER_NAME__"
ROLE_ARN_PLACEHOLDER="__CONTROLLER_ROLE_ARN__"

echo ">> vendoring Karpenter ${KARPENTER_VERSION} into ${OUT_DIR}"
mkdir -p "${OUT_DIR}"

# helm v4 prints the OCI pull result (`Pulled:` / `Digest:` lines) to STDOUT before the rendered
# manifests; strip that preamble so the embedded bundle is pure YAML documents.
strip_helm_noise() { sed -e '/^Pulled: /d' -e '/^Digest: /d'; }

# 1. CRDs (static; NodePool / NodeClaim / EC2NodeClass / NodeOverlay) from the standalone CRD chart.
{
  echo "# Karpenter CRDs — vendored verbatim from the upstream karpenter-crd Helm chart, pinned."
  echo "# Version: ${KARPENTER_VERSION}   Chart: oci://public.ecr.aws/karpenter/karpenter-crd"
  echo "# Regenerate: hack/fetch-karpenter-manifests.sh   (do NOT hand-edit)"
  echo "# CRDs are server-side-applied (create/update) but NEVER garbage-collected on teardown —"
  echo "# deleting a Karpenter CRD cascades to every NodePool/NodeClaim in the cluster."
  helm template karpenter-crd oci://public.ecr.aws/karpenter/karpenter-crd \
    --version "${KARPENTER_VERSION}" | strip_helm_noise
} >"${OUT_DIR}/crds.yaml"

# 2. Controller install (ServiceAccount + RBAC + Deployment + Service + PDB), rendered with the two
#    cluster-specific values left as placeholders the operator fills in per-CR.
{
  echo "# Karpenter controller install — vendored from the upstream karpenter Helm chart, pinned."
  echo "# Version: ${KARPENTER_VERSION}   Chart: oci://public.ecr.aws/karpenter/karpenter   Namespace: ${NAMESPACE}"
  echo "# Regenerate: hack/fetch-karpenter-manifests.sh   (do NOT hand-edit)"
  echo "# Two fields hold placeholders the operator sets on the parsed objects from spec.capacity.karpenter:"
  echo "#   ${CLUSTER_NAME_PLACEHOLDER}        <- installClusterName  (AWS settings.clusterName)"
  echo "#   ${ROLE_ARN_PLACEHOLDER} <- controllerRoleARN   (IRSA role on the controller ServiceAccount)"
  helm template karpenter oci://public.ecr.aws/karpenter/karpenter \
    --version "${KARPENTER_VERSION}" \
    --namespace "${NAMESPACE}" \
    --set "settings.clusterName=${CLUSTER_NAME_PLACEHOLDER}" \
    --set "serviceAccount.annotations.eks\.amazonaws\.com/role-arn=${ROLE_ARN_PLACEHOLDER}" \
    --set "controller.resources.requests.cpu=1" \
    --set "controller.resources.requests.memory=1Gi" \
    --set "controller.resources.limits.cpu=1" \
    --set "controller.resources.limits.memory=1Gi" | strip_helm_noise
} >"${OUT_DIR}/controller.yaml"

echo ">> done. Update karpenterInstallVersion in internal/controller/zetta_capacity_install.go if bumped,"
echo ">> then: go build ./... && go vet ./..."

#!/usr/bin/env bash
# Re-vendor the pinned upstream Pixie manifests the operator renders for PixieVizier and PixieCloud
# (operator#15, #456). The operator installs Pixie itself: no Pixie operator, no OLM, no Vizier
# updater. It embeds the kustomize output of a pinned commit of our private copy oramasearch/pixie,
# parses it, refuses any bundle that is not exactly the expected object set, and sets every per-cluster
# value on the decoded objects (pixie_render.go). This script regenerates those embedded assets.
#
#   PIXIE_SOURCE=/path/to/oramasearch/pixie ./hack/fetch-pixie-manifests.sh
#
# PIXIE_SOURCE defaults to the umbrella bridge's vendored copy (modules/pixie). The checkout must be
# exactly PIXIE_COMMIT, with no local change, or nothing is written: the commit is what the
# ObservabilityProvider's images must be built from (#455), and the two must never drift.
#
# Requires: kubectl (built-in kustomize), git.
set -euo pipefail

PIXIE_COMMIT="${PIXIE_COMMIT:-4b445e0b7907c79d034bcb4a227bbaede94d370a}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PIXIE_SOURCE="${PIXIE_SOURCE:-${ROOT}/../pixie}"
OUT_DIR="${ROOT}/internal/controller/assets/pixie"

head="$(git -C "${PIXIE_SOURCE}" rev-parse HEAD)"
if [[ "${head}" != "${PIXIE_COMMIT}" ]]; then
  echo "error: ${PIXIE_SOURCE} is at ${head}, not the pinned ${PIXIE_COMMIT}" >&2
  exit 1
fi
if [[ -n "$(git -C "${PIXIE_SOURCE}" status --porcelain -- k8s)" ]]; then
  echo "error: ${PIXIE_SOURCE}/k8s has local changes" >&2
  exit 1
fi

render() {
  local out="$1" source="$2" what="$3"
  {
    echo "# ${what} — kustomize output of oramasearch/pixie ${source}, vendored verbatim."
    echo "# Commit: ${PIXIE_COMMIT}"
    echo "# Copyright 2018- The Pixie Authors. Licensed under the Apache License, Version 2.0 (LICENSE)."
    echo "# Regenerate: hack/fetch-pixie-manifests.sh   (do NOT hand-edit; pixie_render.go transforms it)"
    kubectl kustomize "${PIXIE_SOURCE}/${source}" 2>/dev/null
  } >"${OUT_DIR}/${out}"
}

mkdir -p "${OUT_DIR}"
# Vizier with persistent metadata (the PVC-backed metadata StatefulSet, not etcd) on x86-64: every
# cluster is amd64 (#455). Its NATS comes from vizier_deps, as in //k8s/vizier:vizier_yamls.
render vizier.yaml k8s/vizier/persistent_metadata/x86 "Pixie Vizier (persistent metadata, x86-64)"
render vizier-nats.yaml k8s/vizier_deps/base/nats "Pixie Vizier NATS"
# Self-hosted Pixie Cloud: the services and Ory. Its dependencies are not vendored whole: the
# Elasticsearch (ECK) and Kibana in k8s/cloud_deps are replaced by OpenSearch, and its Postgres by
# CloudNativePG; only the NATS (with JetStream) is kept.
render cloud.yaml k8s/cloud/public "Self-hosted Pixie Cloud services"
render cloud-nats.yaml k8s/cloud_deps/public/nats "Self-hosted Pixie Cloud NATS (JetStream)"
# The metadata index exactly as the indexer defines it (src/cloud/indexer/md/mapping.o.go): the
# operator creates the index with this body, so the indexer's own migration finds nothing to change
# and never reindexes (it runs with manual index management; see pixie_cloud_bootstrap.go).
awk '/^const IndexMapping = `/{f=1;next} f&&/^`/{exit} f' \
  "${PIXIE_SOURCE}/src/cloud/indexer/md/mapping.o.go" >"${OUT_DIR}/md-index.json"
# The profile service's migrations, which the organisation and administrator bootstrap writes
# against (TestPixieBootstrapMatchesVendoredSchema).
rm -rf "${OUT_DIR}/profile-schema" && mkdir -p "${OUT_DIR}/profile-schema"
cp "${PIXIE_SOURCE}"/src/cloud/profile/schema/*.up.sql "${OUT_DIR}/profile-schema/"
cp "${PIXIE_SOURCE}/LICENSE" "${OUT_DIR}/LICENSE"
echo "${PIXIE_COMMIT}" >"${OUT_DIR}/COMMIT"
echo ">> vendored Pixie ${PIXIE_COMMIT} into ${OUT_DIR}"

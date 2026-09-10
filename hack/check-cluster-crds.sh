#!/usr/bin/env bash
# Fail if the cluster is missing a CRD this chart ships.
#
# Helm applies chart/crds on INSTALL ONLY — never on upgrade — and the rollout runbooks pass
# --skip-crds. So a CRD added after the first install lands on no cluster until someone applies it
# by hand, and nothing used to check that they had. The manager registers a watch for every kind
# unconditionally, so one absent CRD is not a degraded controller: the cache never syncs, the
# manager exits, and the pod crash-loops with every other controller down with it.
#
# That is exactly what happened on 2026-09-09 (operator#166): lakee2etests and clerkapplications had
# both accumulated un-applied, and the operator was down for ~12 hours. Run this BEFORE and AFTER
# every helm upgrade.
#
#   ./hack/check-cluster-crds.sh                              # current kube-context
#   ./hack/check-cluster-crds.sh orama-production-cluster-01   # explicit context
#   ./hack/check-cluster-crds.sh <context> --apply             # apply what is missing
#
# Requires: kubectl with access to the target cluster.
set -euo pipefail

CHART_CRDS="$(cd "$(dirname "$0")/.." && pwd)/chart/crds"
CONTEXT="${1:-}"
APPLY=0
for arg in "$@"; do
  [ "$arg" = "--apply" ] && APPLY=1
done
[ "$CONTEXT" = "--apply" ] && CONTEXT=""

kube() {
  if [ -n "$CONTEXT" ]; then
    kubectl --context "$CONTEXT" "$@"
  else
    kubectl "$@"
  fi
}

echo ">> chart CRDs: ${CHART_CRDS}"
echo ">> cluster:    $(kube config current-context 2>/dev/null || echo "${CONTEXT:-unknown}")"

missing=()
for file in "$CHART_CRDS"/*.yaml; do
  # The CRD's own metadata.name, which is what kubectl get crd takes.
  name="$(awk '/^metadata:/{m=1;next} m&&/^  name: /{print $2;exit}' "$file")"
  if [ -z "$name" ]; then
    echo "!! cannot read metadata.name from ${file}" >&2
    exit 2
  fi
  if kube get crd "$name" >/dev/null 2>&1; then
    printf '   present  %s\n' "$name"
  else
    printf '   MISSING  %s   (%s)\n' "$name" "${file#"$CHART_CRDS"/}"
    missing+=("$file")
  fi
done

if [ ${#missing[@]} -eq 0 ]; then
  echo ">> every chart CRD is present on the cluster"
  exit 0
fi

echo
echo "!! ${#missing[@]} CRD(s) the chart ships are absent from this cluster."
echo "!! Rolling the manager against it will crash-loop the whole operator, not just these kinds."

if [ "$APPLY" -eq 0 ]; then
  echo "!! Apply them, then re-run:"
  for file in "${missing[@]}"; do
    echo "     kubectl ${CONTEXT:+--context $CONTEXT }apply -f ${file#"$(cd "$(dirname "$0")/.." && pwd)"/}"
  done
  exit 1
fi

echo ">> applying"
for file in "${missing[@]}"; do
  kube apply -f "$file"
done
echo ">> re-checking"
exec "$0" ${CONTEXT:+"$CONTEXT"}

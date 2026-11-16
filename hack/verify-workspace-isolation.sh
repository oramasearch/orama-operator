#!/bin/sh
# SPDX-License-Identifier: MIT

set -eu

if [ "$#" -ne 4 ]; then
  echo "usage: $0 <workspace-namespace> <lake-namespace> <lake-name> <other-workspace-namespace>" >&2
  exit 2
fi

WORKSPACE_NAMESPACE=$1
LAKE_NAMESPACE=$2
LAKE_NAME=$3
OTHER_WORKSPACE_NAMESPACE=$4
OTHER_WORKSPACE_SUFFIX=${OTHER_WORKSPACE_NAMESPACE##*-ws-}
JOB_NAME=workspace-isolation-verifier

KUBE_SYSTEM_POD_IP=$(kubectl get pods -n kube-system -l k8s-app=kube-dns \
  --field-selector=status.phase=Running -o jsonpath='{.items[0].status.podIP}')
if [ -z "$KUBE_SYSTEM_POD_IP" ]; then
  echo "no running kube-dns pod was found" >&2
  exit 1
fi

if kubectl get rolebindings -n "$WORKSPACE_NAMESPACE" -o name | grep -q .; then
  echo "workspace namespace contains a RoleBinding" >&2
  kubectl get rolebindings -n "$WORKSPACE_NAMESPACE" -o name >&2
  exit 1
fi

BOUND_CLUSTER_ROLES=$(kubectl get clusterrolebindings -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{range .subjects[*]}{.kind}{"/"}{.namespace}{"/"}{.name}{" "}{end}{"\n"}{end}' | \
  awk -v namespace="$WORKSPACE_NAMESPACE" 'index($0, "ServiceAccount/" namespace "/") { print }')
if [ -n "$BOUND_CLUSTER_ROLES" ]; then
  echo "workspace ServiceAccount is named by a ClusterRoleBinding" >&2
  echo "$BOUND_CLUSTER_ROLES" >&2
  exit 1
fi

kubectl delete job "$JOB_NAME" -n "$WORKSPACE_NAMESPACE" --ignore-not-found=true --wait=true
kubectl apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${JOB_NAME}
  namespace: ${WORKSPACE_NAMESPACE}
  labels:
    app.kubernetes.io/name: workspace-isolation-verifier
spec:
  backoffLimit: 0
  activeDeadlineSeconds: 120
  ttlSecondsAfterFinished: 300
  template:
    metadata:
      labels:
        app.kubernetes.io/name: workspace-isolation-verifier
    spec:
      automountServiceAccountToken: false
      restartPolicy: Never
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: verify
          image: busybox:1.37.0
          imagePullPolicy: IfNotPresent
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
          command:
            - /bin/sh
            - -ec
            - |
              must_fail() {
                label=$1
                host=$2
                port=$3
                if nc -w 3 "$host" "$port" </dev/null; then
                  echo "FAIL: $label was reachable"
                  exit 1
                fi
                echo "PASS: $label refused"
              }

              test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token
              nslookup "lake-${LAKE_NAME}.${LAKE_NAMESPACE}.svc.cluster.local"
              nc -w 5 "lake-${LAKE_NAME}.${LAKE_NAMESPACE}.svc.cluster.local" 80 </dev/null
              echo "PASS: DNS and Lake Service are reachable"

              must_fail "Kubernetes API" kubernetes.default.svc 443
              if wget -q -T 3 -O /dev/null https://kubernetes.default.svc/api/v1/pods; then
                echo "FAIL: pod listing unexpectedly succeeded"
                exit 1
              fi
              if wget -q -T 3 -O /dev/null https://kubernetes.default.svc/api/v1/secrets; then
                echo "FAIL: Secret listing unexpectedly succeeded"
                exit 1
              fi
              echo "PASS: Kubernetes pod and Secret listing refused"

              must_fail "another workspace Zetta" \
                "${LAKE_NAME}-ws-${OTHER_WORKSPACE_SUFFIX}-zetta.${OTHER_WORKSPACE_NAMESPACE}.svc.cluster.local" 4800
              must_fail "Lake Postgres" \
                "lake-${LAKE_NAME}-controldb-rw.${LAKE_NAMESPACE}.svc.cluster.local" 5432
              must_fail "cloud metadata" 169.254.169.254 80
              must_fail "kube-system pod" "${KUBE_SYSTEM_POD_IP}" 9153
EOF

if ! kubectl wait -n "$WORKSPACE_NAMESPACE" --for=condition=complete "job/${JOB_NAME}" --timeout=150s; then
  kubectl logs -n "$WORKSPACE_NAMESPACE" "job/${JOB_NAME}" >&2 || true
  exit 1
fi
kubectl logs -n "$WORKSPACE_NAMESPACE" "job/${JOB_NAME}"

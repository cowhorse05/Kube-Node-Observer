#!/usr/bin/env bash
set -euo pipefail

release="${HELM_SMOKE_RELEASE:-kno-smoke}"
namespace="${HELM_SMOKE_NAMESPACE:-kno-smoke}"

cleanup() {
  helm uninstall "$release" --namespace "$namespace" >/dev/null 2>&1 || true
  kubectl delete namespace "$namespace" --wait=false >/dev/null 2>&1 || true
}
trap cleanup EXIT

kubectl cluster-info >/dev/null
helm upgrade --install "$release" charts/kubenode-observer \
  --namespace "$namespace" \
  --create-namespace \
  --set image.repository=kubenode-observer/kno-agent \
  --set image.tag=smoke \
  --set image.pullPolicy=Never \
  --wait=false

fullname="${release}-kubenode-observer"
test "$(kubectl -n "$namespace" get daemonset "$fullname" -o jsonpath='{.spec.template.spec.hostPID}')" = "true"
test "$(kubectl -n "$namespace" get daemonset "$fullname" -o jsonpath='{.spec.template.spec.containers[0].securityContext.privileged}')" = "true"
test "$(kubectl -n "$namespace" get service "$fullname" -o jsonpath='{.spec.ports[0].name}')" = "http"
kubectl get clusterrole "$fullname" >/dev/null

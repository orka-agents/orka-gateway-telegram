#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${KUBE_CONTEXT:-}" ]]; then
  printf 'KUBE_CONTEXT must name the target Kubernetes context\n' >&2
  exit 2
fi
readonly KUBE_CONTEXT
readonly NAMESPACE="${NAMESPACE:-orka-gateway-telegram}"
readonly TUNNEL_DEPLOYMENT="${TUNNEL_DEPLOYMENT:-telegram-quick-tunnel}"
readonly ADAPTER_DEPLOYMENT="${ADAPTER_DEPLOYMENT:-orka-gateway-telegram}"
readonly GATEWAY="${GATEWAY:-telegram}"

for command in kubectl grep head; do
  command -v "${command}" >/dev/null || { echo "required command not found: ${command}" >&2; exit 1; }
done

kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  rollout status "deployment/${TUNNEL_DEPLOYMENT}" --timeout=5m

url=""
for ((attempt = 0; attempt < 30; attempt++)); do
  url="$(kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
    logs "deployment/${TUNNEL_DEPLOYMENT}" --tail=200 2>/dev/null | \
    grep -Eo 'https://[a-z0-9-]+\.trycloudflare\.com' | head -n 1 || true)"
  [[ -z "${url}" ]] || break
  sleep 2
done
[[ -n "${url}" ]] || { echo 'quick tunnel URL not found in cloudflared logs' >&2; exit 1; }

kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  patch configmap orka-gateway-telegram --type merge \
  -p "{\"data\":{\"TELEGRAM_WEBHOOK_URL\":\"${url}/telegram/webhook\"}}"
kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  annotate secret telegram-gateway-outbound \
  gateway.orka.ai/adapter-endpoint="${url}" --overwrite
kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  patch gateway "${GATEWAY}" --type merge \
  -p "{\"spec\":{\"adapter\":{\"endpoint\":\"${url}\"}}}"
kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  rollout restart "deployment/${ADAPTER_DEPLOYMENT}"
kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  rollout status "deployment/${ADAPTER_DEPLOYMENT}" --timeout=5m

printf '%s\n' "${url}"

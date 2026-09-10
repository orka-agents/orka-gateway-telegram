#!/usr/bin/env bash
set -euo pipefail

if [[ -z "${KUBE_CONTEXT:-}" ]]; then
  printf 'KUBE_CONTEXT must name the target Kubernetes context\n' >&2
  exit 2
fi
readonly KUBE_CONTEXT
if [[ -z "${NAMESPACE:-}" ]]; then
  printf 'NAMESPACE must name the existing Orka watched namespace\n' >&2
  exit 2
fi
readonly NAMESPACE
readonly KUBECTL="${KUBECTL:-kubectl}"
readonly TUNNEL_DEPLOYMENT="${TUNNEL_DEPLOYMENT:-telegram-quick-tunnel}"
readonly ADAPTER_DEPLOYMENT="${ADAPTER_DEPLOYMENT:-orka-gateway-telegram}"
readonly GATEWAY="${GATEWAY:-telegram}"

for command in "${KUBECTL}" grep head; do
  command -v "${command}" >/dev/null || { echo "required command not found: ${command}" >&2; exit 1; }
done

namespace_mode="$("${KUBECTL}" --context "${KUBE_CONTEXT}" get namespace "${NAMESPACE}" \
  -o 'jsonpath={.metadata.labels.orka\.ai/controller-mode}')"
if [[ "${namespace_mode}" != harness-v2 ]]; then
  printf 'NAMESPACE must already belong to a harness-v2 Orka installation\n' >&2
  exit 2
fi

# Initial setup creates the routing resources after the tunnel URL is known.
"${KUBECTL}" --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  get "gateway/${GATEWAY}" --output=name >/dev/null
"${KUBECTL}" --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  rollout status "deployment/${TUNNEL_DEPLOYMENT}" --timeout=5m >/dev/null

url=""
for ((attempt = 0; attempt < 30; attempt++)); do
  url="$("${KUBECTL}" --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
    logs "deployment/${TUNNEL_DEPLOYMENT}" --tail=200 2>/dev/null | \
    grep -Eo 'https://[a-z0-9-]+\.trycloudflare\.com' | head -n 1 || true)"
  [[ -z "${url}" ]] || break
  sleep 2
done
[[ -n "${url}" ]] || { echo 'quick tunnel URL not found in cloudflared logs' >&2; exit 1; }

"${KUBECTL}" --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  patch configmap orka-gateway-telegram --type merge \
  -p "{\"data\":{\"TELEGRAM_WEBHOOK_URL\":\"${url}/telegram/webhook\"}}" >/dev/null
"${KUBECTL}" --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  annotate secret telegram-gateway-outbound \
  gateway.orka.ai/adapter-endpoint="${url}" --overwrite >/dev/null
"${KUBECTL}" --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  patch gateway "${GATEWAY}" --type merge \
  -p "{\"spec\":{\"adapter\":{\"endpoint\":\"${url}\"}}}" >/dev/null
"${KUBECTL}" --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  rollout restart "deployment/${ADAPTER_DEPLOYMENT}" >/dev/null
"${KUBECTL}" --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  rollout status "deployment/${ADAPTER_DEPLOYMENT}" --timeout=5m >/dev/null

printf '%s\n' "${url}"

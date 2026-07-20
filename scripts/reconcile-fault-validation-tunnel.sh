#!/usr/bin/env bash
set -Eeuo pipefail

umask 077

fail() {
  printf 'fault-validation tunnel reconciliation failed: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

[[ -n "${KUBE_CONTEXT:-}" ]] || fail 'set KUBE_CONTEXT explicitly'

readonly KUBECTL_BIN="${KUBECTL_BIN:-kubectl}"
readonly KUBE_CONTEXT
readonly NAMESPACE='orka-gateway-telegram-fi'
readonly TUNNEL_DEPLOYMENT='telegram-fault-quick-tunnel'
readonly TUNNEL_SELECTOR='app.kubernetes.io/name=telegram-fault-quick-tunnel,app.kubernetes.io/instance=fault-validation'
readonly ADAPTER_DEPLOYMENT='orka-gateway-telegram-shadow'
readonly ADAPTER_CONFIGMAP='orka-gateway-telegram-shadow'
readonly GATEWAY='telegram-fault-validation'
readonly INBOUND_SECRET='telegram-fault-gateway-inbound'
readonly OUTBOUND_SECRET='telegram-fault-gateway-outbound'
readonly WAIT_TIMEOUT="${WAIT_TIMEOUT:-5m}"
readonly TUNNEL_URL_ATTEMPTS="${TUNNEL_URL_ATTEMPTS:-90}"
readonly FORCE_ADAPTER_RESTART="${FORCE_ADAPTER_RESTART:-false}"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
readonly REPO_ROOT
readonly GATEWAY_TEMPLATE="${REPO_ROOT}/deploy/fault-validation/gateway-and-binding.yaml.tmpl"
readonly -a KUBECTL=("${KUBECTL_BIN}" --context "${KUBE_CONTEXT}")

for command in "${KUBECTL_BIN}" jq grep tail envsubst mktemp rm sleep; do
  require_command "${command}"
done
[[ -f "${GATEWAY_TEMPLATE}" ]] || fail "gateway template not found: ${GATEWAY_TEMPLATE}"
[[ "${TUNNEL_URL_ATTEMPTS}" =~ ^[1-9][0-9]*$ ]] || fail 'TUNNEL_URL_ATTEMPTS must be a positive integer'
case "${FORCE_ADAPTER_RESTART}" in
  true|false) ;;
  *) fail 'FORCE_ADAPTER_RESTART must be true or false' ;;
esac

TEMP_DIR="$(mktemp -d)"
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  rm -rf -- "${TEMP_DIR}"
  exit "${status}"
}
trap cleanup EXIT INT TERM

newest_tunnel_pod() {
  local pod_list
  pod_list="$("${KUBECTL[@]}" --namespace "${NAMESPACE}" get pods \
    --selector "${TUNNEL_SELECTOR}" --output=json 2>/dev/null)" || return 0
  jq -r '
    [.items[]
      | select(.metadata.deletionTimestamp == null)
      | select(.status.phase == "Running")]
    | sort_by([.metadata.creationTimestamp // "", .metadata.name // ""])
    | last
    | .metadata.name // empty
  ' <<<"${pod_list}"
}

current_tunnel_url() {
  local attempt pod logs url
  for ((attempt = 1; attempt <= TUNNEL_URL_ATTEMPTS; attempt++)); do
    pod="$(newest_tunnel_pod)"
    if [[ -n "${pod}" ]]; then
      logs="$("${KUBECTL[@]}" --namespace "${NAMESPACE}" logs "pod/${pod}" \
        --container cloudflared --tail=500 2>/dev/null || true)"
      url="$(LC_ALL=C grep -Eo 'https://[a-z0-9][a-z0-9-]*\.trycloudflare\.com' <<<"${logs}" | tail -n 1 || true)"
      if [[ -n "${url}" ]]; then
        printf '%s\n' "${url%/}"
        return 0
      fi
    fi
    sleep 2
  done
  return 1
}

"${KUBECTL[@]}" --namespace "${NAMESPACE}" \
  rollout status "deployment/${TUNNEL_DEPLOYMENT}" --timeout="${WAIT_TIMEOUT}" >/dev/null

TUNNEL_URL="$(current_tunnel_url)" || fail 'current Quick Tunnel URL not found in the newest running pod logs'
readonly TUNNEL_URL
readonly WEBHOOK_URL="${TUNNEL_URL}/telegram/webhook"

old_webhook_url="$("${KUBECTL[@]}" --namespace "${NAMESPACE}" get \
  "configmap/${ADAPTER_CONFIGMAP}" --output=jsonpath='{.data.TELEGRAM_WEBHOOK_URL}' 2>/dev/null || true)"
old_outbound_endpoint="$("${KUBECTL[@]}" --namespace "${NAMESPACE}" get \
  "secret/${OUTBOUND_SECRET}" --output=json 2>/dev/null | \
  jq -r '.metadata.annotations["gateway.orka.ai/adapter-endpoint"] // empty' || true)"
old_gateway_endpoint="$("${KUBECTL[@]}" --namespace "${NAMESPACE}" get \
  "gateway/${GATEWAY}" --output=json 2>/dev/null | \
  jq -r '.spec.adapter.endpoint // empty' || true)"

if [[ "${old_webhook_url}" != "${WEBHOOK_URL}" ]]; then
  config_patch="$(jq -cn --arg url "${WEBHOOK_URL}" '{data:{TELEGRAM_WEBHOOK_URL:$url}}')"
  "${KUBECTL[@]}" --namespace "${NAMESPACE}" patch "configmap/${ADAPTER_CONFIGMAP}" \
    --type=merge --patch "${config_patch}" >/dev/null
  unset config_patch
fi

"${KUBECTL[@]}" --namespace "${NAMESPACE}" label "secret/${INBOUND_SECRET}" \
  gateway.orka.ai/inbound-auth=true \
  gateway.orka.ai/gateway-name="${GATEWAY}" \
  --overwrite >/dev/null
"${KUBECTL[@]}" --namespace "${NAMESPACE}" annotate "secret/${INBOUND_SECRET}" \
  gateway.orka.ai/gateway-name="${GATEWAY}" \
  --overwrite >/dev/null

"${KUBECTL[@]}" --namespace "${NAMESPACE}" label "secret/${OUTBOUND_SECRET}" \
  gateway.orka.ai/outbound-auth=true \
  gateway.orka.ai/gateway-name="${GATEWAY}" \
  --overwrite >/dev/null
"${KUBECTL[@]}" --namespace "${NAMESPACE}" annotate "secret/${OUTBOUND_SECRET}" \
  gateway.orka.ai/gateway-name="${GATEWAY}" \
  gateway.orka.ai/adapter-endpoint="${TUNNEL_URL}" \
  --overwrite >/dev/null

export TUNNEL_URL
# The single quotes intentionally pass the literal variable allowlist to envsubst.
# shellcheck disable=SC2016
envsubst '${TUNNEL_URL}' <"${GATEWAY_TEMPLATE}" >"${TEMP_DIR}/gateway-and-binding.yaml"
# shellcheck disable=SC2016
if grep -Fq '${TUNNEL_URL}' "${TEMP_DIR}/gateway-and-binding.yaml"; then
  fail 'gateway template still contains an unresolved tunnel URL placeholder'
fi
"${KUBECTL[@]}" apply --filename "${TEMP_DIR}/gateway-and-binding.yaml" >/dev/null

restart_required=false
if [[ "${FORCE_ADAPTER_RESTART}" == true || \
      "${old_webhook_url}" != "${WEBHOOK_URL}" || \
      "${old_outbound_endpoint}" != "${TUNNEL_URL}" || \
      "${old_gateway_endpoint}" != "${TUNNEL_URL}" ]]; then
  restart_required=true
fi
if [[ "${restart_required}" == true ]]; then
  "${KUBECTL[@]}" --namespace "${NAMESPACE}" \
    rollout restart "deployment/${ADAPTER_DEPLOYMENT}" >/dev/null
  "${KUBECTL[@]}" --namespace "${NAMESPACE}" \
    rollout status "deployment/${ADAPTER_DEPLOYMENT}" --timeout="${WAIT_TIMEOUT}" >/dev/null
fi

printf '%s\n' "${TUNNEL_URL}"

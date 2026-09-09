#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

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

# Validate the adapter and built-in Codex v2 route without reading Kubernetes
# Secret data. Protocol authentication uses the operator's local token file.
readonly ADAPTER_DEPLOYMENT="${ADAPTER_DEPLOYMENT:-orka-gateway-telegram}"
readonly ADAPTER_SERVICE="${ADAPTER_SERVICE:-orka-gateway-telegram}"
readonly GATEWAY_CLASS="${GATEWAY_CLASS:-telegram-chat-live-c65ebad8}"
readonly GATEWAY="${GATEWAY:-telegram}"
readonly GATEWAY_BINDING="${GATEWAY_BINDING:-telegram-ai}"
readonly AI_AGENT="${AI_AGENT:-telegram-ai}"
readonly LOCAL_PORT="${LOCAL_PORT:-18080}"
readonly WAIT_TIMEOUT="${WAIT_TIMEOUT:-5m}"

PORT_FORWARD_PID=""
CURL_CONFIG=""
PORT_FORWARD_LOG=""

cleanup() {
  local status=$?
  if [[ -n "${PORT_FORWARD_PID}" ]]; then
    kill "${PORT_FORWARD_PID}" >/dev/null 2>&1 || true
    wait "${PORT_FORWARD_PID}" >/dev/null 2>&1 || true
  fi
  [[ -z "${CURL_CONFIG}" ]] || rm -f "${CURL_CONFIG}"
  [[ -z "${PORT_FORWARD_LOG}" ]] || rm -f "${PORT_FORWARD_LOG}"
  exit "${status}"
}
trap cleanup EXIT INT TERM

require_command() {
  command -v "$1" >/dev/null 2>&1 || {
    printf 'required command not found: %s\n' "$1" >&2
    exit 1
  }
}

wait_for_local_adapter() {
  local attempts=0
  until curl --silent --show-error --fail \
    --config "${CURL_CONFIG}" \
    "http://127.0.0.1:${LOCAL_PORT}/v1/health" >/dev/null; do
    attempts=$((attempts + 1))
    if (( attempts >= 30 )); then
      printf 'adapter did not become reachable through port-forward\n' >&2
      if [[ -s "${PORT_FORWARD_LOG}" ]]; then
        sed -n '1,80p' "${PORT_FORWARD_LOG}" >&2
      fi
      return 1
    fi
    sleep 1
  done
}

wait_for_current_status() {
  local resource="$1"
  local namespace="$2"
  local field="$3"
  local expected="$4"
  local attempts=0
  while (( attempts < 60 )); do
    local args=(--context "${KUBE_CONTEXT}")
    [[ -z "${namespace}" ]] || args+=(--namespace "${namespace}")
    local payload
    payload="$("${KUBECTL}" "${args[@]}" get "${resource}" --output=json)"
    local generation observed value
    generation="$(jq -r '.metadata.generation // 0' <<<"${payload}")"
    observed="$(jq -r '.status.observedGeneration // 0' <<<"${payload}")"
    value="$(jq -r "${field} // empty" <<<"${payload}")"
    if [[ "${value}" == "${expected}" && "${observed}" == "${generation}" ]]; then
      return 0
    fi
    attempts=$((attempts + 1))
    sleep 2
  done
  printf 'timed out waiting for current status on %s\n' "${resource}" >&2
  return 1
}

wait_for_current_agent() {
  local name="$1"
  local attempts=0
  while (( attempts < 60 )); do
    local payload
    payload="$("${KUBECTL}" --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
      get "agent/${name}" --output=json)"
    if jq -e '
      . as $agent |
      .status.ready == true and
      any(.status.conditions[]?;
        .type == "Ready" and
        .status == "True" and
        .observedGeneration == $agent.metadata.generation)
    ' <<<"${payload}" >/dev/null; then
      printf '%s' "${payload}"
      return 0
    fi
    attempts=$((attempts + 1))
    sleep 2
  done
  printf 'timed out waiting for current status on agent/%s\n' "${name}" >&2
  return 1
}

for command in "${KUBECTL}" curl jq; do
  require_command "${command}"
done

namespace_mode="$("${KUBECTL}" --context "${KUBE_CONTEXT}" get namespace "${NAMESPACE}" \
  -o 'jsonpath={.metadata.labels.orka\.ai/controller-mode}')"
if [[ "${namespace_mode}" != harness-v2 ]]; then
  printf 'NAMESPACE must already belong to a harness-v2 Orka installation\n' >&2
  exit 2
fi

if [[ -z "${ORKA_GATEWAY_OUTBOUND_TOKEN_FILE:-}" ]]; then
  printf 'set ORKA_GATEWAY_OUTBOUND_TOKEN_FILE to a mode-0600 token file\n' >&2
  exit 2
fi
if [[ ! -f "${ORKA_GATEWAY_OUTBOUND_TOKEN_FILE}" ]]; then
  printf 'outbound token file does not exist\n' >&2
  exit 2
fi
if [[ -L "${ORKA_GATEWAY_OUTBOUND_TOKEN_FILE}" ]]; then
  printf 'outbound token file must not be a symlink\n' >&2
  exit 2
fi
token_mode="$(stat -f '%Lp' "${ORKA_GATEWAY_OUTBOUND_TOKEN_FILE}" 2>/dev/null || stat -c '%a' "${ORKA_GATEWAY_OUTBOUND_TOKEN_FILE}")"
if [[ "${token_mode}" != "600" ]]; then
  printf 'outbound token file must have mode 0600 (got %s)\n' "${token_mode}" >&2
  exit 2
fi

outbound_token="$(tr -d '\r\n' < "${ORKA_GATEWAY_OUTBOUND_TOKEN_FILE}")"
if [[ -z "${outbound_token}" ]]; then
  printf 'outbound token file is empty\n' >&2
  exit 2
fi

CURL_CONFIG="$(mktemp)"
PORT_FORWARD_LOG="$(mktemp)"
chmod 0600 "${CURL_CONFIG}" "${PORT_FORWARD_LOG}"
printf 'header = "Authorization: Bearer %s"\n' "${outbound_token}" > "${CURL_CONFIG}"
unset outbound_token

printf 'Checking Kubernetes context and base rollout...\n'
"${KUBECTL}" --context "${KUBE_CONTEXT}" cluster-info >/dev/null
"${KUBECTL}" --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  rollout status "deployment/${ADAPTER_DEPLOYMENT}" --timeout="${WAIT_TIMEOUT}"
"${KUBECTL}" --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  wait --for=jsonpath='{.status.phase}'=Bound "pvc/${ADAPTER_DEPLOYMENT}-data" --timeout="${WAIT_TIMEOUT}"
"${KUBECTL}" --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  get "service/${ADAPTER_SERVICE}" >/dev/null

for secret in telegram-adapter-secrets telegram-gateway-inbound telegram-gateway-outbound; do
  "${KUBECTL}" --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" get "secret/${secret}" --output=name >/dev/null
done

ingress_url="$("${KUBECTL}" --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  get configmap/orka-gateway-telegram -o 'jsonpath={.data.ORKA_GATEWAY_INGRESS_URL}')"
if [[ "${ingress_url}" != */api/v1/gateways/"${NAMESPACE}"/"${GATEWAY}"/events ]]; then
  printf 'adapter ingress URL must target the selected namespace and Gateway\n' >&2
  exit 2
fi

printf 'Opening a local-only port-forward for authenticated protocol checks...\n'
"${KUBECTL}" --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  port-forward "service/${ADAPTER_SERVICE}" "${LOCAL_PORT}:8080" \
  >"${PORT_FORWARD_LOG}" 2>&1 &
PORT_FORWARD_PID=$!
wait_for_local_adapter

health_json="$(curl --silent --show-error --fail \
  --config "${CURL_CONFIG}" \
  "http://127.0.0.1:${LOCAL_PORT}/v1/health")"
jq -e '.status == "ok"' <<<"${health_json}" >/dev/null

capabilities_json="$(curl --silent --show-error --fail \
  --config "${CURL_CONFIG}" \
  "http://127.0.0.1:${LOCAL_PORT}/v1/capabilities")"
jq -e '
  .protocolVersion == "orka.gateway.v1" and
  .capabilities.inboundText == true and
  .capabilities.outboundText == true and
  .capabilities.idempotentDelivery == true
' <<<"${capabilities_json}" >/dev/null

printf 'Checking the Orka Codex v2 route...\n'
wait_for_current_status "gatewayclass/${GATEWAY_CLASS}" "" '.status.accepted' true
wait_for_current_status "gateway/${GATEWAY}" "${NAMESPACE}" '.status.ready' true
agent_json="$(wait_for_current_agent "${AI_AGENT}")"
jq -e '
  .spec.runtime.type == "codex" and
  .spec.runtime.contractVersion == "orka.harness.v2" and
  .spec.runtime.defaultAllowBash == true and
  .spec.secretRef == null and
  .spec.model.temperature == null and
  (.spec.model.name | type == "string" and length > 0)
' <<<"${agent_json}" >/dev/null
wait_for_current_status "gatewaybinding/${GATEWAY_BINDING}" "${NAMESPACE}" '.status.ready' true
"${KUBECTL}" --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" \
  get "gatewaybinding/${GATEWAY_BINDING}" --output=json | \
  jq -e --arg agent "${AI_AGENT}" '.spec.agentRef.name == $agent' >/dev/null

if [[ -n "${ADAPTER_URL:-}" ]]; then
  curl --silent --show-error --fail --config "${CURL_CONFIG}" \
    "${ADAPTER_URL%/}/v1/health" | jq -e '.status == "ok"' >/dev/null
fi

printf 'Protocol and resource readiness checks passed.\n'
printf 'Send a Telegram message from the configured sender/chat, then verify activity with:\n'
printf '  kubectl --context %q --namespace %q get gatewaybinding/%q -o jsonpath=' \
  "${KUBE_CONTEXT}" "${NAMESPACE}" "${GATEWAY_BINDING}"
printf '%q\n' '{.status.lastInboundActivity}{" -> "}{.status.lastOutboundActivity}{"\n"}'

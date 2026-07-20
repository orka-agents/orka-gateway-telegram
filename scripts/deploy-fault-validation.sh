#!/usr/bin/env bash
set -Eeuo pipefail

umask 077

fail() {
  printf 'fault-validation deployment failed: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

validate_immutable_image() {
  local variable_name="$1"
  local image="$2"
  [[ -n "${image}" ]] || fail "set ${variable_name} to an immutable image reference"
  [[ "${image}" =~ ^[A-Za-z0-9._/@:+-]+$ ]] || fail "${variable_name} contains unsupported characters"
  [[ "${image}" =~ @sha256:[[:xdigit:]]{64}$ ]] || \
    fail "${variable_name} must end with an immutable @sha256 digest"
  [[ "${image}" != *REPLACE_WITH_* ]] || fail "${variable_name} still contains a placeholder"
}

[[ -n "${KUBE_CONTEXT:-}" ]] || fail 'set KUBE_CONTEXT explicitly'
[[ -n "${TELEGRAM_FAULT_PROXY_IMAGE:-}" ]] || fail 'set TELEGRAM_FAULT_PROXY_IMAGE explicitly'
[[ -n "${ORKA_GATEWAY_TELEGRAM_IMAGE:-}" ]] || fail 'set ORKA_GATEWAY_TELEGRAM_IMAGE explicitly'

readonly KUBECTL_BIN="${KUBECTL_BIN:-kubectl}"
readonly KUBE_CONTEXT
readonly TELEGRAM_FAULT_PROXY_IMAGE
readonly ORKA_GATEWAY_TELEGRAM_IMAGE
readonly NAMESPACE='orka-gateway-telegram-fi'
readonly LIVE_NAMESPACE='orka-gateway-telegram'
readonly GATEWAY_CLASS='telegram-chat-live-c65ebad8'
readonly GATEWAY='telegram-fault-validation'
readonly GATEWAY_BINDING='telegram-fault-echo'
readonly AGENT_RUNTIME='telegram-fault-echo-runtime'
readonly AGENT='telegram-fault-echo'
readonly WAIT_TIMEOUT="${WAIT_TIMEOUT:-5m}"
readonly STATUS_ATTEMPTS="${STATUS_ATTEMPTS:-150}"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
REPO_ROOT="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
readonly REPO_ROOT
readonly FIXTURE_DIR="${REPO_ROOT}/deploy/fault-validation"
readonly RECONCILE_SCRIPT="${SCRIPT_DIR}/reconcile-fault-validation-tunnel.sh"
readonly -a KUBECTL=("${KUBECTL_BIN}" --context "${KUBE_CONTEXT}")

[[ "${NAMESPACE}" != "${LIVE_NAMESPACE}" ]] || fail 'isolated namespace guard failed'
validate_immutable_image TELEGRAM_FAULT_PROXY_IMAGE "${TELEGRAM_FAULT_PROXY_IMAGE}"
validate_immutable_image ORKA_GATEWAY_TELEGRAM_IMAGE "${ORKA_GATEWAY_TELEGRAM_IMAGE}"
[[ "${STATUS_ATTEMPTS}" =~ ^[1-9][0-9]*$ ]] || fail 'STATUS_ATTEMPTS must be a positive integer'
[[ -d "${FIXTURE_DIR}" ]] || fail "fixture directory not found: ${FIXTURE_DIR}"
[[ -x "${RECONCILE_SCRIPT}" ]] || fail "tunnel reconciler is not executable: ${RECONCILE_SCRIPT}"

for command in "${KUBECTL_BIN}" openssl sed grep jq mktemp chmod rm sleep tr; do
  require_command "${command}"
done

TEMP_DIR="$(mktemp -d)"
cleanup() {
  local status=$?
  trap - EXIT INT TERM
  rm -rf -- "${TEMP_DIR}"
  exit "${status}"
}
trap cleanup EXIT INT TERM

apply_secret() {
  local name="$1"
  shift
  "${KUBECTL[@]}" --namespace "${NAMESPACE}" create secret generic "${name}" \
    "$@" --dry-run=client --output=yaml | \
    "${KUBECTL[@]}" --namespace "${NAMESPACE}" apply --filename - >/dev/null
}

label_fixture_secret() {
  local name="$1"
  local app_name="$2"
  "${KUBECTL[@]}" --namespace "${NAMESPACE}" label "secret/${name}" \
    app.kubernetes.io/name="${app_name}" \
    app.kubernetes.io/instance=fault-validation \
    app.kubernetes.io/part-of=orka-gateway-telegram \
    --overwrite >/dev/null
}

wait_for_current_status() {
  local resource="$1"
  local namespace="$2"
  local field="$3"
  local expected="$4"
  local attempt payload generation observed value

  for ((attempt = 1; attempt <= STATUS_ATTEMPTS; attempt++)); do
    if [[ -n "${namespace}" ]]; then
      payload="$("${KUBECTL[@]}" --namespace "${namespace}" get "${resource}" --output=json 2>/dev/null || true)"
    else
      payload="$("${KUBECTL[@]}" get "${resource}" --output=json 2>/dev/null || true)"
    fi
    if [[ -n "${payload}" ]]; then
      generation="$(jq -r '.metadata.generation // 0' <<<"${payload}")"
      observed="$(jq -r '.status.observedGeneration // 0' <<<"${payload}")"
      value="$(jq -r "${field} // empty" <<<"${payload}")"
      if [[ "${value}" == "${expected}" && "${observed}" == "${generation}" ]]; then
        return 0
      fi
    fi
    sleep 2
  done
  fail "timed out waiting for current status on ${resource}"
}

wait_for_current_agent() {
  local attempt payload
  for ((attempt = 1; attempt <= STATUS_ATTEMPTS; attempt++)); do
    payload="$("${KUBECTL[@]}" --namespace "${NAMESPACE}" get \
      "agent/${AGENT}" --output=json 2>/dev/null || true)"
    if [[ -n "${payload}" ]] && jq -e '
      . as $agent |
      .status.ready == true and
      any(.status.conditions[]?;
        .type == "Ready" and
        .status == "True" and
        .observedGeneration == $agent.metadata.generation)
    ' <<<"${payload}" >/dev/null; then
      return 0
    fi
    sleep 2
  done
  fail "timed out waiting for current status on agent/${AGENT}"
}

"${KUBECTL[@]}" cluster-info >/dev/null
"${KUBECTL[@]}" get "gatewayclass/${GATEWAY_CLASS}" >/dev/null
"${KUBECTL[@]}" apply --filename "${FIXTURE_DIR}/namespace.yaml" >/dev/null

{
  printf '900000001:'
  openssl rand -hex 32 | tr -d '\r\n'
} >"${TEMP_DIR}/telegram-bot-token"
openssl rand -hex 32 >"${TEMP_DIR}/telegram-webhook-secret"
openssl rand -hex 32 >"${TEMP_DIR}/gateway-inbound-token"
openssl rand -hex 32 >"${TEMP_DIR}/gateway-outbound-token"
openssl rand -hex 32 >"${TEMP_DIR}/fault-proxy-control-token"
openssl rand -hex 32 >"${TEMP_DIR}/echo-runtime-token"
chmod 0600 "${TEMP_DIR}"/*

apply_secret telegram-fault-adapter-secrets \
  --from-file=telegram-bot-token="${TEMP_DIR}/telegram-bot-token" \
  --from-file=telegram-webhook-secret="${TEMP_DIR}/telegram-webhook-secret"
apply_secret telegram-fault-gateway-inbound \
  --from-file=token="${TEMP_DIR}/gateway-inbound-token"
apply_secret telegram-fault-gateway-outbound \
  --from-file=token="${TEMP_DIR}/gateway-outbound-token"
apply_secret telegram-fault-proxy-control \
  --from-file=control-token="${TEMP_DIR}/fault-proxy-control-token"
apply_secret telegram-fault-echo-runtime-token \
  --from-file=token="${TEMP_DIR}/echo-runtime-token"

label_fixture_secret telegram-fault-adapter-secrets orka-gateway-telegram
label_fixture_secret telegram-fault-gateway-inbound orka-gateway-telegram
label_fixture_secret telegram-fault-gateway-outbound orka-gateway-telegram
label_fixture_secret telegram-fault-proxy-control telegram-fault-proxy
label_fixture_secret telegram-fault-echo-runtime-token telegram-fault-echo-runtime

"${KUBECTL[@]}" --namespace "${NAMESPACE}" label secret/telegram-fault-gateway-inbound \
  gateway.orka.ai/inbound-auth=true \
  gateway.orka.ai/gateway-name="${GATEWAY}" \
  --overwrite >/dev/null
"${KUBECTL[@]}" --namespace "${NAMESPACE}" annotate secret/telegram-fault-gateway-inbound \
  gateway.orka.ai/gateway-name="${GATEWAY}" \
  --overwrite >/dev/null

"${KUBECTL[@]}" --namespace "${NAMESPACE}" label secret/telegram-fault-gateway-outbound \
  gateway.orka.ai/outbound-auth=true \
  gateway.orka.ai/gateway-name="${GATEWAY}" \
  --overwrite >/dev/null
"${KUBECTL[@]}" --namespace "${NAMESPACE}" annotate secret/telegram-fault-gateway-outbound \
  gateway.orka.ai/gateway-name="${GATEWAY}" \
  --overwrite >/dev/null

readonly RUNTIME_ENDPOINT='http://telegram-fault-echo-runtime.orka-gateway-telegram-fi.svc.cluster.local:8080'
"${KUBECTL[@]}" --namespace "${NAMESPACE}" label secret/telegram-fault-echo-runtime-token \
  orka.ai/agent-runtime-auth=true \
  orka.ai/agent-runtime-name="${AGENT_RUNTIME}" \
  --overwrite >/dev/null
"${KUBECTL[@]}" --namespace "${NAMESPACE}" annotate secret/telegram-fault-echo-runtime-token \
  orka.ai/agent-runtime-endpoint="${RUNTIME_ENDPOINT}" \
  --overwrite >/dev/null

"${KUBECTL_BIN}" kustomize "${FIXTURE_DIR}" >"${TEMP_DIR}/base-placeholder.yaml"
sed \
  -e "s|docker.io/sozercan/telegram-fault-proxy:REPLACE_WITH_IMMUTABLE_TAG|${TELEGRAM_FAULT_PROXY_IMAGE}|g" \
  -e "s|docker.io/sozercan/orka-gateway-telegram:REPLACE_WITH_ADAPTER_IMAGE|${ORKA_GATEWAY_TELEGRAM_IMAGE}|g" \
  "${TEMP_DIR}/base-placeholder.yaml" >"${TEMP_DIR}/base-rendered.yaml"
if grep -Fq 'REPLACE_WITH_' "${TEMP_DIR}/base-rendered.yaml"; then
  fail 'rendered manifests still contain an image placeholder'
fi
if grep -Eq '^[[:space:]]*namespace:[[:space:]]*orka-gateway-telegram[[:space:]]*$' \
  "${TEMP_DIR}/base-rendered.yaml"; then
  fail 'rendered manifests target the live namespace'
fi
"${KUBECTL[@]}" apply --filename "${TEMP_DIR}/base-rendered.yaml" >/dev/null

# These processes read their bearer tokens only during startup. Explicitly
# restart them after every Secret refresh so rerunning this script cannot leave
# a new Secret paired with a process that still holds the previous value.
for deployment in telegram-fault-proxy telegram-fault-echo-runtime; do
  "${KUBECTL[@]}" --namespace "${NAMESPACE}" rollout restart \
    "deployment/${deployment}" >/dev/null
done

"${KUBECTL[@]}" --namespace "${NAMESPACE}" wait \
  --for=jsonpath='{.status.phase}'=Bound pvc/orka-gateway-telegram-shadow-data \
  --timeout="${WAIT_TIMEOUT}" >/dev/null
for deployment in \
  telegram-fault-proxy \
  telegram-fault-echo-runtime \
  orka-gateway-telegram-shadow \
  telegram-fault-quick-tunnel; do
  "${KUBECTL[@]}" --namespace "${NAMESPACE}" rollout status \
    "deployment/${deployment}" --timeout="${WAIT_TIMEOUT}" >/dev/null
done

TUNNEL_URL="$(
  env \
    KUBE_CONTEXT="${KUBE_CONTEXT}" \
    KUBECTL_BIN="${KUBECTL_BIN}" \
    WAIT_TIMEOUT="${WAIT_TIMEOUT}" \
    FORCE_ADAPTER_RESTART=true \
    "${RECONCILE_SCRIPT}"
)"
readonly TUNNEL_URL
[[ "${TUNNEL_URL}" =~ ^https://[a-z0-9][a-z0-9-]*\.trycloudflare\.com$ ]] || \
  fail 'tunnel reconciler returned an unexpected URL'

wait_for_current_status "gatewayclass/${GATEWAY_CLASS}" '' '.status.accepted' true
wait_for_current_status "agentruntime/${AGENT_RUNTIME}" "${NAMESPACE}" '.status.ready' true
wait_for_current_agent
wait_for_current_status "gateway/${GATEWAY}" "${NAMESPACE}" '.status.ready' true
wait_for_current_status "gatewaybinding/${GATEWAY_BINDING}" "${NAMESPACE}" '.status.ready' true

printf 'Fault-validation fixture ready.\n'
printf '  Context: %s\n' "${KUBE_CONTEXT}"
printf '  Namespace: %s\n' "${NAMESPACE}"
printf '  Fault proxy Service: %s\n' 'telegram-fault-proxy:8080'
printf '  Shadow adapter Service: %s\n' 'orka-gateway-telegram-shadow:8080'
printf '  Quick Tunnel: %s\n' "${TUNNEL_URL}"
printf '  Gateway / binding: %s / %s\n' "${GATEWAY}" "${GATEWAY_BINDING}"
printf '  Fake account / chat / sender IDs: %s / %s / %s\n' '900000001' '900000002' '900000002'

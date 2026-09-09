#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'manifest rendering failed: %s\n' "$*" >&2
  exit 2
}

require_value() {
  [[ -n "${!1:-}" ]] || fail "set $1 explicitly"
}

validate_base_url() {
  local name="$1" scheme="$2" value="${!1}"
  local host='([A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?|\[[0-9A-Fa-f:]+\])'
  [[ "${value}" =~ ^${scheme}://${host}(:[0-9]{1,5})?/?$ ]] ||
    fail "$name must be a base URL without credentials, a path, query, or fragment"
}

[[ $# -le 1 ]] || fail 'usage: render-manifests.sh [adapter|routing|tunnel]'
mode="${1:-adapter}"
case "${mode}" in adapter|routing|tunnel) ;; *) fail 'expected adapter, routing, or tunnel mode' ;; esac

require_value NAMESPACE
[[ ${#NAMESPACE} -le 63 && "${NAMESPACE}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] ||
  fail 'NAMESPACE must be a Kubernetes namespace name'

readonly KUBECTL="${KUBECTL:-kubectl}"
for command in "${KUBECTL}" jq; do
  command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
done
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
readonly REPO_ROOT="${SCRIPT_DIR}/.."

patches='[]'
case "${mode}" in
  adapter)
    require_value ORKA_API_URL
    validate_base_url ORKA_API_URL 'https?'
    "${SCRIPT_DIR}/validate-image.sh"
    webhook_url=''
    if [[ -n "${ADAPTER_URL:-}" ]]; then
      validate_base_url ADAPTER_URL https
      webhook_url="${ADAPTER_URL%/}/telegram/webhook"
    fi
    patches="$(jq -cn \
      --arg ingress "${ORKA_API_URL%/}/api/v1/gateways/${NAMESPACE}/telegram/events" \
      --arg webhook "${webhook_url}" \
      --arg image "${IMAGE}:${TAG}" \
      --arg storage "${STORAGE_CLASS:-}" '
      [
        {target:{version:"v1",kind:"ConfigMap",name:"orka-gateway-telegram"},patch:([
          {op:"replace",path:"/data/ORKA_GATEWAY_INGRESS_URL",value:$ingress},
          {op:"replace",path:"/data/TELEGRAM_WEBHOOK_URL",value:$webhook}
        ]|tojson)},
        {target:{group:"apps",version:"v1",kind:"Deployment",name:"orka-gateway-telegram"},patch:([
          {op:"replace",path:"/spec/template/spec/containers/0/image",value:$image}
        ]|tojson)}
      ] + if $storage == "" then [] else [
        {target:{version:"v1",kind:"PersistentVolumeClaim",name:"orka-gateway-telegram-data"},patch:([
          {op:"add",path:"/spec/storageClassName",value:$storage}
        ]|tojson)}
      ] end')"
    source_dir="${REPO_ROOT}/deploy"
    ;;
  routing)
    for variable in ADAPTER_URL AGENT_MODEL TELEGRAM_ACCOUNT_ID TELEGRAM_CHAT_ID TELEGRAM_SENDER_ID; do
      require_value "${variable}"
    done
    validate_base_url ADAPTER_URL https
    for variable in TELEGRAM_ACCOUNT_ID TELEGRAM_CHAT_ID TELEGRAM_SENDER_ID; do
      [[ "${!variable}" =~ ^[1-9][0-9]*$ ]] || fail "$variable must be a positive numeric Telegram identity"
    done
    patches="$(jq -cn \
      --arg model "${AGENT_MODEL}" --arg endpoint "${ADAPTER_URL%/}" \
      --arg account "${TELEGRAM_ACCOUNT_ID}" --arg chat "${TELEGRAM_CHAT_ID}" --arg sender "${TELEGRAM_SENDER_ID}" '
      [
        {target:{group:"core.orka.ai",version:"v1alpha1",kind:"Agent",name:"telegram-ai"},patch:([
          {op:"replace",path:"/spec/model/name",value:$model}
        ]|tojson)},
        {target:{group:"gateway.orka.ai",version:"v1alpha1",kind:"Gateway",name:"telegram"},patch:([
          {op:"replace",path:"/spec/adapter/endpoint",value:$endpoint}
        ]|tojson)},
        {target:{group:"gateway.orka.ai",version:"v1alpha1",kind:"GatewayBinding",name:"telegram-ai"},patch:([
          {op:"replace",path:"/spec/match/accountId",value:$account},
          {op:"replace",path:"/spec/match/contextId",value:$chat},
          {op:"replace",path:"/spec/senderPolicy/allowedSenderIds",value:[$sender]}
        ]|tojson)}
      ]')"
    source_dir="${REPO_ROOT}/deploy/routing"
    ;;
  tunnel)
    source_dir="${REPO_ROOT}/deploy/tunnel"
    ;;
esac

temp_dir="$(mktemp -d)"
trap 'rm -rf -- "${temp_dir}"' EXIT
cp -R "${source_dir}" "${temp_dir}/base"
jq -n --arg namespace "${NAMESPACE}" --argjson patches "${patches}" '
  {apiVersion:"kustomize.config.k8s.io/v1beta1",kind:"Kustomization",
   namespace:$namespace,resources:["base"],patches:$patches}
' >"${temp_dir}/kustomization.yaml"
"${KUBECTL}" kustomize "${temp_dir}"

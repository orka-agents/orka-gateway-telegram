#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
temp_dir="$(mktemp -d)"
trap 'rm -rf -- "${temp_dir}"' EXIT

export RENDER_TEST_KUBECTL
RENDER_TEST_KUBECTL="$(command -v "${KUBECTL:-kubectl}")"
cat >"${temp_dir}/kubectl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ "${1:-}" == kustomize ]] || { printf 'renderer attempted a cluster operation\n' >&2; exit 1; }
exec "${RENDER_TEST_KUBECTL}" "$@"
SH
chmod +x "${temp_dir}/kubectl"

unset IMAGE_REF KUBE_CONTEXT ADAPTER_URL STORAGE_CLASS
export KUBECTL="${temp_dir}/kubectl"
export NAMESPACE=render-test-v2
export ORKA_API_URL=http://orka-api.render-test-v2.svc.cluster.local:8080
export IMAGE=registry.example.com/team/telegram-adapter
export TAG=0123456789abcdef0123456789abcdef01234567
export AGENT_MODEL=test-codex-model
export TELEGRAM_ACCOUNT_ID=10001 TELEGRAM_CHAT_ID=10002 TELEGRAM_SENDER_ID=10002

"${SCRIPT_DIR}/render-manifests.sh" >"${temp_dir}/adapter.yaml"
export ADAPTER_URL=https://telegram.example.com/
STORAGE_CLASS=example-storage "${SCRIPT_DIR}/render-manifests.sh" >"${temp_dir}/configured-adapter.yaml"
"${SCRIPT_DIR}/render-manifests.sh" routing >"${temp_dir}/routing.yaml"
"${SCRIPT_DIR}/render-manifests.sh" tunnel >"${temp_dir}/tunnel.yaml"
for identity in 1 999999999999999999 9223372036854775807; do
  TELEGRAM_ACCOUNT_ID="${identity}" TELEGRAM_CHAT_ID="${identity}" TELEGRAM_SENDER_ID="${identity}" \
    "${SCRIPT_DIR}/render-manifests.sh" routing >"${temp_dir}/routing-${identity}.yaml"
done

python3 -c '
import os
from pathlib import Path
import sys
import yaml

directory = Path(sys.argv[1])
namespace = os.environ["NAMESPACE"]

def read(name):
    text = (directory / name).read_text()
    assert "REPLACE_WITH_" not in text, name
    documents = list(yaml.safe_load_all(text))
    for document in documents:
        assert document["kind"] not in {"Namespace", "AgentRuntime", "Secret"}, document["kind"]
        assert document["metadata"]["namespace"] == namespace, document
    return {document["kind"]: document for document in documents}

adapter = read("adapter.yaml")
assert set(adapter) == {"ServiceAccount", "ConfigMap", "Service", "PersistentVolumeClaim", "Deployment"}
deployment = adapter["Deployment"]["spec"]
assert deployment["replicas"] == 1
assert deployment["strategy"]["type"] == "Recreate"
assert deployment["template"]["spec"]["containers"][0]["image"] == os.environ["IMAGE"] + ":" + os.environ["TAG"]
assert adapter["ConfigMap"]["data"]["ORKA_GATEWAY_INGRESS_URL"] == os.environ["ORKA_API_URL"] + "/api/v1/gateways/" + namespace + "/telegram/events"
assert adapter["ConfigMap"]["data"]["TELEGRAM_WEBHOOK_URL"] == ""
assert "storageClassName" not in adapter["PersistentVolumeClaim"]["spec"]

configured = read("configured-adapter.yaml")
assert configured["ConfigMap"]["data"]["TELEGRAM_WEBHOOK_URL"] == "https://telegram.example.com/telegram/webhook"
assert configured["PersistentVolumeClaim"]["spec"]["storageClassName"] == "example-storage"

routing = read("routing.yaml")
assert set(routing) == {"Agent", "Gateway", "GatewayBinding"}
agent = routing["Agent"]["spec"]
assert agent["runtime"]["contractVersion"] == "orka.harness.v2"
assert agent["runtime"]["type"] == "codex"
assert "runtimeRef" not in agent["runtime"]
assert "secretRef" not in agent
assert agent["model"] == {"name": os.environ["AGENT_MODEL"]}
assert routing["Gateway"]["spec"]["adapter"]["endpoint"] == "https://telegram.example.com"
binding = routing["GatewayBinding"]["spec"]
assert binding["agentRef"]["name"] == routing["Agent"]["metadata"]["name"]
assert binding["gatewayRef"]["name"] == routing["Gateway"]["metadata"]["name"]
assert binding["match"] == {"accountId": "10001", "contextId": "10002"}
assert binding["senderPolicy"] == {"mode": "allowlist", "allowedSenderIds": ["10002"]}
assert binding["taskDefaults"]["retryPolicy"] == {"maxRetries": 0}

for identity in ("1", "999999999999999999", "9223372036854775807"):
    boundary = read("routing-" + identity + ".yaml")["GatewayBinding"]["spec"]
    assert boundary["match"] == {"accountId": identity, "contextId": identity}
    assert boundary["senderPolicy"]["allowedSenderIds"] == [identity]

tunnel = read("tunnel.yaml")
assert set(tunnel) == {"Deployment"}
assert tunnel["Deployment"]["spec"]["template"]["spec"]["containers"][0]["args"][-1] == "http://orka-gateway-telegram:8080"
' "${temp_dir}"

for api_url in \
  https://orka.example.com \
  https://orka.example.com:65535/ \
  https://192.0.2.1:8443 \
  'https://[2001:db8::1]:8443' \
  'https://[2001:db8:0:0:0:0:0:1]' \
  http://orka-api.render-test-v2.svc:8080 \
  http://ORKA-API.RENDER-TEST-V2.SVC.CLUSTER.LOCAL:8080/; do
  ORKA_API_URL="${api_url}" "${SCRIPT_DIR}/render-manifests.sh" >"${temp_dir}/supported-url.yaml"
done

expect_failure() {
  if "$@" >"${temp_dir}/unexpected-output" 2>"${temp_dir}/expected-error"; then
    printf 'expected command to fail: %s\n' "$1" >&2
    exit 1
  fi
  [[ ! -s "${temp_dir}/unexpected-output" ]] || {
    printf 'invalid input produced manifests\n' >&2
    exit 1
  }
}

expect_failure env NAMESPACE= "${SCRIPT_DIR}/render-manifests.sh"
expect_failure env NAMESPACE=invalid/namespace "${SCRIPT_DIR}/render-manifests.sh" tunnel
expect_failure env ORKA_API_URL=http://example.com "${SCRIPT_DIR}/render-manifests.sh"
expect_failure env ORKA_API_URL=http://10.0.0.1:8080 "${SCRIPT_DIR}/render-manifests.sh"
expect_failure env ORKA_API_URL=http://localhost:8080 "${SCRIPT_DIR}/render-manifests.sh"
expect_failure env ORKA_API_URL=http://example.com/path "${SCRIPT_DIR}/render-manifests.sh"
expect_failure env ORKA_API_URL=http://user@example.com "${SCRIPT_DIR}/render-manifests.sh"
for api_url in \
  https://api..example.com \
  https://api.-bad.example.com \
  https://api.bad-.example.com \
  https://orka.example.com:0 \
  https://orka.example.com:65536 \
  https://orka.example.com:99999 \
  https://orka.example.com: \
  https://999.0.0.1 \
  'https://[2001:db8::1]invalid' \
  'https://[2001:db8:::1]' \
  'https://[2001:db8:1]' \
  'https://[v1.invalid]'; do
  expect_failure env ORKA_API_URL="${api_url}" "${SCRIPT_DIR}/render-manifests.sh"
done
expect_failure env IMAGE=registry.example.com/team/ "${SCRIPT_DIR}/render-manifests.sh"
expect_failure env IMAGE='[::::]/team/adapter' "${SCRIPT_DIR}/render-manifests.sh"
expect_failure env IMAGE=registry.example.com:65536/team/adapter "${SCRIPT_DIR}/render-manifests.sh"
expect_failure env IMAGE_REF=registry.example.com/team/other:release "${SCRIPT_DIR}/render-manifests.sh"
expect_failure env AGENT_MODEL= "${SCRIPT_DIR}/render-manifests.sh" routing
expect_failure env ADAPTER_URL=http://example.com "${SCRIPT_DIR}/render-manifests.sh" routing
expect_failure env ADAPTER_URL=https://api..example.com "${SCRIPT_DIR}/render-manifests.sh" routing
expect_failure env ADAPTER_URL=https://telegram.example.com:65536 "${SCRIPT_DIR}/render-manifests.sh" routing
for variable in TELEGRAM_ACCOUNT_ID TELEGRAM_CHAT_ID TELEGRAM_SENDER_ID; do
  for identity in 0 -1 01 not-an-id 9223372036854775808 18446744073709551616 999999999999999999999999999999999999; do
    expect_failure env "${variable}=${identity}" "${SCRIPT_DIR}/render-manifests.sh" routing
  done
done

# A wrong or unclaimed namespace must stop both cluster helpers before they
# read credentials, patch resources, or start a port-forward.
cat >"${temp_dir}/namespace-kubectl" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
[[ $# == 7 && $1 == --context && $2 == test-context && $3 == get && $4 == namespace && $5 == render-test-v2 && $6 == -o ]] || {
  printf 'unexpected cluster operation\n' >>"${RENDER_TEST_CLUSTER_LOG}"
  exit 1
}
printf '%s' "${RENDER_TEST_NAMESPACE_MODE}"
SH
chmod +x "${temp_dir}/namespace-kubectl"
export RENDER_TEST_CLUSTER_LOG="${temp_dir}/cluster-operations"
for script in live-validate.sh reconcile-quick-tunnel.sh; do
  expect_failure env KUBE_CONTEXT= "${SCRIPT_DIR}/${script}"
  expect_failure env KUBE_CONTEXT=test-context NAMESPACE= "${SCRIPT_DIR}/${script}"
  for mode in '' harness-v1; do
    expect_failure env KUBE_CONTEXT=test-context KUBECTL="${temp_dir}/namespace-kubectl" \
      RENDER_TEST_NAMESPACE_MODE="${mode}" ORKA_GATEWAY_OUTBOUND_TOKEN_FILE= "${SCRIPT_DIR}/${script}"
  done
done
[[ ! -e "${RENDER_TEST_CLUSTER_LOG}" ]] || { printf 'namespace guard failed\n' >&2; exit 1; }

printf 'Manifest rendering and namespace guards passed.\n'

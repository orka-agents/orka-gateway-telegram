#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT
cd "${REPO_ROOT}"
unset IMAGE_REF
FIXTURE_DIR="$(mktemp -d)"
readonly FIXTURE_DIR
trap 'rm -rf "${FIXTURE_DIR}"' EXIT
export TOOL_MARKER="${FIXTURE_DIR}/called"
cat >"${FIXTURE_DIR}/forbidden-tool" <<'SH'
#!/usr/bin/env bash
touch "${TOOL_MARKER:?}"
exit 97
SH
chmod +x "${FIXTURE_DIR}/forbidden-tool"

for image in \
  ghcr.io/orka-agents/orka-gateway-telegram \
  docker.io/example/adapter \
  registry.example.com:5000/team/adapter \
  localhost:5000/team/adapter \
  '[::1]:5000/team/adapter' \
  example/adapter \
  registry.example.com/team/adapter__test--name; do
  IMAGE="${image}" TAG=abc123 ./scripts/validate-image.sh
done

for image in \
  '' adapter \
  /team/adapter \
  registry.example.com/team/ \
  registry.example.com/team//adapter \
  registry.example.com/Team/Adapter \
  registry.example.com/team/adapter:tag \
  registry.example.com/team/adapter@sha256:0123 \
  registry.example.com/team/adapter___name \
  registry.example.com/team/adapter..name \
  'registry.example.com/team/adapter|oops' \
  'registry.example.com/team/adapter&oops' \
  'registry.example.com/team/adapter name' \
  https://registry.example.com/team/adapter; do
  if IMAGE="${image}" TAG=abc123 ./scripts/validate-image.sh >/dev/null 2>&1; then
    printf 'Accepted invalid repository: %s\n' "${image}" >&2
    exit 1
  fi
done

for tag in '' dev latest abc-dirty .abc -abc 'a/b' 'a b'; do
  if IMAGE=ghcr.io/example/adapter TAG="${tag}" ./scripts/validate-image.sh >/dev/null 2>&1; then
    printf 'Accepted invalid release tag: %s\n' "${tag}" >&2
    exit 1
  fi
done

if IMAGE=ghcr.io/example/adapter TAG=abc123 IMAGE_REF=ghcr.io/example/other:latest \
  ./scripts/validate-image.sh >/dev/null 2>&1; then
  printf 'Accepted IMAGE_REF override\n' >&2
  exit 1
fi

# Missing configuration must stop before any Docker or Kubernetes command.
for target in deploy rollout-status live-validate; do
  if make --no-print-directory "${target}" KUBE_CONTEXT= NAMESPACE=example \
    IMAGE=ghcr.io/example/adapter TAG=abc123 \
    KUBECTL="${FIXTURE_DIR}/forbidden-tool" DOCKER="${FIXTURE_DIR}/forbidden-tool" >/dev/null 2>&1; then
    printf '%s accepted an empty Kubernetes context\n' "${target}" >&2
    exit 1
  fi
  if make --no-print-directory "${target}" KUBE_CONTEXT=example NAMESPACE= \
    IMAGE=ghcr.io/example/adapter TAG=abc123 \
    KUBECTL="${FIXTURE_DIR}/forbidden-tool" DOCKER="${FIXTURE_DIR}/forbidden-tool" >/dev/null 2>&1; then
    printf '%s accepted an empty namespace\n' "${target}" >&2
    exit 1
  fi
  if [[ -e "${TOOL_MARKER}" ]]; then
    printf '%s called an external tool before rejecting missing configuration\n' "${target}" >&2
    exit 1
  fi
done

if make --no-print-directory deploy KUBE_CONTEXT=example NAMESPACE=example \
  ORKA_API_URL= IMAGE=ghcr.io/example/adapter TAG=abc123 \
  KUBECTL="${FIXTURE_DIR}/forbidden-tool" >/dev/null 2>&1; then
  printf 'deploy accepted an empty Orka API URL\n' >&2
  exit 1
fi
if [[ -e "${TOOL_MARKER}" ]]; then
  printf 'deploy called an external tool before rejecting the missing Orka API URL\n' >&2
  exit 1
fi

# A renderer that emits partial output and then fails must never reach apply.
cat >"${FIXTURE_DIR}/failed-renderer" <<'SH'
#!/usr/bin/env bash
if [[ "${1:-}" == kustomize ]]; then
  printf 'apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: partial-render\n'
  exit 1
fi
touch "${TOOL_MARKER:?}"
exit 97
SH
chmod +x "${FIXTURE_DIR}/failed-renderer"
if make --no-print-directory deploy KUBE_CONTEXT=example NAMESPACE=example \
  ORKA_API_URL=http://orka-api.example.svc:8080 IMAGE=ghcr.io/example/adapter TAG=abc123 \
  KUBECTL="${FIXTURE_DIR}/failed-renderer" >/dev/null 2>&1; then
  printf 'deploy accepted a failed render\n' >&2
  exit 1
fi
if [[ -e "${TOOL_MARKER}" ]]; then
  printf 'deploy called the cluster after a failed render\n' >&2
  exit 1
fi

printf 'Build configuration checks passed\n'

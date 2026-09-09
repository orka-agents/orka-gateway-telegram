#!/usr/bin/env bash
set -euo pipefail

export LC_ALL=C

validate_repository() {
  local image="${IMAGE:-}"
  local component='[a-z0-9]+(([._]|__|-+)[a-z0-9]+)*'
  local host_component='([a-zA-Z0-9]|[a-zA-Z0-9][a-zA-Z0-9-]*[a-zA-Z0-9])'
  local host="(${host_component}(\.${host_component})*|\[[a-fA-F0-9:]+\])(:[0-9]+)?"
  local repository="^(${host}/)?${component}(/${component})*$"

  # Repository components follow distribution/reference's name grammar.
  # Require a namespace or registry prefix for publishing and deployment.
  if [[ -z "${image}" || ${#image} -gt 255 || "${image}" != */* || ! "${image}" =~ ${repository} ]]; then
    printf 'IMAGE must be a registry or namespaced repository without a tag or digest\n' >&2
    return 2
  fi
}

validate_tag() {
  local tag="${TAG:-}"
  if [[ -z "${tag}" || ${#tag} -gt 128 || ! "${tag}" =~ ^[a-zA-Z0-9_][a-zA-Z0-9_.-]*$ ]]; then
    printf 'TAG must be a valid container image tag\n' >&2
    return 2
  fi
  case "${tag}" in
    dev|latest|*dirty*)
      printf 'TAG must identify a clean release, not a development build\n' >&2
      return 2
      ;;
  esac
}

if [[ -n "${IMAGE_REF:-}" ]]; then
  printf 'IMAGE_REF is unsupported; set IMAGE and TAG separately\n' >&2
  exit 2
fi

case "${1:-release}" in
  repository) validate_repository ;;
  tag) validate_tag ;;
  release) validate_repository; validate_tag ;;
  *) printf 'Usage: validate-image.sh [repository|tag|release]\n' >&2; exit 2 ;;
esac

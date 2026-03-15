#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WORKSPACE_DIR="${ACPNET_E2E_WORKSPACE:-$ROOT_DIR}"
SERVER_BIN="${ACPNET_E2E_SERVER_BIN:-$(command -v acpnet || true)}"
REPO_OWNER="${ACPNET_E2E_REPO_OWNER:-zhangweiii}"
REPO_NAME="${ACPNET_E2E_REPO_NAME:-acpnet}"
CONTAINER_IMAGE="${ACPNET_E2E_IMAGE:-node:20-bookworm-slim}"
RUN_CONTAINER=0
RUN_CODEX=1
RUN_CLAUDE=1
KEEP_TMP=0

log() {
  printf '[acpnet-e2e] %s\n' "$*"
}

die() {
  printf '[acpnet-e2e] ERROR: %s\n' "$*" >&2
  exit 1
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

check_json_result() {
  local file="$1"
  local expected="$2"
  python3 - "$file" "$expected" <<'PY'
import json
import sys

path = sys.argv[1]
expected = sys.argv[2]
chunks = []
end_turn = False

with open(path, "r", encoding="utf-8") as handle:
    for raw in handle:
        raw = raw.strip()
        if not raw:
            continue
        msg = json.loads(raw)
        params = msg.get("params", {})
        update = params.get("update", {})
        content = update.get("content", {})
        if update.get("sessionUpdate") == "agent_message_chunk":
            text = content.get("text")
            if isinstance(text, str):
                chunks.append(text)
        result = msg.get("result", {})
        if result.get("stopReason") == "end_turn":
            end_turn = True

message = "".join(chunks)
if expected not in message:
    print(f"expected text not found: {expected}", file=sys.stderr)
    print(f"assembled message: {message}", file=sys.stderr)
    sys.exit(1)
if not end_turn:
    print("missing end_turn", file=sys.stderr)
    sys.exit(1)
PY
}

cleanup() {
  if [[ -n "${SERVER_PID:-}" ]]; then
    kill "${SERVER_PID}" >/dev/null 2>&1 || true
    wait "${SERVER_PID}" 2>/dev/null || true
  fi
  if [[ -n "${TMP_DIR:-}" && "${KEEP_TMP}" -ne 1 ]]; then
    rm -rf "${TMP_DIR}"
  fi
}

trap cleanup EXIT

while [[ $# -gt 0 ]]; do
  case "$1" in
    --container)
      RUN_CONTAINER=1
      ;;
    --skip-codex)
      RUN_CODEX=0
      ;;
    --skip-claude)
      RUN_CLAUDE=0
      ;;
    --keep-tmp)
      KEEP_TMP=1
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
  shift
done

[[ -n "${SERVER_BIN}" ]] || die "acpnet is not installed or not in PATH"
need_cmd curl
need_cmd tar
need_cmd npx
need_cmd python3

if [[ "${RUN_CONTAINER}" -eq 1 ]]; then
  need_cmd docker
fi

if [[ "${RUN_CODEX}" -eq 1 ]]; then
  need_cmd codex
fi

if [[ "${RUN_CLAUDE}" -eq 1 ]]; then
  need_cmd claude
fi

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/acpnet-e2e.XXXXXX")"
TOKEN="acpnet-e2e-$(date +%s)"
TCP_PORT=4631
HTTP_PORT=4633
SERVER_LOG="${TMP_DIR}/server.log"

VERSION="$("${SERVER_BIN}" version | awk '{print $NF}')"
[[ -n "${VERSION}" ]] || die "failed to detect installed acpnet version"
log "using ${SERVER_BIN} ${VERSION}"

HOST_ARCH="$(uname -m)"
case "${HOST_ARCH}" in
  arm64|aarch64)
    LINUX_ARCH=arm64
    ;;
  x86_64|amd64)
    LINUX_ARCH=amd64
    ;;
  *)
    die "unsupported host arch: ${HOST_ARCH}"
    ;;
esac

RELEASE_TARBALL="acpnet_linux_${LINUX_ARCH}.tar.gz"
RELEASE_URL="https://github.com/${REPO_OWNER}/${REPO_NAME}/releases/download/v${VERSION}/${RELEASE_TARBALL}"

log "downloading ${RELEASE_URL}"
curl -fsSL "${RELEASE_URL}" -o "${TMP_DIR}/${RELEASE_TARBALL}"
tar -xzf "${TMP_DIR}/${RELEASE_TARBALL}" -C "${TMP_DIR}"
CLIENT_BIN="${TMP_DIR}/acpnet"
[[ -x "${CLIENT_BIN}" ]] || die "release client binary not found after extraction"

log "starting brew-installed acpnet serve"
"${SERVER_BIN}" serve \
  --listen "127.0.0.1:${TCP_PORT}" \
  --http-listen "127.0.0.1:${HTTP_PORT}" \
  --http-path /v1/connect \
  --token "${TOKEN}" \
  --map /workspace="${WORKSPACE_DIR}" \
  >"${SERVER_LOG}" 2>&1 &
SERVER_PID=$!

for _ in {1..20}; do
  if curl -fsS "http://127.0.0.1:${HTTP_PORT}/healthz" >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
curl -fsS "http://127.0.0.1:${HTTP_PORT}/healthz" >/dev/null || die "server health check failed"

run_local_acpx() {
  local agent="$1"
  local transport="$2"
  local expected="$3"
  local home_dir="${TMP_DIR}/${agent}-${transport}-home"
  local server

  mkdir -p "${home_dir}/.acpx"
  if [[ "${transport}" == "raw" ]]; then
    server="tcp://127.0.0.1:${TCP_PORT}"
  else
    server="http://127.0.0.1:${HTTP_PORT}/v1/connect"
  fi

  cat > "${home_dir}/.acpx/config.json" <<EOF
{
  "agents": {
    "${agent}": {
      "command": "${SERVER_BIN} client --server ${server} --token ${TOKEN} --agent ${agent}"
    }
  }
}
EOF

  log "verifying local acpx ${agent} over ${transport}"
  HOME="${home_dir}" npx -y acpx@0.3.0 --format json --json-strict "${agent}" exec "Reply with exactly ${expected}" > "${home_dir}/result.jsonl"
  check_json_result "${home_dir}/result.jsonl" "${expected}"
}

if [[ "${RUN_CODEX}" -eq 1 ]]; then
  run_local_acpx codex raw BREW_LOCAL_CODEX_RAW_OK
  run_local_acpx codex http BREW_LOCAL_CODEX_HTTP_OK
fi

if [[ "${RUN_CLAUDE}" -eq 1 ]]; then
  run_local_acpx claude raw BREW_LOCAL_CLAUDE_RAW_OK
  run_local_acpx claude http BREW_LOCAL_CLAUDE_HTTP_OK
fi

run_container_acpx() {
  local agent="$1"
  local transport="$2"
  local expected="$3"
  local test_dir="${TMP_DIR}/container-${agent}-${transport}"
  local server

  mkdir -p "${test_dir}"
  if [[ "${transport}" == "raw" ]]; then
    server="tcp://host.docker.internal:${TCP_PORT}"
  else
    server="http://host.docker.internal:${HTTP_PORT}/v1/connect"
  fi

  cat > "${test_dir}/config.json" <<EOF
{
  "agents": {
    "${agent}": {
      "command": "/opt/acpnet/acpnet client --server ${server} --token ${TOKEN} --agent ${agent}"
    }
  }
}
EOF

  log "verifying container acpx ${agent} over ${transport}"
  docker run --rm \
    --entrypoint sh \
    -v "${WORKSPACE_DIR}:/workspace" \
    -v "${TMP_DIR}:/opt/acpnet" \
    -v "${test_dir}/config.json:/root/.acpx/config.json:ro" \
    -w /workspace \
    "${CONTAINER_IMAGE}" \
    -lc "npx -y acpx@0.3.0 --format json --json-strict ${agent} exec 'Reply with exactly ${expected}'" \
    > "${test_dir}/result.jsonl"

  check_json_result "${test_dir}/result.jsonl" "${expected}"
}

if [[ "${RUN_CONTAINER}" -eq 1 && "${RUN_CODEX}" -eq 1 ]]; then
  run_container_acpx codex raw BREW_CONTAINER_CODEX_RAW_OK
  run_container_acpx codex http BREW_CONTAINER_CODEX_HTTP_OK
fi

if [[ "${RUN_CONTAINER}" -eq 1 && "${RUN_CLAUDE}" -eq 1 ]]; then
  run_container_acpx claude raw BREW_CONTAINER_CLAUDE_RAW_OK
  run_container_acpx claude http BREW_CONTAINER_CLAUDE_HTTP_OK
fi

log "all requested checks passed"
log "server log: ${SERVER_LOG}"

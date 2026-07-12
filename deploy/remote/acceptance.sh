#!/bin/sh
set -eu

env_file="deploy/remote/.env"
base_url=""
idle_seconds=0
restart_check=0
require_logins=0

usage() {
  cat <<'EOF'
Usage: deploy/remote/acceptance.sh [options]

Options:
  --env-file PATH       Compose environment file (default: deploy/remote/.env)
  --base-url URL        Published or TLS URL to probe (default: local host port)
  --idle-seconds N      Keep the authenticated WebSocket open for N seconds
  --require-logins      Fail unless Codex, Claude, and GitHub are authenticated
  --restart-check       Restart Synara and verify readiness and home persistence
  -h, --help            Show this help

Run --restart-check only after active agent turns and terminals have drained.
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --env-file)
      [ "$#" -ge 2 ] || { echo "acceptance: --env-file requires a path" >&2; exit 2; }
      env_file="$2"
      shift 2
      ;;
    --base-url)
      [ "$#" -ge 2 ] || { echo "acceptance: --base-url requires a URL" >&2; exit 2; }
      base_url="$2"
      shift 2
      ;;
    --idle-seconds)
      [ "$#" -ge 2 ] || { echo "acceptance: --idle-seconds requires a value" >&2; exit 2; }
      idle_seconds="$2"
      shift 2
      ;;
    --require-logins)
      require_logins=1
      shift
      ;;
    --restart-check)
      restart_check=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "acceptance: unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

case "$idle_seconds" in
  ''|*[!0-9]*)
    echo "acceptance: --idle-seconds must be a non-negative integer" >&2
    exit 2
    ;;
esac

[ -f "$env_file" ] || {
  echo "acceptance: environment file not found: $env_file" >&2
  exit 2
}

command -v docker >/dev/null 2>&1 || {
  echo "acceptance: docker is required" >&2
  exit 2
}
command -v curl >/dev/null 2>&1 || {
  echo "acceptance: curl is required" >&2
  exit 2
}

compose() {
  docker compose --env-file "$env_file" -f deploy/remote/docker-compose.yml "$@"
}

pass() {
  printf 'PASS  %s\n' "$1"
}

warn() {
  printf 'WARN  %s\n' "$1"
}

compose config --quiet
container_id="$(compose ps -q synara)"
[ -n "$container_id" ] || {
  echo "acceptance: Synara container is not running" >&2
  exit 1
}
[ "$(docker inspect --format '{{.State.Running}}' "$container_id")" = "true" ] || {
  echo "acceptance: Synara container is not in the running state" >&2
  exit 1
}
pass "Compose service is running"

if [ -z "$base_url" ]; then
  host_port="$(docker inspect --format '{{(index (index .NetworkSettings.Ports "3773/tcp") 0).HostPort}}' "$container_id")"
  base_url="http://127.0.0.1:${host_port}"
fi
base_url="${base_url%/}"

deploy/remote/smoke.sh "$base_url"
pass "HTTP liveness and readiness"

unauthenticated_status="$(curl --silent --output /dev/null --write-out '%{http_code}' "${base_url}/ws")"
[ "$unauthenticated_status" = "401" ] || {
  echo "acceptance: unauthenticated /ws returned HTTP ${unauthenticated_status}, expected 401" >&2
  exit 1
}
pass "Unauthenticated WebSocket request is rejected"

websocket_probe_url="$base_url"
case "$base_url" in
  http://127.0.0.1:*|http://localhost:*|http://\[::1\]:*)
    # The WebSocket client runs inside the Synara container. Host loopback
    # publishing is not reachable there through the host-side mapped port.
    websocket_probe_url="http://127.0.0.1:3773"
    ;;
esac

compose exec -T \
  -e SYNARA_ACCEPTANCE_BASE_URL="$websocket_probe_url" \
  -e SYNARA_ACCEPTANCE_IDLE_SECONDS="$idle_seconds" \
  synara node <<'NODE'
const token = process.env.SYNARA_AUTH_TOKEN;
if (!token) throw new Error("SYNARA_AUTH_TOKEN is not available in the container");

const endpoint = new URL(process.env.SYNARA_ACCEPTANCE_BASE_URL);
endpoint.protocol = endpoint.protocol === "https:" ? "wss:" : "ws:";
endpoint.pathname = "/ws";
endpoint.search = "";
endpoint.hash = "";
endpoint.searchParams.set("token", token);

const holdSeconds = Number(process.env.SYNARA_ACCEPTANCE_IDLE_SECONDS ?? "0");
const timeoutMs = Math.max(15_000, holdSeconds * 1_000 + 15_000);

await new Promise((resolve, reject) => {
  const socket = new WebSocket(endpoint);
  let opened = false;
  let closeTimer;
  const timeout = setTimeout(() => {
    socket.close();
    reject(new Error(`WebSocket probe timed out after ${timeoutMs}ms`));
  }, timeoutMs);

  socket.addEventListener("open", () => {
    opened = true;
    closeTimer = setTimeout(() => socket.close(1000, "acceptance complete"), holdSeconds * 1_000);
  });
  socket.addEventListener("close", () => {
    clearTimeout(timeout);
    clearTimeout(closeTimer);
    if (opened) resolve();
    else reject(new Error("WebSocket closed before authentication completed"));
  });
  socket.addEventListener("error", () => {
    if (!opened) {
      clearTimeout(timeout);
      clearTimeout(closeTimer);
      reject(new Error("Authenticated WebSocket failed to open"));
    }
  });
});
NODE
pass "Authenticated WebSocket opens and remains stable for ${idle_seconds}s"

compose exec -T synara sh -lc '
  set -eu
  test "$(id -u)" != "0"
  test -w /home/synara/.synara
  test -w /workspace
  mkdir -p /home/synara/.synara/remote-acceptance
  printf "%s\n" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" > /home/synara/.synara/remote-acceptance/persistence-marker
  workspace_probe=/workspace/.synara-remote-acceptance-write
  trap '\''rm -f "$workspace_probe"'\'' EXIT HUP INT TERM
  printf "ok\n" > "$workspace_probe"
  test "$(cat "$workspace_probe")" = "ok"
'
pass "Non-root home and workspace writes"

compose exec -T synara sh -lc '
  set -eu
  codex --version
  claude --version
  gh --version | sed -n "1p"
  git --version
  rg --version | sed -n "1p"
  jq --version
'
pass "Remote agent toolchain"

compose exec -T synara sh -lc '
  cd /app/apps/server
  node --input-type=module <<'\''NODE'\''
import * as pty from "node-pty";

await new Promise((resolve, reject) => {
  const child = pty.spawn("/bin/sh", ["-lc", "printf SYNARA_PTY_OK"], {
    name: "xterm-256color",
    cols: 80,
    rows: 24,
    cwd: "/workspace",
    env: process.env,
  });
  let output = "";
  const timeout = setTimeout(() => {
    child.kill();
    reject(new Error("PTY probe timed out"));
  }, 5_000);
  child.onData((chunk) => {
    output += chunk;
  });
  child.onExit(({ exitCode }) => {
    clearTimeout(timeout);
    if (exitCode === 0 && output.includes("SYNARA_PTY_OK")) resolve();
    else reject(new Error(`PTY probe failed: exit=${exitCode} output=${JSON.stringify(output)}`));
  });
});
NODE
'
pass "node-pty can spawn and collect a real shell"

login_failures=0
check_login() {
  label="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    pass "${label} login is valid"
  else
    warn "${label} is not logged in"
    if [ "$require_logins" -eq 1 ]; then
      login_failures=$((login_failures + 1))
    fi
  fi
}

check_login "Codex" compose exec -T synara codex login status
check_login "Claude" compose exec -T synara claude auth status
check_login "GitHub CLI" compose exec -T synara gh auth status --active --hostname github.com

if [ "$restart_check" -eq 1 ]; then
  echo "INFO  Restarting Synara for the explicit persistence check"
  compose restart synara >/dev/null

  ready=0
  attempt=0
  while [ "$attempt" -lt 60 ]; do
    if curl --fail --silent --show-error "${base_url}/ready" >/dev/null 2>&1; then
      ready=1
      break
    fi
    attempt=$((attempt + 1))
    sleep 2
  done
  [ "$ready" -eq 1 ] || {
    echo "acceptance: Synara did not become ready within 120 seconds after restart" >&2
    exit 1
  }
  compose exec -T synara test -s /home/synara/.synara/remote-acceptance/persistence-marker
  pass "Controlled restart preserves home state and returns to ready"
fi

if [ "$login_failures" -gt 0 ]; then
  echo "acceptance: ${login_failures} required provider login check(s) failed" >&2
  exit 1
fi

echo "acceptance: all requested automated checks passed for ${base_url}"

#!/bin/sh
set -eu

base_url="${1:-http://127.0.0.1:${SYNARA_HOST_PORT:-3773}}"
base_url="${base_url%/}"

json_value() {
  python_expression="$1"
  node_expression="$2"
  if command -v python3 >/dev/null 2>&1; then
    python3 -c "import json,sys; data=json.load(sys.stdin); assert ${python_expression}"
    return
  fi
  if command -v node >/dev/null 2>&1; then
    node -e "const data=JSON.parse(require('fs').readFileSync(0,'utf8')); if (!(${node_expression})) process.exit(1)"
    return
  fi
  echo "smoke: Python 3 or Node.js is required to validate JSON (jq is not required)." >&2
  exit 2
}

health="$(curl --fail --silent --show-error "${base_url}/health")"
printf '%s' "${health}" | json_value \
  'data.get("status") == "ok"' \
  'data.status === "ok"'

ready="$(curl --fail --silent --show-error "${base_url}/ready")"
printf '%s' "${ready}" | json_value \
  'data.get("status") == "ready" and data.get("startupReady") is True' \
  'data.status === "ready" && data.startupReady === true'

echo "smoke: ${base_url} is live and ready"

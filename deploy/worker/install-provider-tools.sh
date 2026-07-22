#!/bin/sh

set -eu

attempt=1
max_attempts=3

while :; do
  rm -rf node_modules

  if npm ci --omit=dev --include=optional --ignore-scripts --no-audit --no-fund \
    && node node_modules/@anthropic-ai/claude-code/install.cjs \
    && ./node_modules/.bin/codex --version \
    && ./node_modules/.bin/claude --version; then
    npm cache clean --force
    exit 0
  fi

  npm cache clean --force >/dev/null 2>&1 || true

  if [ "${attempt}" -ge "${max_attempts}" ]; then
    echo "provider-tools install failed after ${attempt} attempts" >&2
    exit 1
  fi

  echo "provider-tools install attempt ${attempt} failed; retrying" >&2
  attempt=$((attempt + 1))
done

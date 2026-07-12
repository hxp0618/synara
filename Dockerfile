# syntax=docker/dockerfile:1.7

FROM oven/bun:1.3.14 AS bun

FROM node:24-bookworm AS runtime-base

ARG TARGETARCH
ARG GH_VERSION=2.96.0
ARG JQ_VERSION=1.8.1
ARG RIPGREP_VERSION=14.1.1
ARG TINI_VERSION=0.19.0
ARG GITHUB_DOWNLOAD_PREFIX=
ARG SYNARA_UID=1000
ARG SYNARA_GID=1000

COPY --from=bun /usr/local/bin/bun /usr/local/bin/bun

RUN set -eux; \
  case "${TARGETARCH}" in \
    amd64) gh_arch="amd64"; jq_arch="amd64"; rg_target="x86_64-unknown-linux-musl"; tini_arch="amd64" ;; \
    arm64) gh_arch="arm64"; jq_arch="arm64"; rg_target="aarch64-unknown-linux-gnu"; tini_arch="arm64" ;; \
    *) echo "Unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
  esac; \
  curl --fail --location --retry 5 --retry-all-errors --connect-timeout 15 \
    "${GITHUB_DOWNLOAD_PREFIX}https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${gh_arch}.tar.gz" \
    --output /tmp/gh.tar.gz; \
  tar -xzf /tmp/gh.tar.gz -C /tmp; \
  install -m 0755 "/tmp/gh_${GH_VERSION}_linux_${gh_arch}/bin/gh" /usr/local/bin/gh; \
  curl --fail --location --retry 5 --retry-all-errors --connect-timeout 15 \
    "${GITHUB_DOWNLOAD_PREFIX}https://github.com/jqlang/jq/releases/download/jq-${JQ_VERSION}/jq-linux-${jq_arch}" \
    --output /usr/local/bin/jq; \
  chmod 0755 /usr/local/bin/jq; \
  curl --fail --location --retry 5 --retry-all-errors --connect-timeout 15 \
    "${GITHUB_DOWNLOAD_PREFIX}https://github.com/BurntSushi/ripgrep/releases/download/${RIPGREP_VERSION}/ripgrep-${RIPGREP_VERSION}-${rg_target}.tar.gz" \
    --output /tmp/ripgrep.tar.gz; \
  tar -xzf /tmp/ripgrep.tar.gz -C /tmp; \
  install -m 0755 "/tmp/ripgrep-${RIPGREP_VERSION}-${rg_target}/rg" /usr/local/bin/rg; \
  curl --fail --location --retry 5 --retry-all-errors --connect-timeout 15 \
    "${GITHUB_DOWNLOAD_PREFIX}https://github.com/krallin/tini/releases/download/v${TINI_VERSION}/tini-${tini_arch}" \
    --output /usr/local/bin/tini; \
  chmod 0755 /usr/local/bin/tini; \
  rm -rf /tmp/gh.tar.gz /tmp/gh_* /tmp/ripgrep.tar.gz /tmp/ripgrep-*; \
  npm install --global \
    @openai/codex@0.144.1 \
    @anthropic-ai/claude-code@2.1.197; \
  npm cache clean --force

# npm 11 leaves dependency lifecycle scripts pending by default. Run Claude's
# documented postinstall explicitly as root so its native launcher is present.
RUN node /usr/local/lib/node_modules/@anthropic-ai/claude-code/install.cjs

# Bun invokes `node-gyp` directly for native packages such as node-pty. The
# official Node image ships it inside npm but does not expose a PATH shim.
RUN ln -s /usr/local/lib/node_modules/npm/node_modules/node-gyp/bin/node-gyp.js \
  /usr/local/bin/node-gyp

# Reuse the Node headers already present in the base image. Otherwise node-gyp
# downloads them again, which makes remote/container builds slow and brittle.
ENV npm_config_nodedir=/usr/local

# The official Node image already reserves uid/gid 1000 for the `node` user.
# Reuse that account at the defaults, while still allowing operators to match a
# different host uid/gid for bind-mounted workspaces.
RUN set -eux; \
  existing_group="$(getent group "${SYNARA_GID}" | cut -d: -f1 || true)"; \
  if [ -n "${existing_group}" ]; then \
    groupmod --new-name synara "${existing_group}"; \
  else \
    groupadd --gid "${SYNARA_GID}" synara; \
  fi; \
  existing_user="$(getent passwd "${SYNARA_UID}" | cut -d: -f1 || true)"; \
  if [ -n "${existing_user}" ]; then \
    usermod --login synara --home /home/synara --move-home \
      --gid "${SYNARA_GID}" --shell /bin/bash "${existing_user}"; \
  else \
    useradd --uid "${SYNARA_UID}" --gid "${SYNARA_GID}" \
      --create-home --home-dir /home/synara --shell /bin/bash synara; \
  fi; \
  mkdir -p /workspace /home/synara/.synara; \
  chown -R "${SYNARA_UID}:${SYNARA_GID}" /workspace /home/synara

FROM runtime-base AS build

WORKDIR /app
ENV ELECTRON_SKIP_BINARY_DOWNLOAD=1 \
  PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1
COPY package.json bun.lock bunfig.toml turbo.json tsconfig.base.json ./
COPY apps/desktop/package.json ./apps/desktop/package.json
COPY apps/marketing/package.json ./apps/marketing/package.json
COPY apps/provider-host/package.json ./apps/provider-host/package.json
COPY apps/server/package.json ./apps/server/package.json
COPY apps/web/package.json ./apps/web/package.json
COPY packages/contracts/package.json ./packages/contracts/package.json
COPY packages/effect-acp/package.json ./packages/effect-acp/package.json
COPY packages/shared/package.json ./packages/shared/package.json
COPY scripts/package.json ./scripts/package.json
RUN --mount=type=cache,target=/root/.bun/install/cache \
  bun install --frozen-lockfile

COPY . .
RUN bun run --cwd apps/web build \
  && bun run --cwd apps/server build
RUN --mount=type=cache,target=/root/.bun/install/cache \
  bun scripts/prepare-server-runtime-package.ts /runtime \
  && cd /runtime \
  && bun install --production

FROM runtime-base AS runtime

WORKDIR /app

# Install only server production dependencies in the runtime image. The full
# monorepo dependency tree contains browser/build tooling and is several GB.
COPY --from=build /runtime/node_modules ./node_modules
COPY --from=build /runtime/apps/server/node_modules ./apps/server/node_modules
COPY --from=build /runtime/apps/server/package.json ./apps/server/package.json
COPY --from=build /app/apps/server/dist ./apps/server/dist
COPY --from=build /runtime/package.json ./package.json

ENV HOME=/home/synara \
  PATH=/home/synara/.local/bin:${PATH} \
  NPM_CONFIG_UPDATE_NOTIFIER=false

WORKDIR /workspace
USER synara

EXPOSE 3773

ENTRYPOINT ["/usr/local/bin/tini", "--"]
CMD ["node", "/app/apps/server/dist/index.mjs"]

FROM golang:1.26-bookworm AS agentd-build

ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src
COPY services/control-plane/go.mod services/control-plane/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY services/control-plane .
RUN --mount=type=cache,target=/go/pkg/mod \
  --mount=type=cache,target=/root/.cache/go-build \
  CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/synara-agentd ./cmd/agentd

FROM oven/bun:1.3.14 AS provider-host-build

WORKDIR /src
COPY apps/provider-host/src ./src
RUN bun build src/index.ts --target=node --outfile=/out/provider-host.mjs

FROM node:24-alpine AS worker-runtime-base

RUN apk add --no-cache bash ca-certificates git jq openssh-client ripgrep

RUN npm install --global @openai/codex@0.144.1 @anthropic-ai/claude-code@2.1.197 && \
  node /usr/local/lib/node_modules/@anthropic-ai/claude-code/install.cjs && \
  npm cache clean --force

RUN addgroup -g 10001 -S synara-worker && \
  adduser -u 10001 -S -D -h /home/synara -s /bin/bash -G synara-worker synara-worker && \
  mkdir -p /data/workspaces && chown -R 10001:10001 /data /home/synara

FROM worker-runtime-base AS worker

COPY --from=agentd-build /out/synara-agentd /usr/local/bin/synara-agentd
COPY --from=provider-host-build /out/provider-host.mjs /opt/synara/provider-host/index.mjs
RUN printf '%s\n' '#!/bin/sh' 'exec node /opt/synara/provider-host/index.mjs "$@"' \
  > /usr/local/bin/provider-host && chmod 0755 /usr/local/bin/provider-host

ENV HOME=/home/synara \
  PATH=/home/synara/.local/bin:${PATH} \
  SYNARA_AGENTD_WORKSPACE_ROOT=/data/workspaces \
  NPM_CONFIG_UPDATE_NOTIFIER=false

WORKDIR /data
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/synara-agentd"]

# Preserve the Synara web/server runtime as the default image. Worker images are
# selected explicitly with `--target worker`.
FROM runtime AS default

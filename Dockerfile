# syntax=docker/dockerfile:1.7@sha256:a57df69d0ea827fb7266491f2813635de6f17269be881f696fbfdf2d83dda33e

ARG BUN_IMAGE=oven/bun:1.3.14@sha256:e10577f0db68676a7024391c6e5cb4b879ebd17188ab750cf10024a6d700e5c4
ARG SERVER_RUNTIME_IMAGE=node:24-bookworm@sha256:d5adb040f90e206d1dc91453d08a4fa4165ec0faebd62a3421e6181a14e7f41f
ARG AGENTD_BUILD_IMAGE=golang:1.26-bookworm@sha256:e60d708a92ad26a6d61901334510d3debd23ddcba125663ecd6008d42e8ec669
ARG WORKER_RUNTIME_IMAGE=node:24-alpine@sha256:a0b9bf06e4e6193cf7a0f58816cc935ff8c2a908f81e6f1a95432d679c54fbfd

FROM ${BUN_IMAGE} AS bun

FROM ${SERVER_RUNTIME_IMAGE} AS provider-tools-bookworm

WORKDIR /opt/synara/provider-tools
COPY deploy/worker/provider-tools/package.json deploy/worker/provider-tools/package-lock.json ./
RUN npm ci --omit=dev --ignore-scripts --no-audit --no-fund \
  && node node_modules/@anthropic-ai/claude-code/install.cjs \
  && npm cache clean --force

FROM ${SERVER_RUNTIME_IMAGE} AS runtime-base

ARG TARGETARCH
ARG GH_VERSION=2.96.0
ARG JQ_VERSION=1.8.1
ARG RIPGREP_VERSION=14.1.1
ARG TINI_VERSION=0.19.0
ARG GITHUB_DOWNLOAD_PREFIX=
ARG SYNARA_UID=1000
ARG SYNARA_GID=1000

COPY --from=bun /usr/local/bin/bun /usr/local/bin/bun
COPY --from=provider-tools-bookworm /opt/synara/provider-tools /opt/synara/provider-tools

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
  rm -rf /tmp/gh.tar.gz /tmp/gh_* /tmp/ripgrep.tar.gz /tmp/ripgrep-*

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
COPY patches ./patches
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
  PATH=/opt/synara/provider-tools/node_modules/.bin:/home/synara/.local/bin:${PATH} \
  NPM_CONFIG_UPDATE_NOTIFIER=false

WORKDIR /workspace
USER synara

EXPOSE 3773

ENTRYPOINT ["/usr/local/bin/tini", "--"]
CMD ["node", "/app/apps/server/dist/index.mjs"]

FROM ${AGENTD_BUILD_IMAGE} AS agentd-build

ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src
COPY services/control-plane/go.mod services/control-plane/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY services/control-plane .
RUN --mount=type=cache,target=/go/pkg/mod \
  --mount=type=cache,target=/root/.cache/go-build \
  CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/synara-agentd ./cmd/agentd

FROM ${BUN_IMAGE} AS provider-host-build

WORKDIR /src
COPY package.json bun.lock bunfig.toml ./
COPY apps/desktop/package.json ./apps/desktop/package.json
COPY apps/marketing/package.json ./apps/marketing/package.json
COPY apps/provider-host/package.json ./apps/provider-host/package.json
COPY apps/server/package.json ./apps/server/package.json
COPY apps/web/package.json ./apps/web/package.json
COPY packages/contracts/package.json ./packages/contracts/package.json
COPY packages/effect-acp/package.json ./packages/effect-acp/package.json
COPY packages/shared/package.json ./packages/shared/package.json
COPY scripts/package.json ./scripts/package.json
COPY patches ./patches
RUN --mount=type=cache,target=/root/.bun/install/cache \
  bun install --frozen-lockfile --filter @synara/provider-host
COPY apps/provider-host/src ./apps/provider-host/src
COPY packages/contracts/src ./packages/contracts/src
RUN bun build apps/provider-host/src/index.ts --target=node --outfile=/out/provider-host.mjs

FROM provider-host-build AS provider-host-fixture-build

RUN --mount=type=cache,target=/root/.bun/install/cache \
  bun install --frozen-lockfile --filter @synara/scripts
COPY scripts/stage3-provider-acceptance/provider-host-fixture.ts ./scripts/stage3-provider-acceptance/provider-host-fixture.ts
RUN bun build scripts/stage3-provider-acceptance/provider-host-fixture.ts \
  --target=node \
  --outfile=/out/provider-host-fixture.mjs

FROM ${WORKER_RUNTIME_IMAGE} AS worker-provider-tools

WORKDIR /opt/synara/provider-tools
COPY deploy/worker/provider-tools/package.json deploy/worker/provider-tools/package-lock.json ./
RUN npm ci --omit=dev --ignore-scripts --no-audit --no-fund \
  && node node_modules/@anthropic-ai/claude-code/install.cjs \
  && npm sbom --omit=dev --sbom-format spdx > /tmp/provider-tools.raw.spdx.json \
  && npm cache clean --force

FROM ${WORKER_RUNTIME_IMAGE} AS worker-runtime-base

ARG TARGETARCH
ARG SYNARA_VERSION
ARG SYNARA_GIT_SHA
ARG SOURCE_DATE_EPOCH=0
ARG BUN_IMAGE
ARG AGENTD_BUILD_IMAGE
ARG WORKER_RUNTIME_IMAGE

COPY deploy/worker/apk-packages.lock /opt/synara/worker-apk-packages.lock
RUN xargs apk add --no-cache < /opt/synara/worker-apk-packages.lock

COPY --from=worker-provider-tools /opt/synara/provider-tools /opt/synara/provider-tools
COPY --from=worker-provider-tools /tmp/provider-tools.raw.spdx.json /tmp/provider-tools.raw.spdx.json
COPY bun.lock /opt/synara/provider-host/bun.lock
COPY apps/provider-host/package.json /opt/synara/provider-host/package.json
COPY deploy/worker/worker-image-manifest.mjs /opt/synara/build/worker-image-manifest.mjs
RUN node /opt/synara/build/worker-image-manifest.mjs \
    --version "${SYNARA_VERSION}" \
    --git-sha "${SYNARA_GIT_SHA}" \
    --source-date-epoch "${SOURCE_DATE_EPOCH}" \
    --architecture "${TARGETARCH}" \
    --base-image "agentd-build=${AGENTD_BUILD_IMAGE}" \
    --base-image "provider-host-build=${BUN_IMAGE}" \
    --base-image "worker-runtime=${WORKER_RUNTIME_IMAGE}" \
    --provider-tools-lockfile /opt/synara/provider-tools/package-lock.json \
    --provider-host-lockfile /opt/synara/provider-host/bun.lock \
    --provider-host-package /opt/synara/provider-host/package.json \
    --worker-apk-lockfile /opt/synara/worker-apk-packages.lock \
    --raw-provider-tools-sbom /tmp/provider-tools.raw.spdx.json \
    --provider-tools-sbom-output /opt/synara/provider-tools.spdx.json \
    --manifest-output /opt/synara/worker-image-manifest.json \
  && rm -rf /opt/synara/build /tmp/provider-tools.raw.spdx.json

RUN addgroup -g 10001 -S synara-worker && \
  adduser -u 10001 -S -D -h /home/synara -s /bin/bash -G synara-worker synara-worker && \
  mkdir -p /data/workspaces && chown -R 10001:10001 /data /home/synara

FROM worker-runtime-base AS worker

ARG SYNARA_VERSION
ARG SYNARA_GIT_SHA

COPY --from=agentd-build /out/synara-agentd /usr/local/bin/synara-agentd
COPY --from=provider-host-build /out/provider-host.mjs /opt/synara/provider-host/index.mjs
RUN printf '%s\n' '#!/bin/sh' 'exec node /opt/synara/provider-host/index.mjs "$@"' \
  > /usr/local/bin/provider-host && chmod 0755 /usr/local/bin/provider-host

ENV HOME=/home/synara \
  PATH=/opt/synara/provider-tools/node_modules/.bin:/home/synara/.local/bin:${PATH} \
  SYNARA_AGENTD_WORKSPACE_ROOT=/data/workspaces \
  SYNARA_AGENTD_WORKER_IMAGE_MANIFEST_PATH=/opt/synara/worker-image-manifest.json \
  NPM_CONFIG_UPDATE_NOTIFIER=false

LABEL org.opencontainers.image.title="Synara Worker" \
  org.opencontainers.image.version="${SYNARA_VERSION}" \
  org.opencontainers.image.revision="${SYNARA_GIT_SHA}"

WORKDIR /data
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/synara-agentd"]

# Deterministic Target acceptance image. This extends the production Worker
# image with a bundled Provider Host Protocol fixture, while keeping the
# default and production `worker` targets free of test-only runtime behavior.
FROM worker AS worker-acceptance

COPY --from=provider-host-fixture-build /out/provider-host-fixture.mjs /opt/synara/acceptance/provider-host-fixture.mjs

# Preserve the Synara web/server runtime as the default image. Worker images are
# selected explicitly with `--target worker`.
FROM runtime AS default

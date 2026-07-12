# syntax=docker/dockerfile:1.7

FROM golang:1.26-alpine AS build

ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}

WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/control-plane ./cmd/api && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/control-plane-metadata ./cmd/metadata && \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/synara-agentd ./cmd/agentd && \
    mkdir -p /out/data && chown 10001:10001 /out/data

FROM scratch

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/control-plane /usr/local/bin/control-plane
COPY --from=build /out/control-plane-metadata /usr/local/bin/control-plane-metadata
COPY --from=build /out/synara-agentd /usr/local/bin/synara-agentd
COPY --from=build --chown=10001:10001 /out/data /data

USER 10001:10001
EXPOSE 3780
ENTRYPOINT ["/usr/local/bin/control-plane"]

# syntax=docker/dockerfile:1.7
# =============================================================================
# VPN Gate sidecar: Mihomo + the vpngate controller (backend/cmd/vpngate)
# Build from the repository root:
#   docker build -f deploy/vpngate/Dockerfile -t sub2api-vpngate:local .
# =============================================================================

ARG GOLANG_IMAGE=golang:1.27.0-alpine
ARG ALPINE_IMAGE=alpine:3.21
ARG GOPROXY=https://goproxy.cn,direct
ARG GOSUMDB=sum.golang.google.cn

FROM --platform=${BUILDPLATFORM} ${GOLANG_IMAGE} AS builder
ARG GOPROXY
ARG GOSUMDB
ARG TARGETOS
ARG TARGETARCH
ENV GOPROXY=${GOPROXY}
ENV GOSUMDB=${GOSUMDB}
WORKDIR /app/backend
COPY backend/go.mod backend/go.sum ./
RUN --mount=type=cache,id=sub2api-gomod,target=/go/pkg/mod \
    go mod download
COPY backend/ ./
RUN --mount=type=cache,id=sub2api-gomod,target=/go/pkg/mod \
    --mount=type=cache,id=sub2api-gobuild,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/vpngate ./cmd/vpngate

FROM ${ALPINE_IMAGE} AS mihomo
ARG TARGETARCH
# Same pinned release and checksums as any-auto-register's Dockerfile.
ARG MIHOMO_VERSION=1.19.31
RUN apk add --no-cache curl && \
    case "${TARGETARCH}" in \
      amd64) asset="mihomo-linux-amd64-v${MIHOMO_VERSION}.gz"; sha256="d5e74bbddbdfff49a1aef7775bf5911da59f0d7196ed509a0ac914b3653dd5f1" ;; \
      arm64) asset="mihomo-linux-arm64-v${MIHOMO_VERSION}.gz"; sha256="9e0f11afbf38426b8bd88fdc594678f8161c57eccb4e1b77acb12b493904f1d4" ;; \
      *) echo "Unsupported Mihomo architecture: ${TARGETARCH}" >&2; exit 1 ;; \
    esac && \
    curl --fail --location --silent --show-error \
      "https://github.com/MetaCubeX/mihomo/releases/download/v${MIHOMO_VERSION}/${asset}" -o /tmp/mihomo.gz && \
    echo "${sha256}  /tmp/mihomo.gz" | sha256sum -c - && \
    gzip -dc /tmp/mihomo.gz > /usr/local/bin/mihomo && \
    chmod 0755 /usr/local/bin/mihomo && \
    /usr/local/bin/mihomo -v

FROM ${ALPINE_IMAGE}
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -g 1000 vpngate && \
    adduser -u 1000 -G vpngate -s /sbin/nologin -D vpngate && \
    mkdir -p /data/pool /data/state && \
    chown vpngate:vpngate /data/state && \
    chmod 0700 /data/state
COPY --from=mihomo /usr/local/bin/mihomo /usr/local/bin/mihomo
COPY --from=builder /out/vpngate /usr/local/bin/vpngate
USER vpngate
ENTRYPOINT ["/usr/local/bin/vpngate"]

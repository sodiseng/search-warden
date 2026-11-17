# syntax=docker/dockerfile:1
#
# search-warden gateway image.
# Build from the repository root (the build context must contain gateway/):
#   docker build -t search-warden .
# Multi-arch:
#   docker buildx build --platform linux/amd64,linux/arm64 -t search-warden .

ARG GO_VERSION=1.27.4

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG SRC_DIR=gateway
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=
WORKDIR /src

# Dependencies first for layer caching (go.sum appears once third-party modules are added).
COPY ${SRC_DIR}/go.* ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY ${SRC_DIR}/ ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags "-s -w \
        -X github.com/sodiseng/search-warden/gateway/internal/buildinfo.Version=${VERSION} \
        -X github.com/sodiseng/search-warden/gateway/internal/buildinfo.Commit=${COMMIT} \
        -X github.com/sodiseng/search-warden/gateway/internal/buildinfo.Date=${DATE}" \
      -o /out/warden ./cmd/warden \
 && mkdir -p /out/data

# distroless: no shell, no package manager, runs as uid 65532 (nonroot).
FROM gcr.io/distroless/static-debian13:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="search-warden" \
      org.opencontainers.image.description="Tavily + Exa multi-key gateway with native MCP compatibility" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.source="https://github.com/sodiseng/search-warden" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=build /out/warden /warden
# Pre-create /data owned by nonroot so a fresh named volume is writable.
COPY --from=build --chown=65532:65532 /out/data /data

ENV WARDEN_DATA_DIR=/data \
    WARDEN_DB=sqlite:///data/db \
    WARDEN_LISTEN=:18683 \
    WARDEN_ADMIN_LISTEN=shared \
    WARDEN_ADMIN_BASE_PATH=/admin/

USER 65532:65532
# Gateway + panel on one port (panel at /admin/). Dockerfile comments must be on
# their own line: after an instruction a `#` is read as an argument.
EXPOSE 18683
VOLUME ["/data"]

# The binary probes its own /healthz (distroless has no curl/wget).
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/warden", "healthcheck"]

ENTRYPOINT ["/warden"]
CMD ["serve"]

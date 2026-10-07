# syntax=docker/dockerfile:1.7
FROM rust:1.89-bookworm AS rust-build
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates clang git libssl-dev pkg-config protobuf-compiler \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /source
# The locked workspace supplies the official instruction builder and the
# retained observer projection bridge. No migration executables are packaged.
COPY . .
ENV SQLX_OFFLINE=true
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/usr/local/cargo/git \
    --mount=type=cache,target=/source/target \
    cargo build --locked --release -p loyal-yield-orchestrator --bin loyal-klend-proxy \
        -p balance-sweep-ata-monitor --bin earn-domain-bridge \
    && mkdir -p /out \
    && cp target/release/loyal-klend-proxy target/release/earn-domain-bridge /out/

FROM golang:1.26.6-bookworm AS build
WORKDIR /source
COPY go/workers/go.mod go/workers/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY go/workers/ ./
ARG LOYAL_IMAGE_VERSION
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    flags="-s -w -X github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine.Release=sha-${LOYAL_IMAGE_VERSION}" && \
    CGO_ENABLED=0 go build -trimpath -ldflags="$flags" -o /out/loyal-observer ./cmd/loyal-observer && \
    CGO_ENABLED=0 go build -trimpath -ldflags="$flags" -o /out/loyal-engine ./cmd/loyal-engine && \
    CGO_ENABLED=0 go build -trimpath -ldflags="$flags" -o /out/loyal-evidence ./cmd/loyal-evidence
FROM debian:bookworm-slim
ENV RETAIL_KLEND_PROXY_PATH=/usr/local/bin/loyal-klend-proxy \
    EARN_DOMAIN_BRIDGE_BINARY=/usr/local/bin/earn-domain-bridge
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates libssl3 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ /usr/local/bin/
COPY --from=rust-build /out/ /usr/local/bin/
RUN chmod 0555 /usr/local/bin/loyal-observer /usr/local/bin/loyal-engine \
        /usr/local/bin/loyal-evidence /usr/local/bin/loyal-klend-proxy /usr/local/bin/earn-domain-bridge \
    && mkdir -p /usr/local/share/loyal-workers \
    && cd /usr/local/bin \
    && sha256sum loyal-observer loyal-engine loyal-evidence loyal-klend-proxy earn-domain-bridge \
        > /usr/local/share/loyal-workers/SHA256SUMS \
    && chmod 0444 /usr/local/share/loyal-workers/SHA256SUMS
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/loyal-engine"]

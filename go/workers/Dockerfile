# syntax=docker/dockerfile:1.7
FROM golang:1.26.6-bookworm AS build
WORKDIR /source
COPY go/workers/go.mod go/workers/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY go/workers/ ./
ARG LOYAL_IMAGE_VERSION
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    flags="-s -w -X github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine.Release=${LOYAL_IMAGE_VERSION}" && \
    CGO_ENABLED=0 go build -trimpath -ldflags="$flags" -o /out/loyal-observer ./cmd/loyal-observer && \
    CGO_ENABLED=0 go build -trimpath -ldflags="$flags" -o /out/loyal-engine ./cmd/loyal-engine && \
    CGO_ENABLED=0 go build -trimpath -ldflags="$flags" -o /out/loyal-evidence ./cmd/loyal-evidence
FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/ /usr/local/bin/
RUN chmod 0555 /usr/local/bin/loyal-observer /usr/local/bin/loyal-engine \
        /usr/local/bin/loyal-evidence \
    && mkdir -p /usr/local/share/loyal-workers \
    && cd /usr/local/bin \
    && sha256sum loyal-observer loyal-engine loyal-evidence \
        > /usr/local/share/loyal-workers/SHA256SUMS \
    && chmod 0444 /usr/local/share/loyal-workers/SHA256SUMS
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/loyal-engine"]

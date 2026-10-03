FROM golang:1.26.6 AS build
WORKDIR /source
COPY go/workers/go.mod go/workers/go.sum ./
RUN go mod download
COPY go/workers/ ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/loyal-observer ./cmd/loyal-observer && \
    CGO_ENABLED=0 go build -trimpath -o /out/loyal-engine ./cmd/loyal-engine && \
    CGO_ENABLED=0 go build -trimpath -o /out/loyal-evidence ./cmd/loyal-evidence
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/loyal-engine"]

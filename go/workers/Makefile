GO ?= go
.PHONY: fmt vet test race build verify release
fmt:
	gofmt -w cmd internal
vet:
	$(GO) vet ./...
test:
	$(GO) test ./...
race:
	$(GO) test -race ./...
build:
	$(GO) build ./cmd/loyal-observer ./cmd/loyal-engine ./cmd/loyal-evidence
verify:
	../../scripts/verify-workers-v2.sh
# Static Linux binaries for deploy/hetzner. The release identity is the
# commit, stamped by the linker; there is no version environment variable.
GOARCH ?= amd64
RELEASE ?= sha-$(shell git rev-parse HEAD)
release:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) $(GO) build -trimpath \
		-ldflags "-s -w -X github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine.Release=$(RELEASE)" \
		-o bin/ ./cmd/loyal-observer ./cmd/loyal-engine

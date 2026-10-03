GO ?= go
.PHONY: fmt vet test race build verify
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

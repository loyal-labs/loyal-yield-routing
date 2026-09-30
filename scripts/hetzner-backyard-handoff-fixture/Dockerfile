# Toolchain version matches deployed go.mod. Public modules checked by go.sum
# and sum.golang.org; runtime network is disabled by parent command.
ARG GO_IMAGE
ARG PG_IMAGE
FROM ${GO_IMAGE} AS build
WORKDIR /fixture/go/backyard-rwa-worker
COPY go/backyard-rwa-worker/go.mod go/backyard-rwa-worker/go.sum ./
ENV GOTOOLCHAIN=local GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org
RUN go mod download && go mod verify
COPY go/backyard-rwa-worker/ ./
RUN go test -race -c ./internal/backyardrwa -o /handoff.test

FROM ${PG_IMAGE}
RUN apt-get update && apt-get install -y --no-install-recommends python3 && rm -rf /var/lib/apt/lists/*
COPY --from=build /handoff.test /handoff.test
COPY --chown=postgres:postgres . /fixture/
USER postgres
WORKDIR /fixture
ENTRYPOINT ["python3", "scripts/hetzner-backyard-handoff-fixture/run.py", "--pg-bindir", "/usr/lib/postgresql/17/bin", "--ack-isolated", "--test-binary", "/handoff.test"]

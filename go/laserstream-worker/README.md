# Loyal combined LaserStream worker

This module owns the single confirmed LaserStream subscription for Kamino,
balance-sweep ATA, Earn account, Earn MAX transaction, and slot-progress
filters.

## Runtime ownership

The Go executable `loyal-laserstream-worker` owns:

- the direct Helius gRPC connection and durable replay cursor selection;
- atomic, overlapping filter-set handoffs;
- Kamino reserve decoding, Timescale persistence, observation floors, confirmed
  RPC seeding, and periodic verification;
- SPL Token ATA decoding, exact Timescale deduplication, confirmed RPC seeding,
  and invalid-account rechecks;
- dynamic Loyal Earn watch-set derivation;
- atomic Earn reconciliation-job, Autodeposit request, and replay-cursor writes;
- policy transaction delivery and acknowledgement;
- reconnect supervision, progress timeouts, readiness, Prometheus metrics,
  structured alerts, and optional OTLP metric export.

The image also contains `earn-domain-bridge`, a supervised compatibility
processor built from the existing Rust domain implementation. Go sends each
Earn MAX policy transaction over a private protobuf pipe and does not advance
the shared stream frontier until the bridge acknowledges durable policy/memo
projection. The bridge runs the existing Earn and Autodeposit RPC-proof
consumers and Earn APY projection. This deliberately preserves the mature chain
proof and mutation semantics while removing their independent LaserStream
connection; a bridge exit is fatal to the Go worker and immediately fails
readiness.

## Parallel filter-set handoff

A filter update does not mutate the active stream in place:

1. Read the active application-durable frontier.
2. Open a candidate subscription with the complete replacement filter set.
3. Set `from_slot` to the smaller of the new binding replay start or the active
   frontier minus the configured overlap.
4. Keep both network subscriptions connected.
5. Freeze old durable delivery at an application boundary.
6. Process candidate replay until it reaches that boundary; only one stream
   invokes domain handlers at a time.
7. Atomically promote the candidate and cancel the old stream.

The overlap deliberately produces duplicate deliveries. Every handler retains
the existing production idempotency key and returns only after its database
commit or bridge acknowledgement.

## Health and alerts

The executable serves:

- `/healthz` — process liveness;
- `/readyz` — connection, progress freshness, stream frontier, and per-domain
  durable frontiers;
- `/metrics` — Prometheus metrics.

A stopped stream, dead reconciliation bridge, stalled frontier, database write
failure, or exhausted confirmed-state verification emits a structured error and
fails readiness. The service exits for terminal ownership failures so Render
restarts it from durable cursors.

## Verification

Go 1.25.1 is pinned by `go.mod`.

```sh
GO_BIN=/path/to/go1.25.1/bin/go \
  ../../scripts/verify-go-laserstream-handoff.sh
```

The isolated verifier creates a PostgreSQL cluster and a TimescaleDB container,
applies the real Timescale migrations, and proves:

- one combined accounts + transactions + slots request;
- two physical subscriptions only during handoff;
- negative-overlap and deeper new-binding replay;
- race-safe promotion and candidate-failure rollback;
- no concurrent old/candidate durable handlers;
- exact Earn job/cursor/Autodeposit writes;
- exact ATA observation deduplication against the real schema;
- Kamino reserve decoding and verified persistence against the real schema;
- no gaps despite deliberate overlap duplicates.

`internal/kamino` and `internal/watch` also contain opt-in, read-only production
compatibility tests for current account bytes and Loyal schemas.

## Packaging and cutover status

`Dockerfile.go-laserstream-worker` packages the Go executable and supervised
Earn domain bridge. `.github/workflows/go-laserstream-worker.yml` runs Go race
tests, real-schema E2E verification, Rust bridge checks, and publishes immutable
`ghcr.io/loyal-labs/loyal-yield-routing/go-laserstream-worker:sha-<commit>`
images from trusted `main` pushes.

This PR intentionally does not repoint Render. Deploy the image to staging and
complete shadow/parity evidence before changing or stopping either production
Rust monitor.

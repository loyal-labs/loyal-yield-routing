# Loyal Go LaserStream worker

This module owns the transport boundary for Loyal's combined LaserStream worker.
It uses one confirmed `SubscribeRequest` containing the Kamino, balance-sweep,
Earn account, Earn MAX transaction, and slot-progress filters.

## Parallel filter-set handoff

A filter update does not mutate the active stream in place:

1. Read the active application-durable frontier.
2. Open a candidate subscription with the complete replacement filter set.
3. Set `from_slot` to the smaller of:
   - the caller's replay start, including any new binding's observation slot;
   - the active frontier minus the configured overlap.
4. Open the candidate network stream while the old stream remains connected.
5. Freeze the old delivery gate at a durable boundary.
6. Process candidate replay through the same idempotent durable handlers until
   it reaches that stable boundary. Only one stream invokes domain handlers at
   a time.
7. Swap ownership and cancel the old stream.

The overlap deliberately produces duplicate deliveries. Domain persistence must
retain its existing dedupe keys and must not return from `Handler.Handle` before
the update is durable.

The manager connects through the official Helius Go protobuf package but owns
replay itself. The high-level SDK's receive-side slot tracker is not used as an
application durability acknowledgement.

## Verification

Go 1.25.1 is pinned by `go.mod`.

```sh
GO_BIN=/path/to/go1.25.1/bin/go \
  ../../scripts/verify-go-laserstream-handoff.sh
```

The isolated E2E runs an in-process Yellowstone gRPC server and creates a
throwaway local PostgreSQL 17 cluster. It proves:

- one combined accounts + transactions + slots request;
- exactly two physical subscriptions during handoff;
- replay from the deeper new-binding slot when necessary;
- promotion only after the candidate reaches the frozen durable frontier;
- cancellation of the old stream after promotion;
- rollback to the still-connected old stream when a candidate fails;
- no concurrent old/candidate calls into domain handlers;
- `ON CONFLICT` absorbs overlap replays into exact-once durable rows;
- no event gaps despite overlap duplicates.

## Cutover status

This module currently implements and verifies the combined request, direct gRPC
transport, durable-handler routing contract, and parallel handoff primitive. It
is not yet wired into Render and does not replace the Rust domain processors.
Kamino decoding/verification and Earn/Autodeposit persistence must be connected
as `Handler` implementations before production cutover.

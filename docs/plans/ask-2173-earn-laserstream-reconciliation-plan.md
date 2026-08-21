# ASK-2173: durable Earn reconciliation from LaserStream

## Goal

Make the account-only LaserStream subscription the single source of Earn
reconciliation wake-ups without letting Solana RPC or reconciliation failures
interrupt balance-sweep monitoring.

The existing loyal-balance-sweep-ata-monitor process owns both halves for now:

    LaserStream account update
            |
            v
    normalize + map affected vaults
            |
            v
    one PostgreSQL transaction
      - insert deduplicated jobs
      - advance ingestion cursor
            |
            v
    in-process reconciliation consumer
      - claim oldest ready job per vault
      - collect chain proof
      - apply canonical mutation + complete job atomically

There is no transaction subscription and no new Render, fleet, or Loyal App
worker.

## Ownership boundaries

### Stream path

The stream path does only work required to accept an event:

1. preserve account channel, pubkey, signature, slot, and deletion state;
2. resolve every watched vault affected by the account;
3. serialize the normalized event and the matching vault binding;
4. insert one job per affected vault and advance the cursor in one transaction;
5. notify the local consumer.

It does not call Solana RPC, inspect Kamino state, or write canonical Earn
positions.

### Reconciliation path

One process-owned task drains the durable queue independently of LaserStream
sessions. A reconnect or watch-set rebuild does not cancel it.

The consumer claims bounded work with a lease. Jobs are ordered per vault, so a
failed proof blocks later work for that vault but not unrelated vaults or ATA
monitoring. On success, the canonical mutation and job completion commit
together. On failure, the claim is released, error and attempt count remain
visible, and the job gets a retry time.

### Chain proof

Deposit, policy, and cleanup proof semantics remain in earn_reconciliation.rs.
Cleanup inventory uses getTokenAccountsByOwner for each of SPL Token and
Token-2022 with confirmed commitment and a minimum context slot. It never
requests an entire token program.

## Durable schema

Migration 49 adds loyal_yield.earn_reconciliation_jobs.

- Identity: consumer name, event key, settings, vault index, and vault pubkey.
- Evidence: normalized event JSON and the matched immutable watch binding.
- Retry state: attempt count, next attempt, claim owner/expiry, and last error.
- Completion: nullable completion timestamp.

laserstream_replay_cursors.durable_slot now means every affected job through
this slot is durable, not every proof has completed. This is safe because
restart recovery drains the table instead of depending on a replayed event.

## Failure behavior

| Failure | Result |
| --- | --- |
| Database enqueue fails | Event loop exits; LaserStream restarts from the old cursor |
| Process dies after enqueue | Cursor and pending jobs survive; next process drains them |
| Proof/RPC fails | Job remains pending; subscription and ATA handling continue |
| Process dies during a claim | Lease expires; another consumer attempt reclaims it |
| Canonical write fails | Completion rolls back with it; job remains retryable |
| Duplicate LaserStream event | Unique job identity makes enqueue idempotent |
| Watch-set/session rebuild | Process-owned consumer continues independently |

## Verification

The frozen verifier is documented in
docs/plans/ask-2173-earn-laserstream-verifier.md and executed by:

    bash scripts/verify-earn-laserstream-reconciliation.sh \
      --app-root /path/to/loyal-app

It starts isolated PostgreSQL, applies migrations, simulates account events,
checks canonical database state, proves ingest-only crash recovery, retains and
retries a failed proof, replays duplicates, and runs the focused Rust checks.

## Rollout

Pre-merge:

1. Keep the verifier and required CI green.
2. Review migration 49 and the process-owned consumer boundary.
3. Publish an immutable laserstream-workers image for the merged commit.

Post-merge:

1. Apply migration 49 before the worker starts.
2. Redeploy loyal-balance-sweep-ata-monitor with the immutable image.
3. Do not redeploy or create a fleet reconciliation worker.
4. Verify LaserStream session health, cursor movement, pending/failed job age,
   completed jobs, and absence of bulk ATA reseeds.
5. Alert on an old pending job, repeated proof failures, a stalled ingestion
   cursor, or a stopped monitor.

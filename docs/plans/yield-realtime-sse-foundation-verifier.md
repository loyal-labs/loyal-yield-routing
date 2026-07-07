# Yield Realtime SSE Foundation Verifier

Use this as the fixed verifier for the first realtime implementation slice in
`loyal-yield-routing`.

This verifier intentionally covers only the foundation:

- a durable Yield Neon event table;
- a small event-emitting SQL helper that uses `pg_notify` as a wakeup;
- a standalone Render-compatible SSE service that listens, catches up by event
  id, authenticates scoped clients, and sends invalidation messages.

It must not require frontend hooks, Earn app wiring, autodeposit worker wakeups,
or business-table triggers yet. Those are later slices.

## Goal

The repo must contain a deployable realtime foundation that can safely deliver
at-least-once invalidation events from Yield Neon to authenticated SSE clients
without becoming canonical financial state and without changing existing Earn,
autodeposit, same-mint, or frontend behavior.

Overall PASS requires the schema, migration wiring, service implementation,
local verification commands, and no-wiring boundary checks below to pass.

## Required Checks

### 1. Durable Event Outbox Schema

PASS only if a new Yield migration creates `loyal_yield.realtime_events` as an
append-only outbox with a monotonic primary key and enough routing keys for Earn
and autodeposit invalidation.

Required columns:

- `id BIGSERIAL PRIMARY KEY`
- `created_at TIMESTAMPTZ NOT NULL DEFAULT now()`
- `event_type TEXT NOT NULL`
- `scope TEXT NOT NULL`
- `reason TEXT NOT NULL`
- `solana_env TEXT`
- `wallet_address TEXT`
- `settings_pda TEXT`
- `smart_account_address TEXT`
- `vault_pubkey TEXT`
- `target_id BIGINT`
- `scheduled_slot_id BIGINT`
- `execution_id BIGINT`
- `source_table TEXT`
- `source_id TEXT`
- `payload JSONB NOT NULL DEFAULT '{}'::jsonb`

Required indexes:

- `id`
- `(scope, id)`
- `(settings_pda, id)` where `settings_pda IS NOT NULL`
- `(wallet_address, id)` where `wallet_address IS NOT NULL`
- `(target_id, id)` where `target_id IS NOT NULL`
- `(source_table, source_id)` where both are non-null
- `created_at`

PASS requires this table to be created by the normal `yield-migrations` path and
validated by `yield-migrations --check`; ad hoc SQL outside the migration path
does not count.

### 2. Notify Helper Is Wakeup-Only

PASS only if the migration defines a helper equivalent to:

```sql
loyal_yield.emit_realtime_event(...) RETURNS BIGINT
```

The helper must insert into `loyal_yield.realtime_events`, return the inserted
event id, and call:

```sql
pg_notify('loyal_yield_realtime', json_build_object('event_id', id)::text)
```

The notification payload must not include balances, full financial state,
private payloads, signatures, or raw evidence. It is a wakeup only.

PASS requires no business-table triggers in this slice. There must be no
automatic emissions wired to existing Earn/autodeposit tables yet.

### 3. SSE Service Shape

PASS only if the repo contains a standalone realtime service binary that can run
as a Render Web Service and exposes:

- `GET /healthz` returning `200`;
- `GET /events?token=...` returning `text/event-stream`;
- HTTP binding to `0.0.0.0:$PORT`;
- periodic SSE heartbeats;
- graceful shutdown on process termination.

The service must read its configuration from env vars, with at least:

- `NEON_DATABASE_URL`
- `REALTIME_AUTH_SECRET`
- optional `REALTIME_ALLOWED_ORIGINS`
- optional `REALTIME_HEARTBEAT_SECONDS`
- optional `REALTIME_CATCH_UP_LIMIT`
- optional `REALTIME_CHANNEL`, defaulting to `loyal_yield_realtime`

PASS requires the listener connection to reject or fail fast on Neon pooled
`-pooler` URLs, because `LISTEN/NOTIFY` needs a direct session connection.

### 4. Cursor Catch-Up And At-Least-Once Delivery

PASS only if the service treats notifications as wakeups and always queries
durable rows from `loyal_yield.realtime_events`.

Required behavior:

- on startup, establish a direct Postgres listener and `LISTEN` on the realtime
  channel;
- on notification, query events with `id > cursor ORDER BY id ASC LIMIT N`;
- on periodic fallback tick, run the same catch-up query even without a notify;
- update the service cursor only after loading rows from the durable table;
- send SSE `id: <event_id>` so clients can dedupe or reconnect;
- honor `Last-Event-ID` by starting a client from that id when available;
- if a requested cursor is older than retained events or a client queue
  overflows, send a `resync_required` invalidation rather than pretending the
  client is current.

PASS requires pushed event data to remain an invalidation envelope. The service
must not push canonical balances, position amounts, or execution evidence as
trusted UI state.

### 5. Auth And Scope Filtering

PASS only if `/events` requires a signed short-lived token and filters outgoing
events by token scope.

Required token claims:

- expiry;
- `walletAddress` and/or `settingsPda`;
- optional `smartAccountAddress`;
- optional `solanaEnv`, defaulting to mainnet when omitted;
- allowed scopes.

Required filtering:

- an event scoped to a wallet is sent only to tokens for that wallet;
- an event scoped to a settings PDA is sent only to tokens for that settings PDA;
- event `scope` must be in the token's allowed scopes;
- malformed, expired, or badly signed tokens return `401`;
- tokens and full request URLs are not logged.

The exact token format is implementation-defined, but it must be HMAC-verified
or stronger and must not rely on unauthenticated query parameters for routing.

### 6. No Existing Logic Wired Yet

PASS only if this slice is additive.

Required negative checks:

```sh
rg -n "emit_realtime_event|loyal_yield_realtime|realtime_events" \
  crates/balance-sweep-autodeposit-trigger \
  crates/balance-sweep-ata-projector \
  crates/balance-sweep-ata-monitor \
  scripts \
  src
```

The command may match only the new realtime service, migration files, migration
runner validation, verifier docs, or package metadata. It is FAIL if existing
workers, frontend routes, app UI, executor scripts, or business logic now depend
on realtime delivery.

### 7. Render Packaging Boundary

PASS only if the service is packaged in the existing light-worker image path or
another repo-approved immutable image path without converting existing workers
back to Render source/Docker builds.

Required evidence:

- Docker build includes the realtime binary;
- Render-facing command can run the service binary;
- no existing Render worker command is changed to depend on the realtime
  service;
- no persistent disk is required.

Blueprint/service creation may remain deferred if the binary and image path are
ready and documented; live Render deployment is not required for this slice.

### 8. Local Verification Commands

PASS only if the narrow local checks for this slice pass, or any failure is
explicitly attributed to pre-existing unrelated merge conflicts.

Required commands:

```sh
cargo fmt --check
```

```sh
cargo check -p loyal-yield-realtime
```

```sh
cargo check -p loyal-yield-orchestrator --bin yield-migrations
```

```sh
rg -n "pooler|LISTEN|NOTIFY|Last-Event-ID|text/event-stream|resync_required" \
  crates/loyal-yield-realtime crates/loyal-yield-orchestrator/migrations
```

Secret-backed live migration checks are optional for this slice and must use the
repo's 1Password pattern if run:

```sh
op run --env-file=.env.1password -- sh -c 'bun run yield:migrate:check'
```

## Verdict Format

```text
Durable Event Outbox Schema: PASS|FAIL - note
Notify Helper Is Wakeup-Only: PASS|FAIL - note
SSE Service Shape: PASS|FAIL - note
Cursor Catch-Up And At-Least-Once Delivery: PASS|FAIL - note
Auth And Scope Filtering: PASS|FAIL - note
No Existing Logic Wired Yet: PASS|FAIL - note
Render Packaging Boundary: PASS|FAIL - note
Local Verification Commands: PASS|FAIL - note
Overall Verdict: PASS|FAIL
```

Overall PASS requires every required section to pass. Live Render deployment,
frontend token endpoints, frontend hooks, business-table triggers, and worker
LISTEN wakeups are deliberately out of scope for this verifier.

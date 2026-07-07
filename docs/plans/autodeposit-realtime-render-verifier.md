# Autodeposit Realtime Render Verifier

Use this as the verifier-first goal for the first wired realtime slice:
autodeposit "Execute now" wakeups plus the Render SSE gateway.

This verifier checks the end state, not the implementation steps. Do not mark it
PASS because a migration was written, a worker compiled, or a Render deploy
started. It passes only when a skeptical runner can prove from repo files,
database readbacks, live Render service state, and logs that:

- Yield Neon emits durable autodeposit realtime events for scheduled-slot
  changes;
- the autodeposit trigger wakes from Neon `LISTEN/NOTIFY` while keeping the
  existing polling path as fallback;
- the SSE gateway is deployed as a Render Web Service and can stream scoped
  invalidations from the durable event table;
- the production autodeposit trigger is deployed on the updated immutable
  light-worker image;
- no Loyal frontend app code was changed or wired in this slice.

## Goal

When a balance-sweep scheduled slot is marked `requested`, production should no
longer wait for only the normal 5-15 second poll cycle before the autodeposit
trigger sees it. The database must write a durable realtime event and notify
listeners; the Render autodeposit trigger must treat that notification as a
wake-up, re-query the durable eligible-slot path, and execute through the
existing claim/executor boundary. The Render SSE service must independently
listen to the same durable event stream and deliver authenticated invalidation
events to clients, without becoming canonical financial state.

Overall PASS requires every Required Check below to pass. Frontend token
endpoints, frontend hooks, and any change inside `loyal-apps` are out of scope
and must remain untouched.

## Required Checks

### 1. Existing Realtime Foundation Still Passes

PASS only if the prior realtime foundation remains intact:

- `loyal_yield.realtime_events` exists with the columns and indexes required by
  `docs/plans/yield-realtime-sse-foundation-verifier.md`;
- `loyal_yield.emit_realtime_event(...)` inserts a durable row, returns the row
  id, and calls `pg_notify('loyal_yield_realtime', '{"event_id":...}')`;
- notify payloads remain wake-up only and do not include balances, full
  financial state, private keys, signatures, or raw evidence;
- the SSE service still rejects or fails fast on Neon pooled `-pooler` URLs.

Required command evidence:

```sh
cargo check -p loyal-yield-realtime
```

```sh
cargo check -p loyal-yield-orchestrator --bin yield-migrations
```

```sh
rg -n "realtime_events|emit_realtime_event|pg_notify|pooler|Last-Event-ID|resync_required" \
  crates/loyal-yield-realtime crates/loyal-yield-orchestrator/migrations \
  crates/loyal-yield-orchestrator/src/bin/yield-migrations.rs
```

### 2. Autodeposit Durable Event Emission

PASS only if a new normal Yield migration wires autodeposit scheduled-slot
changes into the durable realtime event outbox.

Required schema behavior:

- the migration is registered in both migration paths used by the repo;
- `yield-migrations --check` sees the migration as applied after deployment;
- a trigger or equivalent database-owned function emits exactly one durable
  realtime event for relevant `balance_sweep_scheduled_slots` inserts or
  updates;
- the event is scoped to `autodeposit`;
- `source_table = 'balance_sweep_scheduled_slots'`;
- `source_id = slot.id::text`;
- `target_id`, `scheduled_slot_id`, and `execution_id` are populated when
  available;
- wallet/settings/vault routing keys are copied from
  `loyal_yield.balance_sweep_targets`;
- event reasons distinguish at least `scheduled_slot_requested`,
  `scheduled_slot_selected`, `scheduled_slot_executed`,
  `scheduled_slot_failed`, and `scheduled_slot_released`;
- payloads are small metadata only, such as slot status, request source, and
  booleans. They must not contain balances, claim tokens, signatures, private
  payloads, or raw execution evidence.

Required negative behavior:

- no event is emitted for a no-op update where the relevant slot fields did not
  change;
- event emission failure must not bypass the durable slot update silently. It is
  better for the database transaction to fail than to mark a slot requested
  while losing the wake-up event.

Required live database evidence through 1Password:

```sh
op run --env-file=.env.1password -- sh -c 'bun run yield:migrate:check'
```

```sh
op run --env-file=.env.1password -- sh -c 'psql "$NEON_DATABASE_URL" -X -v ON_ERROR_STOP=1'
```

The SQL readback must prove:

```sql
SELECT version, name
FROM loyal_yield.schema_migrations
WHERE name ILIKE '%autodeposit%realtime%'
ORDER BY version DESC
LIMIT 1;
```

returns one row, and:

```sql
SELECT tgname
FROM pg_trigger
WHERE tgrelid = 'loyal_yield.balance_sweep_scheduled_slots'::regclass
  AND NOT tgisinternal
  AND tgname ILIKE '%realtime%';
```

returns the installed trigger.

### 3. Autodeposit Trigger LISTEN Wake-Up

PASS only if `balance-sweep-autodeposit-trigger` can wake immediately from the
Neon realtime channel while keeping its existing durable execution path.

Required behavior:

- continuous mode opens a direct Postgres listener and `LISTEN`s on
  `loyal_yield_realtime` by default;
- the channel is configurable by env/CLI for staging or future split streams;
- `--once`, claim completion, release, and one-target claim modes do not block
  waiting for notifications;
- a listener failure logs a warning and falls back to the existing
  `poll_interval_seconds` sleep rather than killing production autodeposit;
- a relevant notification causes the next loop iteration immediately instead of
  waiting for the full poll interval;
- notification payloads are parsed only to find a durable event id. The worker
  must query `loyal_yield.realtime_events` and then the existing eligible-slot
  SQL before acting;
- only relevant `scope = 'autodeposit'` events wake execution. Irrelevant
  scopes can be ignored until the fallback tick;
- the worker still runs `project_surplus_lots_once` before execution and still
  executes through `execute_eligible_targets_once`, claim tokens, and
  `BALANCE_SWEEP_EXECUTOR_COMMAND`;
- requested slots stay prioritized by durable SQL ordering, not by notify
  arrival order.

Required command evidence:

```sh
cargo check -p balance-sweep-autodeposit-trigger
```

```sh
rg -n "PgListener|LISTEN|loyal_yield_realtime|realtime_events|poll_interval_seconds|execute_eligible_targets_once|project_surplus_lots_once" \
  crates/balance-sweep-autodeposit-trigger/src/main.rs
```

Required local or live smoke evidence:

- start the worker in non-executing or otherwise safe one-shot/listener test
  posture against Yield Neon;
- insert or update a scoped autodeposit test event through the normal
  `emit_realtime_event`/scheduled-slot trigger path;
- logs show the listener received a realtime wake-up and immediately scanned;
- no transaction execution happens unless the command is intentionally run with
  production `--execute-eligible` and valid executor secrets.

### 4. Render SSE Web Service Is Live

PASS only if the realtime gateway is deployed as a Render Web Service.

Required Render service state:

- service name is clearly realtime-specific, for example
  `loyal-yield-realtime`;
- service type is Web Service, not background worker;
- runtime is the approved immutable private GHCR image path, normally
  `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-<commit>`;
- command is `/usr/local/bin/loyal-yield-realtime`;
- health check path is `/healthz`;
- no persistent disk is attached;
- instance count is one for v1, or every instance independently listens and
  dedupes by durable cursor;
- env includes direct `NEON_DATABASE_URL`, `REALTIME_AUTH_SECRET`, optional
  explicit `REALTIME_ALLOWED_ORIGINS`, and any selected `REALTIME_CHANNEL`;
- the configured Neon host is direct and does not contain `-pooler`.

Required runtime evidence:

- `GET /healthz` on the Render URL returns `200 ok`;
- an invalid or missing SSE token returns `401`;
- with a valid short-lived test token that includes wallet/settings scope and no
  cluster field, `/events?token=...` returns `text/event-stream`;
- inserting a safe test event through `loyal_yield.emit_realtime_event(...)`
  produces an SSE event with `id: <event_id>`, `event: loyal_yield`, and an
  invalidation JSON body;
- reconnecting with `Last-Event-ID` replays missed durable events or emits
  `resync_required` when the cursor is no longer safe.

PASS requires the SSE payload to remain an invalidation envelope. It must not
push canonical balances, position amounts, claim tokens, signatures, or full
execution records.

### 5. Production Autodeposit Worker Is Deployed

PASS only if the production Render background worker
`loyal-balance-sweep-autodeposit-trigger` is live on the updated immutable
light-worker image that contains the LISTEN wake-up code.

Required Render state:

- service id remains `srv-d8lplql7vvec73f1it6g` unless an operator documents a
  replacement;
- runtime remains `image`;
- image is `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-<commit>`
  for the commit containing this implementation;
- command remains
  `/usr/local/bin/balance-sweep-autodeposit-trigger --execute-eligible`;
- `BALANCE_SWEEP_EXECUTOR_COMMAND` still points at
  `bun scripts/execute-autodeposit-policy.ts --require-lot-claim`;
- `BALANCE_SWEEP_EXECUTE_ELIGIBLE=true` remains production-only;
- Render latest deploy is `live`;
- logs from the new deploy show listener startup, realtime wake-up handling, and
  the normal `projected autodeposit surplus lots` /
  `scanned eligible autodeposit lots for execution` summaries.

Staging may be updated too, but staging must remain non-executing:

- command omits `--execute-eligible`;
- `BALANCE_SWEEP_EXECUTE_ELIGIBLE=false`;
- signer/executor secrets are absent unless a separate staging execution
  verifier approves them.

### 6. Deployment Image And Render Readback

PASS only if deployment follows the repo's image-based worker convention.

Required evidence:

- the implementation is committed and pushed;
- the `worker-images` GitHub Actions workflow succeeds for that commit;
- the light-worker image tag `sha-<commit>` exists;
- Render SSE and production autodeposit services point at that immutable tag or
  digest;
- existing background workers are not converted back to Render source/Docker
  builds;
- private GHCR access continues to use the configured Render registry
  credential, not a plaintext token in repo files.

Required readbacks should avoid printing secrets. Acceptable evidence includes
service id, service name, runtime, image tag/digest, command, health path,
deploy id/status, non-secret env var names, and host-only Neon URL fingerprints.

### 7. No Frontend App Wiring

PASS only if this repo slice does not edit or require frontend app changes.

Required negative checks:

```sh
git diff --name-only HEAD~1..HEAD
```

and, if a sibling app checkout exists:

```sh
git -C ../loyal-apps status --short
```

The implementation may update docs, migrations, the realtime service, the
autodeposit trigger, Docker/image metadata, and Render configuration for this
repo. It is FAIL if this slice changes `../loyal-apps`, frontend hooks, app API
routes, or client cache invalidation code.

### 8. Local And Live Verification Commands

PASS only if the narrow check set required by `AGENTS.md` passes, or any failure
is explicitly attributed to pre-existing unrelated state.

Required local checks:

```sh
cargo fmt --check
```

```sh
cargo check -p loyal-yield-realtime -p balance-sweep-autodeposit-trigger -p loyal-yield-orchestrator --bin yield-migrations
```

Required live checks, using the repo's 1Password pattern and never printing
plaintext secrets:

```sh
op run --env-file=.env.1password -- sh -c 'bun run yield:migrate:check'
```

```sh
op run --env-file=.env.1password -- sh -c 'psql "$NEON_DATABASE_URL" -X -v ON_ERROR_STOP=1'
```

Required Render checks:

```sh
op run --env-file=.env.1password -- sh -c 'render services --output json'
```

```sh
op run --env-file=.env.1password -- sh -c 'render deploys list srv-d8lplql7vvec73f1it6g --output json'
```

Equivalent Render API readbacks are acceptable if they are secret-safe and show
the same facts.

## Verdict Format

```text
Existing Realtime Foundation Still Passes: PASS|FAIL - note
Autodeposit Durable Event Emission: PASS|FAIL - note
Autodeposit Trigger LISTEN Wake-Up: PASS|FAIL - note
Render SSE Web Service Is Live: PASS|FAIL - note
Production Autodeposit Worker Is Deployed: PASS|FAIL - note
Deployment Image And Render Readback: PASS|FAIL - note
No Frontend App Wiring: PASS|FAIL - note
Local And Live Verification Commands: PASS|FAIL - note
Overall Verdict: PASS|FAIL
```

Overall PASS requires every required section to pass. If a required check
mis-encodes the real production safety goal, update this verifier explicitly and
state why before continuing; do not quietly weaken it to match an incomplete
implementation.

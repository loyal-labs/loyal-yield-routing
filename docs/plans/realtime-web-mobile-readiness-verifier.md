# Realtime Web And Mobile Readiness Verifier

Use this document as the fixed verifier-first goal for preparing and deploying
`loyal-yield-realtime` as the stable SSE invalidation protocol consumed later by
Loyal web and mobile clients.

The verifier checks observable end state, not whether an implementation
checklist was followed. Run it cold against the checkout, Yield Neon, Render,
and the public SSE endpoint. Return `PASS` only when every Required Check passes.
Otherwise return `FAIL` with the failing check, command or request, observed
result, and next corrective action. Never expose a bearer token, HMAC secret,
database credential, wallet secret, authorization header, or complete
authenticated request URL in evidence.

## Required protocol outcome

The deployed system must implement this boundary:

```text
authenticated Loyal app -> short-lived identity token -> Render SSE
Yield transaction -> durable outbox row -> LISTEN/NOTIFY wakeup -> SSE invalidation
client invalidation -> canonical REST/RPC refetch
```

SSE is never canonical state for balances, positions, or transaction history.

## 1. Deployment source is reconciled before release

PASS only if all of these are true:

- live realtime source tag `sha-2712fed78b8f9c79fc5f3e68638ac126a0cd68dc`
  resolves to commit `2712fed78b8f9c79fc5f3e68638ac126a0cd68dc`;
- live autodeposit source tag
  `sha-34a991c257d2349f85a09373b8426e0700c1d32e` resolves to commit
  `34a991c257d2349f85a09373b8426e0700c1d32e`;
- both commits are ancestors of the implementation commit, including every
  later worker fix already on the release branch;
- migrations `0013` and `0014` are unchanged from their pre-task Git blobs;
- all schema changes are in new, forward-only migrations;
- the final live images are immutable `light-workers:sha-<full-commit>` tags,
  use registry credential `loyal-ghcr`, and their digests are recorded.

Required evidence:

```sh
git merge-base --is-ancestor 2712fed78b8f9c79fc5f3e68638ac126a0cd68dc HEAD
git merge-base --is-ancestor 34a991c257d2349f85a09373b8426e0700c1d32e HEAD
git diff 2712fed78b8f9c79fc5f3e68638ac126a0cd68dc -- \
  crates/loyal-yield-orchestrator/migrations/0013_earn_realtime_events.sql \
  crates/loyal-yield-orchestrator/migrations/0014_autodeposit_execution_slot_realtime.sql
```

The migration diff may show later files but must show no edits to `0013` or
`0014` made by this task.

## 2. Bearer token contract is strict and expires open streams

PASS only if `GET /events` accepts authentication exclusively through:

```http
Authorization: Bearer <token>
Accept: text/event-stream
Last-Event-ID: <optional decimal event id>
```

`?token=` must be rejected even when the token is otherwise valid. Logs and
errors must not contain tokens, authorization values, or complete authenticated
URLs.

The accepted version-1 claims are exactly documented and validated:

```json
{
  "v": 1,
  "iss": "loyal-apps",
  "aud": "loyal-yield-realtime",
  "iat": 0,
  "exp": 0,
  "walletAddress": "base58 pubkey",
  "settingsPda": "base58 pubkey",
  "earnVaultAddress": "base58 pubkey for accountIndex 1",
  "solanaEnv": "mainnet-beta",
  "scopes": ["earn", "autodeposit"],
  "clientKind": "web"
}
```

Required validation:

- known `v`, exact issuer and audience;
- `iat <= now < exp`, no future-issued token, and lifetime at most the
  configured five-minute maximum;
- wallet, settings PDA, Earn vault, cluster, at least one allowlisted scope,
  and allowlisted `clientKind` (`web` or `mobile`) are all present and valid;
- signatures use constant-time HMAC comparison;
- current secret is required and an optional previous secret permits
  zero-downtime rotation;
- the stream is closed at `exp`, not merely rejected at admission;
- concurrent valid web and mobile streams for one identity both remain open;
- invalid signature, expiry, issuer, audience, cluster, scope, client kind, or
  identity returns `401` without revealing which secret or identity exists.

The signing secret exists only in server-side 1Password/Render configuration.
No repo, browser bundle, mobile binary, log, verifier artifact, or chat output
may contain it.

## 3. Browser CORS is exact-origin and native traffic still works

PASS only if `REALTIME_ALLOWED_ORIGINS` contains the explicit production Loyal
origins and any explicitly approved staging alias, with no localhost entry in
production. The checked-in deployment record must list the exact non-secret
origins.

Required behavior:

- `OPTIONS /events` for an allowed origin permits `GET` and `OPTIONS` and
  headers `Authorization`, `Accept`, `Last-Event-ID`, and `Content-Type`;
- the response echoes only the exact matched origin and includes
  `Vary: Origin`;
- `Access-Control-Allow-Origin: *` and credentialed/cookie CORS are absent;
- an unknown browser origin receives no CORS permission;
- an authenticated request with no `Origin` header can connect for native
  React Native use.

## 4. Replay is identity-scoped in SQL and safe for mobile absence

PASS only if replay SQL applies scope, exact wallet, exact settings PDA, exact
Earn vault, and exact cluster predicates before ordering and limiting. Private
rows with any missing required identity field or cluster must never match.

Required cursor behavior:

- `Last-Event-ID` is primary;
- optional decimal `cursor=<eventId>` is allowed only as a non-sensitive mobile
  fallback; authentication remains header-only;
- conflicting header/query cursors return `400`;
- event IDs are decimal strings in SSE JSON and SSE `id` fields;
- valid cursors replay every retained matching row, then continue live;
- no cursor snapshots the current high-water mark and receives only later rows;
- replay fetches `limit + 1` matching rows; a true per-identity overflow emits
  `resync_required` and closes;
- a cursor older than retained history emits `resync_required` and closes;
- after resync, reconnecting without the stale cursor succeeds;
- more than 500 unrelated global rows after a cursor do not hide a matching
  retained row.

Retention is configurable, defaults to seven days, and has bounded batched
cleanup. Cleanup must not turn an in-range matching cursor into silent data
loss.

## 5. Canonical event envelope and autodeposit state machine are truthful

PASS only if client-facing autodeposit progress uses one event type:

```json
{
  "schemaVersion": 1,
  "eventId": "123456",
  "eventType": "earn.autodeposit.execution.changed",
  "occurredAt": "2026-07-10T00:00:00Z",
  "state": "selected",
  "targetId": "1",
  "scheduledSlotId": "2",
  "executionId": "3",
  "failureCode": "optional_safe_code"
}
```

Allowed states are `scheduled`, `requested`, `selected`, `pull_confirmed`,
`completed`, `failed`, `canceled`, and `released`.

Required semantics:

- one execute-now lifecycle keeps the same `targetId` and `scheduledSlotId`,
  and uses the same `executionId` after allocation;
- `pull_confirmed` means the wallet-to-Earn-vault USDC pull confirmed;
- `completed` is inserted only after Kamino top-up confirmation and successful
  yield deposit/position persistence;
- the final deposit/position records durably link back to the sweep execution;
- each event row is inserted in the same database transaction as the durable
  transition it represents;
- failures expose an allowlisted safe code, never raw exceptions;
- payloads contain invalidation/progress metadata only, never canonical
  balances, full positions, raw evidence, signatures, or secret material;
- duplicate legacy `autodeposit_slot_changed` and old canonical sweep events
  are no longer emitted for the same transition;
- `earn.position.changed`, `earn.transaction.recorded`, and
  `earn.onboarding.changed` continue where applicable.

An integration proof must show `requested -> selected -> pull_confirmed ->
completed` in order and must prove no `completed` row exists before Kamino and
position persistence.

## 6. New private events have complete identity and cluster isolation

PASS only if new forward migrations and database constraints/triggers ensure
private Earn/autodeposit events contain exact wallet, settings PDA, Earn vault
(account index 1), and `solana_env` derived from their owning target/policy.

Required database evidence:

- retained rows are safely backfilled when all identity fields can be derived;
- retained incomplete rows are excluded from delivery and force resync rather
  than acting as wildcards;
- mainnet, devnet, another wallet, another settings PDA, or another Earn vault
  cannot match the token;
- trigger functions emit every correlation and identity field;
- migration checks validate the new columns, indexes, functions, constraints,
  triggers, and retention objects.

## 7. Execute-now wakes immediately without weakening idempotency

PASS only if a committed newly requested scheduled slot emits a small
`pg_notify` wake-up and the production autodeposit worker uses a direct Neon
session to `LISTEN`.

Required behavior:

- notify payload is a wake-up hint only; the worker re-reads durable state and
  claims through the existing locking/claim-token/idempotency path;
- bursts are debounced into one claim cycle;
- duplicate notifications cannot create duplicate execution;
- listener failure reconnects and the periodic poll remains a fallback;
- a missed notification is still claimed by fallback polling;
- request-to-selected latency is recorded and a live canary shows notification
  wake-up instead of waiting for the normal poll interval.

## 8. Runtime protections and observability are operational

PASS only if the deployed realtime service has:

- bounded per-client queues and slow-client disconnect on overflow;
- frequent heartbeats;
- graceful SIGTERM shutdown so clients reconnect/replay;
- `GET /healthz` for process liveness;
- `GET /readyz` that fails when DB connectivity, listener state, or broadcast
  cursor lag is unhealthy;
- independent durable replay/listeners per instance; no correctness dependency
  on one process's memory.

Safe metrics or structured logs must cover active connections, stream lifetime,
`clientKind`, auth failures by reason, expiration closures, listener reconnects,
DB high-water versus broadcast cursor, replay counts, resync reasons, slow
client closures, and request-to-selected-to-pull-confirmed-to-completed latency.
Wallet addresses and other user identifiers must not be metric labels or log
fields.

## 9. Checked-in verification proves adversarial cases

PASS only if focused Rust checks/tests and
`scripts/verify-realtime-sse-smoke.ts` prove at least:

- exact-origin preflight, rejected unknown origin, and native no-origin access;
- query-token rejection and bearer admission;
- invalid signature, expired token, wrong issuer/audience/cluster/identity;
- expiry-driven closure of an already-open stream;
- simultaneous web/mobile streams for one identity;
- cross-user and cross-cluster non-delivery;
- disconnect/replay and stale-cursor resync;
- matching replay after more than 500 unrelated rows;
- correlated execute-now progress through true completion;
- no early completion, no duplicate execution from duplicate notify, and
  successful fallback polling after a missed notify.

Required local commands:

```sh
cargo fmt --check
cargo test -p loyal-yield-realtime-core
cargo check -p loyal-yield-realtime-core -p loyal-yield-realtime \
  -p balance-sweep-autodeposit-trigger
cargo check -p loyal-yield-orchestrator --bin yield-migrations
bun run verify:realtime:render-config
```

No frontend build is required.

## 10. Staged migration, deploy, and live proof are recorded

PASS only if the final checked-in deployment record proves:

- exact forward migration versions, first verified on a safe staging/devnet
  target, then applied before code depending on them;
- production realtime and autodeposit service IDs;
- final image tags and digests for both services;
- direct (non-pooler) Neon LISTEN configuration without recording credentials;
- configured token lifetime, replay limit, retention, heartbeat, queue limit,
  and exact allowed origins;
- the 1Password reference for the shared signing secret, never its value;
- live canary results for health/readiness, CORS, bearer auth, expiry, replay,
  cross-user/cluster isolation, execute-now ordering, notification wake-up, and
  fallback polling;
- latest deploys are live, recent Render error logs are clean, and listener /
  broadcast lag is healthy;
- `render.yaml` matches the known-good live image/config state after rollout.

Adding `sync: false` to `render.yaml` alone does not count as setting an existing
service secret. Render readback must confirm required key names exist, while
never printing their values.

## Required verdict

Return a table with checks 1-10, `PASS` or `FAIL`, and concise evidence. Overall
`PASS` is permitted only if every check passes against the deployed end state.
Repo-only success, unapplied migrations, an unbuilt image, or a green health
endpoint without adversarial canaries is overall `FAIL`.

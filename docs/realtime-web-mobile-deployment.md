# Realtime Web And Mobile Protocol And Deployment Record

This is the routing-owned handoff for Loyal web and mobile SSE clients. It
records the stable protocol and the exact deployed state. SSE is an invalidation
and progress plane only; clients must refetch canonical REST/RPC state.

## Token contract

`GET /events` accepts only `Authorization: Bearer <token>`. Query-string tokens
are rejected. The base64url payload and HMAC-SHA256 signature are separated by
one `.`.

Version 1 claims:

```json
{
  "v": 1,
  "iss": "loyal-apps",
  "aud": "loyal-yield-realtime",
  "iat": 0,
  "exp": 0,
  "walletAddress": "base58 public key",
  "settingsPda": "base58 public key",
  "earnVaultAddress": "base58 accountIndex 1 vault public key",
  "solanaEnv": "mainnet-beta",
  "scopes": ["earn", "autodeposit"],
  "clientKind": "web"
}
```

`clientKind` is `web` or `mobile`; supported clusters are `mainnet-beta` and
`devnet`; supported scopes are `earn`, `autodeposit`, and `onboarding`. All
three identity fields and the cluster are mandatory. `iat <= now < exp`, and
the configured maximum lifetime is 300 seconds. The service supports an
optional `REALTIME_AUTH_PREVIOUS_SECRET` during rotation and closes each stream
at its token expiry.

Signing-secret reference:

```text
1Password Environment: loyal-noncritical-env
Variable: REALTIME_AUTH_SECRET
```

The value is concealed and must never be copied into a repository, client
bundle, mobile binary, URL, log, or deployment record.

## SSE and cursor contract

```http
GET /events
Authorization: Bearer <token>
Accept: text/event-stream
Last-Event-ID: <optional decimal event id>
```

`Last-Event-ID` is primary. Native clients may instead send the non-sensitive
decimal `cursor=<eventId>` query parameter. Conflicting header/query cursors
return `400`. With no cursor, the service snapshots the current high-water mark
and delivers later events. Valid cursors replay retained matching rows and then
continue live. Expired cursors or more than 500 matching replay rows receive
`resync_required` and the stream closes. Clients then do a full canonical
refresh and reconnect without the stale cursor.

Replay predicates are applied in SQL before `limit + 1` using exact scope,
wallet, settings PDA, Earn vault, and cluster. Event IDs and correlation IDs are
JSON strings.

## Browser and native access

Production `REALTIME_ALLOWED_ORIGINS`:

```text
https://askloyal.com,https://www.askloyal.com
```

Browser preflight permits `GET`/`OPTIONS` and `Authorization`, `Accept`,
`Last-Event-ID`, and `Content-Type`. It echoes only an exact configured origin,
sets `Vary: Origin`, and does not enable credentialed CORS. Authenticated native
requests without an `Origin` header are valid. Production has no localhost
origin.

## Event contract

Canonical autodeposit progress uses:

```json
{
  "schemaVersion": 1,
  "eventId": "123456",
  "eventType": "earn.autodeposit.execution.changed",
  "occurredAt": "2026-07-10T00:00:00Z",
  "scope": "autodeposit",
  "state": "selected",
  "targetId": "1",
  "scheduledSlotId": "2",
  "executionId": "3",
  "failureCode": "optional_safe_code"
}
```

States are `scheduled`, `requested`, `selected`, `pull_confirmed`, `completed`,
`failed`, `canceled`, and `released`. `pull_confirmed` means USDC reached the
Earn vault. `completed` is written only after Kamino confirmation and durable
deposit/holding/position persistence, in the same transaction that links the
yield deposit and position to the balance-sweep execution.

Other canonical invalidations remain `earn.position.changed`,
`earn.transaction.recorded`, and `earn.onboarding.changed`. Payloads never carry
canonical balances, full positions, raw exceptions, signatures, or secret
material.

## Retention and runtime configuration

```text
REALTIME_HEARTBEAT_SECONDS=15
REALTIME_CATCH_UP_LIMIT=500
REALTIME_CLIENT_BUFFER=1024
REALTIME_MAX_TOKEN_LIFETIME_SECONDS=300
REALTIME_RETENTION_DAYS=7
REALTIME_RETENTION_BATCH_SIZE=1000
REALTIME_RETENTION_INTERVAL_SECONDS=3600
REALTIME_READY_MAX_LAG=1000
BALANCE_SWEEP_REALTIME_DEBOUNCE_MILLISECONDS=250
```

`/healthz` is process liveness. `/readyz` requires database access, a connected
listener, and bounded database-to-broadcast cursor lag. `/metrics` exposes only
aggregate connection, client-kind, auth-reason, expiry, listener, replay,
resync, overflow, cursor, and autodeposit phase-latency metrics.

## Release record

Source reconciliation before implementation:

- prior live realtime tag: `sha-2712fed78b8f9c79fc5f3e68638ac126a0cd68dc`;
- prior live realtime digest:
  `sha256:7dcf4532450487e4c090c197ac8abf0daf6afff6099bd1df033ceb14e7869a75`;
- prior live autodeposit tag:
  `sha-34a991c257d2349f85a09373b8426e0700c1d32e`;
- prior live autodeposit digest:
  `sha256:cb13c2aaff0ff2019ddbe2fbe1efa09fa08da1966ed9fc68274cc31e8ccc6707`;
- both commits are ancestors of the implementation branch; migrations `0013`
  and `0014` remain unchanged.

Final rollout evidence is filled only after production verification:

```text
Migration: pending production apply (0015 realtime_web_mobile_protocol)
Implementation commit: pending
Realtime image tag/digest: pending
Autodeposit image tag/digest: pending
Realtime deploy ID: pending
Autodeposit deploy ID: pending
Live verifier: pending
Recent Render error logs: pending
Listener/broadcast lag: pending
```

Isolated pre-production evidence:

- final Neon branch `br-noisy-frog-aqv3jhuv` accepted migration 15 through the
  real migration ledger and `--check` validator after the verifier-driven
  trigger and conservative historical-backfill corrections;
- retained-row backfill produced zero deliverable rows with incomplete private
  identity and zero deliverable legacy autodeposit event rows;
- a branch-only lifecycle emitted one each of `scheduled`, `requested`,
  `selected`, and `pull_confirmed` with target `5089`, slot `22112`, and
  execution `5562` once allocated; linking the execution did not duplicate
  `selected`;
- historical execution `5548` proved the atomic true-completion link on the
  isolated branch: execution, scheduled slot `19257`, yield deposit, yield
  position, and final `completed` event shared the same correlation IDs;
- local SSE smoke passed exact CORS, bearer negatives, expiry closure,
  simultaneous web/mobile streams, user/cluster isolation, replay after 501
  unrelated rows, matching replay overflow, and expired-cursor resync;
- the non-executing worker received a three-notification burst as
  `wakeup_count=3` followed by one scan, while periodic polling remained active.

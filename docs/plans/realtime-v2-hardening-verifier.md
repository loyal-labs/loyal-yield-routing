# Realtime V2 Hardening Verifier

Use this as the verifier-first goal for hardening the Yield realtime system after
the first deployed SSE/autodeposit slice.

This verifier checks the end state, not the implementation path. It passes only
when a skeptical runner can prove from repo files, local checks, safe live
smokes, and Render readbacks that realtime semantics are centralized, deployment
posture is self-checking, and the existing autodeposit execution boundary still
holds.

## Goal

The V2 implementation must keep the V1 architecture:

```text
durable DB event row -> pg_notify wakeup -> long-running services -> canonical refetch
```

but make it less fragile by removing duplicated realtime semantics from
individual binaries, replacing shell-quoted smoke ceremonies with checked-in
scripts, and making Render/env posture auditable without printing secrets.

Overall PASS requires every Required Check below to pass.

## Required Checks

### 1. Shared Realtime Core

PASS only if a shared Rust crate owns the common realtime contract used by both
the SSE gateway and autodeposit trigger.

Required shared items:

- default channel `loyal_yield_realtime`;
- default mainnet environment for tokens/events;
- autodeposit scope and scheduled-slot reason constants;
- token claim type and HMAC verification helpers;
- realtime event row and invalidation payload types;
- durable event fetch helpers for latest/min/catch-up/event-by-id;
- notify payload `event_id` parser;
- pooled Neon URL detection/rejection.

`loyal-yield-realtime` and `balance-sweep-autodeposit-trigger` must import these
items from the shared crate instead of defining their own copies.

### 2. Wake-Up Semantics Stay Durable

PASS only if both consumers still treat `LISTEN/NOTIFY` as wake-up only.

Required behavior:

- notify payloads are parsed only for `event_id`;
- consumers re-query `loyal_yield.realtime_events` before acting;
- the SSE service catches up by durable cursor and supports `Last-Event-ID`;
- the autodeposit trigger wakes only for `scope = 'autodeposit'`;
- fallback polling/ticks remain in both services;
- pushed SSE payloads remain invalidations, not canonical balances or execution
  evidence.

### 3. Autodeposit Boundary Preserved

PASS only if V2 does not move money movement into realtime code.

Required evidence:

- `project_surplus_lots_once` still runs before execution scans;
- `execute_eligible_targets_once` still owns execution attempts;
- the executor command remains external and requires
  `--require-lot-claim` in Render posture checks;
- one-shot claim, complete, release, and `--once` modes do not wait for
  realtime notifications;
- requested slots are still selected by durable SQL ordering, not notify arrival
  order.

### 4. First-Class Verification Scripts

PASS only if quote-fragile one-off smoke commands are replaced by checked-in
scripts and package scripts.

Required scripts:

- a Render config verifier that checks realtime and autodeposit service shape,
  immutable image tags, direct Neon host fingerprints, required env names, and
  autodeposit executor safety without printing secrets;
- an SSE smoke verifier that creates a short-lived token from
  `REALTIME_AUTH_SECRET`, omits any cluster claim, opens `/events`, emits a safe
  `loyal_yield.emit_realtime_event(...)`, verifies the live SSE event id/body,
  and verifies `Last-Event-ID` replay;
- both scripts must fail closed when required env vars are missing and must not
  print secrets, tokens, full database URLs, or private keys.

### 5. Render Posture Is Repo-Auditable

PASS only if repo config/docs make the live V2 service shape auditable.

Required evidence:

- `render.yaml` or an explicit deploy/readback script names
  `loyal-yield-realtime` as a web service using the immutable light-worker image
  and command `/usr/local/bin/loyal-yield-realtime`;
- production autodeposit remains an image background worker with command
  `/usr/local/bin/balance-sweep-autodeposit-trigger --execute-eligible`;
- Render checks prove `REALTIME_AUTH_SECRET` is present but never print it;
- Render checks prove `NEON_DATABASE_URL` is direct and host-only in output;
- existing workers are not converted back to Render source/Docker builds.

### 6. Live Smoke And Deployment

PASS only if the V2 commit is built and deployed, or a genuine external blocker
is documented.

Required live evidence when Render/1Password/GitHub are available:

- implementation commit is pushed;
- `worker-images` succeeds for that commit;
- realtime and production autodeposit services are live on the resulting
  immutable `light-workers:sha-<commit>` tag or digest;
- `bun run verify:realtime:sse` passes against the Render URL;
- `bun run verify:realtime:render-config` passes against live Render;
- a safe autodeposit-scoped event wakes the production autodeposit worker and
  logs the normal projection/execution scan summaries.

### 7. Local Checks

PASS only if these checks pass:

```sh
cargo fmt --check
cargo check -p loyal-yield-realtime-core -p loyal-yield-realtime -p balance-sweep-autodeposit-trigger -p loyal-yield-orchestrator --bin yield-migrations
bun run autodeposit:test
bun run verify:realtime:render-config -- --help
bun run verify:realtime:sse -- --help
```

Live DB checks, when run, must use the repo's 1Password pattern:

```sh
op run --env-file=.env.1password -- sh -c 'bun run yield:migrate:check'
```

### 8. No Frontend Wiring

PASS only if this V2 slice does not edit or require frontend app wiring.

Required negative checks:

```sh
git status --short
git diff --name-only -- docs/plans/realtime-v2-hardening-verifier.md crates/loyal-yield-realtime-core crates/loyal-yield-realtime crates/balance-sweep-autodeposit-trigger scripts/verify-realtime-render-config.ts scripts/verify-realtime-sse-smoke.ts scripts/execute-autodeposit-policy.test.ts package.json render.yaml docs/render-worker-images.md Cargo.lock
git -C ../loyal-apps status --short
```

The main repo or sibling app may have unrelated dirty files, but this slice must
not modify them. PASS requires documenting any unrelated dirty files and staging
or deploying only the V2 files. It is FAIL if V2 depends on frontend hooks, token
endpoints, cache invalidation wiring, or any `../loyal-apps` edit.

## Verdict Format

```text
Shared Realtime Core: PASS|FAIL - note
Wake-Up Semantics Stay Durable: PASS|FAIL - note
Autodeposit Boundary Preserved: PASS|FAIL - note
First-Class Verification Scripts: PASS|FAIL - note
Render Posture Is Repo-Auditable: PASS|FAIL - note
Live Smoke And Deployment: PASS|FAIL - note
Local Checks: PASS|FAIL - note
No Frontend Wiring: PASS|FAIL - note
Overall Verdict: PASS|FAIL
```

Overall PASS requires every required section to pass. If this verifier
mis-encodes the production safety goal, update the verifier explicitly and state
why before continuing; do not quietly weaken it to fit an incomplete
implementation.

# September 11 Earn reliability follow-up

## Incident and scope

At 2026-09-11 16:20:29 UTC the autodeposit trigger emitted exactly:
`autodeposit dependency returned a transient server error; execution will retry`
(`autodeposit_dependency_unavailable`, executor exit 27, stage `read_wallet_balance`).
The dependency failed **before pull/claim**; the original record did not retain
its exact HTTP status. This event is not evidence of lost or duplicate funds.
The trigger continued polling and four pulls/four completions were recorded in
the preceding 24 hours, but old scan `succeeded` counters only meant exit zero.

Separate proven stuck work discovered in that investigation:

- Target 4356 / scheduled slot 238750: confirmed pull 84 and top-up 85 already
  have deposit 20980 / holding event 1037717 for execution 10677. Recovery read
  a now-closed vault ATA before checking that accounting. Position 4361 was
  subsequently fully withdrawn; replaying the old finalizer could resurrect it.
- Positive idle balances (including 1–3 raw units and balances below $6) cannot
  pass the direct-deposit custody guard, while the active Rust fleet planner's
  economic floor excludes small drains. Claim-before-preflight repeatedly
  created residual slots; target 7068 alone had 147,533 scheduled rows.
- Fleet submission 26267 / opportunity 551285 / decision 14973 / vault 4132
  remains pending despite a finalized successful transaction at slot 443023824.
  Its latest source collateral is 2 raw units, blocking the strict source-zero
  predicate. `ready_count=0` can mean retry backoff, not completed work.

Main's cleanup reconciliation fix #232 does not address these causes. Go shadow
planner changes in #233 do not prove the active Rust planner drains small idle
balances. Keep the legacy same-mint monitor suspended.

## Idle tolerance

`AUTODEPOSIT_IDLE_TOLERANCE_RAW` (raw USDC units, default 25000000 = $25) is read by
both the executor (`scripts/execute-autodeposit-policy.ts`, direct-deposit idle guard)
and the trigger (`crates/balance-sweep-autodeposit-trigger`, overdue-idle check). Set
it once on the Render service; the trigger passes its environment to the executor it
spawns. The overdue alert only reports vaults whose live idle exceeds this value, so
stale deferral markers on historical slots do not page. Re-tighten it here, not in
code, once executable fleet idle draining ships (ASK-2274).

## Safety boundaries

Initial wallet/vault balance reads have at most three HTTP attempts and an
eight-second whole-read deadline each, including response bodies and backoff.
Only HTTP 500/502/503/504 retry; persistent dependency failures still page with
safe operation/stage/target/slot/status/retry context, never provider URLs/bodies.
No send or post-pull retries are added by this wrapper.

The direct idle guard and fleet economic limits remain. Pre-claim idle deferral
must update the same eligible, unclaimed slot, preserve durable first-blocked
age, and fail closed on ownership races; selected recovery is not released.
This stops new slot amplification, **not the uneconomic drain deadlock**.
Historical row cleanup and a safe economic/custody drain policy need separate
review; do not delete rows, reset claims, re-pull, or ignore dust.

Recovery completion must use exact existing deposit/holding/plan evidence under
the claim lease. Link-only repair must preserve execution 10677, later withdrawal,
position balances/status, and event history. A missing account is never
universally zero. Missing/conflicting evidence must remain pending/fail closed.
Pull and Kamino deposit remain separate Solana transactions (combined size
exceeds Solana's transaction limit).

For a nonzero fleet source only, a bounded finalized RPC receipt must match the
persisted signed transaction bytes, signature and confirmed slot before accepting
the freshly observed source/target state. Existing lease/slot/mint/positive-target
fences remain. No residual is treated as zero, no historical balance is restored,
and no transaction is resent; unavailable or conflicting evidence keeps the
existing durable backoff and `fleet_reconciliation_stalled` alert.

## Completion signals

The trigger opts into completed 28, deferred 29, recovery-pending 30 and noop 31.
Failures 20–27 and not-actionable 23 retain their meanings; legacy exit zero is
`process_success_unclassified`, never a completed deposit. Scan counters expose
each outcome separately; completed is set only after persistence and logging.

Every 15 minutes, a read-only progress query (20-second SQL / 30-second client
bounds) emits `autodeposit_progress_overdue` for selected claims older than
15 minutes or actionable idle work blocked over one hour with at least three
deferrals. Idle detection requires live lots, actual wallet surplus and a live
route owner, using durable first-blocked history, not slot creation/update age.
Normal five-minute retry backoff does not hide already-overdue marked work.
At most one oldest representative per owning stage (five total) pages per pass,
with stable target/slot/stage IDs and affected-target counts. Query failure emits
`autodeposit_progress_check_failed`; ordinary execution continues.

**This is not a stopped-process or hung-child watchdog.** The monitor shares the
trigger loop, whose blocking child `.status()` can prevent another check.
External service-liveness coverage is unchanged; a running process is not proof
of completed deposits or rebalances.

## Verification and release

This code review is not a production repair or deployment. Independent final
review and verification must pass before release. Run the combined targeted
Bun executor/helper tests and lint, isolated PostgreSQL ownership/accounting and
same-slot deferral contracts, trigger child-exit/overdue-query tests, and relevant
Rust fleet external-contract checks. Exercise actual child exit propagation and
OTLP failure mapping, not only enum values. Confirm the light-worker image
includes every imported executor helper.

Local integration gates passed: 121 Bun tests (both PostgreSQL suites executed),
14 trigger tests plus its explicit PostgreSQL query contract, nine targeted fleet
Rust tests, executor/helper ESLint, existing finalized-topup/finalization E2E and
loopback OTLP alert verifiers. An isolated explicit-COPY import smoke check passed;
a full production image build is still a CI/release gate.

After merge, an authorized operator should:

1. Wait for `worker-images` to publish the merged commit's immutable
   `ghcr.io/loyal-labs/loyal-yield-routing/light-workers:sha-<commit>` image.
   Follow [the worker image runbook](../render-worker-images.md), retain
   `runtime: image` and `loyal-ghcr`, and pin the affected trigger/fleet services
   to that verified SHA/digest. Publishing alone does not deploy. Do not switch
   workers to Render Docker builds or reactivate the legacy monitor.
2. Read back deployed image versions, commands and fresh trigger/fleet polling
   logs. Treat polling/`healthy`/empty-ready queues as liveness only. Confirm
   completed versus deferred/recovery-pending/unclassified outcomes agree with
   durable execution state; verify persistent failures still reach ClickStack.
3. Read back target 4356 / slot 238750 and exact linked accounting: recovery must
   complete without another pull, duplicated deposit/event, or changes to the
   later withdrawn position. If exact evidence does not match, stop and escalate;
   do not invoke the old finalizer or reset ownership manually.
4. Verify new idle retries keep the same slot and first-blocked age while delay
   and deferral count advance. Check actionable blocked-work alerts, not just
   absence of transient HTTP errors. Small-balance drain remains unresolved.
5. Verify submission 26267's terminal accounting against its exact persisted
   transaction and confirmed effects, preserving later source/target balances.
   If it remains pending, retain/escalate its stalled alert; never infer that
   2 collateral units are zero or replay the transaction.

Keep production SQL inline and bounded/read-only, for example:

```sh
op run --env-file=/Users/zotho/Dev/loyal/.env.1password.loyal-noncritical-env -- sh -c 'PGOPTIONS="-c default_transaction_read_only=on -c statement_timeout=20000" psql "$NEON_DATABASE_URL" -X -v ON_ERROR_STOP=1 -c "BEGIN READ ONLY; SELECT now(); ROLLBACK;"'
```

Use secret-safe Render/ClickStack/RPC readback, record stable IDs and deployment
SHA, and never print credentials or signed payloads. Any manual stale-row repair
requires its own reviewed evidence-backed procedure and separate authorization.

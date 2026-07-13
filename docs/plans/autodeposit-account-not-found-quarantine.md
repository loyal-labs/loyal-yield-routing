# Autodeposit `AccountNotFound` Quarantine Plan

## Problem

Deterministic Solana `AccountNotFound` failures currently remain retryable. The
executor releases the pre-send claim, restores the claimed lots to `open`, marks
the scheduled slot `failed`, and delays eligibility for five minutes. The same
target can then be selected again even when the missing account cannot recover
without a policy or delegation repair.

This creates a noisy retry loop and repeatedly spends executor capacity on a
known-broken wallet state. The read-only production verification below found
561 released claims from one unique wallet in 48 hours, illustrating that
repeated log and execution records must not be treated as independently
affected wallets.

The relevant current paths are:

- `scripts/execute-autodeposit-policy.ts`: pre-send failure handling, claim
  release, and retry delay;
- `crates/balance-sweep-autodeposit-trigger/src/main.rs`: executable-target
  selection and trigger-side claim release.

## Goal

Quarantine a target when a confirmed, deterministic missing on-chain account
makes autodeposit impossible, while preserving its pending funds and allowing
automatic recovery after the underlying policy or delegation state is repaired.

Transient RPC failures must remain retryable. A missing account must not be
classified from an ambiguous transport error or from a repeated log message
alone.

## Non-goals

- Permanently disabling a wallet or managed vault.
- Dropping, consuming, or silently closing pending surplus lots.
- Treating every executor error as an account-state failure.
- Repairing or recreating Solana accounts automatically.
- Inferring unique affected wallets from repeated Render log lines.

## Proposed Behavior

### 1. Classify the Missing Account

The executor should retain structured context for every account it expects to
read or use, including at least:

- missing public key;
- account role, such as route policy, sweep policy, recurring delegation, token
  account, or other execution account;
- expected owner or program ID when known;
- target, managed-vault, wallet, policy, and scheduled-slot identifiers;
- RPC commitment and observation timestamp.

Before quarantining, confirm the deterministic condition with an explicit
`getAccountInfo`-style read at the chosen commitment. The result must be a
successful RPC response whose account value is null. Timeouts, rate limits,
transport errors, and unavailable RPC nodes remain transient failures.

### 2. Persist a Reversible Blocked State

Add structured execution-health state at the smallest scope that owns the
broken dependency. Prefer the managed-vault or target scope over an individual
scheduled slot because policy and delegation state may be shared across several
slots or target rows.

Suggested fields:

- `execution_status`: `ready` or `blocked`;
- `execution_blocked_reason`: initially `account_not_found`;
- `blocked_account_pubkey`;
- `blocked_account_role`;
- `blocked_at`;
- `blocked_policy_id` or equivalent state/version fingerprint;
- sanitized `last_execution_error` for operator diagnosis.

Use a dedicated reversible state rather than changing the product-level
`active` flag. Preserve the distinction between a user-disabled target and an
operationally blocked target.

If the missing dependency is shared across multiple targets, block all affected
targets consistently in one database transaction. Do not assume that recreating
one scheduled slot or target row repairs shared wallet policy state.

### 3. Stop Normal Selection While Blocked

The trigger's executable-target query must exclude blocked targets before any
claim is taken. This prevents repeated claim/release cycles and removes known
broken targets from normal executor capacity.

On the first confirmed deterministic failure:

1. record the structured failure;
2. mark the current scheduled slot `blocked` if a new status is introduced, or
   `failed` with an explicit non-retryable classification;
3. release the claim without making the target normally eligible again;
4. restore its lots to a pending blocked state so their amounts remain
   attributable and recoverable;
5. emit one structured warning when the block is created.

Subsequent scans should report the target as blocked without launching the
executor or emitting the same error on every poll.

### 4. Recover Only After State Repair

A blocked target may return to `ready` only after a read-only health check proves
that its required state is valid. Recovery should require all applicable checks:

- the previously missing account now exists;
- its owner or program ID matches expectations;
- the active policy/delegation identifier or state version is current;
- recurring delegation and delegate relationships are valid;
- the target still meets normal product and lifecycle eligibility rules.

Recovery can be triggered by a changed policy/delegation fingerprint or by a
low-frequency reconciliation scan. It must not be driven only by the passage of
time. Once recovery succeeds, clear the block atomically and make the preserved
lots eligible for a new scheduled slot.

If the active policy changes while blocked, validate the new policy account
rather than continuing to probe only the old missing public key.

### 5. Preserve Useful Error Evidence

Do not overwrite the actionable executor error with only a generic message such
as `claim released before autodeposit pull`. Store a sanitized structured error
that identifies the missing public key and account role without storing secrets
or raw transaction material.

Monitoring should report:

- newly blocked targets and unique wallets;
- currently blocked targets and unique wallets;
- blocks by missing-account role;
- recovered targets;
- suppressed retry count, if useful;
- oldest block age.

Counts must come from database target/execution rows, not repeated Render log
lines.

## Error Classification

| Condition | Classification | Action |
| --- | --- | --- |
| Successful RPC read returns no account | Deterministic | Block target |
| Account exists with wrong owner/program | Deterministic state mismatch | Block under a distinct reason |
| RPC timeout, 429, transport, or node failure | Transient | Release and retry with backoff |
| Blockhash expiry or ambiguous send result | Transaction/RPC | Use existing confirmation and retry rules |
| Database has no active policy row | Configuration/state | Fail before claim where possible |
| Database policy row points to a closed account | Deterministic | Block and require policy repair |

## Implementation Sequence

1. Add structured error extraction to the TypeScript executor, including the
   missing public key and account role.
2. Add the reversible blocked-state schema and indexes required by target
   selection and reconciliation.
3. Update the failure transaction to persist the block, release the claim, and
   preserve lots without immediate retry eligibility.
4. Exclude blocked targets in the Rust trigger's executable-target query.
5. Add a read-only reconciliation path that validates repaired on-chain state
   and atomically unblocks eligible targets.
6. Add structured monitoring for new blocks, active blocks, and recoveries.
7. Deploy schema support first, then executor classification, then trigger
   exclusion/reconciliation so mixed worker versions remain safe.

## Rollout Safety

- Start by recording the proposed classification without suppressing retries to
  confirm the missing-account role and affected scope.
- Compare execution-row counts with unique-wallet counts to detect accidental
  over-grouping.
- Enable quarantine only after the observed classifier distinguishes confirmed
  null accounts from RPC failures.
- Keep an operator-controlled unblock path, but make it run the same on-chain
  validation as automatic recovery.
- Do not auto-recreate policies, delegations, or token accounts as part of this
  change.

## Acceptance Criteria

The plan is complete when all of the following are demonstrated:

1. A confirmed missing route-policy or delegation account blocks the affected
   target after its first deterministic failure.
2. The blocked target is not claimed or executed again on subsequent trigger
   polls.
3. Its pending lot amounts remain stored and attributable while blocked.
4. A transient RPC failure does not create a persistent block.
5. Recreating or replacing the required account and passing validation unblocks
   the target and permits a later successful execution.
6. Shared broken policy state blocks every affected target without blocking
   unrelated wallets.
7. Monitoring reports execution counts and unique-wallet counts separately.
8. Existing claim-token ownership and no-double-execution guarantees remain
   intact.
9. No production transaction, deploy, restart, or account repair is performed
   by the verification procedure itself.

## Open Verification Item

Closed by the read-only verification on 2026-07-13 UTC: the currently active
same-mint route-policy row points to
`45XAg7C8Uhxn6jkSnzm9Q7sSvDbvQ9PEUXjzjm78TvL6`, and a direct confirmed
`getAccountInfo` response returned `value = null` at slot `432703041`. The
current sweep-policy and recurring-delegation accounts returned non-null
accounts owned by their expected programs. No production state was changed.

## Read-only Production Verification

The verification observed these live boundaries on 2026-07-13 UTC:

- `loyal-balance-sweep-autodeposit-trigger` was live on deploy
  `dep-d9ahaai8qa3s73appmvg`, instance
  `srv-d8lplql7vvec73f1it6g-q76ql`, command
  `/usr/local/bin/balance-sweep-autodeposit-trigger --execute-eligible`, and
  immutable image `light-workers:sha-5fc5f7825a717cfb0ccaab57cee5e38f09aa7f53`.
- `loyal-same-mint-yield-monitor` was live on deploy
  `dep-d98i3g3eo5us73dibkog`, instance
  `srv-d8n7gqbbc2fs73emk610-xwm46`, its expected execute/poll command, and
  immutable image `light-workers:sha-8a930e16482378ab11c9ff94ab3677a50b028b35`.
- The autodeposit service emitted 561 `AccountNotFound` records for the route
  policy above from `2026-07-12T17:23:03Z` through
  `2026-07-13T19:16:16Z`. The same missing policy also appeared in the
  same-mint monitor's recent failure set.
- Neon linked the missing route policy to active target `6016`, managed vault
  `6666`, and one unique wallet. The target's sweep policy and recurring
  delegation were still present and active.
- In the preceding 48 hours, that target had 561 released claims, no completed
  execution or execution-lot rows, and 25 open lots preserving `11387152` raw
  units. The released claims summed to `4171160941` raw units because the same
  funds were repeatedly claimed and restored.
- The latest worker version preserved the actionable same-mint top-up error on
  58 slot failures; 503 earlier failures contained only the older generic claim
  release message.

These rows and the confirmed-null chain read establish the causal chain:
an active Neon route-policy reference points to a closed/missing Solana policy
account; both the rebalance and autodeposit top-up paths consume that reference;
the autodeposit executor claims pending lots before its top-up preflight fails,
then releases them for another attempt.

## Implemented Operations

Migration `0017_autodeposit_account_not_found_quarantine.sql` adds a minimal
target-local block reason, structured JSON evidence, check/recovery timestamps,
one scan index, the `blocked` scheduled-slot status, and the DB-backed
`balance_sweep_execution_block_metrics` view. It does not add a separate block
table. The executor persists confirmed-null evidence and marks every active
target sharing the exact dependency in the same claim-release transaction.
Both the outer trigger scan and the Rust and TypeScript claim transactions
exclude marked targets.

Automatic reconciliation runs at most once per
`BALANCE_SWEEP_ACCOUNT_NOT_FOUND_RECONCILE_SECONDS` interval, defaulting to 300
seconds. It and the manual mode use the same read-only Solana validation for the
current route policy, sweep policy, recurring delegation relationship, and
token accounts before atomically recovering the DB block and rescheduling its
preserved lots. Neither recovery mode sends a Solana transaction.

After an approved policy/delegation repair, an operator can request the same
target-scoped validation manually:

```sh
op run --env-file=/Users/zotho/Dev/loyal/.env.1password.loyal-noncritical-env -- \
  sh -c 'bun scripts/execute-autodeposit-policy.ts --recover-account-not-found-target-id <target-id>'
```

This command mutates recovery and scheduling rows in the configured database,
so running it against production requires explicit approval even though its
Solana access is read-only. Monitoring reads from:

```sql
SELECT *
FROM loyal_yield.balance_sweep_execution_block_metrics
ORDER BY account_role;
```

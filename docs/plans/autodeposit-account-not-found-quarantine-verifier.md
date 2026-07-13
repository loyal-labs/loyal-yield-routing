# Autodeposit `AccountNotFound` Quarantine Verifier

Use this as the verifier-first goal for implementing
`docs/plans/autodeposit-account-not-found-quarantine.md`.

This verifier has two verdicts:

- `IMPLEMENTATION PASS` means the repository is safe to publish and roll out.
- `ROLLOUT PASS` means the migration and worker image have later been deployed
  with approval and the live readbacks pass.

The implementation task may finish with `IMPLEMENTATION PASS` and
`ROLLOUT PENDING`. Do not mutate production while running the implementation
checks.

## Required implementation checks

### 1. Structured reversible state

PASS only if a registered Yield migration adds a minimal target-scoped block
state without changing product-level `active` or lifecycle state:

- nullable reason `account_not_found`, where non-null means blocked;
- structured JSON evidence containing managed-vault, policy, scheduled-slot,
  missing-account, role, expected-owner, fingerprint, commitment, slot/time,
  and sanitized actionable-error fields;
- block, recovery-check, and recovered timestamps/error state;
- an index used by target selection and recovery scans.

The migration must add `blocked` to the scheduled-slot status enum and expose a
DB-backed monitoring view with separate new, active, and recovered target and
unique-wallet counts, grouped by missing-account role.

The migration must be registered in both repository migration paths and their
schema validation.

### 2. Deterministic classification

PASS only if the TypeScript executor extracts an `AccountNotFound` pubkey,
matches it to a known target dependency role, and performs an explicit
`getAccountInfo(pubkey, "confirmed")` before creating a block.

- A successful RPC response with `value === null` is deterministic and may
  block.
- A timeout, 429, transport error, thrown RPC error, or non-null account is not
  classified as `account_not_found` and remains retryable.
- Unknown pubkeys are not quarantined under an invented role.
- The stored error and RPC URL are sanitized; no secret or raw key material is
  persisted.

Focused tests must cover confirmed null, non-null, thrown/transient RPC, known
role mapping, and unknown pubkey behavior.

### 3. Atomic block and lot preservation

PASS only if the first confirmed deterministic pre-send failure atomically:

- restores the selected claim's lots to `open` with the exact amount capped at
  each lot's original amount;
- releases the selected claim;
- marks every target sharing the broken dependency without blocking unrelated
  wallets;
- marks the failed scheduled slot `blocked` and keeps the actionable error;
- never replaces that error with only `claim released before autodeposit pull`.

The block must not alter the product-level target `active` or lifecycle fields.

### 4. Exclusion before and during claims

PASS only if active blocks are excluded in all of these places:

- the Rust trigger's executable-target selection;
- the Rust one-target claim transaction;
- the TypeScript claim transaction;
- the executor target load or an equivalent race-safe pre-claim guard.

The check must be inside the claiming transaction as well as the outer scan so
a concurrent block cannot produce a new selected claim.

### 5. Validated automatic and manual recovery

PASS only if the worker has a low-frequency automatic reconciliation mode and
an explicit target-scoped manual mode. Both must call the same validation code
and must not send Solana transactions.

Recovery must reload the current dependency, so a changed active policy is
validated instead of probing only the old missing pubkey. It must require:

- target product/lifecycle eligibility;
- current route policy, sweep policy, and recurring delegation accounts to
  exist;
- each known account owner/program to match;
- the recurring delegation layout/discriminator checks to pass;
- the current dependency fingerprint to match the account just validated.

Only then may one DB transaction mark the block recovered and reschedule the
preserved open lots. A transient validation error, missing account, wrong owner,
stale fingerprint, or inactive target must leave the block active.

### 6. Focused verification

Run from the repository root:

```sh
bun run autodeposit:test
NO_DNA=1 cargo fmt --check
NO_DNA=1 cargo check -p balance-sweep-autodeposit-trigger
NO_DNA=1 cargo check -p loyal-yield-orchestrator --bin yield-migrations
bun run lint -- scripts/execute-autodeposit-policy.ts scripts/execute-autodeposit-policy.test.ts
git diff --check
```

Also inspect the changed SQL and claim paths:

```sh
rg -n "account_not_found|execution_block|getAccountInfo|blocked|recovered|unique_wallet|claim released before" \
  crates/balance-sweep-autodeposit-trigger \
  crates/loyal-yield-orchestrator/migrations \
  crates/loyal-yield-orchestrator/src/bin/yield-migrations.rs \
  crates/loyal-yield-orchestrator/src/store.rs \
  scripts/execute-autodeposit-policy.ts \
  scripts/execute-autodeposit-policy.test.ts
```

`IMPLEMENTATION PASS` requires every section above and every focused command to
pass. If a repository-wide command fails for an unrelated pre-existing reason,
record the exact failure and still require the changed-file checks to pass.

## Required rollout checks

Run these only after explicit approval for the migration and worker-image
rollout.

1. Apply the new Yield migration before either worker image changes.
2. Deploy an immutable `light-workers:sha-<commit>` image to
   `loyal-balance-sweep-autodeposit-trigger`, then to
   `loyal-same-mint-yield-monitor` if its shared image is intentionally updated.
3. Read back the Render deploy, image, command, instance, and startup logs.
4. Query the block table and monitoring view to prove that one deterministic
   missing route policy produces one active target block and one unique wallet,
   with preserved open lots and no new claims on later polls.
5. Prove a transient RPC fixture or controlled staging failure creates no block.
6. After an approved policy/delegation repair, run the manual validated recovery
   first, confirm the block is recovered and its lots are scheduled, then leave
   automatic reconciliation enabled.

`ROLLOUT PASS` requires those live readbacks. A merged PR, green image build, or
successful migration alone is not enough.

## Verdict format

Report every numbered section as `PASS`, `FAIL`, or `PENDING APPROVAL`, followed
by:

```text
IMPLEMENTATION: PASS|FAIL
ROLLOUT: PASS|FAIL|PENDING APPROVAL
```

## Implementation Run: 2026-07-14

1. `PASS` — migration 0017 adds only target-local block/evidence/check fields,
   one partial scan index, the blocked slot enum value, and the monitoring view;
   both migration registries and schema validation include it.
2. `PASS` — known-role extraction requires a successful confirmed-null
   `getAccountInfoAndContext` response; non-null, thrown/transient, and unknown
   account cases remain retryable in focused tests.
3. `PASS` — the claim-release statement caps lot restoration, releases the
   claim, marks every exact-dependency target, and preserves the detailed error
   and blocked slot in one statement without changing target product state.
4. `PASS` — target marks are checked in the Rust scan, Rust target/slot claim
   transaction, TypeScript target load, and TypeScript claim transaction.
5. `PASS` — automatic and manual modes share current-policy validation,
   validate account owners and the recurring-delegation relationships, send no
   Solana transactions, and recover the target/slots/lots atomically.
6. `PASS` — all focused commands passed. Migration 0017 and the four modified
   TypeScript SQL statements also executed or parse-prepared successfully on a
   fresh throwaway local PostgreSQL schema. Targeted strict TypeScript checking
   passed.

```text
IMPLEMENTATION: PASS
ROLLOUT: PENDING APPROVAL
```

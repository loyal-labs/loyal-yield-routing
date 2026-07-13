# Durable Autodeposit Pull and Top-Up Execution

## Outcome

ASK-1731 is fixed by treating the wallet pull, Kamino top-up, and application persistence as one durable database-owned execution. Every Solana transaction is built and signed first. Its deterministic signature, blockhash, last-valid block height, and exact signed bytes are persisted under a fenced lease before those bytes can be broadcast.

Recovery always reconciles the persisted signature before it may construct another transaction. Pulls are never replaced automatically. A top-up is replaced only after the previous signature is proven failed or expired without landing, and every attempt remains append-only audit evidence.

This PR changes code and schema only. Applying the production migration, deploying workers, and recovering historical production executions are separate, explicitly authorized rollout operations.

## Failure being addressed

The previous executor performed two independent irreversible operations:

1. Pull subscription USDC from the user's wallet ATA into the Earn vault ATA.
2. Deposit USDC from that vault into Kamino through the same-mint route policy.

The pull could land before the process recorded enough information to recover, and the top-up subprocess treated some ambiguous RPC/confirmation errors as terminal. Retrying the outer job could therefore consider another pull, while the idle-vault worker could independently consume the credited vault balance.

Read-only production evidence gathered for ASK-1731 on 2026-07-13 showed recent `BlockhashNotFound`, ambiguous confirmation, and vault-balance-race partial executions. Those observations establish urgency, but this PR makes no claim that production rows were repaired.

## Atomic composition decision

Atomic pull plus Kamino deposit is not available in the deployed policy architecture:

- `prepareEarnUsdcAutodepositPull` in `@loyal-labs/smart-account-vaults` builds a transaction authorized by the subscription sweep policy and recurring delegation.
- `same-mint-reserve-swap` builds the Kamino deposit as a separate Squads ProgramInteraction payload authorized by the active same-mint route policy.
- The two operations use different policy accounts, constraint families, payload construction paths, and independently signed outer Solana transactions.
- No current action or policy builder can compose both authorization families into one Squads payload. Adding or deploying such a policy is outside ASK-1731 and would itself require policy creation, packet/compute proof, and rollout.

The code therefore implements the durable saga below rather than assuming a combined transaction can be authorized.

## Authoritative lifecycle

`balance_sweep_executions.lifecycle_state` is the authoritative state machine:

```text
pull_confirmation_pending
  -> deposit_pending
  -> deposit_confirmation_pending
  -> deposit_confirmed
  -> completed

ambiguous persisted signature
  -> needs_reconciliation
  -> its evidence-derived next state
```

There is deliberately no durable `pull_pending` row. A new execution becomes visible only when it already owns a signed pull attempt.

`balance_sweep_execution_attempts` is the append-only external-attempt ledger. Each row records:

- execution, operation (`pull` or `top_up`), and monotonically increasing attempt number;
- deterministic signature, blockhash, last-valid block height, and exact signed transaction bytes;
- lease owner and fence that authorized persistence;
- classification: `prepared`, `landed`, `failed`, `expired_not_landed`, or `unknown`;
- broadcast time, confirmed slot, error, and chain evidence.

An execution points to its successful top-up attempt. Failed, expired, and unknown attempts are never cleared or overwritten. Uniqueness permits only one landed attempt for each operation on an execution and prevents a signature from being attached twice.

Legacy fields are projections of this lifecycle, not a second state machine:

| Durable fact | Legacy/application projection |
| --- | --- |
| Pull landed | `signature`, `slot`, balance evidence, `amount_raw`, `confirmed_pull_amount_raw`, `decoded_evidence.status = durable_deposit_pending` |
| Top-up landed | `kamino_deposit_signature`, successful attempt id, `decoded_evidence.status = durable_deposit_confirmed` |
| Application linkage complete | existing `mark_autodeposit_execution_completed`, then lifecycle `completed` and `decoded_evidence.status = executed` |
| Recoverable/ambiguous | lifecycle plus append-only attempt classification; `completion_failure_code` is descriptive only |

## Transactional schema rollout

Migration `0017_durable_autodeposit_execution.sql` executes as one transaction.

1. It removes the old execution insert trigger before any prepared row can be misreported as a confirmed pull.
2. It adds lifecycle, top-up target/policy, reservation, successful-attempt, shared lease, and append-only attempt storage.
3. It recovers real `claim_token` and `scheduled_slot_id` values from existing claim/slot linkage.
4. It classifies a historical row as `completed` only when a matching Kamino deposit and holding event prove the complete application linkage.
5. Every historical row without that complete linkage becomes `needs_reconciliation`; none is exposed as a new pull or blind `deposit_pending` retry.
6. Historical `partial_executed_pull_top_up_blocked` amounts are reserved. Rows already marked `partial_executed_pull_idle_vault_deposited` are not double-reserved and still require reconciliation.
7. Existing pull and successful top-up signatures are inserted as landed historical attempts without inventing missing signed bytes.
8. Compatibility triggers map an older worker's confirmed insert into `needs_reconciliation`, reserve its amount, and capture its landed pull attempt. An older completion update maps into authoritative `completed`. This keeps the migration-before-image rollout safe.
9. Uniqueness uses real identifiers: claim token, scheduled slot, signature, successful deposit execution, and successful Kamino signature.

The production migration must be applied before the new images start using the new tables. This PR does not run it.

## Shared fencing and durable reservation

Both workers coordinate through `vault_operation_leases`, keyed by `(cluster, vault_pubkey)`.

- Acquisition creates or increments a monotonic fence only after the prior lease expires.
- Renewal, release, and every persist-before-send transition compare owner, fence, and expiry.
- A stale owner cannot append a pull/top-up attempt or create a different signed transaction.
- A fresh autodeposit pull cannot acquire the vault while any non-terminal execution exists. A matching claim/slot recovery may acquire it only when no different execution has an active signed attempt.
- An active persisted autodeposit attempt blocks the idle-vault worker from taking over an expired lease.
- The matching autodeposit recovery owner may take over and must reconcile that attempt first.
- Before an idle-vault obligation-setup or deposit send, the Rust worker stores the exact signature, blockhash, last-valid block height, and signed bytes in the lease row. While that blocking evidence exists, no expired lease takeover is allowed. Setup evidence is cleared only after its exact signature confirms; value-moving deposit evidence is cleared only after confirmation and database reconciliation. An ambiguous send remains blocked for explicit reconciliation.

The lease is coordination, not ownership of funds. Durable ownership is `reserved_amount_raw` on each non-terminal execution. After a pull is confirmed, the exact credited amount is reserved until top-up confirmation. `reserved_autodeposit_amount_raw(cluster, vault, mint)` sums those reservations without mixing clusters.

Both idle-balance discovery and idle-deposit execution subtract that function from the live/recorded vault balance. The executor rechecks the live amount and reservation under the shared lease before it builds the value-moving transaction. Thus process death does not make a confirmed autodeposit credit available to an unrelated idle-vault decision.

## Exact execution algorithm

### Start or recover

The trigger prioritizes non-terminal durable executions before ordinary requested/scheduled slots and reuses their claim token. It does not create another claim. The executor loads the existing execution before wallet-balance/no-op logic, including when the currently active route policy has changed. The original top-up policy metadata is stored on the execution and passed explicitly to the Rust builder, which selects that exact policy account or fails closed.

The non-executing CLI path does not acquire a lease, write execution state, or recover/broadcast a pending signature. Dry-run remains planning-only.

Live execution requires the real lot-claim token and scheduled-slot id. The executor refuses an unowned `--execute` invocation rather than creating an execution that cannot be found deterministically after a process death.

### Pull

1. Build and sign the pull with a fresh blockhash.
2. Derive and validate its deterministic signature from the signed transaction.
3. In one fenced SQL statement, lock the selected claim and slot, insert the execution, link claim/slot ownership, and insert the signed pull attempt as `prepared`.
4. Reconcile that signature before any send. If it already landed, continue from evidence.
5. If still unknown and unexpired, re-check the fence and broadcast the exact persisted bytes. Recording `broadcast_at` may happen after the RPC call because the signed transaction and signature already exist durably; a crash cannot create a missing-signature window.
6. Read the confirmed transaction's token balance metadata. Require the expected mint/accounts, a positive wallet debit, and an equal vault credit.
7. Persist the confirmed slot and balance evidence, replace `amount_raw` with the actual vault credit, reserve that credit, consume/link the claim and scheduled slot, and advance to `deposit_pending`.

A failed or expired pull becomes `needs_reconciliation` and is never automatically replaced.

### Kamino top-up

1. Use only `confirmed_pull_amount_raw`; never use intended amount or total vault balance.
2. Before any pull execution is persisted or broadcast, run the same-mint builder in dry-run mode and require a fully simulated deposit transaction. A missing Kamino obligation, `missingObligationSetup`, any preflight blocker, or a skipped deposit simulation fails closed with zero pull sends. Obligation setup remains an explicit prerequisite; this PR does not create an untracked setup transaction.
3. After the pull confirms, run the same builder with a fresh blockhash. The Rust JSON response includes the deterministic signature and exact signed transaction.
4. Validate the response signature from the serialized transaction.
5. Append the new top-up attempt and transition to `deposit_confirmation_pending` in one fenced CAS that requires `deposit_pending` and no existing replayable top-up attempt.
6. Reconcile before broadcast, then broadcast only the exact persisted bytes.
7. Confirm success from signature status and confirmed transaction token balances. The vault debit must equal the confirmed pull credit.
8. On success, classify the attempt landed, reference it as the successful attempt, clear the reservation, and enter `deposit_confirmed`.
9. On `failed` or `expired_not_landed`, retain the attempt and return to `deposit_pending`. A later invocation may append one fresh signed top-up attempt.
10. On `unknown`, retain the same active attempt in `needs_reconciliation`; no replacement is allowed.

`BlockhashNotFound` is treated as ambiguous until the persisted signature is reconciled. Only a definitively non-landed top-up gets a fresh blockhash. The pull is never revisited.

The Rust CLI emits serialized signed bytes only when the durable executor passes its explicit machine-output flag. Ordinary human dry runs do not print replayable transactions, and summarized subprocess logs redact that field.

### Application persistence

`deposit_confirmed` performs no Solana operation. It invokes `record_durable_autodeposit_yield_deposit`, one PostgreSQL statement whose PL/pgSQL body holds an advisory transaction lock and atomically writes the unique deposit, position amount/principal, holding event, and final event linkage. Any exception rolls back every boundary. A retry either sees the complete signature-linked evidence and validates it or performs the complete transition; it cannot observe an incremented principal with a stale current amount. The existing completion function then links the execution and transitions it to `completed`.

If the chain succeeds and any database write fails, the execution remains `deposit_confirmed`; recovery retries database persistence only.

## Verification evidence

The implementation is accepted only with all of the following:

- `scripts/durable-autodeposit-execution.test.ts` injects one crash after pull/top-up construction, attempt persistence, broadcast, chain landing, confirmation persistence, and before completion persistence. Repeated recovery completes with one landed pull.
- Tests prove a landed confirmation timeout is recognized without another send.
- Tests prove a definitive top-up expiry retains history and permits one new attempt.
- Tests prove `BlockhashNotFound` replaces only the top-up and never the pull.
- Tests prove the top-up/application amount comes from confirmed pull evidence, not requested amount.
- Tests prove a stale fence cannot append or broadcast a competing top-up.
- Tests prove database failure after chain success causes only completion persistence to rerun.
- `test-durable-autodeposit-application-persistence.sql` injects database exceptions after the deposit insert, existing-position update, holding-event insert, and final linkage update. Every injected boundary rolls back completely; the successful call and duplicate retry produce one deposit, one correctly valued event, and one position update.
- The pre-pull obligation test proves an absent obligation sends zero pulls and a later fully preparable invocation completes with exactly one pull.
- Rust compilation covers the shared lease/send-intent and reservation checks used by the idle worker.
- Migration 17 is executed against an isolated PostgreSQL 17 database from both an empty compatible schema and fixtures representing completed legacy, blocked partial, idle-consumed partial, and new prepared rows.
- The focused Bun tests, ESLint, TypeScript check, Rust checks/tests required by `AGENTS.md`, and `git diff --check` pass.

Final local evidence on 2026-07-13:

- focused Bun suites: 40 passed, 0 failed, including the missing-obligation pre-pull gate;
- `bun run lint` and the targeted ES2022 TypeScript check: passed;
- same-mint binary tests: 17 passed; orchestrator library tests: 14 passed; trigger tests: 3 passed;
- targeted Rust checks and `cargo fmt --all -- --check`: passed (one pre-existing `swap_lanes` dead-code warning);
- migration runner `--check`: migrations 1-17 up to date on the isolated compatible-schema database;
- synthetic SQL: completed/partial/new backfill, legacy-worker compatibility, fresh-pull exclusion, matching recovery, stale fence, and persisted-signature takeover assertions all passed;
- application-ledger SQL fault injection: all four boundaries rolled back, then success and duplicate retry produced one matching deposit/event/position linkage;
- `git diff --check` and changed-file credential-pattern scan: passed.

The unrelated root Next.js build is not a release gate for these worker surfaces. It compiled successfully, then hit the existing repository TypeScript target error at `packages/loyal-actions/src/constants.ts:81` (`BigInt` literal with a target below ES2020); this PR does not change that package or the root target.

## Rollout and recovery boundary

After merge, an explicitly authorized rollout should:

1. Back up/inspect current linkage and duplicate assumptions read-only.
2. Apply migration 17 transactionally.
3. Deploy compatible autodeposit trigger/executor, same-mint monitor, and same-mint reserve-swap images together.
4. Observe new executions before enabling any historical recovery.
5. Reconcile historical partial executions individually from chain evidence, then enable broader recovery only with operator approval.

None of those production actions, including migration application or recovery of known executions, is performed or claimed by this PR.

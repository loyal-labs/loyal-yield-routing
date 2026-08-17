# Autoswap web rollout verifier

This is the standing verifier for making cross-mint Earn optimization safe and
clear for a dark web rollout. Run it cold against both repositories, a
disposable PostgreSQL database, finalized mainnet RPC, and the deployed app and
workers. Return `PASS_AUTOSWAP_USER_TEST_READY` only when every required gate
passes. A rendered toggle, successful policy create, simulation, submitted
signature, or healthy deployment is not sufficient.

The implementation stays deliberately small. There are three durable user
states and no generic lifecycle framework:

```text
off     = no enrollment row
on      = enrollment enabled and its exact two policy accounts are canonical
paused  = enrollment disabled while those policies remain installed
```

`installing`, `finalizing`, `pausing`, `resuming`, and `deleting` are transient
web-operation states. Policy-catalog health is evidence, not another source of
user intent.

## V0 — Scope and user journey

Required:

- Autoswap is available only for an authenticated wallet with an Earn position
  and the server-side rollout allowlist permits setup. Missing/blank rollout
  configuration fails closed.
- A wallet with an existing enrollment can still view, pause, and delete it if
  rollout visibility is later removed.
- The Earn card matches Autodeposit: its switch directly pauses/resumes; the
  settings button opens configuration; delete is available only inside the
  settings pane and requires an explicit second confirmation.
- Setup asks for one understandable value: a daily USD cap applied independently
  to each supported source stablecoin. Copy says "per stablecoin".
- V1 uses the verified 50 bps maximum slippage. The web does not offer 100/200
  bps controls that the worker will not use.
- Installed risk limits are read-only. Changing them requires delete/recreate.
- Setup explains two wallet approvals without presenting the two policy shards
  as a product concept.

## V1 — One enrollment record, exact policy identity

Required:

- `cross_mint_vault_opt_ins` remains the single user-intent record. It stores
  vault identity, `enabled`, immutable cap/slippage, generation, and the exact
  account+seed for the classic and Token-2022 policies.
- Row absence means off; `enabled=false` means paused. No status enum, shadow
  enrollment table, or frontend-only durable state is added.
- Both policy identities are distinct, positive, and immutable for the row's
  lifetime.
- Setup confirmation records the row only after finalized strict readback of
  both policies. Replaying setup confirmation never resumes a paused row.
- Idempotent pause/resume returns the existing state without advancing
  generation. A real transition advances generation exactly once. Stale
  expected-generation writes fail.
- Planner activation and initial-withdraw publication require the exact bound
  source-shard policy, matching risk settings, and `enabled=true`.

## V2 — Pause and recovery safety

Required:

- Pause is authenticated and signatureless. The committed disabled row is
  visible before the response returns.
- Pausing before planning, after lease, or before initial publication prevents
  a new withdrawal; stale generation cannot win activation.
- Pausing after a finalized withdrawal does not block continuation,
  reconciliation, target deposit, source recovery, or quarantine. The existing
  `continue_or_recover_existing` control remains independent.
- The card optimistically shows `Pausing...`/`Resuming...`, reverts on failure,
  and settles to `Paused`/`On` without opening the settings pane.
- Resume is signatureless but strict: both bound policy accounts must decode at
  finalized commitment to the enrolled vault, signer, shards, cap, slippage,
  and custody constraints. Missing, modified, or ambiguous policies fail
  closed and remain paused.

## V3 — Deletion and interrupted cleanup

Required:

- Delete commits pause before preparing any on-chain removal.
- Delete preparation refuses with typed `movement_in_progress` while any
  nonterminal cross-mint decision exists for the vault.
- One wallet transaction removes the exact two bound policies. Wallet rejection
  or send/finality failure leaves Autoswap paused and retryable.
- Confirmation requires a finalized successful signature and finalized absence
  of both exact accounts before deleting the enrollment row.
- If the removal lands but browser/API confirmation is lost, retry/reconcile
  proves the same finalized absence and reaches off without another transaction.
- Deletion never depends on rediscovering a policy pair from matching cap values.

## V4 — Authorization, limits, and dark controls

Required:

- Every route derives wallet/settings/vault identity from the authenticated
  principal; request bodies cannot select another wallet or vault.
- Setup and confirmation enforce a cap from $1 through $1,000 per stablecoin
  for the dark rollout and exactly 50 bps slippage. Raw amounts remain integer
  base units and fit PostgreSQL/Squads bounds.
- The server-side enrollment allowlist is enforced by setup APIs as well as the
  UI. Invalid, duplicate, or empty-interior entries fail closed.
- The production database `start_new_movements` gate remains a separate global
  kill switch. Closing it blocks new work but leaves recovery running.
- No secret, signed wire bytes, RPC URL, or production-user state enters source,
  logs, test artifacts, or chat.

## V5 — Focused verification

Required local evidence:

- Rust formatting/checks for touched crates and `bun run verify:cross-mint:store`
  pass against a disposable database.
- Database scenarios prove setup replay, pause/resume idempotency, stale
  generation, exact policy binding, pause-before-withdraw, pause-after-withdraw
  continuation, active-movement delete rejection, and finalized deletion.
- `packages/loyal-actions` tests/typecheck and
  `packages/smart-account-vaults` typecheck pass; policy fingerprints remain
  identical to the already-finalized 30-pair evidence.
- Web lint and focused typecheck pass. Per `loyal-apps/AGENTS.md`, do not run a
  local frontend production build; Vercel is the production-build gate.
- TypeScript tests exist only for external contracts that static checks miss:
  auth/rollout rejection, persisted state transitions, stale-generation
  conflict, movement-safe deletion, and finalized-chain confirmation. UI copy,
  field mirrors, mocked JSON shapes, and implementation call order do not get
  unit tests.
- An authenticated API E2E covers off -> setup -> finalizing -> on -> paused ->
  on -> delete-cancel -> paused -> delete -> off, including interrupted setup,
  stale generation, movement-blocked deletion, and lost-confirmation recovery.
  It uses an Autoswap-only read route so verification cannot trigger unrelated
  Earn or Autodeposit reconciliation.
- A focused rendered check covers the Autoswap controls, copy, optimistic
  rollback, settings, and two-step delete interaction on desktop and narrow
  layout; the webpage is not used as the lifecycle oracle.

## V6 — Deployed dark and mainnet evidence

Required:

- Migration, pinned worker image, and web code are deployed with enrollment
  hidden and `start_new_movements=false`; current Earn and Autodeposit journeys
  still work.
- An allowlisted disposable smart account creates both policies, strictly reads
  them at finalized commitment, pauses and proves zero new starts, resumes, and
  completes two bounded finalized canaries: one classic source and one
  Token-2022 source.
- Each canary advances only from movement-attributed finalized source debit,
  target credit, and deposit debit. Predicted output and aggregate ATA balances
  do not count.
- A controlled pause after withdrawal reaches safe completion or recovery.
- Deletion is blocked during that movement, then succeeds after terminalization;
  both policy accounts are absent and the enrollment row is gone.
- Recent Render/Vercel logs have no migration, auth, policy, RPC, Jupiter,
  reconciliation, panic, or retry-loop regression. Rollback is documented as:
  hide setup and close new starts; never disable recovery or auto-delete user
  policies.

## Nice to have, not required for the first web cohort

- Mobile Autoswap UI.
- Editing an installed cap without policy recreation.
- User-selectable slippage profiles beyond the verified 50 bps envelope.
- Push notifications for pause/resume or completed optimization.

## Verdict

Report V0-V6 individually as `PASS`, `FAIL`, or `NOT_RUN` and return exactly
one overall verdict:

- `PASS_AUTOSWAP_USER_TEST_READY`
- `FAIL_USER_JOURNEY_OR_SCOPE`
- `FAIL_ENROLLMENT_OR_FENCING`
- `FAIL_PAUSE_RECOVERY_OR_DELETION`
- `FAIL_AUTHORIZATION_OR_LIMITS`
- `FAIL_LOCAL_VERIFICATION`
- `FAIL_DEPLOYED_OR_MAINNET_VERIFICATION`

Only `PASS_AUTOSWAP_USER_TEST_READY` completes the standing goal.

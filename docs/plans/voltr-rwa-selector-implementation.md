# Voltr RWA pilot implementation and release plan

## Current critical path — replaces earlier status summaries

**Not ready for external deposits.** The worker image has passed CI and deployed successfully; the compatible frontend is deployed and its live vault API is verified. Current-release funded money-flow and rotation proofs remain incomplete. Historical entries below retain evidence but do not define the current execution order. Existing hot-admin approval, 100-USDC vault cap, 10-USDC working equity, three reviewed USDC lanes, preserved spending history and full release acceptance remain unchanged.

### Delivery order

1. **Resolve execution access and freeze one release candidate.** Verify the existing admin/delegate signing path, database access, Git signing and deployment credentials without exposing secrets. Consolidate only Voltr prerequisites and selector work; the selector branch includes unrelated fleet ancestry and must not be merged wholesale. Freeze feature scope. Do not add standalone tools or tests unless a concrete failure on the next milestone requires them.
2. **Prepare the actual runtime.** Freshly reconcile current holdings and pending operations; resolve historical residue with existing approved policies. Install the 100-USDC on-chain cap and three exact initializer policies, apply reviewed migrations, activate the pilot budget without resetting history, and pin finalized policy identities in the release manifest. Keep the client closed during these steps.
3. **Deploy a uniquely identified candidate and complete one financed round trip.** Build the immutable GHCR worker image, pin it on Render and verify environment/database/program compatibility. Use the smallest fully executable authorized tranche (up to 10 USDC). Prove actual user deposit → allocation → swap/collateral deposit → borrow/redeposit → NAV → withdrawal request → repay/unwind → return → claim. Reconcile exact signed transactions and final balances. Fix only failures that block this journey.
4. **Prove rotation on the same release.** A → B → A, including closure/recreation when required, with the other reviewed lane covered. Verify destination-capacity loss, risk/withdrawal priority, restart/ambiguous-submit recovery and complete economic move costs. Controlled race tests supplement the released live flow; they do not substitute for it.
5. **Open the capped user/partner surface.** Finish current worker/reconciliation readiness integration, deploy the compatible frontend, verify a real wallet journey against that release, then enable deposits within the already installed cap. Declare ready only after all gates below pass.

### Current implementation ownership

- Coordinator: current goal, production state, review, credentials, deployment, budget and all financial actions. Fresh production read confirms no lease, pending operation or latch; pilot budget generation 819 is now activated after finalized cleanup.
- GLM Flash access implementation completed (task 607, steered from 602): three route-scoped views and all three reader queries, with focused checks.
- GLM Flash verifier implementation completed (task 608, steered from 603): independent open-deposit evidence evaluation, same-batch consumed-report proof, bounded deadline and sanitized errors. Funded-flow and rotation proof remain missing.
- Both agents use `glm-5.3-flash` through the installed external-subagents runner, with separate file ownership and no secret access, deployment or financial authority. Coordinator reviews actual diffs and checks before release.

### Acceptance board

| Gate | Current evidence | What closes it |
|---|---|---|
| Access | Admin/delegate public identities verified from mounted environment; Git SSH signature cryptographically verified | Cleared for current session; verify deployment credentials during release |
| Release scope | Worker `bdd173a` published by passing Actions run 35190543782 and deployed to replacement `srv-dalootgae00c73c4qhag`; frontend `368575fe` deployed as `dpl_8CzMsfnytBUwuEivcrJkzbd4rAQR`, live API HTTP 200 | Publish and deploy the reviewed activation-baseline fix; complete funded acceptance |
| Chain/database activation | 100-USDC cap finalized at 447724467; policies 149–151 finalized and pinned; migrations 74–80 applied and validated | Cleared: return finalized at 447899062; pilot budget generation 819 activated at 447899153 |
| Financed money flow | Captured protocol legs and local admission pass | Full released deposit-to-claim receipt set |
| Automatic routing | Selector/transition code and controlled tests exist | Same-release rotation with current capacity/cost evidence |
| Public access | Client gate now checks current release lease, pilot authority, pending work, report identity, NAV/custody, freshness and cap; default remains closed; hosted APIs verify the expected vault and 100-USDC cap at slot 447890790, plus inactive worker state; typecheck/lint and 33 controlled service cases pass | Complete real wallet journey, then enable |

**Runtime startup result:** the replacement image loaded the pinned manifest (`54dfdce596a237ef0c3726f20aa999ffd61152fdc3dcfb30608227fd975d68b0`) and acquired the correct fenced route lease, then refused NAV admission because the goal budget is absent. The worker retired the unsigned attempts; no signature, broadcast or pending operation remains. Both Render services are suspended. Activate the budget before restarting the new service; do not resume the historical service.

**Client deployment result:** release `368575fe` is Ready as `dpl_8CzMsfnytBUwuEivcrJkzbd4rAQR` at `https://loyal-vault-pilot-of93wxffq-loyals-projects-4b3ed656.vercel.app`. Both `/api/vault` and `/api/worker` return HTTP 200. The vault response confirms HXtk, 100-USDC cap, 23 raw idle/book value and zero circulating LP at slot 447890790. The worker response truthfully reports inactive lease and stale observation. Server-only RPC and restricted database access are configured; deposits remain disabled and consumed NAV proof is unavailable. This is deployed API evidence, not a funded wallet-flow proof.

**Reviewed implementation candidate:** frontend `8859a01aa94e6c051afa172f6cb91fe432cd96e3` contains both GLM patches and coordinator corrections and is pushed. Typecheck/lint pass; 33 deposit-service and 107 consumed-report cases pass. The sole verifier accepts one coherent open-deposit row and rejects 37 altered variants; its fast-tier overall verdict remains FAIL with missing live acceptance evidence. Production SQL validation was rolled back: only the three pilot views were selectable, unrelated routes returned zero rows, and base-table access, writes and schema creation were denied. The rollback test preceded the approved installation described below.

**Latest local follow-up:** frontend `3d15b413` restores the omitted public-only browser fixture and replaces R00's unconditional reserved-config failure with actual consumed-report validation (GLM follow-up task 613). Fixture field allowlist, both Ed25519 signatures and exact current claim messages were verified without private keys or new signing. The full verifier now records R05 PASS (14 recovery scenarios, zero network submissions). Its two-browser read-budget check passes (shared immediate reads; two upstream requests in 11 seconds). R00 correctly fails for missing consumed-report proof; overall acceptance remains FAIL. The temporary test servers are stopped. This follow-up is pushed and included in the current deployment. Report: `/private/tmp/voltr-pilot-browser-fixed.json`.

**Approved reader provisioning:** the user explicitly approved the restricted role, three views and delivery of the generated credential to `loyal-vault-pilot` (Production and Preview, server-only `LOYAL_VAULT_DEMO_OBSERVATION_DATABASE_URL`). Installation completed. An actual login verified all three queries, read-only defaults, a five-second timeout and no base-table privileges. The credential was delivered directly to Vercel without a plaintext file. Deposits remain disabled.

**Publication authorized:** the user explicitly approved publishing the pending implementation and rollout notes to the existing Loyal GitHub repositories. Frontend `3d15b413` and provisioning note `368575fe` are pushed. Deployment `dpl_8CzMsfnytBUwuEivcrJkzbd4rAQR` is Ready and both served APIs are verified.

**Current activation work:** complete. The approved 214898 raw PYUSD conversion yielded 214921 raw USDC. All three accounting-aware bridge steps finalized: report, stage, restore. HXtk idle and book both equal 214944 raw USDC; receipt NAV, tracked custody, manager and strategy cash are zero. Strict finalized flat validation passed. Pilot budget generation 819 was activated at finalized slot 447899153 without resetting history. Public receipts are in `pilot-cleanup-return-finalized.json`; signed intents remain private. Both workers remain suspended pending the new release.

**Immediate release fixes:** the cleanup compiler reuses the current bridge message path, with exact flat-position and custody/book checks; operator signing remains separate. The worker also needs to recognize the exact consumed-ticket baseline already archived by validated pilot activation before its first own report. This baseline must not create journal/NAV evidence or accept any later sequence. GLM implementations received coordinator corrections and integrated database/lifecycle verification. The live cleanup proved the exact book and plain-SPL staging semantics. Build one replacement worker image after these fixes pass; update the frontend image pin with the same release. Frontend verifier follow-up `f9df7346` is pushed and will accompany that deployment.

**Latest live evidence:** `docs/evidence/voltr-selector-2026-09-16/pilot-cap-finalized-result.json` and `initializer-finalized-installations.json`. The first initializer attempt expired unspent; its signed wire and finalized absence proof are retained. Installation now uses consistent confirmed prestate/simulation/fee/preflight with finalized terminal recovery. Existing legacy-manifest tests explicitly omit installed pins; release pins are shared byte-for-byte with `docs/manifests/backyard-rwa-v2.json`.

**Progress reporting:** report a deployed milestone, a resolved blocker, or a concrete failure in the money flow. Counts of component tests are supporting detail. Do not append more historical status narratives as a substitute for updating this board.

### Work locations

Worker implementation: isolated `feat/voltr-rwa-selector` checkout, based on `fleet/integration` at `0058abf5ce6e386a3e5245231b99b11a4fce595e`. Frontend implementation: isolated `ask-2025-voltr-pilot-access` checkout. Preserve both original dirty workspaces. No broad fleet merge or old-worker resume.

## Historical implementation notes

The sections below describe successive implementation stages. Statements such as “not yet connected” may have been superseded; use the acceptance board above and current source to determine remaining work.

## Implemented

- **One Go economic comparison.** Keep is valued from actual collateral, debt
  and idle cash. Candidates use the existing executor's single borrow/redeposit
  pass (approximately 1.5x), projected utilization and borrowing rate, one
  evaluation horizon, complete economic move costs, idle remainder, a margin,
  and a persistent advantage. Capacity opening and economic persistence are
  separate. Missing capacity or a bound, current move quote prevents a trade
  recommendation. Gross spending authorization is distinct from economic cost.
- **Existing feed plus official enrichment.** Read-only Timescale sessions read
  the existing verified reserve view. Bounded HTTP requests read native yields
  from Kamino's batch stats dictionary. Mint identity, timestamps, numeric
  validity and source type are checked. Real padded KLend rate curves are
  supported. Unproven incentives contribute no income. A separate bounded background sample reads
  economics and its own confirmed snapshot. It never changes the execution
  observer or runs inside the transaction tick. Shadow SQL skips a locked route
  row after 25ms and has a one-second deadline.
- **Truthful withdrawals.** Covered claims do not trigger an unwind. An
  uncovered claim can use the existing full-exit recipe, while demand remains
  the actual claim amount. Typed planner decisions use canonical actions. The basic-USDC executor keeps
  its existing wire compatibility mapping until shared-custody admission is
  complete. Old signed requests retain
  their identities and recovery path. Full-custody bridge effects cannot be
  hidden by clamping the declared amount. Already-staged, journal-explained cash
  is restored even if a new deposit covers the withdrawal or its claim disappears.
- **Bounded unwind support in the existing route row.** The intent refers to
  the existing budget's exit reserve. It does not create a second balance or
  remaining-spend counter. Writes use the current lease, row lock, generation
  and unresolved-transaction fence. The worker reloads intent on construction
  refreshes. Completion requires flat custody/position and reconciled NAV;
  completion pauses new entry pending fresh admission.
- **Ownership checks.** The selector observer reads all three pilot obligations
  and collateral custodies in the same confirmed batch. Residues keep their
  source visible. Multiple exposures produce an explicit hold instead of a
  fabricated single-position NAV. An absent ONyc obligation is supported as a
  lifecycle state.
- **Historical incident disposition.** Migration 78 attaches evidence-backed
  resolution metadata to the exact strategy-one restore incident. It preserves
  the failure, signed wire, timestamps and original evidence. Runtime recovery
  checks the disposition against its own row instead of embedding one signature.
  Migration execution was tested locally; it has not been applied in production.
- **Conservative deployment limits.** Optional limits live inside the existing
  budget. Legacy records retain their previous limits. Adoption can only narrow
  limits and cannot reduce committed spend/exit room, change the campaign's
  identity, reopen it, or reset reservations. This is not a new ongoing funding
  authorization.
- **Health-hold reporting.** Unvalued health/integrity observations preserve the
  last good position projection and proceed to a durable hold, rather than
  failing while trying to store a zero-valued position.

The production comparison currently runs as **shadow output**. Its `ENTER` and
`SWITCH` results are not connected to transaction creation. `CommitUnwindIntent`
is a guarded primitive for an already admitted exit, not an automatic admission
path. No live switch or permission expansion is enabled by this work.

## Read-only inspection

Build `go/backyard-rwa-worker/cmd/backyard-rwa-worker`, then run from the directory
containing the mounted 1Password environment:

```sh
op run --env-file=.env.1password -- /path/to/backyard-rwa-worker --selector-shadow
```

This command requires `SOLANA_RPC_URL`, `NEON_DATABASE_URL` and `TIMESCALEDB_URL`.
It does not load a signer, acquire a lease, broadcast, or write to either database.
It prints the observed lifecycle decision, normalized market inputs, shadow
result and explicit activation blockers.

`BACKYARD_RWA_SELECTOR_SHADOW=1` runs an independent observation task inside an
**already authorized running worker** and persists its latest shadow projection
under `state.selector`. It is not a no-broadcast mode: the existing worker retains
its lifecycle execution responsibilities. Use the standalone command above for
read-only research. Missing economics cannot suppress valid on-chain safety work. Unresolved
transactions skip shadow persistence; coherent NAV-report samples update
economic evidence without authorizing a move. Both paths preserve the waiting
period while still expiring stale evidence.

## Verified so far

- Baseline Go suite passed before changes.
- Full Go suite passed with the disposable PostgreSQL admission/recovery tests
  enabled. Live transaction/fixture-export cases retain their explicit gates.
- New cases cover covered withdrawals, missing evidence, projected borrow costs,
  partial capacity, closed current lanes, capacity reopening, persistence gaps,
  risk priority, source residue, intent restart/fencing, migration identity and
  preservation of budget/spend state. Review regressions cover staged cash after
  demand changes, USDC execution compatibility, NAV persistence, feasible partial
  allocation, blocked shadow SQL, and background-sample cancellation.
- `go vet ./...` passed. `cargo check -p loyal-yield-store --locked --offline`
  passed for migration registration.
- Live read-only observation at slot **447392407**, September 16 at **01:01 UTC**,
  decoded native yield and verified reserve economics for PRIME, syrupUSDC and
  ONyc. The independent chain observer returned **`kamino_stale`**. No trade was
  recommended or attempted. See
  [shadow evidence](../evidence/voltr-selector-2026-09-16/shadow.json).

These are source, controlled database and read-only data checks. They do not
establish transaction simulation success, deployment, or a live lifecycle PASS.

## Remaining implementation and release gates

1. **Prove the USDC execution lifecycle against deployed protocols.** The local
   shared-USDC admission changes now use canonical actions and count the common
   cash account once. Controlled tests cover payoff/entry and partial-capacity
   planning. Initial swaps consume all working cash; unused capital stays in
   Voltr. Partial hard-LTV repayment now has local admission, revalidation and
   durable unwind continuation; current-release lifecycle proof remains required.
2. **Connect genuine pair capacity and complete economic move quotes.** The
   existing feed and API reserve-wide availability are insufficient for the
   chosen obligation's exact pair caps, withdrawals and swap depth. The shadow
   reader deliberately leaves pair capacity unknown. Reuse the verified
   execution recipes once item 1 is complete; do not treat a principal debit
   from the budget module as an economic fee.
3. **Admit repeatable obligation recreation.** The existing prerequisite tool
   invokes `executeTransactionSyncV2` using the Squads administrator. The
   delegated worker cannot claim that authority. Three exact per-lane initializer
   policy compilers now pass deployed-Squads boundary tests (767-byte install
   packets; KLend stubbed). They are not installed. Native rent accounting,
   same-journal worker execution/recovery and connected KLend proof remain.
4. **Finish automatic transitions.** Wire a freshly admitted economic result to
   unwind commitment, then freshly admit destination entry after the source is
   flat. The old destination is advisory. When capacity disappears, retain idle
   cash or choose another freshly admitted lane. Never clear the entry pause
   based only on a shadow ranking.
5. **Resolve the deployment envelope.** PRIME has no funded family in the
   current finite canary budget. Narrower configurable caps do not authorize a
   new family, an ongoing budget, admin use, or bypassing the existing activation
   contract. Preserve spent and reserved amounts through any approved handover.
6. **Prove the complete lifecycle on the release image.** Address the observed
   freshness hold without weakening valuation checks. Require compatible
   program/policy/manifest/database pins, A-to-B-to-A including recreation,
   withdrawal, destination-capacity races, ambiguous submission recovery and
   final NAV/custody reconciliation before activating partner capital.

The remaining items are actual work and evidence gaps. They must not be relabeled
as optional enhancements or hidden behind a production-ready claim.

## Independent review

Fable identified six actionable defects in the first local diff. All six were
addressed: shadow/execution isolation, SQL latency, basic-USDC wire compatibility,
staged restoration ordering, NAV persistence and feasible partial-allocation
persistence. A follow-up caught unresolved NAV transactions resetting evidence;
shadow persistence now skips those samples, including a transaction that starts
between observation and the locked database write. The background observer reuses
its program-identity cache.

Repository-wide Rust formatting was not changed: `rustfmt --check` reports the
same unrelated formatting differences on the baseline `store.rs`. The targeted
crate compilation and whitespace diff checks pass.

## Approved rollout continuation: September 16

The service remains suspended. No policy installation, signing, capital movement,
production journal write or image deployment occurred during this implementation.

Completed locally:

- Canonical USDC repayment/borrow/leverage paths use the shared Squads USDC
  account once. Legacy persisted-wire recovery retains its original representation.
- Basic Jupiter routes retain legacy discriminator and exact custody/fee sentinel
  checks, while accepting the variable-length route data the installed policies
  already allow. The historical Phase 1 header restriction remains historical.
- The pilot leaves unused capital in Voltr and finishes one working tranche before
  another allocation. Changed entry capacity returns flat cash before entry-only
  LTV/obligation checks. Whole-cash entry admission prevents stranded mixed cash.
- The canary uses at most 500,000 raw USDC equity per tranche. The 1.5x collateral
  leg is below the existing $1 total transaction ceiling; exact observed fees and
  complete exit admission remain authoritative. This is not a production cap.
- Cash-only NAV remains observable when entry reserves are stale/paused. Nonzero
  collateral/debt, foreign custody, future refresh slots and exposure-hiding
  overrides retain their holds.
- The pinned Voltr binary creates receipt version 2. Its existing custody-donation
  replay proves byte 128 bookkeeping. The worker accepts exact version 2 and still
  rejects unknown reserved fields/versions. Historical proof files are preserved.

Fresh read-only evidence at slot **447403766** verifies the pinned Voltr/adaptor
identities, zero strategy/Squads cash and no withdrawal demand. Voltr holds **23 raw
USDC**. Observation now reaches the historical **custody_transient_mismatch**:
last journaled staged amount 793,417 and armed NAV 3,793,417 remain unreconciled.
Migration 78 is still unapplied; do not clear this hold by changing observations.
See `cash-observation.json` and `receipt-v2-proof.json` in the evidence directory.

Validation: full Go module tests and vet passed; the deployed-Squads basic-policy
and initializer positive/negative tests passed. The captured-binary Voltr reset
sequence passed with an explicit fresh-receipt-version assertion. These checks do
not prove a live complete RWA lifecycle. Deposit cap/ongoing servicing limits,
initializer execution, fresh reserve maintenance, exact pair capacity/move costs,
automatic transitions, partner app integration and release-image canaries remain.

Fable reviewed the continuation. Follow-up findings produced the full-payoff
interest check, truthful partial-risk hold, whole-working-cash boundary, return
before entry checks, and smaller measured canary tranche. The requested initial
user/partner deposit cap is still pending; do not infer it from finite test caps.

The operator approved retaining the existing hot administrator for the initial
user/partner rollout. The activation verifier records the explicit U1/decision-2
amendment. Cold-key separation is no longer a blocker for this approved scope;
the deposit cap and the execution/accounting release gates remain outstanding.

### Capacity and initializer continuation

The confirmed USDC route observation now constrains the existing 1.5x reserve
bound by actual available liquidity, cross-mode allowance, net withdrawal cap,
utilization and global borrowing caps, and origination fees. Queued liquidity,
unreviewed elevation/referrer modes and effective borrow factors above 100%
decline entry. A closed entry does not erase otherwise valid NAV. The bound is
still a ceiling: admission must validate the actual quoted tranche, including
minimum fees, collateral receipt rounding and its full return recipe.

Initializer construction matches the installed KLend 7.3.9 SDK's instruction
bytes, account roles and derived PDAs for all three lanes. The same SDK independently
checks capacity offsets. The captured KLend binary at deployment slot 440486775
and the deployed Squads binary execute each initializer successfully in LiteSVM.
Each creates a 3344-byte obligation with a 17,637,760-lamport vault debit and equal
obligation credit. Metadata stays unchanged; duplicate creation fails without
another vault debit. This proof replaces the KLend stub for creation mechanics.

Evidence: `klend-initializer-proof.json` and the reproducible public
`klend-initializer-capture.json.gz` in `docs/evidence/voltr-selector-2026-09-16/`.
The capture is finalized at slot 447409473. The test uses the captured Rent sysvar, whose minimum agrees with the fresh RPC rent quote. The test explicitly substitutes absent
obligations, funds the local vault and installs local candidate policies. It does
not execute the preceding close/refund, prove production signatures, install live
policies, or wire the initializer into the worker journal. Existing live obligations
hold refundable rent; use actual reconciled refunds and native cash when checking
recreation funding instead of assuming a new top-up is always necessary.

The connected test is `backyard_multiply_initializer_connected_klend` in
`backyard_basic_policy_set`; set `SELECTOR_KLEND_CAPTURE` to the decompressed
public capture and run it with `--ignored`. The Go SDK oracle runs with
`KLEND_SDK_ORACLE=1` after installing `tools/backyard-voltr` dependencies.

Fable's follow-up also found the worker rejecting its own borrowed-USDC swap
decision. Decision validation now admits exact pilot collateral/debt edges and
continues rejecting USDC-to-USDC conversion actions. The regression exercises
decision production and validation together.


### Native initializer journal contract

The typed initializer now compiles through the existing persisted build input,
fee/rent valuation and finalized reconciliation path. This is still a foundation,
not an enabled worker dispatch path. Its confirmed preflight requires the exact
absent obligation, installed policy hash, hot-admin Settings graph, metadata,
mints, market, native payer funds and current Rent sysvar. The original preflight
slot bounds the final send valuation; later fee/oracle reads cannot extend it.
Missing prerequisites produce a durable budget hold with the existing
expired-and-absent signed-wire recovery path.

Native rent is priced once as setup, separate from token principal and network
fees. Finalized reconciliation binds the persisted transaction bytes and checks
all native account deltas, the exact vault-to-obligation rent transfer, the
network fee payer, and the created empty Multiply state. Unexpected token effects
or return data reject reconciliation. PostgreSQL settlement rejects a mismatched
wire and retains the reservation until accepted finality. No receipt success is
inferred from account presence alone.

The connected captured-program proof executes the exact exported Go legacy
messages, bound by `go-initializer-messages.json` and its SHA-256. It also verifies
that policy account data
and lamports stay unchanged and returned data is empty on all three initializers.
Migration 79 adds only the three initializer lanes and the existing OnRe USDC
route-neutral lifecycle to journal constraints. The actual migration's positive
and negative engine/lane cases were executed in a disposable PostgreSQL schema.
It has not been applied to production.

Validation: full Go suite with a fresh disposable PostgreSQL cluster; targeted
initializer preflight, cost/freshness, native receipt and settlement tests; Go
vet; `cargo check -p loyal-yield-store`; captured Squads/KLend initializer proof.
The SDK oracle and capture evidence remain separately described above.

Manifest schema and the build/sign/simulate/persist function now enforce reviewed
initializer pins before signer access; the embedded manifest deliberately has no
initializer pins yet. Still required before live initializer activation: actual installed policy
evidence, fresh whole-loop entry admission, remaining native exit-fee liquidity
and the controlled release. Worker preparation/admission/dispatch is now connected
as described in the pilot runtime section below.
The initial partner/user deposit cap decision was subsequently delegated to the agent (see below). The
hot-admin continuation is approved; no cold-key condition is reintroduced.


## Goal contract: delegated pilot completion (September 16)

The operator instructed: "choose whatever works ... finish testing ... set a goal
and go implement whatever we are still missing." This grants the remaining
routine pilot configuration choices and supersedes the pending cap question.
The prior hot-admin approval remains in force. This amendment governs the pilot
release alongside the unchanged accounting/protocol requirements above and the
strategy-two activation contract; historical canary limits remain historical.

- **Objective:** deployed user/partner deposit access to the HXtk vault, with one
  active reviewed USDC Multiply loop plus idle cash, automatic eligible-opportunity
  selection, and working withdrawal/claim and transaction recovery.
- **Pilot choices:** 100 USDC total vault deposit cap; at most 10 USDC working
  equity per allocation; start lifecycle tests at 1 USDC or the smallest fully
  executable route amount established by actual quotes. PRIME, syrupUSDC and ONyc
  USDC lanes only. Existing hot admin and delegated executor remain pinned.
- **Invariants:** confirmed/finalized evidence as appropriate; no duplicate send;
  no unexplained custody or fabricated NAV; closed destination capacity means
  keep or idle; withdrawal and risk recovery precede economic optimization.
  Existing spent/reserved history survives the production envelope transition.
- **Verification:** one current release assessment in this document, backed by the
  existing worker/protocol/app verifiers and structured authoritative receipts.
  PASS requires current identities, database/policy readback, release-image
  deposit/allocation, A-to-B-to-A with close/recreation, liquidity restoration,
  request/wait/claim, and failure/restart/capacity-race coverage. Fixture-only
  cases must be identified. Local tests or a deployed process alone cannot pass.
- **Delivery:** scoped release from the isolated selector worktree, immutable
  GHCR worker image pinned on Render, compatible user access surface, verified
  deposit cap, healthy service and live readiness readback. Do not resume the
  old pinned image or merge unrelated fleet changes by accident.
- **Current verdict:** FAIL (implementation and release work remains). No
  additional cap or cold-admin approval is pending. Record a real external
  dependency only if encountered; continue independent implementation meanwhile.

### Audited journal association implementation

Recovered finalized strategy-two bootstrap at slot 446086069 and captured the exact 1717-row strategy-one journal inventory. Migration 80 and a shared accounting CTE now separate that audited retired history from the current receipt without changing statuses, signatures, wires, effects, timestamps, failure history or accounting values. New decisions stamp their current strategy config. Unknown/copied/mutated metadata remains visible; migration refuses unexpected inventory or concurrent work. Local PostgreSQL inventory replay, worker suite and store compile pass. Evidence: `docs/evidence/voltr-selector-2026-09-16/strategy-journal-association.md`. Production application remains part of the controlled release.

### Pilot accounting and activation boundary

Implemented versioned pilot authority in the existing budget, preserving cumulative gross history while accounting for reusable principal and separately bounded execution costs. Chosen gross authorization ceilings are 20 USDC per transaction, 100 USDC per family and 150 USDC total; the 100-USDC vault cap and 10-USDC equity tranche remain separate. New entries stop at 5 USDC in cumulative execution costs while existing reserved recovery stays available. Limits cannot change with an outstanding exit. Activation checks both physical and journal-derived stops, validates archived budget identity and inherited history on every pilot budget read, and requires finalized flat state for all eight configured current/historical lanes. Local PostgreSQL, repeated-rotation, restart, and exact native settlement checks pass. Pilot activation remains disabled. The measured execution-cost producer is now connected to admission, retries, build and send; current-limit enforcement now uses the locked durable budget for the current transaction and every exit step. Planning now projects verified persisted pilot authority, sizes at most 10 USDC equity and preserves full exit amounts. Initializer preparation/admission/dispatch is connected; actual initializer policy pins, selector switching orchestration and activation are still required.

The expanded live read found 0.214898 PYUSD in the historical shared debt custody, despite the three pilot lanes being flat. This remains visible and blocks activation. A read-only quote for conversion to USDC succeeds; actual conversion/return and reconciliation remain required. Evidence and precise limitations: `docs/evidence/voltr-selector-2026-09-16/pilot-budget-validation.md`.

### Pilot initializer runtime

The worker now dispatches `INITIALIZE_KAMINO_OBLIGATION` through the existing prepare → record decision → measured admission → build/simulate/persist → exact recovery/finality path. It can select initialization only under verified pilot authority, a complete initializer manifest binding, an absent obligation, an otherwise ready entry market, and no token position/custody, withdrawal demand, unwind, or outstanding transaction. Preparation measures current rent and the exact-message fee, and revalidates policy, metadata, native balances, market and account absence. Shared locked admission reserves rent plus fee, records only the measured fee as execution expense and refuses to consume an existing exit reservation.

Local tests cover all three lane decisions, prerequisite/priority rejection, account-appearance and withdrawal races during preparation, production measured PostgreSQL admission/retry/pre-signing authorization, and journal-before-build dispatch with refusal propagation. Full worker/database suite and Go vet pass. Fable's read-only review found no new blocker. A withdrawal arriving after preparation can still precede the native initializer on chain; initialization moves no user principal, and the following worker observation resolves withdrawal priority. Embedded initializer bindings remain empty, so live initialization is still disabled pending installation evidence and release.

### Pilot forecast and full-payoff release correction

The selector now caps modeled deployment at the same 10-USDC pilot tranche as the executor, retains the rest of the whole vault as idle (including the configured buffer), and continues to value KEEP from actual source holdings. Quotes for a larger, unexecutable amount cannot authorize a candidate. Shadow observation now reads the same validated durable pilot authority as production. The full worker/database suite and vet passed for commit `f3adae5`; Fable found no material defect. Economic thresholds still need an observed-cost review before activation.

A realistic fully redeposited one-pass position exposed a separate exit mismatch: collateral is approximately 1.5 times equity and debt 0.5 times equity, while the historical 45% temporary release LTV releases only about 0.389 times equity. That cannot fund the existing single full-payoff requirement. Historical overcollateralized fixtures did not demonstrate the pilot recipe's exit feasibility.

Pilot preparation and projected exit costing now use a shared release ceiling of `min(55%, protocol maximum LTV minus 5 percentage points, unchanged worker hard limit minus 5 percentage points)`. The ceiling must exceed 50%; otherwise admission holds. The calculation independently respects the market's global allowed borrow value and minimum remaining collateral, current reserve prices, bounded interest, group zero, and effective borrow factor 100. It does not use cached obligation valuations. Receipt conversion remains conservative. The exact quote minimum must still cover the complete payoff and the whole remaining exit must fit its durable reservation. Historical release sizing remains 45%.

An explicit request mode is bound into the persisted intent and requires pilot budget authority at admission, build and send. Current release revalidation refuses a wire after its safe allowance shrinks. Entry projections that rely on a later release re-simulate the exact unsigned entry before signing and sending, then compare equivalent refreshed risk settings and prices. This avoids confusing stale persisted reserve prices with changed oracle prices. They also recheck normalized receipt backing, total collateral, funding cash, retained release output and the remaining payoff window; a deterioration requires a new complete admission. Fable identified the configuration race, the refreshed-price mismatch, the minimum-collateral constraint and the backing risk during review. Tests use realistic 10-USDC account fixtures across all three lanes, verify the old funding shortfall, test market-cap/price/configuration/mode drift, and check the SDK market-cap offset. The full worker/PostgreSQL suite, Go vet and the explicit SDK parity check pass, and the worker binary builds. Fable's final focused source review found no further material defect after those fixes. These are controlled tests, not an executed protocol lifecycle.

### Exact selector entry handoff

The existing route state now carries the exact selected destination quote and gross equity. A live evaluation rechecks pilot authority, route lease/version, current observations, pending work, recovery holds and economic persistence before accepting an entry from reconciled idle. Quote-free shadow rankings cannot open entry. The selector copies its selected quote into the result, so quotes with equal net equity but different inputs cannot be exchanged during persistence. Evaluation has a one-second deadline and a short database lock timeout; it does no network work inside the route transaction.

Pilot observation covers all three ownership lanes. Observed exposure determines accounting; a selected destination only supplies the preferred lane while flat. Allocation uses exactly the quoted equity and refuses to silently resize when available cash/capacity falls. An absent obligation is initialized only for a selected amount that still fits. The entry binds to its first allocation operation in the same transaction as measured admission; rollback does not consume it, same-operation retries retain it, and a returned attempt requires a fresh quote. The record owns no balance or spending counter.

Admission, build and final send recheck quote expiry and allocation identity. Expiry does not interrupt an already funded tranche or its exit. A signed expiry refusal preserves bytes and reservations until finalized blockhash expiry followed by signature absence; reconciliation never depends on an unexpired entry choice. Completing an unwind removes the old entry and pauses new allocation for fresh selection. Pilot unwind commitment now also validates the archived pilot authority.

Fable found and reviewed fixes for equal-net quote confusion, reusing one choice for multiple allocation attempts, and the signed-expiry recovery classification. The final read-only review found no remaining material defect. The full Go/PostgreSQL suite passes (19.879 seconds), including exact quote persistence, restart/lease/generation/recovery fences, unchanged budget history, allocation association rollback/retry, capacity changes, and actual worker signed-expiry recovery using controlled RPC responses. Go vet, the worker build and the whitespace check pass. These are local tests; no production service or funds changed.

**Release remains FAIL / not activated.** The exact entry handoff is implemented, but its complete move-cost producer and background live evaluator are not connected yet. The corrected full exit still needs a pinned-program execution proof. Initializer policy installation, historical PYUSD return, controlled journal/pilot activation, user access deployment and current-release live deposit/rotation/withdrawal verification remain required. No further user approval is pending for the agreed hot-admin pilot.

### Recipe pricing, chain-slot expiry and deposit remainder

The three USDC pilot lanes now use the same reserve-derived minimum deposit size as the bounded deposit builder. A completed loop with a smaller custody remainder stays settled instead of repeatedly selecting a deposit that construction refuses. Residue remains in custody, NAV and the exit calculation. Initial collateral-only positions still proceed to borrowing; the prior builder already selected borrowing, but the decision reason could misleadingly describe another deposit. If the rounding window is unavailable, the observer preserves accounting and returns a zero entry bound. NAV, withdrawal and risk priorities remain ahead of entry. A controlled test exercises the actual production observation decoder, including unchanged collateral/NAV and a changed observation identity when the bound becomes unavailable.

Added a selector recipe pricing helper that compiles and prices every supplied message, including repeated NAV reports. It reuses observed token prices only within the sample, counts network fees for each occurrence, classifies swap loss/origination/rounding with the existing execution-cost classifier, and records recoverable initializer rent separately. It creates no operations, reservations or synthetic RPC accounts. Controlled RPC tests cover principal/rent separation, repeated reports, exact basic-lane Jupiter minimum output, stale prices, changed initializer fees and malformed effects. This helper is not yet a complete source-to-destination recipe producer and has no live caller.

Move quotes now carry the starting chain slot and earliest expiry slot, bounded to 32 slots. Selection, durable acceptance, planning and allocation/initializer admission, build and send enforce that window as well as wall-clock expiry. The producer must capture its sample start before constructing any quote/reserve-derived input; fresh fee observations cannot refresh older economic evidence. Final build checks also intersect the original complete exit admission's validity. Tests cover fresh timestamps with expired/future/missing slot evidence, expiry during collection, durable allocation refusal, signed initializer recovery, and continued handling of already funded capital.

Fable's source review identified the need to preserve quote slot validity and the final build/admission intersection; both are implemented. The full Go/PostgreSQL suite passes (20.072 seconds), as do Go vet, worker build and whitespace validation. These are local controlled tests, not live protocol lifecycle proof. No production service or funds changed.

The destination producer still must verify the pinned Maple/OnRe farm user accounts; policy readiness alone does not inspect them and the obligation initializer does not create them. Its entry recipe includes optional initialization, allocation, initial swap/deposit, borrow, leverage swap/redeposit and all required NAV reports. Budgeting an extra report after allocation is conservative because allocation already reports NAV. Missing setup or incomplete economic evidence must leave that candidate unavailable. Source exit pricing must reuse the existing finite exit templates while excluding returned principal from expense; `ExitAfterMicros` is gross movement, not move cost. All release gates above remain open.

### Destination recipe and exact borrow sizing

The destination forecast now reads the actual empty obligation, reserve/market,
policy, adaptor/ticket, mint/custody and required farm accounts. It refuses
missing farm registration, changed reserve bindings, unavailable pair capacity
or native funding. It builds and prices optional initialization, allocation,
initial swap/deposit, a single borrow, leverage swap/redeposit and NAV reports.
These are explicitly hypothetical amounts passed through the existing unsigned
compilers and expense classifier; no fabricated account images are simulated,
no operations are inserted and no transaction is signed or sent.

A separate exact reverse-swap quote checks that the prospective completed loop
can fund full repayment. The shared scalar release calculation preserves actual
protocol global borrow allowance, minimum retained collateral and the reviewed
release ceiling. Actual execution retains the preexisting receipt conversion
floors. Forecast debt covers eleven borrow-to-payoff windows; receipt backing
covers eighteen sample-to-payoff windows, including allocation and optional
initialization. Two deposits' sub-unit backing rounding is added before maximum
rate compounding. Both release conversions are rounded conservatively. Exit
feasibility evidence is retained separately and is not charged as entry expense.
Every retained lookup-table slot, including the reverse quote's, advances the
observation floor without extending the original 32-slot validity window.

The selected quote now records exact borrow principal and its origination fee
ceiling. Execution may receive more initial collateral, but cannot silently
increase the forecast borrow/swap size. If the reviewed principal exceeds the
current target, construction holds. Admission, build and send validate the
conserved borrow effects and enforce both the exact principal and fee ceiling.
Lower fees are allowed. A funded tranche can finish after quote expiry while
current protocol, execution-cost and exit-reservation checks remain required.
Candidate income now uses the quoted borrow plus fee, rather than assuming a
fixed leverage amount after costs.

Validation: full Go/PostgreSQL suite passes (20.396 seconds), as do Go vet,
worker build, formatting and whitespace checks. Controlled tests exercise
1-USDC and 10-USDC destination recipes, repayment quote shortfall, global-market
capacity, insufficient release margin, minimum collateral, missing/drifted farm
and custody accounts, a lookup read beyond quote expiry, pre-borrow interest
crossing a receipt-rounding boundary, and durable exact principal/fee binding
across restart. The Maple lookup fixture uses the existing documented offline
reconstruction; this is not chain-executed proof. Fable reviewed and confirmed
fixes for principal sizing, fee drift, protocol release limits, initializer/ALT
freshness and the longer collateral interest horizon.

The producer still has no live caller. Source-to-destination composition,
source-close native refunds, live selector/unwind orchestration and the release
steps listed above remain required. No deployment or production funds changed;
release verdict remains FAIL / not activated.


### Complete movement forecast and post-exit cash sizing

The source producer now reuses the existing finite payoff/release/withdrawal/
return graph. Each retained future cost carries its exact unsigned template;
legacy records without templates remain readable but cannot supply selector
forecasts. Every message occurrence is priced, including repeated NAV reports.
The source cash floor starts with actual custody and adds only enforced swap
minimum outputs, subtracts maximum repayment, and includes existing Voltr idle
once. Optimistic bridge reservation amounts never become spendable cash.

Composition binds that source evidence to the destination recipe, original
observation, earliest slot expiry and current native funding. Actual delegate
fees and vault setup balances must cover the combined recipe. Hypothetical
source-close rent refunds are not credited before reconciliation. The quote
retains whole-vault minimum idle after exit; both the producer and selector
subtract the configured idle buffer from that amount before choosing entry
size. This prevents pre-exit NAV or anticipated swap gains from overfunding the
next allocation.

Fable identified stale retained-cost slots and post-exit idle-buffer sizing;
both are fixed with regression coverage. Controlled tests exercise a fully
redeposited 10-USDC loop with collateral release, repayment-funding swap,
maximum payoff, final withdrawal/swap and idle return; missing templates,
repeated NAV fees, slot expiry, combined native funding and buffered sizing are
also covered. The full Go/PostgreSQL suite passes (20.022 seconds), as do Go
vet, worker build and whitespace checks. These are forecast/fixture tests, not
live execution receipts.
The complete producer has no live caller yet. Atomic economic unwind handoff,
background live evaluation and all deployment/lifecycle release gates remain
open. No production funds or deployment changed.

### Atomic economic switch commitment

`RecordSelectorEvaluation` now commits SWITCH and its bounded unwind intent in
one transaction under the pre-observation route version and current lease. The
selected movement quote carries the source receipt bound, full-payoff debt
ceiling and gross remaining exit debit. Acceptance requires that debit to fit
the existing source-family exit reservation; it never adds spending authority.
The same write stores the decision, clears entry permission and advances the
generation once. Destination entry must be selected afresh after reconciliation.

The source recipe's initial cost-only NAV anchor remains conservative economic
expense. If accounting is already current, that anchor is excluded from the
remaining gross exit commitment: the previously settled NAV consumed its own
reservation already. Required NAV work still takes priority over selection and
preserves economic persistence. A controlled budget admission/settlement test
proves that unchanged return pricing fits exactly the remaining reservation.
PostgreSQL tests prove rejected missing/underfunded/mismatched/stale decisions
leave state unchanged, and accepted switches retain their result and unwind
across restart without changing the budget. These are local accounting and
journal tests, not protocol lifecycle proof. The background live evaluator and
all previously listed release gates remain outstanding.

Validation: full Go/PostgreSQL suite passes (20.204 seconds), as do Go vet,
worker build and whitespace checks. Fable identified the extra NAV reservation
mismatch and reviewed its correction. Before enabling the evaluator, automatic
unwind recovery still needs a fresh, bounded same-source admission after a
prolonged interruption exceeds the original payoff-interest window. The current
intent rejects holdings beyond its envelope; the new runtime must renew that
envelope against current protocol evidence and existing reserved exit spending
under the route lock, without silently enlarging it. This is an outstanding
release requirement, not evidence that restart recovery is complete.

### Unwind renewal after downtime

Debt above a committed payoff-interest bound now marks the snapshot as requiring
fresh unwind admission instead of creating a permanent manual recovery latch.
Unexpected source ownership or collateral growth still fails closed. Pending
transaction recovery remains first, and executable hard-LTV reduction retains
its existing fresh per-leg admission ahead of ordinary renewal.

The worker's renewal hook reads the route version before a new confirmed source
observation, reprices the remaining full exit, then replaces only the same
source intent under the current lease/route lock. It validates pilot authority,
original intent, position bounds, quote time and every retained observation
slot. Existing exit reservations must cover the fresh gross bound; no budget is
expanded or reset. Pending transactions, recovery latches and unresolved capital
recovery refuse renewal. Success resumes from another observation, without
creating a transaction or authorizing a destination. A refused budget renewal
is a normal journaled hold and can be retried; insufficient reserved capacity
still requires resolution before ordinary economic unwinding can continue.

Controlled tests cover interest expiry, hard-LTV priority, worker ordering,
restart with a committed intent, successful bounded renewal, unchanged budget,
stale route version, insufficient exit reservation, pending recovery, missing
cost evidence and a final RPC slot behind retained pricing evidence. Fable's
read-only review identified the last freshness check, which is now implemented.
Live outage recovery and all release lifecycle gates remain unproven; the
background opportunity evaluator is still not enabled.

Validation: full Go/PostgreSQL suite passes (20.142 seconds), as do Go vet,
worker build and whitespace checks. Fable confirmed the focused freshness fix
and found no remaining material issue in that review. No production mutation.

### Background live evaluator (not activated)

`BACKYARD_RWA_SELECTOR_LIVE=1` now opts into the background evaluator; live and
shadow modes are mutually exclusive. Collection is separate from the serialized
transaction loop. It requires validated pilot authority and a live route lease,
reads the route version before accounts, and accepts results only through the
existing atomic entry/switch transaction. The deployment setting is unchanged.
The current conservative shadow policy still needs review against actual pilot
rates and complete movement costs before live activation.

Each sample prices the source once, then at most three destinations in parallel.
Destination pricing may reduce the requested tranche to exact currently admitted
pair capacity; exact-size callers retain their original refusal contract. Feed
snapshots are not mutated. Failed/closed destinations receive no executable
capacity or quote, while current holdings remain the KEEP baseline. Collection
ends eight seconds after the source observation and cancels slow siblings;
completed quotes remain subject to their original 32-slot and wall-clock gates.

Ordinary economic selection waits for a funded tranche to finish its initial
deposit, borrow and redeposit. It does not compare a temporary unlevered source
against an alternative's finished loop and interrupt the accepted entry. Actual
withdrawal, accounting and risk handling retain priority, and sub-receipt custody
rounding residue does not prevent later evaluation of a settled loop.

Controlled production-producer tests cover partial capacity, subsequent capacity
closure, a blocked sibling, a slow RPC sibling, unchanged feed input, exact-size
API preservation, stale observations, duplicate market rejection and the
intermediate entry state. Fable identified the entry sequencing and slow-sibling
issues; both are fixed. No live mode was enabled and no production funds changed.

Validation: full Go/PostgreSQL suite passes (21.174 seconds), focused live
collection race tests pass (2.956 seconds), and Go vet, worker build and
whitespace checks pass. Fable confirmed both focused corrections. Local Git
signing failed twice in the configured 1Password helper; the tested renewal and
evaluator changes remain local pending signing. No signing policy was disabled.


### Explicit pilot budget activation command

The operator command `--activate-pilot-budget` now exposes the existing audited
transition. It requires the fixed route, acquires an exclusive expiring lease,
obtains finalized flat-state evidence through the transition, archives prior
spending and releases the lease even after cancellation. It does not load a
signer, install policies, enable deposits or start the worker. Its output is
explicitly `PILOT_BUDGET_AUTHORITY_NOT_DEPOSIT_READINESS`; retry returns existing
validated authority without refreshing it or resetting the budget.

Full Go/PostgreSQL tests pass (21.325 seconds), including first activation via
the command wrapper, occupied-lease refusal, absent/existing historical budgets,
idempotence and lease release. Go vet and worker build pass. A command invocation
with all configuration unset fails with a fixed configuration hold, as expected.

A fresh public finalized read at slot **447471906** still refuses activation:
`pilot_transition_custody_not_flat`. Historical custody
`J4YFQzxhQ3pht2RRYes5yv1spPYBqvHzxn4zMX7iriHn` retains **214898 raw PYUSD**;
Voltr idle retains **23 raw USDC**. No funds moved. Public evidence is retained
in `docs/evidence/voltr-selector-2026-09-16/pilot-flat-finalized-447471906.json.gz`;
uncompressed SHA-256 is
`21f37f05cfd49644675cf9e0a8bb21f51fc8f56136baf787640bceb40d58ede4`.
The historical PYUSD conversion and return, policy installation, journal
migration/activation, policy calibration, deployment and released lifecycle
proof remain actual work. The production budget activation command was not run.

Fable reviewed the activation wrapper and found no material defect; that review
was read-only and did not independently run tests or production actions.

### Client cutover progress — 2026-09-16

Isolated client work lives in `/private/tmp/loyal-vault-pilot`, branch `ask-2025-voltr-pilot-access`, copied from the existing ASK-2025 demo without local environment, browser wallet or build artifacts. The reader now uses strategy-two config/authority, validates the current version-2 192-byte receipt including reserved bytes, decodes receipt-tracked custody without adding it to measured allocation, and pins adaptor report bounds. It no longer interprets reserved config bytes as report history. Note: the deployed worker pins config v2/ticket v1; repository adaptor v3 source is not the deployed account contract.

Typecheck and scoped lint pass. `scripts/verify-pilot-accounts.ts --live` passed against finalized slot 447474257, including mutated numeric bounds, reserved config bytes and receipt version/padding rejection. Fable identified and the implementation corrected both the receipt-version mismatch and missing numeric-limit checks. Current receipt position/custody tracking and manager USDC custody were zero. This is read-only account-reader evidence, not accounting reconciliation, service activation or lifecycle proof. NAV freshness remains unknown pending consumed-report evidence; deposits remain unavailable, while withdrawal request/claim preparation does not require fresh NAV. Changes remain local and unreleased; signing availability is still pending.

### Client pilot-cap admission — 2026-09-16

The actual deposit preflight now refuses preparation unless the observed on-chain Voltr cap is positive and no greater than 100 USDC. It retains the lower chain cap and actual NAV headroom; the app does not pretend a local limit constrains direct or concurrent deposits. The existing service and NAV gates remain independent, and claims are unaffected. Typecheck, scoped lint and all 20 preflight invariant checks pass, including oversized/zero/negative caps, exactly filling pilot headroom and a one-raw-unit overflow. The chain cap itself has not been changed; signed installation/readback is still required before opening deposits.

### Cap-only activation tooling — 2026-09-16

The historical `activation *-vault-config` command changes fee/HWM-related configuration and targets the historical large cap, so it is not the pilot cap installer. New commands are `activation simulate-pilot-cap` and `activation execute-pilot-cap --journal <absolute-path>`. Execution requires the existing `CONFIRM_MAINNET=1` fence and approved hot admin. Both use the same single MaxCap instruction with target 100,000,000 raw USDC. They pin mainnet and the worker's current Voltr deployment slot/ELF hash, require a lossless v4 vault decode, reject current NAV above the target, and compare the entire simulated/readback vault against the original with only maxCap changed. No fees, HWM, LP state or custody are intentionally changed.

The execution path exclusively creates and flushes a mode-0600 attempt journal containing signed wire/hash, expected signature and original vault before sending. Retain it permanently for that attempt. A rerun with the same journal reconciles the same signature, without loading a signer or creating another transaction. Ambiguous send/readback returns a pending-reconciliation result with the signature. A missing/failed signature never automatically authorizes a replacement; operator recovery remains explicit. This journal records no private key.

Three controlled tests (15 assertions) pass: captured-vault byte preservation and malicious unrelated changes, exact cap instruction/admin, and pending/failed attempt reconciliation with no new prepare/send. Package typecheck still reports its pre-existing `rwa-multiply-strategy2-policy-mask.ts:165` optional-number error; no errors are reported in the new module. This installer has NOT been simulated against mainnet or executed. Git signing and release, actual cap installation, full service activation and live lifecycle proofs remain outstanding.

Fable's recovery review also required binding success to the retained wire and finalized readback. Recovery now uses the existing canonical signed-wire verifier, reconstructs the exact cap-only message, compares the finalized transaction message to that retained message, and reads finalized accounts at or after its finalized status slot. The controlled recovery test now contains a correctly shaped retained cap wire. These fixes pass the same three tests; mainnet simulation/execution remains unperformed.

### Cap instruction live simulation — 2026-09-16

The mounted Environment lacks `SOLANA_RPC_URL`; secret access was slow but the diagnostic completed and returned that explicit missing-variable gate. No secret-access process from these two attempts remains active, and neither sent a transaction. The simulation command now uses a public-RPC unsigned path (`sigVerify:false`) with the approved admin public address, avoiding secret access entirely. Execution still uses the approved signer, signed simulation and durable journal.

A current public simulation against pinned Voltr succeeded at slot **447477664**, estimated fee **5,000 lamports**, compute **5,536 units**, cap **100 USDC**. Evidence: `docs/evidence/voltr-selector-2026-09-16/pilot-cap-unsigned-simulation-447477664.json`. Initial strict byte comparison identified Voltr's automatic `lastUpdatedTs` configuration stamp at offset 656; existing bootstrap proof confirms this behavior. The check now permits only cap plus a nondecreasing timestamp, preserving every other byte and account lamports. Three focused tests /16 assertions pass, including backward timestamp rejection. This is unsigned simulation, not proof of signer access, executed cap installation or deposit readiness. The actual on-chain cap remains unchanged.

The subsequent authorized execute attempt with explicit public RPC stopped before the journal existed. Finalized readback at slot447478148 still shows cap1,000,000,000,000raw; no cap installation is claimed. The exact send code writes its journal before submission, and no journal was created. Required signer availability is being checked separately; do not rerun an execute attempt without resolving that pre-send failure.

The serialized mounted-environment presence check completed with `admin_signer_missing`: `SOLANA_TESTING_PK` is absent. This explains the pre-journal execution failure. An asynchronous request asks the user to populate the existing approved hot-admin signer in the repository's 1Password Environment, never in chat or plaintext files. No further signed cap attempt is authorized by mere elapsed time or a missing answer. Unsigned implementation/protocol verification can continue; signer provisioning is an external deployment dependency. All environment-tool sessions for this turn are terminal.

### Current Maple Kamino leg and release proof — 2026-09-16

Added an opt-in current-compiler exporter and a separate Maple branch of the captured-program harness; the historical Ethena probe remains unchanged. The exporter sizes collateral from finalized reserve prices (slot447479681), exports zero-signature messages with an artificial blockhash, and the read-only capture verifies their installed policy hashes. Captured program/account state is finalized at slot447480268. The actual KLend ELF hash matches the existing pinned9db16dd4… release; captured Squads ELF is1c95bd7b… at deployment slot443245754.

The harness seeded12,676,440raw syrupUSDC (about15USDC at the sizing read),100,000raw USDC and delegate SOL locally. Current Go deposit/borrow/repay/withdraw messages all executed through captured Squads/KLend: borrow credited5,000,000raw USDC, repayment extinguished the debt, and withdrawal returned all12,676,440collateral units with zero remaining receipt/debt. The borrowed cash remains available in this position experiment; this is **not financed entry, a swap test, or a full10-USDC pilot lifecycle**.

A separate branch reconstructs that exact post-borrow state and executes the current pilot release compiler/model. The five-step interest bound is5,000,006raw debt; the predicted release is4,994,113collateral units, leaving7,682,327receipts. Actual execution matched both amounts exactly and left debt unchanged (5,000,000raw at the captured clock), using252,360compute units. This closes the narrow deployed-KLend release-sizing proof for the captured Maple state. Real swap minimums/costs, release-to-payoff continuity, other pilot lanes, initializer/close/recreate, risk/capacity races and released user/partner lifecycle remain unproven.

Reproducible artifact: `docs/evidence/voltr-selector-2026-09-16/maple-protocol-probe-447480268.tar.gz`, SHA-256 `3514e3ad2910e788cfe1b8ec85e1d1f766456d2bd0b7f7833f5f2b9024992870`. It contains the exact plans, final account capture, five program ELFs, and local execution witnesses (11files,1,587,466bytes). Program bytes are retained once, not duplicated in every account-state witness. Extract into an explicit temporary directory, then run `SELECTOR_PROTOCOL_DIR=<dir> SELECTOR_PROTOCOL_RELEASE=1 cargo test -p squads-test-harness --test rwa_kamino_controlled_probe selector_maple_position_executes_current_go_messages -- --ignored --nocapture`. The Go exporters are opt-in `TestExportSelectorProtocolPosition` (`SELECTOR_PROTOCOL_PLAN`, public `SOLANA_RPC_URL`) and `TestExportSelectorProtocolRelease` (`SELECTOR_PROTOCOL_DIR`). Both exporters and the connected Rust probe passed; no signer, send or production state change occurred. Typecheck remains blocked only by the previously recorded policy-mask optional-number error.

`TestSelectorProtocolMessagesMatchCurrentCompiler` also passes against the retained capture: it recompiles all four leg requests and the pilot release with the current Go compiler and requires byte-identical unsigned wires and hashes. Run it with `SELECTOR_PROTOCOL_DIR=<extracted-dir>` to detect compiler drift before reusing this evidence.

Independent replay from a fresh extraction passed both the Rust protocol probe and the Go compiler comparison. Fable's final source/evidence review found no material defect in this bounded proof; Fable did not rerun it. The seeded USDC buffer is 100,000 raw units (0.1 USDC). The release branch ends before swap/payoff, so this evidence does not establish financed lifecycle or deposit readiness.

The operator package typecheck now passes. The policy-mask decoder uses the installed SDK’s `Policy.deserialize(raw)` directly; `fromAccountInfo` delegates to that exact method and never inspects rent metadata. This removes the optional `rentEpoch` type mismatch without changing policy decoding or round-trip validation. No policy or production state changed.

### Partial repayment protocol proof and admission design — 2026-09-16

The current Go finite-repayment compiler now has a captured-Maple branch proof: a 1,000,000-raw-USDC partial repayment debited exactly 1 USDC, left collateral custody and receipts unchanged, and reduced debt from 5 to 4 USDC (remaining scaled debt 4611686018427387904000000). It consumed 211,899 compute units. The branch then executed the existing payoff and withdrawal messages, ending with zero debt/receipts, all seeded collateral returned and the original 0.1-USDC cash buffer. Installed policy hashes still matched before each continuation. This uses the same captured programs/slot and seeded position as the earlier probe; it does not exercise a hard-LTV state, admission, signing, advancing interest windows or a financed lifecycle.

Evidence: `docs/evidence/voltr-selector-2026-09-16/maple-partial-repayment-probe-447480268.tar.gz`, SHA-256 `ca68953d0a00f4fac71e4477fbe9a7ec9956db48dcf785afb53d5c7e8e21af0b`, 1,635,791 bytes. A fresh extraction passed both the Rust probe with `SELECTOR_PROTOCOL_PARTIAL=1 SELECTOR_PROTOCOL_RELEASE=1 SELECTOR_PROTOCOL_DIR=<dir>` and `TestSelectorProtocolMessagesMatchCurrentCompiler`. The latter also reconstructs the partial wire and rejects changed lane, action, amount, payoff/release mode or instruction leg. Export the branch with opt-in `TestExportSelectorProtocolPartialRepayment`; no RPC or signer is needed after the captured position witness exists.

Fable's admission review recommends reusing `pricePhase3ProjectedPositionReturn` with the exact partial-repayment simulation and a distinct `RepaymentProjection`. Preserve recovery admission: current repayment cost plus the newly priced complete remaining exit must fit inside the existing family exit reserve. No budget enlargement or simulated accounting settlement. Before build/send, re-simulate and validate the repayment and remaining payoff/release bounds under the original deadline. After finalized reconciliation, retain unwind intent and existing NAV priority. Prefer available debt cash before collateral funding dust, but cap a partial request strictly below observed debt (`min(cash, debt-1)`); cash equal to observed debt may still fall short of the accrued full-payoff allowance. Essential pending runtime tests cover that boundary, dust starvation, admission rollback, expiration/absence reserve restoration, restart/NAV/unwind, and debt/configuration/custody drift. The current runtime hold remains in place until this admission path and its checks are implemented.

### Partial hard-LTV repayment runtime — 2026-09-16

Implemented pilot-only finite partial repayment using the existing `DeleverRouteStep`, exact Kamino compiler and complete projected-return planner. After funded payoff priority, hard-LTV repair uses debt cash before collateral funding dust and caps its request below observed debt. Admission checks the actual custody prestate, simulates the exact unsigned repayment, verifies fixed conserved debit, unchanged collateral receipts/custody and reduced positive debt, then prices NAV, any funding release/swap, remaining payoff, collateral withdrawal and full return. It retains the existing recovery invariant: current upper cost plus the remaining exit must fit the already reserved family exit. It does not enlarge budget authority, book simulated proceeds or reset spending history.

Build and send re-simulate the retained repayment message. They reject changed custody/debt, expired observation, greater payoff bound/rate, changed interest basis/time window, risk settings/prices and changed redeemable collateral amount. Even increased collateral backing requires fresh pricing because the gross return may grow. Funded payoff tails receive these checks without requiring an unnecessary collateral release. Where release is needed, existing release sizing and funding validation also apply.

Admission atomically creates or preserves an existing source unwind and pauses/clears entry in the same leased budget transaction. The new reason is `hard_ltv_reduction`. This makes principal-equal cash residues continue to payoff after NAV/restart rather than being mistaken for another borrowed tranche. A proven-unspent attempt restores its original reserve but retains the committed risk exit. Current balances and finalized reconciliation still determine each subsequent leg and accounting; simulation never marks the unwind complete.

Controlled regressions cover complete remaining-exit pricing, fixed partial transfer, cash equal to observed principal but below the payoff allowance, collateral dust priority, pilot authority, simulation failure, receipt/cash/collateral/debt changes, funded-tail price/backing drift, atomic DB rollback, durable residual-cash continuation and original reserve/spending preservation on unspent release. The DB continuation test exercises the production persistence/release helpers; it is not a full production admission-to-signer integration test. Fable reviewed the implementation and identified the funded-tail and residual-cash issues; both were corrected, and the focused follow-up found no further material defect. Full worker/PostgreSQL suite passed (22.113 seconds), Go vet and worker build passed, and whitespace validation passed. Changes remain local; no service deployment or live transaction occurred. Full financed lifecycle, current-release admission/sign/send races across all pilot lanes, and deposit readiness remain required.

### Production partial admission and client report ticket — 2026-09-16

Extended the controlled PostgreSQL partial-repayment test through actual `admitPhase3Withdrawal`, its idempotent retry, and `authorizePhase3Build`, including measured pilot execution cost. It passes, and the original reserve restoration/unwind persistence assertions remain. This replaces the prior helper-only coverage limitation for admission and pre-signing authorization; signed-send and live lifecycle evidence remain outstanding.

The isolated client now includes the pinned v1 report ticket in its coherent finalized batch. Its decoder validates owner, address, non-executable flag, exact discriminator/version/bump/strategy, reserved bytes and armed-state consistency. The RPC adapter preserves the executable flag, and missing flag evidence cannot validate the ticket. Public finalized slot 447487174 showed the ticket unarmed with last-consumed sequence zero; receipt position and manager USDC custody were zero. Identity/layout/armed-state mutation checks passed, as did typecheck and scoped lint. This is input for consumed-report reconciliation, not fresh NAV or readiness; the deposit gate remains closed.

The exported preflight verification function was explicitly invoked and passed 20 checks. The broader fast demo verifier ran with local test-server permission and retained FAIL in `/private/tmp/loyal-vault-pilot-fast-report.json`: deployment, genuine NAV/accounting, browser/lifecycle and handoff gates remain missing. It also exposed a verifier assumption that withdrawal arithmetic can run against the current zero circulating LP supply; the pinned SDK correctly rejects those withdrawal calculations. That empty-vault path needs explicit unavailable-quote coverage before the readiness verifier can proceed. No frontend build, deployment, signing or fund movement occurred.


### Empty-vault verifier correction — 2026-09-16

The demo verifier now distinguishes an empty circulating LP supply/asset balance from a funded withdrawal snapshot. It requires both positive withdrawal helpers to refuse with the pinned SDK supply/assets error when empty, and preserves the existing withdrawal/receipt arithmetic assertions for funded snapshots. It does not fabricate an LP supply or turn the unavailable quote into a zero payout. The fast verifier completes R02 without the prior `Invalid LP supply` exception and retains FAIL for the actual missing deployment/accounting/lifecycle gates. Typecheck and scoped lint pass. This is a verifier correction, not a deposit-readiness change.


### Consumed-report wire decoder — 2026-09-16

Added a narrow client decoder for the current zero-capital `REPORT_NAV` transaction: one pinned delegate/Squads/NAV-policy instruction, exact two-instruction arm/Voltr payload, full pinned account arrays, identical report payloads, zero capital amount, supported report version, nonzero sequence/digest, sequence equal to observed slot and bounded NAV. It returns wire facts only and makes no signature validity, finality, consumed-report or freshness claim. The current Go `CompileBridgeMessage` generated the checked-in unsigned vector (`scripts/fixtures/client-report-compiler-vector.json`); it is artificial and never submitted. `verify-report-wire.ts` matches the current compiler report/message hash, rejects all 1,027 truncations and five amount/sequence/NAV/digest/mode mutations. Typecheck and scoped lint pass.

Fable confirmed the minimal next integration: existing server-side DB access locates the latest report signature; finalized transaction/trace verification supplies the evidence; each public and wallet-preparation coherent batch separately matches current disarmed ticket/sequence, receipt NAV and age. Candidate worker observation report fields must never supply consumed evidence. Post-report capital mutations, unresolved work, service/release readiness and cap checks remain separate deposit gates. The decoder is not yet wired into that server verification path, so deposits remain unavailable.


### Initializer installation artifacts and first live simulation

Added the offline `compile-backyard-multiply-initializers` binary, reusing the existing exact-lane compiler. It requires the caller's finalized Settings seed and emits three bounded legacy installation instructions; it never signs or sends. Public finalized Settings at slot 447651751 had seed 148, threshold 1, zero timelock and only the approved BAq… admin with mask 7. Candidate policies are seeds 149–151, each 767 bytes. These are not installed manifest pins.

Reproduce the artifact with `cargo run --locked --offline -p loyal-actions --bin compile-backyard-multiply-initializers -- --policy-seed-before 148` (refresh the seed first). From `tools/backyard-voltr`, run `bun src/verify/simulate-multiply-initializer-install.ts <artifact> <new-output>`. The simulator rechecks finalized genesis, Settings authority and seed, enforces packet size, disables signature verification, and writes evidence exclusively. The retained run passed for seed 149 at slot 447652263 with 38,915 CU. Evidence includes the compiler artifact SHA-256 and simulated Settings/policy accounts. Typecheck and compiler build pass.

Only the first installation is simulated against live state: later sequential seeds require preceding installed state. Prior captured-program tests cover three-policy installation mechanics but cannot establish current installed authority. Durable signed installation/recovery, finalized policy-byte verification, activation manifest pins and released lifecycle remain outstanding. No policy was installed and no transaction was broadcast.


Initializer installation preparation now reuses `assertPolicyMatchesArtifact` from the existing installer for complete decoded authority/constraint verification. The initializer artifact reader accepts only the retained reviewed compiler bytes (SHA-256 `97da2aa6cb44bfd5bdabb58d064efbf0d7c9646cf9602eb8094c80bb22626d33`), binds all three canonical seeds/PDAs and checks each policy's delegate, threshold, account index and constraint count. Changed compiler output requires review rather than silently changing installation authority. The deployed simulation allocates 934 bytes, of which 576 decode and the remainder must be zero.

The updated unsigned simulator verifies post-Settings seed/authority and the complete policy, including its timestamp against the simulation slot's block time. It passed at finalized slot 447652911, 38,915 CU, with no broadcast. Three controlled tests reject changed artifact authority, wrong lane/owner/executable/rent, truncated/nonzero padding and time mismatch. This does not supply installed account hashes: the timestamp and hence raw hash are simulation-specific. Signed installation and recovery are still to be connected; no manifest was enabled.


### Sequential initializer installer — local implementation

`tools/backyard-voltr/src/activation/install-multiply-initializers.ts` now provides one-policy installation and retained-wire recovery. Invoke it with the reviewed artifact, index 0–2 and an absolute journal path. A fresh attempt additionally requires `--execute`, `CONFIRM_MAINNET=1` and the approved admin signer through the mounted environment. Rerunning the same journal performs read-only recovery without needing the signer. It never automatically replaces or resends a retained attempt; unavailable finality remains pending and finalized failures are reported explicitly.

Before signing, it checks the live Settings authority and consecutive seed, prior exact initializer policies, absent candidate addresses and the captured Squads ELF hash. Signed simulation checks full policy semantics, rent and bounded fees; a second read must preserve all protected account bytes and balances. The exact signed wire is cryptographically verified and exclusively persisted with flush before a single submission. Finalized recovery matches the transaction message, verifies native account effects and current policy bytes, and emits a manifest binding only after those checks. It does not edit or enable the manifest.

Five artifact/readback/retained-wire tests (15 assertions), tool-package typecheck and whitespace checks pass. Tests cover signature refusal and wrong-wire/lane substitution, not a successfully signed admin transaction or a full installer RPC lifecycle. Signing access is still unavailable, no installer execution occurred, and broader recovery/race testing remains necessary before using the live command. Fable's additional review was unavailable due to a usage limit.

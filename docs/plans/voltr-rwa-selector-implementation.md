# Voltr RWA selector implementation status

**Status: local shadow implementation and safety fixes. Autonomous rotation is
not complete and this branch is not ready for partner capital.**

Base: `fleet/integration` at `0058abf5ce6e386a3e5245231b99b11a4fce595e`.
Work is isolated on `feat/voltr-rwa-selector`; the older dirty checkout and the
suspended production worker were not changed.

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
   Voltr. Unsupported partial hard-LTV repayment is a truthful recovery hold,
   so autonomous partial risk reduction still needs implementation and proof.
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

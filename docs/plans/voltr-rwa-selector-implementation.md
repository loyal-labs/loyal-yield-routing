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

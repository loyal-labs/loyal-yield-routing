# Codex handoff — Backyard RWA Phase 3 all-market support

Implement v2 at docs/plans/backyard-rwa-phase3-family-activation-verifier.md.
That file alone defines acceptance; this handoff supplies implementation order.
The operator requested the revision to remove late cap/proof blockers and ship
all catalogued markets through one shared implementation.

## Runtime goal text

> Ship deployed support for the exact 11 catalogued Backyard RWA lanes on
> mainnet-beta under docs/plans/backyard-rwa-phase3-family-activation-verifier.md
> v2. Extend the existing serialized Go worker, preserve its journal/lease and
> installed policy authority, and enforce 1/20/60 USDC gross caps with reserved
> exits before broadcast. Verify all lanes in batches and run three new-family
> canaries ending LIVE_VALIDATED or proven CAPACITY_PENDING. Preserve Prime/Maple
> proof. Complete only when verify:rwa-phase3-family-activation proves R01-R08,
> deployment identity, truthful readiness and flat finalized canary custody.
> Follow the contract's standing authorization, expiry and exclusions; report
> precise external gates after independent in-scope work is exhausted.
> Documentation completion does not complete this implementation goal.

Use one runtime goal and worker queue. Verify actual Phase 2 goal closure and
unresolved work before live activation; PR #223 alone does not prove goal/lease
status. Replace any superseded runtime objective explicitly, rather than trying
to satisfy v1. Do not create another competing goal on the serialized worker.

## Authorization

The contract's standing envelope covers local implementation, tests and
signed-unsent simulation, three bounded canaries, all-11-lane runtime support,
forward-rollover repair, committed immutable deploys and admin squash-merges of
docs-only evidence PRs. Code changes follow normal validation/merge rules.
Proceed without another approval inside that envelope; stop at its explicit
identity, authority, cap, destructive-action or scope boundaries.

Caps remain 1 USDC per transaction including fees, 20 per family across all
attempts/cleanup, and 60 for the goal. Three times 20 leaves no extra margin.
Sibling support does not authorize additional funded canaries or indefinite
capital allocation after the finite goal expires.

## Implementation details and order

1. **Baseline and feasibility.** Start from current main, preserving unrelated
   changes. Bootstrap the sole read-only verifier and capture all baseline
   conditions before worker edits. Resolve exact identities, custody/exposure,
   credentials/accounts, simulation and deployment access. Prove a bounded
   controlled-state simulation sample before depending on it. Measure each
   full lifecycle under the new accounting; record finite stage deadlines and
   concrete external gates. Required setup/protocol closes must surface here.
2. **Governor and shared runtime.** Extend the existing package/journal. Persist
   spent amounts, unresolved reservations and conservative exit reserves with
   the lease; enforce admission before signing and authorization before sending.
   Carry explicit mint/program/extensions/decimals, debt custody, USDC valuation,
   repayment and conversion for USDC/PYUSD/USDG/USDS. Reuse RuntimeRoute and
   remove Maple-only/two-route assumptions where necessary; no worker clones
   or general plugin framework.
3. **Decisive proof.** Exercise cap/exit-reserve rejection without signing/sending,
   restart and ambiguous-submission reservations, and successful reserved-budget
   unwind. Batch proof for all 11 bindings and paths. Reuse unchanged structural
   evidence while explicitly covering token programs, decimals, farms and swap
   differences. Repair only observed policy defects through forward rollover.
4. **Deployment and queue.** Deploy verified immutable source and confirm R01
   before any live Phase 3 broadcast. Keep one worker lease while processing
   OnRe/ONyc/USDC, AUTO/AUTO/PYUSD, Ethena/USDe/PYUSD, subject to evidenced
   same-family substitution. Advance only when flat/reconciled or after an
   unfunded capacity HOLD with no exposure/unresolved submission. A partial
   entry must unwind. This fixed activation sequence is explicitly allowed;
   moving open positions between markets and yield selection remain excluded.
5. **Truthful readiness.** Funded canaries complete their full lifecycle.
   CAPACITY_PENDING requires a real deployed unfunded capacity HOLD, fresh
   evidence, no exposure/submission, and complete controlled-state execution
   through production builders and actual enforcement. It is not current-chain
   or future-capacity proof. Missing code, policies, accounts, funding, quotes
   or tooling cannot use this outcome. All 11 lanes need support; six siblings
   require batched proof rather than another live funding matrix.
6. **Close-out.** Reconcile deployed identity, exact lane matrix, policy bindings,
   budgets/reservations, lease, operations and finalized custody. Update existing
   admin/partner handoff, promote stable checks, run every R01-R08 condition,
   retain results and close only on current PASS.

The Phase 2 recorded 13.651629 excludes restoration/fees and does not normalize
all assets to USDC. Do not use it to certify these caps. Measure the real action
graph, reserve exits before entry, and shrink canary size if needed without
removing required lifecycle actions. Never bypass a cap to recover.

## Rules for keeping the goal moving

- Diagnose the first false condition with a discriminating check; preserve its
  evidence and fix the implementation. Do not repeat unchanged expensive suites,
  redeploy without a fix, or introduce acceptance gates at close-out.
- Retry capacity only after meaningful economic-state changes; a new slot is
  insufficient. No blind resends or unbounded command/retry loops.
- Finish independent local work and eligible families around family-specific
  gates, but never bypass shared lease/accounting/ambiguous-submission failures.
- Report LIVE_VALIDATED, SUPPORTED_READY and CAPACITY_PENDING separately.
  Controlled-state success is not live finalization evidence.
- Consumer webapp, Earn Max UI, optimizer, arbitrary routing, new authority/
  programs, destructive closes, second writers/journals and unrelated work
  remain excluded under the contract.

Run the sole verifier through the mounted secret environment without reading
or copying it:

```sh
op run --env-file=.env.1password -- \
  bun run --cwd tools/backyard-voltr verify:rwa-phase3-family-activation
```

Do not claim the command exists or passes until implemented and executed.

## Implementation checkpoint — 2026-09-04

The implementation goal is active in Codex. Baseline and fresh token/capacity
reads are retained under `docs/evidence/backyard-rwa-go/phase3/`. The sole
verifier exists but remains a fail-closed measurement scaffold: R01-R08 are
NOT_IMPLEMENTED, not satisfied by the presence of new source or tests.

Implemented locally: fixed 1/20/60 budget accounting with exit reservations;
lease-locked PostgreSQL reservation and wire binding; guards before production
bridge/Kamino/Jupiter signer loading and at broadcast intent; decimal-aware
reserve valuation and target borrowing. The race-enabled Go suite passed with
the disposable PostgreSQL test enabled, covering concurrent duplicate admission,
restart retention, unbound-wire rejection and stale-writer fencing. This is
not yet the complete R01 witness, migration validation or live proof.

Do not deploy this intermediate implementation: reservation producers, fresh
cost/fee/exit estimation, typed durable HOLD handling, finalized settlement and
safe expired-unsent release still need integration. The new gates intentionally
refuse transactions without an initialized durable goal budget and admission.
No production budget was initialized; no Phase 3 transaction was signed or sent.

Next: finish the bounded simulation/setup/cost preflight; complete R01 admission
and recovery end-to-end; extend the shared runtime and exact bindings to all 11
lanes; implement the flat sequential queue; replace every verifier scaffold
with authoritative behavioral measurements before deployment/canary closeout.
Fresh state confirms ONyc/USDe use 9 decimals. PYUSD/USDG include Token-2022
extensions, currently disabled transfer hooks and zero transfer-fee schedules;
decode and recheck these settings in production rather than assuming plain SPL
accounts. Non-USDC debt custody/NAV/conversion is still unfinished.

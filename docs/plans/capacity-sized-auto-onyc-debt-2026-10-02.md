# Capacity-sized AUTO/ONyc debt implementation plan

**Goal:** Allocate eligible equity to one existing AUTO or ONyc position while borrowing only the amount fresh reserve capacity and unchanged risk/economic limits permit.

**Architecture:** Keep the current single-active-route journal and existing transaction graph. Separate collateral-equity capacity from additional debt room; quote and bind exact raw borrowing. Desired leverage remains a ceiling; effective positions may lie between 1x, 1.5x and 1.75x. Use actual holdings for carry and a stable ratio for partial withdrawals.

**Stack:** Existing Go worker, pgx journal, Kamino/Squads/Jupiter builders and tests. No new dependencies.

## Approved boundary

- User approved this first implementation after the ASK-2316 study. Code/tests only; no deployment, production DB writes, canary, policy updates, alert changes, monitor rearming, fee changes or HWM changes.
- Base is live `0bfa72edd603a5515b9de66b0cc62c69af2f83bc`, not unrelated main/fleet ancestry.
- AUTO/AUTO/PYUSD and OnRe/ONyc/USDC only. Keep one position and existing entry authority. Maple, PRIME and other lanes retain their contracts.
- Keep max desired leverage 1.75, post-move LTV 4500 bps, instant borrow LTV 5000 bps, hard/release limits, 10,000,000-raw minimum borrow, partial-withdrawal limits, pilot equity and spending limits unchanged.
- Keep hourly routine NAV, urgent reporting, exact admin fee 2000/seven zero terms, warning-only fee accumulation, fee forecast guards, ownership/custody/ticket/program checks.
- No concurrent portfolio, higher TVL, leveraged top-ups, new route or new persistence framework.

## Grounded caller map

Codebase graph MCP is unavailable in this runtime. Parent used exact source reads and reference scans of every Go file in `internal/backyardrwa`, including tests. `caller-map.json` in the task artifact directory records the affected symbols; search again before changing signatures, JSON/type/string references and callers. These are task-directed references, not a whole-repo completeness claim.

- `kaminoPairEntryCapacityAuthorized`: called from destination quote collection; the reviewed wrapper also feeds `route_observe.go`. Today it returns min(2*fee-adjusted debt room, collateral/deposit ceilings), so scarce borrowing clips equity.
- `observeSelectorDestinationForecastAuthorized`: currently falls back to 1x only at zero capacity plus utilization blocked, and not on reentry. Its forecast borrows a fixed 50% of the initial collateral. Quote fields already carry `BorrowReceiveRaw`, `BorrowFeeRaw`, prices and asset outputs.
- `leverageBorrowStep` -> `selectKaminoLeg` -> `leverageUpBorrowRaw` -> `leverageUpCapFee` in `execution_observe.go` -> admission/build/final-send validation. Existing target marker150/175 is not the raw wire amount.
- `decideLeverageTarget` stores desired discrete levels and uses fee-reserved economics. `selectorTrancheInProgress` treats unmet targets as unfinished. Both must distinguish a completed capacity-limited position from a borrow/redeposit leg still in flight.
- `currentLeverageLevel` snaps every debt position to 1.5/1.75. `currentPositionAPY` also snaps. `leverageSpread` subtracts an assumed half-equity existing loan. These assumptions do not describe a capped position.
- `partialWithdrawalTargetLTVBPS` uses the stored desired level; later release/swap/repay/stage legs use that target. Replacing it with the changing current LTV on every leg is WRONG: releasing collateral would raise the target and erase repayment. Preserve a stable admitted ratio through the journal/restart boundary, or an equivalently proved reconstruction from attributed state.

## Task 1 — capacity and exact entry/borrow contract

Files: `kamino_pair_capacity.go`, `route_observe.go`, `state.go`, `selector_destination.go`, `selector.go`, `selector_entry.go`, `execution_observe.go`, relevant borrow admission/prestate/final-send validators and their existing tests.

- [x] Red cases: eligible $100k collateral with $10k additional debt room retains eligible equity and quotes a <=$10k loan, rather than clipping equity to $20k. Known zero room produces explicit 1x. Unknown/invalid evidence cannot authorize borrowing. Non-AUTO/OnRe behavior stays unchanged.
- [x] Extract/reuse same-batch debt-room arithmetic: available liquidity, utilization, global debt, net withdrawal cap, outside-eMode limit, queue/config and protocol restrictions. Include exact fee/rounding; preserve existing margins rather than inventing a large reserve.
- [x] Distinguish additional principal/fee-inclusive liability room from total debt. Zero new room never means existing debt must be repaid. Total exposure remains bounded by existing debt plus new room and existing risk caps.
- [x] Separate collateral-only equity room from financing room for approved lanes. Cap the loan against actual conservative entry collateral, debt-unit prices, exact fees and redeposit/deposit room. Use `Unlevered` only for a truly zero-loan recipe.
- [x] Preserve debt raw units (including non-par PYUSD) and bridge USDC equity. Reuse existing price authority; same decimals are not parity. Unknown data holds rather than silently becoming zero.
- [x] Entry and B2 UP must bind a fixed reviewed raw amount before signing. If fresh room shrinks below it at admission or final send, reject and replan; never resize an already-built/signed wire or admit a larger loan after room expands.
- [x] Trace decide -> prepare -> refreshed decide/decisionsEqual -> admission -> build -> validateForDelegate/final send. Tests must reach these boundaries, not merely test a sizing helper.

## Task 2 — settled targeting, economics and withdrawals

Files: `leverage_target.go`, `leverage_up.go`, `leverage_level.go`, `selector.go`, `leverage_watch.go`, `current_apy.go`, `partial_withdrawal.go`, any minimal journal support required, and their existing tests.

- [x] Red cases: a completed 1.1x/1.36x capacity-limited position is not perpetually unfinished and does not repeatedly request an impossible 1.5x borrow. Real borrow/collateral cash in flight remains unfinished and must reconcile first.
- [x] Carry separate desired ceiling and effective position semantics using the smallest existing-state extension needed. Retain economic minimum benefit/fee reserves and existing spread policy. UP pays the exact clipped loan's full cost; DOWN and hard risk/withdrawal actions remain live with missing new-entry evidence.
- [x] Use actual collateral/debt and current borrow cost for held-position APY, and actual existing raw debt when projecting incremental reserve utilization. Do not count existing debt twice or subtract a hard-coded 0.5*equity. Keep the app and watch's shared APY source consistent; benchmark level columns may remain hypothetical but must use correct source debt.
- [x] Feed/priced persistence must use the same capped amounts as executable quotes. A full-size hypothetical 1.5x advantage cannot authorize a weak capped loan or keep its window alive incorrectly.
- [x] Preserve a stable actual/effective target across a partial withdrawal's release -> swap -> repay -> stage sequence and a process restart. Demonstrate on a 1.36x-style position that the debt share is repaid and the remaining position keeps its admitted ratio within existing rounding/drift tolerance. Preserve 90%/minimum-remainder/six-round full-exit fallbacks and explicit down-to-1x behavior.
- [x] Reuse debt-free top-up then capped borrowing. Do not add direct top-up alongside debt or force leverage reductions just because new reserve room is zero.

## Task 3 — verification, documentation and release candidate

- [x] Focused red/green contract tests: positive/zero/unknown/shrinking room, fees/overflow, protocol/queued/eMode exclusions, non-par PYUSD, fully available legacy behavior, actual APY/borrow deltas, partial/full withdrawal and restart, stale quote/oracle, one nonterminal operation, no duplicate borrowing and no authority expansion.
- [x] Include `validateForDelegate`/final-send and prepared immutable-wire checks for changed loan paths. Default fixtures must not waive new guards; use coherent evidence for real-money assertions.
- [x] Run scoped native Go tests and race, `go vet ./...`, `go build ./...`; full suite comparison against the unchanged base. Prior full base has seven SDK dependency/parity top-level failures plus seven nested catalog cases; report current failures and skips honestly rather than fixing unrelated baseline issues.
- [x] Run changed DB contracts against the existing disposable local `/private/tmp/backyard-phase3-pg.vlad` socket and `phase3_budget_test` database only, when available. Its native helper safety guard must remain. Do not create services or use production credentials. Existing test data may need narrowly scoped fixture cleanup, never production state.
- [x] Update owning worker docs and this plan with behavior/limits. Report remaining exact-wire/live gaps; implementation/tests do not grant production authority.
- [x] One consolidated independent code review after implementation. Commit reviewed code with conventional commits, no coauthor attribution. Parent owns PR/push/activation decisions; implementation worker does not push.

## Required counterexamples and acceptance

1. Fresh loan room is 10m raw, desired receive is 50m: fixed request <= fee-safe10m; equity is retained if collateral capacity allows. Room0 => explicit1x, unknown => no borrow.
2. An existing funded position has debt40m and additional room0: keep its debt absent an existing economic/risk down rule; do not derive target debt0.
3. After quote/build for receiveX, the room falls below X+fee: final validator rejects before send; a later larger room never mutates the signed amount.
4. A settled C136/D36 position has 1.36x carry, not 1.5x carry. With a partial user exit, its admitted ratio survives release/repay and restart; target cannot be recalculated upward from temporarily reduced collateral.
5. Fees stay2000bps, NAV remains gross assets-debt, policy/capital caps and exact route identity remain enforced. PRIME/CASH/PST/reUSD remain unadmitted.

## Ownership and stop conditions

One implementation worker owns the coupled Go changes and covering tests. Parent does not edit those files concurrently. Report the chosen capacity/ratio persistence design before a large rewrite. If preserving the withdrawal/restart invariant requires a new schema, policy change or disabling partial withdrawals/economic rotation, stop and explain the smallest alternative before expanding scope. No nested agents or new research wave.

## Candidate implementation contract (2026-10-02; not activated)

- `capacity_borrow.go` observes additional fee-inclusive liability room. AUTO/OnRe collateral equity no longer inherits the 1.5x financing bound. Zero additional room does not target existing debt for repayment.
- Fresh reserve/oracle/clock evidence produces 1.5x/1.75x raw ceilings. Target economics prices the clipped increment using existing budget price intervals, fees and minimum-benefit guards. The desired discrete level remains a ceiling, not a claim about actual holdings.
- `leverageTarget.borrowRaw` and `sourceDebtRaw` are one economic authorization. `operationId` is set atomically with its decision. New decisions cannot reuse it; final entry authority requires that same operation ID and fixed receive. Old level-only JSON remains readable but grants no new loan authority. Already signed legacy operations are not compatible with this new approval fence. They must be resolved under the mandatory stopped/fenced upgrade boundary below; they are never silently resized.
- The first partial release atomically captures actual LTV in `state.partialWithdrawal`, bound to its lane, generation and originating operation. The origin and every continuation retain the immutable context in operation evidence. Planning, admission and send verify provenance/context. Restart, repayment and cancelled demand do not reset the ratio. A matched terminal observation can clear it only after cash is settled and debt returns to the ratio. Sparse safety holds preserve it.
- Current APY uses actual supplied/idle collateral and current borrowing cost. Hypothetical watch level columns remain benchmarks. Their USDC debt value is converted with the matching observation’s debt price/decimals before subtracting explicit source raw debt. Missing non-USDC authority omits leveraged APY columns and `spreadBps` (logs show `spread=unavailable`); 1x and current held-position APY remain available.

Validation and remaining release gaps are recorded in the implementation report under `/tmp/voltr/ask2316-capacity-implementation-20261002/`. No deployment, policy/capital increase, fee change or production test is authorized by this candidate.

## Mandatory stopped/fenced upgrade boundary

This is an operational prerequisite, not authorization to perform a rollout now.
The old image can create level-marker UP operations that the candidate correctly
refuses. A momentarily idle journal is **not** an upgrade boundary. Selector-entry
pause is **not** sufficient: B2 borrowing bypasses that pause. There is no new
maintenance-mode flag in this change.

1. Inventory and stop/suspend **all old decision producers** for the route: the
   worker service, replicas, one-off executions and any CLI execution sessions.
   Use the platform's existing stop/suspend controls. Capture evidence that every
   producer stopped. Record the exact old image digest and manifest for recovery.
2. Confirm the route lease is expired or has been fenced through the existing
   reviewed lease mechanism. Do not fake expiry by editing journal rows. Record
   the lease owner, fencing token, expiry, state version and inspection time.
   A process can be stopped while its lease remains live; wait for its expiry
   before treating the route as quiescent. Do not start the candidate yet.
3. While all producers remain stopped/fenced, inspect the **full nonterminal
   journal**, including legacy states and ambiguous submission/signature evidence.
   Read-only SQL against the route's existing tables can provide this evidence:

   ```sql
   SELECT route_key, state_version, lease_owner, fencing_token,
          lease_expires_at, state->'leverageTarget' AS leverage_target,
          state->'partialWithdrawal' AS partial_withdrawal,
          state->'selectorUnwind' AS selector_unwind
   FROM loyal_yield.multiply_route_states WHERE route_key = :route_key;

   SELECT operation_id, action, status, strategy_key, transaction_signature,
          broadcast_intent_at, confirmed_slot, expected_effects
   FROM loyal_yield.multiply_operations
   WHERE route_key = :route_key
     AND status IN ('prepared','signed_persisted','broadcast_intent','confirmed',
       'reconciliation_pending','decided','built','simulated','signed',
       'submitted','reconciling','manual_recovery')
   ORDER BY created_at, operation_id;
   ```

   `:route_key` denotes the operator's bound query parameter, not a worker CLI
   flag. Do not print signed wires or credentials. Manual-recovery and ambiguous
   rows require an explicit disposition; an empty subset of recent rows is not
   proof that the full journal is clear.
4. Independently inspect a fresh confirmed **same-route, manifest-bound** account
   batch: obligation/receipts and debt, collateral custody, debt custody, bridge
   USDC custody, Voltr strategy custody, report ticket and the route journal's
   latest reconciled effects. Also inspect leverage/withdrawal/unwind state.
   Prove there is no unfinished borrow/redeposit, released withdrawal collateral,
   repayment cash, staged-but-unrestored cash, ambiguous submission, armed report
   ticket or unreconciled accounting transition. A settled funded position may
   remain; do not confuse outstanding settled debt with unfinished borrowed cash.
   The existing `--selector-shadow` command is read-only observation/economics
   support, not a maintenance fence or complete legacy-recovery certificate.
   `--inspect-pilot-flat-state` is only a flat-state check; it does not certify
   a funded intermediate chain. Use the journal query and manifest-account
   evidence together. No new catch-all CLI check is claimed here.
5. If any journal/account/context evidence is dirty or uncertain, **do not replace
   the image**. Only the exact old image and an explicitly reviewed recovery path
   may reconcile/drain the old operation. If that path needs an old producer
   restarted, it invalidates the boundary: stop/fence it again and repeat steps
   2–4 after resolution. Never erase/relabel a pending or ambiguous operation,
   grant a fresh approval to an old signed wire, or resize/re-sign it as a shortcut.
6. Once the full inspection is clean, keep every old producer stopped throughout
   replacement. Start **only** the reviewed candidate with its reviewed manifest.
   The inspection ceases to be valid if any old producer runs again in between.
   If the platform/operator cannot guarantee this exclusion, do not deploy;
   require a separately reviewed same-operation legacy recovery path or
   maintenance gate first.
7. Rollback is also a gated decision. Keep all old binaries stopped: they must
   never receive candidate raw-sized or context-bound operations. Resolve every
   candidate operation and unfinished chain using the reviewed candidate or an
   explicitly reviewed candidate-aware recovery path. Then repeat the stopped/
   fenced journal, custody and context inspection from steps 2–4. Before any old
   binary starts, review compatibility of all remaining durable state and funded
   positions, including fractional leverage and withdrawal behavior. If a clean,
   old-compatible state cannot be proved, rollback remains blocked; use candidate-
   aware recovery instead. Never erase an operation or issue a fresh approval to
   make a rollback possible.

No stop, lease mutation, journal inspection in production, drain, rollback,
replacement or activation has been performed by this implementation task.

## Verification completed (2026-10-02)

Implementation and the single independent review are complete. All three review
findings were closed, including the explicit candidate-aware rollback rule.
Final full suite: 1,411 named passes, 98 skips, and the seven known SDK/parity
failures plus seven nested cases; no new failing name. Focused race plus the
existing disposable database: 81 named passes, zero skips or failures. Vet and
worker build passed. Existing baseline SDK failures still require the normal
CI dependency environment; skipped tests are not claimed verified.

Captured/synthetic account and local test-key checks cover the changed money
boundaries. No live canary or full production lifecycle was executed. Production
activation remains separately gated by the stopped/fenced runbook above and
explicit approval; this plan does not authorize deployment.

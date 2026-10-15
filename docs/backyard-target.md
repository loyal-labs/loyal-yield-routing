# Backyard target

Every tick Backyard computes where the vault should be, `target(chain, rates,
inputs)`, and takes one step toward it, `step(chain, target)`. The target is
never stored. A restart, a lost operation or a changed input needs no
recovery of a plan: the next tick computes the target again from what is on
chain.

## Inputs

| Input | Where it lives | Written by |
|---|---|---|
| The chain | one confirmed batch: every registry lane's obligation and custody, the reserves, Voltr | the view |
| Withdrawal demand | Voltr's withdrawal receipts in the same batch | vault users |
| The rates | the selector's latest result (`state.selector.result`): the lane the vault's cash enters (`ENTER`), on a quote current at the batch, or a held lane's tranche to rotate into another (`ROTATE`), sampled within `MaxSampleGap` of the batch | the selector sample |
| A lane's level | `loyal_yield.backyard_lane_levels`: 1, 1.5, 1.75, 2, 2.5 or 3, never above the lane's cap, and whether the up step toward it is worth its cost at the last sample | the selector sample |
| An operator exit | `loyal_yield.backyard_lane_exits`: lane X target debt 0, optionally equity 0 | `loyal-engine backyard exit-lane` |

Nothing else about a move is stored: no unwind intent, tranche,
partial-withdrawal record, working lane, selector entry or debt-clear
authority.

## Rules, in priority

1. **Hard LTV.** A held lane at or past its brake steps down to its level's
   LTV. The brake is the lower of the collateral reserve's
   `liquidationThresholdBPS - 1000` and the obligation's borrow limit (max
   LTV; the reserve's before a deposit) - 300 bps (`stepWithdrawMarginBPS`),
   so a down step at the brake still has withdraw room. A brake at or under the cap's LTV (a lender cut) stops entries and
   up steps on the lane, never this step: it then steps down to the highest
   level under the brake, and repays all of it under 1.5x's.
2. **Term maturity** (wYLDS, 30-day term). Under 1 day left without room for
   the keep-alive borrow, or past maturity: debt 0. Under 3 days with room:
   the keep-alive borrow (the minimum up step); an unfunded withdrawal goes
   first until the term's last day. A term whose end is unknown holds
   `stale`.
3. **Operator exit.** Debt 0; with `--equity-zero` the collateral then leaves
   as USDC: one atomic step withdraws it and swaps it to USDC in the Squads
   bridge account, so none of it waits in a custody another lane shares.
   That USDC is vault cash: a withdrawal stages it to Voltr, otherwise rule 5
   enters it in the selector's current destination, or it waits for one. A deposit left at or under dust is
   rounding residue, not a leg: the exit is done.
4. **Custody in flight** finishes first: collateral a held lane's next leg
   takes (above dust and, for a deposit, at least the one collateral receipt
   KLend mints; less is vault cash its next step takes whole), in the
   withdrawal's or an exit's direction when there is one, and
   debt-mint cash above dust that a withdrawal needs or no held lane that
   steps owes, which converts toward Voltr (wYLDS stakes to AUTO collateral).
   Custody is vault cash; debt-mint cash makes no lane held.
   Then **withdrawal demand** beyond Voltr idle: Squads cash stages, and the
   held lane with the most equity (an overflow lane included) releases the
   rest plus 1% (at least $1) at its own LTV, up to under 90% of its equity;
   the next lane frees what is still missing. 90% of a lane or more is the
   operator's exit.
5. **Selector choices.** An overflow source (AUTO/AUTO/PYUSD) below its level
   with its own borrowing closed moves the collateral it can release down to
   its cap's LTV into its overflow lane, when that lane's level is
   approved, its reserve has room and it has levered the previous move.
   **Cash:** the vault's Squads cash, and the Voltr idle beyond the queue an
   `ENTER` quote current at the batch allocates, enter the selector's latest
   destination (an `ENTER`'s lane or a `ROTATE`'s), held or not: an entry
   beside a position is a top-up. A flat vault's view is that lane's; its
   allocated cash waits for the selector while no entry is current, and
   returns to Voltr when the lane cannot take it. Squads cash is the
   destination's whenever the selector names one, viewed or not (unviewed,
   it waits). Beside held lanes, Squads cash with no destination named waits
   (`stale`) for the selector's next choice, except a USDC-debt home lane's
   step excess, which joins its position. A destination whose obligation is
   absent initializes it first, for an entry's allocation or a rotation's
   Squads cash alike.
   **5r. Rotation** comes before the overflow move: while the latest result
   is `ROTATE` (or `CANARY_ROTATE`), current within `MaxSampleGap`, its
   destination is viewed and not exiting, and the source holds no more
   collateral than the position it priced, the source releases its tranche
   (`sourceRelease`: the equity's share of its debt and of its collateral
   receipts, under 90% of its equity) at its own LTV in two steps: one
   repays the debt share (`rotate_repay`), one withdraws the collateral share
   and sells it to USDC in the Squads account (`rotate_withdraw`). No
   collateral waits in custody, so no rule reads the selector for a custody's
   direction; the cash rule then enters the USDC in the destination. A
   rotation is left while its release is left on the source or, current
   within `MaxSampleGap`, Squads holds cash; while one is left the selector
   records no sample, and a sample that holds for recovery or safety records
   nothing either: the release and its cash's entry finish from the result
   that started them, and no next tranche is released while this one's cash
   is idle (Looping, 2026-10-14: a KEEP recorded right after the release let
   the residue rule put the USDC back into Maple, and the canary released
   again). Every lane's level stays frozen while the latest result is not
   `KEEP`.
6. **Level.** Down to the stored level (or the lane's cap, when lower) from
   above its band; up toward the stored level when the selector approved the
   step and the reserve has room.

Only rules 1 to 3 ever set debt 0. A stored level of 1 is economic: the lane
holds at 1.5.

## Levels and caps

One ladder (`leverageLadder`, `leverage_level.go`) holds every level and the
spread rule at it. The spread is the lane's token yield less the borrow APY
once the step is borrowed (up) or at today's borrowing (down); a lane moves at
most one level per selector decision, and its stored level advances no
further than the up step that decision priced, one level above the position.

| Level | LTV | Up to it at spread | Down from it under |
|---|---|---|---|
| 1.5x | 33.3% | 1 pt | 0 |
| 1.75x | 42.9% | 2 pts | 1 pt |
| 2x | 50% | 2.5 pts | 1.5 pts |
| 2.5x | 60% | 2.5 pts | 1.5 pts |
| 3x | 66.7% | 2.5 pts | 1.5 pts |

Each lane climbs no higher than its cap (`maxLevel`, `route_catalog.go`;
owner decision 2026-10-14). An up step lands, after a 1% conversion loss and
the borrow fee, within the cap's LTV; the obligation's borrow limit (KLend's
allowed borrow value over its deposits, the number it enforces, also the down
step's withdraw room) bounds the borrow before the redeposit, so a step to a high level may take more than
one transaction.

Each cap is the highest level whose LTV sits under the lane's brake (owner
decision 2026-10-15); the spread rule, not the cap, keeps a thin or
negative-spread lane low. Lender limits as read from the reserves on
2026-10-15:

| Lane | Cap | Liquidation | Max LTV | Brake |
|---|---|---|---|---|
| AUTO/AUTO/PYUSD, AUTO/AUTO/wYLDS | 3x (66.7%) | 80% | 78% | 70% |
| OnRe/ONyc/USDC | 2.5x (60%; 3x sits over the brake) | 75% | 66% | 63% |
| Maple/syrupUSDC/USDC | 3x (66.7%) | 90% | 88% | 80% |
| Prime lanes | 3x (66.7%) | 91% | 88% | 81% |

Migration 0114 widens `backyard_lane_levels`' CHECK to the ladder; it applies
before the binary that writes the new levels. Rolling the binary back past
the ladder needs the stored levels back on the old one first, or the old
binary holds the whole vault on `invalid_lane_levels`, the brake included:
`UPDATE loyal_yield.backyard_lane_levels SET level = 1.75 WHERE level > 1.75`. Lowering a cap delevers a lane
held above it by rule 6 (Maple at 1.75x steps down to 1.5x).

The selector scores every move over the same 30-day horizon, 30-minute
persistence, $0.25 minimum benefit and 10 bps uncertainty (on the vault's
investable equity for cash, on the tranche for a rotation): cash entering a
lane earns its fee-reserved gain against nothing;
a held lane's tranche rotating earns the destination's fee-reserved gain on
what the move leaves, against the tranche's gross gain where it is. Both
sides are priced where the live leverage rule settles them (`settledLevel`):
the ladder climbed one rung at a time up to the lane's cap, each rung taken
when its step has room today, the spread rule holds at the cumulative borrow
reaching it, and the step's horizon gain covers its own priced cost
(`upStepGain`, the rule's own test). The destination climbs from 1x in its
quote, whose first step is the entry's own transaction to 1.5x; a source
climbs from where it is (`settleHeld`), and staying earns its share of that
position less its share of the steps' cost. The market-level screens
(forward APY, gain ceiling) price no step, so they walk the spread rule
alone. A move's cost is the destination's signed round trip (a collateral
bought under its mark gains what its sale loses), its up steps with their
unwinds, the network fees, an absent
obligation's rent and, on a rotation, the network fees of the source's
release; the source's own swap loss is its exit, which staying pays later.
Cash outranks a rotation. Pairs sharing a collateral custody (the overflow
pair, Prime's) never rotate. Each sample prices a held source's tranche
into one destination only: the one whose forward APY beats the source's by
the most (`forward_apy_not_best` labels the others). A selector destination
nothing holds is refreshed beside the valued lanes; when Kamino refuses it,
only that lane goes unviewed and the refusal never counts toward the
vault's refresh latch.

## Steps

`Decide` returns the next atomic transaction (`docs/backyard-atomic-step.md`,
Voltr allocate/stage/restore, a conversion, a report) or a hold:

A hold is not a journal row: it is `loyal_backyard_hold{lane,reason}` on
/metrics and one log line when it changes (`docs/backyard-one-writer.md`).

| Hold | Meaning |
|---|---|
| `at_target` | the vault is where the target says |
| `no_room` | the target wants a move the chain has no room for: a full debt reserve, no withdraw room, a release a lane cannot make; counted on `loyal_backyard_step_no_room_total{lane,rule}` |
| `stale` | an input is not current: an expired entry quote, an unknown term end, an absent obligation awaiting its initializer |
| `chain_refused` | the transaction's signed simulation failed: the row is journaled failed with the simulation error and never sent; the next tick decides again |

## Operator exit

```sh
loyal-engine backyard exit-lane --vault rwa --lane AUTO/AUTO/wYLDS --by "<operator>"                 # debt 0
loyal-engine backyard exit-lane --vault rwa --lane AUTO/AUTO/wYLDS --by "<operator>" --equity-zero   # debt 0, equity 0
loyal-engine backyard exit-lane --vault rwa --lane AUTO/AUTO/wYLDS --clear                          # remove the exit
```

The row is the whole authority: the worker delevers the lane by atomic down
steps on its next ticks and, with `--equity-zero`, withdraws the collateral
and swaps it to USDC in one more step (rule 3). Clearing the row stops it at whatever state the chain is in; the
level rule then applies again. The command reads `NEON_DATABASE_URL` from the
credential directory like the other operator commands.

## Deploying the target binary

- Migration 0103 is additive: it creates the two tables and copies each
  lane's stored level from the old route state (`leverageTargets`), approval
  off, idempotently. It applies while the old binary still runs, which
  ignores the tables.
- The new binary ignores the old route-state keys. Its first selector sample
  decides each lane's approval again.
- Migration 0105 (shipped with the one-writer binary,
  `docs/backyard-one-writer.md`) removes the old keys once that binary is
  live.
- Deploy with no Backyard operation in flight.

On the live book (`TestLiveStateRestartComputesTheCurrentState`, read at
slot 455902661 on 2026-10-12): AUTO/AUTO/PYUSD and AUTO/AUTO/wYLDS both at
1.75x (LTV 4284 bps), PYUSD borrowing closed. The target is the current state
right after the deploy and after the next sample approves both levels: no
step, no overflow move.

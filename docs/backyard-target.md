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
| The rates | the selector's latest result (`state.selector.result`): the lane a flat vault enters, on a quote current at the batch | the selector sample |
| A lane's level | `loyal_yield.backyard_lane_levels`: 1, 1.5 or 1.75, and whether the up step toward it is worth its cost at the last sample | the selector sample |
| An operator exit | `loyal_yield.backyard_lane_exits`: lane X target debt 0, optionally equity 0 | `loyal-engine backyard exit-lane` |

Nothing else about a move is stored: no unwind intent, tranche,
partial-withdrawal record, working lane, selector entry or debt-clear
authority.

## Rules, in priority

1. **Hard LTV.** A held lane at or past `min(6000, liquidationThresholdBPS -
   1500)` bps steps down to its level's LTV.
2. **Term maturity** (wYLDS, 30-day term). Under 1 day left without room for
   the keep-alive borrow, or past maturity: debt 0. Under 3 days with room:
   the keep-alive borrow (the minimum up step); an unfunded withdrawal goes
   first until the term's last day. A term whose end is unknown holds
   `stale`.
3. **Operator exit.** Debt 0; with `--equity-zero` the collateral then leaves
   as USDC: one atomic step withdraws it and swaps it to USDC in the Squads
   bridge account, so none of it waits in a custody another lane shares.
   That USDC is vault cash: a withdrawal stages it to Voltr, otherwise rule 5
   tops up the home lane with it. A deposit left at or under dust is
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
   the live cap's LTV into its overflow lane, when that lane's level is
   approved, its reserve has room and it has levered the previous move. New
   Voltr cash tops up the home lane (the held non-overflow lane with the most
   equity). A flat vault enters the selector's lane; Squads cash an entry
   allocated waits for the selector while no entry is current, and returns to
   Voltr when the lane cannot take it.
6. **Level.** Down to a stored 1.5 from above it; up toward the stored level
   when the selector approved the step and the reserve has room.

Only rules 1 to 3 ever set debt 0. A stored level of 1 is economic: the lane
holds at 1.5. Held lanes keep their capital; the selector moves no lane's
capital to another lane.

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
loyal-engine backyard exit-lane --lane AUTO/AUTO/wYLDS --by "<operator>"                 # debt 0
loyal-engine backyard exit-lane --lane AUTO/AUTO/wYLDS --by "<operator>" --equity-zero   # debt 0, equity 0
loyal-engine backyard exit-lane --lane AUTO/AUTO/wYLDS --clear                          # remove the exit
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

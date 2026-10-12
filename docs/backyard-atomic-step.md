# Backyard atomic leverage step

Every Backyard leverage move is one transaction: the KLend leg that opens it,
the conversion, and the KLend leg that closes it either all land or none do.
The worker never holds borrowed cash or released collateral between
transactions, so there is no intermediate state to sequence, price, guard or
recover.

## Step shapes

Each step is a single v1 transaction whose instructions are Squads policy
executes (the vault signs nothing at top level):

| Direction | Instructions |
|---|---|
| up | refresh reserves + obligation, `Squads{borrow}`, `Squads{swap \| Hastra stake}`, refresh, `Squads{deposit}` |
| down | refresh reserves + obligation, `Squads{withdraw}`, `Squads{swap \| Hastra redeem}`, refresh, `Squads{repay}` |

- The open leg's amount is decided before the quote: the capacity-sized borrow
  (up), or the collateral receipts the down step withdraws.
- The conversion is quoted for exactly the open leg's output plus any custody
  already held. Its on-chain minimum output (Jupiter's slippage floor, or
  Hastra's quoted price) is the guaranteed output.
- The close leg moves the held custody plus that guaranteed output. If the
  conversion delivers less, the close leg lacks custody and the whole step
  fails atomically; if it delivers more, the excess stays as idle custody and
  is swept by the next step's close leg.
- KLend repays `min(amount, owed)`, so a down step that repays all debt asks
  for the payoff's upper bound and never leaves a dust remainder by rounding.
- KLend checks obligation health per top-level instruction, so the refresh
  sits before each KLend leg, and the withdrawal of a down step is bounded by
  the obligation's allowed borrow value (less a 300 bps margin) at the moment
  it runs, before the repay.

The worker's own minimum-output check stays: the effects persisted with the
operation record the collateral custody's minimum after-balance and the debt
custody's exact (up) or minimum (down) after-balance, and reconcile checks the
receipt against them.

## Which decisions are steps

| Decision reason | Direction | Repay / borrow |
|---|---|---|
| `leverage_up` | up | capacity-sized borrow to the stored level |
| `term_keepalive` | up | the keep-alive borrow |
| `hard_ltv_delever` | down | debt down to the stored level's LTV |
| `leverage_down` | down | the whole debt (payoff upper bound), repeated while a withdrawal limit leaves debt |
| `leverage_down_partial` | down | debt down to the 1.5x LTV |
| `withdrawal_delever` | down | the whole debt, for an admitted unwind or drain |
| `withdrawal_partial_release` beside debt | down | the withdrawal's debt share (+1%) |

Down decisions carry a fixed marker amount; prepare sizes the repay from the
same snapshot, so a refreshed snapshot re-decides to an equal decision.

Hard LTV is `min(6000, liquidationThresholdBPS - 1500)` bps as before; the
step no longer needs an idle repayment buffer, so the old
`hard_ltv_without_repayment_buffer` manual stop is gone.

## Why one transaction

The old chain (borrow, then swap, then deposit; release, then swap, then
repay) left custody idle between legs. Each idle state needed its own
decision, its own residue rule, its own cost forecast and its own guard
against a concurrent withdrawal, and a failed middle leg stranded cash or
collateral that the next tick had to recognise. One transaction removes every
one of those states: the position moves from one healthy obligation to the
next or does not move.

## Why no flash loan

A KLend flash loan would let a down step repay before it withdraws, but KLend
refuses flash borrow and flash repay when called by CPI (`flash_ixs.rs`,
lines 27-30). Backyard's vault is a Squads smart-account PDA; every
instruction it signs runs as a CPI from `ExecuteTransactionSyncV2`, so a flash
loan is unavailable by construction. Withdraw-then-repay inside one transaction
is the closest atomic shape, which is why the withdrawal is bounded by the
obligation's allowed borrow value at that instruction.

## v1 transactions, no lookup tables

Steps are compiled as v1 transactions (SIMD-0385): up to 4,096 bytes and 64
account keys, every key static. There are no address lookup tables: no table
to create, extend, freeze or keep in sync with the policies, and no table
account in the persisted wire. Every Backyard transaction, step or not, uses
the one v1 compiler (`message_v1.go`).

- The compute budget lives in the v1 message config, not in ComputeBudget
  instructions: the CU limit is always set (200,000 per instruction, at most
  1,400,000), the priority fee is a lamport total, AUTO-collateral lanes
  request a 64 KiB heap, and loaded account data is capped at 64 MiB.
- RPC reads ask for `maxSupportedTransactionVersion` of at least 1.
- The binding limit is the 64 keys, not the bytes: a step's Jupiter route is
  quoted with `maxAccounts` = 64 minus the keys the step's KLend legs, refreshes
  and Squads executes already use, so the route always fits.
- Wires persisted by an older binary (legacy or v0, count-prefixed signature)
  still split, so a row in flight across the upgrade lands and settles.

## Keys and bytes per lane

From `TestEveryEarnStepFitsOneV1Transaction`. "fixture" is the recorded
Jupiter route; "max N" is a route that uses the whole `maxAccounts` budget N
the step leaves. A route one account larger does not compile.

| Lane | Dir | Route | Keys | Bytes |
|---|---|---|---:|---:|
| Maple/syrupUSDC/USDC | up | fixture | 39 | 1753 |
| Maple/syrupUSDC/USDC | up | max 32 | 64 | 2595 |
| Maple/syrupUSDC/USDC | down | fixture | 38 | 1718 |
| Maple/syrupUSDC/USDC | down | max 33 | 64 | 2594 |
| OnRe/ONyc/USDC | up | fixture | 41 | 1821 |
| OnRe/ONyc/USDC | up | max 32 | 64 | 2596 |
| OnRe/ONyc/USDC | down | fixture | 45 | 1966 |
| OnRe/ONyc/USDC | down | max 33 | 64 | 2598 |
| AUTO/AUTO/PYUSD | up | fixture | 53 | 2258 |
| AUTO/AUTO/PYUSD | up | max 35 | 64 | 2615 |
| AUTO/AUTO/PYUSD | down | fixture | 42 | 1874 |
| AUTO/AUTO/PYUSD | down | max 36 | 64 | 2609 |
| Prime/PRIME/USDC | up | fixture | 37 | 1687 |
| Prime/PRIME/USDC | up | max 34 | 64 | 2597 |
| Prime/PRIME/USDC | down | fixture | 36 | 1652 |
| Prime/PRIME/USDC | down | max 35 | 64 | 2596 |
| Prime/PRIME/PYUSD | up | fixture | 39 | 1755 |
| Prime/PRIME/PYUSD | up | max 34 | 64 | 2598 |
| Prime/PRIME/PYUSD | down | fixture | 47 | 2038 |
| Prime/PRIME/PYUSD | down | max 35 | 64 | 2603 |
| Prime/PRIME/USDS | up | fixture | 48 | 2073 |
| Prime/PRIME/USDS | up | max 34 | 64 | 2604 |
| Prime/PRIME/USDS | down | fixture | 46 | 2000 |
| Prime/PRIME/USDS | down | max 35 | 64 | 2602 |
| Ethena/USDe/PYUSD (exit-only) | up | fixture | 39 | 1755 |
| Ethena/USDe/PYUSD (exit-only) | up | max 34 | 64 | 2598 |
| Ethena/USDe/PYUSD (exit-only) | down | fixture | 38 | 1720 |
| Ethena/USDe/PYUSD (exit-only) | down | max 35 | 64 | 2597 |
| AUTO/AUTO/wYLDS | up | Hastra | 31 | 1454 |
| AUTO/AUTO/wYLDS | down | Hastra | 29 | 1386 |

Every lane and direction fits; the largest step is 2,615 of 4,096 bytes.

Split-policy lanes cap each KLend leg at `1e12` raw (the installed policy
literal). For 6-decimal stables that is $1M per step; for Ethena's 9-decimal
USDe it is about 1,000 USDe per step, so its exit runs in more steps.

## Live simulations

Read-only `simulateTransaction` against the public mainnet RPC on
2026-10-12 UTC (unsigned: `sigVerify` off, recent blockhash replaced), opening with 10,000,000 raw (10 tokens):

| Lane | Dir | Slot | Bytes | Keys | CU | LTV before -> after | Result |
|---|---|---:|---:|---:|---:|---|---|
| AUTO/AUTO/wYLDS | up | 455874134 | 1454 | 31 | 481,529 | 4283 -> 4284 bps | OK |
| AUTO/AUTO/wYLDS | down | 455874151 | 1386 | 29 | 467,781 | 4283 -> 4282 bps | OK |
| AUTO/AUTO/PYUSD | down | 455874166 | 2012 | 46 | 591,210 | 4284 -> 4284 bps | OK |
| AUTO/AUTO/PYUSD | up | 455874202 | 1913 | 43 | n/a | n/a | KLend 6098 `BorrowingAboveUtilizationRateDisabled` (PYUSD reserve at 90.08% of its 90% limit) |

The AUTO/AUTO/PYUSD up refusal is the reserve's live borrow limit, the reason
the wYLDS overflow lane exists; the step itself compiled, signed-shape
verified and reached the borrow instruction.

Reproduce one with the operator command (read-only, no key material):

```sh
loyal-engine backyard simulate-step --lane AUTO/AUTO/wYLDS --dir up --amount-raw 10000000
```

It reads `BACKYARD_SOLANA_RPC_URL` from `CREDENTIALS_DIRECTORY`; point it at a
public RPC and space calls, as public RPC rate-limits.

## Rollout gate

The change ships only with the owner's deploy go-ahead. It does not change
installed Squads policies or raise any borrowing limit.

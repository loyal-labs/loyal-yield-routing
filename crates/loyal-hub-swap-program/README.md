# Loyal Hub Swap Program

`loyal-hub-swap-program` is the on-chain inventory leg for Loyal yield routing.
It lets a Loyal-controlled hub fill stablecoin swaps from hot inventory while a
Squads smart account remains protected by a narrow Loyal Action.

The program is intentionally small. It does not quote prices, choose routes, or
own strategy logic. The caller supplies an exact-in swap amount, an exact output
amount, a user minimum, and a fee cap. The program validates the accounts and
then performs two SPL Token `transfer_checked` calls:

1. Move the input token from the user vault into the hub inventory account.
2. Move the output token from the hub inventory account back to the user vault.

If any validation fails, neither transfer is committed.

## Accounts And Authorities

The program has two PDAs:

- `config`: stores the admin, hub authorizer, maximum fee in basis points, pause
  flag, and fixed-size allowed mint list.
- `hub-authority`: owns the hub inventory token accounts and signs hub inventory
  transfers through `invoke_signed`.

Hub inventory accounts are canonical associated token accounts for
`hub-authority`. The program rejects any other hub source or destination account,
even if that account is an otherwise valid SPL Token account owned by the hub
authority.

The admin can initialize config, pause swaps, set the maximum fee, and withdraw
hub inventory. The allowed mint list is immutable after initialization. The hub
authorizer must sign each swap. This keeps treasury inventory approval separate
from the delegated smart-account executor.

## Swap Validation

`swap_exact_in` accepts only a configured mint pair and a bounded fee. The
input and output amounts must be non-zero, and `amount_out` must be at least
`min_out`. The requested fee cap must stay within the config maximum.

Both mints must be in the config allowlist, and they must be different from one
another. The user and hub token accounts must be distinct mutable accounts. Each
token account is unpacked and checked against the expected mint and owner before
any CPI runs.

The token program must be SPL Token. The hub authority must be the
program-derived `hub-authority` PDA. The hub input and output accounts must be
the canonical inventory accounts for their mints. The user vault and hub
authorizer must both sign the transaction.

The fee check normalizes both token amounts to 18 decimals before comparing the
output against the input less `max_fee_bps`. This keeps the check stable across
USDC/PYUSD-style decimal differences without adding an oracle dependency.

## Loyal Actions

Loyal Actions are the product-level permissions that let a delegated executor
use a smart account for a specific yield-routing step. In the current test
crate, each Loyal Action is implemented as a Squads `ProgramInteraction` policy.

For Loyal Hub swaps, the action constrains the delegated executor to the
`swap_exact_in` instruction on this program. The route action pins the Loyal Hub
config PDA, the smart-account vault, the allowed route mints, the hub
inventory accounts, the hub authorizer, the SPL Token program, and the maximum
fee argument in instruction data.

That means a delegated executor can choose a permitted route amount at execution
time, but cannot redirect the call to another program, use unapproved mints,
skip the hub authorizer, or exceed the configured fee ceiling.

The same route can include a Jupiter lane. A rebalance may fill part of the swap
through Loyal Hub inventory first, then send the residual through Jupiter if the
Loyal Action allows both lanes.

## Module Layout

The program is split by responsibility:

| File | Responsibility |
| --- | --- |
| `src/lib.rs` | Entrypoint and public exports |
| `src/constants.rs` | Instruction tags, PDA seeds, config size |
| `src/codec.rs` | Small byte readers for instruction and state decoding |
| `src/instruction.rs` | Instruction enum and instruction-data parsing |
| `src/state.rs` | Config parsing, account read/write, PDA derivation |
| `src/processor.rs` | Instruction handlers and call ordering |
| `src/token.rs` | SPL Token account validation and checked transfers |
| `src/validation.rs` | Shared guard functions used before state changes or CPIs |

Keep new checks close to the layer that owns the risk. Account layout checks
belong in `state.rs`, SPL Token checks belong in `token.rs`, and instruction
flow checks belong in `processor.rs`.

## Testing

Run the lean Squads/Loyal Action coverage with:

```bash
bun run test:squads
```

That script builds the local SBF programs and runs the LiteSVM tests. The
Loyal Hub coverage lives in
`crates/squads-test-harness/tests/loyal_hub_swap.rs` and covers successful hub
fills, missing authorizer signatures, wrong token accounts, same-mint rejection,
non-canonical inventory rejection, duplicate mutable account rejection, fee caps,
pauses, max-fee updates, inventory withdrawals, and Jupiter residual fallback.

Historical replay tests are intentionally ignored by default. Use the dedicated
hub hindsight script only when route economics or replay-sensitive behavior
changes:

```bash
bun run test:squads:hub-hindsight
```

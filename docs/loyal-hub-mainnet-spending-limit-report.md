# ASK-1556 Mainnet E2E Spending Limit Proof

Date: 2026-06-29

PR: https://github.com/loyal-labs/loyal-yield-routing/pull/6  
Branch: `ASK-1556-create-spending-limit-for-loyal-hub`  
State file: `.agents/ask-1556-mainnet-e2e-state.json`

## Summary

PASS for the ASK-1556 spending-limit proof. The mainnet user Loyal Hub route executed through the Hub swap with an embedded hourly spending limit. The USDC limit was consumed by exactly the Hub input amount:

- Hub input mint: `EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v`
- Route input amount: `999999` raw
- Hourly cap: `1099999` raw, equal to ceil(`999999 * 1.10`)
- Before Hub swap remaining: `1099999`
- After Hub swap remaining: `100000`
- Consumed: `999999`
- Hub swap signature: `SayEEh2dKDGioqyZyyMAoXVeCs8CQPeDfXkF2myQPsjiL59Bmn7RvMPYA1mWa2SbaXJtwdcZXU8an7TXomHovwj`

The original planned amount was `1000000` raw with cap `1100000`, but the post-Kamino-withdraw vault balance was `999999` raw USDC. The live run adjusted the Hub swap amount to `999999` and cap to `1099999`.

## Preflight

System key:

- Public key: `GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N`
- Keypair path: `/Users/zotho/.config/solana/id.json`
- RPC used: public mainnet, because `.env.1password` was absent and `op run --environment loyal-noncritical-env` hung without output

Balances before live execution:

- SOL: `81354058` lamports
- System USDC ATA `41z4FLBrLhVx4TPGFZJk44YfvUccDvBf85emoJ4ACDcB`: `4306772` raw
- System PYUSD ATA `Hbm6NC9vmjfnajCXNSZb6W5AUR5ko9SrjgjN7BfZ6AFf`: `3447077` raw
- Jupiter quote for `1000000` raw USDC to PYUSD: HTTP `200`, out amount `999792`

Hub state before run:

- Program: `LHUB3MMwYEwXqbfMdr1AQ8vkrJoubH37qoBxiy38smH`
- Admin: `GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N`
- Hub authorizer: `GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N`
- Inventory rebalancer: `GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N`
- `max_fee_bps`: `50`
- `paused`: `false`
- `lane_count`: `4`

## Commands

Plain simulation:

```sh
bun run hub:mainnet-test -- --state-file .agents/ask-1556-mainnet-e2e-state.json --policy-spending-limit-hourly-raw 1100000 --policy-amount-in-raw 1000000 --policy-amount-out-raw 995000 --allow-authority-handoff
```

Result: PASS. Stopped after `seed-hub-inventory` simulation as expected in default simulate-only mode.

Simulate all:

```sh
bun run hub:mainnet-test -- --state-file .agents/ask-1556-mainnet-e2e-state.json --policy-spending-limit-hourly-raw 1100000 --policy-amount-in-raw 1000000 --policy-amount-out-raw 995000 --allow-authority-handoff --simulate-all
```

Result: known best-effort boundary. It simulated `seed-hub-inventory`, `create-treasury-vault`, and `create-user-vault`, then failed at later Squads setup because Solana simulations do not persist prior simulated account creation. Live execution still ran the script's signed simulation before every submitted transaction.

Live execution was resumed across several guarded commands as blockers were discovered:

```sh
CONFIRM_MAINNET=1 bun run hub:mainnet-test -- --state-file .agents/ask-1556-mainnet-e2e-state.json --policy-spending-limit-hourly-raw 1100000 --policy-amount-in-raw 1000000 --policy-amount-out-raw 995000 --allow-authority-handoff --execute
CONFIRM_MAINNET=1 bun run hub:mainnet-test -- --state-file .agents/ask-1556-mainnet-e2e-state.json --policy-spending-limit-hourly-raw 1100000 --policy-amount-in-raw 1000000 --policy-amount-out-raw 995000 --allow-authority-handoff --skip-rpc-send-preflight --execute
CONFIRM_MAINNET=1 bun run hub:mainnet-test -- --state-file .agents/ask-1556-mainnet-e2e-state.json --policy-spending-limit-hourly-raw 1099999 --policy-amount-in-raw 999999 --policy-amount-out-raw 995000 --update-policy --allow-authority-handoff --skip-rpc-send-preflight --execute
solana transfer CqASKDbsrGs1fBFUPhKL9tfzQnTad1CKqn7m51DYX2y9 0.005 --allow-unfunded-recipient --url mainnet-beta --keypair /Users/zotho/.config/solana/id.json
CONFIRM_MAINNET=1 bun tmp/ask-1556-run-deposit-prefix.ts
CONFIRM_MAINNET=1 bun run hub:mainnet-test -- --state-file .agents/ask-1556-mainnet-e2e-state.json --inventory-per-lane-raw 0 --user-vault-usdc-fund-raw 0 --user-vault-setup-lamports 0 --policy-spending-limit-hourly-raw 1099999 --policy-amount-in-raw 999999 --policy-amount-out-raw 995000 --update-policy --allow-authority-handoff --skip-rpc-send-preflight --execute
CONFIRM_MAINNET=1 bun run hub:mainnet-test -- --state-file .agents/ask-1556-mainnet-e2e-state.json --inventory-per-lane-raw 0 --cleanup-only --allow-authority-handoff --skip-rpc-send-preflight --execute
```

`--skip-rpc-send-preflight` was used only after the script's own signed `simulateTransaction` passed and RPC send preflight returned stale `MissingAccount` errors.

## Key Transactions

- Seed Hub inventory: `2g4L8uK7FTqsT9jNfQHMoWv3s8YDyXNMHDcbVkA3y6vcBTPeVDmGeimoSFxnCBreb8aJKV399ouFtDEAA554oe2v`
- Create treasury vault: `4sn6cyfLqm1xb1xJQdd3WpTUAY3H2fstiiVDA7XzfkTBtMgdVdWLTdBTNusRKsCvHGtGhihjE1j6sByRiZAne2Pv`
- Create user vault: `2h7KD5KZVZC7qK2cCQfGCuHHYGsvNVWFLaKELa5gZNyVjjp1SmLkD2wqx1h2rcupn6MR1rE2gKvUVEDbSTv1MvwN`
- Create route policy: `61CD1kxGTxgh7m6erMX1hyJk1rVm78zxCtJ7VULd9F3MH42FDLAPZRwGdkazhFs7pjvDJr8WXmDSFdEiXrE18az7`
- Update route policy to `999999`/`1099999`: `4iBK6gHNgtdBX3MCJhyJMi5xG9w4pSzSsVqYNfFqvoULUkGRYuZrDohdyPdxSHmgojaxxp42XSzHnDw2SLUbpKDh`, `25Q4TzjBseLYbv4MmXv4gYviF8gxFpjv4NLnXQGJhaYA6M3d52TvpE6J738Ng5uqsTuPfrevkPTGjZRt76W1qWqm`
- Policy route withdraw: `GdcaMzcuMwi8g6CUVmhU4SUVKCByMWwsBP9HskQ1qXbETw66EpTGHJt8Bq7ePciert8urYTHDot82L7BJBQGqTc`
- Policy route Hub swap: `SayEEh2dKDGioqyZyyMAoXVeCs8CQPeDfXkF2myQPsjiL59Bmn7RvMPYA1mWa2SbaXJtwdcZXU8an7TXomHovwj`
- User vault SOL top-up: `5uuyQPqMjAJyQNXJ8EgaZhj7uYfvXbE9gc66mscmL6gfN8F3HJSXsu1jFnxkkTvUuKBeAPQUfNyNcZ6WWCLL9nzt`
- Deposit prefix setup: `2w86QqKEHnPLyjTHfXKv7KSLhHmM4C45dKbxZCkct92tiqWvrGr1ieQWjeEcGoW6tq5EkM55pUX2e64vJxfQMPQd`
- Update route policy to include both stable-mint limits: `2cnjeRXUbVKJY4XELoixGLu6f3rarmL6d4YjzVG2HDLfiMqF3xWkYWyRSZnvgL543voMSCJW5zgybeKAHwiRdWjm`, `3EuEvdseE7w6mXxgNUC5G7enC8zrYiZ57XbFLx3Dg6ZczAaaft1FSjWwE3SZyTwqmNUwu7J4SY8ne58utpwBNGqe`
- Policy route deposit: `Fe38btyYFjUzzcaFT2T5kRfvumZd8ErSi2BENJmH9tBSvJvmNt3uGsHfajte4o1296o2xprA1UKorJGWwkhXw2j`
- Cleanup Hub inventory: `n2PTipa3ftLrAnvpkpLo53gaoEoHWwT1i4AS4r2RSasS1dc9BYhadk6YEC7j4MwXeiE1zio3vUm31qazZZuWmM4`

Finalized status checks:

- Hub swap: finalized, `err=null`
- Deposit: finalized, `err=null`
- Cleanup: finalized, `err=null`

## Spending-Limit Evidence

State path: `.agents/ask-1556-mainnet-e2e-state.json`

```json
{
  "status": "passed",
  "policy": "59UaZaGsUacVDiQbb3ah5txYhEDBuMheLyGbUgeTMjjJ",
  "mint": "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
  "amountInRaw": "999999",
  "capRaw": "1099999",
  "before": {
    "remainingInPeriodRaw": "1099999",
    "lastReset": "1782757030"
  },
  "after": {
    "remainingInPeriodRaw": "100000",
    "lastReset": "1782757030"
  },
  "consumedRaw": "999999"
}
```

The script assertion passed only after decoding the on-chain ProgramInteraction policy before and after `policy-route-hub-swap` and checking `before.remaining - after.remaining === 999999`.

## Code Fixes From The Run

- Added on-chain ProgramInteraction policy decoding to `scripts/mainnet-loyal-hub-tests.ts`.
- Added before/after spending-limit snapshots and exact consumption assertion around `policy-route-hub-swap`.
- Added `--skip-rpc-send-preflight` so the script can rely on its own signed simulation when RPC send preflight serves stale state.
- Updated the route policy to embed spending limits for both route stable mints. The live run proved the USDC limit; the PYUSD limit is needed because the deposit leg spends PYUSD from the vault and Squads rejects unlisted token outflows once embedded limits are present.

## Cleanup

Cleanup-only completed successfully.

Final Hub state:

- Admin: `GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N`
- Hub authorizer: `GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N`
- Inventory rebalancer: `GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N`
- Lane 0 USDC/PYUSD: `0`/`0`
- Lane 1 USDC/PYUSD: `0`/`0`

## Residual Notes

The unrelated treasury rebalance policy path failed at `create-treasury-withdraw-policy` with Squads `InstructionDidNotDeserialize` (`Custom: 102`). The ASK-1556 user route and spending-limit proof had already passed by then. The run skipped the remaining treasury rebalance and lane rebalance flow, then executed cleanup-only.


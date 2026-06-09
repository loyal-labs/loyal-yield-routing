# Mainnet Yield Routing Test Report

Date: 2026-06-09

## Target

Validate the Loyal yield routing orchestration on Solana mainnet with real funds:

- Fund and prepare a Squads vault.
- Seed a Kamino position.
- Trigger the worker loop to reroute funds.
- Verify same-mint routing, cross-mint/Jupiter readiness, DB state, and blockchain state.
- Check that actual Jupiter swaps work on mainnet.
- Recheck the live accounts against the latest Squads Policy Framework and the
  current non-legacy ProgramInteraction policy code path.

Accounts used:

- System/setup key: `GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N`
- Additional provided key: `6T7U8nSZmkRYsrsA5ivhk4YHkqE1JkCdCNHYVtCrMThr`
- Worker delegated signer from 1Password env: `BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ`

Latest account/policy status checked on 2026-06-09:

- Settings: `4aWMf1dFxviHisBFfi9apgqNDUBH4rLWHQYHUANbLAdi`
- Vault: `EVaVYyDRuD3mnSjwJktAnmfe6v4QuDgEEu66ZCDvFopr`
- Managed vault id: `92`
- Active DB route policy id: `37`
- Active route policy seed/account: seed `2`, `BJgmjzDJUJdDE5XNU5RJphxrdWM7Nym3c8XSDrnpS1y4`
- Active route modes: `{same_mint,cross_mint_jupiter}`
- Active stable/liquidity mints: USDC and USDT
- Active swap lane still contains split swap-policy metadata:
  `policy_account = B53LFhxNE5rAsQmbyEggDzYSDG7UPgYqVU4KSbjcPKJg`,
  `constraint_index = 0`

Current status: these live accounts are still configured with old split
cross-mint metadata. Current orchestrator code rejects split
`swap_policy_account` metadata and requires one unified compact
ProgramInteraction policy, so the live accounts are not currently runnable by
the current non-legacy orchestrator path.

## Setup Performed

Used a persistent interactive shell with `op signin`, then ran subsequent 1Password-backed commands inside that shell.

Initial funding observed:

- `GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N`: `0.08 SOL`, `25 USDC`
- `BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ`: `0.02 SOL`

Created a Squads smart account and vault on mainnet:

- Settings: `4aWMf1dFxviHisBFfi9apgqNDUBH4rLWHQYHUANbLAdi`
- Vault: `EVaVYyDRuD3mnSjwJktAnmfe6v4QuDgEEu66ZCDvFopr`
- Smart account create signature: `2obgKnEiFwPvmUmbgw7oLW5XyuuinPnAxHkZgCbiMnVZYaAqF8tivm95r57BLpD5M9ZYA4Z65fANvjdE8hdxLFtS`

The normal policy init create path was brittle on mainnet because the Squads program derives the settings PDA from the global program config index. A fast creator helper was added and kept:

- `crates/loyal-yield-policy-init/src/bin/create_squads_fast.rs`

Additional helpers added and kept:

- `crates/loyal-yield-orchestrator/src/bin/resolve_kamino_targets.rs`
- `crates/loyal-yield-policy-init/src/bin/sign_send_versioned_tx.rs`

## Same-Mint Test

Installed a same-mint policy on the new Squads settings:

- Policy account: `2UWE4yu43VQpRcdT5QPmikrDWQMw4JFiB7kpkTNkaKK4`
- DB managed vault id: `92`
- DB route policy id: `36`
- Policy install signature: `3XBngyzMunJoWj6gUjHrwF4siniMqyj8GQWFBAJshzFGWkyrg7PbZNyiYmWixXbe5kLszwdQdVEUQVfHwhnmBhXt`
- Confirmed slot: `425155432`

Created vault token accounts:

- USDC ATA: `7T51A827fqEp1ZV5hC5dHBMJHGpKthCvswb4xQKm3MbH`
- Source collateral ATA: `7n2M9yU4ap2GyYgh2xnRny6xCKKn6UqdfgmvJ98DJhVV`
- Target collateral ATA: `6j19pCtMugTnJU84neP79WhAH1h6p3e9YvUz8eeCnuUd`

Funded the vault with `5 USDC`:

- Transfer signature: `398kS7zrL7GsHttpHcNskHp1kDYpGHYQoBkHLTF3aAZZo83RmremnRFhkq9iDqJQVKpoCPQmH5w7dnc6RT68NxqN`

Seeded the source Kamino reserve through Squads sync execution:

- Source reserve: `D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59`
- Market: `7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF`
- Amount: `5,000,000` raw USDC
- Deposit signature: `5T75MKBFDxGhf8iVaNym24d3dQdKESM4ipx3zdaQeaFnoYDT9prRdKby4umhfiPzbZRFyn9RyDn8cBoy8Wsh8W2i`

Ran the worker once with mainnet cluster and target overrides.

Result:

- Active vaults: `1`
- Planned decisions: `1`
- Claimed decisions: `1`
- Built transactions: `1`
- Simulation ok: `true`
- Submitted: `true`
- Worker route signature: `2ZxhYB1GzSanpSQctKVQXAFMNpXiM4DnfgtQNneG6niEf2ECFmaUXBZsK2DKmXxdUeTDopBiCmUp8Pdt4CdUCbzu`
- Confirmed slot: `425156548`

Route executed:

- Source: Main Market USDC reserve `D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59`
- Target: Figure Market USDC reserve `9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu`
- Decision amount: `4,215,044` raw
- Source APY: `198` bps
- Target APY: `511` bps
- Edge: `313` bps

On-chain vault balances after routing:

- Source collateral mint `B8V6WVjPxW1UGwVDfxH2d2r8SyT4cqn7dQRK6XneVa7D`: `0`
- Target collateral mint `DKaVQFXD6Qz4USTkRWyPun3oU6r1RfYsWJ8YqLpnSnN5`: `4.102798`
- USDC mint `EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v`: `0.784955`

DB verification after a dry reconciliation pass:

- Source reserve `D6q6...`: `0`, `has_value = false`
- Target reserve `9GJ9...`: `4,102,798`, `has_value = true`
- Current positions observed slot: `425156979`

Same-mint conclusion: routing worked on-chain and DB current-position state reconciled correctly.

## Direct Jupiter Swap Check

Tested actual Jupiter USDC to USDT swaps outside the orchestration path using the system key.

Quote:

- Input: `1,000,000` raw USDC
- Output quote: `1,000,326` raw USDT on the final successful attempt
- Route plan length: `2`
- Quote endpoint used: `https://lite-api.jup.ag/swap/v1/quote`

First direct swap attempt reached Jupiter and downstream pool programs, but failed preflight with `ComputationalBudgetExceeded`. Logs showed Jupiter `SharedAccountsRoute` and pool swap execution before exhausting the supplied compute budget.

Mitigation:

- Pre-created the system key USDT ATA:
  - ATA: `6GQrYccwFRSBDAMrcPcut7DkuEggRx3KS7FGeqQJBzVb`
  - Signature: `2HrNJxXLZQkqp8fPpFSVGnnNSpQc9tPio5vfMBWwtUjtztNPQGFX1uwTs7y2rPpsX5f96oaBfQWhyG69ekAhGptb`

Second direct swap succeeded:

- Jupiter swap signature: `3H1DVUTnf5Mo8bTaugnpTZGjmpFUDX9PmHtjFDDhs7igbhmQqiYPnEWkbTXm6NPemNLL8nmxLurzjtgKYm7mFr5J`

System key final token balances:

- USDC: `19`
- USDT: `0.999663`

Jupiter conclusion: actual Jupiter swaps work on mainnet, but setup instruction overhead can matter for compute budget.

## Cross-Mint Policy Recheck

The 2026-06-09 recheck used the latest Squads policies branch as the source of
truth:

- Policy PDA family: `["smart_account", "policy", settings_key, policy_seed]`
- Policy type: compact `PolicyCreationPayload::ProgramInteraction`
- Updates: `SettingsAction::PolicyUpdate`
- Legacy spending-limit actions are not part of the current route policy path.

The live DB state for the accounts above is still the previous split topology:

- Main withdraw/deposit policy: `BJgmjzDJUJdDE5XNU5RJphxrdWM7Nym3c8XSDrnpS1y4`
- Swap-only Jupiter policy: `B53LFhxNE5rAsQmbyEggDzYSDG7UPgYqVU4KSbjcPKJg`
- Active DB route policy id: `37`
- Route modes: `{same_mint,cross_mint_jupiter}`
- Stable/liquidity mints: USDC and USDT
- Swap lane policy account: `B53LFhxNE5rAsQmbyEggDzYSDG7UPgYqVU4KSbjcPKJg`
- Swap lane constraint index: `0`

That split metadata is stale for the current code. Current orchestrator route
planning and transaction building both reject it instead of building
multi-policy cross-mint routes.

Policy update dry-runs:

- Seed `1` update against existing policy
  `2UWE4yu43VQpRcdT5QPmikrDWQMw4JFiB7kpkTNkaKK4`: operation `update`; legacy
  transaction fit the packet; simulation failed in
  `ExecuteSettingsTransactionSync` with `InstructionError(2, Custom(102))`
  / `InstructionDidNotDeserialize`.
- Seed `4` create for a fresh unified policy
  `Hn7hzhSCJAYiYoE25rHeyz5jfU1GUAQkUNCkcr8vsbg`: operation `create`; legacy
  transaction fit the packet at `974` bytes; simulation failed in
  `ExecuteSettingsTransactionSync` with `InstructionError(2, Custom(102))`
  / `InstructionDidNotDeserialize`.

No mainnet policy update was submitted. The current code now performs an
additional non-dry-run simulation preflight for existing settings and refuses to
send a policy create/update when the actual policy transaction simulation fails.

## Code Corrections

Corrections in the current 2026-06-09 code state:

- `loyal-actions` now emits only compact
  `PolicyCreationPayload::ProgramInteraction` route policies.
- Existing policy accounts are updated with `SettingsAction::PolicyUpdate`
  instead of delete/create churn.
- `yield-policy:init` chooses create versus update by checking whether each
  derived policy account already exists.
- The policy-init CLI no longer exposes a legacy ProgramInteraction encoding
  option; the default topology is the unified all-in-one compact policy.
- `yield-policy:init` simulates policy transactions for existing settings before
  non-dry-run submission and fails closed if Squads rejects the payload.
- The root `bun run yield-policy:init` script now names the
  `loyal-yield-policy-init` binary explicitly.
- The orchestrator planner and route builder reject old split
  `swap_policy_account` metadata with a typed unsupported-policy error.

Earlier corrections applied before the historical 2026-06-08 mainnet run:

- Added a live Jupiter quote/swap-instruction provider for the worker.
- Made the worker reject Jupiter setup/cleanup instructions so vault token accounts must be pre-created outside the Squads-protected route.
- Fixed `YIELD_ROUTER_KEYPAIR` parsing so JSON-array, hex, base58, and base64 key material formats work consistently.
- Recorded worker planning skips and submitted signatures in the worker report.

## Cross-Mint Orchestration Test

This section is historical evidence from the 2026-06-08 split-policy run. It is
not the current supported topology after the 2026-06-09 non-legacy policy
change.

Prepared required vault token accounts:

- Vault USDT ATA creation signature: `KHj8RWnphS11Qn7MeTo2EUXXtBpPAKWQ5WbJcF2dAZitTM5udZpftErKcFanGKsffbXexbojojcrMj8pedKGdn7`
- Vault WSOL ATA creation signature: `2FVJhwZMoArRECiBST91tmLR24vZkQQ66SYarpWrSpcRNu8298zcpyyUVz7vNFApA76Y8TAvyBwo1gRfeYiCt9iU`
- Target USDT collateral ATA creation signature: `2tTb81bPQQppjS7McqBaSdB3r9GCwzJEtfxG5FRimVWhuh1NbWo2nnKxdHKGgwk7GJyzdU22ctHN45qGfK5rSMFG`

Resolved USDT Kamino target:

- Reserve: `H3t6qZ1JkguCNTi9uzVKqQ7dvt2cum4XiXWom6Gn5e5S`
- Market: `7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF`
- Liquidity mint: `Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB`
- Collateral mint: `B8zf4kojJbwgCRKA7rLaLhRCZBGhgAJp8wPBVZZHMhSv`
- Token program: SPL Token

Ran `yield_route_worker` on mainnet with a forced USDC-to-USDT target override. The worker planned and claimed one cross-mint decision, then built a three-instruction split route. It submitted the first two steps:

- Decision id: `28`
- Amount: `4,102,798` raw USDC collateral-side liquidity
- Source reserve: `9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu`
- Target reserve: `H3t6qZ1JkguCNTi9uzVKqQ7dvt2cum4XiXWom6Gn5e5S`
- Withdraw signature: `4ekXVpezEiAT1hB5quQga5YnsZ1rTCqjSfSYVxy1weuG5cQSiZT8NyB7so1a7JQGQa1iuuaVFxFXusiKUmVvKQCq`
- Jupiter swap signature: `5sP3kG1Ec1dKCEenyTZLGfnUWyUML5RcPFuYdUYxm4WMPAHjNaFtkqpeqcNa4pduY8NXUS4AJpgpgb74PD8zXjaE`, slot `425179583`

The deposit step initially failed simulation with Squads `IllegalAccountOwner` because the target USDT collateral ATA did not yet exist. After creating the target collateral ATA, the stranded `4.099744` USDT liquidity was deposited manually through Squads sync execution:

- Manual recovery deposit signature: `5tBFjft8hakCRwZYcthzhjNBrJBm42hKM8imQi5SrmQmWr1P2dD5roGaLDajqbJJgnZQyduScDF78n7oF3u5qLxi`
- Recovery deposit slot: `425180827`
- Simulated compute for recovery deposit: `80,251` units

The worker decision row remains `failed` because its third sequential step failed before the recovery deposit. Funds were recovered on-chain and reconciled into the target reserve afterwards.

## Final Verification

The first part of this section is historical verification from the 2026-06-08
split-policy run. The latest 2026-06-09 verification is listed afterwards and
supersedes the split-policy routing status.

A dry `yield_route_worker --once` pass on mainnet reconciled the vault and skipped planning with `NoEdge`:

- Active vaults: `1`
- Reconciled vaults: `1`
- Planned decisions: `0`
- Claimed decisions: `0`
- Submitted: `false`
- Skip: `vault 92 skipped planning: NoEdge`

Current DB state for vault `92` after reconciliation at observed slot `425181200`:

- Source reserve `9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu`: `0`, `has_value = false`
- Target reserve `H3t6qZ1JkguCNTi9uzVKqQ7dvt2cum4XiXWom6Gn5e5S`: `3,502,811`, `has_value = true`

Current on-chain vault token balances:

- USDC liquidity ATA `7T51A827fqEp1ZV5hC5dHBMJHGpKthCvswb4xQKm3MbH`: `0.897276`
- USDT liquidity ATA `4mKVRYQ4ePx9uT4wRWsnwSHHVPrKApqhfmyProM58sMa`: `0`
- Source USDC collateral ATA `6j19pCtMugTnJU84neP79WhAH1h6p3e9YvUz8eeCnuUd`: `0`
- Target USDT collateral ATA `b9AmYaPZrqiPhbfY6UMBH2zz1xV3u3Uh4dQKMfbEn9E`: `3.502811`

Latest 2026-06-09 verification:

- Live DB still marks policy id `37` active for vault `92`.
- Policy id `37` still carries split Jupiter lane metadata with
  `swap_lanes[0].policy_account = B53LFhxNE5rAsQmbyEggDzYSDG7UPgYqVU4KSbjcPKJg`.
- Current orchestrator rejects that split metadata before execution.
- Compact all-in-one policy update/create dry-runs both fail Squads simulation
  with `InstructionDidNotDeserialize`, so no mainnet policy update was sent.
- The live accounts therefore need a compatible compact ProgramInteraction
  policy update before current cross-mint orchestration can run again.

Historical cross-mint conclusion: the worker executed the mainnet cross-mint
withdraw and Jupiter swap through split Squads ProgramInteraction policies, and
the funds ended in the target USDT Kamino reserve after creating the missing
target collateral ATA and submitting the recovery deposit. Current code no
longer treats this split-policy topology as valid.

## Remaining Follow-Ups

1. Resolve the compact ProgramInteraction create/update mismatch on mainnet.

   The local encoder matches the policies-branch shape checked on 2026-06-09,
   but the deployed mainnet Squads program rejects the actual
   `ExecuteSettingsTransactionSync` payload with
   `InstructionDidNotDeserialize`. Do not submit policy changes for these
   accounts until the full transaction simulation passes.

2. Update the live DB/on-chain policy for vault `92` only after compact
   ProgramInteraction preflight succeeds.

   The current DB still points at split `swap_policy_account` metadata. Current
   orchestrator code intentionally rejects that state.

3. Add an idempotent "ensure vault token accounts" setup phase before route
   submission.

   The worker deliberately rejects Jupiter setup/cleanup instructions. It should
   derive and create required vault ATAs in a separate payer-funded setup
   transaction before submitting a protected route. For cross-mint USDC to USDT,
   the required vault accounts are source collateral, source liquidity, target
   liquidity, and target collateral.

4. Improve partial-route recovery bookkeeping for historical split executions.

   Decision `28` correctly shows the worker failure at step 2, but the later
   recovery deposit is not linked to that decision. Add a recovery/follow-up
   status or metadata path so operational history can show "worker partial
   success, manual recovery complete".

5. Timescale target rows used for overrides still need freshness cleanup.

   Some latest rows used during these tests had stale March 2026 timestamps, so
   forced target overrides were used for the historical mainnet cross-mint test.

## Validation Commands

Passed:

- `cargo fmt`
- `cargo test -p loyal-actions`
- `cargo test -p loyal-yield-policy-init`
- `bun run yield-policy:init -- --help`
- `cargo test -p loyal-yield-orchestrator -- --test-threads=1`
  using a temporary local PostgreSQL 17 database with migration
  `crates/loyal-yield-orchestrator/migrations/0001_loyal_yield_orchestration.sql`
- `cargo test -p squads-test-harness --test usdc_pyusd_kamino_route cross_mint_route_execution_pack_size_is_packet_bound_by_measurement -- --exact`
- Mainnet dry-run through a permanent `op signin` shell:
  `loyal-yield-policy-init --cluster mainnet --settings 4aWM... --topology all-in-one --withdraw-action-seed 4 --dry-run`

Residual failure:

- `bun run test:squads` still fails in `kamino_reserves` with
  `InstructionError(0, IncorrectProgramId)` while invoking mock
  `KLend2g3c...`.
- `cargo test -p squads-test-harness --test yield_route_policy_adversarial happy_path_same_mint_jupiter_and_hub_routes_are_live -- --exact`
  fails at the same KLend mock setup step before reaching the Jupiter or Loyal
  Hub cross-mint route assertions.

## References

- [Squads Smart Account Program policies branch](https://github.com/Squads-Protocol/smart-account-program/tree/policies)
- [Jupiter `ROUTE_IX_DISCM` source](https://docs.rs/jupiter_interface/latest/src/jupiter_interface/instructions.rs.html)
- [Jupiter routing documentation](https://dev.jup.ag/docs/swap/routing)

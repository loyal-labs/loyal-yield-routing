# Mainnet Yield Routing Test Report

Date: 2026-06-08

## Target

Validate the Loyal yield routing orchestration on Solana mainnet with real funds:

- Fund and prepare a Squads vault.
- Seed a Kamino position.
- Trigger the worker loop to reroute funds.
- Verify same-mint routing, cross-mint/Jupiter readiness, DB state, and blockchain state.
- Check that actual Jupiter swaps work on mainnet.

Accounts used:

- System/setup key: `GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N`
- Additional provided key: `6T7U8nSZmkRYsrsA5ivhk4YHkqE1JkCdCNHYVtCrMThr`
- Worker delegated signer from 1Password env: `BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ`

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

## Cross-Mint Orchestration Attempt

Resolved a USDT Kamino target:

- Reserve: `H3t6qZ1JkguCNTi9uzVKqQ7dvt2cum4XiXWom6Gn5e5S`
- Market: `7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF`
- Liquidity mint: `Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB`
- Collateral mint: `B8zf4kojJbwgCRKA7rLaLhRCZBGhgAJp8wPBVZZHMhSv`
- Token program: SPL Token

Tried to install a Jupiter cross-mint policy on the same settings.

Legacy all-in-one encoding failed before submission:

- Transaction size: `1726` bytes
- Solana packet limit: `1232` bytes

Compiled all-in-one encoding dry-run fit the packet:

- Transaction size: `978` bytes
- But Squads rejected it with `InstructionDidNotDeserialize` (`102`)
- No cross-mint policy transaction was submitted.

Cross-mint conclusion: cross-mint routing is not currently executable through this orchestration on mainnet.

## Blockers

1. Cross-mint policy creation does not fit with legacy encoding.

   The Jupiter all-in-one policy with USDC and USDT allowlists was `1726` bytes, above the `1232` byte packet limit.

2. Latest Squads policies-branch compiled encoding is not accepted by the current mainnet path.

   Reproduction used the existing mainnet settings account and the next expected Squads policy seed after the installed same-mint policy. The initial run used a persistent shell with `op signin` and the delegated signer loaded from `YIELD_ROUTER_KEYPAIR`.

   ```sh
   op run --env-file=.env.1password -- sh -c 'cargo run -p loyal-yield-policy-init --bin loyal-yield-policy-init -- --cluster mainnet --rpc-url https://api.mainnet-beta.solana.com -k GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N.json --settings 4aWMf1dFxviHisBFfi9apgqNDUBH4rLWHQYHUANbLAdi --topology all-in-one --program-interaction-encoding compiled --stable-mint EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v,Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB --kamino-market 7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF --kamino-liquidity-mint EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v,Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB --swap-lane jupiter --withdraw-action-seed 2 --swap-action-seed 3 --deposit-action-seed 4 --dry-run'
   ```

   The all-in-one compiled policy dry-run still fit the packet:

   - Policy account: `BJgmjzDJUJdDE5XNU5RJphxrdWM7Nym3c8XSDrnpS1y4`
   - Transaction bytes (legacy transaction format): `978`
   - Message bytes: `913`
   - Instruction data bytes: `668`
   - Simulation slot: `425161966`
   - Simulation error: `InstructionError(2, Custom(102))`
   - Squads log: `InstructionDidNotDeserialize. Error Number: 102. Error Message: The program could not deserialize the given instruction.`

   The policy-support probe in the same dry-run deserialized `execute_settings_transaction_sync` correctly and failed later with `AccountNotInitialized` on the intentionally missing probe policy account. That isolates the `102` failure to the actual compiled `PolicyCreate` payload, not the outer instruction discriminator, signer list, settings account, or delegated signer.

   Local fixes applied after checking the Squads policies branch:

   - Corrected the local V2 compiled hook wire model so `CompiledHook.instruction_data` uses a `u16` length prefix, matching Squads `SmallVec<u16, u8>`.
   - Corrected the local policy decoder skip path for compiled hooks to read the same `u16` length prefix.
   - Added a `--delegated-signer <PUBKEY>` override to the policy-init CLI because policy creation only needs the delegated signer's public key; this avoids requiring the router private key for dry-run/policy construction checks.

   Re-running the mainnet cross-mint all-in-one compiled dry-run after those local fixes still failed the same way:

   ```sh
   cargo run -p loyal-yield-policy-init --bin loyal-yield-policy-init -- --cluster mainnet --rpc-url https://api.mainnet-beta.solana.com -k GTpqQfB9wgXWqdhkEmSWsnHVvxaPbJs1qWsomh1MjQ5N.json --settings 4aWMf1dFxviHisBFfi9apgqNDUBH4rLWHQYHUANbLAdi --delegated-signer BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ --topology all-in-one --program-interaction-encoding compiled --stable-mint EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v,Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB --kamino-market 7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF --kamino-liquidity-mint EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v,Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB --swap-lane jupiter --withdraw-action-seed 2 --swap-action-seed 3 --deposit-action-seed 4 --dry-run
   ```

   - Simulation slot: `425164696`
   - Transaction bytes: `978`
   - Message bytes: `913`
   - Instruction data bytes: `668`
   - Simulation error: `InstructionError(2, Custom(102))`
   - Squads log: `InstructionDidNotDeserialize. Error Number: 102. Error Message: The program could not deserialize the given instruction.`

   The deployed mainnet program also does not match the locally built `policies` branch binary:

   - Squads policies branch commit tested: `c8138e972a967fcd9341d6c49a2bbf619432bca9`
   - Locally built policies-branch SBF SHA-256: `8fc55f8d19af93c02a0db6a612d24612aadab04a0a1bb9e1062f5c5329d9be92`
   - Dumped mainnet `SMRT...` SBF SHA-256: `49cf27024d211ab827eadc11219a935abf9a5138ece1c0b0631c26790fd4f3c0`
   - Mainnet `SMRT...` last deployed slot: `383815455`
   - Mainnet `SMRT...` upgrade authority: `HT3JknwuufXdtVJggz5Z9JcnYtanPpLzTCqLWsVX1Vu2`

   Root cause: the transaction is reaching a Squads Smart Account binary/ABI path that does not deserialize the latest policies-branch V2 `PolicyCreate` payload. This is an ABI/version mismatch, not a policy-validation failure. The policies branch is the relevant source of truth for the current policy work: it defines `PolicyCreationPayload` with five variants, where tag `3` is `LegacyProgramInteraction` and tag `4` is the compact V2 `ProgramInteraction` payload. That V2 payload has `account_index`, `pubkey_table`, compiled instruction constraints, optional compiled hooks, and compiled spending limits. Our no-hook compiled all-in-one payload is intended to use that tag `4` shape; reverting to legacy is not the latest-policy fix.

   The failed legacy comparison is still useful only as a size proof. Re-running the same USDC/USDT all-in-one policy with legacy encoding failed before simulation with `transaction is 1662 bytes, which exceeds the 1232 byte Solana packet limit`. Solana's transaction format keeps instruction data as opaque bytes inside the packet, so v0/address lookup tables can reduce account-key bytes but cannot remove the oversized policy payload itself. The latest Squads policies branch solves that class of problem with the compact V2 payload, so the route should stay on compiled V2 once the program/ABI version is verified.

   Proposed fix:

   - Pin the Squads Smart Account dependency to the `policies` branch, preferably to exact commit `c8138e972a967fcd9341d6c49a2bbf619432bca9` or a later reviewed policies commit, and generate or vendor the local policy wire types from that source instead of hand-maintaining enum variants and small-vector wrappers.
   - Do not submit compact V2 policies to the current mainnet `SMRT...` binary unless Squads upgrades that program to a binary matching the policies branch. The current mainnet deployment is controlled by upgrade authority `HT3JknwuufXdtVJggz5Z9JcnYtanPpLzTCqLWsVX1Vu2`, so this repo cannot fix the existing `4aWM...` settings account path by code changes alone.
   - If immediate mainnet V2 testing is required before Squads upgrades `SMRT...`, deploy the policies-branch program under a new program id, create a fresh settings/vault under that program, and make the Loyal action builder/policy-init CLI accept a configurable Squads program id for instruction program ids and PDA derivations. Existing settings created under `SMRT...` cannot be reused under a different program id.
   - Keep `--program-interaction-encoding compiled` for latest policies, but gate it on the hash/ABI check above. If the check fails, fail policy installation with an actionable error instead of falling back to legacy.
   - Add a golden serialization test that compares the local serialized `SettingsAction::PolicyCreate { policy_creation_payload: ProgramInteraction(..) }` bytes against the pinned Squads policies-branch crate for a tiny V2 payload. This catches enum order, `SmallVec` length width, and field-order drift before mainnet simulation.
   - Continue deriving policy PDAs from the settings account's sequential internal `policy_seed`. The policies-branch handler increments `settings.policy_seed` and derives the next policy PDA from that counter; arbitrary seeds produce `MissingAccount`. On existing settings `4aWM...`, the next policy after the same-mint seed `1` is seed `2`.
   - After the V2 binary/ABI check passes, prefer the compact all-in-one route policy again. Keep the split withdraw/swap/deposit topology as the fallback only if the V2 all-in-one policy still exceeds packet or compute limits.

   References checked:

   - [Squads Smart Account Program policies branch](https://github.com/Squads-Protocol/smart-account-program/tree/policies): current policy work branch.
   - [Squads policies-branch README](https://raw.githubusercontent.com/Squads-Protocol/smart-account-program/policies/README.md): confirms the `SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG` program id and gives the deployed-program hash verification flow.
   - [Squads policies-branch `PolicyCreationPayload`](https://raw.githubusercontent.com/Squads-Protocol/smart-account-program/policies/programs/squads_smart_account_program/src/state/policies/policy_core/payloads.rs): defines `LegacyProgramInteraction` followed by compact V2 `ProgramInteraction`.
   - [Squads policies-branch `ProgramInteractionPolicyCreationPayload`](https://raw.githubusercontent.com/Squads-Protocol/smart-account-program/policies/programs/squads_smart_account_program/src/state/policies/implementations/program_interaction.rs): defines the latest V2 payload, built-in pubkey indexes, and compiled hook layout.
   - [Squads policies-branch settings handler](https://raw.githubusercontent.com/Squads-Protocol/smart-account-program/policies/programs/squads_smart_account_program/src/state/settings.rs): derives policy PDAs from the incremented `settings.policy_seed`.
   - [Solana transaction structure docs](https://solana.com/docs/core/transactions/transaction-structure): confirms the `1232` byte packet limit and that instruction data remains opaque bytes inside the transaction packet.

3. Worker cross-mint quote provider is not live-Jupiter enabled.

   The planner requires `quote.swap.instruction` for cross-mint execution. The current conservative quote provider cannot produce a live Jupiter swap instruction for the orchestration route.

4. Actual Jupiter instructions do not match current policy assumptions.

   Earlier instruction inspection showed Jupiter swap instructions with a different discriminator/account layout than the policy currently expects. Some routes also include setup instructions and shared-account route layouts that are not represented by the current Squads policy constraints.

5. Post-route decision bookkeeping is incomplete.

   The same-mint decision row was marked `confirmed`, but `post_snapshot_id` remained `null`. A later dry worker pass reconciled `vault_reserve_positions_current` correctly, so the current-position truth is correct, but the confirmed decision is not linked to its post snapshot.

6. Timescale target rows used for overrides had stale `observedAt` timestamps.

   The worker run used high `--max-apy-age-secs` because the latest rows API returned rows with March 2026 timestamps for these reserves, even though the reserve account metadata decoded over RPC was valid.

## Token Account Requirement

For the same-mint test, the minimum vault token accounts were:

- Source liquidity ATA
- Source collateral ATA
- Target collateral ATA

For cross-mint USDC to USDT, expect at least:

- Source collateral ATA
- Source liquidity ATA
- Target liquidity ATA
- Target collateral ATA

Recommendation: add an idempotent "ensure vault token accounts" setup phase to the worker. It should derive required ATAs from the planned route and create missing accounts in a separate payer-funded setup transaction before submitting the Squads-protected route. This avoids pre-provisioning every possible reserve while keeping route policy execution narrow.

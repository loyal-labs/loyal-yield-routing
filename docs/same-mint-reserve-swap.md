# Same-Mint Reserve Swap Script

This is the first local proof script for moving a vault's USDC position between Kamino USDC reserves with Neon as the control plane.

The script is intentionally scoped to Main USDC <-> Prime USDC. Use `--direction main-to-prime` or `--direction prime-to-main`; `main-to-prime` is the default.

- Main USDC reserve: `D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59`
- Prime USDC reserve: `9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu`
- Liquidity mint: `EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v`

Run the full lifecycle dry-run through 1Password:

```sh
op run --env-file=.env.1password -- bun run same-mint:swap -- \
  --settings <SQUADS_SETTINGS> \
  --vault-index <VAULT_INDEX> \
  --e2e-main-prime-main <AMOUNT_RAW>
```

`--e2e-main-prime-main` is the verifier-facing command. It defaults to dry-run and expands into five child invocations of this same binary: policy create/update with lookup-table provisioning, initial Main USDC deposit, Main -> Prime move, Prime -> Main move, and full Main USDC withdrawal. Add `--execute` only after explicit approval; then each child phase keeps its own simulation-before-send guard and exits on the first failed phase.

Run only the route dry-run through 1Password:

```sh
op run --env-file=.env.1password -- bun run same-mint:swap -- \
  --settings <SQUADS_SETTINGS> \
  --vault-index <VAULT_INDEX> \
  --direction main-to-prime \
  --reconcile-from-chain \
  --seed-from-user-position
```

`--reconcile-from-chain` previews the vault's source/target Kamino obligation accounts and vault USDC ATA from chain. Dry-run prints the derived obligation addresses, obligation collateral amounts, bridge USDC ATA status, required reserves, policy preflight, route-build status, and every execution preflight blocker without writing Neon rows.

`--seed-from-user-position` is a temporary planning bridge for this first proof when chain reconciliation is not available. It reads the active `loyal_yield.user_yield_positions` row for the selected settings/vault index and previews `current_amount_raw` as the source amount. When `--reconcile-from-chain` is present, chain obligation state is authoritative and the user-position row is informational only. Under `--execute --reconcile-from-chain`, the script first reconciles the chain preview through `NeonSqlClient::reconcile_vault`, writing the normal `vault_position_snapshots`, `vault_position_snapshot_positions`, and `vault_reserve_positions_current` rows before planning.

Live execution is blocked until the dry-run proves the required chain state.

- Source reserve has a non-zero chain `amountRaw` with `amountSemantics = kamino_obligation_collateral_deposited_amount`.
- `obligationExists` is true for the source market obligation.
- `vaultLiquidityTokenAccountExists` is true for the vault USDC ATA.
- The destination obligation already exists, or the script can temporarily update the policy to a market-scoped KLend `init_obligation` setup policy, execute init with the vault as owner/payer, and restore the route policy before value movement.
- KLend reserve and obligation refreshes are emitted as public pre-instructions. They are not part of the protected route policy.

The dry-run must also show the required policy state:

- `policyPreflight.neonAllowsRequiredMarkets` is true.
- `policyPreflight.policyAccountDecode.decodedAllowsRequiredMarkets` is true.
- `policyPreflight.policyAccountDecode.decodedAllowsRequiredRouteSteps` is true.
- `policyPreflight.policyAccountDecode.decodedAllowsInitObligation` is false for the normal route policy. Missing-obligation setup uses a temporary init-only policy, then restores the route policy.
- `policyPreflight.policyAccountDecode.decodedAllowsRefreshObligation` is false for the normal route policy.
- `routeExecution.policyConstraintValidation.matches` is true.

The decision state must be clear:

- `executionPreflightBlocker` is null.
- `executionPreflightBlockers` is empty.
- `routeBuildError` is null.
- No active decision exists for the vault.

Policy create/update dry-run:

```sh
op run --env-file=.env.1password -- bun run same-mint:swap -- \
  --settings <SQUADS_SETTINGS> \
  --vault-index <VAULT_INDEX> \
  --update-policy \
  --provision-lookup-table
```

Policy update mode uses `SOLANA_TESTING_PK` as the Squads settings authority and writes `YIELD_ROUTER_KEYPAIR` as the delegated ProgramInteraction signer. It simulates the settings transaction before any send. `--execute` submits only after the authority key matches the selected policy row.

By default, `--update-policy` targets the next derived policy seed, which is useful for proving fresh policy creation. Add `--update-active-policy` to intentionally update the currently active DB policy in place:

```sh
op run --env-file=.env.1password -- bun run same-mint:swap -- \
  --settings <SQUADS_SETTINGS> \
  --vault-index <VAULT_INDEX> \
  --update-policy \
  --update-active-policy
```

That active-policy path is useful for narrowing an already live policy without creating another policy account.

Policy update and route execution compile v0 transactions, matching the sibling Earn verifier's `compilePreparedOperation(...)` pattern. Pass one or more lookup tables with `--lookup-table <PUBKEY>` or set `YIELD_ROUTE_LOOKUP_TABLES` to a comma/whitespace separated list. The dry-run reports `transaction.packetSizeBytes`, `transaction.packetDataSizeBytes`, lookup-table counts, static key counts, and `instructionDataBytes` before simulation. If the serialized packet is already over Solana's 1232-byte limit, simulation is skipped and the JSON reports the packet blocker.

`--provision-lookup-table` is supported in policy update mode and in the top-level lifecycle command. In dry-run it reports `lookupTableProvisioning.requiredAddresses`, the missing addresses from any supplied lookup tables, and the lookup table address it would derive from the current slot. With `--execute`, it creates a fresh lookup table using `SOLANA_TESTING_PK`, extends it with the missing policy-update account keys, waits until the table is warm for lookup use, reloads the table, and then builds the policy transactions with that table.

Initial Main USDC deposit dry-run:

```sh
op run --env-file=.env.1password -- bun run same-mint:swap -- \
  --settings <SQUADS_SETTINGS> \
  --vault-index <VAULT_INDEX> \
  --deposit-main-usdc <AMOUNT_RAW>
```

Initial deposit mode uses `SOLANA_TESTING_PK` as the funding wallet. If the Main obligation is missing, execute mode first runs the same temporary init-only policy setup/restore sequence used by route execution. It then builds a user-signed funding transaction that creates the vault USDC ATA idempotently and transfers USDC into it, followed by a `YIELD_ROUTER_KEYPAIR`-signed ProgramInteraction execution that deposits into Kamino Main USDC. The KLend obligation refresh is a public pre-instruction before the protected deposit. In dry-run, the funding transaction is simulated, while the policy deposit transaction reports a packet summary and skips simulation when the funding transaction has not landed yet. `--execute` submits funding first, reloads chain state, simulates the policy deposit, submits it, and reconciles `vault_reserve_positions_current` only after confirmation.

Full Main USDC withdraw dry-run, after the position has been moved back to Main:

```sh
op run --env-file=.env.1password -- bun run same-mint:swap -- \
  --settings <SQUADS_SETTINGS> \
  --vault-index <VAULT_INDEX> \
  --full-withdraw-main-usdc
```

Full withdraw mode uses `YIELD_ROUTER_KEYPAIR` to execute one policy-mediated KLend `withdraw_obligation_collateral_and_redeem_reserve_collateral_v2` instruction against the Main USDC obligation. KLend reserve and obligation refreshes are public pre-instructions. Dry-run reports the decoded policy withdraw constraint index, packet/simulation status when a non-zero Main obligation exists, the Main obligation account proof, and the vault lamport proof before execution. `--execute` submits only after simulation passes, reloads chain state, reconciles `vault_reserve_positions_current` only after confirmation, and reports whether all tracked Main/Prime positions are zero, whether the Main obligation closed, and whether the vault lamports increased by at least the closed obligation lamports.

Execute command shape, after explicit approval:

```sh
op run --env-file=.env.1password -- bun run same-mint:swap -- \
  --settings <SQUADS_SETTINGS> \
  --vault-index <VAULT_INDEX> \
  --direction main-to-prime \
  --reconcile-from-chain \
  --seed-from-user-position \
  --execute
```

Current implementation note: the script reuses `loyal-yield-orchestrator` same-mint input validation and reads current positions from Neon. It can preview the chain state needed for current-position reconciliation and can seed current-position rows from `user_yield_positions` through the existing snapshot store when `--execute --seed-from-user-position` is approved.

`--execute` first requires a chain preflight with a non-zero source collateral account. The decoded Squads route policy-account state must include `YIELD_ROUTER_KEYPAIR` as a delegated signer, both required markets, and the required live KLend same-mint withdraw/deposit route steps. If the destination obligation is absent, the script temporarily switches to an init-only policy, executes KLend `init_obligation`, restores the route policy, and then runs public refresh pre-instructions plus the protected withdraw/deposit pair. Route execution indexes are selected from the decoded Squads ProgramInteraction policy account when chain preflight is available, instead of trusting stale route metadata.

If the execution preflight fails, the script returns before writing a `rebalance_decisions` row. After that preflight passes, it calls `prepare_same_mint_rebalance`, reloads the persisted `loyal_yield.rebalance_decisions` row by decision id, verifies its `same_mint` execution plan fields and idempotency key, simulates the Squads ProgramInteraction route built from that row, submits the transaction, waits for confirmation, and calls `confirm_same_mint_rebalance` to finalize the decision and current-position snapshot. If any adapter step fails after the decision is created, the script marks the decision failed instead of leaving it active.

Current policy evidence: policy construction uses the requested compact constraint semantics for same-mint USDC, namely KLend market plus liquidity mint. It does not constrain by reserve in the route policy payload. The generated same-mint withdraw/deposit constraints use account `2` for the allowed market list and account `5` for the USDC liquidity mint.

Live E2E evidence for settings `6jgk...`, vault index `1`:

- Initial deposit into Main USDC succeeded: `471SuRWWh9DvPK5fh8ZwMw5FpipKhSp5KFpminaxNXTCLVTUT7okkaxS5dBoDeMiqfD4JkFBETdHtMHuv42y1CmW`.
- Main -> Prime succeeded as decision `206`: `4fhjqHJbPhZsKoQp3fydNgXvAr8EjRJGWRXnHSDya1vjEj3JqXzVkGZZS6TvZtWDPkkEPjsdu8YVpe2SAsqsVXKw`.
- Prime -> Main succeeded as decision `207`: `5KfLgjfhQPSi9F9hP6bSvfWJ2H2k7MwaqDQ2BvEh4B8w4zPCV5dYYZpyiHV5oGo3CXiSvSoxJZUU5QGh7g8kMaGH`.
- Full Main withdrawal succeeded: `5GVJ5wM5qpUAS2RaHnahBVTBM9SKjMcRWyasx1FW6gAiGmMAFsg237Unyb9fYrZYKkQKEawpPpkd29LExzfAWxtX`.
- Final readback showed Main and Prime positions at `0`, both tracked obligations closed, vault USDC ATA at `997` raw, and snapshot `217`.
- Full-withdraw rent proof reported `closedObligationLamports = 24165120`, `rentRefundLamports = 24165120`, and `refundAtLeastClosedObligationLamports = true`.

The active route policy was later narrowed in place after moving KLend refresh out of policy execution. Active policy `77DRX1HR3WdTTsTLaHowiEnCpm3hHCXVXgwxxboUhaKQ` now decodes as:

- `instructionCount = 2`
- `decodedAllowsInitObligation = false`
- `decodedAllowsRefreshObligation = false`
- `decodedAllowsRequiredMarkets = true`
- `decodedAllowsRequiredRouteSteps = true`
- route steps: KLend withdraw and KLend deposit only
- live update signature: `RfV9gXFJM7nxM5vtQHG6iJ7EqYt1N5B49CEAYAtgRjftkzC9cW4G69STweadr3tav4JCvNEEKdCXqQ64w2ezRDR`

The normal route readback after that update confirms the DB active policy and on-chain policy account agree. Route execution is currently blocked only because the source reserve has no value after the successful full withdrawal.

Operational note: a fresh next-seed bootstrap policy account `HPhDWjk7VDZefbcZfxSbmGYDFYmzwW5pwSx5C8UxfP4N` was created while proving the fresh-policy path, but its finalize update was not sent because simulation failed with `InsufficientFundsForRent`. The settings authority had only about `0.0018 SOL`, while resizing a smaller bootstrap policy to the final policy size needed more rent. The DB active policy remains `77DR...`.

Implementation note: the Squads harness has a newer pubkey-table ProgramInteraction representation, but the sibling generated SDK for the deployed Squads program uses the raw ProgramInteraction payload. A brief dry-run against the deployed program confirmed the pubkey-table tag is rejected with `InstructionDidNotDeserialize`, so this script stays on the live-compatible raw payload.

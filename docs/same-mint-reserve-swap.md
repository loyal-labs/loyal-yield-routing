# Same-Mint Reserve Swap Script

This is the local proof script for moving a vault's USDC position between Kamino
USDC reserves with Neon as the control plane.

The legacy lifecycle helpers are scoped to Main USDC <-> Prime USDC. Use
`--direction main-to-prime` or `--direction prime-to-main`; `main-to-prime` is
the default for those helpers. The fleet monitor is broader: it loads every
fresh Safe-basket USDC reserve from Timescale, reconciles all of those reserves
from chain into Neon, and routes to the highest-APY USDC reserve when there is a
positive edge. The route execution preflight then fails closed if the on-chain
policy does not allow the selected market.

- Main USDC reserve: `D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59`
- Prime USDC reserve: `9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu`
- Liquidity mint: `EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v`

The examples below use these shell aliases:

```sh
SWAP='op run --env-file=.env.1password -- bun run same-mint:swap --'
MONITOR='op run --env-file=.env.1password -- bun run same-mint:monitor --'
MONITOR_E2E='op run --env-file=.env.1password -- bun run same-mint:monitor-e2e --'
CLEANUP='op run --env-file=.env.1password -- bun run same-mint:alt-cleanup --'
```

Run the full lifecycle dry-run through 1Password:

```sh
$SWAP \
  --settings <SQUADS_SETTINGS> \
  --vault-index <VAULT_INDEX> \
  --e2e-main-prime-main <AMOUNT_RAW>
```

`--e2e-main-prime-main` is the verifier-facing command. It defaults to dry-run and expands into five child invocations of this same binary: policy create/update with durable policy lookup-table provisioning, initial Main USDC deposit, Main -> Prime move, Prime -> Main move, and full Main USDC withdrawal. Add `--execute` only after explicit approval; then each child phase keeps its own simulation-before-send guard and exits on the first failed phase.

Run only the route dry-run through 1Password:

```sh
$SWAP \
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
- If the destination obligation is missing, the decoded route policy must expose a target-market `init_obligation` constraint. In `--optimization-cycle --execute`, the script executes that init constraint first, confirms it, reloads chain state, and only then builds the same-mint route. No rebalance decision or source withdrawal is sent before the destination obligation exists.
- KLend reserve and obligation refreshes are emitted as public pre-instructions. They are not part of the protected route policy.

The dry-run must also show the required policy state:

- `policyPreflight.neonAllowsRequiredMarkets` is true.
- `policyPreflight.policyAccountDecode.decodedAllowsRequiredMarkets` is true.
- `policyPreflight.policyAccountDecode.decodedAllowsRequiredRouteSteps` is true.
- `policyPreflight.policyAccountDecode.decodedAllowsInitObligation` is true for the normal route policy, and `targetObligationSetup.initObligationInstructionConstraintIndex` points at the target-market init constraint when setup is needed.
- `policyPreflight.policyAccountDecode.decodedAllowsRefreshObligation` is false for the normal route policy.

The route execution check must also report
`routeExecution.policyConstraintValidation.matches = true`.

The decision state must be clear:

- `executionPreflightBlocker` is null.
- `executionPreflightBlockers` is empty.
- `routeBuildError` is null.
- No active decision exists for the vault.

Policy create/update dry-run:

```sh
$SWAP \
  --settings <SQUADS_SETTINGS> \
  --vault-index <VAULT_INDEX> \
  --update-policy \
  --provision-lookup-table
```

Policy update mode uses `SOLANA_TESTING_PK` as the Squads settings authority and writes `YIELD_ROUTER_KEYPAIR` as the delegated ProgramInteraction signer. It simulates the settings transaction before any send. `--execute` submits only after the authority key matches the selected policy row.

By default, `--update-policy` targets the next derived policy seed, which is useful for proving fresh policy creation. Add `--update-active-policy` to intentionally update the currently active DB policy in place:

```sh
$SWAP \
  --settings <SQUADS_SETTINGS> \
  --vault-index <VAULT_INDEX> \
  --update-policy \
  --update-active-policy
```

That active-policy path is useful for narrowing an already live policy without creating another policy account.

Policy update and route execution compile v0 transactions, matching the sibling Earn verifier's `compilePreparedOperation(...)` pattern. Pass one or more lookup tables with `--lookup-table <PUBKEY>` or set `YIELD_ROUTE_LOOKUP_TABLES` to a comma/whitespace separated list. Route execution also loads durable lookup tables from the Neon `loyal_yield.route_lookup_tables` registry for the cluster/scope/authority. The dry-run reports `transaction.packetSizeBytes`, `transaction.packetDataSizeBytes`, lookup-table counts, static key counts, and `instructionDataBytes` before simulation. If the serialized packet is already over Solana's 1232-byte limit, simulation is skipped and the JSON reports the packet blocker.

`--provision-lookup-table` is policy-update/admin-only and must be combined with `--update-policy`. In dry-run it reports `lookupTableProvisioning.requiredAddresses` and the missing addresses from supplied or registry lookup tables. With `--execute`, it creates or extends a durable table only when needed, waits until the table is warm for lookup use, reloads it, and records the table plus create/extend signatures in the registry. It cannot be combined with `--optimization-cycle`.

Route ALT setup is a separate explicit provisioning mode:

```sh
$SWAP \
  --settings <SQUADS_SETTINGS> \
  --vault-index <VAULT_INDEX> \
  --source-reserve <SOURCE_RESERVE> \
  --target-reserve <TARGET_RESERVE> \
  --provision-route-lookup-table \
  --reconcile-from-chain
```

Add `--execute` only after approval. This mode uses `YIELD_ROUTER_KEYPAIR` as the lookup-table authority and payer, reuses an authority-matching durable table when capacity allows, extends only missing addresses, records the durable registry row, and exits without writing a rebalance decision or sending the route transaction. It cannot be combined with `--optimization-cycle` or `--seed-from-user-position`. Normal route execution is reuse-only: if durable lookup-table coverage is incomplete, it fails closed before route simulation or send.

ALT cleanup dry-run:

```sh
$CLEANUP \
  --include-env-authorities \
  --authority 62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5 \
  --scan-program-accounts \
  --scan-history
```

The cleanup command protects registry tables, `YIELD_ROUTE_LOOKUP_TABLES`, and explicit `--allowlist` tables. It keeps the JSON report on stdout; add `--trace-timing` to emit timestamped phase trace logs to stderr. `--include-env-authorities` derives audited public keys from present `YIELD_ROUTER_KEYPAIR`, `POLICY_KEYPAIR`, `DEPLOYMENT_PK`, and `SOLANA_TESTING_PK` values without printing secrets. `--scan-program-accounts` discovers currently reclaimable ALT accounts by authority, with paginated `getProgramAccountsV2` fallback on Helius. `--scan-history` adds recent create/extend/deactivate/close evidence from audited signer history; use `--history-limit <N>` to widen the search and `--min-slot <SLOT>` for post-deploy audits. Limited scans stop once the requested candidate limit is reached, and candidate accounts are loaded in batches. Add `--execute` only after approval; active orphan tables are deactivated first, and closeable deactivated tables are closed after the ALT cooldown. For live cleanup, `--simulate-before-submit` simulates each signed cleanup transaction immediately before submit, and `--bundle-size <N>` groups up to N close/deactivate lookup-table instructions that share the same authority signer into one transaction.

Initial Main USDC deposit dry-run:

```sh
$SWAP \
  --settings <SQUADS_SETTINGS> \
  --vault-index <VAULT_INDEX> \
  --deposit-main-usdc <AMOUNT_RAW>
```

Initial deposit mode uses `SOLANA_TESTING_PK` as the funding wallet. If the Main obligation is missing, execute mode first executes the Main-market `init_obligation` constraint already present in the route policy, confirms it, and reloads chain state. It then builds a user-signed funding transaction that creates the vault USDC ATA idempotently and transfers USDC into it, followed by a `YIELD_ROUTER_KEYPAIR`-signed ProgramInteraction execution that deposits into Kamino Main USDC. The KLend obligation refresh is a public pre-instruction before the protected deposit. In dry-run, the funding transaction is simulated, while the policy deposit transaction reports a packet summary and skips simulation when the funding transaction has not landed yet. `--execute` submits funding first, reloads chain state, simulates the policy deposit, submits it, and reconciles `vault_reserve_positions_current` only after confirmation.

Setup-only obligation initialization for a policy-eligible Safe USDC reserve:

```sh
$SWAP \
  --settings <SQUADS_SETTINGS> \
  --vault-index <VAULT_INDEX> \
  --setup-obligation-reserve <RESERVE>
```

With `--execute`, this setup/admin mode executes the target-market
`init_obligation` constraint already present in the route policy. In admin mode
`SOLANA_TESTING_PK` may pay the outer transaction, while `YIELD_ROUTER_KEYPAIR`
remains the delegated policy signer. Fleet optimization uses
`YIELD_ROUTER_KEYPAIR` as both outer payer and delegated signer, initializes a
missing target obligation the same way, reloads chain state, and only then sends
the same-mint route.

Full reserve withdraw dry-run, after the position has been moved to any Safe
USDC reserve:

```sh
$SWAP \
  --settings <SQUADS_SETTINGS> \
  --vault-index <VAULT_INDEX> \
  --full-withdraw-reserve <CURRENT_RESERVE>
```

`--full-withdraw-main-usdc` remains as a compatibility alias for Main USDC.
Full withdraw mode uses `YIELD_ROUTER_KEYPAIR` to execute one policy-mediated
KLend `withdraw_obligation_collateral_and_redeem_reserve_collateral_v2`
instruction against the selected reserve's obligation. KLend reserve and
obligation refreshes are public pre-instructions. Dry-run reports the decoded
policy withdraw constraint index, packet/simulation status when a non-zero
obligation exists, the obligation account proof, the vault/wallet USDC proofs,
the policy account proof, and the rent cleanup preview. `--execute` submits only
after simulation passes, then uses the settings authority from
`SOLANA_TESTING_PK` to recover the vault USDC into the authority wallet, close
the vault USDC ATA, remove the route policy account, reload chain state,
reconcile `vault_reserve_positions_current` after confirmation, and mark the
selected route policy and managed vault inactive so fleet monitoring stops for
that vault. The output separates `policyWithdraw`, `walletRecovery`, and
`policyClose` signatures so verifier runs can check signer boundaries and close
evidence directly.

Fleet monitor dry-run:

```sh
$MONITOR \
  --once \
  --all-active-vaults
```

After the same-mint frontend/SDK E2E passed, Render runs the same monitor in
fleet execution mode:

```sh
/usr/local/bin/same-mint-yield-monitor --all-active-vaults --execute --poll-interval-seconds 300 --rebalance-cooldown-seconds 300
```

Fleet mode uses `YIELD_ROUTER_KEYPAIR` for active-policy discovery and route
execution; it does not use `SOLANA_TESTING_PK` and it does not provision ALTs.
A confirmed same-mint rebalance for a vault suppresses another same-vault
execution for 300 seconds; user deposits do not start that cooldown.

Monitor E2E dry-run:

```sh
$MONITOR_E2E \
  --settings <SQUADS_SETTINGS> \
  --vault-index <VAULT_INDEX> \
  --amount-raw <AMOUNT_RAW>
```

The E2E command requires Main USDC to have a real positive APY edge to another
eligible Safe USDC reserve before setup. With `--execute`, it creates or updates
the policy, pre-initializes the best target obligation when needed, deposits
into Main USDC, reads Neon after each phase, runs the fleet monitor until this
vault is executed, then withdraws from the reserve where the monitor landed.

Execute command shape, after explicit approval:

```sh
$SWAP \
  --settings <SQUADS_SETTINGS> \
  --vault-index <VAULT_INDEX> \
  --source-reserve <SOURCE_RESERVE> \
  --target-reserve <TARGET_RESERVE> \
  --optimization-cycle \
  --reconcile-from-chain \
  --execute
```

Current implementation note: the script reuses `loyal-yield-orchestrator` same-mint input validation and reads current positions from Neon. It can preview the chain state needed for current-position reconciliation and can seed current-position rows from `user_yield_positions` through the existing snapshot store when `--execute --seed-from-user-position` is approved.

`--optimization-cycle --execute` first requires a chain preflight with a non-zero source collateral account. If the destination obligation is missing, the decoded Squads route policy-account state must include a target-market `init_obligation` constraint; the script executes that setup transaction with `YIELD_ROUTER_KEYPAIR`, confirms it, and reloads chain state before route planning continues. The decoded policy must also include `YIELD_ROUTER_KEYPAIR` as a delegated signer, both required markets, and the required live KLend same-mint withdraw/deposit route steps. The route transaction uses `YIELD_ROUTER_KEYPAIR` as the fee payer and only optimization signer, and can only reuse durable lookup-table coverage loaded from the registry, CLI, or environment. Route execution indexes are selected from the decoded Squads ProgramInteraction policy account when chain preflight is available, instead of trusting stale route metadata.

If the execution preflight fails, the script returns before writing a `rebalance_decisions` row. After that preflight passes, it calls `prepare_same_mint_rebalance`, reloads the persisted `loyal_yield.rebalance_decisions` row by decision id, verifies its `same_mint` execution plan fields and idempotency key, simulates the Squads ProgramInteraction route built from that row, submits the transaction, waits for confirmation, and calls `confirm_same_mint_rebalance` to finalize the decision and current-position snapshot. If any adapter step fails after the decision is created, the script marks the decision failed instead of leaving it active.

Current policy evidence: policy construction uses the requested compact constraint semantics for same-mint USDC, namely KLend market plus liquidity mint. It does not constrain by reserve in the route policy payload. The generated same-mint withdraw/deposit constraints use account `2` for the allowed market list and account `5` for the USDC liquidity mint.

Live E2E evidence for settings `6jgk...`, vault index `1`:

- Initial deposit into Main USDC succeeded: `471SuRWWh9DvPK5fh8ZwMw5FpipKhSp5KFpminaxNXTCLVTUT7okkaxS5dBoDeMiqfD4JkFBETdHtMHuv42y1CmW`.
- Main -> Prime succeeded as decision `206`: `4fhjqHJbPhZsKoQp3fydNgXvAr8EjRJGWRXnHSDya1vjEj3JqXzVkGZZS6TvZtWDPkkEPjsdu8YVpe2SAsqsVXKw`.
- Prime -> Main succeeded as decision `207`: `5KfLgjfhQPSi9F9hP6bSvfWJ2H2k7MwaqDQ2BvEh4B8w4zPCV5dYYZpyiHV5oGo3CXiSvSoxJZUU5QGh7g8kMaGH`.
- Full Main withdrawal succeeded: `5GVJ5wM5qpUAS2RaHnahBVTBM9SKjMcRWyasx1FW6gAiGmMAFsg237Unyb9fYrZYKkQKEawpPpkd29LExzfAWxtX`.
- Final readback showed Main and Prime positions at `0`, both tracked obligations closed, vault USDC ATA at `997` raw, and snapshot `217`.
- Full-withdraw rent proof reported `closedObligationLamports = 24165120`, `rentRefundLamports = 24165120`, and `refundAtLeastClosedObligationLamports = true`.

The active route policy was later narrowed in place after moving KLend refresh
out of policy execution. That historical proof predates the obligation-ready
policy shape. The next live policy update must decode with KLend withdraw,
KLend deposit, and market-scoped `init_obligation`, while
`decodedAllowsRefreshObligation` remains false.

The normal route readback after that update confirms the DB active policy and on-chain policy account agree. Route execution is currently blocked only because the source reserve has no value after the successful full withdrawal.

Operational note: a fresh next-seed bootstrap policy account `HPhDWjk7VDZefbcZfxSbmGYDFYmzwW5pwSx5C8UxfP4N` was created while proving the fresh-policy path, but its finalize update was not sent because simulation failed with `InsufficientFundsForRent`. The settings authority had only about `0.0018 SOL`, while resizing a smaller bootstrap policy to the final policy size needed more rent. The DB active policy remains `77DR...`.

Implementation note: the Squads test crate has a newer pubkey-table
ProgramInteraction representation, but the sibling generated SDK for the
deployed Squads program uses the raw ProgramInteraction payload. A brief dry-run
against the deployed program confirmed the pubkey-table tag is rejected with
`InstructionDidNotDeserialize`, so this script stays on the live-compatible raw
payload.

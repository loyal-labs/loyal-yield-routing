# Treasury Loyal Hub Rebalance Policy Verifier

Use this verifier to decide whether the treasury Jupiter rebalance is actually guarded by a production-compatible Squads ProgramInteraction policy.

Required PASS conditions:

1. `scripts/mainnet-loyal-hub-tests.ts` no longer executes the treasury USDC to PYUSD rebalance as an unconstrained treasury-vault sync transaction. The rebalance's vault-signed inner payload is exactly three policy-checked instructions: Loyal Hub `WithdrawInventory`, Jupiter V6 swap A to B, and Token/Token-2022 `TransferChecked` of token B back into the Hub lane inventory. ATA setup and compute-budget instructions may happen outside the policy execution. Jupiter setup/cleanup instructions must not silently expand the guarded payload.

2. The treasury policy setup uses the same non-legacy ProgramInteraction creation model used by the production frontend builders in `/Users/zotho/Dev/loyal/loyal-app-main-policies/packages/smart-account-vaults/src/client.ts`: `createEarnProgramInteractionPolicyCreationPayload` and `createEarnInitObligationPolicyCreationPayload`. In this repo that means using the compact/generated ProgramInteraction payload shape, not the legacy raw policy payload encoder.

3. The package SDK exposes a named treasury Loyal Hub rebalance policy builder, with public types, that returns policy setup instruction(s), action account, route/index metadata, and a spec. It must support configurable lane, input/output mints, token programs, output decimals, max withdraw amount, max top-up amount, and max Jupiter slippage.

4. The policy constraints are tight enough that wrong program IDs, wrong Hub instruction tag, wrong lane, wrong input/output mint, wrong Hub inventory account, wrong treasury token account owner, wrong token program, excessive withdraw/top-up amount, excessive Jupiter slippage, or unrelated extra guarded instructions do not match the returned route indexes.

5. The TypeScript ABI generation surface includes the Loyal Hub `WITHDRAW_INVENTORY` constants/account indexes used by the SDK rather than duplicating those offsets by hand in the new builder.

6. Verification commands pass locally:
   - `bun run --cwd packages/loyal-actions test`
   - `bun run --cwd packages/loyal-actions typecheck`
   - `bun run hub:mainnet-test -- --help`

7. No plaintext secrets are written or printed, and no live mainnet transaction is sent. Any blockchain access used during the task is read-only.

Verdict: PASS only if every required condition is true.

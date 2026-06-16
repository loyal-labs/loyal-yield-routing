# Same-Mint Obligation-Ready Policy Verifier

This document is the fixed success test for the same-mint Earn policy and orchestrator work. Use it as the success target for implementation and review. The implementation can change as measurements and dry-runs teach us more, but the verdict below should stay stable unless it misstates the product goal.

PASS means a vault created through the Earn flow can deposit Main USDC, discover the best Safe USDC KLend reserve, start with that target obligation missing, and execute one atomic same-mint route ordered as protected source withdraw, authorized target `init_obligation`, target refresh/farm setup, and protected target deposit. The proof must cover the two-policy lifecycle now used by Earn: one route policy for same-mint deposit/withdraw and one setup policy for `init_obligation`. The same proof must also cover top-up and full withdrawal cleanup.

FAIL if the source withdrawal and target deposit are not in the same submitted transaction when the target obligation starts missing. The route may initialize the target obligation between withdraw and deposit, but a standalone source withdrawal before an authorized init/deposit path is built is FAIL.

## Policy Shape

Start by measuring the real generated policy and transaction shape. The required design is two policies with distinct accounts and seeds:

- Route policy: exactly the same-mint KLend withdraw constraint and the same-mint KLend deposit constraint. Same-mint route execution must use only the withdraw/deposit constraint indexes.
- Setup policy: a narrow KLend `init_obligation` constraint scoped to approved Safe USDC markets for the vault.

`refresh_obligation` must not be embedded in either protected policy; reserve and obligation refreshes remain public pre-instructions. The verifier must decode both policy accounts and prove the setup constraint can initialize only approved Safe USDC market obligations for the vault. It must also prove the route policy does not include init during normal withdraw/deposit execution.

Measure route policy account size, setup policy account size, route policy create packet size, setup policy create packet size, init-obligation execution packet size, same-mint route packet size, full-withdraw cleanup packet size, and lookup-table use.

`route_policies` must record both policies. `managed_vaults.active_policy_id` remains the route policy, while nullable `managed_vaults.setup_policy_id` points at the setup policy. Full withdrawal must close or deactivate both lifecycle artifacts. Replacing `active_policy_id` with the setup policy is FAIL.

## Ownership Boundaries

Reuse existing boundaries. `loyal-actions` owns policy construction and route indexes, plus sizing and decode/detection. Frontend backend prepare/confirm routes own authenticated wallet/settings validation, response metadata, and DB lifecycle records. Neon route-policy and vault rows remain the control-plane record of active policies. The same-mint orchestrator owns chain reconciliation, missing-obligation init ordering, route execution, top-up behavior, and cleanup pickup. `verify-earn-mainnet-flow.ts` is the live driver and should be extended instead of creating a parallel verifier.

Frontend and backend routes must not require `YIELD_ROUTER_KEYPAIR`. Execution-only optimizer paths must not require `SOLANA_TESTING_PK`. `SOLANA_TESTING_PK` may remain in explicit setup/admin or local user-funded flows that are outside autonomous optimizer execution.

## Frontend And Confirm Evidence

Prepared Earn responses for deposits, top-ups, route setup, and withdrawals must expose enough evidence to audit the route without guessing: reserve, market, liquidity mint, obligation PDA, collateral ATA, route policy account, route policy seed/id, optional setup policy account and seed/id, delegated signer, vault settings, vault index, vault pubkey, lookup-table evidence, and packet metrics.

Confirm routes must validate authenticated wallet ownership for the settings/vault, required signatures, confirmation slots, route policy metadata, optional setup policy metadata, lifecycle transitions, and duplicate confirm behavior. A confirm route that can activate policy state for the wrong wallet/settings owner is FAIL. A duplicate confirm that creates conflicting active state is FAIL.

## Missing-Obligation Ordering

The orchestrator proof must begin with the best fresh Safe USDC target reserve missing its vault obligation. It must detect the current reserve and best target from fresh chain/DB evidence, derive the authorized init execution from decoded policy metadata, and build one submitted transaction ordered as withdraw, init obligation into the vault, refresh/farm setup, and deposit. The transaction must be simulated before submit, and the post-confirmation DB state must show value moved only to the chosen target.

The route must not proceed when fresh Safe USDC candidates are unavailable, when no positive APY edge exists, when the destination obligation is missing and no authorized inline init path exists, or when DB and chain state disagree. A success result based on a standalone withdraw without same-transaction init/deposit evidence is FAIL.

## Cleanup

Full withdrawal from the current reserve must prove wallet USDC return, KLend empty-obligation close plus rent refund where applicable, route policy closure or deactivation, setup policy closure or deactivation, inactive DB rows for both policies and the managed vault, zero current reserve positions, and a later fleet poll that ignores the vault.

Any active policy/vault row still discoverable for autonomous execution after cleanup is FAIL. Any setup policy artifact that survives cleanup as active/discoverable state is FAIL.

## Static Proof Before Live E2E

Before the live proof, run focused checks for touched surfaces. Required local evidence includes targeted Rust tests/typechecks for `loyal-actions`, Squads policy helpers, and same-mint orchestrator changes; targeted TypeScript tests/typechecks/build for SDK and frontend backend surfaces touched by this work; policy decode proof showing withdraw/deposit/init allowed while refresh is absent; packet/account metric proof for policy create/update, init execution, route execution, and full withdrawal; and Slop Guard on this verifier document.

Do not get stuck broadening tests before the main behavior works. Finish the real policy/orchestrator/frontend lifecycle first, then update focused tests to pin the chosen shape, packet/ALT evidence, policy decode/detection, confirm idempotency, signer boundaries, and cleanup.

## Live Proof

The live run must prepare and confirm route policy plus setup policy plus Main USDC deposit, prove the best-target Safe USDC obligation is initially missing, let the orchestrator build and submit the inline withdraw/init/deposit transaction, verify final holding in the chosen best reserve, perform a top-up that reuses both policies, then perform full withdrawal and cleanup. A later fleet poll must show the cleaned-up vault is ignored.

The live proof must show both policies are recorded, setup is used only for the inline init step, both policies are closed or deactivated, and both DB rows are inactive.

## Failure Cases To Pin

After the main path works, focused tests should cover refresh embedded in the protected policy, missing destination obligation with no authorized setup policy init path, setup policy replacing `active_policy_id`, withdrawal before target init confirmation, stale DB/chain disagreement, duplicate confirms, no fresh Safe USDC candidates, no positive APY edge, and inactive-vault discovery after cleanup.

## Final Report

The final report must include route/setup policy account and packet measurements, policy decode evidence, static/local command results, the live E2E command and key output, the inline init policy source and constraint index, the route signature and slot, top-up evidence, full-withdraw cleanup evidence, and fleet-poll evidence showing the cleaned-up vault is ignored.

Overall verdict is PASS only when every required section above has direct current evidence.

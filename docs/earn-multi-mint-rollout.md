# Earn multi-mint rollout and rollback

This runbook covers manual deposits, holdings, withdrawals, earnings, and
same-mint routing for CASH, USDG, PYUSD, USDC, USDT, and USDS. It does not
enable non-USDC autodeposit, cross-mint swaps, or automatic changes to existing
user policies.

## Controls

The app uses `NEXT_PUBLIC_EARN_ENABLED_STABLECOINS`, a comma-separated symbol
allowlist. Missing or blank configuration means `USDC` only. The same resolved
list gates the selector and every web/mobile manual-deposit prepare endpoint.
It does not hide holdings or block withdrawals.

The router uses `EARN_ROUTER_ENABLED_STABLE_MINTS`, a comma-separated mint
address allowlist. Keep the app and router lists aligned by asset, but deploy
them independently: app controls new deposits; router controls opportunity
publication and execution.

Invalid or duplicate app values are deployment errors. Never use an empty
string to mean all assets.

## Dark deployment

1. Deploy upgraded routing readers/executor with its allowlist containing USDC
   only. Do not add `--execute` or change existing execution posture.
2. Deploy the app backend and frontend with
   `NEXT_PUBLIC_EARN_ENABLED_STABLECOINS=USDC`.
3. Run the read-only app report with production read credentials:

   ```sh
   op run --env-file=.env.1password -- sh -c \
     'bun run --cwd frontend verify:earn-multi-mint-readiness'
   ```

4. Archive the JSON artifact. For every asset intended for the next stage,
   require an eligible Safe reserve, fresh APY/history, and matching on-chain
   owner, market, mint, and token program.
5. Check existing USDC deposit, holdings, withdrawal, earnings, and autodeposit
   behavior before enabling another asset.

## Per-asset enablement

Enable one asset at a time in this order:

1. USDC regression control
2. USDT
3. USDS
4. PYUSD
5. USDG
6. CASH

For each additional asset:

1. Add its mint to the router allowlist only after upgraded readers and the
   executor are live. Keep execution posture unchanged during observation.
2. Confirm a dry-run opportunity retains the same source/target mint and token
   program.
3. Add its symbol to the app allowlist and deploy the new frontend bundle.
4. Use a fresh test user whose new policy has the six-mint-compatible shape.
   Do not modify an existing user's policy.
5. With separate value-movement approval, simulate and submit a bounded
   deposit. Record submitted and finalized signatures plus before/after wallet,
   idle, and reserve balances.
6. Verify app reconciliation, the exact holding/source ID, earnings/APY
   coverage, partial withdrawal, selected-source Max, and final all-source-zero
   cleanup.
7. Observe one normal monitoring window before proceeding to the next asset.

Legacy classic-token policies may continue to deposit USDC, USDT, and USDS.
Legacy Token-2022 deposit attempts must show `earn_policy_update_required`.
Users must retain holdings and withdrawal access in either case.

## Rollback

Rollback is per asset and never edits a user policy:

1. Remove the asset symbol from `NEXT_PUBLIC_EARN_ENABLED_STABLECOINS` and
   redeploy the app. This disables new manual deposits while leaving holdings
   and withdrawals visible.
2. Remove its mint from `EARN_ROUTER_ENABLED_STABLE_MINTS` and redeploy or
   restart the router according to the normal pinned-image process. This stops
   new opportunities for that mint; do not delete snapshots or decisions.
3. Keep reconciliation/read services running and verify affected users can
   still withdraw each exact source.
4. Capture the first failing signature or request ID, snapshot generation,
   selected source/reserve, mint, token program, and reconciliation state.
5. Do not broaden another mint or retry value movement until the failure is
   classified and the read model agrees with finalized chain state.

## Evidence states

Track these independently:

- `codeVerified`: frozen local verifier R0-R7 passed for the reviewed commits.
- `dataReady`: read-only production report is clean for the enabled asset.
- `deployed`: immutable app and worker revisions are confirmed live.
- `canaryFinalized`: signatures and before/after balance evidence are finalized
  and reconciled.
- `userReady`: all previous states are true and monitoring/rollback are live.

A passing build, simulation, submitted signature, or healthy worker is not a
substitute for the next state.

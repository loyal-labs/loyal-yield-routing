# Earn Multi-Mint Dark-Readiness Results

Verified on 2026-08-11 against:

- app code tree `6f918d0b74e6ea1066478005db7946bca4f71321`
- routing code tree `491e3a0644522b9423c0837fd65e31baf4e32591`
- branch `codex/earn-multi-mint-simple` in both repositories

No service was deployed, no external environment was changed, no transaction
was signed, and no on-chain policy was created, updated, or migrated.

## Verdict

| Gate | Result | Evidence |
| --- | --- | --- |
| D0 exact scope | PASS | The product registry is exactly CASH, USDG, PYUSD, USDC, USDT, and USDS. No migration, worker topology, deployment configuration, generated ABI, or autodeposit endpoint changed. Both branches are current with fetched `origin/main`; app is two commits ahead and routing is three commits ahead. `git diff --check` passed. |
| D1 app dark deposits | PASS | Missing/blank app configuration resolves to USDC only; malformed and duplicate values fail; explicit subsets are canonical. All three manual-deposit prepare paths call `resolveEnabledEarnProductAsset` before policy, reserve, or transaction work. Holdings and exact-source withdrawals use the complete registry independently. |
| D2 router dark activity | PASS | Missing/blank router configuration resolves to the USDC mint only. Focused Rust tests cover missing, blank, unsupported, duplicate, empty-interior, subset, and explicit all-six values. Planner/monitor/revalidation use the runtime gate. Already-signed reconciliation runs first without the gate. The shared catalog deliberately keeps all-six exit coverage without enabling routes. |
| D3 policy and money safety | PASS | Focused app tests cover legacy SPL compatibility, typed Token-2022 policy updates, reserve identity, exact source intent, partial/max isolation, complete zero proof, and multi-source APY/earnings behavior. No existing policy was changed. |
| D4 integration and regressions | PASS | App production build, smart-account typecheck, 119 focused app checks, `cargo fmt --check`, `cargo check --workspace`, `bun run test:squads`, and `cargo test --workspace` passed. Routing was merged with current `origin/main` on the feature branch before the final run. See baseline notes below. |
| D5 dark artifact | PASS | The signerless app artifact reports only USDC enabled with missing configuration; explicit all-six marks all six enabled but leaves `deployed`, `canaryFinalized`, and `userReady` false; duplicate configuration exits nonzero. Router unit tests prove the matching USDC-only default. |

`DARK_CODE_READY`: **PASS**

`USER_READY`: **false**

Production deployment, live data readiness, canary submission/finalization, and
production reconciliation: **NOT RUN**.

## Defect found and repaired

The first cold run found that `resolve_enabled_stable_mints(None)` enabled all
six routing mints. Because the production Blueprint did not set the router
allowlist, a future routing deploy would have made the five new mints active
instead of dark. The resolver now defaults missing or blank configuration to
USDC only and fails closed on unsupported, duplicate, or empty-interior values.

The shared lookup-table catalog remains complete for all six mints. Catalog
membership is address coverage for compilation and safe exits, not permission
to plan or execute. Runtime workers still require the separate rollout gate.

## Commands and observations

App:

- `bun run --cwd packages/smart-account-vaults typecheck` — pass.
- Focused Earn tests — 119 pass, 0 fail across API, UI helper, portfolio,
  policy, reserve, reconciliation, earnings, and intent suites.
- `op run --env-file=/Users/taequn/loyal/loyal-apps/.env.mainnet.1password -- sh -c 'bun run --cwd frontend build'` — pass with pre-existing warnings.
- Signerless readiness with no rollout env — only USDC has
  `depositEnabled: true`; data readiness is `unknown` without explicit RPC and
  Timescale inputs; every release flag remains false.
- Signerless readiness with explicit all-six env — all six have
  `depositEnabled: true`; release flags remain false.
- Duplicate `USDC,USDC` env — exit 1 with a bounded duplicate error.

Routing:

- `cargo fmt --check` — pass.
- `cargo check --workspace` — pass with four existing dead-code warnings in
  `same-mint-reserve-swap`.
- `bun run test:squads` — pass after building the required mock and Loyal Hub
  SBF programs.
- `cargo test --workspace` — pass after the prescribed SBF build; live DB,
  LaserStream, and heavyweight hindsight tests remain ignored by their
  existing explicit gates.

## Baseline exceptions

- Scoped Ultracite cannot start because `frontend/biome.jsonc` contains rules
  unknown to the locked Biome 2.3.2 binary. The same command fails with the
  same configuration errors on current app `main`, before reading a feature
  file.
- The broad `packages/smart-account-vaults/src` test filter reports 48 pass and
  11 fail on the feature branch. Current app `main` reports 45 pass and the
  identical 11 named failures. The feature adds three passing policy tests and
  no new failure. Focused feature tests and package typecheck pass.
- The first blanket routing test attempt lacked the mock SBF fixture and failed
  two Squads tests at setup. Running the repository-prescribed SBF build fixed
  the setup; both the Squads gate and the subsequent full workspace suite pass.

These baseline exceptions do not enable a mint, authorize production use, or
weaken the money-path assertions above.

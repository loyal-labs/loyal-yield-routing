# Earn Multi-Mint Dark-Readiness Verifier

Status: frozen before dark-readiness repair on 2026-08-11. Amended twice after
the first verifier run: shared catalog coverage is explicitly not runtime
enablement and must remain complete for safe exits; already-signed submission
reconciliation is explicitly independent from new-route eligibility.

Run cold against:

- app worktree `/private/tmp/loyal-apps-earn-multi-mint-simple`
- routing worktree `/private/tmp/loyal-yield-routing-earn-multi-mint-simple`
- branch `codex/earn-multi-mint-simple` in both repositories

The goal is `DARK_CODE_READY`, not production deployment or `USER_READY`.
Nothing in this verifier merges `main`, deploys a service, changes an external
environment variable, signs a transaction, or mutates an on-chain policy.

## D0 — Exact scope and branch state

Required:

- The supported product universe is exactly CASH, USDG, PYUSD, USDC, USDT,
  and USDS.
- Routing remains same-mint; autodeposit remains USDC-only; lifecycle states,
  schemas, worker topology, and existing on-chain policies do not change.
- Changes stay on `codex/earn-multi-mint-simple` and `git diff --check` passes.
- No build output, secret, deployment configuration, or generated ABI is
  tracked.

## D1 — App deposits are dark by default

Required:

- Missing or blank `NEXT_PUBLIC_EARN_ENABLED_STABLECOINS` resolves to USDC
  only.
- Unsupported, empty interior, or duplicate entries fail closed.
- An explicit subset enables only that canonical subset; all six require an
  explicit all-six value.
- The same resolved list drives the selector and every web/mobile manual
  deposit prepare path.
- A disabled-mint request fails before reserve lookup, policy reads, or
  transaction construction.
- Holdings and exact-source withdrawals resolve against the complete product
  registry, not the deposit rollout subset.

## D2 — Router activity is dark by default

Required:

- Missing or blank `EARN_ROUTER_ENABLED_STABLE_MINTS` resolves to mainnet USDC
  only, never all six.
- Unsupported or duplicate mint values fail closed.
- An explicit supported subset is returned in canonical order; all six require
  an explicit all-six value.
- The planner, monitor, revalidator, and executor consume the same resolved
  routing list for new-route eligibility. Periodic optimizer position discovery
  may use that same subset.
- Already-signed submission reconciliation runs before and independently from
  the rollout list, so disabling a mint stops new work without abandoning a
  submitted transaction. Full holdings and withdrawals remain driven by the
  app's complete registry and fresh RPC state, not the routing rollout list.
- The shared lookup-table catalog may default to the full six-mint address
  universe because catalog coverage is exit infrastructure, not execution
  eligibility. Catalog membership must never enable planning or execution.
- Readiness/evidence collection may assess all six mints, but must not claim
  that an omitted runtime allowlist enables all six.

Focused Rust tests must prove missing, blank, duplicate, unsupported, subset,
and explicit all-six behavior.

## D3 — Policy and money safety

Required:

- New policies have the six-mint-compatible shape without changing old
  policies.
- Legacy policies still support classic SPL Token assets and return typed HTTP
  409 `earn_policy_update_required` for Token-2022 deposits.
- Reserve owner, market, liquidity mint, and token program are checked before
  instruction construction.
- Deposit intent remains `{mint, amountRaw}` and withdrawal intent remains
  `{sourceId, amountRaw | "max"}`. Partial/Max never drain another source.
- Complete fresh RPC holdings and all-source zero proof remain the cleanup
  authority; APY gaps remain unknown rather than borrowed from another source.

## D4 — Current-main integration and regression evidence

Required:

- Fetch current `origin/main` in both repositories and record ahead/behind
  state.
- App applies cleanly to current main. Routing either applies cleanly or is
  refreshed on the feature branch; no unresolved merge conflict is allowed.
- App production build passes using the documented 1Password environment.
- Smart-account package typecheck and focused Earn tests pass.
- Routing format, check, and tests pass.
- New files pass scoped Ultracite; pre-existing suite failures are accepted
  only when identical at base/current main and unrelated to this feature.

## D5 — Dark-state artifact

Required:

- The signerless readiness command reports only USDC `depositEnabled: true`
  when the app allowlist is absent.
- Explicit all-six app configuration marks all six enabled without changing
  `deployed`, `canaryFinalized`, or `userReady` from false.
- Invalid app configuration exits nonzero.
- A routing unit/CLI-level check proves an absent or blank runtime allowlist
  yields exactly the USDC mint.
- Documentation says both app and routing default to USDC-only and describes
  explicit, independent per-mint enablement.

## Verdict

Report D0-D5 individually as `PASS`, `FAIL`, or `NOT RUN`.

`DARK_CODE_READY` is `PASS` only if D0-D5 all pass. `USER_READY` must remain
false and production rollout evidence must remain `NOT RUN` until separately
authorized.

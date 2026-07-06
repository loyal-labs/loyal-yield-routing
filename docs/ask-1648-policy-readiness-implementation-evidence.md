# ASK-1648 Policy Readiness Implementation Evidence

Recorded: 2026-07-06

Branch: `ASK-1648-hub-policy-ingestion-readiness`

Scope: local verification for PR 1, `feat(yield-routing): persist Loyal Hub
route readiness`.

## What Changed

- Added canonical route-mode helpers for `same_mint_kamino` and
  `cross_mint_loyal_hub`.
- Normalized new route-policy rows so incoming legacy `same_mint` is stored as
  `same_mint_kamino`.
- Kept same-mint monitor and executor read paths tolerant of historical
  `same_mint` rows.
- Enriched policy-monitor `swap_lanes` persistence with action account and
  withdraw/swap/deposit instruction constraint indexes.
- Added read-only Hub readiness helpers that parse both monitor-style
  `kind = loyal_hub` and existing setup-style `lane = loyal_hub` JSON.

## Verification

- `NO_DNA=1 cargo fmt --check`: passed.
- `NO_DNA=1 cargo test -p loyal-yield-orchestrator route_mode_normalization --lib`: passed.
- `NO_DNA=1 cargo test -p loyal-yield-orchestrator hub_readiness --lib`: passed.
- `NO_DNA=1 cargo test -p loyal-squads-policy-monitor policy_event_persists_hub_lane_route_metadata --lib`: passed.
- `NO_DNA=1 cargo check -p loyal-squads-policy-monitor`: passed.
- `NO_DNA=1 cargo check -p loyal-yield-orchestrator --bin same-mint-yield-monitor --bin same-mint-reserve-swap`: passed with the existing `same-mint-reserve-swap` dead-code warning for `SelectedVault.swap_lanes`.

## Non-Goals Confirmed

This branch does not add a cross-mint planner, Hub route executor, staging Render
service, DB operator write, Hub admin binding, or live transaction path.

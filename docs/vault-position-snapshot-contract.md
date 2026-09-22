# Vault position snapshot storage contract

Shared rules for every service that reads or writes `loyal_yield.vault_position_snapshots`,
`loyal_yield.vault_position_snapshot_positions`, `loyal_yield.vault_reserve_positions_current`,
and `loyal_yield.vault_idle_token_balances_current`. The Rust fleet workers, the Go
`kamino-fleet-planner`, and `apps/web` in `loyal-app` all bind to these tables. A change to
one writer or reader that breaks a rule here breaks the others; check every row of the
tables below before changing cadence, columns, or retention.

Background: on 2026-09-21 the positions history table was 774 GB, 96% of it
`amount_raw = 0` rows written by the 5-minute fleet sweep. Phase 1
(loyal-yield-routing#259, loyal-app#785) stopped writing those rows; Phase 2 rewrote the
table to funded rows only.

## Rules

1. **History holds funded rows only.** `vault_position_snapshot_positions` contains a row
   for a `(snapshot_id, reserve)` pair only when `amount_raw > 0`. The table has
   `CHECK (amount_raw > 0 AND has_value)`. A reserve with no row in a snapshot is zero.
   Readers must treat absence as zero; they must not require a row per reserve.
2. **Current state holds the full reserve set.** `vault_reserve_positions_current` keeps one
   row per `(vault_id, reserve)` in the policy universe, including zero rows, and is the only
   place a reader may enumerate "which reserves were checked".
3. **`observed_at` on the current row is a liveness signal.** Every sweep updates it even
   when nothing else changed. The Go planner's `LoadVaultPosition` marks a row
   `stale_vault_position` past 5 minutes and skips the vault; the sweep interval is 300 s
   (`DEFAULT_FLEET_POSITION_SWEEP_INTERVAL_SECONDS`). Lengthening the sweep, or skipping the
   current-row update on unchanged sweeps, requires changing that gate in the same release.
4. **A snapshot row is immutable once written.** `observed_slot` and `observed_at` on
   `vault_position_snapshots` are earnings-interval boundaries in `apps/web`
   (`findCompleteYieldVaultExposureSnapshots`, `earnings-calculator.server.ts`) and are read
   back through `rebalance_decisions.post_snapshot_id`. Never update them in place.
5. **Retention must respect every FK.** Six columns reference `vault_position_snapshots(id)`:
   `rebalance_decisions.source_snapshot_id` and `post_snapshot_id`,
   `rebalance_opportunities.source_snapshot_id`, `signed_route_submissions.source_snapshot_id`,
   `user_yield_position_holding_events.source_snapshot_id`,
   `vault_reserve_positions_current.snapshot_id` (all NO ACTION), and
   `vault_position_snapshot_positions.snapshot_id` (CASCADE). A snapshot referenced by any of
   them, or marked `is_current`, cannot be deleted. Earnings for the `ALL` range also need
   every snapshot where a funded exposure changed, so retention must be change-point based,
   not age based.
6. **`planning_metadata` keys are a wire format.** The Rust sweep writes snake_case
   (`amount_semantics`, `redeemable_source_liquidity_amount_raw`,
   `redeemable_liquidity_amount_raw`, `source_collateral_amount_raw`,
   `idle_vault_liquidity_amount_raw`). The Go planner decodes exactly those keys and rejects
   a row whose `amount_semantics` is not `kamino_obligation_collateral_deposited_amount` or
   `redeemable_liquidity_amount`. `apps/web` writes camelCase (`amountSemantics`,
   `measuredAmountRaw`) on its own rows; those rows are `frontend_*` / `user_yield_positions`
   sources and are not planner inputs. Any query over semantics must read both spellings.

## Who touches what

| Service | Table | Access | Depends on rule |
| --- | --- | --- | --- |
| Rust `same-mint-reserve-swap --fleet-reconciler` (sweep) | snapshots, positions, current, idle | write | 1, 2, 3, 6 |
| Rust `reconcile_vault_transaction_guarded`, `confirm_same_mint_rebalance_guarded`, `apply_earn_observed_balances` | positions | write, `amount_raw > 0` only | 1 |
| Rust fleet-worker expiry check (`lib.rs` `SELECT ... WHERE snapshot_id = $1 AND reserve IN`) | positions | read | 1 (target row may be absent) |
| Rust `fleet-orchestration-production-evidence` | positions | read, LEFT JOIN + COALESCE | 1 |
| Rust `fleet-opportunity-planner` | current, idle | read | 2 |
| Go `kamino-fleet-planner` `LoadVaultPosition`, `LoadFleet` | current (`has_value AND amount_raw > 0`) | read | 2, 3, 6 |
| Go `kamino-fleet-planner` `idle_shadow.go` | idle | read | 2 |
| Go `kamino-fleet-planner` publish | `rebalance_opportunities.source_snapshot_id` | write (copies `current.snapshot_id`) | 5 |
| `apps/web` `yield-deposit-repository.server.ts` (two writers) | snapshots, positions, current, idle | write, `amountRaw > 0` only | 1, 2, 4, 6 |
| `apps/web` `findCompleteYieldVaultExposureSnapshots` | snapshots (all rows for a vault), positions (`has_value`) | read | 1, 4, 5 |
| `apps/web` `syncConfirmedRebalanceHoldingEventsForVault` | snapshots via `post_snapshot_id`, positions `amount_raw > 0` | read | 4, 5 |

Go does not read the two history tables at all. It is therefore unaffected by Phase 1 and
Phase 2, and affected by any change to rule 3 or rule 6.

## Promoting the Go planner

Before `KAMINO_FLEET_MODE=publish`, confirm the sweep still updates
`vault_reserve_positions_current.observed_at` at least every 5 minutes for every active
vault, or relax the 5-minute gate in `store.go` in the same release. Measured on 2026-09-22:
2,031 of 2,076 funded rows were under 5 minutes old at any instant; the rest are vaults
whose sweep task was retrying.

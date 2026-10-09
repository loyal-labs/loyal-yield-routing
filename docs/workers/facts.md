# Worker facts: where each truth lives and who writes it

The Go workers are being rebuilt around one rule: **every fact has one source of truth and one writer.** Where today's schema keeps a second copy, a lease or a reconciler so that two writers can tolerate each other, that machinery is deleted once the fact has a single home.

This file is the contract. A worker PR must agree with it, or change it first.

The inventory behind it was taken from main @4d3fbf97 plus loyal-app main @2ac3dc2d. It covers 97 relations (91 tables, 6 views) after migrations 0093–0097 and timescale 0009.

## Rules

1. **Chain facts are read from the chain when a decision needs them.** These are balances, Kamino reserves and obligations, Squads policies, ALT contents, Voltr vault state and transaction receipts. A stored copy is only a *projection*, used for history, UI or enumeration. It has one writer, the observer, which upserts by slot, and no executor plans from it.
2. **An operation is one row, written by the family that owns it.** The row holds the intent, the plan, the signed wire and its hash, the landing state and the receipt. The family's advisory lock (`HoldFamily`) is the only concurrency control. Row leases, fencing tokens and claim columns go.
3. **User intent is written only by the user's action.** That covers opt-ins, toggles and floors, written by the app route. Chain state never shares a row with intent; such rows are split.
4. **Limits live on chain.** Per-transaction and per-window caps are the Squads spending limit, and fee payers hold only what they may spend. Go does not keep a second ledger of the same limit.
5. **No trigger writes another table**, except to emit `realtime_events`, which is the trigger's own declared output.
6. **A writer is deleted in the same PR that moves its readers to the true source**, never before. Deleting fleetexec's position writes while the planner still reads `vault_reserve_positions_current` would let the planner act on stale holdings.

## Facts

### Chain facts

| Fact | Truth | Projection kept (one writer: observer) | Deleted once readers read the chain | Decision readers to move |
|---|---|---|---|---|
| Kamino reserve state | reserve accounts | `kamino.reserve_updates` (history; dedupe by PK `(reserve, slot)`), `kamino.supported_reserves` (catalog), view `kamino.latest_reserve_updates` | `kamino.reserve_confirmed_verifications`, `reserve_confirmed_observation_floors`, `reserve_current_states`, `reserve_update_dedupe`, view `latest_verified_reserve_updates`: verification layers that exist because planners read the DB | `fleet/market_epoch.go:172,200`, `backyard/selector_feed.go:32` |
| Vault holdings (per-reserve, idle) | obligations, cToken and vault token accounts | `vault_reserve_positions_current`, `vault_idle_token_balances_current` | `vault_position_snapshots`, `vault_position_snapshot_positions` (a second shape of the same holdings); writes from `fleetexec/reconciliation.go:355-380`, `fleetexec/position_sweep_store.go:193-241`, app `/position/reconcile` | `fleet/store.go:159,282`, `autodeposit/reserve.go:181`, `autodeposit/withdrawn.go:45` |
| Wallet token balances | ATAs | `loyal_prod.balance_sweep_wallet_ata_observations` (dedupe by PK), `balance_sweep_wallet_balances_current`, `balance_sweep_wallet_balance_events` | `loyal_prod.balance_sweep_wallet_ata_observation_dedupe`, view `latest_balance_sweep_wallet_ata_observations` (no reader); engine and app writes to balances and events | `autodeposit/queue.go:241`, `claims.go:259,332` (the execution path already re-reads the wallet) |
| Managed vaults and their Squads policies | Squads settings and policy accounts | `managed_vaults`, `route_policies`, `earn_max_policy_sets`, `cross_mint_swap_policies` (enumeration and UI) | app writes to `managed_vaults` and `route_policies`; `balance_sweep_policies` (a legacy app mirror of the same policies, no inserts) | executors read the policy accounts of the vaults they act on: `fleet`, `fleetexec/cross_mint_store.go:1121`, `multiply/store.go:202,240,382,988,1041` |
| Transactions users sign (deposits, withdrawals, refunds) | the transaction | `user_yield_position_deposits`, `user_yield_position_withdrawals`, `earn_chain_refund_events` | `earn_chain_mutations` (written, never read) | — |
| Kamino Multiply position value | obligation | `multiply_position_snapshots` (history) | engine and backyard writes to it | `multiply/store.go:395` |

### Our own operations (one row per intent, written by the owning family)

| Family | Today: tables for one intent | Target | Deleted |
|---|---|---|---|
| Rebalance (retail) | `optimizer_epochs` → `rebalance_opportunities` → `rebalance_decisions` → `signed_route_submissions`, linked by 4 triggers, with leases on each | one operation row per rebalance; the plan reads reserves and vaults from the chain at plan time | `active_rebalance_opportunity_slots`, `route_account_conflict_leases`, `target_capacity_frontiers`, `target_capacity_reservations` (headroom = reserve on chain minus the family's open operations), `fleet_planning_clusters`, `fleet_orchestration_health_snapshots` (go to `/metrics`), `orchestration_outbox` (dead), view `fleet_orchestration_status`, and the triggers `signed_route_submission_finishes_terminal_state`, `signed_route_submission_advances_target_capacity`, `rebalance_decision_links_execute_opportunity`, `rebalance_opportunity_active_slot` |
| Fee payers | `route_fee_payer_shards` (no code writer), `route_fee_payer_spend_reservations`, view `route_fee_payer_shard_status` | a fee payer holds only what it may spend (rule 4) | all three |
| Lookup tables | 21 `lookup_table_*` tables and `route_lookup_tables`: manifests, revisions, heads, drifts, bindings, usage leases, budget reservations, broadcast permits, controls | the ALT account is the truth (addresses, `last_extended_slot`, deactivation slot); one registry row per ALT; create, extend and close are operation rows | everything except the registry, including `lookup_table_legacy_cleanup_*` and `lookup_table_terminal_repair_operations` (read, never written) and the triggers `lookup_table_request_consumer_priority` and `lookup_table_vault_bindings_reservation_accounting` |
| Autodeposit | `balance_sweep_scheduled_slots`, `balance_sweep_lot_claims` and their claim items, `balance_sweep_transaction_attempts`, `balance_sweep_executions`, `balance_sweep_execution_lots` (never read), `autodeposit_reconciliation_requests` (an observer→engine handoff queue; `EnqueueAutodepositReconciliationRequest` has no caller) | lots stay an observer projection of arrivals; one operation row per pull, written by the engine | claims and claim items, attempts, `execution_lots`, `reconciliation_requests`, the SQL function `finalize_confirmed_autodeposit`'s writes to positions, and autodeposit's `projection_offsets` writes |
| Earn Max and Backyard | `multiply_operations` and `multiply_route_states`, written by retail multiply, backyard and the observer, with leases and fencing | operation rows and route state per family under its lock; the observer writes none | the observer's writes (`observer/earn/app.go:177`, `earnmax_cashflow.go:478`, `monitor.go:448`) and the lease and fencing columns |
| Cross-mint | `cross_mint_no_effect_receipts` (proof that a leg had no effect, so a replacement can be signed) | landing already answers this: a wire past its blockhash expiry without a signature never landed | the no-effect receipts |

### User intent (written only by the app route that records the choice)

| Fact | Table | Change |
|---|---|---|
| Autodeposit on/off and wallet floor | `balance_sweep_targets.desired_active`, `wallet_balance_floor_raw` | split from the chain columns (`chain_status`, accounts, signatures), which are read from the chain or projected by the observer; the app writes `chain_status` at `earn-autodeposit-repository.server.ts:2167,2624` today |
| Cross-mint opt-in | `cross_mint_vault_opt_ins` | the observer's write is deleted |
| Cross-mint kill switch | `cross_mint_movement_controls` | an operator config row; kept |
| Backyard manual-recovery latch | `backyard_manual_recovery_latches` | an operator halt; kept |

### Projections (one writer each; already true unless noted)

These need no change:
- `earn_apy_hourly_snapshots`, `earn_forecast_snapshots`, `earn_fleet_allocations_hourly`, `earn_reserve_share_prices`: written by the observer.
- `realtime_events`: written by triggers (rule 5); `realtime_configuration` is the config those triggers read.

These change:
- **`user_yield_positions` and `user_yield_position_holding_events`:** the observer becomes the only writer. Deleted are writes from `autodeposit/reserve.go:147`, `fleetexec/position_sweep_store.go:279,287`, `finalize_confirmed_autodeposit` and app routes.
- **`earn_reconciliation_jobs`:** today an observer queue with claim leases, retries and dead-lettering. It is deleted; the observer applies stream events idempotently by slot.

### Infrastructure (kept)

- `loyal_yield.schema_migrations` and `loyal.timescale_schema_migrations`: the migration ledgers.
- `laserstream_replay_cursors` and `projection_offsets`: observer only.
- `push_campaign_sends`: app only.

### App-only tables (out of scope until the app moves)

`solana_week_quest_completions`, `earn_earnings_snapshots` (a cached payload), `earn_deposit_onboarding_attempts` (no live insert).

## Order

Deletions that change no reader land first:
- dead relations and their readers' dead branches;
- written-never-read tables;
- uncalled writers (`EnqueueAutodepositReconciliationRequest`).

Every other row above lands with the family that reads it, in the family's PR (observer → lookup tables → multiply → retail → backyard → cross-mint). That PR moves the decision readers to the chain, deletes the duplicate writers, and drops the tables in a delete-only migration.

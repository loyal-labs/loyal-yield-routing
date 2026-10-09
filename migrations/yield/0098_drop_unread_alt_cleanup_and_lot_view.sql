-- Objects with no live writer or reader.
--
-- pending_balance_sweep_surplus_lots (0007) has no reader in the workers, the
-- yield scripts or loyal-app; autodeposit reads balance_sweep_surplus_lots.
DROP VIEW IF EXISTS loyal_yield.pending_balance_sweep_surplus_lots;

-- Legacy ALT cleanup attempts and their budget reservations (0021) were
-- written only by the retired Rust cleanup CLI. The Go ALT budget no longer
-- counts them. Dropping a table drops its own triggers; reservations
-- reference attempts, so they go first.
DROP TABLE IF EXISTS loyal_yield.lookup_table_legacy_cleanup_budget_reservations;
DROP TABLE IF EXISTS loyal_yield.lookup_table_legacy_cleanup_attempts;
DROP FUNCTION IF EXISTS loyal_yield.guard_lookup_table_legacy_cleanup_budget_mutation();
DROP FUNCTION IF EXISTS loyal_yield.guard_lookup_table_legacy_cleanup_attempt_budget();

-- Reusable ALT control-plane tables that only the retired Rust services read
-- or wrote: alerting (0021), the route readiness projection (0017), the
-- legacy-table import audit (0019), pre-cutover probes (0021/0022), terminal
-- repair parents (0028) and rollout controls (0017). The Go engine keeps
-- lookup_table_terminal_repair_operations, which it reads to skip repaired
-- permanent failures; it loses only its foreign key to the dropped parent.

-- route_lookup_tables keeps legacy_kind and legacy_import_run_id (its check
-- constraint and guard_retired_legacy_lookup_table_reference read them). Its
-- update guard looked up the import evidence on every update of an imported
-- row, so it goes with the evidence.
DROP TRIGGER IF EXISTS route_lookup_tables_legacy_kind_immutable
    ON loyal_yield.route_lookup_tables;
DROP FUNCTION IF EXISTS loyal_yield.guard_lookup_table_legacy_registry_update();
ALTER TABLE loyal_yield.route_lookup_tables
    DROP CONSTRAINT IF EXISTS route_lookup_tables_legacy_import_run_id_fkey;

ALTER TABLE loyal_yield.lookup_table_terminal_repair_operations
    DROP CONSTRAINT IF EXISTS lookup_table_terminal_repair_operations_repair_id_fkey;

-- Dropping a table drops its own triggers. Children before parents.
DROP TABLE IF EXISTS loyal_yield.lookup_table_alert_deliveries;
DROP TABLE IF EXISTS loyal_yield.lookup_table_alert_incidents;
DROP TABLE IF EXISTS loyal_yield.lookup_table_alert_rules;

DROP TABLE IF EXISTS loyal_yield.lookup_table_route_readiness_current;

DROP TABLE IF EXISTS loyal_yield.lookup_table_legacy_import_evidence;
DROP TABLE IF EXISTS loyal_yield.lookup_table_legacy_import_runs;

DROP TABLE IF EXISTS loyal_yield.lookup_table_precutover_probe_shared_tables;
DROP TABLE IF EXISTS loyal_yield.lookup_table_precutover_probe_runs;

DROP TABLE IF EXISTS loyal_yield.lookup_table_terminal_repair_bindings;
DROP TABLE IF EXISTS loyal_yield.lookup_table_terminal_repair_requests;
DROP TABLE IF EXISTS loyal_yield.lookup_table_terminal_repairs;

DROP TABLE IF EXISTS loyal_yield.lookup_table_rollout_controls;

-- Trigger functions left without a trigger. reject_lookup_table_terminal_repair_mutation
-- stays: it still guards lookup_table_terminal_repair_operations.
DROP FUNCTION IF EXISTS loyal_yield.guard_lookup_table_alert_rule_mutation();
DROP FUNCTION IF EXISTS loyal_yield.guard_lookup_table_alert_incident_mutation();
DROP FUNCTION IF EXISTS loyal_yield.guard_lookup_table_legacy_import_evidence_insert();
DROP FUNCTION IF EXISTS loyal_yield.guard_lookup_table_legacy_import_audit_mutation();
DROP FUNCTION IF EXISTS loyal_yield.guard_lookup_table_precutover_probe_run_mutation();
DROP FUNCTION IF EXISTS loyal_yield.guard_lookup_table_precutover_probe_shared_table_mutation();
DROP FUNCTION IF EXISTS loyal_yield.guard_lookup_table_precutover_probe_bundle_consistency();
DROP FUNCTION IF EXISTS loyal_yield.guard_rollout_during_legacy_cleanup();
-- Only the pre-cutover bundle guard and 0022's backfill called it.
DROP FUNCTION IF EXISTS loyal_yield.hash_length_prefixed_text(TEXT[]);

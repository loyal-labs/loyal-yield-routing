-- Read-only contract used by both migration entrypoints and release verification.
WITH expected_columns(name, data_type, required) AS (
    VALUES ('cluster', 'text', true),
           ('observed_hour', 'timestamp with time zone', true),
           ('observed_at', 'timestamp with time zone', true),
           ('reserve_amounts', 'jsonb', true),
           ('idle_amount_raw', 'numeric(39,0)', true),
           ('vaults_total', 'integer', true),
           ('vaults_included', 'integer', true),
           ('vaults_missing', 'integer', true),
           ('vaults_invalid', 'integer', true),
           ('vaults_stale', 'integer', true),
           ('excluded_amount_raw', 'numeric(39,0)', true),
           ('oldest_source_at', 'timestamp with time zone', false),
           ('newest_source_at', 'timestamp with time zone', false),
           ('calc_version', 'smallint', true)
), columns_valid AS (
    SELECT count(*) = 14 AS valid
    FROM expected_columns expected
    JOIN pg_attribute attribute
      ON attribute.attrelid = to_regclass('loyal_yield.earn_fleet_allocations_hourly')
     AND attribute.attname = expected.name
     AND NOT attribute.attisdropped
     AND attribute.attnotnull = expected.required
     AND format_type(attribute.atttypid, attribute.atttypmod) = expected.data_type
), primary_key_valid AS (
    SELECT count(*) = 1 AS valid
    FROM pg_index index
    JOIN pg_class relation ON relation.oid = index.indexrelid
    JOIN pg_am method ON method.oid = relation.relam
    WHERE index.indrelid = to_regclass('loyal_yield.earn_fleet_allocations_hourly')
      AND index.indisready AND index.indisvalid AND method.amname = 'btree'
      AND index.indisprimary AND index.indisunique
      AND index.indpred IS NULL AND index.indexprs IS NULL
      AND index.indnatts = index.indnkeyatts AND index.indnkeyatts = 2
      AND pg_get_indexdef(index.indexrelid, 1, true) = 'cluster'
      AND pg_get_indexdef(index.indexrelid, 2, true) = 'observed_hour'
), expected_checks(name, expression) AS (
    VALUES ('earn_fleet_allocations_hourly_reserve_amounts_check',
            '(jsonb_typeof(reserve_amounts) = ''object''::text)'),
           ('earn_fleet_allocations_hourly_idle_amount_raw_check',
            '(idle_amount_raw >= (0)::numeric)'),
           ('earn_fleet_allocations_hourly_excluded_amount_raw_check',
            '(excluded_amount_raw >= (0)::numeric)'),
           ('earn_fleet_allocations_hourly_calc_version_check',
            '(calc_version > 0)'),
           ('earn_fleet_allocations_hourly_vault_counts_check',
            '((vaults_included >= 0) AND (vaults_missing >= 0) AND (vaults_invalid >= 0) AND (vaults_stale >= 0) AND (vaults_total = (((vaults_included + vaults_missing) + vaults_invalid) + vaults_stale)))')
), checks_valid AS (
    -- A check with the right name and a different rule must not pass.
    SELECT count(*) = 5 AS valid
    FROM expected_checks expected
    JOIN pg_constraint check_constraint
      ON check_constraint.conrelid = to_regclass('loyal_yield.earn_fleet_allocations_hourly')
     AND check_constraint.contype = 'c' AND check_constraint.convalidated
     AND check_constraint.conname = expected.name
     AND pg_get_expr(check_constraint.conbin, check_constraint.conrelid) = expected.expression
)
SELECT columns_valid.valid AND primary_key_valid.valid AND checks_valid.valid AS valid
FROM columns_valid CROSS JOIN primary_key_valid CROSS JOIN checks_valid;

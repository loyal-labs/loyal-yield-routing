-- Small durable invalidation records for incremental Earn chart checkpoints.
-- Existing history starts at revision zero; every subsequent mutation is tracked.
SET LOCAL lock_timeout = '5s';

CREATE TABLE loyal_yield.earn_history_day_revisions (
    settings TEXT NOT NULL,
    vault_index SMALLINT NOT NULL,
    history_day DATE NOT NULL,
    revision BIGINT NOT NULL DEFAULT 1,
    PRIMARY KEY (settings, vault_index, history_day)
);

CREATE FUNCTION loyal_yield.bump_earn_history_day()
RETURNS TRIGGER LANGUAGE plpgsql SET search_path = pg_catalog AS $$
DECLARE
    item JSONB;
    old_item JSONB;
    new_item JSONB;
    scope_settings TEXT;
    scope_index SMALLINT;
    affected_at TIMESTAMPTZ;
    affected_day DATE;
    previous_key TEXT;
    current_key TEXT;
BEGIN
    IF TG_OP <> 'INSERT' THEN old_item := to_jsonb(OLD); END IF;
    IF TG_OP <> 'DELETE' THEN new_item := to_jsonb(NEW); END IF;
    IF TG_OP = 'UPDATE' THEN
        IF TG_TABLE_NAME = 'vault_position_snapshots' AND
           (old_item->'vault_id', old_item->'observed_at', old_item->'observed_slot', old_item->'context'->'publication_scope') IS NOT DISTINCT FROM
           (new_item->'vault_id', new_item->'observed_at', new_item->'observed_slot', new_item->'context'->'publication_scope') THEN RETURN NULL; END IF;
        IF TG_TABLE_NAME = 'vault_position_snapshot_positions' AND
           (old_item->'snapshot_id', old_item->'reserve', old_item->'liquidity_mint', old_item->'amount_raw', old_item->'has_value') IS NOT DISTINCT FROM
           (new_item->'snapshot_id', new_item->'reserve', new_item->'liquidity_mint', new_item->'amount_raw', new_item->'has_value') THEN RETURN NULL; END IF;
        IF TG_TABLE_NAME = 'managed_vaults' AND
           (old_item->'settings', old_item->'vault_index', old_item->'vault_pubkey') IS NOT DISTINCT FROM
           (new_item->'settings', new_item->'vault_index', new_item->'vault_pubkey') THEN RETURN NULL; END IF;
        IF TG_TABLE_NAME = 'user_yield_positions' AND
           (old_item->'settings', old_item->'vault_index', old_item->'vault_pubkey', old_item->'initial_reserve', old_item->'initial_market', old_item->'initial_liquidity_mint') IS NOT DISTINCT FROM
           (new_item->'settings', new_item->'vault_index', new_item->'vault_pubkey', new_item->'initial_reserve', new_item->'initial_market', new_item->'initial_liquidity_mint') THEN RETURN NULL; END IF;
        IF old_item IS NOT DISTINCT FROM new_item THEN RETURN NULL; END IF;
    END IF;

    FOR item IN SELECT value FROM jsonb_array_elements(jsonb_build_array(old_item, new_item)) WHERE value <> 'null'::jsonb LOOP
        scope_settings := NULL;
        scope_index := NULL;
        affected_at := NULL;
        IF TG_TABLE_NAME = 'vault_position_snapshot_positions' THEN
            SELECT v.settings, v.vault_index, s.observed_at INTO scope_settings, scope_index, affected_at
            FROM loyal_yield.vault_position_snapshots s JOIN loyal_yield.managed_vaults v ON v.id = s.vault_id
            WHERE s.id = (item->>'snapshot_id')::bigint;
        ELSIF TG_TABLE_NAME IN ('vault_position_snapshots', 'vault_idle_token_balances_current') THEN
            SELECT v.settings, v.vault_index INTO scope_settings, scope_index
            FROM loyal_yield.managed_vaults v WHERE v.id = (item->>'vault_id')::bigint;
            affected_at := (item->>'observed_at')::timestamptz;
        ELSIF TG_TABLE_NAME = 'user_yield_position_holding_events' THEN
            SELECT p.settings, p.vault_index INTO scope_settings, scope_index
            FROM loyal_yield.user_yield_positions p WHERE p.id = (item->>'position_id')::bigint;
            affected_at := (item->>'observed_at')::timestamptz;
        ELSE
            scope_settings := item->>'settings';
            scope_index := (item->>'vault_index')::smallint;
            -- Position metadata can affect the entire accounting history.
            affected_at := CASE WHEN TG_TABLE_NAME IN ('user_yield_positions', 'managed_vaults')
                THEN '1970-01-01Z'::timestamptz ELSE (item->>'confirmed_at')::timestamptz END;
        END IF;
        IF scope_settings IS NOT NULL AND scope_index IS NOT NULL AND affected_at IS NOT NULL THEN
            affected_day := (affected_at AT TIME ZONE 'UTC')::date;
            current_key := scope_settings || ':' || scope_index || ':' || affected_day;
            IF current_key IS DISTINCT FROM previous_key THEN
                INSERT INTO loyal_yield.earn_history_day_revisions (settings, vault_index, history_day)
                VALUES (scope_settings, scope_index, affected_day)
                ON CONFLICT (settings, vault_index, history_day) DO UPDATE
                SET revision = loyal_yield.earn_history_day_revisions.revision + 1;
            END IF;
            previous_key := current_key;
        END IF;
    END LOOP;
    RETURN NULL;
END;
$$;

CREATE TRIGGER earn_history_day_revision AFTER INSERT OR UPDATE OR DELETE ON loyal_yield.user_yield_positions
FOR EACH ROW EXECUTE FUNCTION loyal_yield.bump_earn_history_day();
CREATE TRIGGER earn_history_day_revision AFTER INSERT OR UPDATE OR DELETE ON loyal_yield.user_yield_position_deposits
FOR EACH ROW EXECUTE FUNCTION loyal_yield.bump_earn_history_day();
CREATE TRIGGER earn_history_day_revision AFTER INSERT OR UPDATE OR DELETE ON loyal_yield.user_yield_position_withdrawals
FOR EACH ROW EXECUTE FUNCTION loyal_yield.bump_earn_history_day();
CREATE TRIGGER earn_history_day_revision AFTER INSERT OR UPDATE OR DELETE ON loyal_yield.user_yield_position_holding_events
FOR EACH ROW EXECUTE FUNCTION loyal_yield.bump_earn_history_day();
CREATE TRIGGER earn_history_day_revision AFTER INSERT OR UPDATE OR DELETE ON loyal_yield.vault_position_snapshots
FOR EACH ROW EXECUTE FUNCTION loyal_yield.bump_earn_history_day();
CREATE TRIGGER earn_history_day_revision AFTER INSERT OR UPDATE OR DELETE ON loyal_yield.vault_position_snapshot_positions
FOR EACH ROW EXECUTE FUNCTION loyal_yield.bump_earn_history_day();

CREATE TRIGGER earn_history_day_revision AFTER INSERT OR UPDATE OR DELETE ON loyal_yield.vault_idle_token_balances_current
FOR EACH ROW EXECUTE FUNCTION loyal_yield.bump_earn_history_day();

CREATE TRIGGER earn_history_day_revision AFTER INSERT OR UPDATE OR DELETE ON loyal_yield.managed_vaults
FOR EACH ROW EXECUTE FUNCTION loyal_yield.bump_earn_history_day();

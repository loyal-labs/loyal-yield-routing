CREATE TABLE IF NOT EXISTS loyal_yield.route_lookup_tables (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    scope TEXT NOT NULL,
    table_address TEXT NOT NULL,
    authority TEXT NOT NULL,
    payer TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'usable',
    durable BOOLEAN NOT NULL DEFAULT TRUE,
    address_count INTEGER NOT NULL DEFAULT 0 CHECK (address_count >= 0 AND address_count <= 256),
    address_hash TEXT NOT NULL DEFAULT '',
    addresses JSONB NOT NULL DEFAULT '[]'::jsonb,
    create_signature TEXT,
    extend_signatures JSONB NOT NULL DEFAULT '[]'::jsonb,
    last_extended_slot BIGINT,
    warmup_slot BIGINT,
    deactivated_slot BIGINT,
    deactivate_signature TEXT,
    closed_signature TEXT,
    close_recipient TEXT,
    reclaimed_lamports BIGINT,
    notes TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (table_address)
);

CREATE INDEX IF NOT EXISTS route_lookup_tables_active_scope_idx
    ON loyal_yield.route_lookup_tables (cluster, scope, authority, status)
    WHERE durable = TRUE
      AND status IN ('active', 'warming', 'usable');

CREATE UNIQUE INDEX IF NOT EXISTS route_lookup_tables_unique_active_scope_idx
    ON loyal_yield.route_lookup_tables (cluster, scope, authority)
    WHERE durable = TRUE
      AND status IN ('active', 'warming', 'usable');

CREATE INDEX IF NOT EXISTS route_lookup_tables_cleanup_idx
    ON loyal_yield.route_lookup_tables (authority, durable, status, updated_at DESC);

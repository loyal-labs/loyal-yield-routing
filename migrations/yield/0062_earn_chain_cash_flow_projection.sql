CREATE TABLE IF NOT EXISTS loyal_yield.earn_chain_refund_events (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL CHECK (cluster IN ('mainnet-beta', 'devnet')),
    settings TEXT NOT NULL,
    vault_index SMALLINT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    wallet_address TEXT NOT NULL,
    refund_signature TEXT NOT NULL,
    confirmed_slot BIGINT NOT NULL,
    refund_kind TEXT NOT NULL,
    confirmed_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS earn_chain_refund_events_signature_uidx
    ON loyal_yield.earn_chain_refund_events (refund_signature);

CREATE INDEX IF NOT EXISTS earn_chain_refund_events_wallet_idx
    ON loyal_yield.earn_chain_refund_events (wallet_address, confirmed_slot DESC);

COMMENT ON TABLE loyal_yield.earn_chain_refund_events IS
    'Finalized LaserStream projection of policy, token-account, and vault rent refunds.';

CREATE TABLE IF NOT EXISTS loyal_yield.earn_chain_mutations (
    id BIGSERIAL PRIMARY KEY,
    mutation_kind TEXT NOT NULL,
    chain_signature TEXT NOT NULL,
    settings TEXT NOT NULL,
    vault_index SMALLINT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    confirmed_slot BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (mutation_kind, chain_signature, vault_pubkey)
);

COMMENT ON TABLE loyal_yield.earn_chain_mutations IS
    'Stable chain identities claimed atomically with Earn projection writes; sibling account frames and replay cannot double-apply a mutation.';

CREATE OR REPLACE FUNCTION loyal_yield.emit_earn_chain_refund_realtime_event()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM loyal_yield.emit_realtime_event(
        p_event_type => 'earn.transaction.recorded',
        p_scope => 'earn',
        p_reason => 'refund_projected',
        p_solana_env => NEW.cluster,
        p_wallet_address => NEW.wallet_address,
        p_settings_pda => NEW.settings,
        p_smart_account_address => NEW.vault_pubkey,
        p_vault_pubkey => NEW.vault_pubkey,
        p_source_table => 'earn_chain_refund_events',
        p_source_id => NEW.id::text,
        p_payload => '{}'::jsonb
    );
    RETURN NEW;
END;
$$;

CREATE TRIGGER earn_chain_refund_realtime_event
AFTER INSERT ON loyal_yield.earn_chain_refund_events
FOR EACH ROW EXECUTE FUNCTION loyal_yield.emit_earn_chain_refund_realtime_event();

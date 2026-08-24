CREATE TABLE loyal_yield.earn_activity_events (
    id BIGSERIAL PRIMARY KEY,
    idempotency_key TEXT NOT NULL,
    cluster TEXT NOT NULL,
    settings TEXT NOT NULL,
    authority TEXT NOT NULL,
    wallet TEXT NOT NULL,
    vault_index SMALLINT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    event_type TEXT NOT NULL,
    signature TEXT NOT NULL,
    instruction_index INTEGER NOT NULL DEFAULT 0,
    event_slot BIGINT NOT NULL,
    event_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    entity_kind TEXT NOT NULL,
    entity_key TEXT NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT earn_activity_events_idempotency UNIQUE (idempotency_key),
    CONSTRAINT earn_activity_events_type_check CHECK (
        event_type IN (
            'autodeposit_created',
            'autodeposit_closed',
            'autoswap_created',
            'autoswap_closed'
        )
    ),
    CONSTRAINT earn_activity_events_slot_check CHECK (event_slot >= 0),
    CONSTRAINT earn_activity_events_instruction_index_check
        CHECK (instruction_index >= 0)
);

CREATE INDEX earn_activity_events_wallet_history_idx
    ON loyal_yield.earn_activity_events (
        cluster,
        settings,
        wallet,
        vault_index,
        event_at DESC,
        event_slot DESC,
        id DESC
    );

-- Only backfill rows whose original transaction evidence is still explicit.
-- Mutable last_seen fields are not a safe substitute for lost history.
INSERT INTO loyal_yield.earn_activity_events (
    idempotency_key, cluster, settings, authority, wallet, vault_index,
    vault_pubkey, event_type, signature, event_slot, event_at,
    entity_kind, entity_key, metadata
)
SELECT
    concat_ws(':', target.cluster, target.policy_signature,
        'autodeposit_created', target.policy_account),
    target.cluster,
    target.settings,
    target.authority,
    target.wallet,
    target.vault_index,
    target.vault_pubkey,
    'autodeposit_created',
    target.policy_signature,
    target.policy_confirmed_slot,
    target.first_seen_at,
    'autodeposit_policy',
    target.policy_account,
    jsonb_build_object('targetId', target.id)
FROM loyal_yield.balance_sweep_targets AS target
WHERE target.cluster IS NOT NULL
  AND target.policy_signature IS NOT NULL
  AND target.policy_confirmed_slot IS NOT NULL
ON CONFLICT (idempotency_key) DO NOTHING;

INSERT INTO loyal_yield.earn_activity_events (
    idempotency_key, cluster, settings, authority, wallet, vault_index,
    vault_pubkey, event_type, signature, event_slot, event_at,
    entity_kind, entity_key, metadata
)
SELECT
    concat_ws(':', target.cluster, target.close_signature,
        'autodeposit_closed', target.policy_account),
    target.cluster,
    target.settings,
    target.authority,
    target.wallet,
    target.vault_index,
    target.vault_pubkey,
    'autodeposit_closed',
    target.close_signature,
    target.close_slot,
    target.closed_at,
    'autodeposit_policy',
    target.policy_account,
    jsonb_build_object('targetId', target.id)
FROM loyal_yield.balance_sweep_targets AS target
WHERE target.cluster IS NOT NULL
  AND target.close_signature IS NOT NULL
  AND target.close_slot IS NOT NULL
  AND target.closed_at IS NOT NULL
ON CONFLICT (idempotency_key) DO NOTHING;


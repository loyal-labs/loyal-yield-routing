-- Confirmed Subscriptions recurring delegations whose delegatee is a managed
-- vault, recorded by the observer stream whether or not the vault's
-- Autodeposit target exists yet. A target attaches the latest live delegation
-- of its wallet, vault and subscription authority, so a delegation that lands
-- before its Autodeposit policy is kept. revoked_slot is the slot that closed
-- the delegation; a replayed older create cannot revive it.
CREATE TABLE loyal_yield.recurring_delegation_observations (
    recurring_delegation TEXT PRIMARY KEY,
    wallet TEXT NOT NULL,
    vault_pubkey TEXT NOT NULL,
    subscription_authority TEXT NOT NULL,
    nonce BIGINT NOT NULL CHECK (nonce >= 0),
    amount_per_period BIGINT NOT NULL CHECK (amount_per_period >= 0),
    period_length_seconds BIGINT NOT NULL CHECK (period_length_seconds >= 0),
    start_timestamp BIGINT NOT NULL,
    expiry_timestamp BIGINT NOT NULL,
    signature TEXT NOT NULL,
    slot BIGINT NOT NULL CHECK (slot > 0),
    revoked_slot BIGINT CHECK (revoked_slot >= slot),
    observed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX recurring_delegation_observations_owner_idx
    ON loyal_yield.recurring_delegation_observations (wallet, vault_pubkey, subscription_authority, slot DESC);

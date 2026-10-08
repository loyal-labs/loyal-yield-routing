-- Replace the fleet-wide policy setup funding execution lock with a short,
-- atomic balance reservation whenever the route's payer debit is exact.

CREATE TABLE IF NOT EXISTS loyal_yield.route_policy_setup_funding_payers (
    cluster TEXT NOT NULL,
    payer TEXT NOT NULL,
    observed_balance_lamports BIGINT NOT NULL,
    observed_balance_slot BIGINT NOT NULL,
    observed_balance_at TIMESTAMPTZ NOT NULL,
    minimum_balance_lamports BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster, payer),
    CONSTRAINT route_policy_setup_funding_payers_identity_check CHECK (
        NULLIF(btrim(cluster), '') IS NOT NULL
        AND NULLIF(btrim(payer), '') IS NOT NULL
    ),
    CONSTRAINT route_policy_setup_funding_payers_balance_check CHECK (
        observed_balance_lamports >= 0
        AND observed_balance_slot >= 0
        AND minimum_balance_lamports >= 0
        AND minimum_balance_lamports <= observed_balance_lamports
    )
);

CREATE TABLE IF NOT EXISTS loyal_yield.route_policy_setup_funding_reservations (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    payer TEXT NOT NULL,
    semantic_key TEXT NOT NULL UNIQUE,
    opportunity_id BIGINT NOT NULL,
    signed_submission_id BIGINT NOT NULL UNIQUE,
    setup_funding_lamports BIGINT NOT NULL,
    compiled_fee_lamports BIGINT NOT NULL,
    reserved_lamports BIGINT NOT NULL,
    observed_balance_lamports BIGINT NOT NULL,
    observed_balance_slot BIGINT NOT NULL,
    observed_balance_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT route_policy_setup_funding_reservations_payer_fkey
        FOREIGN KEY (cluster, payer)
        REFERENCES loyal_yield.route_policy_setup_funding_payers(cluster, payer)
        ON DELETE RESTRICT,
    CONSTRAINT route_policy_setup_funding_reservations_opportunity_fkey
        FOREIGN KEY (opportunity_id)
        REFERENCES loyal_yield.rebalance_opportunities(id)
        ON DELETE RESTRICT,
    CONSTRAINT route_policy_setup_funding_reservations_submission_fkey
        FOREIGN KEY (signed_submission_id)
        REFERENCES loyal_yield.signed_route_submissions(id)
        ON DELETE RESTRICT,
    CONSTRAINT route_policy_setup_funding_reservations_values_check CHECK (
        NULLIF(btrim(cluster), '') IS NOT NULL
        AND NULLIF(btrim(payer), '') IS NOT NULL
        AND NULLIF(btrim(semantic_key), '') IS NOT NULL
        AND opportunity_id > 0
        AND signed_submission_id > 0
        AND setup_funding_lamports >= 0
        AND compiled_fee_lamports >= 0
        AND reserved_lamports = setup_funding_lamports + compiled_fee_lamports
        AND reserved_lamports > 0
        AND observed_balance_lamports >= reserved_lamports
        AND observed_balance_slot >= 0
    )
);

CREATE INDEX IF NOT EXISTS route_policy_setup_funding_reservations_payer_idx
    ON loyal_yield.route_policy_setup_funding_reservations
        (cluster, payer, observed_balance_slot, created_at);

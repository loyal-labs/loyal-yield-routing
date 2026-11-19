-- An explicit anchor taken from finalized mainnet starts observation after launch.
CREATE TABLE loyal_yield.referral_earn_max_fee_cursor (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    verified_slot bigint NOT NULL CHECK (verified_slot > 0),
    state jsonb NOT NULL,
    checked_slot bigint NOT NULL CHECK (checked_slot >= verified_slot),
    hold_reason text
);
CREATE TABLE loyal_yield.referral_earn_max_fee_harvests (
    signature text PRIMARY KEY,
    slot bigint NOT NULL CHECK (slot > 0),
    transaction_index integer NOT NULL CHECK (transaction_index >= 0),
    treasury_lp_account text NOT NULL,
    amount_lp_raw numeric(20,0) NOT NULL CHECK (amount_lp_raw >= 0)
);
CREATE TABLE loyal_yield.referral_earn_max_fee_assessments (
    signature text PRIMARY KEY,
    slot bigint NOT NULL CHECK (slot > 0),
    transaction_index integer NOT NULL CHECK (transaction_index >= 0),
    effective_supply_raw numeric(20,0) NOT NULL CHECK (effective_supply_raw > 0),
    fee_lp_raw numeric(20,0) NOT NULL CHECK (fee_lp_raw > 0),
    block_time timestamptz NOT NULL,
    block_signatures text[] NOT NULL CHECK (cardinality(block_signatures) > transaction_index),
    harvest_signature text REFERENCES loyal_yield.referral_earn_max_fee_harvests(signature)
);
CREATE INDEX referral_fee_assessments_unharvested ON loyal_yield.referral_earn_max_fee_assessments(slot, transaction_index)
    WHERE harvest_signature IS NULL;
-- A missing friend's ownership evidence cannot change another friend's denominator.
CREATE TABLE loyal_yield.referral_earn_max_fee_weights (
    assessment_signature text NOT NULL REFERENCES loyal_yield.referral_earn_max_fee_assessments(signature),
    referee_app_user_id uuid NOT NULL REFERENCES loyal_yield.referral_bindings(referee_app_user_id),
    referrer_app_user_id uuid NOT NULL,
    shares_raw numeric(20,0) CHECK (shares_raw >= 0),
    hold_reason text,
    realized boolean NOT NULL DEFAULT false,
    last_attempt_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (assessment_signature, referee_app_user_id),
    CHECK ((shares_raw IS NULL) = (hold_reason IS NOT NULL)),
    CHECK (NOT realized OR shares_raw IS NOT NULL)
);
CREATE INDEX referral_fee_weights_retry ON loyal_yield.referral_earn_max_fee_weights(last_attempt_at,assessment_signature,referee_app_user_id)
    WHERE hold_reason IS NOT NULL;
CREATE TABLE loyal_yield.referral_earn_max_fee_remainders (
    referee_app_user_id uuid PRIMARY KEY REFERENCES loyal_yield.referral_bindings(referee_app_user_id),
    remainder text NOT NULL
);
CREATE TABLE loyal_yield.referral_earn_max_fee_lots (
    lot_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    referee_app_user_id uuid NOT NULL REFERENCES loyal_yield.referral_bindings(referee_app_user_id),
    referrer_app_user_id uuid NOT NULL,
    assessment_signature text NOT NULL REFERENCES loyal_yield.referral_earn_max_fee_assessments(signature),
    assessment_slot bigint NOT NULL CHECK (assessment_slot > 0),
    harvest_signature text NOT NULL REFERENCES loyal_yield.referral_earn_max_fee_harvests(signature),
    harvest_slot bigint NOT NULL CHECK (harvest_slot >= assessment_slot),
    vault text NOT NULL,
    lp_mint text NOT NULL,
    treasury_lp_account text NOT NULL,
    amount_lp_raw numeric(20,0) NOT NULL CHECK (amount_lp_raw > 0),
    UNIQUE(harvest_signature, assessment_signature, referee_app_user_id)
);
CREATE INDEX referral_fee_lots_referee ON loyal_yield.referral_earn_max_fee_lots(referee_app_user_id, lot_id);
CREATE FUNCTION loyal_yield.protect_referral_fee_receipt() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Finalized referral fee receipts are immutable';
END;
$$;
CREATE TRIGGER protect_referral_fee_lot BEFORE UPDATE OR DELETE ON loyal_yield.referral_earn_max_fee_lots
FOR EACH ROW EXECUTE FUNCTION loyal_yield.protect_referral_fee_receipt();
CREATE TRIGGER protect_referral_fee_harvest BEFORE UPDATE OR DELETE ON loyal_yield.referral_earn_max_fee_harvests
FOR EACH ROW EXECUTE FUNCTION loyal_yield.protect_referral_fee_receipt();

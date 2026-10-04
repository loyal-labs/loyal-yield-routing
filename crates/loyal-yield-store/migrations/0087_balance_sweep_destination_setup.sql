-- Additive, inactive worker-v2 family journal. This migration creates no job,
-- enables no target, and does not change any legacy pull/top_up attempt shape.
CREATE TABLE loyal_yield.balance_sweep_destination_setup_attempts (
    id BIGSERIAL PRIMARY KEY,
    claim_token TEXT NOT NULL REFERENCES loyal_yield.balance_sweep_lot_claims(claim_token) ON DELETE RESTRICT,
    stage TEXT NOT NULL CHECK (stage IN ('ata','metadata','obligation','farm')),
    account TEXT NOT NULL CHECK (NULLIF(btrim(account),'') IS NOT NULL),
    attempt_number INTEGER NOT NULL DEFAULT 1 CHECK (attempt_number > 0),
    plan JSONB NOT NULL CHECK (jsonb_typeof(plan)='object'),
    signature TEXT NOT NULL UNIQUE CHECK (NULLIF(btrim(signature),'') IS NOT NULL),
    signed_transaction_base64 TEXT NOT NULL CHECK (NULLIF(btrim(signed_transaction_base64),'') IS NOT NULL),
    signed_transaction_sha256 TEXT NOT NULL CHECK (signed_transaction_sha256 ~ '^[0-9a-f]{64}$'),
    recent_blockhash TEXT NOT NULL CHECK (NULLIF(btrim(recent_blockhash),'') IS NOT NULL),
    last_valid_block_height BIGINT NOT NULL CHECK (last_valid_block_height > 0),
    attempt_state TEXT NOT NULL DEFAULT 'prepared'
        CHECK (attempt_state IN ('prepared','submitted','unknown','ambiguous','confirmed','failed','expired')),
    broadcast_count INTEGER NOT NULL DEFAULT 0 CHECK (broadcast_count >= 0),
    last_broadcast_at TIMESTAMPTZ,
    last_status_checked_at TIMESTAMPTZ,
    confirmed_slot BIGINT CHECK (confirmed_slot > 0),
    readback_slot BIGINT CHECK (readback_slot > 0),
    readback_evidence JSONB CHECK (readback_evidence IS NULL OR jsonb_typeof(readback_evidence)='object'),
    error_detail TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(claim_token,stage,attempt_number),
    CONSTRAINT destination_setup_confirmed_readback CHECK (
        attempt_state <> 'confirmed' OR
        (confirmed_slot IS NOT NULL AND readback_slot >= confirmed_slot AND readback_evidence IS NOT NULL)
    )
);

CREATE UNIQUE INDEX balance_sweep_destination_setup_active_idx
    ON loyal_yield.balance_sweep_destination_setup_attempts(claim_token)
    WHERE attempt_state IN ('prepared','submitted','unknown','ambiguous');
CREATE INDEX balance_sweep_destination_setup_recovery_idx
    ON loyal_yield.balance_sweep_destination_setup_attempts(attempt_state,updated_at,id)
    WHERE attempt_state IN ('prepared','submitted','unknown','ambiguous');

CREATE FUNCTION loyal_yield.guard_balance_sweep_setup_wire()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.claim_token IS DISTINCT FROM OLD.claim_token
       OR NEW.stage IS DISTINCT FROM OLD.stage
       OR NEW.account IS DISTINCT FROM OLD.account
       OR NEW.attempt_number IS DISTINCT FROM OLD.attempt_number
       OR NEW.plan IS DISTINCT FROM OLD.plan
       OR NEW.signature IS DISTINCT FROM OLD.signature
       OR NEW.signed_transaction_base64 IS DISTINCT FROM OLD.signed_transaction_base64
       OR NEW.signed_transaction_sha256 IS DISTINCT FROM OLD.signed_transaction_sha256
       OR NEW.recent_blockhash IS DISTINCT FROM OLD.recent_blockhash
       OR NEW.last_valid_block_height IS DISTINCT FROM OLD.last_valid_block_height THEN
        RAISE EXCEPTION 'autodeposit destination setup wire identity is immutable';
    END IF;
    IF NEW.broadcast_count < OLD.broadcast_count
       OR (OLD.attempt_state IN ('confirmed','failed','expired') AND NEW.attempt_state <> OLD.attempt_state)
       OR (OLD.readback_evidence IS NOT NULL AND NEW.readback_evidence IS DISTINCT FROM OLD.readback_evidence) THEN
        RAISE EXCEPTION 'autodeposit destination setup evidence cannot regress';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER guard_balance_sweep_setup_wire
BEFORE UPDATE ON loyal_yield.balance_sweep_destination_setup_attempts
FOR EACH ROW EXECUTE FUNCTION loyal_yield.guard_balance_sweep_setup_wire();

-- Legacy selected-claim release must also respect an unresolved signed setup.
-- A database lease cannot revoke the rent/initialization transaction on chain.
CREATE FUNCTION loyal_yield.guard_balance_sweep_setup_claim_release()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status IN ('released','failed') AND NEW.status IS DISTINCT FROM OLD.status
       AND EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_destination_setup_attempts setup
                  WHERE setup.claim_token=OLD.claim_token
                    AND setup.attempt_state IN ('prepared','submitted','unknown','ambiguous')) THEN
        RAISE EXCEPTION 'autodeposit claim owns unresolved destination setup';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER guard_balance_sweep_setup_claim_release
BEFORE UPDATE OF status ON loyal_yield.balance_sweep_lot_claims
FOR EACH ROW EXECUTE FUNCTION loyal_yield.guard_balance_sweep_setup_claim_release();

COMMENT ON TABLE loyal_yield.balance_sweep_destination_setup_attempts IS
    'Family-owned exact destination setup before wallet pull; confirmed initialization requires account readback evidence.';

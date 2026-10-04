-- A family-owned side journal preserves old Rust's strict expected_effects
-- decoder. No existing operation JSON shape or financial anchor is rewritten.
CREATE TABLE loyal_yield.multiply_operation_evidence (
    operation_id TEXT NOT NULL REFERENCES loyal_yield.multiply_operations(operation_id) ON DELETE RESTRICT,
    evidence_kind TEXT NOT NULL CHECK (evidence_kind IN ('prestate','expired_no_effect','reconciled_receipt')),
    signature TEXT NOT NULL CHECK (NULLIF(btrim(signature),'') IS NOT NULL),
    signed_wire_sha256 TEXT NOT NULL CHECK (signed_wire_sha256 ~ '^[0-9a-f]{64}$'),
    expected_effects_sha256 TEXT NOT NULL CHECK (expected_effects_sha256 ~ '^[0-9a-f]{64}$'),
    observed_slot BIGINT NOT NULL CHECK (observed_slot > 0),
    evidence JSONB NOT NULL CHECK (jsonb_typeof(evidence)='object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(operation_id,evidence_kind)
);

CREATE FUNCTION loyal_yield.guard_multiply_operation_evidence()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'multiply operation evidence is immutable';
END;
$$;
CREATE TRIGGER guard_multiply_operation_evidence
BEFORE UPDATE OR DELETE ON loyal_yield.multiply_operation_evidence
FOR EACH ROW EXECUTE FUNCTION loyal_yield.guard_multiply_operation_evidence();

COMMENT ON TABLE loyal_yield.multiply_operation_evidence IS
    'Immutable exact-wire and original-effect-bound Multiply prestate, expiry, and receipt evidence without changing legacy JSON contracts.';

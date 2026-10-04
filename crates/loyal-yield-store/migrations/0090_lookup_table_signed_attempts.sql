-- Immutable exact packets supplement the existing ALT lifecycle.
-- Application is explicit; worker startup never runs this migration.
-- Existing 0017 operations remain the lifecycle owner; 0021 controls, permits,
-- and 0020 budget reservations remain authoritative. No alternate Go lease.
CREATE TABLE loyal_yield.lookup_table_signed_attempts (
    id BIGSERIAL PRIMARY KEY,
    operation_id BIGINT NOT NULL REFERENCES loyal_yield.lookup_table_operations(id),
    attempt_number INTEGER NOT NULL CHECK (attempt_number > 0),
    source_fencing_token BIGINT NOT NULL CHECK (source_fencing_token > 0),
    cluster TEXT NOT NULL CHECK (length(btrim(cluster)) > 0),
    family_id BIGINT NOT NULL REFERENCES loyal_yield.lookup_table_families(id),
    route_lookup_table_id BIGINT NOT NULL REFERENCES loyal_yield.route_lookup_tables(id),
    operation_kind TEXT NOT NULL CHECK (operation_kind IN ('create','extend','rollover','deactivate','close')),
    table_address TEXT NOT NULL,
    authority TEXT NOT NULL,
    payer TEXT NOT NULL,
    recipient TEXT,
    generation INTEGER NOT NULL CHECK (generation >= 0),
    mutation_epoch BIGINT NOT NULL CHECK (mutation_epoch >= 0),
    recent_slot BIGINT CHECK (recent_slot >= 0),
    expected_deactivation_slot BIGINT CHECK (expected_deactivation_slot >= 0),
    prefix_addresses TEXT[] NOT NULL,
    extension_addresses TEXT[] NOT NULL,
    expected_prefix_hash TEXT NOT NULL CHECK (expected_prefix_hash ~ '^[0-9a-f]{64}$'),
    transaction_signature TEXT NOT NULL UNIQUE CHECK (length(btrim(transaction_signature)) > 0),
    message_hash TEXT NOT NULL CHECK (message_hash ~ '^[0-9a-f]{64}$'),
    signed_transaction BYTEA NOT NULL CHECK (octet_length(signed_transaction) BETWEEN 65 AND 1232),
    signed_transaction_sha256 TEXT NOT NULL CHECK (signed_transaction_sha256 ~ '^[0-9a-f]{64}$'),
    recent_blockhash TEXT NOT NULL CHECK (length(btrim(recent_blockhash)) > 0),
    last_valid_block_height BIGINT NOT NULL CHECK (last_valid_block_height > 0),
    signing_context_slot BIGINT NOT NULL CHECK (signing_context_slot > 0),
    estimated_fee_lamports BIGINT NOT NULL CHECK (estimated_fee_lamports >= 0),
    estimated_rent_lamports BIGINT NOT NULL CHECK (estimated_rent_lamports >= 0),
    estimated_reclaimed_rent_lamports BIGINT NOT NULL CHECK (estimated_reclaimed_rent_lamports >= 0),
    attempt_state TEXT NOT NULL DEFAULT 'prepared' CHECK (attempt_state IN (
        'prepared','submitted','unknown','confirmed','finalized','reconciled','failed','expired'
    )),
    broadcast_count INTEGER NOT NULL DEFAULT 0 CHECK (broadcast_count >= 0),
    last_broadcast_at TIMESTAMPTZ,
    last_status_checked_at TIMESTAMPTZ,
    confirmed_slot BIGINT CHECK (confirmed_slot > 0),
    finalized_slot BIGINT CHECK (finalized_slot > 0),
    readback_slot BIGINT CHECK (readback_slot > 0),
    history_context_slot BIGINT CHECK (history_context_slot > 0),
    history_complete BOOLEAN NOT NULL DEFAULT false,
    observed_block_height BIGINT CHECK (observed_block_height > 0),
    proof_kind TEXT CHECK (proof_kind IN ('effect','no_effect','failed_receipt')),
    readback_evidence JSONB,
    receipt_evidence JSONB,
    error_detail TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (operation_id, attempt_number),
    CHECK (payer = authority),
    CHECK ((operation_kind IN ('create','rollover')) = (recent_slot IS NOT NULL)),
    CHECK ((operation_kind = 'close') = (recipient IS NOT NULL)),
    CHECK ((operation_kind = 'close') = (expected_deactivation_slot IS NOT NULL)),
    CHECK (recent_slot IS NULL OR recent_slot <= signing_context_slot),
    CHECK (encode(sha256(signed_transaction),'hex') = signed_transaction_sha256),
    CHECK (recipient IS NULL OR recipient = authority),
    CHECK (cardinality(prefix_addresses) BETWEEN 0 AND 256),
    CHECK (cardinality(extension_addresses) BETWEEN 0 AND 20),
    CHECK (cardinality(prefix_addresses) + cardinality(extension_addresses) <= 256),
    CHECK (operation_kind NOT IN ('create','rollover') OR cardinality(prefix_addresses) = 0),
    CHECK (operation_kind <> 'extend' OR cardinality(extension_addresses) > 0),
    CHECK (operation_kind NOT IN ('deactivate','close') OR cardinality(extension_addresses) = 0),
    CHECK (readback_evidence IS NULL OR jsonb_typeof(readback_evidence) = 'object'),
    CHECK (receipt_evidence IS NULL OR jsonb_typeof(receipt_evidence) = 'object'),
    CHECK (attempt_state NOT IN ('reconciled','failed','expired') OR COALESCE((
        readback_slot >= signing_context_slot AND readback_evidence IS NOT NULL AND
        readback_evidence ?& ARRAY['address','owner','data_hash','observed_slot','proof'] AND
        jsonb_typeof(readback_evidence->'address') = 'string' AND
        (readback_evidence->>'address') = table_address AND
        jsonb_typeof(readback_evidence->'observed_slot') = 'number' AND
        (readback_evidence->>'observed_slot')::BIGINT = readback_slot AND
        jsonb_typeof(readback_evidence->'owner') = 'string' AND
        jsonb_typeof(readback_evidence->'data_hash') = 'string' AND
        (readback_evidence->>'data_hash') ~ '^[0-9a-f]{64}$' AND
        jsonb_typeof(readback_evidence->'proof') = 'string'
    ), false)),
    CHECK (attempt_state NOT IN ('reconciled','failed') OR COALESCE((
        finalized_slot IS NOT NULL AND readback_slot >= finalized_slot AND
        receipt_evidence IS NOT NULL AND
        receipt_evidence ?& ARRAY['signature','message_hash','signed_transaction_sha256','slot','commitment','err','fee_lamports','table_pre_lamports','table_post_lamports','payer_pre_lamports','payer_post_lamports'] AND
        (receipt_evidence->>'signature') = transaction_signature AND
        (receipt_evidence->>'message_hash') = message_hash AND
        (receipt_evidence->>'signed_transaction_sha256') = signed_transaction_sha256 AND
        jsonb_typeof(receipt_evidence->'slot') = 'number' AND
        (receipt_evidence->>'slot')::BIGINT = finalized_slot AND
        (receipt_evidence->>'commitment') = 'finalized' AND
        jsonb_typeof(receipt_evidence->'fee_lamports') = 'number' AND
        (receipt_evidence->>'fee_lamports')::BIGINT >= 0 AND
        jsonb_typeof(receipt_evidence->'table_pre_lamports') = 'number' AND
        (receipt_evidence->>'table_pre_lamports')::BIGINT >= 0 AND
        jsonb_typeof(receipt_evidence->'table_post_lamports') = 'number' AND
        (receipt_evidence->>'table_post_lamports')::BIGINT >= 0 AND
        jsonb_typeof(receipt_evidence->'payer_pre_lamports') = 'number' AND
        (receipt_evidence->>'payer_pre_lamports')::BIGINT >= 0 AND
        jsonb_typeof(receipt_evidence->'payer_post_lamports') = 'number' AND
        (receipt_evidence->>'payer_post_lamports')::BIGINT >= 0
    ), false)),
    CHECK (attempt_state <> 'reconciled' OR COALESCE((
        proof_kind = 'effect' AND receipt_evidence->'err' = 'null'::jsonb
    ), false)),
    CHECK (attempt_state <> 'failed' OR COALESCE((
        proof_kind = 'failed_receipt' AND receipt_evidence->'err' <> 'null'::jsonb
    ), false)),
    CHECK (attempt_state <> 'expired' OR COALESCE((
        proof_kind = 'no_effect' AND history_complete AND
        history_context_slot >= readback_slot AND observed_block_height > last_valid_block_height
    ), false))
);

CREATE UNIQUE INDEX lookup_table_signed_attempts_one_unresolved_idx
    ON loyal_yield.lookup_table_signed_attempts(operation_id)
    WHERE attempt_state NOT IN ('reconciled','failed','expired');
CREATE UNIQUE INDEX lookup_table_signed_attempts_table_unresolved_idx
    ON loyal_yield.lookup_table_signed_attempts(route_lookup_table_id)
    WHERE attempt_state NOT IN ('reconciled','failed','expired');

CREATE FUNCTION loyal_yield.guard_lookup_table_signed_attempt()
RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    source_op loyal_yield.lookup_table_operations%ROWTYPE;
    source_family loyal_yield.lookup_table_families%ROWTYPE;
    source_table loyal_yield.route_lookup_tables%ROWTYPE;
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'lookup-table signed attempt evidence is retained';
    END IF;
    IF TG_OP = 'UPDATE' THEN
        IF (to_jsonb(NEW) - ARRAY[
            'attempt_state','broadcast_count','last_broadcast_at','last_status_checked_at',
            'confirmed_slot','finalized_slot','readback_slot','history_context_slot',
            'observed_block_height','history_complete','proof_kind','readback_evidence','receipt_evidence',
            'error_detail','updated_at'
        ]) IS DISTINCT FROM (to_jsonb(OLD) - ARRAY[
            'attempt_state','broadcast_count','last_broadcast_at','last_status_checked_at',
            'confirmed_slot','finalized_slot','readback_slot','history_context_slot',
            'observed_block_height','history_complete','proof_kind','readback_evidence','receipt_evidence',
            'error_detail','updated_at'
        ]) THEN
            RAISE EXCEPTION 'lookup-table signed attempt identity is immutable';
        END IF;
        IF OLD.attempt_state IN ('reconciled','failed','expired') OR
           (OLD.history_complete AND NOT NEW.history_complete) OR
           NEW.broadcast_count < OLD.broadcast_count OR
           (OLD.confirmed_slot IS NOT NULL AND (NEW.confirmed_slot IS NULL OR NEW.confirmed_slot < OLD.confirmed_slot)) OR
           (OLD.finalized_slot IS NOT NULL AND (NEW.finalized_slot IS NULL OR NEW.finalized_slot < OLD.finalized_slot)) OR
           (OLD.readback_slot IS NOT NULL AND (NEW.readback_slot IS NULL OR NEW.readback_slot < OLD.readback_slot)) OR
           (OLD.history_context_slot IS NOT NULL AND (NEW.history_context_slot IS NULL OR NEW.history_context_slot < OLD.history_context_slot)) OR
           (OLD.observed_block_height IS NOT NULL AND (NEW.observed_block_height IS NULL OR NEW.observed_block_height < OLD.observed_block_height)) THEN
            RAISE EXCEPTION 'lookup-table signed attempt evidence cannot regress';
        END IF;
    END IF;
    -- Source fencing is checked under the source operation row lock by every
    -- application transition. This trigger adds structural packet ownership.
    SELECT * INTO STRICT source_family FROM loyal_yield.lookup_table_families
        WHERE id = NEW.family_id FOR SHARE;
    SELECT * INTO STRICT source_table FROM loyal_yield.route_lookup_tables
        WHERE id = NEW.route_lookup_table_id FOR UPDATE;
    SELECT * INTO STRICT source_op FROM loyal_yield.lookup_table_operations
        WHERE id = NEW.operation_id FOR UPDATE;
    IF source_op.family_id <> NEW.family_id OR
       source_op.route_lookup_table_id IS DISTINCT FROM NEW.route_lookup_table_id OR
       source_op.operation_kind <> NEW.operation_kind OR
       source_family.cluster <> NEW.cluster OR source_table.cluster <> NEW.cluster OR
       source_table.family_id IS DISTINCT FROM NEW.family_id OR
       source_table.table_address <> NEW.table_address OR
       source_table.authority IS DISTINCT FROM NEW.authority OR source_family.provisioning_authority IS DISTINCT FROM NEW.authority OR
       source_family.payer IS DISTINCT FROM NEW.payer OR source_table.generation IS DISTINCT FROM NEW.generation OR
       source_op.transaction_signature IS DISTINCT FROM NEW.transaction_signature OR
       source_op.message_hash IS DISTINCT FROM NEW.message_hash OR
       source_op.recent_blockhash IS DISTINCT FROM NEW.recent_blockhash OR
       source_op.last_valid_block_height IS DISTINCT FROM NEW.last_valid_block_height THEN
        RAISE EXCEPTION 'lookup-table signed attempt differs from source ownership';
    END IF;
    IF TG_OP = 'INSERT' AND (source_op.operation_state <> 'signed' OR
       source_op.fencing_token <> NEW.source_fencing_token OR
       source_op.lease_owner IS NULL OR source_op.lease_expires_at IS NULL OR source_op.lease_expires_at <= clock_timestamp() OR
       source_op.mutation_epoch <> NEW.mutation_epoch OR source_table.mutation_epoch IS DISTINCT FROM NEW.mutation_epoch) THEN
        RAISE EXCEPTION 'lookup-table signed attempt requires a live source signing lease';
    END IF;
    IF TG_OP = 'UPDATE' AND (source_op.lease_owner IS NULL OR source_op.lease_expires_at IS NULL OR source_op.lease_expires_at <= clock_timestamp()) THEN
        RAISE EXCEPTION 'lookup-table signed attempt transition requires a live source operation lease';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER lookup_table_signed_attempts_guard
    BEFORE INSERT OR UPDATE OR DELETE ON loyal_yield.lookup_table_signed_attempts
    FOR EACH ROW EXECUTE FUNCTION loyal_yield.guard_lookup_table_signed_attempt();

CREATE FUNCTION loyal_yield.guard_lookup_table_operation_unresolved_packet()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- This also protects a packet when a legacy writer attempts to infer a
    -- terminal state, clear its signature for a retry, or mutate its intent.
    -- The proof-owning Go transition resolves the attempt first in the same
    -- short transaction; no source lifecycle flags substitute for that proof.
    IF TG_OP = 'UPDATE' AND EXISTS (
        SELECT 1 FROM loyal_yield.lookup_table_signed_attempts a
        WHERE a.operation_id = OLD.id AND a.attempt_state NOT IN ('reconciled','failed','expired')
    ) AND (
        NEW.family_id IS DISTINCT FROM OLD.family_id OR
        NEW.route_lookup_table_id IS DISTINCT FROM OLD.route_lookup_table_id OR
        NEW.operation_kind IS DISTINCT FROM OLD.operation_kind OR
        NEW.manifest_id IS DISTINCT FROM OLD.manifest_id OR
        NEW.binding_id IS DISTINCT FROM OLD.binding_id OR
        NEW.target_generation IS DISTINCT FROM OLD.target_generation OR
        NEW.target_shard_ordinal IS DISTINCT FROM OLD.target_shard_ordinal OR
        NEW.mutation_epoch IS DISTINCT FROM OLD.mutation_epoch OR
        NEW.transaction_signature IS DISTINCT FROM OLD.transaction_signature OR
        NEW.message_hash IS DISTINCT FROM OLD.message_hash OR
        NEW.recent_blockhash IS DISTINCT FROM OLD.recent_blockhash OR
        NEW.last_valid_block_height IS DISTINCT FROM OLD.last_valid_block_height OR
        (NEW.operation_context - ARRAY['lastBroadcastPauseFence']) IS DISTINCT FROM
            (OLD.operation_context - ARRAY['lastBroadcastPauseFence']) OR
        NEW.operation_state IN ('complete','permanent_failure','cancelled')
    ) THEN
        RAISE EXCEPTION 'unresolved lookup-table packet requires owned chain proof before reset or terminal state';
    END IF;
    IF NEW.operation_kind <> 'verify' AND NEW.route_lookup_table_id IS NOT NULL AND
       NEW.operation_state NOT IN ('complete','permanent_failure','cancelled') AND EXISTS (
        SELECT 1 FROM loyal_yield.lookup_table_signed_attempts a
        WHERE a.route_lookup_table_id = NEW.route_lookup_table_id
          AND a.operation_id <> NEW.id AND a.attempt_state NOT IN ('reconciled','failed','expired')
    ) THEN
        RAISE EXCEPTION 'another unresolved lookup-table packet owns this physical table';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER lookup_table_operations_unresolved_packet_guard
    BEFORE INSERT OR UPDATE ON loyal_yield.lookup_table_operations
    FOR EACH ROW EXECUTE FUNCTION loyal_yield.guard_lookup_table_operation_unresolved_packet();

CREATE FUNCTION loyal_yield.guard_lookup_table_physical_unresolved_packet()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM loyal_yield.lookup_table_signed_attempts a
        WHERE a.route_lookup_table_id = OLD.id AND a.attempt_state NOT IN ('reconciled','failed','expired')
    ) THEN
        IF TG_OP = 'DELETE' THEN
            RAISE EXCEPTION 'unresolved packet retains physical lookup-table ownership';
        END IF;
        IF NEW.family_id IS DISTINCT FROM OLD.family_id OR
           NEW.cluster IS DISTINCT FROM OLD.cluster OR
           NEW.table_address IS DISTINCT FROM OLD.table_address OR
           NEW.authority IS DISTINCT FROM OLD.authority OR
           NEW.generation IS DISTINCT FROM OLD.generation OR
           NEW.shard_ordinal IS DISTINCT FROM OLD.shard_ordinal OR
           NEW.mutation_epoch IS DISTINCT FROM OLD.mutation_epoch OR
           NEW.addresses IS DISTINCT FROM OLD.addresses OR
           NEW.address_hash IS DISTINCT FROM OLD.address_hash OR
           NEW.address_count IS DISTINCT FROM OLD.address_count OR
           NEW.usable_address_count IS DISTINCT FROM OLD.usable_address_count OR
           (NEW.status IS DISTINCT FROM OLD.status AND NEW.status IN ('retired','failed')) OR
           (NEW.desired_state IS DISTINCT FROM OLD.desired_state AND NEW.desired_state IN ('deactivated','closed','failed')) THEN
            RAISE EXCEPTION 'unresolved packet requires owned chain proof before physical lookup-table mutation';
        END IF;
    END IF;
    IF TG_OP = 'DELETE' THEN RETURN OLD; END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER route_lookup_tables_unresolved_packet_guard
    BEFORE UPDATE OR DELETE ON loyal_yield.route_lookup_tables
    FOR EACH ROW EXECUTE FUNCTION loyal_yield.guard_lookup_table_physical_unresolved_packet();

-- Application transitions additionally require all usage/binding and source controls/budget
-- locks in application persist/permit transactions, and preserve canonical lock
-- order in any new trigger/composition. No network IO under these row locks.
-- Do not backfill exact bytes for old signatures: recovery stays chain-first.

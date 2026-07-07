CREATE OR REPLACE FUNCTION loyal_yield.emit_autodeposit_scheduled_slot_realtime_event()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
    target_row RECORD;
    event_reason TEXT;
    event_payload JSONB;
BEGIN
    IF TG_OP = 'UPDATE'
       AND NEW.target_id IS NOT DISTINCT FROM OLD.target_id
       AND NEW.token_mint IS NOT DISTINCT FROM OLD.token_mint
       AND NEW.eligible_after IS NOT DISTINCT FROM OLD.eligible_after
       AND NEW.status IS NOT DISTINCT FROM OLD.status
       AND NEW.request_source IS NOT DISTINCT FROM OLD.request_source
       AND NEW.requested_at IS NOT DISTINCT FROM OLD.requested_at
       AND NEW.claim_token IS NOT DISTINCT FROM OLD.claim_token
       AND NEW.execution_id IS NOT DISTINCT FROM OLD.execution_id
       AND NEW.last_error IS NOT DISTINCT FROM OLD.last_error THEN
        RETURN NEW;
    END IF;

    SELECT
        settings,
        wallet,
        vault_pubkey
    INTO target_row
    FROM loyal_yield.balance_sweep_targets
    WHERE id = NEW.target_id;

    event_reason := 'scheduled_slot_' || NEW.status::text;
    event_payload := jsonb_strip_nulls(jsonb_build_object(
        'status', NEW.status::text,
        'previousStatus', CASE WHEN TG_OP = 'UPDATE' THEN OLD.status::text ELSE NULL END,
        'requestSource', NEW.request_source,
        'requestedAt', NEW.requested_at,
        'eligibleAfter', NEW.eligible_after,
        'tokenMint', NEW.token_mint,
        'hasClaimToken', NEW.claim_token IS NOT NULL,
        'hasExecution', NEW.execution_id IS NOT NULL,
        'hasError', NEW.last_error IS NOT NULL
    ));

    PERFORM loyal_yield.emit_realtime_event(
        p_event_type => 'autodeposit_slot_changed',
        p_scope => 'autodeposit',
        p_reason => event_reason,
        p_wallet_address => target_row.wallet,
        p_settings_pda => target_row.settings,
        p_vault_pubkey => target_row.vault_pubkey,
        p_target_id => NEW.target_id,
        p_scheduled_slot_id => NEW.id,
        p_execution_id => NEW.execution_id,
        p_source_table => 'balance_sweep_scheduled_slots',
        p_source_id => NEW.id::text,
        p_payload => event_payload
    );

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS balance_sweep_scheduled_slots_realtime_event
    ON loyal_yield.balance_sweep_scheduled_slots;

CREATE TRIGGER balance_sweep_scheduled_slots_realtime_event
AFTER INSERT OR UPDATE ON loyal_yield.balance_sweep_scheduled_slots
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.emit_autodeposit_scheduled_slot_realtime_event();

CREATE OR REPLACE FUNCTION loyal_yield.notify_autodeposit_requested_slot()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.status::text <> 'requested' THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND OLD.status::text = 'requested' THEN
        RETURN NEW;
    END IF;

    PERFORM pg_notify(
        'loyal_yield_autodeposit_wakeup',
        json_build_object('scheduled_slot_id', NEW.id)::text
    );
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS balance_sweep_scheduled_slots_autodeposit_wakeup
    ON loyal_yield.balance_sweep_scheduled_slots;

CREATE TRIGGER balance_sweep_scheduled_slots_autodeposit_wakeup
AFTER INSERT OR UPDATE OF status ON loyal_yield.balance_sweep_scheduled_slots
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.notify_autodeposit_requested_slot();

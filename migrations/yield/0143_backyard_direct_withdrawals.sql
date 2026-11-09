ALTER TABLE loyal_yield.backyard_withdrawal_cost_reservations
    DROP CONSTRAINT backyard_withdrawal_cost_reservations_owner_class_check,
    ADD CONSTRAINT backyard_withdrawal_cost_reservations_owner_class_check
        CHECK (owner_class IN ('squads', 'vault_admin', 'unsupported', 'direct')),
    ADD CONSTRAINT backyard_withdrawal_cost_direct_admitted
        CHECK (owner_class <> 'direct' OR wait_reason IS NULL);

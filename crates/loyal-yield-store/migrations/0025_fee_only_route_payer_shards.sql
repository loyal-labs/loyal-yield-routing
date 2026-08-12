-- Bounded fee-only payers for mature fleet routes.
--
-- These keys are deliberately not policy signers, reusable-ALT authorities,
-- reusable-ALT payers, or setup/rent payers. The hard balance ceiling limits
-- hot-key exposure and the rolling reservation ledger prevents concurrent
-- workers from exceeding an operator-approved spend budget.

CREATE TABLE IF NOT EXISTS loyal_yield.route_fee_payer_shards (
    cluster TEXT NOT NULL,
    fee_payer TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT FALSE,
    minimum_balance_lamports BIGINT NOT NULL,
    maximum_balance_lamports BIGINT NOT NULL,
    rolling_window_seconds INTEGER NOT NULL,
    maximum_window_spend_lamports BIGINT NOT NULL,
    maximum_transaction_fee_lamports BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster, fee_payer),
    CONSTRAINT route_fee_payer_shards_identity_check CHECK (
        NULLIF(btrim(cluster), '') IS NOT NULL
        AND NULLIF(btrim(fee_payer), '') IS NOT NULL
    ),
    CONSTRAINT route_fee_payer_shards_low_balance_check CHECK (
        minimum_balance_lamports > 0
        AND maximum_balance_lamports > minimum_balance_lamports
        -- 0.1 SOL is an intentionally hard upper bound for a fee-only key.
        AND maximum_balance_lamports <= 100000000
    ),
    CONSTRAINT route_fee_payer_shards_budget_check CHECK (
        rolling_window_seconds BETWEEN 60 AND 86400
        AND maximum_transaction_fee_lamports > 0
        AND maximum_window_spend_lamports >= maximum_transaction_fee_lamports
        AND maximum_window_spend_lamports
            <= maximum_balance_lamports - minimum_balance_lamports
    )
);

ALTER TABLE loyal_yield.signed_route_submissions
    ADD COLUMN IF NOT EXISTS fee_payer_kind TEXT NOT NULL DEFAULT 'policy';

ALTER TABLE loyal_yield.signed_route_submissions
    DROP CONSTRAINT IF EXISTS signed_route_submissions_fee_payer_kind_check;
ALTER TABLE loyal_yield.signed_route_submissions
    ADD CONSTRAINT signed_route_submissions_fee_payer_kind_check CHECK (
        fee_payer_kind IN ('policy', 'fee_only_shard')
    );

-- Extend migration 0024's immutable wire-evidence guard to cover the payer
-- role introduced here. The role controls whether spend reservation is
-- mandatory, so changing it after handoff is equivalent to changing signer
-- identity.
CREATE OR REPLACE FUNCTION loyal_yield.guard_signed_route_evidence_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.cluster IS DISTINCT FROM OLD.cluster
       OR NEW.semantic_key IS DISTINCT FROM OLD.semantic_key
       OR NEW.opportunity_id IS DISTINCT FROM OLD.opportunity_id
       OR NEW.signed_transaction IS DISTINCT FROM OLD.signed_transaction
       OR NEW.signed_transaction_hash IS DISTINCT FROM OLD.signed_transaction_hash
       OR NEW.message_hash IS DISTINCT FROM OLD.message_hash
       OR NEW.transaction_signature IS DISTINCT FROM OLD.transaction_signature
       OR NEW.recent_blockhash IS DISTINCT FROM OLD.recent_blockhash
       OR NEW.last_valid_block_height IS DISTINCT FROM OLD.last_valid_block_height
       OR NEW.source_snapshot_id IS DISTINCT FROM OLD.source_snapshot_id
       OR NEW.optimizer_epoch_id IS DISTINCT FROM OLD.optimizer_epoch_id
       OR NEW.alt_requirements_fingerprint IS DISTINCT FROM OLD.alt_requirements_fingerprint
       OR NEW.alt_selection_fingerprint IS DISTINCT FROM OLD.alt_selection_fingerprint
       OR NEW.alt_mutation_epochs IS DISTINCT FROM OLD.alt_mutation_epochs
       OR NEW.fee_payer IS DISTINCT FROM OLD.fee_payer
       OR NEW.fee_payer_kind IS DISTINCT FROM OLD.fee_payer_kind
       OR NEW.compiled_fee_lamports IS DISTINCT FROM OLD.compiled_fee_lamports
       OR NEW.writable_account_keys IS DISTINCT FROM OLD.writable_account_keys
       OR NEW.conflict_account_keys IS DISTINCT FROM OLD.conflict_account_keys
       OR NEW.executor_owner IS DISTINCT FROM OLD.executor_owner
       OR NEW.executor_fencing_token IS DISTINCT FROM OLD.executor_fencing_token
       OR NEW.created_at IS DISTINCT FROM OLD.created_at
       OR (
            NEW.submission_state IS NOT DISTINCT FROM OLD.submission_state
            AND NEW.submission_state_entered_at IS DISTINCT FROM OLD.submission_state_entered_at
       )
       OR (
            OLD.decision_id IS NOT NULL
            AND NEW.decision_id IS DISTINCT FROM OLD.decision_id
       )
    THEN
        RAISE EXCEPTION 'signed route wire and identity evidence is immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TABLE IF NOT EXISTS loyal_yield.route_fee_payer_spend_reservations (
    id BIGSERIAL PRIMARY KEY,
    cluster TEXT NOT NULL,
    fee_payer TEXT NOT NULL,
    semantic_key TEXT NOT NULL UNIQUE,
    opportunity_id BIGINT NOT NULL,
    signed_submission_id BIGINT NOT NULL UNIQUE,
    compiled_fee_lamports BIGINT NOT NULL,
    observed_balance_lamports BIGINT NOT NULL,
    observed_balance_slot BIGINT NOT NULL,
    observed_balance_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT route_fee_payer_spend_reservations_shard_fkey
        FOREIGN KEY (cluster, fee_payer)
        REFERENCES loyal_yield.route_fee_payer_shards(cluster, fee_payer)
        ON DELETE RESTRICT,
    CONSTRAINT route_fee_payer_spend_reservations_opportunity_fkey
        FOREIGN KEY (opportunity_id)
        REFERENCES loyal_yield.rebalance_opportunities(id)
        ON DELETE RESTRICT,
    CONSTRAINT route_fee_payer_spend_reservations_submission_fkey
        FOREIGN KEY (signed_submission_id)
        REFERENCES loyal_yield.signed_route_submissions(id)
        ON DELETE RESTRICT,
    CONSTRAINT route_fee_payer_spend_reservations_values_check CHECK (
        NULLIF(btrim(cluster), '') IS NOT NULL
        AND NULLIF(btrim(fee_payer), '') IS NOT NULL
        AND NULLIF(btrim(semantic_key), '') IS NOT NULL
        AND opportunity_id > 0
        AND signed_submission_id > 0
        AND compiled_fee_lamports >= 0
        AND observed_balance_lamports >= compiled_fee_lamports
        AND observed_balance_slot >= 0
    )
);

CREATE OR REPLACE FUNCTION loyal_yield.guard_fee_payer_shard_alt_authority_separation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM loyal_yield.lookup_table_families family
        WHERE family.cluster = NEW.cluster
          AND NEW.fee_payer IN (family.provisioning_authority, family.payer)
    ) OR EXISTS (
        SELECT 1
        FROM loyal_yield.route_lookup_tables route_table
        WHERE route_table.cluster = NEW.cluster
          AND NEW.fee_payer IN (route_table.authority, route_table.payer)
    ) OR EXISTS (
        SELECT 1
        FROM loyal_yield.route_policies policy
        WHERE NEW.fee_payer IN (
            policy.settings,
            policy.authority,
            policy.policy_account,
            policy.vault_pubkey
        ) OR NEW.fee_payer = ANY(policy.delegated_signers)
    ) OR EXISTS (
        SELECT 1
        FROM loyal_yield.managed_vaults vault
        WHERE NEW.fee_payer IN (vault.settings, vault.vault_pubkey)
    ) THEN
        RAISE EXCEPTION
            'fee-only route payer cannot be a policy, vault, or reusable ALT authority/payer';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS route_fee_payer_shards_alt_authority_separation
    ON loyal_yield.route_fee_payer_shards;
CREATE TRIGGER route_fee_payer_shards_alt_authority_separation
BEFORE INSERT OR UPDATE OF cluster, fee_payer, enabled
ON loyal_yield.route_fee_payer_shards
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.guard_fee_payer_shard_alt_authority_separation();

CREATE OR REPLACE FUNCTION loyal_yield.guard_alt_authority_fee_payer_shard_separation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM loyal_yield.route_fee_payer_shards shard
        WHERE shard.cluster = NEW.cluster
          AND shard.fee_payer IN (NEW.provisioning_authority, NEW.payer)
    ) THEN
        RAISE EXCEPTION
            'reusable ALT authority or payer cannot be a fee-only route payer';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS lookup_table_families_fee_payer_shard_separation
    ON loyal_yield.lookup_table_families;
CREATE TRIGGER lookup_table_families_fee_payer_shard_separation
BEFORE INSERT OR UPDATE OF cluster, provisioning_authority, payer
ON loyal_yield.lookup_table_families
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.guard_alt_authority_fee_payer_shard_separation();

CREATE OR REPLACE FUNCTION loyal_yield.guard_route_alt_fee_payer_shard_separation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM loyal_yield.route_fee_payer_shards shard
        WHERE shard.cluster = NEW.cluster
          AND shard.fee_payer IN (NEW.authority, NEW.payer)
    ) THEN
        RAISE EXCEPTION
            'route ALT authority or payer cannot be a fee-only route payer';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS route_lookup_tables_fee_payer_shard_separation
    ON loyal_yield.route_lookup_tables;
CREATE TRIGGER route_lookup_tables_fee_payer_shard_separation
BEFORE INSERT OR UPDATE OF cluster, authority, payer
ON loyal_yield.route_lookup_tables
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.guard_route_alt_fee_payer_shard_separation();

CREATE OR REPLACE FUNCTION loyal_yield.guard_policy_fee_payer_shard_separation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM loyal_yield.route_fee_payer_shards shard
        WHERE shard.fee_payer IN (
            NEW.settings,
            NEW.authority,
            NEW.policy_account,
            NEW.vault_pubkey
        ) OR shard.fee_payer = ANY(NEW.delegated_signers)
    ) THEN
        RAISE EXCEPTION 'policy or vault key cannot be a fee-only route payer';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS route_policies_fee_payer_shard_separation
    ON loyal_yield.route_policies;
CREATE TRIGGER route_policies_fee_payer_shard_separation
BEFORE INSERT OR UPDATE OF settings, authority, policy_account, vault_pubkey, delegated_signers
ON loyal_yield.route_policies
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.guard_policy_fee_payer_shard_separation();

CREATE OR REPLACE FUNCTION loyal_yield.guard_vault_fee_payer_shard_separation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM loyal_yield.route_fee_payer_shards shard
        WHERE shard.fee_payer IN (NEW.settings, NEW.vault_pubkey)
    ) THEN
        RAISE EXCEPTION 'managed vault key cannot be a fee-only route payer';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS managed_vaults_fee_payer_shard_separation
    ON loyal_yield.managed_vaults;
CREATE TRIGGER managed_vaults_fee_payer_shard_separation
BEFORE INSERT OR UPDATE OF settings, vault_pubkey
ON loyal_yield.managed_vaults
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.guard_vault_fee_payer_shard_separation();

CREATE OR REPLACE FUNCTION loyal_yield.guard_signed_route_fee_payer_role()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.fee_payer_kind = 'policy' THEN
        IF jsonb_typeof(NEW.alt_mutation_epochs -> 'tables') IS DISTINCT FROM 'array'
           OR jsonb_array_length(NEW.alt_mutation_epochs -> 'tables') = 0
        THEN
            RAISE EXCEPTION
                'policy route fee payer requires selected reusable-v2 table evidence';
        END IF;
        IF NOT EXISTS (
            SELECT 1
            FROM loyal_yield.rebalance_opportunities opportunity
            JOIN loyal_yield.managed_vaults vault
              ON vault.id = opportunity.vault_id
             AND vault.active
            JOIN loyal_yield.route_policies policy
              ON policy.id = vault.active_policy_id
             AND policy.active
            WHERE opportunity.id = NEW.opportunity_id
              AND opportunity.cluster = NEW.cluster
              AND NEW.fee_payer = ANY(policy.delegated_signers)
              AND NOT EXISTS (
                  SELECT 1
                  FROM jsonb_array_elements(
                      NEW.alt_mutation_epochs -> 'tables'
                  ) selected
                  LEFT JOIN loyal_yield.route_lookup_tables route_table
                    ON route_table.id = (selected ->> 'tableId')::BIGINT
                  LEFT JOIN loyal_yield.lookup_table_families family
                    ON family.id = route_table.family_id
                  WHERE route_table.id IS NULL
                     OR route_table.cluster <> NEW.cluster
                     OR route_table.authority <> NEW.fee_payer
                     OR route_table.payer <> NEW.fee_payer
                     OR route_table.family_id IS NULL
                     OR family.id IS NULL
                     OR family.cluster <> NEW.cluster
                     OR family.provisioning_authority <> NEW.fee_payer
                     OR family.payer <> NEW.fee_payer
              )
        ) THEN
            RAISE EXCEPTION
                'policy route fee payer is not the vault delegated signer and reusable-v2 authority/payer';
        END IF;
    ELSIF NEW.fee_payer_kind = 'fee_only_shard' THEN
        IF NOT EXISTS (
            SELECT 1
            FROM loyal_yield.route_fee_payer_shards shard
            WHERE shard.cluster = NEW.cluster
              AND shard.fee_payer = NEW.fee_payer
              AND shard.enabled
              AND NOT EXISTS (
                  SELECT 1
                  FROM loyal_yield.lookup_table_families family
                  WHERE family.cluster = shard.cluster
                    AND shard.fee_payer IN (
                        family.provisioning_authority, family.payer
                    )
              )
              AND NOT EXISTS (
                  SELECT 1
                  FROM loyal_yield.route_lookup_tables route_table
                  WHERE route_table.cluster = shard.cluster
                    AND shard.fee_payer IN (
                        route_table.authority, route_table.payer
                    )
              )
              AND NOT EXISTS (
                  SELECT 1
                  FROM loyal_yield.route_policies policy
                  WHERE shard.fee_payer IN (
                      policy.settings,
                      policy.authority,
                      policy.policy_account,
                      policy.vault_pubkey
                  ) OR shard.fee_payer = ANY(policy.delegated_signers)
              )
              AND NOT EXISTS (
                  SELECT 1
                  FROM loyal_yield.managed_vaults vault
                  WHERE shard.fee_payer IN (vault.settings, vault.vault_pubkey)
              )
        ) THEN
            RAISE EXCEPTION
                'fee-only route payer is not an enabled authority-separated shard';
        END IF;
    ELSE
        RAISE EXCEPTION 'unknown signed route fee payer kind %', NEW.fee_payer_kind;
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS signed_route_submission_fee_payer_role
    ON loyal_yield.signed_route_submissions;
CREATE TRIGGER signed_route_submission_fee_payer_role
BEFORE INSERT OR UPDATE OF
    fee_payer_kind, fee_payer, opportunity_id, cluster, alt_mutation_epochs
ON loyal_yield.signed_route_submissions
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.guard_signed_route_fee_payer_role();

CREATE INDEX IF NOT EXISTS route_fee_payer_spend_window_idx
    ON loyal_yield.route_fee_payer_spend_reservations
        (cluster, fee_payer, created_at DESC);

CREATE OR REPLACE FUNCTION loyal_yield.reject_route_fee_payer_spend_reservation_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'fee-only route payer spend reservations are immutable';
END;
$$;

DROP TRIGGER IF EXISTS route_fee_payer_spend_reservations_immutable
    ON loyal_yield.route_fee_payer_spend_reservations;
CREATE TRIGGER route_fee_payer_spend_reservations_immutable
BEFORE UPDATE OR DELETE ON loyal_yield.route_fee_payer_spend_reservations
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.reject_route_fee_payer_spend_reservation_mutation();

CREATE OR REPLACE FUNCTION loyal_yield.require_fee_only_submission_reservation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.fee_payer_kind = 'fee_only_shard'
       AND NOT EXISTS (
           SELECT 1
           FROM loyal_yield.route_fee_payer_spend_reservations reservation
           WHERE reservation.signed_submission_id = NEW.id
             AND reservation.cluster = NEW.cluster
             AND reservation.fee_payer = NEW.fee_payer
             AND reservation.opportunity_id = NEW.opportunity_id
       )
    THEN
        RAISE EXCEPTION
            'fee-only signed route submission requires an atomic spend reservation';
    END IF;
    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS signed_route_submission_requires_fee_reservation
    ON loyal_yield.signed_route_submissions;
CREATE CONSTRAINT TRIGGER signed_route_submission_requires_fee_reservation
AFTER INSERT ON loyal_yield.signed_route_submissions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
EXECUTE FUNCTION loyal_yield.require_fee_only_submission_reservation();

-- Machine-readable authority and live-budget proof for operators. In
-- particular, the constant false columns are part of the shard contract, not
-- inferred capabilities.
CREATE OR REPLACE VIEW loyal_yield.route_fee_payer_shard_status AS
SELECT
    shard.cluster,
    shard.fee_payer,
    shard.enabled,
    'fee_only'::TEXT AS payer_role,
    FALSE AS delegated_policy_signer,
    FALSE AS reusable_alt_authority,
    FALSE AS reusable_alt_payer,
    FALSE AS setup_farm_or_rent_payer,
    NOT EXISTS (
        SELECT 1
        FROM loyal_yield.lookup_table_families family
        WHERE family.cluster = shard.cluster
          AND shard.fee_payer IN (family.provisioning_authority, family.payer)
    ) AND NOT EXISTS (
        SELECT 1
        FROM loyal_yield.route_lookup_tables route_table
        WHERE route_table.cluster = shard.cluster
          AND shard.fee_payer IN (route_table.authority, route_table.payer)
    ) AND NOT EXISTS (
        SELECT 1
        FROM loyal_yield.route_policies policy
        WHERE shard.fee_payer IN (
            policy.settings,
            policy.authority,
            policy.policy_account,
            policy.vault_pubkey
        ) OR shard.fee_payer = ANY(policy.delegated_signers)
    ) AND NOT EXISTS (
        SELECT 1
        FROM loyal_yield.managed_vaults vault
        WHERE shard.fee_payer IN (vault.settings, vault.vault_pubkey)
    ) AS database_authority_separation_passes,
    shard.minimum_balance_lamports,
    shard.maximum_balance_lamports,
    shard.rolling_window_seconds,
    shard.maximum_window_spend_lamports,
    shard.maximum_transaction_fee_lamports,
    COALESCE(window_spend.reserved_spend_lamports, 0)::BIGINT
        AS current_window_reserved_lamports,
    GREATEST(
        shard.maximum_window_spend_lamports
            - COALESCE(window_spend.reserved_spend_lamports, 0),
        0
    )::BIGINT AS current_window_remaining_lamports,
    COALESCE(window_spend.reservation_count, 0)::BIGINT
        AS current_window_reservation_count,
    window_spend.last_reserved_at,
    latest_balance.observed_balance_lamports AS latest_observed_balance_lamports,
    latest_balance.observed_balance_slot AS latest_observed_balance_slot,
    latest_balance.observed_balance_at AS latest_observed_balance_at,
    COALESCE(floor_spend.unconfirmed_spend_lamports, 0)::BIGINT
        AS current_unconfirmed_floor_reservation_lamports,
    COALESCE(floor_spend.landed_after_observation_lamports, 0)::BIGINT
        AS current_landed_after_observation_lamports,
    shard.updated_at
FROM loyal_yield.route_fee_payer_shards shard
LEFT JOIN LATERAL (
    SELECT
        SUM(reservation.compiled_fee_lamports)::BIGINT AS reserved_spend_lamports,
        COUNT(*)::BIGINT AS reservation_count,
        MAX(reservation.created_at) AS last_reserved_at
    FROM loyal_yield.route_fee_payer_spend_reservations reservation
    WHERE reservation.cluster = shard.cluster
      AND reservation.fee_payer = shard.fee_payer
      AND reservation.created_at >= clock_timestamp()
          - shard.rolling_window_seconds * interval '1 second'
) window_spend ON TRUE
LEFT JOIN LATERAL (
    SELECT
        reservation.observed_balance_lamports,
        reservation.observed_balance_slot,
        reservation.observed_balance_at
    FROM loyal_yield.route_fee_payer_spend_reservations reservation
    WHERE reservation.cluster = shard.cluster
      AND reservation.fee_payer = shard.fee_payer
    ORDER BY reservation.observed_balance_at DESC, reservation.id DESC
    LIMIT 1
) latest_balance ON TRUE
LEFT JOIN LATERAL (
    SELECT
        COALESCE(SUM(reservation.compiled_fee_lamports) FILTER (
            WHERE submission.confirmed_slot IS NULL
              AND submission.submission_state NOT IN ('reconciled', 'expired', 'failed')
        ), 0)::BIGINT AS unconfirmed_spend_lamports,
        COALESCE(SUM(reservation.compiled_fee_lamports) FILTER (
            WHERE latest_balance.observed_balance_slot IS NOT NULL
              AND submission.confirmed_slot > latest_balance.observed_balance_slot
        ), 0)::BIGINT AS landed_after_observation_lamports
    FROM loyal_yield.route_fee_payer_spend_reservations reservation
    JOIN loyal_yield.signed_route_submissions submission
      ON submission.id = reservation.signed_submission_id
    WHERE reservation.cluster = shard.cluster
      AND reservation.fee_payer = shard.fee_payer
) floor_spend ON TRUE;

COMMENT ON TABLE loyal_yield.route_fee_payer_shards IS
    'Operator-managed low-balance keys authorized only to pay mature route transaction fees';
COMMENT ON TABLE loyal_yield.route_fee_payer_spend_reservations IS
    'Immutable rolling-budget reservations committed atomically with signed fleet routes';
COMMENT ON VIEW loyal_yield.route_fee_payer_shard_status IS
    'Machine-readable fee-only authority proof and rolling spend status';

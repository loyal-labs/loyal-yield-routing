use crate::fleet_orchestration::{
    MultiplyAction, MultiplyOperation, MultiplyOperationStatus, MultiplyPosition,
    MultiplyRouteState, RouteGoal, MULTIPLY_ENGINE_VERSION,
};
use crate::MultiplyPositionSnapshotInput;
use crate::{NeonSqlClient, OrchestratorError};
use chrono::{DateTime, Utc};
use serde_json::Value;
use sha2::{Digest, Sha256};
use sqlx::{postgres::PgRow, Row};

const OPERATION_COLUMNS: &str = "operation_id, route_key, cycle, engine_version, action, strategy_key, status, idempotency_key, expected_effects, policy_account, policy_data_sha256, message_sha256, signed_wire, signed_wire_sha256, transaction_signature, source_instruction_index, recent_blockhash, last_valid_block_height, broadcast_intent_at, confirmed_slot, reconciliation_sha256, created_at, updated_at";

#[derive(Clone, Debug)]
pub struct StoredMultiplyRouteState {
    pub route_key: String,
    pub settings: String,
    pub vault_index: i16,
    pub vault: String,
    pub state: MultiplyRouteState,
    pub version: i64,
    pub fencing_token: i64,
    pub current_operation: Option<MultiplyOperation>,
}

#[derive(Clone, Debug)]
pub struct ReadyEarnMaxPolicySet {
    pub settings: String,
    pub vault_index: u8,
    pub vault: String,
    pub policy_seed_base: u64,
    pub observed_slot: u64,
}

#[derive(Clone, Debug)]
pub struct MultiplyRouteLease {
    pub route_key: String,
    pub owner: String,
    pub expires_at: DateTime<Utc>,
    pub fencing_token: i64,
    pub version: i64,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SignedOperation {
    pub wire: Vec<u8>,
    pub wire_sha256: String,
    pub transaction_signature: String,
    pub recent_blockhash: String,
    pub last_valid_block_height: i64,
}

impl SignedOperation {
    pub fn new(
        wire: Vec<u8>,
        transaction_signature: String,
        recent_blockhash: String,
        last_valid_block_height: u64,
    ) -> Result<Self, OrchestratorError> {
        let last_valid_block_height = i64::try_from(last_valid_block_height).map_err(|_| {
            OrchestratorError::StoreInvariant("block height exceeds PostgreSQL BIGINT".to_owned())
        })?;
        let value = Self {
            wire_sha256: format!("{:x}", Sha256::digest(&wire)),
            wire,
            transaction_signature,
            recent_blockhash,
            last_valid_block_height,
        };
        if value.wire.is_empty()
            || value.transaction_signature.trim().is_empty()
            || value.recent_blockhash.trim().is_empty()
            || value.last_valid_block_height <= 0
        {
            return Err(OrchestratorError::StoreInvariant(
                "signed operation identity is incomplete".to_owned(),
            ));
        }
        Ok(value)
    }
}

impl NeonSqlClient {
    pub async fn record_multiply_position_snapshot(
        &self,
        input: &MultiplyPositionSnapshotInput,
    ) -> Result<bool, OrchestratorError> {
        if input.route_key.trim().is_empty() || input.generation == 0 || input.observed_slot == 0 {
            return Err(invariant("Multiply position snapshot identity is invalid"));
        }
        let generation = i64::try_from(input.generation)
            .map_err(|_| invariant("Multiply snapshot generation exceeds BIGINT"))?;
        let observed_slot = i64::try_from(input.observed_slot)
            .map_err(|_| invariant("Multiply snapshot slot exceeds BIGINT"))?;
        let valuation_slot = input
            .valuation_slot
            .map(i64::try_from)
            .transpose()
            .map_err(|_| invariant("Multiply valuation slot exceeds BIGINT"))?;
        let result = sqlx::query(
            r#"
            INSERT INTO loyal_yield.multiply_position_snapshots (
                route_key, generation, observed_slot, observed_at, strategy_key,
                claim_raw, collateral_raw, debt_raw, equity_usd_micros,
                collateral_value_usd_micros, debt_value_usd_micros,
                leverage_bps, ltv_bps, health_factor_ppm, supply_apy_bps,
                borrow_apy_bps, forecast_apy_bps, valuation_source,
                valuation_slot, valuation_observed_at, coverage_start_at
            ) VALUES (
                $1, $2, $3, $4, $5,
                $6::numeric, $7::numeric, $8::numeric, $9::numeric,
                $10::numeric, $11::numeric,
                $12, $13, $14, $15,
                $16, $17, $18,
                $19, $20, $21
            )
            ON CONFLICT (route_key, observed_slot) DO NOTHING
            "#,
        )
        .bind(&input.route_key)
        .bind(generation)
        .bind(observed_slot)
        .bind(input.observed_at)
        .bind(&input.strategy_key)
        .bind(input.claim_raw.to_string())
        .bind(input.collateral_raw.to_string())
        .bind(input.debt_raw.to_string())
        .bind(&input.equity_usd_micros)
        .bind(&input.collateral_value_usd_micros)
        .bind(&input.debt_value_usd_micros)
        .bind(
            input
                .leverage_bps
                .map(i64::try_from)
                .transpose()
                .map_err(|_| invariant("leverage exceeds BIGINT"))?,
        )
        .bind(
            input
                .ltv_bps
                .map(i64::try_from)
                .transpose()
                .map_err(|_| invariant("LTV exceeds BIGINT"))?,
        )
        .bind(
            input
                .health_factor_ppm
                .map(i64::try_from)
                .transpose()
                .map_err(|_| invariant("health factor exceeds BIGINT"))?,
        )
        .bind(
            input
                .supply_apy_bps
                .map(i64::try_from)
                .transpose()
                .map_err(|_| invariant("supply APY exceeds BIGINT"))?,
        )
        .bind(
            input
                .borrow_apy_bps
                .map(i64::try_from)
                .transpose()
                .map_err(|_| invariant("borrow APY exceeds BIGINT"))?,
        )
        .bind(input.forecast_apy_bps)
        .bind(&input.valuation_source)
        .bind(valuation_slot)
        .bind(input.valuation_observed_at)
        .bind(input.coverage_start_at)
        .execute(self.pool())
        .await?;
        Ok(result.rows_affected() == 1)
    }

    pub async fn load_unbootstrapped_earn_max_policy_set(
        &self,
    ) -> Result<Option<ReadyEarnMaxPolicySet>, OrchestratorError> {
        let row = sqlx::query(
            r#"
            SELECT policy.settings, policy.vault_index, policy.vault,
                   policy.policy_seed_base, policy.observed_slot
            FROM loyal_yield.earn_max_policy_sets policy
            WHERE policy.status = 'ready'
              AND policy.manifest_version = 'earn-max-v2'
              AND NOT EXISTS (
                  SELECT 1
                  FROM loyal_yield.multiply_route_states route
                  WHERE route.settings = policy.settings
                    AND route.vault_index = policy.vault_index
              )
            ORDER BY policy.observed_slot, policy.settings
            LIMIT 1
            "#,
        )
        .fetch_optional(self.pool())
        .await?;
        row.map(|row| {
            let vault_index: i16 = row.try_get("vault_index")?;
            let policy_seed_base: i64 = row.try_get("policy_seed_base")?;
            let observed_slot: i64 = row.try_get("observed_slot")?;
            Ok(ReadyEarnMaxPolicySet {
                settings: row.try_get("settings")?,
                vault_index: u8::try_from(vault_index)
                    .map_err(|_| invariant("Earn MAX vault index exceeds u8"))?,
                vault: row.try_get("vault")?,
                policy_seed_base: u64::try_from(policy_seed_base)
                    .map_err(|_| invariant("Earn MAX policy seed base is negative"))?,
                observed_slot: u64::try_from(observed_slot)
                    .map_err(|_| invariant("Earn MAX observed slot is negative"))?,
            })
        })
        .transpose()
    }

    pub async fn load_earn_max_policy_seed_base(
        &self,
        settings: &str,
        vault_index: u8,
    ) -> Result<Option<u64>, OrchestratorError> {
        let value: Option<i64> = sqlx::query_scalar(
            r#"
            SELECT policy_seed_base
            FROM loyal_yield.earn_max_policy_sets
            WHERE settings = $1 AND vault_index = $2
            LIMIT 1
            "#,
        )
        .bind(settings)
        .bind(i16::from(vault_index))
        .fetch_optional(self.pool())
        .await?;
        value
            .map(|seed| {
                u64::try_from(seed).map_err(|_| invariant("Earn MAX policy seed base is negative"))
            })
            .transpose()
    }

    pub async fn earn_max_policy_set_ready(
        &self,
        settings: &str,
        vault_index: u8,
        policy_seed_base: u64,
    ) -> Result<bool, OrchestratorError> {
        let ready = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM loyal_yield.earn_max_policy_sets
                WHERE settings = $1
                  AND vault_index = $2
                  AND manifest_version = 'earn-max-v2'
                  AND status = 'ready'
                  AND policy_seed_base = $3
            )
            "#,
        )
        .bind(settings)
        .bind(i16::from(vault_index))
        .bind(
            i64::try_from(policy_seed_base)
                .map_err(|_| invariant("Earn MAX policy seed base exceeds BIGINT"))?,
        )
        .fetch_one(self.pool())
        .await?;
        Ok(ready)
    }

    pub async fn create_multiply_route_state(
        &self,
        route_key: &str,
        state: &MultiplyRouteState,
    ) -> Result<bool, OrchestratorError> {
        validate_route(route_key, state)?;
        if state.generation != 1 {
            return Err(invariant("new route generation must be one"));
        }
        let encoded = encode(state)?;
        let result = sqlx::query(
            "INSERT INTO loyal_yield.multiply_route_states (route_key, settings, vault_index, vault, state) VALUES ($1, $2, $3, $4, $5) ON CONFLICT (route_key) DO NOTHING",
        )
        .bind(route_key)
        .bind(&state.settings)
        .bind(i16::from(state.vault_index))
        .bind(&state.vault)
        .bind(encoded)
        .execute(self.pool())
        .await?;
        Ok(result.rows_affected() == 1)
    }

    pub async fn load_multiply_route_state(
        &self,
        route_key: &str,
    ) -> Result<Option<StoredMultiplyRouteState>, OrchestratorError> {
        let row = sqlx::query(
            "SELECT route_key, settings, vault_index, vault, state, state_version, fencing_token FROM loyal_yield.multiply_route_states WHERE route_key=$1",
        )
        .bind(route_key)
        .fetch_optional(self.pool())
        .await?;
        let Some(row) = row else { return Ok(None) };
        let route_key: String = row.try_get("route_key")?;
        let settings: String = row.try_get("settings")?;
        let vault_index: i16 = row.try_get("vault_index")?;
        let vault: String = row.try_get("vault")?;
        let version: i64 = row.try_get("state_version")?;
        let state: MultiplyRouteState = serde_json::from_value(row.try_get::<Value, _>("state")?)
            .map_err(|error| invariant(&error.to_string()))?;
        validate_route(&route_key, &state)?;
        if state.settings != settings
            || i16::from(state.vault_index) != vault_index
            || state.vault != vault
            || u64::try_from(version).ok() != Some(state.generation)
        {
            return Err(invariant("route columns drifted from typed state"));
        }
        let current_operation = match &state.current_operation_id {
            Some(operation_id) => self.load_multiply_operation(operation_id).await?,
            None => None,
        };
        if current_operation.as_ref().is_some_and(|operation| {
            operation.route_key != route_key || operation.status.is_terminal()
        }) {
            return Err(invariant("route points at an invalid current operation"));
        }
        Ok(Some(StoredMultiplyRouteState {
            route_key,
            settings,
            vault_index,
            vault,
            state,
            version,
            fencing_token: row.try_get("fencing_token")?,
            current_operation,
        }))
    }

    pub async fn load_multiply_route_state_by_settings(
        &self,
        settings: &str,
        vault_index: u8,
    ) -> Result<Option<StoredMultiplyRouteState>, OrchestratorError> {
        if settings.trim().is_empty() {
            return Err(invariant("settings is empty"));
        }
        let key = sqlx::query_scalar::<_, String>(
            "SELECT route_key FROM loyal_yield.multiply_route_states WHERE settings=$1 AND vault_index=$2",
        )
        .bind(settings)
        .bind(i16::from(vault_index))
        .fetch_optional(self.pool())
        .await?;
        match key {
            Some(key) => self.load_multiply_route_state(&key).await,
            None => Ok(None),
        }
    }

    pub async fn load_unadmitted_multiply_route_state(
        &self,
    ) -> Result<Option<StoredMultiplyRouteState>, OrchestratorError> {
        let key = sqlx::query_scalar::<_, String>(
            r#"
            SELECT route.route_key
            FROM loyal_yield.multiply_route_states route
            INNER JOIN loyal_yield.earn_max_policy_sets policy
              ON policy.settings = route.settings
             AND policy.vault_index = route.vault_index
            WHERE policy.status = 'ready'
              AND policy.manifest_version = 'earn-max-v2'
              AND route.state ->> 'engineVersion' = 'earn_max_v2'
              AND route.state ->> 'goal' IN ('idle', 'claimed')
              AND route.state -> 'currentOperationId' = 'null'::jsonb
            ORDER BY route.updated_at, route.route_key
            LIMIT 1
            "#,
        )
        .fetch_optional(self.pool())
        .await?;
        match key {
            Some(key) => self.load_multiply_route_state(&key).await,
            None => Ok(None),
        }
    }

    pub async fn load_claimable_multiply_route_state(
        &self,
    ) -> Result<Option<StoredMultiplyRouteState>, OrchestratorError> {
        let key = sqlx::query_scalar::<_, String>(
            r#"
            SELECT route_key
            FROM loyal_yield.multiply_route_states
            WHERE state #>> '{withdrawal,status}' = 'claimable'
              AND state ->> 'engineVersion' = 'earn_max_v2'
              AND state -> 'currentOperationId' = 'null'::jsonb
            ORDER BY updated_at, route_key
            LIMIT 1
            "#,
        )
        .fetch_optional(self.pool())
        .await?;
        match key {
            Some(key) => self.load_multiply_route_state(&key).await,
            None => Ok(None),
        }
    }

    pub async fn load_multiply_operation(
        &self,
        operation_id: &str,
    ) -> Result<Option<MultiplyOperation>, OrchestratorError> {
        let query = format!(
            "SELECT {OPERATION_COLUMNS} FROM loyal_yield.multiply_operations WHERE operation_id=$1"
        );
        sqlx::query(&query)
            .bind(operation_id)
            .fetch_optional(self.pool())
            .await?
            .map(decode_operation)
            .transpose()
    }

    pub async fn multiply_operation_exists_for_signature(
        &self,
        route_key: &str,
        transaction_signature: &str,
    ) -> Result<bool, OrchestratorError> {
        let exists = sqlx::query_scalar(
            r#"
            SELECT EXISTS (
                SELECT 1
                FROM loyal_yield.multiply_operations
                WHERE route_key = $1
                  AND transaction_signature = $2
            )
            "#,
        )
        .bind(route_key)
        .bind(transaction_signature)
        .fetch_one(self.pool())
        .await?;
        Ok(exists)
    }

    pub async fn lease_next_multiply_route_state(
        &self,
        owner: &str,
        expires_at: DateTime<Utc>,
    ) -> Result<Option<MultiplyRouteLease>, OrchestratorError> {
        validate_lease_input(owner, expires_at)?;
        let row = sqlx::query(
            r#"
            WITH candidate AS (
                SELECT route.route_key
                FROM loyal_yield.multiply_route_states route
                INNER JOIN loyal_yield.earn_max_policy_sets policy
                  ON policy.settings = route.settings
                 AND policy.vault_index = route.vault_index
                 AND policy.status = 'ready'
                 AND policy.manifest_version = 'earn-max-v2'
                 AND policy.policy_seed_base = (route.state ->> 'policySeedBase')::BIGINT
                WHERE (route.lease_owner IS NULL OR route.lease_expires_at <= now())
                  AND route.state ->> 'engineVersion' = 'earn_max_v2'
                  AND (
                    route.state -> 'currentOperationId' <> 'null'::jsonb
                    OR route.state ->> 'goal' IN ('deploy', 'move')
                    OR (
                      route.state ->> 'goal' = 'withdraw'
                      AND route.state #>> '{withdrawal,status}' <> 'claimable'
                    )
                    OR (
                      route.state ->> 'goal' IN ('idle', 'claimed')
                      AND NOT EXISTS (
                        SELECT 1
                        FROM loyal_yield.multiply_position_snapshots snapshot
                        WHERE snapshot.route_key = route.route_key
                          AND snapshot.observed_at > now() - interval '5 minutes'
                      )
                    )
                  )
                  AND NOT COALESCE((
                    route.state #>> '{withdrawal,status}' = 'requested'
                    AND (route.state #>> '{withdrawal,requestedAt}')::timestamptz
                      > now() - interval '30 seconds'
                  ), FALSE)
                ORDER BY route.updated_at, route.route_key
                FOR UPDATE OF route SKIP LOCKED
                LIMIT 1
            )
            UPDATE loyal_yield.multiply_route_states route
            SET lease_owner = $1,
                lease_expires_at = $2,
                fencing_token = route.fencing_token + 1
            FROM candidate
            WHERE route.route_key = candidate.route_key
            RETURNING route.route_key, route.state_version, route.fencing_token
            "#,
        )
        .bind(owner)
        .bind(expires_at)
        .fetch_optional(self.pool())
        .await?;
        row.map(|row| decode_lease(row, owner, expires_at))
            .transpose()
    }

    pub async fn lease_multiply_route_state(
        &self,
        route_key: &str,
        owner: &str,
        expires_at: DateTime<Utc>,
    ) -> Result<Option<MultiplyRouteLease>, OrchestratorError> {
        validate_lease_input(owner, expires_at)?;
        if route_key.trim().is_empty() {
            return Err(invariant("route key is empty"));
        }
        let row = sqlx::query(
            "UPDATE loyal_yield.multiply_route_states SET lease_owner=$2, lease_expires_at=$3, fencing_token=fencing_token+1 WHERE route_key=$1 AND (lease_owner IS NULL OR lease_expires_at<=now()) RETURNING state_version, fencing_token",
        )
        .bind(route_key)
        .bind(owner)
        .bind(expires_at)
        .fetch_optional(self.pool())
        .await?;
        row.map(|row| {
            Ok(MultiplyRouteLease {
                route_key: route_key.to_owned(),
                owner: owner.to_owned(),
                expires_at,
                fencing_token: row.try_get("fencing_token")?,
                version: row.try_get("state_version")?,
            })
        })
        .transpose()
    }

    pub async fn renew_multiply_route_lease(
        &self,
        lease: &mut MultiplyRouteLease,
        expires_at: DateTime<Utc>,
    ) -> Result<bool, OrchestratorError> {
        validate_lease_input(&lease.owner, expires_at)?;
        let result = sqlx::query(
            "UPDATE loyal_yield.multiply_route_states SET lease_expires_at=$5 WHERE route_key=$1 AND state_version=$2 AND lease_owner=$3 AND fencing_token=$4 AND lease_expires_at>now()",
        )
        .bind(&lease.route_key)
        .bind(lease.version)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(expires_at)
        .execute(self.pool())
        .await?;
        if result.rows_affected() == 1 {
            lease.expires_at = expires_at;
            Ok(true)
        } else {
            Ok(false)
        }
    }

    pub async fn save_multiply_route_state(
        &self,
        lease: &mut MultiplyRouteLease,
        state: &MultiplyRouteState,
    ) -> Result<bool, OrchestratorError> {
        validate_next_route(lease, state)?;
        let encoded = encode(state)?;
        let row = sqlx::query(
            "UPDATE loyal_yield.multiply_route_states SET state=$5, state_version=state_version+1, updated_at=now() WHERE route_key=$1 AND state_version=$2 AND lease_owner=$3 AND fencing_token=$4 AND lease_expires_at>now() RETURNING state_version",
        )
        .bind(&lease.route_key)
        .bind(lease.version)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(encoded)
        .fetch_optional(self.pool())
        .await?;
        advance_lease_version(lease, row)
    }

    pub async fn prepare_multiply_operation(
        &self,
        lease: &mut MultiplyRouteLease,
        route: &MultiplyRouteState,
        operation: &MultiplyOperation,
    ) -> Result<bool, OrchestratorError> {
        validate_next_route(lease, route)?;
        operation
            .validate()
            .map_err(|error| invariant(&error.to_string()))?;
        if operation.status != MultiplyOperationStatus::Prepared
            || operation.route_key != lease.route_key
            || route.current_operation_id.as_deref() != Some(&operation.operation_id)
            || operation.cycle != route.cycle
        {
            return Err(invariant("prepared operation is not bound to its route"));
        }
        let mut tx = self.pool().begin().await?;
        let inserted = sqlx::query(
            "INSERT INTO loyal_yield.multiply_operations (operation_id, route_key, cycle, engine_version, action, strategy_key, status, idempotency_key, expected_effects, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,'prepared',$7,$8,$9,$9) ON CONFLICT (idempotency_key) DO NOTHING",
        )
        .bind(&operation.operation_id)
        .bind(&operation.route_key)
        .bind(i64_from_u64(operation.cycle, "cycle")?)
        .bind(MULTIPLY_ENGINE_VERSION)
        .bind(operation.action.as_str())
        .bind(operation.strategy_key.map(|key| key.as_str()))
        .bind(&operation.idempotency_key)
        .bind(serde_json::to_value(&operation.expected_effects).map_err(|error| invariant(&error.to_string()))?)
        .bind(operation.created_at)
        .execute(&mut *tx)
        .await?;
        if inserted.rows_affected() != 1 {
            tx.rollback().await?;
            return Ok(false);
        }
        let row = sqlx::query(
            "UPDATE loyal_yield.multiply_route_states SET state=$5, state_version=state_version+1, updated_at=now() WHERE route_key=$1 AND state_version=$2 AND lease_owner=$3 AND fencing_token=$4 AND lease_expires_at>now() RETURNING state_version",
        )
        .bind(&lease.route_key)
        .bind(lease.version)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(encode(route)?)
        .fetch_optional(&mut *tx)
        .await?;
        let Some(row) = row else {
            tx.rollback().await?;
            return Ok(false);
        };
        let version: i64 = row.try_get("state_version")?;
        tx.commit().await?;
        lease.version = version;
        Ok(true)
    }

    pub async fn admit_external_multiply_operation(
        &self,
        lease: &mut MultiplyRouteLease,
        route: &MultiplyRouteState,
        operation: &MultiplyOperation,
    ) -> Result<bool, OrchestratorError> {
        validate_next_route(lease, route)?;
        operation
            .validate()
            .map_err(|error| invariant(&error.to_string()))?;
        if operation.status != MultiplyOperationStatus::Reconciled
            || !matches!(
                operation.action,
                crate::fleet_orchestration::MultiplyAction::DepositClaimAsset
                    | crate::fleet_orchestration::MultiplyAction::Claim
            )
            || operation.route_key != lease.route_key
            || operation.cycle != route.cycle
            || route.current_operation_id.is_some()
        {
            return Err(invariant("external operation is not bound to its route"));
        }
        let mut tx = self.pool().begin().await?;
        let inserted = sqlx::query(
            "INSERT INTO loyal_yield.multiply_operations (operation_id, route_key, cycle, engine_version, action, strategy_key, status, idempotency_key, expected_effects, message_sha256, signed_wire_sha256, transaction_signature, source_instruction_index, recent_blockhash, confirmed_slot, reconciliation_sha256, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$6,'reconciled',$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$16) ON CONFLICT DO NOTHING",
        )
        .bind(&operation.operation_id)
        .bind(&operation.route_key)
        .bind(i64_from_u64(operation.cycle, "cycle")?)
        .bind(MULTIPLY_ENGINE_VERSION)
        .bind(operation.action.as_str())
        .bind(operation.strategy_key.map(|key| key.as_str()))
        .bind(&operation.idempotency_key)
        .bind(serde_json::to_value(&operation.expected_effects).map_err(|error| invariant(&error.to_string()))?)
        .bind(&operation.message_sha256)
        .bind(&operation.signed_wire_sha256)
        .bind(&operation.transaction_signature)
        .bind(operation.source_instruction_index.map(i32::from))
        .bind(&operation.recent_blockhash)
        .bind(operation.confirmed_slot.map(|slot| i64_from_u64(slot, "slot")).transpose()?)
        .bind(&operation.reconciliation_sha256)
        .bind(operation.created_at)
        .execute(&mut *tx)
        .await?;
        if inserted.rows_affected() != 1 {
            tx.rollback().await?;
            return Ok(false);
        }
        let row = update_route_in_transaction(&mut tx, lease, route).await?;
        let Some(version) = row else {
            tx.rollback().await?;
            return Ok(false);
        };
        if operation.action == MultiplyAction::Claim {
            let MultiplyPosition::Idle { claim } = &route.position else {
                tx.rollback().await?;
                return Err(invariant("Claimed route must have an idle position"));
            };
            let confirmed_slot = operation
                .confirmed_slot
                .ok_or_else(|| invariant("Claim operation omitted its confirmed slot"))?;
            let snapshot = sqlx::query(
                r#"
                INSERT INTO loyal_yield.multiply_position_snapshots (
                    route_key, generation, observed_slot, observed_at, strategy_key,
                    claim_raw, collateral_raw, debt_raw, equity_usd_micros,
                    collateral_value_usd_micros, debt_value_usd_micros,
                    leverage_bps, ltv_bps, health_factor_ppm, supply_apy_bps,
                    borrow_apy_bps, forecast_apy_bps, valuation_source,
                    valuation_slot, valuation_observed_at, coverage_start_at
                ) VALUES (
                    $1, $2, $3, $4, NULL,
                    $5::numeric, 0, 0, $5::numeric,
                    0, 0,
                    NULL, NULL, NULL, NULL,
                    NULL, NULL, 'confirmed_claim_transfer',
                    $3, $4, $6
                )
                ON CONFLICT (route_key, observed_slot) DO NOTHING
                "#,
            )
            .bind(&route.route_key)
            .bind(i64_from_u64(route.generation, "generation")?)
            .bind(i64_from_u64(confirmed_slot, "slot")?)
            .bind(route.observed_at)
            .bind(claim.amount_raw.to_string())
            .bind(
                route
                    .deposit
                    .as_ref()
                    .map(|deposit| deposit.observed_at)
                    .unwrap_or(route.observed_at),
            )
            .execute(&mut *tx)
            .await?;
            if snapshot.rows_affected() != 1 {
                tx.rollback().await?;
                return Err(invariant("Claim snapshot observed slot already exists"));
            }
        }
        tx.commit().await?;
        lease.version = version;
        Ok(true)
    }

    pub async fn persist_signed_operation(
        &self,
        lease: &MultiplyRouteLease,
        operation_id: &str,
        policy_account: &str,
        policy_data_sha256: &str,
        message_sha256: &str,
        signed: &SignedOperation,
    ) -> Result<bool, OrchestratorError> {
        validate_hash(policy_data_sha256)?;
        validate_hash(message_sha256)?;
        let result = sqlx::query(
            "UPDATE loyal_yield.multiply_operations operation SET status='signed_persisted', policy_account=$6, policy_data_sha256=$7, message_sha256=$8, signed_wire=$9, signed_wire_sha256=$10, transaction_signature=$11, recent_blockhash=$12, last_valid_block_height=$13, updated_at=now() FROM loyal_yield.multiply_route_states route WHERE operation.operation_id=$1 AND operation.route_key=$2 AND operation.status='prepared' AND route.route_key=operation.route_key AND route.state_version=$3 AND route.lease_owner=$4 AND route.fencing_token=$5 AND route.lease_expires_at>now()",
        )
        .bind(operation_id)
        .bind(&lease.route_key)
        .bind(lease.version)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(policy_account)
        .bind(policy_data_sha256)
        .bind(message_sha256)
        .bind(&signed.wire)
        .bind(&signed.wire_sha256)
        .bind(&signed.transaction_signature)
        .bind(&signed.recent_blockhash)
        .bind(signed.last_valid_block_height)
        .execute(self.pool())
        .await?;
        Ok(result.rows_affected() == 1)
    }

    pub async fn mark_multiply_broadcast_intent(
        &self,
        lease: &MultiplyRouteLease,
        operation_id: &str,
        at: DateTime<Utc>,
    ) -> Result<bool, OrchestratorError> {
        self.advance_operation(
            lease,
            operation_id,
            "signed_persisted",
            "broadcast_intent",
            Some(at),
            None,
        )
        .await
    }

    pub async fn mark_multiply_confirmed(
        &self,
        lease: &MultiplyRouteLease,
        operation_id: &str,
        confirmed_slot: u64,
    ) -> Result<bool, OrchestratorError> {
        self.advance_operation(
            lease,
            operation_id,
            "broadcast_intent",
            "confirmed",
            None,
            Some(confirmed_slot),
        )
        .await
    }

    async fn advance_operation(
        &self,
        lease: &MultiplyRouteLease,
        operation_id: &str,
        from: &str,
        to: &str,
        broadcast_at: Option<DateTime<Utc>>,
        confirmed_slot: Option<u64>,
    ) -> Result<bool, OrchestratorError> {
        let confirmed_slot = confirmed_slot
            .map(|slot| i64_from_u64(slot, "slot"))
            .transpose()?;
        let result = sqlx::query(
            "UPDATE loyal_yield.multiply_operations operation SET status=$7, broadcast_intent_at=COALESCE($8, operation.broadcast_intent_at), confirmed_slot=COALESCE($9, operation.confirmed_slot), updated_at=now() FROM loyal_yield.multiply_route_states route WHERE operation.operation_id=$1 AND operation.route_key=$2 AND operation.status=$6 AND route.route_key=operation.route_key AND route.state_version=$3 AND route.lease_owner=$4 AND route.fencing_token=$5 AND route.lease_expires_at>now()",
        )
        .bind(operation_id)
        .bind(&lease.route_key)
        .bind(lease.version)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(from)
        .bind(to)
        .bind(broadcast_at)
        .bind(confirmed_slot)
        .execute(self.pool())
        .await?;
        Ok(result.rows_affected() == 1)
    }

    pub async fn cancel_prepared_multiply_operation(
        &self,
        lease: &mut MultiplyRouteLease,
        operation_id: &str,
        route: &MultiplyRouteState,
    ) -> Result<bool, OrchestratorError> {
        validate_next_route(lease, route)?;
        if route.current_operation_id.is_some() {
            return Err(invariant("cancelled route must clear current operation"));
        }
        let mut tx = self.pool().begin().await?;
        let deleted = sqlx::query(
            "DELETE FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND route_key=$2 AND status='prepared' RETURNING operation_id",
        )
        .bind(operation_id)
        .bind(&lease.route_key)
        .fetch_optional(&mut *tx)
        .await?;
        if deleted.is_none() {
            tx.rollback().await?;
            return Ok(false);
        }
        let row = update_route_in_transaction(&mut tx, lease, route).await?;
        let Some(version) = row else {
            tx.rollback().await?;
            return Ok(false);
        };
        tx.commit().await?;
        lease.version = version;
        Ok(true)
    }

    pub async fn expire_multiply_operation(
        &self,
        lease: &mut MultiplyRouteLease,
        operation_id: &str,
        route: &MultiplyRouteState,
    ) -> Result<bool, OrchestratorError> {
        validate_next_route(lease, route)?;
        if route.current_operation_id.is_some() {
            return Err(invariant("expired route must clear current operation"));
        }
        let mut tx = self.pool().begin().await?;
        let result = sqlx::query(
            "UPDATE loyal_yield.multiply_operations SET status='expired', signed_wire=NULL, updated_at=now() WHERE operation_id=$1 AND route_key=$2 AND status IN ('signed_persisted','broadcast_intent') AND last_valid_block_height IS NOT NULL RETURNING operation_id",
        )
        .bind(operation_id)
        .bind(&lease.route_key)
        .fetch_optional(&mut *tx)
        .await?;
        if result.is_none() {
            tx.rollback().await?;
            return Ok(false);
        }
        let row = update_route_in_transaction(&mut tx, lease, route).await?;
        let Some(version) = row else {
            tx.rollback().await?;
            return Ok(false);
        };
        tx.commit().await?;
        lease.version = version;
        Ok(true)
    }

    pub async fn mark_multiply_manual_recovery(
        &self,
        lease: &mut MultiplyRouteLease,
        operation_id: &str,
        route: &MultiplyRouteState,
    ) -> Result<bool, OrchestratorError> {
        validate_next_route(lease, route)?;
        if route.current_operation_id.is_some() || route.goal != RouteGoal::ManualRecovery {
            return Err(invariant("manual recovery route is not terminal"));
        }
        let mut tx = self.pool().begin().await?;
        let operation = sqlx::query(
            "UPDATE loyal_yield.multiply_operations SET status='manual_recovery', updated_at=now() WHERE operation_id=$1 AND route_key=$2 AND status IN ('signed_persisted','broadcast_intent','confirmed','reconciliation_pending') RETURNING operation_id",
        )
        .bind(operation_id)
        .bind(&lease.route_key)
        .fetch_optional(&mut *tx)
        .await?;
        if operation.is_none() {
            tx.rollback().await?;
            return Ok(false);
        }
        let row = update_route_in_transaction(&mut tx, lease, route).await?;
        let Some(version) = row else {
            tx.rollback().await?;
            return Ok(false);
        };
        tx.commit().await?;
        lease.version = version;
        Ok(true)
    }

    pub async fn reconcile_multiply_operation(
        &self,
        lease: &mut MultiplyRouteLease,
        operation_id: &str,
        transaction_signature: &str,
        reconciliation_sha256: &str,
        confirmed_slot: u64,
        route: &MultiplyRouteState,
    ) -> Result<bool, OrchestratorError> {
        validate_hash(reconciliation_sha256)?;
        validate_next_route(lease, route)?;
        if route.current_operation_id.is_some() {
            return Err(invariant("reconciled route must clear current operation"));
        }
        let slot = i64_from_u64(confirmed_slot, "slot")?;
        let mut tx = self.pool().begin().await?;
        let operation = sqlx::query(
            "UPDATE loyal_yield.multiply_operations SET status='reconciled', signed_wire=NULL, confirmed_slot=$3, reconciliation_sha256=$4, updated_at=now() WHERE operation_id=$1 AND route_key=$2 AND status IN ('confirmed','reconciliation_pending') AND transaction_signature=$5 RETURNING operation_id",
        )
        .bind(operation_id)
        .bind(&lease.route_key)
        .bind(slot)
        .bind(reconciliation_sha256)
        .bind(transaction_signature)
        .fetch_optional(&mut *tx)
        .await?;
        if operation.is_none() {
            tx.rollback().await?;
            return Ok(false);
        }
        let row = sqlx::query(
            "UPDATE loyal_yield.multiply_route_states SET state=$5, state_version=state_version+1, updated_at=now() WHERE route_key=$1 AND state_version=$2 AND lease_owner=$3 AND fencing_token=$4 AND lease_expires_at>now() RETURNING state_version",
        )
        .bind(&lease.route_key)
        .bind(lease.version)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .bind(encode(route)?)
        .fetch_optional(&mut *tx)
        .await?;
        let Some(row) = row else {
            tx.rollback().await?;
            return Ok(false);
        };
        let version: i64 = row.try_get("state_version")?;
        tx.commit().await?;
        lease.version = version;
        Ok(true)
    }

    pub async fn release_multiply_route_lease(
        &self,
        lease: &MultiplyRouteLease,
    ) -> Result<bool, OrchestratorError> {
        let result = sqlx::query(
            "UPDATE loyal_yield.multiply_route_states SET lease_owner=NULL, lease_expires_at=NULL WHERE route_key=$1 AND state_version=$2 AND lease_owner=$3 AND fencing_token=$4",
        )
        .bind(&lease.route_key)
        .bind(lease.version)
        .bind(&lease.owner)
        .bind(lease.fencing_token)
        .execute(self.pool())
        .await?;
        Ok(result.rows_affected() == 1)
    }
}

fn validate_route(route_key: &str, state: &MultiplyRouteState) -> Result<(), OrchestratorError> {
    state
        .validate_persisted()
        .map_err(|error| invariant(&error.to_string()))?;
    if route_key != state.route_key {
        return Err(invariant("route key does not match typed state"));
    }
    Ok(())
}

fn validate_next_route(
    lease: &MultiplyRouteLease,
    state: &MultiplyRouteState,
) -> Result<(), OrchestratorError> {
    validate_route(&lease.route_key, state)?;
    if i64::try_from(state.generation).ok() != lease.version.checked_add(1) {
        return Err(invariant("route generation must advance exactly once"));
    }
    Ok(())
}

fn validate_lease_input(owner: &str, expires_at: DateTime<Utc>) -> Result<(), OrchestratorError> {
    if owner.trim().is_empty() || expires_at <= Utc::now() {
        Err(invariant("lease owner or expiry is invalid"))
    } else {
        Ok(())
    }
}

fn validate_hash(value: &str) -> Result<(), OrchestratorError> {
    if value.len() == 64 && value.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        Ok(())
    } else {
        Err(invariant("expected a 64-character hexadecimal hash"))
    }
}

fn i64_from_u64(value: u64, field: &str) -> Result<i64, OrchestratorError> {
    i64::try_from(value).map_err(|_| invariant(&format!("{field} exceeds PostgreSQL BIGINT")))
}

fn encode(state: &MultiplyRouteState) -> Result<Value, OrchestratorError> {
    serde_json::to_value(state).map_err(|error| invariant(&error.to_string()))
}

fn advance_lease_version(
    lease: &mut MultiplyRouteLease,
    row: Option<PgRow>,
) -> Result<bool, OrchestratorError> {
    if let Some(row) = row {
        lease.version = row.try_get("state_version")?;
        Ok(true)
    } else {
        Ok(false)
    }
}

async fn update_route_in_transaction(
    tx: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    lease: &MultiplyRouteLease,
    route: &MultiplyRouteState,
) -> Result<Option<i64>, OrchestratorError> {
    let row = sqlx::query(
        "UPDATE loyal_yield.multiply_route_states SET state=$5, state_version=state_version+1, updated_at=now() WHERE route_key=$1 AND state_version=$2 AND lease_owner=$3 AND fencing_token=$4 AND lease_expires_at>now() RETURNING state_version",
    )
    .bind(&lease.route_key)
    .bind(lease.version)
    .bind(&lease.owner)
    .bind(lease.fencing_token)
    .bind(encode(route)?)
    .fetch_optional(&mut **tx)
    .await?;
    row.map(|row| row.try_get("state_version"))
        .transpose()
        .map_err(Into::into)
}

fn decode_lease(
    row: PgRow,
    owner: &str,
    expires_at: DateTime<Utc>,
) -> Result<MultiplyRouteLease, OrchestratorError> {
    Ok(MultiplyRouteLease {
        route_key: row.try_get("route_key")?,
        owner: owner.to_owned(),
        expires_at,
        fencing_token: row.try_get("fencing_token")?,
        version: row.try_get("state_version")?,
    })
}

fn decode_operation(row: PgRow) -> Result<MultiplyOperation, OrchestratorError> {
    let action = serde_json::from_value(Value::String(row.try_get("action")?))
        .map_err(|error| invariant(&error.to_string()))?;
    let status = serde_json::from_value(Value::String(row.try_get("status")?))
        .map_err(|error| invariant(&error.to_string()))?;
    let strategy_key = row
        .try_get::<Option<String>, _>("strategy_key")?
        .map(|value| serde_json::from_value(Value::String(value)))
        .transpose()
        .map_err(|error| invariant(&error.to_string()))?;
    let cycle: i64 = row.try_get("cycle")?;
    let last_valid: Option<i64> = row.try_get("last_valid_block_height")?;
    let confirmed_slot: Option<i64> = row.try_get("confirmed_slot")?;
    let operation = MultiplyOperation {
        operation_id: row.try_get("operation_id")?,
        route_key: row.try_get("route_key")?,
        cycle: u64::try_from(cycle).map_err(|_| invariant("operation cycle is invalid"))?,
        engine_version: row.try_get("engine_version")?,
        action,
        strategy_key,
        status,
        idempotency_key: row.try_get("idempotency_key")?,
        expected_effects: serde_json::from_value(row.try_get("expected_effects")?)
            .map_err(|error| invariant(&error.to_string()))?,
        policy_account: row.try_get("policy_account")?,
        policy_data_sha256: row.try_get("policy_data_sha256")?,
        message_sha256: row.try_get("message_sha256")?,
        signed_wire: row.try_get("signed_wire")?,
        signed_wire_sha256: row.try_get("signed_wire_sha256")?,
        transaction_signature: row.try_get("transaction_signature")?,
        source_instruction_index: row
            .try_get::<Option<i32>, _>("source_instruction_index")?
            .map(|value| {
                u16::try_from(value).map_err(|_| invariant("source instruction index is invalid"))
            })
            .transpose()?,
        recent_blockhash: row.try_get("recent_blockhash")?,
        last_valid_block_height: last_valid
            .map(|value| u64::try_from(value).map_err(|_| invariant("operation expiry is invalid")))
            .transpose()?,
        broadcast_intent_at: row.try_get("broadcast_intent_at")?,
        confirmed_slot: confirmed_slot
            .map(|value| u64::try_from(value).map_err(|_| invariant("operation slot is invalid")))
            .transpose()?,
        reconciliation_sha256: row.try_get("reconciliation_sha256")?,
        created_at: row.try_get("created_at")?,
        updated_at: row.try_get("updated_at")?,
    };
    operation
        .validate()
        .map_err(|error| invariant(&error.to_string()))?;
    Ok(operation)
}

fn invariant(message: &str) -> OrchestratorError {
    OrchestratorError::StoreInvariant(message.to_owned())
}

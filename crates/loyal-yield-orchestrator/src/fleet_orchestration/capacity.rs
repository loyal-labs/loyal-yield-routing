use super::queue::RebalanceOpportunityLease;
use super::{
    evaluate_economics, route_fee_budget, EconomicPolicy, OpportunityInput, RouteFeePolicy,
};
use crate::{DecisionId, NeonSqlClient, OrchestratorError};
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use sqlx::{PgConnection, Row};

/// Fleet admission intentionally limits one planning/execution wave to two
/// percent of the currently observed target supply, with a small floor for a
/// newly listed reserve. This is the same ceiling used by the planner's final
/// capacity band; execution rechecks it against durable concurrent inflow.
pub fn maximum_target_inflight_usd_micros(observed_supply_usd_micros: i64) -> i64 {
    (observed_supply_usd_micros / 50).max(4_000_000)
}

/// Dilution model shared by execution-time revalidation and admission. Active
/// reservations are supply that is not yet reflected by the market snapshot.
pub fn projected_target_apy_bps(
    observed_target_apy_bps: i64,
    observed_supply_usd_micros: i64,
    committed_inflow_usd_micros: i64,
) -> Result<i64, OrchestratorError> {
    if observed_supply_usd_micros < 0 || committed_inflow_usd_micros < 0 {
        return Err(OrchestratorError::StoreInvariant(
            "target capacity projection requires nonnegative supply and inflow".to_owned(),
        ));
    }
    if observed_supply_usd_micros == 0 || committed_inflow_usd_micros == 0 {
        return Ok(observed_target_apy_bps);
    }
    let numerator = i128::from(observed_target_apy_bps)
        .checked_mul(i128::from(observed_supply_usd_micros))
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant("target capacity APY numerator overflowed".to_owned())
        })?;
    let denominator = i128::from(observed_supply_usd_micros)
        .checked_add(i128::from(committed_inflow_usd_micros))
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "target capacity APY denominator overflowed".to_owned(),
            )
        })?;
    i64::try_from(numerator / denominator).map_err(|_| {
        OrchestratorError::StoreInvariant("target capacity APY does not fit i64".to_owned())
    })
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TargetCapacityObservation {
    pub cluster: String,
    pub target_reserve: String,
    pub liquidity_mint: String,
    pub observed_supply_usd_micros: i64,
    pub observed_slot: i64,
    pub maximum_inflight_usd_micros: i64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TargetCapacityProjection {
    pub observation: TargetCapacityObservation,
    /// Changes only when the immutable target-market observation changes.
    /// Concurrent reservations from this observation do not invalidate it.
    pub telemetry_version: i64,
    /// Audit cursor for reservations already serialized at the frontier.
    /// This is deliberately not a build-invalidating compare-and-swap fence.
    pub reservation_generation: i64,
    pub committed_inflow_usd_micros: i64,
    pub available_inflight_usd_micros: i64,
    pub released_after_telemetry_count: u64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TargetCapacityReservationInput {
    pub projection: TargetCapacityProjection,
    pub principal_usd_micros: i64,
    /// Current-route economic inputs are replayed while holding the target
    /// frontier lock after all earlier reservations have become visible.
    pub economic_opportunity: OpportunityInput,
    pub current_observed_target_apy_bps: i64,
    pub economic_policy: EconomicPolicy,
    pub fee_policy: RouteFeePolicy,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TargetCapacityReservationState {
    Active,
    AwaitingTelemetry,
    Released,
}

impl TargetCapacityReservationState {
    fn parse(value: &str) -> Result<Self, OrchestratorError> {
        match value {
            "active" => Ok(Self::Active),
            "awaiting_telemetry" => Ok(Self::AwaitingTelemetry),
            "released" => Ok(Self::Released),
            other => Err(OrchestratorError::StoreInvariant(format!(
                "unknown target-capacity reservation state {other:?}"
            ))),
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct TargetCapacityReservationRecord {
    pub id: i64,
    pub cluster: String,
    pub target_reserve: String,
    pub liquidity_mint: String,
    pub opportunity_id: i64,
    pub decision_id: Option<DecisionId>,
    pub signed_submission_id: Option<i64>,
    pub principal_usd_micros: i64,
    pub admitted_observed_supply_usd_micros: i64,
    pub admitted_observed_slot: i64,
    pub admitted_maximum_inflight_usd_micros: i64,
    pub admitted_telemetry_version: i64,
    pub reservation_generation: i64,
    pub admitted_observed_target_apy_bps: i64,
    pub admitted_projected_target_apy_bps: i64,
    pub admitted_source_apy_bps: i64,
    pub admitted_edge_bps: i64,
    pub admitted_net_holding_gain_usd_micros: i64,
    pub admitted_fee_cap_lamports: i64,
    pub reservation_fencing_token: i64,
    pub state_version: i64,
    pub state: TargetCapacityReservationState,
    pub movement_slot: Option<i64>,
    pub released_at: Option<DateTime<Utc>>,
    pub release_reason: Option<String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

impl NeonSqlClient {
    /// Incorporates a fresh target observation and releases only movements the
    /// observation is new enough to contain. The returned version is an
    /// telemetry fence: decision admission fails only if the underlying market
    /// observation changes before the signed handoff commits. Reservation
    /// churn is serialized and re-evaluated without invalidating siblings.
    pub async fn observe_target_capacity(
        &self,
        observation: TargetCapacityObservation,
    ) -> Result<TargetCapacityProjection, OrchestratorError> {
        validate_observation(&observation)?;
        let mut tx = self.pool().begin().await?;
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.target_capacity_frontiers
                (cluster, target_reserve, liquidity_mint,
                 observed_supply_usd_micros, observed_slot,
                 maximum_inflight_usd_micros)
            VALUES ($1, $2, $3, $4, $5, $6)
            ON CONFLICT (cluster, target_reserve, liquidity_mint) DO NOTHING
            "#,
        )
        .bind(&observation.cluster)
        .bind(&observation.target_reserve)
        .bind(&observation.liquidity_mint)
        .bind(observation.observed_supply_usd_micros)
        .bind(observation.observed_slot)
        .bind(observation.maximum_inflight_usd_micros)
        .execute(&mut *tx)
        .await?;

        let frontier = sqlx::query(
            r#"
            SELECT observed_supply_usd_micros, observed_slot,
                   maximum_inflight_usd_micros, telemetry_version,
                   reservation_generation
            FROM loyal_yield.target_capacity_frontiers
            WHERE cluster = $1 AND target_reserve = $2 AND liquidity_mint = $3
            FOR UPDATE
            "#,
        )
        .bind(&observation.cluster)
        .bind(&observation.target_reserve)
        .bind(&observation.liquidity_mint)
        .fetch_one(&mut *tx)
        .await?;
        let durable_slot: i64 = frontier.try_get("observed_slot")?;
        let durable_supply: i64 = frontier.try_get("observed_supply_usd_micros")?;
        let durable_maximum: i64 = frontier.try_get("maximum_inflight_usd_micros")?;
        if observation.observed_slot < durable_slot {
            return Err(OrchestratorError::StoreInvariant(format!(
                "target capacity observation slot {} is older than durable slot {}",
                observation.observed_slot, durable_slot
            )));
        }
        if observation.observed_slot == durable_slot
            && (observation.observed_supply_usd_micros != durable_supply
                || observation.maximum_inflight_usd_micros != durable_maximum)
        {
            return Err(OrchestratorError::StoreInvariant(
                "target capacity observation conflicts with durable evidence at the same slot"
                    .to_owned(),
            ));
        }

        if observation.observed_slot > durable_slot {
            sqlx::query(
                r#"
                UPDATE loyal_yield.target_capacity_frontiers
                SET observed_supply_usd_micros = $4,
                    observed_slot = $5,
                    maximum_inflight_usd_micros = $6,
                    telemetry_version = telemetry_version + 1,
                    updated_at = now()
                WHERE cluster = $1 AND target_reserve = $2 AND liquidity_mint = $3
                "#,
            )
            .bind(&observation.cluster)
            .bind(&observation.target_reserve)
            .bind(&observation.liquidity_mint)
            .bind(observation.observed_supply_usd_micros)
            .bind(observation.observed_slot)
            .bind(observation.maximum_inflight_usd_micros)
            .execute(&mut *tx)
            .await?;
        }

        let released = sqlx::query(
            r#"
            UPDATE loyal_yield.target_capacity_reservations
            SET reservation_state = 'released',
                released_at = now(),
                release_reason = 'target_telemetry_reflected_movement',
                state_version = state_version + 1,
                updated_at = now()
            WHERE cluster = $1 AND target_reserve = $2 AND liquidity_mint = $3
              AND reservation_state = 'awaiting_telemetry'
              -- Equal-slot ordering is ambiguous without a transaction index;
              -- only strictly newer telemetry can prove reflection.
              AND movement_slot < $4
            "#,
        )
        .bind(&observation.cluster)
        .bind(&observation.target_reserve)
        .bind(&observation.liquidity_mint)
        .bind(observation.observed_slot)
        .execute(&mut *tx)
        .await?;
        let committed_inflow_usd_micros: i64 = sqlx::query_scalar(
            r#"
            SELECT COALESCE(SUM(principal_usd_micros), 0)::BIGINT
            FROM loyal_yield.target_capacity_reservations
            WHERE cluster = $1 AND target_reserve = $2 AND liquidity_mint = $3
              AND reservation_state <> 'released'
            "#,
        )
        .bind(&observation.cluster)
        .bind(&observation.target_reserve)
        .bind(&observation.liquidity_mint)
        .fetch_one(&mut *tx)
        .await?;
        let versions = sqlx::query(
            r#"
            SELECT telemetry_version, reservation_generation
            FROM loyal_yield.target_capacity_frontiers
            WHERE cluster = $1 AND target_reserve = $2 AND liquidity_mint = $3
            "#,
        )
        .bind(&observation.cluster)
        .bind(&observation.target_reserve)
        .bind(&observation.liquidity_mint)
        .fetch_one(&mut *tx)
        .await?;
        let telemetry_version: i64 = versions.try_get("telemetry_version")?;
        let reservation_generation: i64 = versions.try_get("reservation_generation")?;
        let available_inflight_usd_micros = observation
            .maximum_inflight_usd_micros
            .saturating_sub(committed_inflow_usd_micros)
            .max(0);
        tx.commit().await?;
        Ok(TargetCapacityProjection {
            observation,
            telemetry_version,
            reservation_generation,
            committed_inflow_usd_micros,
            available_inflight_usd_micros,
            released_after_telemetry_count: released.rows_affected(),
        })
    }

    /// Reserves capacity inside the caller's signed-decision transaction.
    /// The market observation must still match, but sibling reservations do
    /// not stale the build. Instead, the target-local lock exposes all earlier
    /// inflow so dilution, route economics, and the signed fee are rechecked
    /// against the exact admission order.
    #[doc(hidden)]
    pub async fn reserve_target_capacity_in_connection(
        connection: &mut PgConnection,
        opportunity_lease: &RebalanceOpportunityLease,
        input: &TargetCapacityReservationInput,
        compiled_fee_lamports: i64,
    ) -> Result<TargetCapacityReservationRecord, OrchestratorError> {
        validate_reservation_input(opportunity_lease, input, compiled_fee_lamports)?;
        let observation = &input.projection.observation;
        let frontier = sqlx::query(
            r#"
            SELECT observed_supply_usd_micros, observed_slot,
                   maximum_inflight_usd_micros, telemetry_version
            FROM loyal_yield.target_capacity_frontiers
            WHERE cluster = $1 AND target_reserve = $2 AND liquidity_mint = $3
            FOR UPDATE
            "#,
        )
        .bind(&observation.cluster)
        .bind(&observation.target_reserve)
        .bind(&observation.liquidity_mint)
        .fetch_optional(&mut *connection)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "target capacity frontier disappeared before decision admission".to_owned(),
            )
        })?;
        let durable_supply: i64 = frontier.try_get("observed_supply_usd_micros")?;
        let durable_slot: i64 = frontier.try_get("observed_slot")?;
        let durable_maximum: i64 = frontier.try_get("maximum_inflight_usd_micros")?;
        let durable_telemetry_version: i64 = frontier.try_get("telemetry_version")?;
        if durable_supply != observation.observed_supply_usd_micros
            || durable_slot != observation.observed_slot
            || durable_maximum != observation.maximum_inflight_usd_micros
            || durable_telemetry_version != input.projection.telemetry_version
        {
            return Err(OrchestratorError::StoreInvariant(
                "target capacity telemetry changed after economic revalidation; retry from fresh telemetry"
                    .to_owned(),
            ));
        }

        let committed: i64 = sqlx::query_scalar(
            r#"
            SELECT COALESCE(SUM(principal_usd_micros), 0)::BIGINT
            FROM loyal_yield.target_capacity_reservations
            WHERE cluster = $1 AND target_reserve = $2 AND liquidity_mint = $3
              AND reservation_state <> 'released'
            "#,
        )
        .bind(&observation.cluster)
        .bind(&observation.target_reserve)
        .bind(&observation.liquidity_mint)
        .fetch_one(&mut *connection)
        .await?;
        let next_committed = committed
            .checked_add(input.principal_usd_micros)
            .ok_or_else(|| {
                OrchestratorError::StoreInvariant(
                    "target capacity committed inflow overflowed".to_owned(),
                )
            })?;
        if next_committed > durable_maximum {
            return Err(OrchestratorError::StoreInvariant(format!(
                "target capacity exhausted: requested {}, committed {}, maximum {} USD micros",
                input.principal_usd_micros, committed, durable_maximum
            )));
        }

        let admitted_projected_target_apy_bps = projected_target_apy_bps(
            input.current_observed_target_apy_bps,
            durable_supply,
            next_committed,
        )?;
        let mut economic_opportunity = input.economic_opportunity.clone();
        economic_opportunity.target_net_apy_bps = input.current_observed_target_apy_bps;
        let economic_score = evaluate_economics(
            &economic_opportunity,
            &input.economic_policy,
            admitted_projected_target_apy_bps,
        )
        .map_err(|reason| {
            OrchestratorError::StoreInvariant(format!(
                "target capacity atomic economics became ineligible after committed inflow: {reason:?}"
            ))
        })?;
        let fee_budget = route_fee_budget(
            economic_score.net_holding_gain_usd_micros,
            input.fee_policy,
        )
        .map_err(|reason| {
            OrchestratorError::StoreInvariant(format!(
                "target capacity atomic fee budget became ineligible after committed inflow: {reason:?}"
            ))
        })?;
        if compiled_fee_lamports > fee_budget.cap_lamports {
            return Err(OrchestratorError::StoreInvariant(format!(
                "target capacity atomic fee cap exceeded after committed inflow: compiled {}, cap {} lamports",
                compiled_fee_lamports, fee_budget.cap_lamports
            )));
        }

        let reservation_generation: i64 = sqlx::query_scalar(
            r#"
            UPDATE loyal_yield.target_capacity_frontiers
            SET reservation_generation = reservation_generation + 1,
                updated_at = now()
            WHERE cluster = $1 AND target_reserve = $2 AND liquidity_mint = $3
            RETURNING reservation_generation
            "#,
        )
        .bind(&observation.cluster)
        .bind(&observation.target_reserve)
        .bind(&observation.liquidity_mint)
        .fetch_one(&mut *connection)
        .await?;
        let row = sqlx::query(
            r#"
            INSERT INTO loyal_yield.target_capacity_reservations
                (cluster, target_reserve, liquidity_mint, opportunity_id,
                 principal_usd_micros, admitted_observed_supply_usd_micros,
                 admitted_observed_slot, admitted_maximum_inflight_usd_micros,
                 admitted_telemetry_version, reservation_generation,
                 admitted_observed_target_apy_bps,
                 admitted_projected_target_apy_bps, admitted_source_apy_bps,
                 admitted_edge_bps, admitted_net_holding_gain_usd_micros,
                 admitted_fee_cap_lamports, reservation_fencing_token)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
                    $13, $14, $15, $16, $17)
            RETURNING *
            "#,
        )
        .bind(&observation.cluster)
        .bind(&observation.target_reserve)
        .bind(&observation.liquidity_mint)
        .bind(opportunity_lease.opportunity.id)
        .bind(input.principal_usd_micros)
        .bind(observation.observed_supply_usd_micros)
        .bind(observation.observed_slot)
        .bind(observation.maximum_inflight_usd_micros)
        .bind(durable_telemetry_version)
        .bind(reservation_generation)
        .bind(input.current_observed_target_apy_bps)
        .bind(admitted_projected_target_apy_bps)
        .bind(economic_opportunity.source_net_apy_bps)
        .bind(economic_score.capacity_adjusted_net_edge_bps)
        .bind(economic_score.net_holding_gain_usd_micros)
        .bind(fee_budget.cap_lamports)
        .bind(opportunity_lease.fencing_token)
        .fetch_one(&mut *connection)
        .await?;
        target_capacity_reservation_from_row(&row)
    }

    pub(crate) async fn attach_target_capacity_reservation_in_connection(
        connection: &mut PgConnection,
        opportunity_lease: &RebalanceOpportunityLease,
        decision_id: DecisionId,
        signed_submission_id: i64,
    ) -> Result<TargetCapacityReservationRecord, OrchestratorError> {
        let row = sqlx::query(
            r#"
            UPDATE loyal_yield.target_capacity_reservations reservation
            SET decision_id = $3,
                signed_submission_id = $4,
                state_version = state_version + 1,
                updated_at = now()
            FROM loyal_yield.signed_route_submissions submission
            WHERE reservation.opportunity_id = $1
              AND reservation.reservation_fencing_token = $2
              AND reservation.reservation_state = 'active'
              AND reservation.decision_id IS NULL
              AND reservation.signed_submission_id IS NULL
              AND submission.id = $4
              AND submission.opportunity_id = reservation.opportunity_id
              AND submission.decision_id = $3
              AND submission.executor_fencing_token = $2
            RETURNING reservation.*
            "#,
        )
        .bind(opportunity_lease.opportunity.id)
        .bind(opportunity_lease.fencing_token)
        .bind(decision_id.as_i64())
        .bind(signed_submission_id)
        .fetch_optional(&mut *connection)
        .await?
        .ok_or_else(|| {
            OrchestratorError::StoreInvariant(
                "target capacity reservation could not attach to its signed decision handoff"
                    .to_owned(),
            )
        })?;
        target_capacity_reservation_from_row(&row)
    }

    /// Cleanup is intentionally limited to an unattached pre-handoff row and
    /// requires both independent fences. Once signed bytes are linked, only a
    /// proven terminal submission transition may release capacity.
    pub async fn release_unattached_target_capacity_reservation(
        &self,
        opportunity_id: i64,
        expected_state_version: i64,
        expected_reservation_fencing_token: i64,
        reason: &str,
    ) -> Result<bool, OrchestratorError> {
        if opportunity_id <= 0
            || expected_state_version <= 0
            || expected_reservation_fencing_token <= 0
            || reason.trim().is_empty()
            || reason.len() > 256
        {
            return Err(OrchestratorError::StoreInvariant(
                "target capacity release requires identity, state/fencing versions, and bounded reason"
                    .to_owned(),
            ));
        }
        let mut tx = self.pool().begin().await?;
        let identity = sqlx::query(
            r#"
            SELECT cluster, target_reserve, liquidity_mint
            FROM loyal_yield.target_capacity_reservations
            WHERE opportunity_id = $1
            "#,
        )
        .bind(opportunity_id)
        .fetch_optional(&mut *tx)
        .await?;
        let Some(identity) = identity else {
            tx.rollback().await?;
            return Ok(false);
        };
        let cluster: String = identity.try_get("cluster")?;
        let target_reserve: String = identity.try_get("target_reserve")?;
        let liquidity_mint: String = identity.try_get("liquidity_mint")?;
        sqlx::query(
            r#"
            SELECT 1
            FROM loyal_yield.target_capacity_frontiers
            WHERE cluster = $1 AND target_reserve = $2 AND liquidity_mint = $3
            FOR UPDATE
            "#,
        )
        .bind(&cluster)
        .bind(&target_reserve)
        .bind(&liquidity_mint)
        .fetch_one(&mut *tx)
        .await?;
        let released = sqlx::query(
            r#"
            UPDATE loyal_yield.target_capacity_reservations
            SET reservation_state = 'released',
                released_at = now(),
                release_reason = $4,
                state_version = state_version + 1,
                updated_at = now()
            WHERE opportunity_id = $1
              AND state_version = $2
              AND reservation_fencing_token = $3
              AND reservation_state = 'active'
              AND decision_id IS NULL
              AND signed_submission_id IS NULL
            RETURNING id
            "#,
        )
        .bind(opportunity_id)
        .bind(expected_state_version)
        .bind(expected_reservation_fencing_token)
        .bind(reason)
        .fetch_optional(&mut *tx)
        .await?;
        if released.is_none() {
            tx.rollback().await?;
            return Ok(false);
        }
        tx.commit().await?;
        Ok(true)
    }
}

fn validate_observation(observation: &TargetCapacityObservation) -> Result<(), OrchestratorError> {
    if observation.cluster.trim().is_empty()
        || observation.target_reserve.trim().is_empty()
        || observation.liquidity_mint.trim().is_empty()
        || observation.observed_supply_usd_micros < 0
        || observation.observed_slot < 0
        || observation.maximum_inflight_usd_micros <= 0
    {
        return Err(OrchestratorError::StoreInvariant(
            "target capacity observation requires identity and nonnegative bounded telemetry"
                .to_owned(),
        ));
    }
    Ok(())
}

fn validate_reservation_input(
    opportunity_lease: &RebalanceOpportunityLease,
    input: &TargetCapacityReservationInput,
    compiled_fee_lamports: i64,
) -> Result<(), OrchestratorError> {
    validate_observation(&input.projection.observation)?;
    let opportunity = &opportunity_lease.opportunity;
    if input.principal_usd_micros <= 0
        || compiled_fee_lamports <= 0
        || input.projection.telemetry_version < 0
        || input.projection.reservation_generation < 0
        || opportunity_lease.fencing_token <= 0
        || opportunity_lease.claim_kind != super::queue::RebalanceOpportunityClaimKind::Execute
        || opportunity.cluster != input.projection.observation.cluster
        || opportunity.target_reserve != input.projection.observation.target_reserve
        || opportunity.liquidity_mint != input.projection.observation.liquidity_mint
        || opportunity.principal_usd_micros != input.principal_usd_micros
        || input.economic_opportunity.opportunity_id != opportunity.id
        || input.economic_opportunity.optimizer_epoch_id != opportunity.optimizer_epoch_id
        || input.economic_opportunity.vault_id != opportunity.vault_id.as_i64()
        || input.economic_opportunity.mint != opportunity.liquidity_mint
        || input.economic_opportunity.target_reserve != opportunity.target_reserve
        || input.economic_opportunity.notional_usd_micros != input.principal_usd_micros
        || input.economic_opportunity.target_net_apy_bps != input.current_observed_target_apy_bps
        || opportunity
            .source_reserve
            .as_ref()
            .is_some_and(|source| source != &input.economic_opportunity.source_reserve)
    {
        return Err(OrchestratorError::StoreInvariant(
            "target capacity reservation does not match its execute opportunity and fence"
                .to_owned(),
        ));
    }
    Ok(())
}

fn target_capacity_reservation_from_row(
    row: &sqlx::postgres::PgRow,
) -> Result<TargetCapacityReservationRecord, OrchestratorError> {
    Ok(TargetCapacityReservationRecord {
        id: row.try_get("id")?,
        cluster: row.try_get("cluster")?,
        target_reserve: row.try_get("target_reserve")?,
        liquidity_mint: row.try_get("liquidity_mint")?,
        opportunity_id: row.try_get("opportunity_id")?,
        decision_id: row
            .try_get::<Option<i64>, _>("decision_id")?
            .map(DecisionId),
        signed_submission_id: row.try_get("signed_submission_id")?,
        principal_usd_micros: row.try_get("principal_usd_micros")?,
        admitted_observed_supply_usd_micros: row.try_get("admitted_observed_supply_usd_micros")?,
        admitted_observed_slot: row.try_get("admitted_observed_slot")?,
        admitted_maximum_inflight_usd_micros: row
            .try_get("admitted_maximum_inflight_usd_micros")?,
        admitted_telemetry_version: row.try_get("admitted_telemetry_version")?,
        reservation_generation: row.try_get("reservation_generation")?,
        admitted_observed_target_apy_bps: row.try_get("admitted_observed_target_apy_bps")?,
        admitted_projected_target_apy_bps: row.try_get("admitted_projected_target_apy_bps")?,
        admitted_source_apy_bps: row.try_get("admitted_source_apy_bps")?,
        admitted_edge_bps: row.try_get("admitted_edge_bps")?,
        admitted_net_holding_gain_usd_micros: row
            .try_get("admitted_net_holding_gain_usd_micros")?,
        admitted_fee_cap_lamports: row.try_get("admitted_fee_cap_lamports")?,
        reservation_fencing_token: row.try_get("reservation_fencing_token")?,
        state_version: row.try_get("state_version")?,
        state: TargetCapacityReservationState::parse(row.try_get("reservation_state")?)?,
        movement_slot: row.try_get("movement_slot")?,
        released_at: row.try_get("released_at")?,
        release_reason: row.try_get("release_reason")?,
        created_at: row.try_get("created_at")?,
        updated_at: row.try_get("updated_at")?,
    })
}

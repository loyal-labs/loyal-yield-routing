use chrono::{DateTime, Utc};
use serde_json::Value;
use sha2::{Digest, Sha256};
use sqlx::FromRow;
use std::time::Duration;

use crate::pipeline::{
    AttemptStatus, BatchStatus, DecisionWorkItem, ReadyAttempt, ReserveTarget,
    ReserveTargetCandidate, VaultReconcileJob,
};
use crate::{DecisionId, NeonSqlClient, OrchestratorError, VaultId};

#[derive(Debug, FromRow)]
struct ReserveTargetRow {
    id: i64,
    cluster: String,
    strategy: String,
    liquidity_mint: String,
    target_reserve: String,
    target_market: Option<String>,
    target_supply_apy_bps: i64,
    target_epoch: String,
    stale: bool,
}

#[derive(Debug, FromRow)]
struct VaultReconcileJobRow {
    id: i64,
    vault_id: i64,
    target_id: Option<i64>,
    cluster: String,
    liquidity_mint: String,
    target_reserve: String,
    target_epoch: String,
    attempt_count: i32,
}

#[derive(Debug, FromRow)]
struct DecisionWorkItemRow {
    decision_id: i64,
    vault_id: i64,
    cluster: String,
    liquidity_mint: String,
    source_reserve: String,
    target_reserve: String,
    amount_raw: i64,
}

#[derive(Debug, FromRow)]
struct ReadyAttemptRow {
    attempt_id: i64,
    decision_id: i64,
    vault_id: i64,
    cluster: String,
    liquidity_mint: String,
    source_reserve: String,
    target_reserve: String,
    amount_raw: i64,
    estimated_compute_units: Option<i64>,
}

impl NeonSqlClient {
    pub async fn upsert_worker_cursor(
        &self,
        worker_kind: &str,
        cluster: &str,
        partition_key: &str,
        cursor: Value,
        observed_at: Option<DateTime<Utc>>,
    ) -> Result<(), OrchestratorError> {
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.worker_cursors
                (worker_kind, cluster, partition_key, cursor, observed_at)
            VALUES ($1, $2, $3, $4, $5)
            ON CONFLICT (worker_kind, cluster, partition_key) DO UPDATE SET
                cursor = EXCLUDED.cursor,
                observed_at = EXCLUDED.observed_at,
                updated_at = now()
            "#,
        )
        .bind(worker_kind)
        .bind(cluster)
        .bind(partition_key)
        .bind(cursor)
        .bind(observed_at)
        .execute(self.pool())
        .await?;
        Ok(())
    }

    pub async fn upsert_reserve_target(
        &self,
        candidate: ReserveTargetCandidate,
    ) -> Result<ReserveTarget, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        let previous_target: Option<String> = sqlx::query_scalar(
            r#"
            SELECT target_reserve
            FROM loyal_yield.reserve_targets_current
            WHERE cluster = $1 AND strategy = $2 AND liquidity_mint = $3
            FOR UPDATE
            "#,
        )
        .bind(&candidate.cluster)
        .bind(&candidate.strategy)
        .bind(&candidate.liquidity_mint)
        .fetch_optional(&mut *tx)
        .await?;

        let row = sqlx::query_as::<_, ReserveTargetRow>(
            r#"
            INSERT INTO loyal_yield.reserve_targets_current
                (cluster, strategy, liquidity_mint, target_reserve, target_market,
                 target_supply_apy_bps, observed_slot, observed_at, source_cursor,
                 filters, target_epoch, stale)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, FALSE)
            ON CONFLICT (cluster, strategy, liquidity_mint) DO UPDATE SET
                target_reserve = EXCLUDED.target_reserve,
                target_market = EXCLUDED.target_market,
                target_supply_apy_bps = EXCLUDED.target_supply_apy_bps,
                observed_slot = EXCLUDED.observed_slot,
                observed_at = EXCLUDED.observed_at,
                source_cursor = EXCLUDED.source_cursor,
                filters = EXCLUDED.filters,
                target_epoch = EXCLUDED.target_epoch,
                stale = FALSE,
                updated_at = now()
            RETURNING
                id,
                cluster,
                strategy,
                liquidity_mint,
                target_reserve,
                target_market,
                target_supply_apy_bps,
                target_epoch,
                stale
            "#,
        )
        .bind(&candidate.cluster)
        .bind(&candidate.strategy)
        .bind(&candidate.liquidity_mint)
        .bind(&candidate.target_reserve)
        .bind(&candidate.target_market)
        .bind(candidate.target_supply_apy_bps)
        .bind(candidate.observed_slot)
        .bind(candidate.observed_at)
        .bind(&candidate.source_cursor)
        .bind(&candidate.filters)
        .bind(&candidate.target_epoch)
        .fetch_one(&mut *tx)
        .await?;

        let reason = match previous_target.as_deref() {
            None => "initial_target",
            Some(previous) if previous == candidate.target_reserve => "target_refreshed",
            Some(_) => "target_changed",
        };
        sqlx::query(
            r#"
            INSERT INTO loyal_yield.reserve_target_snapshots
                (target_id, cluster, strategy, liquidity_mint, target_reserve,
                 target_market, target_supply_apy_bps, previous_target_reserve,
                 observed_slot, observed_at, source_cursor, reason)
            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
            "#,
        )
        .bind(row.id)
        .bind(&candidate.cluster)
        .bind(&candidate.strategy)
        .bind(&candidate.liquidity_mint)
        .bind(&candidate.target_reserve)
        .bind(&candidate.target_market)
        .bind(candidate.target_supply_apy_bps)
        .bind(previous_target)
        .bind(candidate.observed_slot)
        .bind(candidate.observed_at)
        .bind(candidate.source_cursor)
        .bind(reason)
        .execute(&mut *tx)
        .await?;

        tx.commit().await?;
        Ok(reserve_target_from_row(row))
    }

    pub async fn reserve_targets(
        &self,
        cluster: &str,
        strategy: &str,
    ) -> Result<Vec<ReserveTarget>, OrchestratorError> {
        let rows = sqlx::query_as::<_, ReserveTargetRow>(
            r#"
            SELECT
                id,
                cluster,
                strategy,
                liquidity_mint,
                target_reserve,
                target_market,
                target_supply_apy_bps,
                target_epoch,
                stale
            FROM loyal_yield.reserve_targets_current
            WHERE cluster = $1 AND strategy = $2 AND NOT stale
            ORDER BY liquidity_mint
            "#,
        )
        .bind(cluster)
        .bind(strategy)
        .fetch_all(self.pool())
        .await?;
        Ok(rows.into_iter().map(reserve_target_from_row).collect())
    }

    pub async fn enqueue_reconcile_jobs_for_target(
        &self,
        target: &ReserveTarget,
        limit: i64,
    ) -> Result<u64, OrchestratorError> {
        let idempotency_suffix = format!(
            "{}:{}:{}",
            target.liquidity_mint, target.target_reserve, target.target_epoch
        );
        let result = sqlx::query(
            r#"
            INSERT INTO loyal_yield.vault_reconcile_jobs
                (vault_id, target_id, cluster, liquidity_mint, target_reserve,
                 target_epoch, idempotency_key)
            SELECT
                vault.id,
                $1,
                vault.cluster,
                $2,
                $3,
                $4,
                vault.id::text || ':' || $5
            FROM loyal_yield.managed_vaults vault
            JOIN loyal_yield.route_policies policy
                ON policy.id = vault.active_policy_id
            WHERE vault.cluster = $6
              AND vault.active
              AND policy.active
              AND policy.route_modes @> ARRAY['same_mint']::TEXT[]
              AND policy.kamino_liquidity_mints @> ARRAY[$2]::TEXT[]
              AND ($7::TEXT IS NULL OR policy.kamino_markets @> ARRAY[$7]::TEXT[])
              AND NOT EXISTS (
                  SELECT 1
                  FROM loyal_yield.rebalance_decisions decision
                  WHERE decision.vault_id = vault.id
                    AND decision.status::text = ANY($8)
              )
            ORDER BY vault.id
            LIMIT $9
            ON CONFLICT (idempotency_key) DO NOTHING
            "#,
        )
        .bind(target.id)
        .bind(&target.liquidity_mint)
        .bind(&target.target_reserve)
        .bind(&target.target_epoch)
        .bind(idempotency_suffix)
        .bind(&target.cluster)
        .bind(&target.target_market)
        .bind(active_decision_status_strings())
        .bind(limit)
        .execute(self.pool())
        .await?;
        Ok(result.rows_affected())
    }

    pub async fn claim_reconcile_jobs(
        &self,
        worker_id: &str,
        limit: i64,
        lease_for: Duration,
    ) -> Result<Vec<VaultReconcileJob>, OrchestratorError> {
        let rows = sqlx::query_as::<_, VaultReconcileJobRow>(
            r#"
            UPDATE loyal_yield.vault_reconcile_jobs
            SET
                status = 'leased'::loyal_yield.worker_job_status,
                lease_owner = $1,
                lease_expires_at = now() + ($2::TEXT)::INTERVAL,
                attempt_count = attempt_count + 1,
                updated_at = now()
            WHERE id IN (
                SELECT id
                FROM loyal_yield.vault_reconcile_jobs
                WHERE status IN ('pending', 'failed')
                  AND next_attempt_at <= now()
                ORDER BY created_at ASC
                LIMIT $3
                FOR UPDATE SKIP LOCKED
            )
            RETURNING
                id,
                vault_id,
                target_id,
                cluster,
                liquidity_mint,
                target_reserve,
                target_epoch,
                attempt_count
            "#,
        )
        .bind(worker_id)
        .bind(interval_literal(lease_for))
        .bind(limit)
        .fetch_all(self.pool())
        .await?;
        Ok(rows.into_iter().map(vault_job_from_row).collect())
    }

    pub async fn complete_reconcile_job(
        &self,
        job_id: i64,
        worker_id: &str,
    ) -> Result<bool, OrchestratorError> {
        let result = sqlx::query(
            r#"
            UPDATE loyal_yield.vault_reconcile_jobs
            SET status = 'succeeded'::loyal_yield.worker_job_status,
                updated_at = now()
            WHERE id = $1
              AND lease_owner = $2
              AND status = 'leased'::loyal_yield.worker_job_status
            "#,
        )
        .bind(job_id)
        .bind(worker_id)
        .execute(self.pool())
        .await?;
        Ok(result.rows_affected() == 1)
    }

    pub async fn fail_reconcile_job(
        &self,
        job_id: i64,
        worker_id: &str,
        error_code: &str,
        error_message: &str,
        retry_after: Duration,
    ) -> Result<bool, OrchestratorError> {
        let result = sqlx::query(
            r#"
            UPDATE loyal_yield.vault_reconcile_jobs
            SET status = 'failed'::loyal_yield.worker_job_status,
                last_error_code = $3,
                last_error_message = $4,
                next_attempt_at = now() + ($5::TEXT)::INTERVAL,
                lease_owner = NULL,
                lease_expires_at = NULL,
                updated_at = now()
            WHERE id = $1
              AND lease_owner = $2
              AND status = 'leased'::loyal_yield.worker_job_status
            "#,
        )
        .bind(job_id)
        .bind(worker_id)
        .bind(error_code)
        .bind(error_message)
        .bind(interval_literal(retry_after))
        .execute(self.pool())
        .await?;
        Ok(result.rows_affected() == 1)
    }

    pub async fn claim_planned_decisions_for_simulation(
        &self,
        limit: i64,
    ) -> Result<Vec<DecisionWorkItem>, OrchestratorError> {
        let rows = sqlx::query_as::<_, DecisionWorkItemRow>(
            r#"
            UPDATE loyal_yield.rebalance_decisions decision
            SET status = 'simulating'::loyal_yield.decision_status,
                updated_at = now()
            FROM loyal_yield.managed_vaults vault
            WHERE decision.id IN (
                SELECT id
                FROM loyal_yield.rebalance_decisions
                WHERE status = 'planned'::loyal_yield.decision_status
                ORDER BY created_at ASC
                LIMIT $1
                FOR UPDATE SKIP LOCKED
            )
              AND vault.id = decision.vault_id
            RETURNING
                decision.id AS decision_id,
                decision.vault_id,
                vault.cluster,
                decision.liquidity_mint AS "liquidity_mint!",
                decision.source_reserve AS "source_reserve!",
                decision.target_reserve AS "target_reserve!",
                decision.amount_raw AS "amount_raw!"
            "#,
        )
        .bind(limit)
        .fetch_all(self.pool())
        .await?;
        Ok(rows.into_iter().map(decision_work_item_from_row).collect())
    }

    pub async fn record_rebalance_attempt(
        &self,
        decision: &DecisionWorkItem,
        status: AttemptStatus,
        worker_id: &str,
        simulation_slot: Option<i64>,
        compute_units: Option<i64>,
        error_code: Option<&str>,
        error_message: Option<&str>,
        payload: Value,
    ) -> Result<i64, OrchestratorError> {
        let row_id: i64 = sqlx::query_scalar(
            r#"
            INSERT INTO loyal_yield.rebalance_attempts
                (decision_id, status, worker_id, simulation_slot, compute_units,
                 error_code, error_message, payload, idempotency_key)
            VALUES ($1, $2::TEXT::loyal_yield.rebalance_attempt_status, $3, $4, $5, $6, $7, $8, $9)
            ON CONFLICT (idempotency_key) DO UPDATE SET
                status = EXCLUDED.status,
                worker_id = EXCLUDED.worker_id,
                simulation_slot = EXCLUDED.simulation_slot,
                compute_units = EXCLUDED.compute_units,
                error_code = EXCLUDED.error_code,
                error_message = EXCLUDED.error_message,
                payload = EXCLUDED.payload,
                updated_at = now()
            RETURNING id
            "#,
        )
        .bind(decision.decision_id.as_i64())
        .bind(status.as_str())
        .bind(worker_id)
        .bind(simulation_slot)
        .bind(compute_units)
        .bind(error_code)
        .bind(error_message)
        .bind(payload)
        .bind(attempt_idempotency_key(decision, status))
        .fetch_one(self.pool())
        .await?;
        Ok(row_id)
    }

    pub async fn claim_ready_attempts_for_batch(
        &self,
        worker_id: &str,
        limit: i64,
    ) -> Result<Vec<ReadyAttempt>, OrchestratorError> {
        let rows = sqlx::query_as::<_, ReadyAttemptRow>(
            r#"
            UPDATE loyal_yield.rebalance_attempts attempt
            SET status = 'batched'::loyal_yield.rebalance_attempt_status,
                worker_id = $1,
                updated_at = now()
            FROM loyal_yield.rebalance_decisions decision,
                 loyal_yield.managed_vaults vault
            WHERE attempt.id IN (
                SELECT attempt.id
                FROM loyal_yield.rebalance_attempts attempt
                JOIN loyal_yield.rebalance_decisions decision
                    ON decision.id = attempt.decision_id
                WHERE attempt.status = 'ready'::loyal_yield.rebalance_attempt_status
                  AND decision.status = 'ready'::loyal_yield.decision_status
                ORDER BY attempt.created_at ASC
                LIMIT $2
                FOR UPDATE SKIP LOCKED
            )
              AND decision.id = attempt.decision_id
              AND vault.id = decision.vault_id
            RETURNING
                attempt.id AS attempt_id,
                decision.id AS decision_id,
                decision.vault_id,
                vault.cluster,
                decision.liquidity_mint AS "liquidity_mint!",
                decision.source_reserve AS "source_reserve!",
                decision.target_reserve AS "target_reserve!",
                decision.amount_raw AS "amount_raw!",
                attempt.compute_units AS estimated_compute_units
            "#,
        )
        .bind(worker_id)
        .bind(limit)
        .fetch_all(self.pool())
        .await?;
        Ok(rows.into_iter().map(ready_attempt_from_row).collect())
    }

    pub async fn ready_attempts_for_batch(
        &self,
        limit: i64,
    ) -> Result<Vec<ReadyAttempt>, OrchestratorError> {
        let rows = sqlx::query_as::<_, ReadyAttemptRow>(
            r#"
            SELECT
                attempt.id AS attempt_id,
                decision.id AS decision_id,
                decision.vault_id,
                vault.cluster,
                decision.liquidity_mint AS "liquidity_mint!",
                decision.source_reserve AS "source_reserve!",
                decision.target_reserve AS "target_reserve!",
                decision.amount_raw AS "amount_raw!",
                attempt.compute_units AS estimated_compute_units
            FROM loyal_yield.rebalance_attempts attempt
            JOIN loyal_yield.rebalance_decisions decision
                ON decision.id = attempt.decision_id
            JOIN loyal_yield.managed_vaults vault
                ON vault.id = decision.vault_id
            WHERE attempt.status = 'ready'::loyal_yield.rebalance_attempt_status
              AND decision.status = 'ready'::loyal_yield.decision_status
            ORDER BY attempt.created_at ASC
            LIMIT $1
            "#,
        )
        .bind(limit)
        .fetch_all(self.pool())
        .await?;
        Ok(rows.into_iter().map(ready_attempt_from_row).collect())
    }

    pub async fn insert_rebalance_batch(
        &self,
        cluster: &str,
        signer: &str,
        fee_payer: &str,
        status: BatchStatus,
        attempts: &[ReadyAttempt],
        signed_transaction: Option<Vec<u8>>,
        signature: Option<&str>,
        payload: Value,
    ) -> Result<i64, OrchestratorError> {
        let mut tx = self.pool().begin().await?;
        let batch_id: i64 = sqlx::query_scalar(
            r#"
            INSERT INTO loyal_yield.rebalance_batches
                (cluster, status, signer, fee_payer, signed_transaction, signature, payload, idempotency_key)
            VALUES ($1, $2::TEXT::loyal_yield.rebalance_batch_status, $3, $4, $5, $6, $7, $8)
            ON CONFLICT (idempotency_key) DO UPDATE SET updated_at = now()
            RETURNING id
            "#,
        )
        .bind(cluster)
        .bind(status.as_str())
        .bind(signer)
        .bind(fee_payer)
        .bind(signed_transaction)
        .bind(signature)
        .bind(payload)
        .bind(batch_idempotency_key(attempts))
        .fetch_one(&mut *tx)
        .await?;

        for (index, attempt) in attempts.iter().enumerate() {
            sqlx::query(
                r#"
                INSERT INTO loyal_yield.rebalance_batch_decisions
                    (batch_id, decision_id, attempt_id, position)
                VALUES ($1, $2, $3, $4)
                ON CONFLICT (batch_id, decision_id) DO NOTHING
                "#,
            )
            .bind(batch_id)
            .bind(attempt.decision.decision_id.as_i64())
            .bind(attempt.attempt_id)
            .bind(i16::try_from(index).map_err(|_| {
                OrchestratorError::StoreInvariant("batch position exceeds SMALLINT".to_owned())
            })?)
            .execute(&mut *tx)
            .await?;
        }

        tx.commit().await?;
        Ok(batch_id)
    }

    pub async fn sweep_expired_reconcile_leases(
        &self,
        max_attempts: i32,
    ) -> Result<(u64, u64), OrchestratorError> {
        let released = sqlx::query(
            r#"
            UPDATE loyal_yield.vault_reconcile_jobs
            SET status = 'pending'::loyal_yield.worker_job_status,
                lease_owner = NULL,
                lease_expires_at = NULL,
                next_attempt_at = now(),
                updated_at = now()
            WHERE status = 'leased'::loyal_yield.worker_job_status
              AND lease_expires_at <= now()
              AND attempt_count < $1
            "#,
        )
        .bind(max_attempts)
        .execute(self.pool())
        .await?
        .rows_affected();

        let dead = sqlx::query(
            r#"
            UPDATE loyal_yield.vault_reconcile_jobs
            SET status = 'dead'::loyal_yield.worker_job_status,
                lease_owner = NULL,
                lease_expires_at = NULL,
                last_error_code = COALESCE(last_error_code, 'retry_budget_exhausted'),
                last_error_message = COALESCE(last_error_message, 'retry budget exhausted while sweeping expired reconcile lease'),
                updated_at = now()
            WHERE status = 'leased'::loyal_yield.worker_job_status
              AND lease_expires_at <= now()
              AND attempt_count >= $1
            "#,
        )
        .bind(max_attempts)
        .execute(self.pool())
        .await?
        .rows_affected();

        Ok((released, dead))
    }
}

fn reserve_target_from_row(row: ReserveTargetRow) -> ReserveTarget {
    ReserveTarget {
        id: row.id,
        cluster: row.cluster,
        strategy: row.strategy,
        liquidity_mint: row.liquidity_mint,
        target_reserve: row.target_reserve,
        target_market: row.target_market,
        target_supply_apy_bps: row.target_supply_apy_bps,
        target_epoch: row.target_epoch,
        stale: row.stale,
    }
}

fn vault_job_from_row(row: VaultReconcileJobRow) -> VaultReconcileJob {
    VaultReconcileJob {
        id: row.id,
        vault_id: VaultId(row.vault_id),
        target_id: row.target_id,
        cluster: row.cluster,
        liquidity_mint: row.liquidity_mint,
        target_reserve: row.target_reserve,
        target_epoch: row.target_epoch,
        attempt_count: row.attempt_count,
    }
}

fn decision_work_item_from_row(row: DecisionWorkItemRow) -> DecisionWorkItem {
    DecisionWorkItem {
        decision_id: DecisionId(row.decision_id),
        vault_id: VaultId(row.vault_id),
        cluster: row.cluster,
        liquidity_mint: row.liquidity_mint,
        source_reserve: row.source_reserve,
        target_reserve: row.target_reserve,
        amount_raw: row.amount_raw,
    }
}

fn ready_attempt_from_row(row: ReadyAttemptRow) -> ReadyAttempt {
    ReadyAttempt {
        attempt_id: row.attempt_id,
        decision: DecisionWorkItem {
            decision_id: DecisionId(row.decision_id),
            vault_id: VaultId(row.vault_id),
            cluster: row.cluster,
            liquidity_mint: row.liquidity_mint,
            source_reserve: row.source_reserve,
            target_reserve: row.target_reserve,
            amount_raw: row.amount_raw,
        },
        estimated_compute_units: row.estimated_compute_units,
    }
}

fn interval_literal(duration: Duration) -> String {
    format!("{} milliseconds", duration.as_millis())
}

fn active_decision_status_strings() -> Vec<String> {
    crate::ACTIVE_DECISION_STATUSES
        .iter()
        .map(|status| (*status).to_owned())
        .collect()
}

fn attempt_idempotency_key(decision: &DecisionWorkItem, status: AttemptStatus) -> String {
    let mut hasher = Sha256::new();
    hasher.update(b"attempt");
    hasher.update(decision.decision_id.as_i64().to_le_bytes());
    hasher.update(status.as_str().as_bytes());
    format!("{:x}", hasher.finalize())
}

fn batch_idempotency_key(attempts: &[ReadyAttempt]) -> String {
    let mut hasher = Sha256::new();
    hasher.update(b"batch");
    for attempt in attempts {
        hasher.update(attempt.attempt_id.to_le_bytes());
        hasher.update(attempt.decision.decision_id.as_i64().to_le_bytes());
    }
    format!("{:x}", hasher.finalize())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::pipeline::DecisionWorkItem;

    #[test]
    fn attempt_idempotency_includes_status() {
        let decision = DecisionWorkItem {
            decision_id: DecisionId(7),
            vault_id: VaultId(2),
            cluster: "mainnet".to_owned(),
            liquidity_mint: "USDC".to_owned(),
            source_reserve: "source".to_owned(),
            target_reserve: "target".to_owned(),
            amount_raw: 10,
        };

        assert_ne!(
            attempt_idempotency_key(&decision, AttemptStatus::Ready),
            attempt_idempotency_key(&decision, AttemptStatus::Failed)
        );
    }

    #[test]
    fn batch_idempotency_is_order_sensitive() {
        let decision = DecisionWorkItem {
            decision_id: DecisionId(1),
            vault_id: VaultId(1),
            cluster: "mainnet".to_owned(),
            liquidity_mint: "USDC".to_owned(),
            source_reserve: "a".to_owned(),
            target_reserve: "b".to_owned(),
            amount_raw: 1,
        };
        let first = ReadyAttempt {
            attempt_id: 1,
            decision: decision.clone(),
            estimated_compute_units: None,
        };
        let second = ReadyAttempt {
            attempt_id: 2,
            decision,
            estimated_compute_units: None,
        };

        assert_ne!(
            batch_idempotency_key(&[first.clone(), second.clone()]),
            batch_idempotency_key(&[second, first])
        );
    }
}

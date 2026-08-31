use std::{
    future::Future,
    pin::Pin,
    sync::atomic::{AtomicUsize, Ordering},
};

use anyhow::Result;
use balance_sweep_ata_monitor::{
    autodeposit_reconciliation_retry_alert, process_next_autodeposit_reconciliation_request,
    AutodepositChainReader, AutodepositReconciliationAlertKind,
    AutodepositReconciliationProcessOutcome, EarnReconciliationDeferralKind,
};
use loyal_yield_store::{
    sqlx, AutodepositChainObservation, AutodepositTargetSnapshotContext, BalanceSweepTargetId,
    OrchestratorConfig, OrchestratorStore,
};
use solana_client::{
    client_error::{ClientError, ClientErrorKind},
    rpc_custom_error::JSON_RPC_SERVER_ERROR_MIN_CONTEXT_SLOT_NOT_REACHED,
    rpc_request::{RpcError as SolanaRpcError, RpcResponseErrorData},
};

const DATABASE_URL_ENV: &str = "AUTODEPOSIT_RECONCILIATION_ALERTS_TEST_DATABASE_URL";

struct ScriptedAutodepositChainReader {
    lag_attempts_remaining: AtomicUsize,
}

impl ScriptedAutodepositChainReader {
    fn new(lag_attempts: usize) -> Self {
        Self {
            lag_attempts_remaining: AtomicUsize::new(lag_attempts),
        }
    }
}

impl AutodepositChainReader for ScriptedAutodepositChainReader {
    fn autodeposit_snapshot<'a>(
        &'a self,
        target: AutodepositTargetSnapshotContext,
        minimum_slot: u64,
    ) -> Pin<Box<dyn Future<Output = Result<AutodepositChainObservation>> + Send + 'a>> {
        Box::pin(async move {
            let should_lag = self
                .lag_attempts_remaining
                .fetch_update(Ordering::SeqCst, Ordering::SeqCst, |remaining| {
                    remaining.checked_sub(1)
                })
                .is_ok();
            if should_lag {
                return Err(
                    anyhow::Error::new(ClientError::from(ClientErrorKind::RpcError(
                        SolanaRpcError::RpcResponseError {
                            code: JSON_RPC_SERVER_ERROR_MIN_CONTEXT_SLOT_NOT_REACHED,
                            message: "Minimum context slot has not been reached".to_owned(),
                            data: RpcResponseErrorData::Empty,
                        },
                    )))
                    .context("read confirmed Autodeposit snapshot"),
                );
            }

            Ok(AutodepositChainObservation {
                target_id: target.target_id,
                observation_slot: minimum_slot + 1,
                observation_complete: true,
                policy_valid: true,
                subscription_authority_valid: true,
                recurring_delegation_valid: true,
                token_delegate_valid: true,
                wallet_balance_raw: 0,
            })
        })
    }
}

#[derive(Debug)]
struct ScenarioResult {
    deferred_attempts: Vec<i32>,
    alerts: Vec<AutodepositReconciliationAlertKind>,
}

#[tokio::test]
#[ignore = "requires a throwaway PostgreSQL database from the isolated verifier"]
async fn rpc_lag_retries_resolve_with_expected_alerts() {
    let database_url = std::env::var(DATABASE_URL_ENV).expect("throwaway database URL");
    let store =
        OrchestratorStore::connect(OrchestratorConfig::new(database_url).with_max_connections(2))
            .await
            .expect("connect to throwaway database");

    let immediate = run_scenario(&store, "immediate", 1_000, 0).await;
    assert!(
        immediate.alerts.is_empty(),
        "an immediately available confirmed snapshot must emit no operational alert"
    );
    assert!(immediate.deferred_attempts.is_empty());

    let transient = run_scenario(&store, "transient", 2_000, 2).await;
    assert_eq!(transient.deferred_attempts, [1, 2]);
    assert!(
        transient.alerts.is_empty(),
        "ordinary RPC catch-up must resolve without an operational alert"
    );

    let persistent = run_scenario(&store, "persistent", 3_000, 6).await;
    assert_eq!(persistent.deferred_attempts, [1, 2, 3, 4, 5, 6]);
    assert_eq!(
        persistent.alerts,
        [AutodepositReconciliationAlertKind::RpcBehind],
        "persistent lag must emit one Autodeposit-specific alert at attempt 6"
    );
    assert!(!persistent
        .alerts
        .contains(&AutodepositReconciliationAlertKind::RequestFailed));
    assert_eq!(
        persistent.alerts[0].error_code(),
        "autodeposit_reconciliation_rpc_behind"
    );
    assert!(persistent
        .alerts
        .iter()
        .all(|alert| alert.error_code() != "earn_reconciliation_job_failed"));
}

async fn run_scenario(
    store: &OrchestratorStore,
    suffix: &str,
    requested_slot: u64,
    lag_attempts: usize,
) -> ScenarioResult {
    let target_id = insert_target(store, suffix, requested_slot).await;
    store
        .enqueue_autodeposit_reconciliation_request(target_id, requested_slot)
        .await
        .expect("enqueue Autodeposit reconciliation request");

    let reader = ScriptedAutodepositChainReader::new(lag_attempts);
    let claim_owner = format!("autodeposit-alert-e2e-{suffix}");
    let mut deferred_attempts = Vec::new();
    let mut alerts = Vec::new();
    let mut completed_slot = None;

    for _ in 0..=lag_attempts + 1 {
        let outcome =
            process_next_autodeposit_reconciliation_request(store, &claim_owner, &reader, 120, 0)
                .await
                .expect("process scripted Autodeposit reconciliation request");
        match outcome {
            AutodepositReconciliationProcessOutcome::Deferred {
                target_id: deferred_target,
                attempt_count,
                kind,
                error,
            } => {
                assert_eq!(deferred_target, target_id);
                assert_eq!(kind, EarnReconciliationDeferralKind::RpcBehind);
                assert!(error.contains("Minimum context slot has not been reached"));
                deferred_attempts.push(attempt_count);
                if let Some(alert) = autodeposit_reconciliation_retry_alert(kind, attempt_count) {
                    alerts.push(alert);
                }
            }
            AutodepositReconciliationProcessOutcome::Completed {
                target_id: completed_target,
                requested_slot: completed_requested_slot,
                observed_slot,
                chain_status,
                still_pending,
            } => {
                assert_eq!(completed_target, target_id);
                assert_eq!(completed_requested_slot, requested_slot);
                assert_eq!(observed_slot, requested_slot + 1);
                assert_eq!(chain_status, "active");
                assert!(!still_pending);
                completed_slot = Some(observed_slot);
                break;
            }
            other => panic!("unexpected Autodeposit E2E outcome: {other:?}"),
        }
    }

    let completed_slot = completed_slot.expect("scripted RPC lag must eventually resolve");
    let request_state: (i64, i64, i32, Option<String>, Option<String>) = sqlx::query_as(
        r#"
        SELECT requested_slot, processed_slot, attempt_count, claim_owner, last_error
        FROM loyal_yield.autodeposit_reconciliation_requests
        WHERE target_id = $1
        "#,
    )
    .bind(target_id.as_i64())
    .fetch_one(store.pool())
    .await
    .expect("load resolved Autodeposit request");
    assert_eq!(request_state.0, completed_slot as i64);
    assert_eq!(request_state.1, completed_slot as i64);
    assert_eq!(request_state.2, 0);
    assert_eq!(request_state.3, None);
    assert_eq!(request_state.4, None);

    let target_state: (String, i64) = sqlx::query_as(
        "SELECT chain_status, chain_observation_slot FROM loyal_yield.balance_sweep_targets WHERE id = $1",
    )
    .bind(target_id.as_i64())
    .fetch_one(store.pool())
    .await
    .expect("load resolved Autodeposit target");
    assert_eq!(target_state, ("active".to_owned(), completed_slot as i64));

    ScenarioResult {
        deferred_attempts,
        alerts,
    }
}

async fn insert_target(
    store: &OrchestratorStore,
    suffix: &str,
    initial_slot: u64,
) -> BalanceSweepTargetId {
    let target_id: i64 = sqlx::query_scalar(
        r#"
        INSERT INTO loyal_yield.balance_sweep_targets (
            settings, authority, policy_seed, policy_account, vault_index,
            vault_pubkey, wallet, wallet_usdc_ata, vault_usdc_ata, token_mint,
            wallet_token_ata, vault_token_ata, delegated_signers, threshold,
            max_amount_per_period, desired_active, chain_status,
            chain_observation_slot, wallet_balance_floor_raw, last_seen_slot,
            last_seen_signature, cluster, subscription_authority,
            recurring_delegation, setup_generation
        ) VALUES (
            $1, $2, 1, $3, 1, $4, $2, $5, $6, $7, $5, $6,
            ARRAY[$8], 1, 10000000, TRUE, 'pending', 0, 0, $9, $10,
            'mainnet-beta', $11, $12, 1
        )
        RETURNING id
        "#,
    )
    .bind(format!("settings-{suffix}"))
    .bind(format!("wallet-{suffix}"))
    .bind(format!("policy-{suffix}"))
    .bind(format!("vault-{suffix}"))
    .bind(format!("wallet-ata-{suffix}"))
    .bind(format!("vault-ata-{suffix}"))
    .bind(format!("mint-{suffix}"))
    .bind(format!("signer-{suffix}"))
    .bind(initial_slot as i64)
    .bind(format!("signature-{suffix}"))
    .bind(format!("subscription-authority-{suffix}"))
    .bind(format!("recurring-delegation-{suffix}"))
    .fetch_one(store.pool())
    .await
    .expect("insert production-shaped Autodeposit target");
    BalanceSweepTargetId(target_id)
}

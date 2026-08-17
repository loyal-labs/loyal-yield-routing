//! Durable cross-mint continuation for the fleet executor.
//!
//! This module is intentionally a narrow integration boundary. The existing
//! same-mint executor remains the owner of atomic reserve moves; this path owns
//! only the finalized withdraw -> Jupiter ExactIn -> deposit movement.

use super::*;
use loyal_actions::jupiter::{
    parse_and_validate_jupiter_exact_in_build, JupiterBuildLimits, JupiterExactInBuildExpectation,
    JupiterLookupTableSnapshot, JupiterMintSnapshot, JupiterTokenAccountSnapshot, JupiterV2Dialect,
    SOLANA_MAX_COMPUTE_UNITS,
};
use loyal_yield_store::fleet_orchestration::{
    CrossMintBalanceAnchors, CrossMintContinuationLease, CrossMintCustodyPhase,
    CrossMintExpectedEffect, CrossMintFallbackCapacityInput, CrossMintLegPublicationInput,
    CrossMintLegPurpose, CrossMintLegReconciliationInput, CrossMintMovementActivationInput,
    CrossMintMovementCloseInput, CrossMintMovementLeg, CrossMintMovementRecord,
    CrossMintPolicyBindings, CrossMintReconciledEffect, CrossMintTerminalOutcome,
    KaminoPositionAnchor, SignedRouteSubmissionRecord, TokenBalanceAnchor, TokenBalanceDelta,
};
use serde::Deserialize;
use solana_client::{
    rpc_client::GetConfirmedSignaturesForAddress2Config, rpc_config::RpcTransactionConfig,
};
use solana_sdk::program_pack::Pack;
use solana_transaction_status_client_types::{
    option_serializer::OptionSerializer, TransactionStatus, UiTransactionEncoding,
    UiTransactionStatusMeta, UiTransactionTokenBalance,
};

const CROSS_MINT_ROUTE_KIND: &str = "cross_mint_jupiter";
const CROSS_MINT_ENABLE_ENV: &str = "EARN_ROUTER_ENABLE_CROSS_MINT_JUPITER";
const JUPITER_BUILD_URL_ENV: &str = "JUPITER_SWAP_BUILD_URL";
const JUPITER_API_KEY_ENV: &str = "JUPITER_API_KEY";
const CROSS_MINT_MAX_SLIPPAGE_BPS_ENV: &str = "EARN_ROUTER_CROSS_MINT_MAX_SLIPPAGE_BPS";
const CROSS_MINT_MAX_VALUE_LOSS_BPS_ENV: &str = "EARN_ROUTER_CROSS_MINT_MAX_VALUE_LOSS_BPS";
const DEFAULT_JUPITER_BUILD_URL: &str = "https://api.jup.ag/swap/v2/build";
const DEFAULT_MAX_SLIPPAGE_BPS: u16 = 50;
const DEFAULT_MAX_VALUE_LOSS_BPS: u16 = 50;

#[derive(Clone, Debug)]
pub(super) struct CrossMintWorkerConfig {
    enabled: bool,
    build_url: String,
    api_key: Option<String>,
    maximum_slippage_bps: u16,
    maximum_value_loss_bps: u16,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum CrossMintOpportunityDisposition {
    SameMint,
    CrossMint,
}

#[derive(Debug)]
pub(super) enum CrossMintWorkResult {
    NoWork,
    CancelledBeforeWithdraw {
        decision_id: i64,
    },
    ClosedForManualIntervention {
        decision_id: i64,
    },
    Continued {
        decision_id: i64,
        leg: CrossMintMovementLeg,
        purpose: CrossMintLegPurpose,
        submission_id: i64,
    },
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct JupiterBuildEnvelope {
    other_amount_threshold: String,
    slippage_bps: u16,
    addresses_by_lookup_table_address: BTreeMap<String, Vec<String>>,
}

#[derive(Clone, Debug)]
struct JupiterLaneContract {
    program_id: Pubkey,
    dialect_constraint_indexes: BTreeMap<JupiterV2Dialect, u8>,
    maximum_slippage_bps: u16,
    action_account: Pubkey,
}

impl JupiterLaneContract {
    fn constraint_index(&self, dialect: JupiterV2Dialect) -> Result<u8, Box<dyn Error>> {
        self.dialect_constraint_indexes
            .get(&dialect)
            .copied()
            .ok_or_else(|| format!("finalized swap policy does not authorize {dialect:?}").into())
    }
}

#[derive(Debug)]
struct FinalizedPolicyAccountReadback {
    context_slot: u64,
    data_sha256: String,
    data: Vec<u8>,
    decoded: DecodedPolicyAccount,
}

#[derive(Debug)]
struct ValidatedSwapPolicyAccount {
    policy_seed: u64,
    delegated_signer: Pubkey,
    dialect_constraint_indexes: BTreeMap<JupiterV2Dialect, u8>,
}

#[derive(Debug)]
struct PreparedCrossMintLeg {
    leg: CrossMintMovementLeg,
    purpose: CrossMintLegPurpose,
    policy_account: String,
    expected_effect: CrossMintExpectedEffect,
    expected_balance_anchors: CrossMintBalanceAnchors,
    /// Exact protected instructions and finalized ALTs used for the
    /// pre-withdraw atomic simulation. These are never published as a combined
    /// transaction; each durable movement leg remains independently signed.
    preflight_instructions: Vec<Instruction>,
    preflight_lookup_tables: Vec<AddressLookupTableAccount>,
    transaction: VersionedTransaction,
    optimizer_epoch_id: i64,
    last_valid_block_height: i64,
    compiled_fee_lamports: i64,
    writable_account_keys: Vec<String>,
    conflict_account_keys: Vec<String>,
    alt_requirements_fingerprint: String,
    alt_selection_fingerprint: String,
    alt_mutation_epochs: Value,
}

#[derive(Debug)]
struct PreparedCrossMintContinuation {
    lease: CrossMintContinuationLease,
    leg: PreparedCrossMintLeg,
}

fn kamino_position_anchor(
    position: &ChainPositionSummary,
) -> Result<KaminoPositionAnchor, Box<dyn Error>> {
    Ok(KaminoPositionAnchor {
        reserve: position.reserve.clone(),
        market: position.market.clone(),
        obligation: position.obligation.clone(),
        obligation_exists: position.obligation_exists,
        deposited_collateral_amount_raw: i64::try_from(position.amount_raw)?,
        minimum_deposit_amount_raw: None,
    })
}

fn finalized_kamino_position_anchor(
    runtime: &SameMintRouteRuntime,
    vault: Pubkey,
    expected: Option<&KaminoPositionAnchor>,
    min_context_slot: u64,
) -> Result<Option<KaminoPositionAnchor>, Box<dyn Error>> {
    let Some(expected) = expected else {
        return Ok(None);
    };
    let rpc = RpcClient::new_with_commitment(runtime.rpc.url(), CommitmentConfig::finalized());
    let reserve = Pubkey::from_str(&expected.reserve)?;
    let market = Pubkey::from_str(&expected.market)?;
    let obligation = Pubkey::from_str(&expected.obligation)?;
    let reserve_summary =
        load_kamino_reserve_summary_at_or_after(&rpc, &reserve, Some(min_context_slot))?;
    if reserve_summary.market != market {
        return Err("finalized Kamino reserve market differs from the signed anchor".into());
    }
    let obligation_summary = load_kamino_obligation_summary_at_or_after(
        &rpc,
        &obligation,
        &vault,
        &market,
        &reserve,
        Some(min_context_slot),
    )?;
    let observed = KaminoPositionAnchor {
        reserve: reserve.to_string(),
        market: market.to_string(),
        obligation: obligation.to_string(),
        obligation_exists: obligation_summary.exists,
        deposited_collateral_amount_raw: i64::try_from(
            obligation_summary.reserve_deposited_amount_raw,
        )?,
        minimum_deposit_amount_raw: Some(i64::try_from(minimum_kamino_deposit_amount_raw(
            &reserve_summary,
        )?)?),
    };
    if observed.reserve != expected.reserve
        || observed.market != expected.market
        || observed.obligation != expected.obligation
    {
        return Err("finalized Kamino position identity differs from the signed anchor".into());
    }
    Ok(Some(observed))
}

#[derive(Debug, thiserror::Error)]
#[error("movement-attributed custody differs from the finalized ATA balance")]
struct CrossMintCustodyMismatch {
    token_account: String,
    expected_amount_raw: i64,
    actual_amount_raw: Option<u64>,
}

#[derive(Debug, thiserror::Error)]
#[error("finalized cross-mint invariant failed: {0}")]
struct FinalizedCrossMintInvariant(String);

#[derive(Debug, thiserror::Error)]
#[error("custody token-account history contains an unrecognized finalized signature")]
struct CrossMintCustodyHistoryMismatch {
    token_account: String,
    anchor_slot: i64,
    signature: String,
    signature_slot: u64,
}

#[derive(Debug, thiserror::Error)]
#[error(
    "finalized RPC history for {token_account} did not reach custody anchor slot {anchor_slot}"
)]
struct CrossMintCustodyHistoryUnavailable {
    token_account: String,
    anchor_slot: i64,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum SourceIdleContinuation {
    Swap,
    RecoverSource,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum TargetIdleContinuation {
    DepositPrimary,
    RebindFallback,
    StopAtBoundFallback,
}

fn select_source_idle_continuation(
    rollout_enabled: bool,
    swap_preparation_succeeded: bool,
) -> SourceIdleContinuation {
    if rollout_enabled && swap_preparation_succeeded {
        SourceIdleContinuation::Swap
    } else {
        SourceIdleContinuation::RecoverSource
    }
}

fn select_target_idle_continuation(
    active_target_is_intended: bool,
    active_target_is_eligible: bool,
    deposit_preparation_succeeded: bool,
) -> TargetIdleContinuation {
    if active_target_is_eligible && deposit_preparation_succeeded {
        TargetIdleContinuation::DepositPrimary
    } else if active_target_is_intended {
        TargetIdleContinuation::RebindFallback
    } else {
        // A movement gets one deterministic fallback rebind. Do not oscillate
        // capacity among reserves when the selected fallback itself is broken.
        TargetIdleContinuation::StopAtBoundFallback
    }
}

pub(super) fn reconciliation_error_requires_quarantine(error: &(dyn Error + 'static)) -> bool {
    error
        .downcast_ref::<FinalizedCrossMintInvariant>()
        .is_some()
}

fn classify_reconciliation_store_error(error: OrchestratorError) -> Box<dyn Error> {
    match error {
        OrchestratorError::StoreInvariant(detail) => Box::new(FinalizedCrossMintInvariant(detail)),
        transient => Box::new(transient),
    }
}

fn manual_intervention_evidence(
    movement: &CrossMintMovementRecord,
    error: &(dyn Error + 'static),
) -> Option<(String, Value)> {
    if let Some(mismatch) = error.downcast_ref::<CrossMintCustodyMismatch>() {
        return Some((
            "finalized custody aggregate no longer matches its reconciled anchor".to_owned(),
            json!({
                "kind": "custody_balance_mismatch",
                "phase": format!("{:?}", movement.phase),
                "custodyMint": movement.custody_mint,
                "attributedAmountRaw": movement.custody_amount_raw.to_string(),
                "tokenAccount": mismatch.token_account,
                "expectedAggregateAmountRaw": mismatch.expected_amount_raw.to_string(),
                "actualAggregateAmountRaw": mismatch.actual_amount_raw.map(|amount| amount.to_string()),
                "commitment": "finalized",
                "safeResponse": "stop automatic continuation; inspect the user-owned vault ATA and policy before manual recovery",
            }),
        ));
    }
    if let Some(mismatch) = error.downcast_ref::<CrossMintCustodyHistoryMismatch>() {
        return Some((
            "finalized custody history contains an unattributed transaction".to_owned(),
            json!({
                "kind": "custody_history_mismatch",
                "phase": format!("{:?}", movement.phase),
                "custodyMint": movement.custody_mint,
                "tokenAccount": mismatch.token_account,
                "anchorSlot": mismatch.anchor_slot,
                "unrecognizedSignature": mismatch.signature,
                "unrecognizedSignatureSlot": mismatch.signature_slot,
                "commitment": "finalized",
                "safeResponse": "stop automatic continuation and attribute every token-account transaction since the custody anchor",
            }),
        ));
    }
    let detail = error.to_string();
    let kind = if detail.contains("vault is no longer active")
        || detail.contains("policy no longer authorizes")
    {
        "recovery_policy_revoked"
    } else if detail.contains("no safe same-target-mint fallback reserve") {
        "no_safe_recovery_target"
    } else {
        return None;
    };
    Some((
        "automatic recovery has no currently authorized destination".to_owned(),
        json!({
            "kind": kind,
            "phase": format!("{:?}", movement.phase),
            "custodyMint": movement.custody_mint,
            "custodyAccount": movement.custody_account,
            "attributedAmountRaw": movement.custody_amount_raw.to_string(),
            "observedAggregateAmountRaw": movement.custody_observed_balance_raw.map(|amount| amount.to_string()),
            "commitment": "finalized",
            "safeResponse": "leave funds in the vault-owned custody account and require an operator-approved policy or destination",
        }),
    ))
}

impl CrossMintWorkerConfig {
    pub(super) fn from_env() -> Result<Self, Box<dyn Error>> {
        let enabled = strict_bool_env(CROSS_MINT_ENABLE_ENV, false)?;
        let build_url = env::var(JUPITER_BUILD_URL_ENV)
            .unwrap_or_else(|_| DEFAULT_JUPITER_BUILD_URL.to_owned());
        let parsed = reqwest::Url::parse(&build_url)
            .map_err(|_| format!("{JUPITER_BUILD_URL_ENV} must be an absolute HTTPS URL"))?;
        if parsed.scheme() != "https" || parsed.host_str().is_none() {
            return Err(format!("{JUPITER_BUILD_URL_ENV} must be an absolute HTTPS URL").into());
        }
        let maximum_slippage_bps =
            bounded_bps_env(CROSS_MINT_MAX_SLIPPAGE_BPS_ENV, DEFAULT_MAX_SLIPPAGE_BPS)?;
        let maximum_value_loss_bps = bounded_bps_env(
            CROSS_MINT_MAX_VALUE_LOSS_BPS_ENV,
            DEFAULT_MAX_VALUE_LOSS_BPS,
        )?;
        let api_key = env::var(JUPITER_API_KEY_ENV)
            .ok()
            .filter(|value| !value.trim().is_empty());
        Ok(Self {
            enabled,
            build_url,
            api_key,
            maximum_slippage_bps,
            maximum_value_loss_bps,
        })
    }
}

fn strict_bool_env(name: &str, default: bool) -> Result<bool, Box<dyn Error>> {
    match env::var(name) {
        Ok(value) if value == "1" || value.eq_ignore_ascii_case("true") => Ok(true),
        Ok(value) if value == "0" || value.eq_ignore_ascii_case("false") => Ok(false),
        Ok(_) => Err(format!("{name} must be true, false, 1, or 0").into()),
        Err(env::VarError::NotPresent) => Ok(default),
        Err(error) => Err(error.into()),
    }
}

fn bounded_bps_env(name: &str, default: u16) -> Result<u16, Box<dyn Error>> {
    let value = match env::var(name) {
        Ok(value) => value.parse::<u16>()?,
        Err(env::VarError::NotPresent) => default,
        Err(error) => return Err(error.into()),
    };
    if value == 0 || value > 1_000 {
        return Err(format!("{name} must be in 1..=1000").into());
    }
    Ok(value)
}

pub(super) fn classify_opportunity(
    opportunity: &RebalanceOpportunityRecord,
) -> Result<CrossMintOpportunityDisposition, String> {
    let plan_kind = opportunity
        .execution_plan
        .get("kind")
        .and_then(Value::as_str);
    let plan_source = opportunity
        .execution_plan
        .get("source_liquidity_mint")
        .and_then(Value::as_str);
    let plan_target = opportunity
        .execution_plan
        .get("target_liquidity_mint")
        .and_then(Value::as_str);
    let record_is_cross = opportunity.source_liquidity_mint != opportunity.target_liquidity_mint;
    let plan_is_cross = plan_kind == Some(CROSS_MINT_ROUTE_KIND);
    if !record_is_cross && !plan_is_cross {
        return Ok(CrossMintOpportunityDisposition::SameMint);
    }
    if !record_is_cross
        || !plan_is_cross
        || plan_source != Some(opportunity.source_liquidity_mint.as_str())
        || plan_target != Some(opportunity.target_liquidity_mint.as_str())
        || opportunity.source_reserve.is_none()
    {
        return Err(format!(
            "opportunity {} has inconsistent cross-mint route identity",
            opportunity.id
        ));
    }
    if opportunity.execution_plan.get("policy_bindings").is_none() {
        return Err(format!(
            "opportunity {} has no durable base/swap/base policy bindings",
            opportunity.id
        ));
    }
    Ok(CrossMintOpportunityDisposition::CrossMint)
}

pub(super) async fn revalidate_cross_mint_opportunity(
    runtime: &SameMintRouteRuntime,
    config: &CrossMintWorkerConfig,
    lease: &RebalanceOpportunityLease,
) -> Result<(), Box<dyn Error>> {
    if !config.enabled {
        return Err("cross-mint Jupiter execution is disabled at the worker".into());
    }
    let gates = runtime
        .client
        .cross_mint_movement_gates(&lease.opportunity.cluster)
        .await?;
    if !gates.start_new_movements {
        return Err("starting new cross-mint movements is disabled".into());
    }
    let lane = jupiter_lane_contract(&lease.opportunity.execution_plan)?;
    validate_jupiter_lane(&lane, config.maximum_slippage_bps)?;
    let provisional = provisional_cross_mint_movement(&lease.opportunity)?;
    let withdraw = prepare_withdraw_leg(runtime, &lease.opportunity, &provisional).await?;
    let certification =
        certify_cross_mint_before_withdraw(runtime, config, &lease.opportunity, &withdraw).await?;
    let mut execution_plan = lease.opportunity.execution_plan.clone();
    execution_plan
        .as_object_mut()
        .ok_or("cross-mint execution plan is not an object")?
        .insert(
            "revalidation_preflight_certification".to_owned(),
            certification,
        );
    let route_fingerprint = cross_mint_route_fingerprint(&lease.opportunity);
    let requirements_fingerprint = cross_mint_requirements_fingerprint(&lease.opportunity, &lane)?;
    runtime
        .client
        .advance_rebalance_opportunity(
            lease.opportunity.id,
            lease,
            RebalanceOpportunityAdvance {
                next_state: RebalanceOpportunityState::Ready,
                available_at: Some(Utc::now()),
                decision_id: None,
                reason: None,
                route_fingerprint: Some(route_fingerprint),
                requirements_fingerprint: Some(requirements_fingerprint),
                execution_plan: Some(execution_plan),
                provisioning_request_id: None,
            },
        )
        .await?;
    Ok(())
}

pub(super) async fn process_continuation_before_new_work(
    runtime: &SameMintRouteRuntime,
    options: &FleetWorkerOptions,
    config: &CrossMintWorkerConfig,
) -> Result<CrossMintWorkResult, Box<dyn Error>> {
    if options.claim_kind != RebalanceOpportunityClaimKind::Execute {
        return Ok(CrossMintWorkResult::NoWork);
    }
    let gates = runtime
        .client
        .cross_mint_movement_gates(&options.cluster)
        .await?;
    if !gates.continue_or_recover_existing {
        return Ok(CrossMintWorkResult::NoWork);
    }
    let Some(lease) = runtime
        .client
        .claim_cross_mint_continuation(&options.cluster, &options.owner, options.lease_seconds)
        .await?
    else {
        return Ok(CrossMintWorkResult::NoWork);
    };
    if !config.enabled
        && lease.movement.phase == CrossMintCustodyPhase::SourceReserve
        && lease.movement.custody_version == 0
    {
        let rpc = RpcClient::new_with_commitment(runtime.rpc.url(), CommitmentConfig::finalized());
        let observed_slot = i64::try_from(rpc.get_slot()?)?;
        let closed = runtime
            .client
            .close_cross_mint_movement(
                &lease,
                CrossMintMovementCloseInput {
                    outcome: CrossMintTerminalOutcome::CancelledBeforeWithdraw,
                    observed_slot,
                    reason: "start_authority_revoked_before_withdraw".to_owned(),
                    evidence: json!({
                        "kind": "start_authority_revoked_before_withdraw",
                        "rolloutEnabled": false,
                        "safeResponse": "cancel the unstarted movement without publishing withdrawal bytes",
                    }),
                },
            )
            .await?;
        return Ok(CrossMintWorkResult::CancelledBeforeWithdraw {
            decision_id: closed.decision_id.as_i64(),
        });
    }
    let prepared = match prepare_next_leg(runtime, config, &lease).await {
        Ok(prepared) => prepared,
        Err(error) => {
            let Some((reason, evidence)) =
                manual_intervention_evidence(&lease.movement, error.as_ref())
            else {
                return Err(error);
            };
            let rpc =
                RpcClient::new_with_commitment(runtime.rpc.url(), CommitmentConfig::finalized());
            let observed_slot = i64::try_from(rpc.get_slot()?)?;
            let closed = runtime
                .client
                .close_cross_mint_movement(
                    &lease,
                    CrossMintMovementCloseInput {
                        outcome: CrossMintTerminalOutcome::ManualIntervention,
                        observed_slot,
                        reason,
                        evidence,
                    },
                )
                .await?;
            return Ok(CrossMintWorkResult::ClosedForManualIntervention {
                decision_id: closed.decision_id.as_i64(),
            });
        }
    };
    let unstarted_source_reserve = prepared.lease.movement.phase
        == CrossMintCustodyPhase::SourceReserve
        && prepared.lease.movement.custody_version == 0
        && prepared.leg.leg == CrossMintMovementLeg::Withdraw;
    let cancellation_lease = prepared.lease.clone();
    match publish_prepared_leg(&runtime.client, prepared.lease, prepared.leg).await {
        Ok(result) => Ok(result),
        Err(publication_error) if unstarted_source_reserve => {
            let rpc =
                RpcClient::new_with_commitment(runtime.rpc.url(), CommitmentConfig::finalized());
            let observed_slot = i64::try_from(rpc.get_slot()?)?;
            let cancellation = runtime
                .client
                .close_cross_mint_movement(
                    &cancellation_lease,
                    CrossMintMovementCloseInput {
                        outcome: CrossMintTerminalOutcome::CancelledBeforeWithdraw,
                        observed_slot,
                        reason: "start_authority_revoked_before_withdraw".to_owned(),
                        evidence: json!({
                            "kind": "start_authority_revoked_before_withdraw",
                            "publicationError": publication_error.to_string(),
                            "continuationControlGeneration":
                                cancellation_lease.control_generation,
                        }),
                    },
                )
                .await;
            match cancellation {
                Ok(_) => Err(format!(
                    "unstarted cross-mint continuation was cancelled after withdrawal admission failed: {publication_error}"
                )
                .into()),
                Err(cancellation_error) => Err(format!(
                    "unstarted withdrawal publication failed ({publication_error}); safe cancellation also failed ({cancellation_error})"
                )
                .into()),
            }
        }
        Err(error) => Err(error),
    }
}

pub(super) async fn owner_has_live_continuation_lease(
    client: &NeonSqlClient,
    cluster: &str,
    owner: &str,
) -> Result<bool, Box<dyn Error>> {
    Ok(loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT EXISTS (
            SELECT 1
            FROM loyal_yield.rebalance_decisions decision
            JOIN loyal_yield.rebalance_opportunities opportunity
              ON opportunity.decision_id = decision.id
             AND opportunity.cluster = $1
            WHERE decision.movement_route = 'cross_mint_jupiter'
              AND decision.status = 'confirming'::loyal_yield.decision_status
              AND decision.terminal_outcome IS NULL
              AND decision.continuation_lease_owner = $2
              AND decision.continuation_lease_expires_at > now()
        )
        "#,
    )
    .bind(cluster)
    .bind(owner)
    .fetch_one(client.pool())
    .await?)
}

pub(super) fn is_cross_mint_submission(submission: &SignedRouteSubmissionRecord) -> bool {
    submission.movement_leg != "route"
}

pub(super) async fn reconcile_finalized_submission(
    runtime: &SameMintRouteRuntime,
    lease: &SignedRouteSubmissionLease,
) -> Result<i64, Box<dyn Error>> {
    let submission = &lease.submission;
    if submission.state != SignedRouteSubmissionState::ReconciliationPending
        || !is_cross_mint_submission(submission)
        || submission.required_commitment != "finalized"
    {
        return Err("cross-mint reconciler received an invalid submission state".into());
    }
    let signature = Signature::from_str(&submission.transaction_signature)
        .map_err(|error| FinalizedCrossMintInvariant(error.to_string()))?;
    let confirmed = runtime.rpc.get_transaction_with_config(
        &signature,
        RpcTransactionConfig {
            encoding: Some(UiTransactionEncoding::Base64),
            commitment: Some(CommitmentConfig::finalized()),
            max_supported_transaction_version: Some(0),
        },
    )?;
    let vault_id: i64 = sqlx::query_scalar(
        "SELECT vault_id FROM loyal_yield.rebalance_opportunities WHERE id = $1",
    )
    .bind(submission.opportunity_id)
    .fetch_one(&runtime.pool)
    .await?;
    let vault_owner: String =
        sqlx::query_scalar("SELECT vault_pubkey FROM loyal_yield.managed_vaults WHERE id = $1")
            .bind(vault_id)
            .fetch_one(&runtime.pool)
            .await?;
    let vault_owner = Pubkey::from_str(&vault_owner)
        .map_err(|error| FinalizedCrossMintInvariant(error.to_string()))?;
    let receipt = (|| -> Result<_, Box<dyn Error>> {
        let finalized_slot = submission
            .finalized_slot
            .ok_or("cross-mint reconciliation is missing finalized_slot")?;
        if i64::try_from(confirmed.slot)? != finalized_slot {
            return Err(
                "finalized transaction slot differs from the durable finality receipt".into(),
            );
        }
        let transaction = confirmed
            .transaction
            .transaction
            .decode()
            .ok_or("finalized RPC transaction bytes did not decode")?;
        if bincode::serialize(&transaction)? != submission.signed_transaction {
            return Err("finalized transaction bytes differ from the persisted signed wire".into());
        }
        if transaction.signatures.first() != Some(&signature) {
            return Err("finalized transaction signature differs from persisted identity".into());
        }
        let meta = confirmed
            .transaction
            .meta
            .as_ref()
            .ok_or("finalized transaction omitted status metadata")?;
        if let Some(error) = &meta.err {
            return Err(format!("finalized cross-mint transaction failed: {error:?}").into());
        }
        let expected: CrossMintExpectedEffect =
            serde_json::from_value(submission.expected_effect.clone())?;
        let expected_anchors: CrossMintBalanceAnchors =
            serde_json::from_value(submission.expected_balance_anchors.clone())?;
        let account_keys = finalized_transaction_account_keys(&transaction, meta)?;
        let (debit, debit_post) = finalized_balance_delta(
            meta,
            &account_keys,
            expected_anchors.debit.as_ref(),
            vault_owner,
            true,
        )?;
        let (credit, credit_post) = finalized_balance_delta(
            meta,
            &account_keys,
            expected_anchors.credit.as_ref(),
            vault_owner,
            false,
        )?;
        if expected.debit.is_some() != debit.is_some()
            || expected.credit_mint.is_some() != credit.is_some()
        {
            return Err("finalized token deltas do not cover the signed leg expectation".into());
        }
        Ok((
            finalized_slot,
            CrossMintReconciledEffect { debit, credit },
            CrossMintBalanceAnchors {
                debit: debit_post,
                credit: credit_post,
                kamino_position: finalized_kamino_position_anchor(
                    runtime,
                    vault_owner,
                    expected_anchors.kamino_position.as_ref(),
                    u64::try_from(finalized_slot)?,
                )?,
            },
        ))
    })()
    .map_err(|error| FinalizedCrossMintInvariant(error.to_string()))?;
    let (finalized_slot, effect, reconciled_balance_anchors) = receipt;
    runtime
        .client
        .reconcile_cross_mint_leg(
            lease,
            CrossMintLegReconciliationInput {
                finalized_slot,
                effect,
                reconciled_balance_anchors,
            },
        )
        .await
        .map_err(classify_reconciliation_store_error)?;
    Ok(finalized_slot)
}

pub(super) async fn inspect_expired_submission(
    runtime: &SameMintRouteRuntime,
    lease: &SignedRouteSubmissionLease,
) -> Result<ExpiredRouteCheckOutcome, Box<dyn Error>> {
    let submission = &lease.submission;
    if !is_cross_mint_submission(submission)
        || !matches!(
            submission.state,
            SignedRouteSubmissionState::ExpiryCheckPending
                | SignedRouteSubmissionState::EffectAmbiguous
        )
    {
        return Err("cross-mint expiry verifier received an invalid submission".into());
    }
    let effect_check_slot = submission
        .effect_check_slot
        .ok_or("cross-mint expiry check is missing effect_check_slot")?;
    let signature = Signature::from_str(&submission.transaction_signature)?;
    let statuses = runtime
        .rpc
        .get_signature_statuses_with_history(&[signature])?;
    if let Some(status) = statuses.value.into_iter().next().flatten() {
        return cross_mint_late_status_outcome(&status);
    }

    let expected_anchors: CrossMintBalanceAnchors =
        serde_json::from_value(submission.expected_balance_anchors.clone())?;
    let vault_id: i64 = sqlx::query_scalar(
        "SELECT vault_id FROM loyal_yield.rebalance_opportunities WHERE id = $1",
    )
    .bind(submission.opportunity_id)
    .fetch_one(&runtime.pool)
    .await?;
    let vault_owner: String =
        sqlx::query_scalar("SELECT vault_pubkey FROM loyal_yield.managed_vaults WHERE id = $1")
            .bind(vault_id)
            .fetch_one(&runtime.pool)
            .await?;
    let vault_owner = Pubkey::from_str(&vault_owner)?;
    let custody_anchor_slot: Option<i64> = sqlx::query_scalar(
        "SELECT custody_reconciled_slot FROM loyal_yield.rebalance_decisions WHERE id = $1",
    )
    .bind(
        submission
            .decision_id
            .ok_or("cross-mint expiry check has no durable decision identity")?
            .as_i64(),
    )
    .fetch_one(&runtime.pool)
    .await?;
    let Some(custody_anchor_slot) = custody_anchor_slot else {
        return Ok(ExpiredRouteCheckOutcome::EffectAmbiguous {
            detail: "expired_cross_mint_has_no_custody_history_anchor".to_owned(),
        });
    };
    let recognized_signatures = recognized_movement_signatures(
        runtime,
        submission
            .decision_id
            .ok_or("cross-mint expiry check has no durable decision identity")?,
        custody_anchor_slot,
    )
    .await?;
    for anchor in [
        expected_anchors.debit.as_ref(),
        expected_anchors.credit.as_ref(),
    ]
    .into_iter()
    .flatten()
    {
        let token_account = Pubkey::from_str(&anchor.token_account)?;
        let mint = Pubkey::from_str(&anchor.mint)?;
        let token_program = canonical_earn_token_program(mint)?;
        let response = runtime.rpc.get_account_with_config(
            &token_account,
            RpcAccountInfoConfig {
                encoding: Some(UiAccountEncoding::Base64),
                commitment: Some(CommitmentConfig::finalized()),
                min_context_slot: Some(u64::try_from(effect_check_slot)?),
                ..RpcAccountInfoConfig::default()
            },
        )?;
        let Some(account) = response.value else {
            return Ok(ExpiredRouteCheckOutcome::EffectAmbiguous {
                detail: "expired_cross_mint_anchor_account_missing".to_owned(),
            });
        };
        let amount = match unpack_token_account_amount(&account, token_program, mint, vault_owner) {
            Ok(amount) => amount,
            Err(_) => {
                return Ok(ExpiredRouteCheckOutcome::EffectAmbiguous {
                    detail: "expired_cross_mint_anchor_program_or_binding_changed".to_owned(),
                })
            }
        };
        if i64::try_from(amount)? != anchor.amount_raw {
            return Ok(ExpiredRouteCheckOutcome::EffectAmbiguous {
                detail: "expired_cross_mint_balance_changed_or_ownership_ambiguous".to_owned(),
            });
        }
        // Read the finalized account first, then inspect history. A mutation
        // finalized before the balance observation cannot hide by restoring
        // the same aggregate amount between the two checks.
        if let Err(error) = verify_finalized_token_account_history(
            &runtime.rpc,
            token_account,
            custody_anchor_slot,
            &recognized_signatures,
        ) {
            if error
                .downcast_ref::<CrossMintCustodyHistoryMismatch>()
                .is_some()
                || error
                    .downcast_ref::<CrossMintCustodyHistoryUnavailable>()
                    .is_some()
            {
                return Ok(ExpiredRouteCheckOutcome::EffectAmbiguous {
                    detail: "expired_cross_mint_anchor_history_is_unproven_or_contains_unrecognized_signature"
                        .to_owned(),
                });
            }
            return Err(error);
        }
    }
    if let Some(expected_position) = expected_anchors.kamino_position.as_ref() {
        let observed = match finalized_kamino_position_anchor(
            runtime,
            vault_owner,
            Some(expected_position),
            u64::try_from(effect_check_slot)?,
        ) {
            Ok(Some(observed)) => observed,
            Ok(None) => unreachable!("position readback requires a position anchor"),
            Err(_) => {
                return Ok(ExpiredRouteCheckOutcome::EffectAmbiguous {
                    detail: "expired_cross_mint_kamino_position_readback_failed".to_owned(),
                })
            }
        };
        if &observed != expected_position {
            return Ok(ExpiredRouteCheckOutcome::EffectAmbiguous {
                detail: "expired_cross_mint_kamino_position_changed".to_owned(),
            });
        }
        if !finalized_account_history_is_recognized(
            &runtime.rpc,
            Pubkey::from_str(&expected_position.obligation)?,
            custody_anchor_slot,
            &recognized_signatures,
        )? {
            return Ok(ExpiredRouteCheckOutcome::EffectAmbiguous {
                detail:
                    "expired_cross_mint_kamino_position_history_contains_unrecognized_signature"
                        .to_owned(),
            });
        }
    }
    let observed_slot = i64::try_from(
        runtime
            .rpc
            .get_slot_with_commitment(CommitmentConfig::finalized())?,
    )?;
    if observed_slot < effect_check_slot {
        return Ok(ExpiredRouteCheckOutcome::EffectAmbiguous {
            detail: "expired_cross_mint_balance_observation_precedes_effect_check".to_owned(),
        });
    }
    Ok(ExpiredRouteCheckOutcome::EffectAbsent { observed_slot })
}

fn cross_mint_late_status_outcome(
    status: &TransactionStatus,
) -> Result<ExpiredRouteCheckOutcome, Box<dyn Error>> {
    let slot = i64::try_from(status.slot)?;
    if status.satisfies_commitment(CommitmentConfig::finalized()) {
        return match &status.err {
            Some(error) => Ok(ExpiredRouteCheckOutcome::ConfirmedFailure {
                slot,
                detail: safe_same_mint_operational_error(&format!(
                    "late_finalized_cross_mint_transaction_error:{error:?}"
                )),
            }),
            None => Ok(ExpiredRouteCheckOutcome::Finalized { slot }),
        };
    }
    if status.satisfies_commitment(CommitmentConfig::confirmed()) {
        return match &status.err {
            Some(error) => Ok(ExpiredRouteCheckOutcome::ConfirmedFailure {
                slot,
                detail: safe_same_mint_operational_error(&format!(
                    "late_cross_mint_transaction_error:{error:?}"
                )),
            }),
            None => Ok(ExpiredRouteCheckOutcome::Confirmed { slot }),
        };
    }
    Ok(ExpiredRouteCheckOutcome::SeenUnconfirmed {
        detail: safe_same_mint_operational_error(&format!(
            "late_cross_mint_signature_seen_below_confirmed_at_slot_{slot}:{:?}",
            status.err
        )),
    })
}

fn finalized_transaction_account_keys(
    transaction: &VersionedTransaction,
    meta: &UiTransactionStatusMeta,
) -> Result<Vec<Pubkey>, Box<dyn Error>> {
    let mut keys = transaction.message.static_account_keys().to_vec();
    match &meta.loaded_addresses {
        OptionSerializer::Some(loaded) => {
            for key in loaded.writable.iter().chain(&loaded.readonly) {
                keys.push(Pubkey::from_str(key)?);
            }
        }
        OptionSerializer::None | OptionSerializer::Skip => {
            if matches!(transaction.message, VersionedMessage::V0(_)) {
                return Err("versioned finalized transaction omitted loaded addresses".into());
            }
        }
    }
    Ok(keys)
}

fn finalized_balance_delta(
    meta: &UiTransactionStatusMeta,
    account_keys: &[Pubkey],
    expected_pre: Option<&TokenBalanceAnchor>,
    expected_owner: Pubkey,
    debit: bool,
) -> Result<(Option<TokenBalanceDelta>, Option<TokenBalanceAnchor>), Box<dyn Error>> {
    let Some(expected_pre) = expected_pre else {
        return Ok((None, None));
    };
    let token_account = Pubkey::from_str(&expected_pre.token_account)?;
    let account_index = account_keys
        .iter()
        .position(|key| key == &token_account)
        .ok_or("expected token account is absent from finalized transaction keys")?;
    let account_index = u8::try_from(account_index)?;
    let pre = unique_token_balance(&meta.pre_token_balances, account_index)?;
    let post = unique_token_balance(&meta.post_token_balances, account_index)?;
    let pre_amount = validated_ui_token_amount(pre, expected_pre, expected_owner)?;
    let post_amount = validated_ui_token_amount(post, expected_pre, expected_owner)?;
    if pre_amount != expected_pre.amount_raw {
        return Err("finalized transaction pre-balance differs from the signed anchor".into());
    }
    let amount_raw = if debit {
        pre_amount
            .checked_sub(post_amount)
            .filter(|amount| *amount > 0)
            .ok_or("finalized debit did not reduce the expected token account")?
    } else {
        post_amount
            .checked_sub(pre_amount)
            .filter(|amount| *amount > 0)
            .ok_or("finalized credit did not increase the expected token account")?
    };
    Ok((
        Some(TokenBalanceDelta {
            mint: expected_pre.mint.clone(),
            token_account: expected_pre.token_account.clone(),
            amount_raw,
        }),
        Some(TokenBalanceAnchor {
            mint: expected_pre.mint.clone(),
            token_account: expected_pre.token_account.clone(),
            amount_raw: post_amount,
        }),
    ))
}

fn unique_token_balance(
    balances: &OptionSerializer<Vec<UiTransactionTokenBalance>>,
    account_index: u8,
) -> Result<&UiTransactionTokenBalance, Box<dyn Error>> {
    let balances = match balances {
        OptionSerializer::Some(balances) => balances,
        OptionSerializer::None | OptionSerializer::Skip => {
            return Err("finalized transaction omitted token balance metadata".into())
        }
    };
    let mut matches = balances
        .iter()
        .filter(|balance| balance.account_index == account_index);
    let balance = matches
        .next()
        .ok_or("finalized transaction omitted an expected token balance")?;
    if matches.next().is_some() {
        return Err("finalized transaction duplicated token balance metadata".into());
    }
    Ok(balance)
}

fn validated_ui_token_amount(
    balance: &UiTransactionTokenBalance,
    expected: &TokenBalanceAnchor,
    expected_owner: Pubkey,
) -> Result<i64, Box<dyn Error>> {
    let expected_mint = Pubkey::from_str(&expected.mint)?;
    let token_program = canonical_earn_token_program(expected_mint)?;
    if balance.mint != expected.mint
        || balance.owner.as_ref() != OptionSerializer::Some(&expected_owner.to_string())
        || balance.program_id.as_ref() != OptionSerializer::Some(&token_program.to_string())
    {
        return Err("finalized token balance mint, owner, or program differs from custody".into());
    }
    Ok(balance.ui_token_amount.amount.parse::<i64>()?)
}

fn canonical_earn_token_program(mint: Pubkey) -> Result<Pubkey, Box<dyn Error>> {
    loyal_actions::earn_stablecoin(mint)
        .map(|asset| asset.token_program)
        .ok_or_else(|| format!("mint {mint} is outside the canonical Earn registry").into())
}

fn unpack_token_account_amount(
    account: &Account,
    expected_token_program: Pubkey,
    expected_mint: Pubkey,
    expected_authority: Pubkey,
) -> Result<u64, Box<dyn Error>> {
    if account.owner != expected_token_program {
        return Err("token account owner differs from its canonical token program".into());
    }
    let (mint, authority, amount, initialized) = if expected_token_program == spl_token::ID {
        let token = spl_token::state::Account::unpack(&account.data)?;
        (
            token.mint,
            token.owner,
            token.amount,
            token.state == spl_token::state::AccountState::Initialized,
        )
    } else if expected_token_program == loyal_actions::TOKEN_2022_PROGRAM_ID {
        let token = spl_token_2022::extension::StateWithExtensions::<
            spl_token_2022::state::Account,
        >::unpack(&account.data)?;
        (
            token.base.mint,
            token.base.owner,
            token.base.amount,
            token.base.state == spl_token_2022::state::AccountState::Initialized,
        )
    } else {
        return Err("canonical Earn mint uses an unsupported token program".into());
    };
    if !initialized || mint != expected_mint || authority != expected_authority {
        return Err("token account mint, authority, or state differs from custody".into());
    }
    Ok(amount)
}

async fn publish_prepared_leg(
    client: &NeonSqlClient,
    lease: CrossMintContinuationLease,
    prepared: PreparedCrossMintLeg,
) -> Result<CrossMintWorkResult, Box<dyn Error>> {
    let leg = prepared.leg;
    let purpose = prepared.purpose;
    // Generations are scoped to (movement, leg), not to continuation claims.
    // A first swap follows a successful withdrawal with generation 1; only a
    // proved-no-effect retry of that swap advances it to generation 2.
    let generation = next_leg_generation(client, lease.movement.decision_id, leg).await?;
    let input = CrossMintLegPublicationInput {
        leg,
        purpose,
        generation,
        policy_account: prepared.policy_account.clone(),
        expected_effect: prepared.expected_effect.clone(),
        expected_balance_anchors: prepared.expected_balance_anchors.clone(),
        submission: signed_submission_input(&lease, prepared, generation)?,
    };
    let submission = client.append_cross_mint_leg(&lease, input).await?;
    Ok(CrossMintWorkResult::Continued {
        decision_id: lease.movement.decision_id.as_i64(),
        leg,
        purpose,
        submission_id: submission.id,
    })
}

pub(super) async fn activate_cross_mint_opportunity(
    runtime: &SameMintRouteRuntime,
    options: &FleetWorkerOptions,
    config: &CrossMintWorkerConfig,
    lease: &RebalanceOpportunityLease,
) -> Result<CrossMintWorkResult, Box<dyn Error>> {
    if !config.enabled {
        return Err("cross-mint Jupiter execution is disabled at the worker".into());
    }
    let gates = runtime
        .client
        .cross_mint_movement_gates(&options.cluster)
        .await?;
    if !gates.start_new_movements {
        return Err("starting new cross-mint movements is disabled".into());
    }
    let policy_bindings = cross_mint_policy_bindings(&lease.opportunity.execution_plan)?;
    let capacity = cross_mint_capacity_reservation(runtime, lease).await?;
    // The exact withdrawal is compiled before activation so the movement's
    // fee cap is checked before any durable custody transition exists.
    let provisional = provisional_cross_mint_movement(&lease.opportunity)?;
    let withdraw = prepare_withdraw_leg(runtime, &lease.opportunity, &provisional).await?;
    let preflight_certification =
        certify_cross_mint_before_withdraw(runtime, config, &lease.opportunity, &withdraw).await?;
    let movement = runtime
        .client
        .activate_cross_mint_movement(
            lease,
            CrossMintMovementActivationInput {
                capacity,
                initial_withdraw_compiled_fee_lamports: withdraw.compiled_fee_lamports,
                preflight_certification,
                policy_bindings,
            },
        )
        .await?;
    // Activation leaves the movement continuation-available. Claim that fence
    // and publish the exact withdrawal compiled above; rebuilding here would
    // invalidate the fee and signed-byte evidence admitted with capacity.
    let continuation = runtime
        .client
        .claim_cross_mint_continuation(&options.cluster, &options.owner, options.lease_seconds)
        .await?
        .ok_or("activated cross-mint movement did not become continuation-claimable")?;
    if continuation.movement.decision_id != movement.decision_id {
        return Err(
            "an older cross-mint continuation appeared during activation; retry recovery-first"
                .into(),
        );
    }
    match publish_prepared_leg(&runtime.client, continuation.clone(), withdraw).await {
        Ok(result) => Ok(result),
        Err(publication_error) => {
            // Activation may commit immediately before the start gate or a
            // bound policy is revoked. No signed bytes were admitted in that
            // case, so close the untouched movement and release its capacity
            // instead of leaving a permanent continuation reservation.
            let rpc =
                RpcClient::new_with_commitment(runtime.rpc.url(), CommitmentConfig::finalized());
            let observed_slot = i64::try_from(rpc.get_slot()?)?;
            let cancellation = runtime
                .client
                .close_cross_mint_movement(
                    &continuation,
                    CrossMintMovementCloseInput {
                        outcome: CrossMintTerminalOutcome::CancelledBeforeWithdraw,
                        observed_slot,
                        reason: "start_authority_revoked_before_withdraw".to_owned(),
                        evidence: json!({
                            "kind": "start_authority_revoked_before_withdraw",
                            "publicationError": publication_error.to_string(),
                            "activationControlGeneration": gates.generation,
                        }),
                    },
                )
                .await;
            match cancellation {
                Ok(_) => Err(format!(
                    "initial cross-mint withdrawal was not admitted and activation was cancelled: {publication_error}"
                )
                .into()),
                Err(cancellation_error) => Err(format!(
                    "initial cross-mint withdrawal publication failed ({publication_error}); safe cancellation also failed ({cancellation_error})"
                )
                .into()),
            }
        }
    }
}

fn provisional_cross_mint_movement(
    opportunity: &RebalanceOpportunityRecord,
) -> Result<CrossMintMovementRecord, Box<dyn Error>> {
    Ok(CrossMintMovementRecord {
        decision_id: DecisionId(1),
        opportunity_id: opportunity.id,
        cluster: opportunity.cluster.clone(),
        vault_id: opportunity.vault_id,
        source_snapshot_id: opportunity.source_snapshot_id,
        source_reserve: opportunity
            .source_reserve
            .clone()
            .ok_or("cross-mint source reserve is absent")?,
        intended_target_reserve: opportunity.target_reserve.clone(),
        active_target_reserve: opportunity.target_reserve.clone(),
        source_mint: opportunity.source_liquidity_mint.clone(),
        target_mint: opportunity.target_liquidity_mint.clone(),
        planned_amount_raw: opportunity.amount_raw,
        preflight_certification: json!({"status": "pre_activation"}),
        custody_mint: opportunity.source_liquidity_mint.clone(),
        custody_amount_raw: opportunity.amount_raw,
        custody_account: opportunity
            .source_reserve
            .clone()
            .ok_or("cross-mint source reserve is absent")?,
        custody_observed_balance_raw: None,
        custody_reconciled_slot: None,
        custody_version: 0,
        phase: CrossMintCustodyPhase::SourceReserve,
        terminal_outcome: None,
        terminal_evidence: None,
        terminal_reason: None,
        terminal_observed_slot: None,
        continuation_available_at: Some(Utc::now()),
        continuation_fencing_token: 0,
        continuation_attempt_count: 0,
    })
}

async fn prepare_next_leg(
    runtime: &SameMintRouteRuntime,
    config: &CrossMintWorkerConfig,
    lease: &CrossMintContinuationLease,
) -> Result<PreparedCrossMintContinuation, Box<dyn Error>> {
    let opportunity = runtime
        .client
        .rebalance_opportunity(lease.movement.opportunity_id)
        .await?
        .ok_or("cross-mint movement opportunity no longer exists")?;
    if classify_opportunity(&opportunity)? != CrossMintOpportunityDisposition::CrossMint {
        return Err("cross-mint movement lost its immutable opportunity identity".into());
    }
    match lease.movement.phase {
        CrossMintCustodyPhase::SourceReserve => Ok(PreparedCrossMintContinuation {
            lease: lease.clone(),
            leg: prepare_withdraw_leg(runtime, &opportunity, &lease.movement).await?,
        }),
        CrossMintCustodyPhase::SourceIdle => {
            let swap = if config.enabled {
                Some(prepare_jupiter_swap_leg(runtime, config, &opportunity, &lease.movement).await)
            } else {
                None
            };
            match (
                select_source_idle_continuation(
                    config.enabled,
                    swap.as_ref().is_some_and(Result::is_ok),
                ),
                swap,
            ) {
                (SourceIdleContinuation::Swap, Some(Ok(prepared))) => {
                    Ok(PreparedCrossMintContinuation {
                        lease: lease.clone(),
                        leg: prepared,
                    })
                }
                (SourceIdleContinuation::RecoverSource, Some(Err(_)) | None) => {
                    prepare_source_recovery(runtime, &opportunity, lease).await
                }
                _ => Err("source-idle continuation selection became inconsistent".into()),
            }
        }
        CrossMintCustodyPhase::TargetIdle => {
            let active_target_is_intended =
                lease.movement.active_target_reserve == lease.movement.intended_target_reserve;
            let active_target_is_eligible = current_target_reserve_is_eligible(
                runtime,
                &lease.movement,
                &lease.movement.active_target_reserve,
            )
            .await?;
            let primary_preparation = if active_target_is_eligible {
                Some(
                    prepare_deposit_leg(
                        runtime,
                        &opportunity,
                        &lease.movement,
                        &lease.movement.active_target_reserve,
                        if active_target_is_intended {
                            CrossMintLegPurpose::OptimizeYield
                        } else {
                            CrossMintLegPurpose::FallbackTarget
                        },
                    )
                    .await,
                )
            } else {
                None
            };
            match select_target_idle_continuation(
                active_target_is_intended,
                active_target_is_eligible,
                primary_preparation.as_ref().is_some_and(Result::is_ok),
            ) {
                TargetIdleContinuation::DepositPrimary => {
                    let prepared = primary_preparation
                        .ok_or("eligible target deposit was not prepared")??;
                    Ok(PreparedCrossMintContinuation {
                        lease: lease.clone(),
                        leg: prepared,
                    })
                }
                TargetIdleContinuation::RebindFallback => {
                    prepare_rebound_fallback_deposit(runtime, &opportunity, lease).await
                }
                TargetIdleContinuation::StopAtBoundFallback => match primary_preparation {
                    Some(Err(error)) => Err(error),
                    _ => Err(
                        "no safe same-target-mint fallback reserve remains after the bound fallback"
                            .into(),
                    ),
                },
            }
        }
        CrossMintCustodyPhase::TargetReserve
        | CrossMintCustodyPhase::ClosedByUser
        | CrossMintCustodyPhase::ManualIntervention => {
            Err("terminal cross-mint movement was claimed for continuation".into())
        }
    }
}

async fn prepare_source_recovery(
    runtime: &SameMintRouteRuntime,
    opportunity: &RebalanceOpportunityRecord,
    lease: &CrossMintContinuationLease,
) -> Result<PreparedCrossMintContinuation, Box<dyn Error>> {
    let recovery = prepare_deposit_leg(
        runtime,
        opportunity,
        &lease.movement,
        &lease.movement.source_reserve,
        CrossMintLegPurpose::RecoverSource,
    )
    .await?;
    Ok(PreparedCrossMintContinuation {
        lease: lease.clone(),
        leg: recovery,
    })
}

async fn prepare_rebound_fallback_deposit(
    runtime: &SameMintRouteRuntime,
    opportunity: &RebalanceOpportunityRecord,
    lease: &CrossMintContinuationLease,
) -> Result<PreparedCrossMintContinuation, Box<dyn Error>> {
    // A failed primary preparation can be caused by a stale obligation or a
    // simulation failure, but it can also race a user mutation. Re-prove the
    // policy and custody history before atomically moving capacity.
    let vault = movement_vault(runtime, opportunity).await?;
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let rpc = RpcClient::new_with_commitment(runtime.rpc.url(), CommitmentConfig::finalized());
    verify_attributed_custody_with_history(runtime, &rpc, &lease.movement, vault_pubkey).await?;
    let fallback = fallback_target_observation(runtime, &lease.movement).await?;
    runtime
        .client
        .rebind_cross_mint_fallback_capacity(
            lease,
            CrossMintFallbackCapacityInput {
                target: fallback.clone(),
            },
        )
        .await?;
    let refreshed = runtime
        .client
        .claim_cross_mint_continuation(&lease.movement.cluster, &lease.owner, 120)
        .await?
        .ok_or("fallback target was rebound but no continuation became claimable")?;
    let deposit = prepare_deposit_leg(
        runtime,
        opportunity,
        &refreshed.movement,
        &fallback.observation.target_reserve,
        CrossMintLegPurpose::FallbackTarget,
    )
    .await?;
    Ok(PreparedCrossMintContinuation {
        lease: refreshed,
        leg: deposit,
    })
}

fn cross_mint_policy_bindings(plan: &Value) -> Result<CrossMintPolicyBindings, Box<dyn Error>> {
    CrossMintPolicyBindings::from_execution_plan(plan).map_err(Into::into)
}

fn jupiter_lane_contract(plan: &Value) -> Result<JupiterLaneContract, Box<dyn Error>> {
    let bindings = cross_mint_policy_bindings(plan)?;
    Ok(JupiterLaneContract {
        program_id: loyal_actions::JUPITER_V6_PROGRAM_ID,
        dialect_constraint_indexes: BTreeMap::from([
            (JupiterV2Dialect::RouteV2, 0),
            (JupiterV2Dialect::SharedAccountsRouteV2, 1),
        ]),
        maximum_slippage_bps: bindings.swap.max_slippage_bps,
        action_account: Pubkey::from_str(&bindings.swap.policy_account)?,
    })
}

fn validate_jupiter_lane(
    lane: &JupiterLaneContract,
    configured_maximum_slippage_bps: u16,
) -> Result<(), Box<dyn Error>> {
    if lane.program_id != loyal_actions::JUPITER_V6_PROGRAM_ID
        || lane.maximum_slippage_bps == 0
        || lane.dialect_constraint_indexes.is_empty()
        || lane
            .dialect_constraint_indexes
            .iter()
            .any(|(dialect, index)| match dialect {
                JupiterV2Dialect::RouteV2 => *index != 0,
                JupiterV2Dialect::SharedAccountsRouteV2 => {
                    lane.dialect_constraint_indexes.len() == 2 && *index != 1
                }
            })
    {
        return Err("stored Jupiter lane is outside the supported ExactIn contract".into());
    }
    // The runtime cap may be tighter than the user's policy. Use the tighter
    // value for every build; do not reject a correctly bounded policy merely
    // because rollout configuration is more conservative.
    if configured_maximum_slippage_bps == 0 {
        return Err("cross-mint rollout slippage cap must be positive".into());
    }
    Ok(())
}

fn load_finalized_policy_account_readback(
    rpc: &RpcClient,
    policy_account: Pubkey,
    minimum_slot: u64,
) -> Result<FinalizedPolicyAccountReadback, Box<dyn Error>> {
    let response = rpc.get_account_with_config(
        &policy_account,
        RpcAccountInfoConfig {
            encoding: Some(UiAccountEncoding::Base64),
            commitment: Some(CommitmentConfig::finalized()),
            min_context_slot: Some(minimum_slot),
            ..RpcAccountInfoConfig::default()
        },
    )?;
    let account = response
        .value
        .ok_or_else(|| format!("finalized policy account {policy_account} does not exist"))?;
    if account.owner != loyal_actions::SQUADS_SMART_ACCOUNT_PROGRAM_ID || account.executable {
        return Err(format!(
            "policy account {policy_account} is not a non-executable Squads policy account"
        )
        .into());
    }
    let decoded = decode_squads_policy_account(&account.data)
        .map_err(|error| format!("failed to decode policy account {policy_account}: {error}"))?;
    let data_sha256 = format!("{:x}", Sha256::digest(&account.data));
    Ok(FinalizedPolicyAccountReadback {
        context_slot: response.context.slot,
        data_sha256,
        data: account.data,
        decoded,
    })
}

fn validate_earn_policy_readback(
    readback: &FinalizedPolicyAccountReadback,
    bindings: &CrossMintPolicyBindings,
    minimum_slot: u64,
    constraint_index: u8,
    expected_route_step: &'static str,
) -> Result<(), Box<dyn Error>> {
    let decoded = &readback.decoded;
    if readback.context_slot < minimum_slot
        || decoded.threshold != 1
        || decoded.account_index != bindings.vault_index
        || decoded.delegated_signers != [bindings.delegated_signer.clone()]
        || decoded
            .instructions
            .get(usize::from(constraint_index))
            .and_then(|step| step.route_step)
            != Some(expected_route_step)
    {
        return Err(format!(
            "finalized Earn policy bytes differ from the immutable {expected_route_step} binding"
        )
        .into());
    }
    Ok(())
}

fn validate_swap_policy_readback(
    policy_account: Pubkey,
    readback: &FinalizedPolicyAccountReadback,
    opportunity: &RebalanceOpportunityRecord,
    bindings: &CrossMintPolicyBindings,
) -> Result<ValidatedSwapPolicyAccount, Box<dyn Error>> {
    let detected = loyal_actions::detect_jupiter_cross_mint_policy_account(&readback.data)?
        .ok_or("finalized swap policy is not a generalized Jupiter policy")?;
    let source_mint = Pubkey::from_str(&opportunity.source_liquidity_mint)?;
    let target_mint = Pubkey::from_str(&opportunity.target_liquidity_mint)?;
    let source = loyal_actions::earn_stablecoin(source_mint)
        .ok_or("cross-mint source is not a canonical Earn stablecoin")?;
    loyal_actions::earn_stablecoin(target_mint)
        .ok_or("cross-mint target is not a canonical Earn stablecoin")?;
    let expected_shard = if source.token_program == spl_token::ID {
        "classic"
    } else {
        "token_2022"
    };
    if readback.context_slot < bindings.swap.observed_slot
        || detected.policy_account != policy_account
        || detected.settings.to_string() != bindings.settings
        || detected.account_index != bindings.vault_index
        || detected.vault.to_string() != bindings.vault_pubkey
        || detected.delegated_signer.to_string() != bindings.delegated_signer
        || detected.threshold != 1
        || bindings.swap.source_shard != expected_shard
        || !detected.source_shard.contains(source_mint)
        || detected.max_slippage_bps != bindings.swap.max_slippage_bps
        || detected.daily_source_mint_spending_cap != bindings.swap.daily_source_mint_spending_cap
        || detected.dialect_constraint_indexes
            != BTreeMap::from([
                (JupiterV2Dialect::RouteV2, 0),
                (JupiterV2Dialect::SharedAccountsRouteV2, 1),
            ])
    {
        return Err(
            "finalized swap policy bytes differ from the immutable generalized binding".into(),
        );
    }
    validate_generalized_manifest_fingerprint(&detected, &bindings.swap.manifest_fingerprint)?;
    Ok(ValidatedSwapPolicyAccount {
        policy_seed: detected.policy_seed,
        delegated_signer: detected.delegated_signer,
        dialect_constraint_indexes: detected.dialect_constraint_indexes,
    })
}

fn cross_mint_route_fingerprint(opportunity: &RebalanceOpportunityRecord) -> String {
    stable_fingerprint(&[
        CROSS_MINT_ROUTE_KIND,
        &opportunity.cluster,
        &opportunity.vault_id.as_i64().to_string(),
        opportunity.source_reserve.as_deref().unwrap_or_default(),
        &opportunity.target_reserve,
        &opportunity.source_liquidity_mint,
        &opportunity.target_liquidity_mint,
    ])
}

fn cross_mint_requirements_fingerprint(
    opportunity: &RebalanceOpportunityRecord,
    lane: &JupiterLaneContract,
) -> Result<String, Box<dyn Error>> {
    let bindings = cross_mint_policy_bindings(&opportunity.execution_plan)?;
    Ok(stable_fingerprint(&[
        &cross_mint_route_fingerprint(opportunity),
        &bindings.settings,
        &bindings.vault_index.to_string(),
        &bindings.vault_pubkey,
        &bindings.delegated_signer,
        &bindings.withdraw.policy_account,
        &bindings.withdraw.observed_slot.to_string(),
        &bindings.withdraw.observed_signature,
        &bindings.withdraw.source_commitment,
        &bindings.withdraw.constraint_index.to_string(),
        &bindings.swap.policy_account,
        &bindings.swap.source_shard,
        &bindings.swap.observed_slot.to_string(),
        &bindings.swap.observed_signature,
        &bindings.swap.source_commitment,
        &bindings.swap.manifest_fingerprint,
        &bindings.swap.max_slippage_bps.to_string(),
        &bindings.swap.daily_source_mint_spending_cap.to_string(),
        &bindings.deposit.policy_account,
        &bindings.deposit.observed_slot.to_string(),
        &bindings.deposit.observed_signature,
        &bindings.deposit.source_commitment,
        &bindings.deposit.constraint_index.to_string(),
        &lane.action_account.to_string(),
        &lane.program_id.to_string(),
    ]))
}

// The transaction-building functions below are kept separate by leg. They
// share compilation helpers but never compose more than one protected value
// movement into a Solana transaction.

async fn prepare_withdraw_leg(
    runtime: &SameMintRouteRuntime,
    opportunity: &RebalanceOpportunityRecord,
    movement: &CrossMintMovementRecord,
) -> Result<PreparedCrossMintLeg, Box<dyn Error>> {
    prepare_kamino_leg(
        runtime,
        opportunity,
        movement,
        &movement.source_reserve,
        CrossMintMovementLeg::Withdraw,
        CrossMintLegPurpose::OptimizeYield,
    )
    .await
}

async fn prepare_deposit_leg(
    runtime: &SameMintRouteRuntime,
    opportunity: &RebalanceOpportunityRecord,
    movement: &CrossMintMovementRecord,
    reserve: &str,
    purpose: CrossMintLegPurpose,
) -> Result<PreparedCrossMintLeg, Box<dyn Error>> {
    prepare_kamino_leg(
        runtime,
        opportunity,
        movement,
        reserve,
        CrossMintMovementLeg::Deposit,
        purpose,
    )
    .await
}

async fn prepare_kamino_leg(
    runtime: &SameMintRouteRuntime,
    opportunity: &RebalanceOpportunityRecord,
    movement: &CrossMintMovementRecord,
    reserve: &str,
    leg: CrossMintMovementLeg,
    purpose: CrossMintLegPurpose,
) -> Result<PreparedCrossMintLeg, Box<dyn Error>> {
    let vault = movement_vault(runtime, opportunity).await?;
    let rpc = RpcClient::new_with_commitment(runtime.rpc.url(), CommitmentConfig::finalized());
    let reserves = vec![reserve.to_owned()];
    let preview = load_chain_reconcile_preview_from_rpc(
        &rpc,
        &vault,
        &reserves,
        movement
            .custody_reconciled_slot
            .map(u64::try_from)
            .transpose()?,
    )?;
    let position = chain_position_for_reserve(&preview, reserve)?;
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let mint = Pubkey::from_str(match leg {
        CrossMintMovementLeg::Withdraw => &movement.source_mint,
        CrossMintMovementLeg::Deposit => &movement.custody_mint,
        CrossMintMovementLeg::Swap => return Err("Kamino builder received swap leg".into()),
    })?;
    let token_program = canonical_earn_token_program(mint)?;
    if position.liquidity_mint != mint.to_string()
        || position.liquidity_token_program != token_program.to_string()
    {
        return Err(
            "Kamino position mint or token program differs from the canonical Earn asset".into(),
        );
    }
    let vault_ata = derive_associated_token_address(&vault_pubkey, &mint, &token_program);
    let bindings = cross_mint_policy_bindings(&opportunity.execution_plan)?;
    let signer = policy_keypair_from_env()?;
    if signer.pubkey().to_string() != bindings.delegated_signer {
        return Err("POLICY_KEYPAIR does not match the immutable delegated signer binding".into());
    }
    let fee_payer = signer.pubkey();
    let account_index = u8::try_from(vault.vault_index)?;
    let (policy_account_text, minimum_policy_slot, policy_index, route_step) = match leg {
        CrossMintMovementLeg::Withdraw => (
            bindings.withdraw.policy_account.as_str(),
            bindings.withdraw.observed_slot,
            bindings.withdraw.constraint_index,
            KAMINO_WITHDRAW_ROUTE_STEP,
        ),
        CrossMintMovementLeg::Deposit if purpose == CrossMintLegPurpose::RecoverSource => (
            bindings.withdraw.policy_account.as_str(),
            bindings.withdraw.observed_slot,
            1,
            KAMINO_DEPOSIT_ROUTE_STEP,
        ),
        CrossMintMovementLeg::Deposit => (
            bindings.deposit.policy_account.as_str(),
            bindings.deposit.observed_slot,
            bindings.deposit.constraint_index,
            KAMINO_DEPOSIT_ROUTE_STEP,
        ),
        CrossMintMovementLeg::Swap => unreachable!(),
    };
    let policy_account = Pubkey::from_str(policy_account_text)?;
    let routed = match leg {
        CrossMintMovementLeg::Withdraw => {
            let collateral_amount =
                required_plan_i64(&opportunity.execution_plan, "source_collateral_amount_raw")?;
            if collateral_amount <= 0 || position.amount_raw != u64::try_from(collateral_amount)? {
                return Err(
                    "cross-mint withdrawal collateral no longer matches planned custody".into(),
                );
            }
            kamino_withdraw_instruction(
                vault_pubkey,
                position,
                vault_ata,
                u64::try_from(collateral_amount)?,
            )?
        }
        CrossMintMovementLeg::Deposit => {
            verify_attributed_custody_with_history(runtime, &rpc, movement, vault_pubkey).await?;
            if !position.obligation_exists {
                return Err("cross-mint target obligation is not ready".into());
            }
            kamino_deposit_to_obligation_instruction(
                vault_pubkey,
                position,
                vault_ata,
                u64::try_from(movement.custody_amount_raw)?,
            )?
        }
        CrossMintMovementLeg::Swap => unreachable!(),
    };
    let policy_readback =
        load_finalized_policy_account_readback(&rpc, policy_account, minimum_policy_slot)?;
    validate_earn_policy_readback(
        &policy_readback,
        &bindings,
        minimum_policy_slot,
        policy_index,
        route_step,
    )?;
    let constraint_validation = validate_route_policy_constraints(
        &policy_readback.decoded,
        &[policy_index],
        &[(route_step, routed.instruction())],
    );
    if !constraint_validation.matches {
        return Err(format!(
            "finalized Earn policy does not authorize the exact {route_step} instruction: {}",
            constraint_validation.failures.join("; ")
        )
        .into());
    }
    let (outer, _, _, _) = build_program_interaction_policy_execution_instruction(
        policy_account,
        signer.pubkey(),
        account_index,
        routed,
        policy_index,
    )?;
    let refresh_positions = obligation_refresh_positions_for_route(&preview, position, position)?;
    let mut lookup_table_requirements = outer.lookup_table_requirements().clone();
    let mut instructions = Vec::with_capacity(refresh_positions.len() + 2);
    for position in refresh_positions {
        let refresh = kamino_refresh_reserve_instruction(position)?;
        lookup_table_requirements.merge(refresh.lookup_table_requirements())?;
        instructions.push(refresh.instruction().clone());
    }
    if position.obligation_exists {
        let refresh = kamino_refresh_obligation_instruction(position)?;
        lookup_table_requirements.merge(refresh.lookup_table_requirements())?;
        instructions.push(refresh.instruction().clone());
    }
    instructions.push(outer.instruction().clone());
    let manifest = route_lookup_table_manifest(
        fee_payer,
        &instructions,
        &vault,
        &lookup_table_requirements,
        &[vault_ata],
    )?;
    let options = cross_mint_cli_options(opportunity, &vault, runtime.rpc.url());
    let signers: [&dyn Signer; 1] = [&signer];
    let phase = prepare_route_lookup_table_phase(
        &runtime.client,
        &rpc,
        &options,
        &vault,
        &movement.source_reserve,
        reserve,
        CROSS_MINT_ROUTE_KIND,
        format!(
            "cross_mint:{}:{}:{}",
            movement.decision_id.as_i64(),
            leg.as_str(),
            reserve
        ),
        fee_payer,
        instructions,
        manifest,
        &signers,
        false,
    )
    .await?;
    let RouteLookupTablePhase {
        instructions,
        resolution,
        ..
    } = phase;
    let selected_tables = resolution
        .selected_bundle
        .as_ref()
        .ok_or_else(|| {
            resolution
                .blocker
                .clone()
                .unwrap_or_else(|| "cross-mint leg has no selected lookup-table bundle".to_owned())
        })?
        .tables
        .clone();
    let (_, mut lookup_tables_by_id, lookup_table_failures) =
        verify_reusable_lookup_table_candidates(
            &rpc,
            selected_tables.clone(),
            u64::try_from(resolution.observed_slot)?,
        );
    if !lookup_table_failures.is_empty() {
        return Err("cross-mint leg lookup tables changed after resolution".into());
    }
    let preflight_lookup_tables = selected_tables
        .iter()
        .map(|table| {
            lookup_tables_by_id
                .remove(&table.table_id)
                .ok_or_else(|| format!("verified lookup table {} is missing", table.table_id))
        })
        .collect::<Result<Vec<_>, _>>()?;
    if leg == CrossMintMovementLeg::Deposit {
        // The resolver compiles and signs internally. Re-read finalized
        // custody before accepting those bytes for durable publication; any
        // mutation during resolution invalidates the prepared transaction.
        verify_attributed_custody_with_history(runtime, &rpc, movement, vault_pubkey).await?;
    }
    let transaction = resolution.selected_transaction.ok_or_else(|| {
        resolution
            .blocker
            .unwrap_or_else(|| "cross-mint leg has no compiled transaction".to_owned())
    })?;
    let expected_effect = match leg {
        CrossMintMovementLeg::Withdraw => CrossMintExpectedEffect {
            debit: None,
            credit_mint: Some(movement.source_mint.clone()),
            credit_token_account: Some(vault_ata.to_string()),
            minimum_credit_amount_raw: Some(1),
        },
        CrossMintMovementLeg::Deposit => CrossMintExpectedEffect {
            debit: Some(TokenBalanceDelta {
                mint: movement.custody_mint.clone(),
                token_account: movement.custody_account.clone(),
                amount_raw: movement.custody_amount_raw,
            }),
            credit_mint: None,
            credit_token_account: None,
            minimum_credit_amount_raw: None,
        },
        CrossMintMovementLeg::Swap => unreachable!(),
    };
    let expected_balance_anchors = match leg {
        CrossMintMovementLeg::Withdraw => {
            let (amount, exists) = load_finalized_token_amount(
                &rpc,
                vault_ata,
                mint,
                vault_pubkey,
                token_program,
                u64::try_from(preview.observed_slot)?,
            )?;
            CrossMintBalanceAnchors {
                debit: None,
                credit: Some(TokenBalanceAnchor {
                    mint: movement.source_mint.clone(),
                    token_account: vault_ata.to_string(),
                    amount_raw: i64::try_from(if exists { amount } else { 0 })?,
                }),
                kamino_position: Some(kamino_position_anchor(position)?),
            }
        }
        CrossMintMovementLeg::Deposit => {
            let amount =
                verify_attributed_custody_with_history(runtime, &rpc, movement, vault_pubkey)
                    .await?;
            CrossMintBalanceAnchors {
                debit: Some(TokenBalanceAnchor {
                    mint: movement.custody_mint.clone(),
                    token_account: movement.custody_account.clone(),
                    amount_raw: i64::try_from(amount)?,
                }),
                credit: None,
                kamino_position: Some(kamino_position_anchor(position)?),
            }
        }
        CrossMintMovementLeg::Swap => unreachable!(),
    };
    Ok(PreparedCrossMintLeg {
        leg,
        purpose,
        policy_account: policy_account_text.to_owned(),
        expected_effect,
        expected_balance_anchors,
        preflight_instructions: instructions,
        preflight_lookup_tables,
        transaction,
        optimizer_epoch_id: opportunity.optimizer_epoch_id,
        last_valid_block_height: resolution.last_valid_block_height,
        compiled_fee_lamports: i64::try_from(
            resolution
                .selected_compiled_fee_lamports
                .ok_or("cross-mint leg omitted compiled fee")?,
        )?,
        writable_account_keys: resolution.writable_account_keys,
        conflict_account_keys: resolution.conflict_account_keys,
        alt_requirements_fingerprint: resolution.requirements_fingerprint,
        alt_selection_fingerprint: resolution
            .selection_fingerprint
            .ok_or("cross-mint leg omitted ALT selection fingerprint")?,
        alt_mutation_epochs: resolution.evidence,
    })
}

async fn prepare_jupiter_swap_leg(
    runtime: &SameMintRouteRuntime,
    config: &CrossMintWorkerConfig,
    opportunity: &RebalanceOpportunityRecord,
    movement: &CrossMintMovementRecord,
) -> Result<PreparedCrossMintLeg, Box<dyn Error>> {
    let vault = movement_vault(runtime, opportunity).await?;
    let (current_source_apy_bps, current_target_apy_bps) =
        current_post_withdraw_route_apys(runtime, opportunity).await?;
    let bindings = cross_mint_policy_bindings(&opportunity.execution_plan)?;
    if movement.custody_amount_raw <= 0
        || u64::try_from(movement.custody_amount_raw)?
            > bindings.swap.daily_source_mint_spending_cap
    {
        return Err("source-idle custody exceeds the exact daily swap policy cap".into());
    }
    let lane = jupiter_lane_contract(&opportunity.execution_plan)?;
    validate_jupiter_lane(&lane, config.maximum_slippage_bps)?;
    let effective_slippage_bps = lane.maximum_slippage_bps.min(config.maximum_slippage_bps);
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let input_mint = Pubkey::from_str(&movement.source_mint)?;
    let output_mint = Pubkey::from_str(&movement.target_mint)?;
    let input_token_program = canonical_earn_token_program(input_mint)?;
    let output_token_program = canonical_earn_token_program(output_mint)?;
    let input_ata =
        derive_associated_token_address(&vault_pubkey, &input_mint, &input_token_program);
    let output_ata =
        derive_associated_token_address(&vault_pubkey, &output_mint, &output_token_program);
    if movement.custody_account != input_ata.to_string() {
        return Err("movement source-idle custody is not the canonical Earn ATA".into());
    }
    let rpc = RpcClient::new_with_commitment(runtime.rpc.url(), CommitmentConfig::finalized());
    verify_attributed_custody_with_history(runtime, &rpc, movement, vault_pubkey).await?;
    let response = fetch_jupiter_build(
        config,
        input_mint,
        output_mint,
        u64::try_from(movement.custody_amount_raw)?,
        vault_pubkey,
        effective_slippage_bps,
    )
    .await?;
    let envelope: JupiterBuildEnvelope = serde_json::from_slice(&response)?;
    let minimum_output = envelope.other_amount_threshold.parse::<u64>()?;
    let effective_value_loss_bps = effective_maximum_value_loss_bps(
        &opportunity.execution_plan,
        config.maximum_value_loss_bps,
    )?;
    validate_signed_minimum_output_value_loss(
        u64::try_from(movement.custody_amount_raw)?,
        minimum_output,
        effective_value_loss_bps,
    )?;
    if envelope.slippage_bps > effective_slippage_bps {
        return Err("fresh Jupiter build fails post-withdraw economics".into());
    }
    validate_post_withdraw_swap_economics(
        &opportunity.execution_plan,
        movement.custody_amount_raw,
        i64::try_from(minimum_output)?,
        current_source_apy_bps,
        current_target_apy_bps,
    )?;
    let custody_anchor_slot = u64::try_from(
        movement
            .custody_reconciled_slot
            .ok_or("source-idle custody has no finalized reconciliation anchor")?,
    )?;
    let (input_mint_account, input_token_account, output_mint_account, output_token_account) =
        finalized_swap_accounts(
            &rpc,
            input_mint,
            input_ata,
            output_mint,
            output_ata,
            custody_anchor_slot,
        )?;
    let input_pre_balance = unpack_token_account_amount(
        &input_token_account,
        input_token_program,
        input_mint,
        vault_pubkey,
    )?;
    let output_pre_balance = unpack_token_account_amount(
        &output_token_account,
        output_token_program,
        output_mint,
        vault_pubkey,
    )?;
    let lookup_tables = finalized_jupiter_lookup_tables(
        &rpc,
        &envelope.addresses_by_lookup_table_address,
        custody_anchor_slot,
    )?;
    let expected = JupiterExactInBuildExpectation {
        authority: vault_pubkey,
        input_mint: JupiterMintSnapshot {
            address: input_mint,
            owner_program: input_mint_account.owner,
            data: input_mint_account.data,
        },
        output_mint: JupiterMintSnapshot {
            address: output_mint,
            owner_program: output_mint_account.owner,
            data: output_mint_account.data,
        },
        input_token_account: JupiterTokenAccountSnapshot {
            address: input_ata,
            owner_program: input_token_account.owner,
            data: input_token_account.data,
        },
        output_token_account: JupiterTokenAccountSnapshot {
            address: output_ata,
            owner_program: output_token_account.owner,
            data: output_token_account.data,
        },
        additional_token_accounts: vec![],
        input_amount: u64::try_from(movement.custody_amount_raw)?,
        minimum_output_amount: minimum_output,
        maximum_slippage_bps: effective_slippage_bps,
        requested_platform_fee_bps: 0,
        lookup_tables: lookup_tables
            .iter()
            .map(|table| JupiterLookupTableSnapshot {
                address: table.key,
                addresses: table.addresses.clone(),
            })
            .collect(),
        limits: JupiterBuildLimits::default(),
    };
    let validated = parse_and_validate_jupiter_exact_in_build(&response, &expected)?;
    let swap_policy_readback = load_finalized_policy_account_readback(
        &rpc,
        lane.action_account,
        bindings.swap.observed_slot,
    )?;
    let detected_swap = validate_swap_policy_readback(
        lane.action_account,
        &swap_policy_readback,
        opportunity,
        &bindings,
    )?;
    let swap_constraint_index = lane.constraint_index(validated.dialect)?;
    if detected_swap
        .dialect_constraint_indexes
        .get(&validated.dialect)
        != Some(&swap_constraint_index)
    {
        return Err("fresh Jupiter dialect mapping differs from finalized policy bytes".into());
    }
    let policy_validation = validate_route_policy_constraints(
        &swap_policy_readback.decoded,
        &[swap_constraint_index],
        &[("jupiter_exact_in", &validated.swap_instruction)],
    );
    if !policy_validation.matches {
        return Err(format!(
            "fresh post-withdraw Jupiter build does not satisfy the finalized swap policy: {}",
            policy_validation.failures.join("; ")
        )
        .into());
    }
    // The strict parser accepts only idempotent creation of the already-derived
    // canonical output ATA. Finalized snapshots above prove that ATA already
    // exists with the expected canonical token-program owner and mint, so omit Jupiter's
    // redundant setup instruction and keep the signed swap leg setup-free.
    let (route_blockhash, route_last_valid_block_height) =
        rpc.get_latest_blockhash_with_commitment(CommitmentConfig::finalized())?;
    let signer = policy_keypair_from_env()?;
    let fee_payer = signer.pubkey();
    let swap_outer = wrap_policy_instruction(
        lane.action_account,
        signer.pubkey(),
        u8::try_from(vault.vault_index)?,
        validated.swap_instruction.clone(),
        swap_constraint_index,
    );
    let measurement_instructions = vec![
        ComputeBudgetInstruction::set_compute_unit_limit(SOLANA_MAX_COMPUTE_UNITS),
        ComputeBudgetInstruction::set_compute_unit_price(
            validated.compute_budget.unit_price_micro_lamports,
        ),
        swap_outer.clone(),
    ];
    let signers: [&dyn Signer; 1] = [&signer];
    let measurement = compile_versioned_transaction(
        fee_payer,
        &measurement_instructions,
        &validated.lookup_tables,
        route_blockhash,
        &signers,
    )?;
    let simulation = rpc.simulate_transaction(&measurement)?;
    if let Some(error) = simulation.value.err {
        return Err(format!("Jupiter measurement simulation failed: {error:?}").into());
    }
    let units = simulation
        .value
        .units_consumed
        .ok_or("Jupiter measurement simulation omitted unitsConsumed")?;
    let compute_limit = validated.compute_budget.buffered_unit_limit(units);
    validated.instructions_with_compute_unit_limit(compute_limit)?;
    let final_instructions = vec![
        ComputeBudgetInstruction::set_compute_unit_limit(compute_limit),
        ComputeBudgetInstruction::set_compute_unit_price(
            validated.compute_budget.unit_price_micro_lamports,
        ),
        swap_outer,
    ];
    let preflight_instructions = final_instructions.iter().skip(2).cloned().collect();
    let preflight_lookup_tables = validated.lookup_tables.clone();
    // Last finalized custody read before signing the exact wrapped message.
    verify_attributed_custody_with_history(runtime, &rpc, movement, vault_pubkey).await?;
    let transaction = compile_versioned_transaction(
        fee_payer,
        &final_instructions,
        &validated.lookup_tables,
        route_blockhash,
        &signers,
    )?;
    let packet = transaction_packet_summary(&transaction, &validated.lookup_tables)?;
    if !packet.fits_packet_data_size {
        return Err("wrapped Jupiter transaction exceeds Solana packet limit".into());
    }
    let final_simulation = rpc.simulate_transaction(&transaction)?;
    if let Some(error) = final_simulation.value.err {
        return Err(format!("Jupiter final simulation failed: {error:?}").into());
    }
    if rpc.get_block_height()? > route_last_valid_block_height {
        return Err("Jupiter blockhash expired before signing".into());
    }
    verify_attributed_custody_with_history(runtime, &rpc, movement, vault_pubkey).await?;
    let fee = versioned_message_fee(&rpc, &transaction.message)?;
    Ok(PreparedCrossMintLeg {
        leg: CrossMintMovementLeg::Swap,
        purpose: CrossMintLegPurpose::OptimizeYield,
        policy_account: bindings.swap.policy_account.clone(),
        expected_effect: CrossMintExpectedEffect {
            debit: Some(TokenBalanceDelta {
                mint: movement.custody_mint.clone(),
                token_account: movement.custody_account.clone(),
                amount_raw: movement.custody_amount_raw,
            }),
            credit_mint: Some(movement.target_mint.clone()),
            credit_token_account: Some(output_ata.to_string()),
            minimum_credit_amount_raw: Some(i64::try_from(minimum_output)?),
        },
        expected_balance_anchors: CrossMintBalanceAnchors {
            debit: Some(TokenBalanceAnchor {
                mint: movement.custody_mint.clone(),
                token_account: movement.custody_account.clone(),
                amount_raw: i64::try_from(input_pre_balance)?,
            }),
            credit: Some(TokenBalanceAnchor {
                mint: movement.target_mint.clone(),
                token_account: output_ata.to_string(),
                amount_raw: i64::try_from(output_pre_balance)?,
            }),
            kamino_position: None,
        },
        preflight_instructions,
        preflight_lookup_tables,
        transaction,
        optimizer_epoch_id: opportunity.optimizer_epoch_id,
        last_valid_block_height: i64::try_from(route_last_valid_block_height)?,
        compiled_fee_lamports: i64::try_from(fee)?,
        writable_account_keys: exact_writable_account_keys(fee_payer, &final_instructions),
        conflict_account_keys: semantic_route_conflict_keys(&vault),
        alt_requirements_fingerprint: stable_fingerprint(&[
            "jupiter-build-v2",
            &validated.structure.unique_account_count.to_string(),
            &validated
                .structure
                .packet_bytes_with_compute_limit
                .to_string(),
        ]),
        alt_selection_fingerprint: stable_fingerprint(
            &validated
                .lookup_tables
                .iter()
                .map(|table| table.key.to_string())
                .collect::<Vec<_>>()
                .iter()
                .map(String::as_str)
                .collect::<Vec<_>>(),
        ),
        alt_mutation_epochs: json!({
            "source": "jupiter_build_v2_finalized_rpc",
            "blockhashFetchedAt": validated.blockhash_fetched_at,
            "computeUnitLimit": compute_limit,
            "simulationUnits": units,
            "lookupTables": validated.lookup_tables.iter().map(|table| table.key.to_string()).collect::<Vec<_>>(),
        }),
    })
}

fn validate_signed_minimum_output_value_loss(
    source_amount: u64,
    signed_minimum_output: u64,
    maximum_value_loss_bps: u16,
) -> Result<(), Box<dyn Error>> {
    let minimum_economic_output = u64::try_from(
        u128::from(source_amount)
            .checked_mul(u128::from(10_000u16 - maximum_value_loss_bps))
            .ok_or("maximum value-loss calculation overflowed")?
            / 10_000,
    )?;
    if signed_minimum_output == 0 || signed_minimum_output < minimum_economic_output {
        return Err("signed Jupiter minimum output exceeds the maximum value loss".into());
    }
    Ok(())
}

async fn fetch_jupiter_build(
    config: &CrossMintWorkerConfig,
    input_mint: Pubkey,
    output_mint: Pubkey,
    amount: u64,
    taker: Pubkey,
    slippage_bps: u16,
) -> Result<Vec<u8>, Box<dyn Error>> {
    let client = reqwest::Client::builder()
        .timeout(Duration::from_secs(15))
        .build()?;
    let mut request = client.get(&config.build_url).query(&[
        ("inputMint", input_mint.to_string()),
        ("outputMint", output_mint.to_string()),
        ("amount", amount.to_string()),
        ("taker", taker.to_string()),
        ("maxAccounts", "48".to_owned()),
        ("slippageBps", slippage_bps.to_string()),
        ("onlyDirectRoutes", "true".to_owned()),
        ("dexes", "AlphaQ".to_owned()),
    ]);
    if let Some(api_key) = config.api_key.as_deref() {
        request = request.header("x-api-key", api_key);
    }
    let response = request.send().await?;
    if !response.status().is_success() {
        return Err(format!("Jupiter /build returned HTTP {}", response.status()).into());
    }
    let bytes = response.bytes().await?;
    if bytes.len() > 2_000_000 {
        return Err("Jupiter /build response exceeds 2 MB".into());
    }
    Ok(bytes.to_vec())
}

fn effective_maximum_value_loss_bps(
    plan: &Value,
    configured_maximum_value_loss_bps: u16,
) -> Result<u16, Box<dyn Error>> {
    let planned = plan
        .get("cross_mint_maximum_value_loss_bps")
        .and_then(Value::as_u64)
        .ok_or("cross-mint plan is missing its user value-loss cap")?;
    let planned = u16::try_from(planned)?;
    if planned == 0 || planned > 1_000 || configured_maximum_value_loss_bps == 0 {
        return Err("cross-mint value-loss caps must be in 1..=1000 bps".into());
    }
    Ok(planned.min(configured_maximum_value_loss_bps))
}

async fn certify_cross_mint_before_withdraw(
    runtime: &SameMintRouteRuntime,
    config: &CrossMintWorkerConfig,
    opportunity: &RebalanceOpportunityRecord,
    withdraw: &PreparedCrossMintLeg,
) -> Result<Value, Box<dyn Error>> {
    if withdraw.leg != CrossMintMovementLeg::Withdraw
        || withdraw.purpose != CrossMintLegPurpose::OptimizeYield
    {
        return Err("preflight certification requires the exact prepared withdrawal".into());
    }
    let bindings = cross_mint_policy_bindings(&opportunity.execution_plan)?;
    if withdraw.policy_account != bindings.withdraw.policy_account
        || withdraw.preflight_instructions.is_empty()
    {
        return Err("prepared withdrawal differs from its immutable policy binding".into());
    }
    let vault = movement_vault(runtime, opportunity).await?;
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let rpc = RpcClient::new_with_commitment(runtime.rpc.url(), CommitmentConfig::finalized());
    let withdraw_policy_account = Pubkey::from_str(&bindings.withdraw.policy_account)?;
    let swap_policy_account = Pubkey::from_str(&bindings.swap.policy_account)?;
    let deposit_policy_account = Pubkey::from_str(&bindings.deposit.policy_account)?;
    let withdraw_readback = load_finalized_policy_account_readback(
        &rpc,
        withdraw_policy_account,
        bindings.withdraw.observed_slot,
    )?;
    validate_earn_policy_readback(
        &withdraw_readback,
        &bindings,
        bindings.withdraw.observed_slot,
        bindings.withdraw.constraint_index,
        KAMINO_WITHDRAW_ROUTE_STEP,
    )?;
    validate_earn_policy_readback(
        &withdraw_readback,
        &bindings,
        bindings.withdraw.observed_slot,
        1,
        KAMINO_DEPOSIT_ROUTE_STEP,
    )?;
    let swap_readback = load_finalized_policy_account_readback(
        &rpc,
        swap_policy_account,
        bindings.swap.observed_slot,
    )?;
    let detected_swap =
        validate_swap_policy_readback(swap_policy_account, &swap_readback, opportunity, &bindings)?;
    let deposit_readback = load_finalized_policy_account_readback(
        &rpc,
        deposit_policy_account,
        bindings.deposit.observed_slot,
    )?;
    validate_earn_policy_readback(
        &deposit_readback,
        &bindings,
        bindings.deposit.observed_slot,
        bindings.deposit.constraint_index,
        KAMINO_DEPOSIT_ROUTE_STEP,
    )?;

    let signer = policy_keypair_from_env()?;
    if signer.pubkey() != detected_swap.delegated_signer {
        return Err("POLICY_KEYPAIR does not match the finalized swap policy signer".into());
    }
    let input_mint = Pubkey::from_str(&opportunity.source_liquidity_mint)?;
    let output_mint = Pubkey::from_str(&opportunity.target_liquidity_mint)?;
    let input_token_program = canonical_earn_token_program(input_mint)?;
    let output_token_program = canonical_earn_token_program(output_mint)?;
    let input_ata =
        derive_associated_token_address(&vault_pubkey, &input_mint, &input_token_program);
    let output_ata =
        derive_associated_token_address(&vault_pubkey, &output_mint, &output_token_program);
    let input_amount = required_plan_i64(
        &opportunity.execution_plan,
        "redeemable_source_liquidity_amount_raw",
    )?;
    if input_amount <= 0 || input_amount != opportunity.amount_raw {
        return Err(
            "cross-mint planned swap amount differs from redeemable source liquidity".into(),
        );
    }
    let input_amount = u64::try_from(input_amount)?;
    if input_amount > bindings.swap.daily_source_mint_spending_cap {
        return Err("planned source amount exceeds the daily swap policy cap".into());
    }
    let effective_slippage_bps = bindings
        .swap
        .max_slippage_bps
        .min(config.maximum_slippage_bps);
    if effective_slippage_bps == 0 {
        return Err("effective cross-mint slippage cap is zero".into());
    }
    let effective_value_loss_bps = effective_maximum_value_loss_bps(
        &opportunity.execution_plan,
        config.maximum_value_loss_bps,
    )?;
    let minimum_policy_slot = bindings
        .withdraw
        .observed_slot
        .max(bindings.swap.observed_slot)
        .max(bindings.deposit.observed_slot);
    let (input_mint_account, input_token_account, output_mint_account, output_token_account) =
        finalized_swap_accounts(
            &rpc,
            input_mint,
            input_ata,
            output_mint,
            output_ata,
            minimum_policy_slot,
        )?;
    let input_pre_balance = unpack_token_account_amount(
        &input_token_account,
        input_token_program,
        input_mint,
        vault_pubkey,
    )?;
    let output_pre_balance = unpack_token_account_amount(
        &output_token_account,
        output_token_program,
        output_mint,
        vault_pubkey,
    )?;
    let response = fetch_jupiter_build(
        config,
        input_mint,
        output_mint,
        input_amount,
        vault_pubkey,
        effective_slippage_bps,
    )
    .await?;
    let response_sha256 = format!("{:x}", Sha256::digest(&response));
    let envelope: JupiterBuildEnvelope = serde_json::from_slice(&response)?;
    let minimum_output = envelope.other_amount_threshold.parse::<u64>()?;
    validate_signed_minimum_output_value_loss(
        input_amount,
        minimum_output,
        effective_value_loss_bps,
    )?;
    if envelope.slippage_bps > effective_slippage_bps {
        return Err("pre-withdraw Jupiter build exceeds effective slippage".into());
    }
    let target_preview = load_chain_reconcile_preview_from_rpc(
        &rpc,
        &vault,
        std::slice::from_ref(&opportunity.target_reserve),
        Some(minimum_policy_slot),
    )?;
    let target_position = chain_position_for_reserve(&target_preview, &opportunity.target_reserve)?;
    if !target_position.obligation_exists
        || target_position.liquidity_mint != opportunity.target_liquidity_mint
        || target_position.liquidity_token_program != output_token_program.to_string()
    {
        return Err(
            "target obligation, mint, or token program is not ready before withdrawal".into(),
        );
    }
    let target_deposit = kamino_deposit_to_obligation_instruction(
        vault_pubkey,
        target_position,
        output_ata,
        minimum_output,
    )?;
    let deposit_policy_validation = validate_route_policy_constraints(
        &deposit_readback.decoded,
        &[bindings.deposit.constraint_index],
        &[(KAMINO_DEPOSIT_ROUTE_STEP, target_deposit.instruction())],
    );
    if !deposit_policy_validation.matches {
        return Err(format!(
            "finalized target policy does not authorize the exact minimum-output deposit: {}",
            deposit_policy_validation.failures.join("; ")
        )
        .into());
    }
    let (current_source_apy_bps, current_target_apy_bps) =
        current_post_withdraw_route_apys(runtime, opportunity).await?;
    validate_post_withdraw_swap_economics(
        &opportunity.execution_plan,
        i64::try_from(input_amount)?,
        i64::try_from(minimum_output)?,
        current_source_apy_bps,
        current_target_apy_bps,
    )?;
    let lookup_tables = finalized_jupiter_lookup_tables(
        &rpc,
        &envelope.addresses_by_lookup_table_address,
        minimum_policy_slot,
    )?;
    let expected = JupiterExactInBuildExpectation {
        authority: vault_pubkey,
        input_mint: JupiterMintSnapshot {
            address: input_mint,
            owner_program: input_mint_account.owner,
            data: input_mint_account.data,
        },
        output_mint: JupiterMintSnapshot {
            address: output_mint,
            owner_program: output_mint_account.owner,
            data: output_mint_account.data,
        },
        input_token_account: JupiterTokenAccountSnapshot {
            address: input_ata,
            owner_program: input_token_account.owner,
            data: input_token_account.data,
        },
        output_token_account: JupiterTokenAccountSnapshot {
            address: output_ata,
            owner_program: output_token_account.owner,
            data: output_token_account.data,
        },
        additional_token_accounts: vec![],
        input_amount,
        minimum_output_amount: minimum_output,
        maximum_slippage_bps: effective_slippage_bps,
        requested_platform_fee_bps: 0,
        lookup_tables: lookup_tables
            .iter()
            .map(|table| JupiterLookupTableSnapshot {
                address: table.key,
                addresses: table.addresses.clone(),
            })
            .collect(),
        limits: JupiterBuildLimits::default(),
    };
    let validated = parse_and_validate_jupiter_exact_in_build(&response, &expected)?;
    let swap_constraint_index = detected_swap
        .dialect_constraint_indexes
        .get(&validated.dialect)
        .copied()
        .ok_or("fresh Jupiter build dialect is absent from the finalized swap policy")?;
    let policy_validation = validate_route_policy_constraints(
        &swap_readback.decoded,
        &[swap_constraint_index],
        &[("jupiter_exact_in", &validated.swap_instruction)],
    );
    if !policy_validation.matches {
        return Err(format!(
            "fresh Jupiter build does not satisfy finalized swap policy bytes: {}",
            policy_validation.failures.join("; ")
        )
        .into());
    }

    let (blockhash, last_valid_block_height) =
        rpc.get_latest_blockhash_with_commitment(CommitmentConfig::finalized())?;
    let compute_limit = SOLANA_MAX_COMPUTE_UNITS;
    validated.instructions_with_compute_unit_limit(compute_limit)?;
    let wrapped = wrap_policy_instruction(
        swap_policy_account,
        signer.pubkey(),
        bindings.vault_index,
        validated.swap_instruction.clone(),
        swap_constraint_index,
    );
    let mut instructions = vec![
        ComputeBudgetInstruction::set_compute_unit_limit(compute_limit),
        ComputeBudgetInstruction::set_compute_unit_price(
            validated.compute_budget.unit_price_micro_lamports,
        ),
    ];
    instructions.extend(withdraw.preflight_instructions.iter().cloned());
    instructions.push(wrapped);
    let mut simulation_lookup_tables = withdraw.preflight_lookup_tables.clone();
    for table in &validated.lookup_tables {
        if !simulation_lookup_tables
            .iter()
            .any(|existing| existing.key == table.key)
        {
            simulation_lookup_tables.push(table.clone());
        }
    }
    let signers: [&dyn Signer; 1] = [&signer];
    let transaction = compile_versioned_transaction(
        signer.pubkey(),
        &instructions,
        &simulation_lookup_tables,
        blockhash,
        &signers,
    )?;
    let packet = transaction_packet_summary(&transaction, &simulation_lookup_tables)?;
    if !packet.fits_packet_data_size {
        return Err(
            "atomic withdraw-plus-swap preflight exceeds the Solana simulation packet limit".into(),
        );
    }
    let simulation = rpc.simulate_transaction(&transaction)?;
    if let Some(error) = simulation.value.err {
        return Err(
            format!("atomic withdraw-plus-swap preflight simulation failed: {error:?}").into(),
        );
    }
    let simulation_units = simulation
        .value
        .units_consumed
        .ok_or("atomic withdraw-plus-swap simulation omitted unitsConsumed")?;
    let observed_block_height = rpc.get_block_height()?;
    if observed_block_height > last_valid_block_height {
        return Err("pre-withdraw Jupiter certification blockhash expired".into());
    }
    let message_hash = format!(
        "{:x}",
        Sha256::digest(bincode::serialize(&transaction.message)?)
    );
    Ok(json!({
        "kind": "cross_mint_preflight",
        "certifiedAt": Utc::now(),
        "cluster": opportunity.cluster,
        "sourceMint": opportunity.source_liquidity_mint,
        "targetMint": opportunity.target_liquidity_mint,
        "inputAmountRaw": input_amount.to_string(),
        "minimumOutputAmountRaw": minimum_output.to_string(),
        "effectiveSlippageBps": effective_slippage_bps,
        "effectiveMaximumValueLossBps": effective_value_loss_bps,
        "finalizedPolicyReadbacks": {
            "withdraw": {
                "policyAccount": bindings.withdraw.policy_account,
                "contextSlot": withdraw_readback.context_slot,
                "dataSha256": withdraw_readback.data_sha256,
            },
            "swap": {
                "policyAccount": bindings.swap.policy_account,
                "contextSlot": swap_readback.context_slot,
                "dataSha256": swap_readback.data_sha256,
                "policySeed": detected_swap.policy_seed.to_string(),
                "sourceShard": bindings.swap.source_shard,
                "manifestFingerprint": bindings.swap.manifest_fingerprint,
                "dialect": match validated.dialect {
                    JupiterV2Dialect::RouteV2 => "route_v2",
                    JupiterV2Dialect::SharedAccountsRouteV2 => "shared_accounts_route_v2",
                },
                "constraintIndex": swap_constraint_index,
                "dailySourceMintSpendingCap":
                    bindings.swap.daily_source_mint_spending_cap.to_string(),
            },
            "deposit": {
                "policyAccount": bindings.deposit.policy_account,
                "contextSlot": deposit_readback.context_slot,
                "dataSha256": deposit_readback.data_sha256,
            },
        },
        "jupiterBuild": {
            "responseSha256": response_sha256,
            "routeStepCount": validated.route_step_count,
            "quotedOutputAmountRaw": validated.quoted_output_amount.to_string(),
            "setupInstructionCount": validated.setup_instructions.len(),
            "lookupTables": validated.lookup_tables.iter().map(|table| table.key.to_string()).collect::<Vec<_>>(),
            "computeUnitLimit": compute_limit,
            "packetSizeBytes": packet.packet_size_bytes,
            "packetDataSizeBytes": packet.packet_data_size_bytes,
            "fitsPacketDataSize": packet.fits_packet_data_size,
            "messageSha256": message_hash,
            "lastValidBlockHeight": last_valid_block_height,
            "observedBlockHeight": observed_block_height,
            "inputPreBalanceRaw": input_pre_balance.to_string(),
            "outputPreBalanceRaw": output_pre_balance.to_string(),
            "simulationAttempted": true,
            "simulationUnits": simulation_units,
            "simulationTopology": "withdraw_then_swap_atomic_preflight_only",
            "simulationLookupTables": simulation_lookup_tables.iter().map(|table| table.key.to_string()).collect::<Vec<_>>(),
            "targetDepositPolicyValidated": true,
            "targetReserve": opportunity.target_reserve,
            "targetObligation": target_position.obligation,
        },
    }))
}

fn finalized_swap_accounts(
    rpc: &RpcClient,
    input_mint: Pubkey,
    input_ata: Pubkey,
    output_mint: Pubkey,
    output_ata: Pubkey,
    minimum_slot: u64,
) -> Result<(Account, Account, Account, Account), Box<dyn Error>> {
    let response = rpc.get_multiple_accounts_with_config(
        &[input_mint, input_ata, output_mint, output_ata],
        RpcAccountInfoConfig {
            encoding: Some(UiAccountEncoding::Base64),
            commitment: Some(CommitmentConfig::finalized()),
            min_context_slot: Some(minimum_slot),
            ..RpcAccountInfoConfig::default()
        },
    )?;
    let mut values = response.value.into_iter();
    let input_mint_account = values.next().flatten().ok_or("input mint is missing")?;
    let input_token_account = values.next().flatten().ok_or("input ATA is missing")?;
    let output_mint_account = values.next().flatten().ok_or("output mint is missing")?;
    let output_token_account = values.next().flatten().ok_or("output ATA is missing")?;
    Ok((
        input_mint_account,
        input_token_account,
        output_mint_account,
        output_token_account,
    ))
}

fn finalized_jupiter_lookup_tables(
    rpc: &RpcClient,
    expected: &BTreeMap<String, Vec<String>>,
    minimum_slot: u64,
) -> Result<Vec<AddressLookupTableAccount>, Box<dyn Error>> {
    let keys = expected
        .keys()
        .map(|key| Pubkey::from_str(key))
        .collect::<Result<Vec<_>, _>>()?;
    let response = rpc.get_multiple_accounts_with_config(
        &keys,
        RpcAccountInfoConfig {
            encoding: Some(UiAccountEncoding::Base64),
            commitment: Some(CommitmentConfig::finalized()),
            min_context_slot: Some(minimum_slot),
            ..RpcAccountInfoConfig::default()
        },
    )?;
    keys.into_iter()
        .zip(response.value)
        .map(|(key, account)| {
            let account = account.ok_or_else(|| format!("Jupiter ALT {key} is missing"))?;
            if account.owner != address_lookup_table_program::id() {
                return Err(format!("Jupiter ALT {key} has the wrong owner").into());
            }
            let table = AddressLookupTable::deserialize(&account.data)?;
            if table.meta.deactivation_slot != u64::MAX {
                return Err(format!("Jupiter ALT {key} is deactivating").into());
            }
            let addresses = table.addresses.iter().copied().collect::<Vec<_>>();
            let expected_addresses = expected
                .get(&key.to_string())
                .ok_or("Jupiter ALT response identity changed")?
                .iter()
                .map(|address| Pubkey::from_str(address))
                .collect::<Result<Vec<_>, _>>()?;
            if addresses != expected_addresses {
                return Err(format!("Jupiter ALT {key} ordered contents changed").into());
            }
            Ok(AddressLookupTableAccount { key, addresses })
        })
        .collect()
}

fn wrap_policy_instruction(
    policy: Pubkey,
    signer: Pubkey,
    account_index: u8,
    instruction: Instruction,
    instruction_constraint_index: u8,
) -> Instruction {
    let mut transaction_accounts = Vec::new();
    let compiled = compile_squads_inner_instruction(&mut transaction_accounts, instruction);
    execute_program_interaction_policy_instruction(
        policy,
        signer,
        account_index,
        vec![compiled],
        vec![instruction_constraint_index],
        transaction_accounts,
    )
}

async fn verify_attributed_custody_with_history(
    runtime: &SameMintRouteRuntime,
    rpc: &RpcClient,
    movement: &CrossMintMovementRecord,
    vault: Pubkey,
) -> Result<u64, Box<dyn Error>> {
    let anchor_slot = movement
        .custody_reconciled_slot
        .ok_or("idle cross-mint custody has no finalized reconciliation anchor")?;
    let token_account = Pubkey::from_str(&movement.custody_account)?;
    let amount = verify_attributed_custody(rpc, movement, vault)?;
    let recognized_signatures =
        recognized_movement_signatures(runtime, movement.decision_id, anchor_slot).await?;
    verify_finalized_token_account_history(
        rpc,
        token_account,
        anchor_slot,
        &recognized_signatures,
    )?;
    Ok(amount)
}

async fn recognized_movement_signatures(
    runtime: &SameMintRouteRuntime,
    decision_id: DecisionId,
    anchor_slot: i64,
) -> Result<BTreeSet<String>, Box<dyn Error>> {
    Ok(loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT transaction_signature
        FROM loyal_yield.signed_route_submissions
        WHERE decision_id = $1
          AND submission_state = 'reconciled'
          AND finalized_slot >= $2
        "#,
    )
    .bind(decision_id.as_i64())
    .bind(anchor_slot)
    .fetch_all(&runtime.pool)
    .await?
    .into_iter()
    .collect())
}

fn verify_finalized_token_account_history(
    rpc: &RpcClient,
    token_account: Pubkey,
    anchor_slot: i64,
    recognized_signatures: &BTreeSet<String>,
) -> Result<(), Box<dyn Error>> {
    let anchor_slot_u64 = u64::try_from(anchor_slot)?;
    let mut before = None;
    let mut observed_anchor = false;
    loop {
        let page = rpc.get_signatures_for_address_with_config(
            &token_account,
            GetConfirmedSignaturesForAddress2Config {
                before,
                until: None,
                limit: Some(1_000),
                commitment: Some(CommitmentConfig::finalized()),
            },
        )?;
        if page.is_empty() {
            return if observed_anchor {
                Ok(())
            } else {
                Err(CrossMintCustodyHistoryUnavailable {
                    token_account: token_account.to_string(),
                    anchor_slot,
                }
                .into())
            };
        }
        let page_len = page.len();
        let mut reached_pre_anchor_history = false;
        for status in &page {
            if status.slot < anchor_slot_u64 {
                reached_pre_anchor_history = true;
                break;
            }
            if !recognized_signatures.contains(&status.signature) {
                return Err(CrossMintCustodyHistoryMismatch {
                    token_account: token_account.to_string(),
                    anchor_slot,
                    signature: status.signature.clone(),
                    signature_slot: status.slot,
                }
                .into());
            }
            observed_anchor |= status.slot == anchor_slot_u64;
        }
        if reached_pre_anchor_history || page_len < 1_000 {
            return if observed_anchor {
                Ok(())
            } else {
                Err(CrossMintCustodyHistoryUnavailable {
                    token_account: token_account.to_string(),
                    anchor_slot,
                }
                .into())
            };
        }
        before = Some(Signature::from_str(
            &page
                .last()
                .ok_or("custody history page unexpectedly became empty")?
                .signature,
        )?);
    }
}

fn validate_generalized_manifest_fingerprint(
    detected: &loyal_actions::DetectedJupiterCrossMintPolicyAccount,
    bound_fingerprint: &str,
) -> Result<(), Box<dyn Error>> {
    let expected =
        loyal_actions::generalized_cross_mint_manifest_fingerprint(&detected.manifest_semantics());
    if bound_fingerprint != expected {
        return Err(
            "finalized generalized policy fingerprint differs from its decoded semantics".into(),
        );
    }
    Ok(())
}

fn finalized_account_history_is_recognized(
    rpc: &RpcClient,
    account: Pubkey,
    anchor_slot: i64,
    recognized_signatures: &BTreeSet<String>,
) -> Result<bool, Box<dyn Error>> {
    let anchor_slot = u64::try_from(anchor_slot)?;
    let mut before = None;
    loop {
        let page = rpc.get_signatures_for_address_with_config(
            &account,
            GetConfirmedSignaturesForAddress2Config {
                before,
                until: None,
                limit: Some(1_000),
                commitment: Some(CommitmentConfig::finalized()),
            },
        )?;
        if page.is_empty() {
            return Ok(true);
        }
        for status in &page {
            if status.slot < anchor_slot {
                return Ok(true);
            }
            if !recognized_signatures.contains(&status.signature) {
                return Ok(false);
            }
        }
        if page.len() < 1_000 {
            return Ok(true);
        }
        before = Some(Signature::from_str(
            &page
                .last()
                .ok_or("account history page unexpectedly became empty")?
                .signature,
        )?);
    }
}

fn verify_attributed_custody(
    rpc: &RpcClient,
    movement: &CrossMintMovementRecord,
    vault: Pubkey,
) -> Result<u64, Box<dyn Error>> {
    let mint = Pubkey::from_str(&movement.custody_mint)?;
    let token_program = canonical_earn_token_program(mint)?;
    let token_account = Pubkey::from_str(&movement.custody_account)?;
    let canonical = derive_associated_token_address(&vault, &mint, &token_program);
    let expected_amount_raw = movement.custody_observed_balance_raw.ok_or_else(|| {
        Box::<dyn Error>::from(CrossMintCustodyMismatch {
            token_account: movement.custody_account.clone(),
            expected_amount_raw: movement.custody_amount_raw,
            actual_amount_raw: None,
        })
    })?;
    if token_account != canonical {
        return Err(CrossMintCustodyMismatch {
            token_account: token_account.to_string(),
            expected_amount_raw,
            actual_amount_raw: None,
        }
        .into());
    }
    let minimum_slot = movement
        .custody_reconciled_slot
        .ok_or("idle cross-mint custody has no finalized reconciliation anchor")?;
    let (amount, exists) = load_finalized_token_amount(
        rpc,
        token_account,
        mint,
        vault,
        token_program,
        u64::try_from(minimum_slot)?,
    )
    .map_err(|_| CrossMintCustodyMismatch {
        token_account: token_account.to_string(),
        expected_amount_raw,
        actual_amount_raw: None,
    })?;
    if !exists || amount != u64::try_from(expected_amount_raw)? {
        return Err(CrossMintCustodyMismatch {
            token_account: token_account.to_string(),
            expected_amount_raw,
            actual_amount_raw: exists.then_some(amount),
        }
        .into());
    }
    Ok(amount)
}

fn load_finalized_token_amount(
    rpc: &RpcClient,
    token_account: Pubkey,
    mint: Pubkey,
    authority: Pubkey,
    token_program: Pubkey,
    minimum_slot: u64,
) -> Result<(u64, bool), Box<dyn Error>> {
    let response = rpc.get_account_with_config(
        &token_account,
        RpcAccountInfoConfig {
            encoding: Some(UiAccountEncoding::Base64),
            commitment: Some(CommitmentConfig::finalized()),
            min_context_slot: Some(minimum_slot),
            ..RpcAccountInfoConfig::default()
        },
    )?;
    let Some(account) = response.value else {
        return Ok((0, false));
    };
    Ok((
        unpack_token_account_amount(&account, token_program, mint, authority)?,
        true,
    ))
}

async fn movement_vault(
    runtime: &SameMintRouteRuntime,
    opportunity: &RebalanceOpportunityRecord,
) -> Result<SelectedVault, Box<dyn Error>> {
    let bindings = cross_mint_policy_bindings(&opportunity.execution_plan)?;
    let vault_index = i16::from(bindings.vault_index);
    let vault = load_active_vault(&runtime.pool, &bindings.settings, vault_index)
        .await?
        .ok_or("cross-mint vault is no longer active")?;
    validate_vault_policy(&vault)?;
    if vault.id != opportunity.vault_id
        || vault.settings != bindings.settings
        || vault.vault_index != vault_index
        || vault.vault_pubkey != bindings.vault_pubkey
        || vault.threshold != 1
        || vault.delegated_signers != [bindings.delegated_signer.clone()]
    {
        return Err("active vault identity no longer matches the immutable route binding".into());
    }
    Ok(vault)
}

async fn current_post_withdraw_route_apys(
    runtime: &SameMintRouteRuntime,
    opportunity: &RebalanceOpportunityRecord,
) -> Result<(i64, i64), Box<dyn Error>> {
    let enabled_mints = enabled_stable_mints_from_env()?;
    let config = FleetObservationConfig {
        cluster: opportunity.cluster.clone(),
        stablecoin_valuations: code_owned_stablecoin_valuations(&enabled_mints)?,
        enabled_mints,
        enable_cross_mint_jupiter: true,
        ..FleetObservationConfig::default()
    };
    let epoch = runtime.current_market_epoch(&config).await?;
    let source = epoch
        .reserves
        .iter()
        .find(|reserve| {
            opportunity.source_reserve.as_deref() == Some(reserve.reserve.as_str())
                && reserve.liquidity_mint == opportunity.source_liquidity_mint
        })
        .ok_or("current market epoch no longer has the recovery source reserve")?;
    let target = epoch
        .reserves
        .iter()
        .find(|reserve| {
            reserve.reserve == opportunity.target_reserve
                && reserve.liquidity_mint == opportunity.target_liquidity_mint
                && reserve.target_eligible
        })
        .ok_or("intended target is no longer eligible before the cross-mint swap")?;
    Ok((source.supply_apy_bps, target.supply_apy_bps))
}

fn validate_post_withdraw_swap_economics(
    execution_plan: &Value,
    source_amount_raw: i64,
    minimum_target_amount_raw: i64,
    source_apy_bps: i64,
    target_apy_bps: i64,
) -> Result<(), Box<dyn Error>> {
    if source_amount_raw <= 0 || minimum_target_amount_raw <= 0 {
        return Err(
            "post-withdraw economics require positive finalized and threshold amounts".into(),
        );
    }
    let costs = execution_plan
        .get("estimated_execution_costs")
        .ok_or("cross-mint plan is missing its persisted cost breakdown")?;
    if costs.get("kind").and_then(Value::as_str) != Some("cross_mint_jupiter") {
        return Err("cross-mint plan has the wrong persisted cost breakdown kind".into());
    }
    let swap_cost = costs
        .get("jupiter_swap_usd_micros")
        .and_then(Value::as_i64)
        .filter(|cost| *cost >= 0)
        .ok_or("cross-mint plan has no nonnegative Jupiter swap cost")?;
    let deposit_cost = costs
        .get("deposit_usd_micros")
        .and_then(Value::as_i64)
        .filter(|cost| *cost >= 0)
        .ok_or("cross-mint plan has no nonnegative deposit cost")?;
    let horizon = required_plan_i64(execution_plan, "holding_horizon_seconds")?;
    if horizon <= 0 {
        return Err("cross-mint holding horizon must be positive".into());
    }
    const YEAR_SECONDS: i128 = 365 * 24 * 60 * 60;
    const BPS: i128 = 10_000;
    let future_value = |principal: i64, apy_bps: i64| -> Option<i128> {
        let principal = i128::from(principal);
        principal.checked_add(
            principal
                .checked_mul(i128::from(apy_bps))?
                .checked_mul(i128::from(horizon))?
                .checked_div(YEAR_SECONDS.checked_mul(BPS)?)?,
        )
    };
    let source_recovery_value = future_value(source_amount_raw, source_apy_bps)
        .ok_or("source recovery future value overflowed")?
        .checked_sub(i128::from(deposit_cost))
        .ok_or("source recovery economics overflowed")?;
    let swap_and_deposit_value = future_value(minimum_target_amount_raw, target_apy_bps)
        .ok_or("swap-and-deposit future value overflowed")?
        .checked_sub(i128::from(swap_cost))
        .and_then(|value| value.checked_sub(i128::from(deposit_cost)))
        .ok_or("swap-and-deposit economics overflowed")?;
    if swap_and_deposit_value <= source_recovery_value {
        return Err(
            "fresh swap-and-deposit economics do not beat source recovery at the signed threshold"
                .into(),
        );
    }
    Ok(())
}

async fn cross_mint_capacity_reservation(
    runtime: &SameMintRouteRuntime,
    lease: &RebalanceOpportunityLease,
) -> Result<TargetCapacityReservationInput, Box<dyn Error>> {
    let opportunity = &lease.opportunity;
    let enabled_mints = enabled_stable_mints_from_env()?;
    let config = FleetObservationConfig {
        cluster: opportunity.cluster.clone(),
        stablecoin_valuations: code_owned_stablecoin_valuations(&enabled_mints)?,
        enabled_mints,
        enable_cross_mint_jupiter: true,
        ..FleetObservationConfig::default()
    };
    let epoch = runtime.current_market_epoch(&config).await?;
    let target = epoch
        .reserves
        .iter()
        .find(|reserve| {
            reserve.reserve == opportunity.target_reserve
                && reserve.liquidity_mint == opportunity.target_liquidity_mint
                && reserve.target_eligible
        })
        .ok_or("current market epoch no longer has an eligible cross-mint target")?;
    let source = epoch
        .reserves
        .iter()
        .find(|reserve| {
            opportunity.source_reserve.as_deref() == Some(reserve.reserve.as_str())
                && reserve.liquidity_mint == opportunity.source_liquidity_mint
        })
        .ok_or("current market epoch no longer has the cross-mint source")?;
    let observation = TargetCapacityObservation {
        cluster: opportunity.cluster.clone(),
        target_reserve: opportunity.target_reserve.clone(),
        liquidity_mint: opportunity.target_liquidity_mint.clone(),
        observed_supply_usd_micros: target.total_supply_usd_micros,
        observed_slot: target.slot,
        maximum_inflight_usd_micros: maximum_target_inflight_usd_micros(
            target.total_supply_usd_micros,
        ),
    };
    let projection = runtime.client.observe_target_capacity(observation).await?;
    let economic_opportunity = OpportunityInput {
        opportunity_id: opportunity.id,
        optimizer_epoch_id: opportunity.optimizer_epoch_id,
        vault_id: opportunity.vault_id.as_i64(),
        tenant_id: required_plan_string(&opportunity.execution_plan, "policy_authority")
            .unwrap_or_else(|_| "cross-mint".to_owned()),
        source_snapshot_id: opportunity
            .source_snapshot_id
            .map(SnapshotId::as_i64)
            .unwrap_or(opportunity.id)
            .max(1),
        observed_slot: target.slot.max(source.slot).max(1),
        mint: opportunity.target_liquidity_mint.clone(),
        source_reserve: opportunity
            .source_reserve
            .clone()
            .ok_or("cross-mint source reserve is absent")?,
        target_reserve: opportunity.target_reserve.clone(),
        notional_usd_micros: opportunity.principal_usd_micros,
        source_net_apy_bps: source.supply_apy_bps,
        target_net_apy_bps: target.supply_apy_bps,
        confidence_ppm: u32::try_from(required_plan_i64(
            &opportunity.execution_plan,
            "confidence_ppm",
        )?)?,
        expected_service_millis: u64::try_from(required_plan_i64(
            &opportunity.execution_plan,
            "expected_service_millis",
        )?)?,
        holding_horizon_seconds: u64::try_from(required_plan_i64(
            &opportunity.execution_plan,
            "holding_horizon_seconds",
        )?)?,
        estimated_execution_cost_usd_micros: required_plan_i64(
            &opportunity.execution_plan,
            "estimated_execution_cost_usd_micros",
        )?,
        age_seconds: 0,
        fairness_credit: 0,
        writable_conflict_keys: Vec::new(),
    };
    Ok(TargetCapacityReservationInput {
        projection,
        principal_usd_micros: opportunity.principal_usd_micros,
        economic_opportunity,
        current_observed_target_apy_bps: target.supply_apy_bps,
        economic_policy: EconomicPolicy::default(),
        fee_policy: RouteFeePolicy::default(),
    })
}

async fn fallback_target_observation(
    runtime: &SameMintRouteRuntime,
    movement: &CrossMintMovementRecord,
) -> Result<loyal_yield_store::fleet_orchestration::TargetCapacityProjection, Box<dyn Error>> {
    let enabled_mints = enabled_stable_mints_from_env()?;
    let config = FleetObservationConfig {
        cluster: movement.cluster.clone(),
        stablecoin_valuations: code_owned_stablecoin_valuations(&enabled_mints)?,
        enabled_mints,
        enable_cross_mint_jupiter: true,
        ..FleetObservationConfig::default()
    };
    let epoch = runtime.current_market_epoch(&config).await?;
    let fallback = epoch
        .reserves
        .iter()
        .filter(|reserve| {
            reserve.liquidity_mint == movement.target_mint
                && reserve.reserve != movement.active_target_reserve
                && reserve.target_eligible
        })
        .max_by(|left, right| {
            left.supply_apy_bps
                .cmp(&right.supply_apy_bps)
                .then_with(|| left.reserve.cmp(&right.reserve))
        })
        .ok_or("no safe same-target-mint fallback reserve is currently eligible")?;
    let observation = TargetCapacityObservation {
        cluster: movement.cluster.clone(),
        target_reserve: fallback.reserve.clone(),
        liquidity_mint: movement.target_mint.clone(),
        observed_supply_usd_micros: fallback.total_supply_usd_micros,
        observed_slot: fallback.slot,
        maximum_inflight_usd_micros: maximum_target_inflight_usd_micros(
            fallback.total_supply_usd_micros,
        ),
    };
    Ok(runtime.client.observe_target_capacity(observation).await?)
}

async fn current_target_reserve_is_eligible(
    runtime: &SameMintRouteRuntime,
    movement: &CrossMintMovementRecord,
    reserve: &str,
) -> Result<bool, Box<dyn Error>> {
    let enabled_mints = enabled_stable_mints_from_env()?;
    let config = FleetObservationConfig {
        cluster: movement.cluster.clone(),
        stablecoin_valuations: code_owned_stablecoin_valuations(&enabled_mints)?,
        enabled_mints,
        enable_cross_mint_jupiter: true,
        ..FleetObservationConfig::default()
    };
    let epoch = runtime.current_market_epoch(&config).await?;
    Ok(epoch.reserves.iter().any(|candidate| {
        candidate.reserve == reserve
            && candidate.liquidity_mint == movement.target_mint
            && candidate.target_eligible
    }))
}

fn cross_mint_cli_options(
    opportunity: &RebalanceOpportunityRecord,
    vault: &SelectedVault,
    rpc_url: String,
) -> CliOptions {
    CliOptions {
        settings: vault.settings.clone(),
        vault_index: vault.vault_index,
        direction: Direction::MainToPrime,
        source_reserve: opportunity.source_reserve.clone(),
        target_reserve: Some(opportunity.target_reserve.clone()),
        update_policy: false,
        update_active_policy: false,
        initial_deposit_reserve: None,
        initial_deposit_amount_raw: None,
        idle_vault_deposit_reserve: None,
        idle_vault_deposit_amount_raw: None,
        full_withdraw_main_usdc: false,
        full_withdraw_reserve: None,
        setup_obligation_reserve: None,
        e2e_deposit_amount_raw: None,
        execute: true,
        prepare_only: false,
        read_only: false,
        fused_execute: false,
        optimization_cycle: true,
        reconcile_from_chain: true,
        reconcile_current_positions: false,
        reconcile_reserves: Vec::new(),
        seed_from_user_position: false,
        expected_source_snapshot_id: opportunity.source_snapshot_id.map(SnapshotId::as_i64),
        expected_liquidity_mint: Some(opportunity.source_liquidity_mint.clone()),
        expected_amount_raw: Some(opportunity.amount_raw),
        expected_route_amount_semantics: opportunity
            .execution_plan
            .get("route_amount_semantics")
            .and_then(Value::as_str)
            .map(str::to_owned),
        expected_idle_token_account: None,
        expected_idle_observed_slot: None,
        expected_idle_observed_at: None,
        expected_source_apy_bps: Some(opportunity.source_apy_bps),
        expected_observed_target_apy_bps: Some(opportunity.target_apy_bps),
        expected_target_apy_bps: Some(opportunity.target_apy_bps),
        expected_edge_bps: Some(opportunity.estimated_edge_bps),
        principal_usd_micros: Some(opportunity.principal_usd_micros),
        confidence_ppm: opportunity
            .execution_plan
            .get("confidence_ppm")
            .and_then(Value::as_u64)
            .and_then(|value| u32::try_from(value).ok()),
        expected_service_millis: opportunity
            .execution_plan
            .get("expected_service_millis")
            .and_then(Value::as_u64),
        holding_horizon_seconds: opportunity
            .execution_plan
            .get("holding_horizon_seconds")
            .and_then(Value::as_u64),
        estimated_execution_cost_usd_micros: opportunity
            .execution_plan
            .get("estimated_execution_cost_usd_micros")
            .and_then(Value::as_i64),
        expected_cost_lamports: Some(opportunity.estimated_cost_lamports),
        current_economic_fee_cap_lamports: Some(opportunity.estimated_cost_lamports),
        expected_route_fee_payer: Some(
            policy_keypair_from_env()
                .map(|key| key.pubkey().to_string())
                .unwrap_or_default(),
        ),
        optimizer_epoch_id: Some(opportunity.optimizer_epoch_id),
        optimizer_market_slot: opportunity
            .execution_plan
            .get("optimizer_market_slot")
            .and_then(Value::as_i64),
        opportunity_id: Some(opportunity.id),
        opportunity_lease_owner: opportunity.lease_owner.clone(),
        opportunity_fencing_token: Some(opportunity.fencing_token),
        cluster: opportunity.cluster.clone(),
        rpc_url,
    }
}

fn signed_submission_input(
    lease: &CrossMintContinuationLease,
    prepared: PreparedCrossMintLeg,
    generation: i64,
) -> Result<SignedRouteSubmissionInput, Box<dyn Error>> {
    let bytes = bincode::serialize(&prepared.transaction)?;
    let signed_transaction_hash = format!("{:x}", Sha256::digest(&bytes));
    let message_hash = format!(
        "{:x}",
        Sha256::digest(bincode::serialize(&prepared.transaction.message)?)
    );
    let signature = prepared
        .transaction
        .signatures
        .first()
        .ok_or("cross-mint signed transaction has no signature")?
        .to_string();
    let fee_payer = prepared
        .transaction
        .message
        .static_account_keys()
        .first()
        .ok_or("cross-mint signed transaction has no fee payer")?
        .to_string();
    let recent_blockhash = prepared.transaction.message.recent_blockhash().to_string();
    Ok(SignedRouteSubmissionInput {
        cluster: lease.movement.cluster.clone(),
        semantic_key: format!(
            "cross-mint:{}:{}:{}",
            lease.movement.decision_id.as_i64(),
            prepared.leg.as_str(),
            generation
        ),
        opportunity_id: lease.movement.opportunity_id,
        decision_id: Some(lease.movement.decision_id),
        signed_transaction: bytes,
        signed_transaction_hash,
        message_hash,
        transaction_signature: signature,
        recent_blockhash,
        last_valid_block_height: prepared.last_valid_block_height,
        source_snapshot_id: lease.movement.source_snapshot_id,
        optimizer_epoch_id: prepared.optimizer_epoch_id,
        alt_requirements_fingerprint: prepared.alt_requirements_fingerprint,
        alt_selection_fingerprint: prepared.alt_selection_fingerprint,
        alt_mutation_epochs: prepared.alt_mutation_epochs,
        fee_payer,
        fee_payer_kind: RouteFeePayerKind::Policy,
        fee_payer_balance_lamports: None,
        fee_payer_balance_slot: None,
        fee_payer_balance_observed_at: None,
        policy_setup_funding_lamports: None,
        compiled_fee_lamports: prepared.compiled_fee_lamports,
        writable_account_keys: prepared.writable_account_keys,
        conflict_account_keys: prepared.conflict_account_keys,
        executor_owner: lease.owner.clone(),
        executor_fencing_token: lease.fencing_token,
    })
}

async fn next_leg_generation(
    client: &NeonSqlClient,
    decision_id: DecisionId,
    leg: CrossMintMovementLeg,
) -> Result<i64, Box<dyn Error>> {
    let maximum: Option<i64> = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT max(leg_generation)
        FROM loyal_yield.signed_route_submissions
        WHERE decision_id = $1
          AND movement_leg = $2
        "#,
    )
    .bind(decision_id.as_i64())
    .bind(leg.as_str())
    .fetch_one(client.pool())
    .await?;
    maximum
        .unwrap_or(0)
        .checked_add(1)
        .ok_or_else(|| "cross-mint leg generation overflowed".into())
}

#[cfg(test)]
mod cross_mint_reconciliation_tests {
    use super::*;
    use solana_transaction_status_client_types::{
        TransactionConfirmationStatus, UiLoadedAddresses,
    };

    #[test]
    fn source_idle_rollout_or_preparation_failure_selects_source_recovery() {
        assert_eq!(
            select_source_idle_continuation(true, true),
            SourceIdleContinuation::Swap
        );
        assert_eq!(
            select_source_idle_continuation(true, false),
            SourceIdleContinuation::RecoverSource
        );
        assert_eq!(
            select_source_idle_continuation(false, true),
            SourceIdleContinuation::RecoverSource
        );
    }

    #[test]
    fn target_deposit_failure_selects_one_bound_fallback() {
        assert_eq!(
            select_target_idle_continuation(true, true, true),
            TargetIdleContinuation::DepositPrimary
        );
        assert_eq!(
            select_target_idle_continuation(true, true, false),
            TargetIdleContinuation::RebindFallback
        );
        assert_eq!(
            select_target_idle_continuation(true, false, false),
            TargetIdleContinuation::RebindFallback
        );
        assert_eq!(
            select_target_idle_continuation(false, true, false),
            TargetIdleContinuation::StopAtBoundFallback
        );
        assert_eq!(
            select_target_idle_continuation(false, false, false),
            TargetIdleContinuation::StopAtBoundFallback
        );
    }

    #[test]
    fn value_loss_is_bounded_by_signed_threshold_not_optimistic_quote() {
        validate_signed_minimum_output_value_loss(1_000_000, 995_000, 50).unwrap();
        let error = validate_signed_minimum_output_value_loss(1_000_000, 990_025, 50).unwrap_err();
        assert!(error.to_string().contains("signed Jupiter minimum output"));
    }

    #[test]
    fn generalized_swap_binding_accepts_a_tighter_rollout_cap_and_maps_both_dialects() {
        let mut lane = JupiterLaneContract {
            program_id: loyal_actions::JUPITER_V6_PROGRAM_ID,
            dialect_constraint_indexes: BTreeMap::from([
                (JupiterV2Dialect::RouteV2, 0),
                (JupiterV2Dialect::SharedAccountsRouteV2, 1),
            ]),
            maximum_slippage_bps: 50,
            action_account: Pubkey::new_unique(),
        };

        validate_jupiter_lane(&lane, 25).unwrap();
        assert_eq!(lane.constraint_index(JupiterV2Dialect::RouteV2).unwrap(), 0);
        assert_eq!(
            lane.constraint_index(JupiterV2Dialect::SharedAccountsRouteV2)
                .unwrap(),
            1
        );
        lane.dialect_constraint_indexes
            .insert(JupiterV2Dialect::SharedAccountsRouteV2, 0);
        assert!(validate_jupiter_lane(&lane, 25).is_err());
    }

    #[test]
    fn zero_generalized_policy_slippage_is_rejected_at_binding_boundary() {
        let lane = JupiterLaneContract {
            program_id: loyal_actions::JUPITER_V6_PROGRAM_ID,
            dialect_constraint_indexes: BTreeMap::from([(JupiterV2Dialect::RouteV2, 0)]),
            maximum_slippage_bps: 0,
            action_account: Pubkey::new_unique(),
        };

        assert!(validate_jupiter_lane(&lane, 25).is_err());
    }

    #[test]
    fn generalized_readback_rejects_stale_and_malformed_bound_fingerprints() {
        let detected = loyal_actions::DetectedJupiterCrossMintPolicyAccount {
            settings: Pubkey::new_unique(),
            policy_seed: 7,
            policy_account: Pubkey::new_unique(),
            account_index: 2,
            vault: Pubkey::new_unique(),
            delegated_signer: Pubkey::new_unique(),
            threshold: 1,
            source_shard: loyal_actions::jupiter::JupiterCrossMintSourceShard::Classic,
            max_slippage_bps: 50,
            daily_source_mint_spending_cap: 1_000_000,
            dialect_constraint_indexes: BTreeMap::from([
                (JupiterV2Dialect::RouteV2, 0),
                (JupiterV2Dialect::SharedAccountsRouteV2, 1),
            ]),
        };
        let valid = loyal_actions::generalized_cross_mint_manifest_fingerprint(
            &detected.manifest_semantics(),
        );

        validate_generalized_manifest_fingerprint(&detected, &valid).unwrap();
        assert!(validate_generalized_manifest_fingerprint(&detected, "not-a-fingerprint").is_err());
        assert!(
            validate_generalized_manifest_fingerprint(&detected, &format!("{valid}0")).is_err()
        );
    }

    #[test]
    fn generalized_plan_binding_preserves_manifest_and_defers_dialect_selection() {
        let plan = json!({
            "policy_bindings": {
                "settings": "settings",
                "vault_index": 0,
                "vault_pubkey": "vault",
                "delegated_signer": "signer",
                "withdraw": {
                    "policy_account": "withdraw-policy",
                    "observed_slot": 10,
                    "observed_signature": "withdraw-signature",
                    "source_commitment": "finalized",
                    "constraint_index": 0,
                },
                "swap": {
                    "policy_account": Pubkey::new_unique().to_string(),
                    "source_shard": "classic",
                    "enrollment_generation": 1,
                    "observed_slot": 11,
                    "observed_signature": "swap-signature",
                    "source_commitment": "finalized",
                    "max_slippage_bps": 50,
                    "daily_source_mint_spending_cap": 1_000_000,
                    "manifest_fingerprint": "a".repeat(64),
                },
                "deposit": {
                    "policy_account": "deposit-policy",
                    "observed_slot": 12,
                    "observed_signature": "deposit-signature",
                    "source_commitment": "finalized",
                    "constraint_index": 1,
                },
            }
        });

        let bindings = cross_mint_policy_bindings(&plan).unwrap();
        assert_eq!(bindings.swap.source_shard, "classic");
        assert_eq!(bindings.swap.enrollment_generation, 1);
        assert_eq!(bindings.swap.manifest_fingerprint, "a".repeat(64));
        let lane = jupiter_lane_contract(&plan).unwrap();
        assert_eq!(lane.constraint_index(JupiterV2Dialect::RouteV2).unwrap(), 0);
        assert_eq!(
            lane.constraint_index(JupiterV2Dialect::SharedAccountsRouteV2)
                .unwrap(),
            1
        );
    }

    #[test]
    fn deterministic_store_reconciliation_failures_quarantine_but_transient_classes_retry() {
        let deterministic = classify_reconciliation_store_error(OrchestratorError::StoreInvariant(
            "finalized effect drift".to_owned(),
        ));
        assert!(reconciliation_error_requires_quarantine(
            deterministic.as_ref()
        ));

        let non_invariant =
            classify_reconciliation_store_error(OrchestratorError::AmountOutOfRange {
                value: u64::MAX,
            });
        assert!(!reconciliation_error_requires_quarantine(
            non_invariant.as_ref()
        ));
    }

    #[test]
    fn late_cross_mint_status_preserves_finality_before_reconciliation() {
        let finalized = TransactionStatus {
            slot: 42,
            confirmations: None,
            status: Ok(()),
            err: None,
            confirmation_status: Some(TransactionConfirmationStatus::Finalized),
        };
        assert!(matches!(
            cross_mint_late_status_outcome(&finalized).unwrap(),
            ExpiredRouteCheckOutcome::Finalized { slot: 42 }
        ));

        let confirmed = TransactionStatus {
            slot: 41,
            confirmations: Some(2),
            status: Ok(()),
            err: None,
            confirmation_status: Some(TransactionConfirmationStatus::Confirmed),
        };
        assert!(matches!(
            cross_mint_late_status_outcome(&confirmed).unwrap(),
            ExpiredRouteCheckOutcome::Confirmed { slot: 41 }
        ));

        let processed = TransactionStatus {
            slot: 40,
            confirmations: Some(0),
            status: Ok(()),
            err: None,
            confirmation_status: Some(TransactionConfirmationStatus::Processed),
        };
        assert!(matches!(
            cross_mint_late_status_outcome(&processed).unwrap(),
            ExpiredRouteCheckOutcome::SeenUnconfirmed { .. }
        ));
    }

    fn ui_balance(
        index: u8,
        mint: Pubkey,
        owner: Pubkey,
        amount: i64,
    ) -> UiTransactionTokenBalance {
        let token_program = loyal_actions::earn_stablecoin(mint)
            .expect("test balance must use a canonical Earn mint")
            .token_program;
        serde_json::from_value(json!({
            "accountIndex": index,
            "mint": mint.to_string(),
            "uiTokenAmount": {
                "uiAmount": null,
                "decimals": 6,
                "amount": amount.to_string(),
                "uiAmountString": amount.to_string(),
            },
            "owner": owner.to_string(),
            "programId": token_program.to_string(),
        }))
        .unwrap()
    }

    fn meta(
        pre: UiTransactionTokenBalance,
        post: UiTransactionTokenBalance,
    ) -> UiTransactionStatusMeta {
        UiTransactionStatusMeta {
            err: None,
            status: Ok(()),
            fee: 5_000,
            pre_balances: vec![1],
            post_balances: vec![1],
            inner_instructions: OptionSerializer::None,
            log_messages: OptionSerializer::None,
            pre_token_balances: OptionSerializer::Some(vec![pre]),
            post_token_balances: OptionSerializer::Some(vec![post]),
            rewards: OptionSerializer::None,
            loaded_addresses: OptionSerializer::Some(UiLoadedAddresses::default()),
            return_data: OptionSerializer::Skip,
            compute_units_consumed: OptionSerializer::Skip,
            cost_units: OptionSerializer::Skip,
        }
    }

    #[test]
    fn finalized_debit_preserves_preexisting_balance_and_attributes_only_delta() {
        let mint = loyal_actions::USDC_MINT;
        let owner = Pubkey::new_unique();
        let token_account = Pubkey::new_unique();
        let metadata = meta(
            ui_balance(0, mint, owner, 1_000_023),
            ui_balance(0, mint, owner, 100_023),
        );
        let expected = TokenBalanceAnchor {
            mint: mint.to_string(),
            token_account: token_account.to_string(),
            amount_raw: 1_000_023,
        };

        let (delta, post) =
            finalized_balance_delta(&metadata, &[token_account], Some(&expected), owner, true)
                .unwrap();

        assert_eq!(delta.unwrap().amount_raw, 900_000);
        assert_eq!(post.unwrap().amount_raw, 100_023);
    }

    #[test]
    fn finalized_credit_uses_transaction_delta_not_quote_or_aggregate_balance() {
        let mint = loyal_actions::USDC_MINT;
        let owner = Pubkey::new_unique();
        let token_account = Pubkey::new_unique();
        let metadata = meta(
            ui_balance(0, mint, owner, 23),
            ui_balance(0, mint, owner, 895_023),
        );
        let expected = TokenBalanceAnchor {
            mint: mint.to_string(),
            token_account: token_account.to_string(),
            amount_raw: 23,
        };

        let (delta, post) =
            finalized_balance_delta(&metadata, &[token_account], Some(&expected), owner, false)
                .unwrap();

        assert_eq!(delta.unwrap().amount_raw, 895_000);
        assert_eq!(post.unwrap().amount_raw, 895_023);
    }

    #[test]
    fn finalized_balance_owner_mutation_fails_closed() {
        let mint = loyal_actions::USDC_MINT;
        let owner = Pubkey::new_unique();
        let wrong_owner = Pubkey::new_unique();
        let token_account = Pubkey::new_unique();
        let metadata = meta(
            ui_balance(0, mint, wrong_owner, 100),
            ui_balance(0, mint, wrong_owner, 90),
        );
        let expected = TokenBalanceAnchor {
            mint: mint.to_string(),
            token_account: token_account.to_string(),
            amount_raw: 100,
        };

        let error =
            finalized_balance_delta(&metadata, &[token_account], Some(&expected), owner, true)
                .unwrap_err();

        assert!(error.to_string().contains("mint, owner, or program"));
    }

    #[test]
    fn finalized_token_2022_credit_uses_the_canonical_program_metadata() {
        let mint = loyal_actions::PYUSD_MINT;
        let owner = Pubkey::new_unique();
        let token_account = Pubkey::new_unique();
        let metadata = meta(
            ui_balance(0, mint, owner, 7),
            ui_balance(0, mint, owner, 500_007),
        );
        let expected = TokenBalanceAnchor {
            mint: mint.to_string(),
            token_account: token_account.to_string(),
            amount_raw: 7,
        };

        let (delta, post) =
            finalized_balance_delta(&metadata, &[token_account], Some(&expected), owner, false)
                .unwrap();

        assert_eq!(delta.unwrap().amount_raw, 500_000);
        assert_eq!(post.unwrap().amount_raw, 500_007);
    }

    #[test]
    fn post_withdraw_economics_compare_remaining_swap_path_with_source_recovery() {
        let plan = json!({
            "holding_horizon_seconds": 365 * 24 * 60 * 60,
            "estimated_execution_costs": {
                "kind": "cross_mint_jupiter",
                "withdraw_usd_micros": 9_999_999,
                "jupiter_swap_usd_micros": 1_000,
                "deposit_usd_micros": 500,
            }
        });

        validate_post_withdraw_swap_economics(&plan, 1_000_000, 999_000, 100, 500)
            .expect("remaining target path beats source recovery despite sunk withdrawal cost");
        let error =
            validate_post_withdraw_swap_economics(&plan, 1_000_000, 950_000, 100, 200).unwrap_err();
        assert!(error.to_string().contains("do not beat source recovery"));
    }
}

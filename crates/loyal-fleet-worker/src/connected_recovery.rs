//! Disposable connected-fixture crash boundaries. No queue rows, signed wires,
//! receipts, or custody state are manufactured here.
use super::*;
use std::sync::Mutex;

static INITIAL_ARMED: Mutex<Option<i64>> = Mutex::new(None);
static INITIAL_CAPTURED: Mutex<Option<(CrossMintContinuationLease, CrossMintLegPublicationInput)>> =
    Mutex::new(None);

pub(super) fn arm_initial_withdraw(opportunity_id: i64) {
    assert!(INITIAL_CAPTURED.lock().unwrap().is_none());
    assert!(INITIAL_ARMED
        .lock()
        .unwrap()
        .replace(opportunity_id)
        .is_none());
}

pub(crate) fn initial_withdraw_crash_armed(opportunity_id: i64) -> bool {
    *INITIAL_ARMED.lock().unwrap() == Some(opportunity_id)
}

pub(crate) fn capture_initial_withdraw(
    lease: CrossMintContinuationLease,
    leg: PreparedCrossMintLeg,
) -> Result<(), Box<dyn Error>> {
    if INITIAL_ARMED.lock().unwrap().take() != Some(lease.movement.opportunity_id) {
        return Err("unexpected initial withdrawal crash capture".into());
    }
    let input = CrossMintLegPublicationInput {
        leg: leg.leg,
        purpose: leg.purpose,
        generation: 1,
        policy_account: leg.policy_account.clone(),
        expected_effect: leg.expected_effect.clone(),
        expected_balance_anchors: leg.expected_balance_anchors.clone(),
        submission: signed_submission_input(&lease, leg, 1)?,
    };
    *INITIAL_CAPTURED.lock().unwrap() = Some((lease, input));
    Ok(())
}

pub(super) async fn cross_mint_reconcile_during_expiry(
    runtime: &SameMintRouteRuntime,
    lease: &SignedRouteSubmissionLease,
) -> Result<(), Box<dyn Error>> {
    let decision_id = lease
        .submission
        .decision_id
        .ok_or("reconciliation decision missing")?;
    let before: Value =
        sqlx::query_scalar("SELECT to_jsonb(d) FROM loyal_yield.rebalance_decisions d WHERE id=$1")
            .bind(decision_id.as_i64())
            .fetch_one(runtime.client.pool())
            .await?;
    let signed_before = wires(&runtime.client, lease.submission.opportunity_id).await?;
    let mut blocker = runtime.client.pool().begin().await?;
    let blocker_pid: i32 = sqlx::query_scalar("SELECT pg_backend_pid()")
        .fetch_one(&mut *blocker)
        .await?;
    sqlx::query("SELECT id FROM loyal_yield.rebalance_decisions WHERE id=$1 FOR UPDATE")
        .bind(decision_id.as_i64())
        .fetch_one(&mut *blocker)
        .await?;
    let release = async {
        let mut waiting = false;
        for _ in 0..50 {
            waiting = sqlx::query_scalar(
                "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))",
            )
            .bind(blocker_pid)
            .fetch_one(runtime.client.pool())
            .await?;
            if waiting {
                break;
            }
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        if !waiting || Utc::now() >= lease.expires_at {
            return Err::<(), Box<dyn Error>>(
                "reconciliation did not enter DB write under a live lease".into(),
            );
        }
        tokio::time::sleep(
            (lease.expires_at - Utc::now()).to_std().unwrap_or_default()
                + Duration::from_millis(100),
        )
        .await;
        blocker.rollback().await?;
        Ok(())
    };
    let (result, release_result) =
        tokio::join!(reconcile_finalized_submission(runtime, lease), release);
    release_result?;
    match result {
        Ok(_) => return Err("cross-mint reconciliation crossed its lease expiry".into()),
        Err(error)
            if error
                .to_string()
                .contains("cross-mint effect receipt lost its reconciliation fence") => {}
        Err(error) => {
            return Err(
                format!("blocked reconciliation failed outside its lease fence: {error}").into(),
            )
        }
    }
    let after: Value =
        sqlx::query_scalar("SELECT to_jsonb(d) FROM loyal_yield.rebalance_decisions d WHERE id=$1")
            .bind(decision_id.as_i64())
            .fetch_one(runtime.client.pool())
            .await?;
    if before != after
        || wires(&runtime.client, lease.submission.opportunity_id).await? != signed_before
    {
        return Err("expired reconciliation changed custody or signed work".into());
    }
    Ok(())
}

async fn restarted_client() -> Result<NeonSqlClient, Box<dyn Error>> {
    Ok(NeonSqlClient::connect(NeonSqlConfig::new(env::var("FLEET_TEST_DATABASE_URL")?)).await?)
}

pub(super) async fn same_mint_after_pre_persistence_crash(
    runtime: &SameMintRouteRuntime,
    old: RebalanceOpportunityLease,
    rpc_url: &str,
) -> Result<RebalanceOpportunityLease, Box<dyn Error>> {
    let before = wires(&runtime.client, old.opportunity.id).await?;
    if before != json!([]) {
        return Err("pre-persistence boundary already has signed work".into());
    }
    let chain_before = chain_evidence(runtime)?;
    crate::connected_faults::arm(old.opportunity.id);
    let request =
        same_mint_request_from_opportunity(&old, rpc_url, RebalanceOpportunityClaimKind::Execute)?;
    let result = execute_same_mint_route_with_runtime(request, runtime, None).await;
    let captured = crate::connected_faults::take()
        .ok_or("same-mint did not reach signed pre-persistence crash boundary")?;
    if result.state == SameMintRouteExecutionState::SubmissionQueued
        || captured.submission.signed_transaction.is_empty()
        || captured.lease.opportunity.id != old.opportunity.id
    {
        return Err("same-mint crash did not interrupt the exact signed Go work".into());
    }
    if runtime
        .client
        .lease_next_rebalance_opportunity(
            &old.opportunity.cluster,
            "connected-early-same-executor",
            RebalanceOpportunityClaimKind::Execute,
            Utc::now() + ChronoDuration::seconds(60),
        )
        .await?
        .is_some()
    {
        return Err("pre-persistence live owner admitted duplicate execution".into());
    }
    // Start the real atomic write while the lease is live, but hold its first
    // vault-row lock until the lease expires. This proves validation is not
    // merely a pre-transaction clock check.
    let mut blocker = runtime.client.pool().begin().await?;
    let blocker_pid: i32 = sqlx::query_scalar("SELECT pg_backend_pid()")
        .fetch_one(&mut *blocker)
        .await?;
    sqlx::query("SELECT id FROM loyal_yield.managed_vaults WHERE id=$1 FOR UPDATE")
        .bind(old.opportunity.vault_id.as_i64())
        .fetch_one(&mut *blocker)
        .await?;
    let blocked_client = restarted_client().await?;
    let (input, lease, capacity, submission) = (
        captured.input.clone(),
        captured.lease.clone(),
        captured.capacity.clone(),
        captured.submission.clone(),
    );
    let blocked_write = tokio::spawn(async move {
        blocked_client
            .prepare_same_mint_rebalance_with_signed_submission(input, &lease, capacity, submission)
            .await
    });
    let mut waiting = false;
    for _ in 0..50 {
        waiting = sqlx::query_scalar(
            "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))",
        )
        .bind(blocker_pid)
        .fetch_one(runtime.client.pool())
        .await?;
        if waiting {
            break;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    if !waiting || Utc::now() >= captured.lease.expires_at {
        blocked_write.abort();
        return Err("signed write never reached the held DB lock under a live lease".into());
    }
    let wait = (captured.lease.expires_at - Utc::now())
        .to_std()
        .unwrap_or_default();
    tokio::time::sleep(wait + Duration::from_millis(100)).await;
    blocker.rollback().await?;
    match blocked_write.await? {
        Err(error)
            if error.to_string().contains("expired") || error.to_string().contains("fenced") => {}
        other => return Err(format!("same-mint write crossed its lease expiry: {other:?}").into()),
    }
    let restarted = restarted_client().await?;
    let next = restarted
        .lease_next_rebalance_opportunity(
            &old.opportunity.cluster,
            "connected-restarted-same-executor",
            RebalanceOpportunityClaimKind::Execute,
            Utc::now() + ChronoDuration::seconds(60),
        )
        .await?
        .ok_or("same-mint expired execute lease did not recover")?;
    if next.opportunity.id != old.opportunity.id
        || next.opportunity.optimizer_epoch_id != old.opportunity.optimizer_epoch_id
        || next.opportunity.execution_plan != old.opportunity.execution_plan
        || next.fencing_token <= captured.lease.fencing_token
        || next.owner == captured.lease.owner
    {
        return Err("same-mint restart substituted Go work or lost the owner fence".into());
    }
    let durable_before: Value = sqlx::query_scalar(
        "SELECT to_jsonb(o) FROM loyal_yield.rebalance_opportunities o WHERE id=$1",
    )
    .bind(old.opportunity.id)
    .fetch_one(restarted.pool())
    .await?;
    match restarted
        .prepare_same_mint_rebalance_with_signed_submission(
            captured.input,
            &captured.lease,
            captured.capacity,
            captured.submission,
        )
        .await
    {
        Err(error)
            if error.to_string().contains("expired") || error.to_string().contains("fenced") => {}
        other => {
            return Err(format!(
                "stale same-mint append did not fail on its owner fence: {other:?}"
            )
            .into())
        }
    }
    let durable_after: Value = sqlx::query_scalar(
        "SELECT to_jsonb(o) FROM loyal_yield.rebalance_opportunities o WHERE id=$1",
    )
    .bind(old.opportunity.id)
    .fetch_one(restarted.pool())
    .await?;
    if durable_before != durable_after || wires(&restarted, old.opportunity.id).await? != before {
        return Err("pre-persistence crash or stale append changed durable work".into());
    }
    let decisions: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.rebalance_decisions WHERE vault_id=$1",
    )
    .bind(old.opportunity.vault_id.as_i64())
    .fetch_one(restarted.pool())
    .await?;
    if decisions != 0 {
        return Err("crashed same-mint preparation left a decision".into());
    }
    unchanged_chain(&chain_before, &chain_evidence(runtime)?)?;
    // Reopen the same Go-persisted evidence after restart; only cache fetch
    // time changes, never the market observation or expiry timestamp.
    let persisted: Value =
        sqlx::query_scalar("SELECT market_state FROM loyal_yield.optimizer_epochs WHERE id=$1")
            .bind(next.opportunity.optimizer_epoch_id)
            .fetch_one(restarted.pool())
            .await?;
    let mints = enabled_stable_mints_from_env()?;
    runtime.market_epoch_cache.lock().await.insert(
        format!("{}:{}", next.opportunity.cluster, mints.join(",")),
        CachedMarketEpoch {
            epoch: serde_json::from_value(persisted)?,
            fetched_at: Instant::now(),
        },
    );
    eprintln!("connected same-mint pre-persistence recovery: signed-input-captured=true; stale-owner-rejected=true; no-decision-or-submission=true; no-chain-effects=true");
    Ok(next)
}

async fn wires(client: &NeonSqlClient, opportunity_id: i64) -> Result<Value, Box<dyn Error>> {
    Ok(sqlx::query_scalar(
        "SELECT COALESCE(jsonb_agg(jsonb_build_object('id',id,'semanticKey',semantic_key, \
         'signature',transaction_signature,'wire',encode(signed_transaction,'hex'), \
         'hash',signed_transaction_hash,'state',submission_state, \
         'broadcastCount',broadcast_count,'expectedEffect',expected_effect, \
         'anchors',expected_balance_anchors,'leg',movement_leg,'generation',leg_generation, \
         'effect',reconciled_effect,'slot',reconciled_slot) ORDER BY id),'[]'::jsonb) \
         FROM loyal_yield.signed_route_submissions WHERE opportunity_id=$1",
    )
    .bind(opportunity_id)
    .fetch_one(client.pool())
    .await?)
}

fn chain_evidence(runtime: &SameMintRouteRuntime) -> Result<Value, Box<dyn Error>> {
    Ok(runtime.rpc.send(
        solana_client::rpc_request::RpcRequest::Custom {
            method: "executionEvidence",
        },
        json!([]),
    )?)
}

fn unchanged_chain(before: &Value, after: &Value) -> Result<(), Box<dyn Error>> {
    for key in ["submissionAttempts", "transactions", "receipts", "accounts"] {
        if before.get(key).is_none() || before[key].is_null() || before[key] != after[key] {
            return Err(
                format!("recovery boundary changed or omitted chain evidence: {key}").into(),
            );
        }
    }
    Ok(())
}

/// Crash after retained preparation/signing but before signed persistence, in
/// source-idle and target-idle custody. The minimum real continuation lease is
/// ten seconds; wait for it rather than editing DB timestamps.
pub(super) async fn continue_after_pre_persistence_crash(
    runtime: &SameMintRouteRuntime,
    options: &FleetWorkerOptions,
    config: &CrossMintWorkerConfig,
    opportunity_id: i64,
    completed_legs: usize,
) -> Result<CrossMintWorkResult, Box<dyn Error>> {
    let before = wires(&runtime.client, opportunity_id).await?;
    let rows = before.as_array().ok_or("missing durable leg evidence")?;
    if rows.len() != completed_legs || rows.iter().any(|row| row["state"] != "reconciled") {
        return Err("between-leg restart requires exactly the preceding reconciled legs".into());
    }
    let chain_before = chain_evidence(runtime)?;
    let captured = if completed_legs == 0 {
        Some(
            INITIAL_CAPTURED
                .lock()
                .unwrap()
                .take()
                .ok_or("initial withdrawal never reached signed crash boundary")?,
        )
    } else {
        None
    };
    let (old, captured_input) = if let Some((lease, input)) = captured {
        (lease, Some(input))
    } else {
        (
            runtime
                .client
                .claim_cross_mint_continuation(
                    &options.cluster,
                    "connected-crashed-before-persistence",
                    10,
                )
                .await?
                .ok_or("between-leg continuation was not claimable")?,
            None,
        )
    };
    if old.movement.opportunity_id != opportunity_id {
        return Err("pre-persistence recovery claimed substituted work".into());
    }
    let expected_phase = match completed_legs {
        0 => CrossMintCustodyPhase::SourceReserve,
        1 => CrossMintCustodyPhase::SourceIdle,
        2 => CrossMintCustodyPhase::TargetIdle,
        _ => return Err("unsupported between-leg crash boundary".into()),
    };
    if old.movement.phase != expected_phase {
        return Err("between-leg crash has unexpected custody phase".into());
    }
    // Use the actual retained signed input, never a fabricated payload.
    let stale_input = if let Some(input) = captured_input {
        input
    } else {
        let prepared = prepare_next_leg(runtime, config, &old).await?;
        let generation =
            next_leg_generation(&runtime.client, old.movement.decision_id, prepared.leg.leg)
                .await?;
        CrossMintLegPublicationInput {
            leg: prepared.leg.leg,
            purpose: prepared.leg.purpose,
            generation,
            policy_account: prepared.leg.policy_account.clone(),
            expected_effect: prepared.leg.expected_effect.clone(),
            expected_balance_anchors: prepared.leg.expected_balance_anchors.clone(),
            submission: signed_submission_input(&old, prepared.leg, generation)?,
        }
    };
    if runtime
        .client
        .claim_cross_mint_continuation(&options.cluster, "connected-early-continuation", 10)
        .await?
        .is_some()
    {
        return Err("live continuation lease admitted another owner".into());
    }
    // Start the real append under a live lease and release its DB lock only
    // after expiry; never alter custody or lease timestamps in the fixture.
    let mut blocker = runtime.client.pool().begin().await?;
    let blocker_pid: i32 = sqlx::query_scalar("SELECT pg_backend_pid()")
        .fetch_one(&mut *blocker)
        .await?;
    if completed_legs == 0 {
        // Initial leg: expiry while acquiring the authority row.
        sqlx::query("SELECT id FROM loyal_yield.rebalance_decisions WHERE id=$1 FOR UPDATE")
            .bind(old.movement.decision_id.as_i64())
            .fetch_one(&mut *blocker)
            .await?;
    } else {
        // Later legs: expiry after authority validation, inside signed-byte
        // persistence. The final checked update must roll back the entire tx.
        sqlx::query("LOCK TABLE loyal_yield.signed_route_submissions IN SHARE MODE")
            .execute(&mut *blocker)
            .await?;
    }
    let blocked_client = restarted_client().await?;
    let (blocked_lease, blocked_input) = (old.clone(), stale_input.clone());
    let blocked_write = tokio::spawn(async move {
        blocked_client
            .append_cross_mint_leg(&blocked_lease, blocked_input)
            .await
    });
    let mut waiting = false;
    for _ in 0..50 {
        waiting = sqlx::query_scalar(
            "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))",
        )
        .bind(blocker_pid)
        .fetch_one(runtime.client.pool())
        .await?;
        if waiting {
            break;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    if !waiting || Utc::now() >= old.expires_at {
        blocked_write.abort();
        return Err("cross-mint append never waited on its DB lock under a live lease".into());
    }
    let wait = (old.expires_at - Utc::now()).to_std().unwrap_or_default();
    tokio::time::sleep(wait + Duration::from_millis(100)).await;
    blocker.rollback().await?;
    match blocked_write.await? {
        Err(error)
            if error.to_string().contains("expired") || error.to_string().contains("fenced") => {}
        Ok(_) => return Err("cross-mint signed append crossed its lease expiry".into()),
        Err(error) => {
            return Err(format!(
                "cross-mint blocked append failed outside its lease fence: {error}"
            )
            .into())
        }
    }
    let restarted = restarted_client().await?;
    let next = restarted
        .claim_cross_mint_continuation(
            &options.cluster,
            "connected-restarted-before-persistence",
            60,
        )
        .await?
        .ok_or("expired continuation did not recover")?;
    if next.movement.decision_id != old.movement.decision_id
        || next.movement.opportunity_id != opportunity_id
        || next.movement.phase != old.movement.phase
        || next.movement.custody_version != old.movement.custody_version
        || next.movement.custody_account != old.movement.custody_account
        || next.movement.custody_mint != old.movement.custody_mint
        || next.movement.custody_amount_raw != old.movement.custody_amount_raw
        || next.movement.custody_observed_balance_raw != old.movement.custody_observed_balance_raw
        || next.movement.custody_reconciled_slot != old.movement.custody_reconciled_slot
        || next.owner == old.owner
        || next.fencing_token <= old.fencing_token
    {
        return Err("continuation restart changed custody or failed to advance owner fence".into());
    }
    match restarted.append_cross_mint_leg(&old, stale_input).await {
        Err(error)
            if error
                .to_string()
                .contains("continuation lease is stale, expired, or fenced") => {}
        other => {
            return Err(
                format!("stale append was not rejected by the owner fence: {other:?}").into(),
            );
        }
    }
    if wires(&restarted, opportunity_id).await? != before {
        return Err("pre-persistence crash or stale owner changed durable submissions".into());
    }
    unchanged_chain(&chain_before, &chain_evidence(runtime)?)?;
    // Restart reloads the same durable Go epoch, not a synthetic fresh quote.
    let opportunity = restarted
        .rebalance_opportunity(opportunity_id)
        .await?
        .ok_or("recovery lost Go opportunity")?;
    let persisted: Value =
        sqlx::query_scalar("SELECT market_state FROM loyal_yield.optimizer_epochs WHERE id=$1")
            .bind(opportunity.optimizer_epoch_id)
            .fetch_one(restarted.pool())
            .await?;
    let mints = enabled_stable_mints_from_env()?;
    runtime.market_epoch_cache.lock().await.insert(
        format!("{}:{}", options.cluster, mints.join(",")),
        CachedMarketEpoch {
            epoch: serde_json::from_value(persisted)?,
            fetched_at: Instant::now(),
        },
    );
    let prepared = prepare_next_leg(runtime, config, &next).await?;
    let result = publish_prepared_leg(&restarted, prepared.lease, prepared.leg).await?;
    let after = wires(&restarted, opportunity_id).await?;
    let after_rows = after.as_array().ok_or("missing restarted publication")?;
    if after_rows.len() != completed_legs + 1
        || after_rows[..completed_legs] != rows[..]
        || after_rows[completed_legs]["state"] != "signed"
    {
        return Err("restart duplicated a submission or changed an earlier persisted leg".into());
    }
    unchanged_chain(&chain_before, &chain_evidence(runtime)?)?;
    eprintln!(
        "connected recovery: completed_legs={completed_legs}; pre-persistence-owner-fenced=true; prior-wires-unchanged=true"
    );
    Ok(result)
}

/// Crash after signed persistence and before broadcast. Recover from a fresh
/// DB client; neither owner receives a signer or submits bytes here. Release
/// the recovered claim through the retained defer API for the real CLI.
pub(super) async fn recover_persisted_before_broadcast(
    runtime: &SameMintRouteRuntime,
    cluster: &str,
    opportunity_id: i64,
    leg_index: usize,
) -> Result<(), Box<dyn Error>> {
    let before = wires(&runtime.client, opportunity_id).await?;
    let chain_before = chain_evidence(runtime)?;
    let old = runtime
        .client
        .lease_pending_signed_route_submissions(
            cluster,
            "connected-crashed-before-broadcast",
            1,
            Utc::now() + ChronoDuration::seconds(2),
        )
        .await?;
    if old.len() != 1 || old[0].submission.opportunity_id != opportunity_id {
        return Err("post-persistence crash did not claim the connected signed leg".into());
    }
    let rows = before.as_array().ok_or("missing persisted wire snapshot")?;
    if rows.len() != leg_index + 1
        || rows[leg_index]["state"] != "signed"
        || rows[leg_index]["broadcastCount"].as_i64() != Some(0)
        || rows[leg_index]["id"].as_i64() != Some(old[0].submission.id)
    {
        return Err(
            "post-persistence boundary contains duplicate or already-broadcast work".into(),
        );
    }
    if !runtime
        .client
        .lease_pending_signed_route_submissions(
            cluster,
            "connected-early-confirmer",
            1,
            Utc::now() + ChronoDuration::seconds(2),
        )
        .await?
        .is_empty()
    {
        return Err("live signed-submission lease admitted another owner".into());
    }
    tokio::time::sleep(Duration::from_millis(2100)).await;
    let restarted = restarted_client().await?;
    let next = restarted
        .lease_pending_signed_route_submissions(
            cluster,
            "connected-restarted-before-broadcast",
            1,
            Utc::now() + ChronoDuration::seconds(60),
        )
        .await?;
    if next.len() != 1
        || next[0].submission.id != old[0].submission.id
        || next[0].owner == old[0].owner
        || next[0].fencing_token <= old[0].fencing_token
        || next[0].submission.signed_transaction != old[0].submission.signed_transaction
        || next[0].submission.transaction_signature != old[0].submission.transaction_signature
    {
        return Err(
            "signed restart changed persisted bytes or failed to fence the old owner".into(),
        );
    }
    let now = Utc::now();
    match restarted
        .defer_signed_route_submission_lease_batch(&old, now, now, "connected stale owner probe")
        .await
    {
        Err(error)
            if error
                .to_string()
                .contains("defer batch contains a stale, expired, or divergent fence") => {}
        other => {
            return Err(
                format!("stale confirmer defer did not fail on its fence: {other:?}").into(),
            );
        }
    }
    if restarted
        .defer_signed_route_submission_lease_batch(
            &next,
            now,
            now,
            "connected recovered pre-broadcast claim",
        )
        .await?
        != 1
    {
        return Err("recovered confirmer could not release its own lease".into());
    }
    if restarted
        .claim_cross_mint_continuation(cluster, "connected-post-persistence-continuation", 10)
        .await?
        .is_some()
    {
        return Err("signed restart admitted a duplicate movement continuation".into());
    }
    if restarted
        .lease_next_rebalance_opportunity(
            cluster,
            "connected-post-persistence-duplicate-executor",
            RebalanceOpportunityClaimKind::Execute,
            Utc::now() + ChronoDuration::seconds(60),
        )
        .await?
        .is_some()
    {
        return Err("signed restart admitted duplicate executable work".into());
    }
    if wires(&restarted, opportunity_id).await? != before {
        return Err("signed persistence recovery duplicated or modified durable work".into());
    }
    unchanged_chain(&chain_before, &chain_evidence(runtime)?)?;
    eprintln!(
        "connected recovery: leg={leg_index}; post-persistence-owner-fenced=true; durable-wire-unchanged=true; no-broadcast=true"
    );
    Ok(())
}

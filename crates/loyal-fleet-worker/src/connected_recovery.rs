//! Disposable connected-fixture crash boundaries. No queue rows, signed wires,
//! receipts, or custody state are manufactured here.
use super::*;

async fn restarted_client() -> Result<NeonSqlClient, Box<dyn Error>> {
    Ok(NeonSqlClient::connect(NeonSqlConfig::new(env::var("FLEET_TEST_DATABASE_URL")?)).await?)
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
    let old = runtime
        .client
        .claim_cross_mint_continuation(&options.cluster, "connected-crashed-before-persistence", 10)
        .await?
        .ok_or("between-leg continuation was not claimable")?;
    if old.movement.opportunity_id != opportunity_id {
        return Err("pre-persistence recovery claimed substituted work".into());
    }
    let expected_phase = match completed_legs {
        1 => CrossMintCustodyPhase::SourceIdle,
        2 => CrossMintCustodyPhase::TargetIdle,
        _ => return Err("unsupported between-leg crash boundary".into()),
    };
    if old.movement.phase != expected_phase {
        return Err("between-leg crash has unexpected custody phase".into());
    }
    let prepared = prepare_next_leg(runtime, config, &old).await?;
    let generation =
        next_leg_generation(&runtime.client, old.movement.decision_id, prepared.leg.leg).await?;
    // Use the retained signed-input builder. This payload is never broadcast;
    // it exists solely to attempt a real fenced append after owner takeover.
    let stale_input = CrossMintLegPublicationInput {
        leg: prepared.leg.leg,
        purpose: prepared.leg.purpose,
        generation,
        policy_account: prepared.leg.policy_account.clone(),
        expected_effect: prepared.leg.expected_effect.clone(),
        expected_balance_anchors: prepared.leg.expected_balance_anchors.clone(),
        submission: signed_submission_input(&old, prepared.leg, generation)?,
    };
    if runtime
        .client
        .claim_cross_mint_continuation(&options.cluster, "connected-early-continuation", 10)
        .await?
        .is_some()
    {
        return Err("live continuation lease admitted another owner".into());
    }
    let wait = (old.expires_at - Utc::now()).to_std().unwrap_or_default();
    tokio::time::sleep(wait + Duration::from_millis(100)).await;
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
    options: &FleetWorkerOptions,
    opportunity_id: i64,
    leg_index: usize,
) -> Result<(), Box<dyn Error>> {
    let before = wires(&runtime.client, opportunity_id).await?;
    let chain_before = chain_evidence(runtime)?;
    let old = runtime
        .client
        .lease_pending_signed_route_submissions(
            &options.cluster,
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
            &options.cluster,
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
            &options.cluster,
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
        .claim_cross_mint_continuation(
            &options.cluster,
            "connected-post-persistence-continuation",
            10,
        )
        .await?
        .is_some()
    {
        return Err("signed restart admitted a duplicate movement continuation".into());
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

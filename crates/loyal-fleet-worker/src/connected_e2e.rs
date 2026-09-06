//! Connected Go -> retained worker boundary. Invoked only by the disposable
//! local verifier while its SVM RPC server is alive. Never seed queue work here.
use super::*;
#[path = "connected_recovery.rs"]
mod connected_recovery;
#[path = "connected_terminal.rs"]
mod connected_terminal;
use loyal_yield_orchestrator::{
    LookupTableManifestWrite, SharedMarketCatalogPlanPolicy, SharedMarketCatalogUpsert,
};

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "requires the running disposable Go/SVM fixture"]
async fn consume_go_cross_mint_opportunity() {
    run_connected_cross_mint()
        .await
        .expect("connected retained worker lifecycle");
}

// Derive fixture catalog roles from the retained action SDK and actual local
// account state, rather than guessing roles from Go's flat ALT address list.
async fn connected_catalog_requirements(
    runtime: &SameMintRouteRuntime,
    opportunity: &RebalanceOpportunityRecord,
    bindings: &CrossMintPolicyBindings,
) -> Result<
    (
        Vec<LookupTableManifestAddressRecord>,
        Vec<LookupTableManifestAddressRecord>,
    ),
    Box<dyn Error>,
> {
    let vault = load_active_vault(
        &runtime.pool,
        &bindings.settings,
        i16::from(bindings.vault_index),
    )
    .await?
    .ok_or("fixture vault missing")?;
    if vault.id != opportunity.vault_id {
        return Err("catalog fixture vault differs from Go work".into());
    }
    let rpc = &runtime.rpc;
    let source = required_plan_string(&opportunity.execution_plan, "source_reserve")?;
    let target = required_plan_string(&opportunity.execution_plan, "target_reserve")?;
    let preview = load_chain_reconcile_preview_from_rpc(
        rpc,
        &vault,
        &[source.to_owned(), target.to_owned()],
        None,
    )?;
    let source_position = chain_position_for_reserve(&preview, &source)?;
    let target_position = chain_position_for_reserve(&preview, &target)?;
    let vault_pubkey = Pubkey::from_str(&vault.vault_pubkey)?;
    let signer = policy_keypair_from_env()?;
    let mut instructions = Vec::new();
    let mut manifests = Vec::new();
    for (position, binding, withdraw) in [
        (source_position, &bindings.withdraw, true),
        (target_position, &bindings.deposit, false),
    ] {
        let mint = Pubkey::from_str(&position.liquidity_mint)?;
        let ata = derive_associated_token_address(
            &vault_pubkey,
            &mint,
            &canonical_earn_token_program(mint)?,
        );
        let routed = if withdraw {
            kamino_withdraw_instruction(
                vault_pubkey,
                position,
                ata,
                u64::try_from(required_plan_i64(
                    &opportunity.execution_plan,
                    "source_collateral_amount_raw",
                )?)?,
            )?
        } else {
            kamino_deposit_to_obligation_instruction(vault_pubkey, position, ata, 1)?
        };
        let (outer, _, _, _) = build_program_interaction_policy_execution_instruction(
            Pubkey::from_str(&binding.policy_account)?,
            signer.pubkey(),
            u8::try_from(vault.vault_index)?,
            routed,
            binding.constraint_index,
        )?;
        let mut requirements = outer.lookup_table_requirements().clone();
        instructions.clear();
        for refresh_position in
            obligation_refresh_positions_for_route(&preview, position, position)?
        {
            let refresh = kamino_refresh_reserve_instruction(refresh_position)?;
            requirements.merge(refresh.lookup_table_requirements())?;
            instructions.push(refresh.instruction().clone());
        }
        if position.obligation_exists {
            let refresh = kamino_refresh_obligation_instruction(position)?;
            requirements.merge(refresh.lookup_table_requirements())?;
            instructions.push(refresh.instruction().clone());
        }
        instructions.push(outer.instruction().clone());
        manifests.push(route_lookup_table_manifest(
            signer.pubkey(),
            &instructions,
            &vault,
            &requirements,
            &[ata],
        )?);
    }
    let mut addresses = BTreeMap::<String, LookupTableManifestAddressRecord>::new();
    let mut vault_addresses = BTreeMap::<String, LookupTableManifestAddressRecord>::new();
    for manifest in manifests {
        for row in vault_manifest_addresses(&manifest) {
            vault_addresses.entry(row.address.clone()).or_insert(row);
        }
        for row in shared_market_manifest_addresses(&manifest) {
            if let Some(existing) = addresses.get_mut(&row.address) {
                let roles = existing
                    .account_role
                    .split(',')
                    .chain(row.account_role.split(','))
                    .collect::<BTreeSet<_>>()
                    .into_iter()
                    .collect::<Vec<_>>()
                    .join(",");
                existing.account_role = roles;
                existing.is_writable |= row.is_writable;
            } else {
                addresses.insert(row.address.clone(), row);
            }
        }
    }
    Ok((
        addresses
            .into_values()
            .enumerate()
            .map(|(ordinal, mut row)| {
                row.ordinal = ordinal as i32;
                row
            })
            .collect(),
        vault_addresses
            .into_values()
            .enumerate()
            .map(|(ordinal, mut row)| {
                row.ordinal = ordinal as i32;
                row
            })
            .collect(),
    ))
}

async fn run_connected_cross_mint() -> Result<(), Box<dyn Error>> {
    let path = env::var("KAMINO_CONNECTED_REQUEST_PATH")?;
    let request: Value = serde_json::from_slice(&std::fs::read(path)?)?;
    let database = env::var("FLEET_TEST_DATABASE_URL")?;
    let connection: sqlx::postgres::PgConnectOptions = database.parse()?;
    if connection.get_host() != "127.0.0.1"
        || !matches!(connection.get_database(), Some("fleet" | "fleet_same_mint"))
    {
        return Err("connected worker requires disposable loopback /fleet database".into());
    }
    let cluster = request["cluster"].as_str().ok_or("missing cluster")?;
    if cluster != "localnet" {
        return Err("unexpected fixture cluster".into());
    }
    let rpc_url = request["rpcUrl"].as_str().ok_or("missing local RPC")?;
    let url = reqwest::Url::parse(rpc_url)?;
    if url.scheme() != "http" || url.host_str() != Some("127.0.0.1") {
        return Err("RPC must be loopback HTTP".into());
    }
    let opportunity_id = request["opportunityId"]
        .as_i64()
        .ok_or("missing Go opportunity id")?;
    let epoch_id = request["epochId"].as_i64().ok_or("missing Go epoch id")?;
    let client = NeonSqlClient::connect(NeonSqlConfig::new(database)).await?;
    let epoch_json: Value =
        sqlx::query_scalar("SELECT market_state FROM loyal_yield.optimizer_epochs WHERE id=$1")
            .bind(epoch_id)
            .fetch_one(client.pool())
            .await?;
    let epoch: ImmutableMarketEpoch = serde_json::from_value(epoch_json)?;
    let mut mints = enabled_stable_mints_from_env()?;
    mints.sort();
    mints.dedup();
    let mut market_cache = BTreeMap::new();
    // Consume the Go-persisted immutable snapshot, not substituted economics.
    market_cache.insert(
        format!("{cluster}:{}", mints.join(",")),
        CachedMarketEpoch {
            epoch,
            fetched_at: Instant::now(),
        },
    );
    let runtime = SameMintRouteRuntime {
        rpc: Arc::new(RpcClient::new_with_commitment(
            rpc_url.to_owned(),
            CommitmentConfig::confirmed(),
        )),
        pool: client.pool().clone(),
        client: client.clone(),
        timescale: None,
        rpc_cache: Arc::new(SameMintRouteRpcCache::default()),
        market_epoch_cache: Arc::new(AsyncMutex::new(market_cache)),
    };
    if request["setupOnly"].as_bool() == Some(true) {
        let opportunity = client
            .rebalance_opportunity(opportunity_id)
            .await?
            .ok_or("missing Go work during catalog setup")?;
        if opportunity.execution_plan != request["executionPlan"] {
            return Err("catalog setup received a substituted Go plan".into());
        }
        setup_connected_catalog(
            &runtime,
            &opportunity,
            &mints,
            Some(&request["fixturePolicyBindings"]),
        )
        .await?;
        return Ok(());
    }
    if request["sameMint"].as_bool() == Some(true) {
        let opportunity = client
            .rebalance_opportunity(opportunity_id)
            .await?
            .ok_or("missing Go same-mint work")?;
        if opportunity.execution_plan != request["executionPlan"]
            || opportunity.optimizer_epoch_id != epoch_id
        {
            return Err("same-mint handoff substituted Go work".into());
        }
        let lease = if opportunity.state == RebalanceOpportunityState::Leased
            && opportunity.lease_kind == Some(RebalanceOpportunityClaimKind::Execute)
        {
            // Go atomically transfers its prepared revalidation into this
            // execute lease. Consume that fence without rewriting the plan.
            RebalanceOpportunityLease {
                owner: opportunity
                    .lease_owner
                    .clone()
                    .ok_or("missing execute owner")?,
                fencing_token: opportunity.fencing_token,
                expires_at: opportunity
                    .lease_expires_at
                    .ok_or("missing execute expiry")?,
                claim_kind: RebalanceOpportunityClaimKind::Execute,
                opportunity,
            }
        } else {
            client
                .lease_next_rebalance_opportunity(
                    cluster,
                    "connected-rust-same-mint",
                    RebalanceOpportunityClaimKind::Execute,
                    Utc::now() + ChronoDuration::seconds(60),
                )
                .await?
                .ok_or("same-mint work not claimable")?
        };
        if lease.opportunity.id != opportunity_id {
            return Err("same-mint executor claimed another opportunity".into());
        }
        let execution_request = same_mint_request_from_opportunity(
            &lease,
            rpc_url,
            RebalanceOpportunityClaimKind::Execute,
        )?;
        let result = execute_same_mint_route_with_runtime(execution_request, &runtime, None).await;
        eprintln!(
            "connected same-mint execution state: {:?}, reason: {:?}",
            result.state, result.reason
        );
        if result.state != SameMintRouteExecutionState::SubmissionQueued {
            return Err("same-mint signed persistence failed".into());
        }
        for _ in 0..2 {
            let output = std::process::Command::new(env::var("KAMINO_CONNECTED_CONFIRMER_PATH")?)
                .args([
                    "--execute",
                    "--once",
                    "--cluster",
                    cluster,
                    "--rpc-url",
                    rpc_url,
                    "--ws-url",
                    "ws://127.0.0.1:9",
                    "--worker-id",
                    "connected-same-confirmer",
                ])
                .env_clear()
                .env("NEON_DATABASE_URL", env::var("FLEET_TEST_DATABASE_URL")?)
                .env("OBSERVABILITY_ENABLED", "false")
                .env("NO_PROXY", "127.0.0.1,localhost")
                .env("HTTP_PROXY", "http://127.0.0.1:9")
                .env("HTTPS_PROXY", "http://127.0.0.1:9")
                .output()?;
            eprintln!(
                "connected same-mint confirmer: {} {}",
                String::from_utf8_lossy(&output.stdout),
                String::from_utf8_lossy(&output.stderr)
            );
            if !output.status.success() {
                return Err("same-mint confirmer failed".into());
            }
            tokio::time::sleep(Duration::from_secs(2)).await;
        }
        let old = client
            .lease_reconciliation_pending_signed_route_submissions(
                cluster,
                "connected-same-crashed-reconciler",
                1,
                Utc::now() + ChronoDuration::milliseconds(250),
            )
            .await?;
        if old.len() != 1 {
            return Err("same-mint lost confirmed submission".into());
        }
        tokio::time::sleep(Duration::from_millis(300)).await;
        let restarted =
            NeonSqlClient::connect(NeonSqlConfig::new(env::var("FLEET_TEST_DATABASE_URL")?))
                .await?;
        let reconciliations = restarted
            .lease_reconciliation_pending_signed_route_submissions(
                cluster,
                "connected-same-reconciler",
                1,
                Utc::now() + ChronoDuration::seconds(60),
            )
            .await?;
        if reconciliations.len() != 1
            || reconciliations[0].submission.id != old[0].submission.id
            || reconciliations[0].fencing_token <= old[0].fencing_token
        {
            return Err("same-mint reconciliation takeover invalid".into());
        }
        let before_stale: Value = sqlx::query_scalar(
            "SELECT to_jsonb(d) FROM loyal_yield.rebalance_decisions d WHERE id=$1",
        )
        .bind(old[0].submission.decision_id.unwrap().as_i64())
        .fetch_one(client.pool())
        .await?;
        if reconcile_same_mint_submission_effect(&runtime, &old[0])
            .await
            .is_ok()
        {
            return Err("same-mint stale reconciler committed".into());
        }
        // Exercise the durable transaction guard too, independently of the
        // worker's early lease check. Stale calls must not confirm a decision.
        let stale_commit = client
            .advance_decision_guarded(
                old[0].submission.decision_id.unwrap(),
                DecisionAdvance::Confirm {
                    slot: Some(1000),
                    post_snapshot_id: None,
                },
                Some(&old[0]),
            )
            .await;
        if !matches!(stale_commit,Err(ref error) if error.to_string().contains("expired or fenced"))
        {
            return Err(
                format!("stale durable confirmation was not fenced: {stale_commit:?}").into(),
            );
        }
        let after_stale: Value = sqlx::query_scalar(
            "SELECT to_jsonb(d) FROM loyal_yield.rebalance_decisions d WHERE id=$1",
        )
        .bind(old[0].submission.decision_id.unwrap().as_i64())
        .fetch_one(client.pool())
        .await?;
        if before_stale != after_stale {
            return Err("stale reconciler changed durable decision".into());
        }
        eprintln!(
            "connected same-mint stale worker and durable transaction rejected; decision unchanged"
        );
        if reconciliations.len() != 1
            || reconciliations[0].submission.opportunity_id != opportunity_id
        {
            return Err("same-mint confirmation missing exact submission".into());
        }
        let reconciled_slot =
            reconcile_same_mint_submission_effect(&runtime, &reconciliations[0]).await?;
        client
            .advance_signed_route_submission(
                &reconciliations[0],
                SignedRouteSubmissionAdvance::Reconciled { reconciled_slot },
            )
            .await?;
        eprintln!(
            "connected same-mint reconciled: submission={}, signature={}, slot={reconciled_slot}",
            reconciliations[0].submission.id, reconciliations[0].submission.transaction_signature
        );
        let before: Value = runtime.rpc.send(
            solana_client::rpc_request::RpcRequest::Custom {
                method: "executionEvidence",
            },
            json!([]),
        )?;
        let signature:String=runtime.rpc.send(solana_client::rpc_request::RpcRequest::SendTransaction,json!([BASE64_STANDARD.encode(&reconciliations[0].submission.signed_transaction),{"encoding":"base64"}]))?;
        let after: Value = runtime.rpc.send(
            solana_client::rpc_request::RpcRequest::Custom {
                method: "executionEvidence",
            },
            json!([]),
        )?;
        if signature != reconciliations[0].submission.transaction_signature
            || before["transactions"] != after["transactions"]
            || before["accounts"] != after["accounts"]
            || before["receipts"] != after["receipts"]
        {
            return Err("same-mint persisted replay duplicated effects".into());
        }
        connected_terminal::verify(&runtime, &lease.opportunity, true).await?;
        return Err("connected same-mint recovery evidence not yet complete".into());
    }
    let lease = client
        .lease_next_rebalance_opportunity(
            cluster,
            "connected-rust-executor",
            RebalanceOpportunityClaimKind::Execute,
            Utc::now() + ChronoDuration::seconds(60),
        )
        .await?
        .ok_or("Go opportunity was not claimable by retained executor")?;
    if lease.opportunity.id != opportunity_id
        || lease.opportunity.optimizer_epoch_id != epoch_id
        || lease.opportunity.execution_plan != request["executionPlan"]
    {
        return Err("Rust claimed substituted work or a changed Go plan".into());
    }
    let count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM loyal_yield.rebalance_opportunities WHERE cluster=$1",
    )
    .bind(cluster)
    .fetch_one(client.pool())
    .await?;
    if count != 1 {
        return Err("connected fixture contains extra opportunities".into());
    }
    // Initial service controls are fixture input, never a queue/state shortcut.
    sqlx::query("INSERT INTO loyal_yield.cross_mint_movement_controls(cluster,start_new_movements,continue_or_recover_existing,generation,updated_by) VALUES($1,true,true,1,'connected-local-verifier')")
        .bind(cluster).execute(client.pool()).await?;
    let options = FleetWorkerOptions {
        claim_kind: RebalanceOpportunityClaimKind::Execute,
        cluster: cluster.to_owned(),
        rpc_url: rpc_url.to_owned(),
        owner: lease.owner.clone(),
        concurrency: 1,
        fused_execute_concurrency: 0,
        lease_seconds: 60,
        poll_interval_milliseconds: 100,
        route_kind: Some("cross_mint_jupiter".to_owned()),
        once: true,
    };
    let config = CrossMintWorkerConfig {
        enabled: true,
        build_url: format!("{rpc_url}/build"),
        api_key: None,
        maximum_slippage_bps: 50,
        maximum_value_loss_bps: 50,
    };
    setup_connected_catalog(&runtime, &lease.opportunity, &mints, None).await?;
    let result = activate_cross_mint_opportunity(&runtime, &options, &config, &lease).await?;
    run_connected_legs(
        runtime,
        client,
        options,
        config,
        lease,
        result,
        mints,
        epoch_id,
        opportunity_id,
    )
    .await
}

async fn setup_connected_catalog(
    runtime: &SameMintRouteRuntime,
    opportunity: &RebalanceOpportunityRecord,
    mints: &[String],
    fixture_bindings: Option<&Value>,
) -> Result<(), Box<dyn Error>> {
    let client = &runtime.client;
    let cluster = opportunity.cluster.as_str();
    let bindings = if let Some(bindings) = fixture_bindings {
        cross_mint_policy_bindings(&json!({"policy_bindings":bindings}))?
    } else {
        cross_mint_policy_bindings(&opportunity.execution_plan)?
    };
    let (catalog, vault_addresses) =
        connected_catalog_requirements(runtime, opportunity, &bindings).await?;
    let head = client
        .upsert_shared_market_catalog(SharedMarketCatalogUpsert {
            cluster: cluster.to_owned(),
            catalog_version: "test".to_owned(),
            desired_set_hash: lookup_table_manifest_address_records_hash(&catalog),
            enabled_mints_hash: stable_fingerprint_owned(&mints),
            reserve_set_hash: stable_fingerprint_owned(
                &catalog
                    .iter()
                    .filter(|r| r.account_role == "reserve")
                    .map(|r| r.address.clone())
                    .collect::<Vec<_>>(),
            ),
            addresses: catalog,
            source_slot: Some(1000),
            source_observed_at: Some(Utc::now()),
            source_metadata: json!({"fixture":"local-sdk-derived"}),
            reason: "local execution fixture".to_owned(),
            updated_by: "connected-local-verifier".to_owned(),
        })
        .await?;
    let head = client
        .reconcile_shared_market_catalog_head(
            cluster,
            head.catalog_revision_id,
            SharedMarketCatalogPlanPolicy {
                shared_shard_capacity: 254,
                max_extension_addresses: 20,
                operation_context: json!({"fixture":"local"}),
                estimated_fee_lamports: None,
                estimated_rent_lamports: None,
            },
            Utc::now() + ChronoDuration::minutes(5),
        )
        .await?;
    if head.readiness_state != SharedMarketCatalogReadiness::Active {
        return Err(format!("local catalog not active: {head:?}").into());
    }
    let (family_id, table_id): (i64,i64) = sqlx::query_as("SELECT family.id,route_table.id FROM loyal_yield.lookup_table_families family JOIN loyal_yield.route_lookup_tables route_table ON route_table.family_id=family.id WHERE family.cluster=$1 AND family.kind='vault_shards'").bind(cluster).fetch_one(client.pool()).await?;
    let vault = load_active_vault(
        &runtime.pool,
        &bindings.settings,
        i16::from(bindings.vault_index),
    )
    .await?
    .ok_or("fixture vault missing")?;
    if vault.id != opportunity.vault_id {
        return Err("catalog fixture vault differs from Go work".into());
    }
    let manifest = client
        .persist_lookup_table_manifest(LookupTableManifestWrite {
            family_id,
            subject_kind: LookupTableManifestSubject::Vault,
            subject_key: vault.vault_pubkey.clone(),
            vault_id: Some(vault.id),
            desired_set_hash: lookup_table_manifest_address_records_hash(&vault_addresses),
            source_slot: Some(1000),
            planner_version: "test".to_owned(),
            catalog_version: "test".to_owned(),
            addresses: vault_addresses,
        })
        .await?;
    sqlx::query("INSERT INTO loyal_yield.lookup_table_vault_bindings(vault_id,family_id,route_lookup_table_id,manifest_id,desired_head_revision,allocation_mode,reserved_capacity,lifecycle_state,active_from_slot,activated_at) VALUES($1,$2,$3,$4,1,'packed_shard',32,'active',1000,clock_timestamp())").bind(vault.id.as_i64()).bind(family_id).bind(table_id).bind(manifest.id).execute(client.pool()).await?;
    eprintln!(
        "connected fixture catalog activated: revision={}",
        head.catalog_revision_id
    );
    Ok(())
}

async fn run_connected_legs(
    runtime: SameMintRouteRuntime,
    client: NeonSqlClient,
    options: FleetWorkerOptions,
    config: CrossMintWorkerConfig,
    lease: RebalanceOpportunityLease,
    result: CrossMintWorkResult,
    mints: Vec<String>,
    epoch_id: i64,
    opportunity_id: i64,
) -> Result<(), Box<dyn Error>> {
    let cluster = options.cluster.as_str();
    let rpc_url = options.rpc_url.as_str();
    eprintln!("connected activation: {result:?}");
    for leg_index in 0..3 {
        if leg_index > 0 {
            // Reload the exact Go-owned immutable epoch from disposable storage.
            // Preserve its observation/expiry timestamps; only cache-fetch time changes.
            let persisted: Value = sqlx::query_scalar(
                "SELECT market_state FROM loyal_yield.optimizer_epochs WHERE id=$1",
            )
            .bind(epoch_id)
            .fetch_one(client.pool())
            .await?;
            runtime.market_epoch_cache.lock().await.insert(
                format!("{cluster}:{}", mints.join(",")),
                CachedMarketEpoch {
                    epoch: serde_json::from_value(persisted)?,
                    fetched_at: Instant::now(),
                },
            );
            let continued = connected_recovery::continue_after_pre_persistence_crash(
                &runtime,
                &options,
                &config,
                opportunity_id,
                leg_index,
            )
            .await?;
            eprintln!("connected continuation {leg_index}: {continued:?}");
            if !matches!(continued, CrossMintWorkResult::Continued { .. }) {
                let evidence: Value = sqlx::query_scalar("SELECT to_jsonb(m) FROM loyal_yield.rebalance_decisions m WHERE id IN (SELECT decision_id FROM loyal_yield.rebalance_opportunities WHERE cluster=$1)").bind(cluster).fetch_one(client.pool()).await?;
                eprintln!("connected stopped movement: {evidence}");
                return Err("connected movement did not publish its next leg".into());
            }
        }
        if !matches!(
            process_continuation_before_new_work(&runtime, &options, &config).await?,
            CrossMintWorkResult::NoWork
        ) {
            return Err("in-flight signed leg admitted duplicate continuation".into());
        }
        connected_recovery::recover_persisted_before_broadcast(
            &runtime,
            &options,
            opportunity_id,
            leg_index,
        )
        .await?;
        // The unmodified retained CLI broadcasts only persisted bytes; it receives
        // no signer and no inherited production endpoint or credential.
        for _ in 0..2 {
            let output = std::process::Command::new(env::var("KAMINO_CONNECTED_CONFIRMER_PATH")?)
                .args([
                    "--execute",
                    "--once",
                    "--cluster",
                    cluster,
                    "--rpc-url",
                    rpc_url,
                    "--ws-url",
                    "ws://127.0.0.1:9",
                    "--worker-id",
                    "connected-confirmer",
                ])
                .env_clear()
                .env("NEON_DATABASE_URL", env::var("FLEET_TEST_DATABASE_URL")?)
                .env("OBSERVABILITY_ENABLED", "false")
                .env("NO_PROXY", "127.0.0.1,localhost")
                .env("HTTP_PROXY", "http://127.0.0.1:9")
                .env("HTTPS_PROXY", "http://127.0.0.1:9")
                .output()?;
            eprintln!(
                "connected confirmer: {} {}",
                String::from_utf8_lossy(&output.stdout),
                String::from_utf8_lossy(&output.stderr)
            );
            if !output.status.success() {
                return Err("retained confirmer failed".into());
            }
            tokio::time::sleep(Duration::from_secs(2)).await;
        }
        let old_leases = client
            .lease_reconciliation_pending_signed_route_submissions(
                cluster,
                "connected-crashed-reconciler",
                1,
                Utc::now() + ChronoDuration::milliseconds(250),
            )
            .await?;
        if old_leases.len() != 1 {
            return Err("signed leg did not reach retained reconciliation".into());
        }
        // A lost worker's real lease expires; another owner resumes from durable
        // confirmation. Do not rewrite submission states or manufacture receipts.
        tokio::time::sleep(Duration::from_millis(300)).await;
        let restarted =
            NeonSqlClient::connect(NeonSqlConfig::new(env::var("FLEET_TEST_DATABASE_URL")?))
                .await?;
        let reconciliations = restarted
            .lease_reconciliation_pending_signed_route_submissions(
                cluster,
                "connected-restarted-reconciler",
                1,
                Utc::now() + ChronoDuration::seconds(60),
            )
            .await?;
        if reconciliations.len() != 1
            || reconciliations[0].submission.id != old_leases[0].submission.id
        {
            return Err("restart substituted reconciliation work".into());
        }
        if reconcile_finalized_submission(&runtime, &old_leases[0])
            .await
            .is_ok()
        {
            return Err("stale reconciler committed after takeover".into());
        }
        let reconciled_slot = reconcile_finalized_submission(&runtime, &reconciliations[0]).await?;
        let before: Value = runtime.rpc.send(
            solana_client::rpc_request::RpcRequest::Custom {
                method: "executionEvidence",
            },
            json!([]),
        )?;
        let replayed: String = runtime.rpc.send(solana_client::rpc_request::RpcRequest::SendTransaction,json!([BASE64_STANDARD.encode(&reconciliations[0].submission.signed_transaction),{"encoding":"base64"}]))?;
        let after: Value = runtime.rpc.send(
            solana_client::rpc_request::RpcRequest::Custom {
                method: "executionEvidence",
            },
            json!([]),
        )?;
        if replayed != reconciliations[0].submission.transaction_signature
            || before["transactions"] != after["transactions"]
            || before["receipts"] != after["receipts"]
            || before["accounts"] != after["accounts"]
        {
            return Err("persisted-wire replay duplicated chain effects".into());
        }
        eprintln!(
            "connected leg reconciled: submission={}, signature={}, slot={reconciled_slot}; expired-owner-rejected=true; exact-wire-replay-no-effect=true",
            reconciliations[0].submission.id, reconciliations[0].submission.transaction_signature
        );
    }
    let advanced: u64 = runtime.rpc.send(
        solana_client::rpc_request::RpcRequest::Custom {
            method: "advanceSlot",
        },
        json!([1001]),
    )?;
    if advanced != 1001 {
        return Err("local telemetry slot did not advance".into());
    }
    let vault = movement_vault(&runtime, &lease.opportunity).await?;
    let preview = load_chain_reconcile_preview_from_rpc(
        &runtime.rpc,
        &vault,
        &[lease.opportunity.target_reserve.clone()],
        Some(1001),
    )?;
    let target = chain_position_for_reserve(&preview, &lease.opportunity.target_reserve)?;
    let supply = runtime
        .rpc
        .get_token_supply(&Pubkey::from_str(&target.collateral_mint)?)?;
    if supply.decimals != 6 {
        return Err("local one-to-one collateral valuation expects six decimals".into());
    }
    // The mock's exchange rate is exactly one. Supply is read after actual
    // SPL minting, never calculated from a planned amount or simulated receipt.
    let projection = client
        .observe_target_capacity(TargetCapacityObservation {
            cluster: cluster.to_owned(),
            target_reserve: lease.opportunity.target_reserve.clone(),
            liquidity_mint: lease.opportunity.target_liquidity_mint.clone(),
            observed_supply_usd_micros: supply.amount.parse()?,
            observed_slot: 1001,
            maximum_inflight_usd_micros: 100_000_000_000,
        })
        .await?;
    eprintln!("connected balance-derived capacity telemetry: {projection:?}");
    let terminal: Value = sqlx::query_scalar("SELECT jsonb_build_object('opportunity',to_jsonb(o),'decision',to_jsonb(d),'submissions',(SELECT jsonb_agg(jsonb_build_object('id',s.id,'state',s.submission_state,'signature',s.transaction_signature,'reconciledSlot',s.reconciled_slot,'effect',s.reconciled_effect)) FROM loyal_yield.signed_route_submissions s WHERE s.opportunity_id=o.id),'activeCapacity',(SELECT count(*) FROM loyal_yield.target_capacity_reservations r WHERE r.decision_id=d.id AND r.reservation_state<>'released'),'conflicts',(SELECT count(*) FROM loyal_yield.route_account_conflict_leases c WHERE c.opportunity_id=o.id)) FROM loyal_yield.rebalance_opportunities o JOIN loyal_yield.rebalance_decisions d ON d.id=o.decision_id WHERE o.id=$1").bind(opportunity_id).fetch_one(client.pool()).await?;
    eprintln!("connected terminal evidence: {terminal}");
    if terminal["opportunity"]["opportunity_state"] != "completed"
        || terminal["decision"]["status"] != "confirmed"
        || terminal["decision"]["terminal_outcome"] != "completed_target"
        || terminal["decision"]["custody_amount_raw"].as_i64() != Some(0)
        || terminal["activeCapacity"].as_i64() != Some(0)
        || terminal["conflicts"].as_i64() != Some(0)
    {
        return Err("cross-mint terminal state or resource release failed".into());
    }
    let submissions = terminal["submissions"]
        .as_array()
        .ok_or("missing submission evidence")?;
    if submissions.len() != 3
        || submissions
            .iter()
            .any(|s| s["state"] != "reconciled" || s["reconciledSlot"].as_i64() != Some(1000))
    {
        return Err("cross-mint signed lifecycle evidence incomplete".into());
    }
    connected_terminal::verify(&runtime, &lease.opportunity, false).await?;
    // This is deliberately fail-closed until execution, confirmation, exact
    // reconciliation and recovery assertions below are implemented and run.
    Err("connected lifecycle has not yet verified terminal reconciliation".into())
}

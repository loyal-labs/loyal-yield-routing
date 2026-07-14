//! Signerless semantic monitoring for reusable Address Lookup Tables.
//!
//! This process reads control-plane state and finalized RPC state. It never
//! creates route demand, mutates an ALT, or reads a keypair environment value.

use std::{env, error::Error, process::ExitCode, str::FromStr, time::Duration};

use chrono::Utc;
use loyal_yield_orchestrator::{
    apply_lookup_table_alert_rule, complete_lookup_table_alert_delivery,
    complete_lookup_table_render_failure_delivery, enqueue_lookup_table_test_alerts,
    evaluate_lookup_table_alerts, fail_lookup_table_alert_delivery,
    lease_lookup_table_alert_deliveries, lease_lookup_table_alert_deliveries_by_ids,
    load_lookup_table_alert_rules, load_lookup_table_alert_snapshot,
    record_lookup_table_alert_observation,
    rpc_safety::{redacted_external_error, validate_rpc_endpoint, validate_rpc_genesis_hash},
    LeasedLookupTableAlertDelivery, LookupTableAlertSnapshot, LookupTableAlertThresholds,
    LookupTableRpcAudit, NeonSqlClient, NeonSqlConfig,
};
use reqwest::{Client as HttpClient, StatusCode};
use serde::Serialize;
use serde_json::{json, Value};
use solana_client::rpc_client::RpcClient;
use solana_sdk::{
    address_lookup_table::{program as alt_program, state::AddressLookupTable},
    commitment_config::CommitmentConfig,
    pubkey::Pubkey,
};

const DATABASE_URL_ENV: &str = "NEON_DATABASE_URL";
const RPC_URL_ENV: &str = "SOLANA_RPC_URL";
const CLUSTER_ENV: &str = "YIELD_ALT_CLUSTER";
const POLICY_PUBKEY_ENV: &str = "YIELD_ALT_POLICY_PUBKEY";
const WEBHOOK_URL_ENV: &str = "YIELD_ALT_ALERT_WEBHOOK_URL";
const WEBHOOK_BEARER_ENV: &str = "YIELD_ALT_ALERT_WEBHOOK_BEARER_TOKEN";
const PRODUCTION_ENV: &str = "YIELD_ALT_ALERT_PRODUCTION";
const FORBIDDEN_ENVIRONMENTS: [&str; 5] = [
    "POLICY_KEYPAIR",
    "YIELD_ROUTER_KEYPAIR",
    "SOLANA_TESTING_PK",
    "DEPLOYMENT_PK",
    "TIMESCALEDB_URL",
];

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum RunMode {
    Once,
    Watch,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum DeliveryMode {
    Webhook,
    RenderFailure,
    None,
}

#[derive(Debug, Clone)]
struct Options {
    database_url: String,
    rpc_url: String,
    cluster: String,
    policy_pubkey: Pubkey,
    run_mode: RunMode,
    production: bool,
    delivery_mode: DeliveryMode,
    webhook_url: Option<String>,
    webhook_bearer: Option<String>,
    test_alerts: bool,
    interval: Duration,
    reminder_interval: Duration,
    delivery_lease: Duration,
    delivery_batch_size: i64,
    delivery_max_attempts: i32,
    worker_id: String,
    thresholds: LookupTableAlertThresholds,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum MonitorOutcome {
    Success,
    RenderFailureDelivered,
}

impl MonitorOutcome {
    const fn exit_code(self) -> u8 {
        match self {
            Self::Success => 0,
            Self::RenderFailureDelivered => 1,
        }
    }
}

#[derive(Debug, Default, Serialize)]
#[serde(rename_all = "camelCase")]
struct DispatchSummary {
    delivered: usize,
    retry_wait: usize,
    dead_letter: usize,
    render_failure_deliveries: usize,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct CycleSummary {
    event: &'static str,
    cluster: String,
    policy_pubkey: String,
    finalized_slot: u64,
    active_condition_count: usize,
    transition_count: usize,
    delivery: DispatchSummary,
    signer_loaded: bool,
    lookup_table_mutations: usize,
    route_demand_mutations: usize,
}

#[tokio::main]
async fn main() -> ExitCode {
    let outcome = match run().await {
        Ok(outcome) => outcome,
        Err(error) => {
            eprintln!(
                "{}",
                json!({
                    "event": "reusable_alt_alert_monitor_fatal",
                    "error": redacted_external_error(&error.to_string()),
                    "signerLoaded": false,
                })
            );
            return ExitCode::FAILURE;
        }
    };
    ExitCode::from(outcome.exit_code())
}

async fn run() -> Result<MonitorOutcome, Box<dyn Error>> {
    let options = parse_args_with_env(
        env::args().skip(1),
        |key| env::var(key).ok(),
        |key| env::var_os(key).is_some(),
    )?;
    validate_rpc_endpoint(&options.rpc_url)
        .map_err(|error| format!("invalid reusable ALT alert RPC endpoint: {error}"))?;
    if let Some(webhook_url) = options.webhook_url.as_deref() {
        validate_rpc_endpoint(webhook_url)
            .map_err(|error| format!("invalid reusable ALT alert webhook endpoint: {error}"))?;
    }

    let rpc =
        RpcClient::new_with_commitment(options.rpc_url.clone(), CommitmentConfig::finalized());
    let genesis_hash = rpc
        .get_genesis_hash()
        .map_err(|_| "failed to read genesis hash from reusable ALT alert RPC")?;
    validate_rpc_genesis_hash(&options.cluster, genesis_hash).map_err(|error| {
        format!("refusing reusable ALT alert reads from mismatched RPC: {error}")
    })?;

    let client = NeonSqlClient::connect(NeonSqlConfig::new(options.database_url.clone())).await?;
    require_alert_schema(&client).await?;
    let http = HttpClient::builder()
        .timeout(Duration::from_secs(15))
        .build()?;

    if options.test_alerts {
        let rules = load_lookup_table_alert_rules(client.pool()).await?;
        let test_id = format!("{}-{}", options.worker_id, Utc::now().timestamp_micros());
        let delivery_ids = enqueue_lookup_table_test_alerts(
            client.pool(),
            &options.cluster,
            &options.policy_pubkey.to_string(),
            &test_id,
            &rules,
            options.delivery_max_attempts,
            Utc::now(),
        )
        .await?;
        if delivery_ids.len() != 9 {
            return Err("safe test-alert delivery did not enqueue all nine rules".into());
        }
        let delivery =
            dispatch_targeted_test_deliveries(&client, &http, &options, &delivery_ids).await?;
        println!(
            "{}",
            json!({
                "event": "reusable_alt_alert_test",
                "testId": test_id,
                "testedRuleCount": delivery_ids.len(),
                "cluster": options.cluster,
                "delivery": delivery,
                "signerLoaded": false,
                "lookupTableMutations": 0,
                "routeDemandMutations": 0,
            })
        );
        return delivery_outcome(&options, &delivery);
    }

    loop {
        let summary = run_cycle(&client, &rpc, &http, &options).await?;
        let outcome = delivery_outcome(&options, &summary.delivery)?;
        println!("{}", serde_json::to_string(&summary)?);
        if outcome == MonitorOutcome::RenderFailureDelivered {
            return Ok(outcome);
        }
        if options.run_mode == RunMode::Once {
            return Ok(MonitorOutcome::Success);
        }
        tokio::time::sleep(options.interval).await;
    }
}

async fn require_alert_schema(client: &NeonSqlClient) -> Result<(), Box<dyn Error>> {
    let ready: bool = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT EXISTS (
            SELECT 1 FROM loyal_yield.schema_migrations WHERE version = 21
        )
        AND to_regclass('loyal_yield.lookup_table_alert_rules') IS NOT NULL
        AND to_regclass('loyal_yield.lookup_table_alert_incidents') IS NOT NULL
        AND to_regclass('loyal_yield.lookup_table_alert_deliveries') IS NOT NULL
        "#,
    )
    .fetch_one(client.pool())
    .await?;
    if !ready {
        return Err("reusable ALT alert migration 21 is not applied".into());
    }
    Ok(())
}

async fn run_cycle(
    client: &NeonSqlClient,
    rpc: &RpcClient,
    http: &HttpClient,
    options: &Options,
) -> Result<CycleSummary, Box<dyn Error>> {
    let policy_pubkey = options.policy_pubkey.to_string();
    let snapshot = load_lookup_table_alert_snapshot(
        client.pool(),
        &options.cluster,
        &policy_pubkey,
        &options.thresholds,
    )
    .await?;
    let rpc_audit = audit_finalized_physical_tables(rpc, &snapshot)?;
    let rules = load_lookup_table_alert_rules(client.pool()).await?;
    let observations = evaluate_lookup_table_alerts(&snapshot, &rpc_audit, &options.thresholds)
        .into_iter()
        .zip(&rules)
        .map(|(observation, rule)| apply_lookup_table_alert_rule(observation, rule))
        .collect::<Result<Vec<_>, _>>()?;
    let active_condition_count = observations
        .iter()
        .filter(|observation| observation.active)
        .count();
    let mut transition_count = 0;
    let observed_at = Utc::now();
    for observation in &observations {
        let transition = record_lookup_table_alert_observation(
            client.pool(),
            &options.cluster,
            &policy_pubkey,
            "cluster",
            observation,
            observed_at,
            options.reminder_interval,
            options.delivery_max_attempts,
        )
        .await?;
        transition_count += usize::from(transition.event_kind.is_some());
    }
    let delivery = dispatch_deliveries(client, http, options).await?;
    Ok(CycleSummary {
        event: "reusable_alt_alert_cycle",
        cluster: options.cluster.clone(),
        policy_pubkey,
        finalized_slot: rpc_audit.finalized_slot,
        active_condition_count,
        transition_count,
        delivery,
        signer_loaded: false,
        lookup_table_mutations: 0,
        route_demand_mutations: 0,
    })
}

fn audit_finalized_physical_tables(
    rpc: &RpcClient,
    snapshot: &LookupTableAlertSnapshot,
) -> Result<LookupTableRpcAudit, Box<dyn Error>> {
    let finalized_slot = rpc.get_slot_with_commitment(CommitmentConfig::finalized())?;
    let mut audit = LookupTableRpcAudit {
        finalized_slot,
        ..LookupTableRpcAudit::default()
    };

    let mut parsed_expectations = Vec::with_capacity(snapshot.physical_expectations.len());
    for expectation in &snapshot.physical_expectations {
        let Ok(address) = Pubkey::from_str(&expectation.table_address) else {
            audit
                .authority_prefix_drift_table_ids
                .push(expectation.table_id);
            audit.evidence.push(json!({
                "tableId": expectation.table_id,
                "reason": "invalid_registered_table_address",
                "mutationEpoch": expectation.mutation_epoch,
            }));
            continue;
        };
        parsed_expectations.push((expectation, address));
    }

    for chunk in parsed_expectations.chunks(100) {
        let addresses = chunk
            .iter()
            .map(|(_, address)| *address)
            .collect::<Vec<_>>();
        let response =
            rpc.get_multiple_accounts_with_commitment(&addresses, CommitmentConfig::finalized())?;
        if response.value.len() != chunk.len() {
            return Err(
                "finalized reusable ALT audit returned an incomplete account vector".into(),
            );
        }
        audit.finalized_slot = audit.finalized_slot.min(response.context.slot);
        let observed_slot = response.context.slot;
        for ((expectation, _), account) in chunk.iter().zip(response.value) {
            if expectation.orphaned && account.is_none() {
                audit.absent_orphan_table_ids.push(expectation.table_id);
            }
            if !matches!(expectation.desired_state.as_str(), "active" | "standby") {
                continue;
            }

            let mut reason =
                (!expectation.registry_authority_matches).then_some("registry_authority_mismatch");
            let mut observed_authority = None;
            let mut observed_count = None;
            match account {
                None => reason = Some("missing_finalized_account"),
                Some(account) if account.owner != alt_program::id() => {
                    reason = Some("owner_mismatch")
                }
                Some(account) => match AddressLookupTable::deserialize(&account.data) {
                    Err(_) => reason = Some("decode_failed"),
                    Ok(table) => {
                        observed_authority =
                            table.meta.authority.map(|authority| authority.to_string());
                        observed_count = Some(table.addresses.len());
                        let observed_addresses = table
                            .addresses
                            .iter()
                            .map(ToString::to_string)
                            .collect::<Vec<_>>();
                        let authority_matches = observed_authority.as_deref()
                            == Some(expectation.expected_authority.as_str());
                        let active = table.meta.deactivation_slot == u64::MAX;
                        let warm = observed_slot > table.meta.last_extended_slot;
                        let prefix_matches = observed_addresses
                            .starts_with(expectation.expected_addresses.as_slice());
                        let exact_length =
                            observed_addresses.len() == expectation.expected_addresses.len();
                        if !authority_matches {
                            reason = Some("authority_mismatch");
                        } else if !active {
                            reason = Some("unexpected_deactivation");
                        } else if !expectation.has_inflight_operation && !warm {
                            reason = Some("not_warm_at_finalized_slot");
                        } else if !expectation.has_inflight_operation
                            && (!prefix_matches || !exact_length)
                        {
                            reason = Some("ordered_prefix_mismatch");
                        }
                    }
                },
            }
            if let Some(reason) = reason {
                audit
                    .authority_prefix_drift_table_ids
                    .push(expectation.table_id);
                audit.evidence.push(json!({
                    "tableId": expectation.table_id,
                    "reason": reason,
                    "mutationEpoch": expectation.mutation_epoch,
                    "registryAuthorityMatches": expectation.registry_authority_matches,
                    "expectedAuthority": expectation.expected_authority,
                    "observedAuthority": observed_authority,
                    "expectedAddressCount": expectation.expected_addresses.len(),
                    "observedAddressCount": observed_count,
                }));
            }
        }
    }
    audit.authority_prefix_drift_table_ids.sort_unstable();
    audit.authority_prefix_drift_table_ids.dedup();
    audit.absent_orphan_table_ids.sort_unstable();
    audit.absent_orphan_table_ids.dedup();
    Ok(audit)
}

async fn dispatch_deliveries(
    client: &NeonSqlClient,
    http: &HttpClient,
    options: &Options,
) -> Result<DispatchSummary, Box<dyn Error>> {
    match options.delivery_mode {
        DeliveryMode::Webhook => dispatch_webhooks(client, http, options).await,
        DeliveryMode::RenderFailure => dispatch_render_failures(client, options).await,
        DeliveryMode::None => Ok(DispatchSummary::default()),
    }
}

async fn dispatch_targeted_test_deliveries(
    client: &NeonSqlClient,
    http: &HttpClient,
    options: &Options,
    delivery_ids: &[i64],
) -> Result<DispatchSummary, Box<dyn Error>> {
    if options.delivery_mode == DeliveryMode::None {
        return Err("--test-alerts requires an explicit delivery channel".into());
    }
    let deliveries = lease_lookup_table_alert_deliveries_by_ids(
        client.pool(),
        &options.worker_id,
        delivery_ids,
        options.delivery_lease,
    )
    .await?;
    if deliveries.len() != delivery_ids.len() {
        return Err(format!(
            "safe test-alert delivery leased {} of its exact {} rows",
            deliveries.len(),
            delivery_ids.len()
        )
        .into());
    }
    let summary = match options.delivery_mode {
        DeliveryMode::Webhook => deliver_webhook_batch(client, http, options, deliveries).await?,
        DeliveryMode::RenderFailure => deliver_render_failure_batch(client, deliveries).await?,
        DeliveryMode::None => unreachable!("checked above"),
    };
    let delivered_count: i64 = loyal_yield_orchestrator::sqlx::query_scalar(
        r#"
        SELECT count(*)
        FROM loyal_yield.lookup_table_alert_deliveries
        WHERE id = ANY($1::BIGINT[])
          AND delivery_state = 'delivered'
        "#,
    )
    .bind(delivery_ids)
    .fetch_one(client.pool())
    .await?;
    if delivered_count != i64::try_from(delivery_ids.len())? {
        return Err(format!(
            "safe test-alert delivery completed {delivered_count} of {} exact rule rows (retry_wait={}, dead_letter={})",
            delivery_ids.len(),
            summary.retry_wait,
            summary.dead_letter
        )
        .into());
    }
    Ok(summary)
}

async fn dispatch_webhooks(
    client: &NeonSqlClient,
    http: &HttpClient,
    options: &Options,
) -> Result<DispatchSummary, Box<dyn Error>> {
    let deliveries = lease_lookup_table_alert_deliveries(
        client.pool(),
        &options.worker_id,
        options.delivery_batch_size,
        options.delivery_lease,
    )
    .await?;
    deliver_webhook_batch(client, http, options, deliveries).await
}

async fn deliver_webhook_batch(
    client: &NeonSqlClient,
    http: &HttpClient,
    options: &Options,
    deliveries: Vec<LeasedLookupTableAlertDelivery>,
) -> Result<DispatchSummary, Box<dyn Error>> {
    let webhook_url = options
        .webhook_url
        .as_deref()
        .ok_or("webhook delivery mode lacks YIELD_ALT_ALERT_WEBHOOK_URL")?;
    let mut summary = DispatchSummary::default();
    for delivery in deliveries {
        let mut request = http.post(webhook_url).json(&delivery.payload);
        if let Some(token) = options.webhook_bearer.as_deref() {
            request = request.bearer_auth(token);
        }
        match request.send().await {
            Ok(response) if response.status().is_success() => {
                complete_lookup_table_alert_delivery(
                    client.pool(),
                    delivery.id,
                    delivery.fencing_token,
                    i32::from(response.status().as_u16()),
                )
                .await?;
                summary.delivered += 1;
            }
            Ok(response) => {
                let status = response.status();
                record_delivery_failure(
                    client,
                    &delivery,
                    format!("alert webhook returned HTTP {}", status.as_u16()),
                    Some(status),
                )
                .await?;
                if delivery.attempt_count >= delivery.max_attempts {
                    summary.dead_letter += 1;
                } else {
                    summary.retry_wait += 1;
                }
            }
            Err(error) => {
                record_delivery_failure(
                    client,
                    &delivery,
                    redacted_external_error(&error.to_string()),
                    None,
                )
                .await?;
                if delivery.attempt_count >= delivery.max_attempts {
                    summary.dead_letter += 1;
                } else {
                    summary.retry_wait += 1;
                }
            }
        }
    }
    Ok(summary)
}

async fn record_delivery_failure(
    client: &NeonSqlClient,
    delivery: &LeasedLookupTableAlertDelivery,
    error: String,
    status: Option<StatusCode>,
) -> Result<(), Box<dyn Error>> {
    let exponential = 30_u64.saturating_mul(1_u64 << delivery.attempt_count.min(7));
    fail_lookup_table_alert_delivery(
        client.pool(),
        delivery,
        Duration::from_secs(exponential.min(3_600)),
        &error,
        status.map(|status| i32::from(status.as_u16())),
    )
    .await?;
    Ok(())
}

async fn dispatch_render_failures(
    client: &NeonSqlClient,
    options: &Options,
) -> Result<DispatchSummary, Box<dyn Error>> {
    let deliveries = lease_lookup_table_alert_deliveries(
        client.pool(),
        &options.worker_id,
        options.delivery_batch_size,
        options.delivery_lease,
    )
    .await?;
    deliver_render_failure_batch(client, deliveries).await
}

async fn deliver_render_failure_batch(
    client: &NeonSqlClient,
    deliveries: Vec<LeasedLookupTableAlertDelivery>,
) -> Result<DispatchSummary, Box<dyn Error>> {
    let mut summary = DispatchSummary::default();
    for delivery in deliveries {
        eprintln!(
            "{}",
            json!({
                "event": "reusable_alt_render_failure_delivery",
                "deliveryId": delivery.id,
                "alert": sanitized_render_alert(&delivery.payload),
                "action": "intentional_nonzero_exit_for_render_notification",
                "signerLoaded": false,
            })
        );
        complete_lookup_table_render_failure_delivery(
            client.pool(),
            delivery.id,
            delivery.fencing_token,
        )
        .await?;
        summary.render_failure_deliveries += 1;
    }
    Ok(summary)
}

fn sanitized_render_alert(payload: &Value) -> Value {
    let keys = [
        "event",
        "condition",
        "severity",
        "cluster",
        "policyPubkey",
        "incidentId",
        "revision",
        "fingerprint",
        "summary",
        "testId",
        "ruleVersion",
        "ruleEnabled",
    ];
    let mut sanitized = serde_json::Map::new();
    for key in keys {
        if let Some(value) = payload.get(key) {
            sanitized.insert(key.to_owned(), value.clone());
        }
    }
    Value::Object(sanitized)
}

fn delivery_outcome(
    options: &Options,
    summary: &DispatchSummary,
) -> Result<MonitorOutcome, Box<dyn Error>> {
    if options.delivery_mode == DeliveryMode::RenderFailure && summary.render_failure_deliveries > 0
    {
        Ok(MonitorOutcome::RenderFailureDelivered)
    } else if options.production && options.delivery_mode == DeliveryMode::None {
        Err("production alert monitor has no explicit delivery channel".into())
    } else if options.production && (summary.retry_wait > 0 || summary.dead_letter > 0) {
        Err("production reusable ALT webhook delivery was not accepted".into())
    } else {
        Ok(MonitorOutcome::Success)
    }
}

fn parse_args_with_env<I, F, P>(
    args: I,
    mut env_value: F,
    mut env_present: P,
) -> Result<Options, Box<dyn Error>>
where
    I: IntoIterator<Item = String>,
    F: FnMut(&str) -> Option<String>,
    P: FnMut(&str) -> bool,
{
    for key in FORBIDDEN_ENVIRONMENTS {
        if env_present(key) {
            return Err(format!(
                "environment {key} must not be present in the signerless reusable ALT alert service"
            )
            .into());
        }
    }

    let mut cluster = env_value(CLUSTER_ENV);
    let mut policy_pubkey = env_value(POLICY_PUBKEY_ENV);
    let database_url = env_value(DATABASE_URL_ENV)
        .ok_or("NEON_DATABASE_URL is required for reusable ALT alerts")?;
    let rpc_url =
        env_value(RPC_URL_ENV).ok_or("SOLANA_RPC_URL is required for reusable ALT alerts")?;
    let webhook_url = env_value(WEBHOOK_URL_ENV).filter(|value| !value.trim().is_empty());
    let webhook_bearer = env_value(WEBHOOK_BEARER_ENV).filter(|value| !value.trim().is_empty());
    let mut production = parse_env_bool(env_value(PRODUCTION_ENV).as_deref())?;
    let mut render_failure_delivery = false;
    let mut run_mode = RunMode::Once;
    let mut run_mode_explicit = false;
    let mut test_alerts = false;
    let mut interval_seconds = env_u64(&mut env_value, "YIELD_ALT_ALERT_INTERVAL_SECONDS", 60)?;
    let mut reminder_seconds = env_u64(&mut env_value, "YIELD_ALT_ALERT_REMINDER_SECONDS", 3_600)?;
    let mut delivery_lease_seconds =
        env_u64(&mut env_value, "YIELD_ALT_ALERT_DELIVERY_LEASE_SECONDS", 60)?;
    let mut delivery_batch_size =
        env_i64(&mut env_value, "YIELD_ALT_ALERT_DELIVERY_BATCH_SIZE", 25)?;
    let mut delivery_max_attempts =
        env_i32(&mut env_value, "YIELD_ALT_ALERT_DELIVERY_MAX_ATTEMPTS", 8)?;
    let mut worker_id = env_value("RENDER_INSTANCE_ID")
        .unwrap_or_else(|| format!("alt-alert-monitor-{}", std::process::id()));
    let mut thresholds = LookupTableAlertThresholds::default();
    thresholds.missing_coverage_grace = Duration::from_secs(env_u64(
        &mut env_value,
        "YIELD_ALT_ALERT_MISSING_COVERAGE_GRACE_SECONDS",
        thresholds.missing_coverage_grace.as_secs(),
    )?);
    thresholds.operation_backlog_age = Duration::from_secs(env_u64(
        &mut env_value,
        "YIELD_ALT_ALERT_OPERATION_BACKLOG_SECONDS",
        thresholds.operation_backlog_age.as_secs(),
    )?);
    thresholds.operation_backlog_depth = env_i64(
        &mut env_value,
        "YIELD_ALT_ALERT_OPERATION_BACKLOG_DEPTH",
        thresholds.operation_backlog_depth,
    )?;
    thresholds.capacity_headroom = env_i64(
        &mut env_value,
        "YIELD_ALT_ALERT_CAPACITY_HEADROOM",
        thresholds.capacity_headroom,
    )?;
    thresholds.budget_max_lamports = env_value("YIELD_ALT_ALERT_BUDGET_MAX_LAMPORTS")
        .or_else(|| env_value("YIELD_ALT_MAX_LAMPORTS"))
        .map(|value| value.parse::<i64>())
        .transpose()?;
    thresholds.budget_window = Duration::from_secs(env_u64(
        &mut env_value,
        "YIELD_ALT_ALERT_BUDGET_WINDOW_SECONDS",
        thresholds.budget_window.as_secs(),
    )?);
    thresholds.budget_alert_percent = env_i64(
        &mut env_value,
        "YIELD_ALT_ALERT_BUDGET_PERCENT",
        thresholds.budget_alert_percent,
    )?;
    thresholds.cleanup_grace = Duration::from_secs(env_u64(
        &mut env_value,
        "YIELD_ALT_ALERT_CLEANUP_GRACE_SECONDS",
        thresholds.cleanup_grace.as_secs(),
    )?);

    let mut args = args.into_iter();
    while let Some(argument) = args.next() {
        match argument.as_str() {
            "--cluster" => cluster = Some(next_value(&mut args, "--cluster")?),
            "--policy-pubkey" => policy_pubkey = Some(next_value(&mut args, "--policy-pubkey")?),
            "--once" => {
                if run_mode_explicit && run_mode != RunMode::Once {
                    return Err("--once and --watch are mutually exclusive".into());
                }
                run_mode = RunMode::Once;
                run_mode_explicit = true;
            }
            "--watch" => {
                if run_mode_explicit && run_mode != RunMode::Watch {
                    return Err("--once and --watch are mutually exclusive".into());
                }
                run_mode = RunMode::Watch;
                run_mode_explicit = true;
            }
            "--production" => production = true,
            "--render-failure-delivery" => render_failure_delivery = true,
            "--test-alerts" => test_alerts = true,
            "--interval-seconds" => interval_seconds = next_parse(&mut args, "--interval-seconds")?,
            "--reminder-seconds" => reminder_seconds = next_parse(&mut args, "--reminder-seconds")?,
            "--delivery-lease-seconds" => {
                delivery_lease_seconds = next_parse(&mut args, "--delivery-lease-seconds")?
            }
            "--delivery-batch-size" => {
                delivery_batch_size = next_parse(&mut args, "--delivery-batch-size")?
            }
            "--delivery-max-attempts" => {
                delivery_max_attempts = next_parse(&mut args, "--delivery-max-attempts")?
            }
            "--worker-id" => worker_id = next_value(&mut args, "--worker-id")?,
            "--missing-coverage-grace-seconds" => {
                thresholds.missing_coverage_grace =
                    Duration::from_secs(next_parse(&mut args, "--missing-coverage-grace-seconds")?)
            }
            "--operation-backlog-seconds" => {
                thresholds.operation_backlog_age =
                    Duration::from_secs(next_parse(&mut args, "--operation-backlog-seconds")?)
            }
            "--operation-backlog-depth" => {
                thresholds.operation_backlog_depth =
                    next_parse(&mut args, "--operation-backlog-depth")?
            }
            "--capacity-headroom" => {
                thresholds.capacity_headroom = next_parse(&mut args, "--capacity-headroom")?
            }
            "--budget-max-lamports" => {
                thresholds.budget_max_lamports =
                    Some(next_parse(&mut args, "--budget-max-lamports")?)
            }
            "--budget-window-seconds" => {
                thresholds.budget_window =
                    Duration::from_secs(next_parse(&mut args, "--budget-window-seconds")?)
            }
            "--budget-alert-percent" => {
                thresholds.budget_alert_percent = next_parse(&mut args, "--budget-alert-percent")?
            }
            "--cleanup-grace-seconds" => {
                thresholds.cleanup_grace =
                    Duration::from_secs(next_parse(&mut args, "--cleanup-grace-seconds")?)
            }
            "--help" | "-h" => return Err(USAGE.into()),
            _ => return Err(format!("unknown argument {argument:?}\n{USAGE}").into()),
        }
    }

    let cluster = cluster.ok_or("YIELD_ALT_CLUSTER or --cluster is required")?;
    if cluster == "mainnet-beta" {
        production = true;
    }
    let policy_pubkey = Pubkey::from_str(
        &policy_pubkey.ok_or("YIELD_ALT_POLICY_PUBKEY or --policy-pubkey is required")?,
    )?;
    let delivery_mode = if webhook_url.is_some() {
        DeliveryMode::Webhook
    } else if render_failure_delivery {
        DeliveryMode::RenderFailure
    } else {
        DeliveryMode::None
    };
    if production && delivery_mode == DeliveryMode::None {
        return Err(
            "production requires YIELD_ALT_ALERT_WEBHOOK_URL or explicit --render-failure-delivery"
                .into(),
        );
    }
    if test_alerts && delivery_mode == DeliveryMode::None {
        return Err("--test-alerts requires a webhook or --render-failure-delivery".into());
    }
    if !(1..=3_600).contains(&interval_seconds)
        || !(60..=31_536_000).contains(&reminder_seconds)
        || !(10..=3_600).contains(&delivery_lease_seconds)
        || !(1..=100).contains(&delivery_batch_size)
        || !(1..=100).contains(&delivery_max_attempts)
        || thresholds.operation_backlog_depth <= 0
        || thresholds.capacity_headroom < 0
        || !(1..=100).contains(&thresholds.budget_alert_percent)
        || thresholds
            .budget_max_lamports
            .is_some_and(|value| value <= 0)
        || worker_id.trim().is_empty()
    {
        return Err("invalid reusable ALT alert interval, delivery, or threshold value".into());
    }

    Ok(Options {
        database_url,
        rpc_url,
        cluster,
        policy_pubkey,
        run_mode,
        production,
        delivery_mode,
        webhook_url,
        webhook_bearer,
        test_alerts,
        interval: Duration::from_secs(interval_seconds),
        reminder_interval: Duration::from_secs(reminder_seconds),
        delivery_lease: Duration::from_secs(delivery_lease_seconds),
        delivery_batch_size,
        delivery_max_attempts,
        worker_id,
        thresholds,
    })
}

fn parse_env_bool(value: Option<&str>) -> Result<bool, Box<dyn Error>> {
    match value.map(str::trim) {
        None | Some("") | Some("0") | Some("false") | Some("FALSE") => Ok(false),
        Some("1") | Some("true") | Some("TRUE") => Ok(true),
        Some(_) => Err("YIELD_ALT_ALERT_PRODUCTION must be true/false or 1/0".into()),
    }
}

fn env_u64<F>(env_value: &mut F, key: &str, default: u64) -> Result<u64, Box<dyn Error>>
where
    F: FnMut(&str) -> Option<String>,
{
    env_value(key)
        .map(|value| value.parse::<u64>())
        .transpose()
        .map(|value| value.unwrap_or(default))
        .map_err(Into::into)
}

fn env_i64<F>(env_value: &mut F, key: &str, default: i64) -> Result<i64, Box<dyn Error>>
where
    F: FnMut(&str) -> Option<String>,
{
    env_value(key)
        .map(|value| value.parse::<i64>())
        .transpose()
        .map(|value| value.unwrap_or(default))
        .map_err(Into::into)
}

fn env_i32<F>(env_value: &mut F, key: &str, default: i32) -> Result<i32, Box<dyn Error>>
where
    F: FnMut(&str) -> Option<String>,
{
    env_value(key)
        .map(|value| value.parse::<i32>())
        .transpose()
        .map(|value| value.unwrap_or(default))
        .map_err(Into::into)
}

fn next_value<I>(args: &mut I, flag: &str) -> Result<String, Box<dyn Error>>
where
    I: Iterator<Item = String>,
{
    args.next()
        .ok_or_else(|| format!("{flag} requires a value").into())
}

fn next_parse<T, I>(args: &mut I, flag: &str) -> Result<T, Box<dyn Error>>
where
    T: FromStr,
    T::Err: Error + 'static,
    I: Iterator<Item = String>,
{
    Ok(next_value(args, flag)?.parse::<T>()?)
}

const USAGE: &str = "Usage: route-lookup-table-alert-monitor [--once|--watch] [--cluster <CLUSTER>] [--policy-pubkey <PUBKEY>] [--production] [--render-failure-delivery] [--test-alerts] [threshold flags]\n\nRequires NEON_DATABASE_URL, SOLANA_RPC_URL, and YIELD_ALT_POLICY_PUBKEY. Uses finalized RPC reads. Webhook delivery uses YIELD_ALT_ALERT_WEBHOOK_URL and optional YIELD_ALT_ALERT_WEBHOOK_BEARER_TOKEN. Mainnet/production requires a webhook or explicit --render-failure-delivery. The Render fallback durably records delivery, emits a sanitized JSON alert, and intentionally exits nonzero. This process rejects signer environments, never creates route demand, and never mutates an ALT.";

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::{BTreeMap, BTreeSet};

    fn base_env() -> BTreeMap<String, String> {
        BTreeMap::from([
            (DATABASE_URL_ENV.to_owned(), "postgres://example".to_owned()),
            (RPC_URL_ENV.to_owned(), "https://rpc.example".to_owned()),
            (
                POLICY_PUBKEY_ENV.to_owned(),
                Pubkey::new_unique().to_string(),
            ),
            (CLUSTER_ENV.to_owned(), "mainnet-beta".to_owned()),
        ])
    }

    fn parse(
        args: &[&str],
        values: BTreeMap<String, String>,
        present: BTreeSet<String>,
    ) -> Result<Options, Box<dyn Error>> {
        parse_args_with_env(
            args.iter().map(|value| (*value).to_owned()),
            |key| values.get(key).cloned(),
            |key| present.contains(key),
        )
    }

    #[test]
    fn production_rejects_missing_delivery_channel() {
        let error = parse(&["--once"], base_env(), BTreeSet::new()).unwrap_err();
        assert!(error.to_string().contains("production requires"));
    }

    #[test]
    fn explicit_render_failure_delivery_is_accepted_and_nonzero() {
        let options = parse(
            &["--once", "--render-failure-delivery"],
            base_env(),
            BTreeSet::new(),
        )
        .unwrap();
        assert_eq!(options.delivery_mode, DeliveryMode::RenderFailure);
        assert_eq!(MonitorOutcome::RenderFailureDelivered.exit_code(), 1);
        let outcome = delivery_outcome(
            &options,
            &DispatchSummary {
                render_failure_deliveries: 1,
                ..DispatchSummary::default()
            },
        )
        .unwrap();
        assert_eq!(outcome, MonitorOutcome::RenderFailureDelivered);
    }

    #[test]
    fn webhook_is_preferred_when_both_delivery_modes_are_configured() {
        let mut values = base_env();
        values.insert(
            WEBHOOK_URL_ENV.to_owned(),
            "https://alerts.example/hook".to_owned(),
        );
        let options = parse(
            &["--watch", "--render-failure-delivery"],
            values,
            BTreeSet::new(),
        )
        .unwrap();
        assert_eq!(options.delivery_mode, DeliveryMode::Webhook);
        assert_eq!(options.run_mode, RunMode::Watch);
        assert!(delivery_outcome(
            &options,
            &DispatchSummary {
                retry_wait: 1,
                ..DispatchSummary::default()
            }
        )
        .is_err());
    }

    #[test]
    fn signer_environment_is_rejected_without_reading_its_value() {
        let error = parse(
            &["--once", "--render-failure-delivery"],
            base_env(),
            BTreeSet::from(["POLICY_KEYPAIR".to_owned()]),
        )
        .unwrap_err();
        assert!(error.to_string().contains("POLICY_KEYPAIR"));
        assert!(error.to_string().contains("must not be present"));
    }

    #[test]
    fn timescale_environment_is_rejected_from_the_control_plane_monitor() {
        let error = parse(
            &["--once", "--render-failure-delivery"],
            base_env(),
            BTreeSet::from(["TIMESCALEDB_URL".to_owned()]),
        )
        .unwrap_err();
        assert!(error.to_string().contains("TIMESCALEDB_URL"));
        assert!(error.to_string().contains("must not be present"));
    }

    #[test]
    fn safe_test_rejects_missing_delivery_channel_before_enqueue() {
        let mut values = base_env();
        values.insert(CLUSTER_ENV.to_owned(), "devnet".to_owned());
        let error = parse(&["--once", "--test-alerts"], values, BTreeSet::new()).unwrap_err();
        assert!(error.to_string().contains("--test-alerts requires"));
    }

    #[test]
    fn test_payload_sanitizer_drops_unrecognized_fields() {
        let payload = json!({
            "event": "open",
            "condition": "missing_coverage",
            "summary": "coverage missing",
            "details": {"possiblySensitive": "not emitted"},
            "authorization": "never emit",
        });
        let sanitized = sanitized_render_alert(&payload);
        assert_eq!(sanitized["condition"], "missing_coverage");
        assert!(sanitized.get("details").is_none());
        assert!(sanitized.get("authorization").is_none());
    }
}

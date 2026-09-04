use std::{
    env,
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc,
    },
    time::Duration,
};

use anyhow::{Context, Result};
use balance_sweep_ata_monitor::{
    earn_apy::{
        earn_apy_strategy_for_risk_profile, EarnApyRefreshConfig, EarnApySnapshotRefresher,
    },
    earn_reconciliation::{decode_laserstream_squads_policy_transaction, project_earn_max_memos},
    run_autodeposit_reconciliation_consumer, run_earn_reconciliation_consumer, EarnMonitorMetrics,
    EarnPolicyTransactionRead, RpcEarnChainReader,
};
use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine};
use helius_laserstream::grpc::{subscribe_update::UpdateOneof, SubscribeUpdate};
use loyal_observability::init_from_env;
use loyal_squads_policy_monitor::{
    Cluster as PolicyCluster, Commitment as PolicyCommitment, MonitorConfig as PolicyMonitorConfig,
    PolicyMonitor, PostgresPolicyMatchSink, EARN_MAX_POLICY_PROJECTION_CONSUMER,
};
use loyal_yield_store::{OrchestratorConfig, OrchestratorStore};
use prost::Message;
use serde::Serialize;
use solana_sdk::pubkey::Pubkey;
use tokio::{
    io::{AsyncBufReadExt, AsyncWriteExt, BufReader},
    sync::{Mutex, Notify},
};

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Ack<'a> {
    ok: bool,
    slot: u64,
    error: Option<&'a str>,
}

#[tokio::main]
async fn main() -> Result<()> {
    let observability = init_from_env("loyal-earn-domain-bridge")?;
    let meter = observability.meter("loyal-earn-domain-bridge");
    let database_url = env::var("NEON_DATABASE_URL").context("NEON_DATABASE_URL is required")?;
    let rpc_url = env::var("SOLANA_RPC_URL").context("SOLANA_RPC_URL is required")?;
    let cluster = env::var("SOLANA_CLUSTER").unwrap_or_else(|_| "mainnet".to_owned());
    let policy_cluster = match cluster.as_str() {
        "mainnet" | "mainnet-beta" => PolicyCluster::Mainnet,
        "devnet" => PolicyCluster::Devnet,
        other => anyhow::bail!("unsupported policy-monitor cluster {other}"),
    };
    let earn_max_delegate = env::var("EARN_MAX_DELEGATE")
        .context("EARN_MAX_DELEGATE is required")?
        .parse::<Pubkey>()
        .context("EARN_MAX_DELEGATE must be a Solana pubkey")?;
    let store = OrchestratorStore::connect(OrchestratorConfig::new(database_url.clone())).await?;
    let policy_monitor = Arc::new(Mutex::new(
        PolicyMonitor::new(
            PolicyMonitorConfig {
                cluster: policy_cluster,
                commitment: PolicyCommitment::Confirmed,
                ws_url: String::new(),
            },
            PostgresPolicyMatchSink::from_store(store.clone()),
        )
        .with_earn_max_projection(rpc_url.clone(), earn_max_delegate),
    ));
    let chain = Arc::new(RpcEarnChainReader::new(&rpc_url, store.clone()));
    let running = Arc::new(AtomicBool::new(true));
    let wake = Arc::new(Notify::new());
    let consumer_name = format!("earn-smart-account:{cluster}");
    let metrics = EarnMonitorMetrics::new(&meter, "earn-smart-account", &cluster);
    let mut tasks = Vec::new();
    let earn_workers = env_usize("EARN_RECONCILIATION_CONCURRENCY", 4)?;
    let autodeposit_workers = env_usize("AUTODEPOSIT_RECONCILIATION_CONCURRENCY", 4)?;
    for worker in 0..earn_workers {
        tasks.push(tokio::spawn(run_earn_reconciliation_consumer(
            store.clone(),
            consumer_name.clone(),
            format!("go-bridge-earn:{}:{worker}", std::process::id()),
            chain.clone(),
            policy_monitor.clone(),
            wake.clone(),
            running.clone(),
            metrics.clone(),
        )));
    }
    for worker in 0..autodeposit_workers {
        tasks.push(tokio::spawn(run_autodeposit_reconciliation_consumer(
            store.clone(),
            format!("go-bridge-autodeposit:{}:{worker}", std::process::id()),
            chain.clone(),
            wake.clone(),
            running.clone(),
        )));
    }
    let disable_earn_apy_refresh = match env::var("DISABLE_EARN_APY_REFRESH").as_deref() {
        Err(_) | Ok("false") => false,
        Ok("true") => true,
        Ok(other) => anyhow::bail!("DISABLE_EARN_APY_REFRESH must be true or false, got {other}"),
    };
    if !disable_earn_apy_refresh {
        let timescale_url = env::var("TIMESCALEDB_URL").context("TIMESCALEDB_URL is required")?;
        let profiles = env::var("EARN_APY_RISK_PROFILES").unwrap_or_else(|_| "safe".to_owned());
        let strategies = profiles
            .split(',')
            .map(str::trim)
            .filter(|value| !value.is_empty())
            .map(|value| {
                earn_apy_strategy_for_risk_profile(value)
                    .with_context(|| format!("unsupported Earn APY risk profile {value}"))
            })
            .collect::<Result<Vec<_>>>()?;
        if strategies.is_empty() {
            anyhow::bail!("at least one Earn APY risk profile is required");
        }
        let refresher = EarnApySnapshotRefresher::connect(
            &timescale_url,
            &database_url,
            EarnApyRefreshConfig {
                strategies,
                ..EarnApyRefreshConfig::default()
            },
        )
        .await?;
        let interval = Duration::from_secs(env_u64("EARN_APY_REFRESH_INTERVAL_SECONDS", 3_600)?);
        tasks.push(tokio::spawn(async move {
            loop {
                match refresher.refresh(chrono::Utc::now()).await {
                    Ok(outcome) => tracing::info!(inserted_or_updated = outcome.inserted_or_updated, "refreshed Earn APY snapshots"),
                    Err(error) => tracing::error!(%error, event = "earn_apy_refresh_stalled", "Earn APY snapshot refresh failed"),
                }
                tokio::time::sleep(interval).await;
            }
        }));
    }

    let mut lines = BufReader::new(tokio::io::stdin()).lines();
    let mut stdout = tokio::io::stdout();
    stdout.write_all(b"EARN_BRIDGE_READY\n").await?;
    stdout.flush().await?;
    while let Some(line) = lines.next_line().await? {
        let result = process_update(&line, &store, &policy_monitor).await;
        let (slot, error) = match result {
            Ok(slot) => (slot, None),
            Err(error) => (0, Some(format!("{error:#}"))),
        };
        let ack = serde_json::to_string(&Ack {
            ok: error.is_none(),
            slot,
            error: error.as_deref(),
        })?;
        stdout
            .write_all(format!("EARN_BRIDGE_ACK {ack}\n").as_bytes())
            .await?;
        stdout.flush().await?;
        wake.notify_waiters();
    }

    running.store(false, Ordering::Relaxed);
    wake.notify_waiters();
    for task in tasks {
        task.abort();
        let _ = task.await;
    }
    Ok(())
}

fn env_u64(name: &str, fallback: u64) -> Result<u64> {
    env::var(name)
        .ok()
        .map(|value| {
            value
                .parse()
                .with_context(|| format!("{name} must be an integer"))
        })
        .transpose()
        .map(|value| value.unwrap_or(fallback))
}

fn env_usize(name: &str, fallback: usize) -> Result<usize> {
    let value = env_u64(name, fallback as u64)?;
    usize::try_from(value).with_context(|| format!("{name} exceeds usize"))
}

async fn process_update(
    encoded: &str,
    store: &OrchestratorStore,
    policy_monitor: &Mutex<PolicyMonitor<PostgresPolicyMatchSink>>,
) -> Result<u64> {
    let bytes = BASE64_STANDARD
        .decode(encoded.trim())
        .context("decode Go policy update")?;
    let update =
        SubscribeUpdate::decode(bytes.as_slice()).context("decode protobuf policy update")?;
    let Some(UpdateOneof::Transaction(transaction_update)) = update.update_oneof else {
        anyhow::bail!("Go bridge received a non-transaction policy update");
    };
    let slot = transaction_update.slot;
    let transaction = transaction_update
        .transaction
        .context("policy transaction payload was missing")?;
    if let EarnPolicyTransactionRead::Transaction(transaction) =
        decode_laserstream_squads_policy_transaction(transaction, slot)?
    {
        if !transaction.instructions.is_empty() {
            policy_monitor
                .lock()
                .await
                .process_policy_instructions(
                    &transaction.signature,
                    transaction.slot,
                    transaction.instructions.clone(),
                )
                .await
                .map_err(|error| anyhow::anyhow!(error))?;
        }
        project_earn_max_memos(store, &transaction).await?;
    }
    store
        .advance_projection_offset(
            EARN_MAX_POLICY_PROJECTION_CONSUMER,
            i64::try_from(slot).context("policy slot exceeds BIGINT")?,
        )
        .await?;
    Ok(slot)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn decodes_go_generated_yellowstone_update() {
        let bytes = BASE64_STANDARD
            .decode("ChxlYXJuX21heF9wb2xpY3lfdHJhbnNhY3Rpb25zIgkKBQoDAQIDECo=")
            .expect("base64 Go fixture");
        let update = SubscribeUpdate::decode(bytes.as_slice()).expect("Go protobuf fixture");
        let Some(UpdateOneof::Transaction(transaction)) = update.update_oneof else {
            panic!("fixture was not a transaction");
        };
        assert_eq!(transaction.slot, 42);
        assert_eq!(
            transaction
                .transaction
                .expect("transaction payload")
                .signature,
            [1, 2, 3]
        );
    }
}

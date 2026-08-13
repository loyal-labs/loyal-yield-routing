use std::{
    collections::HashMap,
    env,
    sync::{
        atomic::{AtomicBool, Ordering},
        Arc,
    },
    time::Duration,
};

use anyhow::{bail, Context, Result};
use kamino_reserve_monitor::{
    snapshot_from_account,
    source::{
        AccountEventSender, AccountUpdateEvent, AccountUpdateSource, DurableReplayCursor,
        LaserstreamAccountUpdateSource, SubscriptionConfig, CONFIRMED_COMMITMENT,
        LASERSTREAM_SOURCE,
    },
    timescale::{ReserveUpdateRecord, TimescaleSink, TimescaleSinkConfig},
    ReserveSnapshot,
};
use klend_interface::KLEND_PROGRAM_ID;
use solana_client::rpc_client::RpcClient;
use solana_sdk::{commitment_config::CommitmentConfig, pubkey::Pubkey};

#[tokio::test(flavor = "multi_thread")]
#[ignore = "requires live HELIUS_API_KEY, LASERSTREAM_ENDPOINT, and TIMESCALEDB_URL"]
async fn live_laserstream_account_update_inserts_confirmed_event() -> Result<()> {
    let Some(api_key) = env_var("HELIUS_API_KEY") else {
        eprintln!("skipping live smoke: HELIUS_API_KEY is not set");
        return Ok(());
    };
    let Some(endpoint) = env_var("LASERSTREAM_ENDPOINT") else {
        eprintln!("skipping live smoke: LASERSTREAM_ENDPOINT is not set");
        return Ok(());
    };
    let Some(timescaledb_url) = env_var("TIMESCALEDB_URL") else {
        eprintln!("skipping live smoke: TIMESCALEDB_URL is not set");
        return Ok(());
    };
    let rpc_url = env_var("SOLANA_RPC_URL")
        .unwrap_or_else(|| "https://api.mainnet-beta.solana.com".to_string());

    let timescale = TimescaleSink::connect(TimescaleSinkConfig::new(timescaledb_url)).await?;
    let targets = timescale.load_supported_targets(&[]).await?;
    let target = targets
        .into_iter()
        .next()
        .context("no active supported Kamino reserve targets in Timescale")?;

    let rpc = RpcClient::new_with_commitment(rpc_url, CommitmentConfig::confirmed());
    let seed_slot = rpc.get_slot().context("fetch confirmed seed slot")?;
    let running = Arc::new(AtomicBool::new(true));
    let (tx, mut rx) = AccountEventSender::channel(256);
    let source = LaserstreamAccountUpdateSource {
        endpoint,
        api_key,
        replay_cursor: DurableReplayCursor::new(seed_slot, 32),
        config: SubscriptionConfig {
            max_reconnect_attempts: 3,
            reconnect_base_delay: Duration::from_millis(500),
            reconnect_max_delay: Duration::from_secs(5),
            heartbeat_interval: Duration::from_secs(15),
        },
    };
    let worker = source.spawn(vec![target.reserve], tx, running.clone());

    let mut snapshots = HashMap::<Pubkey, ReserveSnapshot>::new();
    let result = tokio::time::timeout(Duration::from_secs(120), async {
        while let Some(event) = rx.recv().await {
            let AccountUpdateEvent::AccountUpdate {
                metadata,
                reserve,
                slot,
                owner,
                data,
                received_at,
                received_instant,
            } = event
            else {
                if let AccountUpdateEvent::Failed {
                    reserve,
                    attempts,
                    error,
                } = event
                {
                    bail!("LaserStream failed for {reserve} after {attempts} attempt(s): {error}");
                }
                continue;
            };

            if reserve != target.reserve {
                continue;
            }
            if owner != KLEND_PROGRAM_ID.to_string() {
                bail!("LaserStream reserve {reserve} owner {owner} did not match KLend");
            }
            let snapshot = snapshot_from_account(&target, slot, &data, 400.0)
                .with_context(|| format!("decode reserve account {reserve}"))?;
            if let Some(expected_market) = target.market {
                if snapshot.market != Some(expected_market) {
                    bail!(
                        "LaserStream reserve {reserve} decoded market {:?}, expected {expected_market}",
                        snapshot.market
                    );
                }
            }
            if let Some(expected_mint) = target.liquidity_mint {
                if snapshot.liquidity_mint != expected_mint {
                    bail!(
                        "LaserStream reserve {reserve} decoded mint {}, expected {expected_mint}",
                        snapshot.liquidity_mint
                    );
                }
            }

            let enriched_target = target_with_snapshot_metadata(&target, &snapshot);
            let account_data_hash = TimescaleSink::account_data_hash(&data);
            let decoded_at = chrono::Utc::now();
            let record = ReserveUpdateRecord {
                kind: "reserve_update",
                source: metadata.source,
                observed_at: snapshot.observed_at,
                slot,
                target: &enriched_target,
                snapshot: &snapshot,
                diff_summary: "laserstream_live_smoke",
                diff: None,
                raw_account_data_base64: None,
                source_commitment: metadata.source_commitment,
                account_data_hash: &account_data_hash,
                received_at,
                decoded_at,
                receive_to_decode_ms: received_instant.elapsed().as_millis(),
            };
            let outcome = timescale.insert(&record).await?;
            assert_eq!(record.source, LASERSTREAM_SOURCE);
            assert_eq!(record.source_commitment, CONFIRMED_COMMITMENT);
            snapshots.insert(reserve, snapshot);
            return Ok(outcome.inserted);
        }
        bail!("LaserStream event channel closed before receiving account update")
    })
    .await
    .context("timed out waiting for LaserStream account update")?;

    running.store(false, Ordering::Relaxed);
    worker.abort();
    result.map(|_| ())
}

fn env_var(key: &str) -> Option<String> {
    env::var(key).ok().filter(|value| !value.trim().is_empty())
}

fn target_with_snapshot_metadata(
    target: &kamino_reserve_monitor::ReserveTarget,
    snapshot: &ReserveSnapshot,
) -> kamino_reserve_monitor::ReserveTarget {
    let mut enriched = target.clone();
    if enriched.market.is_none() {
        enriched.market = snapshot.market;
    }
    if enriched.symbol.is_none() {
        enriched.symbol = snapshot.symbol.clone();
    }
    if enriched.liquidity_mint.is_none() {
        enriched.liquidity_mint = Some(snapshot.liquidity_mint);
    }
    enriched
}

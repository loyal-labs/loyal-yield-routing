//! Out-of-band reconciliation for wallet ATA updates the stream cannot record.
//!
//! A LaserStream update for a wallet ATA that is no longer an SPL Token account
//! carries the post-close state of a transaction the cluster may still roll
//! back, and the wallet may recreate the ATA moments later. Instead of trusting
//! that frame, the event loop hands the target to this queue, which waits and
//! then reads the account's actual state over RPC. Keeping the read here also
//! keeps RPC latency out of the hot event loop.

use std::{collections::HashMap, time::Duration};

use anyhow::{Context, Result};
use chrono::Utc;
use serde_json::json;
use solana_client::rpc_client::RpcClient;
use solana_sdk::{commitment_config::CommitmentConfig, pubkey::Pubkey};
use tokio::{
    sync::mpsc,
    task::JoinHandle,
    time::{self, Instant},
};

use crate::{
    account_data_hash, process_account_update, AtaObservationSink, AtaTarget, AtaUpdateOutcome,
    AtaUpdateSkip, BalanceSweepAtaObservation, ObservationInsertOutcome, CONFIRMED_COMMITMENT,
};
use loyal_actions::USDC_MINT;

pub const RPC_RECHECK_SOURCE: &str = "rpc_recheck";

#[derive(Debug, Clone, Copy)]
pub struct AtaRecheckConfig {
    /// How long to wait after the skipped update before reading the account.
    pub delay: Duration,
    /// How long to wait before retrying a recheck that failed on RPC.
    pub retry_backoff: Duration,
    /// How many RPC reads a single target gets before the recheck is dropped.
    pub max_attempts: u32,
}

impl Default for AtaRecheckConfig {
    fn default() -> Self {
        Self {
            delay: Duration::from_secs(30),
            retry_backoff: Duration::from_secs(30),
            max_attempts: 3,
        }
    }
}

#[derive(Debug)]
struct AtaRecheckRequest {
    target: AtaTarget,
    skip: AtaUpdateSkip,
    stream_slot: u64,
}

#[derive(Debug)]
struct PendingRecheck {
    target: AtaTarget,
    skip: AtaUpdateSkip,
    stream_slot: u64,
    attempt: u32,
    due_at: Instant,
}

/// Enqueues wallet ATA rechecks. Cloning is cheap and keeps the worker alive.
#[derive(Clone, Debug)]
pub struct AtaRecheckHandle {
    tx: mpsc::UnboundedSender<AtaRecheckRequest>,
}

impl AtaRecheckHandle {
    pub fn enqueue(&self, target: &AtaTarget, skip: AtaUpdateSkip, stream_slot: u64) {
        let request = AtaRecheckRequest {
            target: target.clone(),
            skip,
            stream_slot,
        };
        if self.tx.send(request).is_err() {
            tracing::warn!(
                wallet_usdc_ata = %target.wallet_usdc_ata,
                "wallet ATA recheck worker is gone, dropping recheck"
            );
        }
    }
}

/// Spawns the recheck worker. The worker stops once every handle is dropped.
pub fn spawn_ata_recheck_worker<S>(
    rpc_url: String,
    sink: S,
    config: AtaRecheckConfig,
) -> (AtaRecheckHandle, JoinHandle<()>)
where
    S: AtaObservationSink + Send + Sync + 'static,
{
    let (tx, rx) = mpsc::unbounded_channel();
    let task = tokio::spawn(async move { run_recheck_worker(rx, rpc_url, sink, config).await });
    (AtaRecheckHandle { tx }, task)
}

async fn run_recheck_worker<S>(
    mut rx: mpsc::UnboundedReceiver<AtaRecheckRequest>,
    rpc_url: String,
    sink: S,
    config: AtaRecheckConfig,
) where
    S: AtaObservationSink + Send + Sync + 'static,
{
    let mut pending: HashMap<Pubkey, PendingRecheck> = HashMap::new();
    loop {
        let next_due = pending.values().map(|entry| entry.due_at).min();
        tokio::select! {
            request = rx.recv() => {
                let Some(request) = request else {
                    break;
                };
                // A later update for the same ATA replaces the pending one: only
                // the account's final state matters, and one read settles it.
                pending.insert(
                    request.target.wallet_usdc_ata,
                    PendingRecheck {
                        target: request.target,
                        skip: request.skip,
                        stream_slot: request.stream_slot,
                        attempt: 0,
                        due_at: Instant::now() + config.delay,
                    },
                );
            }
            _ = wait_until(next_due) => {
                run_due_rechecks(&mut pending, &rpc_url, &sink, config).await;
            }
        }
    }
}

async fn wait_until(deadline: Option<Instant>) {
    match deadline {
        Some(deadline) => time::sleep_until(deadline).await,
        None => std::future::pending::<()>().await,
    }
}

async fn run_due_rechecks<S>(
    pending: &mut HashMap<Pubkey, PendingRecheck>,
    rpc_url: &str,
    sink: &S,
    config: AtaRecheckConfig,
) where
    S: AtaObservationSink,
{
    let now = Instant::now();
    let due = pending
        .iter()
        .filter(|(_, entry)| entry.due_at <= now)
        .map(|(account, _)| *account)
        .collect::<Vec<_>>();
    for account in due {
        let Some(mut entry) = pending.remove(&account) else {
            continue;
        };
        entry.attempt = entry.attempt.saturating_add(1);
        let Err(error) = recheck_wallet_ata(&entry, rpc_url, sink).await else {
            continue;
        };
        if entry.attempt >= config.max_attempts {
            tracing::warn!(
                wallet_usdc_ata = %account,
                attempts = entry.attempt,
                error = %error,
                "giving up on wallet ATA recheck"
            );
            continue;
        }
        tracing::warn!(
            wallet_usdc_ata = %account,
            attempt = entry.attempt,
            error = %error,
            "retrying wallet ATA recheck"
        );
        entry.due_at = Instant::now() + config.retry_backoff;
        pending.insert(account, entry);
    }
}

async fn recheck_wallet_ata<S>(entry: &PendingRecheck, rpc_url: &str, sink: &S) -> Result<()>
where
    S: AtaObservationSink,
{
    let account = entry.target.wallet_usdc_ata;
    let rpc_url = rpc_url.to_owned();
    let fetched = tokio::task::spawn_blocking(move || {
        let rpc = RpcClient::new_with_commitment(rpc_url, CommitmentConfig::confirmed());
        rpc.get_account_with_commitment(&account, CommitmentConfig::confirmed())
    })
    .await
    .context("join wallet ATA recheck task")?
    .with_context(|| format!("fetch wallet ATA {account} recheck state"))?;
    let slot = fetched.context.slot;

    let Some(fetched_account) = fetched.value else {
        record_closed_ata(entry, slot, sink).await?;
        return Ok(());
    };
    let outcome = process_account_update(
        &entry.target,
        fetched_account.lamports,
        slot,
        fetched_account.owner,
        fetched_account.data,
        RPC_RECHECK_SOURCE,
        CONFIRMED_COMMITMENT,
        None,
        Utc::now(),
        sink,
    )
    .await?;
    match outcome {
        AtaUpdateOutcome::Recorded(recorded) => {
            tracing::info!(
                wallet_usdc_ata = %account,
                slot,
                event_id = recorded.event_id,
                inserted = recorded.inserted,
                "recorded wallet ATA recheck observation"
            );
        }
        // The account is still not a USDC token account, so the balance the
        // sweep pipeline can act on is zero either way.
        AtaUpdateOutcome::Skipped(AtaUpdateSkip::NonSplTokenOwner { .. }) => {
            record_closed_ata(entry, slot, sink).await?;
        }
        AtaUpdateOutcome::Skipped(AtaUpdateSkip::UnexpectedMint { mint }) => {
            tracing::warn!(
                wallet_usdc_ata = %account,
                mint = %mint,
                slot,
                "wallet ATA recheck found a non-USDC token account"
            );
        }
    }
    Ok(())
}

async fn record_closed_ata<S>(
    entry: &PendingRecheck,
    slot: u64,
    sink: &S,
) -> Result<ObservationInsertOutcome>
where
    S: AtaObservationSink,
{
    let target = &entry.target;
    let observed_at = Utc::now();
    let hash = account_data_hash(&[]);
    let observation = BalanceSweepAtaObservation {
        target_id: target.id,
        cluster: target.cluster.clone(),
        wallet: target.wallet.clone(),
        wallet_usdc_ata: target.wallet_usdc_ata.to_string(),
        vault_pubkey: target.vault_pubkey.clone(),
        vault_usdc_ata: target.vault_usdc_ata.to_string(),
        amount_raw: 0,
        owner: Some(target.wallet.clone()),
        mint: USDC_MINT.to_string(),
        slot,
        observed_at,
        source: RPC_RECHECK_SOURCE.to_owned(),
        source_commitment: CONFIRMED_COMMITMENT.to_owned(),
        txn_signature: None,
        account_data_hash: hash.clone(),
        raw_account_data_base64: String::new(),
        raw_evidence: json!({
            "closed": true,
            "account_data_hash": hash,
            "source": RPC_RECHECK_SOURCE,
            "recheck_reason": entry.skip.reason(),
            "stream_slot": entry.stream_slot,
            "wallet": target.wallet,
            "wallet_usdc_ata": target.wallet_usdc_ata.to_string(),
            "vault_pubkey": target.vault_pubkey,
            "vault_usdc_ata": target.vault_usdc_ata.to_string(),
        }),
        received_at: observed_at,
    };
    let outcome = sink.record_observation(observation).await?;
    tracing::info!(
        wallet_usdc_ata = %target.wallet_usdc_ata,
        slot,
        event_id = outcome.event_id,
        inserted = outcome.inserted,
        reason = entry.skip.reason(),
        "recorded zero balance for closed wallet ATA"
    );
    Ok(outcome)
}

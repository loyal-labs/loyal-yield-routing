use std::{collections::BTreeSet, sync::Arc, time::Duration};

use anyhow::{bail, Context, Result};
use chrono::{DateTime, Utc};
use solana_client::nonblocking::rpc_client::RpcClient;
use solana_sdk::{account::Account, commitment_config::CommitmentConfig, pubkey::Pubkey};
use tokio::time::Instant;

/// Solana `getMultipleAccounts` accepts at most 100 addresses per request.
pub const CONFIRMED_REFRESH_BATCH_SIZE: usize = 100;

#[derive(Clone, Debug)]
pub struct ConfirmedReserveState {
    pub reserve: Pubkey,
    /// `None` is retained as a reserve-scoped invalid observation so other
    /// valid accounts from the same HTTP batch can still be admitted.
    pub account: Option<Account>,
    /// The context slot returned with the exact account batch, not a separate
    /// `getSlot` observation.
    pub verified_slot: u64,
    pub verified_at: DateTime<Utc>,
    pub received_instant: Instant,
}

#[derive(Clone)]
pub struct ConfirmedReserveVerifier {
    rpc: Arc<RpcClient>,
}

impl ConfirmedReserveVerifier {
    pub fn new(rpc_url: String, request_timeout: Duration) -> Self {
        Self {
            rpc: Arc::new(RpcClient::new_with_timeout_and_commitment(
                rpc_url,
                request_timeout,
                CommitmentConfig::confirmed(),
            )),
        }
    }

    /// RPC batch failures remain all-or-nothing, but a missing account is
    /// returned as a reserve-scoped invalid observation instead of hiding the
    /// valid accounts in the same response.
    pub async fn fetch(&self, reserves: &[Pubkey]) -> Result<Vec<ConfirmedReserveState>> {
        let unique = reserves.iter().copied().collect::<BTreeSet<_>>();
        if unique.len() != reserves.len() {
            bail!("confirmed reserve refresh requires unique reserve addresses");
        }

        let mut states = Vec::with_capacity(reserves.len());
        for batch in reserves.chunks(CONFIRMED_REFRESH_BATCH_SIZE) {
            let response = self
                .rpc
                .get_multiple_accounts_with_commitment(batch, CommitmentConfig::confirmed())
                .await
                .context("fetch confirmed reserve account batch")?;
            if response.value.len() != batch.len() {
                bail!(
                    "confirmed reserve response returned {} accounts for {} addresses",
                    response.value.len(),
                    batch.len()
                );
            }
            let verified_at = Utc::now();
            let received_instant = Instant::now();
            for (reserve, account) in batch.iter().copied().zip(response.value) {
                states.push(ConfirmedReserveState {
                    reserve,
                    account,
                    verified_slot: response.context.slot,
                    verified_at,
                    received_instant,
                });
            }
        }
        Ok(states)
    }
}

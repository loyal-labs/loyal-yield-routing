use sha2::{Digest, Sha256};
use solana_rpc_client::rpc_client::RpcClient;
use solana_rpc_client_api::{
    config::{RpcSendTransactionConfig, RpcSimulateTransactionConfig},
    response::RpcSimulateTransactionResult,
};
use solana_sdk::{
    commitment_config::CommitmentConfig, hash::Hash, signature::Signature,
    transaction::VersionedTransaction,
};

use crate::{ConfirmationObservation, OrchestratorError};

#[derive(Debug, Clone)]
pub struct RpcAdapterConfig {
    pub url: String,
    pub commitment: CommitmentConfig,
    pub skip_preflight: bool,
    pub max_retries: Option<usize>,
}

impl RpcAdapterConfig {
    pub fn new(url: impl Into<String>) -> Self {
        Self {
            url: url.into(),
            commitment: CommitmentConfig::confirmed(),
            skip_preflight: true,
            max_retries: Some(0),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct BlockhashWindow {
    pub blockhash: Hash,
    pub last_valid_block_height: u64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct RpcSimulationReport {
    pub slot: u64,
    pub error: Option<String>,
    pub logs: Vec<String>,
    pub logs_hash: Option<String>,
    pub units_consumed: Option<u64>,
    pub replacement_blockhash: Option<String>,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RpcErrorClass {
    RateLimited,
    BlockhashExpired,
    Unavailable,
    Rejected,
    Unknown,
}

pub struct RpcAdapter {
    client: RpcClient,
    config: RpcAdapterConfig,
}

impl RpcAdapter {
    pub fn new(config: RpcAdapterConfig) -> Self {
        let client = RpcClient::new_with_commitment(config.url.clone(), config.commitment);
        Self { client, config }
    }

    pub fn from_url(url: impl Into<String>) -> Self {
        Self::new(RpcAdapterConfig::new(url))
    }

    pub fn latest_blockhash(&self) -> Result<BlockhashWindow, OrchestratorError> {
        let (blockhash, last_valid_block_height) = self
            .client
            .get_latest_blockhash_with_commitment(self.config.commitment)
            .map_err(rpc_error)?;
        Ok(BlockhashWindow {
            blockhash,
            last_valid_block_height,
        })
    }

    pub fn block_height(&self) -> Result<u64, OrchestratorError> {
        self.client.get_block_height().map_err(rpc_error)
    }

    pub fn simulate_transaction(
        &self,
        transaction: &VersionedTransaction,
    ) -> Result<RpcSimulationReport, OrchestratorError> {
        let response = self
            .client
            .simulate_transaction_with_config(
                transaction,
                RpcSimulateTransactionConfig {
                    sig_verify: true,
                    commitment: self.config.commitment.ok(),
                    ..RpcSimulateTransactionConfig::default()
                },
            )
            .map_err(rpc_error)?;
        Ok(simulation_report_from_rpc(
            response.context.slot,
            response.value,
        ))
    }

    pub fn send_transaction(
        &self,
        transaction: &VersionedTransaction,
    ) -> Result<Signature, OrchestratorError> {
        self.client
            .send_transaction_with_config(
                transaction,
                RpcSendTransactionConfig {
                    skip_preflight: self.config.skip_preflight,
                    preflight_commitment: Some(self.config.commitment.commitment),
                    max_retries: self.config.max_retries,
                    ..RpcSendTransactionConfig::default()
                },
            )
            .map_err(rpc_error)
    }

    pub fn signature_status(
        &self,
        signature: &Signature,
    ) -> Result<ConfirmationObservation, OrchestratorError> {
        let response = self
            .client
            .get_signature_statuses(&[*signature])
            .map_err(rpc_error)?;
        Ok(signature_status_observation(
            response.value.into_iter().next().flatten(),
        ))
    }

    pub fn signature_status_with_history(
        &self,
        signature: &Signature,
    ) -> Result<ConfirmationObservation, OrchestratorError> {
        let response = self
            .client
            .get_signature_statuses_with_history(&[*signature])
            .map_err(rpc_error)?;
        Ok(signature_status_observation(
            response.value.into_iter().next().flatten(),
        ))
    }
}

pub fn simulation_report_from_rpc(
    slot: u64,
    value: RpcSimulateTransactionResult,
) -> RpcSimulationReport {
    let logs = value.logs.unwrap_or_default();
    RpcSimulationReport {
        slot,
        error: value.err.map(|error| format!("{error:?}")),
        logs_hash: (!logs.is_empty()).then(|| hash_logs(&logs)),
        logs,
        units_consumed: value.units_consumed,
        replacement_blockhash: value
            .replacement_blockhash
            .map(|blockhash| blockhash.blockhash),
    }
}

pub fn classify_rpc_error_message(message: &str) -> RpcErrorClass {
    let lower = message.to_ascii_lowercase();
    if lower.contains("429")
        || lower.contains("too many requests")
        || lower.contains("rate limit")
        || lower.contains("rate-limit")
    {
        return RpcErrorClass::RateLimited;
    }
    if lower.contains("blockhash not found")
        || lower.contains("block height exceeded")
        || lower.contains("transaction expired")
    {
        return RpcErrorClass::BlockhashExpired;
    }
    if lower.contains("node is unhealthy")
        || lower.contains("node unhealthy")
        || lower.contains("temporarily unavailable")
        || lower.contains("503")
        || lower.contains("timeout")
        || lower.contains("timed out")
    {
        return RpcErrorClass::Unavailable;
    }
    if lower.contains("transaction simulation failed")
        || lower.contains("preflight")
        || lower.contains("signature verification failed")
    {
        return RpcErrorClass::Rejected;
    }
    RpcErrorClass::Unknown
}

pub fn hash_logs(logs: &[String]) -> String {
    let mut hasher = Sha256::new();
    for log in logs {
        hasher.update(log.as_bytes());
        hasher.update([0]);
    }
    format!("{:x}", hasher.finalize())
}

fn signature_status_observation(
    status: Option<solana_transaction_status_client_types::TransactionStatus>,
) -> ConfirmationObservation {
    let Some(status) = status else {
        return ConfirmationObservation::Unknown;
    };
    let slot = i64::try_from(status.slot).ok();
    match status.err {
        Some(error) => ConfirmationObservation::Failed {
            reason: format!("{error:?}"),
        },
        None => ConfirmationObservation::Confirmed { slot },
    }
}

fn rpc_error(error: solana_rpc_client_api::client_error::Error) -> OrchestratorError {
    OrchestratorError::Rpc(error.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn classifies_rate_limit_errors() {
        assert_eq!(
            classify_rpc_error_message("HTTP status 429 Too Many Requests"),
            RpcErrorClass::RateLimited
        );
        assert_eq!(
            classify_rpc_error_message("transaction simulation failed"),
            RpcErrorClass::Rejected
        );
    }

    #[test]
    fn hashes_logs_stably() {
        let logs = vec![
            "Program A invoke [1]".to_owned(),
            "Program A success".to_owned(),
        ];

        assert_eq!(hash_logs(&logs), hash_logs(&logs));
        assert_ne!(hash_logs(&logs), hash_logs(&logs[..1]));
    }
}

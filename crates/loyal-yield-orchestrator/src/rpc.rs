use serde_json::{json, Value};
use solana_client::{rpc_client::RpcClient, rpc_config::RpcSimulateTransactionConfig};
use solana_sdk::{
    instruction::Instruction,
    signature::{Keypair, Signature},
    signer::Signer,
    transaction::Transaction,
};
use thiserror::Error;

#[derive(Debug, Error)]
pub enum RpcSubmitError {
    #[error("RPC error: {0}")]
    Rpc(String),
    #[error("submitted signature {0} did not have a status after confirmation")]
    MissingSignatureStatus(Signature),
}

#[derive(Debug, Clone)]
pub struct SimulationOutcome {
    pub ok: bool,
    pub slot: u64,
    pub report: Value,
}

#[derive(Debug, Clone)]
pub struct SubmissionOutcome {
    pub signature: Signature,
    pub slot: u64,
    pub report: Value,
}

pub struct RpcRouteSubmitter<'a> {
    rpc: &'a RpcClient,
}

impl<'a> RpcRouteSubmitter<'a> {
    pub fn new(rpc: &'a RpcClient) -> Self {
        Self { rpc }
    }

    pub fn simulate_instructions(
        &self,
        instructions: &[Instruction],
        signer: &Keypair,
    ) -> Result<SimulationOutcome, RpcSubmitError> {
        let transaction = self.signed_transaction(instructions, signer)?;
        let simulation = self
            .rpc
            .simulate_transaction_with_config(
                &transaction,
                RpcSimulateTransactionConfig {
                    sig_verify: true,
                    inner_instructions: true,
                    ..RpcSimulateTransactionConfig::default()
                },
            )
            .map_err(|error| RpcSubmitError::Rpc(error.to_string()))?;

        let ok = simulation.value.err.is_none();
        Ok(SimulationOutcome {
            ok,
            slot: simulation.context.slot,
            report: json!({
                "attempted": true,
                "ok": ok,
                "slot": simulation.context.slot,
                "error": simulation.value.err.as_ref().map(|error| format!("{error:?}")),
                "unitsConsumed": simulation.value.units_consumed,
                "loadedAccountsDataSize": simulation.value.loaded_accounts_data_size,
                "logs": simulation.value.logs,
                "instructionCount": instructions.len(),
            }),
        })
    }

    pub fn submit_and_confirm(
        &self,
        instructions: &[Instruction],
        signer: &Keypair,
    ) -> Result<SubmissionOutcome, RpcSubmitError> {
        let transaction = self.signed_transaction(instructions, signer)?;
        let signature = self
            .rpc
            .send_and_confirm_transaction(&transaction)
            .map_err(|error| RpcSubmitError::Rpc(error.to_string()))?;
        let slot = confirmed_signature_slot(self.rpc, signature)?;
        Ok(SubmissionOutcome {
            signature,
            slot,
            report: json!({
                "attempted": true,
                "signature": signature.to_string(),
                "slot": slot,
                "instructionCount": instructions.len(),
            }),
        })
    }

    fn signed_transaction(
        &self,
        instructions: &[Instruction],
        signer: &Keypair,
    ) -> Result<Transaction, RpcSubmitError> {
        let blockhash = self
            .rpc
            .get_latest_blockhash()
            .map_err(|error| RpcSubmitError::Rpc(error.to_string()))?;
        Ok(Transaction::new_signed_with_payer(
            instructions,
            Some(&signer.pubkey()),
            &[signer],
            blockhash,
        ))
    }
}

fn confirmed_signature_slot(rpc: &RpcClient, signature: Signature) -> Result<u64, RpcSubmitError> {
    let statuses = rpc
        .get_signature_statuses(&[signature])
        .map_err(|error| RpcSubmitError::Rpc(error.to_string()))?;
    if let Some(status) = statuses.value.into_iter().flatten().next() {
        return Ok(status.slot);
    }
    Err(RpcSubmitError::MissingSignatureStatus(signature))
}

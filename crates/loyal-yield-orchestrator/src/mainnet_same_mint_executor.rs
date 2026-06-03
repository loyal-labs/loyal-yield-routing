//! RPC-backed same-mint route execution for mainnet-style pre-production runs.
//!
//! This module deliberately separates live Kamino route preparation from batch
//! execution. A `SameMintRoutePreparer` must quote the planned route and return
//! the exact Solana instructions to simulate/submit; the executor never invents
//! reserve exchange rates or placeholder instructions.

use std::{
    env,
    sync::{Arc, Mutex},
    time::Duration,
};

use solana_rpc_client::rpc_client::RpcClient;
use solana_sdk::{
    hash::Hash, instruction::Instruction, pubkey::Pubkey, signature::Keypair, signer::Signer,
    transaction::Transaction,
};
use thiserror::Error;

use crate::{
    yield_router_keypair_from_env, PolicySignerError, SameMintBatchSimulation,
    SameMintBatchSubmission, SameMintLoopFuture, SameMintRouteExecution, SameMintRouteExecutor,
    SameMintRouteLoopError, SameMintRouteQuote, SameMintRouteQuoteRequest,
};

pub const SOLANA_RPC_URL_ENV: &str = "SOLANA_RPC_URL";
pub const SAME_MINT_SUBMIT_TXS_ENV: &str = "SAME_MINT_SUBMIT_TXS";
const DEFAULT_RPC_TIMEOUT: Duration = Duration::from_secs(30);

pub trait SameMintRoutePreparer {
    fn prepare_same_mint_route<'a>(
        &'a self,
        request: SameMintRouteQuoteRequest,
    ) -> SameMintLoopFuture<'a, SameMintRouteQuote>;
}

#[derive(Debug, Clone)]
pub struct MainnetSameMintExecutorConfig {
    pub rpc_url: String,
    pub rpc_timeout: Duration,
    pub submit_transactions: bool,
}

impl MainnetSameMintExecutorConfig {
    pub fn new(rpc_url: impl Into<String>) -> Self {
        Self {
            rpc_url: rpc_url.into(),
            rpc_timeout: DEFAULT_RPC_TIMEOUT,
            submit_transactions: false,
        }
    }

    pub fn from_env() -> Result<Self, MainnetSameMintExecutorConfigError> {
        let rpc_url = env::var(SOLANA_RPC_URL_ENV).map_err(|error| match error {
            env::VarError::NotPresent => MainnetSameMintExecutorConfigError::MissingEnv {
                name: SOLANA_RPC_URL_ENV,
            },
            env::VarError::NotUnicode(_) => MainnetSameMintExecutorConfigError::InvalidEnv {
                name: SOLANA_RPC_URL_ENV,
            },
        })?;
        Ok(Self::new(rpc_url)
            .with_submit_transactions(optional_bool_env(SAME_MINT_SUBMIT_TXS_ENV)?))
    }

    pub fn with_rpc_timeout(mut self, rpc_timeout: Duration) -> Self {
        self.rpc_timeout = rpc_timeout;
        self
    }

    pub fn with_submit_transactions(mut self, submit_transactions: bool) -> Self {
        self.submit_transactions = submit_transactions;
        self
    }
}

#[derive(Debug, Error)]
pub enum MainnetSameMintExecutorConfigError {
    #[error("{name} is not set")]
    MissingEnv { name: &'static str },
    #[error("{name} must be valid unicode")]
    InvalidEnv { name: &'static str },
    #[error("{name} must be true/false, 1/0, or yes/no")]
    InvalidBool { name: &'static str },
}

pub struct MainnetSameMintExecutor<P> {
    rpc: Arc<RpcClient>,
    fee_payer: Arc<Mutex<Keypair>>,
    preparer: P,
    submit_transactions: bool,
}

impl<P> MainnetSameMintExecutor<P> {
    pub fn new(config: MainnetSameMintExecutorConfig, fee_payer: Keypair, preparer: P) -> Self {
        let rpc = RpcClient::new_with_timeout(config.rpc_url, config.rpc_timeout);
        Self::with_rpc_client(
            Arc::new(rpc),
            fee_payer,
            preparer,
            config.submit_transactions,
        )
    }

    pub fn from_env(
        config: MainnetSameMintExecutorConfig,
        preparer: P,
    ) -> Result<Self, PolicySignerError> {
        Ok(Self::new(
            config,
            yield_router_keypair_from_env()?,
            preparer,
        ))
    }

    pub fn with_rpc_client(
        rpc: Arc<RpcClient>,
        fee_payer: Keypair,
        preparer: P,
        submit_transactions: bool,
    ) -> Self {
        Self {
            rpc,
            fee_payer: Arc::new(Mutex::new(fee_payer)),
            preparer,
            submit_transactions,
        }
    }

    pub fn fee_payer_pubkey(&self) -> Result<Pubkey, SameMintRouteLoopError> {
        let fee_payer = self
            .fee_payer
            .lock()
            .map_err(|_| SameMintRouteLoopError::submission("fee payer keypair lock poisoned"))?;
        Ok(fee_payer.pubkey())
    }

    pub fn submit_transactions(&self) -> bool {
        self.submit_transactions
    }
}

impl<P> SameMintRouteExecutor for MainnetSameMintExecutor<P>
where
    P: SameMintRoutePreparer + Sync,
{
    fn quote_same_mint_route<'a>(
        &'a self,
        request: SameMintRouteQuoteRequest,
    ) -> SameMintLoopFuture<'a, SameMintRouteQuote> {
        self.preparer.prepare_same_mint_route(request)
    }

    fn simulate_same_mint_batch<'a>(
        &'a self,
        routes: &'a [SameMintRouteExecution],
    ) -> SameMintLoopFuture<'a, SameMintBatchSimulation> {
        Box::pin(async move {
            let instructions = collect_batch_instructions(routes, BatchPhase::Simulation)?;
            let rpc = Arc::clone(&self.rpc);
            let fee_payer = Arc::clone(&self.fee_payer);
            tokio::task::spawn_blocking(move || {
                simulate_signed_batch(&rpc, &fee_payer, instructions)
            })
            .await
            .map_err(|error| {
                BatchPhase::Simulation.error(format!("simulation task failed: {error}"))
            })?
        })
    }

    fn submit_same_mint_batch<'a>(
        &'a self,
        routes: &'a [SameMintRouteExecution],
    ) -> SameMintLoopFuture<'a, SameMintBatchSubmission> {
        Box::pin(async move {
            if !self.submit_transactions {
                return Err(SameMintRouteLoopError::submission(
                    "mainnet same-mint submission is disabled; set SAME_MINT_SUBMIT_TXS=true",
                ));
            }

            let instructions = collect_batch_instructions(routes, BatchPhase::Submission)?;
            let rpc = Arc::clone(&self.rpc);
            let fee_payer = Arc::clone(&self.fee_payer);
            tokio::task::spawn_blocking(move || submit_signed_batch(&rpc, &fee_payer, instructions))
                .await
                .map_err(|error| {
                    BatchPhase::Submission.error(format!("submission task failed: {error}"))
                })?
        })
    }
}

fn simulate_signed_batch(
    rpc: &RpcClient,
    fee_payer: &Mutex<Keypair>,
    instructions: Vec<Instruction>,
) -> Result<SameMintBatchSimulation, SameMintRouteLoopError> {
    let transaction = signed_transaction(rpc, fee_payer, instructions, BatchPhase::Simulation)?;
    let simulation = rpc.simulate_transaction(&transaction).map_err(|error| {
        BatchPhase::Simulation.error(format!("RPC simulation request failed: {error}"))
    })?;

    if let Some(error) = simulation.value.err {
        return Err(BatchPhase::Simulation.error(format!(
            "simulation returned {error:?}{}",
            logs_suffix(simulation.value.logs.as_deref())
        )));
    }

    Ok(SameMintBatchSimulation {
        preflight_chain_slot: Some(slot_to_i64(
            simulation.context.slot,
            BatchPhase::Simulation,
        )?),
    })
}

fn submit_signed_batch(
    rpc: &RpcClient,
    fee_payer: &Mutex<Keypair>,
    instructions: Vec<Instruction>,
) -> Result<SameMintBatchSubmission, SameMintRouteLoopError> {
    let transaction = signed_transaction(rpc, fee_payer, instructions, BatchPhase::Submission)?;
    let signature = rpc
        .send_and_confirm_transaction(&transaction)
        .map_err(|error| BatchPhase::Submission.error(format!("RPC submission failed: {error}")))?;
    let submitted_slot = rpc
        .get_slot()
        .ok()
        .and_then(|slot| i64::try_from(slot).ok());

    Ok(SameMintBatchSubmission {
        signature: signature.to_string(),
        submitted_slot,
    })
}

fn signed_transaction(
    rpc: &RpcClient,
    fee_payer: &Mutex<Keypair>,
    instructions: Vec<Instruction>,
    phase: BatchPhase,
) -> Result<Transaction, SameMintRouteLoopError> {
    let recent_blockhash = rpc
        .get_latest_blockhash()
        .map_err(|error| phase.error(format!("failed to fetch latest blockhash: {error}")))?;
    Ok(sign_transaction(
        fee_payer,
        instructions,
        recent_blockhash,
        phase,
    )?)
}

fn sign_transaction(
    fee_payer: &Mutex<Keypair>,
    instructions: Vec<Instruction>,
    recent_blockhash: Hash,
    phase: BatchPhase,
) -> Result<Transaction, SameMintRouteLoopError> {
    let fee_payer = fee_payer
        .lock()
        .map_err(|_| phase.error("fee payer keypair lock poisoned"))?;
    let signers: [&dyn Signer; 1] = [&*fee_payer];
    Ok(Transaction::new_signed_with_payer(
        &instructions,
        Some(&fee_payer.pubkey()),
        &signers,
        recent_blockhash,
    ))
}

fn collect_batch_instructions(
    routes: &[SameMintRouteExecution],
    phase: BatchPhase,
) -> Result<Vec<Instruction>, SameMintRouteLoopError> {
    if routes.is_empty() {
        return Err(phase.error("same-mint batch contains no routes"));
    }

    let mut instructions = Vec::new();
    for route in routes {
        if route.quote.route_instructions.is_empty() {
            return Err(phase.error(format!(
                "decision {} has no executable route instructions",
                route.decision_id
            )));
        }
        instructions.extend(route.quote.route_instructions.iter().cloned());
    }
    Ok(instructions)
}

fn slot_to_i64(slot: u64, phase: BatchPhase) -> Result<i64, SameMintRouteLoopError> {
    i64::try_from(slot).map_err(|_| phase.error(format!("slot {slot} does not fit i64")))
}

fn optional_bool_env(name: &'static str) -> Result<bool, MainnetSameMintExecutorConfigError> {
    match env::var(name) {
        Ok(value) => {
            parse_bool(&value).ok_or(MainnetSameMintExecutorConfigError::InvalidBool { name })
        }
        Err(env::VarError::NotPresent) => Ok(false),
        Err(env::VarError::NotUnicode(_)) => {
            Err(MainnetSameMintExecutorConfigError::InvalidEnv { name })
        }
    }
}

fn parse_bool(value: &str) -> Option<bool> {
    match value.trim().to_ascii_lowercase().as_str() {
        "1" | "true" | "yes" => Some(true),
        "0" | "false" | "no" => Some(false),
        _ => None,
    }
}

fn logs_suffix(logs: Option<&[String]>) -> String {
    let Some(logs) = logs else {
        return String::new();
    };
    if logs.is_empty() {
        String::new()
    } else {
        format!("; logs: {}", logs.join(" | "))
    }
}

#[derive(Clone, Copy)]
enum BatchPhase {
    Simulation,
    Submission,
}

impl BatchPhase {
    fn error(self, message: impl Into<String>) -> SameMintRouteLoopError {
        match self {
            Self::Simulation => SameMintRouteLoopError::simulation(message.into()),
            Self::Submission => SameMintRouteLoopError::submission(message.into()),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{DecisionId, VaultId};
    use std::sync::Mutex;

    #[derive(Default)]
    struct StaticPreparer {
        requests: Mutex<Vec<SameMintRouteQuoteRequest>>,
    }

    impl SameMintRoutePreparer for StaticPreparer {
        fn prepare_same_mint_route<'a>(
            &'a self,
            request: SameMintRouteQuoteRequest,
        ) -> SameMintLoopFuture<'a, SameMintRouteQuote> {
            Box::pin(async move {
                self.requests.lock().expect("requests lock").push(request);
                Ok(SameMintRouteQuote {
                    redeem_amount_raw: 100,
                    deposit_amount_raw: 99,
                    route_instructions: vec![test_instruction(9)],
                })
            })
        }
    }

    #[tokio::test]
    async fn quote_delegates_to_route_preparer() {
        let executor = executor(false, StaticPreparer::default());
        let quote = executor
            .quote_same_mint_route(SameMintRouteQuoteRequest {
                decision_id: DecisionId(7),
                vault_id: VaultId(3),
                source_reserve: "source".to_owned(),
                target_reserve: "target".to_owned(),
                liquidity_mint: "USDC".to_owned(),
                redeem_amount_raw: 100,
            })
            .await
            .expect("quote");

        assert_eq!(quote.deposit_amount_raw, 99);
        assert_eq!(quote.route_instructions, vec![test_instruction(9)]);
        assert_eq!(
            executor
                .preparer
                .requests
                .lock()
                .expect("requests lock")
                .as_slice(),
            &[SameMintRouteQuoteRequest {
                decision_id: DecisionId(7),
                vault_id: VaultId(3),
                source_reserve: "source".to_owned(),
                target_reserve: "target".to_owned(),
                liquidity_mint: "USDC".to_owned(),
                redeem_amount_raw: 100,
            }]
        );
    }

    #[tokio::test]
    async fn submit_fails_closed_when_disabled() {
        let executor = executor(false, StaticPreparer::default());
        let error = executor
            .submit_same_mint_batch(&[route_with_instructions(
                DecisionId(1),
                vec![test_instruction(1)],
            )])
            .await
            .unwrap_err();

        assert!(error
            .to_string()
            .contains("mainnet same-mint submission is disabled"));
    }

    #[test]
    fn batch_instruction_collection_rejects_missing_route_instructions() {
        let error = collect_batch_instructions(
            &[route_with_instructions(DecisionId(4), Vec::new())],
            BatchPhase::Simulation,
        )
        .unwrap_err();

        assert_eq!(
            error.to_string(),
            "decision 4 has no executable route instructions"
        );
    }

    #[test]
    fn signs_batch_with_all_route_instructions() {
        let fee_payer = Mutex::new(Keypair::new());
        let transaction = sign_transaction(
            &fee_payer,
            vec![test_instruction(1), test_instruction(2)],
            Hash::new_unique(),
            BatchPhase::Simulation,
        )
        .expect("signed transaction");

        assert_eq!(transaction.message.instructions.len(), 2);
        assert_eq!(transaction.signatures.len(), 1);
    }

    #[test]
    fn bool_env_parser_accepts_expected_values() {
        assert_eq!(parse_bool("true"), Some(true));
        assert_eq!(parse_bool("1"), Some(true));
        assert_eq!(parse_bool("no"), Some(false));
        assert_eq!(parse_bool("wat"), None);
    }

    fn executor<P>(submit: bool, preparer: P) -> MainnetSameMintExecutor<P> {
        MainnetSameMintExecutor::with_rpc_client(
            Arc::new(RpcClient::new("http://127.0.0.1:8899".to_owned())),
            Keypair::new(),
            preparer,
            submit,
        )
    }

    fn route_with_instructions(
        decision_id: DecisionId,
        route_instructions: Vec<Instruction>,
    ) -> SameMintRouteExecution {
        SameMintRouteExecution {
            decision_id,
            vault_id: VaultId(1),
            source_reserve: "source".to_owned(),
            target_reserve: "target".to_owned(),
            liquidity_mint: "USDC".to_owned(),
            quote: SameMintRouteQuote {
                redeem_amount_raw: 100,
                deposit_amount_raw: 99,
                route_instructions,
            },
        }
    }

    fn test_instruction(tag: u8) -> Instruction {
        Instruction {
            program_id: Pubkey::new_from_array([tag; 32]),
            accounts: Vec::new(),
            data: vec![tag],
        }
    }
}

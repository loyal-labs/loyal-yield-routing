use std::{env, str::FromStr};

use loyal_yield_orchestrator::{
    keypair_from_hex, RpcAdapter, RpcAdapterConfig, SameMintKaminoRouteAccounts,
    SameMintPolicyExecutionRequest, SameMintPolicyRoute, SimulationWorker,
    YIELD_ROUTER_KEYPAIR_ENV,
};
use serde::Deserialize;
use solana_sdk::{
    commitment_config::CommitmentConfig,
    pubkey::Pubkey,
    signature::{Keypair, Signer},
    transaction::{Transaction, VersionedTransaction},
};

const RPC_URL_ENV: &str = "LOYAL_YIELD_PREPROD_RPC_URL";
const FIXTURE_ENV: &str = "LOYAL_YIELD_PREPROD_SAME_MINT_FIXTURE";

#[derive(Debug, Deserialize)]
struct PreproductionFixture {
    policy_account: String,
    instruction_constraint_indexes: [u8; 2],
    vault_index: u8,
    vault_owner: String,
    vault_liquidity_token_account: String,
    source_collateral_token_account: String,
    target_collateral_token_account: String,
    source_reserve: ReserveFixture,
    target_reserve: ReserveFixture,
    amount_raw: u64,
}

#[derive(Debug, Deserialize)]
struct ReserveFixture {
    reserve: String,
    lending_market: String,
    lending_market_authority: String,
    liquidity_mint: String,
    liquidity_supply: String,
    collateral_mint: String,
}

#[test]
#[ignore = "requires live preproduction accounts, RPC URL, and YIELD_ROUTER_KEYPAIR"]
fn preproduction_same_mint_policy_execution_simulates_on_rpc() {
    let rpc_url = env::var(RPC_URL_ENV).unwrap_or_else(|_| {
        panic!("{RPC_URL_ENV} must point at the Solana RPC endpoint for this preproduction test")
    });
    let fixture_json = env::var(FIXTURE_ENV)
        .unwrap_or_else(|_| panic!("{FIXTURE_ENV} must contain the same-mint route fixture JSON"));
    let signer = keypair_from_env();
    let fixture: PreproductionFixture =
        serde_json::from_str(&fixture_json).expect("fixture JSON must match preproduction schema");

    let instruction =
        SimulationWorker::build_same_mint_policy_execution(SameMintPolicyExecutionRequest {
            route: SameMintPolicyRoute {
                action_account: pubkey(&fixture.policy_account),
                instruction_constraint_indexes: fixture.instruction_constraint_indexes,
            },
            signer: signer.pubkey(),
            vault_index: fixture.vault_index,
            accounts: SameMintKaminoRouteAccounts {
                source_reserve: fixture.source_reserve.accounts(),
                target_reserve: fixture.target_reserve.accounts(),
                vault_owner: pubkey(&fixture.vault_owner),
                vault_liquidity_token_account: pubkey(&fixture.vault_liquidity_token_account),
                source_collateral_token_account: pubkey(&fixture.source_collateral_token_account),
                target_collateral_token_account: pubkey(&fixture.target_collateral_token_account),
            },
            amount: fixture.amount_raw,
        })
        .expect("preproduction fixture should build a policy execution instruction");

    let rpc = RpcAdapter::new(RpcAdapterConfig {
        url: rpc_url,
        commitment: CommitmentConfig::confirmed(),
        skip_preflight: true,
        max_retries: Some(0),
    });
    let blockhash = rpc.latest_blockhash().expect("fetch latest blockhash");
    let transaction = Transaction::new_signed_with_payer(
        &[instruction],
        Some(&signer.pubkey()),
        &[&signer],
        blockhash.blockhash,
    );
    let transaction = VersionedTransaction::from(transaction);
    let report = rpc
        .simulate_transaction(&transaction)
        .expect("preproduction simulation RPC call");

    assert!(
        report.error.is_none(),
        "preproduction same-mint route simulation failed: {:?}\nlogs:\n{}",
        report.error,
        report.logs.join("\n")
    );
}

impl ReserveFixture {
    fn accounts(&self) -> loyal_yield_orchestrator::KaminoReserveAccounts {
        loyal_yield_orchestrator::KaminoReserveAccounts {
            reserve: pubkey(&self.reserve),
            lending_market: pubkey(&self.lending_market),
            lending_market_authority: pubkey(&self.lending_market_authority),
            liquidity_mint: pubkey(&self.liquidity_mint),
            liquidity_supply: pubkey(&self.liquidity_supply),
            collateral_mint: pubkey(&self.collateral_mint),
        }
    }
}

fn keypair_from_env() -> Keypair {
    let value = env::var(YIELD_ROUTER_KEYPAIR_ENV).unwrap_or_else(|_| {
        panic!("{YIELD_ROUTER_KEYPAIR_ENV} must contain the delegated signer hex keypair")
    });
    keypair_from_hex(&value).expect("delegated signer keypair must be valid hex")
}

fn pubkey(value: &str) -> Pubkey {
    Pubkey::from_str(value).expect("fixture pubkey must be valid")
}

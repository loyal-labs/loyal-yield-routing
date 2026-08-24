use std::{fs, path::PathBuf, str::FromStr};

use balance_sweep_ata_monitor::smart_account::EARN_OBLIGATIONS;
use balance_sweep_ata_monitor::{
    enqueue_normalized_earn_update, normalize_laserstream_update,
    process_next_earn_reconciliation_job_with_policy_monitor, subscribe_request_json,
    EarnReconciliationProcessOutcome, RpcEarnChainReader, SubscriptionWatchSet,
    EARN_SMART_ACCOUNTS,
};
use clap::Parser;
use helius_laserstream::grpc::{
    subscribe_update::UpdateOneof, SubscribeUpdate, SubscribeUpdateAccount,
    SubscribeUpdateAccountInfo,
};
use loyal_squads_policy_monitor::{
    Cluster, Commitment, MonitorConfig, PolicyMonitor, PostgresPolicyMatchSink,
};
use loyal_yield_store::{EarnSubscriptionTarget, OrchestratorConfig, OrchestratorStore};
use serde::{Deserialize, Serialize};
use solana_sdk::{pubkey::Pubkey, signature::Signature};
use tokio::sync::Mutex;

#[derive(Debug, Parser)]
struct Args {
    #[arg(long)]
    postgres_url: String,
    #[arg(long)]
    rpc_url: String,
    #[arg(long)]
    state: PathBuf,
    #[arg(long)]
    transaction: PathBuf,
    #[arg(long)]
    subscribe_request_output: Option<PathBuf>,
    #[arg(long)]
    projected_earn_state_output: Option<PathBuf>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct LocalState {
    market: String,
    obligation: String,
    settings_pda: String,
    vault_pubkey: String,
    wallet_address: String,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "snake_case")]
enum Stage {
    RoutePolicy,
    SetupPolicy,
    InitialDeposit,
    TopUp,
    PartialWithdrawal,
    FullWithdrawal,
}

#[derive(Debug, Deserialize)]
struct ChainTransaction {
    signature: String,
    slot: u64,
    stage: Stage,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct ProjectedPolicyIdentity<'a> {
    account: &'a str,
    seed: String,
    setup_policy: ProjectedSetupPolicyIdentity<'a>,
}

#[derive(Serialize)]
struct ProjectedSetupPolicyIdentity<'a> {
    account: &'a str,
    seed: String,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct ProjectedEarnState<'a> {
    settings_pda: &'a str,
    policy: ProjectedPolicyIdentity<'a>,
}

fn initial_watch_set(state: &LocalState) -> anyhow::Result<SubscriptionWatchSet> {
    SubscriptionWatchSet::from_targets(
        Vec::new(),
        vec![EarnSubscriptionTarget {
            environment: "mainnet-beta".to_owned(),
            settings: state.settings_pda.clone(),
            wallet: state.wallet_address.clone(),
            earn_max: false,
            vault_index: 1,
            vault_pubkey: Some(state.vault_pubkey.clone()),
            policy_accounts: Vec::new(),
            markets: vec![state.market.clone()],
            autodeposit_accounts: Vec::new(),
            observation_start_slot: None,
        }],
    )
}

fn emulated_update(
    state: &LocalState,
    transaction: &ChainTransaction,
) -> anyhow::Result<SubscribeUpdate> {
    let (filter, account_pubkey) = match transaction.stage {
        Stage::RoutePolicy | Stage::SetupPolicy => {
            (EARN_SMART_ACCOUNTS, state.settings_pda.as_str())
        }
        Stage::InitialDeposit | Stage::TopUp | Stage::PartialWithdrawal | Stage::FullWithdrawal => {
            (EARN_OBLIGATIONS, state.obligation.as_str())
        }
    };
    Ok(SubscribeUpdate {
        filters: vec![filter.to_owned()],
        created_at: None,
        update_oneof: Some(UpdateOneof::Account(SubscribeUpdateAccount {
            account: Some(SubscribeUpdateAccountInfo {
                pubkey: Pubkey::from_str(account_pubkey)?.to_bytes().to_vec(),
                lamports: 1,
                owner: Vec::new(),
                executable: false,
                rent_epoch: 0,
                data: Vec::new(),
                write_version: 1,
                txn_signature: Some(
                    Signature::from_str(&transaction.signature)?
                        .as_ref()
                        .to_vec(),
                ),
            }),
            slot: transaction.slot,
            is_startup: false,
        })),
    })
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let args = Args::parse();
    let state: LocalState = serde_json::from_str(&fs::read_to_string(&args.state)?)?;
    let transaction: ChainTransaction =
        serde_json::from_str(&fs::read_to_string(&args.transaction)?)?;
    let store = OrchestratorStore::connect(OrchestratorConfig::new(args.postgres_url)).await?;
    let monitor = Mutex::new(PolicyMonitor::new(
        MonitorConfig {
            cluster: Cluster::Mainnet,
            commitment: Commitment::Finalized,
            ws_url: "local-client-earn-emulator".to_owned(),
        },
        PostgresPolicyMatchSink::from_store(store.clone()),
    ));
    let chain = RpcEarnChainReader::new(args.rpc_url, store.clone());
    let consumer_name = "ask-2212-local-client-earn";
    let claim_owner = "ask-2212-local-client-earn-e2e";
    let targets = store.load_earn_subscription_targets("mainnet-beta").await?;
    let watch_set = if targets.is_empty() {
        initial_watch_set(&state)?
    } else {
        SubscriptionWatchSet::from_targets(Vec::new(), targets)?
    };
    let update = normalize_laserstream_update(emulated_update(&state, &transaction)?)?
        .ok_or_else(|| anyhow::anyhow!("emulated LaserStream account update was ignored"))?;
    enqueue_normalized_earn_update(&store, consumer_name, &update, &watch_set).await?;
    let outcome = process_next_earn_reconciliation_job_with_policy_monitor(
        &store,
        consumer_name,
        claim_owner,
        &chain,
        Some(&monitor),
        120,
        1,
    )
    .await?;
    if !matches!(outcome, EarnReconciliationProcessOutcome::Completed { .. }) {
        anyhow::bail!("client Earn reconciliation did not complete: {outcome:?}");
    }
    let context = store
        .load_earn_reconciliation_context(&state.settings_pda, 1, &state.vault_pubkey)
        .await?;
    println!(
        "projected {:?}: route_policy={}, setup_policy={}",
        transaction.stage,
        context.route_policy.is_some(),
        context.setup_policy.is_some()
    );

    if let Some(output) = args.projected_earn_state_output {
        let route = context
            .route_policy
            .as_ref()
            .ok_or_else(|| anyhow::anyhow!("projected route policy is missing"))?;
        let setup = context
            .setup_policy
            .as_ref()
            .ok_or_else(|| anyhow::anyhow!("projected setup policy is missing"))?;
        let projected = ProjectedEarnState {
            settings_pda: &state.settings_pda,
            policy: ProjectedPolicyIdentity {
                account: &route.policy_account,
                seed: route.policy_seed.to_string(),
                setup_policy: ProjectedSetupPolicyIdentity {
                    account: &setup.policy_account,
                    seed: setup.policy_seed.to_string(),
                },
            },
        };
        fs::write(output, serde_json::to_vec_pretty(&projected)?)?;
    }

    if let Some(output) = args.subscribe_request_output {
        let refreshed = store.load_earn_subscription_targets("mainnet-beta").await?;
        let request =
            subscribe_request_json(&SubscriptionWatchSet::from_targets(Vec::new(), refreshed)?);
        fs::write(output, serde_json::to_vec_pretty(&request)?)?;
    }
    Ok(())
}

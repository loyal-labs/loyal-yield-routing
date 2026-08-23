use loyal_fleet_worker::multiply::{run, view::route_view, WorkerRuntime};
use loyal_observability::init_from_env;
use loyal_yield_store::fleet_orchestration::StrategyKey;
use loyal_yield_store::{NeonSqlClient, NeonSqlConfig};
use solana_client::nonblocking::rpc_client::RpcClient;
use solana_sdk::{
    commitment_config::CommitmentConfig,
    pubkey::Pubkey,
    signature::{Keypair, Signature},
};
use std::{env, error::Error, process, str::FromStr, time::Duration};

enum Command {
    Run,
    Deposit,
    Move,
    Withdraw,
    Claim,
    Status,
    RoleProbe,
}

struct Options {
    command: Command,
    route_key: Option<String>,
    request_id: Option<String>,
    signature: Option<String>,
    wallet_account: Option<String>,
    destination_account: Option<String>,
    amount_raw: Option<u64>,
    strategy: Option<StrategyKey>,
}

#[tokio::main]
async fn main() {
    let result = async {
        let options = parse_options()?;
        if matches!(options.command, Command::RoleProbe) {
            println!(
                "{}",
                serde_json::json!({
                    "schemaVersion": 1,
                    "event": "fleet_worker_role_probe",
                    "status": "pass",
                    "role": "multiply_route_worker",
                    "networkAccessed": false,
                    "secretsLoaded": false,
                    "databaseMutated": false,
                    "transactionSent": false
                })
            );
            return Ok(());
        }
        let _observability = init_from_env("multiply-route-worker");
        let runtime = runtime().await?;
        match options.command {
            Command::Run => run(&runtime, options.route_key.as_deref()).await,
            Command::Deposit => {
                let result = runtime
                    .admit_confirmed_deposit(
                        required_option(&options.route_key, "--route")?,
                        required_option(&options.request_id, "--request-id")?.to_owned(),
                        Signature::from_str(required_option(&options.signature, "--signature")?)?,
                        Pubkey::from_str(required_option(
                            &options.wallet_account,
                            "--wallet-account",
                        )?)?,
                        options.strategy.ok_or("deposit requires --strategy")?,
                    )
                    .await?;
                println!("{}", serde_json::to_string(&result)?);
                Ok(())
            }
            Command::Move => {
                let result = runtime
                    .request_move(
                        required_option(&options.route_key, "--route")?,
                        options.strategy.ok_or("move requires --strategy")?,
                    )
                    .await?;
                println!("{}", serde_json::to_string(&result)?);
                Ok(())
            }
            Command::Withdraw => {
                let result = runtime
                    .request_withdrawal(
                        required_option(&options.route_key, "--route")?,
                        required_option(&options.request_id, "--request-id")?.to_owned(),
                        required_option(&options.destination_account, "--destination-account")?
                            .to_owned(),
                        options.amount_raw.ok_or("withdraw requires --amount-raw")?,
                    )
                    .await?;
                println!("{}", serde_json::to_string(&result)?);
                Ok(())
            }
            Command::Claim => {
                let result = runtime
                    .tick(Some(required_option(&options.route_key, "--route")?))
                    .await?;
                println!("{}", serde_json::to_string(&result)?);
                Ok(())
            }
            Command::Status => {
                let route_key = options
                    .route_key
                    .as_deref()
                    .ok_or("status requires --route")?;
                let stored = runtime
                    .store
                    .load_multiply_route_state(route_key)
                    .await?
                    .ok_or("route not found")?;
                println!(
                    "{}",
                    serde_json::to_string_pretty(&route_view(&stored.state))?
                );
                Ok(())
            }
            Command::RoleProbe => unreachable!("role probe exits before runtime construction"),
        }
    }
    .await;
    if let Err(error) = result {
        eprintln!(
            "{}",
            serde_json::json!({"condition":"multiply_worker_failed","error":safe_error(error.as_ref())})
        );
        process::exit(2);
    }
}

async fn runtime() -> Result<WorkerRuntime, Box<dyn Error>> {
    let rpc_url = required_env("SOLANA_RPC_URL")?;
    let database_url = required_env("NEON_DATABASE_URL")?;
    let delegate = loyal_yield_orchestrator::policy_keypair_from_env()?;
    let delegate_bytes = delegate.to_bytes();
    let fee_payer = Keypair::try_from(delegate_bytes.as_slice())?;
    let store = NeonSqlClient::connect(
        NeonSqlConfig::new(database_url)
            .with_max_connections(2)
            .with_acquire_timeout(Duration::from_secs(10)),
    )
    .await?;
    Ok(WorkerRuntime {
        store,
        rpc: RpcClient::new_with_timeout_and_commitment(
            rpc_url,
            Duration::from_secs(20),
            CommitmentConfig::confirmed(),
        ),
        fee_payer,
        delegate,
        worker_id: format!("multiply:{}", process::id()),
    })
}

fn parse_options() -> Result<Options, Box<dyn Error>> {
    let mut args = env::args().skip(1);
    let command = match args.next().as_deref() {
        None | Some("run") => Command::Run,
        Some("deposit") => Command::Deposit,
        Some("move") => Command::Move,
        Some("withdraw") => Command::Withdraw,
        Some("claim") => Command::Claim,
        Some("status") => Command::Status,
        Some("--role-probe") => Command::RoleProbe,
        Some("help" | "--help" | "-h") => {
            println!("multiply-route-worker [run|deposit|move|withdraw|claim|status|--role-probe] [options]");
            process::exit(0);
        }
        Some(value) => return Err(format!("unknown command {value}").into()),
    };
    let mut route_key = None;
    let mut request_id = None;
    let mut signature = None;
    let mut wallet_account = None;
    let mut destination_account = None;
    let mut amount_raw = None;
    let mut strategy = None;
    while let Some(flag) = args.next() {
        match flag.as_str() {
            "--route" => route_key = Some(args.next().ok_or("--route requires a value")?),
            "--request-id" => {
                request_id = Some(args.next().ok_or("--request-id requires a value")?)
            }
            "--signature" => signature = Some(args.next().ok_or("--signature requires a value")?),
            "--wallet-account" => {
                wallet_account = Some(args.next().ok_or("--wallet-account requires a value")?)
            }
            "--destination-account" => {
                destination_account = Some(
                    args.next()
                        .ok_or("--destination-account requires a value")?,
                )
            }
            "--amount-raw" => {
                amount_raw = Some(
                    args.next()
                        .ok_or("--amount-raw requires a value")?
                        .parse()?,
                )
            }
            "--strategy" => {
                strategy = Some(parse_strategy(
                    &args.next().ok_or("--strategy requires a value")?,
                )?)
            }
            _ => return Err(format!("unknown option {flag}").into()),
        }
    }
    Ok(Options {
        command,
        route_key,
        request_id,
        signature,
        wallet_account,
        destination_account,
        amount_raw,
        strategy,
    })
}

fn parse_strategy(value: &str) -> Result<StrategyKey, Box<dyn Error>> {
    match value {
        "syrup_usdc_usdc" => Ok(StrategyKey::SyrupUsdcUsdc),
        "syrup_usdc_pyusd" => Ok(StrategyKey::SyrupUsdcPyusd),
        _ => Err("strategy must be syrup_usdc_usdc or syrup_usdc_pyusd".into()),
    }
}

fn required_option<'a>(value: &'a Option<String>, flag: &str) -> Result<&'a str, Box<dyn Error>> {
    value
        .as_deref()
        .ok_or_else(|| format!("{flag} is required").into())
}

fn required_env(name: &str) -> Result<String, Box<dyn Error>> {
    env::var(name)
        .ok()
        .filter(|value| !value.trim().is_empty())
        .ok_or_else(|| format!("{name} is required").into())
}

fn safe_error(error: &dyn Error) -> String {
    let message = error.to_string();
    if message.contains("postgres") || message.contains("http") || message.contains("keypair") {
        "external dependency failed; inspect terminal logs".to_owned()
    } else {
        message
    }
}

use loyal_fleet_worker::multiply::{run, view::route_view, WorkerRuntime};
use loyal_observability::init_from_env;
use loyal_yield_store::{NeonSqlClient, NeonSqlConfig};
use solana_client::nonblocking::rpc_client::RpcClient;
use solana_sdk::{commitment_config::CommitmentConfig, signature::Keypair};
use std::{env, error::Error, process, time::Duration};

enum Command {
    Run,
    Status,
    RoleProbe,
}

struct Options {
    command: Command,
    route_key: Option<String>,
    owner: String,
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
        let runtime = runtime(options.owner).await?;
        match options.command {
            Command::Run => run(&runtime, options.route_key.as_deref()).await,
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

async fn runtime(owner: String) -> Result<WorkerRuntime, Box<dyn Error>> {
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
        worker_id: owner,
    })
}

fn parse_options() -> Result<Options, Box<dyn Error>> {
    parse_options_from(env::args().skip(1), env::var("MULTIPLY_WORKER_OWNER").ok())
}

fn parse_options_from(
    mut args: impl Iterator<Item = String>,
    env_owner: Option<String>,
) -> Result<Options, Box<dyn Error>> {
    let command = match args.next().as_deref() {
        None | Some("run") => Command::Run,
        Some("status") => Command::Status,
        Some("--role-probe") => Command::RoleProbe,
        Some("help" | "--help" | "-h") => {
            println!("multiply-route-worker [run|status|--role-probe] [--route ROUTE] [--owner OWNER] (env: MULTIPLY_WORKER_OWNER)");
            process::exit(0);
        }
        Some(value) => return Err(format!("unknown command {value}").into()),
    };
    let mut route_key = None;
    let mut owner = env_owner;
    while let Some(flag) = args.next() {
        match flag.as_str() {
            "--owner" => owner = Some(args.next().ok_or("--owner requires a value")?),
            "--route" => route_key = Some(args.next().ok_or("--route requires a value")?),
            _ => return Err(format!("unknown option {flag}").into()),
        }
    }
    let owner = owner.unwrap_or_else(|| format!("multiply:{}", process::id()));
    if owner.is_empty()
        || owner.len() > 256
        || !owner
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || b"_.:-".contains(&b))
    {
        return Err("owner must be 1-256 ASCII letters, digits, or _.:-".into());
    }
    Ok(Options {
        command,
        route_key,
        owner,
    })
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

#[cfg(test)]
mod tests {
    use super::*;

    fn options(args: &[&str], owner: Option<&str>) -> Result<Options, Box<dyn Error>> {
        parse_options_from(args.iter().map(|s| s.to_string()), owner.map(str::to_owned))
    }

    #[test]
    fn deployment_owner_cli_and_environment_contract() {
        let legacy = options(&["run"], None).unwrap();
        assert_eq!(legacy.owner, format!("multiply:{}", process::id()));
        let a = options(&["run"], Some("multiply:production:host-a-release-001")).unwrap();
        assert_eq!(a.owner, "multiply:production:host-a-release-001");
        let b = options(
            &["run", "--owner", "multiply:production:host-b-release-002"],
            Some(&a.owner),
        )
        .unwrap();
        assert_ne!(a.owner, b.owner);
        assert_eq!(b.owner, "multiply:production:host-b-release-002");
        for invalid in ["", " ", "host/a", "host\nother"] {
            assert!(options(&["run", "--owner", invalid], None).is_err());
            assert!(options(&["run"], Some(invalid)).is_err());
        }
        assert!(options(&["run", "--owner"], None).is_err());
        assert!(options(&["run"], Some(&"a".repeat(257))).is_err());
    }
}

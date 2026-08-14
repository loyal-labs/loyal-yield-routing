use std::{env, error::Error, time::Duration};

use chrono::{Duration as ChronoDuration, Utc};
use loyal_observability::init_from_env;
use loyal_yield_orchestrator::{NeonSqlClient, NeonSqlConfig};
use serde_json::json;

const DEFAULT_REFRESH_INTERVAL_SECONDS: u64 = 5;
const DEFAULT_LEASE_SECONDS: i64 = 15;

#[derive(Debug, Clone, PartialEq, Eq)]
struct Options {
    database_url: String,
    cluster: String,
    owner: String,
    refresh_interval_seconds: u64,
    lease_seconds: i64,
    once: bool,
}

fn parse_args_with_env(
    args: impl IntoIterator<Item = String>,
    read_env: impl Fn(&str) -> Option<String>,
) -> Result<Options, Box<dyn Error>> {
    let mut args = args.into_iter();
    let mut once = false;
    let mut cluster = read_env("YIELD_ROUTE_CLUSTER")
        .or_else(|| read_env("YIELD_ALT_CLUSTER"))
        .unwrap_or_else(|| "mainnet-beta".to_owned());
    let mut refresh_interval_seconds = DEFAULT_REFRESH_INTERVAL_SECONDS;
    let mut lease_seconds = DEFAULT_LEASE_SECONDS;
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--once" => once = true,
            "--cluster" => cluster = args.next().ok_or("--cluster requires a value")?,
            "--refresh-interval-seconds" => {
                refresh_interval_seconds = args
                    .next()
                    .ok_or("--refresh-interval-seconds requires a value")?
                    .parse()?;
            }
            "--lease-seconds" => {
                lease_seconds = args
                    .next()
                    .ok_or("--lease-seconds requires a value")?
                    .parse()?;
            }
            "--help" | "-h" => {
                return Err("fleet-health-projector [--once] [--cluster NAME] [--refresh-interval-seconds N] [--lease-seconds N]".into());
            }
            value => return Err(format!("unknown argument {value}").into()),
        }
    }
    if cluster.trim().is_empty()
        || refresh_interval_seconds == 0
        || !(3..=300).contains(&lease_seconds)
        || i64::try_from(refresh_interval_seconds).unwrap_or(i64::MAX) >= lease_seconds
    {
        return Err("projector requires a cluster, refresh interval > 0, lease in 3..=300 seconds, and lease longer than refresh interval".into());
    }
    let database_url = read_env("NEON_DATABASE_URL")
        .ok_or("NEON_DATABASE_URL must be set for fleet health projection")?;
    let owner = format!("fleet-health-projector:{cluster}:{}", std::process::id());
    Ok(Options {
        database_url,
        cluster,
        owner,
        refresh_interval_seconds,
        lease_seconds,
        once,
    })
}

async fn run() -> Result<(), Box<dyn Error>> {
    let options = parse_args_with_env(env::args().skip(1), |key| env::var(key).ok())?;
    let client = NeonSqlClient::connect(
        NeonSqlConfig::new(options.database_url.clone()).with_max_connections(4),
    )
    .await?;
    client
        .require_schema_migration(34, "fleet_health_snapshot_projection")
        .await?;
    loop {
        let lease = client
            .claim_fleet_health_projection_lease(
                &options.cluster,
                &options.owner,
                Utc::now() + ChronoDuration::seconds(options.lease_seconds),
            )
            .await?;
        match lease {
            Some(lease) => {
                let refresh = client
                    .refresh_fleet_orchestration_health_snapshot(&lease)
                    .await?;
                let cached = client.fleet_orchestration_status(&options.cluster).await?;
                if serde_json::to_value(&cached)? != serde_json::to_value(&refresh.status)? {
                    return Err(
                        "cached fleet health payload differs from its source refresh".into(),
                    );
                }
                println!(
                    "{}",
                    json!({
                        "status": "fleet_health_snapshot_refreshed",
                        "cluster": refresh.cluster,
                        "owner": refresh.refresh_owner,
                        "fencingToken": refresh.fencing_token,
                        "rowCount": refresh.status.len(),
                        "sourceWatermark": refresh.source_watermark,
                        "refreshDurationMilliseconds": refresh.refresh_duration_milliseconds,
                        "refreshedAt": refresh.refreshed_at,
                        "nextRefreshAt": refresh.refreshed_at
                            + ChronoDuration::seconds(
                                i64::try_from(options.refresh_interval_seconds)
                                    .unwrap_or(i64::MAX),
                            ),
                    })
                );
            }
            None => println!(
                "{}",
                json!({
                    "status": "fleet_health_snapshot_refresh_skipped",
                    "cluster": options.cluster,
                    "reason": "another_projector_holds_live_lease",
                })
            ),
        }
        if options.once {
            return Ok(());
        }
        tokio::time::sleep(Duration::from_secs(options.refresh_interval_seconds)).await;
    }
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn Error>> {
    let _observability = init_from_env("loyal-fleet-health-projector")?;
    run().await
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeMap;

    fn options(args: &[&str]) -> Result<Options, Box<dyn Error>> {
        let values = BTreeMap::from([
            ("NEON_DATABASE_URL", "postgresql://127.0.0.1/test"),
            ("YIELD_ROUTE_CLUSTER", "localnet"),
        ]);
        parse_args_with_env(args.iter().map(|value| (*value).to_owned()), |key| {
            values.get(key).map(|value| (*value).to_owned())
        })
    }

    #[test]
    fn defaults_keep_lease_longer_than_refresh_interval() {
        let options = options(&[]).unwrap();
        assert_eq!(options.cluster, "localnet");
        assert_eq!(options.refresh_interval_seconds, 5);
        assert_eq!(options.lease_seconds, 15);
    }

    #[test]
    fn rejects_refresh_interval_that_can_outlive_lease() {
        assert!(options(&["--refresh-interval-seconds", "15", "--lease-seconds", "15"]).is_err());
    }
}

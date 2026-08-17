use std::{env, error::Error, time::Duration};

use chrono::Duration as ChronoDuration;
use loyal_observability::init_from_env;
use loyal_yield_orchestrator::{
    fleet_orchestration::FleetHealthSnapshotProjection, NeonSqlClient, NeonSqlConfig,
};
use serde_json::json;

const DEFAULT_REFRESH_INTERVAL_SECONDS: u64 = 5;

#[derive(Debug, Clone, PartialEq, Eq)]
struct Options {
    database_url: String,
    cluster: String,
    refresh_interval_seconds: u64,
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
            "--help" | "-h" => {
                return Err("fleet-health-projector [--once] [--cluster NAME] [--refresh-interval-seconds N]".into());
            }
            value => return Err(format!("unknown argument {value}").into()),
        }
    }
    if cluster.trim().is_empty() || !(1..=300).contains(&refresh_interval_seconds) {
        return Err("projector requires a cluster and refresh interval in 1..=300 seconds".into());
    }
    let database_url = read_env("NEON_DATABASE_URL")
        .ok_or("NEON_DATABASE_URL must be set for fleet health projection")?;
    Ok(Options {
        database_url,
        cluster,
        refresh_interval_seconds,
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
        let projection = client
            .project_fleet_orchestration_health_snapshot(
                &options.cluster,
                ChronoDuration::seconds(options.refresh_interval_seconds as i64),
            )
            .await?;
        match projection {
            FleetHealthSnapshotProjection::Published(refresh) => {
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
            FleetHealthSnapshotProjection::Busy => println!(
                "{}",
                json!({
                    "status": "fleet_health_snapshot_refresh_skipped",
                    "cluster": options.cluster,
                    "reason": "another_projector_is_refreshing",
                })
            ),
            FleetHealthSnapshotProjection::NotDue { refreshed_at } => println!(
                "{}",
                json!({
                    "status": "fleet_health_snapshot_refresh_skipped",
                    "cluster": options.cluster,
                    "reason": "snapshot_is_fresh",
                    "refreshedAt": refreshed_at,
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
    fn defaults_to_five_second_refresh_interval() {
        let options = options(&[]).unwrap();
        assert_eq!(options.cluster, "localnet");
        assert_eq!(options.refresh_interval_seconds, 5);
    }

    #[test]
    fn rejects_zero_refresh_interval() {
        assert!(options(&["--refresh-interval-seconds", "0"]).is_err());
        assert!(options(&["--refresh-interval-seconds", "301"]).is_err());
    }

    #[test]
    fn rejects_removed_lease_option() {
        assert!(options(&["--lease-seconds", "15"]).is_err());
    }
}

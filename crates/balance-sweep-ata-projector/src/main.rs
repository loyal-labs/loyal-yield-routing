use std::time::Duration;

use anyhow::Result;
use balance_sweep_ata_observations::{
    observation_to_wallet_balance_update, BalanceSweepAtaObservationEvent, TimescaleAtaConfig,
    TimescaleAtaObservationSink, TimescaleAtaStream,
};
use clap::Parser;
use loyal_yield_orchestrator::{
    OrchestratorConfig, OrchestratorError, OrchestratorStore, ProjectedWalletAtaBalanceUpdateInput,
};
use tokio::time;

const CONSUMER_NAME: &str = "balance_sweep_ata_projector";

#[derive(Debug, Parser)]
#[command(about = "Project raw Loyal wallet ATA observations from Timescale into Yield Neon")]
struct Args {
    #[arg(long, env = "TIMESCALEDB_URL")]
    timescaledb_url: String,
    #[arg(long, env = "BALANCE_SWEEP_ATA_STREAM", default_value = "production")]
    ata_stream: TimescaleAtaStream,
    #[arg(long, env = "NEON_DATABASE_URL")]
    postgres_url: String,
    #[arg(long, default_value_t = 1000)]
    batch_limit: i64,
    #[arg(long, default_value_t = 10)]
    poll_interval_seconds: u64,
    #[arg(long)]
    once: bool,
    #[arg(long)]
    from_event_id: Option<i64>,
    #[arg(long)]
    advance_cursor: bool,
}

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();
    let args = Args::parse();
    let timescale = TimescaleAtaObservationSink::connect(
        TimescaleAtaConfig::new(args.timescaledb_url).with_stream(args.ata_stream),
    )
    .await?;
    let store = OrchestratorStore::connect(OrchestratorConfig::new(args.postgres_url)).await?;
    let consumer_name = consumer_name(args.ata_stream);
    tracing::info!(
        ata_stream = %args.ata_stream,
        consumer_name,
        "starting balance sweep ATA projector"
    );

    loop {
        let outcome = if let Some(from_event_id) = args.from_event_id {
            project_repair_once(
                &timescale,
                &store,
                &consumer_name,
                from_event_id,
                args.batch_limit,
                args.advance_cursor,
            )
            .await?
        } else {
            project_cursor_once(&timescale, &store, &consumer_name, args.batch_limit).await?
        };
        tracing::info!(
            projected = outcome.projected,
            previous_event_id = outcome.previous_event_id,
            last_event_id = outcome.last_event_id,
            "projected wallet ATA observations"
        );
        if args.once {
            return Ok(());
        }
        time::sleep(Duration::from_secs(args.poll_interval_seconds)).await;
    }
}

fn consumer_name(stream: TimescaleAtaStream) -> String {
    format!("{CONSUMER_NAME}:{}", stream.as_str())
}

#[derive(Debug, Clone, Copy)]
struct ProjectorOutcome {
    projected: usize,
    previous_event_id: i64,
    last_event_id: i64,
}

async fn project_cursor_once(
    timescale: &TimescaleAtaObservationSink,
    store: &OrchestratorStore,
    consumer_name: &str,
    batch_limit: i64,
) -> Result<ProjectorOutcome> {
    let outcome = store
        .project_wallet_ata_balance_updates(
            consumer_name,
            batch_limit,
            |last_event_id, limit| async move {
                let rows = timescale
                    .observations_after_event_id(last_event_id, limit)
                    .await
                    .map_err(|error| {
                        OrchestratorError::StoreInvariant(format!(
                            "fetch Timescale ATA observations: {error}"
                        ))
                    })?;
                Ok(rows.into_iter().map(projected_update_from_event).collect())
            },
        )
        .await?;

    Ok(ProjectorOutcome {
        projected: outcome.projected_count,
        previous_event_id: outcome.previous_event_id,
        last_event_id: outcome.last_event_id,
    })
}

async fn project_repair_once(
    timescale: &TimescaleAtaObservationSink,
    store: &OrchestratorStore,
    consumer_name: &str,
    from_event_id: i64,
    batch_limit: i64,
    advance_cursor: bool,
) -> Result<ProjectorOutcome> {
    let previous_event_id = store.projection_offset(consumer_name).await?;
    let rows = timescale
        .observations_after_event_id(from_event_id, batch_limit)
        .await?;
    let mut projected = 0_usize;
    let mut last_event_id = from_event_id;

    for event in rows {
        last_event_id = event.event_id;
        store
            .record_wallet_ata_balance_update(observation_to_wallet_balance_update(
                event.observation,
            ))
            .await?;
        projected += 1;
    }

    if advance_cursor && last_event_id > previous_event_id {
        store
            .advance_projection_offset(consumer_name, last_event_id)
            .await?;
    }

    Ok(ProjectorOutcome {
        projected,
        previous_event_id,
        last_event_id,
    })
}

fn projected_update_from_event(
    event: BalanceSweepAtaObservationEvent,
) -> ProjectedWalletAtaBalanceUpdateInput {
    ProjectedWalletAtaBalanceUpdateInput {
        event_id: event.event_id,
        update: observation_to_wallet_balance_update(event.observation),
    }
}

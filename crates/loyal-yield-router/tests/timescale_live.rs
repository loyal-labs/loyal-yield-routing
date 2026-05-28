use loyal_yield_router::timescale::{
    QueryOrder, ReserveHistoryQuery, ReserveUpdateFilter, ReserveWindowStatsQuery,
    SubscribeOptions, TimescaleRouterClient, TimescaleRouterClientConfig,
};
use tokio::time::{timeout, Duration};

#[tokio::test]
#[ignore = "requires TIMESCALEDB_TEST_URL pointing at the live Kamino TimescaleDB"]
async fn fetches_latest_reserves_from_live_timescaledb() -> sqlx::Result<()> {
    let client = live_client().await?;
    let rows = client.latest_reserves(ReserveUpdateFilter::new()).await?;

    assert!(!rows.is_empty());
    assert!(rows
        .windows(2)
        .all(|pair| pair[0].supply_apy >= pair[1].supply_apy));
    Ok(())
}

#[tokio::test]
#[ignore = "requires TIMESCALEDB_TEST_URL pointing at the live Kamino TimescaleDB"]
async fn fetches_ordered_updates_after_live_cursor() -> sqlx::Result<()> {
    let client = live_client().await?;
    let rows = client
        .reserve_history(ReserveHistoryQuery {
            limit: 10,
            order: QueryOrder::Asc,
            ..ReserveHistoryQuery::default()
        })
        .await?;
    assert!(!rows.is_empty());

    let cursor = rows[0].cursor();
    let after = client
        .reserve_updates_after(&cursor, ReserveUpdateFilter::new(), 10)
        .await?;
    assert!(after.iter().all(|row| row.cursor() > cursor));
    assert!(after
        .windows(2)
        .all(|pair| pair[0].cursor() < pair[1].cursor()));
    Ok(())
}

#[tokio::test]
#[ignore = "requires TIMESCALEDB_TEST_URL pointing at the live Kamino TimescaleDB"]
async fn fetches_last_rows_and_window_stats_from_live_timescaledb() -> sqlx::Result<()> {
    let client = live_client().await?;
    let rows = client
        .reserve_history(ReserveHistoryQuery {
            limit: 10,
            order: QueryOrder::Desc,
            ..ReserveHistoryQuery::default()
        })
        .await?;
    assert!(!rows.is_empty());
    assert!(rows
        .windows(2)
        .all(|pair| pair[0].cursor() > pair[1].cursor()));

    let stats = client
        .reserve_window_stats(ReserveWindowStatsQuery {
            limit: 10,
            ..ReserveWindowStatsQuery::default()
        })
        .await?;
    assert!(!stats.is_empty());
    Ok(())
}

#[tokio::test]
#[ignore = "requires TIMESCALEDB_TEST_URL pointing at the live Kamino TimescaleDB"]
async fn subscribe_yields_catch_up_rows_before_notifications_when_available() -> sqlx::Result<()> {
    let client = live_client().await?;
    let rows = client
        .reserve_history(ReserveHistoryQuery {
            limit: 2,
            order: QueryOrder::Desc,
            ..ReserveHistoryQuery::default()
        })
        .await?;
    if rows.len() < 2 {
        return Ok(());
    }

    let mut stream = client
        .subscribe(
            ReserveUpdateFilter::new(),
            SubscribeOptions {
                start_after: Some(rows[1].cursor()),
                catch_up_limit: 10,
            },
        )
        .await?;
    let item = timeout(Duration::from_secs(10), stream.next_update())
        .await
        .map_err(|_| sqlx::Error::Protocol("timed out waiting for catch-up row".to_string()))??;

    assert!(item.notification.is_none());
    assert!(item.row.cursor() > rows[1].cursor());
    Ok(())
}

async fn live_client() -> sqlx::Result<TimescaleRouterClient> {
    let url = std::env::var("TIMESCALEDB_TEST_URL").map_err(|_| {
        sqlx::Error::Protocol("TIMESCALEDB_TEST_URL is required for live tests".to_string())
    })?;
    TimescaleRouterClient::connect(TimescaleRouterClientConfig::new(url)).await
}

use chrono::{TimeZone, Utc};
use loyal_yield_router::timescale::{
    ReserveUpdateCursor, ReserveUpdateEventIdCursor, ReserveUpdateRow,
};

#[test]
fn reserve_update_row_exposes_durable_cursor() {
    let row = reserve_update_row("USDC", 0.04, 1_000_000.0, false);

    assert_eq!(
        row.event_id_cursor(),
        ReserveUpdateEventIdCursor { event_id: 7 }
    );
    assert_eq!(row.source_commitment, "confirmed");
    assert_eq!(row.symbol.as_deref(), Some("USDC"));
    assert_eq!(row.liquidity_mint, "mint-a");
    assert_eq!(row.total_supply_usd_estimate, 1_000_000.0);
    assert!(!row.reserve_last_update_stale);
}

#[test]
fn legacy_reserve_update_cursor_stays_available() {
    let row = reserve_update_row("USDC", 0.04, 1_000_000.0, false);

    assert_eq!(
        row.cursor(),
        ReserveUpdateCursor {
            observed_at: Utc.with_ymd_and_hms(2026, 5, 28, 0, 0, 0).unwrap(),
            slot: 42,
            reserve: "reserve-a".to_string(),
        }
    );
}

#[test]
fn reserve_update_cursor_orders_like_timescale_catch_up_query() {
    let first = ReserveUpdateCursor {
        observed_at: Utc.with_ymd_and_hms(2026, 5, 28, 0, 0, 0).unwrap(),
        slot: 42,
        reserve: "reserve-a".to_string(),
    };
    let same_time_higher_slot = ReserveUpdateCursor {
        slot: 43,
        ..first.clone()
    };
    let same_slot_next_reserve = ReserveUpdateCursor {
        reserve: "reserve-b".to_string(),
        ..first.clone()
    };

    assert!(first < same_time_higher_slot);
    assert!(first < same_slot_next_reserve);
}

fn reserve_update_row(
    symbol: &str,
    supply_apy: f64,
    total_supply_usd_estimate: f64,
    stale: bool,
) -> ReserveUpdateRow {
    ReserveUpdateRow {
        event_id: 7,
        observed_at: Utc.with_ymd_and_hms(2026, 5, 28, 0, 0, 0).unwrap(),
        slot: 42,
        source: "poll".to_string(),
        source_commitment: "confirmed".to_string(),
        reserve: "reserve-a".to_string(),
        market: Some("main".to_string()),
        market_name: Some("Main market".to_string()),
        symbol: Some(symbol.to_string()),
        liquidity_mint: "mint-a".to_string(),
        supply_apy,
        borrow_apy: 0.08,
        utilization: 0.5,
        total_supply_usd_estimate,
        total_borrow_usd_estimate: 500_000.0,
        reserve_last_update_stale: stale,
        diff_changed: true,
        changed_fields: vec!["supply_apy".to_string()],
        diff_summary: "changed supply_apy".to_string(),
        record: serde_json::json!({
            "target": {
                "reserve": "reserve-a",
                "liquidity_mint": "mint-a"
            }
        }),
    }
}

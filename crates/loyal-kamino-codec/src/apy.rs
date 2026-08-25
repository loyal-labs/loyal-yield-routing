use anyhow::Result;
use chrono::{DateTime, Utc};
use klend_interface::{state::Reserve, Fraction};
use serde::Serialize;
use solana_sdk::pubkey::Pubkey;

use crate::ReserveTarget;

const SLOTS_PER_SECOND: f64 = 2.0;
const SECONDS_PER_YEAR: f64 = 365.25 * 24.0 * 60.0 * 60.0;
pub const RESERVE_OBSERVATION_SCHEMA_VERSION: u16 = 2;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
pub struct BorrowRateCurvePointSnapshot {
    pub utilization_rate_bps: u32,
    pub borrow_rate_bps: u32,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize)]
pub struct WithdrawalCapSnapshot {
    pub config_capacity: i64,
    pub current_total: i64,
    pub last_interval_start_timestamp: u64,
    pub interval_length_seconds: u64,
}

#[derive(Debug, Clone, Serialize)]
pub struct ReserveSnapshot {
    pub observation_schema_version: u16,
    pub observed_at: DateTime<Utc>,
    pub slot: u64,
    pub reserve: Pubkey,
    pub market: Option<Pubkey>,
    pub symbol: Option<String>,
    pub liquidity_mint: Pubkey,
    pub mint_decimals: u64,
    pub reserve_last_update_slot: u64,
    pub reserve_last_update_stale: bool,
    pub reserve_price_status: u8,
    pub available_amount: f64,
    pub borrowed_amount: f64,
    pub borrowed_amount_sf: String,
    pub total_supply_amount: f64,
    pub market_price_usd: f64,
    pub market_price_last_updated_ts: u64,
    pub cumulative_borrow_rate_bsf: [u64; 4],
    pub total_supply_usd_estimate: f64,
    pub total_borrow_usd_estimate: f64,
    pub utilization: f64,
    pub borrow_apr: f64,
    pub supply_apr: f64,
    pub borrow_apy: f64,
    pub supply_apy: f64,
    pub protocol_take_rate_pct: u8,
    pub host_fixed_interest_rate_bps: u16,
    pub reserve_status: u8,
    pub emergency_mode: bool,
    pub loan_to_value_pct: u8,
    pub liquidation_threshold_pct: u8,
    pub borrow_factor_pct: u64,
    pub deposit_limit: u64,
    pub borrow_limit: u64,
    pub utilization_limit_block_borrowing_above_pct: u8,
    pub disable_usage_as_coll_outside_emode: bool,
    pub borrow_limit_outside_elevation_group: u64,
    pub borrowed_amount_outside_elevation_group: u64,
    pub origination_fee_sf: u64,
    pub flash_loan_fee_sf: u64,
    pub borrow_rate_curve: [BorrowRateCurvePointSnapshot; 11],
    pub deposit_withdrawal_cap: WithdrawalCapSnapshot,
    pub debt_withdrawal_cap: WithdrawalCapSnapshot,
}

#[derive(Debug, Clone, Serialize)]
pub struct ReserveDiff {
    pub changed: bool,
    pub changed_fields: Vec<&'static str>,
    pub reserve_last_update_slot: Option<U64Diff>,
    pub reserve_last_update_stale: Option<BoolDiff>,
    pub reserve_price_status: Option<U8Diff>,
    pub available_amount: Option<NumberDiff>,
    pub borrowed_amount: Option<NumberDiff>,
    pub borrowed_amount_sf: Option<StringDiff>,
    pub total_supply_amount: Option<NumberDiff>,
    pub market_price_usd: Option<NumberDiff>,
    pub market_price_last_updated_ts: Option<U64Diff>,
    pub cumulative_borrow_rate_bsf: Option<U64ArrayDiff>,
    pub utilization: Option<NumberDiff>,
    pub borrow_apy: Option<NumberDiff>,
    pub supply_apy: Option<NumberDiff>,
    pub total_supply_usd_estimate: Option<NumberDiff>,
    pub total_borrow_usd_estimate: Option<NumberDiff>,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize)]
pub struct DiffSummaryItem {
    pub field: &'static str,
    pub label: &'static str,
    pub value: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct NumberDiff {
    pub previous: f64,
    pub current: f64,
    pub delta: f64,
}

#[derive(Debug, Clone, Serialize)]
pub struct U64Diff {
    pub previous: u64,
    pub current: u64,
    pub delta: i64,
}

#[derive(Debug, Clone, Serialize)]
pub struct U8Diff {
    pub previous: u8,
    pub current: u8,
    pub delta: i16,
}

#[derive(Debug, Clone, Serialize)]
pub struct BoolDiff {
    pub previous: bool,
    pub current: bool,
}

#[derive(Debug, Clone, Serialize)]
pub struct StringDiff {
    pub previous: String,
    pub current: String,
}

#[derive(Debug, Clone, Serialize)]
pub struct U64ArrayDiff {
    pub previous: [u64; 4],
    pub current: [u64; 4],
}

impl ReserveDiff {
    pub fn summary(&self) -> String {
        let items = self.summary_items();
        if items.is_empty() {
            if self.changed {
                self.changed_fields.join(",")
            } else {
                "none".to_string()
            }
        } else {
            items
                .iter()
                .map(|item| format!("{}:{}", item.field, item.value))
                .collect::<Vec<_>>()
                .join(";")
        }
    }

    pub fn summary_items(&self) -> Vec<DiffSummaryItem> {
        if !self.changed {
            return Vec::new();
        }

        let mut items = Vec::new();
        push_u64_delta(
            &mut items,
            "reserve_last_update_slot",
            "update_slot",
            self.reserve_last_update_slot.as_ref(),
        );
        push_bool_change(
            &mut items,
            "reserve_last_update_stale",
            "stale",
            self.reserve_last_update_stale.as_ref(),
        );
        push_i16_delta(
            &mut items,
            "reserve_price_status",
            "price_status",
            self.reserve_price_status.as_ref().map(|diff| diff.delta),
        );
        push_number_delta(
            &mut items,
            "available_amount",
            "available",
            self.available_amount.as_ref(),
        );
        push_number_delta(
            &mut items,
            "borrowed_amount",
            "borrowed",
            self.borrowed_amount.as_ref(),
        );
        push_number_delta(
            &mut items,
            "market_price_usd",
            "price_usd",
            self.market_price_usd.as_ref(),
        );
        push_u64_delta(
            &mut items,
            "market_price_last_updated_ts",
            "price_ts",
            self.market_price_last_updated_ts.as_ref(),
        );
        push_u64_array_delta(
            &mut items,
            "cumulative_borrow_rate_bsf",
            "borrow_rate_acc",
            self.cumulative_borrow_rate_bsf.as_ref(),
        );
        push_bps_delta(
            &mut items,
            "utilization",
            "utilization",
            self.utilization.as_ref(),
        );
        push_bps_delta(
            &mut items,
            "borrow_apy",
            "borrow_apy",
            self.borrow_apy.as_ref(),
        );
        push_bps_delta(
            &mut items,
            "supply_apy",
            "supply_apy",
            self.supply_apy.as_ref(),
        );
        push_usd_delta(
            &mut items,
            "total_supply_usd_estimate",
            "supply_usd_delta",
            self.total_supply_usd_estimate.as_ref(),
        );
        push_usd_delta(
            &mut items,
            "total_borrow_usd_estimate",
            "borrow_usd_delta",
            self.total_borrow_usd_estimate.as_ref(),
        );
        items
    }
}

pub fn snapshot_from_account(
    target: &ReserveTarget,
    slot: u64,
    data: &[u8],
    slot_duration_ms: f64,
) -> Result<ReserveSnapshot> {
    snapshot_from_account_at(target, slot, data, slot_duration_ms, Utc::now())
}

pub fn snapshot_from_account_at(
    target: &ReserveTarget,
    slot: u64,
    data: &[u8],
    slot_duration_ms: f64,
    observed_at: DateTime<Utc>,
) -> Result<ReserveSnapshot> {
    let reserve = klend_interface::from_account_data::<Reserve>(data)?;
    Ok(snapshot_from_reserve(
        target,
        slot,
        reserve,
        slot_duration_ms,
        observed_at,
    ))
}

fn snapshot_from_reserve(
    target: &ReserveTarget,
    slot: u64,
    reserve: &Reserve,
    slot_duration_ms: f64,
    observed_at: DateTime<Utc>,
) -> ReserveSnapshot {
    let borrowed_amount = scaled_fraction_to_f64(u128::from(reserve.liquidity.borrowed_amount_sf));
    let borrowed_amount_sf = u128::from(reserve.liquidity.borrowed_amount_sf).to_string();
    let available_amount = reserve.liquidity.total_available_amount as f64;
    let accumulated_protocol_fees =
        scaled_fraction_to_f64(u128::from(reserve.liquidity.accumulated_protocol_fees_sf));
    let accumulated_referrer_fees =
        scaled_fraction_to_f64(u128::from(reserve.liquidity.accumulated_referrer_fees_sf));
    let pending_referrer_fees =
        scaled_fraction_to_f64(u128::from(reserve.liquidity.pending_referrer_fees_sf));
    let market_price_usd = scaled_fraction_to_f64(u128::from(reserve.liquidity.market_price_sf));
    let total_supply_amount = total_supply_amount(
        available_amount,
        borrowed_amount,
        accumulated_protocol_fees,
        accumulated_referrer_fees,
        pending_referrer_fees,
    );
    let utilization = utilization_ratio(borrowed_amount, total_supply_amount);
    let apy = compute_reserve_apy(
        reserve,
        utilization,
        slot_duration_ms,
        reserve.config.protocol_take_rate_pct,
        reserve.config.host_fixed_interest_rate_bps,
    );
    let liquidity_mint = reserve.liquidity.mint_pubkey;
    let symbol = target
        .symbol
        .clone()
        .or_else(|| {
            reserve
                .config
                .token_info
                .name_str()
                .map(str::trim)
                .filter(|name| !name.is_empty())
                .map(str::to_string)
        })
        .or_else(|| token_symbol_for_mint(liquidity_mint).map(str::to_string));
    let mint_decimals = reserve.liquidity.mint_decimals;
    let mint_factor = 10_f64.powi(mint_decimals as i32);

    ReserveSnapshot {
        observation_schema_version: RESERVE_OBSERVATION_SCHEMA_VERSION,
        observed_at,
        slot,
        reserve: target.reserve,
        market: Some(reserve.lending_market),
        symbol,
        liquidity_mint,
        mint_decimals,
        reserve_last_update_slot: reserve.last_update.slot,
        reserve_last_update_stale: reserve.last_update.stale != 0,
        reserve_price_status: reserve.last_update.price_status,
        available_amount,
        borrowed_amount,
        borrowed_amount_sf,
        total_supply_amount,
        market_price_usd,
        market_price_last_updated_ts: reserve.liquidity.market_price_last_updated_ts,
        cumulative_borrow_rate_bsf: reserve.liquidity.cumulative_borrow_rate_bsf.value,
        total_supply_usd_estimate: total_supply_amount * market_price_usd / mint_factor,
        total_borrow_usd_estimate: borrowed_amount * market_price_usd / mint_factor,
        utilization,
        borrow_apr: apy.borrow_apr,
        supply_apr: apy.supply_apr,
        borrow_apy: apy.borrow_apy,
        supply_apy: apy.supply_apy,
        protocol_take_rate_pct: reserve.config.protocol_take_rate_pct,
        host_fixed_interest_rate_bps: reserve.config.host_fixed_interest_rate_bps,
        reserve_status: reserve.config.status,
        emergency_mode: reserve.config.emergency_mode != 0,
        loan_to_value_pct: reserve.config.loan_to_value_pct,
        liquidation_threshold_pct: reserve.config.liquidation_threshold_pct,
        borrow_factor_pct: reserve.config.borrow_factor_pct,
        deposit_limit: reserve.config.deposit_limit,
        borrow_limit: reserve.config.borrow_limit,
        utilization_limit_block_borrowing_above_pct: reserve
            .config
            .utilization_limit_block_borrowing_above_pct,
        disable_usage_as_coll_outside_emode: reserve.config.disable_usage_as_coll_outside_emode
            != 0,
        borrow_limit_outside_elevation_group: reserve.config.borrow_limit_outside_elevation_group,
        borrowed_amount_outside_elevation_group: reserve.borrowed_amount_outside_elevation_group,
        origination_fee_sf: reserve.config.fees.origination_fee_sf,
        flash_loan_fee_sf: reserve.config.fees.flash_loan_fee_sf,
        borrow_rate_curve: reserve.config.borrow_rate_curve.points.map(|point| {
            BorrowRateCurvePointSnapshot {
                utilization_rate_bps: point.utilization_rate_bps,
                borrow_rate_bps: point.borrow_rate_bps,
            }
        }),
        deposit_withdrawal_cap: withdrawal_cap_snapshot(reserve.config.deposit_withdrawal_cap),
        debt_withdrawal_cap: withdrawal_cap_snapshot(reserve.config.debt_withdrawal_cap),
    }
}

fn withdrawal_cap_snapshot(cap: klend_interface::state::WithdrawalCaps) -> WithdrawalCapSnapshot {
    WithdrawalCapSnapshot {
        config_capacity: cap.config_capacity,
        current_total: cap.current_total,
        last_interval_start_timestamp: cap.last_interval_start_timestamp,
        interval_length_seconds: cap.config_interval_length_seconds,
    }
}

#[derive(Debug, Clone, Copy)]
struct ApyValues {
    borrow_apr: f64,
    supply_apr: f64,
    borrow_apy: f64,
    supply_apy: f64,
}

fn compute_reserve_apy(
    reserve: &Reserve,
    utilization: f64,
    slot_duration_ms: f64,
    protocol_take_rate_pct: u8,
    host_fixed_interest_rate_bps: u16,
) -> ApyValues {
    let slot_adjustment_factor = 1000.0 / SLOTS_PER_SECOND / slot_duration_ms;
    let curve_borrow_apr = borrow_curve_apr(reserve, utilization) * slot_adjustment_factor;
    let host_fixed_apr = (host_fixed_interest_rate_bps as f64 / 10_000.0) * slot_adjustment_factor;
    let borrow_apr = curve_borrow_apr + host_fixed_apr;
    let supply_apr = supply_apr(utilization, curve_borrow_apr, protocol_take_rate_pct);

    ApyValues {
        borrow_apr,
        supply_apr,
        borrow_apy: apr_to_apy(borrow_apr, slot_duration_ms),
        supply_apy: apr_to_apy(supply_apr, slot_duration_ms),
    }
}

fn total_supply_amount(
    available_amount: f64,
    borrowed_amount: f64,
    accumulated_protocol_fees: f64,
    accumulated_referrer_fees: f64,
    pending_referrer_fees: f64,
) -> f64 {
    (available_amount + borrowed_amount
        - accumulated_protocol_fees
        - accumulated_referrer_fees
        - pending_referrer_fees)
        .max(0.0)
}

fn utilization_ratio(borrowed_amount: f64, total_supply_amount: f64) -> f64 {
    if total_supply_amount > 0.0 {
        borrowed_amount / total_supply_amount
    } else {
        0.0
    }
}

fn supply_apr(utilization: f64, curve_borrow_apr: f64, protocol_take_rate_pct: u8) -> f64 {
    let protocol_supplier_share = 1.0 - protocol_take_rate_pct as f64 / 100.0;
    utilization * curve_borrow_apr * protocol_supplier_share
}

fn scaled_fraction_to_f64(bits: u128) -> f64 {
    let value: f64 = Fraction::from_bits(bits).to_num();
    value
}

fn token_symbol_for_mint(mint: Pubkey) -> Option<&'static str> {
    match mint.to_string().as_str() {
        "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v" => Some("USDC"),
        "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB" => Some("USDT"),
        "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo" => Some("PYUSD"),
        "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA" => Some("USDS"),
        "2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH" => Some("USDG"),
        "DEkqHyPN7GMRJ5cArtQFAWefqbZb33Hyf6s5iCwjEonT" => Some("USDE"),
        "Eh6XEPhSwoLv5wFApukmnaVSHQ6sAnoD9BmgmwQoN2sN" => Some("SUSDE"),
        _ => None,
    }
}

fn borrow_curve_apr(reserve: &Reserve, utilization: f64) -> f64 {
    let mut points = reserve
        .config
        .borrow_rate_curve
        .points
        .iter()
        .map(|point| {
            (
                point.utilization_rate_bps as f64 / 10_000.0,
                point.borrow_rate_bps as f64 / 10_000.0,
            )
        })
        .collect::<Vec<_>>();
    points.sort_by(|a, b| a.0.total_cmp(&b.0));
    points.dedup_by(|a, b| (a.0 - b.0).abs() < f64::EPSILON && (a.1 - b.1).abs() < f64::EPSILON);

    let Some(first) = points.first().copied() else {
        return 0.0;
    };
    if utilization <= first.0 {
        return first.1;
    }
    for pair in points.windows(2) {
        let (floor_util, floor_rate) = pair[0];
        let (ceil_util, ceil_rate) = pair[1];
        if utilization <= ceil_util {
            let width = ceil_util - floor_util;
            if width <= f64::EPSILON {
                return ceil_rate;
            }
            let t = (utilization - floor_util) / width;
            return floor_rate + (ceil_rate - floor_rate) * t;
        }
    }
    points.last().map(|(_, rate)| *rate).unwrap_or(0.0)
}

fn apr_to_apy(apr: f64, slot_duration_ms: f64) -> f64 {
    if apr <= 0.0 {
        return 0.0;
    }
    let periods = SECONDS_PER_YEAR * 1000.0 / slot_duration_ms;
    (1.0 + apr / periods).powf(periods) - 1.0
}

pub fn diff_snapshot(previous: &ReserveSnapshot, current: &ReserveSnapshot) -> ReserveDiff {
    let mut changed_fields = Vec::new();
    let reserve_last_update_slot = track_change(
        &mut changed_fields,
        "reserve_last_update_slot",
        u64_diff(
            previous.reserve_last_update_slot,
            current.reserve_last_update_slot,
        ),
    );
    let reserve_last_update_stale = track_change(
        &mut changed_fields,
        "reserve_last_update_stale",
        bool_diff(
            previous.reserve_last_update_stale,
            current.reserve_last_update_stale,
        ),
    );
    let reserve_price_status = track_change(
        &mut changed_fields,
        "reserve_price_status",
        u8_diff(previous.reserve_price_status, current.reserve_price_status),
    );
    let available_amount = track_change(
        &mut changed_fields,
        "available_amount",
        number_diff(previous.available_amount, current.available_amount),
    );
    let borrowed_amount = track_change(
        &mut changed_fields,
        "borrowed_amount",
        number_diff(previous.borrowed_amount, current.borrowed_amount),
    );
    let borrowed_amount_sf = track_change(
        &mut changed_fields,
        "borrowed_amount_sf",
        string_diff(&previous.borrowed_amount_sf, &current.borrowed_amount_sf),
    );
    let total_supply_amount = track_change(
        &mut changed_fields,
        "total_supply_amount",
        number_diff(previous.total_supply_amount, current.total_supply_amount),
    );
    let market_price_usd = track_change(
        &mut changed_fields,
        "market_price_usd",
        number_diff(previous.market_price_usd, current.market_price_usd),
    );
    let market_price_last_updated_ts = track_change(
        &mut changed_fields,
        "market_price_last_updated_ts",
        u64_diff(
            previous.market_price_last_updated_ts,
            current.market_price_last_updated_ts,
        ),
    );
    let cumulative_borrow_rate_bsf = track_change(
        &mut changed_fields,
        "cumulative_borrow_rate_bsf",
        u64_array_diff(
            previous.cumulative_borrow_rate_bsf,
            current.cumulative_borrow_rate_bsf,
        ),
    );
    let utilization = track_change(
        &mut changed_fields,
        "utilization",
        number_diff(previous.utilization, current.utilization),
    );
    let borrow_apy = track_change(
        &mut changed_fields,
        "borrow_apy",
        number_diff(previous.borrow_apy, current.borrow_apy),
    );
    let supply_apy = track_change(
        &mut changed_fields,
        "supply_apy",
        number_diff(previous.supply_apy, current.supply_apy),
    );
    let total_supply_usd_estimate = track_change(
        &mut changed_fields,
        "total_supply_usd_estimate",
        number_diff(
            previous.total_supply_usd_estimate,
            current.total_supply_usd_estimate,
        ),
    );
    let total_borrow_usd_estimate = track_change(
        &mut changed_fields,
        "total_borrow_usd_estimate",
        number_diff(
            previous.total_borrow_usd_estimate,
            current.total_borrow_usd_estimate,
        ),
    );
    track_field_change(
        &mut changed_fields,
        "reserve_status",
        previous.reserve_status != current.reserve_status,
    );
    track_field_change(
        &mut changed_fields,
        "emergency_mode",
        previous.emergency_mode != current.emergency_mode,
    );
    track_field_change(
        &mut changed_fields,
        "loan_to_value_pct",
        previous.loan_to_value_pct != current.loan_to_value_pct,
    );
    track_field_change(
        &mut changed_fields,
        "liquidation_threshold_pct",
        previous.liquidation_threshold_pct != current.liquidation_threshold_pct,
    );
    track_field_change(
        &mut changed_fields,
        "borrow_factor_pct",
        previous.borrow_factor_pct != current.borrow_factor_pct,
    );
    track_field_change(
        &mut changed_fields,
        "deposit_limit",
        previous.deposit_limit != current.deposit_limit,
    );
    track_field_change(
        &mut changed_fields,
        "borrow_limit",
        previous.borrow_limit != current.borrow_limit,
    );
    track_field_change(
        &mut changed_fields,
        "utilization_limit_block_borrowing_above_pct",
        previous.utilization_limit_block_borrowing_above_pct
            != current.utilization_limit_block_borrowing_above_pct,
    );
    track_field_change(
        &mut changed_fields,
        "disable_usage_as_coll_outside_emode",
        previous.disable_usage_as_coll_outside_emode != current.disable_usage_as_coll_outside_emode,
    );
    track_field_change(
        &mut changed_fields,
        "borrow_limit_outside_elevation_group",
        previous.borrow_limit_outside_elevation_group
            != current.borrow_limit_outside_elevation_group,
    );
    track_field_change(
        &mut changed_fields,
        "borrowed_amount_outside_elevation_group",
        previous.borrowed_amount_outside_elevation_group
            != current.borrowed_amount_outside_elevation_group,
    );
    track_field_change(
        &mut changed_fields,
        "origination_fee_sf",
        previous.origination_fee_sf != current.origination_fee_sf,
    );
    track_field_change(
        &mut changed_fields,
        "flash_loan_fee_sf",
        previous.flash_loan_fee_sf != current.flash_loan_fee_sf,
    );
    track_field_change(
        &mut changed_fields,
        "borrow_rate_curve",
        previous.borrow_rate_curve != current.borrow_rate_curve,
    );
    track_field_change(
        &mut changed_fields,
        "deposit_withdrawal_cap",
        previous.deposit_withdrawal_cap != current.deposit_withdrawal_cap,
    );
    track_field_change(
        &mut changed_fields,
        "debt_withdrawal_cap",
        previous.debt_withdrawal_cap != current.debt_withdrawal_cap,
    );

    ReserveDiff {
        changed: !changed_fields.is_empty(),
        changed_fields,
        reserve_last_update_slot,
        reserve_last_update_stale,
        reserve_price_status,
        available_amount,
        borrowed_amount,
        borrowed_amount_sf,
        total_supply_amount,
        market_price_usd,
        market_price_last_updated_ts,
        cumulative_borrow_rate_bsf,
        utilization,
        borrow_apy,
        supply_apy,
        total_supply_usd_estimate,
        total_borrow_usd_estimate,
    }
}

fn track_field_change(changed_fields: &mut Vec<&'static str>, field: &'static str, changed: bool) {
    if changed {
        changed_fields.push(field);
    }
}

fn track_change<T>(
    changed_fields: &mut Vec<&'static str>,
    field: &'static str,
    diff: Option<T>,
) -> Option<T> {
    if diff.is_some() {
        changed_fields.push(field);
    }
    diff
}

fn number_diff(previous: f64, current: f64) -> Option<NumberDiff> {
    (previous != current).then_some(NumberDiff {
        previous,
        current,
        delta: current - previous,
    })
}

fn u64_diff(previous: u64, current: u64) -> Option<U64Diff> {
    (previous != current).then_some(U64Diff {
        previous,
        current,
        delta: if current >= previous {
            (current - previous) as i64
        } else {
            -((previous - current) as i64)
        },
    })
}

fn u8_diff(previous: u8, current: u8) -> Option<U8Diff> {
    (previous != current).then_some(U8Diff {
        previous,
        current,
        delta: current as i16 - previous as i16,
    })
}

fn bool_diff(previous: bool, current: bool) -> Option<BoolDiff> {
    (previous != current).then_some(BoolDiff { previous, current })
}

fn string_diff(previous: &str, current: &str) -> Option<StringDiff> {
    (previous != current).then_some(StringDiff {
        previous: previous.to_string(),
        current: current.to_string(),
    })
}

fn u64_array_diff(previous: [u64; 4], current: [u64; 4]) -> Option<U64ArrayDiff> {
    (previous != current).then_some(U64ArrayDiff { previous, current })
}

fn push_bps_delta(
    items: &mut Vec<DiffSummaryItem>,
    field: &'static str,
    label: &'static str,
    diff: Option<&NumberDiff>,
) {
    if let Some(diff) = diff {
        push_diff_item(
            items,
            field,
            label,
            format!("{:+.4}bps", diff.delta * 10_000.0),
        );
    }
}

fn push_usd_delta(
    items: &mut Vec<DiffSummaryItem>,
    field: &'static str,
    label: &'static str,
    diff: Option<&NumberDiff>,
) {
    if let Some(diff) = diff {
        push_diff_item(items, field, label, format!("{:+.2}usd", diff.delta));
    }
}

fn push_number_delta(
    items: &mut Vec<DiffSummaryItem>,
    field: &'static str,
    label: &'static str,
    diff: Option<&NumberDiff>,
) {
    if let Some(diff) = diff {
        push_diff_item(items, field, label, format!("{:+.6}", diff.delta));
    }
}

fn push_u64_delta(
    items: &mut Vec<DiffSummaryItem>,
    field: &'static str,
    label: &'static str,
    diff: Option<&U64Diff>,
) {
    if let Some(diff) = diff {
        push_diff_item(items, field, label, format!("{:+}", diff.delta));
    }
}

fn push_i16_delta(
    items: &mut Vec<DiffSummaryItem>,
    field: &'static str,
    label: &'static str,
    delta: Option<i16>,
) {
    if let Some(delta) = delta {
        push_diff_item(items, field, label, format!("{:+}", delta));
    }
}

fn push_bool_change(
    items: &mut Vec<DiffSummaryItem>,
    field: &'static str,
    label: &'static str,
    diff: Option<&BoolDiff>,
) {
    if let Some(diff) = diff {
        push_diff_item(
            items,
            field,
            label,
            format!("{}->{}", diff.previous, diff.current),
        );
    }
}

fn push_u64_array_delta(
    items: &mut Vec<DiffSummaryItem>,
    field: &'static str,
    label: &'static str,
    diff: Option<&U64ArrayDiff>,
) {
    if let Some(diff) = diff {
        let value = diff
            .previous
            .iter()
            .zip(diff.current.iter())
            .enumerate()
            .filter(|(_, (previous, current))| previous != current)
            .map(|(index, (previous, current))| {
                if current >= previous {
                    format!("limb{}:+{}", index, current - previous)
                } else {
                    format!("limb{}:-{}", index, previous - current)
                }
            })
            .collect::<Vec<_>>()
            .join("|");
        push_diff_item(items, field, label, value);
    }
}

fn push_diff_item(
    items: &mut Vec<DiffSummaryItem>,
    field: &'static str,
    label: &'static str,
    value: String,
) {
    items.push(DiffSummaryItem {
        field,
        label,
        value,
    });
}

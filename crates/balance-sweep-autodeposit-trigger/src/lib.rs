use chrono::{DateTime, Duration, Utc};
use serde::{Deserialize, Serialize};
use thiserror::Error;

const AUTODEPOSIT_SCHEDULE_DELAY: Duration = Duration::hours(1);
const LEGACY_SURPLUS_CLASSIFICATION_DB_VALUE: &str = "unknown";

pub fn surplus_lot_classification_db_value() -> &'static str {
    LEGACY_SURPLUS_CLASSIFICATION_DB_VALUE
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum LotStatus {
    Open,
    Selected,
    Consumed,
    Depleted,
    Suppressed,
}

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct SurplusLot {
    pub id: i64,
    pub source_event_id: i64,
    pub source_signature: Option<String>,
    pub original_amount_raw: i64,
    pub remaining_amount_raw: i64,
    pub eligible_after: DateTime<Utc>,
    pub status: LotStatus,
    pub confidence: String,
    pub reason: String,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PositiveDelta {
    pub source_event_id: i64,
    pub source_signature: Option<String>,
    pub amount_raw: i64,
    pub observed_at: DateTime<Utc>,
    pub confidence: String,
    pub reason: String,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SweepCaps {
    pub eligible_lot_amount_raw: i64,
    pub wallet_balance_raw: i64,
    pub wallet_balance_floor_raw: i64,
    pub remaining_allowance_raw: Option<i64>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SweepAmountDecision {
    NoEligibleLots,
    NoWalletExcess {
        excess_raw: i64,
    },
    AllowanceExhausted {
        excess_raw: i64,
    },
    Sweep {
        amount_raw: i64,
        eligible_lot_amount_raw: i64,
        excess_raw: i64,
        capped_by_wallet_floor: bool,
        capped_by_remaining_allowance: bool,
    },
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SelectedLot {
    pub lot_id: i64,
    pub amount_raw: i64,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct LotSelection {
    pub amount_raw: i64,
    pub lots: Vec<SelectedLot>,
}

#[derive(Debug, Error, PartialEq, Eq)]
pub enum LotError {
    #[error("positive delta must be greater than zero")]
    NonPositiveDelta,
    #[error("negative outflow amount must be greater than zero")]
    NonPositiveOutflow,
    #[error("lot {lot_id} has invalid remaining amount {remaining_amount_raw}")]
    InvalidLotRemaining {
        lot_id: i64,
        remaining_amount_raw: i64,
    },
}

pub fn scheduled_eligible_after(observed_at: DateTime<Utc>) -> DateTime<Utc> {
    observed_at + AUTODEPOSIT_SCHEDULE_DELAY
}

pub fn initial_surplus_amount(
    amount_raw: i64,
    wallet_balance_floor_raw: Option<i64>,
) -> Option<i64> {
    let floor = wallet_balance_floor_raw?;
    let surplus = amount_raw - floor;
    (surplus > 0).then_some(surplus)
}

pub fn positive_delta_to_lot(
    next_lot_id: i64,
    source_event_id: i64,
    source_signature: Option<String>,
    amount_raw: i64,
    observed_at: DateTime<Utc>,
) -> Result<SurplusLot, LotError> {
    lot_from_positive_delta(
        next_lot_id,
        PositiveDelta {
            source_event_id,
            source_signature,
            amount_raw,
            observed_at,
            confidence: "derived".to_owned(),
            reason: "wallet balance increase scheduled for autodeposit after one hour".to_owned(),
        },
    )
}

pub fn lot_from_positive_delta(
    next_lot_id: i64,
    delta: PositiveDelta,
) -> Result<SurplusLot, LotError> {
    if delta.amount_raw <= 0 {
        return Err(LotError::NonPositiveDelta);
    }
    Ok(SurplusLot {
        id: next_lot_id,
        source_event_id: delta.source_event_id,
        source_signature: delta.source_signature,
        original_amount_raw: delta.amount_raw,
        remaining_amount_raw: delta.amount_raw,
        eligible_after: scheduled_eligible_after(delta.observed_at),
        status: LotStatus::Open,
        confidence: delta.confidence,
        reason: delta.reason,
        created_at: delta.observed_at,
    })
}

pub fn apply_external_outflow_newest_first(
    lots: &mut [SurplusLot],
    amount_raw: i64,
) -> Result<i64, LotError> {
    if amount_raw <= 0 {
        return Err(LotError::NonPositiveOutflow);
    }

    let mut remaining_outflow = amount_raw;
    lots.sort_by_key(|lot| (lot.created_at, lot.id));
    for lot in lots.iter_mut().rev() {
        if remaining_outflow == 0 {
            break;
        }
        if lot.status != LotStatus::Open || lot.remaining_amount_raw == 0 {
            continue;
        }
        if lot.remaining_amount_raw < 0 {
            return Err(LotError::InvalidLotRemaining {
                lot_id: lot.id,
                remaining_amount_raw: lot.remaining_amount_raw,
            });
        }
        let consumed = remaining_outflow.min(lot.remaining_amount_raw);
        lot.remaining_amount_raw -= consumed;
        remaining_outflow -= consumed;
        if lot.remaining_amount_raw == 0 {
            lot.status = LotStatus::Depleted;
        }
    }

    Ok(amount_raw - remaining_outflow)
}

pub fn compute_sweep_amount(caps: SweepCaps) -> SweepAmountDecision {
    if caps.eligible_lot_amount_raw <= 0 {
        return SweepAmountDecision::NoEligibleLots;
    }
    let excess_raw = caps.wallet_balance_raw - caps.wallet_balance_floor_raw;
    if excess_raw <= 0 {
        return SweepAmountDecision::NoWalletExcess { excess_raw };
    }
    if matches!(caps.remaining_allowance_raw, Some(allowance) if allowance <= 0) {
        return SweepAmountDecision::AllowanceExhausted { excess_raw };
    }

    let mut amount_raw = caps.eligible_lot_amount_raw;
    let mut capped_by_wallet_floor = false;
    let mut capped_by_remaining_allowance = false;
    if amount_raw > excess_raw {
        amount_raw = excess_raw;
        capped_by_wallet_floor = true;
    }
    if let Some(allowance) = caps.remaining_allowance_raw {
        if amount_raw > allowance {
            amount_raw = allowance;
            capped_by_remaining_allowance = true;
        }
    }

    SweepAmountDecision::Sweep {
        amount_raw,
        eligible_lot_amount_raw: caps.eligible_lot_amount_raw,
        excess_raw,
        capped_by_wallet_floor,
        capped_by_remaining_allowance,
    }
}

pub fn select_eligible_lots(
    lots: &[SurplusLot],
    now: DateTime<Utc>,
    wallet_balance_raw: i64,
    wallet_balance_floor_raw: i64,
    remaining_allowance_raw: Option<i64>,
) -> Result<(SweepAmountDecision, Option<LotSelection>), LotError> {
    let mut eligible = lots
        .iter()
        .filter(|lot| {
            lot.status == LotStatus::Open
                && lot.remaining_amount_raw > 0
                && lot.eligible_after <= now
        })
        .collect::<Vec<_>>();
    eligible.sort_by_key(|lot| (lot.eligible_after, lot.created_at, lot.id));
    for lot in &eligible {
        if lot.remaining_amount_raw < 0 {
            return Err(LotError::InvalidLotRemaining {
                lot_id: lot.id,
                remaining_amount_raw: lot.remaining_amount_raw,
            });
        }
    }

    let eligible_lot_amount_raw = eligible
        .iter()
        .map(|lot| lot.remaining_amount_raw)
        .sum::<i64>();
    let decision = compute_sweep_amount(SweepCaps {
        eligible_lot_amount_raw,
        wallet_balance_raw,
        wallet_balance_floor_raw,
        remaining_allowance_raw,
    });
    let SweepAmountDecision::Sweep { amount_raw, .. } = decision else {
        return Ok((decision, None));
    };

    let mut remaining = amount_raw;
    let mut selected = Vec::new();
    for lot in eligible {
        if remaining == 0 {
            break;
        }
        let amount = remaining.min(lot.remaining_amount_raw);
        selected.push(SelectedLot {
            lot_id: lot.id,
            amount_raw: amount,
        });
        remaining -= amount;
    }

    Ok((
        decision,
        Some(LotSelection {
            amount_raw,
            lots: selected,
        }),
    ))
}

pub fn apply_autodeposit_consumption(
    lots: &mut [SurplusLot],
    selection: &LotSelection,
) -> Result<i64, LotError> {
    let mut consumed_total = 0_i64;
    for selected in &selection.lots {
        let Some(lot) = lots.iter_mut().find(|lot| lot.id == selected.lot_id) else {
            continue;
        };
        if lot.remaining_amount_raw < selected.amount_raw {
            return Err(LotError::InvalidLotRemaining {
                lot_id: lot.id,
                remaining_amount_raw: lot.remaining_amount_raw,
            });
        }
        lot.remaining_amount_raw -= selected.amount_raw;
        consumed_total += selected.amount_raw;
        lot.status = if lot.remaining_amount_raw == 0 {
            LotStatus::Consumed
        } else {
            LotStatus::Open
        };
    }
    Ok(consumed_total)
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::TimeZone;

    fn observed_at() -> DateTime<Utc> {
        Utc.with_ymd_and_hms(2026, 6, 16, 12, 30, 0)
            .single()
            .expect("valid test timestamp")
    }

    #[test]
    fn scheduled_eligible_after_always_waits_one_hour() {
        assert_eq!(
            scheduled_eligible_after(observed_at()),
            observed_at() + Duration::hours(1)
        );
    }

    #[test]
    fn positive_delta_lot_uses_fixed_one_hour_delay() {
        let lot = positive_delta_to_lot(
            7,
            42,
            Some("txn-signature".to_owned()),
            1_000_000,
            observed_at(),
        )
        .expect("positive delta creates lot");

        assert_eq!(lot.eligible_after, observed_at() + Duration::hours(1));
        assert_eq!(lot.remaining_amount_raw, 1_000_000);
    }

    #[test]
    fn legacy_db_classification_is_not_scheduler_behavior() {
        assert_eq!(surplus_lot_classification_db_value(), "unknown");
    }
}

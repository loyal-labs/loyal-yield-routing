use chrono::{DateTime, Duration, Utc};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use thiserror::Error;

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SurplusClassification {
    EarnWithdrawal,
    SimpleInbound,
    ComplexDefi,
    Unknown,
    ExplicitRedeposit,
}

impl SurplusClassification {
    pub fn as_db_str(self) -> &'static str {
        match self {
            SurplusClassification::EarnWithdrawal => "earn_withdrawal",
            SurplusClassification::SimpleInbound => "simple_inbound",
            SurplusClassification::ComplexDefi => "complex_defi",
            SurplusClassification::Unknown => "unknown",
            SurplusClassification::ExplicitRedeposit => "explicit_redeposit",
        }
    }
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
    pub classification: SurplusClassification,
    pub eligible_after: DateTime<Utc>,
    pub status: LotStatus,
    pub confidence: String,
    pub reason: String,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ClassifiedPositiveDelta {
    pub source_event_id: i64,
    pub source_signature: Option<String>,
    pub amount_raw: i64,
    pub classification: SurplusClassification,
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

pub fn eligible_after(
    classification: SurplusClassification,
    observed_at: DateTime<Utc>,
) -> DateTime<Utc> {
    observed_at
        + match classification {
            SurplusClassification::EarnWithdrawal => Duration::hours(24),
            SurplusClassification::SimpleInbound => Duration::minutes(2),
            SurplusClassification::ComplexDefi => Duration::hours(1),
            SurplusClassification::Unknown => Duration::minutes(30),
            SurplusClassification::ExplicitRedeposit => Duration::zero(),
        }
}

pub fn classify_from_evidence(
    txn_signature: Option<&str>,
    raw_evidence: &Value,
) -> (SurplusClassification, &'static str, String) {
    let source = raw_evidence
        .get("autodeposit_source_classification")
        .or_else(|| raw_evidence.get("source_classification"))
        .or_else(|| raw_evidence.get("classification"))
        .and_then(Value::as_str);

    match source {
        Some("earn_withdrawal") => (
            SurplusClassification::EarnWithdrawal,
            "explicit",
            "raw evidence marked the source as an Earn withdrawal".to_owned(),
        ),
        Some("simple_inbound") => (
            SurplusClassification::SimpleInbound,
            "explicit",
            "raw evidence marked the source as a simple inbound transfer".to_owned(),
        ),
        Some("complex_defi") => (
            SurplusClassification::ComplexDefi,
            "explicit",
            "raw evidence marked the source as complex DeFi activity".to_owned(),
        ),
        Some("explicit_redeposit") => (
            SurplusClassification::ExplicitRedeposit,
            "explicit",
            "raw evidence marked the source as an explicit redeposit".to_owned(),
        ),
        Some(other) => (
            SurplusClassification::Unknown,
            "unknown",
            format!("raw evidence carried an unrecognized classification {other}"),
        ),
        None if txn_signature.is_none() => (
            SurplusClassification::Unknown,
            "unknown",
            "no transaction signature was available for source classification".to_owned(),
        ),
        None => (
            SurplusClassification::Unknown,
            "unknown",
            "transaction signature is present but no classifier has labeled it yet".to_owned(),
        ),
    }
}

pub fn positive_delta_to_lot(
    next_lot_id: i64,
    source_event_id: i64,
    source_signature: Option<String>,
    amount_raw: i64,
    observed_at: DateTime<Utc>,
    raw_evidence: &Value,
) -> Result<SurplusLot, LotError> {
    let (classification, confidence, reason) =
        classify_from_evidence(source_signature.as_deref(), raw_evidence);
    lot_from_positive_delta(
        next_lot_id,
        ClassifiedPositiveDelta {
            source_event_id,
            source_signature,
            amount_raw,
            classification,
            observed_at,
            confidence: confidence.to_owned(),
            reason,
        },
    )
}

pub fn lot_from_positive_delta(
    next_lot_id: i64,
    delta: ClassifiedPositiveDelta,
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
        classification: delta.classification,
        eligible_after: eligible_after(delta.classification, delta.observed_at),
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

    fn at(seconds: i64) -> DateTime<Utc> {
        DateTime::<Utc>::from_timestamp(seconds, 0).unwrap()
    }

    fn lot(
        id: i64,
        amount_raw: i64,
        classification: SurplusClassification,
        observed_at: DateTime<Utc>,
    ) -> SurplusLot {
        lot_from_positive_delta(
            id,
            ClassifiedPositiveDelta {
                source_event_id: id * 10,
                source_signature: Some(format!("sig-{id}")),
                amount_raw,
                classification,
                observed_at,
                confidence: "test".to_owned(),
                reason: "test fixture".to_owned(),
            },
        )
        .unwrap()
    }

    #[test]
    fn classification_windows_match_product_rules() {
        let observed_at = at(1_000);
        assert_eq!(
            eligible_after(SurplusClassification::EarnWithdrawal, observed_at),
            observed_at + Duration::hours(24)
        );
        assert_eq!(
            eligible_after(SurplusClassification::SimpleInbound, observed_at),
            observed_at + Duration::minutes(2)
        );
        assert_eq!(
            eligible_after(SurplusClassification::ComplexDefi, observed_at),
            observed_at + Duration::hours(1)
        );
        assert_eq!(
            eligible_after(SurplusClassification::Unknown, observed_at),
            observed_at + Duration::minutes(30)
        );
        assert_eq!(
            eligible_after(SurplusClassification::ExplicitRedeposit, observed_at),
            observed_at
        );
    }

    #[test]
    fn missing_signature_falls_back_to_unknown_classification() {
        let (classification, confidence, reason) =
            classify_from_evidence(None, &serde_json::json!({}));

        assert_eq!(classification, SurplusClassification::Unknown);
        assert_eq!(confidence, "unknown");
        assert!(reason.contains("no transaction signature"));
    }

    #[test]
    fn explicit_evidence_selects_simple_inbound_window() {
        let lot = positive_delta_to_lot(
            1,
            10,
            Some("sig".to_owned()),
            5_000_000,
            at(1_000),
            &serde_json::json!({"classification": "simple_inbound"}),
        )
        .unwrap();

        assert_eq!(lot.classification, SurplusClassification::SimpleInbound);
        assert_eq!(lot.eligible_after, at(1_000) + Duration::minutes(2));
    }

    #[test]
    fn mixed_defi_and_inbound_sweeps_only_the_eligible_inbound_lot() {
        let start = at(1_000);
        let lots = vec![
            lot(1, 5_000_000, SurplusClassification::ComplexDefi, start),
            lot(
                2,
                5_000_000,
                SurplusClassification::SimpleInbound,
                start + Duration::seconds(10),
            ),
        ];

        let (_, selection) = select_eligible_lots(
            &lots,
            start + Duration::minutes(3),
            20_000_000,
            10_000_000,
            None,
        )
        .unwrap();

        assert_eq!(
            selection.unwrap().lots,
            vec![SelectedLot {
                lot_id: 2,
                amount_raw: 5_000_000,
            }]
        );
    }

    #[test]
    fn later_simple_inbound_can_sweep_before_older_defi_matures() {
        let start = at(1_000);
        let lots = vec![
            lot(1, 5_000_000, SurplusClassification::ComplexDefi, start),
            lot(
                2,
                5_000_000,
                SurplusClassification::SimpleInbound,
                start + Duration::minutes(10),
            ),
        ];

        let (_, selection) = select_eligible_lots(
            &lots,
            start + Duration::minutes(13),
            20_000_000,
            10_000_000,
            None,
        )
        .unwrap();

        assert_eq!(selection.unwrap().lots[0].lot_id, 2);
    }

    #[test]
    fn external_outflow_depletes_newest_open_lots_first() {
        let start = at(1_000);
        let mut lots = vec![
            lot(1, 5_000_000, SurplusClassification::ComplexDefi, start),
            lot(
                2,
                5_000_000,
                SurplusClassification::SimpleInbound,
                start + Duration::minutes(2),
            ),
        ];

        let consumed = apply_external_outflow_newest_first(&mut lots, 2_000_000).unwrap();

        assert_eq!(consumed, 2_000_000);
        assert_eq!(lots[0].remaining_amount_raw, 5_000_000);
        assert_eq!(lots[1].remaining_amount_raw, 3_000_000);
        assert_eq!(lots[1].status, LotStatus::Open);
    }

    #[test]
    fn depleted_defi_lot_only_sweeps_remaining_amount_later() {
        let start = at(1_000);
        let mut lots = vec![lot(1, 5_000_000, SurplusClassification::ComplexDefi, start)];
        apply_external_outflow_newest_first(&mut lots, 2_000_000).unwrap();

        let (_, selection) = select_eligible_lots(
            &lots,
            start + Duration::hours(2),
            13_000_000,
            10_000_000,
            None,
        )
        .unwrap();

        assert_eq!(
            selection.unwrap().lots,
            vec![SelectedLot {
                lot_id: 1,
                amount_raw: 3_000_000,
            }]
        );
    }

    #[test]
    fn selection_is_capped_by_wallet_floor_and_remaining_allowance() {
        let start = at(1_000);
        let lots = vec![lot(
            1,
            8_000_000,
            SurplusClassification::SimpleInbound,
            start,
        )];

        let (decision, selection) = select_eligible_lots(
            &lots,
            start + Duration::minutes(3),
            20_000_000,
            10_000_000,
            Some(6_000_000),
        )
        .unwrap();

        assert_eq!(
            decision,
            SweepAmountDecision::Sweep {
                amount_raw: 6_000_000,
                eligible_lot_amount_raw: 8_000_000,
                excess_raw: 10_000_000,
                capped_by_wallet_floor: false,
                capped_by_remaining_allowance: true,
            }
        );
        assert_eq!(selection.unwrap().amount_raw, 6_000_000);
    }

    #[test]
    fn autodeposit_consumption_keeps_partial_lot_open_for_later() {
        let start = at(1_000);
        let mut lots = vec![lot(
            1,
            8_000_000,
            SurplusClassification::SimpleInbound,
            start,
        )];
        let selection = LotSelection {
            amount_raw: 6_000_000,
            lots: vec![SelectedLot {
                lot_id: 1,
                amount_raw: 6_000_000,
            }],
        };

        let consumed = apply_autodeposit_consumption(&mut lots, &selection).unwrap();

        assert_eq!(consumed, 6_000_000);
        assert_eq!(lots[0].remaining_amount_raw, 2_000_000);
        assert_eq!(lots[0].status, LotStatus::Open);
    }

    #[test]
    fn autodeposit_consumption_marks_fully_used_lots_consumed() {
        let start = at(1_000);
        let mut lots = vec![lot(
            1,
            5_000_000,
            SurplusClassification::SimpleInbound,
            start,
        )];
        let selection = LotSelection {
            amount_raw: 5_000_000,
            lots: vec![SelectedLot {
                lot_id: 1,
                amount_raw: 5_000_000,
            }],
        };

        apply_autodeposit_consumption(&mut lots, &selection).unwrap();

        assert_eq!(lots[0].remaining_amount_raw, 0);
        assert_eq!(lots[0].status, LotStatus::Consumed);
    }
}

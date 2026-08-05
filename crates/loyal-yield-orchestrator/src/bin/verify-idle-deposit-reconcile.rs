//! Isolated verification of confirmed idle-vault deposit reconciliation.
//!
//! Replays the ASK-2027 production incident and its adversarial neighbourhood
//! against the shipped predicate, with no database, RPC, or network access. The
//! legacy predicate is reimplemented verbatim below so every check states both
//! what the old code did and what the new code does: a regression that only
//! asserts the fix passes cannot show the bug was ever real.
//!
//! ASK-2027: vault DSVs9cZZ…4tUL deposited 363.85 USDC into Kamino reserve
//! AYL4…VR2Z. The transaction confirmed at slot 437282488 with no error, but
//! 1,001 raw units stayed in the idle ATA — 1,000 that arrived between the
//! planner's observation and execution, plus one unit of Kamino rounding. The
//! legacy predicate demanded the ATA be drained to
//! `current_idle_balance - deposited`, which the ATA monitor had already
//! rewritten to the post-deposit value, so the requirement collapsed to "the
//! ATA is exactly empty". The decision never left `confirming`, and the unique
//! partial index over non-terminal decisions froze the vault for 9h38m.

use std::{error::Error, process::ExitCode};

use loyal_yield_orchestrator::fleet_orchestration::{
    classify_idle_deposit_post_effect, reconciliation_is_stalled,
    reconciliation_retry_delay_seconds, IdleDepositIdentityField, IdleDepositPostEffectDecision,
    IdleDepositPostEffectObservation, IdleDepositRouteContract, RECONCILIATION_FAST_RETRY_ATTEMPTS,
    RECONCILIATION_MAX_RETRY_SECONDS, RECONCILIATION_MIN_RETRY_SECONDS,
    RECONCILIATION_STALL_ATTEMPTS,
};
use serde_json::json;

type VerifyResult<T> = Result<T, Box<dyn Error>>;

// Production identities and amounts from decision 4944 / submission 1244.
const USDC_MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const IDLE_ATA: &str = "7ZTei2w9zzgPAyxCCn4egCe6xgoyXQpZLY95tDqApXXP";
const OTHER_MINT: &str = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB";
const OTHER_ATA: &str = "2DGC5qXSFZ51Z4kRmLXBDjcyRGN5KMZddEpYkD8a1t6Q";
const CONFIRMED_SLOT: i64 = 437_282_488;
const DEPOSITED_RAW: i64 = 363_850_000;
const BASELINE_IDLE_RAW: i64 = 363_850_000;
/// Residual left in the ATA by the production transaction.
const INCIDENT_RESIDUAL_RAW: i64 = 1_001;
/// Residual the ATA monitor observed later the same morning.
const INCIDENT_RESIDUAL_LATER_RAW: i64 = 1_008;

fn incident_contract() -> IdleDepositRouteContract<'static> {
    IdleDepositRouteContract {
        confirmed_slot: CONFIRMED_SLOT,
        liquidity_mint: USDC_MINT,
        idle_token_account: IDLE_ATA,
        deposited_amount_raw: DEPOSITED_RAW,
        baseline_idle_amount_raw: Some(BASELINE_IDLE_RAW),
    }
}

fn observation(
    observed_slot: i64,
    idle_amount_raw: i64,
) -> IdleDepositPostEffectObservation<'static> {
    IdleDepositPostEffectObservation {
        observed_slot,
        target_liquidity_mint: USDC_MINT,
        vault_liquidity_ata: IDLE_ATA,
        idle_amount_raw,
    }
}

/// The predicate as it shipped before this fix, reproduced from
/// `reconcile_idle_submission_effect`.
///
/// `stored_idle_amount_raw` is the `vault_idle_token_balances_current` row the
/// old code derived its expectation from — a live projection the ATA monitor
/// rewrites the moment the deposit lands, not the pre-deposit baseline.
fn legacy_chain_predicate_admits(
    stored_idle_amount_raw: Option<i64>,
    deposited_amount_raw: i64,
    target_amount_raw: i64,
    observed_liquidity_mint: &str,
    observed_vault_liquidity_ata: &str,
    observed_idle_amount_raw: i64,
) -> bool {
    let expected_idle_after = stored_idle_amount_raw
        .map(|stored| stored.saturating_sub(deposited_amount_raw))
        .unwrap_or_default()
        .max(0);
    if observed_liquidity_mint != USDC_MINT || target_amount_raw == 0 {
        return false;
    }
    if observed_vault_liquidity_ata != IDLE_ATA || observed_idle_amount_raw > expected_idle_after {
        return false;
    }
    true
}

/// The legacy current-state fast path, reproduced verbatim.
fn legacy_fast_path_admits(
    stored_idle_amount_raw: i64,
    deposited_amount_raw: i64,
    target_amount_raw: i64,
    target_observed_slot: i64,
    idle_observed_slot: i64,
    confirmed_slot: i64,
) -> bool {
    let expected_idle_after = stored_idle_amount_raw
        .saturating_sub(deposited_amount_raw)
        .max(0);
    target_observed_slot >= confirmed_slot
        && idle_observed_slot >= confirmed_slot
        && target_amount_raw > 0
        && stored_idle_amount_raw <= expected_idle_after
}

fn reconciles(decision: IdleDepositPostEffectDecision) -> bool {
    matches!(decision, IdleDepositPostEffectDecision::Reconcile(_))
}

fn ensure(condition: bool, detail: &str) -> VerifyResult<()> {
    if condition {
        Ok(())
    } else {
        Err(detail.to_owned().into())
    }
}

/// Check 1 — the incident itself. The confirmed deposit must reconcile against
/// the state the production reconciler actually saw, and must have been
/// rejected by the predicate that shipped.
fn verify_incident_replay() -> VerifyResult<()> {
    let contract = incident_contract();
    for (label, residual) in [
        ("at_execution", INCIDENT_RESIDUAL_RAW),
        ("after_ata_monitor_refresh", INCIDENT_RESIDUAL_LATER_RAW),
    ] {
        let decision =
            classify_idle_deposit_post_effect(contract, observation(CONFIRMED_SLOT + 1, residual));
        ensure(
            reconciles(decision),
            &format!("ASK-2027 residual {residual} ({label}) must reconcile"),
        )?;

        // The stored balance the ATA monitor had already written back.
        ensure(
            !legacy_chain_predicate_admits(
                Some(residual),
                DEPOSITED_RAW,
                346_651_323,
                USDC_MINT,
                IDLE_ATA,
                residual,
            ),
            &format!("legacy predicate must reject residual {residual} ({label})"),
        )?;
    }

    // Even against the pre-deposit baseline the legacy predicate rejects, so
    // the bug was not merely a stale-projection race: an exact-drain demand
    // cannot survive liquidity arriving after the planner's observation.
    ensure(
        !legacy_chain_predicate_admits(
            Some(BASELINE_IDLE_RAW),
            DEPOSITED_RAW,
            346_651_323,
            USDC_MINT,
            IDLE_ATA,
            INCIDENT_RESIDUAL_RAW,
        ),
        "legacy predicate must reject the incident even against the planning baseline",
    )?;
    Ok(())
}

/// Check 2 — any residual reconciles. The old predicate admitted exactly one
/// idle balance; the new one admits every balance, because balance arithmetic
/// no longer gates the transition.
fn verify_residual_neighbourhood() -> VerifyResult<()> {
    let contract = incident_contract();
    let residuals = [
        0,
        1,
        1_001,
        1_008,
        999_999,
        // A user deposit landing after the route executed leaves far more in
        // the ATA than the plan predicted.
        500_000_000,
        i64::MAX,
    ];
    let mut legacy_admitted = 0usize;
    for residual in residuals {
        ensure(
            reconciles(classify_idle_deposit_post_effect(
                contract,
                observation(CONFIRMED_SLOT, residual),
            )),
            &format!("residual {residual} must reconcile"),
        )?;
        if legacy_chain_predicate_admits(
            Some(residual),
            DEPOSITED_RAW,
            346_651_323,
            USDC_MINT,
            IDLE_ATA,
            residual,
        ) {
            legacy_admitted += 1;
        }
    }
    ensure(
        legacy_admitted == 1,
        "legacy predicate must admit exactly the empty-ATA case across the residual sweep",
    )
}

/// Check 3 — the legacy fast path was unreachable. Its expectation was derived
/// from the same balance it compared against, so it could only fire on an
/// exactly empty ATA. The replacement fires on any fresh, identity-bound
/// observation, which is what keeps the common case off the RPC preview.
fn verify_fast_path_reachability() -> VerifyResult<()> {
    let contract = incident_contract();
    for stored_idle in [0i64, 1, 1_001, 12_345, 363_850_000] {
        let legacy = legacy_fast_path_admits(
            stored_idle,
            DEPOSITED_RAW,
            346_651_323,
            CONFIRMED_SLOT,
            CONFIRMED_SLOT,
            CONFIRMED_SLOT,
        );
        ensure(
            legacy == (stored_idle == 0),
            &format!("legacy fast path must be reachable only at an empty ATA, not {stored_idle}"),
        )?;
        ensure(
            reconciles(classify_idle_deposit_post_effect(
                contract,
                observation(CONFIRMED_SLOT, stored_idle),
            )),
            &format!("fast path must reconcile a fresh observation holding {stored_idle}"),
        )?;
    }
    Ok(())
}

/// Check 4 — a fully withdrawn position still reconciles. The ASK-2027 user
/// withdrew 363.000012 USDC 45 minutes after the deposit; a predicate keyed on
/// a non-zero target position would have stranded the decision a second time.
fn verify_position_withdrawn_after_deposit() -> VerifyResult<()> {
    ensure(
        reconciles(classify_idle_deposit_post_effect(
            incident_contract(),
            observation(CONFIRMED_SLOT + 6_348, 1_008),
        )),
        "a deposit withdrawn after confirmation must still reconcile",
    )?;
    ensure(
        !legacy_chain_predicate_admits(Some(1_008), DEPOSITED_RAW, 0, USDC_MINT, IDLE_ATA, 1_008),
        "legacy predicate must reject a fully withdrawn position",
    )
}

/// Check 5 — freshness is still mandatory. An observation from before the
/// confirmed slot cannot describe post-deposit state and must never close a
/// decision, however plausible its balances look.
fn verify_stale_observation_never_reconciles() -> VerifyResult<()> {
    let contract = incident_contract();
    for behind in [1i64, 2, 284, 100_000, CONFIRMED_SLOT] {
        let decision =
            classify_idle_deposit_post_effect(contract, observation(CONFIRMED_SLOT - behind, 0));
        ensure(
            matches!(
                decision,
                IdleDepositPostEffectDecision::ObservationPredatesConfirmation { .. }
            ),
            &format!("observation {behind} slots behind confirmation must not reconcile"),
        )?;
    }
    ensure(
        reconciles(classify_idle_deposit_post_effect(
            contract,
            observation(CONFIRMED_SLOT, 0),
        )),
        "an observation exactly at the confirmed slot must reconcile",
    )
}

/// Check 6 — identity is still mandatory. A preview describing a different mint
/// or a different ATA is not evidence about this route.
fn verify_identity_mismatch_never_reconciles() -> VerifyResult<()> {
    let contract = incident_contract();
    let wrong_mint = IdleDepositPostEffectObservation {
        target_liquidity_mint: OTHER_MINT,
        ..observation(CONFIRMED_SLOT + 1, 0)
    };
    ensure(
        matches!(
            classify_idle_deposit_post_effect(contract, wrong_mint),
            IdleDepositPostEffectDecision::IdentityMismatch {
                field: IdleDepositIdentityField::LiquidityMint
            }
        ),
        "a foreign liquidity mint must be reported as an identity mismatch",
    )?;
    let wrong_ata = IdleDepositPostEffectObservation {
        vault_liquidity_ata: OTHER_ATA,
        ..observation(CONFIRMED_SLOT + 1, 0)
    };
    ensure(
        matches!(
            classify_idle_deposit_post_effect(contract, wrong_ata),
            IdleDepositPostEffectDecision::IdentityMismatch {
                field: IdleDepositIdentityField::IdleTokenAccount
            }
        ),
        "a foreign idle ATA must be reported as an identity mismatch",
    )?;
    // Identity outranks freshness: a mismatched account is not a stale read.
    let stale_and_wrong = IdleDepositPostEffectObservation {
        vault_liquidity_ata: OTHER_ATA,
        ..observation(CONFIRMED_SLOT - 1, 0)
    };
    ensure(
        matches!(
            classify_idle_deposit_post_effect(contract, stale_and_wrong),
            IdleDepositPostEffectDecision::IdentityMismatch { .. }
        ),
        "identity mismatch must outrank staleness",
    )
}

/// Check 7 — residual evidence arithmetic. The surplus recorded on the decision
/// is what the ATA holds beyond the plan's prediction, never negative, and
/// defined when the plan carried no baseline.
fn verify_residual_evidence() -> VerifyResult<()> {
    let contract = incident_contract();
    let IdleDepositPostEffectDecision::Reconcile(evidence) = classify_idle_deposit_post_effect(
        contract,
        observation(CONFIRMED_SLOT, INCIDENT_RESIDUAL_RAW),
    ) else {
        return Err("incident must reconcile".to_owned().into());
    };
    ensure(
        evidence.idle_amount_raw == INCIDENT_RESIDUAL_RAW
            && evidence.planned_residual_raw == 0
            && evidence.unexplained_surplus_raw == INCIDENT_RESIDUAL_RAW,
        "incident evidence must report the full 1,001 raw residual as unexplained surplus",
    )?;

    // A plan that deliberately left liquidity behind predicts a residual, and
    // that predicted part is not surplus.
    let partial = IdleDepositRouteContract {
        deposited_amount_raw: 100_000_000,
        baseline_idle_amount_raw: Some(363_850_000),
        ..contract
    };
    let IdleDepositPostEffectDecision::Reconcile(evidence) =
        classify_idle_deposit_post_effect(partial, observation(CONFIRMED_SLOT, 263_851_000))
    else {
        return Err("partial deposit must reconcile".to_owned().into());
    };
    ensure(
        evidence.planned_residual_raw == 263_850_000 && evidence.unexplained_surplus_raw == 1_000,
        "partial deposit evidence must separate planned residual from surplus",
    )?;

    // No baseline, and a drained ATA: no surplus to report either way.
    let no_baseline = IdleDepositRouteContract {
        baseline_idle_amount_raw: None,
        ..contract
    };
    let IdleDepositPostEffectDecision::Reconcile(evidence) =
        classify_idle_deposit_post_effect(no_baseline, observation(CONFIRMED_SLOT, 0))
    else {
        return Err("baseline-free contract must reconcile".to_owned().into());
    };
    ensure(
        evidence.planned_residual_raw == 0 && evidence.unexplained_surplus_raw == 0,
        "a drained ATA must report no surplus",
    )?;

    // Saturating arithmetic must not manufacture a negative surplus.
    let overdrawn = IdleDepositRouteContract {
        deposited_amount_raw: i64::MAX,
        baseline_idle_amount_raw: Some(i64::MIN),
        ..contract
    };
    let IdleDepositPostEffectDecision::Reconcile(evidence) =
        classify_idle_deposit_post_effect(overdrawn, observation(CONFIRMED_SLOT, 0))
    else {
        return Err("saturating contract must reconcile".to_owned().into());
    };
    ensure(
        evidence.planned_residual_raw == 0 && evidence.unexplained_surplus_raw == 0,
        "residual evidence must never report negative amounts",
    )
}

/// Check 8 — the retry schedule is bounded and monotone. Reconciliation never
/// abandons a confirmed movement, so the backoff is the only thing standing
/// between a failing predicate and a permanent one-per-second RPC loop.
fn verify_retry_schedule() -> VerifyResult<()> {
    let mut previous = 0i64;
    for attempt in 0..10_000 {
        let delay = reconciliation_retry_delay_seconds(attempt);
        ensure(
            (RECONCILIATION_MIN_RETRY_SECONDS..=RECONCILIATION_MAX_RETRY_SECONDS).contains(&delay),
            &format!("attempt {attempt} delay {delay} left the bounded range"),
        )?;
        ensure(
            delay >= previous,
            &format!("attempt {attempt} delay {delay} regressed below {previous}"),
        )?;
        previous = delay;
    }
    ensure(
        reconciliation_retry_delay_seconds(i32::MAX) == RECONCILIATION_MAX_RETRY_SECONDS
            && reconciliation_retry_delay_seconds(i32::MIN) == RECONCILIATION_MIN_RETRY_SECONDS,
        "the schedule must saturate at both extremes rather than overflow",
    )?;
    ensure(
        reconciliation_retry_delay_seconds(RECONCILIATION_FAST_RETRY_ATTEMPTS)
            == RECONCILIATION_MIN_RETRY_SECONDS
            && reconciliation_retry_delay_seconds(RECONCILIATION_FAST_RETRY_ATTEMPTS + 1)
                > RECONCILIATION_MIN_RETRY_SECONDS,
        "backoff must engage immediately after the fast-retry window",
    )?;

    // The incident ran 5,782 attempts in 9h38m. Bound what a full day of the
    // same permanent failure would now cost.
    let mut attempts_in_a_day = 0i64;
    let mut elapsed = 0i64;
    while elapsed < 86_400 {
        elapsed += reconciliation_retry_delay_seconds(
            i32::try_from(attempts_in_a_day).unwrap_or(i32::MAX),
        );
        attempts_in_a_day += 1;
    }
    ensure(
        attempts_in_a_day < 1_600,
        &format!(
            "a stuck submission must cost under 1,600 attempts per day, not {attempts_in_a_day}"
        ),
    )?;
    ensure(
        !reconciliation_is_stalled(RECONCILIATION_STALL_ATTEMPTS - 1)
            && reconciliation_is_stalled(RECONCILIATION_STALL_ATTEMPTS)
            && reconciliation_is_stalled(5_782),
        "the stall signal must fire at its threshold and stay latched",
    )?;
    Ok(())
}

type NamedCheck = (&'static str, fn() -> VerifyResult<()>);

fn main() -> ExitCode {
    let checks: [NamedCheck; 8] = [
        ("ask_2027_incident_replay", verify_incident_replay),
        ("residual_neighbourhood", verify_residual_neighbourhood),
        ("fast_path_reachability", verify_fast_path_reachability),
        (
            "position_withdrawn_after_deposit",
            verify_position_withdrawn_after_deposit,
        ),
        (
            "stale_observation_never_reconciles",
            verify_stale_observation_never_reconciles,
        ),
        (
            "identity_mismatch_never_reconciles",
            verify_identity_mismatch_never_reconciles,
        ),
        ("residual_evidence", verify_residual_evidence),
        ("retry_schedule", verify_retry_schedule),
    ];

    let mut failures = Vec::new();
    for (name, check) in checks {
        match check() {
            Ok(()) => println!("{}", json!({"check": name, "status": "pass"})),
            Err(error) => {
                let detail = error.to_string();
                println!(
                    "{}",
                    json!({"check": name, "status": "fail", "detail": detail})
                );
                failures.push(name);
            }
        }
    }

    if failures.is_empty() {
        println!(
            "{}",
            json!({"status": "pass", "checks": checks.len(), "subject": "idle_deposit_reconciliation"})
        );
        ExitCode::SUCCESS
    } else {
        println!("{}", json!({"status": "fail", "failedChecks": failures}));
        ExitCode::FAILURE
    }
}

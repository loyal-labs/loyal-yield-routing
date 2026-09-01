//! Offline V03 compiler. It validates the complete requested lane set but never
//! discovers addresses, signs packets, simulates, or emits installable policies.
use serde::{Deserialize, Serialize};
use std::{collections::BTreeSet, io::Read};

const EXPECTED_LANES: [&str; 11] = [
    "OnRe/ONyc/USDC",
    "OnRe/ONyc/USDG",
    "OnRe/ONyc/USDS",
    "Prime/PRIME/USDC",
    "Prime/PRIME/PYUSD",
    "Prime/PRIME/USDS",
    "Maple/syrupUSDC/USDC",
    "Maple/syrupUSDC/USDG",
    "Maple/syrupUSDC/PYUSD",
    "AUTO/AUTO/PYUSD",
    "Ethena/USDe/PYUSD",
];

#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct Input {
    lanes: Vec<Lane>,
    unresolved: Vec<Unresolved>,
    #[serde(default)]
    live_readback: Option<LiveReadback>,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct Lane {
    market: String,
    collateral: String,
    debt: String,
    candidate_identity: CandidateIdentity,
}

impl Lane {
    fn key(&self) -> String {
        format!("{}/{}/{}", self.market, self.collateral, self.debt)
    }
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct CandidateIdentity {
    evidence: String,
    finalized_slot: u64,
    market: String,
    collateral_reserve: String,
    collateral_mint: String,
    debt_reserve: String,
    debt_mint: String,
    collateral_token_program: String,
    debt_token_program: String,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct Unresolved {
    label: String,
    resume_condition: String,
}

#[derive(Clone, Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct LiveReadback {
    latest_finalized_slot: u64,
    checked_klend_accounts: usize,
    all_present_and_klend_owned: bool,
    scope: String,
}

#[derive(Serialize)]
#[serde(rename_all = "camelCase")]
struct Blocked<'a> {
    schema: &'a str,
    verdict: &'a str,
    blocker: &'a str,
    resume_condition: &'a str,
    unresolved: &'a [Unresolved],
    lanes: &'a [Lane],
    resolved_candidate_count: usize,
    live_readback: &'a Option<LiveReadback>,
    broadcast: bool,
}

fn validate(input: &Input) -> Result<(), String> {
    let actual = input.lanes.iter().map(Lane::key).collect::<BTreeSet<_>>();
    let expected = EXPECTED_LANES
        .into_iter()
        .map(str::to_owned)
        .collect::<BTreeSet<_>>();
    if input.lanes.len() != EXPECTED_LANES.len() || actual != expected {
        return Err("catalog must contain each requested lane exactly once".to_owned());
    }
    for lane in &input.lanes {
        let identity = &lane.candidate_identity;
        if identity.finalized_slot == 0
            || [
                &identity.evidence,
                &identity.market,
                &identity.collateral_reserve,
                &identity.collateral_mint,
                &identity.debt_reserve,
                &identity.debt_mint,
                &identity.collateral_token_program,
                &identity.debt_token_program,
            ]
            .iter()
            .any(|value| value.is_empty())
        {
            return Err(format!(
                "{} has an incomplete candidate identity",
                lane.key()
            ));
        }
    }
    if input.unresolved.is_empty() {
        return Err("this compiler remains BLOCKED until a complete current decoded graph, measured signed packets, and signed-unsent simulation are supplied".to_owned());
    }
    if input
        .unresolved
        .iter()
        .any(|blocker| blocker.label.is_empty() || blocker.resume_condition.is_empty())
    {
        return Err("every blocker needs a label and resume condition".to_owned());
    }
    Ok(())
}

fn compile(input: &Input) -> Result<Blocked<'_>, String> {
    validate(input)?;
    let blocker = input.unresolved.first().expect("validated non-empty");
    Ok(Blocked {
        schema: "loyal-backyard-rwa-policy-catalog/v1",
        verdict: "BLOCKED",
        blocker: &blocker.label,
        resume_condition: &blocker.resume_condition,
        unresolved: &input.unresolved,
        lanes: &input.lanes,
        resolved_candidate_count: input.lanes.len(),
        live_readback: &input.live_readback,
        broadcast: false,
    })
}

fn main() -> Result<(), String> {
    let mut bytes = Vec::new();
    std::io::stdin()
        .read_to_end(&mut bytes)
        .map_err(|error| error.to_string())?;
    let input: Input = serde_json::from_slice(&bytes)
        .map_err(|error| format!("invalid catalog graph: {error}"))?;
    println!(
        "{}",
        serde_json::to_string(&compile(&input)?).map_err(|error| error.to_string())?
    );
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fixture() -> Input {
        serde_json::from_str(include_str!(
            "../../fixtures/backyard_rwa_policy_catalog_v1.json"
        ))
        .expect("fixture is valid compiler input")
    }

    #[test]
    fn fixture_keeps_every_requested_lane_visible_while_blocked() {
        let input = fixture();
        let output = compile(&input).expect("complete blocked catalog compiles");
        assert_eq!(output.verdict, "BLOCKED");
        assert_eq!(output.resolved_candidate_count, 11);
        assert_eq!(output.lanes.len(), 11);
        assert!(!output.unresolved.is_empty());
        assert_eq!(
            output
                .live_readback
                .as_ref()
                .map(|readback| readback.latest_finalized_slot),
            Some(443_332_933)
        );
    }

    #[test]
    fn missing_lane_cannot_be_hidden_by_a_blocker() {
        let mut input = fixture();
        input.lanes.pop();
        assert_eq!(
            validate(&input),
            Err("catalog must contain each requested lane exactly once".to_owned())
        );
    }

    #[test]
    fn empty_blocker_list_cannot_be_mistaken_for_readiness() {
        let mut input = fixture();
        input.unresolved.clear();
        assert!(validate(&input)
            .expect_err("unproven catalog cannot become ready")
            .contains("remains BLOCKED"));
    }
}

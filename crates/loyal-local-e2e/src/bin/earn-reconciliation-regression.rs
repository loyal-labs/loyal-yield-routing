use std::{env, fs, path::PathBuf};

use anyhow::{bail, Context, Result};
use balance_sweep_ata_monitor::local_e2e::{
    earn_reconciliation_regression_report, EarnReconciliationRegressionReport,
};
use loyal_yield_store::{
    EarnDirectMutation, EarnReconciliationEnqueueInput, EarnReconciliationVaultInput,
    EarnRefundMutation, OrchestratorConfig, OrchestratorStore, PolicyMatchInput,
};
use serde::Serialize;
use serde_json::json;

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct Report {
    transaction_classification: EarnReconciliationRegressionReport,
    legacy_unknown_policy_accepted: bool,
    mainnet_refund_normalized: bool,
    operational_alerts: usize,
}

#[tokio::main]
async fn main() -> Result<()> {
    let (postgres_url, output) = parse_args()?;
    let store = OrchestratorStore::connect(OrchestratorConfig::new(postgres_url)).await?;
    let transaction_classification = earn_reconciliation_regression_report();

    let legacy_unknown_policy_accepted = store
        .record_policy_match(PolicyMatchInput {
            signature: "local-e2e-legacy-policy-signature".to_owned(),
            slot: 10,
            cluster: "mainnet-beta".to_owned(),
            source_commitment: "unknown".to_owned(),
            settings: "local-e2e-settings".to_owned(),
            authority: "local-e2e-authority".to_owned(),
            policy_seed: 1,
            policy_account: "local-e2e-policy".to_owned(),
            vault_index: 0,
            vault_pubkey: "local-e2e-vault".to_owned(),
            delegated_signers: vec!["local-e2e-delegated-signer".to_owned()],
            threshold: 1,
            route_modes: vec!["kamino_deposit".to_owned()],
            stable_mints: vec!["local-e2e-mint-a".to_owned()],
            kamino_markets: vec![],
            kamino_liquidity_mints: vec!["local-e2e-mint-a".to_owned()],
            universe_preset: None,
            risk_profile: None,
            swap_lanes: json!([]),
        })
        .await
        .is_ok();

    let consumer_name = "local-e2e-earn-regression";
    store
        .enqueue_earn_reconciliation_jobs(EarnReconciliationEnqueueInput {
            consumer_name: consumer_name.to_owned(),
            event_key: "local-e2e-refund-event".to_owned(),
            durable_slot: 42,
            event_payload: json!({"signature": "local-e2e-refund-signature"}),
            vaults: vec![EarnReconciliationVaultInput {
                settings: "local-e2e-settings".to_owned(),
                vault_index: 0,
                vault_pubkey: "local-e2e-vault".to_owned(),
                vault_payload: json!({}),
            }],
            autodeposit_target_ids: vec![],
        })
        .await?;
    let job = store
        .claim_earn_reconciliation_job(consumer_name, "local-e2e-claim", 120)
        .await?
        .context("local E2E refund job was not claimable")?;
    let mainnet_refund_normalized = store
        .complete_earn_reconciliation_job(
            job.id,
            "local-e2e-claim",
            &EarnDirectMutation::Refund(EarnRefundMutation {
                cluster: "mainnet".to_owned(),
                full_cleanup: false,
                settings: "local-e2e-settings".to_owned(),
                vault_index: 0,
                vault_pubkey: "local-e2e-vault".to_owned(),
                wallet: "local-e2e-wallet".to_owned(),
                refund_signature: "local-e2e-refund-signature".to_owned(),
                confirmed_slot: 42,
                refund_kind: "policy".to_owned(),
                observed_at: None,
            }),
        )
        .await
        .is_ok();

    let report = Report {
        operational_alerts: usize::from(!transaction_classification.unrelated_single_mint_is_noop)
            + usize::from(!transaction_classification.unrelated_multi_mint_is_noop)
            + usize::from(!legacy_unknown_policy_accepted)
            + usize::from(!mainnet_refund_normalized),
        transaction_classification,
        legacy_unknown_policy_accepted,
        mainnet_refund_normalized,
    };
    fs::write(&output, serde_json::to_vec_pretty(&report)?)
        .with_context(|| format!("write {}", output.display()))?;

    if !report
        .transaction_classification
        .unrelated_single_mint_is_noop
        || !report
            .transaction_classification
            .unrelated_multi_mint_is_noop
        || !report
            .transaction_classification
            .earn_anchored_single_mint_is_detected
        || report.transaction_classification.retry_alert_emissions != 1
        || !report.legacy_unknown_policy_accepted
        || !report.mainnet_refund_normalized
        || report.operational_alerts != 0
    {
        eprintln!("{}", serde_json::to_string_pretty(&report)?);
        bail!("Earn reconciliation regression contract failed")
    }
    println!("PASS: unrelated wallet load completed as no-op, legacy policy and refund paths completed, zero alerts");
    Ok(())
}

fn parse_args() -> Result<(String, PathBuf)> {
    let mut args = env::args().skip(1);
    let mut postgres_url = None;
    let mut output = None;
    while let Some(arg) = args.next() {
        match arg.as_str() {
            "--postgres-url" => postgres_url = args.next(),
            "--output" => output = args.next().map(PathBuf::from),
            other => bail!("unknown argument {other}"),
        }
    }
    Ok((
        postgres_url.context("--postgres-url is required")?,
        output.context("--output is required")?,
    ))
}

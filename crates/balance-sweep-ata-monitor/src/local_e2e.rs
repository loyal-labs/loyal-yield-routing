use serde::Serialize;
use serde_json::{json, Value};

use crate::{
    earn_reconciliation::{classify_transaction_cash_flow, should_emit_reconciliation_retry_alert},
    EarnVaultWatch, EarnWatchAccount, NormalizedEarnUpdate,
};

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct EarnReconciliationRegressionReport {
    pub unrelated_single_mint_is_noop: bool,
    pub unrelated_multi_mint_is_noop: bool,
    pub earn_anchored_single_mint_is_detected: bool,
    pub retry_alert_emissions: usize,
}

pub fn earn_reconciliation_regression_report() -> EarnReconciliationRegressionReport {
    let vault = EarnVaultWatch {
        environment: "mainnet".to_owned(),
        settings: "local-e2e-settings".to_owned(),
        wallet: "local-e2e-wallet".to_owned(),
        earn_max: false,
        vault: "local-e2e-vault".to_owned(),
        vault_index: 0,
        accounts: vec![EarnWatchAccount {
            pubkey: "local-e2e-vault".to_owned(),
            role: "vault".to_owned(),
        }],
    };
    let update = NormalizedEarnUpdate {
        event_key: Some("local-e2e-wallet-update".to_owned()),
        filters: vec!["earn_wallet_token_accounts".to_owned()],
        event_kind: "account_updated".to_owned(),
        account_pubkey: Some("local-e2e-wallet-token".to_owned()),
        slot: 42,
        signature: Some("local-e2e-signature".to_owned()),
    };
    let mint_a = "local-e2e-mint-a";
    let mint_b = "local-e2e-mint-b";

    let unrelated_single = classify_transaction_cash_flow(
        &cash_flow_transaction(&vault.wallet, &[mint_a], false),
        &vault.wallet,
        [mint_a],
        &update,
        &vault,
    );
    let unrelated_multi = classify_transaction_cash_flow(
        &cash_flow_transaction(&vault.wallet, &[mint_a, mint_b], false),
        &vault.wallet,
        [mint_a, mint_b],
        &update,
        &vault,
    );
    let anchored_single = classify_transaction_cash_flow(
        &cash_flow_transaction(&vault.wallet, &[mint_a], true),
        &vault.wallet,
        [mint_a],
        &update,
        &vault,
    );

    EarnReconciliationRegressionReport {
        unrelated_single_mint_is_noop: matches!(unrelated_single, Ok(None)),
        unrelated_multi_mint_is_noop: matches!(unrelated_multi, Ok(None)),
        earn_anchored_single_mint_is_detected: matches!(anchored_single, Ok(Some(_))),
        retry_alert_emissions: (1..=4)
            .filter(|attempt_count| {
                should_emit_reconciliation_retry_alert(
                    crate::earn_reconciliation::EarnReconciliationDeferralKind::Failure,
                    *attempt_count,
                )
            })
            .count(),
    }
}

fn cash_flow_transaction(wallet: &str, mints: &[&str], earn_anchored: bool) -> Value {
    let pre = mints
        .iter()
        .enumerate()
        .map(|(index, mint)| token_balance(index, wallet, mint, 100))
        .collect::<Vec<_>>();
    let post = mints
        .iter()
        .enumerate()
        .map(|(index, mint)| token_balance(index, wallet, mint, 50))
        .collect::<Vec<_>>();
    let mut account_keys = vec![json!(wallet)];
    if earn_anchored {
        account_keys.push(json!("local-e2e-vault"));
    }
    json!({
        "slot": 42,
        "meta": {
            "preTokenBalances": pre,
            "postTokenBalances": post,
            "preBalances": [1_000_000],
            "postBalances": [995_000],
            "fee": 5_000
        },
        "transaction": { "message": { "accountKeys": account_keys } }
    })
}

fn token_balance(index: usize, owner: &str, mint: &str, amount: u64) -> Value {
    json!({
        "accountIndex": index,
        "owner": owner,
        "mint": mint,
        "uiTokenAmount": { "amount": amount.to_string() }
    })
}

//! Source-backed initial state for the connected Go Autodeposit acceptance test.
//! Real Squads/Subscriptions/SPL programs create the authorization artifacts.
//! KLend inventory is a clearly labeled fixed-price mock, never mainnet proof.
#[path = "workers_v2_autodeposit/mock_topup.rs"]
mod mock_topup;
use base64::{engine::general_purpose::STANDARD, Engine};
use loyal_actions::build_canonical_subscription_sweep_policy_create;
use serde_json::{json, Map, Value};
use sha2::{Digest, Sha256};
use solana_sdk::rent::Rent;
use solana_sdk::signature::{Keypair, SeedDerivable, Signer};
use squads_test_harness::prelude::*;

const NONCE: u64 = 7;
const BUDGET: u64 = 250_000;
const WALLET_AMOUNT: u64 = 1_000_000;
const AMOUNT: u64 = 100_000;

#[test]
fn source_authorizations_export_connected_go_initial_state() {
    let mut c = create_funded_squads_test_context_with_config(FundedSquadsTestConfig {
        vault_index: 1,
        ..FundedSquadsTestConfig::default()
    })
    .unwrap()
    .expect("real Squads SBF fixture required");
    c.svm.warp_to_slot(1000);
    let subscriptions_path = add_subscriptions_program_from_env_or_fixture(&mut c.svm)
        .unwrap()
        .expect("real Subscriptions SBF fixture required");
    // Retained Subscriptions SBF initialization profile, matching the existing
    // subscription_sweep_policy harness. Financial execution restores default
    // Rent and receives genuinely funded rent-exempt authorization accounts.
    c.svm.set_sysvar(&Rent {
        lamports_per_byte_year: 1,
        exemption_threshold: 1.0,
        burn_percent: 0,
    });
    let wallet = c.wallet_pubkey();
    let automation = Keypair::from_seed(&[13; 32]).unwrap();
    c.svm
        .airdrop(&automation.pubkey(), LAMPORTS_PER_SOL)
        .unwrap();
    let unauthorized = Keypair::from_seed(&[20; 32]).unwrap();
    c.svm
        .airdrop(&unauthorized.pubkey(), LAMPORTS_PER_SOL)
        .unwrap();
    // Initial total inventory includes both the wallet and mock reserve supply.
    seed_spl_mint_if_missing(
        &mut c.svm,
        USDC_MINT,
        None,
        USDC_DECIMALS,
        WALLET_AMOUNT + 1_000_000,
    );
    let wallet_ata = derive_associated_token_account(wallet, USDC_MINT);
    let vault_ata = derive_associated_token_account(c.vault, USDC_MINT);
    seed_spl_token_account(&mut c.svm, wallet_ata, USDC_MINT, wallet, WALLET_AMOUNT);
    seed_spl_token_account(&mut c.svm, vault_ata, USDC_MINT, c.vault, 0);
    let authority = derive_subscription_authority(wallet, USDC_MINT);
    try_send_instructions(
        &mut c.svm,
        &[subscription_init_authority_instruction(
            wallet, authority, USDC_MINT, wallet_ata,
        )],
        &c.wallet,
        &[],
    )
    .unwrap();
    // Existing real Subscriptions harness contract, subscription_sweep_policy.rs:518.
    let init_id = i64::from_le_bytes(
        c.svm.get_account(&authority).unwrap().data[98..106]
            .try_into()
            .unwrap(),
    );
    let delegation = derive_recurring_delegation(authority, wallet, c.vault, NONCE);
    try_send_instructions(
        &mut c.svm,
        &[subscription_create_recurring_delegation_instruction(
            SubscriptionRecurringDelegationArgs {
                delegator: wallet,
                subscription_authority: authority,
                delegation,
                delegatee: c.vault,
                nonce: NONCE,
                amount_per_period: BUDGET,
                period_length_s: 3600,
                start_ts: 0,
                expiry_ts: i64::MAX,
                expected_subscription_authority_init_id: init_id,
            },
        )],
        &c.wallet,
        &[],
    )
    .unwrap();
    // A fresh Settings account allocates policy seed1. Jumping to seed9 is
    // rejected; this is a state-counter mismatch, not a different creator ABI.
    let seed_gap_creator = build_canonical_subscription_sweep_policy_create(
        c.pool.settings,
        wallet,
        wallet,
        automation.pubkey(),
        9,
        wallet,
        c.vault,
        BUDGET,
    )
    .unwrap();
    let seed_gap_failure =
        try_send_instructions(&mut c.svm, &[seed_gap_creator], &c.wallet, &[]).unwrap_err();
    assert!(
        seed_gap_failure.contains("Custom(6024)"),
        "unexpected seed-gap error: {seed_gap_failure}"
    );
    let policy_seed = 1;
    let (policy, _) = derive_squads_policy(&c.pool.settings, policy_seed);
    let creator = build_canonical_subscription_sweep_policy_create(
        c.pool.settings,
        wallet,
        wallet,
        automation.pubkey(),
        policy_seed,
        wallet,
        c.vault,
        BUDGET,
    )
    .unwrap();
    c.svm.expire_blockhash();
    let creator_tx = solana_sdk::transaction::Transaction::new_signed_with_payer(
        &[creator],
        Some(&wallet),
        &[&c.wallet],
        c.svm.latest_blockhash(),
    );
    let creator_wire = bincode::serialize(&creator_tx).unwrap();
    assert!(
        creator_wire.len() <= 1232,
        "canonical creator must be a legal packet"
    );
    let creator_pre: Vec<u64> = creator_tx
        .message
        .account_keys
        .iter()
        .map(|key| c.svm.get_account(key).map(|a| a.lamports).unwrap_or(0))
        .collect();
    let creator_meta = c
        .svm
        .send_transaction(creator_tx.clone())
        .expect("actual canonical SDK policy creation through real Squads SBF");
    let creator_post: Vec<u64> = creator_tx
        .message
        .account_keys
        .iter()
        .map(|key| c.svm.get_account(key).map(|a| a.lamports).unwrap_or(0))
        .collect();
    assert_eq!(get_spl_token_amount(&c.svm, wallet_ata), WALLET_AMOUNT);
    assert_eq!(get_spl_token_amount(&c.svm, vault_ata), 0);
    assert_eq!(
        c.svm.get_account(&policy).unwrap().owner,
        SQUADS_SMART_ACCOUNT_PROGRAM_ID
    );
    assert_eq!(
        c.svm.get_account(&delegation).unwrap().owner,
        SUBSCRIPTIONS_PROGRAM_ID
    );
    let (mock_accounts, mock_topup) =
        mock_topup::initialize(&mut c, &automation, vault_ata, AMOUNT);
    let default_rent = Rent::default();
    c.svm.set_sysvar(&default_rent);
    for key in [
        c.pool.settings,
        policy,
        authority,
        delegation,
        wallet_ata,
        vault_ata,
        USDC_MINT,
    ]
    .into_iter()
    .chain(mock_accounts.iter().copied())
    {
        let account = c.svm.get_account(&key).unwrap();
        let required = default_rent.minimum_balance(account.data.len());
        if account.lamports < required {
            try_send_instructions(
                &mut c.svm,
                &[solana_sdk::system_instruction::transfer(
                    &wallet,
                    &key,
                    required - account.lamports,
                )],
                &c.wallet,
                &[],
            )
            .unwrap();
        }
    }
    let mut accounts = Map::<String, Value>::new();
    for key in [
        c.pool.settings,
        wallet,
        c.vault,
        automation.pubkey(),
        unauthorized.pubkey(),
        wallet_ata,
        vault_ata,
        USDC_MINT,
        authority,
        delegation,
        policy,
    ]
    .into_iter()
    .chain(mock_accounts.iter().copied())
    {
        let a = c.svm.get_account(&key).unwrap();
        accounts.insert(key.to_string(), json!({"Address":key.to_string(),"Owner":a.owner.to_string(),"Lamports":a.lamports,"Data":STANDARD.encode(a.data),"Executable":a.executable}));
    }
    let fixture = json!({"schemaVersion":1,"provenance": {
        "producer":"squads-test-harness/tests/workers_v2_autodeposit.rs source_authorizations_export_connected_go_initial_state",
        "squadsProgramSha256":format!("{:x}",Sha256::digest(std::fs::read(&c.loaded_program_path).unwrap())),
        "subscriptionsProgramSha256":format!("{:x}",Sha256::digest(std::fs::read(subscriptions_path).unwrap())),
        "mockProgramSha256":format!("{:x}",Sha256::digest(std::fs::read(std::env::var("MOCK_YIELD_PROTOCOLS_PROGRAM_SO").expect("configured fixed-price mock SBF required")).unwrap())),
        "initializationRentProfile":"retained low-rent Subscriptions harness; actual wallet SOL topups and default Rent restored before snapshot",
        "scope":"real Squads/Subscriptions/SPL authorization accounts; connected Go builds the financial execution packet; no KLend production claim"
    },"settings":c.pool.settings.to_string(),"wallet":wallet.to_string(),"vault":c.vault.to_string(),
    "walletATA":wallet_ata.to_string(),"vaultATA":vault_ata.to_string(),"subscriptionAuthority":authority.to_string(),
    "recurringDelegation":delegation.to_string(),"policy":policy.to_string(),"policySeed":policy_seed,
    "nonce":NONCE,"budgetRaw":BUDGET,"amountRaw":AMOUNT,"walletBeforeRaw":WALLET_AMOUNT,
    "periodLength":3600,"startTimestamp":0,"expiryTimestamp":i64::MAX,
    "executorSeedBase64":STANDARD.encode([13;32]),"accounts":accounts,
    "policyCreator": {"signature":creator_meta.signature.to_string(),"slot":1000,"wireBase64":STANDARD.encode(creator_wire),"preLamports":creator_pre,"postLamports":creator_post,"logs":creator_meta.logs},
    "seedGapRejected":true,"mockTopUp":mock_topup});
    if let Some(path) = std::env::var_os("WORKERS_V2_AUTODEPOSIT_SVM_FIXTURE_OUTPUT") {
        std::fs::write(path, serde_json::to_vec_pretty(&fixture).unwrap()).unwrap();
    }
}

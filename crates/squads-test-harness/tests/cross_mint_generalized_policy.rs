//! External-contract coverage for the durable two-shard cross-mint policy set.
//!
//! This test intentionally uses the production Rust action bytes and the
//! loaded Squads SBF.  The local Jupiter fixture is the strongest available
//! executable contract, but it only implements the captured USDC -> PYUSD
//! RouteV2 path.  PYUSD is seeded with the SPL Token mock representation here
//! so that its canonical Token-2022 ATA can still be checked by the policy;
//! this does not claim to emulate a Token-2022 CPI or the reverse Jupiter
//! route.  Those gaps remain covered by the live build/parser verifiers.

mod common;

use common::{
    decode_jupiter_swap_data, jupiter_fixture_transaction, load_jupiter_usdc_pyusd_fixture,
    parse_fixture_amount, seed_jupiter_fixture_accounts,
};
use loyal_actions::{
    create_jupiter_cross_mint_policy_set, derive_associated_token_account,
    detect_jupiter_cross_mint_policy_account, earn_stablecoin,
    jupiter::{
        JupiterCrossMintPolicySeeds, JupiterCrossMintSourceShard, JupiterV2Dialect,
        SOLANA_PACKET_DATA_SIZE,
    },
    EarnStablecoinPair, LoyalActionStep, PYUSD_MINT, USDC_MINT,
};
use solana_sdk::{
    instruction::AccountMeta,
    message::Message,
    pubkey::Pubkey,
    signature::{Keypair, Signer},
    transaction::Transaction,
};
use squads_test_harness::{
    create_funded_squads_test_context_with_mock_programs,
    execute_squads_program_interaction_instruction, get_spl_token_amount, loyal_action_context,
    seed_mock_jupiter_spl_accounts, seed_spl_mint_if_missing, seed_spl_token_account,
    try_send_instructions, MockProgram, SquadsCompiledInstruction,
};

const CLASSIC_POLICY_SEED: u64 = 1;
const TOKEN_2022_POLICY_SEED: u64 = 2;
const MAX_SLIPPAGE_BPS: u16 = 50;
const DAILY_SOURCE_CAP: u64 = 1_500_000;

#[test]
fn generalized_cross_mint_policy_set_creates_reads_executes_and_rejects_mutations() {
    let mut context = create_funded_squads_test_context_with_mock_programs(&[MockProgram::Jupiter])
        .expect("create funded Squads test context");
    let context = context
        .as_mut()
        .expect("generalized cross-mint policy test requires the Squads SBF fixture");

    let delegated_signer = Keypair::new();
    context
        .svm
        .airdrop(&delegated_signer.pubkey(), 100_000_000)
        .expect("airdrop delegated signer");
    let action_context = loyal_action_context(context, delegated_signer.pubkey());
    let policy_set = create_jupiter_cross_mint_policy_set(
        action_context,
        MAX_SLIPPAGE_BPS,
        DAILY_SOURCE_CAP,
        JupiterCrossMintPolicySeeds {
            classic: CLASSIC_POLICY_SEED,
            token_2022: TOKEN_2022_POLICY_SEED,
        },
    )
    .expect("build generalized cross-mint policy set");

    assert_create_fits_and_executes(context, &policy_set.classic.instruction, "classic shard");
    assert_create_fits_and_executes(
        context,
        &policy_set.token_2022.instruction,
        "Token-2022 shard",
    );

    let classic_policy = read_policy(context, policy_set.classic.account);
    assert_eq!(
        classic_policy.source_shard,
        JupiterCrossMintSourceShard::Classic
    );
    assert_eq!(classic_policy.max_slippage_bps, MAX_SLIPPAGE_BPS);
    assert_eq!(
        classic_policy.daily_source_mint_spending_cap,
        DAILY_SOURCE_CAP
    );
    assert_eq!(
        classic_policy.dialect_constraint_indexes[&JupiterV2Dialect::RouteV2],
        0
    );
    assert_eq!(
        classic_policy.dialect_constraint_indexes[&JupiterV2Dialect::SharedAccountsRouteV2],
        1
    );

    let token_2022_policy = read_policy(context, policy_set.token_2022.account);
    assert_eq!(
        token_2022_policy.source_shard,
        JupiterCrossMintSourceShard::Token2022
    );
    assert_eq!(token_2022_policy.max_slippage_bps, MAX_SLIPPAGE_BPS);
    assert_eq!(
        token_2022_policy.daily_source_mint_spending_cap,
        DAILY_SOURCE_CAP
    );
    assert_eq!(
        token_2022_policy.dialect_constraint_indexes[&JupiterV2Dialect::RouteV2],
        0
    );
    assert_eq!(
        token_2022_policy.dialect_constraint_indexes[&JupiterV2Dialect::SharedAccountsRouteV2],
        1
    );

    let fixture = load_jupiter_usdc_pyusd_fixture();
    let in_amount = parse_fixture_amount(&fixture.in_amount);
    let out_amount = parse_fixture_amount(&fixture.out_amount);
    let vault_usdc = canonical_ata(context.vault, USDC_MINT);
    let vault_pyusd = canonical_ata(context.vault, PYUSD_MINT);

    // The fixture is a real RouteV2 instruction.  The mock uses SPL Token
    // accounts, while the production registry intentionally derives PYUSD's
    // canonical ATA with Token-2022; that lets this test cover the policy's
    // canonical-destination check without misrepresenting the mock's scope.
    seed_mock_jupiter_spl_accounts(&mut context.svm, in_amount * 2, out_amount * 3);
    seed_spl_token_account(
        &mut context.svm,
        vault_usdc,
        USDC_MINT,
        context.vault,
        in_amount * 2,
    );
    seed_spl_token_account(&mut context.svm, vault_pyusd, PYUSD_MINT, context.vault, 0);

    let usdc_to_pyusd = EarnStablecoinPair::new(USDC_MINT, PYUSD_MINT).unwrap();
    let classic_route_v2 = policy_set
        .classic
        .step_for_pair(usdc_to_pyusd, JupiterV2Dialect::RouteV2)
        .expect("classic source selects RouteV2 constraint index 0");
    assert_eq!(classic_route_v2.instruction_constraint_index(), 0);
    let first_accounts = fixture_accounts(&fixture, context.vault, vault_usdc, vault_pyusd);
    seed_jupiter_fixture_accounts(&mut context.svm, &fixture, &first_accounts.0);
    let first_swap = fixture_swap(
        &fixture,
        classic_route_v2,
        &delegated_signer,
        first_accounts,
        decode_jupiter_swap_data(&fixture),
    );
    try_send_instructions(&mut context.svm, &[first_swap], &delegated_signer, &[])
        .expect("classic source -> canonical PYUSD ATA fixture swap executes");
    assert_eq!(get_spl_token_amount(&context.svm, vault_usdc), in_amount);
    assert_eq!(get_spl_token_amount(&context.svm, vault_pyusd), out_amount);

    // Spending limits belong to the source mint, not to a destination or
    // dialect constraint.  A second 1,000,000-unit movement exceeds the one
    // USDC cap even though the destination and fixture are otherwise valid.
    let before_over_cap = get_spl_token_amount(&context.svm, vault_pyusd);
    let second_accounts = fixture_accounts(&fixture, context.vault, vault_usdc, vault_pyusd);
    let second_swap = fixture_swap(
        &fixture,
        classic_route_v2,
        &delegated_signer,
        second_accounts,
        decode_jupiter_swap_data(&fixture),
    );
    assert!(
        try_send_instructions(&mut context.svm, &[second_swap], &delegated_signer, &[]).is_err(),
        "source cap must aggregate across repeated destinations"
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_pyusd),
        before_over_cap
    );

    // The source-shard selection is part of the production API.  Both
    // cross-token directions are represented, and SharedAccountsRouteV2 is
    // intentionally index 1 even though this local mock cannot execute it.
    let pyusd_to_usdc = usdc_to_pyusd.reversed();
    let token_2022_route_v2 = policy_set
        .token_2022
        .step_for_pair(pyusd_to_usdc, JupiterV2Dialect::RouteV2)
        .expect("Token-2022 source selects RouteV2 constraint index 0");
    assert_eq!(token_2022_route_v2.instruction_constraint_index(), 0);
    let token_2022_shared = policy_set
        .token_2022
        .step_for_pair(pyusd_to_usdc, JupiterV2Dialect::SharedAccountsRouteV2)
        .expect("Token-2022 source selects SharedAccountsRouteV2 index 1");
    assert_eq!(token_2022_shared.instruction_constraint_index(), 1);

    // This is the strongest reverse-direction assertion available from the
    // fixture: the generalized policy selects the right source shard, while
    // the captured local mock rejects the reverse route because it only
    // implements the USDC -> PYUSD router contract.
    let reverse_accounts = fixture_accounts(&fixture, context.vault, vault_pyusd, vault_usdc);
    let reverse_swap = fixture_swap(
        &fixture,
        token_2022_route_v2,
        &delegated_signer,
        reverse_accounts,
        decode_jupiter_swap_data(&fixture),
    );
    assert!(
        try_send_instructions(&mut context.svm, &[reverse_swap], &delegated_signer, &[]).is_err(),
        "local Jupiter fixture must reject its unsupported reverse direction"
    );

    // Wrong source shard: the classic policy has no PYUSD spending limit.
    let wrong_source = fixture_accounts(&fixture, context.vault, vault_pyusd, vault_usdc);
    let wrong_source_swap = fixture_swap(
        &fixture,
        classic_route_v2,
        &delegated_signer,
        wrong_source,
        decode_jupiter_swap_data(&fixture),
    );
    assert!(
        try_send_instructions(
            &mut context.svm,
            &[wrong_source_swap],
            &delegated_signer,
            &[]
        )
        .is_err(),
        "wrong source shard must not authorize a PYUSD decrease"
    );

    // Unsupported source mint: no spending-limit entry means a vault-token
    // decrease cannot pass the Squads policy envelope.
    let unsupported_mint = Pubkey::new_unique();
    let unsupported_source =
        derive_associated_token_account(context.vault, unsupported_mint, spl_token::ID);
    seed_spl_mint_if_missing(&mut context.svm, unsupported_mint, None, 6, 0);
    seed_spl_token_account(
        &mut context.svm,
        unsupported_source,
        unsupported_mint,
        context.vault,
        in_amount,
    );
    let unsupported_accounts =
        fixture_accounts(&fixture, context.vault, unsupported_source, vault_usdc);
    let unsupported_swap = fixture_swap(
        &fixture,
        classic_route_v2,
        &delegated_signer,
        unsupported_accounts,
        decode_jupiter_swap_data(&fixture),
    );
    assert!(
        try_send_instructions(
            &mut context.svm,
            &[unsupported_swap],
            &delegated_signer,
            &[]
        )
        .is_err(),
        "unsupported source mint must not be spendable without a limit"
    );

    // The destination must be one of the canonical vault ATAs.
    let noncanonical_destination = Keypair::new().pubkey();
    seed_spl_token_account(
        &mut context.svm,
        noncanonical_destination,
        PYUSD_MINT,
        context.vault,
        0,
    );
    let noncanonical_accounts = fixture_accounts(
        &fixture,
        context.vault,
        vault_usdc,
        noncanonical_destination,
    );
    let noncanonical_swap = fixture_swap(
        &fixture,
        classic_route_v2,
        &delegated_signer,
        noncanonical_accounts,
        decode_jupiter_swap_data(&fixture),
    );
    assert!(
        try_send_instructions(
            &mut context.svm,
            &[noncanonical_swap],
            &delegated_signer,
            &[]
        )
        .is_err(),
        "noncanonical destination must be rejected"
    );

    let mut excessive_slippage = decode_jupiter_swap_data(&fixture);
    excessive_slippage[24..26].copy_from_slice(&(MAX_SLIPPAGE_BPS + 1).to_le_bytes());
    assert_mutation_rejected(
        context,
        &fixture,
        classic_route_v2,
        &delegated_signer,
        (vault_usdc, vault_pyusd),
        excessive_slippage,
        "slippage above the policy bound",
    );

    let mut nonzero_fee = decode_jupiter_swap_data(&fixture);
    nonzero_fee[26] = 1;
    assert_mutation_rejected(
        context,
        &fixture,
        classic_route_v2,
        &delegated_signer,
        (vault_usdc, vault_pyusd),
        nonzero_fee,
        "nonzero platform fee",
    );

    let mut wrong_dialect = decode_jupiter_swap_data(&fixture);
    wrong_dialect[..8].copy_from_slice(&JupiterV2Dialect::SharedAccountsRouteV2.discriminator());
    assert_mutation_rejected(
        context,
        &fixture,
        classic_route_v2,
        &delegated_signer,
        (vault_usdc, vault_pyusd),
        wrong_dialect,
        "SharedAccountsRouteV2 data under RouteV2 index 0",
    );
}

fn assert_create_fits_and_executes(
    context: &mut squads_test_harness::FundedSquadsTestContext,
    instruction: &solana_sdk::instruction::Instruction,
    label: &str,
) {
    context.svm.expire_blockhash();
    let message = Message::new_with_blockhash(
        std::slice::from_ref(instruction),
        Some(&context.wallet.pubkey()),
        &context.svm.latest_blockhash(),
    );
    let transaction = Transaction::new(
        std::slice::from_ref(&context.wallet),
        message,
        context.svm.latest_blockhash(),
    );
    let packet_bytes = bincode::serialize(&transaction).expect("serialize policy create packet");
    assert!(
        packet_bytes.len() <= SOLANA_PACKET_DATA_SIZE,
        "{label} policy create packet is {} bytes, above {SOLANA_PACKET_DATA_SIZE}",
        packet_bytes.len()
    );
    try_send_instructions(
        &mut context.svm,
        std::slice::from_ref(instruction),
        &context.wallet,
        &[],
    )
    .unwrap_or_else(|error| panic!("{label} policy create must execute: {error}"));
}

fn read_policy(
    context: &squads_test_harness::FundedSquadsTestContext,
    policy: Pubkey,
) -> loyal_actions::DetectedJupiterCrossMintPolicyAccount {
    let account = context
        .svm
        .get_account(&policy)
        .expect("created generalized policy account");
    detect_jupiter_cross_mint_policy_account(&account.data)
        .expect("decode generalized policy account")
        .expect("created account must be a generalized cross-mint policy")
}

fn canonical_ata(vault: Pubkey, mint: Pubkey) -> Pubkey {
    let stablecoin = earn_stablecoin(mint).expect("canonical Earn stablecoin");
    derive_associated_token_account(vault, mint, stablecoin.token_program)
}

type FixtureAccounts = (Vec<AccountMeta>, Vec<usize>, usize);

fn fixture_accounts(
    fixture: &common::JupiterBuildFixture,
    vault: Pubkey,
    vault_input: Pubkey,
    vault_output: Pubkey,
) -> FixtureAccounts {
    jupiter_fixture_transaction(fixture, vault, vault_input, vault_output)
}

fn fixture_swap(
    _fixture: &common::JupiterBuildFixture,
    step: LoyalActionStep,
    signer: &Keypair,
    fixture_accounts: FixtureAccounts,
    data: Vec<u8>,
) -> solana_sdk::instruction::Instruction {
    let (transaction_accounts, instruction_accounts, program_id_index) = fixture_accounts;
    execute_squads_program_interaction_instruction(
        step.action_account(),
        signer.pubkey(),
        0,
        vec![SquadsCompiledInstruction {
            program_id_index,
            accounts: instruction_accounts,
            data,
        }],
        vec![step.instruction_constraint_index()],
        transaction_accounts,
    )
}

fn assert_mutation_rejected(
    context: &mut squads_test_harness::FundedSquadsTestContext,
    fixture: &common::JupiterBuildFixture,
    step: LoyalActionStep,
    signer: &Keypair,
    vault_token_accounts: (Pubkey, Pubkey),
    data: Vec<u8>,
    label: &str,
) {
    let (vault_input, vault_output) = vault_token_accounts;
    let before_input = get_spl_token_amount(&context.svm, vault_input);
    let before_output = get_spl_token_amount(&context.svm, vault_output);
    let accounts = fixture_accounts(fixture, context.vault, vault_input, vault_output);
    let instruction = fixture_swap(fixture, step, signer, accounts, data);
    assert!(
        try_send_instructions(&mut context.svm, &[instruction], signer, &[]).is_err(),
        "{label} must be rejected"
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_input),
        before_input
    );
    assert_eq!(
        get_spl_token_amount(&context.svm, vault_output),
        before_output
    );
}

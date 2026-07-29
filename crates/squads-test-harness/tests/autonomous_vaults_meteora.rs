use loyal_actions::{
    autonomous_vaults::{
        create_meteora_policies, derive_meteora_vault_token_accounts, MeteoraPolicyError,
        METEORA_ADD_LIQUIDITY_BY_STRATEGY2_DISCRIMINATOR, METEORA_CLAIM_FEE2_DISCRIMINATOR,
        METEORA_CLASSIC_TOKEN_REMAINING_ACCOUNTS_INFO, METEORA_DLMM_PROGRAM_ID,
        METEORA_EVENT_AUTHORITY, METEORA_LOYAL_MINT, METEORA_LOYAL_RESERVE,
        METEORA_MEMO_PROGRAM_ID, METEORA_POOL, METEORA_REMOVE_LIQUIDITY_BY_RANGE2_DISCRIMINATOR,
        METEORA_SPOT_BALANCED_STRATEGY, METEORA_USDC_RESERVE,
    },
    decode_squads_policy_create_actions, derive_action_account, SquadsAccountConstraintKindView,
    SquadsDataConstraintView, SquadsDataOperatorView, SquadsDataValueView,
    SquadsInstructionConstraintView, USDC_MINT,
};
use solana_sdk::{
    hash::Hash,
    instruction::{AccountMeta, Instruction},
    message::{v0::Message, VersionedMessage},
    packet::PACKET_DATA_SIZE,
    pubkey::Pubkey,
    signature::{Keypair, Signer},
    transaction::VersionedTransaction,
};
use std::collections::BTreeMap;

const ADD_POLICY_DATA_BYTES: usize = 941;
const REMOVE_POLICY_DATA_BYTES: usize = 887;
const CLAIM_POLICY_DATA_BYTES: usize = 848;

#[test]
fn independently_decodes_and_adversarially_matches_three_meteora_policies() {
    let settings = Pubkey::new_unique();
    let authority = Keypair::new();
    let delegated_signer = Pubkey::new_unique();
    let vault = Pubkey::new_unique();
    let position = Pubkey::new_unique();
    let lower_bin_arrays = vec![Pubkey::new_unique(), Pubkey::new_unique()];
    let upper_bin_arrays = vec![Pubkey::new_unique(), Pubkey::new_unique()];
    let vault_index = 2;
    let policies = create_meteora_policies(
        settings,
        authority.pubkey(),
        delegated_signer,
        vault,
        vault_index,
        43,
        44,
        45,
        vec![position],
        lower_bin_arrays.clone(),
        upper_bin_arrays.clone(),
    )
    .expect("build autonomous Meteora policies");

    let plans = [
        (&policies.add_liquidity, 43, ADD_POLICY_DATA_BYTES),
        (&policies.remove_liquidity, 44, REMOVE_POLICY_DATA_BYTES),
        (&policies.claim_fees, 45, CLAIM_POLICY_DATA_BYTES),
    ];
    let decoded = plans
        .iter()
        .map(|(plan, seed, expected_data_bytes)| {
            assert_eq!(plan.policy, derive_action_account(&settings, *seed).0);
            assert_eq!(plan.policy_seed, *seed);
            assert_eq!(plan.create_instruction.data.len(), *expected_data_bytes);
            let actions = decode_squads_policy_create_actions(&plan.create_instruction)
                .expect("independently decode Meteora policy create");
            assert_eq!(actions.len(), 1);
            let action = &actions[0];
            assert_eq!(action.settings, settings);
            assert_eq!(action.authority, authority.pubkey());
            assert_eq!(action.delegated_signers, vec![delegated_signer]);
            assert_eq!(action.threshold, 1);
            assert_eq!(action.payload.vault_index, vault_index);
            assert_eq!(action.payload.constraints.len(), 1);
            action.payload.constraints[0].clone()
        })
        .collect::<Vec<_>>();

    let (vault_loyal, vault_usdc) = derive_meteora_vault_token_accounts(vault);
    assert_account_graph(
        &decoded[0],
        &[
            (0, vec![position]),
            (1, vec![METEORA_POOL]),
            (2, vec![METEORA_DLMM_PROGRAM_ID]),
            (3, vec![vault_loyal]),
            (4, vec![vault_usdc]),
            (5, vec![METEORA_LOYAL_RESERVE]),
            (6, vec![METEORA_USDC_RESERVE]),
            (7, vec![METEORA_LOYAL_MINT]),
            (8, vec![USDC_MINT]),
            (9, vec![vault]),
            (10, vec![spl_token::id()]),
            (11, vec![spl_token::id()]),
            (12, vec![METEORA_EVENT_AUTHORITY]),
            (13, vec![METEORA_DLMM_PROGRAM_ID]),
            (14, sorted(lower_bin_arrays.clone())),
            (15, sorted(upper_bin_arrays.clone())),
        ],
    );
    assert_account_graph(
        &decoded[1],
        &[
            (0, vec![position]),
            (1, vec![METEORA_POOL]),
            (2, vec![METEORA_DLMM_PROGRAM_ID]),
            (3, vec![vault_loyal]),
            (4, vec![vault_usdc]),
            (5, vec![METEORA_LOYAL_RESERVE]),
            (6, vec![METEORA_USDC_RESERVE]),
            (7, vec![METEORA_LOYAL_MINT]),
            (8, vec![USDC_MINT]),
            (9, vec![vault]),
            (10, vec![spl_token::id()]),
            (11, vec![spl_token::id()]),
            (12, vec![METEORA_MEMO_PROGRAM_ID]),
            (13, vec![METEORA_EVENT_AUTHORITY]),
            (14, vec![METEORA_DLMM_PROGRAM_ID]),
            (15, sorted(lower_bin_arrays.clone())),
            (16, sorted(upper_bin_arrays.clone())),
        ],
    );
    assert_account_graph(
        &decoded[2],
        &[
            (0, vec![METEORA_POOL]),
            (1, vec![position]),
            (2, vec![vault]),
            (3, vec![METEORA_LOYAL_RESERVE]),
            (4, vec![METEORA_USDC_RESERVE]),
            (5, vec![vault_loyal]),
            (6, vec![vault_usdc]),
            (7, vec![METEORA_LOYAL_MINT]),
            (8, vec![USDC_MINT]),
            (9, vec![spl_token::id()]),
            (10, vec![spl_token::id()]),
            (11, vec![METEORA_MEMO_PROGRAM_ID]),
            (12, vec![METEORA_EVENT_AUTHORITY]),
            (13, vec![METEORA_DLMM_PROGRAM_ID]),
            (14, sorted(lower_bin_arrays.clone())),
            (15, sorted(upper_bin_arrays.clone())),
        ],
    );

    assert_eq!(
        decoded[0].data_constraints,
        vec![
            slice_equals(0, &METEORA_ADD_LIQUIDITY_BY_STRATEGY2_DISCRIMINATOR),
            SquadsDataConstraintView {
                data_offset: 28,
                data_value: SquadsDataValueView::U32Le(3),
                operator: SquadsDataOperatorView::LessThanOrEqualTo,
            },
            slice_equals(40, &METEORA_SPOT_BALANCED_STRATEGY),
            slice_equals(105, &METEORA_CLASSIC_TOKEN_REMAINING_ACCOUNTS_INFO),
        ]
    );
    assert_eq!(
        decoded[1].data_constraints,
        vec![
            slice_equals(0, &METEORA_REMOVE_LIQUIDITY_BY_RANGE2_DISCRIMINATOR),
            slice_equals(18, &METEORA_CLASSIC_TOKEN_REMAINING_ACCOUNTS_INFO),
        ]
    );
    assert_eq!(
        decoded[2].data_constraints,
        vec![
            slice_equals(0, &METEORA_CLAIM_FEE2_DISCRIMINATOR),
            slice_equals(16, &METEORA_CLASSIC_TOKEN_REMAINING_ACCOUNTS_INFO),
        ]
    );
    assert!(decoded[0]
        .data_constraints
        .iter()
        .all(|constraint| ![8, 16].contains(&constraint.data_offset)));

    let add_a = add_instruction(
        vault,
        vault_loyal,
        vault_usdc,
        position,
        lower_bin_arrays[0],
        upper_bin_arrays[0],
        7,
        11,
        -220,
        -190,
    );
    let add_b = add_instruction(
        vault,
        vault_loyal,
        vault_usdc,
        position,
        lower_bin_arrays[1],
        upper_bin_arrays[1],
        u64::MAX,
        u64::MAX,
        -200,
        -175,
    );
    assert!(constraint_matches(&decoded[0], &add_a));
    assert!(constraint_matches(&decoded[0], &add_b));

    let remove_a = remove_instruction(
        vault,
        vault_loyal,
        vault_usdc,
        position,
        lower_bin_arrays[0],
        upper_bin_arrays[0],
        -220,
        -190,
        1,
    );
    let remove_b = remove_instruction(
        vault,
        vault_loyal,
        vault_usdc,
        position,
        lower_bin_arrays[1],
        upper_bin_arrays[1],
        -200,
        -175,
        10_000,
    );
    assert!(constraint_matches(&decoded[1], &remove_a));
    assert!(constraint_matches(&decoded[1], &remove_b));

    let claim_a = claim_instruction(
        vault,
        vault_loyal,
        vault_usdc,
        position,
        lower_bin_arrays[0],
        upper_bin_arrays[0],
        -220,
        -190,
    );
    let claim_b = claim_instruction(
        vault,
        vault_loyal,
        vault_usdc,
        position,
        lower_bin_arrays[1],
        upper_bin_arrays[1],
        -200,
        -175,
    );
    assert!(constraint_matches(&decoded[2], &claim_a));
    assert!(constraint_matches(&decoded[2], &claim_b));

    for (name, constraint, valid) in [
        ("add", &decoded[0], &add_a),
        ("remove", &decoded[1], &remove_a),
        ("claim", &decoded[2], &claim_a),
    ] {
        for account_constraint in &constraint.account_constraints {
            let mut mutated = valid.clone();
            mutated.accounts[account_constraint.account_index as usize].pubkey =
                Pubkey::new_unique();
            assert!(
                !constraint_matches(constraint, &mutated),
                "{name} accepted mutated account {}",
                account_constraint.account_index
            );
        }
        let mut wrong_program = valid.clone();
        wrong_program.program_id = Pubkey::new_unique();
        assert!(!constraint_matches(constraint, &wrong_program));
        let mut wrong_discriminator = valid.clone();
        wrong_discriminator.data[0] ^= 1;
        assert!(!constraint_matches(constraint, &wrong_discriminator));
        let mut wrong_tail = valid.clone();
        let final_byte = wrong_tail.data.len() - 1;
        wrong_tail.data[final_byte] = 1;
        assert!(!constraint_matches(constraint, &wrong_tail));
    }

    let mut unsafe_slippage = add_a.clone();
    unsafe_slippage.data[28..32].copy_from_slice(&4u32.to_le_bytes());
    assert!(!constraint_matches(&decoded[0], &unsafe_slippage));
    let mut unreviewed_strategy = add_a;
    unreviewed_strategy.data[40] = 6;
    assert!(!constraint_matches(&decoded[0], &unreviewed_strategy));

    for (name, instruction, expected_packet_bytes) in [
        (
            "Meteora add liquidity",
            &policies.add_liquidity.create_instruction,
            1_215,
        ),
        (
            "Meteora remove liquidity",
            &policies.remove_liquidity.create_instruction,
            1_161,
        ),
        (
            "Meteora claim fees",
            &policies.claim_fees.create_instruction,
            1_122,
        ),
    ] {
        let packet_bytes = packet_bytes(instruction, &authority);
        println!("{name} policy_create_packet_bytes={packet_bytes}");
        assert_eq!(packet_bytes, expected_packet_bytes);
        assert!(packet_bytes <= PACKET_DATA_SIZE);
    }
}

#[test]
fn rejects_empty_allowlists_and_duplicate_policy_seeds() {
    let settings = Pubkey::new_unique();
    let authority = Pubkey::new_unique();
    let delegated = Pubkey::new_unique();
    let vault = Pubkey::new_unique();
    let position = Pubkey::new_unique();
    let lower = Pubkey::new_unique();
    let upper = Pubkey::new_unique();

    let build = |positions, lower_arrays, upper_arrays, seeds: [u64; 3]| {
        create_meteora_policies(
            settings,
            authority,
            delegated,
            vault,
            0,
            seeds[0],
            seeds[1],
            seeds[2],
            positions,
            lower_arrays,
            upper_arrays,
        )
    };
    assert!(matches!(
        build(vec![], vec![lower], vec![upper], [1, 2, 3]),
        Err(MeteoraPolicyError::NoApprovedPositions)
    ));
    assert!(matches!(
        build(vec![position], vec![], vec![upper], [1, 2, 3]),
        Err(MeteoraPolicyError::NoLowerBinArrayCandidates)
    ));
    assert!(matches!(
        build(vec![position], vec![lower], vec![], [1, 2, 3]),
        Err(MeteoraPolicyError::NoUpperBinArrayCandidates)
    ));
    assert!(matches!(
        build(vec![position], vec![lower], vec![upper], [1, 1, 3]),
        Err(MeteoraPolicyError::DuplicatePolicySeeds)
    ));
}

#[test]
fn final_expanded_shard_fits_packets_and_pins_the_zero_bin_window() {
    let settings = Pubkey::new_unique();
    let authority = Keypair::new();
    let delegated = Pubkey::new_unique();
    let vault = Pubkey::new_unique();
    let position = Pubkey::new_unique();
    let lower = Pubkey::new_unique();
    let upper = Pubkey::new_unique();
    let policies = create_meteora_policies(
        settings,
        authority.pubkey(),
        delegated,
        vault,
        0,
        11,
        12,
        13,
        vec![position],
        vec![lower],
        vec![upper],
    )
    .expect("build final expanded Meteora shard");
    let constraints = [
        &policies.add_liquidity.create_instruction,
        &policies.remove_liquidity.create_instruction,
        &policies.claim_fees.create_instruction,
    ]
    .map(|instruction| {
        let actions = decode_squads_policy_create_actions(instruction)
            .expect("decode final expanded Meteora shard");
        assert_eq!(actions.len(), 1);
        assert!(packet_bytes(instruction, &authority) <= PACKET_DATA_SIZE);
        actions[0].payload.constraints[0].clone()
    });
    let (vault_loyal, vault_usdc) = derive_meteora_vault_token_accounts(vault);
    let add = add_instruction(
        vault,
        vault_loyal,
        vault_usdc,
        position,
        lower,
        upper,
        1,
        1,
        0,
        0,
    );
    let remove = remove_instruction(
        vault,
        vault_loyal,
        vault_usdc,
        position,
        lower,
        upper,
        0,
        0,
        10_000,
    );
    let claim = claim_instruction(vault, vault_loyal, vault_usdc, position, lower, upper, 0, 0);
    assert!(constraint_matches(&constraints[0], &add));
    assert!(constraint_matches(&constraints[1], &remove));
    assert!(constraint_matches(&constraints[2], &claim));

    for ((constraint, valid), upper_index) in constraints
        .iter()
        .zip([add, remove, claim])
        .zip([15, 16, 15])
    {
        let mut wrong_upper = valid;
        wrong_upper.accounts[upper_index].pubkey = Pubkey::new_unique();
        assert!(!constraint_matches(constraint, &wrong_upper));
    }
}

fn add_instruction(
    vault: Pubkey,
    vault_loyal: Pubkey,
    vault_usdc: Pubkey,
    position: Pubkey,
    lower_bin_array: Pubkey,
    upper_bin_array: Pubkey,
    amount_x: u64,
    amount_y: u64,
    min_bin_id: i32,
    max_bin_id: i32,
) -> Instruction {
    let mut data = METEORA_ADD_LIQUIDITY_BY_STRATEGY2_DISCRIMINATOR.to_vec();
    data.extend_from_slice(&amount_x.to_le_bytes());
    data.extend_from_slice(&amount_y.to_le_bytes());
    data.extend_from_slice(&(-190i32).to_le_bytes());
    data.extend_from_slice(&3i32.to_le_bytes());
    data.extend_from_slice(&min_bin_id.to_le_bytes());
    data.extend_from_slice(&max_bin_id.to_le_bytes());
    data.extend_from_slice(&METEORA_SPOT_BALANCED_STRATEGY);
    data.extend_from_slice(&METEORA_CLASSIC_TOKEN_REMAINING_ACCOUNTS_INFO);
    assert_eq!(data.len(), 109);
    Instruction {
        program_id: METEORA_DLMM_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(position, false),
            AccountMeta::new(METEORA_POOL, false),
            AccountMeta::new(METEORA_DLMM_PROGRAM_ID, false),
            AccountMeta::new(vault_loyal, false),
            AccountMeta::new(vault_usdc, false),
            AccountMeta::new(METEORA_LOYAL_RESERVE, false),
            AccountMeta::new(METEORA_USDC_RESERVE, false),
            AccountMeta::new_readonly(METEORA_LOYAL_MINT, false),
            AccountMeta::new_readonly(USDC_MINT, false),
            AccountMeta::new_readonly(vault, true),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(METEORA_EVENT_AUTHORITY, false),
            AccountMeta::new_readonly(METEORA_DLMM_PROGRAM_ID, false),
            AccountMeta::new(lower_bin_array, false),
            AccountMeta::new(upper_bin_array, false),
        ],
        data,
    }
}

#[allow(clippy::too_many_arguments)]
fn remove_instruction(
    vault: Pubkey,
    vault_loyal: Pubkey,
    vault_usdc: Pubkey,
    position: Pubkey,
    lower_bin_array: Pubkey,
    upper_bin_array: Pubkey,
    from_bin_id: i32,
    to_bin_id: i32,
    bps_to_remove: u16,
) -> Instruction {
    let mut data = METEORA_REMOVE_LIQUIDITY_BY_RANGE2_DISCRIMINATOR.to_vec();
    data.extend_from_slice(&from_bin_id.to_le_bytes());
    data.extend_from_slice(&to_bin_id.to_le_bytes());
    data.extend_from_slice(&bps_to_remove.to_le_bytes());
    data.extend_from_slice(&METEORA_CLASSIC_TOKEN_REMAINING_ACCOUNTS_INFO);
    assert_eq!(data.len(), 22);
    Instruction {
        program_id: METEORA_DLMM_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(position, false),
            AccountMeta::new(METEORA_POOL, false),
            AccountMeta::new(METEORA_DLMM_PROGRAM_ID, false),
            AccountMeta::new(vault_loyal, false),
            AccountMeta::new(vault_usdc, false),
            AccountMeta::new(METEORA_LOYAL_RESERVE, false),
            AccountMeta::new(METEORA_USDC_RESERVE, false),
            AccountMeta::new_readonly(METEORA_LOYAL_MINT, false),
            AccountMeta::new_readonly(USDC_MINT, false),
            AccountMeta::new_readonly(vault, true),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(METEORA_MEMO_PROGRAM_ID, false),
            AccountMeta::new_readonly(METEORA_EVENT_AUTHORITY, false),
            AccountMeta::new_readonly(METEORA_DLMM_PROGRAM_ID, false),
            AccountMeta::new(lower_bin_array, false),
            AccountMeta::new(upper_bin_array, false),
        ],
        data,
    }
}

#[allow(clippy::too_many_arguments)]
fn claim_instruction(
    vault: Pubkey,
    vault_loyal: Pubkey,
    vault_usdc: Pubkey,
    position: Pubkey,
    lower_bin_array: Pubkey,
    upper_bin_array: Pubkey,
    min_bin_id: i32,
    max_bin_id: i32,
) -> Instruction {
    let mut data = METEORA_CLAIM_FEE2_DISCRIMINATOR.to_vec();
    data.extend_from_slice(&min_bin_id.to_le_bytes());
    data.extend_from_slice(&max_bin_id.to_le_bytes());
    data.extend_from_slice(&METEORA_CLASSIC_TOKEN_REMAINING_ACCOUNTS_INFO);
    assert_eq!(data.len(), 20);
    Instruction {
        program_id: METEORA_DLMM_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(METEORA_POOL, false),
            AccountMeta::new(position, false),
            AccountMeta::new_readonly(vault, true),
            AccountMeta::new(METEORA_LOYAL_RESERVE, false),
            AccountMeta::new(METEORA_USDC_RESERVE, false),
            AccountMeta::new(vault_loyal, false),
            AccountMeta::new(vault_usdc, false),
            AccountMeta::new_readonly(METEORA_LOYAL_MINT, false),
            AccountMeta::new_readonly(USDC_MINT, false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(METEORA_MEMO_PROGRAM_ID, false),
            AccountMeta::new_readonly(METEORA_EVENT_AUTHORITY, false),
            AccountMeta::new_readonly(METEORA_DLMM_PROGRAM_ID, false),
            AccountMeta::new(lower_bin_array, false),
            AccountMeta::new(upper_bin_array, false),
        ],
        data,
    }
}

fn constraint_matches(
    constraint: &SquadsInstructionConstraintView,
    instruction: &Instruction,
) -> bool {
    if constraint.program_id != instruction.program_id {
        return false;
    }
    if !constraint.account_constraints.iter().all(|constraint| {
        let Some(actual) = instruction.accounts.get(constraint.account_index as usize) else {
            return false;
        };
        match &constraint.kind {
            SquadsAccountConstraintKindView::Pubkey(allowed) => allowed.contains(&actual.pubkey),
            SquadsAccountConstraintKindView::AccountData(_) => false,
        }
    }) {
        return false;
    }
    constraint.data_constraints.iter().all(|constraint| {
        let offset = constraint.data_offset as usize;
        match (&constraint.data_value, constraint.operator) {
            (SquadsDataValueView::U8Slice(expected), SquadsDataOperatorView::Equals) => {
                instruction.data.get(offset..offset + expected.len()) == Some(expected)
            }
            (SquadsDataValueView::U32Le(maximum), SquadsDataOperatorView::LessThanOrEqualTo) => {
                let Some(bytes) = instruction.data.get(offset..offset + 4) else {
                    return false;
                };
                u32::from_le_bytes(bytes.try_into().expect("four-byte constraint")) <= *maximum
            }
            _ => false,
        }
    })
}

fn assert_account_graph(
    constraint: &SquadsInstructionConstraintView,
    expected: &[(u8, Vec<Pubkey>)],
) {
    assert_eq!(constraint.program_id, METEORA_DLMM_PROGRAM_ID);
    let actual = constraint
        .account_constraints
        .iter()
        .map(|constraint| {
            assert_eq!(constraint.owner, None);
            let SquadsAccountConstraintKindView::Pubkey(pubkeys) = &constraint.kind else {
                panic!("Meteora policy contains an account-data constraint")
            };
            (constraint.account_index, pubkeys.clone())
        })
        .collect::<BTreeMap<_, _>>();
    assert_eq!(actual, expected.iter().cloned().collect::<BTreeMap<_, _>>());
}

fn slice_equals(offset: u64, expected: &[u8]) -> SquadsDataConstraintView {
    SquadsDataConstraintView {
        data_offset: offset,
        data_value: SquadsDataValueView::U8Slice(expected.to_vec()),
        operator: SquadsDataOperatorView::Equals,
    }
}

fn sorted(mut pubkeys: Vec<Pubkey>) -> Vec<Pubkey> {
    pubkeys.sort();
    pubkeys.dedup();
    pubkeys
}

fn packet_bytes(instruction: &Instruction, authority: &Keypair) -> usize {
    let message = Message::try_compile(
        &authority.pubkey(),
        std::slice::from_ref(instruction),
        &[],
        Hash::new_unique(),
    )
    .expect("compile policy create message");
    let transaction = VersionedTransaction::try_new(
        VersionedMessage::V0(message),
        std::slice::from_ref(authority),
    )
    .expect("sign policy create transaction");
    bincode::serialize(&transaction)
        .expect("serialize policy create transaction")
        .len()
}

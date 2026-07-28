use loyal_actions::{
    autonomous_vaults::{create_kamino_policies, KaminoReservePolicyTemplate},
    decode_squads_policy_create_actions, derive_kamino_user_metadata,
    derive_kamino_vanilla_obligation, SquadsAccountConstraintKindView, SquadsDataOperatorView,
    SquadsDataValueView, KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
    KAMINO_INIT_OBLIGATION_DISCRIMINATOR, KAMINO_LEND_PROGRAM_ID,
    KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR, USDC_MINT,
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

const KAMINO_V2_ACCOUNT_COUNT: usize = 17;

#[test]
fn independently_decodes_split_deployed_kamino_policies() {
    let settings = Pubkey::new_unique();
    let authority = Keypair::new();
    let delegated_signer = Pubkey::new_unique();
    let vault = Pubkey::new_unique();
    let vault_index = 2;
    let templates = templates(vault);

    let policies = create_kamino_policies(
        settings,
        authority.pubkey(),
        delegated_signer,
        vault,
        vault_index,
        41,
        42,
        templates.clone(),
    )
    .expect("build autonomous Kamino policies");

    let operations = decode_squads_policy_create_actions(&policies.operations.create_instruction)
        .expect("independently decode operations policy");
    let init = decode_squads_policy_create_actions(&policies.init_obligation.create_instruction)
        .expect("independently decode init-obligation policy");

    assert_eq!(operations.len(), 1);
    assert_eq!(init.len(), 1);
    for action in [&operations[0], &init[0]] {
        assert_eq!(action.settings, settings);
        assert_eq!(action.authority, authority.pubkey());
        assert_eq!(action.delegated_signers, vec![delegated_signer]);
        assert_eq!(action.threshold, 1);
        assert_eq!(action.payload.vault_index, vault_index);
    }

    assert_eq!(operations[0].payload.constraints.len(), 2);
    for (constraint_index, discriminator) in [
        KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
        KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
    ]
    .into_iter()
    .enumerate()
    {
        let constraint = &operations[0].payload.constraints[constraint_index];
        assert_eq!(constraint.program_id, KAMINO_LEND_PROGRAM_ID);
        assert_eq!(constraint.account_constraints.len(), 6);
        for index in [0, 1, 2, 4, 5, 9] {
            assert_allowed_pubkeys(
                constraint,
                index as u8,
                templates
                    .iter()
                    .map(|template| {
                        if constraint_index == 0 {
                            template.deposit_instruction.accounts[index].pubkey
                        } else {
                            template.withdraw_instruction.accounts[index].pubkey
                        }
                    })
                    .collect(),
            );
        }
        assert_data_prefix(constraint, &discriminator);
    }

    let valid_deposit = templates[0].deposit_instruction.clone();
    assert!(matches_any_constraint(
        &operations[0].payload.constraints,
        &valid_deposit
    ));
    for (index, name) in [
        (0, "wrong vault owner"),
        (1, "wrong obligation"),
        (2, "wrong market"),
        (4, "wrong reserve"),
        (5, "wrong liquidity mint"),
        (9, "wrong vault USDC account"),
    ] {
        let mut mutated = valid_deposit.clone();
        mutated.accounts[index].pubkey = Pubkey::new_unique();
        assert!(
            !matches_any_constraint(&operations[0].payload.constraints, &mutated),
            "{name} must be rejected"
        );
    }
    let mut wrong_program = valid_deposit.clone();
    wrong_program.program_id = Pubkey::new_unique();
    assert!(!matches_any_constraint(
        &operations[0].payload.constraints,
        &wrong_program
    ));
    let mut wrong_discriminator = valid_deposit.clone();
    wrong_discriminator.data[0] ^= 1;
    assert!(!matches_any_constraint(
        &operations[0].payload.constraints,
        &wrong_discriminator
    ));
    let mut different_amount = valid_deposit.clone();
    different_amount.data[8..16].copy_from_slice(&u64::MAX.to_le_bytes());
    assert!(
        matches_any_constraint(&operations[0].payload.constraints, &different_amount),
        "the venue policy intentionally leaves the delegated amount dynamic"
    );
    assert_ne!(operations[0].delegated_signers, vec![authority.pubkey()]);

    assert_eq!(init[0].payload.constraints.len(), 1);
    let init_constraint = &init[0].payload.constraints[0];
    assert_eq!(init_constraint.program_id, KAMINO_LEND_PROGRAM_ID);
    assert_exact_pubkey(init_constraint, 0, vault);
    assert_exact_pubkey(init_constraint, 1, vault);
    assert_allowed_pubkeys(
        init_constraint,
        2,
        templates
            .iter()
            .map(|template| derive_kamino_vanilla_obligation(vault, template.market))
            .collect(),
    );
    assert_allowed_pubkeys(
        init_constraint,
        3,
        templates.iter().map(|template| template.market).collect(),
    );
    assert_exact_pubkey(init_constraint, 4, Pubkey::default());
    assert_exact_pubkey(init_constraint, 5, Pubkey::default());
    assert_exact_pubkey(init_constraint, 6, derive_kamino_user_metadata(vault));
    assert_exact_pubkey(init_constraint, 7, solana_sdk::sysvar::rent::id());
    assert_exact_pubkey(init_constraint, 8, solana_sdk::system_program::ID);
    let mut expected = KAMINO_INIT_OBLIGATION_DISCRIMINATOR.to_vec();
    expected.extend_from_slice(&[0, 0]);
    assert_data_prefix(init_constraint, &expected);

    for (name, instruction) in [
        ("Kamino operations", &policies.operations.create_instruction),
        (
            "Kamino init obligation",
            &policies.init_obligation.create_instruction,
        ),
    ] {
        let packet_bytes = packet_bytes(instruction, &authority);
        println!("{name} policy_create_packet_bytes={packet_bytes}");
        assert!(
            packet_bytes <= PACKET_DATA_SIZE,
            "{name} policy create transaction is {packet_bytes} bytes; packet limit is {PACKET_DATA_SIZE}"
        );
    }
}

fn matches_any_constraint(
    constraints: &[loyal_actions::SquadsInstructionConstraintView],
    instruction: &Instruction,
) -> bool {
    constraints
        .iter()
        .any(|constraint| constraint_matches(constraint, instruction))
}

fn constraint_matches(
    constraint: &loyal_actions::SquadsInstructionConstraintView,
    instruction: &Instruction,
) -> bool {
    if constraint.program_id != instruction.program_id {
        return false;
    }
    let accounts_match = constraint.account_constraints.iter().all(|constraint| {
        let Some(actual) = instruction.accounts.get(constraint.account_index as usize) else {
            return false;
        };
        match &constraint.kind {
            SquadsAccountConstraintKindView::Pubkey(allowed) => allowed.contains(&actual.pubkey),
            SquadsAccountConstraintKindView::AccountData(_) => false,
        }
    });
    accounts_match
        && constraint.data_constraints.iter().all(|constraint| {
            let offset = constraint.data_offset as usize;
            match (&constraint.data_value, constraint.operator) {
                (SquadsDataValueView::U8Slice(expected), SquadsDataOperatorView::Equals) => {
                    instruction.data.get(offset..offset + expected.len()) == Some(expected)
                }
                _ => false,
            }
        })
}

fn templates(vault: Pubkey) -> Vec<KaminoReservePolicyTemplate> {
    [
        (Pubkey::new_unique(), Pubkey::new_unique()),
        (Pubkey::new_unique(), Pubkey::new_unique()),
    ]
    .into_iter()
    .map(|(market, reserve)| template(vault, market, reserve))
    .collect()
}

fn template(vault: Pubkey, market: Pubkey, reserve: Pubkey) -> KaminoReservePolicyTemplate {
    let vault_usdc = Pubkey::new_unique();
    let obligation = derive_kamino_vanilla_obligation(vault, market);
    let deposit_instruction = protected_instruction(
        vault,
        obligation,
        market,
        reserve,
        vault_usdc,
        KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
    );
    let mut withdraw_instruction = protected_instruction(
        vault,
        obligation,
        market,
        reserve,
        vault_usdc,
        KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
    );
    for (deposit_index, withdraw_index) in [
        (3, 3),
        (6, 8),
        (7, 7),
        (8, 6),
        (10, 10),
        (13, 13),
        (14, 14),
        (15, 15),
        (16, 16),
    ] {
        withdraw_instruction.accounts[withdraw_index].pubkey =
            deposit_instruction.accounts[deposit_index].pubkey;
    }
    KaminoReservePolicyTemplate {
        market,
        reserve,
        vault_usdc,
        deposit_instruction,
        withdraw_instruction,
    }
}

fn protected_instruction(
    vault: Pubkey,
    obligation: Pubkey,
    market: Pubkey,
    reserve: Pubkey,
    vault_usdc: Pubkey,
    discriminator: [u8; 8],
) -> Instruction {
    let mut accounts = (0..KAMINO_V2_ACCOUNT_COUNT)
        .map(|_| AccountMeta::new(Pubkey::new_unique(), false))
        .collect::<Vec<_>>();
    for (index, pubkey) in [
        (0, vault),
        (1, obligation),
        (2, market),
        (4, reserve),
        (5, USDC_MINT),
        (9, vault_usdc),
        (11, spl_token::id()),
        (12, spl_token::id()),
    ] {
        accounts[index].pubkey = pubkey;
    }
    let mut data = discriminator.to_vec();
    data.extend_from_slice(&1u64.to_le_bytes());
    Instruction {
        program_id: KAMINO_LEND_PROGRAM_ID,
        accounts,
        data,
    }
}

fn assert_exact_pubkey(
    constraint: &loyal_actions::SquadsInstructionConstraintView,
    index: u8,
    expected: Pubkey,
) {
    let constraints = constraint
        .account_constraints
        .iter()
        .map(|constraint| (constraint.account_index, &constraint.kind))
        .collect::<BTreeMap<_, _>>();
    assert_eq!(
        constraints.get(&index),
        Some(&&SquadsAccountConstraintKindView::Pubkey(vec![expected]))
    );
}

fn assert_allowed_pubkeys(
    constraint: &loyal_actions::SquadsInstructionConstraintView,
    index: u8,
    mut expected: Vec<Pubkey>,
) {
    expected.sort();
    expected.dedup();
    let constraints = constraint
        .account_constraints
        .iter()
        .map(|constraint| (constraint.account_index, &constraint.kind))
        .collect::<BTreeMap<_, _>>();
    assert_eq!(
        constraints.get(&index),
        Some(&&SquadsAccountConstraintKindView::Pubkey(expected))
    );
}

fn assert_data_prefix(
    constraint: &loyal_actions::SquadsInstructionConstraintView,
    expected: &[u8],
) {
    assert_eq!(constraint.data_constraints.len(), 1);
    let data = &constraint.data_constraints[0];
    assert_eq!(data.data_offset, 0);
    assert_eq!(data.operator, SquadsDataOperatorView::Equals);
    assert_eq!(
        data.data_value,
        SquadsDataValueView::U8Slice(expected.to_vec())
    );
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

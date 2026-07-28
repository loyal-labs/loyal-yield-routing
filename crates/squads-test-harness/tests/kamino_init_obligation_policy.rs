use loyal_actions::{
    decode_squads_policy_create_actions, derive_action_account, derive_kamino_user_metadata,
    update_init_obligation_yield_route_action, LoyalActionContext, SquadsAccountConstraintKindView,
    SquadsAccountConstraintView, SquadsDataOperatorView, SquadsDataValueView,
    SquadsInstructionConstraintView, YieldRouteUniverse,
    KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR, KAMINO_FIGURE_MARKET as KAMINO_PRIME_MARKET,
    KAMINO_INIT_OBLIGATION_DISCRIMINATOR, KAMINO_LEND_PROGRAM_ID, KAMINO_MAIN_MARKET,
    KAMINO_MAPLE_MARKET, KAMINO_VANILLA_OBLIGATION_ID, KAMINO_VANILLA_OBLIGATION_TAG, USDC_MINT,
};
use solana_sdk::{pubkey::Pubkey, sysvar};
use solana_system_interface::program as system_program;

#[test]
fn init_obligation_policy_update_anchors_owner_market_and_seed_accounts() {
    let settings = Pubkey::new_unique();
    let authority = Pubkey::new_unique();
    let delegated_signer = Pubkey::new_unique();
    let vault = Pubkey::new_unique();
    let vault_index = 1;
    let policy_seed = 37;
    let policy = derive_action_account(&settings, policy_seed).0;
    let markets = vec![KAMINO_MAIN_MARKET, KAMINO_PRIME_MARKET];
    let context = LoyalActionContext {
        settings,
        authority,
        delegated_signer,
        account_index: vault_index,
        vault,
    };

    let setup = update_init_obligation_yield_route_action(
        context,
        YieldRouteUniverse::new(vec![USDC_MINT], markets.clone(), vec![USDC_MINT]),
        policy,
        vault_index,
    )
    .expect("build init-obligation policy update");

    let decoded = decode_squads_policy_create_actions(&setup.instructions[0])
        .expect("decode init-obligation policy update");
    assert_eq!(decoded.len(), 1);
    let action = &decoded[0];
    assert_eq!(action.settings, settings);
    assert_eq!(action.authority, authority);
    assert_eq!(action.policy_account, policy);
    assert_eq!(action.delegated_signers, vec![delegated_signer]);
    assert_eq!(action.threshold, 1);
    assert_eq!(action.payload.vault_index, vault_index);
    assert_eq!(action.payload.constraints.len(), 2);
    assert_eq!(
        action.payload.constraints[0].program_id,
        KAMINO_LEND_PROGRAM_ID
    );
    assert!(data_prefix_matches(
        &action.payload.constraints[0],
        &KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR
    ));

    let constraint = &action.payload.constraints[1];
    assert_init_obligation_constraint(constraint, vault, &markets);

    assert_rejects_mutation(
        constraint,
        vault,
        &markets,
        "unsupported market",
        |mutated| {
            replace_pubkeys(mutated, 3, vec![KAMINO_MAPLE_MARKET]);
        },
    );
    assert_rejects_mutation(constraint, vault, &markets, "wrong owner", |mutated| {
        replace_pubkeys(mutated, 0, vec![Pubkey::new_unique()]);
    });
    assert_rejects_mutation(constraint, vault, &markets, "wrong fee payer", |mutated| {
        replace_pubkeys(mutated, 1, vec![Pubkey::new_unique()]);
    });
    assert_rejects_mutation(
        constraint,
        vault,
        &markets,
        "wrong user metadata PDA",
        |mutated| {
            replace_pubkeys(mutated, 6, vec![Pubkey::new_unique()]);
        },
    );
    assert_rejects_mutation(
        constraint,
        vault,
        &markets,
        "wrong seed account 0",
        |mutated| {
            replace_pubkeys(mutated, 4, vec![Pubkey::new_unique()]);
        },
    );
    assert_rejects_mutation(
        constraint,
        vault,
        &markets,
        "wrong seed account 1",
        |mutated| {
            replace_pubkeys(mutated, 5, vec![Pubkey::new_unique()]);
        },
    );
    assert_rejects_mutation(
        constraint,
        vault,
        &markets,
        "wrong init discriminator",
        |mutated| {
            if let SquadsDataValueView::U8Slice(bytes) = &mut mutated.data_constraints[0].data_value
            {
                bytes[0] ^= 1;
            }
        },
    );
}

fn assert_rejects_mutation(
    constraint: &SquadsInstructionConstraintView,
    vault: Pubkey,
    markets: &[Pubkey],
    name: &'static str,
    mutate: impl FnOnce(&mut SquadsInstructionConstraintView),
) {
    let mut mutated = constraint.clone();
    mutate(&mut mutated);
    assert!(
        !init_obligation_constraint_matches(&mutated, vault, markets),
        "{name} should not match the init-obligation policy"
    );
}

fn assert_init_obligation_constraint(
    constraint: &SquadsInstructionConstraintView,
    vault: Pubkey,
    markets: &[Pubkey],
) {
    assert!(
        init_obligation_constraint_matches(constraint, vault, markets),
        "init-obligation policy shape should match expected KLend constraints"
    );
}

fn init_obligation_constraint_matches(
    constraint: &SquadsInstructionConstraintView,
    vault: Pubkey,
    markets: &[Pubkey],
) -> bool {
    let mut expected_data_prefix = KAMINO_INIT_OBLIGATION_DISCRIMINATOR.to_vec();
    expected_data_prefix.push(KAMINO_VANILLA_OBLIGATION_TAG);
    expected_data_prefix.push(KAMINO_VANILLA_OBLIGATION_ID);

    constraint.program_id == KAMINO_LEND_PROGRAM_ID
        && account_pubkeys_match(constraint, 0, &[vault])
        && account_pubkeys_match(constraint, 1, &[vault])
        && account_pubkeys_match(constraint, 3, markets)
        && account_pubkeys_match(constraint, 4, &[Pubkey::default()])
        && account_pubkeys_match(constraint, 5, &[Pubkey::default()])
        && account_pubkeys_match(constraint, 6, &[derive_kamino_user_metadata(vault)])
        && account_pubkeys_match(constraint, 7, &[sysvar::rent::id()])
        && account_pubkeys_match(constraint, 8, &[system_program::ID])
        && data_prefix_matches(constraint, &expected_data_prefix)
}

fn account_pubkeys_match(
    constraint: &SquadsInstructionConstraintView,
    account_index: u8,
    expected: &[Pubkey],
) -> bool {
    let Some(account_constraint) = constraint
        .account_constraints
        .iter()
        .find(|candidate| candidate.account_index == account_index)
    else {
        return false;
    };
    matches!(
        &account_constraint.kind,
        SquadsAccountConstraintKindView::Pubkey(pubkeys) if pubkeys == expected
    ) && account_constraint.owner.is_none()
}

fn data_prefix_matches(
    constraint: &SquadsInstructionConstraintView,
    expected_data_prefix: &[u8],
) -> bool {
    matches!(
        constraint.data_constraints.as_slice(),
        [data_constraint]
            if data_constraint.data_offset == 0
                && data_constraint.operator == SquadsDataOperatorView::Equals
                && data_constraint.data_value == SquadsDataValueView::U8Slice(expected_data_prefix.to_vec())
    )
}

fn replace_pubkeys(
    constraint: &mut SquadsInstructionConstraintView,
    account_index: u8,
    replacement: Vec<Pubkey>,
) {
    let account_constraint = account_constraint_mut(constraint, account_index);
    account_constraint.kind = SquadsAccountConstraintKindView::Pubkey(replacement);
    account_constraint.owner = None;
}

fn account_constraint_mut(
    constraint: &mut SquadsInstructionConstraintView,
    account_index: u8,
) -> &mut SquadsAccountConstraintView {
    constraint
        .account_constraints
        .iter_mut()
        .find(|candidate| candidate.account_index == account_index)
        .expect("account constraint should exist")
}

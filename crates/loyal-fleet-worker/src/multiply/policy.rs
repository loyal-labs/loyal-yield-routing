use super::config::{
    EarnMaxTopology, PolicyConfig, StrategyConfig, JUPITER, KLEND, TOKEN, TOKEN_2022, USDC_MINT,
};
use loyal_actions::{
    create_semantic_program_interaction_policy_instruction,
    decode_program_interaction_policy_account, decode_squads_policy_create_actions,
    derive_action_account, update_semantic_program_interaction_policy_instruction,
    SemanticProgramInteractionConstraint as Constraint,
    SemanticProgramInteractionDataConstraint as DataConstraint, SquadsProgramInteractionPolicyView,
};
use loyal_yield_store::fleet_orchestration::MultiplyAction;
use solana_sdk::{instruction::Instruction, pubkey::Pubkey};
use std::{error::Error, str::FromStr};

pub const REFRESH_RESERVE: [u8; 8] = klend_interface::discriminators::REFRESH_RESERVE;
pub const REFRESH_OBLIGATION: [u8; 8] = klend_interface::discriminators::REFRESH_OBLIGATION;
pub const DEPOSIT_COLLATERAL: [u8; 8] =
    klend_interface::discriminators::DEPOSIT_RESERVE_LIQUIDITY_AND_OBLIGATION_COLLATERAL_V2;
pub const BORROW_DEBT: [u8; 8] = klend_interface::discriminators::BORROW_OBLIGATION_LIQUIDITY_V2;
pub const WITHDRAW_COLLATERAL: [u8; 8] = klend_interface::discriminators::WITHDRAW_OBLIGATION_COLLATERAL_AND_REDEEM_RESERVE_COLLATERAL_V2;
pub const REPAY_DEBT: [u8; 8] = klend_interface::discriminators::REPAY_OBLIGATION_LIQUIDITY_V2;
const SHARED_ACCOUNTS_ROUTE: [u8; 8] = [0xc1, 0x20, 0x9b, 0x33, 0x41, 0xd6, 0x9c, 0x81];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum PolicyFamily {
    Deposit,
    Borrow,
    SwapClaimToCollateral,
    SwapDebtToCollateral,
    SwapCollateralToDebt,
    SwapCollateralToClaim,
    Repay,
    Withdraw,
}

pub fn family_for_action(action: MultiplyAction) -> Result<PolicyFamily, Box<dyn Error>> {
    match action {
        MultiplyAction::DepositCollateral => Ok(PolicyFamily::Deposit),
        MultiplyAction::BorrowDebt => Ok(PolicyFamily::Borrow),
        MultiplyAction::SwapClaimToCollateral => Ok(PolicyFamily::SwapClaimToCollateral),
        MultiplyAction::SwapDebtToCollateral => Ok(PolicyFamily::SwapDebtToCollateral),
        MultiplyAction::SwapCollateralToDebt => Ok(PolicyFamily::SwapCollateralToDebt),
        MultiplyAction::SwapCollateralToClaim => Ok(PolicyFamily::SwapCollateralToClaim),
        MultiplyAction::RepayDebt => Ok(PolicyFamily::Repay),
        MultiplyAction::WithdrawCollateral | MultiplyAction::WithdrawRemainingCollateral => {
            Ok(PolicyFamily::Withdraw)
        }
        MultiplyAction::Claim | MultiplyAction::DepositClaimAsset => {
            Err("action has no strategy policy family".into())
        }
    }
}

impl PolicyFamily {
    pub fn policy(self, config: StrategyConfig) -> PolicyConfig {
        match self {
            Self::Deposit => config.deposit_policy,
            Self::Borrow => config.borrow_policy,
            Self::SwapClaimToCollateral => config.claim_to_collateral_policy,
            Self::SwapDebtToCollateral => config.debt_to_collateral_policy,
            Self::SwapCollateralToDebt => config.collateral_to_debt_policy,
            Self::SwapCollateralToClaim => config.collateral_to_claim_policy,
            Self::Repay => config.repay_policy,
            Self::Withdraw => config.withdraw_policy,
        }
    }
}

pub fn canonical_policy_create(
    topology: EarnMaxTopology,
    config: StrategyConfig,
    family: PolicyFamily,
    settings: Pubkey,
    authority: Pubkey,
    delegate: Pubkey,
) -> Result<Instruction, Box<dyn Error>> {
    let policy = family.policy(config);
    if derive_action_account(&settings, policy.seed).0 != policy.account {
        return Err("configured policy account is not the PDA for its seed".into());
    }
    create_semantic_program_interaction_policy_instruction(
        settings,
        authority,
        delegate,
        policy.seed,
        0,
        canonical_constraints(topology, config, family)?,
    )
    .map_err(Into::into)
}

pub fn canonical_policy_update(
    topology: EarnMaxTopology,
    config: StrategyConfig,
    family: PolicyFamily,
    settings: Pubkey,
    authority: Pubkey,
    delegate: Pubkey,
) -> Result<Instruction, Box<dyn Error>> {
    update_semantic_program_interaction_policy_instruction(
        settings,
        authority,
        family.policy(config).account,
        delegate,
        0,
        canonical_constraints(topology, config, family)?,
    )
    .map_err(Into::into)
}

pub fn canonical_policy_payload(
    update: &Instruction,
) -> Result<SquadsProgramInteractionPolicyView, Box<dyn Error>> {
    let actions = decode_squads_policy_create_actions(update)?;
    let [action] = actions.as_slice() else {
        return Err("canonical policy update did not decode to one action".into());
    };
    Ok(action.payload.clone())
}

pub fn current_policy_matches(
    data: &[u8],
    policy: PolicyConfig,
    delegate: Pubkey,
    expected: &SquadsProgramInteractionPolicyView,
) -> Result<bool, Box<dyn Error>> {
    let Some(current) = decode_program_interaction_policy_account(data)? else {
        return Ok(false);
    };
    Ok(current.policy_seed == policy.seed
        && current.policy_account == policy.account
        && current.delegated_signer == delegate
        && current.threshold == 1
        && current.payload == *expected)
}

pub fn constraint_indexes(
    _config: StrategyConfig,
    action: MultiplyAction,
    instructions: &[Instruction],
) -> Result<Vec<u8>, Box<dyn Error>> {
    let indexes = match action {
        MultiplyAction::DepositCollateral => vec![0, 0, 1, 2],
        MultiplyAction::BorrowDebt => vec![0, 0, 1, 2],
        MultiplyAction::WithdrawCollateral | MultiplyAction::WithdrawRemainingCollateral => {
            match instructions.len() {
                3 => vec![0, 1, 2],
                4 => vec![0, 0, 1, 2],
                _ => return Err("withdraw action graph must contain 3 or 4 instructions".into()),
            }
        }
        MultiplyAction::RepayDebt => vec![0, 0, 1, 2],
        MultiplyAction::SwapClaimToCollateral
        | MultiplyAction::SwapDebtToCollateral
        | MultiplyAction::SwapCollateralToDebt
        | MultiplyAction::SwapCollateralToClaim => vec![route_constraint_index(instructions)?],
        MultiplyAction::Claim | MultiplyAction::DepositClaimAsset => {
            return Err("action does not use a strategy policy".into())
        }
    };
    if indexes.len() != instructions.len() {
        return Err("policy constraint count does not match the action graph".into());
    }
    Ok(indexes)
}

fn canonical_constraints(
    topology: EarnMaxTopology,
    config: StrategyConfig,
    family: PolicyFamily,
) -> Result<Vec<Constraint>, Box<dyn Error>> {
    let key = |value: &str| Pubkey::from_str(value);
    let program = key(KLEND)?;
    let obligation = config.obligation;
    let collateral = key(config.collateral_reserve)?;
    let debt = key(config.debt_reserve)?;
    let refresh = constraint(program, vec![(0, vec![collateral, debt])], REFRESH_RESERVE);
    // KLend itself validates the remaining reserve tail against the current
    // obligation deposits and borrows. Pinning only the obligation keeps one
    // stable contract for empty, collateral-only, and leveraged positions.
    let refresh_obligation =
        || constraint(program, vec![(1, vec![obligation])], REFRESH_OBLIGATION);
    let constraints = match family {
        PolicyFamily::Deposit => vec![
            refresh.clone(),
            refresh_obligation(),
            constraint(
                program,
                pins(
                    topology,
                    config,
                    &[0, 1, 4, 9, 11, 12, 14, 15],
                    MultiplyAction::DepositCollateral,
                )?,
                DEPOSIT_COLLATERAL,
            ),
        ],
        PolicyFamily::Borrow => vec![
            refresh.clone(),
            refresh_obligation(),
            constraint(
                program,
                pins(
                    topology,
                    config,
                    &[0, 1, 4, 8, 10, 12, 13, 14],
                    MultiplyAction::BorrowDebt,
                )?,
                BORROW_DEBT,
            ),
        ],
        PolicyFamily::SwapClaimToCollateral
        | PolicyFamily::SwapDebtToCollateral
        | PolicyFamily::SwapCollateralToDebt
        | PolicyFamily::SwapCollateralToClaim => {
            let (source, destination, source_mint, destination_mint, optional_program) =
                swap_accounts(topology, config, family);
            vec![Constraint {
                program_id: key(JUPITER)?,
                account_pubkeys: vec![
                    (0, vec![key(TOKEN)?]),
                    (2, vec![topology.vault]),
                    (3, vec![source]),
                    (6, vec![destination]),
                    (7, vec![key(source_mint)?]),
                    (8, vec![key(destination_mint)?]),
                    (9, vec![key(JUPITER)?]),
                    (10, vec![key(optional_program)?]),
                ],
                data: vec![DataConstraint::SliceEquals {
                    offset: 0,
                    value: SHARED_ACCOUNTS_ROUTE.to_vec(),
                }],
            }]
        }
        PolicyFamily::Repay => vec![
            refresh.clone(),
            refresh_obligation(),
            constraint(
                program,
                pins(
                    topology,
                    config,
                    &[0, 1, 3, 6, 7, 9, 10, 12],
                    MultiplyAction::RepayDebt,
                )?,
                REPAY_DEBT,
            ),
        ],
        PolicyFamily::Withdraw => vec![
            refresh.clone(),
            refresh_obligation(),
            constraint(
                program,
                pins(
                    topology,
                    config,
                    &[0, 1, 4, 9, 11, 12, 14, 15],
                    MultiplyAction::WithdrawCollateral,
                )?,
                WITHDRAW_COLLATERAL,
            ),
        ],
    };
    Ok(constraints)
}

fn constraint(
    program_id: Pubkey,
    account_pubkeys: Vec<(u8, Vec<Pubkey>)>,
    discriminator: [u8; 8],
) -> Constraint {
    Constraint {
        program_id,
        account_pubkeys,
        data: vec![DataConstraint::SliceEquals {
            offset: 0,
            value: discriminator.to_vec(),
        }],
    }
}

fn swap_accounts(
    topology: EarnMaxTopology,
    config: StrategyConfig,
    family: PolicyFamily,
) -> (Pubkey, Pubkey, &'static str, &'static str, &'static str) {
    let optional = if config.debt_token_program == TOKEN_2022 {
        TOKEN_2022
    } else {
        JUPITER
    };
    match family {
        PolicyFamily::SwapClaimToCollateral => (
            topology.claim_custody,
            config.collateral_custody,
            USDC_MINT,
            config.collateral_mint,
            JUPITER,
        ),
        PolicyFamily::SwapDebtToCollateral => (
            config.debt_custody,
            config.collateral_custody,
            config.debt_mint,
            config.collateral_mint,
            optional,
        ),
        PolicyFamily::SwapCollateralToDebt => (
            config.collateral_custody,
            config.debt_custody,
            config.collateral_mint,
            config.debt_mint,
            optional,
        ),
        PolicyFamily::SwapCollateralToClaim => (
            config.collateral_custody,
            topology.claim_custody,
            config.collateral_mint,
            USDC_MINT,
            JUPITER,
        ),
        _ => unreachable!("non-swap family passed to swap_accounts"),
    }
}

fn route_constraint_index(instructions: &[Instruction]) -> Result<u8, Box<dyn Error>> {
    let [instruction] = instructions else {
        return Err("Jupiter action must contain one instruction".into());
    };
    if instruction.data.get(..8) != Some(SHARED_ACCOUNTS_ROUTE.as_slice()) {
        return Err("Jupiter action is not SharedAccountsRoute".into());
    }
    let route_count = instruction
        .data
        .get(9..13)
        .and_then(|bytes| <[u8; 4]>::try_from(bytes).ok())
        .map(u32::from_le_bytes)
        .ok_or("Jupiter route count is absent")?;
    match route_count {
        1..=4 => Ok(0),
        _ => return Err("Jupiter route must contain one to four legs".into()),
    }
}

fn pins(
    topology: EarnMaxTopology,
    config: StrategyConfig,
    indexes: &[u8],
    action: MultiplyAction,
) -> Result<Vec<(u8, Vec<Pubkey>)>, Box<dyn Error>> {
    let template = super::builder::policy_template(config, topology.vault, action)?;
    indexes
        .iter()
        .map(|index| {
            let account = template
                .accounts
                .get(usize::from(*index))
                .ok_or("policy pin is outside the canonical instruction")?;
            Ok((*index, vec![account.pubkey]))
        })
        .collect()
}

use super::config::{
    EarnMaxTopology, PolicyConfig, StrategyConfig, JUPITER, KLEND, PYUSD_MINT, TOKEN, TOKEN_2022,
    USDC_MINT,
};
use loyal_actions::{
    create_semantic_program_interaction_policy_instruction,
    decode_program_interaction_policy_account, decode_squads_policy_create_actions,
    derive_action_account, update_semantic_program_interaction_policy_instruction,
    SemanticProgramInteractionConstraint as Constraint,
    SemanticProgramInteractionDataConstraint as DataConstraint, SquadsProgramInteractionPolicyView,
};
use loyal_yield_store::fleet_orchestration::{MultiplyAction, StrategyKey};
use solana_sdk::{instruction::Instruction, pubkey::Pubkey};
use std::{error::Error, str::FromStr};

pub const DEPOSIT_COLLATERAL: [u8; 8] =
    klend_interface::discriminators::DEPOSIT_RESERVE_LIQUIDITY_AND_OBLIGATION_COLLATERAL_V2;
pub const BORROW_DEBT: [u8; 8] = klend_interface::discriminators::BORROW_OBLIGATION_LIQUIDITY_V2;
pub const WITHDRAW_COLLATERAL: [u8; 8] = klend_interface::discriminators::WITHDRAW_OBLIGATION_COLLATERAL_AND_REDEEM_RESERVE_COLLATERAL_V2;
pub const REPAY_DEBT: [u8; 8] = klend_interface::discriminators::REPAY_OBLIGATION_LIQUIDITY_V2;
const SHARED_ACCOUNTS_ROUTE: [u8; 8] = [0xc1, 0x20, 0x9b, 0x33, 0x41, 0xd6, 0x9c, 0x81];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum PolicyFamily {
    Collateral,
    Debt,
    Swap,
}

impl PolicyFamily {
    pub const fn label(self) -> &'static str {
        match self {
            Self::Collateral => "CollateralLifecycle",
            Self::Debt => "DebtLifecycle",
            Self::Swap => "SwapRoutes",
        }
    }

    pub fn policy(self, config: StrategyConfig) -> PolicyConfig {
        match self {
            Self::Collateral => config.collateral_policy,
            Self::Debt => config.debt_policy,
            Self::Swap => config.swap_policy,
        }
    }
}

pub fn family_for_action(action: MultiplyAction) -> Result<PolicyFamily, Box<dyn Error>> {
    match action {
        MultiplyAction::DepositCollateral
        | MultiplyAction::WithdrawCollateral
        | MultiplyAction::WithdrawRemainingCollateral => Ok(PolicyFamily::Collateral),
        MultiplyAction::BorrowDebt | MultiplyAction::RepayDebt => Ok(PolicyFamily::Debt),
        MultiplyAction::SwapClaimToCollateral
        | MultiplyAction::SwapDebtToCollateral
        | MultiplyAction::SwapCollateralToDebt
        | MultiplyAction::SwapCollateralToClaim => Ok(PolicyFamily::Swap),
        MultiplyAction::Claim
        | MultiplyAction::DepositClaimAsset
        | MultiplyAction::RequestWithdrawal
        | MultiplyAction::CancelWithdrawal => Err("action has no strategy policy family".into()),
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
        canonical_bootstrap_constraints(topology, family)?,
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
        canonical_constraints(topology, family)?,
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

pub fn canonical_policy_payload_matches(
    actual: &SquadsProgramInteractionPolicyView,
    expected: &SquadsProgramInteractionPolicyView,
) -> bool {
    actual.vault_index == expected.vault_index
        && actual.constraints == expected.constraints
        && actual.spending_limits == expected.spending_limits
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
        && canonical_policy_payload_matches(&current.payload, expected))
}

pub fn constraint_indexes(
    config: StrategyConfig,
    action: MultiplyAction,
    instructions: &[Instruction],
) -> Result<Vec<u8>, Box<dyn Error>> {
    let [instruction] = instructions else {
        return Err("policy execution must contain exactly one terminal instruction".into());
    };
    let index = match action {
        MultiplyAction::DepositCollateral => 0,
        MultiplyAction::WithdrawCollateral | MultiplyAction::WithdrawRemainingCollateral => 1,
        MultiplyAction::BorrowDebt => match config.key {
            StrategyKey::SyrupUsdcUsdc => 0,
            StrategyKey::SyrupUsdcPyusd => 2,
        },
        MultiplyAction::RepayDebt => match config.key {
            StrategyKey::SyrupUsdcUsdc => 1,
            StrategyKey::SyrupUsdcPyusd => 3,
        },
        MultiplyAction::SwapClaimToCollateral => route_constraint_index(instruction, 0)?,
        MultiplyAction::SwapDebtToCollateral => route_constraint_index(
            instruction,
            match config.key {
                StrategyKey::SyrupUsdcUsdc => 0,
                StrategyKey::SyrupUsdcPyusd => 2,
            },
        )?,
        MultiplyAction::SwapCollateralToDebt => route_constraint_index(
            instruction,
            match config.key {
                StrategyKey::SyrupUsdcUsdc => 1,
                StrategyKey::SyrupUsdcPyusd => 3,
            },
        )?,
        MultiplyAction::SwapCollateralToClaim => route_constraint_index(instruction, 1)?,
        MultiplyAction::Claim
        | MultiplyAction::DepositClaimAsset
        | MultiplyAction::RequestWithdrawal
        | MultiplyAction::CancelWithdrawal => {
            return Err("action does not use a strategy policy".into())
        }
    };
    Ok(vec![index])
}

fn canonical_bootstrap_constraints(
    topology: EarnMaxTopology,
    family: PolicyFamily,
) -> Result<Vec<Constraint>, Box<dyn Error>> {
    let full = canonical_constraints(topology, family)?;
    let safe_index = match family {
        PolicyFamily::Collateral => 1,
        PolicyFamily::Debt => 1,
        PolicyFamily::Swap => 1,
    };
    Ok(vec![full
        .get(safe_index)
        .ok_or("bootstrap constraint is absent")?
        .clone()])
}

fn canonical_constraints(
    topology: EarnMaxTopology,
    family: PolicyFamily,
) -> Result<Vec<Constraint>, Box<dyn Error>> {
    let usdc = topology.strategy(StrategyKey::SyrupUsdcUsdc);
    let pyusd = topology.strategy(StrategyKey::SyrupUsdcPyusd);
    ensure_shared_collateral(usdc, pyusd)?;
    let program = Pubkey::from_str(KLEND)?;
    match family {
        PolicyFamily::Collateral => Ok(vec![
            collateral_constraint(
                topology,
                usdc,
                pyusd,
                MultiplyAction::DepositCollateral,
                DEPOSIT_COLLATERAL,
            )?,
            collateral_constraint(
                topology,
                usdc,
                pyusd,
                MultiplyAction::WithdrawCollateral,
                WITHDRAW_COLLATERAL,
            )?,
        ]),
        PolicyFamily::Debt => Ok(vec![
            constraint(
                program,
                pins(
                    topology,
                    usdc,
                    &[0, 1, 4, 8, 10, 12, 13, 14],
                    MultiplyAction::BorrowDebt,
                )?,
                BORROW_DEBT,
            ),
            constraint(
                program,
                pins(
                    topology,
                    usdc,
                    &[0, 1, 3, 6, 7, 9, 10, 12],
                    MultiplyAction::RepayDebt,
                )?,
                REPAY_DEBT,
            ),
            constraint(
                program,
                pins(
                    topology,
                    pyusd,
                    &[0, 1, 4, 8, 10, 12, 13, 14],
                    MultiplyAction::BorrowDebt,
                )?,
                BORROW_DEBT,
            ),
            constraint(
                program,
                pins(
                    topology,
                    pyusd,
                    &[0, 1, 3, 6, 7, 9, 10, 12],
                    MultiplyAction::RepayDebt,
                )?,
                REPAY_DEBT,
            ),
        ]),
        PolicyFamily::Swap => Ok(vec![
            swap_constraint(
                topology,
                topology.claim_custody,
                usdc.collateral_custody,
                USDC_MINT,
                usdc.collateral_mint,
                JUPITER,
            )?,
            swap_constraint(
                topology,
                usdc.collateral_custody,
                topology.claim_custody,
                usdc.collateral_mint,
                USDC_MINT,
                JUPITER,
            )?,
            swap_constraint(
                topology,
                pyusd.debt_custody,
                pyusd.collateral_custody,
                PYUSD_MINT,
                pyusd.collateral_mint,
                TOKEN_2022,
            )?,
            swap_constraint(
                topology,
                pyusd.collateral_custody,
                pyusd.debt_custody,
                pyusd.collateral_mint,
                PYUSD_MINT,
                TOKEN_2022,
            )?,
            swap_constraint(
                topology,
                topology.claim_custody,
                pyusd.debt_custody,
                USDC_MINT,
                PYUSD_MINT,
                TOKEN_2022,
            )?,
            swap_constraint(
                topology,
                pyusd.debt_custody,
                topology.claim_custody,
                PYUSD_MINT,
                USDC_MINT,
                TOKEN_2022,
            )?,
        ]),
    }
}

fn ensure_shared_collateral(
    usdc: StrategyConfig,
    pyusd: StrategyConfig,
) -> Result<(), Box<dyn Error>> {
    if usdc.market != pyusd.market
        || usdc.collateral_reserve != pyusd.collateral_reserve
        || usdc.collateral_mint != pyusd.collateral_mint
        || usdc.collateral_custody != pyusd.collateral_custody
    {
        return Err("Earn MAX debt variants do not share the exact collateral tuple".into());
    }
    Ok(())
}

fn collateral_constraint(
    topology: EarnMaxTopology,
    usdc: StrategyConfig,
    pyusd: StrategyConfig,
    action: MultiplyAction,
    discriminator: [u8; 8],
) -> Result<Constraint, Box<dyn Error>> {
    let indexes: &[u8] = match action {
        MultiplyAction::DepositCollateral => &[0, 1, 4, 9, 11, 12, 14, 15],
        MultiplyAction::WithdrawCollateral => &[0, 1, 4, 9, 11, 12, 14, 15],
        _ => return Err("invalid collateral policy action".into()),
    };
    let mut account_pubkeys = pins(topology, usdc, indexes, action)?;
    let obligation = account_pubkeys
        .iter_mut()
        .find(|(index, _)| *index == 1)
        .ok_or("collateral policy omitted obligation pin")?;
    obligation.1 = vec![usdc.obligation, pyusd.obligation];
    Ok(constraint(
        Pubkey::from_str(KLEND)?,
        account_pubkeys,
        discriminator,
    ))
}

fn swap_constraint(
    topology: EarnMaxTopology,
    source: Pubkey,
    destination: Pubkey,
    source_mint: &str,
    destination_mint: &str,
    optional_program: &str,
) -> Result<Constraint, Box<dyn Error>> {
    Ok(Constraint {
        program_id: Pubkey::from_str(JUPITER)?,
        account_pubkeys: vec![
            (0, vec![Pubkey::from_str(TOKEN)?]),
            (2, vec![topology.vault]),
            (3, vec![source]),
            (6, vec![destination]),
            (7, vec![Pubkey::from_str(source_mint)?]),
            (8, vec![Pubkey::from_str(destination_mint)?]),
            (9, vec![Pubkey::from_str(JUPITER)?]),
            (10, vec![Pubkey::from_str(optional_program)?]),
        ],
        data: vec![DataConstraint::SliceEquals {
            offset: 0,
            value: SHARED_ACCOUNTS_ROUTE.to_vec(),
        }],
    })
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

fn route_constraint_index(instruction: &Instruction, index: u8) -> Result<u8, Box<dyn Error>> {
    if instruction.data.get(..8) != Some(SHARED_ACCOUNTS_ROUTE.as_slice()) {
        return Err("Jupiter action is not SharedAccountsRoute".into());
    }
    let route_count = instruction
        .data
        .get(9..13)
        .and_then(|bytes| <[u8; 4]>::try_from(bytes).ok())
        .map(u32::from_le_bytes)
        .ok_or("Jupiter route count is absent")?;
    if !(1..=4).contains(&route_count) {
        return Err("Jupiter route must contain one to four legs".into());
    }
    Ok(index)
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

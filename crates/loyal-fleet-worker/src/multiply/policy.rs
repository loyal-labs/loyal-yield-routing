use super::config::{EarnMaxTopology, PolicyConfig, StrategyConfig, FARMS, JUPITER, KLEND, TOKEN};
use loyal_actions::{
    create_semantic_program_interaction_policy_instruction,
    decode_program_interaction_policy_account, decode_squads_policy_create_actions,
    derive_action_account, earn_max_policy_constraints,
    update_semantic_program_interaction_policy_instruction, EarnMaxPolicyBoundary,
    EarnMaxPolicyFamily, EarnMaxPolicyLane, SemanticProgramInteractionConstraint as Constraint,
    SquadsProgramInteractionPolicyView, EARN_MAX_SHARED_ACCOUNTS_ROUTE,
};
use loyal_yield_store::fleet_orchestration::{MultiplyAction, StrategyKey};
use solana_sdk::{instruction::Instruction, pubkey::Pubkey};
use std::{error::Error, str::FromStr};

pub const DEPOSIT_COLLATERAL: [u8; 8] =
    klend_interface::discriminators::DEPOSIT_RESERVE_LIQUIDITY_AND_OBLIGATION_COLLATERAL_V2;
pub const BORROW_DEBT: [u8; 8] = klend_interface::discriminators::BORROW_OBLIGATION_LIQUIDITY_V2;
pub const WITHDRAW_COLLATERAL: [u8; 8] = klend_interface::discriminators::WITHDRAW_OBLIGATION_COLLATERAL_AND_REDEEM_RESERVE_COLLATERAL_V2;
pub const REPAY_DEBT: [u8; 8] = klend_interface::discriminators::REPAY_OBLIGATION_LIQUIDITY_V2;

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
        MultiplyAction::BorrowDebt => 0,
        MultiplyAction::RepayDebt => 1,
        MultiplyAction::SwapClaimToCollateral => {
            route_constraint_index(instruction, usdc_lane_index(config.key, false)?)?
        }
        MultiplyAction::SwapDebtToCollateral => {
            route_constraint_index(instruction, strategy_lane_index(config.key, false))?
        }
        MultiplyAction::SwapCollateralToDebt => {
            route_constraint_index(instruction, strategy_lane_index(config.key, true))?
        }
        MultiplyAction::SwapCollateralToClaim => {
            route_constraint_index(instruction, usdc_lane_index(config.key, true)?)?
        }
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
    let strategies = topology.strategy_catalog();
    let boundary = EarnMaxPolicyBoundary {
        vault: topology.vault,
        klend_program: Pubkey::from_str(KLEND)?,
        farms_program: Pubkey::from_str(FARMS)?,
        jupiter_program: Pubkey::from_str(JUPITER)?,
        classic_token_program: Pubkey::from_str(TOKEN)?,
        deposit_discriminator: DEPOSIT_COLLATERAL,
        withdraw_discriminator: WITHDRAW_COLLATERAL,
        borrow_discriminator: BORROW_DEBT,
        repay_discriminator: REPAY_DEBT,
        lanes: strategies
            .iter()
            .map(|strategy| {
                Ok(EarnMaxPolicyLane {
                    obligation: strategy.obligation,
                    collateral_reserve: Pubkey::from_str(strategy.collateral_reserve)?,
                    collateral_custody: strategy.collateral_custody,
                    debt_reserve: Pubkey::from_str(strategy.debt_reserve)?,
                    debt_custody: strategy.debt_custody,
                    debt_token_program: Pubkey::from_str(strategy.debt_token_program)?,
                })
            })
            .collect::<Result<Vec<_>, solana_sdk::pubkey::ParsePubkeyError>>()?,
    };
    let family = match family {
        PolicyFamily::Collateral => EarnMaxPolicyFamily::Collateral,
        PolicyFamily::Debt => EarnMaxPolicyFamily::Debt,
        PolicyFamily::Swap => EarnMaxPolicyFamily::Swap,
    };
    earn_max_policy_constraints(&boundary, family).map_err(Into::into)
}

const fn strategy_lane_index(key: StrategyKey, reverse: bool) -> u8 {
    let base = match key {
        StrategyKey::OnycUsdc => 0,
        StrategyKey::OnycUsds => 2,
        StrategyKey::PrimeUsdc => 4,
        StrategyKey::PrimePyusd => 6,
        StrategyKey::PrimeUsds => 8,
        StrategyKey::SyrupUsdcUsdc => 10,
        StrategyKey::SyrupUsdcPyusd => 12,
    };
    base + reverse as u8
}

fn usdc_lane_index(key: StrategyKey, reverse: bool) -> Result<u8, Box<dyn Error>> {
    let usdc_key = match key {
        StrategyKey::OnycUsdc | StrategyKey::OnycUsds => StrategyKey::OnycUsdc,
        StrategyKey::PrimeUsdc | StrategyKey::PrimePyusd | StrategyKey::PrimeUsds => {
            StrategyKey::PrimeUsdc
        }
        StrategyKey::SyrupUsdcUsdc | StrategyKey::SyrupUsdcPyusd => StrategyKey::SyrupUsdcUsdc,
    };
    Ok(strategy_lane_index(usdc_key, reverse))
}

fn route_constraint_index(instruction: &Instruction, index: u8) -> Result<u8, Box<dyn Error>> {
    if instruction.data.get(..8) != Some(EARN_MAX_SHARED_ACCOUNTS_ROUTE.as_slice()) {
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

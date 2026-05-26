use crate::ids::*;
use crate::protocols::{
    jupiter_constraint, kamino_market_mint_constraint, kamino_mint_constraint,
    loyal_hub_constraint, unique_pubkeys,
};
use crate::squads::{
    create_program_interaction_action_instruction, derive_action_account, LoyalActionError, Result,
    SquadsInstructionConstraint,
};
use solana_sdk::{instruction::Instruction, pubkey::Pubkey};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct LoyalActionContext {
    pub settings: Pubkey,
    pub authority: Pubkey,
    pub delegated_signer: Pubkey,
    pub account_index: u8,
    pub vault: Pubkey,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct YieldRouteUniverse {
    pub stable_mints: Vec<Pubkey>,
    pub kamino_markets: Vec<Pubkey>,
    pub kamino_liquidity_mints: Vec<Pubkey>,
}

impl YieldRouteUniverse {
    pub fn new(
        stable_mints: Vec<Pubkey>,
        kamino_markets: Vec<Pubkey>,
        kamino_liquidity_mints: Vec<Pubkey>,
    ) -> Self {
        Self {
            stable_mints: unique_pubkeys(stable_mints),
            kamino_markets: unique_pubkeys(kamino_markets),
            kamino_liquidity_mints: unique_pubkeys(kamino_liquidity_mints),
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct JupiterSwapContract {
    pub program_id: Pubkey,
    pub exact_in_discriminator: u8,
    pub include_intermediate_token_accounts: bool,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SwapLane {
    Jupiter(JupiterSwapContract),
    LoyalHub {
        hub_authorizer: Pubkey,
        max_fee_bps: u16,
    },
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum KaminoScope {
    MarketMint,
    MintOnly,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RouteTopology {
    ThreeStep,
    CombinedKamino,
    AllInOne { scope: KaminoScope },
    SwapOnly,
}

impl Default for RouteTopology {
    fn default() -> Self {
        Self::ThreeStep
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct YieldRouteActionSeeds {
    pub withdraw: u64,
    pub swap: u64,
    pub deposit: u64,
}

impl Default for YieldRouteActionSeeds {
    fn default() -> Self {
        Self {
            withdraw: YIELD_ROUTE_WITHDRAW_ACTION_SEED,
            swap: YIELD_ROUTE_SWAP_ACTION_SEED,
            deposit: YIELD_ROUTE_DEPOSIT_ACTION_SEED,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct YieldRouteActionAccounts {
    pub withdraw: Pubkey,
    pub swap: Pubkey,
    pub deposit: Pubkey,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct LoyalActionStep {
    action_account: Pubkey,
    instruction_constraint_index: u8,
}

impl LoyalActionStep {
    pub fn new(action_account: Pubkey, instruction_constraint_index: u8) -> Self {
        Self {
            action_account,
            instruction_constraint_index,
        }
    }

    pub fn action_account(&self) -> Pubkey {
        self.action_account
    }

    pub fn instruction_constraint_index(&self) -> u8 {
        self.instruction_constraint_index
    }

    pub fn instruction_constraint_indexes(&self) -> Vec<u8> {
        vec![self.instruction_constraint_index]
    }
}

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct YieldRouteSteps {
    withdraw: Option<LoyalActionStep>,
    deposit: Option<LoyalActionStep>,
    jupiter_swap: Option<LoyalActionStep>,
    loyal_hub_swap: Option<LoyalActionStep>,
}

impl YieldRouteSteps {
    pub fn withdraw(&self) -> Result<LoyalActionStep> {
        self.withdraw.ok_or(LoyalActionError::MissingActionStep)
    }

    pub fn deposit(&self) -> Result<LoyalActionStep> {
        self.deposit.ok_or(LoyalActionError::MissingActionStep)
    }

    pub fn jupiter_swap(&self) -> Result<LoyalActionStep> {
        self.jupiter_swap.ok_or(LoyalActionError::MissingActionStep)
    }

    pub fn loyal_hub_swap(&self) -> Result<LoyalActionStep> {
        self.loyal_hub_swap
            .ok_or(LoyalActionError::MissingActionStep)
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct YieldRouteActionSpec {
    pub topology: RouteTopology,
    pub universe: YieldRouteUniverse,
    pub swap_lanes: Vec<SwapLane>,
    pub accounts: YieldRouteActionAccounts,
    pub instruction_count: usize,
    pub constraint_count: usize,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct YieldRouteActionSetup {
    pub accounts: YieldRouteActionAccounts,
    pub instructions: Vec<Instruction>,
    steps: YieldRouteSteps,
    pub spec: YieldRouteActionSpec,
}

impl YieldRouteActionSetup {
    pub fn steps(&self) -> YieldRouteSteps {
        self.steps
    }

    pub fn withdraw_step(&self) -> Result<LoyalActionStep> {
        self.steps.withdraw()
    }

    pub fn deposit_step(&self) -> Result<LoyalActionStep> {
        self.steps.deposit()
    }

    pub fn jupiter_swap_step(&self) -> Result<LoyalActionStep> {
        self.steps.jupiter_swap()
    }

    pub fn loyal_hub_swap_step(&self) -> Result<LoyalActionStep> {
        self.steps.loyal_hub_swap()
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct YieldRouteActionInstruction {
    pub account: Pubkey,
    pub instruction: Instruction,
    steps: YieldRouteSteps,
    pub spec: YieldRouteActionSpec,
}

impl YieldRouteActionInstruction {
    pub fn steps(&self) -> YieldRouteSteps {
        self.steps
    }

    pub fn jupiter_swap_step(&self) -> Result<LoyalActionStep> {
        self.steps.jupiter_swap()
    }

    pub fn loyal_hub_swap_step(&self) -> Result<LoyalActionStep> {
        self.steps.loyal_hub_swap()
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct YieldRouteActionPlan {
    pub context: LoyalActionContext,
    pub universe: YieldRouteUniverse,
    pub topology: RouteTopology,
    pub swap_lanes: Vec<SwapLane>,
    pub seeds: YieldRouteActionSeeds,
}

pub struct YieldRouteActionBuilder {
    plan: YieldRouteActionPlan,
}

impl YieldRouteActionBuilder {
    pub fn new(context: LoyalActionContext, universe: YieldRouteUniverse) -> Self {
        Self {
            plan: YieldRouteActionPlan {
                context,
                universe,
                topology: RouteTopology::default(),
                swap_lanes: Vec::new(),
                seeds: YieldRouteActionSeeds::default(),
            },
        }
    }

    pub fn topology(mut self, topology: RouteTopology) -> Self {
        self.plan.topology = topology;
        self
    }

    pub fn swap_lanes(mut self, swap_lanes: Vec<SwapLane>) -> Self {
        self.plan.swap_lanes = swap_lanes;
        self
    }

    pub fn seeds(mut self, seeds: YieldRouteActionSeeds) -> Self {
        self.plan.seeds = seeds;
        self
    }

    pub fn build(self) -> Result<YieldRouteActionSetup> {
        build_yield_route_actions(self.plan)
    }
}

pub fn create_three_step_yield_route_actions(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
    swap_lanes: Vec<SwapLane>,
    seeds: YieldRouteActionSeeds,
) -> Result<YieldRouteActionSetup> {
    YieldRouteActionBuilder::new(context, universe)
        .topology(RouteTopology::ThreeStep)
        .swap_lanes(swap_lanes)
        .seeds(seeds)
        .build()
}

pub fn create_combined_kamino_yield_route_actions(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
    swap_lanes: Vec<SwapLane>,
) -> Result<YieldRouteActionSetup> {
    YieldRouteActionBuilder::new(context, universe)
        .topology(RouteTopology::CombinedKamino)
        .swap_lanes(swap_lanes)
        .build()
}

pub fn create_all_in_one_market_mint_yield_route_action(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
    swap_lanes: Vec<SwapLane>,
) -> Result<YieldRouteActionSetup> {
    YieldRouteActionBuilder::new(context, universe)
        .topology(RouteTopology::AllInOne {
            scope: KaminoScope::MarketMint,
        })
        .swap_lanes(swap_lanes)
        .build()
}

pub fn create_all_in_one_mint_yield_route_action(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
    swap_lanes: Vec<SwapLane>,
) -> Result<YieldRouteActionSetup> {
    YieldRouteActionBuilder::new(context, universe)
        .topology(RouteTopology::AllInOne {
            scope: KaminoScope::MintOnly,
        })
        .swap_lanes(swap_lanes)
        .build()
}

pub fn create_swap_yield_route_action(
    context: LoyalActionContext,
    stable_mints: Vec<Pubkey>,
    swap_lanes: Vec<SwapLane>,
    action_seed: u64,
) -> Result<YieldRouteActionInstruction> {
    let stable_mints = unique_pubkeys(stable_mints);
    validate_stable_mints(&stable_mints)?;
    validate_swap_lanes(&swap_lanes)?;

    let account = derive_action_account(&context.settings, action_seed).0;
    let constraints = stable_swap_constraints(context.vault, stable_mints.clone(), &swap_lanes)?;
    let steps = swap_steps(account, &swap_lanes, 0);
    let instruction = create_program_interaction_action_instruction(
        context.settings,
        context.authority,
        context.delegated_signer,
        action_seed,
        context.account_index,
        constraints.clone(),
    )?;
    let accounts = YieldRouteActionAccounts {
        withdraw: account,
        swap: account,
        deposit: account,
    };

    Ok(YieldRouteActionInstruction {
        account,
        instruction,
        steps,
        spec: YieldRouteActionSpec {
            topology: RouteTopology::SwapOnly,
            universe: YieldRouteUniverse::new(stable_mints, vec![], vec![]),
            swap_lanes,
            accounts,
            instruction_count: 1,
            constraint_count: constraints.len(),
        },
    })
}

fn build_yield_route_actions(plan: YieldRouteActionPlan) -> Result<YieldRouteActionSetup> {
    validate_plan(&plan)?;

    match plan.topology {
        RouteTopology::ThreeStep => build_three_step(plan),
        RouteTopology::CombinedKamino => build_combined_kamino(plan),
        RouteTopology::AllInOne { scope } => build_all_in_one(plan, scope),
        RouteTopology::SwapOnly => build_swap_only(plan),
    }
}

fn build_swap_only(plan: YieldRouteActionPlan) -> Result<YieldRouteActionSetup> {
    let action = derive_action_account(&plan.context.settings, plan.seeds.swap).0;
    let accounts = YieldRouteActionAccounts {
        withdraw: action,
        swap: action,
        deposit: action,
    };
    let constraints = stable_swap_constraints(
        plan.context.vault,
        plan.universe.stable_mints.clone(),
        &plan.swap_lanes,
    )?;
    let instruction = action_instruction(plan.context, plan.seeds.swap, constraints.clone())?;
    let steps = swap_steps(action, &plan.swap_lanes, 0);

    setup(plan, accounts, vec![instruction], steps, constraints.len())
}

fn build_three_step(plan: YieldRouteActionPlan) -> Result<YieldRouteActionSetup> {
    let accounts = action_accounts(plan.context.settings, plan.seeds);
    let withdraw_constraints = vec![kamino_market_mint_constraint(
        plan.context.vault,
        plan.universe.kamino_markets.clone(),
        plan.universe.kamino_liquidity_mints.clone(),
        KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
    )];
    let swap_constraints = stable_swap_constraints(
        plan.context.vault,
        plan.universe.stable_mints.clone(),
        &plan.swap_lanes,
    )?;
    let deposit_constraints = vec![kamino_market_mint_constraint(
        plan.context.vault,
        plan.universe.kamino_markets.clone(),
        plan.universe.kamino_liquidity_mints.clone(),
        KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
    )];
    let instructions = vec![
        action_instruction(
            plan.context,
            plan.seeds.withdraw,
            withdraw_constraints.clone(),
        )?,
        action_instruction(plan.context, plan.seeds.swap, swap_constraints.clone())?,
        action_instruction(
            plan.context,
            plan.seeds.deposit,
            deposit_constraints.clone(),
        )?,
    ];
    let steps = YieldRouteSteps {
        withdraw: Some(LoyalActionStep::new(accounts.withdraw, 0)),
        deposit: Some(LoyalActionStep::new(accounts.deposit, 0)),
        ..swap_steps(accounts.swap, &plan.swap_lanes, 0)
    };

    setup(
        plan,
        accounts,
        instructions,
        steps,
        2 + swap_constraints.len(),
    )
}

fn build_combined_kamino(plan: YieldRouteActionPlan) -> Result<YieldRouteActionSetup> {
    let rebalance = derive_action_account(&plan.context.settings, plan.seeds.withdraw).0;
    let swap = derive_action_account(&plan.context.settings, plan.seeds.swap).0;
    let accounts = YieldRouteActionAccounts {
        withdraw: rebalance,
        swap,
        deposit: rebalance,
    };
    let kamino_constraints = vec![
        kamino_market_mint_constraint(
            plan.context.vault,
            plan.universe.kamino_markets.clone(),
            plan.universe.kamino_liquidity_mints.clone(),
            KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
        ),
        kamino_market_mint_constraint(
            plan.context.vault,
            plan.universe.kamino_markets.clone(),
            plan.universe.kamino_liquidity_mints.clone(),
            KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
        ),
    ];
    let swap_constraints = stable_swap_constraints(
        plan.context.vault,
        plan.universe.stable_mints.clone(),
        &plan.swap_lanes,
    )?;
    let instructions = vec![
        action_instruction(
            plan.context,
            plan.seeds.withdraw,
            kamino_constraints.clone(),
        )?,
        action_instruction(plan.context, plan.seeds.swap, swap_constraints.clone())?,
    ];
    let steps = YieldRouteSteps {
        withdraw: Some(LoyalActionStep::new(accounts.withdraw, 0)),
        deposit: Some(LoyalActionStep::new(accounts.deposit, 1)),
        ..swap_steps(accounts.swap, &plan.swap_lanes, 0)
    };

    setup(
        plan,
        accounts,
        instructions,
        steps,
        2 + swap_constraints.len(),
    )
}

fn build_all_in_one(
    plan: YieldRouteActionPlan,
    scope: KaminoScope,
) -> Result<YieldRouteActionSetup> {
    let action = derive_action_account(&plan.context.settings, plan.seeds.withdraw).0;
    let accounts = YieldRouteActionAccounts {
        withdraw: action,
        swap: action,
        deposit: action,
    };
    let mut constraints = Vec::with_capacity(2 + plan.swap_lanes.len());
    constraints.push(kamino_constraint(
        scope,
        plan.context.vault,
        plan.universe.kamino_markets.clone(),
        plan.universe.kamino_liquidity_mints.clone(),
        KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
    ));
    constraints.extend(stable_swap_constraints(
        plan.context.vault,
        plan.universe.stable_mints.clone(),
        &plan.swap_lanes,
    )?);
    constraints.push(kamino_constraint(
        scope,
        plan.context.vault,
        plan.universe.kamino_markets.clone(),
        plan.universe.kamino_liquidity_mints.clone(),
        KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
    ));
    let instruction = action_instruction(plan.context, plan.seeds.withdraw, constraints.clone())?;
    let steps = YieldRouteSteps {
        withdraw: Some(LoyalActionStep::new(action, 0)),
        deposit: Some(LoyalActionStep::new(
            action,
            1 + plan.swap_lanes.len() as u8,
        )),
        ..swap_steps(action, &plan.swap_lanes, 1)
    };

    setup(plan, accounts, vec![instruction], steps, constraints.len())
}

fn setup(
    plan: YieldRouteActionPlan,
    accounts: YieldRouteActionAccounts,
    instructions: Vec<Instruction>,
    steps: YieldRouteSteps,
    constraint_count: usize,
) -> Result<YieldRouteActionSetup> {
    Ok(YieldRouteActionSetup {
        accounts,
        spec: YieldRouteActionSpec {
            topology: plan.topology,
            universe: plan.universe,
            swap_lanes: plan.swap_lanes,
            accounts,
            instruction_count: instructions.len(),
            constraint_count,
        },
        instructions,
        steps,
    })
}

fn action_accounts(settings: Pubkey, seeds: YieldRouteActionSeeds) -> YieldRouteActionAccounts {
    YieldRouteActionAccounts {
        withdraw: derive_action_account(&settings, seeds.withdraw).0,
        swap: derive_action_account(&settings, seeds.swap).0,
        deposit: derive_action_account(&settings, seeds.deposit).0,
    }
}

fn action_instruction(
    context: LoyalActionContext,
    action_seed: u64,
    constraints: Vec<SquadsInstructionConstraint>,
) -> Result<Instruction> {
    create_program_interaction_action_instruction(
        context.settings,
        context.authority,
        context.delegated_signer,
        action_seed,
        context.account_index,
        constraints,
    )
}

fn kamino_constraint(
    scope: KaminoScope,
    vault: Pubkey,
    markets: Vec<Pubkey>,
    liquidity_mints: Vec<Pubkey>,
    discriminator: [u8; 8],
) -> SquadsInstructionConstraint {
    match scope {
        KaminoScope::MarketMint => {
            kamino_market_mint_constraint(vault, markets, liquidity_mints, discriminator)
        }
        KaminoScope::MintOnly => kamino_mint_constraint(vault, liquidity_mints, discriminator),
    }
}

fn stable_swap_constraints(
    vault: Pubkey,
    allowed_mints: Vec<Pubkey>,
    swap_lanes: &[SwapLane],
) -> Result<Vec<SquadsInstructionConstraint>> {
    validate_stable_mints(&allowed_mints)?;
    validate_swap_lanes(swap_lanes)?;

    let allowed_mints = unique_pubkeys(allowed_mints);
    let mut constraints = Vec::with_capacity(swap_lanes.len());
    for lane in swap_lanes {
        match *lane {
            SwapLane::Jupiter(contract) => {
                constraints.push(jupiter_constraint(vault, allowed_mints.clone(), contract))
            }
            SwapLane::LoyalHub {
                hub_authorizer,
                max_fee_bps,
            } => constraints.push(loyal_hub_constraint(
                vault,
                allowed_mints.clone(),
                hub_authorizer,
                max_fee_bps,
            )),
        }
    }
    Ok(constraints)
}

fn swap_steps(action: Pubkey, swap_lanes: &[SwapLane], start_index: u8) -> YieldRouteSteps {
    let mut steps = YieldRouteSteps::default();
    for (offset, lane) in swap_lanes.iter().enumerate() {
        let step = LoyalActionStep::new(action, start_index + offset as u8);
        match lane {
            SwapLane::Jupiter(_) => steps.jupiter_swap.get_or_insert(step),
            SwapLane::LoyalHub { .. } => steps.loyal_hub_swap.get_or_insert(step),
        };
    }
    steps
}

fn validate_plan(plan: &YieldRouteActionPlan) -> Result<()> {
    validate_stable_mints(&plan.universe.stable_mints)?;
    if plan.topology != RouteTopology::SwapOnly {
        validate_kamino_universe(&plan.universe)?;
    }
    validate_swap_lanes(&plan.swap_lanes)?;
    validate_action_seeds(plan.topology, plan.seeds)
}

fn validate_stable_mints(stable_mints: &[Pubkey]) -> Result<()> {
    if stable_mints.is_empty() {
        return Err(LoyalActionError::EmptyStableMints);
    }
    Ok(())
}

fn validate_kamino_universe(universe: &YieldRouteUniverse) -> Result<()> {
    if universe.kamino_markets.is_empty() {
        return Err(LoyalActionError::EmptyKaminoMarkets);
    }
    if universe.kamino_liquidity_mints.is_empty() {
        return Err(LoyalActionError::EmptyKaminoLiquidityMints);
    }
    Ok(())
}

fn validate_swap_lanes(swap_lanes: &[SwapLane]) -> Result<()> {
    if swap_lanes.is_empty() {
        return Err(LoyalActionError::EmptySwapLanes);
    }
    for lane in swap_lanes {
        if let SwapLane::LoyalHub { max_fee_bps, .. } = lane {
            if *max_fee_bps > 10_000 {
                return Err(LoyalActionError::InvalidFeeBps);
            }
        }
    }
    Ok(())
}

fn validate_action_seeds(topology: RouteTopology, seeds: YieldRouteActionSeeds) -> Result<()> {
    match topology {
        RouteTopology::ThreeStep
            if seeds.withdraw == seeds.swap
                || seeds.withdraw == seeds.deposit
                || seeds.swap == seeds.deposit =>
        {
            Err(LoyalActionError::DuplicateActionSeeds)
        }
        RouteTopology::CombinedKamino if seeds.withdraw == seeds.swap => {
            Err(LoyalActionError::DuplicateActionSeeds)
        }
        _ => Ok(()),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn context() -> LoyalActionContext {
        LoyalActionContext {
            settings: Pubkey::new_unique(),
            authority: Pubkey::new_unique(),
            delegated_signer: Pubkey::new_unique(),
            account_index: 0,
            vault: Pubkey::new_unique(),
        }
    }

    fn universe() -> YieldRouteUniverse {
        let stable = Pubkey::new_unique();
        let market = Pubkey::new_unique();
        let mint = Pubkey::new_unique();
        YieldRouteUniverse::new(vec![stable, stable], vec![market, market], vec![mint, mint])
    }

    fn jupiter_lane(include_intermediate_token_accounts: bool) -> SwapLane {
        SwapLane::Jupiter(JupiterSwapContract {
            program_id: JUPITER_V6_PROGRAM_ID,
            exact_in_discriminator: 3,
            include_intermediate_token_accounts,
        })
    }

    #[test]
    fn deduplicates_route_universe_in_order() {
        let stable = Pubkey::new_unique();
        let market = Pubkey::new_unique();
        let mint = Pubkey::new_unique();

        let universe =
            YieldRouteUniverse::new(vec![stable, stable], vec![market, market], vec![mint, mint]);

        assert_eq!(universe.stable_mints, vec![stable]);
        assert_eq!(universe.kamino_markets, vec![market]);
        assert_eq!(universe.kamino_liquidity_mints, vec![mint]);
    }

    #[test]
    fn rejects_empty_swap_lanes() {
        let result =
            create_all_in_one_market_mint_yield_route_action(context(), universe(), vec![]);
        assert_eq!(result.unwrap_err(), LoyalActionError::EmptySwapLanes);
    }

    #[test]
    fn builds_one_all_in_one_market_mint_action_with_steps() {
        let context = context();
        let setup = create_all_in_one_market_mint_yield_route_action(
            context,
            universe(),
            vec![jupiter_lane(false)],
        )
        .unwrap();

        assert_eq!(setup.instructions.len(), 1);
        assert_eq!(setup.accounts.withdraw, setup.accounts.swap);
        assert_eq!(setup.accounts.swap, setup.accounts.deposit);
        assert_eq!(
            setup.accounts.withdraw,
            derive_action_account(&context.settings, YIELD_ROUTE_WITHDRAW_ACTION_SEED).0
        );
        assert_eq!(
            setup
                .withdraw_step()
                .unwrap()
                .instruction_constraint_index(),
            0
        );
        assert_eq!(
            setup
                .jupiter_swap_step()
                .unwrap()
                .instruction_constraint_index(),
            1
        );
        assert_eq!(
            setup.deposit_step().unwrap().instruction_constraint_index(),
            2
        );
        assert_eq!(setup.spec.constraint_count, 3);
    }

    #[test]
    fn missing_step_accessors_return_typed_error() {
        let context = context();
        let setup = create_swap_yield_route_action(
            context,
            vec![Pubkey::new_unique()],
            vec![jupiter_lane(true)],
            YIELD_ROUTE_STANDALONE_ACTION_SEED,
        )
        .unwrap();

        assert_eq!(
            setup.loyal_hub_swap_step().unwrap_err(),
            LoyalActionError::MissingActionStep
        );
    }

    #[test]
    fn builds_three_step_action_accounts_from_default_seeds() {
        let context = context();
        let setup = create_three_step_yield_route_actions(
            context,
            universe(),
            vec![jupiter_lane(true)],
            YieldRouteActionSeeds::default(),
        )
        .unwrap();

        assert_eq!(setup.instructions.len(), 3);
        assert_eq!(
            setup.accounts.withdraw,
            derive_action_account(&context.settings, YIELD_ROUTE_WITHDRAW_ACTION_SEED).0
        );
        assert_eq!(
            setup.accounts.swap,
            derive_action_account(&context.settings, YIELD_ROUTE_SWAP_ACTION_SEED).0
        );
        assert_eq!(
            setup.accounts.deposit,
            derive_action_account(&context.settings, YIELD_ROUTE_DEPOSIT_ACTION_SEED).0
        );
        assert_eq!(
            setup
                .withdraw_step()
                .unwrap()
                .instruction_constraint_index(),
            0
        );
        assert_eq!(
            setup
                .jupiter_swap_step()
                .unwrap()
                .instruction_constraint_index(),
            0
        );
        assert_eq!(
            setup.deposit_step().unwrap().instruction_constraint_index(),
            0
        );
    }

    #[test]
    fn builds_swap_action_with_loyal_hub_lane() {
        let context = context();
        let setup = create_swap_yield_route_action(
            context,
            vec![Pubkey::new_unique(), Pubkey::new_unique()],
            vec![
                jupiter_lane(true),
                SwapLane::LoyalHub {
                    hub_authorizer: Pubkey::new_unique(),
                    max_fee_bps: 50,
                },
            ],
            YIELD_ROUTE_STANDALONE_ACTION_SEED,
        )
        .unwrap();

        assert_eq!(
            setup.account,
            derive_action_account(&context.settings, YIELD_ROUTE_STANDALONE_ACTION_SEED).0
        );
        assert_eq!(
            setup.instruction.program_id,
            SQUADS_SMART_ACCOUNT_PROGRAM_ID
        );
        assert_eq!(
            setup
                .jupiter_swap_step()
                .unwrap()
                .instruction_constraint_index(),
            0
        );
        assert_eq!(
            setup
                .loyal_hub_swap_step()
                .unwrap()
                .instruction_constraint_index(),
            1
        );
    }
}

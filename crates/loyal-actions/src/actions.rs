use crate::ids::*;
use crate::jupiter::{
    JupiterCrossMintPolicySeeds, JupiterCrossMintPolicySpec, JupiterCrossMintSourceShard,
    JupiterV2Dialect,
};
use crate::lookup_tables::YieldRouteLookupTableRequirements;
use crate::protocols::{
    jupiter_constraint, jupiter_cross_mint_constraint, kamino_deposit_reserve_liquidity_constraint,
    kamino_deposit_reserve_liquidity_market_mint_constraint, kamino_init_obligation_constraint,
    kamino_redeem_reserve_collateral_constraint,
    kamino_redeem_reserve_collateral_market_mint_constraint, kamino_refresh_obligation_constraint,
    loyal_hub_constraint, unique_pubkeys,
};
use crate::squads::{
    create_program_interaction_action_instruction,
    create_program_interaction_action_instruction_with_daily_spending_limits,
    derive_action_account, update_program_interaction_action_instruction, LoyalActionError, Result,
    SquadsInstructionConstraint,
};
use crate::EarnStablecoinPair;
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

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum KaminoStableRiskProfile {
    Safe,
    Medium,
    Aggressive,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum YieldRouteUniversePreset {
    KaminoStable(KaminoStableRiskProfile),
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

pub fn yield_route_universe_for_preset(preset: YieldRouteUniversePreset) -> YieldRouteUniverse {
    match preset {
        YieldRouteUniversePreset::KaminoStable(profile) => YieldRouteUniverse::new(
            supported_yield_route_stable_mints(),
            kamino_stable_markets_for_profile(profile),
            supported_yield_route_stable_mints(),
        ),
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct JupiterSwapContract {
    pub program_id: Pubkey,
    pub exact_in_discriminator: [u8; 8],
    pub max_slippage_bps: u16,
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
pub enum RouteTopology {
    ThreeStep,
    CombinedKamino,
    AllInOne,
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

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct LoyalActionRoute<const N: usize> {
    action_account: Pubkey,
    instruction_constraint_indexes: [u8; N],
}

pub type SameMintRoute = LoyalActionRoute<2>;
pub type CrossMintRoute = LoyalActionRoute<3>;

impl<const N: usize> LoyalActionRoute<N> {
    pub fn action_account(&self) -> Pubkey {
        self.action_account
    }

    pub fn instruction_constraint_indexes(&self) -> &[u8; N] {
        &self.instruction_constraint_indexes
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
    lookup_table_requirements: YieldRouteLookupTableRequirements,
}

impl YieldRouteActionSetup {
    pub fn steps(&self) -> YieldRouteSteps {
        self.steps
    }

    pub fn lookup_table_requirements(&self) -> &YieldRouteLookupTableRequirements {
        &self.lookup_table_requirements
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

    pub fn same_mint_route(&self) -> Result<SameMintRoute> {
        coalesce_steps([self.withdraw_step()?, self.deposit_step()?])
    }

    pub fn jupiter_route(&self) -> Result<CrossMintRoute> {
        coalesce_steps([
            self.withdraw_step()?,
            self.jupiter_swap_step()?,
            self.deposit_step()?,
        ])
    }

    pub fn loyal_hub_route(&self) -> Result<CrossMintRoute> {
        coalesce_steps([
            self.withdraw_step()?,
            self.loyal_hub_swap_step()?,
            self.deposit_step()?,
        ])
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct YieldRouteActionInstruction {
    pub account: Pubkey,
    pub instruction: Instruction,
    steps: YieldRouteSteps,
    pub spec: YieldRouteActionSpec,
    lookup_table_requirements: YieldRouteLookupTableRequirements,
}

/// One of the two durable cross-mint policy shards.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct JupiterCrossMintPolicyAction {
    pub account: Pubkey,
    pub instruction: Instruction,
    pub spec: JupiterCrossMintPolicySpec,
    lookup_table_requirements: YieldRouteLookupTableRequirements,
}

impl JupiterCrossMintPolicyAction {
    /// Select the immutable constraint index for a fresh Jupiter build.
    ///
    /// Pair choice is deliberately dynamic, but its source must belong to this
    /// shard and its target must be a different canonical Earn stablecoin.
    pub fn step_for_pair(
        &self,
        pair: EarnStablecoinPair,
        dialect: JupiterV2Dialect,
    ) -> Result<LoyalActionStep> {
        if !self.spec.source_shard.contains(pair.input_mint)
            || pair.input_mint == pair.output_mint
            || crate::earn_stablecoin(pair.output_mint).is_none()
        {
            return Err(LoyalActionError::MissingActionStep);
        }
        let constraint_index = match dialect {
            JupiterV2Dialect::RouteV2 => 0,
            JupiterV2Dialect::SharedAccountsRouteV2 => 1,
        };
        Ok(LoyalActionStep::new(self.account, constraint_index))
    }

    pub fn lookup_table_requirements(&self) -> &YieldRouteLookupTableRequirements {
        &self.lookup_table_requirements
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct JupiterCrossMintPolicySet {
    pub classic: JupiterCrossMintPolicyAction,
    pub token_2022: JupiterCrossMintPolicyAction,
}

impl JupiterCrossMintPolicySet {
    pub fn action_for_source_mint(
        &self,
        source_mint: Pubkey,
    ) -> Result<&JupiterCrossMintPolicyAction> {
        if self.classic.spec.source_shard.contains(source_mint) {
            Ok(&self.classic)
        } else if self.token_2022.spec.source_shard.contains(source_mint) {
            Ok(&self.token_2022)
        } else {
            Err(LoyalActionError::InvalidStablecoinPair)
        }
    }
}

impl YieldRouteActionInstruction {
    pub fn steps(&self) -> YieldRouteSteps {
        self.steps
    }

    pub fn lookup_table_requirements(&self) -> &YieldRouteLookupTableRequirements {
        &self.lookup_table_requirements
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
        .topology(RouteTopology::AllInOne)
        .swap_lanes(swap_lanes)
        .build()
}

pub fn create_preset_all_in_one_yield_route_action(
    context: LoyalActionContext,
    preset: YieldRouteUniversePreset,
    swap_lanes: Vec<SwapLane>,
) -> Result<YieldRouteActionSetup> {
    create_all_in_one_market_mint_yield_route_action(
        context,
        yield_route_universe_for_preset(preset),
        swap_lanes,
    )
}

pub fn update_all_in_one_market_mint_yield_route_action(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
    swap_lanes: Vec<SwapLane>,
    policy: Pubkey,
    account_index: u8,
) -> Result<YieldRouteActionSetup> {
    validate_stable_mints(&universe.stable_mints)?;
    validate_kamino_universe(&universe)?;
    validate_swap_lane_values(&swap_lanes)?;

    let constraints = all_in_one_constraints(context.vault, &universe, &swap_lanes)?;
    let instruction = update_program_interaction_action_instruction(
        context.settings,
        context.authority,
        policy,
        context.delegated_signer,
        account_index,
        constraints.clone(),
    )?;
    let accounts = YieldRouteActionAccounts {
        withdraw: policy,
        swap: policy,
        deposit: policy,
    };
    let steps = YieldRouteSteps {
        withdraw: Some(LoyalActionStep::new(policy, 0)),
        deposit: Some(LoyalActionStep::new(policy, 1 + swap_lanes.len() as u8)),
        ..swap_steps(policy, &swap_lanes, 1)
    };

    setup_from_parts(
        context,
        RouteTopology::AllInOne,
        universe,
        swap_lanes,
        accounts,
        vec![instruction],
        steps,
        constraints.len(),
    )
}

pub fn create_same_mint_market_mint_yield_route_action(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
    seed: u64,
) -> Result<YieldRouteActionSetup> {
    let constraints = same_mint_market_mint_constraints(context.vault, &universe)?;
    let policy = derive_action_account(&context.settings, seed).0;
    let instruction = action_instruction(context, seed, constraints.clone())?;
    same_mint_market_mint_setup(
        context,
        universe,
        policy,
        vec![instruction],
        constraints.len(),
    )
}

pub fn update_same_mint_market_mint_yield_route_action(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
    policy: Pubkey,
    account_index: u8,
) -> Result<YieldRouteActionSetup> {
    let constraints = same_mint_market_mint_constraints(context.vault, &universe)?;
    let instruction = update_program_interaction_action_instruction(
        context.settings,
        context.authority,
        policy,
        context.delegated_signer,
        account_index,
        constraints.clone(),
    )?;
    same_mint_market_mint_setup(
        context,
        universe,
        policy,
        vec![instruction],
        constraints.len(),
    )
}

pub fn create_init_obligation_yield_route_action(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
    seed: u64,
) -> Result<YieldRouteActionSetup> {
    validate_stable_mints(&universe.stable_mints)?;
    validate_kamino_universe(&universe)?;

    let constraints = init_obligation_setup_constraints(context.vault, &universe);
    let policy = derive_action_account(&context.settings, seed).0;
    let instruction = action_instruction(context, seed, constraints.clone())?;
    init_obligation_setup(
        context,
        universe,
        policy,
        vec![instruction],
        constraints.len(),
    )
}

pub fn update_init_obligation_yield_route_action(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
    policy: Pubkey,
    account_index: u8,
) -> Result<YieldRouteActionSetup> {
    validate_stable_mints(&universe.stable_mints)?;
    validate_kamino_universe(&universe)?;

    let constraints = init_obligation_setup_constraints(context.vault, &universe);
    let instruction = update_program_interaction_action_instruction(
        context.settings,
        context.authority,
        policy,
        context.delegated_signer,
        account_index,
        constraints.clone(),
    )?;
    init_obligation_setup(
        context,
        universe,
        policy,
        vec![instruction],
        constraints.len(),
    )
}

fn init_obligation_setup_constraints(
    vault: Pubkey,
    universe: &YieldRouteUniverse,
) -> Vec<SquadsInstructionConstraint> {
    vec![
        kamino_deposit_reserve_liquidity_market_mint_constraint(
            vault,
            universe.kamino_markets.clone(),
            universe.kamino_liquidity_mints.clone(),
        ),
        kamino_init_obligation_constraint(vault, universe.kamino_markets.clone()),
    ]
}

fn same_mint_market_mint_constraints(
    vault: Pubkey,
    universe: &YieldRouteUniverse,
) -> Result<Vec<SquadsInstructionConstraint>> {
    validate_stable_mints(&universe.stable_mints)?;
    validate_kamino_universe(universe)?;

    Ok(vec![
        kamino_redeem_reserve_collateral_market_mint_constraint(
            vault,
            universe.kamino_markets.clone(),
            universe.kamino_liquidity_mints.clone(),
        ),
        kamino_deposit_reserve_liquidity_market_mint_constraint(
            vault,
            universe.kamino_markets.clone(),
            universe.kamino_liquidity_mints.clone(),
        ),
    ])
}

fn same_mint_market_mint_setup(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
    policy: Pubkey,
    instructions: Vec<Instruction>,
    constraint_count: usize,
) -> Result<YieldRouteActionSetup> {
    let accounts = YieldRouteActionAccounts {
        withdraw: policy,
        swap: policy,
        deposit: policy,
    };
    let steps = YieldRouteSteps {
        withdraw: Some(LoyalActionStep::new(policy, 0)),
        deposit: Some(LoyalActionStep::new(policy, 1)),
        ..YieldRouteSteps::default()
    };

    setup_from_parts(
        context,
        RouteTopology::AllInOne,
        universe,
        Vec::new(),
        accounts,
        instructions,
        steps,
        constraint_count,
    )
}

fn init_obligation_setup(
    context: LoyalActionContext,
    universe: YieldRouteUniverse,
    policy: Pubkey,
    instructions: Vec<Instruction>,
    constraint_count: usize,
) -> Result<YieldRouteActionSetup> {
    let accounts = YieldRouteActionAccounts {
        withdraw: policy,
        swap: policy,
        deposit: policy,
    };

    setup_from_parts(
        context,
        RouteTopology::AllInOne,
        universe,
        Vec::new(),
        accounts,
        instructions,
        YieldRouteSteps::default(),
        constraint_count,
    )
}

pub fn detect_yield_route_universe_preset(
    kamino_markets: &[Pubkey],
) -> Option<YieldRouteUniversePreset> {
    let markets = unique_pubkeys(kamino_markets.to_vec());
    [
        KaminoStableRiskProfile::Safe,
        KaminoStableRiskProfile::Medium,
        KaminoStableRiskProfile::Aggressive,
    ]
    .into_iter()
    .find(|profile| markets == kamino_stable_markets_for_profile(*profile))
    .map(YieldRouteUniversePreset::KaminoStable)
}

fn kamino_stable_markets_for_profile(profile: KaminoStableRiskProfile) -> Vec<Pubkey> {
    let mut markets = safe_kamino_markets();
    if matches!(
        profile,
        KaminoStableRiskProfile::Medium | KaminoStableRiskProfile::Aggressive
    ) {
        markets.extend([
            KAMINO_JLP_MARKET,
            KAMINO_BITCOIN_MARKET,
            KAMINO_SUPERSTATE_OPENING_BELL_MARKET,
        ]);
    }
    if matches!(profile, KaminoStableRiskProfile::Aggressive) {
        markets.extend([
            KAMINO_HUMA_MARKET,
            KAMINO_SOLSTICE_MARKET,
            KAMINO_XSTOCKS_MARKET,
            KAMINO_ALTCOINS_MARKET,
        ]);
    }
    unique_pubkeys(markets)
}

// Production Kamino Stable preset allowlists are sourced from the local Kamino
// cache/report; USCC was additionally verified against Multiliquid's Solana
// deployment docs.
fn safe_kamino_markets() -> Vec<Pubkey> {
    vec![
        KAMINO_MAIN_MARKET,
        KAMINO_FIGURE_MARKET,
        KAMINO_MAPLE_MARKET,
        KAMINO_ONRE_MARKET,
        KAMINO_ETHENA_MARKET,
    ]
}

pub fn supported_yield_route_stable_mints() -> Vec<Pubkey> {
    vec![
        USDC_MINT,
        USDT_MINT,
        PYUSD_MINT,
        USDS_MINT,
        USDG_MINT,
        USDE_MINT,
        SUSDE_MINT,
        CASH_MINT,
        SYRUP_USDC_MINT,
        USD1_MINT,
        FDUSD_MINT,
        AUSD_MINT,
        EUSX_MINT,
        USCC_MINT,
    ]
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
        lookup_table_requirements: action_lookup_table_requirements(context, accounts),
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

/// Create one durable source-mint shard with both supported Jupiter dialects.
pub fn create_jupiter_cross_mint_policy_action(
    context: LoyalActionContext,
    spec: JupiterCrossMintPolicySpec,
    action_seed: u64,
) -> Result<JupiterCrossMintPolicyAction> {
    validate_jupiter_cross_mint_policy_spec(spec)?;
    let account = derive_action_account(&context.settings, action_seed).0;
    let constraints = vec![
        jupiter_cross_mint_constraint(
            context.vault,
            JupiterV2Dialect::RouteV2,
            spec.max_slippage_bps,
        ),
        jupiter_cross_mint_constraint(
            context.vault,
            JupiterV2Dialect::SharedAccountsRouteV2,
            spec.max_slippage_bps,
        ),
    ];
    let spending_limits = spec
        .source_shard
        .source_mints()
        .into_iter()
        .map(|mint| (mint, spec.daily_source_mint_spending_cap))
        .collect::<Vec<_>>();
    let instruction = create_program_interaction_action_instruction_with_daily_spending_limits(
        context.settings,
        context.authority,
        context.delegated_signer,
        action_seed,
        context.account_index,
        constraints,
        &spending_limits,
    )?;
    let accounts = YieldRouteActionAccounts {
        withdraw: account,
        swap: account,
        deposit: account,
    };

    Ok(JupiterCrossMintPolicyAction {
        account,
        instruction,
        spec,
        lookup_table_requirements: action_lookup_table_requirements(context, accounts),
    })
}

/// Build the complete V1 cross-mint authority set.
///
/// The two policies have disjoint source mints and both contain both Jupiter
/// dialects, so each source-mint cap exists exactly once.
pub fn create_jupiter_cross_mint_policy_set(
    context: LoyalActionContext,
    max_slippage_bps: u16,
    daily_source_mint_spending_cap: u64,
    seeds: JupiterCrossMintPolicySeeds,
) -> Result<JupiterCrossMintPolicySet> {
    if seeds.classic == seeds.token_2022 {
        return Err(LoyalActionError::DuplicateActionSeeds);
    }
    let create = |source_shard, action_seed| {
        create_jupiter_cross_mint_policy_action(
            context,
            JupiterCrossMintPolicySpec {
                source_shard,
                max_slippage_bps,
                daily_source_mint_spending_cap,
            },
            action_seed,
        )
    };

    Ok(JupiterCrossMintPolicySet {
        classic: create(JupiterCrossMintSourceShard::Classic, seeds.classic)?,
        token_2022: create(JupiterCrossMintSourceShard::Token2022, seeds.token_2022)?,
    })
}

fn validate_jupiter_cross_mint_policy_spec(spec: JupiterCrossMintPolicySpec) -> Result<()> {
    if spec.max_slippage_bps == 0 || spec.max_slippage_bps > 10_000 {
        return Err(LoyalActionError::InvalidSlippageBps);
    }
    if spec.daily_source_mint_spending_cap == 0 {
        return Err(LoyalActionError::InvalidDailySpendingCap);
    }
    Ok(())
}

fn build_yield_route_actions(plan: YieldRouteActionPlan) -> Result<YieldRouteActionSetup> {
    validate_plan(&plan)?;

    match plan.topology {
        RouteTopology::ThreeStep => build_three_step(plan),
        RouteTopology::CombinedKamino => build_combined_kamino(plan),
        RouteTopology::AllInOne => build_all_in_one(plan),
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
    let withdraw_constraints = vec![kamino_redeem_reserve_collateral_constraint(
        plan.context.vault,
        plan.universe.kamino_markets.clone(),
        plan.universe.kamino_liquidity_mints.clone(),
    )];
    let swap_constraints = stable_swap_constraints(
        plan.context.vault,
        plan.universe.stable_mints.clone(),
        &plan.swap_lanes,
    )?;
    let deposit_constraints = vec![kamino_deposit_reserve_liquidity_constraint(
        plan.context.vault,
        plan.universe.kamino_markets.clone(),
        plan.universe.kamino_liquidity_mints.clone(),
    )];
    let setup_constraints = vec![kamino_init_obligation_constraint(
        plan.context.vault,
        plan.universe.kamino_markets.clone(),
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
            [deposit_constraints.clone(), setup_constraints.clone()].concat(),
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
        2 + swap_constraints.len() + setup_constraints.len(),
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
        kamino_redeem_reserve_collateral_constraint(
            plan.context.vault,
            plan.universe.kamino_markets.clone(),
            plan.universe.kamino_liquidity_mints.clone(),
        ),
        kamino_deposit_reserve_liquidity_constraint(
            plan.context.vault,
            plan.universe.kamino_markets.clone(),
            plan.universe.kamino_liquidity_mints.clone(),
        ),
        kamino_init_obligation_constraint(plan.context.vault, plan.universe.kamino_markets.clone()),
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
        2 + swap_constraints.len() + 1,
    )
}

fn build_all_in_one(plan: YieldRouteActionPlan) -> Result<YieldRouteActionSetup> {
    let action = derive_action_account(&plan.context.settings, plan.seeds.withdraw).0;
    let accounts = YieldRouteActionAccounts {
        withdraw: action,
        swap: action,
        deposit: action,
    };
    let constraints = all_in_one_constraints(plan.context.vault, &plan.universe, &plan.swap_lanes)?;
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

fn all_in_one_constraints(
    vault: Pubkey,
    universe: &YieldRouteUniverse,
    swap_lanes: &[SwapLane],
) -> Result<Vec<SquadsInstructionConstraint>> {
    let compact_same_mint_usdc = is_compact_same_mint_usdc_policy(universe, swap_lanes);
    let mut constraints = Vec::with_capacity(3 + swap_lanes.len());
    if compact_same_mint_usdc {
        constraints.push(kamino_redeem_reserve_collateral_market_mint_constraint(
            vault,
            universe.kamino_markets.clone(),
            universe.kamino_liquidity_mints.clone(),
        ));
    } else {
        constraints.push(kamino_redeem_reserve_collateral_constraint(
            vault,
            universe.kamino_markets.clone(),
            universe.kamino_liquidity_mints.clone(),
        ));
    }
    constraints.extend(stable_swap_constraints(
        vault,
        universe.stable_mints.clone(),
        swap_lanes,
    )?);
    if compact_same_mint_usdc {
        constraints.push(kamino_deposit_reserve_liquidity_market_mint_constraint(
            vault,
            universe.kamino_markets.clone(),
            universe.kamino_liquidity_mints.clone(),
        ));
    } else {
        constraints.push(kamino_deposit_reserve_liquidity_constraint(
            vault,
            universe.kamino_markets.clone(),
            universe.kamino_liquidity_mints.clone(),
        ));
    }
    if !compact_same_mint_usdc {
        constraints.push(kamino_refresh_obligation_constraint(
            vault,
            universe.kamino_markets.clone(),
        ));
    } else {
        constraints.push(kamino_init_obligation_constraint(
            vault,
            universe.kamino_markets.clone(),
        ));
    }
    Ok(constraints)
}

fn is_compact_same_mint_usdc_policy(
    universe: &YieldRouteUniverse,
    swap_lanes: &[SwapLane],
) -> bool {
    if !swap_lanes.is_empty()
        || unique_pubkeys(universe.stable_mints.clone()) != vec![USDC_MINT]
        || unique_pubkeys(universe.kamino_liquidity_mints.clone()) != vec![USDC_MINT]
    {
        return false;
    }

    let markets = unique_pubkeys(universe.kamino_markets.clone());
    if markets.is_empty() {
        return false;
    }
    for market in markets {
        match market {
            KAMINO_MAIN_MARKET | KAMINO_FIGURE_MARKET | KAMINO_MAPLE_MARKET
            | KAMINO_ONRE_MARKET | KAMINO_ETHENA_MARKET => {}
            _ => return false,
        }
    }
    true
}

fn setup(
    plan: YieldRouteActionPlan,
    accounts: YieldRouteActionAccounts,
    instructions: Vec<Instruction>,
    steps: YieldRouteSteps,
    constraint_count: usize,
) -> Result<YieldRouteActionSetup> {
    setup_from_parts(
        plan.context,
        plan.topology,
        plan.universe,
        plan.swap_lanes,
        accounts,
        instructions,
        steps,
        constraint_count,
    )
}

#[allow(clippy::too_many_arguments)]
fn setup_from_parts(
    context: LoyalActionContext,
    topology: RouteTopology,
    universe: YieldRouteUniverse,
    swap_lanes: Vec<SwapLane>,
    accounts: YieldRouteActionAccounts,
    instructions: Vec<Instruction>,
    steps: YieldRouteSteps,
    constraint_count: usize,
) -> Result<YieldRouteActionSetup> {
    let lookup_table_requirements = action_lookup_table_requirements(context, accounts);
    Ok(YieldRouteActionSetup {
        accounts,
        spec: YieldRouteActionSpec {
            topology,
            universe,
            swap_lanes,
            accounts,
            instruction_count: instructions.len(),
            constraint_count,
        },
        instructions,
        steps,
        lookup_table_requirements,
    })
}

fn action_lookup_table_requirements(
    context: LoyalActionContext,
    accounts: YieldRouteActionAccounts,
) -> YieldRouteLookupTableRequirements {
    let mut requirements = YieldRouteLookupTableRequirements::new(context.settings, context.vault);
    for account in [accounts.withdraw, accounts.swap, accounts.deposit] {
        requirements.add_action_account(account);
        requirements.add_policy(account);
    }
    requirements.add_infrastructure(solana_sdk::system_program::ID);
    requirements
}

fn coalesce_steps<const N: usize>(steps: [LoyalActionStep; N]) -> Result<LoyalActionRoute<N>> {
    let Some(first) = steps.first().copied() else {
        return Err(LoyalActionError::MissingActionStep);
    };
    if steps
        .iter()
        .any(|step| step.action_account() != first.action_account())
    {
        return Err(LoyalActionError::SplitActionRoute);
    }

    Ok(LoyalActionRoute {
        action_account: first.action_account(),
        instruction_constraint_indexes: steps.map(|step| step.instruction_constraint_index()),
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

fn stable_swap_constraints(
    vault: Pubkey,
    allowed_mints: Vec<Pubkey>,
    swap_lanes: &[SwapLane],
) -> Result<Vec<SquadsInstructionConstraint>> {
    validate_stable_mints(&allowed_mints)?;
    validate_swap_lane_values(swap_lanes)?;

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
    if plan.topology == RouteTopology::AllInOne {
        validate_swap_lane_values(&plan.swap_lanes)?;
    } else {
        validate_swap_lanes(&plan.swap_lanes)?;
    }
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
    validate_swap_lane_values(swap_lanes)
}

fn validate_swap_lane_values(swap_lanes: &[SwapLane]) -> Result<()> {
    for lane in swap_lanes {
        if let SwapLane::LoyalHub { max_fee_bps, .. } = lane {
            if *max_fee_bps > loyal_hub_abi::MAX_FEE_BPS as u16 {
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

#[allow(clippy::needless_borrow)]
#[cfg(test)]
mod tests {
    use super::*;
    use crate::protocols::derive_loyal_hub_config;
    use solana_sdk::{
        hash::Hash,
        message::Message,
        packet::PACKET_DATA_SIZE,
        signature::{Keypair, Signer},
        transaction::Transaction,
    };
    use std::collections::BTreeMap;

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

    fn jupiter_lane(_include_intermediate_token_accounts: bool) -> SwapLane {
        SwapLane::Jupiter(JupiterSwapContract {
            program_id: JUPITER_V6_PROGRAM_ID,
            exact_in_discriminator: JUPITER_SWAP_DISCRIMINATOR,
            max_slippage_bps: JUPITER_DEFAULT_MAX_SLIPPAGE_BPS,
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
    fn action_setup_emits_typed_lookup_requirements_with_its_instructions() {
        let context = context();
        let setup =
            create_all_in_one_market_mint_yield_route_action(context, universe(), Vec::new())
                .expect("build action setup");
        let requirements = setup.lookup_table_requirements();

        assert!(requirements
            .provenance()
            .vault_roles(&context.settings)
            .is_some_and(|roles| roles.contains(&crate::VaultRole::Settings)));
        assert!(requirements
            .provenance()
            .vault_roles(&context.vault)
            .is_some_and(|roles| roles.contains(&crate::VaultRole::Vault)));
        assert!(requirements
            .provenance()
            .vault_roles(&setup.accounts.withdraw)
            .is_some_and(|roles| {
                roles.contains(&crate::VaultRole::ActionAccount)
                    && roles.contains(&crate::VaultRole::Policy)
            }));

        let manifest = requirements
            .manifest(context.authority, &setup.instructions)
            .expect("action-owned requirements should exactly classify setup instructions");
        assert!(manifest
            .vault()
            .iter()
            .any(|requirement| requirement.address == context.settings));
        assert!(manifest
            .vault()
            .iter()
            .any(|requirement| requirement.address == setup.accounts.withdraw));
    }

    #[test]
    fn rejects_empty_swap_lanes() {
        let result = create_swap_yield_route_action(context(), vec![USDC_MINT], vec![], 42);
        assert_eq!(result.unwrap_err(), LoyalActionError::EmptySwapLanes);
    }

    #[test]
    fn measures_generalized_cross_mint_policy_packet_shapes() {
        let authority = Keypair::new();
        let context = LoyalActionContext {
            settings: Pubkey::new_unique(),
            authority: authority.pubkey(),
            delegated_signer: Pubkey::new_unique(),
            account_index: 0,
            vault: Pubkey::new_unique(),
        };
        let all_spending_limits = crate::earn_stablecoins()
            .iter()
            .map(|stablecoin| (stablecoin.mint, 1_000_000_000))
            .collect::<Vec<_>>();
        let constraint = |dialect| {
            crate::protocols::jupiter_cross_mint_constraint(
                context.vault,
                dialect,
                JUPITER_DEFAULT_MAX_SLIPPAGE_BPS,
            )
        };
        let instruction = |seed, constraints, spending_limits: &[(Pubkey, u64)]| {
            crate::squads::create_program_interaction_action_instruction_with_daily_spending_limits(
                context.settings,
                context.authority,
                context.delegated_signer,
                seed,
                context.account_index,
                constraints,
                spending_limits,
            )
            .unwrap()
        };
        let both_dialects_all_mints = instruction(
            42,
            vec![
                constraint(JupiterV2Dialect::RouteV2),
                constraint(JupiterV2Dialect::SharedAccountsRouteV2),
            ],
            &all_spending_limits,
        );
        let classic_shard = create_jupiter_cross_mint_policy_action(
            context,
            JupiterCrossMintPolicySpec {
                source_shard: JupiterCrossMintSourceShard::Classic,
                max_slippage_bps: JUPITER_DEFAULT_MAX_SLIPPAGE_BPS,
                daily_source_mint_spending_cap: 1_000_000_000,
            },
            43,
        )
        .unwrap();

        let packet_bytes = |instruction: Instruction| {
            let message = Message::new_with_blockhash(
                &[instruction],
                Some(&authority.pubkey()),
                &Hash::new_unique(),
            );
            bincode::serialize(&Transaction::new(
                &[&authority],
                message,
                Hash::new_unique(),
            ))
            .unwrap()
            .len()
        };
        let all_mints_data_bytes = both_dialects_all_mints.data.len();
        let all_mints_packet_bytes = packet_bytes(both_dialects_all_mints);
        let classic_shard_data_bytes = classic_shard.instruction.data.len();
        let classic_shard_packet_bytes = packet_bytes(classic_shard.instruction);

        eprintln!(
            "generalized_cross_mint_policy_measurement packet_limit={PACKET_DATA_SIZE} all_mints_data_bytes={all_mints_data_bytes} all_mints_packet_bytes={all_mints_packet_bytes} classic_shard_data_bytes={classic_shard_data_bytes} classic_shard_packet_bytes={classic_shard_packet_bytes}"
        );
        assert_eq!(all_mints_packet_bytes, 1_298);
        assert_eq!(classic_shard_packet_bytes, 1_148);
    }

    #[test]
    fn generalized_cross_mint_policy_set_covers_all_pairs_with_two_immutable_shards() {
        let context = context();
        let set = create_jupiter_cross_mint_policy_set(
            context,
            JUPITER_DEFAULT_MAX_SLIPPAGE_BPS,
            1_000_000_000,
            JupiterCrossMintPolicySeeds {
                classic: 41,
                token_2022: 42,
            },
        )
        .unwrap();
        let canonical_atas = crate::earn_stablecoins()
            .iter()
            .map(|stablecoin| {
                crate::derive_associated_token_account(
                    context.vault,
                    stablecoin.mint,
                    stablecoin.token_program,
                )
            })
            .collect::<Vec<_>>();

        for action in [&set.classic, &set.token_2022] {
            let decoded = decode_program_interaction_policy_create(&action.instruction);
            assert_eq!(decoded.account_index, context.account_index);
            assert_eq!(decoded.constraints.len(), 2);
            for (constraint_index, dialect) in [
                JupiterV2Dialect::RouteV2,
                JupiterV2Dialect::SharedAccountsRouteV2,
            ]
            .into_iter()
            .enumerate()
            {
                let constraint = &decoded.constraints[constraint_index];
                assert_eq!(constraint.program_id, JUPITER_V6_PROGRAM_ID);
                let accounts = constraint.account_constraints_by_index();
                let layout = dialect.account_layout();
                assert_eq!(accounts.len(), 2);
                assert_pubkey_constraint(&accounts[&layout.authority], &[context.vault], None);
                assert_pubkey_constraint(
                    &accounts[&layout.output_token_account],
                    &canonical_atas,
                    None,
                );
                assert_data_slice_equals(
                    &constraint.data_constraints[0],
                    0,
                    &dialect.discriminator(),
                );
                assert_data_u16_lte(
                    &constraint.data_constraints[1],
                    dialect.slippage_bps_offset(),
                    JUPITER_DEFAULT_MAX_SLIPPAGE_BPS,
                );
                assert_data_u8_equals(
                    &constraint.data_constraints[2],
                    dialect.platform_fee_bps_offset(),
                    0,
                );
            }
        }

        for pair in crate::earn_stablecoin_pairs() {
            let action = set.action_for_source_mint(pair.input_mint).unwrap();
            assert_eq!(
                action
                    .step_for_pair(pair, JupiterV2Dialect::RouteV2)
                    .unwrap()
                    .instruction_constraint_index(),
                0
            );
            assert_eq!(
                action
                    .step_for_pair(pair, JupiterV2Dialect::SharedAccountsRouteV2)
                    .unwrap()
                    .instruction_constraint_index(),
                1
            );
            let other = if action.spec.source_shard == JupiterCrossMintSourceShard::Classic {
                &set.token_2022
            } else {
                &set.classic
            };
            assert_eq!(
                other
                    .step_for_pair(pair, JupiterV2Dialect::RouteV2)
                    .unwrap_err(),
                LoyalActionError::MissingActionStep
            );
        }
    }

    #[test]
    fn generalized_cross_mint_policy_rejects_unbounded_risk_and_duplicate_seeds() {
        let context = context();
        let spec = JupiterCrossMintPolicySpec {
            source_shard: JupiterCrossMintSourceShard::Classic,
            max_slippage_bps: JUPITER_DEFAULT_MAX_SLIPPAGE_BPS,
            daily_source_mint_spending_cap: 1,
        };
        assert_eq!(
            create_jupiter_cross_mint_policy_action(
                context,
                JupiterCrossMintPolicySpec {
                    max_slippage_bps: 10_001,
                    ..spec
                },
                41,
            )
            .unwrap_err(),
            LoyalActionError::InvalidSlippageBps
        );
        assert_eq!(
            create_jupiter_cross_mint_policy_action(
                context,
                JupiterCrossMintPolicySpec {
                    max_slippage_bps: 0,
                    ..spec
                },
                41,
            )
            .unwrap_err(),
            LoyalActionError::InvalidSlippageBps
        );
        assert_eq!(
            create_jupiter_cross_mint_policy_action(
                context,
                JupiterCrossMintPolicySpec {
                    daily_source_mint_spending_cap: 0,
                    ..spec
                },
                41,
            )
            .unwrap_err(),
            LoyalActionError::InvalidDailySpendingCap
        );
        assert_eq!(
            create_jupiter_cross_mint_policy_set(
                context,
                JUPITER_DEFAULT_MAX_SLIPPAGE_BPS,
                1,
                JupiterCrossMintPolicySeeds {
                    classic: 41,
                    token_2022: 41,
                },
            )
            .unwrap_err(),
            LoyalActionError::DuplicateActionSeeds
        );
    }

    #[test]
    fn all_in_one_allows_same_mint_only_policy() {
        let context = context();
        let setup =
            create_all_in_one_market_mint_yield_route_action(context, universe(), vec![]).unwrap();

        assert_eq!(setup.instructions.len(), 1);
        assert_eq!(setup.spec.constraint_count, 3);
        assert_eq!(
            setup
                .same_mint_route()
                .unwrap()
                .instruction_constraint_indexes(),
            &[0, 1]
        );
        assert_eq!(
            setup.jupiter_route().unwrap_err(),
            LoyalActionError::MissingActionStep
        );
    }

    #[test]
    fn all_in_one_same_mint_usdc_policy_decodes_market_and_mint_scope() {
        let context = context();
        let setup = create_all_in_one_market_mint_yield_route_action(
            context,
            YieldRouteUniverse::new(
                vec![USDC_MINT],
                vec![
                    KAMINO_MAIN_MARKET,
                    KAMINO_FIGURE_MARKET,
                    KAMINO_MAPLE_MARKET,
                    KAMINO_ONRE_MARKET,
                    KAMINO_ETHENA_MARKET,
                ],
                vec![USDC_MINT],
            ),
            vec![],
        )
        .unwrap();

        assert_eq!(setup.instructions.len(), 1);
        assert_eq!(setup.spec.constraint_count, 3);
        assert_eq!(
            setup
                .same_mint_route()
                .unwrap()
                .instruction_constraint_indexes(),
            &[0, 1]
        );

        let decoded = decode_program_interaction_policy_create(&setup.instructions[0]);
        assert_eq!(decoded.constraints.len(), 3);
        let markets = [
            KAMINO_MAIN_MARKET,
            KAMINO_FIGURE_MARKET,
            KAMINO_MAPLE_MARKET,
            KAMINO_ONRE_MARKET,
            KAMINO_ETHENA_MARKET,
        ];
        let mints = [USDC_MINT];

        let withdraw_accounts = decoded.constraints[0].account_constraints_by_index();
        assert_pubkey_constraint(&withdraw_accounts[&0], &[context.vault], None);
        assert_pubkey_constraint(&withdraw_accounts[&2], &markets, None);
        assert_pubkey_constraint(&withdraw_accounts[&5], &mints, None);
        assert_data_slice_equals(
            &decoded.constraints[0].data_constraints[0],
            0,
            &KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
        );

        let deposit_accounts = decoded.constraints[1].account_constraints_by_index();
        assert_pubkey_constraint(&deposit_accounts[&0], &[context.vault], None);
        assert_pubkey_constraint(&deposit_accounts[&2], &markets, None);
        assert_pubkey_constraint(&deposit_accounts[&5], &mints, None);
        assert_data_slice_equals(
            &decoded.constraints[1].data_constraints[0],
            0,
            &KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
        );

        assert_kamino_init_obligation_constraint(&decoded.constraints[2], context.vault);
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
        assert_eq!(setup.spec.constraint_count, 4);

        let same_mint_route = setup.same_mint_route().unwrap();
        assert_eq!(same_mint_route.action_account(), setup.accounts.withdraw);
        assert_eq!(same_mint_route.instruction_constraint_indexes(), &[0, 2]);

        let jupiter_route = setup.jupiter_route().unwrap();
        assert_eq!(jupiter_route.action_account(), setup.accounts.withdraw);
        assert_eq!(jupiter_route.instruction_constraint_indexes(), &[0, 1, 2]);
    }

    #[test]
    fn kamino_stable_presets_are_cumulative() {
        let safe = yield_route_universe_for_preset(YieldRouteUniversePreset::KaminoStable(
            KaminoStableRiskProfile::Safe,
        ));
        let medium = yield_route_universe_for_preset(YieldRouteUniversePreset::KaminoStable(
            KaminoStableRiskProfile::Medium,
        ));
        let aggressive = yield_route_universe_for_preset(YieldRouteUniversePreset::KaminoStable(
            KaminoStableRiskProfile::Aggressive,
        ));

        assert!(safe
            .kamino_markets
            .iter()
            .all(|market| medium.kamino_markets.contains(market)));
        assert!(medium
            .kamino_markets
            .iter()
            .all(|market| aggressive.kamino_markets.contains(market)));
        assert!(safe
            .stable_mints
            .iter()
            .all(|mint| medium.stable_mints.contains(mint)));
        assert_eq!(medium.stable_mints, aggressive.stable_mints);
        assert_eq!(safe.kamino_liquidity_mints, safe.stable_mints);
    }

    #[test]
    fn kamino_stable_presets_include_expected_markets() {
        let safe = yield_route_universe_for_preset(YieldRouteUniversePreset::KaminoStable(
            KaminoStableRiskProfile::Safe,
        ));
        let medium = yield_route_universe_for_preset(YieldRouteUniversePreset::KaminoStable(
            KaminoStableRiskProfile::Medium,
        ));
        let aggressive = yield_route_universe_for_preset(YieldRouteUniversePreset::KaminoStable(
            KaminoStableRiskProfile::Aggressive,
        ));

        for market in [
            KAMINO_MAIN_MARKET,
            KAMINO_FIGURE_MARKET,
            KAMINO_MAPLE_MARKET,
            KAMINO_ONRE_MARKET,
            KAMINO_ETHENA_MARKET,
        ] {
            assert!(safe.kamino_markets.contains(&market));
        }
        for market in [
            KAMINO_JLP_MARKET,
            KAMINO_BITCOIN_MARKET,
            KAMINO_SUPERSTATE_OPENING_BELL_MARKET,
        ] {
            assert!(!safe.kamino_markets.contains(&market));
            assert!(medium.kamino_markets.contains(&market));
        }
        for market in [
            KAMINO_HUMA_MARKET,
            KAMINO_SOLSTICE_MARKET,
            KAMINO_XSTOCKS_MARKET,
            KAMINO_ALTCOINS_MARKET,
        ] {
            assert!(!safe.kamino_markets.contains(&market));
            assert!(!medium.kamino_markets.contains(&market));
            assert!(aggressive.kamino_markets.contains(&market));
        }
    }

    #[test]
    fn supported_yield_route_stable_mints_include_allowlist_and_exclude_explicit_denials() {
        let stable_mints = supported_yield_route_stable_mints();
        let universe = yield_route_universe_for_preset(YieldRouteUniversePreset::KaminoStable(
            KaminoStableRiskProfile::Aggressive,
        ));

        for mint in [
            USDC_MINT,
            USDT_MINT,
            PYUSD_MINT,
            USDS_MINT,
            USDG_MINT,
            USDE_MINT,
            SUSDE_MINT,
            CASH_MINT,
            SYRUP_USDC_MINT,
            USD1_MINT,
            FDUSD_MINT,
            AUSD_MINT,
            EUSX_MINT,
            USCC_MINT,
        ] {
            assert!(stable_mints.contains(&mint));
            assert!(universe.stable_mints.contains(&mint));
            assert!(universe.kamino_liquidity_mints.contains(&mint));
        }
        assert!(!stable_mints.contains(&USDH_MINT));
        assert!(!stable_mints.contains(&Pubkey::default()));
        assert!(!universe.stable_mints.contains(&USDH_MINT));
        assert!(!universe.stable_mints.contains(&Pubkey::new_unique()));
    }

    #[test]
    fn preset_all_in_one_builder_uses_preset_universe() {
        let context = context();
        let preset = YieldRouteUniversePreset::KaminoStable(KaminoStableRiskProfile::Medium);
        let setup =
            create_preset_all_in_one_yield_route_action(context, preset, vec![jupiter_lane(false)])
                .unwrap();

        assert_eq!(setup.instructions.len(), 1);
        assert_eq!(setup.spec.topology, RouteTopology::AllInOne);
        assert_eq!(setup.spec.constraint_count, 4);
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
        assert_eq!(setup.spec.universe, yield_route_universe_for_preset(preset));
    }

    #[test]
    fn preset_detection_requires_exact_market_set() {
        let safe_preset = YieldRouteUniversePreset::KaminoStable(KaminoStableRiskProfile::Safe);
        let medium_preset = YieldRouteUniversePreset::KaminoStable(KaminoStableRiskProfile::Medium);
        let aggressive_preset =
            YieldRouteUniversePreset::KaminoStable(KaminoStableRiskProfile::Aggressive);
        let safe = yield_route_universe_for_preset(safe_preset);
        let medium = yield_route_universe_for_preset(medium_preset);
        let aggressive = yield_route_universe_for_preset(aggressive_preset);

        assert_eq!(
            detect_yield_route_universe_preset(&safe.kamino_markets),
            Some(safe_preset)
        );
        assert_eq!(
            detect_yield_route_universe_preset(&medium.kamino_markets),
            Some(medium_preset)
        );
        assert_eq!(
            detect_yield_route_universe_preset(&aggressive.kamino_markets),
            Some(aggressive_preset)
        );

        let mut custom = safe.kamino_markets.clone();
        custom.push(Pubkey::new_unique());
        assert_eq!(detect_yield_route_universe_preset(&custom), None);
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

    #[test]
    fn route_grouping_rejects_split_action_accounts() {
        let setup = create_three_step_yield_route_actions(
            context(),
            universe(),
            vec![jupiter_lane(false)],
            YieldRouteActionSeeds::default(),
        )
        .unwrap();

        assert_eq!(
            setup.same_mint_route().unwrap_err(),
            LoyalActionError::SplitActionRoute
        );
        assert_eq!(
            setup.jupiter_route().unwrap_err(),
            LoyalActionError::SplitActionRoute
        );
    }

    #[test]
    fn all_in_one_compiled_policy_snapshot_covers_same_mint_jupiter_and_hub_routes() {
        let context = context();
        let hub_authorizer = Pubkey::new_unique();
        let setup = create_all_in_one_market_mint_yield_route_action(
            context,
            universe(),
            vec![
                jupiter_lane(false),
                SwapLane::LoyalHub {
                    hub_authorizer,
                    max_fee_bps: 50,
                },
            ],
        )
        .unwrap();

        assert_eq!(
            setup
                .same_mint_route()
                .unwrap()
                .instruction_constraint_indexes(),
            &[0, 3]
        );
        assert_eq!(
            setup
                .jupiter_route()
                .unwrap()
                .instruction_constraint_indexes(),
            &[0, 1, 3]
        );
        assert_eq!(
            setup
                .loyal_hub_route()
                .unwrap()
                .instruction_constraint_indexes(),
            &[0, 2, 3]
        );

        let decoded = decode_program_interaction_policy_create(&setup.instructions[0]);
        assert_eq!(decoded.account_index, context.account_index);
        assert_eq!(decoded.constraints.len(), 5);
        assert_eq!(decoded.constraints[0].program_id, KAMINO_LEND_PROGRAM_ID);
        assert_eq!(decoded.constraints[1].program_id, JUPITER_V6_PROGRAM_ID);
        assert_eq!(decoded.constraints[2].program_id, LOYAL_HUB_SWAP_PROGRAM_ID);
        assert_eq!(decoded.constraints[3].program_id, KAMINO_LEND_PROGRAM_ID);
        assert_eq!(decoded.constraints[4].program_id, KAMINO_LEND_PROGRAM_ID);

        assert_kamino_withdraw_constraint(&decoded.constraints[0], context.vault);
        assert_jupiter_constraint(&decoded.constraints[1], context.vault);
        assert_hub_constraint(&decoded.constraints[2], context.vault, hub_authorizer, 50);
        assert_kamino_deposit_constraint(&decoded.constraints[3], context.vault);
        assert_kamino_refresh_obligation_constraint(&decoded.constraints[4], context.vault);
    }

    fn assert_kamino_withdraw_constraint(constraint: &DecodedInstructionConstraint, vault: Pubkey) {
        let accounts = constraint.account_constraints_by_index();
        assert_pubkey_constraint(&accounts[&0], &[vault], None);
        assert_eq!(accounts[&2].kind, DecodedAccountConstraintKind::Pubkey);
        assert_eq!(accounts[&2].pubkeys.len(), 1);
        assert_eq!(accounts[&5].owner, Some(spl_token::id()));
        assert_token_authority_constraint(&accounts[&9], vault);
        assert_pubkey_constraint(&accounts[&11], &[spl_token::id()], None);
        assert_pubkey_constraint(&accounts[&12], &[spl_token::id()], None);
        assert_data_slice_equals(
            &constraint.data_constraints[0],
            0,
            &KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
        );
    }

    fn assert_kamino_deposit_constraint(constraint: &DecodedInstructionConstraint, vault: Pubkey) {
        let accounts = constraint.account_constraints_by_index();
        assert_pubkey_constraint(&accounts[&0], &[vault], None);
        assert_eq!(accounts[&2].kind, DecodedAccountConstraintKind::Pubkey);
        assert_eq!(accounts[&2].pubkeys.len(), 1);
        assert_eq!(accounts[&5].owner, Some(spl_token::id()));
        assert_token_authority_constraint(&accounts[&9], vault);
        assert_pubkey_constraint(&accounts[&11], &[spl_token::id()], None);
        assert_pubkey_constraint(&accounts[&12], &[spl_token::id()], None);
        assert_data_slice_equals(
            &constraint.data_constraints[0],
            0,
            &KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
        );
    }

    fn assert_kamino_refresh_obligation_constraint(
        constraint: &DecodedInstructionConstraint,
        vault: Pubkey,
    ) {
        let accounts = constraint.account_constraints_by_index();
        let markets = accounts[&0].pubkeys.clone();
        let obligations = markets
            .iter()
            .map(|market| crate::protocols::derive_kamino_vanilla_obligation(vault, *market))
            .collect::<Vec<_>>();
        assert_pubkey_constraint(&accounts[&0], &markets, None);
        assert_pubkey_constraint(&accounts[&1], &obligations, None);
        assert_data_slice_equals(
            &constraint.data_constraints[0],
            0,
            &KAMINO_REFRESH_OBLIGATION_DISCRIMINATOR,
        );
    }

    fn assert_kamino_init_obligation_constraint(
        constraint: &DecodedInstructionConstraint,
        vault: Pubkey,
    ) {
        let accounts = constraint.account_constraints_by_index();
        let markets = accounts[&3].pubkeys.clone();
        let mut expected_data_prefix = KAMINO_INIT_OBLIGATION_DISCRIMINATOR.to_vec();
        expected_data_prefix.push(KAMINO_VANILLA_OBLIGATION_TAG);
        expected_data_prefix.push(KAMINO_VANILLA_OBLIGATION_ID);

        assert_pubkey_constraint(&accounts[&0], &[vault], None);
        assert_pubkey_constraint(&accounts[&1], &[vault], None);
        assert!(
            !accounts.contains_key(&2),
            "obligation PDA stays unconstrained so setup policy fits packet limits"
        );
        assert_pubkey_constraint(&accounts[&3], &markets, None);
        assert_pubkey_constraint(&accounts[&4], &[Pubkey::default()], None);
        assert_pubkey_constraint(&accounts[&5], &[Pubkey::default()], None);
        assert_pubkey_constraint(
            &accounts[&6],
            &[crate::protocols::derive_kamino_user_metadata(vault)],
            None,
        );
        assert_pubkey_constraint(&accounts[&7], &[solana_sdk::sysvar::rent::id()], None);
        assert_pubkey_constraint(&accounts[&8], &[solana_sdk::system_program::ID], None);
        assert_data_slice_equals(&constraint.data_constraints[0], 0, &expected_data_prefix);
    }

    fn assert_jupiter_constraint(constraint: &DecodedInstructionConstraint, vault: Pubkey) {
        let accounts = constraint.account_constraints_by_index();
        assert_pubkey_constraint(&accounts[&0], &[vault], None);
        assert_token_authority_constraint(&accounts[&1], vault);
        assert_token_authority_constraint(&accounts[&2], vault);
        assert_eq!(accounts[&3].owner, Some(spl_token::id()));
        assert_eq!(accounts[&3].pubkeys.len(), 1);
        assert_eq!(accounts[&4].owner, Some(spl_token::id()));
        assert_eq!(accounts[&4].pubkeys.len(), 1);
        assert_pubkey_constraint(&accounts[&5], &[spl_token::id()], None);
        assert_eq!(accounts.len(), 6);
        assert_data_slice_equals(
            &constraint.data_constraints[0],
            0,
            &JUPITER_SWAP_DISCRIMINATOR,
        );
        assert_data_u16_lte(
            &constraint.data_constraints[1],
            JUPITER_SWAP_SLIPPAGE_BPS_OFFSET,
            JUPITER_DEFAULT_MAX_SLIPPAGE_BPS,
        );
    }

    fn assert_hub_constraint(
        constraint: &DecodedInstructionConstraint,
        vault: Pubkey,
        hub_authorizer: Pubkey,
        max_fee_bps: u16,
    ) {
        let accounts = constraint.account_constraints_by_index();
        assert_pubkey_constraint(
            &accounts[&loyal_hub_abi::swap_exact_in_accounts::CONFIG],
            &[derive_loyal_hub_config()],
            Some(LOYAL_HUB_SWAP_PROGRAM_ID),
        );
        assert_pubkey_constraint(
            &accounts[&loyal_hub_abi::swap_exact_in_accounts::USER_VAULT],
            &[vault],
            None,
        );
        assert_eq!(
            accounts[&loyal_hub_abi::swap_exact_in_accounts::HUB_INPUT].owner,
            None
        );
        assert_eq!(
            accounts[&loyal_hub_abi::swap_exact_in_accounts::HUB_OUTPUT].owner,
            None
        );
        assert_token_authority_constraint_with_owner(
            &accounts[&loyal_hub_abi::swap_exact_in_accounts::USER_INPUT],
            vault,
            None,
        );
        assert_token_authority_constraint_with_owner(
            &accounts[&loyal_hub_abi::swap_exact_in_accounts::USER_OUTPUT],
            vault,
            None,
        );
        assert_eq!(
            accounts[&loyal_hub_abi::swap_exact_in_accounts::INPUT_MINT].owner,
            None
        );
        assert_eq!(
            accounts[&loyal_hub_abi::swap_exact_in_accounts::OUTPUT_MINT].owner,
            None
        );
        assert_pubkey_constraint(
            &accounts[&loyal_hub_abi::swap_exact_in_accounts::HUB_AUTHORIZER],
            &[hub_authorizer],
            None,
        );
        assert_pubkey_constraint(
            &accounts[&loyal_hub_abi::swap_exact_in_accounts::TOKEN_PROGRAM],
            &[spl_token::id()],
            None,
        );
        assert_pubkey_constraint(
            &accounts[&loyal_hub_abi::swap_exact_in_accounts::TOKEN_2022_PROGRAM],
            &[TOKEN_2022_PROGRAM_ID],
            None,
        );
        assert_data_u8_equals(
            &constraint.data_constraints[0],
            LOYAL_HUB_SWAP_TAG_OFFSET,
            LOYAL_HUB_SWAP_EXACT_IN,
        );
        assert_data_u16_lte(
            &constraint.data_constraints[1],
            LOYAL_HUB_SWAP_MAX_FEE_BPS_OFFSET,
            max_fee_bps,
        );
    }

    fn assert_pubkey_constraint(
        constraint: &DecodedAccountConstraint,
        expected_pubkeys: &[Pubkey],
        owner: Option<Pubkey>,
    ) {
        assert_eq!(constraint.kind, DecodedAccountConstraintKind::Pubkey);
        assert_eq!(constraint.pubkeys, expected_pubkeys);
        assert_eq!(constraint.owner, owner);
    }

    fn assert_token_authority_constraint(constraint: &DecodedAccountConstraint, authority: Pubkey) {
        assert_token_authority_constraint_with_owner(constraint, authority, Some(spl_token::id()));
    }

    fn assert_token_authority_constraint_with_owner(
        constraint: &DecodedAccountConstraint,
        authority: Pubkey,
        owner: Option<Pubkey>,
    ) {
        assert_eq!(constraint.kind, DecodedAccountConstraintKind::AccountData);
        assert_eq!(constraint.owner, owner);
        assert_data_slice_equals(&constraint.data_constraints[0], 32, authority.as_ref());
    }

    fn assert_data_u8_equals(constraint: &DecodedDataConstraint, offset: u64, expected: u8) {
        assert_eq!(constraint.offset, offset);
        assert_eq!(constraint.value, DecodedDataValue::U8(expected));
        assert_eq!(constraint.operator, DecodedDataOperator::Equals);
    }

    fn assert_data_u16_lte(constraint: &DecodedDataConstraint, offset: u64, expected: u16) {
        assert_eq!(constraint.offset, offset);
        assert_eq!(constraint.value, DecodedDataValue::U16Le(expected));
        assert_eq!(constraint.operator, DecodedDataOperator::LessThanOrEqualTo);
    }

    fn assert_data_slice_equals(constraint: &DecodedDataConstraint, offset: u64, expected: &[u8]) {
        assert_eq!(constraint.offset, offset);
        assert_eq!(
            constraint.value,
            DecodedDataValue::U8Slice(expected.to_vec())
        );
        assert_eq!(constraint.operator, DecodedDataOperator::Equals);
    }

    #[derive(Debug)]
    struct DecodedProgramInteractionPolicy {
        account_index: u8,
        constraints: Vec<DecodedInstructionConstraint>,
    }

    #[derive(Debug)]
    struct DecodedInstructionConstraint {
        program_id: Pubkey,
        account_constraints: Vec<DecodedAccountConstraint>,
        data_constraints: Vec<DecodedDataConstraint>,
    }

    impl DecodedInstructionConstraint {
        fn account_constraints_by_index(&self) -> BTreeMap<u8, &DecodedAccountConstraint> {
            self.account_constraints
                .iter()
                .map(|constraint| (constraint.account_index, constraint))
                .collect()
        }
    }

    #[derive(Debug)]
    struct DecodedAccountConstraint {
        account_index: u8,
        kind: DecodedAccountConstraintKind,
        pubkeys: Vec<Pubkey>,
        data_constraints: Vec<DecodedDataConstraint>,
        owner: Option<Pubkey>,
    }

    #[derive(Clone, Copy, Debug, PartialEq, Eq)]
    enum DecodedAccountConstraintKind {
        Pubkey,
        AccountData,
    }

    #[derive(Clone, Debug, PartialEq, Eq)]
    struct DecodedDataConstraint {
        offset: u64,
        value: DecodedDataValue,
        operator: DecodedDataOperator,
    }

    #[derive(Clone, Debug, PartialEq, Eq)]
    enum DecodedDataValue {
        U8(u8),
        U16Le(u16),
        U8Slice(Vec<u8>),
        Other(u8),
    }

    #[derive(Clone, Copy, Debug, PartialEq, Eq)]
    enum DecodedDataOperator {
        Equals,
        LessThanOrEqualTo,
        Other(u8),
    }

    struct Cursor<'a> {
        data: &'a [u8],
        offset: usize,
    }

    impl<'a> Cursor<'a> {
        fn new(data: &'a [u8]) -> Self {
            Self { data, offset: 0 }
        }

        fn read_u8(&mut self) -> u8 {
            let value = self.data[self.offset];
            self.offset += 1;
            value
        }

        fn read_u16(&mut self) -> u16 {
            let bytes: [u8; 2] = self.data[self.offset..self.offset + 2].try_into().unwrap();
            self.offset += 2;
            u16::from_le_bytes(bytes)
        }

        fn read_u32(&mut self) -> u32 {
            let bytes: [u8; 4] = self.data[self.offset..self.offset + 4].try_into().unwrap();
            self.offset += 4;
            u32::from_le_bytes(bytes)
        }

        fn read_u64(&mut self) -> u64 {
            let bytes: [u8; 8] = self.data[self.offset..self.offset + 8].try_into().unwrap();
            self.offset += 8;
            u64::from_le_bytes(bytes)
        }

        fn read_pubkey(&mut self) -> Pubkey {
            let bytes: [u8; 32] = self.data[self.offset..self.offset + 32].try_into().unwrap();
            self.offset += 32;
            Pubkey::new_from_array(bytes)
        }

        fn read_vec_u8(&mut self) -> Vec<u8> {
            let len = self.read_u32() as usize;
            let bytes = self.data[self.offset..self.offset + len].to_vec();
            self.offset += len;
            bytes
        }

        fn skip(&mut self, len: usize) {
            self.offset += len;
        }
    }

    fn decode_program_interaction_policy_create(
        instruction: &solana_sdk::instruction::Instruction,
    ) -> DecodedProgramInteractionPolicy {
        let mut cursor = Cursor::new(&instruction.data);
        cursor.skip(8);
        assert_eq!(cursor.read_u8(), SQUADS_SYNC_SIGNER_COUNT);
        assert_eq!(cursor.read_u32(), 1);
        assert_eq!(cursor.read_u8(), 7);
        cursor.read_u64();
        assert_eq!(cursor.read_u8(), 3);

        let account_index = cursor.read_u8();
        let constraints = read_vec(&mut cursor, read_instruction_constraint);
        assert_eq!(cursor.read_u8(), 0);
        assert_eq!(cursor.read_u8(), 0);
        let spending_limit_count = cursor.read_u32();
        for _ in 0..spending_limit_count {
            cursor.skip(32 + 8);
            if cursor.read_u8() == 1 {
                cursor.skip(8);
            }
            if cursor.read_u8() == 4 {
                cursor.skip(8);
            }
            cursor.skip(8);
        }

        DecodedProgramInteractionPolicy {
            account_index,
            constraints,
        }
    }

    fn read_instruction_constraint(cursor: &mut Cursor<'_>) -> DecodedInstructionConstraint {
        DecodedInstructionConstraint {
            program_id: cursor.read_pubkey(),
            account_constraints: read_vec(cursor, read_account_constraint),
            data_constraints: read_vec(cursor, read_data_constraint),
        }
    }

    fn read_account_constraint(cursor: &mut Cursor<'_>) -> DecodedAccountConstraint {
        let account_index = cursor.read_u8();
        let kind_tag = cursor.read_u8();
        let (kind, pubkey_indexes, data_constraints) = match kind_tag {
            0 => (
                DecodedAccountConstraintKind::Pubkey,
                read_pubkey_vec(cursor),
                Vec::new(),
            ),
            1 => (
                DecodedAccountConstraintKind::AccountData,
                Vec::new(),
                read_vec(cursor, read_data_constraint),
            ),
            _ => panic!("unexpected account constraint kind {kind_tag}"),
        };
        let owner = match cursor.read_u8() {
            0 => None,
            1 => Some(cursor.read_pubkey()),
            tag => panic!("unexpected option tag {tag}"),
        };
        DecodedAccountConstraint {
            account_index,
            kind,
            pubkeys: pubkey_indexes,
            data_constraints,
            owner,
        }
    }

    fn read_data_constraint(cursor: &mut Cursor<'_>) -> DecodedDataConstraint {
        let offset = cursor.read_u64();
        let value_tag = cursor.read_u8();
        let value = match value_tag {
            0 => DecodedDataValue::U8(cursor.read_u8()),
            1 => DecodedDataValue::U16Le(cursor.read_u16()),
            2 => {
                cursor.skip(4);
                DecodedDataValue::Other(value_tag)
            }
            3 => {
                cursor.skip(8);
                DecodedDataValue::Other(value_tag)
            }
            4 => {
                cursor.skip(16);
                DecodedDataValue::Other(value_tag)
            }
            5 => DecodedDataValue::U8Slice(cursor.read_vec_u8()),
            _ => DecodedDataValue::Other(value_tag),
        };
        let operator = match cursor.read_u8() {
            0 => DecodedDataOperator::Equals,
            5 => DecodedDataOperator::LessThanOrEqualTo,
            tag => DecodedDataOperator::Other(tag),
        };
        DecodedDataConstraint {
            offset,
            value,
            operator,
        }
    }

    fn read_pubkey_vec(cursor: &mut Cursor<'_>) -> Vec<Pubkey> {
        let len = cursor.read_u32() as usize;
        (0..len).map(|_| cursor.read_pubkey()).collect()
    }

    fn read_vec<T>(cursor: &mut Cursor<'_>, read_item: fn(&mut Cursor<'_>) -> T) -> Vec<T> {
        let len = cursor.read_u32() as usize;
        (0..len).map(|_| read_item(cursor)).collect()
    }
}

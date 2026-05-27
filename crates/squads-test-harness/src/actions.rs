//! Test-harness adapters for the `loyal-actions` SDK.

use loyal_actions::{
    JupiterSwapContract, LoyalActionContext, LoyalActionError, LoyalActionStep, Result, SwapLane,
    YieldRouteActionInstruction, YieldRouteActionSetup, YieldRouteUniverse, JUPITER_V6_PROGRAM_ID,
};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};

use crate::{
    execute_loyal_action_hub_swap, execute_loyal_action_jupiter_swap, execute_loyal_action_step,
    FundedSquadsTestContext, MockKaminoReserveTokenAccounts, SquadsCompiledInstruction,
    MOCK_JUPITER_STABLE_EXACT_IN,
};

pub fn loyal_action_context(
    context: &FundedSquadsTestContext,
    delegated_signer: Pubkey,
) -> LoyalActionContext {
    LoyalActionContext {
        settings: context.pool.settings,
        authority: context.wallet_pubkey(),
        delegated_signer,
        account_index: context.vault_index,
        vault: context.vault,
    }
}

pub fn yield_route_universe_from_mock_reserves(
    stable_mints: Vec<Pubkey>,
    kamino_reserves: Vec<MockKaminoReserveTokenAccounts>,
) -> YieldRouteUniverse {
    YieldRouteUniverse::new(
        stable_mints,
        kamino_reserves
            .iter()
            .map(|reserve| reserve.market)
            .collect(),
        kamino_reserves
            .iter()
            .map(|reserve| reserve.liquidity_mint)
            .collect(),
    )
}

pub fn mock_jupiter_swap_contract(
    include_intermediate_token_accounts: bool,
) -> JupiterSwapContract {
    JupiterSwapContract {
        program_id: JUPITER_V6_PROGRAM_ID,
        exact_in_discriminator: MOCK_JUPITER_STABLE_EXACT_IN,
        include_intermediate_token_accounts,
    }
}

pub fn mock_jupiter_swap_lane(include_intermediate_token_accounts: bool) -> SwapLane {
    SwapLane::Jupiter(mock_jupiter_swap_contract(
        include_intermediate_token_accounts,
    ))
}

pub trait RouteActionExt {
    fn withdraw(&self) -> Result<KaminoAction>;
    fn deposit(&self) -> Result<KaminoAction>;
    fn jupiter(&self) -> Result<JupiterAction>;
    fn hub(&self) -> Result<HubAction>;
}

impl RouteActionExt for YieldRouteActionSetup {
    fn withdraw(&self) -> Result<KaminoAction> {
        self.withdraw_step().map(KaminoAction::new)
    }

    fn deposit(&self) -> Result<KaminoAction> {
        self.deposit_step().map(KaminoAction::new)
    }

    fn jupiter(&self) -> Result<JupiterAction> {
        self.jupiter_swap_step().map(JupiterAction::new)
    }

    fn hub(&self) -> Result<HubAction> {
        self.loyal_hub_swap_step().map(HubAction::new)
    }
}

impl RouteActionExt for YieldRouteActionInstruction {
    fn withdraw(&self) -> Result<KaminoAction> {
        Err(LoyalActionError::MissingActionStep)
    }

    fn deposit(&self) -> Result<KaminoAction> {
        Err(LoyalActionError::MissingActionStep)
    }

    fn jupiter(&self) -> Result<JupiterAction> {
        self.jupiter_swap_step().map(JupiterAction::new)
    }

    fn hub(&self) -> Result<HubAction> {
        self.loyal_hub_swap_step().map(HubAction::new)
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct KaminoAction {
    step: LoyalActionStep,
}

impl KaminoAction {
    fn new(step: LoyalActionStep) -> Self {
        Self { step }
    }

    pub fn build(
        self,
        signer: Pubkey,
        vault_index: u8,
        instructions: Vec<SquadsCompiledInstruction>,
        accounts: Vec<AccountMeta>,
    ) -> Instruction {
        execute_loyal_action_step(self.step, signer, vault_index, instructions, accounts)
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct JupiterAction {
    step: LoyalActionStep,
}

impl JupiterAction {
    fn new(step: LoyalActionStep) -> Self {
        Self { step }
    }

    pub fn build(self, args: JupiterSwapExecution) -> Instruction {
        execute_loyal_action_jupiter_swap(
            self.step,
            args.signer,
            args.vault_index,
            args.vault,
            args.vault_input,
            args.vault_output,
            args.input_mint,
            args.output_mint,
            args.in_amount,
            args.out_amount,
        )
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct JupiterSwapExecution {
    pub signer: Pubkey,
    pub vault_index: u8,
    pub vault: Pubkey,
    pub vault_input: Pubkey,
    pub vault_output: Pubkey,
    pub input_mint: Pubkey,
    pub output_mint: Pubkey,
    pub in_amount: u64,
    pub out_amount: u64,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct HubAction {
    step: LoyalActionStep,
}

impl HubAction {
    fn new(step: LoyalActionStep) -> Self {
        Self { step }
    }

    pub fn build(self, args: HubSwapExecution) -> Instruction {
        execute_loyal_action_hub_swap(
            self.step,
            args.signer,
            args.vault_index,
            args.vault,
            args.vault_input,
            args.vault_output,
            args.input_mint,
            args.output_mint,
            args.hub_authorizer,
            args.amount_in,
            args.amount_out,
            args.min_out,
            args.max_fee_bps,
            args.lane_id,
        )
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct HubSwapExecution {
    pub signer: Pubkey,
    pub vault_index: u8,
    pub vault: Pubkey,
    pub vault_input: Pubkey,
    pub vault_output: Pubkey,
    pub input_mint: Pubkey,
    pub output_mint: Pubkey,
    pub hub_authorizer: Pubkey,
    pub amount_in: u64,
    pub amount_out: u64,
    pub min_out: u64,
    pub max_fee_bps: u16,
    pub lane_id: u8,
}

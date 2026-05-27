//! Test-harness adapters for the `loyal-actions` SDK.

use loyal_actions::{
    CrossMintRoute, JupiterSwapContract, LoyalActionContext, LoyalActionError, LoyalActionStep,
    Result, SameMintRoute, SwapLane, YieldRouteActionInstruction, YieldRouteActionSetup,
    YieldRouteUniverse, JUPITER_V6_PROGRAM_ID,
};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};

use crate::{
    derive_mock_jupiter_swap_authority, execute_loyal_action_hub_swap,
    execute_loyal_action_jupiter_swap, execute_loyal_action_step,
    execute_squads_program_interaction_instruction, mock_jupiter_stable_exact_in_swap_data,
    mock_jupiter_stable_reserve_token_account, FundedSquadsTestContext,
    MockKaminoReserveTokenAccounts, SquadsCompiledInstruction, MOCK_JUPITER_STABLE_EXACT_IN,
};

use crate::execution::{compile_inner_instruction, merge_compiled_instructions};

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
    fn same_mint_route_action(&self) -> Result<SameMintRouteAction>;
    fn jupiter_route_action(&self) -> Result<JupiterRouteAction>;
    fn loyal_hub_route_action(&self) -> Result<HubRouteAction>;
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

    fn same_mint_route_action(&self) -> Result<SameMintRouteAction> {
        YieldRouteActionSetup::same_mint_route(self).map(SameMintRouteAction::new)
    }

    fn jupiter_route_action(&self) -> Result<JupiterRouteAction> {
        YieldRouteActionSetup::jupiter_route(self).map(JupiterRouteAction::new)
    }

    fn loyal_hub_route_action(&self) -> Result<HubRouteAction> {
        YieldRouteActionSetup::loyal_hub_route(self).map(HubRouteAction::new)
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

    fn same_mint_route_action(&self) -> Result<SameMintRouteAction> {
        Err(LoyalActionError::MissingActionStep)
    }

    fn jupiter_route_action(&self) -> Result<JupiterRouteAction> {
        Err(LoyalActionError::MissingActionStep)
    }

    fn loyal_hub_route_action(&self) -> Result<HubRouteAction> {
        Err(LoyalActionError::MissingActionStep)
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

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SameMintRouteAction {
    route: SameMintRoute,
}

impl SameMintRouteAction {
    fn new(route: SameMintRoute) -> Self {
        Self { route }
    }

    pub fn build(
        self,
        signer: Pubkey,
        vault_index: u8,
        withdraw_instructions: Vec<SquadsCompiledInstruction>,
        withdraw_accounts: Vec<AccountMeta>,
        deposit_instructions: Vec<SquadsCompiledInstruction>,
        deposit_accounts: Vec<AccountMeta>,
    ) -> Instruction {
        execute_loyal_action_route(
            self.route,
            signer,
            vault_index,
            vec![
                RouteInstructionPart::Compiled {
                    instructions: withdraw_instructions,
                    accounts: withdraw_accounts,
                },
                RouteInstructionPart::Compiled {
                    instructions: deposit_instructions,
                    accounts: deposit_accounts,
                },
            ],
        )
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

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct JupiterRouteAction {
    route: CrossMintRoute,
}

impl JupiterRouteAction {
    fn new(route: CrossMintRoute) -> Self {
        Self { route }
    }

    pub fn build(self, args: JupiterRouteExecution) -> Instruction {
        execute_loyal_action_route(
            self.route,
            args.swap.signer,
            args.swap.vault_index,
            vec![
                RouteInstructionPart::Compiled {
                    instructions: args.withdraw_instructions,
                    accounts: args.withdraw_accounts,
                },
                RouteInstructionPart::Instruction(jupiter_swap_instruction(args.swap)),
                RouteInstructionPart::Compiled {
                    instructions: args.deposit_instructions,
                    accounts: args.deposit_accounts,
                },
            ],
        )
    }
}

#[derive(Debug)]
pub struct JupiterRouteExecution {
    pub withdraw_instructions: Vec<SquadsCompiledInstruction>,
    pub withdraw_accounts: Vec<AccountMeta>,
    pub swap: JupiterSwapExecution,
    pub deposit_instructions: Vec<SquadsCompiledInstruction>,
    pub deposit_accounts: Vec<AccountMeta>,
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

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct HubRouteAction {
    route: CrossMintRoute,
}

impl HubRouteAction {
    fn new(route: CrossMintRoute) -> Self {
        Self { route }
    }

    pub fn build(self, args: HubRouteExecution) -> Instruction {
        execute_loyal_action_route(
            self.route,
            args.swap.signer,
            args.swap.vault_index,
            vec![
                RouteInstructionPart::Compiled {
                    instructions: args.withdraw_instructions,
                    accounts: args.withdraw_accounts,
                },
                RouteInstructionPart::Instruction(hub_swap_instruction(args.swap)),
                RouteInstructionPart::Compiled {
                    instructions: args.deposit_instructions,
                    accounts: args.deposit_accounts,
                },
            ],
        )
    }
}

#[derive(Debug)]
pub struct HubRouteExecution {
    pub withdraw_instructions: Vec<SquadsCompiledInstruction>,
    pub withdraw_accounts: Vec<AccountMeta>,
    pub swap: HubSwapExecution,
    pub deposit_instructions: Vec<SquadsCompiledInstruction>,
    pub deposit_accounts: Vec<AccountMeta>,
}

enum RouteInstructionPart {
    Compiled {
        instructions: Vec<SquadsCompiledInstruction>,
        accounts: Vec<AccountMeta>,
    },
    Instruction(Instruction),
}

fn execute_loyal_action_route<const N: usize>(
    route: loyal_actions::LoyalActionRoute<N>,
    signer: Pubkey,
    vault_index: u8,
    parts: Vec<RouteInstructionPart>,
) -> Instruction {
    let mut transaction_accounts = Vec::new();
    let mut compiled_instructions = Vec::new();

    for part in parts {
        match part {
            RouteInstructionPart::Compiled {
                instructions,
                accounts,
            } => compiled_instructions.extend(merge_compiled_instructions(
                &mut transaction_accounts,
                instructions,
                accounts,
            )),
            RouteInstructionPart::Instruction(instruction) => compiled_instructions.push(
                compile_inner_instruction(&mut transaction_accounts, instruction),
            ),
        }
    }

    execute_squads_program_interaction_instruction(
        route.action_account(),
        signer,
        vault_index,
        compiled_instructions,
        route.instruction_constraint_indexes().to_vec(),
        transaction_accounts,
    )
}

fn jupiter_swap_instruction(args: JupiterSwapExecution) -> Instruction {
    Instruction {
        program_id: JUPITER_V6_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(args.vault, false),
            AccountMeta::new(args.vault_input, false),
            AccountMeta::new(args.vault_output, false),
            AccountMeta::new_readonly(args.input_mint, false),
            AccountMeta::new_readonly(args.output_mint, false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new(
                mock_jupiter_stable_reserve_token_account(args.input_mint),
                false,
            ),
            AccountMeta::new(
                mock_jupiter_stable_reserve_token_account(args.output_mint),
                false,
            ),
            AccountMeta::new_readonly(derive_mock_jupiter_swap_authority(), false),
        ],
        data: mock_jupiter_stable_exact_in_swap_data(
            args.in_amount,
            args.out_amount,
            args.input_mint,
            args.output_mint,
        ),
    }
}

fn hub_swap_instruction(args: HubSwapExecution) -> Instruction {
    let mut instruction = loyal_actions::loyal_hub_swap_exact_in_instruction(
        args.vault,
        args.vault_input,
        args.vault_output,
        args.input_mint,
        args.output_mint,
        args.hub_authorizer,
        loyal_actions::LoyalHubSwapExactIn {
            amount_in: args.amount_in,
            amount_out: args.amount_out,
            min_out: args.min_out,
            max_fee_bps: args.max_fee_bps,
            lane_id: args.lane_id,
        },
    );
    instruction.accounts[1].is_signer = false;
    instruction
}

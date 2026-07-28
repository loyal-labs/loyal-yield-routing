use crate::state::{KaminoRecord, KaminoReserveRecord};
use anyhow::{bail, Context, Result};
use klend_interface::{
    from_account_data,
    instructions::{
        deposit::{
            deposit_reserve_liquidity_and_obligation_collateral_v2,
            DepositReserveLiquidityAndObligationCollateralV2Accounts,
        },
        obligation::{
            init_obligation, init_obligation_farms_for_reserve, InitObligationAccounts,
            InitObligationFarmsForReserveAccounts,
        },
        referrer::{init_user_metadata, InitUserMetadataAccounts},
        refresh::{
            refresh_obligation, refresh_reserve, RefreshObligationAccounts, RefreshReserveAccounts,
        },
        withdraw::{
            withdraw_obligation_collateral_and_redeem_reserve_collateral_v2,
            WithdrawObligationCollateralAndRedeemReserveCollateralV2Accounts,
        },
    },
    pda::farms_user_state,
    state::Obligation,
    types::InitObligationArgs,
};
use loyal_actions::{
    autonomous_vaults::{
        create_kamino_policies, AutonomousKaminoPolicies, KaminoReservePolicyTemplate,
    },
    derive_kamino_user_metadata, derive_kamino_vanilla_obligation, SquadsAccountConstraintKindView,
    SquadsAccountConstraintView, SquadsDataConstraintView, SquadsDataOperatorView,
    SquadsDataValueView, SquadsInstructionConstraintView, ASSOCIATED_TOKEN_PROGRAM_ID,
    KAMINO_INIT_OBLIGATION_DISCRIMINATOR, KAMINO_LEND_PROGRAM_ID, USDC_MINT,
};
use loyal_yield_orchestrator::{decode_kamino_reserve_account, KaminoReserveCatalogAccount};
use solana_client::rpc_client::RpcClient;
use solana_sdk::pubkey;
use solana_sdk::{
    account::Account,
    commitment_config::CommitmentConfig,
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};

pub const KAMINO_OPERATIONS_POLICY_SEED: u64 = 1;
pub const KAMINO_INIT_POLICY_SEED: u64 = 2;

const APPROVED_KAMINO_PAIRS: [(Pubkey, Pubkey); 2] = [
    (
        pubkey!("47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"),
        pubkey!("AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z"),
    ),
    (
        pubkey!("7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF"),
        pubkey!("D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59"),
    ),
];

#[derive(Clone, Debug)]
pub struct KaminoReservePlan {
    pub decoded: KaminoReserveCatalogAccount,
    pub obligation: Pubkey,
    pub obligation_farm_user_state: Option<Pubkey>,
    pub deposit_instruction: Instruction,
    pub withdraw_instruction: Instruction,
    pub init_obligation_instruction: Instruction,
}

#[derive(Clone, Debug)]
pub struct KaminoPlan {
    pub source_slot: u64,
    pub vault_usdc: Pubkey,
    pub reserves: Vec<KaminoReservePlan>,
    pub policies: AutonomousKaminoPolicies,
    pub operations_constraints: Vec<SquadsInstructionConstraintView>,
    pub init_constraints: Vec<SquadsInstructionConstraintView>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct KaminoObligationSnapshot {
    pub exists: bool,
    pub lamports: u64,
    pub deposited_amount: u64,
    pub deposit_reserves: Vec<Pubkey>,
    pub borrow_reserves: Vec<Pubkey>,
}

pub fn load_plan(
    rpc: &RpcClient,
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    vault: Pubkey,
    vault_index: u8,
) -> Result<KaminoPlan> {
    let reserve_addresses = APPROVED_KAMINO_PAIRS
        .iter()
        .map(|(_, reserve)| *reserve)
        .collect::<Vec<_>>();
    let response = rpc
        .get_multiple_accounts_with_commitment(&reserve_addresses, CommitmentConfig::finalized())
        .context("fetch approved Kamino reserves at finalized commitment")?;
    if response.value.len() != APPROVED_KAMINO_PAIRS.len() {
        bail!("finalized Kamino reserve response has the wrong length");
    }

    let vault_usdc = derive_associated_token_address(vault, USDC_MINT, spl_token::id());
    let mut reserves = Vec::with_capacity(APPROVED_KAMINO_PAIRS.len());
    let mut templates = Vec::with_capacity(APPROVED_KAMINO_PAIRS.len());
    for (((expected_market, reserve), reserve_address), account) in APPROVED_KAMINO_PAIRS
        .iter()
        .zip(reserve_addresses)
        .zip(response.value)
    {
        if reserve != &reserve_address {
            bail!("internal approved Kamino reserve ordering mismatch");
        }
        let account = account.context("approved Kamino reserve account is absent")?;
        let decoded = decode_kamino_reserve_account(reserve_address, &account)
            .context("decode approved Kamino reserve")?;
        validate_reserve(&decoded, *expected_market, reserve_address)?;
        let obligation = derive_kamino_vanilla_obligation(vault, *expected_market);
        let obligation_farm_user_state = decoded
            .collateral_farm
            .map(|farm| farms_user_state(&farm, &obligation).0);
        let deposit_instruction = deposit_instruction(
            vault,
            vault_usdc,
            &decoded,
            obligation,
            obligation_farm_user_state,
        );
        let withdraw_instruction = withdraw_instruction(
            vault,
            vault_usdc,
            &decoded,
            obligation,
            obligation_farm_user_state,
        );
        let init_obligation_instruction = init_obligation(
            InitObligationAccounts {
                obligation_owner: vault,
                fee_payer: vault,
                obligation,
                lending_market: *expected_market,
                seed1_account: Pubkey::default(),
                seed2_account: Pubkey::default(),
                owner_user_metadata: derive_kamino_user_metadata(vault),
            },
            InitObligationArgs { tag: 0, id: 0 },
        );
        templates.push(KaminoReservePolicyTemplate {
            market: *expected_market,
            reserve: reserve_address,
            vault_usdc,
            deposit_instruction: deposit_instruction.clone(),
            withdraw_instruction: withdraw_instruction.clone(),
        });
        reserves.push(KaminoReservePlan {
            decoded,
            obligation,
            obligation_farm_user_state,
            deposit_instruction,
            withdraw_instruction,
            init_obligation_instruction,
        });
    }

    let policies = create_kamino_policies(
        settings,
        authority,
        delegated_signer,
        vault,
        vault_index,
        KAMINO_OPERATIONS_POLICY_SEED,
        KAMINO_INIT_POLICY_SEED,
        templates,
    )
    .context("construct split Kamino policies")?;
    let operations_constraints = vec![
        protected_instruction_constraint(&reserves, false),
        protected_instruction_constraint(&reserves, true),
    ];
    let init_constraints = vec![init_obligation_constraint(&reserves)];

    Ok(KaminoPlan {
        source_slot: response.context.slot,
        vault_usdc,
        reserves,
        policies,
        operations_constraints,
        init_constraints,
    })
}

pub fn record_from_plan(plan: &KaminoPlan) -> KaminoRecord {
    KaminoRecord {
        source_slot: plan.source_slot,
        vault_usdc_token_account: plan.vault_usdc.to_string(),
        reserves: plan
            .reserves
            .iter()
            .map(|reserve| KaminoReserveRecord {
                market: reserve.decoded.market.to_string(),
                reserve: reserve.decoded.reserve.to_string(),
                obligation: reserve.obligation.to_string(),
                market_authority: reserve.decoded.market_authority.to_string(),
                liquidity_mint: reserve.decoded.liquidity_mint.to_string(),
                liquidity_token_program: reserve.decoded.liquidity_token_program.to_string(),
                liquidity_supply: reserve.decoded.liquidity_supply.to_string(),
                collateral_mint: reserve.decoded.collateral_mint.to_string(),
                collateral_supply: reserve.decoded.collateral_supply.to_string(),
                collateral_farm: reserve.decoded.collateral_farm.map(|key| key.to_string()),
                obligation_farm_user_state: reserve
                    .obligation_farm_user_state
                    .map(|key| key.to_string()),
            })
            .collect(),
        operations_policy: None,
        init_obligation_policy: None,
        live_steps: Vec::new(),
    }
}

pub fn validate_record(record: &KaminoRecord, plan: &KaminoPlan) -> Result<()> {
    let fresh = record_from_plan(plan);
    if record.source_slot > plan.source_slot
        || record.vault_usdc_token_account != fresh.vault_usdc_token_account
        || record.reserves != fresh.reserves
    {
        bail!("recorded Kamino account graph does not match the fresh finalized snapshot");
    }
    Ok(())
}

pub fn setup_inner_instructions(vault: Pubkey, vault_usdc: Pubkey) -> Vec<Instruction> {
    vec![
        create_associated_token_account_idempotent_instruction(
            vault,
            vault_usdc,
            vault,
            USDC_MINT,
            spl_token::id(),
        ),
        init_user_metadata(
            InitUserMetadataAccounts {
                owner: vault,
                fee_payer: vault,
                user_metadata: derive_kamino_user_metadata(vault),
                referrer_user_metadata: None,
            },
            Pubkey::default(),
        ),
    ]
}

pub fn farm_setup_inner_instructions(vault: Pubkey, plan: &KaminoPlan) -> Result<Vec<Instruction>> {
    plan.reserves
        .iter()
        .map(|reserve| {
            let reserve_farm_state = reserve
                .decoded
                .collateral_farm
                .context("approved Kamino reserve has no collateral farm")?;
            let obligation_farm = reserve
                .obligation_farm_user_state
                .context("approved Kamino reserve has no derived farm user state")?;
            Ok(init_obligation_farms_for_reserve(
                InitObligationFarmsForReserveAccounts {
                    payer: vault,
                    owner: vault,
                    obligation: reserve.obligation,
                    lending_market_authority: reserve.decoded.market_authority,
                    reserve: reserve.decoded.reserve,
                    reserve_farm_state,
                    obligation_farm,
                    lending_market: reserve.decoded.market,
                },
                0,
            ))
        })
        .collect()
}

pub fn instruction_with_amount(instruction: &Instruction, amount: u64) -> Result<Instruction> {
    if instruction.data.len() < 16 {
        bail!("Kamino instruction data is too short for an amount argument");
    }
    let mut instruction = instruction.clone();
    instruction.data[8..16].copy_from_slice(&amount.to_le_bytes());
    Ok(instruction)
}

pub fn load_obligation_snapshot(
    rpc: &RpcClient,
    vault: Pubkey,
    reserve: &KaminoReservePlan,
) -> Result<KaminoObligationSnapshot> {
    let response = rpc
        .get_account_with_commitment(&reserve.obligation, CommitmentConfig::finalized())
        .context("fetch Kamino obligation at finalized commitment")?;
    decode_obligation_snapshot(
        response.value.as_ref(),
        vault,
        reserve.decoded.market,
        reserve.decoded.reserve,
    )
}

pub fn refresh_instructions(
    rpc: &RpcClient,
    vault: Pubkey,
    reserve: &KaminoReservePlan,
) -> Result<Vec<Instruction>> {
    let snapshot = load_obligation_snapshot(rpc, vault, reserve)?;
    if !snapshot.exists {
        bail!("Kamino obligation must exist before reserve refresh and policy execution");
    }
    let mut remaining_reserves = snapshot
        .deposit_reserves
        .iter()
        .chain(snapshot.borrow_reserves.iter())
        .copied()
        .collect::<Vec<_>>();
    remaining_reserves.sort();
    remaining_reserves.dedup();
    Ok(vec![
        refresh_reserve(RefreshReserveAccounts {
            reserve: reserve.decoded.reserve,
            lending_market: reserve.decoded.market,
            pyth_oracle: reserve.decoded.pyth_oracle,
            switchboard_price_oracle: reserve.decoded.switchboard_price_oracle,
            switchboard_twap_oracle: reserve.decoded.switchboard_twap_oracle,
            scope_prices: reserve.decoded.scope_prices,
        }),
        refresh_obligation(
            RefreshObligationAccounts {
                lending_market: reserve.decoded.market,
                obligation: reserve.obligation,
            },
            remaining_reserves
                .into_iter()
                .map(|reserve| AccountMeta::new(reserve, false))
                .collect(),
        ),
    ])
}

fn decode_obligation_snapshot(
    account: Option<&Account>,
    vault: Pubkey,
    market: Pubkey,
    reserve: Pubkey,
) -> Result<KaminoObligationSnapshot> {
    let Some(account) = account else {
        return Ok(KaminoObligationSnapshot {
            exists: false,
            lamports: 0,
            deposited_amount: 0,
            deposit_reserves: Vec::new(),
            borrow_reserves: Vec::new(),
        });
    };
    if account.owner != KAMINO_LEND_PROGRAM_ID {
        bail!("Kamino obligation has an unexpected account owner");
    }
    let state =
        from_account_data::<Obligation>(&account.data).context("decode Kamino obligation")?;
    if state.owner != vault || state.lending_market != market {
        bail!("Kamino obligation owner or market does not match the autonomous vault manifest");
    }
    let deposited_amount = state
        .deposits
        .iter()
        .find(|deposit| deposit.deposit_reserve == reserve)
        .map(|deposit| deposit.deposited_amount)
        .unwrap_or_default();
    Ok(KaminoObligationSnapshot {
        exists: true,
        lamports: account.lamports,
        deposited_amount,
        deposit_reserves: state
            .deposits
            .iter()
            .filter(|deposit| deposit.deposit_reserve != Pubkey::default())
            .map(|deposit| deposit.deposit_reserve)
            .collect(),
        borrow_reserves: state
            .borrows
            .iter()
            .filter(|borrow| borrow.borrow_reserve != Pubkey::default())
            .map(|borrow| borrow.borrow_reserve)
            .collect(),
    })
}

fn create_associated_token_account_idempotent_instruction(
    funding_address: Pubkey,
    associated_account_address: Pubkey,
    wallet_address: Pubkey,
    token_mint_address: Pubkey,
    token_program_id: Pubkey,
) -> Instruction {
    Instruction {
        program_id: ASSOCIATED_TOKEN_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(funding_address, true),
            AccountMeta::new(associated_account_address, false),
            AccountMeta::new_readonly(wallet_address, false),
            AccountMeta::new_readonly(token_mint_address, false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(token_program_id, false),
        ],
        data: vec![1],
    }
}

fn validate_reserve(
    reserve: &KaminoReserveCatalogAccount,
    expected_market: Pubkey,
    expected_reserve: Pubkey,
) -> Result<()> {
    if reserve.reserve != expected_reserve
        || reserve.market != expected_market
        || reserve.liquidity_mint != USDC_MINT
        || reserve.liquidity_token_program != spl_token::id()
    {
        bail!("approved Kamino reserve no longer matches its market/USDC manifest");
    }
    Ok(())
}

fn deposit_instruction(
    vault: Pubkey,
    vault_usdc: Pubkey,
    reserve: &KaminoReserveCatalogAccount,
    obligation: Pubkey,
    obligation_farm_user_state: Option<Pubkey>,
) -> Instruction {
    deposit_reserve_liquidity_and_obligation_collateral_v2(
        DepositReserveLiquidityAndObligationCollateralV2Accounts {
            owner: vault,
            obligation,
            lending_market: reserve.market,
            lending_market_authority: reserve.market_authority,
            reserve: reserve.reserve,
            reserve_liquidity_mint: reserve.liquidity_mint,
            reserve_liquidity_supply: reserve.liquidity_supply,
            reserve_collateral_mint: reserve.collateral_mint,
            reserve_destination_deposit_collateral: reserve.collateral_supply,
            user_source_liquidity: vault_usdc,
            placeholder_user_destination_collateral: None,
            liquidity_token_program: reserve.liquidity_token_program,
            obligation_farm_user_state,
            reserve_farm_state: reserve.collateral_farm,
        },
        1,
    )
}

fn withdraw_instruction(
    vault: Pubkey,
    vault_usdc: Pubkey,
    reserve: &KaminoReserveCatalogAccount,
    obligation: Pubkey,
    obligation_farm_user_state: Option<Pubkey>,
) -> Instruction {
    withdraw_obligation_collateral_and_redeem_reserve_collateral_v2(
        WithdrawObligationCollateralAndRedeemReserveCollateralV2Accounts {
            owner: vault,
            obligation,
            lending_market: reserve.market,
            lending_market_authority: reserve.market_authority,
            withdraw_reserve: reserve.reserve,
            reserve_liquidity_mint: reserve.liquidity_mint,
            reserve_source_collateral: reserve.collateral_supply,
            reserve_collateral_mint: reserve.collateral_mint,
            reserve_liquidity_supply: reserve.liquidity_supply,
            user_destination_liquidity: vault_usdc,
            placeholder_user_destination_collateral: None,
            liquidity_token_program: reserve.liquidity_token_program,
            obligation_farm_user_state,
            reserve_farm_state: reserve.collateral_farm,
        },
        1,
    )
}

fn derive_associated_token_address(owner: Pubkey, mint: Pubkey, token_program: Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[owner.as_ref(), token_program.as_ref(), mint.as_ref()],
        &ASSOCIATED_TOKEN_PROGRAM_ID,
    )
    .0
}

fn protected_instruction_constraint(
    reserves: &[KaminoReservePlan],
    withdraw: bool,
) -> SquadsInstructionConstraintView {
    const PROTECTED_ACCOUNT_INDEXES: [usize; 6] = [0, 1, 2, 4, 5, 9];
    let instructions = reserves
        .iter()
        .map(|reserve| {
            if withdraw {
                &reserve.withdraw_instruction
            } else {
                &reserve.deposit_instruction
            }
        })
        .collect::<Vec<_>>();
    let instruction = instructions[0];
    SquadsInstructionConstraintView {
        program_id: instruction.program_id,
        account_constraints: PROTECTED_ACCOUNT_INDEXES
            .iter()
            .map(|index| {
                pubkey_allowlist_constraint(
                    *index as u8,
                    instructions
                        .iter()
                        .map(|instruction| instruction.accounts[*index].pubkey)
                        .collect(),
                )
            })
            .collect(),
        data_constraints: vec![equals_bytes(0, instruction.data[..8].to_vec())],
    }
}

fn init_obligation_constraint(reserves: &[KaminoReservePlan]) -> SquadsInstructionConstraintView {
    let instructions = reserves
        .iter()
        .map(|reserve| &reserve.init_obligation_instruction)
        .collect::<Vec<_>>();
    let instruction = instructions[0];
    debug_assert_eq!(instruction.program_id, KAMINO_LEND_PROGRAM_ID);
    debug_assert_eq!(instruction.accounts.len(), 9);
    debug_assert!(instruction
        .data
        .starts_with(&KAMINO_INIT_OBLIGATION_DISCRIMINATOR));
    SquadsInstructionConstraintView {
        program_id: instruction.program_id,
        account_constraints: instruction
            .accounts
            .iter()
            .enumerate()
            .map(|(index, _)| {
                pubkey_allowlist_constraint(
                    index as u8,
                    instructions
                        .iter()
                        .map(|instruction| instruction.accounts[index].pubkey)
                        .collect(),
                )
            })
            .collect(),
        data_constraints: vec![equals_bytes(0, instruction.data.clone())],
    }
}

fn pubkey_allowlist_constraint(
    account_index: u8,
    mut pubkeys: Vec<Pubkey>,
) -> SquadsAccountConstraintView {
    pubkeys.sort();
    pubkeys.dedup();
    SquadsAccountConstraintView {
        account_index,
        kind: SquadsAccountConstraintKindView::Pubkey(pubkeys),
        owner: None,
    }
}

fn equals_bytes(data_offset: u64, bytes: Vec<u8>) -> SquadsDataConstraintView {
    SquadsDataConstraintView {
        data_offset,
        data_value: SquadsDataValueView::U8Slice(bytes),
        operator: SquadsDataOperatorView::Equals,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn approved_pairs_are_exact_and_market_specific() {
        assert_eq!(APPROVED_KAMINO_PAIRS.len(), 2);
        assert_ne!(APPROVED_KAMINO_PAIRS[0].0, APPROVED_KAMINO_PAIRS[1].0);
        assert_ne!(APPROVED_KAMINO_PAIRS[0].1, APPROVED_KAMINO_PAIRS[1].1);
    }

    #[test]
    fn vault_ata_derivation_is_token_program_specific() {
        let owner = Pubkey::new_unique();
        assert_ne!(
            derive_associated_token_address(owner, USDC_MINT, spl_token::id()),
            derive_associated_token_address(owner, USDC_MINT, Pubkey::new_unique())
        );
    }
}

use litesvm::LiteSVM;
pub use loyal_actions::LoyalHubLaneRebalanceTransfer as LoyalHubRebalanceTransfer;
use solana_sdk::{account::Account, hash::hashv, instruction::AccountMeta, pubkey::Pubkey};
use spl_token::solana_program::{program_option::COption, program_pack::Pack};
use std::{env, fs, path::PathBuf};

use crate::*;

pub fn mock_jupiter_swap_data(
    operation: u8,
    amount: u64,
    input_mint: Pubkey,
    output_mint: Pubkey,
) -> Vec<u8> {
    let mut data = Vec::with_capacity(73);
    data.push(operation);
    data.extend_from_slice(&amount.to_le_bytes());
    data.extend_from_slice(input_mint.as_ref());
    data.extend_from_slice(output_mint.as_ref());
    data
}

pub fn mock_jupiter_stable_exact_in_swap_data(
    in_amount: u64,
    out_amount: u64,
    input_mint: Pubkey,
    output_mint: Pubkey,
) -> Vec<u8> {
    mock_jupiter_stable_exact_in_swap_data_with_slippage(
        in_amount,
        out_amount,
        50,
        input_mint,
        output_mint,
    )
}

pub fn mock_jupiter_stable_exact_in_swap_data_with_slippage(
    in_amount: u64,
    out_amount: u64,
    slippage_bps: u16,
    input_mint: Pubkey,
    output_mint: Pubkey,
) -> Vec<u8> {
    let mut data = Vec::with_capacity(90);
    data.extend_from_slice(&MOCK_JUPITER_STABLE_EXACT_IN);
    data.extend_from_slice(&in_amount.to_le_bytes());
    data.extend_from_slice(&out_amount.to_le_bytes());
    data.extend_from_slice(&slippage_bps.to_le_bytes());
    data.extend_from_slice(input_mint.as_ref());
    data.extend_from_slice(output_mint.as_ref());
    data
}

pub fn loyal_hub_config_data(
    admin: Pubkey,
    hub_authorizer: Pubkey,
    inventory_rebalancer: Pubkey,
    max_fee_bps: u16,
    paused: bool,
    lane_count: u8,
    allowed_mints: &[Pubkey],
) -> Vec<u8> {
    loyal_actions::loyal_hub_config_data(
        admin,
        hub_authorizer,
        inventory_rebalancer,
        max_fee_bps,
        paused,
        lane_count,
        allowed_mints,
    )
    .expect("valid Loyal Hub config data")
}

pub fn loyal_hub_initialize_config_data(
    admin: Pubkey,
    hub_authorizer: Pubkey,
    inventory_rebalancer: Pubkey,
    max_fee_bps: u16,
    paused: bool,
    lane_count: u8,
    allowed_mints: &[Pubkey],
) -> Vec<u8> {
    loyal_actions::loyal_hub_initialize_config_data(
        admin,
        hub_authorizer,
        inventory_rebalancer,
        max_fee_bps,
        paused,
        lane_count,
        allowed_mints,
    )
    .expect("valid Loyal Hub initialize config data")
}

pub fn loyal_hub_set_max_fee_data(max_fee_bps: u16) -> Vec<u8> {
    loyal_actions::loyal_hub_set_max_fee_data(max_fee_bps).expect("valid Loyal Hub max fee data")
}

pub fn loyal_hub_set_admin_data() -> Vec<u8> {
    loyal_actions::loyal_hub_set_admin_data()
}

pub fn loyal_hub_set_hub_authorizer_data() -> Vec<u8> {
    loyal_actions::loyal_hub_set_hub_authorizer_data()
}

pub fn loyal_hub_set_inventory_rebalancer_data() -> Vec<u8> {
    loyal_actions::loyal_hub_set_inventory_rebalancer_data()
}

pub fn loyal_hub_set_lane_count_data(lane_count: u8) -> Vec<u8> {
    loyal_actions::loyal_hub_set_lane_count_data(lane_count)
        .expect("valid Loyal Hub lane count data")
}

pub fn loyal_hub_swap_exact_in_data(
    amount_in: u64,
    amount_out: u64,
    min_out: u64,
    max_fee_bps: u16,
    lane_id: u8,
) -> Vec<u8> {
    loyal_actions::loyal_hub_swap_exact_in_data(loyal_actions::LoyalHubSwapExactIn {
        amount_in,
        amount_out,
        min_out,
        max_fee_bps,
        lane_id,
    })
}

pub fn loyal_hub_set_paused_data(paused: bool) -> Vec<u8> {
    loyal_actions::loyal_hub_set_paused_data(paused)
}

pub fn loyal_hub_withdraw_inventory_data(amount: u64, lane_id: u8) -> Vec<u8> {
    loyal_actions::loyal_hub_withdraw_inventory_data(amount, lane_id)
}

pub fn loyal_hub_rebalance_inventory_data(transfers: &[LoyalHubRebalanceTransfer]) -> Vec<u8> {
    loyal_actions::loyal_hub_rebalance_inventory_data(transfers)
        .expect("valid Loyal Hub rebalance transfer batch")
}

pub fn mock_kamino_deposit_reserve_liquidity_data(amount: u64) -> Vec<u8> {
    mock_kamino_reserve_liquidity_data(KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR, amount)
}

pub fn mock_kamino_withdraw_reserve_liquidity_data(amount: u64) -> Vec<u8> {
    mock_kamino_reserve_liquidity_data(KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR, amount)
}

fn mock_kamino_reserve_liquidity_data(discriminator: [u8; 8], amount: u64) -> Vec<u8> {
    let mut data = Vec::with_capacity(16);
    data.extend_from_slice(&discriminator);
    data.extend_from_slice(&amount.to_le_bytes());
    data
}

pub fn mock_kamino_reserve_transaction(
    vault: Pubkey,
    reserve_accounts: MockKaminoReserveTokenAccounts,
    data: Vec<u8>,
) -> (Vec<SquadsCompiledInstruction>, Vec<AccountMeta>) {
    let is_deposit = data.get(..8).is_some_and(|discriminator| {
        discriminator == KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR
    });
    let is_withdraw = data.get(..8).is_some_and(|discriminator| {
        discriminator == KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR
    });
    assert!(
        is_deposit || is_withdraw,
        "mock Kamino transaction requires deposit or withdraw data"
    );

    let transaction_accounts = if is_deposit {
        vec![
            AccountMeta::new(vault, false),
            AccountMeta::new(reserve_accounts.reserve, false),
            AccountMeta::new_readonly(reserve_accounts.market, false),
            AccountMeta::new_readonly(reserve_accounts.lending_market_authority, false),
            AccountMeta::new(reserve_accounts.reserve, false),
            AccountMeta::new_readonly(reserve_accounts.liquidity_mint, false),
            AccountMeta::new(reserve_accounts.reserve_liquidity_supply, false),
            AccountMeta::new(reserve_accounts.collateral_mint, false),
            AccountMeta::new(reserve_accounts.reserve_collateral_supply, false),
            AccountMeta::new(reserve_accounts.vault_liquidity, false),
            AccountMeta::new_readonly(KAMINO_LEND_PROGRAM_ID, false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(solana_sdk::sysvar::instructions::id(), false),
            AccountMeta::new_readonly(KAMINO_LEND_PROGRAM_ID, false),
            AccountMeta::new_readonly(KAMINO_LEND_PROGRAM_ID, false),
            AccountMeta::new_readonly(KAMINO_LEND_PROGRAM_ID, false),
        ]
    } else {
        vec![
            AccountMeta::new(vault, false),
            AccountMeta::new(reserve_accounts.reserve, false),
            AccountMeta::new_readonly(reserve_accounts.market, false),
            AccountMeta::new_readonly(reserve_accounts.lending_market_authority, false),
            AccountMeta::new(reserve_accounts.reserve, false),
            AccountMeta::new_readonly(reserve_accounts.liquidity_mint, false),
            AccountMeta::new(reserve_accounts.reserve_collateral_supply, false),
            AccountMeta::new(reserve_accounts.collateral_mint, false),
            AccountMeta::new(reserve_accounts.reserve_liquidity_supply, false),
            AccountMeta::new(reserve_accounts.vault_liquidity, false),
            AccountMeta::new_readonly(KAMINO_LEND_PROGRAM_ID, false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(solana_sdk::sysvar::instructions::id(), false),
            AccountMeta::new_readonly(KAMINO_LEND_PROGRAM_ID, false),
            AccountMeta::new_readonly(KAMINO_LEND_PROGRAM_ID, false),
            AccountMeta::new_readonly(KAMINO_LEND_PROGRAM_ID, false),
        ]
    };

    (
        vec![SquadsCompiledInstruction {
            program_id_index: 10,
            accounts: vec![0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16],
            data,
        }],
        transaction_accounts,
    )
}

#[derive(Debug)]
pub struct MockKaminoRoutePart {
    pub instructions: Vec<SquadsCompiledInstruction>,
    pub accounts: Vec<AccountMeta>,
    pub lookup_table_requirements: loyal_actions::YieldRouteLookupTableRequirements,
}

impl MockKaminoRoutePart {
    pub fn into_parts(
        self,
    ) -> (
        Vec<SquadsCompiledInstruction>,
        Vec<AccountMeta>,
        loyal_actions::YieldRouteLookupTableRequirements,
    ) {
        (
            self.instructions,
            self.accounts,
            self.lookup_table_requirements,
        )
    }
}

pub fn mock_kamino_reserve_route_part(
    vault: Pubkey,
    reserve: MockKaminoReserveTokenAccounts,
    data: Vec<u8>,
) -> MockKaminoRoutePart {
    let (instructions, accounts) = mock_kamino_reserve_transaction(vault, reserve, data);
    let mut reserve_requirements = loyal_actions::KaminoReserveLookupTableAccounts::new(
        reserve.market,
        reserve.reserve,
        reserve.liquidity_mint,
    );
    reserve_requirements.market_authorities = vec![
        reserve.lending_market_authority,
        loyal_actions::derive_kamino_lending_market_authority(reserve.market),
    ];
    reserve_requirements.liquidity_supply = Some(reserve.reserve_liquidity_supply);
    reserve_requirements.collateral_mint = Some(reserve.collateral_mint);
    reserve_requirements.collateral_supply = Some(reserve.reserve_collateral_supply);
    reserve_requirements.infrastructure = vec![
        KAMINO_LEND_PROGRAM_ID,
        spl_token::id(),
        solana_sdk::sysvar::instructions::id(),
    ];
    let mut lookup_table_requirements = loyal_actions::YieldRouteLookupTableRequirements::default();
    lookup_table_requirements.add_kamino_reserve(reserve_requirements);
    lookup_table_requirements.add_vault_account(vault);
    lookup_table_requirements.add_vault_token_account(reserve.vault_liquidity);

    MockKaminoRoutePart {
        instructions,
        accounts,
        lookup_table_requirements,
    }
}

pub fn derive_mock_jupiter_swap_authority() -> Pubkey {
    Pubkey::find_program_address(&[JUPITER_SWAP_AUTHORITY_SEED], &JUPITER_V6_PROGRAM_ID).0
}

pub fn derive_loyal_hub_config() -> Pubkey {
    loyal_actions::derive_loyal_hub_config()
}

pub fn derive_loyal_hub_authority() -> Pubkey {
    loyal_actions::derive_loyal_hub_authority()
}

pub fn loyal_hub_token_account(mint: Pubkey) -> Pubkey {
    loyal_hub_lane_token_account(mint, 0)
}

pub fn derive_loyal_hub_lane_authority(lane_id: u8) -> Pubkey {
    loyal_actions::derive_loyal_hub_lane_authority(lane_id)
}

pub fn loyal_hub_lane_token_account(mint: Pubkey, lane_id: u8) -> Pubkey {
    loyal_actions::derive_loyal_hub_lane_inventory_account(mint, lane_id)
}

pub fn derive_associated_token_account(owner: Pubkey, mint: Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[owner.as_ref(), spl_token::id().as_ref(), mint.as_ref()],
        &ASSOCIATED_TOKEN_PROGRAM_ID,
    )
    .0
}

pub fn derive_subscription_authority(user: Pubkey, mint: Pubkey) -> Pubkey {
    loyal_actions::derive_subscription_authority(user, mint)
}

pub fn derive_recurring_delegation(
    subscription_authority: Pubkey,
    delegator: Pubkey,
    delegatee: Pubkey,
    nonce: u64,
) -> Pubkey {
    loyal_actions::derive_recurring_delegation(subscription_authority, delegator, delegatee, nonce)
}

pub fn derive_subscription_event_authority() -> Pubkey {
    loyal_actions::derive_subscription_event_authority()
}

pub fn subscription_init_authority_instruction(
    owner: Pubkey,
    subscription_authority: Pubkey,
    token_mint: Pubkey,
    user_ata: Pubkey,
) -> solana_sdk::instruction::Instruction {
    solana_sdk::instruction::Instruction {
        program_id: SUBSCRIPTIONS_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(owner, true),
            AccountMeta::new(subscription_authority, false),
            AccountMeta::new_readonly(token_mint, false),
            AccountMeta::new(user_ata, false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(spl_token::id(), false),
        ],
        data: loyal_actions::subscription_init_authority_data(),
    }
}

pub struct SubscriptionRecurringDelegationArgs {
    pub delegator: Pubkey,
    pub subscription_authority: Pubkey,
    pub delegation: Pubkey,
    pub delegatee: Pubkey,
    pub nonce: u64,
    pub amount_per_period: u64,
    pub period_length_s: u64,
    pub start_ts: i64,
    pub expiry_ts: i64,
    pub expected_subscription_authority_init_id: i64,
}

pub fn subscription_create_recurring_delegation_instruction(
    args: SubscriptionRecurringDelegationArgs,
) -> solana_sdk::instruction::Instruction {
    solana_sdk::instruction::Instruction {
        program_id: SUBSCRIPTIONS_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(args.delegator, true),
            AccountMeta::new_readonly(args.subscription_authority, false),
            AccountMeta::new(args.delegation, false),
            AccountMeta::new_readonly(args.delegatee, false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
        ],
        data: loyal_actions::subscription_create_recurring_delegation_data(
            args.nonce,
            args.amount_per_period,
            args.period_length_s,
            args.start_ts,
            args.expiry_ts,
            args.expected_subscription_authority_init_id,
        ),
    }
}

pub fn subscription_revoke_delegation_instruction(
    authority: Pubkey,
    delegation: Pubkey,
) -> solana_sdk::instruction::Instruction {
    solana_sdk::instruction::Instruction {
        program_id: SUBSCRIPTIONS_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(authority, true),
            AccountMeta::new(delegation, false),
        ],
        data: loyal_actions::subscription_revoke_delegation_data(),
    }
}

pub fn recurring_delegation_amount_pulled_in_period(svm: &LiteSVM, delegation: Pubkey) -> u64 {
    let account = svm
        .get_account(&delegation)
        .expect("recurring delegation account exists");
    let offset = usize::try_from(SUBSCRIPTION_RECURRING_DELEGATION_AMOUNT_PULLED_OFFSET)
        .expect("amount pulled offset fits in usize");
    let amount = account
        .data
        .get(offset..offset + 8)
        .expect("recurring delegation amount field exists");
    u64::from_le_bytes(amount.try_into().expect("amount field is 8 bytes"))
}

pub fn mock_jupiter_usdc_reserve_token_account() -> Pubkey {
    Pubkey::new_from_array(hash32(MOCK_JUPITER_USDC_RESERVE_TOKEN_ACCOUNT_SEED))
}

pub fn mock_jupiter_pyusd_reserve_token_account() -> Pubkey {
    Pubkey::new_from_array(hash32(MOCK_JUPITER_PYUSD_RESERVE_TOKEN_ACCOUNT_SEED))
}

pub fn mock_jupiter_stable_reserve_token_account(mint: Pubkey) -> Pubkey {
    Pubkey::new_from_array(
        hashv(&[
            MOCK_JUPITER_STABLE_RESERVE_TOKEN_ACCOUNT_SEED,
            mint.as_ref(),
        ])
        .to_bytes(),
    )
}

pub fn mock_jupiter_token_accounts() -> MockJupiterTokenAccounts {
    MockJupiterTokenAccounts {
        authority: derive_mock_jupiter_swap_authority(),
        usdc_reserve: mock_jupiter_usdc_reserve_token_account(),
        pyusd_reserve: mock_jupiter_pyusd_reserve_token_account(),
    }
}

pub fn derive_mock_kamino_reserve_liquidity_authority(reserve: Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[KAMINO_RESERVE_LIQUIDITY_AUTHORITY_SEED, reserve.as_ref()],
        &KAMINO_LEND_PROGRAM_ID,
    )
    .0
}

pub fn derive_mock_kamino_collateral_mint_authority(reserve: Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[KAMINO_COLLATERAL_MINT_AUTHORITY_SEED, reserve.as_ref()],
        &KAMINO_LEND_PROGRAM_ID,
    )
    .0
}

pub fn derive_mock_kamino_lending_market_authority(market: Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[KAMINO_LENDING_MARKET_AUTHORITY_SEED, market.as_ref()],
        &KAMINO_LEND_PROGRAM_ID,
    )
    .0
}

pub fn mock_kamino_collateral_mint(reserve: Pubkey) -> Pubkey {
    Pubkey::new_from_array(hashv(&[b"mock-kamino-collateral-mint", reserve.as_ref()]).to_bytes())
}

pub fn seed_empty_system_account_if_missing(svm: &mut LiteSVM, pubkey: Pubkey) {
    if svm.get_account(&pubkey).is_some() {
        return;
    }

    svm.set_account(
        pubkey,
        Account {
            lamports: LAMPORTS_PER_SOL,
            data: vec![],
            owner: solana_sdk::system_program::ID,
            executable: false,
            rent_epoch: 0,
        },
    )
    .expect("seed empty system account");
}

pub fn seed_spl_mint(
    svm: &mut LiteSVM,
    mint: Pubkey,
    mint_authority: Option<Pubkey>,
    decimals: u8,
    supply: u64,
) {
    let mut data = vec![0; spl_token::state::Mint::LEN];
    spl_token::state::Mint {
        mint_authority: mint_authority.map_or(COption::None, COption::Some),
        supply,
        decimals,
        is_initialized: true,
        freeze_authority: COption::None,
    }
    .pack_into_slice(&mut data);

    svm.set_account(
        mint,
        Account {
            lamports: LAMPORTS_PER_SOL,
            data,
            owner: spl_token::id(),
            executable: false,
            rent_epoch: 0,
        },
    )
    .expect("seed SPL mint");
}

pub fn seed_spl_mint_if_missing(
    svm: &mut LiteSVM,
    mint: Pubkey,
    mint_authority: Option<Pubkey>,
    decimals: u8,
    supply: u64,
) {
    if svm.get_account(&mint).is_none() {
        seed_spl_mint(svm, mint, mint_authority, decimals, supply);
    }
}

pub fn seed_spl_token_account(
    svm: &mut LiteSVM,
    token_account: Pubkey,
    mint: Pubkey,
    owner: Pubkey,
    amount: u64,
) {
    let mut data = vec![0; spl_token::state::Account::LEN];
    spl_token::state::Account {
        mint,
        owner,
        amount,
        delegate: COption::None,
        state: spl_token::state::AccountState::Initialized,
        is_native: COption::None,
        delegated_amount: 0,
        close_authority: COption::None,
    }
    .pack_into_slice(&mut data);

    svm.set_account(
        token_account,
        Account {
            lamports: LAMPORTS_PER_SOL,
            data,
            owner: spl_token::id(),
            executable: false,
            rent_epoch: 0,
        },
    )
    .expect("seed SPL token account");
}

pub fn seed_spl_token_account_if_missing(
    svm: &mut LiteSVM,
    token_account: Pubkey,
    mint: Pubkey,
    owner: Pubkey,
    amount: u64,
) {
    if svm.get_account(&token_account).is_none() {
        seed_spl_token_account(svm, token_account, mint, owner, amount);
    }
}

pub fn get_spl_token_amount(svm: &LiteSVM, token_account: Pubkey) -> u64 {
    let account = svm
        .get_account(&token_account)
        .expect("SPL token account exists");
    let token_account =
        spl_token::state::Account::unpack(&account.data).expect("unpack SPL token account");
    token_account.amount
}

pub fn set_spl_token_amount(svm: &mut LiteSVM, token_account: Pubkey, amount: u64) {
    let mut account = svm
        .get_account(&token_account)
        .expect("SPL token account exists");
    let mut token =
        spl_token::state::Account::unpack(&account.data).expect("unpack SPL token account");
    token.amount = amount;
    token.pack_into_slice(&mut account.data);
    svm.set_account(token_account, account)
        .expect("update SPL token account amount");
}

pub fn set_spl_mint_supply(svm: &mut LiteSVM, mint: Pubkey, supply: u64) {
    let mut account = svm.get_account(&mint).expect("SPL mint exists");
    let mut mint_state = spl_token::state::Mint::unpack(&account.data).expect("unpack SPL mint");
    mint_state.supply = supply;
    mint_state.pack_into_slice(&mut account.data);
    svm.set_account(mint, account)
        .expect("update SPL mint supply");
}

pub fn seed_mock_jupiter_spl_accounts(
    svm: &mut LiteSVM,
    usdc_reserve_amount: u64,
    pyusd_reserve_amount: u64,
) -> MockJupiterTokenAccounts {
    let accounts = mock_jupiter_token_accounts();
    seed_empty_system_account_if_missing(svm, accounts.authority);
    seed_spl_mint_if_missing(svm, USDC_MINT, None, USDC_DECIMALS, usdc_reserve_amount);
    seed_spl_mint_if_missing(svm, PYUSD_MINT, None, PYUSD_DECIMALS, pyusd_reserve_amount);
    seed_spl_token_account(
        svm,
        accounts.usdc_reserve,
        USDC_MINT,
        accounts.authority,
        usdc_reserve_amount,
    );
    seed_spl_token_account(
        svm,
        accounts.pyusd_reserve,
        PYUSD_MINT,
        accounts.authority,
        pyusd_reserve_amount,
    );
    accounts
}

pub fn seed_mock_jupiter_stable_reserve_spl_accounts(
    svm: &mut LiteSVM,
    stable_reserves: &[MockJupiterStableReserveTokenAccount],
    reserve_amount: u64,
) {
    let authority = derive_mock_jupiter_swap_authority();
    seed_empty_system_account_if_missing(svm, authority);
    for stable_reserve in stable_reserves {
        seed_spl_token_account(
            svm,
            stable_reserve.reserve,
            stable_reserve.mint,
            authority,
            reserve_amount,
        );
    }
}

pub fn seed_loyal_hub_inventory_spl_accounts(
    svm: &mut LiteSVM,
    stable_mints: &[Pubkey],
    reserve_amount: u64,
) -> Vec<Pubkey> {
    seed_loyal_hub_inventory_spl_accounts_for_lane(svm, stable_mints, reserve_amount, 0)
}

pub fn seed_loyal_hub_inventory_spl_accounts_for_lane(
    svm: &mut LiteSVM,
    stable_mints: &[Pubkey],
    reserve_amount: u64,
    lane_id: u8,
) -> Vec<Pubkey> {
    let authority = derive_loyal_hub_lane_authority(lane_id);
    seed_empty_system_account_if_missing(svm, authority);
    stable_mints
        .iter()
        .map(|mint| {
            let token_account = loyal_hub_lane_token_account(*mint, lane_id);
            seed_spl_token_account(svm, token_account, *mint, authority, reserve_amount);
            token_account
        })
        .collect()
}

pub fn seed_mock_kamino_reserve_spl_accounts(
    svm: &mut LiteSVM,
    reserve: Pubkey,
    market: Pubkey,
    vault: Pubkey,
    vault_liquidity: Pubkey,
    reserve_collateral_supply: Pubkey,
    reserve_liquidity_supply: Pubkey,
) -> MockKaminoReserveTokenAccounts {
    seed_mock_kamino_reserve_spl_accounts_with_mint(
        svm,
        reserve,
        market,
        USDC_MINT,
        USDC_DECIMALS,
        vault,
        vault_liquidity,
        reserve_collateral_supply,
        reserve_liquidity_supply,
    )
}

pub fn seed_mock_kamino_reserve_spl_accounts_with_mint(
    svm: &mut LiteSVM,
    reserve: Pubkey,
    market: Pubkey,
    liquidity_mint: Pubkey,
    liquidity_decimals: u8,
    vault: Pubkey,
    vault_liquidity: Pubkey,
    reserve_collateral_supply: Pubkey,
    reserve_liquidity_supply: Pubkey,
) -> MockKaminoReserveTokenAccounts {
    let lending_market_authority = derive_mock_kamino_lending_market_authority(market);
    let reserve_liquidity_authority = lending_market_authority;
    let collateral_mint_authority = lending_market_authority;
    let collateral_mint = mock_kamino_collateral_mint(reserve);

    seed_empty_system_account_if_missing(svm, market);
    seed_mock_kamino_reserve_account(
        svm,
        reserve,
        market,
        liquidity_mint,
        collateral_mint,
        reserve_liquidity_supply,
        reserve_collateral_supply,
    );
    seed_empty_system_account_if_missing(svm, lending_market_authority);
    seed_spl_mint_if_missing(svm, liquidity_mint, None, liquidity_decimals, 0);
    seed_spl_mint(
        svm,
        collateral_mint,
        Some(collateral_mint_authority),
        KAMINO_COLLATERAL_DECIMALS,
        0,
    );
    seed_spl_token_account_if_missing(svm, vault_liquidity, liquidity_mint, vault, 0);
    seed_spl_token_account_if_missing(
        svm,
        reserve_collateral_supply,
        collateral_mint,
        lending_market_authority,
        0,
    );
    seed_spl_token_account_if_missing(
        svm,
        reserve_liquidity_supply,
        liquidity_mint,
        lending_market_authority,
        0,
    );

    MockKaminoReserveTokenAccounts {
        reserve,
        market,
        lending_market_authority,
        liquidity_mint,
        collateral_mint,
        reserve_liquidity_authority,
        collateral_mint_authority,
        vault_liquidity,
        reserve_collateral_supply,
        reserve_liquidity_supply,
    }
}

fn seed_mock_kamino_reserve_account(
    svm: &mut LiteSVM,
    reserve: Pubkey,
    market: Pubkey,
    liquidity_mint: Pubkey,
    collateral_mint: Pubkey,
    reserve_liquidity_supply: Pubkey,
    reserve_collateral_supply: Pubkey,
) {
    let mut data = vec![0; KAMINO_RESERVE_STATE_LEN];
    data[0..32].copy_from_slice(market.as_ref());
    data[32..64].copy_from_slice(liquidity_mint.as_ref());
    data[64..96].copy_from_slice(collateral_mint.as_ref());
    data[96..128].copy_from_slice(reserve_liquidity_supply.as_ref());
    data[128..160].copy_from_slice(reserve_collateral_supply.as_ref());

    svm.set_account(
        reserve,
        Account {
            lamports: LAMPORTS_PER_SOL,
            data,
            owner: solana_sdk::system_program::ID,
            executable: false,
            rent_epoch: 0,
        },
    )
    .expect("seed mock Kamino reserve account");
}

pub fn add_mock_jupiter_program(svm: &mut LiteSVM) -> std::io::Result<PathBuf> {
    add_mock_yield_protocols_program(svm, JUPITER_V6_PROGRAM_ID)
}

pub fn add_mock_kamino_lend_program(svm: &mut LiteSVM) -> std::io::Result<PathBuf> {
    add_mock_yield_protocols_program(svm, KAMINO_LEND_PROGRAM_ID)
}

pub fn add_loyal_hub_swap_program(svm: &mut LiteSVM) -> std::io::Result<PathBuf> {
    let path = loyal_hub_swap_program_so_path().ok_or_else(|| {
        std::io::Error::new(
            std::io::ErrorKind::NotFound,
            format!(
                "Loyal Hub swap SBF program not found; run `cargo build-sbf -- -p loyal-hub-swap-program` or set {LOYAL_HUB_SWAP_PROGRAM_SO_ENV}"
            ),
        )
    })?;
    let program = fs::read(&path)?;
    svm.add_program(LOYAL_HUB_SWAP_PROGRAM_ID, &program)
        .map_err(|error| {
            std::io::Error::other(format!("add Loyal Hub swap program failed: {error}"))
        })?;
    Ok(path)
}

pub fn add_mock_yield_protocols_program(
    svm: &mut LiteSVM,
    program_id: Pubkey,
) -> std::io::Result<PathBuf> {
    let path = mock_yield_protocols_program_so_path().ok_or_else(|| {
        std::io::Error::new(
            std::io::ErrorKind::NotFound,
            format!(
                "mock yield protocols SBF program not found; run `cargo build-sbf -- -p mock-yield-protocols-program` or set {MOCK_YIELD_PROTOCOLS_PROGRAM_SO_ENV}"
            ),
        )
    })?;
    let program = fs::read(&path)?;
    svm.add_program(program_id, &program).map_err(|error| {
        std::io::Error::other(format!("add mock yield protocols program failed: {error}"))
    })?;
    Ok(path)
}

pub fn mock_yield_protocols_program_so_path() -> Option<PathBuf> {
    if let Some(path) = env::var_os(MOCK_YIELD_PROTOCOLS_PROGRAM_SO_ENV).map(PathBuf::from) {
        if path.exists() {
            return Some(path);
        }
    }

    let manifest_dir = PathBuf::from(env!("CARGO_MANIFEST_DIR"));
    for path in [
        manifest_dir
            .join("../../target/deploy")
            .join(MOCK_YIELD_PROTOCOLS_PROGRAM_SO),
        PathBuf::from("target/deploy").join(MOCK_YIELD_PROTOCOLS_PROGRAM_SO),
    ] {
        if path.exists() {
            return Some(path);
        }
    }

    None
}

pub fn loyal_hub_swap_program_so_path() -> Option<PathBuf> {
    if let Some(path) = env::var_os(LOYAL_HUB_SWAP_PROGRAM_SO_ENV).map(PathBuf::from) {
        if path.exists() {
            return Some(path);
        }
    }

    let manifest_dir = PathBuf::from(env!("CARGO_MANIFEST_DIR"));
    for path in [
        manifest_dir
            .join("../../target/deploy")
            .join(LOYAL_HUB_SWAP_PROGRAM_SO),
        PathBuf::from("target/deploy").join(LOYAL_HUB_SWAP_PROGRAM_SO),
    ] {
        if path.exists() {
            return Some(path);
        }
    }

    None
}

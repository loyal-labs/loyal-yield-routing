#![cfg(kani)]
#![allow(dead_code)]

use core::mem::ManuallyDrop;

use pinocchio::account_info::AccountInfo;

const MINT_DECIMALS_OFFSET: usize = 44;
const MINT_INITIALIZED_OFFSET: usize = 45;
const MINT_DATA_LEN: usize = 82;
const TOKEN_MINT_OFFSET: usize = 0;
const TOKEN_OWNER_OFFSET: usize = 32;
const TOKEN_AMOUNT_OFFSET: usize = 64;
const TOKEN_STATE_OFFSET: usize = 108;
const TOKEN_DATA_LEN: usize = 165;
const TOKEN_STATE_INITIALIZED: u8 = 1;

#[repr(C)]
struct AccountLayout {
    borrow_state: u8,
    is_signer: u8,
    is_writable: u8,
    executable: u8,
    original_data_len: u32,
    key: [u8; 32],
    owner: [u8; 32],
    lamports: u64,
    data_len: u64,
}

const _: () = assert!(core::mem::size_of::<AccountLayout>() == 88);

#[repr(C, align(8))]
struct StackAccount<const DATA_LEN: usize> {
    hdr: AccountLayout,
    data: [u8; DATA_LEN],
}

fn build_account<const DATA_LEN: usize>(
    key: [u8; 32],
    owner: [u8; 32],
    is_signer: bool,
    is_writable: bool,
    data: [u8; DATA_LEN],
) -> StackAccount<DATA_LEN> {
    StackAccount {
        hdr: AccountLayout {
            borrow_state: 0,
            is_signer: u8::from(is_signer),
            is_writable: u8::from(is_writable),
            executable: 0,
            original_data_len: 0,
            key,
            owner,
            lamports: 0,
            data_len: DATA_LEN as u64,
        },
        data,
    }
}

unsafe fn account_info_from_stack<const DATA_LEN: usize>(
    stack: &mut StackAccount<DATA_LEN>,
) -> AccountInfo {
    let hdr_ptr: *mut AccountLayout = &mut stack.hdr;
    core::mem::transmute::<*mut AccountLayout, AccountInfo>(hdr_ptr)
}

fn write_pubkey(data: &mut [u8], offset: usize, key: &[u8; 32]) {
    data[offset..offset + 32].copy_from_slice(key);
}

fn read_u16(data: &[u8], offset: usize) -> u16 {
    u16::from_le_bytes(data[offset..offset + 2].try_into().unwrap())
}

fn read_u64(data: &[u8], offset: usize) -> u64 {
    u64::from_le_bytes(data[offset..offset + 8].try_into().unwrap())
}

fn config_data(
    admin: [u8; 32],
    hub_authorizer: [u8; 32],
    inventory_rebalancer: [u8; 32],
    max_fee_bps: u16,
    paused: bool,
    lane_count: u8,
    allowed_mints: &[[u8; 32]],
) -> [u8; crate::HUB_CONFIG_SPACE] {
    kani::assume(lane_count > 0);
    kani::assume(!allowed_mints.is_empty());
    kani::assume(allowed_mints.len() <= crate::MAX_ALLOWED_MINTS);

    let mut data = [0u8; crate::HUB_CONFIG_SPACE];
    data[loyal_hub_abi::config_account::MAGIC_OFFSET
        ..loyal_hub_abi::config_account::MAGIC_OFFSET + loyal_hub_abi::config_account::MAGIC_LEN]
        .copy_from_slice(crate::CONFIG_MAGIC);
    write_pubkey(
        &mut data,
        loyal_hub_abi::config_account::ADMIN_OFFSET,
        &admin,
    );
    write_pubkey(
        &mut data,
        loyal_hub_abi::config_account::HUB_AUTHORIZER_OFFSET,
        &hub_authorizer,
    );
    write_pubkey(
        &mut data,
        loyal_hub_abi::config_account::INVENTORY_REBALANCER_OFFSET,
        &inventory_rebalancer,
    );
    data[loyal_hub_abi::config_account::MAX_FEE_BPS_OFFSET
        ..loyal_hub_abi::config_account::MAX_FEE_BPS_OFFSET + 2]
        .copy_from_slice(&max_fee_bps.to_le_bytes());
    data[loyal_hub_abi::config_account::PAUSED_OFFSET] = u8::from(paused);
    data[loyal_hub_abi::config_account::LANE_COUNT_OFFSET] = lane_count;
    data[loyal_hub_abi::config_account::MINT_COUNT_OFFSET] = allowed_mints.len() as u8;
    for (index, mint) in allowed_mints.iter().enumerate() {
        let offset = loyal_hub_abi::config_account::ALLOWED_MINT_OFFSET
            + (index * loyal_hub_abi::config_account::ALLOWED_MINT_ITEM_LEN);
        write_pubkey(&mut data, offset, mint);
    }
    data
}

fn valid_config_data(admin: [u8; 32], paused: bool) -> [u8; crate::HUB_CONFIG_SPACE] {
    config_data(admin, [8u8; 32], [9u8; 32], 25, paused, 2, &[[3u8; 32]])
}

fn mint_data(decimals: u8) -> [u8; MINT_DATA_LEN] {
    let mut data = [0u8; MINT_DATA_LEN];
    data[MINT_DECIMALS_OFFSET] = decimals;
    data[MINT_INITIALIZED_OFFSET] = 1;
    data
}

fn token_account_data(mint: [u8; 32], owner: [u8; 32], amount: u64) -> [u8; TOKEN_DATA_LEN] {
    let mut data = [0u8; TOKEN_DATA_LEN];
    write_pubkey(&mut data, TOKEN_MINT_OFFSET, &mint);
    write_pubkey(&mut data, TOKEN_OWNER_OFFSET, &owner);
    data[TOKEN_AMOUNT_OFFSET..TOKEN_AMOUNT_OFFSET + 8].copy_from_slice(&amount.to_le_bytes());
    data[TOKEN_STATE_OFFSET] = TOKEN_STATE_INITIALIZED;
    data
}

fn token_amount<const DATA_LEN: usize>(account: &StackAccount<DATA_LEN>) -> u64 {
    read_u64(&account.data, TOKEN_AMOUNT_OFFSET)
}

fn assume_distinct_pubkeys(keys: &[[u8; 32]]) {
    for (index, left) in keys.iter().enumerate() {
        for right in keys.iter().skip(index + 1) {
            kani::assume(left != right);
        }
    }
}

macro_rules! rebalance_batch_projection_proof {
    (
        $fn_name:ident,
        $transfer_count:literal,
        $lane_count:literal,
        [$((
            $index:literal,
            $from_lane:literal,
            $to_lane:literal,
            $amount:ident,
            $source_start:ident,
            $destination_start:ident,
            $source_authority_key:ident,
            $destination_authority_key:ident,
            $source_inventory_key:ident,
            $destination_inventory_key:ident,
            $source_authority:ident,
            $source_inventory:ident,
            $destination_inventory:ident
        )),+ $(,)?]
    ) => {
        #[kani::proof]
        #[kani::unwind(34)]
        fn $fn_name() {
            let program_id = [42u8; 32];
            let inventory_rebalancer_key = [9u8; 32];
            let mint_key = [3u8; 32];
            $(
                let $amount: u64 = kani::any();
                let $source_start: u64 = kani::any();
                let $destination_start: u64 = kani::any();
                kani::assume($amount > 0);
                kani::assume($source_start >= $amount);
                kani::assume($destination_start <= u64::MAX - $amount);
            )+

            let config_key = crate::derive_config(&program_id).0;
            $(
                let $source_authority_key =
                    crate::derive_hub_authority(&program_id, $from_lane).0;
                let $destination_authority_key =
                    crate::derive_hub_authority(&program_id, $to_lane).0;
                let $source_inventory_key =
                    crate::derive_inventory_account(&program_id, &mint_key, $from_lane);
                let $destination_inventory_key =
                    crate::derive_inventory_account(&program_id, &mint_key, $to_lane);
            )+
            assume_distinct_pubkeys(&[
                $(
                    $source_inventory_key,
                    $destination_inventory_key,
                )+
            ]);

            let config_data = config_data(
                [7u8; 32],
                [8u8; 32],
                inventory_rebalancer_key,
                25,
                false,
                $lane_count,
                &[mint_key],
            );
            let mut config = build_account(config_key, program_id, false, false, config_data);
            let mut inventory_rebalancer =
                build_account(inventory_rebalancer_key, [0u8; 32], true, false, []);
            let mut token_program = build_account(crate::SPL_TOKEN_ID, [0u8; 32], false, false, []);
            let mut mint = build_account(mint_key, crate::SPL_TOKEN_ID, false, false, mint_data(6));
            $(
                let mut $source_authority =
                    build_account($source_authority_key, [0u8; 32], false, false, []);
                let mut $source_inventory = build_account(
                    $source_inventory_key,
                    crate::SPL_TOKEN_ID,
                    false,
                    true,
                    token_account_data(mint_key, $source_authority_key, $source_start),
                );
                let mut $destination_inventory = build_account(
                    $destination_inventory_key,
                    crate::SPL_TOKEN_ID,
                    false,
                    true,
                    token_account_data(mint_key, $destination_authority_key, $destination_start),
                );
            )+

            let accounts: [ManuallyDrop<AccountInfo>; 4 + (3 * $transfer_count)] = unsafe {
                [
                    ManuallyDrop::new(account_info_from_stack(&mut config)),
                    ManuallyDrop::new(account_info_from_stack(&mut inventory_rebalancer)),
                    ManuallyDrop::new(account_info_from_stack(&mut token_program)),
                    ManuallyDrop::new(account_info_from_stack(&mut mint)),
                    $(
                        ManuallyDrop::new(account_info_from_stack(&mut $source_authority)),
                        ManuallyDrop::new(account_info_from_stack(&mut $source_inventory)),
                        ManuallyDrop::new(account_info_from_stack(&mut $destination_inventory)),
                    )+
                ]
            };
            let accounts_slice = unsafe {
                core::slice::from_raw_parts(
                    &accounts as *const _ as *const AccountInfo,
                    4 + (3 * $transfer_count),
                )
            };

            let mut instruction_data = [0u8; 2 + (10 * $transfer_count)];
            instruction_data[0] = crate::REBALANCE_INVENTORY;
            instruction_data[1] = $transfer_count;
            $(
                let offset: usize = 2 + ($index * 10);
                instruction_data[offset] = $from_lane;
                instruction_data[offset + 1] = $to_lane;
                instruction_data[offset + 2..offset + 10]
                    .copy_from_slice(&$amount.to_le_bytes());
            )+
            let result = crate::process_instruction(&program_id, accounts_slice, &instruction_data);

            assert!(result.is_ok());
            $(
                assert_eq!(token_amount(&$source_inventory), $source_start - $amount);
                assert_eq!(
                    token_amount(&$destination_inventory),
                    $destination_start + $amount
                );
            )+
        }
    };
}

#[kani::proof]
#[kani::unwind(34)]
fn verify_live_set_paused_updates_config() {
    let program_id = [42u8; 32];
    let admin_key = [7u8; 32];
    let paused: bool = kani::any();
    let config_key = crate::derive_config(&program_id).0;
    let config_data = valid_config_data(admin_key, !paused);

    let mut config = build_account(config_key, program_id, false, true, config_data);
    let mut admin = build_account(admin_key, [0u8; 32], true, false, []);

    let accounts: [ManuallyDrop<AccountInfo>; 2] = unsafe {
        [
            ManuallyDrop::new(account_info_from_stack(&mut config)),
            ManuallyDrop::new(account_info_from_stack(&mut admin)),
        ]
    };
    let accounts_slice =
        unsafe { core::slice::from_raw_parts(&accounts as *const _ as *const AccountInfo, 2) };

    let instruction_data = [crate::SET_PAUSED, u8::from(paused)];
    let result = crate::process_instruction(&program_id, accounts_slice, &instruction_data);

    assert!(result.is_ok());
    assert_eq!(
        config.data[loyal_hub_abi::config_account::PAUSED_OFFSET],
        u8::from(paused)
    );
}

#[kani::proof]
#[kani::unwind(34)]
fn verify_live_set_max_fee_updates_config() {
    let program_id = [42u8; 32];
    let admin_key = [7u8; 32];
    let max_fee_bps: u16 = kani::any();
    kani::assume(max_fee_bps <= loyal_hub_abi::MAX_FEE_BPS as u16);
    let config_key = crate::derive_config(&program_id).0;
    let config_data = valid_config_data(admin_key, false);

    let mut config = build_account(config_key, program_id, false, true, config_data);
    let mut admin = build_account(admin_key, [0u8; 32], true, false, []);

    let accounts: [ManuallyDrop<AccountInfo>; 2] = unsafe {
        [
            ManuallyDrop::new(account_info_from_stack(&mut config)),
            ManuallyDrop::new(account_info_from_stack(&mut admin)),
        ]
    };
    let accounts_slice =
        unsafe { core::slice::from_raw_parts(&accounts as *const _ as *const AccountInfo, 2) };

    let mut instruction_data = [0u8; 3];
    instruction_data[0] = crate::SET_MAX_FEE;
    instruction_data[1..3].copy_from_slice(&max_fee_bps.to_le_bytes());
    let result = crate::process_instruction(&program_id, accounts_slice, &instruction_data);

    assert!(result.is_ok());
    assert_eq!(
        read_u16(
            &config.data,
            loyal_hub_abi::config_account::MAX_FEE_BPS_OFFSET
        ),
        max_fee_bps
    );
}

#[kani::proof]
#[kani::unwind(34)]
fn verify_live_withdraw_inventory_moves_projected_token_balances() {
    let program_id = [42u8; 32];
    let admin_key = [7u8; 32];
    let mint_key = [3u8; 32];
    let destination_key = [12u8; 32];
    let destination_owner = [13u8; 32];
    let lane_id = 0u8;
    let amount: u64 = kani::any();
    let source_start: u64 = kani::any();
    let destination_start: u64 = kani::any();
    kani::assume(amount > 0);
    kani::assume(source_start >= amount);
    kani::assume(destination_start <= u64::MAX - amount);

    let config_key = crate::derive_config(&program_id).0;
    let hub_authority_key = crate::derive_hub_authority(&program_id, lane_id).0;
    let hub_source_key = crate::derive_inventory_account(&program_id, &mint_key, lane_id);
    kani::assume(hub_source_key != destination_key);

    let config_data = valid_config_data(admin_key, false);
    let mut config = build_account(config_key, program_id, false, false, config_data);
    let mut admin = build_account(admin_key, [0u8; 32], true, false, []);
    let mut hub_source = build_account(
        hub_source_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(mint_key, hub_authority_key, source_start),
    );
    let mut destination = build_account(
        destination_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(mint_key, destination_owner, destination_start),
    );
    let mut mint = build_account(mint_key, crate::SPL_TOKEN_ID, false, false, mint_data(6));
    let mut hub_authority = build_account(hub_authority_key, [0u8; 32], false, false, []);
    let mut token_program = build_account(crate::SPL_TOKEN_ID, [0u8; 32], false, false, []);

    let accounts: [ManuallyDrop<AccountInfo>; 7] = unsafe {
        [
            ManuallyDrop::new(account_info_from_stack(&mut config)),
            ManuallyDrop::new(account_info_from_stack(&mut admin)),
            ManuallyDrop::new(account_info_from_stack(&mut hub_source)),
            ManuallyDrop::new(account_info_from_stack(&mut destination)),
            ManuallyDrop::new(account_info_from_stack(&mut mint)),
            ManuallyDrop::new(account_info_from_stack(&mut hub_authority)),
            ManuallyDrop::new(account_info_from_stack(&mut token_program)),
        ]
    };
    let accounts_slice =
        unsafe { core::slice::from_raw_parts(&accounts as *const _ as *const AccountInfo, 7) };

    let mut instruction_data = [0u8; 10];
    instruction_data[0] = crate::WITHDRAW_INVENTORY;
    instruction_data[1..9].copy_from_slice(&amount.to_le_bytes());
    instruction_data[9] = lane_id;
    let result = crate::process_instruction(&program_id, accounts_slice, &instruction_data);

    assert!(result.is_ok());
    assert_eq!(token_amount(&hub_source), source_start - amount);
    assert_eq!(token_amount(&destination), destination_start + amount);
}

#[kani::proof]
#[kani::unwind(34)]
fn verify_live_rebalance_inventory_moves_one_projected_token_balance() {
    let program_id = [42u8; 32];
    let inventory_rebalancer_key = [9u8; 32];
    let mint_key = [3u8; 32];
    let from_lane_id = 0u8;
    let to_lane_id = 1u8;
    let amount: u64 = kani::any();
    let source_start: u64 = kani::any();
    let destination_start: u64 = kani::any();
    kani::assume(amount > 0);
    kani::assume(source_start >= amount);
    kani::assume(destination_start <= u64::MAX - amount);

    let config_key = crate::derive_config(&program_id).0;
    let source_authority_key = crate::derive_hub_authority(&program_id, from_lane_id).0;
    let destination_authority_key = crate::derive_hub_authority(&program_id, to_lane_id).0;
    let source_inventory_key =
        crate::derive_inventory_account(&program_id, &mint_key, from_lane_id);
    let destination_inventory_key =
        crate::derive_inventory_account(&program_id, &mint_key, to_lane_id);
    kani::assume(source_inventory_key != destination_inventory_key);

    let config_data = valid_config_data([7u8; 32], false);
    let mut config = build_account(config_key, program_id, false, false, config_data);
    let mut inventory_rebalancer =
        build_account(inventory_rebalancer_key, [0u8; 32], true, false, []);
    let mut token_program = build_account(crate::SPL_TOKEN_ID, [0u8; 32], false, false, []);
    let mut mint = build_account(mint_key, crate::SPL_TOKEN_ID, false, false, mint_data(6));
    let mut source_authority = build_account(source_authority_key, [0u8; 32], false, false, []);
    let mut source_inventory = build_account(
        source_inventory_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(mint_key, source_authority_key, source_start),
    );
    let mut destination_inventory = build_account(
        destination_inventory_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(mint_key, destination_authority_key, destination_start),
    );

    let accounts: [ManuallyDrop<AccountInfo>; 7] = unsafe {
        [
            ManuallyDrop::new(account_info_from_stack(&mut config)),
            ManuallyDrop::new(account_info_from_stack(&mut inventory_rebalancer)),
            ManuallyDrop::new(account_info_from_stack(&mut token_program)),
            ManuallyDrop::new(account_info_from_stack(&mut mint)),
            ManuallyDrop::new(account_info_from_stack(&mut source_authority)),
            ManuallyDrop::new(account_info_from_stack(&mut source_inventory)),
            ManuallyDrop::new(account_info_from_stack(&mut destination_inventory)),
        ]
    };
    let accounts_slice =
        unsafe { core::slice::from_raw_parts(&accounts as *const _ as *const AccountInfo, 7) };

    let mut instruction_data = [0u8; 12];
    instruction_data[0] = crate::REBALANCE_INVENTORY;
    instruction_data[1] = 1;
    instruction_data[2] = from_lane_id;
    instruction_data[3] = to_lane_id;
    instruction_data[4..12].copy_from_slice(&amount.to_le_bytes());
    let result = crate::process_instruction(&program_id, accounts_slice, &instruction_data);

    assert!(result.is_ok());
    assert_eq!(token_amount(&source_inventory), source_start - amount);
    assert_eq!(
        token_amount(&destination_inventory),
        destination_start + amount
    );
}

#[kani::proof]
#[kani::unwind(34)]
fn verify_live_rebalance_inventory_moves_two_projected_token_balances() {
    let program_id = [42u8; 32];
    let inventory_rebalancer_key = [9u8; 32];
    let mint_key = [3u8; 32];
    let amount_a: u64 = kani::any();
    let amount_b: u64 = kani::any();
    let source_a_start: u64 = kani::any();
    let destination_a_start: u64 = kani::any();
    let source_b_start: u64 = kani::any();
    let destination_b_start: u64 = kani::any();
    kani::assume(amount_a > 0);
    kani::assume(amount_b > 0);
    kani::assume(source_a_start >= amount_a);
    kani::assume(destination_a_start <= u64::MAX - amount_a);
    kani::assume(source_b_start >= amount_b);
    kani::assume(destination_b_start <= u64::MAX - amount_b);

    let from_lane_a = 0u8;
    let to_lane_a = 1u8;
    let from_lane_b = 2u8;
    let to_lane_b = 3u8;
    let config_key = crate::derive_config(&program_id).0;
    let source_authority_a_key = crate::derive_hub_authority(&program_id, from_lane_a).0;
    let destination_authority_a_key = crate::derive_hub_authority(&program_id, to_lane_a).0;
    let source_authority_b_key = crate::derive_hub_authority(&program_id, from_lane_b).0;
    let destination_authority_b_key = crate::derive_hub_authority(&program_id, to_lane_b).0;
    let source_inventory_a_key =
        crate::derive_inventory_account(&program_id, &mint_key, from_lane_a);
    let destination_inventory_a_key =
        crate::derive_inventory_account(&program_id, &mint_key, to_lane_a);
    let source_inventory_b_key =
        crate::derive_inventory_account(&program_id, &mint_key, from_lane_b);
    let destination_inventory_b_key =
        crate::derive_inventory_account(&program_id, &mint_key, to_lane_b);
    kani::assume(source_inventory_a_key != destination_inventory_a_key);
    kani::assume(source_inventory_a_key != source_inventory_b_key);
    kani::assume(source_inventory_a_key != destination_inventory_b_key);
    kani::assume(destination_inventory_a_key != source_inventory_b_key);
    kani::assume(destination_inventory_a_key != destination_inventory_b_key);
    kani::assume(source_inventory_b_key != destination_inventory_b_key);

    let config_data = config_data(
        [7u8; 32],
        [8u8; 32],
        inventory_rebalancer_key,
        25,
        false,
        4,
        &[mint_key],
    );
    let mut config = build_account(config_key, program_id, false, false, config_data);
    let mut inventory_rebalancer =
        build_account(inventory_rebalancer_key, [0u8; 32], true, false, []);
    let mut token_program = build_account(crate::SPL_TOKEN_ID, [0u8; 32], false, false, []);
    let mut mint = build_account(mint_key, crate::SPL_TOKEN_ID, false, false, mint_data(6));
    let mut source_authority_a = build_account(source_authority_a_key, [0u8; 32], false, false, []);
    let mut source_inventory_a = build_account(
        source_inventory_a_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(mint_key, source_authority_a_key, source_a_start),
    );
    let mut destination_inventory_a = build_account(
        destination_inventory_a_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(mint_key, destination_authority_a_key, destination_a_start),
    );
    let mut source_authority_b = build_account(source_authority_b_key, [0u8; 32], false, false, []);
    let mut source_inventory_b = build_account(
        source_inventory_b_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(mint_key, source_authority_b_key, source_b_start),
    );
    let mut destination_inventory_b = build_account(
        destination_inventory_b_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(mint_key, destination_authority_b_key, destination_b_start),
    );

    let accounts: [ManuallyDrop<AccountInfo>; 10] = unsafe {
        [
            ManuallyDrop::new(account_info_from_stack(&mut config)),
            ManuallyDrop::new(account_info_from_stack(&mut inventory_rebalancer)),
            ManuallyDrop::new(account_info_from_stack(&mut token_program)),
            ManuallyDrop::new(account_info_from_stack(&mut mint)),
            ManuallyDrop::new(account_info_from_stack(&mut source_authority_a)),
            ManuallyDrop::new(account_info_from_stack(&mut source_inventory_a)),
            ManuallyDrop::new(account_info_from_stack(&mut destination_inventory_a)),
            ManuallyDrop::new(account_info_from_stack(&mut source_authority_b)),
            ManuallyDrop::new(account_info_from_stack(&mut source_inventory_b)),
            ManuallyDrop::new(account_info_from_stack(&mut destination_inventory_b)),
        ]
    };
    let accounts_slice =
        unsafe { core::slice::from_raw_parts(&accounts as *const _ as *const AccountInfo, 10) };

    let mut instruction_data = [0u8; 22];
    instruction_data[0] = crate::REBALANCE_INVENTORY;
    instruction_data[1] = 2;
    instruction_data[2] = from_lane_a;
    instruction_data[3] = to_lane_a;
    instruction_data[4..12].copy_from_slice(&amount_a.to_le_bytes());
    instruction_data[12] = from_lane_b;
    instruction_data[13] = to_lane_b;
    instruction_data[14..22].copy_from_slice(&amount_b.to_le_bytes());
    let result = crate::process_instruction(&program_id, accounts_slice, &instruction_data);

    assert!(result.is_ok());
    assert_eq!(token_amount(&source_inventory_a), source_a_start - amount_a);
    assert_eq!(
        token_amount(&destination_inventory_a),
        destination_a_start + amount_a
    );
    assert_eq!(token_amount(&source_inventory_b), source_b_start - amount_b);
    assert_eq!(
        token_amount(&destination_inventory_b),
        destination_b_start + amount_b
    );
}

#[kani::proof]
#[kani::unwind(34)]
fn verify_live_rebalance_inventory_moves_four_projected_token_balances() {
    let program_id = [42u8; 32];
    let inventory_rebalancer_key = [9u8; 32];
    let mint_key = [3u8; 32];
    let amount_0: u64 = kani::any();
    let amount_1: u64 = kani::any();
    let amount_2: u64 = kani::any();
    let amount_3: u64 = kani::any();
    let source_0_start: u64 = kani::any();
    let source_1_start: u64 = kani::any();
    let source_2_start: u64 = kani::any();
    let source_3_start: u64 = kani::any();
    let destination_0_start: u64 = kani::any();
    let destination_1_start: u64 = kani::any();
    let destination_2_start: u64 = kani::any();
    let destination_3_start: u64 = kani::any();
    kani::assume(amount_0 > 0);
    kani::assume(amount_1 > 0);
    kani::assume(amount_2 > 0);
    kani::assume(amount_3 > 0);
    kani::assume(source_0_start >= amount_0);
    kani::assume(source_1_start >= amount_1);
    kani::assume(source_2_start >= amount_2);
    kani::assume(source_3_start >= amount_3);
    kani::assume(destination_0_start <= u64::MAX - amount_0);
    kani::assume(destination_1_start <= u64::MAX - amount_1);
    kani::assume(destination_2_start <= u64::MAX - amount_2);
    kani::assume(destination_3_start <= u64::MAX - amount_3);

    let config_key = crate::derive_config(&program_id).0;
    let source_authority_0_key = crate::derive_hub_authority(&program_id, 0).0;
    let destination_authority_0_key = crate::derive_hub_authority(&program_id, 1).0;
    let source_authority_1_key = crate::derive_hub_authority(&program_id, 2).0;
    let destination_authority_1_key = crate::derive_hub_authority(&program_id, 3).0;
    let source_authority_2_key = crate::derive_hub_authority(&program_id, 4).0;
    let destination_authority_2_key = crate::derive_hub_authority(&program_id, 5).0;
    let source_authority_3_key = crate::derive_hub_authority(&program_id, 6).0;
    let destination_authority_3_key = crate::derive_hub_authority(&program_id, 7).0;
    let source_inventory_0_key = crate::derive_inventory_account(&program_id, &mint_key, 0);
    let destination_inventory_0_key = crate::derive_inventory_account(&program_id, &mint_key, 1);
    let source_inventory_1_key = crate::derive_inventory_account(&program_id, &mint_key, 2);
    let destination_inventory_1_key = crate::derive_inventory_account(&program_id, &mint_key, 3);
    let source_inventory_2_key = crate::derive_inventory_account(&program_id, &mint_key, 4);
    let destination_inventory_2_key = crate::derive_inventory_account(&program_id, &mint_key, 5);
    let source_inventory_3_key = crate::derive_inventory_account(&program_id, &mint_key, 6);
    let destination_inventory_3_key = crate::derive_inventory_account(&program_id, &mint_key, 7);
    assume_distinct_pubkeys(&[
        source_inventory_0_key,
        destination_inventory_0_key,
        source_inventory_1_key,
        destination_inventory_1_key,
        source_inventory_2_key,
        destination_inventory_2_key,
        source_inventory_3_key,
        destination_inventory_3_key,
    ]);

    let config_data = config_data(
        [7u8; 32],
        [8u8; 32],
        inventory_rebalancer_key,
        25,
        false,
        8,
        &[mint_key],
    );
    let mut config = build_account(config_key, program_id, false, false, config_data);
    let mut inventory_rebalancer =
        build_account(inventory_rebalancer_key, [0u8; 32], true, false, []);
    let mut token_program = build_account(crate::SPL_TOKEN_ID, [0u8; 32], false, false, []);
    let mut mint = build_account(mint_key, crate::SPL_TOKEN_ID, false, false, mint_data(6));
    let mut source_authority_0 = build_account(source_authority_0_key, [0u8; 32], false, false, []);
    let mut source_inventory_0 = build_account(
        source_inventory_0_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(mint_key, source_authority_0_key, source_0_start),
    );
    let mut destination_inventory_0 = build_account(
        destination_inventory_0_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(mint_key, destination_authority_0_key, destination_0_start),
    );
    let mut source_authority_1 = build_account(source_authority_1_key, [0u8; 32], false, false, []);
    let mut source_inventory_1 = build_account(
        source_inventory_1_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(mint_key, source_authority_1_key, source_1_start),
    );
    let mut destination_inventory_1 = build_account(
        destination_inventory_1_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(mint_key, destination_authority_1_key, destination_1_start),
    );
    let mut source_authority_2 = build_account(source_authority_2_key, [0u8; 32], false, false, []);
    let mut source_inventory_2 = build_account(
        source_inventory_2_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(mint_key, source_authority_2_key, source_2_start),
    );
    let mut destination_inventory_2 = build_account(
        destination_inventory_2_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(mint_key, destination_authority_2_key, destination_2_start),
    );
    let mut source_authority_3 = build_account(source_authority_3_key, [0u8; 32], false, false, []);
    let mut source_inventory_3 = build_account(
        source_inventory_3_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(mint_key, source_authority_3_key, source_3_start),
    );
    let mut destination_inventory_3 = build_account(
        destination_inventory_3_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(mint_key, destination_authority_3_key, destination_3_start),
    );

    let accounts: [ManuallyDrop<AccountInfo>; 16] = unsafe {
        [
            ManuallyDrop::new(account_info_from_stack(&mut config)),
            ManuallyDrop::new(account_info_from_stack(&mut inventory_rebalancer)),
            ManuallyDrop::new(account_info_from_stack(&mut token_program)),
            ManuallyDrop::new(account_info_from_stack(&mut mint)),
            ManuallyDrop::new(account_info_from_stack(&mut source_authority_0)),
            ManuallyDrop::new(account_info_from_stack(&mut source_inventory_0)),
            ManuallyDrop::new(account_info_from_stack(&mut destination_inventory_0)),
            ManuallyDrop::new(account_info_from_stack(&mut source_authority_1)),
            ManuallyDrop::new(account_info_from_stack(&mut source_inventory_1)),
            ManuallyDrop::new(account_info_from_stack(&mut destination_inventory_1)),
            ManuallyDrop::new(account_info_from_stack(&mut source_authority_2)),
            ManuallyDrop::new(account_info_from_stack(&mut source_inventory_2)),
            ManuallyDrop::new(account_info_from_stack(&mut destination_inventory_2)),
            ManuallyDrop::new(account_info_from_stack(&mut source_authority_3)),
            ManuallyDrop::new(account_info_from_stack(&mut source_inventory_3)),
            ManuallyDrop::new(account_info_from_stack(&mut destination_inventory_3)),
        ]
    };
    let accounts_slice =
        unsafe { core::slice::from_raw_parts(&accounts as *const _ as *const AccountInfo, 16) };

    let mut instruction_data = [0u8; 42];
    instruction_data[0] = crate::REBALANCE_INVENTORY;
    instruction_data[1] = 4;
    instruction_data[2] = 0;
    instruction_data[3] = 1;
    instruction_data[4..12].copy_from_slice(&amount_0.to_le_bytes());
    instruction_data[12] = 2;
    instruction_data[13] = 3;
    instruction_data[14..22].copy_from_slice(&amount_1.to_le_bytes());
    instruction_data[22] = 4;
    instruction_data[23] = 5;
    instruction_data[24..32].copy_from_slice(&amount_2.to_le_bytes());
    instruction_data[32] = 6;
    instruction_data[33] = 7;
    instruction_data[34..42].copy_from_slice(&amount_3.to_le_bytes());
    let result = crate::process_instruction(&program_id, accounts_slice, &instruction_data);

    assert!(result.is_ok());
    assert_eq!(token_amount(&source_inventory_0), source_0_start - amount_0);
    assert_eq!(
        token_amount(&destination_inventory_0),
        destination_0_start + amount_0
    );
    assert_eq!(token_amount(&source_inventory_1), source_1_start - amount_1);
    assert_eq!(
        token_amount(&destination_inventory_1),
        destination_1_start + amount_1
    );
    assert_eq!(token_amount(&source_inventory_2), source_2_start - amount_2);
    assert_eq!(
        token_amount(&destination_inventory_2),
        destination_2_start + amount_2
    );
    assert_eq!(token_amount(&source_inventory_3), source_3_start - amount_3);
    assert_eq!(
        token_amount(&destination_inventory_3),
        destination_3_start + amount_3
    );
}

rebalance_batch_projection_proof!(
    verify_live_rebalance_inventory_moves_eight_projected_token_balances,
    8,
    16,
    [
        (
            0,
            0,
            1,
            amount_0,
            source_0_start,
            destination_0_start,
            source_authority_0_key,
            destination_authority_0_key,
            source_inventory_0_key,
            destination_inventory_0_key,
            source_authority_0,
            source_inventory_0,
            destination_inventory_0
        ),
        (
            1,
            2,
            3,
            amount_1,
            source_1_start,
            destination_1_start,
            source_authority_1_key,
            destination_authority_1_key,
            source_inventory_1_key,
            destination_inventory_1_key,
            source_authority_1,
            source_inventory_1,
            destination_inventory_1
        ),
        (
            2,
            4,
            5,
            amount_2,
            source_2_start,
            destination_2_start,
            source_authority_2_key,
            destination_authority_2_key,
            source_inventory_2_key,
            destination_inventory_2_key,
            source_authority_2,
            source_inventory_2,
            destination_inventory_2
        ),
        (
            3,
            6,
            7,
            amount_3,
            source_3_start,
            destination_3_start,
            source_authority_3_key,
            destination_authority_3_key,
            source_inventory_3_key,
            destination_inventory_3_key,
            source_authority_3,
            source_inventory_3,
            destination_inventory_3
        ),
        (
            4,
            8,
            9,
            amount_4,
            source_4_start,
            destination_4_start,
            source_authority_4_key,
            destination_authority_4_key,
            source_inventory_4_key,
            destination_inventory_4_key,
            source_authority_4,
            source_inventory_4,
            destination_inventory_4
        ),
        (
            5,
            10,
            11,
            amount_5,
            source_5_start,
            destination_5_start,
            source_authority_5_key,
            destination_authority_5_key,
            source_inventory_5_key,
            destination_inventory_5_key,
            source_authority_5,
            source_inventory_5,
            destination_inventory_5
        ),
        (
            6,
            12,
            13,
            amount_6,
            source_6_start,
            destination_6_start,
            source_authority_6_key,
            destination_authority_6_key,
            source_inventory_6_key,
            destination_inventory_6_key,
            source_authority_6,
            source_inventory_6,
            destination_inventory_6
        ),
        (
            7,
            14,
            15,
            amount_7,
            source_7_start,
            destination_7_start,
            source_authority_7_key,
            destination_authority_7_key,
            source_inventory_7_key,
            destination_inventory_7_key,
            source_authority_7,
            source_inventory_7,
            destination_inventory_7
        ),
    ]
);

rebalance_batch_projection_proof!(
    verify_live_rebalance_inventory_moves_max_projected_token_balances,
    16,
    16,
    [
        (
            0,
            0,
            1,
            amount_0,
            source_0_start,
            destination_0_start,
            source_authority_0_key,
            destination_authority_0_key,
            source_inventory_0_key,
            destination_inventory_0_key,
            source_authority_0,
            source_inventory_0,
            destination_inventory_0
        ),
        (
            1,
            1,
            2,
            amount_1,
            source_1_start,
            destination_1_start,
            source_authority_1_key,
            destination_authority_1_key,
            source_inventory_1_key,
            destination_inventory_1_key,
            source_authority_1,
            source_inventory_1,
            destination_inventory_1
        ),
        (
            2,
            2,
            3,
            amount_2,
            source_2_start,
            destination_2_start,
            source_authority_2_key,
            destination_authority_2_key,
            source_inventory_2_key,
            destination_inventory_2_key,
            source_authority_2,
            source_inventory_2,
            destination_inventory_2
        ),
        (
            3,
            3,
            4,
            amount_3,
            source_3_start,
            destination_3_start,
            source_authority_3_key,
            destination_authority_3_key,
            source_inventory_3_key,
            destination_inventory_3_key,
            source_authority_3,
            source_inventory_3,
            destination_inventory_3
        ),
        (
            4,
            4,
            5,
            amount_4,
            source_4_start,
            destination_4_start,
            source_authority_4_key,
            destination_authority_4_key,
            source_inventory_4_key,
            destination_inventory_4_key,
            source_authority_4,
            source_inventory_4,
            destination_inventory_4
        ),
        (
            5,
            5,
            6,
            amount_5,
            source_5_start,
            destination_5_start,
            source_authority_5_key,
            destination_authority_5_key,
            source_inventory_5_key,
            destination_inventory_5_key,
            source_authority_5,
            source_inventory_5,
            destination_inventory_5
        ),
        (
            6,
            6,
            7,
            amount_6,
            source_6_start,
            destination_6_start,
            source_authority_6_key,
            destination_authority_6_key,
            source_inventory_6_key,
            destination_inventory_6_key,
            source_authority_6,
            source_inventory_6,
            destination_inventory_6
        ),
        (
            7,
            7,
            8,
            amount_7,
            source_7_start,
            destination_7_start,
            source_authority_7_key,
            destination_authority_7_key,
            source_inventory_7_key,
            destination_inventory_7_key,
            source_authority_7,
            source_inventory_7,
            destination_inventory_7
        ),
        (
            8,
            8,
            9,
            amount_8,
            source_8_start,
            destination_8_start,
            source_authority_8_key,
            destination_authority_8_key,
            source_inventory_8_key,
            destination_inventory_8_key,
            source_authority_8,
            source_inventory_8,
            destination_inventory_8
        ),
        (
            9,
            9,
            10,
            amount_9,
            source_9_start,
            destination_9_start,
            source_authority_9_key,
            destination_authority_9_key,
            source_inventory_9_key,
            destination_inventory_9_key,
            source_authority_9,
            source_inventory_9,
            destination_inventory_9
        ),
        (
            10,
            10,
            11,
            amount_10,
            source_10_start,
            destination_10_start,
            source_authority_10_key,
            destination_authority_10_key,
            source_inventory_10_key,
            destination_inventory_10_key,
            source_authority_10,
            source_inventory_10,
            destination_inventory_10
        ),
        (
            11,
            11,
            12,
            amount_11,
            source_11_start,
            destination_11_start,
            source_authority_11_key,
            destination_authority_11_key,
            source_inventory_11_key,
            destination_inventory_11_key,
            source_authority_11,
            source_inventory_11,
            destination_inventory_11
        ),
        (
            12,
            12,
            13,
            amount_12,
            source_12_start,
            destination_12_start,
            source_authority_12_key,
            destination_authority_12_key,
            source_inventory_12_key,
            destination_inventory_12_key,
            source_authority_12,
            source_inventory_12,
            destination_inventory_12
        ),
        (
            13,
            13,
            14,
            amount_13,
            source_13_start,
            destination_13_start,
            source_authority_13_key,
            destination_authority_13_key,
            source_inventory_13_key,
            destination_inventory_13_key,
            source_authority_13,
            source_inventory_13,
            destination_inventory_13
        ),
        (
            14,
            14,
            15,
            amount_14,
            source_14_start,
            destination_14_start,
            source_authority_14_key,
            destination_authority_14_key,
            source_inventory_14_key,
            destination_inventory_14_key,
            source_authority_14,
            source_inventory_14,
            destination_inventory_14
        ),
        (
            15,
            15,
            0,
            amount_15,
            source_15_start,
            destination_15_start,
            source_authority_15_key,
            destination_authority_15_key,
            source_inventory_15_key,
            destination_inventory_15_key,
            source_authority_15,
            source_inventory_15,
            destination_inventory_15
        ),
    ]
);

#[kani::proof]
#[kani::unwind(34)]
fn verify_live_swap_exact_in_moves_projected_token_balances() {
    let program_id = [42u8; 32];
    let user_vault_key = [7u8; 32];
    let hub_authorizer_key = [8u8; 32];
    let inventory_rebalancer_key = [9u8; 32];
    let input_mint_key = [3u8; 32];
    let output_mint_key = [4u8; 32];
    let lane_id = 0u8;

    let amount_in: u64 = kani::any();
    let amount_out: u64 = kani::any();
    let min_out: u64 = kani::any();
    let user_input_start: u64 = kani::any();
    let user_output_start: u64 = kani::any();
    let hub_input_start: u64 = kani::any();
    let hub_output_start: u64 = kani::any();
    kani::assume(amount_in > 0);
    kani::assume(amount_out > 0);
    kani::assume(amount_out >= amount_in);
    kani::assume(min_out <= amount_out);
    kani::assume(user_input_start >= amount_in);
    kani::assume(hub_input_start <= u64::MAX - amount_in);
    kani::assume(hub_output_start >= amount_out);
    kani::assume(user_output_start <= u64::MAX - amount_out);

    let config_key = crate::derive_config(&program_id).0;
    let hub_authority_key = crate::derive_hub_authority(&program_id, lane_id).0;
    let hub_input_key = crate::derive_inventory_account(&program_id, &input_mint_key, lane_id);
    let hub_output_key = crate::derive_inventory_account(&program_id, &output_mint_key, lane_id);
    let user_input_key = [20u8; 32];
    let user_output_key = [21u8; 32];
    kani::assume(input_mint_key != output_mint_key);
    kani::assume(user_input_key != user_output_key);
    kani::assume(user_input_key != hub_input_key);
    kani::assume(user_input_key != hub_output_key);
    kani::assume(user_output_key != hub_input_key);
    kani::assume(user_output_key != hub_output_key);
    kani::assume(hub_input_key != hub_output_key);

    let config_data = config_data(
        [6u8; 32],
        hub_authorizer_key,
        inventory_rebalancer_key,
        0,
        false,
        2,
        &[input_mint_key, output_mint_key],
    );
    let mut config = build_account(config_key, program_id, false, false, config_data);
    let mut user_vault = build_account(user_vault_key, [0u8; 32], true, false, []);
    let mut user_input = build_account(
        user_input_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(input_mint_key, user_vault_key, user_input_start),
    );
    let mut user_output = build_account(
        user_output_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(output_mint_key, user_vault_key, user_output_start),
    );
    let mut hub_input = build_account(
        hub_input_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(input_mint_key, hub_authority_key, hub_input_start),
    );
    let mut hub_output = build_account(
        hub_output_key,
        crate::SPL_TOKEN_ID,
        false,
        true,
        token_account_data(output_mint_key, hub_authority_key, hub_output_start),
    );
    let mut input_mint = build_account(
        input_mint_key,
        crate::SPL_TOKEN_ID,
        false,
        false,
        mint_data(6),
    );
    let mut output_mint = build_account(
        output_mint_key,
        crate::SPL_TOKEN_ID,
        false,
        false,
        mint_data(6),
    );
    let mut hub_authority = build_account(hub_authority_key, [0u8; 32], false, false, []);
    let mut hub_authorizer = build_account(hub_authorizer_key, [0u8; 32], true, false, []);
    let mut token_program = build_account(crate::SPL_TOKEN_ID, [0u8; 32], false, false, []);

    let accounts: [ManuallyDrop<AccountInfo>; 11] = unsafe {
        [
            ManuallyDrop::new(account_info_from_stack(&mut config)),
            ManuallyDrop::new(account_info_from_stack(&mut user_vault)),
            ManuallyDrop::new(account_info_from_stack(&mut user_input)),
            ManuallyDrop::new(account_info_from_stack(&mut user_output)),
            ManuallyDrop::new(account_info_from_stack(&mut hub_input)),
            ManuallyDrop::new(account_info_from_stack(&mut hub_output)),
            ManuallyDrop::new(account_info_from_stack(&mut input_mint)),
            ManuallyDrop::new(account_info_from_stack(&mut output_mint)),
            ManuallyDrop::new(account_info_from_stack(&mut hub_authority)),
            ManuallyDrop::new(account_info_from_stack(&mut hub_authorizer)),
            ManuallyDrop::new(account_info_from_stack(&mut token_program)),
        ]
    };
    let accounts_slice =
        unsafe { core::slice::from_raw_parts(&accounts as *const _ as *const AccountInfo, 11) };

    let mut instruction_data = [0u8; 28];
    instruction_data[0] = crate::SWAP_EXACT_IN;
    instruction_data[1..9].copy_from_slice(&amount_in.to_le_bytes());
    instruction_data[9..17].copy_from_slice(&amount_out.to_le_bytes());
    instruction_data[17..25].copy_from_slice(&min_out.to_le_bytes());
    instruction_data[25..27].copy_from_slice(&0u16.to_le_bytes());
    instruction_data[27] = lane_id;
    let result = crate::process_instruction(&program_id, accounts_slice, &instruction_data);

    assert!(result.is_ok());
    assert_eq!(token_amount(&user_input), user_input_start - amount_in);
    assert_eq!(token_amount(&hub_input), hub_input_start + amount_in);
    assert_eq!(token_amount(&hub_output), hub_output_start - amount_out);
    assert_eq!(token_amount(&user_output), user_output_start + amount_out);
}

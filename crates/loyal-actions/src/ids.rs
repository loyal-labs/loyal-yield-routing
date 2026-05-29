use solana_sdk::{pubkey, pubkey::Pubkey};

pub const SQUADS_SMART_ACCOUNT_PROGRAM_ID: Pubkey =
    pubkey!("SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG");
pub(crate) const SQUADS_SEED_PREFIX: &[u8] = b"smart_account";
pub(crate) const SQUADS_SEED_POLICY: &[u8] = b"policy";
pub(crate) const SQUADS_FULL_PERMISSIONS_MASK: u8 = 7;
pub(crate) const SQUADS_SYNC_SIGNER_COUNT: u8 = 1;

pub const JUPITER_V6_PROGRAM_ID: Pubkey = pubkey!("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4");
pub const JUPITER_SWAP_DISCRIMINATOR: [u8; 8] = [187, 100, 250, 204, 49, 196, 175, 20];
pub const JUPITER_SWAP_SLIPPAGE_BPS_OFFSET: u64 = 24;
pub const JUPITER_DEFAULT_MAX_SLIPPAGE_BPS: u16 = 100;
pub const LOYAL_HUB_SWAP_PROGRAM_ID: Pubkey = Pubkey::new_from_array([42; 32]);
pub const LOYAL_HUB_INITIALIZE_CONFIG: u8 = loyal_hub_abi::INITIALIZE_CONFIG;
pub const LOYAL_HUB_SWAP_EXACT_IN: u8 = loyal_hub_abi::SWAP_EXACT_IN;
pub const LOYAL_HUB_WITHDRAW_INVENTORY: u8 = loyal_hub_abi::WITHDRAW_INVENTORY;
pub const LOYAL_HUB_SET_PAUSED: u8 = loyal_hub_abi::SET_PAUSED;
pub const LOYAL_HUB_SET_MAX_FEE: u8 = loyal_hub_abi::SET_MAX_FEE;
pub const LOYAL_HUB_REBALANCE_INVENTORY: u8 = loyal_hub_abi::REBALANCE_INVENTORY;
pub const LOYAL_HUB_MAX_ALLOWED_MINTS: usize = loyal_hub_abi::MAX_ALLOWED_MINTS;
pub const LOYAL_HUB_MAX_REBALANCE_TRANSFERS: usize = loyal_hub_abi::MAX_REBALANCE_TRANSFERS;
pub const LOYAL_HUB_SWAP_TAG_OFFSET: u64 = loyal_hub_abi::SWAP_EXACT_IN_TAG_OFFSET;
pub const LOYAL_HUB_SWAP_MAX_FEE_BPS_OFFSET: u64 =
    loyal_hub_abi::SWAP_EXACT_IN_MAX_FEE_BPS_DATA_OFFSET;
pub const LOYAL_HUB_SWAP_EXACT_IN_DATA_LEN: usize = loyal_hub_abi::SWAP_EXACT_IN_DATA_LEN;
pub const LOYAL_HUB_INITIALIZE_CONFIG_DATA_LEN: usize = loyal_hub_abi::INITIALIZE_CONFIG_DATA_LEN;
pub const LOYAL_HUB_WITHDRAW_INVENTORY_DATA_LEN: usize = loyal_hub_abi::WITHDRAW_INVENTORY_DATA_LEN;
pub const LOYAL_HUB_SET_PAUSED_DATA_LEN: usize = loyal_hub_abi::SET_PAUSED_DATA_LEN;
pub const LOYAL_HUB_SET_MAX_FEE_DATA_LEN: usize = loyal_hub_abi::SET_MAX_FEE_DATA_LEN;
pub const LOYAL_HUB_REBALANCE_INVENTORY_ARGS_OFFSET: usize =
    loyal_hub_abi::REBALANCE_INVENTORY_ARGS_OFFSET;
pub const LOYAL_HUB_CONFIG_SEED: &[u8] = loyal_hub_abi::CONFIG_SEED;
pub const LOYAL_HUB_AUTHORITY_SEED: &[u8] = loyal_hub_abi::HUB_AUTHORITY_SEED;
pub const ASSOCIATED_TOKEN_PROGRAM_ID: Pubkey =
    pubkey!("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL");
pub const KAMINO_LEND_PROGRAM_ID: Pubkey = pubkey!("KvauGMspG5k6rtzrqqn7WNn3oZdyKqLKwK2XWQ8FLjd");
pub const KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR: [u8; 8] =
    [242, 35, 198, 137, 82, 225, 242, 182];
pub const KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR: [u8; 8] =
    [235, 52, 119, 152, 149, 197, 20, 7];

pub const YIELD_ROUTE_WITHDRAW_ACTION_SEED: u64 = 1;
pub const YIELD_ROUTE_SWAP_ACTION_SEED: u64 = 2;
pub const YIELD_ROUTE_DEPOSIT_ACTION_SEED: u64 = 3;
pub const YIELD_ROUTE_STANDALONE_ACTION_SEED: u64 = 1;

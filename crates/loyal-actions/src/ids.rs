use solana_sdk::{pubkey, pubkey::Pubkey};

pub const SQUADS_SMART_ACCOUNT_PROGRAM_ID: Pubkey =
    pubkey!("SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG");
pub(crate) const SQUADS_SEED_PREFIX: &[u8] = b"smart_account";
pub(crate) const SQUADS_SEED_POLICY: &[u8] = b"policy";
pub(crate) const SQUADS_FULL_PERMISSIONS_MASK: u8 = 7;
pub(crate) const SQUADS_SYNC_SIGNER_COUNT: u8 = 1;

pub const JUPITER_V6_PROGRAM_ID: Pubkey = pubkey!("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4");
pub const LOYAL_HUB_SWAP_PROGRAM_ID: Pubkey = Pubkey::new_from_array([42; 32]);
pub const LOYAL_HUB_SWAP_EXACT_IN: u8 = 1;
pub const LOYAL_HUB_CONFIG_SEED: &[u8] = b"config";
pub const LOYAL_HUB_AUTHORITY_SEED: &[u8] = b"hub-authority";
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

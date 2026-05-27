pub use loyal_actions::{
    LOYAL_HUB_AUTHORITY_SEED, LOYAL_HUB_CONFIG_SEED, LOYAL_HUB_INITIALIZE_CONFIG,
    LOYAL_HUB_REBALANCE_INVENTORY, LOYAL_HUB_SET_MAX_FEE, LOYAL_HUB_SET_PAUSED,
    LOYAL_HUB_SWAP_EXACT_IN, LOYAL_HUB_SWAP_MAX_FEE_BPS_OFFSET, LOYAL_HUB_SWAP_PROGRAM_ID,
    LOYAL_HUB_SWAP_TAG_OFFSET, LOYAL_HUB_WITHDRAW_INVENTORY,
};
use solana_sdk::{pubkey, pubkey::Pubkey};

pub const SQUADS_SMART_ACCOUNT_PROGRAM_ID: Pubkey =
    pubkey!("SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG");
pub const SQUADS_SEED_PREFIX: &[u8] = b"smart_account";
pub const SQUADS_SEED_SETTINGS: &[u8] = b"settings";
pub const SQUADS_SEED_SMART_ACCOUNT: &[u8] = b"smart_account";
pub const SQUADS_SEED_POLICY: &[u8] = b"policy";
pub const SQUADS_PROGRAM_CONFIG_SEED: &[u8] = b"program_config";
pub const SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR: [u8; 8] =
    [90, 81, 187, 81, 39, 70, 128, 78];
pub const SQUADS_CREATE_SMART_ACCOUNT_DISCRIMINATOR: [u8; 8] = [197, 102, 253, 231, 77, 84, 50, 17];
pub const SQUADS_PROGRAM_CONFIG_DISCRIMINATOR: [u8; 8] = [196, 210, 90, 231, 144, 149, 140, 63];
pub const SQUADS_FULL_PERMISSIONS_MASK: u8 = 7;
pub const SQUADS_SYNC_SIGNER_COUNT: u8 = 1;
pub const SQUADS_ONE_SIGNER_SETTINGS_SPACE: usize = 168;
pub const DEFAULT_WALLET_AIRDROP_LAMPORTS: u64 = 1_000_000_000;
pub const LAMPORTS_PER_SOL: u64 = 1_000_000_000;
pub const SOL_DECIMALS: u8 = 9;
pub const JUPITER_V6_PROGRAM_ID: Pubkey = pubkey!("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4");
pub const WRAPPED_SOL_MINT: Pubkey = pubkey!("So11111111111111111111111111111111111111112");
pub const USDC_MINT: Pubkey = pubkey!("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v");
pub const PYUSD_MINT: Pubkey = pubkey!("2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo");
pub const USDC_DECIMALS: u8 = 6;
pub const PYUSD_DECIMALS: u8 = 6;
pub const MOCK_JUPITER_SOL_TO_USDC: u8 = 1;
pub const MOCK_JUPITER_USDC_TO_PYUSD: u8 = 2;
pub const MOCK_JUPITER_STABLE_EXACT_IN: u8 = 3;
pub const DEFAULT_LOYAL_HUB_LANE_COUNT: u8 = 32;
pub const ASSOCIATED_TOKEN_PROGRAM_ID: Pubkey =
    pubkey!("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL");
pub const JUPITER_SWAP_AUTHORITY_SEED: &[u8] = b"jupiter-swap-authority";
pub const MOCK_JUPITER_USDC_RESERVE_TOKEN_ACCOUNT_SEED: &[u8] =
    b"mock-jupiter-usdc-reserve-token-account";
pub const MOCK_JUPITER_PYUSD_RESERVE_TOKEN_ACCOUNT_SEED: &[u8] =
    b"mock-jupiter-pyusd-reserve-token-account";
pub const MOCK_JUPITER_STABLE_RESERVE_TOKEN_ACCOUNT_SEED: &[u8] = b"mock-jupiter-stable-reserve";
pub const KAMINO_LEND_PROGRAM_ID: Pubkey = pubkey!("KvauGMspG5k6rtzrqqn7WNn3oZdyKqLKwK2XWQ8FLjd");
pub const KAMINO_MAIN_MARKET: Pubkey = pubkey!("7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF");
pub const KAMINO_MAIN_USDC_RESERVE: Pubkey =
    pubkey!("D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59");
pub const KAMINO_MAIN_PYUSD_RESERVE: Pubkey =
    pubkey!("2gc9Dm1eB6UgVYFBUN9bWks6Kes9PbWSaPaa9DqyvEiN");
pub const KAMINO_PRIME_MARKET: Pubkey = pubkey!("CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA");
pub const KAMINO_PRIME_USDC_RESERVE: Pubkey =
    pubkey!("9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu");
pub const KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR: [u8; 8] =
    [242, 35, 198, 137, 82, 225, 242, 182];
pub const KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR: [u8; 8] =
    [235, 52, 119, 152, 149, 197, 20, 7];
pub const KAMINO_COLLATERAL_DECIMALS: u8 = 6;
pub const KAMINO_RESERVE_LIQUIDITY_AUTHORITY_SEED: &[u8] = b"kamino-reserve-liq-authority";
pub const KAMINO_COLLATERAL_MINT_AUTHORITY_SEED: &[u8] = b"kamino-collateral-mint-authority";
pub const MOCK_YIELD_PROTOCOLS_PROGRAM_SO_ENV: &str = "MOCK_YIELD_PROTOCOLS_PROGRAM_SO";
pub const MOCK_YIELD_PROTOCOLS_PROGRAM_SO: &str = "mock_yield_protocols_program.so";
pub const LOYAL_HUB_SWAP_PROGRAM_SO_ENV: &str = "LOYAL_HUB_SWAP_PROGRAM_SO";
pub const LOYAL_HUB_SWAP_PROGRAM_SO: &str = "loyal_hub_swap_program.so";
pub const SQUADS_SMART_ACCOUNT_PROGRAM_SO_FIXTURE: &str =
    "crates/squads-test-harness/fixtures/squads/squads_smart_account_program.so";

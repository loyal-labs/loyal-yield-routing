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
pub const TOKEN_2022_PROGRAM_ID: Pubkey = pubkey!("TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb");
pub const SUBSCRIPTIONS_PROGRAM_ID: Pubkey =
    pubkey!("De1egAFMkMWZSN5rYXRj9CAdheBamobVNubTsi9avR44");
pub const SUBSCRIPTIONS_INIT_AUTHORITY: u8 = 0;
pub const SUBSCRIPTIONS_CREATE_RECURRING_DELEGATION: u8 = 2;
pub const SUBSCRIPTIONS_REVOKE_DELEGATION: u8 = 3;
pub const SUBSCRIPTIONS_TRANSFER_RECURRING: u8 = 5;
pub const SUBSCRIPTION_AUTHORITY_SEED: &[u8] = b"SubscriptionAuthority";
pub const SUBSCRIPTION_DELEGATION_SEED: &[u8] = b"delegation";
pub const SUBSCRIPTION_EVENT_AUTHORITY_SEED: &[u8] = b"event_authority";
pub const SUBSCRIPTION_TRANSFER_AMOUNT_OFFSET: u64 = 1;
pub const SUBSCRIPTION_TRANSFER_DELEGATOR_OFFSET: u64 = 9;
pub const SUBSCRIPTION_TRANSFER_MINT_OFFSET: u64 = 41;
pub const SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR: u8 = 3;
pub const SUBSCRIPTION_RECURRING_DELEGATION_DATA_LEN: usize = 211;
pub const SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR_OFFSET: u64 = 0;
pub const SUBSCRIPTION_RECURRING_DELEGATION_DELEGATOR_OFFSET: u64 = 3;
pub const SUBSCRIPTION_RECURRING_DELEGATION_DELEGATEE_OFFSET: u64 = 35;
pub const SUBSCRIPTION_RECURRING_DELEGATION_AUTHORITY_OFFSET: u64 = 107;
pub const SUBSCRIPTION_RECURRING_DELEGATION_MINT_OFFSET: u64 = 139;
pub const SUBSCRIPTION_RECURRING_DELEGATION_AMOUNT_PER_PERIOD_OFFSET: u64 = 195;
pub const SUBSCRIPTION_RECURRING_DELEGATION_AMOUNT_PULLED_OFFSET: u64 = 203;
pub const KAMINO_LEND_PROGRAM_ID: Pubkey = pubkey!("KvauGMspG5k6rtzrqqn7WNn3oZdyKqLKwK2XWQ8FLjd");
pub const KAMINO_MAIN_MARKET: Pubkey = pubkey!("7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF");
pub const KAMINO_FIGURE_MARKET: Pubkey = pubkey!("CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA");
pub const KAMINO_MAPLE_MARKET: Pubkey = pubkey!("6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y");
pub const KAMINO_ONRE_MARKET: Pubkey = pubkey!("47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8");
pub const KAMINO_ETHENA_MARKET: Pubkey = pubkey!("BJnbcRHqvppTyGesLzWASGKnmnF1wq9jZu6ExrjT7wvF");
pub const KAMINO_JLP_MARKET: Pubkey = pubkey!("DxXdAyU3kCjnyggvHmY5nAwg5cRbbmdyX3npfDMjjMek");
pub const KAMINO_BITCOIN_MARKET: Pubkey = pubkey!("GMqmFygF5iSm5nkckYU6tieggFcR42SyjkkhK5rswFRs");
pub const KAMINO_SUPERSTATE_OPENING_BELL_MARKET: Pubkey =
    pubkey!("CF32kn7AY8X1bW7ZkGcHc4X9ZWTxqKGCJk6QwrQkDcdw");
pub const KAMINO_HUMA_MARKET: Pubkey = pubkey!("52FSGeeokLpgvgAMdqxyt5Hoc2TbUYj5b8yxrEdZ37Vf");
pub const KAMINO_SOLSTICE_MARKET: Pubkey = pubkey!("9Y7uwXgQ68mGqRtZfuFaP4hc4fxeJ7cE9zTtqTxVhfGU");
pub const KAMINO_XSTOCKS_MARKET: Pubkey = pubkey!("5wJeMrUYECGq41fxRESKALVcHnNX26TAWy4W98yULsua");
pub const KAMINO_ALTCOINS_MARKET: Pubkey = pubkey!("ByYiZxp8QrdN9qbdtaAiePN8AAr3qvTPppNJDpf5DVJ5");
pub const KAMINO_MAIN_USDC_RESERVE: Pubkey =
    pubkey!("D6q6wuQSrifJKZYpR1M8R4YawnLDtDsMmWM1NbBmgJ59");
pub const KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR: [u8; 8] =
    [242, 35, 198, 137, 82, 225, 242, 182];
pub const KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR: [u8; 8] =
    [235, 52, 119, 152, 149, 197, 20, 7];

pub const USDC_MINT: Pubkey = pubkey!("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v");
pub const USDT_MINT: Pubkey = pubkey!("Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB");
pub const PYUSD_MINT: Pubkey = pubkey!("2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo");
pub const USDS_MINT: Pubkey = pubkey!("USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA");
pub const USDG_MINT: Pubkey = pubkey!("2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH");
pub const USDE_MINT: Pubkey = pubkey!("DEkqHyPN7GMRJ5cArtQFAWefqbZb33Hyf6s5iCwjEonT");
pub const SUSDE_MINT: Pubkey = pubkey!("Eh6XEPhSwoLv5wFApukmnaVSHQ6sAnoD9BmgmwQoN2sN");
pub const CASH_MINT: Pubkey = pubkey!("CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH");
pub const SYRUP_USDC_MINT: Pubkey = pubkey!("AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj");
pub const USD1_MINT: Pubkey = pubkey!("USD1ttGY1N17NEEHLmELoaybftRBUSErhqYiQzvEmuB");
pub const FDUSD_MINT: Pubkey = pubkey!("9zNQRsGLjNKwCUU5Gq5LR8beUCPzQMVMqKAi3SSZh54u");
pub const AUSD_MINT: Pubkey = pubkey!("AUSD1jCcCyPLybk1YnvPWsHQSrZ46dxwoMniN4N2UEB9");
pub const EUSX_MINT: Pubkey = pubkey!("3ThdFZQKM6kRyVGLG48kaPg5TRMhYMKY1iCRa9xop1WC");
pub const USCC_MINT: Pubkey = pubkey!("BTRR3sj1Bn2ZjuemgbeQ6SCtf84iXS81CS7UDTSxUCaK");
pub const USDH_MINT: Pubkey = pubkey!("USDH1SM1ojwWUga67PGrgFWUHibbjqMvuMaDkRJTgkX");

pub const YIELD_ROUTE_WITHDRAW_ACTION_SEED: u64 = 1;
pub const YIELD_ROUTE_SWAP_ACTION_SEED: u64 = 2;
pub const YIELD_ROUTE_DEPOSIT_ACTION_SEED: u64 = 3;
pub const YIELD_ROUTE_STANDALONE_ACTION_SEED: u64 = 1;

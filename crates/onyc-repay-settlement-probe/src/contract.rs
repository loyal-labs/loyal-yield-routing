//! Fixed offline ONyc fixture bindings. Not a deployable/general route catalog.
use solana_program::{pubkey, pubkey::Pubkey};
pub const PROGRAM: Pubkey = Pubkey::new_from_array([73; 32]);
pub const SYSTEM: Pubkey = Pubkey::new_from_array([0; 32]);
pub const SQUADS: Pubkey = pubkey!("SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG");
pub const SETTINGS: Pubkey = pubkey!("5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6");
pub const KLEND: Pubkey = pubkey!("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD");
pub const EXECUTOR: Pubkey = pubkey!("62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5");
pub const VAULT: Pubkey = pubkey!("ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh");
pub const CASH: Pubkey = pubkey!("EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe");
pub const ONYC: Pubkey = pubkey!("AVX9wxDTk639eZ4KaiMA7LrLhXe7Lg6DaDDVRa1Q7Ji3");
pub const OBLIGATION: Pubkey = pubkey!("4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei");
pub const MARKET: Pubkey = pubkey!("47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8");
pub const DEBT: Pubkey = pubkey!("AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z");
pub const COLLATERAL: Pubkey = pubkey!("6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ");
pub const USDC: Pubkey = pubkey!("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v");
pub const JUPITER: Pubkey = pubkey!("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4");
pub const WITHDRAW_POLICY: Pubkey = pubkey!("2Wn69xc4ntC2aTjQNi4nnfmTCAqHngWYVfLSyeRbkkKh");
pub const SWAP_POLICY: Pubkey = pubkey!("Z9jqB9pWDf1L1yFKVzXU1XnX8eKLndFP37FUwZMfWyz");
pub const MAX_PRINCIPAL: u64 = 100_000_000_000;
pub const OPERATION: u64 = 1;
pub const RECEIPT_SEED: &[u8] = b"onyc-repay-v1";
pub const RECEIPT_LEN: usize = 64;
pub const BEGIN: &[u8; 8] = b"ONYCBEG1";
pub const SETTLE: &[u8; 8] = b"ONYCEND1";
pub const REPAY_DISC: [u8; 8] = [116, 174, 213, 76, 180, 53, 210, 144];
pub const WITHDRAW_DISC: [u8; 8] = [235, 52, 119, 152, 149, 197, 20, 7];
pub const REFRESH_RESERVE: [u8; 8] = [2, 218, 138, 235, 79, 201, 25, 102];
pub const REFRESH_OBLIGATION: [u8; 8] = [33, 132, 147, 228, 151, 192, 72, 89];
pub const REPAY_KEYS: [Pubkey; 14] = [
    pubkey!("62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5"),
    pubkey!("4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei"),
    pubkey!("47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"),
    pubkey!("AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z"),
    pubkey!("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"),
    pubkey!("8BkQTZsT8ssKMU643De4iiV5Wf3pENdUFTsdtHPueKjB"),
    pubkey!("5NQuKQAR7YGqRz7L4EaRtEcbfKdD5YGucJyHygYBCK8x"),
    pubkey!("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"),
    pubkey!("Sysvar1nstructions1111111111111111111111111"),
    pubkey!("nMqFZFPQsNwot49QAD1B76LxNV7qRG1tnbkXyTjbUAD"),
    pubkey!("7vNfe1qX8iDxP5p3A4fosrjLqdn1YjmmGcZZkG2b4APF"),
    pubkey!("FsvTiXTUFDc4aLbrov4PrvDTjXCWCniL1dxTUkZ1T2ss"),
    pubkey!("FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr"),
    pubkey!("6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ"),
];
pub const WITHDRAW_KEYS: [Pubkey; 17] = [
    pubkey!("ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"),
    pubkey!("4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei"),
    pubkey!("47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"),
    pubkey!("FsvTiXTUFDc4aLbrov4PrvDTjXCWCniL1dxTUkZ1T2ss"),
    pubkey!("6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ"),
    pubkey!("5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5"),
    pubkey!("2c42iUaea3QVLvSPQHUBZBwqdvpiQo5vmeMePq9qx8eo"),
    pubkey!("CtzvqjvpxJDXyraDjP2QrEr8b1xvGvxADRV7w29qrmxd"),
    pubkey!("9YuHgsPVGgWrkpsaRZmeZCV2uXweMEn6TEAcusQKRjgG"),
    pubkey!("AVX9wxDTk639eZ4KaiMA7LrLhXe7Lg6DaDDVRa1Q7Ji3"),
    pubkey!("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"),
    pubkey!("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"),
    pubkey!("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"),
    pubkey!("Sysvar1nstructions1111111111111111111111111"),
    pubkey!("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"),
    pubkey!("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"),
    pubkey!("FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr"),
];
pub const SWAP_KEYS: [Pubkey; 29] = [
    pubkey!("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"),
    pubkey!("6LXutJvKUw8Q5ue2gCgKHQdAN4suWW8awzFVC6XCguFx"),
    pubkey!("ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"),
    pubkey!("AVX9wxDTk639eZ4KaiMA7LrLhXe7Lg6DaDDVRa1Q7Ji3"),
    pubkey!("ABd1w79Uvtgsn4siauY1HUtp2t84UhoP5fN1KwtpJPf7"),
    pubkey!("G4CD7aqqZZ6QKCNHrc1MPdS9Aw8BWmQ5ZkDd54W6mAEG"),
    pubkey!("EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe"),
    pubkey!("5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5"),
    pubkey!("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"),
    pubkey!("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"),
    pubkey!("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"),
    pubkey!("D8cy77BBepLMngZx6ZukaTff5hCt1HrWyKk3Hnd9oitf"),
    pubkey!("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"),
    pubkey!("whirLbMiicVdio4qvUfM5KAg6Ct8VwpYzGff3uctyCc"),
    pubkey!("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"),
    pubkey!("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"),
    pubkey!("MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr"),
    pubkey!("6LXutJvKUw8Q5ue2gCgKHQdAN4suWW8awzFVC6XCguFx"),
    pubkey!("7jhhyxPUKpu42hPGSYwgMXbR2dtVJHKhs8DW3sAAgAvX"),
    pubkey!("5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5"),
    pubkey!("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"),
    pubkey!("ABd1w79Uvtgsn4siauY1HUtp2t84UhoP5fN1KwtpJPf7"),
    pubkey!("4SNzvPoPnnzR6sz5GdEnUpSFoZQUSnpX6KpXRKzDDfLh"),
    pubkey!("G4CD7aqqZZ6QKCNHrc1MPdS9Aw8BWmQ5ZkDd54W6mAEG"),
    pubkey!("EFK2eCu8FoEJerhf9P877oT3wWuq5ciAGWHR21jJaebY"),
    pubkey!("4AnnpKm2SYDWHcWyuPp8q73Ge72wRgxXxvrwE8Z722fS"),
    pubkey!("3J8ahxRgSvEjc1AMf9A9p5qyePWGWXZaqCEkRU2R848h"),
    pubkey!("5fk1ZpjC7k7xD3X8JnK3b5a2kfVrcuxhLQEqyVBcT7xQ"),
    pubkey!("CMDtGA6Ka1jjw4VgAy7Pd49aQxgiUUiK5zPqA6H2QpAL"),
];
pub const REFRESH_COLLATERAL_KEYS: [Pubkey; 6] = [
    pubkey!("6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ"),
    pubkey!("47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"),
    pubkey!("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"),
    pubkey!("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"),
    pubkey!("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"),
    pubkey!("3t4JZcueEzTbVP6kLxXrL3VpWx45jDer4eqysweBchNH"),
];
pub const REFRESH_DEBT_KEYS: [Pubkey; 6] = [
    pubkey!("AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z"),
    pubkey!("47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"),
    pubkey!("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"),
    pubkey!("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"),
    pubkey!("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"),
    pubkey!("3t4JZcueEzTbVP6kLxXrL3VpWx45jDer4eqysweBchNH"),
];
pub const REFRESH_BEFORE_KEYS: [Pubkey; 4] = [
    pubkey!("47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"),
    pubkey!("4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei"),
    pubkey!("6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ"),
    pubkey!("AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z"),
];
pub const REFRESH_AFTER_KEYS: [Pubkey; 3] = [
    pubkey!("47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"),
    pubkey!("4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei"),
    pubkey!("6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ"),
];

pub const CONFIG_SEED: &[u8] = b"config";
pub const HUB_AUTHORITY_SEED: &[u8] = b"hub-authority";
pub const CONFIG_MAGIC: &[u8; 8] = b"LHUBCFG1";
pub const MAX_ALLOWED_MINTS: usize = 8;
pub const HUB_CONFIG_SPACE: usize = 8 + 32 + 32 + 2 + 1 + 1 + (MAX_ALLOWED_MINTS * 32);

pub const INITIALIZE_CONFIG: u8 = 0;
pub const SWAP_EXACT_IN: u8 = 1;
pub const WITHDRAW_INVENTORY: u8 = 2;
pub const SET_PAUSED: u8 = 3;
pub const SET_CONFIG: u8 = 4;

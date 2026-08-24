use loyal_actions::{
    derive_action_account, derive_associated_token_account, derive_kamino_obligation,
    derive_kamino_obligation_farm_user_state, derive_squads_vault,
};
use loyal_yield_store::fleet_orchestration::{MultiplyAction, MultiplyRouteState, StrategyKey};
use solana_sdk::pubkey::Pubkey;
use std::{error::Error, str::FromStr};

pub const MANIFEST_VERSION: &str = "earn-max-v2";
pub const EARN_MAX_VAULT_INDEX: u8 = 0;
pub const MAINNET_GENESIS_HASH: &str = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d";
pub const KLEND: &str = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD";
pub const JUPITER: &str = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4";
pub const JUPITER_SHARED_ACCOUNTS_ROUTE: [u8; 8] = [0xc1, 0x20, 0x9b, 0x33, 0x41, 0xd6, 0x9c, 0x81];
pub const FARMS: &str = "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr";
pub const TOKEN: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
pub const TOKEN_2022: &str = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb";
pub const ONYC_MINT: &str = "5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5";
pub const PRIME_MINT: &str = "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7";
pub const SYRUP_MINT: &str = "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj";
pub const USDC_MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
pub const PYUSD_MINT: &str = "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo";
pub const USDS_MINT: &str = "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA";

const MULTIPLY_OBLIGATION_TAG: u8 = 1;
const MULTIPLY_OBLIGATION_ID: u8 = 0;

#[derive(Clone, Copy, Debug)]
pub struct PolicyConfig {
    pub seed: u64,
    pub account: Pubkey,
}

#[derive(Clone, Copy, Debug)]
pub struct StrategyConfig {
    pub key: StrategyKey,
    pub market: &'static str,
    pub market_authority: &'static str,
    pub oracle: &'static str,
    pub collateral_reserve: &'static str,
    pub collateral_mint: &'static str,
    pub collateral_custody: Pubkey,
    pub collateral_liquidity_supply: &'static str,
    pub collateral_receipt_mint: &'static str,
    pub collateral_mint_supply: &'static str,
    pub collateral_farm_state: Option<&'static str>,
    pub collateral_farm_user: Option<Pubkey>,
    pub debt_reserve: &'static str,
    pub debt_mint: &'static str,
    pub debt_token_program: &'static str,
    pub debt_custody: Pubkey,
    pub debt_liquidity_supply: &'static str,
    pub debt_fee_vault: &'static str,
    pub debt_farm_state: Option<&'static str>,
    pub debt_farm_user: Option<Pubkey>,
    pub obligation: Pubkey,
    pub target_ltv_bps: u16,
    pub collateral_policy: PolicyConfig,
    pub debt_policy: PolicyConfig,
    pub swap_policy: PolicyConfig,
}

impl StrategyConfig {
    pub fn policy(self, action: MultiplyAction) -> Option<PolicyConfig> {
        match action {
            MultiplyAction::DepositCollateral
            | MultiplyAction::WithdrawCollateral
            | MultiplyAction::WithdrawRemainingCollateral => Some(self.collateral_policy),
            MultiplyAction::BorrowDebt | MultiplyAction::RepayDebt => Some(self.debt_policy),
            MultiplyAction::SwapClaimToCollateral
            | MultiplyAction::SwapDebtToCollateral
            | MultiplyAction::SwapCollateralToDebt
            | MultiplyAction::SwapCollateralToClaim => Some(self.swap_policy),
            MultiplyAction::Claim
            | MultiplyAction::DepositClaimAsset
            | MultiplyAction::RequestWithdrawal
            | MultiplyAction::CancelWithdrawal => None,
        }
    }
}

#[derive(Clone, Copy, Debug)]
pub struct EarnMaxTopology {
    pub manifest_version: &'static str,
    pub settings: Pubkey,
    pub vault_index: u8,
    pub vault: Pubkey,
    pub claim_custody: Pubkey,
    pub collateral_custody: Pubkey,
    pub strategies: [StrategyConfig; 7],
}

impl EarnMaxTopology {
    pub fn strategy(self, key: StrategyKey) -> StrategyConfig {
        match key {
            StrategyKey::OnycUsdc => self.strategies[0],
            StrategyKey::OnycUsds => self.strategies[1],
            StrategyKey::PrimeUsdc => self.strategies[2],
            StrategyKey::PrimePyusd => self.strategies[3],
            StrategyKey::PrimeUsds => self.strategies[4],
            StrategyKey::SyrupUsdcUsdc => self.strategies[5],
            StrategyKey::SyrupUsdcPyusd => self.strategies[6],
        }
    }

    pub fn strategy_catalog(self) -> [StrategyConfig; 7] {
        self.strategies
    }
}

#[derive(Clone, Copy)]
struct StrategyTemplate {
    key: StrategyKey,
    market: &'static str,
    market_authority: &'static str,
    oracle: &'static str,
    collateral_reserve: &'static str,
    collateral_mint: &'static str,
    collateral_liquidity_supply: &'static str,
    collateral_receipt_mint: &'static str,
    collateral_mint_supply: &'static str,
    collateral_farm_state: Option<&'static str>,
    debt_reserve: &'static str,
    debt_mint: &'static str,
    debt_token_program: &'static str,
    debt_liquidity_supply: &'static str,
    debt_fee_vault: &'static str,
    debt_farm_state: Option<&'static str>,
    target_ltv_bps: u16,
    policy_seeds: StrategyPolicySeeds,
}

#[derive(Clone, Copy)]
struct StrategyPolicySeeds {
    collateral: u64,
    debt: u64,
    swap: u64,
}

const COMMON_ORACLE: &str = "3t4JZcueEzTbVP6kLxXrL3VpWx45jDer4eqysweBchNH";

const ONYC_MARKET: &str = "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8";
const ONYC_MARKET_AUTHORITY: &str = "FsvTiXTUFDc4aLbrov4PrvDTjXCWCniL1dxTUkZ1T2ss";
const ONYC_COLLATERAL_RESERVE: &str = "6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ";
const ONYC_COLLATERAL_LIQUIDITY_SUPPLY: &str = "9YuHgsPVGgWrkpsaRZmeZCV2uXweMEn6TEAcusQKRjgG";
const ONYC_COLLATERAL_RECEIPT_MINT: &str = "CtzvqjvpxJDXyraDjP2QrEr8b1xvGvxADRV7w29qrmxd";
const ONYC_COLLATERAL_RECEIPT_SUPPLY: &str = "2c42iUaea3QVLvSPQHUBZBwqdvpiQo5vmeMePq9qx8eo";

const ONYC_USDC_TEMPLATE: StrategyTemplate = StrategyTemplate {
    key: StrategyKey::OnycUsdc,
    market: ONYC_MARKET,
    market_authority: ONYC_MARKET_AUTHORITY,
    oracle: COMMON_ORACLE,
    collateral_reserve: ONYC_COLLATERAL_RESERVE,
    collateral_mint: ONYC_MINT,
    collateral_liquidity_supply: ONYC_COLLATERAL_LIQUIDITY_SUPPLY,
    collateral_receipt_mint: ONYC_COLLATERAL_RECEIPT_MINT,
    collateral_mint_supply: ONYC_COLLATERAL_RECEIPT_SUPPLY,
    collateral_farm_state: None,
    debt_reserve: "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z",
    debt_mint: USDC_MINT,
    debt_token_program: TOKEN,
    debt_liquidity_supply: "8BkQTZsT8ssKMU643De4iiV5Wf3pENdUFTsdtHPueKjB",
    debt_fee_vault: "5iLRav31Y7DJwM6bZ7s92jqvV3zd1wZMcp4mYeKXh8cj",
    debt_farm_state: Some("7vNfe1qX8iDxP5p3A4fosrjLqdn1YjmmGcZZkG2b4APF"),
    target_ltv_bps: 5_000,
    policy_seeds: StrategyPolicySeeds {
        collateral: 32,
        debt: 33,
        swap: 34,
    },
};

const ONYC_USDS_TEMPLATE: StrategyTemplate = StrategyTemplate {
    key: StrategyKey::OnycUsds,
    market: ONYC_MARKET,
    market_authority: ONYC_MARKET_AUTHORITY,
    oracle: COMMON_ORACLE,
    collateral_reserve: ONYC_COLLATERAL_RESERVE,
    collateral_mint: ONYC_MINT,
    collateral_liquidity_supply: ONYC_COLLATERAL_LIQUIDITY_SUPPLY,
    collateral_receipt_mint: ONYC_COLLATERAL_RECEIPT_MINT,
    collateral_mint_supply: ONYC_COLLATERAL_RECEIPT_SUPPLY,
    collateral_farm_state: None,
    debt_reserve: "3yDc9ARvtPLhYxZLgucZGuBtZ9bHshBvXTwHxGe3nhmC",
    debt_mint: USDS_MINT,
    debt_token_program: TOKEN,
    debt_liquidity_supply: "21Skwocv5cJoftyejSTtXVaHJWTg88xcWGQtnRvUyKLx",
    debt_fee_vault: "CmMAn2UtLWHsQhwv31Trz4BZwVravs2jgxZYK2daTHaK",
    debt_farm_state: Some("5piFMvvPonJM8zJbCGoPD2jZt59hNURDLDTpXQzgbydc"),
    target_ltv_bps: 5_000,
    policy_seeds: StrategyPolicySeeds {
        collateral: 32,
        debt: 33,
        swap: 34,
    },
};

const PRIME_MARKET: &str = "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA";
const PRIME_MARKET_AUTHORITY: &str = "9SLBVnPz8dRGvafST6zNBZYSSt3HtdU68XQLGR13t3uM";
const PRIME_COLLATERAL_RESERVE: &str = "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh";
const PRIME_COLLATERAL_LIQUIDITY_SUPPLY: &str = "FkSkbRU5A6JXRXo5uaFwCS7jQ6jHYa1DxFtfpXfTz352";
const PRIME_COLLATERAL_RECEIPT_MINT: &str = "FMKBCGqipyj5dm9C58Rb9ZWYeneDzrxd3YaL6amgZ8gW";
const PRIME_COLLATERAL_RECEIPT_SUPPLY: &str = "Eg4wKFWc8aGfAqrcmYu3paz2afY5VqJMo17K95Y4VqFN";

const PRIME_USDC_TEMPLATE: StrategyTemplate = StrategyTemplate {
    key: StrategyKey::PrimeUsdc,
    market: PRIME_MARKET,
    market_authority: PRIME_MARKET_AUTHORITY,
    oracle: COMMON_ORACLE,
    collateral_reserve: PRIME_COLLATERAL_RESERVE,
    collateral_mint: PRIME_MINT,
    collateral_liquidity_supply: PRIME_COLLATERAL_LIQUIDITY_SUPPLY,
    collateral_receipt_mint: PRIME_COLLATERAL_RECEIPT_MINT,
    collateral_mint_supply: PRIME_COLLATERAL_RECEIPT_SUPPLY,
    collateral_farm_state: None,
    debt_reserve: "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu",
    debt_mint: USDC_MINT,
    debt_token_program: TOKEN,
    debt_liquidity_supply: "H6JUwz8c61eQnYUx8avGXydKztKPyGvgWAUjmZUPS3BC",
    debt_fee_vault: "BzSw9sWTxUumr2wHhDiezkaLy3QZQS1KT4a9Fz8GvAQ6",
    debt_farm_state: None,
    target_ltv_bps: 6_500,
    policy_seeds: StrategyPolicySeeds {
        collateral: 32,
        debt: 33,
        swap: 34,
    },
};

const PRIME_PYUSD_TEMPLATE: StrategyTemplate = StrategyTemplate {
    key: StrategyKey::PrimePyusd,
    market: PRIME_MARKET,
    market_authority: PRIME_MARKET_AUTHORITY,
    oracle: COMMON_ORACLE,
    collateral_reserve: PRIME_COLLATERAL_RESERVE,
    collateral_mint: PRIME_MINT,
    collateral_liquidity_supply: PRIME_COLLATERAL_LIQUIDITY_SUPPLY,
    collateral_receipt_mint: PRIME_COLLATERAL_RECEIPT_MINT,
    collateral_mint_supply: PRIME_COLLATERAL_RECEIPT_SUPPLY,
    collateral_farm_state: None,
    debt_reserve: "3ZUAwhEtK8XWfK4fy98z4yoptm4GeyeAu21L11HPXaZ5",
    debt_mint: PYUSD_MINT,
    debt_token_program: TOKEN_2022,
    debt_liquidity_supply: "4LF3i8grZPRbk8d6gXvzRux4rYjGd5AmqrpLLYFpPKKt",
    debt_fee_vault: "4b9U55muKtwx9RimJSuztvyZaKWkmaoferVexgvxrYJr",
    debt_farm_state: None,
    target_ltv_bps: 6_500,
    policy_seeds: StrategyPolicySeeds {
        collateral: 32,
        debt: 33,
        swap: 34,
    },
};

const PRIME_USDS_TEMPLATE: StrategyTemplate = StrategyTemplate {
    key: StrategyKey::PrimeUsds,
    market: PRIME_MARKET,
    market_authority: PRIME_MARKET_AUTHORITY,
    oracle: COMMON_ORACLE,
    collateral_reserve: PRIME_COLLATERAL_RESERVE,
    collateral_mint: PRIME_MINT,
    collateral_liquidity_supply: PRIME_COLLATERAL_LIQUIDITY_SUPPLY,
    collateral_receipt_mint: PRIME_COLLATERAL_RECEIPT_MINT,
    collateral_mint_supply: PRIME_COLLATERAL_RECEIPT_SUPPLY,
    collateral_farm_state: None,
    debt_reserve: "7SzMWArC8WAenndXFmRyfvcvrNPodqUFkmPrmmoRZvn4",
    debt_mint: USDS_MINT,
    debt_token_program: TOKEN,
    debt_liquidity_supply: "5tP1kDJBYnjtrpUaRQhsrU1Y28ahiJVjz8p9mbqJFpz5",
    debt_fee_vault: "DjmdtvsvctUXCZ32y6UGdCEvXPTds6Ci7LFnVhw5HaQY",
    debt_farm_state: None,
    target_ltv_bps: 6_500,
    policy_seeds: StrategyPolicySeeds {
        collateral: 32,
        debt: 33,
        swap: 34,
    },
};

const COMMON_MARKET: &str = "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y";
const COMMON_MARKET_AUTHORITY: &str = "6QbtpY2jDNcncRFmVf343NThnCdaY8gCAsYATPnYQR9g";
const COLLATERAL_RESERVE: &str = "AwCyCPZYJSZ93xcVKNK7jR8e1BHzJXq1D4bReNuh9woY";
const COLLATERAL_LIQUIDITY_SUPPLY: &str = "8Se5SK1Tty2bH4EQVrKW8hwr9Lc9E2cEbkaN59DpcB6i";
const COLLATERAL_RECEIPT_MINT: &str = "9gQ8M4WiFepY9skYntJZ5N3joa3RByiPqao61gMfmGMu";
const COLLATERAL_MINT_SUPPLY: &str = "21GK6yHS3MKhTnF5pN5FuSmnpLiyPXTDrpxxbqMEoX58";

const SYRUP_USDC_USDC_TEMPLATE: StrategyTemplate = StrategyTemplate {
    key: StrategyKey::SyrupUsdcUsdc,
    market: COMMON_MARKET,
    market_authority: COMMON_MARKET_AUTHORITY,
    oracle: COMMON_ORACLE,
    collateral_reserve: COLLATERAL_RESERVE,
    collateral_mint: SYRUP_MINT,
    collateral_liquidity_supply: COLLATERAL_LIQUIDITY_SUPPLY,
    collateral_receipt_mint: COLLATERAL_RECEIPT_MINT,
    collateral_mint_supply: COLLATERAL_MINT_SUPPLY,
    collateral_farm_state: None,
    debt_reserve: "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo",
    debt_mint: USDC_MINT,
    debt_token_program: TOKEN,
    debt_liquidity_supply: "BBcwMNSMyhhBnYE9pevEvkxKHGzTafMP9v3j7Kk7nAWM",
    debt_fee_vault: "HH7GLnRcGHJrdkEueVVj7mccNUjnSeWobDmtu9cHLkJV",
    debt_farm_state: Some("87gUNr8LwYJCT25HjPEHnrfBBjwEMAjfqCfnKcJNqy9Y"),
    target_ltv_bps: 6_500,
    policy_seeds: StrategyPolicySeeds {
        collateral: 32,
        debt: 33,
        swap: 34,
    },
};

const SYRUP_USDC_PYUSD_TEMPLATE: StrategyTemplate = StrategyTemplate {
    key: StrategyKey::SyrupUsdcPyusd,
    market: COMMON_MARKET,
    market_authority: COMMON_MARKET_AUTHORITY,
    oracle: COMMON_ORACLE,
    collateral_reserve: COLLATERAL_RESERVE,
    collateral_mint: SYRUP_MINT,
    collateral_liquidity_supply: COLLATERAL_LIQUIDITY_SUPPLY,
    collateral_receipt_mint: COLLATERAL_RECEIPT_MINT,
    collateral_mint_supply: COLLATERAL_MINT_SUPPLY,
    collateral_farm_state: None,
    debt_reserve: "92qeAka3ZzCGPfJriDXrE7tiNqfATVCAM6ZjjctR3TrS",
    debt_mint: PYUSD_MINT,
    debt_token_program: TOKEN_2022,
    debt_liquidity_supply: "GUENeLN1ufX4K5622DbyYoQFhaWxMKoCFycvLSEYsykN",
    debt_fee_vault: "AwnzukUiajn7b3T9hXcwy19RLPZcHmLANUeqZnzXT6dU",
    debt_farm_state: Some("9AUA7XZ1rynUsZcmVCgj8UFdQuDozFSMpaNGBZAtiPWj"),
    target_ltv_bps: 6_500,
    policy_seeds: StrategyPolicySeeds {
        collateral: 32,
        debt: 33,
        swap: 34,
    },
};

pub fn derive_earn_max_topology(settings: Pubkey) -> Result<EarnMaxTopology, Box<dyn Error>> {
    derive_earn_max_topology_inner(settings, None)
}

pub fn derive_earn_max_topology_with_policy_seed_base(
    settings: Pubkey,
    policy_seed_base: u64,
) -> Result<EarnMaxTopology, Box<dyn Error>> {
    derive_earn_max_topology_inner(settings, Some(policy_seed_base))
}

fn derive_earn_max_topology_inner(
    settings: Pubkey,
    policy_seed_base: Option<u64>,
) -> Result<EarnMaxTopology, Box<dyn Error>> {
    let vault = derive_squads_vault(&settings, EARN_MAX_VAULT_INDEX).0;
    let usdc_mint = Pubkey::from_str(USDC_MINT)?;
    let token = Pubkey::from_str(TOKEN)?;
    let claim_custody = derive_associated_token_account(vault, usdc_mint, token);
    let collateral_custody =
        derive_associated_token_account(vault, Pubkey::from_str(SYRUP_MINT)?, token);
    Ok(EarnMaxTopology {
        manifest_version: MANIFEST_VERSION,
        settings,
        vault_index: EARN_MAX_VAULT_INDEX,
        vault,
        claim_custody,
        collateral_custody,
        strategies: [
            derive_strategy(settings, vault, ONYC_USDC_TEMPLATE, policy_seed_base)?,
            derive_strategy(settings, vault, ONYC_USDS_TEMPLATE, policy_seed_base)?,
            derive_strategy(settings, vault, PRIME_USDC_TEMPLATE, policy_seed_base)?,
            derive_strategy(settings, vault, PRIME_PYUSD_TEMPLATE, policy_seed_base)?,
            derive_strategy(settings, vault, PRIME_USDS_TEMPLATE, policy_seed_base)?,
            derive_strategy(settings, vault, SYRUP_USDC_USDC_TEMPLATE, policy_seed_base)?,
            derive_strategy(settings, vault, SYRUP_USDC_PYUSD_TEMPLATE, policy_seed_base)?,
        ],
    })
}

pub fn topology_for_route(route: &MultiplyRouteState) -> Result<EarnMaxTopology, Box<dyn Error>> {
    let topology = derive_earn_max_topology_with_policy_seed_base(
        Pubkey::from_str(&route.settings)?,
        route.policy_seed_base,
    )?;
    if route.vault_index != topology.vault_index || route.vault != topology.vault.to_string() {
        return Err("route identity does not match the deterministic Earn MAX topology".into());
    }
    Ok(topology)
}

fn derive_strategy(
    settings: Pubkey,
    vault: Pubkey,
    template: StrategyTemplate,
    policy_seed_base: Option<u64>,
) -> Result<StrategyConfig, Box<dyn Error>> {
    let market = Pubkey::from_str(template.market)?;
    let collateral_mint = Pubkey::from_str(template.collateral_mint)?;
    let debt_mint = Pubkey::from_str(template.debt_mint)?;
    let debt_token_program = Pubkey::from_str(template.debt_token_program)?;
    let obligation = derive_kamino_obligation(
        vault,
        market,
        MULTIPLY_OBLIGATION_TAG,
        MULTIPLY_OBLIGATION_ID,
        collateral_mint,
        debt_mint,
    );
    let farm_user = template
        .debt_farm_state
        .map(Pubkey::from_str)
        .transpose()?
        .map(|farm| derive_kamino_obligation_farm_user_state(farm, obligation));
    let seeds = match policy_seed_base {
        Some(base) => StrategyPolicySeeds {
            collateral: base,
            debt: base.checked_add(1).ok_or("Earn MAX policy seed overflow")?,
            swap: base.checked_add(2).ok_or("Earn MAX policy seed overflow")?,
        },
        None => template.policy_seeds,
    };
    Ok(StrategyConfig {
        key: template.key,
        market: template.market,
        market_authority: template.market_authority,
        oracle: template.oracle,
        collateral_reserve: template.collateral_reserve,
        collateral_mint: template.collateral_mint,
        collateral_custody: derive_associated_token_account(
            vault,
            collateral_mint,
            Pubkey::from_str(TOKEN)?,
        ),
        collateral_liquidity_supply: template.collateral_liquidity_supply,
        collateral_receipt_mint: template.collateral_receipt_mint,
        collateral_mint_supply: template.collateral_mint_supply,
        collateral_farm_state: template.collateral_farm_state,
        collateral_farm_user: template
            .collateral_farm_state
            .map(Pubkey::from_str)
            .transpose()?
            .map(|farm| derive_kamino_obligation_farm_user_state(farm, obligation)),
        debt_reserve: template.debt_reserve,
        debt_mint: template.debt_mint,
        debt_token_program: template.debt_token_program,
        debt_custody: derive_associated_token_account(vault, debt_mint, debt_token_program),
        debt_liquidity_supply: template.debt_liquidity_supply,
        debt_fee_vault: template.debt_fee_vault,
        debt_farm_state: template.debt_farm_state,
        debt_farm_user: farm_user,
        obligation,
        target_ltv_bps: template.target_ltv_bps,
        collateral_policy: policy(settings, seeds.collateral),
        debt_policy: policy(settings, seeds.debt),
        swap_policy: policy(settings, seeds.swap),
    })
}

fn policy(settings: Pubkey, seed: u64) -> PolicyConfig {
    PolicyConfig {
        seed,
        account: derive_action_account(&settings, seed).0,
    }
}

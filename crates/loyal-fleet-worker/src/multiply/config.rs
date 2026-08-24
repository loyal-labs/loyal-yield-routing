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
pub const FARMS: &str = "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr";
pub const TOKEN: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
pub const TOKEN_2022: &str = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb";
pub const SYRUP_MINT: &str = "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj";
pub const USDC_MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
pub const PYUSD_MINT: &str = "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo";

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
    pub strategies: [StrategyConfig; 2],
}

impl EarnMaxTopology {
    pub fn strategy(self, key: StrategyKey) -> StrategyConfig {
        match key {
            StrategyKey::SyrupUsdcUsdc => self.strategies[0],
            StrategyKey::SyrupUsdcPyusd => self.strategies[1],
        }
    }

    pub fn strategy_catalog(self) -> [StrategyConfig; 2] {
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

const COMMON_MARKET: &str = "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y";
const COMMON_MARKET_AUTHORITY: &str = "6QbtpY2jDNcncRFmVf343NThnCdaY8gCAsYATPnYQR9g";
const COMMON_ORACLE: &str = "3t4JZcueEzTbVP6kLxXrL3VpWx45jDer4eqysweBchNH";
const COLLATERAL_RESERVE: &str = "AwCyCPZYJSZ93xcVKNK7jR8e1BHzJXq1D4bReNuh9woY";
const COLLATERAL_LIQUIDITY_SUPPLY: &str = "8Se5SK1Tty2bH4EQVrKW8hwr9Lc9E2cEbkaN59DpcB6i";
const COLLATERAL_MINT_SUPPLY: &str = "21GK6yHS3MKhTnF5pN5FuSmnpLiyPXTDrpxxbqMEoX58";

const SYRUP_USDC_USDC_TEMPLATE: StrategyTemplate = StrategyTemplate {
    key: StrategyKey::SyrupUsdcUsdc,
    market: COMMON_MARKET,
    market_authority: COMMON_MARKET_AUTHORITY,
    oracle: COMMON_ORACLE,
    collateral_reserve: COLLATERAL_RESERVE,
    collateral_mint: SYRUP_MINT,
    collateral_liquidity_supply: COLLATERAL_LIQUIDITY_SUPPLY,
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
    let collateral_mint = Pubkey::from_str(SYRUP_MINT)?;
    let usdc_mint = Pubkey::from_str(USDC_MINT)?;
    let token = Pubkey::from_str(TOKEN)?;
    let claim_custody = derive_associated_token_account(vault, usdc_mint, token);
    let collateral_custody = derive_associated_token_account(vault, collateral_mint, token);
    Ok(EarnMaxTopology {
        manifest_version: MANIFEST_VERSION,
        settings,
        vault_index: EARN_MAX_VAULT_INDEX,
        vault,
        claim_custody,
        collateral_custody,
        strategies: [
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

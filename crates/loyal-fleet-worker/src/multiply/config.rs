use loyal_yield_store::fleet_orchestration::{MultiplyAction, StrategyKey};

pub const MAINNET_GENESIS_HASH: &str = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d";
pub const SETTINGS: &str = "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6";
pub const VAULT: &str = "ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh";
pub const DELEGATE: &str = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5";
pub const KLEND: &str = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD";
pub const JUPITER: &str = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4";
pub const FARMS: &str = "FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr";
pub const TOKEN: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
pub const TOKEN_2022: &str = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb";
pub const SYRUP_MINT: &str = "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj";
pub const SYRUP_CUSTODY: &str = "CYwM28WSoYp85HrQGuaVpWy2JhKH6JJah4m65DSWUNiN";
pub const USDC_MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
pub const USDC_CUSTODY: &str = "EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe";
pub const PYUSD_MINT: &str = "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo";
pub const PYUSD_CUSTODY: &str = "J4YFQzxhQ3pht2RRYes5yv1spPYBqvHzxn4zMX7iriHn";
pub const CLAIM_POLICY: PolicyConfig = PolicyConfig {
    seed: 43,
    account: "AwFR2cqo3PeUEetjDroixHFq35XNfCHSeZ2cia34fsZS",
};

#[derive(Clone, Copy, Debug)]
pub struct PolicyConfig {
    pub seed: u64,
    pub account: &'static str,
}

#[derive(Clone, Copy, Debug)]
pub struct StrategyConfig {
    pub key: StrategyKey,
    pub market: &'static str,
    pub market_authority: &'static str,
    pub oracle: &'static str,
    pub collateral_reserve: &'static str,
    pub collateral_mint: &'static str,
    pub collateral_custody: &'static str,
    pub collateral_liquidity_supply: &'static str,
    pub collateral_mint_supply: &'static str,
    pub collateral_farm_state: Option<&'static str>,
    pub collateral_farm_user: Option<&'static str>,
    pub debt_reserve: &'static str,
    pub debt_mint: &'static str,
    pub debt_token_program: &'static str,
    pub debt_custody: &'static str,
    pub debt_liquidity_supply: &'static str,
    pub debt_fee_vault: &'static str,
    pub debt_farm_state: Option<&'static str>,
    pub debt_farm_user: Option<&'static str>,
    pub obligation: &'static str,
    pub target_ltv_bps: u16,
    pub deposit_policy: PolicyConfig,
    pub borrow_policy: PolicyConfig,
    pub claim_to_collateral_policy: PolicyConfig,
    pub debt_to_collateral_policy: PolicyConfig,
    pub collateral_to_debt_policy: PolicyConfig,
    pub collateral_to_claim_policy: PolicyConfig,
    pub repay_policy: PolicyConfig,
    pub withdraw_policy: PolicyConfig,
}

impl StrategyConfig {
    pub fn policy(self, action: MultiplyAction) -> Option<PolicyConfig> {
        match action {
            MultiplyAction::DepositCollateral => Some(self.deposit_policy),
            MultiplyAction::BorrowDebt => Some(self.borrow_policy),
            MultiplyAction::SwapClaimToCollateral => Some(self.claim_to_collateral_policy),
            MultiplyAction::SwapDebtToCollateral => Some(self.debt_to_collateral_policy),
            MultiplyAction::SwapCollateralToDebt => Some(self.collateral_to_debt_policy),
            MultiplyAction::SwapCollateralToClaim => Some(self.collateral_to_claim_policy),
            MultiplyAction::RepayDebt => Some(self.repay_policy),
            MultiplyAction::WithdrawCollateral | MultiplyAction::WithdrawRemainingCollateral => {
                Some(self.withdraw_policy)
            }
            MultiplyAction::Claim | MultiplyAction::DepositClaimAsset => None,
        }
    }
}

pub const SYRUP_USDC_USDC: StrategyConfig = StrategyConfig {
    key: StrategyKey::SyrupUsdcUsdc,
    market: "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y",
    market_authority: "6QbtpY2jDNcncRFmVf343NThnCdaY8gCAsYATPnYQR9g",
    oracle: "3t4JZcueEzTbVP6kLxXrL3VpWx45jDer4eqysweBchNH",
    collateral_reserve: "AwCyCPZYJSZ93xcVKNK7jR8e1BHzJXq1D4bReNuh9woY",
    collateral_mint: SYRUP_MINT,
    collateral_custody: SYRUP_CUSTODY,
    collateral_liquidity_supply: "8Se5SK1Tty2bH4EQVrKW8hwr9Lc9E2cEbkaN59DpcB6i",
    collateral_mint_supply: "21GK6yHS3MKhTnF5pN5FuSmnpLiyPXTDrpxxbqMEoX58",
    collateral_farm_state: None,
    collateral_farm_user: None,
    debt_reserve: "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo",
    debt_mint: USDC_MINT,
    debt_token_program: TOKEN,
    debt_custody: USDC_CUSTODY,
    debt_liquidity_supply: "BBcwMNSMyhhBnYE9pevEvkxKHGzTafMP9v3j7Kk7nAWM",
    debt_fee_vault: "HH7GLnRcGHJrdkEueVVj7mccNUjnSeWobDmtu9cHLkJV",
    debt_farm_state: Some("87gUNr8LwYJCT25HjPEHnrfBBjwEMAjfqCfnKcJNqy9Y"),
    debt_farm_user: Some("CcUorNoacydFVu7SHmhsA1qi9CcEu8K5YFvuS8unAzgr"),
    obligation: "Gtwj2FNuiPoV2mGLC5SpHZ9PCmDrHHKaHXtacRaqm8vT",
    target_ltv_bps: 6_500,
    deposit_policy: PolicyConfig {
        seed: 32,
        account: "4Dsc7yekLPNijqLG23xyZC1oZH8kNfLhi959kfb7igAs",
    },
    borrow_policy: PolicyConfig {
        seed: 34,
        account: "H1eZg6dNJN6nCFy9dUS7y1qigL4rJjpHi5V5GxmgEKEY",
    },
    claim_to_collateral_policy: PolicyConfig {
        seed: 35,
        account: "C1Utas4YohFYscbq6Pq9PPudNppnbdC7CtSNtKPQsvvg",
    },
    debt_to_collateral_policy: PolicyConfig {
        seed: 35,
        account: "C1Utas4YohFYscbq6Pq9PPudNppnbdC7CtSNtKPQsvvg",
    },
    collateral_to_debt_policy: PolicyConfig {
        seed: 44,
        account: "4GraDkpzoX5VrtB1JaHzkWLrW8buUt9REqAF49ztvbmo",
    },
    collateral_to_claim_policy: PolicyConfig {
        seed: 44,
        account: "4GraDkpzoX5VrtB1JaHzkWLrW8buUt9REqAF49ztvbmo",
    },
    repay_policy: PolicyConfig {
        seed: 33,
        account: "9CrvH31Mc3nCZo8F5qVvDDb4s8LYRAX8CtJV96YCj1wF",
    },
    withdraw_policy: PolicyConfig {
        seed: 36,
        account: "AnjRBTcjNXFstcVS6ZHxVxVPCveMLfzGPVkJqEvVpsQK",
    },
};

pub const SYRUP_USDC_PYUSD: StrategyConfig = StrategyConfig {
    key: StrategyKey::SyrupUsdcPyusd,
    debt_reserve: "92qeAka3ZzCGPfJriDXrE7tiNqfATVCAM6ZjjctR3TrS",
    debt_mint: PYUSD_MINT,
    debt_token_program: TOKEN_2022,
    debt_custody: PYUSD_CUSTODY,
    debt_liquidity_supply: "GUENeLN1ufX4K5622DbyYoQFhaWxMKoCFycvLSEYsykN",
    debt_fee_vault: "AwnzukUiajn7b3T9hXcwy19RLPZcHmLANUeqZnzXT6dU",
    debt_farm_state: Some("9AUA7XZ1rynUsZcmVCgj8UFdQuDozFSMpaNGBZAtiPWj"),
    debt_farm_user: Some("6td42LpC6MtM3JRsofxM1Rm5MRCJJfxxRkKDP76Qd6q4"),
    obligation: "ANhCkVi4siA36zbDKxszh8xKg8totzjwX6GXGzoxbvue",
    target_ltv_bps: 8_000,
    deposit_policy: PolicyConfig {
        seed: 40,
        account: "HbLEjju24EUVSi57fhh1LkDQVy5LDrMxiLuRT3uKZjSF",
    },
    borrow_policy: PolicyConfig {
        seed: 38,
        account: "3AV8BYtWkD6ezw6oReN9EAqYKm3oTDhuqwxRBqvW2HAj",
    },
    claim_to_collateral_policy: PolicyConfig {
        seed: 37,
        account: "CF1XNboJmpYHQRBZ9766hNhsp1QNeXANtjkz2ndHjfVR",
    },
    debt_to_collateral_policy: PolicyConfig {
        seed: 39,
        account: "B8zNwPiZTBmSeTrtWSh1H92mgZbkTyTjgJxoaVtKF17K",
    },
    collateral_to_debt_policy: PolicyConfig {
        seed: 45,
        account: "Hw6yXAwFGGs19dM5uCqjEVPQ3AmSZJnUxsQ9d23w8Z9s",
    },
    collateral_to_claim_policy: PolicyConfig {
        seed: 46,
        account: "6j3ioh1ePD1Fzew8sJLbaVqQvSqw5SFYKapQ9AvX6aNy",
    },
    repay_policy: PolicyConfig {
        seed: 41,
        account: "AZM2YGL3NzCLv1beFL9uYhAKfcGEJ7fXeh35gETymbev",
    },
    withdraw_policy: PolicyConfig {
        seed: 42,
        account: "2fKvAbh91VGF6oXptQLQYrpMvdLR8UMHx35tmWA2aGQo",
    },
    ..SYRUP_USDC_USDC
};

pub const STRATEGIES: &[StrategyConfig] = &[SYRUP_USDC_USDC, SYRUP_USDC_PYUSD];

pub fn strategy(key: StrategyKey) -> StrategyConfig {
    match key {
        StrategyKey::SyrupUsdcUsdc => SYRUP_USDC_USDC,
        StrategyKey::SyrupUsdcPyusd => SYRUP_USDC_PYUSD,
    }
}

use crate::{
    CASH_MINT, PYUSD_MINT, TOKEN_2022_PROGRAM_ID, USDC_MINT, USDG_MINT, USDS_MINT, USDT_MINT,
};
use solana_sdk::pubkey::Pubkey;

/// Stablecoin metadata shared by Earn policy construction and route execution.
///
/// Keep this list aligned with the assets an Earn policy can authorize. The
/// ordered pair generator below is the only V1 cross-mint pair registry.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct EarnStablecoin {
    pub symbol: &'static str,
    pub mint: Pubkey,
    pub token_program: Pubkey,
    pub decimals: u8,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct EarnStablecoinPair {
    pub input_mint: Pubkey,
    pub output_mint: Pubkey,
}

impl EarnStablecoinPair {
    pub fn new(input_mint: Pubkey, output_mint: Pubkey) -> Option<Self> {
        (input_mint != output_mint).then_some(Self {
            input_mint,
            output_mint,
        })
    }

    pub fn reversed(self) -> Self {
        Self {
            input_mint: self.output_mint,
            output_mint: self.input_mint,
        }
    }
}

pub const EARN_STABLECOINS: [EarnStablecoin; 6] = [
    EarnStablecoin {
        symbol: "CASH",
        mint: CASH_MINT,
        token_program: TOKEN_2022_PROGRAM_ID,
        decimals: 6,
    },
    EarnStablecoin {
        symbol: "USDG",
        mint: USDG_MINT,
        token_program: TOKEN_2022_PROGRAM_ID,
        decimals: 6,
    },
    EarnStablecoin {
        symbol: "PYUSD",
        mint: PYUSD_MINT,
        token_program: TOKEN_2022_PROGRAM_ID,
        decimals: 6,
    },
    EarnStablecoin {
        symbol: "USDC",
        mint: USDC_MINT,
        token_program: spl_token::ID,
        decimals: 6,
    },
    EarnStablecoin {
        symbol: "USDT",
        mint: USDT_MINT,
        token_program: spl_token::ID,
        decimals: 6,
    },
    EarnStablecoin {
        symbol: "USDS",
        mint: USDS_MINT,
        token_program: spl_token::ID,
        decimals: 6,
    },
];

pub fn earn_stablecoins() -> &'static [EarnStablecoin; 6] {
    &EARN_STABLECOINS
}

pub fn earn_stablecoin(mint: Pubkey) -> Option<&'static EarnStablecoin> {
    EARN_STABLECOINS.iter().find(|asset| asset.mint == mint)
}

pub fn earn_stablecoin_pairs() -> Vec<EarnStablecoinPair> {
    EARN_STABLECOINS
        .iter()
        .flat_map(|input| {
            EARN_STABLECOINS
                .iter()
                .filter_map(move |output| EarnStablecoinPair::new(input.mint, output.mint))
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::BTreeSet;

    #[test]
    fn canonical_earn_registry_generates_every_directed_non_self_pair_once() {
        let pairs = earn_stablecoin_pairs();
        assert_eq!(pairs.len(), 30);
        assert_eq!(pairs.iter().copied().collect::<BTreeSet<_>>().len(), 30);
        assert!(pairs.iter().all(|pair| pair.input_mint != pair.output_mint));
        assert!(pairs.iter().all(|pair| pairs.contains(&pair.reversed())));
        assert!(pairs.iter().all(|pair| {
            earn_stablecoin(pair.input_mint).is_some()
                && earn_stablecoin(pair.output_mint).is_some()
        }));
    }
}

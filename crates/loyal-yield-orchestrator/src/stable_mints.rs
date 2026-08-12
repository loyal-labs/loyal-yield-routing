use loyal_actions::{CASH_MINT, PYUSD_MINT, USDC_MINT, USDG_MINT, USDS_MINT, USDT_MINT};
use sha2::{Digest, Sha256};
use std::{collections::BTreeSet, env, str::FromStr};
use thiserror::Error;

pub const ENABLED_STABLE_MINTS_ENV: &str = "EARN_ROUTER_ENABLED_STABLE_MINTS";
const TOKEN_2022_PROGRAM: &str = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb";

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EarnAsset {
    pub symbol: &'static str,
    pub mint: solana_sdk::pubkey::Pubkey,
    pub token_program: solana_sdk::pubkey::Pubkey,
    pub decimals: u8,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct EarnUniverse {
    pub assets: Vec<EarnAsset>,
}

impl EarnUniverse {
    pub fn canonical() -> Self {
        let token = spl_token::ID;
        let token_2022 = solana_sdk::pubkey::Pubkey::from_str(TOKEN_2022_PROGRAM)
            .expect("Token-2022 program id");
        Self {
            assets: [
                ("CASH", CASH_MINT, token_2022),
                ("USDG", USDG_MINT, token_2022),
                ("PYUSD", PYUSD_MINT, token_2022),
                ("USDC", USDC_MINT, token),
                ("USDT", USDT_MINT, token),
                ("USDS", USDS_MINT, token),
            ]
            .into_iter()
            .map(|(symbol, mint, token_program)| EarnAsset {
                symbol,
                mint,
                token_program,
                decimals: 6,
            })
            .collect(),
        }
    }

    pub fn asset(&self, mint: &str) -> Option<&EarnAsset> {
        self.assets
            .iter()
            .find(|asset| asset.mint.to_string() == mint)
    }

    pub fn mints(&self) -> Vec<String> {
        self.assets
            .iter()
            .map(|asset| asset.mint.to_string())
            .collect()
    }
}

#[derive(Debug, Clone, PartialEq, Eq, Error)]
pub enum StableMintConfigError {
    #[error("{ENABLED_STABLE_MINTS_ENV} contains unsupported stable mint {mint}")]
    UnsupportedMint { mint: String },
    #[error("{ENABLED_STABLE_MINTS_ENV} contains a duplicate stable mint {mint}")]
    DuplicateMint { mint: String },
    #[error("{ENABLED_STABLE_MINTS_ENV} contains an empty stable mint")]
    EmptyMint,
}

pub fn supported_stable_mints() -> Vec<String> {
    EarnUniverse::canonical().mints()
}

pub fn supported_idle_deposit_mints() -> Vec<String> {
    EarnUniverse::canonical().mints()
}

pub fn enabled_stable_mints_from_env() -> Result<Vec<String>, StableMintConfigError> {
    resolve_enabled_stable_mints(env::var(ENABLED_STABLE_MINTS_ENV).ok().as_deref())
}

pub fn enabled_stable_mints_hash(
    enabled_mints: &[String],
) -> Result<String, StableMintConfigError> {
    let canonical = resolve_enabled_stable_mints(Some(&enabled_mints.join(",")))?;
    let mut hasher = Sha256::new();
    for mint in canonical {
        hasher.update((mint.len() as u64).to_le_bytes());
        hasher.update(mint.as_bytes());
    }
    Ok(format!("{:x}", hasher.finalize()))
}

pub fn resolve_enabled_stable_mints(
    configured: Option<&str>,
) -> Result<Vec<String>, StableMintConfigError> {
    let supported_mints = supported_stable_mints();
    let configured = configured.map(str::trim).filter(|value| !value.is_empty());
    let Some(configured) = configured else {
        return Ok(vec![USDC_MINT.to_string()]);
    };
    let supported = supported_mints
        .iter()
        .map(String::as_str)
        .collect::<BTreeSet<_>>();
    let mut configured_set = BTreeSet::new();
    for mint in configured.split(',').map(str::trim) {
        if mint.is_empty() {
            return Err(StableMintConfigError::EmptyMint);
        }
        if !supported.contains(mint) {
            return Err(StableMintConfigError::UnsupportedMint {
                mint: mint.to_owned(),
            });
        }
        if !configured_set.insert(mint) {
            return Err(StableMintConfigError::DuplicateMint {
                mint: mint.to_owned(),
            });
        }
    }
    Ok(supported_mints
        .into_iter()
        .filter(|mint| configured_set.contains(mint.as_str()))
        .collect())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn canonical_universe_has_exact_mints_and_programs() {
        let universe = EarnUniverse::canonical();
        assert_eq!(universe.assets.len(), 6);
        assert_eq!(
            universe
                .assets
                .iter()
                .map(|asset| asset.symbol)
                .collect::<Vec<_>>(),
            vec!["CASH", "USDG", "PYUSD", "USDC", "USDT", "USDS"]
        );
        assert!(universe.assets[..3]
            .iter()
            .all(|asset| asset.token_program != spl_token::ID));
        assert!(universe.assets[3..]
            .iter()
            .all(|asset| asset.token_program == spl_token::ID));
        assert!(universe.assets.iter().all(|asset| asset.decimals == 6));
    }

    #[test]
    fn stable_mint_subset_is_canonical_and_rejects_unknowns() {
        let configured = format!(" {USDC_MINT}, {PYUSD_MINT} ");
        assert_eq!(
            resolve_enabled_stable_mints(Some(&configured)).unwrap(),
            vec![PYUSD_MINT.to_string(), USDC_MINT.to_string()]
        );
        assert!(matches!(
            resolve_enabled_stable_mints(Some("not-a-supported-mint")),
            Err(StableMintConfigError::UnsupportedMint { .. })
        ));
    }

    #[test]
    fn stable_mint_rollout_defaults_to_usdc_and_requires_explicit_expansion() {
        assert_eq!(
            resolve_enabled_stable_mints(None).unwrap(),
            vec![USDC_MINT.to_string()]
        );
        assert_eq!(
            resolve_enabled_stable_mints(Some("  ")).unwrap(),
            vec![USDC_MINT.to_string()]
        );
        assert_eq!(
            resolve_enabled_stable_mints(Some(&supported_stable_mints().join(","))).unwrap(),
            supported_stable_mints()
        );
    }

    #[test]
    fn stable_mint_rollout_rejects_duplicates_and_empty_entries() {
        let duplicate = format!("{USDC_MINT},{USDC_MINT}");
        assert!(matches!(
            resolve_enabled_stable_mints(Some(&duplicate)),
            Err(StableMintConfigError::DuplicateMint { .. })
        ));
        let empty_entry = format!("{USDC_MINT},,{PYUSD_MINT}");
        assert!(matches!(
            resolve_enabled_stable_mints(Some(&empty_entry)),
            Err(StableMintConfigError::EmptyMint)
        ));
    }
}

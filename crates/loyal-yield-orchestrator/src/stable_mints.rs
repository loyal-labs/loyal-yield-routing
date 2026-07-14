use loyal_actions::{CASH_MINT, PYUSD_MINT, USDC_MINT, USDG_MINT, USDS_MINT, USDT_MINT};
use sha2::{Digest, Sha256};
use std::{collections::BTreeSet, env};
use thiserror::Error;

pub const ENABLED_STABLE_MINTS_ENV: &str = "EARN_ROUTER_ENABLED_STABLE_MINTS";

#[derive(Debug, Clone, PartialEq, Eq, Error)]
pub enum StableMintConfigError {
    #[error("{ENABLED_STABLE_MINTS_ENV} contains unsupported stable mint {mint}")]
    UnsupportedMint { mint: String },
    #[error("{ENABLED_STABLE_MINTS_ENV} did not contain any mints")]
    EmptySelection,
}

pub fn supported_stable_mints() -> Vec<String> {
    [
        CASH_MINT, USDG_MINT, PYUSD_MINT, USDC_MINT, USDT_MINT, USDS_MINT,
    ]
    .into_iter()
    .map(|mint| mint.to_string())
    .collect()
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
    let Some(configured) = configured else {
        return Ok(supported_mints);
    };
    let supported = supported_mints
        .iter()
        .map(String::as_str)
        .collect::<BTreeSet<_>>();
    let mut configured_set = BTreeSet::new();
    for mint in configured
        .split(',')
        .map(str::trim)
        .filter(|mint| !mint.is_empty())
    {
        if !supported.contains(mint) {
            return Err(StableMintConfigError::UnsupportedMint {
                mint: mint.to_owned(),
            });
        }
        configured_set.insert(mint);
    }
    if configured_set.is_empty() {
        return Err(StableMintConfigError::EmptySelection);
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
    fn stable_mint_default_is_the_canonical_six() {
        let resolved = resolve_enabled_stable_mints(None).unwrap();
        assert_eq!(resolved, supported_stable_mints());
        assert_eq!(resolved.len(), 6);
        assert_eq!(resolved.iter().collect::<BTreeSet<_>>().len(), 6);
    }

    #[test]
    fn stable_mint_subset_uses_canonical_order_and_deduplicates() {
        let configured = format!(" {USDC_MINT}, {PYUSD_MINT}, {USDC_MINT} ");
        assert_eq!(
            resolve_enabled_stable_mints(Some(&configured)).unwrap(),
            vec![PYUSD_MINT.to_string(), USDC_MINT.to_string()]
        );
        let reverse = format!("{PYUSD_MINT},{USDC_MINT}");
        assert_eq!(
            resolve_enabled_stable_mints(Some(&configured)).unwrap(),
            resolve_enabled_stable_mints(Some(&reverse)).unwrap()
        );
        assert_eq!(
            enabled_stable_mints_hash(&[
                USDC_MINT.to_string(),
                PYUSD_MINT.to_string(),
                USDC_MINT.to_string(),
            ])
            .unwrap(),
            enabled_stable_mints_hash(&[PYUSD_MINT.to_string(), USDC_MINT.to_string()]).unwrap()
        );
    }

    #[test]
    fn stable_mint_subset_rejects_empty_or_unknown_values() {
        assert_eq!(
            resolve_enabled_stable_mints(Some(" , ")).unwrap_err(),
            StableMintConfigError::EmptySelection
        );
        assert_eq!(
            resolve_enabled_stable_mints(Some("not-a-supported-mint")).unwrap_err(),
            StableMintConfigError::UnsupportedMint {
                mint: "not-a-supported-mint".to_owned(),
            }
        );
    }
}

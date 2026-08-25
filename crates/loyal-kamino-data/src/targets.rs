use std::{
    collections::{BTreeSet, HashMap, HashSet},
    str::FromStr,
    time::Duration,
};

use anyhow::{anyhow, bail, Context, Result};
use klend_interface::KLEND_PROGRAM_ID;
use loyal_actions::{
    AUSD_MINT, CASH_MINT, EARN_MAX_OBSERVATION_RESERVES, EUSX_MINT, FDUSD_MINT,
    KAMINO_ALTCOINS_MARKET, KAMINO_BITCOIN_MARKET, KAMINO_ETHENA_MARKET, KAMINO_FIGURE_MARKET,
    KAMINO_HUMA_MARKET, KAMINO_JLP_MARKET, KAMINO_MAIN_MARKET, KAMINO_MAIN_USDC_RESERVE,
    KAMINO_MAPLE_MARKET, KAMINO_ONRE_MARKET, KAMINO_SOLSTICE_MARKET,
    KAMINO_SUPERSTATE_OPENING_BELL_MARKET, KAMINO_XSTOCKS_MARKET, PYUSD_MINT, SUSDE_MINT,
    SYRUP_USDC_MINT, USCC_MINT, USD1_MINT, USDC_MINT, USDE_MINT, USDG_MINT, USDS_MINT, USDT_MINT,
};
pub use loyal_kamino_codec::{ReserveTarget, SupportedReserveRecord};
use reqwest::blocking::Client;
use serde::{de, Deserialize, Deserializer};
use solana_sdk::pubkey::Pubkey;

#[derive(Debug, Clone)]
pub struct SupportedMarket {
    pub market: Pubkey,
    pub name: &'static str,
    pub risk_baskets: &'static [&'static str],
}

#[derive(Debug, Clone)]
pub struct SupportedMint {
    pub mint: Pubkey,
    pub symbol: &'static str,
}

pub struct KaminoApi {
    client: Client,
    base_url: String,
}

#[derive(Debug, Deserialize)]
struct MarketDto {
    #[serde(rename = "lendingMarket", alias = "pubkey", alias = "address")]
    lending_market: String,
    #[serde(default)]
    name: Option<String>,
}

#[derive(Debug, Deserialize)]
struct ReserveMetricDto {
    #[serde(default)]
    reserve: Option<String>,
    #[serde(default, rename = "liquidityToken", alias = "symbol", alias = "token")]
    liquidity_token: Option<String>,
    #[serde(
        default,
        rename = "liquidityTokenMint",
        alias = "mint",
        alias = "mintAddress"
    )]
    liquidity_token_mint: Option<String>,
    #[serde(
        default,
        rename = "supplyApy",
        alias = "supplyAPY",
        deserialize_with = "deserialize_optional_f64"
    )]
    supply_apy: Option<f64>,
    #[serde(
        default,
        rename = "borrowApy",
        alias = "borrowAPY",
        deserialize_with = "deserialize_optional_f64"
    )]
    borrow_apy: Option<f64>,
    #[serde(
        default,
        rename = "totalSupplyUsd",
        alias = "depositTvl",
        alias = "totalDepositUsd",
        deserialize_with = "deserialize_optional_f64"
    )]
    total_supply_usd: Option<f64>,
    #[serde(
        default,
        rename = "totalBorrowUsd",
        alias = "borrowTvl",
        deserialize_with = "deserialize_optional_f64"
    )]
    total_borrow_usd: Option<f64>,
}

#[derive(Debug, Deserialize)]
struct SlotDurationResponse {
    #[serde(
        default,
        rename = "recentSlotDurationInMs",
        alias = "recent_slot_duration_in_ms",
        deserialize_with = "deserialize_optional_f64"
    )]
    recent_slot_duration_in_ms: Option<f64>,
    #[serde(
        default,
        rename = "medianSlotDurationMs",
        alias = "median_slot_duration_ms",
        deserialize_with = "deserialize_optional_f64"
    )]
    median_slot_duration_ms: Option<f64>,
    #[serde(
        default,
        rename = "slotDurationMs",
        alias = "slot_duration_ms",
        deserialize_with = "deserialize_optional_f64"
    )]
    slot_duration_ms: Option<f64>,
    #[serde(default, deserialize_with = "deserialize_optional_f64")]
    duration: Option<f64>,
}

impl KaminoApi {
    pub fn new(base_url: String, timeout: Duration) -> Result<Self> {
        let client = Client::builder()
            .timeout(timeout)
            .build()
            .context("build Kamino HTTP client")?;
        Ok(Self { client, base_url })
    }

    pub fn fetch_slot_duration_ms(&self) -> Result<f64> {
        let url = format!("{}/slots/duration", self.base_url());
        let response = self
            .client
            .get(url)
            .send()
            .context("request Kamino slot duration")?
            .error_for_status()
            .context("Kamino slot duration status")?
            .json::<SlotDurationResponse>()
            .context("decode Kamino slot duration JSON")?;

        response.duration_ms().ok_or_else(|| {
            anyhow!("slot duration response did not contain a recognizable duration field")
        })
    }

    pub fn fetch_loyal_targets(&self, requested_reserves: &[Pubkey]) -> Result<Vec<ReserveTarget>> {
        let mut targets = self.fetch_all_targets(&policy_supported_market_pubkeys())?;
        let allowed_markets = policy_supported_market_pubkeys()
            .into_iter()
            .collect::<HashSet<_>>();
        let allowed_mints = policy_supported_mint_pubkeys()
            .into_iter()
            .collect::<HashSet<_>>();

        targets.retain(|target| {
            target
                .market
                .is_some_and(|market| allowed_markets.contains(&market))
                && target
                    .liquidity_mint
                    .is_some_and(|mint| allowed_mints.contains(&mint))
        });

        if !requested_reserves.is_empty() {
            let requested = requested_reserves.iter().copied().collect::<HashSet<_>>();
            targets.retain(|target| requested.contains(&target.reserve));
            let mut by_reserve = targets
                .into_iter()
                .map(|target| (target.reserve, target))
                .collect::<HashMap<_, _>>();
            return Ok(requested_reserves
                .iter()
                .map(|reserve| {
                    by_reserve
                        .remove(reserve)
                        .unwrap_or_else(|| target_from_requested_reserve(*reserve))
                })
                .collect());
        }

        targets.sort_by_key(|target| {
            (
                target.reserve != KAMINO_MAIN_USDC_RESERVE,
                target
                    .market
                    .map(|market| market.to_string())
                    .unwrap_or_default(),
                target
                    .liquidity_mint
                    .map(|mint| mint.to_string())
                    .unwrap_or_default(),
                target.reserve.to_string(),
            )
        });

        Ok(targets)
    }

    pub fn fetch_supported_reserves(&self) -> Result<Vec<SupportedReserveRecord>> {
        let supported_mints = policy_supported_mints()
            .into_iter()
            .map(|mint| (mint.mint, mint))
            .collect::<HashMap<_, _>>();
        let mut records_by_pair = HashMap::<(Pubkey, Pubkey), (SupportedReserveRecord, f64)>::new();

        for supported_market in policy_supported_markets() {
            let market = MarketDto {
                lending_market: supported_market.market.to_string(),
                name: Some(supported_market.name.to_string()),
            };
            let targets = self
                .fetch_targets_for_market(market)
                .with_context(|| format!("fetch supported market {}", supported_market.market))?;

            for target in targets {
                let Some(liquidity_mint) = target.liquidity_mint else {
                    continue;
                };
                let Some(supported_mint) = supported_mints.get(&liquidity_mint) else {
                    continue;
                };
                let pair = (supported_market.market, liquidity_mint);
                let score = target.api_total_supply_usd.unwrap_or(0.0);
                let record = SupportedReserveRecord {
                    reserve: target.reserve,
                    market: supported_market.market,
                    market_name: target
                        .market_name
                        .or_else(|| Some(supported_market.name.to_string())),
                    symbol: target
                        .symbol
                        .or_else(|| Some(supported_mint.symbol.to_string())),
                    liquidity_mint,
                    risk_baskets: supported_market
                        .risk_baskets
                        .iter()
                        .map(|basket| (*basket).to_string())
                        .collect(),
                };
                match records_by_pair.entry(pair) {
                    std::collections::hash_map::Entry::Occupied(mut entry) => {
                        if score > entry.get().1 {
                            entry.insert((record, score));
                        }
                    }
                    std::collections::hash_map::Entry::Vacant(entry) => {
                        entry.insert((record, score));
                    }
                }
            }
        }

        let mut records = records_by_pair
            .into_values()
            .map(|(record, _)| record)
            .collect::<Vec<_>>();
        records.sort_by_key(|record| {
            (
                record.market.to_string(),
                record.liquidity_mint.to_string(),
                record.reserve.to_string(),
            )
        });
        Ok(records)
    }

    /// Resolve the exact reserve identities required by the seven approved RWA
    /// loops without adding RWA collateral mints to the stable Earn catalog.
    pub fn fetch_earn_max_observation_targets(&self) -> Result<Vec<ReserveTarget>> {
        let required_markets = EARN_MAX_OBSERVATION_RESERVES
            .iter()
            .map(|entry| Pubkey::from_str(entry.market).context("invalid Earn Max market"))
            .collect::<Result<BTreeSet<_>>>()?;
        let market_names = policy_supported_markets()
            .into_iter()
            .map(|market| (market.market, market.name))
            .collect::<HashMap<_, _>>();
        let mut candidates = Vec::new();

        for market in required_markets {
            let name = market_names
                .get(&market)
                .copied()
                .ok_or_else(|| anyhow!("Earn Max observation market {market} is not supported"))?;
            candidates.extend(self.fetch_targets_for_market(MarketDto {
                lending_market: market.to_string(),
                name: Some(name.to_string()),
            })?);
        }

        select_earn_max_observation_targets(candidates)
    }

    fn fetch_all_targets(&self, requested_markets: &[Pubkey]) -> Result<Vec<ReserveTarget>> {
        let requested = requested_markets
            .iter()
            .map(ToString::to_string)
            .collect::<HashSet<_>>();
        let mut markets = self.fetch_markets()?;
        markets.retain(|market| requested.contains(&market.lending_market));

        let mut targets = Vec::new();
        for market in markets {
            match self.fetch_targets_for_market(market) {
                Ok(mut market_targets) => targets.append(&mut market_targets),
                Err(err) => tracing::warn!(error = %err, "skipping Kamino market during discovery"),
            }
        }
        Ok(targets)
    }

    fn fetch_targets_for_market(&self, market: MarketDto) -> Result<Vec<ReserveTarget>> {
        let market_pubkey = Pubkey::from_str(&market.lending_market)
            .with_context(|| format!("market has invalid pubkey {}", market.lending_market))?;
        let metrics = self.fetch_market_metrics(&market.lending_market)?;
        let mut targets = Vec::new();

        for metric in metrics {
            let Some(reserve) = metric.reserve.and_then(|s| Pubkey::from_str(&s).ok()) else {
                continue;
            };
            targets.push(ReserveTarget {
                reserve,
                market: Some(market_pubkey),
                market_name: market.name.clone(),
                symbol: metric.liquidity_token.and_then(normalize_token_symbol),
                liquidity_mint: metric
                    .liquidity_token_mint
                    .and_then(|s| Pubkey::from_str(&s).ok()),
                api_supply_apy: metric.supply_apy,
                api_borrow_apy: metric.borrow_apy,
                api_total_supply_usd: metric.total_supply_usd,
                api_total_borrow_usd: metric.total_borrow_usd,
            });
        }

        Ok(targets)
    }

    fn fetch_markets(&self) -> Result<Vec<MarketDto>> {
        let url = format!(
            "{}/v2/kamino-market?programId={}",
            self.base_url(),
            KLEND_PROGRAM_ID
        );
        self.client
            .get(url)
            .send()
            .context("request Kamino markets")?
            .error_for_status()
            .context("Kamino markets status")?
            .json::<Vec<MarketDto>>()
            .context("decode Kamino markets JSON")
    }

    fn fetch_market_metrics(&self, market: &str) -> Result<Vec<ReserveMetricDto>> {
        let url = format!(
            "{}/kamino-market/{market}/reserves/metrics?env=mainnet-beta",
            self.base_url()
        );
        self.client
            .get(url)
            .send()
            .context("request Kamino reserve metrics")?
            .error_for_status()
            .context("Kamino reserve metrics status")?
            .json::<Vec<ReserveMetricDto>>()
            .context("decode Kamino reserve metrics JSON")
    }

    fn base_url(&self) -> &str {
        self.base_url.trim_end_matches('/')
    }
}

fn select_earn_max_observation_targets(
    candidates: Vec<ReserveTarget>,
) -> Result<Vec<ReserveTarget>> {
    let required = EARN_MAX_OBSERVATION_RESERVES
        .iter()
        .map(|entry| {
            Ok((
                Pubkey::from_str(entry.market).context("invalid Earn Max market")?,
                Pubkey::from_str(entry.reserve).context("invalid Earn Max reserve")?,
                Pubkey::from_str(entry.liquidity_mint).context("invalid Earn Max mint")?,
            ))
        })
        .collect::<Result<Vec<_>>>()?;
    let required_reserves = required
        .iter()
        .map(|(_, reserve, _)| *reserve)
        .collect::<BTreeSet<_>>();
    if required_reserves.len() != required.len() {
        bail!("Earn Max observation manifest contains duplicate reserves");
    }
    let mut by_reserve = HashMap::<Pubkey, Vec<ReserveTarget>>::new();

    for target in candidates {
        if required_reserves.contains(&target.reserve) {
            by_reserve.entry(target.reserve).or_default().push(target);
        }
    }

    let mut selected = Vec::with_capacity(required.len());
    for (market, reserve, mint) in required {
        let matches = by_reserve.remove(&reserve).unwrap_or_default();
        if matches.len() != 1 {
            bail!(
                "Earn Max observation reserve={reserve} resolved {} API rows; expected exactly one",
                matches.len()
            );
        }
        let target = matches.into_iter().next().expect("one target");
        if target.market != Some(market) || target.liquidity_mint != Some(mint) {
            bail!("Earn Max observation reserve={reserve} returned unexpected market or mint");
        }
        selected.push(target);
    }
    selected.sort_by_key(|target| target.reserve.to_string());
    Ok(selected)
}

pub fn resolve_loyal_targets(
    api: &KaminoApi,
    requested_reserves: &[Pubkey],
) -> Result<Vec<ReserveTarget>> {
    api.fetch_loyal_targets(requested_reserves)
}

pub fn loyal_safe_markets() -> Vec<Pubkey> {
    policy_supported_markets()
        .into_iter()
        .filter(|market| market.risk_baskets.contains(&"safe"))
        .map(|market| market.market)
        .collect()
}

pub fn loyal_monitor_stable_mints() -> Vec<Pubkey> {
    policy_supported_mint_pubkeys()
}

pub fn policy_supported_market_pubkeys() -> Vec<Pubkey> {
    policy_supported_markets()
        .into_iter()
        .map(|market| market.market)
        .collect()
}

pub fn policy_supported_mint_pubkeys() -> Vec<Pubkey> {
    policy_supported_mints()
        .into_iter()
        .map(|mint| mint.mint)
        .collect()
}

pub fn policy_supported_markets() -> Vec<SupportedMarket> {
    vec![
        SupportedMarket {
            market: KAMINO_MAIN_MARKET,
            name: "Main Market",
            risk_baskets: &["safe", "medium", "aggressive"],
        },
        SupportedMarket {
            market: KAMINO_FIGURE_MARKET,
            name: "Figure Market",
            risk_baskets: &["safe", "medium", "aggressive"],
        },
        SupportedMarket {
            market: KAMINO_MAPLE_MARKET,
            name: "Maple Market",
            risk_baskets: &["safe", "medium", "aggressive"],
        },
        SupportedMarket {
            market: KAMINO_ONRE_MARKET,
            name: "OnRe Market",
            risk_baskets: &["safe", "medium", "aggressive"],
        },
        SupportedMarket {
            market: KAMINO_ETHENA_MARKET,
            name: "Ethena Market",
            risk_baskets: &["safe", "medium", "aggressive"],
        },
        SupportedMarket {
            market: KAMINO_JLP_MARKET,
            name: "JLP Market",
            risk_baskets: &["medium", "aggressive"],
        },
        SupportedMarket {
            market: KAMINO_BITCOIN_MARKET,
            name: "Bitcoin Market",
            risk_baskets: &["medium", "aggressive"],
        },
        SupportedMarket {
            market: KAMINO_SUPERSTATE_OPENING_BELL_MARKET,
            name: "Superstate Opening Bell Market",
            risk_baskets: &["medium", "aggressive"],
        },
        SupportedMarket {
            market: KAMINO_HUMA_MARKET,
            name: "Huma Market",
            risk_baskets: &["aggressive"],
        },
        SupportedMarket {
            market: KAMINO_SOLSTICE_MARKET,
            name: "Solstice Market",
            risk_baskets: &["aggressive"],
        },
        SupportedMarket {
            market: KAMINO_XSTOCKS_MARKET,
            name: "xStocks Market",
            risk_baskets: &["aggressive"],
        },
        SupportedMarket {
            market: KAMINO_ALTCOINS_MARKET,
            name: "Altcoins Market",
            risk_baskets: &["aggressive"],
        },
    ]
}

pub fn policy_supported_mints() -> Vec<SupportedMint> {
    vec![
        SupportedMint {
            mint: USDC_MINT,
            symbol: "USDC",
        },
        SupportedMint {
            mint: USDT_MINT,
            symbol: "USDT",
        },
        SupportedMint {
            mint: PYUSD_MINT,
            symbol: "PYUSD",
        },
        SupportedMint {
            mint: USDS_MINT,
            symbol: "USDS",
        },
        SupportedMint {
            mint: USDG_MINT,
            symbol: "USDG",
        },
        SupportedMint {
            mint: USDE_MINT,
            symbol: "USDE",
        },
        SupportedMint {
            mint: SUSDE_MINT,
            symbol: "SUSDE",
        },
        SupportedMint {
            mint: CASH_MINT,
            symbol: "CASH",
        },
        SupportedMint {
            mint: SYRUP_USDC_MINT,
            symbol: "SYRUPUSDC",
        },
        SupportedMint {
            mint: USD1_MINT,
            symbol: "USD1",
        },
        SupportedMint {
            mint: FDUSD_MINT,
            symbol: "FDUSD",
        },
        SupportedMint {
            mint: AUSD_MINT,
            symbol: "AUSD",
        },
        SupportedMint {
            mint: EUSX_MINT,
            symbol: "EUSX",
        },
        SupportedMint {
            mint: USCC_MINT,
            symbol: "USCC",
        },
    ]
}

fn target_from_requested_reserve(reserve: Pubkey) -> ReserveTarget {
    ReserveTarget {
        reserve,
        market: None,
        market_name: None,
        symbol: None,
        liquidity_mint: None,
        api_supply_apy: None,
        api_borrow_apy: None,
        api_total_supply_usd: None,
        api_total_borrow_usd: None,
    }
}

fn normalize_token_symbol(symbol: String) -> Option<String> {
    let normalized = symbol.trim().to_ascii_uppercase();
    (!normalized.is_empty()).then_some(normalized)
}

impl SlotDurationResponse {
    fn duration_ms(&self) -> Option<f64> {
        [
            self.recent_slot_duration_in_ms,
            self.median_slot_duration_ms,
            self.slot_duration_ms,
            self.duration,
        ]
        .into_iter()
        .flatten()
        .find(|duration| *duration > 0.0)
        .map(|duration| {
            if duration < 10.0 {
                duration * 1000.0
            } else {
                duration
            }
        })
    }
}

fn deserialize_optional_f64<'de, D>(deserializer: D) -> Result<Option<f64>, D::Error>
where
    D: Deserializer<'de>,
{
    #[derive(Deserialize)]
    #[serde(untagged)]
    enum NumberOrString {
        Number(f64),
        String(String),
    }

    match Option::<NumberOrString>::deserialize(deserializer)? {
        Some(NumberOrString::Number(value)) => Ok(Some(value)),
        Some(NumberOrString::String(value)) => value
            .parse::<f64>()
            .map(Some)
            .map_err(|err| de::Error::custom(format!("invalid numeric string {value:?}: {err}"))),
        None => Ok(None),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn target(market: Pubkey, reserve: Pubkey, mint: Pubkey) -> ReserveTarget {
        ReserveTarget {
            reserve,
            market: Some(market),
            market_name: None,
            symbol: None,
            liquidity_mint: Some(mint),
            api_supply_apy: None,
            api_borrow_apy: None,
            api_total_supply_usd: None,
            api_total_borrow_usd: None,
        }
    }

    fn complete_candidates() -> Vec<ReserveTarget> {
        EARN_MAX_OBSERVATION_RESERVES
            .iter()
            .map(|entry| {
                target(
                    Pubkey::from_str(entry.market).unwrap(),
                    Pubkey::from_str(entry.reserve).unwrap(),
                    Pubkey::from_str(entry.liquidity_mint).unwrap(),
                )
            })
            .collect()
    }

    #[test]
    fn targets_selects_ten_unique_reserve_identities() {
        let selected = select_earn_max_observation_targets(complete_candidates()).unwrap();
        assert_eq!(selected.len(), 10);
    }

    #[test]
    fn targets_fail_closed_on_missing_or_duplicate_identity() {
        let mut missing = complete_candidates();
        missing.pop();
        assert!(select_earn_max_observation_targets(missing).is_err());

        let mut duplicate = complete_candidates();
        duplicate.push(duplicate[0].clone());
        assert!(select_earn_max_observation_targets(duplicate).is_err());
    }
}

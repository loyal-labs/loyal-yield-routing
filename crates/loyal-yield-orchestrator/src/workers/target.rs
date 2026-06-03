use serde_json::json;
use sha2::{Digest, Sha256};
use std::collections::HashMap;

use crate::pipeline::{ReserveApySample, ReserveTargetCandidate, DEFAULT_STRATEGY};

#[derive(Debug, Clone)]
pub struct TargetWorker {
    cluster: String,
    strategy: String,
    min_supply_usd: f64,
}

impl TargetWorker {
    pub fn new(cluster: impl Into<String>) -> Self {
        Self {
            cluster: cluster.into(),
            strategy: DEFAULT_STRATEGY.to_owned(),
            min_supply_usd: 0.0,
        }
    }

    pub fn with_min_supply_usd(mut self, min_supply_usd: f64) -> Self {
        self.min_supply_usd = min_supply_usd.max(0.0);
        self
    }

    pub fn select_targets(&self, samples: &[ReserveApySample]) -> Vec<ReserveTargetCandidate> {
        let mut best_by_mint: HashMap<&str, &ReserveApySample> = HashMap::new();

        for sample in samples {
            if !self.is_eligible(sample) {
                continue;
            }
            best_by_mint
                .entry(sample.liquidity_mint.as_str())
                .and_modify(|current| {
                    if better_sample(sample, current) {
                        *current = sample;
                    }
                })
                .or_insert(sample);
        }

        let mut targets = best_by_mint
            .into_values()
            .map(|sample| self.target_from_sample(sample))
            .collect::<Vec<_>>();
        targets.sort_by(|left, right| left.liquidity_mint.cmp(&right.liquidity_mint));
        targets
    }

    fn is_eligible(&self, sample: &ReserveApySample) -> bool {
        !sample.stale
            && sample.supply_apy_bps >= 0
            && sample.total_supply_usd_estimate.is_finite()
            && sample.total_supply_usd_estimate >= self.min_supply_usd
    }

    fn target_from_sample(&self, sample: &ReserveApySample) -> ReserveTargetCandidate {
        ReserveTargetCandidate {
            cluster: self.cluster.clone(),
            strategy: self.strategy.clone(),
            liquidity_mint: sample.liquidity_mint.clone(),
            target_reserve: sample.reserve.clone(),
            target_market: sample.market.clone(),
            target_supply_apy_bps: sample.supply_apy_bps,
            observed_slot: sample.observed_slot,
            observed_at: sample.observed_at,
            source_cursor: sample.source_cursor.clone(),
            filters: json!({
                "min_supply_usd": self.min_supply_usd,
                "stale": false
            }),
            target_epoch: target_epoch(sample),
        }
    }
}

fn better_sample(candidate: &ReserveApySample, current: &ReserveApySample) -> bool {
    candidate
        .supply_apy_bps
        .cmp(&current.supply_apy_bps)
        .then(candidate.observed_at.cmp(&current.observed_at))
        .then(candidate.observed_slot.cmp(&current.observed_slot))
        .then_with(|| current.reserve.cmp(&candidate.reserve))
        .is_gt()
}

fn target_epoch(sample: &ReserveApySample) -> String {
    let mut hasher = Sha256::new();
    hasher.update(sample.liquidity_mint.as_bytes());
    hasher.update(sample.reserve.as_bytes());
    hasher.update(sample.supply_apy_bps.to_le_bytes());
    if let Some(slot) = sample.observed_slot {
        hasher.update(slot.to_le_bytes());
    }
    hasher.update(sample.observed_at.timestamp_millis().to_le_bytes());
    format!("{:x}", hasher.finalize())
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::TimeZone;
    use serde_json::json;

    fn sample(reserve: &str, mint: &str, apy_bps: i64, stale: bool) -> ReserveApySample {
        ReserveApySample {
            reserve: reserve.to_owned(),
            market: Some("market".to_owned()),
            liquidity_mint: mint.to_owned(),
            supply_apy_bps: apy_bps,
            total_supply_usd_estimate: 10_000.0,
            stale,
            observed_slot: Some(100),
            observed_at: chrono::Utc.timestamp_opt(1_700_000_000, 0).unwrap(),
            source_cursor: json!({"slot": 100, "reserve": reserve}),
        }
    }

    #[test]
    fn target_worker_selects_max_apy_per_mint() {
        let worker = TargetWorker::new("mainnet");
        let targets = worker.select_targets(&[
            sample("reserve-a", "USDC", 120, false),
            sample("reserve-b", "USDC", 250, false),
            sample("reserve-c", "PYUSD", 90, false),
        ]);

        assert_eq!(targets.len(), 2);
        assert_eq!(targets[0].liquidity_mint, "PYUSD");
        assert_eq!(targets[0].target_reserve, "reserve-c");
        assert_eq!(targets[1].liquidity_mint, "USDC");
        assert_eq!(targets[1].target_reserve, "reserve-b");
    }

    #[test]
    fn target_worker_rejects_stale_and_low_supply_rows() {
        let worker = TargetWorker::new("mainnet").with_min_supply_usd(5_000.0);
        let mut low_supply = sample("low", "USDC", 900, false);
        low_supply.total_supply_usd_estimate = 100.0;
        let targets = worker.select_targets(&[
            sample("stale", "USDC", 1_000, true),
            low_supply,
            sample("good", "USDC", 100, false),
        ]);

        assert_eq!(targets.len(), 1);
        assert_eq!(targets[0].target_reserve, "good");
    }
}

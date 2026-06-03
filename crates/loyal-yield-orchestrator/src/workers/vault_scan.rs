use crate::{ReserveTarget, RoutePolicy};

#[derive(Debug, Default, Clone, Copy)]
pub struct VaultScanWorker;

impl VaultScanWorker {
    pub fn policy_supports_target(policy: &RoutePolicy, target: &ReserveTarget) -> bool {
        policy.active
            && policy.route_modes.iter().any(|mode| mode == "same_mint")
            && policy
                .kamino_liquidity_mints
                .iter()
                .any(|mint| mint == &target.liquidity_mint)
            && target.target_market.as_ref().map_or(true, |market| {
                policy
                    .kamino_markets
                    .iter()
                    .any(|policy_market| policy_market == market)
            })
    }

    pub fn fanout_limit(total_vaults: usize, configured_limit: usize) -> usize {
        configured_limit.max(1).min(total_vaults)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{PolicyId, ReserveTarget};
    use chrono::Utc;
    use serde_json::json;

    fn policy() -> RoutePolicy {
        RoutePolicy {
            id: PolicyId(1),
            cluster: "mainnet".to_owned(),
            settings: "settings".to_owned(),
            authority: "authority".to_owned(),
            policy_seed: 1,
            policy_account: "policy".to_owned(),
            vault_index: 0,
            vault_pubkey: "vault".to_owned(),
            delegated_signers: vec!["signer".to_owned()],
            threshold: 1,
            route_modes: vec!["same_mint".to_owned()],
            stable_mints: vec!["USDC".to_owned()],
            kamino_markets: vec!["market-a".to_owned()],
            kamino_liquidity_mints: vec!["USDC".to_owned()],
            universe_preset: None,
            risk_profile: None,
            swap_lanes: json!([]),
            active: true,
            first_seen_at: Utc::now(),
            last_seen_at: Utc::now(),
            last_seen_slot: 1,
            last_seen_signature: "sig".to_owned(),
        }
    }

    fn target() -> ReserveTarget {
        ReserveTarget {
            id: 1,
            cluster: "mainnet".to_owned(),
            strategy: "same_mint_max_apy_v1".to_owned(),
            liquidity_mint: "USDC".to_owned(),
            target_reserve: "reserve".to_owned(),
            target_market: Some("market-a".to_owned()),
            target_supply_apy_bps: 100,
            target_epoch: "epoch".to_owned(),
            stale: false,
        }
    }

    #[test]
    fn vault_scan_requires_same_mint_mint_and_market() {
        assert!(VaultScanWorker::policy_supports_target(
            &policy(),
            &target()
        ));

        let mut wrong_market = target();
        wrong_market.target_market = Some("market-b".to_owned());
        assert!(!VaultScanWorker::policy_supports_target(
            &policy(),
            &wrong_market
        ));

        let mut wrong_mint = target();
        wrong_mint.liquidity_mint = "PYUSD".to_owned();
        assert!(!VaultScanWorker::policy_supports_target(
            &policy(),
            &wrong_mint
        ));
    }

    #[test]
    fn vault_scan_fanout_limit_is_bounded() {
        assert_eq!(VaultScanWorker::fanout_limit(100, 10), 10);
        assert_eq!(VaultScanWorker::fanout_limit(3, 10), 3);
        assert_eq!(VaultScanWorker::fanout_limit(3, 0), 1);
    }
}

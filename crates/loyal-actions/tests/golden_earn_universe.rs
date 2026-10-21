use loyal_actions::{
    earn_stablecoins, yield_route_universe_for_preset, KaminoStableRiskProfile,
    YieldRouteUniversePreset,
};
use serde_json::{json, Value};
use solana_sdk::pubkey::Pubkey;

fn keys(pubkeys: &[Pubkey]) -> Value {
    Value::Array(pubkeys.iter().map(|key| json!(key.to_string())).collect())
}

#[test]
fn golden_earn_universe() {
    let fixture: Value =
        serde_json::from_str(include_str!("../fixtures/earn_universe_v1.json")).unwrap();
    let preset =
        |profile| yield_route_universe_for_preset(YieldRouteUniversePreset::KaminoStable(profile));
    let aggressive = preset(KaminoStableRiskProfile::Aggressive);
    let actual = json!({
        "schema": "loyal-earn-universe/v1",
        "stablecoins": earn_stablecoins().iter().map(|asset| json!({
            "symbol": asset.symbol,
            "mint": asset.mint.to_string(),
            "tokenProgram": asset.token_program.to_string(),
            "decimals": asset.decimals,
        })).collect::<Vec<_>>(),
        "kaminoMarkets": {
            "safe": keys(&preset(KaminoStableRiskProfile::Safe).kamino_markets),
            "medium": keys(&preset(KaminoStableRiskProfile::Medium).kamino_markets),
            "aggressive": keys(&aggressive.kamino_markets),
        },
        "supportedStableMints": keys(&aggressive.stable_mints),
    });
    assert_eq!(actual, fixture);
}

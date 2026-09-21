use balance_sweep_ata_monitor::SubscriptionWatchSet;
use loyal_yield_store::EarnSubscriptionTarget;
fn main() {
    let mut targets = Vec::new();
    for pair in std::env::args().skip(1) {
        let (wallet, settings) = pair.split_once(':').expect("wallet:settings");
        targets.push(EarnSubscriptionTarget {
            environment: "mainnet".to_owned(),
            settings: settings.to_owned(),
            wallet: wallet.to_owned(),
            earn_max: false,
            vault_index: 1,
            vault_pubkey: None,
            policy_accounts: Vec::new(),
            markets: Vec::new(),
            autodeposit_accounts: Vec::new(),
            observation_start_slot: None,
        });
    }
    let watch_set = SubscriptionWatchSet::from_targets(Vec::new(), targets).expect("watch set");
    println!("{}", serde_json::to_string_pretty(&watch_set).unwrap());
}

use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine};
use clap::ValueEnum;
use futures_util::{future::BoxFuture, SinkExt, StreamExt};
use loyal_actions::{
    decode_squads_policy_create_actions, detect_yield_route_policy_create, DetectedSwapLane,
    DetectedYieldRouteMode, DetectedYieldRoutePolicy, KaminoStableRiskProfile,
    YieldRouteUniversePreset, SQUADS_SMART_ACCOUNT_PROGRAM_ID,
};
use loyal_yield_orchestrator::{
    OrchestratorConfig, OrchestratorError, OrchestratorStore, PolicyMatchInput,
};
use serde::Serialize;
use serde_json::{json, Value};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
    transaction::VersionedTransaction,
};
use std::{
    collections::HashSet,
    fmt,
    io::{self, Write},
    str::FromStr,
    time::Duration,
};
use tokio::time::{interval, sleep, MissedTickBehavior};
use tokio_tungstenite::{connect_async, tungstenite::Message};

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, ValueEnum)]
#[serde(rename_all = "lowercase")]
pub enum Cluster {
    Mainnet,
    Devnet,
}

impl fmt::Display for Cluster {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Mainnet => formatter.write_str("mainnet"),
            Self::Devnet => formatter.write_str("devnet"),
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, ValueEnum)]
pub enum Commitment {
    Processed,
    Confirmed,
    Finalized,
}

impl fmt::Display for Commitment {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Processed => formatter.write_str("processed"),
            Self::Confirmed => formatter.write_str("confirmed"),
            Self::Finalized => formatter.write_str("finalized"),
        }
    }
}

#[derive(Debug, Clone)]
pub struct MonitorConfig {
    pub cluster: Cluster,
    pub commitment: Commitment,
    pub ws_url: String,
}

impl MonitorConfig {
    pub fn new(
        cluster: Cluster,
        commitment: Commitment,
        ws_url: Option<String>,
        api_key: Option<String>,
    ) -> Result<Self, MonitorError> {
        let ws_url = match ws_url {
            Some(url) => url,
            None => {
                let api_key = api_key.ok_or(MonitorError::MissingHeliusApiKey)?;
                match cluster {
                    Cluster::Mainnet => {
                        format!("wss://mainnet.helius-rpc.com/?api-key={api_key}")
                    }
                    Cluster::Devnet => format!("wss://devnet.helius-rpc.com/?api-key={api_key}"),
                }
            }
        };

        Ok(Self {
            cluster,
            commitment,
            ws_url,
        })
    }
}

#[derive(Debug)]
pub enum MonitorError {
    MissingHeliusApiKey,
    WebSocket(tokio_tungstenite::tungstenite::Error),
    Json(serde_json::Error),
    Io(io::Error),
    Orchestrator(OrchestratorError),
    Decode(String),
}

impl fmt::Display for MonitorError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::MissingHeliusApiKey => formatter.write_str(
                "missing Helius API key; pass --api-key, set HELIUS_API_KEY, or pass --ws-url",
            ),
            Self::WebSocket(error) => write!(formatter, "websocket error: {error}"),
            Self::Json(error) => write!(formatter, "json error: {error}"),
            Self::Io(error) => write!(formatter, "io error: {error}"),
            Self::Orchestrator(error) => write!(formatter, "orchestrator error: {error}"),
            Self::Decode(message) => formatter.write_str(message),
        }
    }
}

impl std::error::Error for MonitorError {}

impl From<tokio_tungstenite::tungstenite::Error> for MonitorError {
    fn from(value: tokio_tungstenite::tungstenite::Error) -> Self {
        Self::WebSocket(value)
    }
}

impl From<serde_json::Error> for MonitorError {
    fn from(value: serde_json::Error) -> Self {
        Self::Json(value)
    }
}

impl From<io::Error> for MonitorError {
    fn from(value: io::Error) -> Self {
        Self::Io(value)
    }
}

impl From<OrchestratorError> for MonitorError {
    fn from(value: OrchestratorError) -> Self {
        Self::Orchestrator(value)
    }
}

pub trait PolicyMatchSink {
    fn emit(&mut self, event: PolicyMatchEvent) -> BoxFuture<'_, Result<(), MonitorError>>;
}

pub struct StdoutPolicyMatchSink;

impl PolicyMatchSink for StdoutPolicyMatchSink {
    fn emit(&mut self, event: PolicyMatchEvent) -> BoxFuture<'_, Result<(), MonitorError>> {
        let result = (|| {
            let mut stdout = io::stdout().lock();
            serde_json::to_writer(&mut stdout, &event)?;
            stdout.write_all(b"\n")?;
            Ok(())
        })();
        Box::pin(async move { result })
    }
}

pub struct PostgresPolicyMatchSink {
    store: OrchestratorStore,
}

impl PostgresPolicyMatchSink {
    pub async fn connect(url: impl Into<String>) -> Result<Self, MonitorError> {
        let store = OrchestratorStore::connect(OrchestratorConfig::new(url)).await?;
        store.apply_migrations().await?;
        Ok(Self { store })
    }
}

impl PolicyMatchSink for PostgresPolicyMatchSink {
    fn emit(&mut self, event: PolicyMatchEvent) -> BoxFuture<'_, Result<(), MonitorError>> {
        let store = self.store.clone();
        Box::pin(async move {
            store
                .record_policy_match(PolicyMatchInput::from(event))
                .await?;
            Ok(())
        })
    }
}

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct PolicyMatchEvent {
    pub signature: String,
    pub slot: u64,
    pub cluster: Cluster,
    pub settings: String,
    pub authority: String,
    pub policy_seed: u64,
    pub policy_account: String,
    pub vault_index: u8,
    pub vault_pubkey: String,
    pub delegated_signers: Vec<String>,
    pub threshold: u16,
    pub route_modes: Vec<String>,
    pub stable_mints: Vec<String>,
    pub kamino_markets: Vec<String>,
    pub kamino_liquidity_mints: Vec<String>,
    pub universe_preset: Option<String>,
    pub risk_profile: Option<String>,
    pub swap_lanes: Vec<SwapLaneEvent>,
}

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum SwapLaneEvent {
    Jupiter {
        program_id: String,
        exact_in_discriminator: Vec<u8>,
    },
    LoyalHub {
        hub_authorizer: String,
        max_fee_bps: u16,
    },
}

pub struct PolicyMonitor<S> {
    config: MonitorConfig,
    sink: S,
    seen_signatures: HashSet<String>,
}

impl<S: PolicyMatchSink> PolicyMonitor<S> {
    pub fn new(config: MonitorConfig, sink: S) -> Self {
        Self {
            config,
            sink,
            seen_signatures: HashSet::new(),
        }
    }

    pub async fn run(&mut self, once: bool) -> Result<(), MonitorError> {
        let mut backoff = Duration::from_secs(1);
        loop {
            match self.run_connection(once).await {
                Ok(()) if once => return Ok(()),
                Ok(()) => backoff = Duration::from_secs(1),
                Err(error) if once => return Err(error),
                Err(error) => {
                    eprintln!("{error}; reconnecting in {}s", backoff.as_secs());
                    sleep(backoff).await;
                    backoff = (backoff * 2).min(Duration::from_secs(60));
                }
            }
        }
    }

    async fn run_connection(&mut self, once: bool) -> Result<(), MonitorError> {
        let (mut ws, _) = connect_async(&self.config.ws_url).await?;
        ws.send(Message::Text(self.subscription_request().into()))
            .await?;

        let mut pings = interval(Duration::from_secs(60));
        pings.set_missed_tick_behavior(MissedTickBehavior::Delay);

        loop {
            tokio::select! {
                _ = pings.tick() => ws.send(Message::Ping(Vec::new().into())).await?,
                message = ws.next() => {
                    let Some(message) = message else {
                        return Ok(());
                    };
                    match message? {
                        Message::Text(text) => {
                    let (processed, _) = self.process_message_text(&text).await?;
                            if once && processed {
                                return Ok(());
                            }
                        }
                        Message::Binary(bytes) => {
                            let text = String::from_utf8_lossy(&bytes);
                    let (processed, _) = self.process_message_text(&text).await?;
                            if once && processed {
                                return Ok(());
                            }
                        }
                        Message::Ping(payload) => ws.send(Message::Pong(payload)).await?,
                        Message::Close(_) => return Ok(()),
                        Message::Pong(_) | Message::Frame(_) => {}
                    }
                }
            }
        }
    }

    fn subscription_request(&self) -> String {
        json!({
            "jsonrpc": "2.0",
            "id": 1,
            "method": "transactionSubscribe",
            "params": [
                {
                    "accountInclude": [SQUADS_SMART_ACCOUNT_PROGRAM_ID.to_string()],
                    "failed": false,
                    "vote": false
                },
                {
                    "commitment": self.config.commitment.to_string(),
                    "encoding": "base64",
                    "transactionDetails": "full",
                    "maxSupportedTransactionVersion": 0
                }
            ]
        })
        .to_string()
    }

    pub async fn process_notification_text(&mut self, text: &str) -> Result<usize, MonitorError> {
        self.process_message_text(text)
            .await
            .map(|(_, emitted)| emitted)
    }

    async fn process_message_text(&mut self, text: &str) -> Result<(bool, usize), MonitorError> {
        let value: Value = serde_json::from_str(text)?;
        let processed =
            value.get("method").and_then(Value::as_str) == Some("transactionNotification");
        let emitted = self.process_notification(&value).await?;
        Ok((processed, emitted))
    }

    pub async fn process_notification(&mut self, value: &Value) -> Result<usize, MonitorError> {
        let Some(notification) = HeliusNotification::from_value(value) else {
            return Ok(0);
        };
        if !self.seen_signatures.insert(notification.signature.clone()) {
            return Ok(0);
        }

        let instructions = decode_squads_instructions(notification.transaction, notification.meta)?;
        let mut emitted = 0;
        for instruction in instructions {
            let actions = match decode_squads_policy_create_actions(&instruction) {
                Ok(actions) => actions,
                Err(_) => continue,
            };
            for action in actions {
                if let Some(policy) = detect_yield_route_policy_create(&action) {
                    self.sink
                        .emit(PolicyMatchEvent::from_policy(
                            &notification.signature,
                            notification.slot,
                            self.config.cluster,
                            policy,
                        ))
                        .await?;
                    emitted += 1;
                }
            }
        }

        Ok(emitted)
    }
}

impl PolicyMatchEvent {
    fn from_policy(
        signature: &str,
        slot: u64,
        cluster: Cluster,
        policy: DetectedYieldRoutePolicy,
    ) -> Self {
        Self {
            signature: signature.to_owned(),
            slot,
            cluster,
            settings: policy.settings.to_string(),
            authority: policy.authority.to_string(),
            policy_seed: policy.policy_seed,
            policy_account: policy.policy_account.to_string(),
            vault_index: policy.vault_index,
            vault_pubkey: derive_squads_vault(&policy.settings, policy.vault_index).to_string(),
            delegated_signers: pubkeys_to_strings(policy.delegated_signers),
            threshold: policy.threshold,
            route_modes: policy
                .route_modes
                .into_iter()
                .map(route_mode_name)
                .collect(),
            stable_mints: pubkeys_to_strings(policy.stable_mints),
            kamino_markets: pubkeys_to_strings(policy.kamino_markets),
            kamino_liquidity_mints: pubkeys_to_strings(policy.kamino_liquidity_mints),
            universe_preset: policy.universe_preset.map(universe_preset_name),
            risk_profile: policy.universe_preset.and_then(preset_risk_profile_name),
            swap_lanes: policy
                .swap_lanes
                .into_iter()
                .map(SwapLaneEvent::from)
                .collect(),
        }
    }
}

impl From<PolicyMatchEvent> for PolicyMatchInput {
    fn from(event: PolicyMatchEvent) -> Self {
        Self {
            signature: event.signature,
            slot: event.slot,
            cluster: event.cluster.to_string(),
            settings: event.settings,
            authority: event.authority,
            policy_seed: event.policy_seed,
            policy_account: event.policy_account,
            vault_index: event.vault_index,
            vault_pubkey: event.vault_pubkey,
            delegated_signers: event.delegated_signers,
            threshold: event.threshold,
            route_modes: event.route_modes,
            stable_mints: event.stable_mints,
            kamino_markets: event.kamino_markets,
            kamino_liquidity_mints: event.kamino_liquidity_mints,
            universe_preset: event.universe_preset,
            risk_profile: event.risk_profile,
            swap_lanes: json!(event.swap_lanes),
        }
    }
}

fn derive_squads_vault(settings: &Pubkey, vault_index: u8) -> Pubkey {
    Pubkey::find_program_address(
        &[
            b"smart_account",
            settings.as_ref(),
            b"smart_account",
            &[vault_index],
        ],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
    .0
}

impl From<DetectedSwapLane> for SwapLaneEvent {
    fn from(value: DetectedSwapLane) -> Self {
        match value {
            DetectedSwapLane::Jupiter(contract) => Self::Jupiter {
                program_id: contract.program_id.to_string(),
                exact_in_discriminator: contract.exact_in_discriminator.to_vec(),
            },
            DetectedSwapLane::LoyalHub {
                hub_authorizer,
                max_fee_bps,
            } => Self::LoyalHub {
                hub_authorizer: hub_authorizer.to_string(),
                max_fee_bps,
            },
        }
    }
}

fn pubkeys_to_strings(pubkeys: Vec<Pubkey>) -> Vec<String> {
    pubkeys
        .into_iter()
        .map(|pubkey| pubkey.to_string())
        .collect()
}

fn route_mode_name(value: DetectedYieldRouteMode) -> String {
    match value {
        DetectedYieldRouteMode::SameMint => "same_mint",
        DetectedYieldRouteMode::CrossMintJupiter => "cross_mint_jupiter",
        DetectedYieldRouteMode::CrossMintLoyalHub => "cross_mint_loyal_hub",
    }
    .to_owned()
}

fn universe_preset_name(value: YieldRouteUniversePreset) -> String {
    match value {
        YieldRouteUniversePreset::KaminoStable(_) => "kamino_stable",
    }
    .to_string()
}

fn preset_risk_profile_name(value: YieldRouteUniversePreset) -> Option<String> {
    match value {
        YieldRouteUniversePreset::KaminoStable(profile) => {
            Some(kamino_stable_profile_name(profile))
        }
    }
}

fn kamino_stable_profile_name(value: KaminoStableRiskProfile) -> String {
    match value {
        KaminoStableRiskProfile::Safe => "safe",
        KaminoStableRiskProfile::Medium => "medium",
        KaminoStableRiskProfile::Aggressive => "aggressive",
    }
    .to_string()
}

struct HeliusNotification<'a> {
    signature: String,
    slot: u64,
    transaction: &'a Value,
    meta: Option<&'a Value>,
}

impl<'a> HeliusNotification<'a> {
    fn from_value(value: &'a Value) -> Option<Self> {
        if value.get("method").and_then(Value::as_str) != Some("transactionNotification") {
            return None;
        }
        let result = value.get("params")?.get("result")?;
        let slot = result.get("slot")?.as_u64()?;
        let signature = result.get("signature")?.as_str()?.to_owned();
        let tx_wrapper = result.get("transaction")?;
        let meta = tx_wrapper.get("meta");
        if meta
            .and_then(|meta| meta.get("err"))
            .is_some_and(|err| !err.is_null())
        {
            return None;
        }
        Some(Self {
            signature,
            slot,
            transaction: tx_wrapper.get("transaction")?,
            meta,
        })
    }
}

fn decode_squads_instructions(
    transaction_value: &Value,
    meta: Option<&Value>,
) -> Result<Vec<Instruction>, MonitorError> {
    let transaction_bytes = transaction_base64(transaction_value)
        .ok_or_else(|| MonitorError::Decode("missing base64 transaction payload".to_owned()))
        .and_then(|payload| {
            BASE64_STANDARD.decode(payload).map_err(|error| {
                MonitorError::Decode(format!("invalid base64 transaction: {error}"))
            })
        })?;
    let transaction: VersionedTransaction = bincode::deserialize(&transaction_bytes)
        .map_err(|error| MonitorError::Decode(format!("invalid transaction bytes: {error}")))?;

    let mut account_keys = transaction.message.static_account_keys().to_vec();
    account_keys.extend(loaded_addresses(meta)?);

    let mut instructions = Vec::new();
    for compiled in transaction.message.instructions() {
        let Some(program_id) = account_keys
            .get(compiled.program_id_index as usize)
            .copied()
        else {
            continue;
        };
        if program_id != SQUADS_SMART_ACCOUNT_PROGRAM_ID {
            continue;
        }
        let accounts = compiled
            .accounts
            .iter()
            .filter_map(|index| account_keys.get(*index as usize).copied())
            .map(|pubkey| AccountMeta::new_readonly(pubkey, false))
            .collect();
        instructions.push(Instruction {
            program_id,
            accounts,
            data: compiled.data.clone(),
        });
    }

    Ok(instructions)
}

fn transaction_base64(value: &Value) -> Option<&str> {
    value
        .as_array()
        .and_then(|items| items.first())
        .and_then(Value::as_str)
        .or_else(|| value.as_str())
}

fn loaded_addresses(meta: Option<&Value>) -> Result<Vec<Pubkey>, MonitorError> {
    let mut addresses = Vec::new();
    let Some(loaded) = meta.and_then(|meta| meta.get("loadedAddresses")) else {
        return Ok(addresses);
    };
    for key in ["writable", "readonly"] {
        if let Some(values) = loaded.get(key).and_then(Value::as_array) {
            for value in values {
                let text = value.as_str().ok_or_else(|| {
                    MonitorError::Decode("loaded address is not a string".to_owned())
                })?;
                addresses.push(Pubkey::from_str(text).map_err(|error| {
                    MonitorError::Decode(format!("invalid loaded address {text}: {error}"))
                })?);
            }
        }
    }
    Ok(addresses)
}

#[cfg(test)]
mod tests {
    use super::*;
    use loyal_actions::{
        create_all_in_one_market_mint_yield_route_action,
        create_preset_all_in_one_yield_route_action, create_swap_yield_route_action,
        JupiterSwapContract, KaminoStableRiskProfile, LoyalActionContext, SwapLane,
        YieldRouteUniverse, YieldRouteUniversePreset, JUPITER_SWAP_DISCRIMINATOR,
        JUPITER_V6_PROGRAM_ID,
    };
    use solana_sdk::{
        hash::Hash,
        message::{v0, VersionedMessage},
        signature::{Keypair, Signer},
        transaction::VersionedTransaction,
    };

    #[derive(Default)]
    struct VecSink {
        events: Vec<PolicyMatchEvent>,
    }

    impl PolicyMatchSink for VecSink {
        fn emit(&mut self, event: PolicyMatchEvent) -> BoxFuture<'_, Result<(), MonitorError>> {
            self.events.push(event);
            Box::pin(async move { Ok(()) })
        }
    }

    fn config() -> MonitorConfig {
        MonitorConfig {
            cluster: Cluster::Devnet,
            commitment: Commitment::Confirmed,
            ws_url: "ws://localhost".to_owned(),
        }
    }

    fn context(authority: Pubkey) -> LoyalActionContext {
        LoyalActionContext {
            settings: Pubkey::new_unique(),
            authority,
            delegated_signer: Pubkey::new_unique(),
            account_index: 0,
            vault: Pubkey::new_unique(),
        }
    }

    fn universe() -> YieldRouteUniverse {
        YieldRouteUniverse::new(
            vec![Pubkey::new_unique(), Pubkey::new_unique()],
            vec![Pubkey::new_unique()],
            vec![Pubkey::new_unique()],
        )
    }

    fn jupiter_lane() -> SwapLane {
        SwapLane::Jupiter(JupiterSwapContract {
            program_id: JUPITER_V6_PROGRAM_ID,
            exact_in_discriminator: JUPITER_SWAP_DISCRIMINATOR,
            max_slippage_bps: 100,
        })
    }

    fn notification(
        signature: &str,
        slot: u64,
        instruction: Instruction,
        authority: &Keypair,
    ) -> Value {
        let payer = Keypair::new();
        let message = VersionedMessage::V0(
            v0::Message::try_compile(&payer.pubkey(), &[instruction], &[], Hash::new_unique())
                .unwrap(),
        );
        let transaction = VersionedTransaction::try_new(message, &[&payer, authority]).unwrap();
        let bytes = bincode::serialize(&transaction).unwrap();
        json!({
            "jsonrpc": "2.0",
            "method": "transactionNotification",
            "params": {
                "result": {
                    "signature": signature,
                    "slot": slot,
                    "transaction": {
                        "transaction": [BASE64_STANDARD.encode(bytes), "base64"],
                        "meta": { "err": null }
                    }
                }
            }
        })
    }

    #[tokio::test]
    async fn parses_mocked_notification_and_emits_policy_match() {
        let authority = Keypair::new();
        let setup = create_all_in_one_market_mint_yield_route_action(
            context(authority.pubkey()),
            universe(),
            vec![jupiter_lane()],
        )
        .unwrap();
        let mut monitor = PolicyMonitor::new(config(), VecSink::default());

        let emitted = monitor
            .process_notification(&notification(
                "sig1",
                42,
                setup.instructions[0].clone(),
                &authority,
            ))
            .await
            .unwrap();

        assert_eq!(emitted, 1);
        assert_eq!(monitor.sink.events.len(), 1);
        assert_eq!(monitor.sink.events[0].signature, "sig1");
        assert_eq!(monitor.sink.events[0].slot, 42);
        assert_eq!(
            monitor.sink.events[0].route_modes,
            vec!["same_mint", "cross_mint_jupiter"]
        );
    }

    #[tokio::test]
    async fn emits_detected_kamino_stable_preset_shape() {
        let authority = Keypair::new();
        let setup = create_preset_all_in_one_yield_route_action(
            context(authority.pubkey()),
            YieldRouteUniversePreset::KaminoStable(KaminoStableRiskProfile::Aggressive),
            vec![jupiter_lane()],
        )
        .unwrap();
        let mut monitor = PolicyMonitor::new(config(), VecSink::default());

        let emitted = monitor
            .process_notification(&notification(
                "sig1",
                42,
                setup.instructions[0].clone(),
                &authority,
            ))
            .await
            .unwrap();

        assert_eq!(emitted, 1);
        assert_eq!(
            monitor.sink.events[0].universe_preset.as_deref(),
            Some("kamino_stable")
        );
        assert_eq!(
            monitor.sink.events[0].risk_profile.as_deref(),
            Some("aggressive")
        );
    }

    #[tokio::test]
    async fn emitted_json_uses_chain_provided_policy_account() {
        let authority = Keypair::new();
        let setup = create_all_in_one_market_mint_yield_route_action(
            context(authority.pubkey()),
            universe(),
            vec![jupiter_lane()],
        )
        .unwrap();
        let mut instruction = setup.instructions[0].clone();
        let chain_policy = Pubkey::new_unique();
        instruction.accounts[5].pubkey = chain_policy;
        let mut monitor = PolicyMonitor::new(config(), VecSink::default());

        let emitted = monitor
            .process_notification(&notification("sig1", 42, instruction, &authority))
            .await
            .unwrap();

        assert_eq!(emitted, 1);
        assert_eq!(
            monitor.sink.events[0].policy_account,
            chain_policy.to_string()
        );
    }

    #[tokio::test]
    async fn deduplicates_repeated_signatures() {
        let authority = Keypair::new();
        let setup = create_all_in_one_market_mint_yield_route_action(
            context(authority.pubkey()),
            universe(),
            vec![jupiter_lane()],
        )
        .unwrap();
        let value = notification("sig1", 42, setup.instructions[0].clone(), &authority);
        let mut monitor = PolicyMonitor::new(config(), VecSink::default());

        assert_eq!(monitor.process_notification(&value).await.unwrap(), 1);
        assert_eq!(monitor.process_notification(&value).await.unwrap(), 0);
        assert_eq!(monitor.sink.events.len(), 1);
    }

    #[tokio::test]
    async fn ignores_failed_transactions() {
        let authority = Keypair::new();
        let setup = create_all_in_one_market_mint_yield_route_action(
            context(authority.pubkey()),
            universe(),
            vec![jupiter_lane()],
        )
        .unwrap();
        let mut value = notification("sig1", 42, setup.instructions[0].clone(), &authority);
        value["params"]["result"]["transaction"]["meta"]["err"] =
            json!({"InstructionError":[0,"Custom"]});
        let mut monitor = PolicyMonitor::new(config(), VecSink::default());

        assert_eq!(monitor.process_notification(&value).await.unwrap(), 0);
        assert!(monitor.sink.events.is_empty());
    }

    #[tokio::test]
    async fn ignores_non_policy_settings_actions() {
        let authority = Keypair::new();
        let action = create_swap_yield_route_action(
            context(authority.pubkey()),
            universe().stable_mints,
            vec![jupiter_lane()],
            9,
        )
        .unwrap();
        let mut monitor = PolicyMonitor::new(config(), VecSink::default());

        let emitted = monitor
            .process_notification(&notification("sig1", 42, action.instruction, &authority))
            .await
            .unwrap();

        assert_eq!(emitted, 0);
        assert!(monitor.sink.events.is_empty());
    }
}

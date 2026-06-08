use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine};
use clap::ValueEnum;
use futures_util::{future::BoxFuture, SinkExt, StreamExt};
use loyal_actions::{
    decode_squads_policy_create_actions, detect_balance_sweep_policy_create,
    detect_yield_route_policy_create, DetectedBalanceSweepPolicy, DetectedSwapLane,
    DetectedYieldRouteMode, DetectedYieldRoutePolicy, KaminoStableRiskProfile,
    SquadsSettingsActionView, YieldRouteUniversePreset, SQUADS_SMART_ACCOUNT_PROGRAM_ID,
};
use loyal_yield_orchestrator::{
    BalanceSweepExecutionInput, BalanceSweepPolicyMatchInput, OrchestratorConfig,
    OrchestratorError, OrchestratorStore, PolicyMatchInput,
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
    fn emit(&mut self, event: PolicyMonitorEvent) -> BoxFuture<'_, Result<(), MonitorError>>;
    fn emit_execution(
        &mut self,
        event: BalanceSweepExecutionEvent,
    ) -> BoxFuture<'_, Result<(), MonitorError>>;
}

pub struct StdoutPolicyMatchSink;

impl PolicyMatchSink for StdoutPolicyMatchSink {
    fn emit(&mut self, event: PolicyMonitorEvent) -> BoxFuture<'_, Result<(), MonitorError>> {
        let result = (|| {
            let mut stdout = io::stdout().lock();
            serde_json::to_writer(&mut stdout, &event)?;
            stdout.write_all(b"\n")?;
            Ok(())
        })();
        Box::pin(async move { result })
    }

    fn emit_execution(
        &mut self,
        event: BalanceSweepExecutionEvent,
    ) -> BoxFuture<'_, Result<(), MonitorError>> {
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
        Ok(Self { store })
    }

    pub fn from_store(store: OrchestratorStore) -> Self {
        Self { store }
    }
}

impl PolicyMatchSink for PostgresPolicyMatchSink {
    fn emit(&mut self, event: PolicyMonitorEvent) -> BoxFuture<'_, Result<(), MonitorError>> {
        let store = self.store.clone();
        Box::pin(async move {
            match event {
                PolicyMonitorEvent::YieldRoute(event) => {
                    store
                        .record_policy_match(PolicyMatchInput::from(event))
                        .await?;
                }
                PolicyMonitorEvent::BalanceSweep(event) => {
                    store
                        .record_balance_sweep_policy_match(BalanceSweepPolicyMatchInput::from(
                            event,
                        ))
                        .await?;
                }
            }
            Ok(())
        })
    }

    fn emit_execution(
        &mut self,
        event: BalanceSweepExecutionEvent,
    ) -> BoxFuture<'_, Result<(), MonitorError>> {
        let store = self.store.clone();
        Box::pin(async move {
            let targets = store
                .load_active_balance_sweep_targets(&event.cluster.to_string())
                .await?;
            for target in targets {
                if target.wallet_usdc_ata == event.source_wallet_ata
                    && target.vault_usdc_ata == event.destination_vault_ata
                {
                    store
                        .record_balance_sweep_execution(BalanceSweepExecutionInput {
                            target_id: target.id,
                            cluster: event.cluster.to_string(),
                            signature: event.signature,
                            slot: event.slot,
                            source_wallet_ata: event.source_wallet_ata,
                            destination_vault_ata: event.destination_vault_ata,
                            amount_raw: event.amount_raw,
                            source_pre_balance_raw: event.source_pre_balance_raw,
                            source_post_balance_raw: event.source_post_balance_raw,
                            destination_pre_balance_raw: event.destination_pre_balance_raw,
                            destination_post_balance_raw: event.destination_post_balance_raw,
                            source_commitment: event.source_commitment,
                            raw_evidence: event.raw_evidence,
                            decoded_evidence: event.decoded_evidence,
                            received_at: event.received_at,
                            decoded_at: event.decoded_at,
                            dedupe_key: event.dedupe_key,
                        })
                        .await?;
                    break;
                }
            }
            Ok(())
        })
    }
}

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
#[serde(tag = "policy_kind", rename_all = "snake_case")]
pub enum PolicyMonitorEvent {
    YieldRoute(PolicyMatchEvent),
    BalanceSweep(BalanceSweepPolicyEvent),
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
pub struct BalanceSweepPolicyEvent {
    pub signature: String,
    pub slot: u64,
    pub cluster: Cluster,
    pub settings: String,
    pub authority: String,
    pub policy_seed: u64,
    pub policy_account: String,
    pub vault_index: u8,
    pub vault_pubkey: String,
    pub wallet: String,
    pub wallet_usdc_ata: String,
    pub vault_usdc_ata: String,
    pub delegated_signers: Vec<String>,
    pub threshold: u16,
    pub max_amount_per_period: u64,
}

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct BalanceSweepExecutionEvent {
    pub signature: String,
    pub slot: u64,
    pub cluster: Cluster,
    pub source_wallet_ata: String,
    pub destination_vault_ata: String,
    pub amount_raw: u64,
    pub source_pre_balance_raw: Option<u64>,
    pub source_post_balance_raw: Option<u64>,
    pub destination_pre_balance_raw: Option<u64>,
    pub destination_post_balance_raw: Option<u64>,
    pub source_commitment: String,
    pub raw_evidence: Value,
    pub decoded_evidence: Value,
    pub received_at: Option<chrono::DateTime<chrono::Utc>>,
    pub decoded_at: Option<chrono::DateTime<chrono::Utc>>,
    pub dedupe_key: String,
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
                emitted += self.process_detected_action(&notification, &action).await?;
            }
        }
        for execution in detect_balance_sweep_execution_events(
            &notification,
            self.config.cluster,
            self.config.commitment,
        )? {
            self.sink.emit_execution(execution).await?;
            emitted += 1;
        }

        Ok(emitted)
    }

    async fn process_detected_action(
        &mut self,
        notification: &HeliusNotification<'_>,
        action: &SquadsSettingsActionView,
    ) -> Result<usize, MonitorError> {
        let mut emitted = 0;
        if let Some(policy) = detect_yield_route_policy_create(action) {
            self.sink
                .emit(PolicyMonitorEvent::YieldRoute(
                    PolicyMatchEvent::from_policy(
                        &notification.signature,
                        notification.slot,
                        self.config.cluster,
                        policy,
                    ),
                ))
                .await?;
            emitted += 1;
        }
        if let Some(policy) = detect_balance_sweep_policy_create(action) {
            self.sink
                .emit(PolicyMonitorEvent::BalanceSweep(
                    BalanceSweepPolicyEvent::from_policy(
                        &notification.signature,
                        notification.slot,
                        self.config.cluster,
                        policy,
                    ),
                ))
                .await?;
            emitted += 1;
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

impl BalanceSweepPolicyEvent {
    fn from_policy(
        signature: &str,
        slot: u64,
        cluster: Cluster,
        policy: DetectedBalanceSweepPolicy,
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
            vault_pubkey: policy.vault_pubkey.to_string(),
            wallet: policy.wallet.to_string(),
            wallet_usdc_ata: policy.wallet_usdc_ata.to_string(),
            vault_usdc_ata: policy.vault_usdc_ata.to_string(),
            delegated_signers: pubkeys_to_strings(policy.delegated_signers),
            threshold: policy.threshold,
            max_amount_per_period: policy.max_amount_per_period,
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

impl From<BalanceSweepPolicyEvent> for BalanceSweepPolicyMatchInput {
    fn from(event: BalanceSweepPolicyEvent) -> Self {
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
            wallet: event.wallet,
            wallet_usdc_ata: event.wallet_usdc_ata,
            vault_usdc_ata: event.vault_usdc_ata,
            delegated_signers: event.delegated_signers,
            threshold: event.threshold,
            max_amount_per_period: event.max_amount_per_period,
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

fn detect_balance_sweep_execution_events(
    notification: &HeliusNotification<'_>,
    cluster: Cluster,
    commitment: Commitment,
) -> Result<Vec<BalanceSweepExecutionEvent>, MonitorError> {
    let Some(meta) = notification.meta else {
        return Ok(vec![]);
    };
    let account_keys = transaction_account_keys(notification.transaction, notification.meta)?;
    let pre = token_balances_by_account_index(meta.get("preTokenBalances"), &account_keys)?;
    let post = token_balances_by_account_index(meta.get("postTokenBalances"), &account_keys)?;

    let mut decreases = Vec::new();
    let mut increases = Vec::new();
    for (account_index, pre_balance) in &pre {
        let Some(post_balance) = post.get(account_index) else {
            continue;
        };
        if pre_balance.mint != "USDC" && pre_balance.mint != loyal_actions::USDC_MINT.to_string() {
            continue;
        }
        if pre_balance.mint != post_balance.mint {
            continue;
        }
        if pre_balance.amount_raw > post_balance.amount_raw {
            decreases.push((pre_balance, post_balance));
        } else if post_balance.amount_raw > pre_balance.amount_raw {
            increases.push((pre_balance, post_balance));
        }
    }

    let mut events = Vec::new();
    for (source_pre, source_post) in &decreases {
        let amount = source_pre.amount_raw - source_post.amount_raw;
        for (dest_pre, dest_post) in &increases {
            if dest_post.amount_raw - dest_pre.amount_raw != amount {
                continue;
            }
            events.push(BalanceSweepExecutionEvent {
                signature: notification.signature.clone(),
                slot: notification.slot,
                cluster,
                source_wallet_ata: source_pre.account.to_string(),
                destination_vault_ata: dest_pre.account.to_string(),
                amount_raw: amount,
                source_pre_balance_raw: Some(source_pre.amount_raw),
                source_post_balance_raw: Some(source_post.amount_raw),
                destination_pre_balance_raw: Some(dest_pre.amount_raw),
                destination_post_balance_raw: Some(dest_post.amount_raw),
                source_commitment: commitment.to_string(),
                raw_evidence: json!({
                    "preTokenBalances": meta.get("preTokenBalances").cloned().unwrap_or(Value::Null),
                    "postTokenBalances": meta.get("postTokenBalances").cloned().unwrap_or(Value::Null),
                }),
                decoded_evidence: json!({
                    "kind": "token_balance_delta",
                    "mint": source_pre.mint,
                    "source_account_index": source_pre.account_index,
                    "destination_account_index": dest_pre.account_index,
                }),
                received_at: None,
                decoded_at: Some(chrono::Utc::now()),
                dedupe_key: format!(
                    "{}:{}:{}:{}:{}",
                    cluster,
                    notification.signature,
                    source_pre.account,
                    dest_pre.account,
                    amount
                ),
            });
        }
    }
    Ok(events)
}

#[derive(Debug, Clone)]
struct TokenBalanceEvidence {
    account_index: usize,
    account: Pubkey,
    mint: String,
    amount_raw: u64,
}

fn token_balances_by_account_index(
    value: Option<&Value>,
    account_keys: &[Pubkey],
) -> Result<std::collections::BTreeMap<usize, TokenBalanceEvidence>, MonitorError> {
    let mut balances = std::collections::BTreeMap::new();
    let Some(items) = value.and_then(Value::as_array) else {
        return Ok(balances);
    };
    for item in items {
        let account_index = item
            .get("accountIndex")
            .and_then(Value::as_u64)
            .ok_or_else(|| MonitorError::Decode("token balance missing accountIndex".to_owned()))?
            as usize;
        let account = account_keys.get(account_index).copied().ok_or_else(|| {
            MonitorError::Decode(format!(
                "token balance account index {account_index} out of range"
            ))
        })?;
        let mint = item
            .get("mint")
            .and_then(Value::as_str)
            .ok_or_else(|| MonitorError::Decode("token balance missing mint".to_owned()))?
            .to_owned();
        let amount_raw = item
            .get("uiTokenAmount")
            .and_then(|value| value.get("amount"))
            .and_then(Value::as_str)
            .ok_or_else(|| MonitorError::Decode("token balance missing amount".to_owned()))?
            .parse::<u64>()
            .map_err(|error| MonitorError::Decode(format!("invalid token amount: {error}")))?;
        balances.insert(
            account_index,
            TokenBalanceEvidence {
                account_index,
                account,
                mint,
                amount_raw,
            },
        );
    }
    Ok(balances)
}

fn transaction_account_keys(
    transaction_value: &Value,
    meta: Option<&Value>,
) -> Result<Vec<Pubkey>, MonitorError> {
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
    Ok(account_keys)
}

#[cfg(test)]
mod tests {
    use super::*;
    use loyal_actions::{
        create_all_in_one_market_mint_yield_route_action,
        create_preset_all_in_one_yield_route_action, create_swap_yield_route_action,
        derive_subscription_authority, derive_subscription_event_authority, JupiterSwapContract,
        KaminoStableRiskProfile, LoyalActionContext, SquadsAccountConstraintKindView,
        SquadsAccountConstraintView, SquadsDataConstraintView, SquadsDataOperatorView,
        SquadsDataValueView, SquadsInstructionConstraintView, SquadsProgramInteractionPolicyView,
        SquadsSettingsActionView, SwapLane, YieldRouteUniverse, YieldRouteUniversePreset,
        JUPITER_SWAP_DISCRIMINATOR, JUPITER_V6_PROGRAM_ID, SUBSCRIPTIONS_PROGRAM_ID,
        SUBSCRIPTIONS_TRANSFER_RECURRING,
        SUBSCRIPTION_RECURRING_DELEGATION_AMOUNT_PER_PERIOD_OFFSET,
        SUBSCRIPTION_RECURRING_DELEGATION_AUTHORITY_OFFSET,
        SUBSCRIPTION_RECURRING_DELEGATION_DELEGATEE_OFFSET,
        SUBSCRIPTION_RECURRING_DELEGATION_DELEGATOR_OFFSET,
        SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR,
        SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR_OFFSET,
        SUBSCRIPTION_RECURRING_DELEGATION_MINT_OFFSET, SUBSCRIPTION_TRANSFER_DELEGATOR_OFFSET,
        SUBSCRIPTION_TRANSFER_MINT_OFFSET, USDC_MINT,
    };
    use loyal_yield_orchestrator::{sqlx, NeonSqlConfig};
    use solana_sdk::{
        hash::Hash,
        message::{v0, VersionedMessage},
        signature::{Keypair, Signer},
        transaction::VersionedTransaction,
    };
    use std::time::{SystemTime, UNIX_EPOCH};

    #[derive(Default)]
    struct VecSink {
        events: Vec<PolicyMonitorEvent>,
        executions: Vec<BalanceSweepExecutionEvent>,
    }

    impl PolicyMatchSink for VecSink {
        fn emit(&mut self, event: PolicyMonitorEvent) -> BoxFuture<'_, Result<(), MonitorError>> {
            self.events.push(event);
            Box::pin(async move { Ok(()) })
        }

        fn emit_execution(
            &mut self,
            event: BalanceSweepExecutionEvent,
        ) -> BoxFuture<'_, Result<(), MonitorError>> {
            self.executions.push(event);
            Box::pin(async move { Ok(()) })
        }
    }

    fn yield_route_event(event: &PolicyMonitorEvent) -> &PolicyMatchEvent {
        let PolicyMonitorEvent::YieldRoute(event) = event else {
            panic!("expected yield route event");
        };
        event
    }

    fn balance_sweep_event(event: &PolicyMonitorEvent) -> &BalanceSweepPolicyEvent {
        let PolicyMonitorEvent::BalanceSweep(event) = event else {
            panic!("expected balance sweep event");
        };
        event
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

    fn balance_sweep_action() -> SquadsSettingsActionView {
        let settings = Pubkey::new_unique();
        let vault_index = 1;
        let vault = derive_squads_vault(&settings, vault_index);
        let wallet = Pubkey::new_unique();
        let wallet_usdc_ata = Pubkey::new_unique();
        let vault_usdc_ata = Pubkey::new_unique();
        let subscription_authority = derive_subscription_authority(wallet, USDC_MINT);
        SquadsSettingsActionView {
            settings,
            authority: Pubkey::new_unique(),
            policy_seed: 11,
            policy_account: Pubkey::new_unique(),
            delegated_signers: vec![Pubkey::new_unique()],
            threshold: 1,
            payload: SquadsProgramInteractionPolicyView {
                vault_index,
                pubkey_table: vec![],
                constraints: vec![SquadsInstructionConstraintView {
                    program_id: SUBSCRIPTIONS_PROGRAM_ID,
                    account_constraints: vec![
                        account_data_view(
                            0,
                            Some(SUBSCRIPTIONS_PROGRAM_ID),
                            vec![
                                data_u8_eq(
                                    SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR_OFFSET,
                                    SUBSCRIPTION_RECURRING_DELEGATION_DISCRIMINATOR,
                                ),
                                data_pubkey_eq(
                                    SUBSCRIPTION_RECURRING_DELEGATION_DELEGATOR_OFFSET,
                                    wallet,
                                ),
                                data_pubkey_eq(
                                    SUBSCRIPTION_RECURRING_DELEGATION_DELEGATEE_OFFSET,
                                    vault,
                                ),
                                data_pubkey_eq(
                                    SUBSCRIPTION_RECURRING_DELEGATION_AUTHORITY_OFFSET,
                                    subscription_authority,
                                ),
                                data_pubkey_eq(
                                    SUBSCRIPTION_RECURRING_DELEGATION_MINT_OFFSET,
                                    USDC_MINT,
                                ),
                                data_u64_lte(
                                    SUBSCRIPTION_RECURRING_DELEGATION_AMOUNT_PER_PERIOD_OFFSET,
                                    250_000,
                                ),
                            ],
                        ),
                        pubkey_view(1, subscription_authority, Some(SUBSCRIPTIONS_PROGRAM_ID)),
                        pubkey_view(2, wallet_usdc_ata, Some(spl_token::id())),
                        pubkey_view(3, vault_usdc_ata, Some(spl_token::id())),
                        pubkey_view(4, USDC_MINT, Some(spl_token::id())),
                        pubkey_view(5, spl_token::id(), None),
                        pubkey_view(6, vault, None),
                        pubkey_view(7, derive_subscription_event_authority(), None),
                        pubkey_view(8, SUBSCRIPTIONS_PROGRAM_ID, None),
                    ],
                    data_constraints: vec![
                        data_u8_eq(0, SUBSCRIPTIONS_TRANSFER_RECURRING),
                        data_pubkey_eq(SUBSCRIPTION_TRANSFER_DELEGATOR_OFFSET, wallet),
                        data_pubkey_eq(SUBSCRIPTION_TRANSFER_MINT_OFFSET, USDC_MINT),
                    ],
                }],
            },
        }
    }

    fn pubkey_view(
        account_index: u8,
        pubkey: Pubkey,
        owner: Option<Pubkey>,
    ) -> SquadsAccountConstraintView {
        SquadsAccountConstraintView {
            account_index,
            kind: SquadsAccountConstraintKindView::Pubkey(vec![pubkey]),
            owner,
        }
    }

    fn account_data_view(
        account_index: u8,
        owner: Option<Pubkey>,
        data_constraints: Vec<SquadsDataConstraintView>,
    ) -> SquadsAccountConstraintView {
        SquadsAccountConstraintView {
            account_index,
            kind: SquadsAccountConstraintKindView::AccountData(data_constraints),
            owner,
        }
    }

    fn data_u8_eq(offset: u64, value: u8) -> SquadsDataConstraintView {
        SquadsDataConstraintView {
            data_offset: offset,
            data_value: SquadsDataValueView::U8(value),
            operator: SquadsDataOperatorView::Equals,
        }
    }

    fn data_pubkey_eq(offset: u64, pubkey: Pubkey) -> SquadsDataConstraintView {
        SquadsDataConstraintView {
            data_offset: offset,
            data_value: SquadsDataValueView::U8Slice(pubkey.to_bytes().to_vec()),
            operator: SquadsDataOperatorView::Equals,
        }
    }

    fn data_u64_lte(offset: u64, value: u64) -> SquadsDataConstraintView {
        SquadsDataConstraintView {
            data_offset: offset,
            data_value: SquadsDataValueView::U64Le(value),
            operator: SquadsDataOperatorView::LessThanOrEqualTo,
        }
    }

    async fn database_store() -> Option<OrchestratorStore> {
        let url = match std::env::var("DATABASE_URL") {
            Ok(url) => url,
            Err(_) => {
                eprintln!("skipping database test because DATABASE_URL is not set");
                return None;
            }
        };
        let store = OrchestratorStore::connect(
            NeonSqlConfig::new(url)
                .with_max_connections(1)
                .with_acquire_timeout(Duration::from_secs(10)),
        )
        .await
        .expect("connect to test database");
        store.apply_migrations().await.expect("apply migrations");
        Some(store)
    }

    fn unique_signature(test_name: &str) -> String {
        let nanos = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .expect("system clock after unix epoch")
            .as_nanos();
        format!("{test_name}-{nanos}")
    }

    async fn delete_policy_signature(store: &OrchestratorStore, signature: &str) {
        sqlx::query(
            r#"
            DELETE FROM loyal_yield.managed_vaults
            WHERE active_policy_id IN (
                SELECT id
                FROM loyal_yield.route_policies
                WHERE last_seen_signature = $1
            )
            "#,
        )
        .bind(signature)
        .execute(store.pool())
        .await
        .expect("delete test vault");
        sqlx::query("DELETE FROM loyal_yield.route_policies WHERE last_seen_signature = $1")
            .bind(signature)
            .execute(store.pool())
            .await
            .expect("delete test policy");
    }

    async fn delete_sweep_signature(store: &OrchestratorStore, signature: &str) {
        sqlx::query("DELETE FROM loyal_yield.balance_sweep_executions WHERE signature = $1")
            .bind(signature)
            .execute(store.pool())
            .await
            .expect("delete test sweep executions");
        sqlx::query("DELETE FROM loyal_yield.balance_sweep_targets WHERE last_seen_signature = $1")
            .bind(signature)
            .execute(store.pool())
            .await
            .expect("delete test sweep targets");
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

    fn notification_with_token_balances(
        signature: &str,
        slot: u64,
        source_ata: Pubkey,
        destination_ata: Pubkey,
        amount: u64,
    ) -> Value {
        let payer = Keypair::new();
        let instruction = Instruction {
            program_id: spl_token::id(),
            accounts: vec![
                AccountMeta::new(source_ata, false),
                AccountMeta::new(destination_ata, false),
            ],
            data: vec![],
        };
        let message = VersionedMessage::V0(
            v0::Message::try_compile(&payer.pubkey(), &[instruction], &[], Hash::new_unique())
                .unwrap(),
        );
        let transaction = VersionedTransaction::try_new(message, &[&payer]).unwrap();
        let bytes = bincode::serialize(&transaction).unwrap();
        let transaction_value = json!([BASE64_STANDARD.encode(bytes), "base64"]);
        let keys = transaction_account_keys(&transaction_value, None).unwrap();
        let source_index = keys.iter().position(|key| *key == source_ata).unwrap();
        let destination_index = keys.iter().position(|key| *key == destination_ata).unwrap();
        json!({
            "jsonrpc": "2.0",
            "method": "transactionNotification",
            "params": {
                "result": {
                    "signature": signature,
                    "slot": slot,
                    "transaction": {
                        "transaction": transaction_value,
                        "meta": {
                            "err": null,
                            "preTokenBalances": [
                                {
                                    "accountIndex": source_index,
                                    "mint": USDC_MINT.to_string(),
                                    "uiTokenAmount": { "amount": (amount * 4).to_string() }
                                },
                                {
                                    "accountIndex": destination_index,
                                    "mint": USDC_MINT.to_string(),
                                    "uiTokenAmount": { "amount": "0" }
                                }
                            ],
                            "postTokenBalances": [
                                {
                                    "accountIndex": source_index,
                                    "mint": USDC_MINT.to_string(),
                                    "uiTokenAmount": { "amount": (amount * 3).to_string() }
                                },
                                {
                                    "accountIndex": destination_index,
                                    "mint": USDC_MINT.to_string(),
                                    "uiTokenAmount": { "amount": amount.to_string() }
                                }
                            ]
                        }
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
        let event = yield_route_event(&monitor.sink.events[0]);
        assert_eq!(event.signature, "sig1");
        assert_eq!(event.slot, 42);
        assert_eq!(event.route_modes, vec!["same_mint", "cross_mint_jupiter"]);
    }

    #[tokio::test]
    async fn emits_balance_sweep_policy_event_from_sdk_classifier() {
        let mut monitor = PolicyMonitor::new(config(), VecSink::default());
        let transaction = json!(null);
        let notification = HeliusNotification {
            signature: "sweep-sig".to_owned(),
            slot: 77,
            transaction: &transaction,
            meta: None,
        };

        let emitted = monitor
            .process_detected_action(&notification, &balance_sweep_action())
            .await
            .unwrap();

        assert_eq!(emitted, 1);
        let event = balance_sweep_event(&monitor.sink.events[0]);
        assert_eq!(event.signature, "sweep-sig");
        assert_eq!(event.slot, 77);
        assert_eq!(event.max_amount_per_period, 250_000);
        assert_eq!(event.vault_index, 1);
    }

    #[tokio::test]
    async fn emits_balance_sweep_execution_only_from_proven_token_balance_movement() {
        let source = Pubkey::new_unique();
        let destination = Pubkey::new_unique();
        let mut monitor = PolicyMonitor::new(config(), VecSink::default());

        let emitted = monitor
            .process_notification(&notification_with_token_balances(
                "sweep-execution-sig",
                88,
                source,
                destination,
                123,
            ))
            .await
            .unwrap();

        assert_eq!(emitted, 1);
        assert!(monitor.sink.events.is_empty());
        assert_eq!(monitor.sink.executions.len(), 1);
        let execution = &monitor.sink.executions[0];
        assert_eq!(execution.signature, "sweep-execution-sig");
        assert_eq!(execution.source_wallet_ata, source.to_string());
        assert_eq!(execution.destination_vault_ata, destination.to_string());
        assert_eq!(execution.amount_raw, 123);
        assert_eq!(execution.source_pre_balance_raw, Some(492));
        assert_eq!(execution.source_post_balance_raw, Some(369));
        assert_eq!(execution.destination_pre_balance_raw, Some(0));
        assert_eq!(execution.destination_post_balance_raw, Some(123));
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
        let event = yield_route_event(&monitor.sink.events[0]);
        assert_eq!(event.universe_preset.as_deref(), Some("kamino_stable"));
        assert_eq!(event.risk_profile.as_deref(), Some("aggressive"));
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
        let event = yield_route_event(&monitor.sink.events[0]);
        assert_eq!(event.policy_account, chain_policy.to_string());
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

    #[tokio::test]
    async fn mocked_notification_records_policy_match_in_postgres_store() {
        let Some(store) = database_store().await else {
            return;
        };
        let signature = unique_signature("monitor-integration");
        delete_policy_signature(&store, &signature).await;

        let authority = Keypair::new();
        let setup = create_all_in_one_market_mint_yield_route_action(
            context(authority.pubkey()),
            universe(),
            vec![jupiter_lane()],
        )
        .unwrap();
        let mut monitor =
            PolicyMonitor::new(config(), PostgresPolicyMatchSink::from_store(store.clone()));

        let emitted = monitor
            .process_notification(&notification(
                &signature,
                4242,
                setup.instructions[0].clone(),
                &authority,
            ))
            .await
            .unwrap();

        assert_eq!(emitted, 1);
        let count: i64 = sqlx::query_scalar(
            "SELECT COUNT(*) FROM loyal_yield.route_policies WHERE last_seen_signature = $1",
        )
        .bind(&signature)
        .fetch_one(store.pool())
        .await
        .expect("count stored policy");
        assert_eq!(count, 1);

        delete_policy_signature(&store, &signature).await;
    }

    #[tokio::test]
    async fn mocked_notification_records_balance_sweep_execution_in_postgres_store() {
        let Some(store) = database_store().await else {
            return;
        };
        let signature = unique_signature("monitor-sweep-execution");
        let policy_signature = format!("{signature}-policy");
        delete_sweep_signature(&store, &policy_signature).await;
        delete_sweep_signature(&store, &signature).await;

        let source = Pubkey::new_unique();
        let destination = Pubkey::new_unique();
        store
            .record_balance_sweep_policy_match(BalanceSweepPolicyMatchInput {
                signature: policy_signature.clone(),
                slot: 1,
                cluster: Cluster::Devnet.to_string(),
                settings: Pubkey::new_unique().to_string(),
                authority: Pubkey::new_unique().to_string(),
                policy_seed: 1,
                policy_account: Pubkey::new_unique().to_string(),
                vault_index: 1,
                vault_pubkey: Pubkey::new_unique().to_string(),
                wallet: Pubkey::new_unique().to_string(),
                wallet_usdc_ata: source.to_string(),
                vault_usdc_ata: destination.to_string(),
                delegated_signers: vec![Pubkey::new_unique().to_string()],
                threshold: 1,
                max_amount_per_period: 1_000_000,
            })
            .await
            .expect("seed active sweep target");
        let mut monitor =
            PolicyMonitor::new(config(), PostgresPolicyMatchSink::from_store(store.clone()));

        let emitted = monitor
            .process_notification(&notification_with_token_balances(
                &signature,
                90,
                source,
                destination,
                444,
            ))
            .await
            .unwrap();

        assert_eq!(emitted, 1);
        let count: i64 = sqlx::query_scalar(
            "SELECT COUNT(*) FROM loyal_yield.balance_sweep_executions WHERE signature = $1",
        )
        .bind(&signature)
        .fetch_one(store.pool())
        .await
        .expect("count stored sweep execution");
        assert_eq!(count, 1);

        delete_sweep_signature(&store, &signature).await;
        delete_sweep_signature(&store, &policy_signature).await;
    }
}

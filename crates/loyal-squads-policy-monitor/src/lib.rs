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
            let targets = store.load_active_balance_sweep_targets().await?;
            for target in targets {
                if target.wallet_token_ata == event.source_wallet_ata
                    && target.vault_token_ata == event.destination_vault_ata
                    && target.token_mint == loyal_actions::USDC_MINT.to_string()
                {
                    store
                        .record_balance_sweep_execution(BalanceSweepExecutionInput {
                            target_id: target.id,
                            signature: event.signature,
                            slot: event.slot,
                            source_token_ata: event.source_wallet_ata.clone(),
                            source_wallet_ata: event.source_wallet_ata,
                            destination_token_ata: event.destination_vault_ata.clone(),
                            destination_vault_ata: event.destination_vault_ata,
                            token_mint: loyal_actions::USDC_MINT.to_string(),
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
        let wallet_usdc_ata = event.wallet_usdc_ata;
        let vault_usdc_ata = event.vault_usdc_ata;
        Self {
            signature: event.signature,
            slot: event.slot,
            settings: event.settings,
            authority: event.authority,
            policy_seed: event.policy_seed,
            policy_account: event.policy_account,
            vault_index: event.vault_index,
            vault_pubkey: event.vault_pubkey,
            wallet: event.wallet,
            wallet_token_ata: wallet_usdc_ata.clone(),
            wallet_usdc_ata,
            vault_token_ata: vault_usdc_ata.clone(),
            vault_usdc_ata,
            token_mint: loyal_actions::USDC_MINT.to_string(),
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

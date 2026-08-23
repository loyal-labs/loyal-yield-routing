#![allow(clippy::result_large_err)]

use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine};
use clap::ValueEnum;
use futures_util::{future::BoxFuture, SinkExt, StreamExt};
use loyal_actions::{
    decode_squads_policy_create_actions, detect_balance_sweep_policy_create,
    detect_jupiter_cross_mint_policy_action, detect_squads_policy_removals,
    detect_squads_policy_update_identity, detect_yield_route_policy_create,
    generalized_cross_mint_manifest_fingerprint, DetectedBalanceSweepPolicy,
    DetectedJupiterCrossMintPolicy, DetectedJupiterPolicyIdentity, DetectedPolicyRemoval,
    DetectedSwapLane, DetectedYieldRouteMode, DetectedYieldRoutePolicy, KaminoStableRiskProfile,
    SquadsSettingsActionView, YieldRouteUniversePreset, SQUADS_SMART_ACCOUNT_PROGRAM_ID,
};
use loyal_fleet_worker::multiply::{
    config::{derive_earn_max_topology, MANIFEST_VERSION},
    policy::{
        canonical_policy_payload, canonical_policy_update, current_policy_matches, PolicyFamily,
    },
};
use loyal_yield_store::{
    fleet_orchestration::StrategyKey, BalanceSweepExecutionInput, BalanceSweepPolicyMatchInput,
    CrossMintSwapPolicyManifestInput, EarnMaxPolicySetProjectionInput, OrchestratorConfig,
    OrchestratorError, OrchestratorStore, PolicyMatchInput, PolicyRemovalInput,
};
use serde::Serialize;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_client::nonblocking::rpc_client::RpcClient;
use solana_sdk::{
    commitment_config::CommitmentConfig,
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
    transaction::VersionedTransaction,
};
use std::{
    collections::{BTreeSet, HashSet},
    fmt,
    io::{self, Write},
    str::FromStr,
    time::Duration,
};
use tokio::time::{interval, sleep, MissedTickBehavior};
use tokio_tungstenite::{connect_async, tungstenite::Message};

const EARN_MAX_POLICY_PROJECTION_CONSUMER: &str = "earn_max_policy_sets_laserstream_v1";

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, ValueEnum)]
#[serde(rename_all = "lowercase")]
pub enum Cluster {
    Mainnet,
    Devnet,
}

impl fmt::Display for Cluster {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            // The CLI keeps the short `mainnet` spelling, while persisted
            // policy evidence must join the orchestration cluster exactly.
            Self::Mainnet => formatter.write_str("mainnet-beta"),
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

impl Commitment {
    const fn finalized_eligible(self) -> bool {
        matches!(self, Self::Finalized)
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
    fn project_earn_max_policy_set(
        &mut self,
        _input: EarnMaxPolicySetProjectionInput,
    ) -> BoxFuture<'_, Result<(), MonitorError>> {
        Box::pin(async { Ok(()) })
    }
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
                PolicyMonitorEvent::CrossMintSwapPolicyManifest(event) => {
                    store
                        .record_cross_mint_swap_policy_manifest(
                            CrossMintSwapPolicyManifestInput::from(event),
                        )
                        .await?;
                }
                PolicyMonitorEvent::PolicyRemoval(event) => {
                    store
                        .record_policy_removal(PolicyRemovalInput::from(event))
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

    fn project_earn_max_policy_set(
        &mut self,
        input: EarnMaxPolicySetProjectionInput,
    ) -> BoxFuture<'_, Result<(), MonitorError>> {
        let store = self.store.clone();
        Box::pin(async move {
            store
                .project_earn_max_policy_set(EARN_MAX_POLICY_PROJECTION_CONSUMER, input)
                .await?;
            Ok(())
        })
    }
}

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
#[serde(tag = "policy_kind", rename_all = "snake_case")]
pub enum PolicyMonitorEvent {
    YieldRoute(PolicyMatchEvent),
    BalanceSweep(BalanceSweepPolicyEvent),
    CrossMintSwapPolicyManifest(CrossMintSwapPolicyManifestEvent),
    PolicyRemoval(PolicyRemovalEvent),
}

#[derive(Debug, Clone, Copy, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum PolicyMutationKind {
    Create,
    Update,
    Remove,
}

impl PolicyMutationKind {
    const fn as_str(self) -> &'static str {
        match self {
            Self::Create => "create",
            Self::Update => "update",
            Self::Remove => "remove",
        }
    }
}

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct CrossMintSwapPolicyManifestEvent {
    pub signature: String,
    pub slot: u64,
    pub cluster: Cluster,
    pub source_commitment: String,
    pub finalized_eligible: bool,
    pub mutation: PolicyMutationKind,
    pub settings: String,
    pub authority: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub policy_seed: Option<u64>,
    pub policy_account: String,
    pub vault_index: u8,
    pub vault_pubkey: String,
    pub delegated_signer: String,
    pub source_shard: String,
    pub manifest_fingerprint: String,
    pub max_slippage_bps: u16,
    pub daily_source_mint_spending_cap: u64,
}

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct PolicyRemovalEvent {
    pub signature: String,
    pub slot: u64,
    pub cluster: Cluster,
    pub source_commitment: String,
    pub finalized_eligible: bool,
    pub mutation: PolicyMutationKind,
    pub settings: String,
    pub authority: String,
    pub policy_account: String,
}

#[derive(Debug, Clone, Serialize, PartialEq, Eq)]
pub struct PolicyMatchEvent {
    pub signature: String,
    pub slot: u64,
    pub cluster: Cluster,
    pub source_commitment: String,
    pub finalized_eligible: bool,
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
    earn_max_rpc: Option<RpcClient>,
    earn_max_delegate: Option<Pubkey>,
    seen_signatures: HashSet<String>,
}

impl<S: PolicyMatchSink> PolicyMonitor<S> {
    pub fn new(config: MonitorConfig, sink: S) -> Self {
        Self {
            config,
            sink,
            earn_max_rpc: None,
            earn_max_delegate: None,
            seen_signatures: HashSet::new(),
        }
    }

    pub fn with_earn_max_projection(mut self, rpc_url: String, delegate: Pubkey) -> Self {
        self.earn_max_rpc = Some(RpcClient::new_with_commitment(
            rpc_url,
            CommitmentConfig::confirmed(),
        ));
        self.earn_max_delegate = Some(delegate);
        self
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
        if self.seen_signatures.contains(&notification.signature) {
            return Ok(0);
        }

        let instructions = decode_squads_instructions(notification.transaction, notification.meta)?;
        let mut emitted = self
            .process_policy_instructions_inner(
                &notification.signature,
                notification.slot,
                instructions,
            )
            .await?;
        for execution in detect_balance_sweep_execution_events(
            &notification,
            self.config.cluster,
            self.config.commitment,
        )? {
            self.sink.emit_execution(execution).await?;
            emitted += 1;
        }

        // Only acknowledge the transaction after every database sink write has
        // succeeded. A failed write must be replayable after the websocket
        // reconnects; marking it before the sink call would permanently lose
        // policy removals (and leave their catalog rows active).
        self.seen_signatures.insert(notification.signature);

        Ok(emitted)
    }

    /// Processes already-decoded Squads instructions from another transport.
    /// LaserStream uses this entrypoint so policy semantics and persistence stay
    /// identical without opening a second websocket connection.
    pub async fn process_policy_instructions(
        &mut self,
        signature: &str,
        slot: u64,
        instructions: Vec<Instruction>,
    ) -> Result<usize, MonitorError> {
        if self.seen_signatures.contains(signature) {
            return Ok(0);
        }
        let emitted = self
            .process_policy_instructions_inner(signature, slot, instructions)
            .await?;
        self.seen_signatures.insert(signature.to_owned());
        Ok(emitted)
    }

    async fn process_policy_instructions_inner(
        &mut self,
        signature: &str,
        slot: u64,
        instructions: Vec<Instruction>,
    ) -> Result<usize, MonitorError> {
        let mut emitted = 0;
        let mut earn_max_settings = BTreeSet::new();
        for instruction in instructions {
            if self.earn_max_rpc.is_some() {
                earn_max_settings.extend(affected_earn_max_settings(&instruction)?);
            }
            if let Ok(Some(policy)) = detect_jupiter_cross_mint_policy_action(&instruction) {
                self.sink
                    .emit(PolicyMonitorEvent::CrossMintSwapPolicyManifest(
                        CrossMintSwapPolicyManifestEvent::from_policy(
                            signature,
                            slot,
                            self.config.cluster,
                            self.config.commitment,
                            policy,
                        ),
                    ))
                    .await?;
                emitted += 1;
                continue;
            }
            if let Ok(removals) = detect_squads_policy_removals(&instruction) {
                if !removals.is_empty() {
                    for removal in removals {
                        self.sink
                            .emit(PolicyMonitorEvent::PolicyRemoval(
                                PolicyRemovalEvent::from_removal(
                                    signature,
                                    slot,
                                    self.config.cluster,
                                    self.config.commitment,
                                    removal,
                                ),
                            ))
                            .await?;
                        emitted += 1;
                    }
                    continue;
                }
            }
            let actions = match decode_squads_policy_create_actions(&instruction) {
                Ok(actions) => actions,
                Err(_) => continue,
            };
            let mut recognized = 0;
            for action in actions {
                recognized += self
                    .process_detected_action(signature, slot, &action)
                    .await?;
            }
            if recognized == 0 {
                if let Ok(Some(update)) = detect_squads_policy_update_identity(&instruction) {
                    self.sink
                        .emit(PolicyMonitorEvent::PolicyRemoval(
                            PolicyRemovalEvent::from_update_invalidation(
                                signature,
                                slot,
                                self.config.cluster,
                                self.config.commitment,
                                update,
                            ),
                        ))
                        .await?;
                    emitted += 1;
                    continue;
                }
            }
            emitted += recognized;
        }
        for settings in earn_max_settings {
            self.project_earn_max_manifest(settings, signature, slot)
                .await?;
            emitted += 1;
        }
        Ok(emitted)
    }

    async fn project_earn_max_manifest(
        &mut self,
        settings: Pubkey,
        signature: &str,
        slot: u64,
    ) -> Result<(), MonitorError> {
        let rpc = self.earn_max_rpc.as_ref().ok_or_else(|| {
            MonitorError::Decode("Earn MAX projection RPC is not configured".to_owned())
        })?;
        let delegate = self.earn_max_delegate.ok_or_else(|| {
            MonitorError::Decode("Earn MAX projection delegate is not configured".to_owned())
        })?;
        let topology = derive_earn_max_topology(settings)
            .map_err(|error| MonitorError::Decode(error.to_string()))?;
        let strategy = topology.strategy(StrategyKey::SyrupUsdcUsdc);
        let families = [
            PolicyFamily::Deposit,
            PolicyFamily::Borrow,
            PolicyFamily::SwapClaimToCollateral,
            PolicyFamily::SwapCollateralToClaim,
            PolicyFamily::Repay,
            PolicyFamily::Withdraw,
        ];
        let mut expected = Vec::with_capacity(families.len());
        for family in families {
            let policy = family.policy(strategy);
            let update = canonical_policy_update(
                topology, strategy, family, settings, settings, delegate,
            )
            .map_err(|error| MonitorError::Decode(error.to_string()))?;
            let payload = canonical_policy_payload(&update)
                .map_err(|error| MonitorError::Decode(error.to_string()))?;
            expected.push((family, policy, update, payload));
        }
        let addresses = expected
            .iter()
            .map(|(_, policy, _, _)| policy.account)
            .collect::<Vec<_>>();
        let response = rpc
            .get_multiple_accounts_with_commitment(&addresses, CommitmentConfig::confirmed())
            .await
            .map_err(|error| {
                MonitorError::Decode(format!("confirmed policy reload failed: {error}"))
            })?;
        let mut policy_accounts = Vec::with_capacity(expected.len());
        let mut manifest_basis = Vec::with_capacity(expected.len());
        let mut matched = 0_usize;
        let mut present = 0_usize;
        for ((family, policy, update, payload), account) in
            expected.into_iter().zip(response.value.into_iter())
        {
            let semantic_sha256 = format!("{:x}", Sha256::digest(&update.data));
            let (exists, matches, data_sha256) = match account {
                Some(account) => {
                    present += 1;
                    let matches = account.owner == SQUADS_SMART_ACCOUNT_PROGRAM_ID
                        && !account.executable
                        && current_policy_matches(&account.data, policy, delegate, &payload)
                            .map_err(|error| MonitorError::Decode(error.to_string()))?;
                    if matches {
                        matched += 1;
                    }
                    (
                        true,
                        matches,
                        Some(format!("{:x}", Sha256::digest(&account.data))),
                    )
                }
                None => (false, false, None),
            };
            let family = format!("{family:?}").to_ascii_lowercase();
            manifest_basis.push(json!({
                "family": family.as_str(),
                "seed": policy.seed,
                "account": policy.account.to_string(),
                "semanticSha256": semantic_sha256,
            }));
            policy_accounts.push(json!({
                "family": family.as_str(),
                "seed": policy.seed,
                "account": policy.account.to_string(),
                "semanticSha256": semantic_sha256,
                "dataSha256": data_sha256,
                "exists": exists,
                "matches": matches,
            }));
        }
        let manifest = json!({
            "version": MANIFEST_VERSION,
            "settings": settings.to_string(),
            "vaultIndex": topology.vault_index,
            "vault": topology.vault.to_string(),
            "policies": manifest_basis,
        });
        let manifest_sha256 = format!(
            "{:x}",
            Sha256::digest(serde_json::to_vec(&manifest).map_err(MonitorError::Json)?)
        );
        let status = if matched == addresses.len() {
            "ready"
        } else if present == 0 {
            "removed"
        } else {
            "incomplete"
        };
        self.sink
            .project_earn_max_policy_set(EarnMaxPolicySetProjectionInput {
                settings: settings.to_string(),
                vault_index: topology.vault_index,
                vault: topology.vault.to_string(),
                manifest_version: MANIFEST_VERSION.to_owned(),
                manifest_sha256,
                status: status.to_owned(),
                policy_accounts: Value::Array(policy_accounts),
                observed_signature: signature.to_owned(),
                observed_slot: slot,
                observed_at: chrono::Utc::now(),
            })
            .await
    }

    async fn process_detected_action(
        &mut self,
        signature: &str,
        slot: u64,
        action: &SquadsSettingsActionView,
    ) -> Result<usize, MonitorError> {
        let mut emitted = 0;
        if let Some(policy) = detect_yield_route_policy_create(action) {
            self.sink
                .emit(PolicyMonitorEvent::YieldRoute(
                    PolicyMatchEvent::from_policy(
                        signature,
                        slot,
                        self.config.cluster,
                        self.config.commitment,
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
                        signature,
                        slot,
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

fn affected_earn_max_settings(instruction: &Instruction) -> Result<Vec<Pubkey>, MonitorError> {
    let mut settings = BTreeSet::new();
    if let Ok(actions) = decode_squads_policy_create_actions(instruction) {
        for action in actions {
            if is_earn_max_policy(action.settings, action.policy_account)? {
                settings.insert(action.settings);
            }
        }
    }
    if let Ok(removals) = detect_squads_policy_removals(instruction) {
        for identity in removals {
            if is_earn_max_policy(identity.settings, identity.policy_account)? {
                settings.insert(identity.settings);
            }
        }
    }
    if let Ok(Some(identity)) = detect_squads_policy_update_identity(instruction) {
        if is_earn_max_policy(identity.settings, identity.policy_account)? {
            settings.insert(identity.settings);
        }
    }
    Ok(settings.into_iter().collect())
}

fn is_earn_max_policy(settings: Pubkey, account: Pubkey) -> Result<bool, MonitorError> {
    let topology = derive_earn_max_topology(settings)
        .map_err(|error| MonitorError::Decode(error.to_string()))?;
    let strategy = topology.strategy(StrategyKey::SyrupUsdcUsdc);
    Ok([
        PolicyFamily::Deposit,
        PolicyFamily::Borrow,
        PolicyFamily::SwapClaimToCollateral,
        PolicyFamily::SwapCollateralToClaim,
        PolicyFamily::Repay,
        PolicyFamily::Withdraw,
    ]
    .into_iter()
    .any(|family| family.policy(strategy).account == account))
}

impl CrossMintSwapPolicyManifestEvent {
    fn from_policy(
        signature: &str,
        slot: u64,
        cluster: Cluster,
        commitment: Commitment,
        policy: DetectedJupiterCrossMintPolicy,
    ) -> Self {
        let mutation = match policy.identity {
            DetectedJupiterPolicyIdentity::Create { .. } => PolicyMutationKind::Create,
            DetectedJupiterPolicyIdentity::Update { .. } => PolicyMutationKind::Update,
        };
        let manifest_fingerprint =
            generalized_cross_mint_manifest_fingerprint(&policy.manifest_semantics());

        Self {
            signature: signature.to_owned(),
            slot,
            cluster,
            source_commitment: commitment.to_string(),
            finalized_eligible: commitment.finalized_eligible(),
            mutation,
            settings: policy.settings.to_string(),
            authority: policy.authority.to_string(),
            policy_seed: policy.identity.policy_seed(),
            policy_account: policy.identity.policy_account().to_string(),
            vault_index: policy.account_index,
            vault_pubkey: policy.vault.to_string(),
            delegated_signer: policy.delegated_signer.to_string(),
            source_shard: match policy.source_shard {
                loyal_actions::jupiter::JupiterCrossMintSourceShard::Classic => {
                    "classic".to_owned()
                }
                loyal_actions::jupiter::JupiterCrossMintSourceShard::Token2022 => {
                    "token_2022".to_owned()
                }
            },
            manifest_fingerprint,
            max_slippage_bps: policy.max_slippage_bps,
            daily_source_mint_spending_cap: policy.daily_source_mint_spending_cap,
        }
    }
}

impl PolicyRemovalEvent {
    fn from_removal(
        signature: &str,
        slot: u64,
        cluster: Cluster,
        commitment: Commitment,
        removal: DetectedPolicyRemoval,
    ) -> Self {
        Self {
            signature: signature.to_owned(),
            slot,
            cluster,
            source_commitment: commitment.to_string(),
            finalized_eligible: commitment.finalized_eligible(),
            mutation: PolicyMutationKind::Remove,
            settings: removal.settings.to_string(),
            authority: removal.authority.to_string(),
            policy_account: removal.policy_account.to_string(),
        }
    }

    fn from_update_invalidation(
        signature: &str,
        slot: u64,
        cluster: Cluster,
        commitment: Commitment,
        update: DetectedPolicyRemoval,
    ) -> Self {
        Self {
            signature: signature.to_owned(),
            slot,
            cluster,
            source_commitment: commitment.to_string(),
            finalized_eligible: false,
            mutation: PolicyMutationKind::Update,
            settings: update.settings.to_string(),
            authority: update.authority.to_string(),
            policy_account: update.policy_account.to_string(),
        }
    }
}

impl PolicyMatchEvent {
    fn from_policy(
        signature: &str,
        slot: u64,
        cluster: Cluster,
        commitment: Commitment,
        policy: DetectedYieldRoutePolicy,
    ) -> Self {
        Self {
            signature: signature.to_owned(),
            slot,
            cluster,
            source_commitment: commitment.to_string(),
            finalized_eligible: commitment.finalized_eligible(),
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
            source_commitment: event.source_commitment,
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

impl From<CrossMintSwapPolicyManifestEvent> for CrossMintSwapPolicyManifestInput {
    fn from(event: CrossMintSwapPolicyManifestEvent) -> Self {
        Self {
            signature: event.signature,
            slot: event.slot,
            cluster: event.cluster.to_string(),
            source_commitment: event.source_commitment,
            mutation: event.mutation.as_str().to_owned(),
            settings: event.settings,
            authority: event.authority,
            policy_seed: event.policy_seed,
            policy_account: event.policy_account,
            vault_index: event.vault_index,
            vault_pubkey: event.vault_pubkey,
            delegated_signer: event.delegated_signer,
            source_shard: event.source_shard,
            manifest_fingerprint: event.manifest_fingerprint,
            max_slippage_bps: event.max_slippage_bps,
            daily_source_mint_spending_cap: event.daily_source_mint_spending_cap,
        }
    }
}

impl From<PolicyRemovalEvent> for PolicyRemovalInput {
    fn from(event: PolicyRemovalEvent) -> Self {
        Self {
            signature: event.signature,
            slot: event.slot,
            cluster: event.cluster.to_string(),
            source_commitment: event.source_commitment,
            settings: event.settings,
            authority: event.authority,
            policy_account: event.policy_account,
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
        DetectedYieldRouteMode::SameMint => "same_mint_kamino",
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
            .filter_map(|index| {
                let index = *index as usize;
                account_keys.get(index).copied().map(|pubkey| AccountMeta {
                    pubkey,
                    is_signer: transaction.message.is_signer(index),
                    is_writable: transaction.message.is_maybe_writable(index, None),
                })
            })
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
    use loyal_actions::jupiter::{JupiterCrossMintPolicySpec, JupiterCrossMintSourceShard};
    use loyal_actions::{
        create_jupiter_cross_mint_policy_action, derive_squads_vault as sdk_derive_squads_vault,
        remove_policy_instruction, LoyalActionContext,
    };
    use solana_sdk::{
        message::{Message as SolanaMessage, VersionedMessage},
        signature::Signature,
    };

    #[test]
    fn mainnet_events_use_the_orchestration_cluster_name() {
        assert_eq!(Cluster::Mainnet.to_string(), "mainnet-beta");
        assert_eq!(Cluster::Devnet.to_string(), "devnet");
    }

    #[derive(Default)]
    struct RecordingSink {
        events: Vec<PolicyMonitorEvent>,
        executions: Vec<BalanceSweepExecutionEvent>,
    }

    impl PolicyMatchSink for RecordingSink {
        fn emit(&mut self, event: PolicyMonitorEvent) -> BoxFuture<'_, Result<(), MonitorError>> {
            self.events.push(event);
            Box::pin(async { Ok(()) })
        }

        fn emit_execution(
            &mut self,
            event: BalanceSweepExecutionEvent,
        ) -> BoxFuture<'_, Result<(), MonitorError>> {
            self.executions.push(event);
            Box::pin(async { Ok(()) })
        }
    }

    struct RetryOnceSink {
        fail_emits: usize,
        events: Vec<PolicyMonitorEvent>,
    }

    impl PolicyMatchSink for RetryOnceSink {
        fn emit(&mut self, event: PolicyMonitorEvent) -> BoxFuture<'_, Result<(), MonitorError>> {
            if self.fail_emits > 0 {
                self.fail_emits -= 1;
                return Box::pin(async {
                    Err(MonitorError::Decode("transient sink failure".to_owned()))
                });
            }
            self.events.push(event);
            Box::pin(async { Ok(()) })
        }

        fn emit_execution(
            &mut self,
            _event: BalanceSweepExecutionEvent,
        ) -> BoxFuture<'_, Result<(), MonitorError>> {
            Box::pin(async { Ok(()) })
        }
    }

    fn action_context() -> LoyalActionContext {
        let settings = Pubkey::new_unique();
        let account_index = 7;
        LoyalActionContext {
            settings,
            authority: Pubkey::new_unique(),
            delegated_signer: Pubkey::new_unique(),
            account_index,
            vault: sdk_derive_squads_vault(&settings, account_index).0,
        }
    }

    fn monitor(commitment: Commitment) -> PolicyMonitor<RecordingSink> {
        PolicyMonitor::new(
            MonitorConfig::new(
                Cluster::Mainnet,
                commitment,
                Some("ws://test.invalid".to_owned()),
                None,
            )
            .expect("test monitor config"),
            RecordingSink::default(),
        )
    }

    fn notification(instruction: Instruction, payer: Pubkey, signature: &str, slot: u64) -> Value {
        let message = SolanaMessage::new(&[instruction], Some(&payer));
        let transaction = VersionedTransaction {
            signatures: vec![
                Signature::default();
                usize::from(message.header.num_required_signatures)
            ],
            message: VersionedMessage::Legacy(message),
        };
        let encoded = BASE64_STANDARD
            .encode(bincode::serialize(&transaction).expect("serialize test settings transaction"));
        json!({
            "method": "transactionNotification",
            "params": {
                "result": {
                    "signature": signature,
                    "slot": slot,
                    "transaction": {
                        "transaction": [encoded, "base64"],
                        "meta": { "err": null }
                    }
                }
            }
        })
    }

    #[tokio::test]
    async fn generalized_swap_create_emits_one_atomic_manifest() {
        let context = action_context();
        let action = create_jupiter_cross_mint_policy_action(
            context,
            JupiterCrossMintPolicySpec {
                source_shard: JupiterCrossMintSourceShard::Classic,
                max_slippage_bps: 50,
                daily_source_mint_spending_cap: 1_000_000_000,
            },
            51,
        )
        .expect("create generalized swap policy");
        let detected = loyal_actions::detect_jupiter_cross_mint_policy_action(&action.instruction)
            .unwrap()
            .expect("detect generalized policy for canonical fingerprint");
        let expected_fingerprint = loyal_actions::generalized_cross_mint_manifest_fingerprint(
            &detected.manifest_semantics(),
        );
        let mut monitor = monitor(Commitment::Finalized);

        let emitted = monitor
            .process_notification(&notification(
                action.instruction,
                context.authority,
                "generalized-swap-create",
                111,
            ))
            .await
            .expect("process generalized swap create");

        assert_eq!(emitted, 1);
        let [PolicyMonitorEvent::CrossMintSwapPolicyManifest(event)] =
            monitor.sink.events.as_slice()
        else {
            panic!("generalized create must emit one policy-wide manifest");
        };
        assert_eq!(event.policy_account, action.account.to_string());
        assert_eq!(event.source_shard, "classic");
        assert_eq!(event.manifest_fingerprint.len(), 64);
        assert_eq!(event.manifest_fingerprint, expected_fingerprint);
        assert_eq!(event.max_slippage_bps, 50);
        assert_eq!(event.daily_source_mint_spending_cap, 1_000_000_000);
    }

    #[tokio::test]
    async fn removal_emits_family_neutral_policy_account_event() {
        let context = action_context();
        let policy_account = Pubkey::new_unique();
        let instruction =
            remove_policy_instruction(context.settings, context.authority, policy_account);
        let mut monitor = monitor(Commitment::Finalized);

        let emitted = monitor
            .process_notification(&notification(
                instruction,
                context.authority,
                "policy-remove",
                103,
            ))
            .await
            .expect("process policy removal");

        assert_eq!(emitted, 1);
        let [PolicyMonitorEvent::PolicyRemoval(event)] = monitor.sink.events.as_slice() else {
            panic!("removal must emit exactly one family-neutral event");
        };
        assert_eq!(event.mutation, PolicyMutationKind::Remove);
        assert_eq!(event.policy_account, policy_account.to_string());
        assert_eq!(event.settings, context.settings.to_string());
        assert_eq!(event.authority, context.authority.to_string());
        assert!(event.finalized_eligible);

        let serialized = serde_json::to_value(&monitor.sink.events[0])
            .expect("serialize family-neutral removal");
        assert_eq!(serialized["policy_kind"], "policy_removal");
        assert!(serialized.get("policy_seed").is_none());
        assert!(serialized.get("policy_family").is_none());
    }

    #[tokio::test]
    async fn failed_sink_does_not_acknowledge_policy_removal_transaction() {
        let context = action_context();
        let policy_account = Pubkey::new_unique();
        let instruction =
            remove_policy_instruction(context.settings, context.authority, policy_account);
        let mut monitor = PolicyMonitor::new(
            MonitorConfig::new(
                Cluster::Mainnet,
                Commitment::Finalized,
                Some("ws://test.invalid".to_owned()),
                None,
            )
            .expect("test monitor config"),
            RetryOnceSink {
                fail_emits: 1,
                events: Vec::new(),
            },
        );
        let value = notification(instruction, context.authority, "retry-removal", 105);

        assert!(monitor.process_notification(&value).await.is_err());
        assert_eq!(monitor.sink.events.len(), 0);

        assert_eq!(monitor.process_notification(&value).await.unwrap(), 1);
        assert_eq!(monitor.sink.events.len(), 1);
        assert!(matches!(
            monitor.sink.events[0],
            PolicyMonitorEvent::PolicyRemoval(_)
        ));
    }

    #[tokio::test]
    async fn confirmed_observation_is_diagnostic_but_finalized_observation_is_eligible() {
        let context = action_context();
        let action = create_jupiter_cross_mint_policy_action(
            context,
            JupiterCrossMintPolicySpec {
                source_shard: JupiterCrossMintSourceShard::Classic,
                max_slippage_bps: 50,
                daily_source_mint_spending_cap: 1_000_000_000,
            },
            42,
        )
        .expect("create generalized swap policy");
        let mut confirmed = monitor(Commitment::Confirmed);
        let mut finalized = monitor(Commitment::Finalized);

        confirmed
            .process_notification(&notification(
                action.instruction.clone(),
                context.authority,
                "confirmed-create",
                104,
            ))
            .await
            .expect("process confirmed observation");
        finalized
            .process_notification(&notification(
                action.instruction,
                context.authority,
                "finalized-create",
                104,
            ))
            .await
            .expect("process finalized observation");

        let [PolicyMonitorEvent::CrossMintSwapPolicyManifest(confirmed_event)] =
            confirmed.sink.events.as_slice()
        else {
            panic!("expected confirmed generalized policy observation");
        };
        let [PolicyMonitorEvent::CrossMintSwapPolicyManifest(finalized_event)] =
            finalized.sink.events.as_slice()
        else {
            panic!("expected finalized generalized policy observation");
        };
        assert_eq!(confirmed_event.source_commitment, "confirmed");
        assert!(!confirmed_event.finalized_eligible);
        assert_eq!(finalized_event.source_commitment, "finalized");
        assert!(finalized_event.finalized_eligible);
    }
}

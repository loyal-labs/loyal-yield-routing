
fn load_analysis() -> Analysis {
    read_json(&repo_root_path(ANALYSIS_PATH))
}

fn load_history_cache() -> HistoryCache {
    read_json(&repo_root_path(HISTORY_CACHE_PATH))
}

fn repo_root_path(relative_path: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../..")
        .join(relative_path)
}

fn read_json<T: serde::de::DeserializeOwned>(path: &Path) -> T {
    let bytes = fs::read(path).unwrap_or_else(|error| {
        panic!(
            "failed to read {}; run `bun scripts/analyze-kamino-hourly-reserves.mjs --cache-only`: {error}",
            path.display()
        )
    });
    serde_json::from_slice(&bytes)
        .unwrap_or_else(|error| panic!("failed to parse {} as JSON: {error}", path.display()))
}

fn pubkey_from_str(value: &str) -> Pubkey {
    Pubkey::from_str(value).expect("parse pubkey")
}

fn value_as_u8(value: Option<&serde_json::Value>) -> Option<u8> {
    value_as_f64(value).map(|value| value as u8)
}

fn value_as_f64(value: Option<&serde_json::Value>) -> Option<f64> {
    match value? {
        serde_json::Value::Number(number) => number.as_f64(),
        serde_json::Value::String(string) => string.parse().ok(),
        _ => None,
    }
}

#[derive(Clone, Debug)]
struct Backtest {
    reserves: Vec<ReserveMeta>,
    hourly_choices: Vec<HourlyChoices>,
    point_lookup: HashMap<(usize, String), Choice>,
    time_index: HashMap<String, usize>,
    end_timestamp: String,
}

impl Backtest {
    fn point_at(&self, reserve_index: usize, timestamp: &str) -> Option<&Choice> {
        self.point_lookup
            .get(&(reserve_index, timestamp.to_owned()))
    }
}

#[derive(Clone, Debug)]
struct ReserveMeta {
    market_address: Pubkey,
    reserve_address: Pubkey,
    mint_address: Pubkey,
    decimals: u8,
}

#[derive(Clone, Debug)]
struct HourlyChoices {
    timestamp: String,
    choices: Vec<Choice>,
}

#[derive(Clone, Debug)]
struct Choice {
    reserve_index: usize,
    timestamp: String,
    market_address: Pubkey,
    reserve_address: Pubkey,
    mint_address: Pubkey,
    decimals: u8,
    supply_apy: f64,
    deposit_tvl: f64,
    asset_oracle_price_usd: f64,
}

#[derive(Clone, Debug)]
struct DynamicState {
    value_usd: f64,
    point: Choice,
    prev_key: usize,
}

#[derive(Clone, Debug)]
struct HindsightRoute {
    path: Vec<RouteStep>,
    ending_value_usd: f64,
}

#[derive(Clone, Debug)]
struct RouteStep {
    timestamp: String,
    point: Choice,
}

#[derive(Clone, Debug)]
enum HubPricing {
    ZeroFee,
    Discounted { share_of_jupiter: f64, cap_bps: f64 },
}

impl HubPricing {
    fn fee_fraction(&self, jupiter_loss_fraction: f64) -> f64 {
        match self {
            Self::ZeroFee => 0.0,
            Self::Discounted {
                share_of_jupiter,
                cap_bps,
            } => (jupiter_loss_fraction * share_of_jupiter).min(cap_bps / 10_000.0),
        }
    }
}

#[derive(Clone, Debug)]
struct HubTransition {
    route_instructions: Vec<Instruction>,
    treasury_rebalance_instruction: Option<Instruction>,
    next_amount_raw: u64,
    needs_hub_authorizer: bool,
    hub_fee_revenue_usd: f64,
    equivalent_jupiter_user_loss_usd: f64,
}

#[derive(Clone, Debug)]
struct JupiterLikeFeeCandidate {
    share_of_jupiter: f64,
    modeled_apy: f64,
    pricing: HubPricing,
    route: HindsightRoute,
}

#[derive(Clone, Debug)]
struct HubRouteReport {
    skipped: bool,
    user_gross_value_usd: f64,
    user_net_value_usd: f64,
    route_tx_fees_usd: f64,
    treasury_rebalance_loss_usd: f64,
    treasury_rebalance_tx_fees_usd: f64,
    treasury_net_after_fees_usd: f64,
    hub_fee_revenue_usd: f64,
    equivalent_jupiter_user_loss_usd: f64,
    cross_mint_rebalances: u64,
}

impl HubRouteReport {
    fn skipped() -> Self {
        Self {
            skipped: true,
            user_gross_value_usd: 0.0,
            user_net_value_usd: 0.0,
            route_tx_fees_usd: 0.0,
            treasury_rebalance_loss_usd: 0.0,
            treasury_rebalance_tx_fees_usd: 0.0,
            treasury_net_after_fees_usd: 0.0,
            hub_fee_revenue_usd: 0.0,
            equivalent_jupiter_user_loss_usd: 0.0,
            cross_mint_rebalances: 0,
        }
    }
}

#[derive(Clone, Debug)]
struct TreasurySquads {
    pool: SquadsPool,
    vault_index: u8,
    vault: Pubkey,
    token_accounts: HashMap<Pubkey, Pubkey>,
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct Analysis {
    assumptions: AnalysisAssumptions,
    jupiter_costs: HashMap<String, JupiterCost>,
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct AnalysisAssumptions {
    requested_start: String,
    requested_end: String,
    frequency: String,
    pool_change_lamports: u64,
}

#[derive(Clone, Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct JupiterCost {
    available: bool,
    loss_fraction: Option<f64>,
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct HistoryCache {
    reserve_histories: Vec<ReserveHistory>,
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct ReserveHistory {
    market: Market,
    reserve_address: String,
    history: ReserveMetricHistory,
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct Market {
    lending_market: String,
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct ReserveMetricHistory {
    history: Vec<HistoryPoint>,
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct HistoryPoint {
    timestamp: String,
    metrics: Metrics,
}

#[derive(Debug, serde::Deserialize)]
#[serde(rename_all = "camelCase")]
struct Metrics {
    #[serde(default)]
    symbol: Option<String>,
    #[serde(default)]
    decimals: Option<serde_json::Value>,
    #[serde(default)]
    #[serde(rename = "mintAddress")]
    mint_address: Option<String>,
    #[serde(default)]
    #[serde(rename = "supplyInterestAPY")]
    supply_interest_apy: Option<serde_json::Value>,
    #[serde(default)]
    #[serde(rename = "depositTvl")]
    deposit_tvl: Option<serde_json::Value>,
    #[serde(default)]
    #[serde(rename = "assetOraclePriceUSD")]
    asset_oracle_price_usd: Option<serde_json::Value>,
    #[serde(default)]
    #[serde(rename = "assetPriceUSD")]
    asset_price_usd: Option<serde_json::Value>,
}

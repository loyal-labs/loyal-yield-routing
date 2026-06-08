use std::{future::Future, time::Duration};

use base64::{engine::general_purpose::STANDARD as BASE64_STANDARD, Engine as _};
use loyal_actions::JUPITER_SWAP_DISCRIMINATOR;
use reqwest::Url;
use serde::Deserialize;
use serde_json::{json, Value};

use crate::{
    CrossMintQuote, CrossMintQuoteRequest, CrossMintSwapLaneKind, RouteAccountMetaConfig,
    RouteInstructionConfig, RouteQuoteError, RouteQuoteProvider, SameMintQuote,
    SameMintQuoteRequest, SwapQuote,
};

const DEFAULT_JUPITER_BASE_URL: &str = "https://lite-api.jup.ag/swap/v1";
const DEFAULT_TIMEOUT_SECS: u64 = 20;

#[derive(Debug, Clone)]
pub struct JupiterRouteQuoteProvider {
    client: reqwest::Client,
    base_url: String,
}

impl Default for JupiterRouteQuoteProvider {
    fn default() -> Self {
        Self {
            client: reqwest::Client::builder()
                .timeout(Duration::from_secs(DEFAULT_TIMEOUT_SECS))
                .build()
                .expect("build Jupiter reqwest client"),
            base_url: DEFAULT_JUPITER_BASE_URL.to_owned(),
        }
    }
}

impl JupiterRouteQuoteProvider {
    pub fn new(base_url: impl Into<String>) -> Self {
        Self {
            base_url: base_url.into(),
            ..Self::default()
        }
    }
}

impl RouteQuoteProvider for JupiterRouteQuoteProvider {
    fn quote_same_mint(
        &self,
        request: SameMintQuoteRequest<'_>,
    ) -> impl Future<Output = Result<SameMintQuote, RouteQuoteError>> + Send {
        async move { Ok(SameMintQuote::passthrough(request.amount)) }
    }

    fn quote_cross_mint(
        &self,
        request: CrossMintQuoteRequest<'_>,
    ) -> impl Future<Output = Result<CrossMintQuote, RouteQuoteError>> + Send {
        async move {
            if request.lane.kind != CrossMintSwapLaneKind::Jupiter {
                return Err(RouteQuoteError::Unavailable(
                    "live quote provider only supports Jupiter lanes".to_owned(),
                ));
            }

            let slippage_bps = request.lane.max_slippage_bps.unwrap_or(100);
            let quote = self.fetch_quote(&request, slippage_bps).await?;
            let swap = self
                .fetch_swap_instruction(request.vault_pubkey, &quote.raw)
                .await?;

            if !swap.setup_instructions.is_empty() {
                return Err(RouteQuoteError::Unavailable(format!(
                    "Jupiter returned {} setup instructions; pre-create token accounts before routing",
                    swap.setup_instructions.len()
                )));
            }
            if swap.cleanup_instruction.is_some() {
                return Err(RouteQuoteError::Unavailable(
                    "Jupiter returned a cleanup instruction; cleanup instructions are not supported by the route policy".to_owned(),
                ));
            }

            let swap_instruction = swap.swap_instruction.ok_or_else(|| {
                RouteQuoteError::Unavailable(
                    "Jupiter response did not include swapInstruction".to_owned(),
                )
            })?;
            let route_instruction = swap_instruction.into_route_instruction()?;
            if !route_instruction
                .data
                .starts_with(&JUPITER_SWAP_DISCRIMINATOR)
            {
                return Err(RouteQuoteError::Unavailable(
                    "Jupiter returned a swap instruction discriminator outside the configured policy".to_owned(),
                ));
            }

            Ok(CrossMintQuote {
                redeem_collateral_amount: request.amount,
                redeem_liquidity_amount: request.redeem_liquidity_amount,
                swap: SwapQuote {
                    lane_kind: request.lane.kind.as_str().to_owned(),
                    lane_index: request.lane.lane_index,
                    source_mint: request.source.liquidity_mint.clone(),
                    target_mint: request.target.liquidity_mint.clone(),
                    amount_in: request.redeem_liquidity_amount,
                    min_out: quote.other_amount_threshold,
                    max_slippage_bps: request.lane.max_slippage_bps,
                    max_fee_bps: request.lane.max_fee_bps,
                    instruction: Some(route_instruction),
                },
                deposit_liquidity_amount: quote.other_amount_threshold,
                expected_collateral_amount: quote.other_amount_threshold,
            })
        }
    }
}

impl JupiterRouteQuoteProvider {
    async fn fetch_quote(
        &self,
        request: &CrossMintQuoteRequest<'_>,
        slippage_bps: u16,
    ) -> Result<JupiterQuote, RouteQuoteError> {
        let mut url = Url::parse(&format!("{}/quote", self.base_url))
            .map_err(|error| RouteQuoteError::Unavailable(error.to_string()))?;
        url.query_pairs_mut()
            .append_pair("inputMint", &request.source.liquidity_mint)
            .append_pair("outputMint", &request.target.liquidity_mint)
            .append_pair("amount", &request.redeem_liquidity_amount.to_string())
            .append_pair("slippageBps", &slippage_bps.to_string())
            .append_pair("restrictIntermediateTokens", "true");

        let raw = self
            .client
            .get(url)
            .send()
            .await
            .map_err(|error| RouteQuoteError::Unavailable(error.to_string()))?
            .error_for_status()
            .map_err(|error| RouteQuoteError::Unavailable(error.to_string()))?
            .json::<Value>()
            .await
            .map_err(|error| RouteQuoteError::Unavailable(error.to_string()))?;

        let out_amount = parse_u64_field(&raw, "outAmount")?;
        let other_amount_threshold = parse_u64_field(&raw, "otherAmountThreshold")?;
        Ok(JupiterQuote {
            raw,
            out_amount,
            other_amount_threshold,
        })
    }

    async fn fetch_swap_instruction(
        &self,
        vault_pubkey: &str,
        quote: &Value,
    ) -> Result<JupiterSwapInstructionsResponse, RouteQuoteError> {
        let url = format!("{}/swap-instructions", self.base_url);
        let response = self
            .client
            .post(url)
            .json(&json!({
                "userPublicKey": vault_pubkey,
                "quoteResponse": quote,
                "wrapAndUnwrapSol": false,
                "useSharedAccounts": false,
            }))
            .send()
            .await
            .map_err(|error| RouteQuoteError::Unavailable(error.to_string()))?
            .error_for_status()
            .map_err(|error| RouteQuoteError::Unavailable(error.to_string()))?
            .json::<JupiterSwapInstructionsResponse>()
            .await
            .map_err(|error| RouteQuoteError::Unavailable(error.to_string()))?;

        if let Some(error) = response.error.as_ref() {
            return Err(RouteQuoteError::Unavailable(format!(
                "Jupiter swap-instructions error: {error}"
            )));
        }
        Ok(response)
    }
}

#[derive(Debug)]
struct JupiterQuote {
    raw: Value,
    #[allow(dead_code)]
    out_amount: u64,
    other_amount_threshold: u64,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct JupiterSwapInstructionsResponse {
    #[serde(default)]
    setup_instructions: Vec<JupiterInstruction>,
    swap_instruction: Option<JupiterInstruction>,
    cleanup_instruction: Option<JupiterInstruction>,
    error: Option<String>,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct JupiterInstruction {
    program_id: String,
    accounts: Vec<JupiterAccountMeta>,
    data: String,
}

impl JupiterInstruction {
    fn into_route_instruction(self) -> Result<RouteInstructionConfig, RouteQuoteError> {
        let data = BASE64_STANDARD
            .decode(self.data)
            .map_err(|error| RouteQuoteError::Unavailable(error.to_string()))?;
        Ok(RouteInstructionConfig {
            program_id: self.program_id,
            accounts: self
                .accounts
                .into_iter()
                .map(|account| RouteAccountMetaConfig {
                    pubkey: account.pubkey,
                    is_signer: account.is_signer,
                    is_writable: account.is_writable,
                })
                .collect(),
            data,
        })
    }
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct JupiterAccountMeta {
    pubkey: String,
    is_signer: bool,
    is_writable: bool,
}

fn parse_u64_field(value: &Value, field: &'static str) -> Result<u64, RouteQuoteError> {
    value
        .get(field)
        .and_then(Value::as_str)
        .ok_or_else(|| RouteQuoteError::Unavailable(format!("Jupiter quote missing {field}")))?
        .parse::<u64>()
        .map_err(|error| RouteQuoteError::Unavailable(error.to_string()))
}

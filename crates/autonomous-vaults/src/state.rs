use anyhow::{bail, Context, Result};
use serde::{Deserialize, Serialize};
use std::{collections::BTreeMap, env, fs, path::PathBuf};

pub const DEFAULT_STATE_PATH: &str = "docs/plans/autonomous-treasury-vault-mainnet-state.json";

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct VaultState {
    pub schema_version: u8,
    pub cluster: String,
    pub genesis_hash: String,
    pub deployment_signer: String,
    pub delegated_policy_signer: String,
    pub smart_account: Option<SmartAccountRecord>,
    #[serde(default)]
    pub kamino: Option<KaminoRecord>,
    #[serde(default)]
    pub meteora: Option<MeteoraRecord>,
    #[serde(default)]
    pub returns: Option<TreasuryReturnRecord>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct TreasuryReturnRecord {
    pub source_slot: u64,
    pub mother: String,
    pub vault_loyal_token_account: String,
    pub vault_usdc_token_account: String,
    pub mother_loyal_token_account: String,
    pub mother_usdc_token_account: String,
    pub loyal_policy: Option<PolicyRecord>,
    pub usdc_policy: Option<PolicyRecord>,
    #[serde(default)]
    pub live_steps: Vec<LiveStepRecord>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct MeteoraRecord {
    pub source_slot: u64,
    pub pool: String,
    pub program: String,
    pub active_bin_id_at_setup: i32,
    pub position: String,
    pub position_lower_bin_id: i32,
    pub position_upper_bin_id: i32,
    pub position_width: i32,
    pub vault_loyal_token_account: String,
    pub vault_usdc_token_account: String,
    pub bitmap_extension_or_program_sentinel: String,
    pub bin_arrays: Vec<String>,
    pub strategy_range_a_min_bin_id: i32,
    pub strategy_range_a_max_bin_id: i32,
    pub strategy_range_b_min_bin_id: i32,
    pub strategy_range_b_max_bin_id: i32,
    pub add_liquidity_policy: Option<PolicyRecord>,
    pub remove_liquidity_policy: Option<PolicyRecord>,
    pub claim_fee_policy: Option<PolicyRecord>,
    #[serde(default)]
    pub live_steps: Vec<LiveStepRecord>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct SmartAccountRecord {
    pub status: SmartAccountStatus,
    pub account_index: String,
    pub settings: String,
    pub vault_index: u8,
    pub vault: String,
    pub program_config: String,
    pub program_treasury: String,
    pub pending_signature: Option<String>,
    pub last_valid_block_height: Option<u64>,
    pub creation_signature: Option<String>,
    pub finalized_slot: Option<u64>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SmartAccountStatus {
    Planned,
    Finalized,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct KaminoRecord {
    pub source_slot: u64,
    pub vault_usdc_token_account: String,
    pub reserves: Vec<KaminoReserveRecord>,
    pub operations_policy: Option<PolicyRecord>,
    pub init_obligation_policy: Option<PolicyRecord>,
    #[serde(default)]
    pub live_steps: Vec<LiveStepRecord>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct KaminoReserveRecord {
    pub market: String,
    pub reserve: String,
    pub obligation: String,
    pub market_authority: String,
    pub liquidity_mint: String,
    pub liquidity_token_program: String,
    pub liquidity_supply: String,
    pub collateral_mint: String,
    pub collateral_supply: String,
    pub collateral_farm: Option<String>,
    pub obligation_farm_user_state: Option<String>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct PolicyRecord {
    pub status: PolicyStatus,
    pub seed: u64,
    pub policy: String,
    pub pending_signature: Option<String>,
    pub last_valid_block_height: Option<u64>,
    pub creation_signature: Option<String>,
    pub finalized_slot: Option<u64>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PolicyStatus {
    Planned,
    Finalized,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct LiveStepRecord {
    pub name: String,
    pub status: PolicyStatus,
    pub pending_signature: Option<String>,
    pub last_valid_block_height: Option<u64>,
    pub finalized_signature: Option<String>,
    pub finalized_slot: Option<u64>,
    #[serde(default)]
    pub before: BTreeMap<String, u64>,
    #[serde(default)]
    pub after: BTreeMap<String, u64>,
}

impl VaultState {
    pub fn new(
        genesis_hash: String,
        deployment_signer: String,
        delegated_policy_signer: String,
    ) -> Self {
        Self {
            schema_version: 1,
            cluster: "mainnet-beta".to_owned(),
            genesis_hash,
            deployment_signer,
            delegated_policy_signer,
            smart_account: None,
            kamino: None,
            meteora: None,
            returns: None,
        }
    }

    pub fn validate_identity(
        &self,
        genesis_hash: &str,
        deployment_signer: &str,
        delegated_policy_signer: &str,
    ) -> Result<()> {
        if self.cluster != "mainnet-beta" || self.genesis_hash != genesis_hash {
            bail!("state file belongs to a different cluster or genesis hash");
        }
        if self.deployment_signer != deployment_signer {
            bail!("state file deployment signer does not match DEPLOYMENT_PK");
        }
        if self.delegated_policy_signer != delegated_policy_signer {
            bail!("state file delegated signer does not match POLICY_KEYPAIR");
        }
        Ok(())
    }
}

pub fn state_path() -> PathBuf {
    env::var("AUTONOMOUS_VAULT_STATE_PATH")
        .map(PathBuf::from)
        .unwrap_or_else(|_| PathBuf::from(DEFAULT_STATE_PATH))
}

pub fn load(path: &PathBuf) -> Result<Option<VaultState>> {
    match fs::read(path) {
        Ok(bytes) => serde_json::from_slice(&bytes)
            .context("decode autonomous vault state")
            .map(Some),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(error) => Err(error).context("read autonomous vault state"),
    }
}

pub fn save(path: &PathBuf, state: &VaultState) -> Result<()> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent).context("create autonomous vault state directory")?;
    }
    let mut temporary = path.clone();
    temporary.set_extension("json.tmp");
    let mut encoded = serde_json::to_vec_pretty(state).context("encode autonomous vault state")?;
    encoded.push(b'\n');
    fs::write(&temporary, encoded).context("write temporary autonomous vault state")?;
    fs::rename(&temporary, path).context("atomically replace autonomous vault state")?;
    Ok(())
}

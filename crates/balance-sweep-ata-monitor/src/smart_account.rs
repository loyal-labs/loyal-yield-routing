//! Small, explicit routing layer for the Earn wake-up subscription.
//!
//! Earn notifications are hints for the existing application reconcilers. They
//! intentionally never enter the balance-sweep observation/projector path.

use std::{
    collections::{BTreeMap, BTreeSet, HashMap},
    str::FromStr,
};

use anyhow::{Context, Result};
use helius_laserstream::grpc::{
    subscribe_update::UpdateOneof, SubscribeRequest, SubscribeRequestFilterAccounts,
    SubscribeUpdate,
};
use loyal_actions::{
    derive_associated_token_account, derive_kamino_vanilla_obligation, derive_squads_vault,
    earn_stablecoins,
};
use loyal_kamino_data::targets::loyal_safe_markets;
use loyal_yield_store::EarnSubscriptionTarget;
use serde::{Deserialize, Serialize};
use solana_sdk::pubkey::Pubkey;

pub const BALANCE_SWEEP_WALLET_ATAS: &str = "balance_sweep_wallet_atas";
pub const EARN_POLICY_ACCOUNTS: &str = "earn_policy_accounts";
pub const EARN_VAULT_ACCOUNTS: &str = "earn_vault_accounts";
pub const EARN_IDLE_TOKEN_ACCOUNTS: &str = "earn_idle_token_accounts";
pub const EARN_OBLIGATIONS: &str = "earn_obligations";

const EARN_ACCOUNT_CHANNELS: [&str; 4] = [
    EARN_POLICY_ACCOUNTS,
    EARN_VAULT_ACCOUNTS,
    EARN_IDLE_TOKEN_ACCOUNTS,
    EARN_OBLIGATIONS,
];

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnWatchAccount {
    pub pubkey: String,
    pub role: String,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct EarnVaultWatch {
    pub environment: String,
    pub settings: String,
    pub wallet: String,
    pub vault: String,
    pub vault_index: u8,
    pub accounts: Vec<EarnWatchAccount>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct SubscriptionWatchSet {
    pub balance_sweep_accounts: Vec<String>,
    pub earn_vaults: Vec<EarnVaultWatch>,
}

impl SubscriptionWatchSet {
    pub fn from_targets(
        balance_sweep_accounts: Vec<String>,
        targets: Vec<EarnSubscriptionTarget>,
    ) -> Result<Self> {
        let safe_markets = loyal_safe_markets();
        let mut vaults = BTreeMap::<(String, String), EarnVaultWatch>::new();
        for target in targets {
            let settings = Pubkey::from_str(&target.settings)
                .with_context(|| format!("invalid Earn settings {}", target.settings))?;
            let vault_index = u8::try_from(target.vault_index)
                .with_context(|| format!("invalid Earn vault index {}", target.vault_index))?;
            let derived_vault = derive_squads_vault(&settings, vault_index).0;
            let vault = target
                .vault_pubkey
                .as_deref()
                .map(Pubkey::from_str)
                .transpose()
                .context("invalid recorded Earn vault")?
                .unwrap_or(derived_vault);
            if vault != derived_vault {
                anyhow::bail!(
                    "recorded Earn vault {vault} does not match derived vault {derived_vault}"
                );
            }
            let entry = vaults
                .entry((target.environment.clone(), vault.to_string()))
                .or_insert_with(|| EarnVaultWatch {
                    environment: target.environment.clone(),
                    settings: target.settings.clone(),
                    wallet: target.wallet.clone(),
                    vault: vault.to_string(),
                    vault_index,
                    accounts: Vec::new(),
                });
            if entry.settings != target.settings || entry.wallet != target.wallet {
                anyhow::bail!("conflicting Earn identity for vault {vault}");
            }
            entry.accounts.push(EarnWatchAccount {
                pubkey: vault.to_string(),
                role: "vault".to_owned(),
            });
            entry
                .accounts
                .extend(earn_stablecoins().iter().map(|asset| {
                    EarnWatchAccount {
                        pubkey: derive_associated_token_account(
                            vault,
                            asset.mint,
                            asset.token_program,
                        )
                        .to_string(),
                        role: "idle_token".to_owned(),
                    }
                }));
            let markets = safe_markets
                .iter()
                .copied()
                .chain(
                    target
                        .markets
                        .iter()
                        .filter_map(|value| Pubkey::from_str(value).ok()),
                )
                .collect::<BTreeSet<_>>();
            entry
                .accounts
                .extend(markets.into_iter().map(|market| EarnWatchAccount {
                    pubkey: derive_kamino_vanilla_obligation(vault, market).to_string(),
                    role: "obligation".to_owned(),
                }));
            for policy in target.policy_accounts {
                let policy = Pubkey::from_str(&policy)
                    .with_context(|| format!("invalid Earn policy account {policy}"))?;
                entry.accounts.push(EarnWatchAccount {
                    pubkey: policy.to_string(),
                    role: "policy".to_owned(),
                });
            }
        }
        let mut earn_vaults = vaults.into_values().collect::<Vec<_>>();
        for vault in &mut earn_vaults {
            vault.accounts.sort_by(|left, right| {
                (&left.role, &left.pubkey).cmp(&(&right.role, &right.pubkey))
            });
            vault.accounts.dedup();
        }
        Ok(Self {
            balance_sweep_accounts,
            earn_vaults,
        })
    }

    /// Keep Earn routing bindings monotonic for the lifetime of one monitor
    /// process. A just-removed policy or obligation can still have an update
    /// in flight; retaining its vault mapping prevents that update from
    /// becoming an unmapped cursor advance. A process restart rebuilds the
    /// compact set from durable application state and drops stale bindings.
    pub fn retain_previous_earn_bindings(&mut self, previous: &Self) -> Result<()> {
        let mut current = self
            .earn_vaults
            .drain(..)
            .map(|vault| ((vault.environment.clone(), vault.vault.clone()), vault))
            .collect::<BTreeMap<_, _>>();
        for old in &previous.earn_vaults {
            let key = (old.environment.clone(), old.vault.clone());
            match current.get_mut(&key) {
                Some(next) => {
                    if next.settings != old.settings
                        || next.wallet != old.wallet
                        || next.vault_index != old.vault_index
                    {
                        anyhow::bail!("conflicting retained Earn identity for vault {}", old.vault);
                    }
                    next.accounts.extend(old.accounts.iter().cloned());
                    next.accounts.sort_by(|left, right| {
                        (&left.role, &left.pubkey).cmp(&(&right.role, &right.pubkey))
                    });
                    next.accounts.dedup();
                }
                None => {
                    current.insert(key, old.clone());
                }
            }
        }
        self.earn_vaults = current.into_values().collect();
        Ok(())
    }

    pub fn new_earn_vaults<'a>(&'a self, previous: Option<&Self>) -> Vec<&'a EarnVaultWatch> {
        let previous = previous
            .into_iter()
            .flat_map(|set| set.earn_vaults.iter())
            .map(|vault| (vault.environment.as_str(), vault.vault.as_str()))
            .collect::<BTreeSet<_>>();
        self.earn_vaults
            .iter()
            .filter(|vault| !previous.contains(&(vault.environment.as_str(), vault.vault.as_str())))
            .collect()
    }

    pub fn account_channels(&self) -> BTreeMap<&'static str, Vec<String>> {
        let mut channels = BTreeMap::<&'static str, BTreeSet<String>>::new();
        for account in &self.balance_sweep_accounts {
            channels
                .entry(BALANCE_SWEEP_WALLET_ATAS)
                .or_default()
                .insert(account.clone());
        }
        for vault in &self.earn_vaults {
            for account in &vault.accounts {
                let channel = match account.role.as_str() {
                    "policy" => EARN_POLICY_ACCOUNTS,
                    "vault" => EARN_VAULT_ACCOUNTS,
                    "idle_token" => EARN_IDLE_TOKEN_ACCOUNTS,
                    "obligation" => EARN_OBLIGATIONS,
                    _ => continue,
                };
                channels
                    .entry(channel)
                    .or_default()
                    .insert(account.pubkey.clone());
            }
        }
        channels
            .into_iter()
            .filter(|(_, accounts)| !accounts.is_empty())
            .map(|(channel, accounts)| (channel, accounts.into_iter().collect()))
            .collect()
    }

    pub fn affected_vaults<'a>(
        &'a self,
        accounts: impl IntoIterator<Item = &'a str>,
    ) -> Vec<&'a EarnVaultWatch> {
        let accounts = accounts.into_iter().collect::<BTreeSet<_>>();
        self.earn_vaults
            .iter()
            .filter(|vault| {
                vault
                    .accounts
                    .iter()
                    .any(|item| accounts.contains(item.pubkey.as_str()))
            })
            .collect()
    }
}

/// The normalized form is deliberately a hint, not a raw event envelope.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct NormalizedEarnUpdate {
    pub event_key: Option<String>,
    pub filters: Vec<String>,
    pub event_kind: &'static str,
    pub account_pubkey: Option<String>,
    pub slot: u64,
    pub signature: Option<String>,
}

pub fn normalize_laserstream_update(
    update: SubscribeUpdate,
) -> Result<Option<NormalizedEarnUpdate>> {
    let filters = update.filters;
    let earn_filters = filters
        .iter()
        .filter(|filter| EARN_ACCOUNT_CHANNELS.contains(&filter.as_str()))
        .cloned()
        .collect::<Vec<_>>();
    if earn_filters.is_empty() {
        return Ok(None);
    }
    match update.update_oneof {
        Some(UpdateOneof::Account(account_update)) => {
            let account = account_update.account.as_ref();
            let account_pubkey = account
                .map(|account| pubkey_from_bytes(&account.pubkey, "account pubkey"))
                .transpose()?;
            let signature = account_update
                .account
                .as_ref()
                .and_then(|account| account.txn_signature.as_deref())
                .map(|bytes| signature_from_bytes(bytes))
                .transpose()?;
            Ok(Some(NormalizedEarnUpdate {
                event_key: None,
                filters: earn_filters,
                event_kind: if account.is_none() || account.is_some_and(|value| value.lamports == 0)
                {
                    "account_deleted"
                } else {
                    "account"
                },
                account_pubkey,
                slot: account_update.slot,
                signature,
            }))
        }
        _ => Ok(None),
    }
}

pub fn build_multi_channel_subscribe_request(
    watch_set: &SubscriptionWatchSet,
    from_slot: u64,
) -> SubscribeRequest {
    let accounts = watch_set
        .account_channels()
        .into_iter()
        .map(|(channel, addresses)| {
            (
                channel.to_owned(),
                SubscribeRequestFilterAccounts {
                    account: addresses,
                    owner: Vec::new(),
                    filters: Vec::new(),
                    nonempty_txn_signature: Some(true),
                },
            )
        })
        .collect();
    SubscribeRequest {
        accounts,
        transactions: HashMap::new(),
        commitment: Some(helius_laserstream::grpc::CommitmentLevel::Confirmed as i32),
        from_slot: Some(from_slot),
        ..Default::default()
    }
}

#[derive(Debug, Serialize)]
struct RequestJson<'a> {
    request_count: u8,
    commitment: &'static str,
    accounts: BTreeMap<String, Vec<String>>,
    transactions: BTreeMap<String, serde_json::Value>,
    #[serde(skip_serializing_if = "Option::is_none")]
    _marker: Option<&'a str>,
}

pub fn subscribe_request_json(watch_set: &SubscriptionWatchSet) -> serde_json::Value {
    let accounts = watch_set
        .account_channels()
        .into_iter()
        .map(|(channel, addresses)| (channel.to_owned(), addresses))
        .collect();
    serde_json::to_value(RequestJson {
        request_count: 1,
        commitment: "confirmed",
        accounts,
        transactions: BTreeMap::new(),
        _marker: None,
    })
    .expect("request JSON is serializable")
}

fn pubkey_from_bytes(bytes: &[u8], label: &str) -> Result<String> {
    if bytes.len() != 32 {
        anyhow::bail!(
            "LaserStream {label} decoded to {} bytes, expected 32",
            bytes.len()
        );
    }
    let mut array = [0_u8; 32];
    array.copy_from_slice(bytes);
    Ok(Pubkey::new_from_array(array).to_string())
}

fn signature_from_bytes(bytes: &[u8]) -> Result<String> {
    if bytes.is_empty() {
        anyhow::bail!("LaserStream transaction signature was empty");
    }
    Ok(solana_sdk::bs58::encode(bytes).into_string())
}

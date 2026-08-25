//! Small, explicit routing layer for the Earn wake-up subscription.
//!
//! Earn notifications are durable wake-ups for the in-process reconciler. They
//! intentionally never enter the balance-sweep observation/projector path.

use std::{
    collections::{BTreeMap, BTreeSet},
    str::FromStr,
};

use anyhow::{Context, Result};
use helius_laserstream::grpc::{
    subscribe_update::UpdateOneof, SubscribeRequest, SubscribeRequestFilterAccounts,
    SubscribeUpdate,
};
use loyal_actions::{
    derive_associated_token_account, derive_kamino_vanilla_obligation, derive_squads_vault,
    earn_stablecoins, USDC_MINT,
};
use loyal_kamino_data::targets::loyal_safe_markets;
use loyal_yield_store::EarnSubscriptionTarget;
use serde::{Deserialize, Serialize};
use solana_sdk::pubkey::Pubkey;

pub const BALANCE_SWEEP_WALLET_ATAS: &str = "balance_sweep_wallet_atas";
pub const EARN_SMART_ACCOUNTS: &str = "earn_smart_accounts";
pub const EARN_POLICY_ACCOUNTS: &str = "earn_policy_accounts";
pub const EARN_VAULT_ACCOUNTS: &str = "earn_vault_accounts";
pub const EARN_IDLE_TOKEN_ACCOUNTS: &str = "earn_idle_token_accounts";
pub const EARN_WALLET_TOKEN_ACCOUNTS: &str = "earn_wallet_token_accounts";
pub const EARN_OBLIGATIONS: &str = "earn_obligations";
pub const EARN_AUTODEPOSIT_WALLET_ATAS: &str = "earn_autodeposit_wallet_atas";
pub const EARN_SUBSCRIPTION_AUTHORITIES: &str = "earn_subscription_authorities";
pub const EARN_RECURRING_DELEGATIONS: &str = "earn_recurring_delegations";
pub const EARN_WALLETS: &str = "earn_wallets";

const EARN_ACCOUNT_CHANNELS: [&str; 10] = [
    EARN_SMART_ACCOUNTS,
    EARN_POLICY_ACCOUNTS,
    EARN_VAULT_ACCOUNTS,
    EARN_IDLE_TOKEN_ACCOUNTS,
    EARN_WALLET_TOKEN_ACCOUNTS,
    EARN_OBLIGATIONS,
    EARN_AUTODEPOSIT_WALLET_ATAS,
    EARN_SUBSCRIPTION_AUTHORITIES,
    EARN_RECURRING_DELEGATIONS,
    EARN_WALLETS,
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
    #[serde(default)]
    pub earn_max: bool,
    pub vault: String,
    pub vault_index: u8,
    pub accounts: Vec<EarnWatchAccount>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct SubscriptionWatchSet {
    pub balance_sweep_accounts: Vec<String>,
    pub earn_vaults: Vec<EarnVaultWatch>,
    pub observation_start_slot: Option<u64>,
}

impl SubscriptionWatchSet {
    pub fn from_targets(
        balance_sweep_accounts: Vec<String>,
        targets: Vec<EarnSubscriptionTarget>,
    ) -> Result<Self> {
        let safe_markets = loyal_safe_markets();
        let mut vaults = BTreeMap::<(String, String), EarnVaultWatch>::new();
        let mut observation_start_slot: Option<u64> = None;
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
                    earn_max: target.earn_max,
                    vault: vault.to_string(),
                    vault_index,
                    accounts: Vec::new(),
                });
            if entry.settings != target.settings
                || (!entry.wallet.is_empty()
                    && !target.wallet.is_empty()
                    && entry.wallet != target.wallet)
            {
                anyhow::bail!("conflicting Earn identity for vault {vault}");
            }
            if entry.wallet.is_empty() && !target.wallet.is_empty() {
                entry.wallet.clone_from(&target.wallet);
            }
            entry.earn_max |= target.earn_max;
            entry.accounts.push(EarnWatchAccount {
                pubkey: settings.to_string(),
                role: "smart_account".to_owned(),
            });
            let wallet = (!target.wallet.is_empty())
                .then(|| Pubkey::from_str(&target.wallet))
                .transpose()
                .with_context(|| format!("invalid Earn wallet {}", target.wallet))?;
            if let Some(wallet) = wallet {
                entry.accounts.push(EarnWatchAccount {
                    pubkey: wallet.to_string(),
                    role: "wallet".to_owned(),
                });
                entry.accounts.push(EarnWatchAccount {
                    pubkey: derive_associated_token_account(wallet, USDC_MINT, spl_token::ID)
                        .to_string(),
                    role: "autodeposit_wallet_ata".to_owned(),
                });
                entry
                    .accounts
                    .extend(earn_stablecoins().iter().map(|asset| {
                        EarnWatchAccount {
                            pubkey: derive_associated_token_account(
                                wallet,
                                asset.mint,
                                asset.token_program,
                            )
                            .to_string(),
                            role: "wallet_token".to_owned(),
                        }
                    }));
            }
            observation_start_slot = match (observation_start_slot, target.observation_start_slot) {
                (Some(current), Some(next)) => Some(current.min(next)),
                (None, next) => next,
                (current, None) => current,
            };
            for (index, account) in target.autodeposit_accounts.into_iter().enumerate() {
                Pubkey::from_str(&account)
                    .with_context(|| format!("invalid Autodeposit account {account}"))?;
                entry.accounts.push(EarnWatchAccount {
                    pubkey: account,
                    role: if index == 0 {
                        "subscription_authority".to_owned()
                    } else {
                        "recurring_delegation".to_owned()
                    },
                });
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
            observation_start_slot,
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
                        || (!next.wallet.is_empty()
                            && !old.wallet.is_empty()
                            && next.wallet != old.wallet)
                        || next.vault_index != old.vault_index
                    {
                        anyhow::bail!("conflicting retained Earn identity for vault {}", old.vault);
                    }
                    if next.wallet.is_empty() && !old.wallet.is_empty() {
                        next.wallet.clone_from(&old.wallet);
                    }
                    next.earn_max |= old.earn_max;
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
        self.observation_start_slot =
            match (self.observation_start_slot, previous.observation_start_slot) {
                (Some(current), Some(old)) => Some(current.min(old)),
                (None, old) => old,
                (current, None) => current,
            };
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
                    "wallet" => EARN_WALLETS,
                    "smart_account" => EARN_SMART_ACCOUNTS,
                    "policy" => EARN_POLICY_ACCOUNTS,
                    "vault" => EARN_VAULT_ACCOUNTS,
                    "idle_token" => EARN_IDLE_TOKEN_ACCOUNTS,
                    "wallet_token" => EARN_WALLET_TOKEN_ACCOUNTS,
                    "obligation" => EARN_OBLIGATIONS,
                    "autodeposit_wallet_ata" => EARN_AUTODEPOSIT_WALLET_ATAS,
                    "subscription_authority" => EARN_SUBSCRIPTION_AUTHORITIES,
                    "recurring_delegation" => EARN_RECURRING_DELEGATIONS,
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
#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
pub struct NormalizedEarnUpdate {
    pub event_key: Option<String>,
    pub filters: Vec<String>,
    pub event_kind: String,
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
                    "account_deleted".to_owned()
                } else {
                    "account".to_owned()
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
        transactions: BTreeMap::new().into_iter().collect(),
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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn autoswap_uses_targeted_accounts_without_a_transaction_stream() {
        let settings = Pubkey::new_unique();
        let vault = derive_squads_vault(&settings, 1).0;
        let policy = Pubkey::new_unique();
        let request = build_multi_channel_subscribe_request(
            &SubscriptionWatchSet::from_targets(
                Vec::new(),
                vec![EarnSubscriptionTarget {
                    environment: "mainnet-beta".to_owned(),
                    settings: settings.to_string(),
                    wallet: Pubkey::new_unique().to_string(),
                    earn_max: false,
                    vault_index: 1,
                    vault_pubkey: Some(vault.to_string()),
                    policy_accounts: vec![policy.to_string()],
                    markets: Vec::new(),
                    autodeposit_accounts: Vec::new(),
                    observation_start_slot: None,
                }],
            )
            .expect("valid targeted watch set"),
            42,
        );
        assert_eq!(request.from_slot, Some(42));
        assert_eq!(
            request.commitment,
            Some(helius_laserstream::grpc::CommitmentLevel::Confirmed as i32)
        );
        assert!(request.transactions.is_empty());
        assert_eq!(
            request.accounts[EARN_SMART_ACCOUNTS].account,
            vec![settings.to_string()]
        );
        assert_eq!(
            request.accounts[EARN_POLICY_ACCOUNTS].account,
            vec![policy.to_string()]
        );
    }
}

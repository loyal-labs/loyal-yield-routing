//! Local-only execution transport for the connected fleet verifier.
//! JSON lines on stdin/stdout; no network listener and no production credentials.
//! Unknown operations fail closed. Account injection is permitted exactly once,
//! before any simulation or submission. Transaction effects come from LiteSVM.
use base64::{Engine, engine::general_purpose::STANDARD};
use litesvm::LiteSVM;
use serde_json::{Value, json};
use solana_sdk::{
    account::Account, message::VersionedMessage, pubkey::Pubkey, transaction::VersionedTransaction,
};
use spl_token::solana_program::program_pack::Pack;
use squads_test_harness::{
    add_mock_jupiter_program, add_mock_kamino_lend_program,
    add_squads_program_from_env_or_sibling_checkout,
};
use std::{
    collections::BTreeMap,
    error::Error,
    io::{self, BufRead, Write},
    str::FromStr,
};

type Result<T> = std::result::Result<T, Box<dyn Error>>;

struct LocalChain {
    svm: Option<LiteSVM>,
    initialized: bool,
    receipts: BTreeMap<String, Value>,
    submissions: u64,
    transactions: BTreeMap<String, Value>,
    history: Vec<String>,
    slot: u64,
}

impl LocalChain {
    fn new() -> Result<Self> {
        let mut svm = LiteSVM::new();
        add_squads_program_from_env_or_sibling_checkout(&mut svm)?
            .ok_or("required Squads SBF missing")?;
        add_mock_kamino_lend_program(&mut svm)?;
        add_mock_jupiter_program(&mut svm)?;
        svm.warp_to_slot(1000);
        Ok(Self {
            svm: Some(svm),
            initialized: false,
            receipts: BTreeMap::new(),
            submissions: 0,
            transactions: BTreeMap::new(),
            history: Vec::new(),
            slot: 1000,
        })
    }

    fn handle(&mut self, request: &Value) -> Result<Value> {
        let method = request["method"].as_str().ok_or("missing method")?;
        let params = &request["params"];
        if method == "initialize" {
            if self.initialized {
                return Err("chain fixture is immutable after initialization".into());
            }
            let svm = self.svm.as_mut().unwrap();
            for (key, value) in params["accounts"]
                .as_object()
                .ok_or("missing initial accounts")?
            {
                let address = Pubkey::from_str(key)?;
                if value["Address"].as_str() != Some(key) {
                    return Err("account key mismatch".into());
                }
                if value["Executable"].as_bool() != Some(false) {
                    return Err("fixture cannot inject executable programs".into());
                }
                svm.set_account(
                    address,
                    Account {
                        lamports: value["Lamports"].as_u64().ok_or("missing lamports")?,
                        owner: Pubkey::from_str(value["Owner"].as_str().ok_or("missing owner")?)?,
                        data: STANDARD.decode(value["Data"].as_str().unwrap_or(""))?,
                        executable: false,
                        rent_epoch: 0,
                    },
                )?;
            }
            self.initialized = true;
            return Ok(json!({"slot":1000,"blockhash":svm.latest_blockhash().to_string()}));
        }
        if !self.initialized {
            return Err("initialize chain before RPC".into());
        }
        match method {
            "getGenesisHash" => Ok(json!(
                solana_sdk::hash::Hash::new_from_array([42; 32]).to_string()
            )),
            "advanceSlot" => {
                let slot = params[0].as_u64().ok_or("missing slot")?;
                if slot <= self.slot {
                    return Err("local slot must advance".into());
                }
                self.svm.as_mut().unwrap().warp_to_slot(slot);
                self.slot = slot;
                Ok(json!(slot))
            }
            "getSlot" | "getBlockHeight" => Ok(json!(self.slot)),
            // No contention in the deterministic local chain.
            "getRecentPrioritizationFees" => Ok(json!([{"slot":1000,"prioritizationFee":0}])),
            "getLatestBlockhash" => Ok(json!({"context":{"slot":self.slot},"value":{
                "blockhash":self.svm.as_ref().unwrap().latest_blockhash().to_string(),"lastValidBlockHeight":1150
            }})),
            "getAccountInfo" => {
                let key = Pubkey::from_str(params[0].as_str().ok_or("missing address")?)?;
                let value = self.svm.as_ref().unwrap().get_account(&key).map(|a| json!({
                    "owner":a.owner.to_string(),"lamports":a.lamports,"executable":a.executable,
                    "rentEpoch":a.rent_epoch,"data":[STANDARD.encode(a.data),"base64"]
                })).unwrap_or(Value::Null);
                Ok(json!({"context":{"slot":self.slot},"value":value}))
            }
            "getMultipleAccounts" => {
                let values: Result<Vec<Value>> = params[0]
                    .as_array()
                    .ok_or("missing addresses")?
                    .iter()
                    .map(|key| {
                        let address = Pubkey::from_str(key.as_str().ok_or("invalid address")?)?;
                        Ok(self.svm.as_ref().unwrap().get_account(&address).map(|a| json!({
                        "owner":a.owner.to_string(),"lamports":a.lamports,"executable":a.executable,
                        "rentEpoch":a.rent_epoch,"data":[STANDARD.encode(a.data),"base64"]
                    })).unwrap_or(Value::Null))
                    })
                    .collect();
                Ok(json!({"context":{"slot":self.slot},"value":values?}))
            }
            "simulateTransaction" => {
                let wire = STANDARD.decode(params[0].as_str().ok_or("missing wire")?)?;
                let tx: VersionedTransaction = bincode::deserialize(&wire)?;
                // Solana RPC accepts unsigned simulation only with sigVerify=false.
                // Production Go explicitly requests replacement of the blockhash.
                let verify = params[1]["sigVerify"].as_bool().unwrap_or(false);
                let replace = params[1]["replaceRecentBlockhash"]
                    .as_bool()
                    .unwrap_or(false);
                if verify && replace {
                    return Err(
                        "cannot combine signature verification and blockhash replacement".into(),
                    );
                }
                let svm = self
                    .svm
                    .take()
                    .unwrap()
                    .with_sigverify(verify)
                    .with_blockhash_check(!replace);
                let result = match svm.simulate_transaction(tx) {
                    Ok(simulation) => {
                        json!({"err":null,"logs":simulation.meta.logs,"unitsConsumed":simulation.meta.compute_units_consumed})
                    }
                    Err(failure) => {
                        json!({"err":failure.err,"logs":failure.meta.logs,"unitsConsumed":failure.meta.compute_units_consumed})
                    }
                };
                self.svm = Some(svm.with_sigverify(true).with_blockhash_check(true));
                Ok(json!({"context":{"slot":self.slot},"value":result}))
            }
            "sendTransaction" => {
                let wire = STANDARD.decode(params[0].as_str().ok_or("missing wire")?)?;
                let tx: VersionedTransaction = bincode::deserialize(&wire)?;
                tx.verify_and_hash_message()?;
                let signature = tx
                    .signatures
                    .first()
                    .ok_or("unsigned submission")?
                    .to_string();
                self.submissions += 1;
                if self.receipts.contains_key(&signature) {
                    return Ok(json!(signature)); // Solana's already-processed retry: no execution.
                }
                let svm = self.svm.as_mut().unwrap();
                let (keys, loaded) = transaction_keys(svm, &tx)?;
                let pre_balances = lamport_balances(svm, &keys);
                let pre_tokens = token_balances(svm, &keys)?;
                let tx_signature = tx.signatures[0];
                let result = svm.send_transaction(tx);
                // Rejected packets are not fabricated into finalized receipts.
                if svm.get_transaction(&tx_signature).is_none() {
                    return Err(format!("transaction rejected before recording: {result:?}").into());
                }
                let (error, logs, units) = match result {
                    Ok(meta) => (Value::Null, meta.logs, meta.compute_units_consumed),
                    Err(failure) => (
                        serde_json::to_value(failure.err)?,
                        failure.meta.logs,
                        failure.meta.compute_units_consumed,
                    ),
                };
                let post_balances = lamport_balances(svm, &keys);
                let post_tokens = token_balances(svm, &keys)?;
                let fee = pre_balances[0]
                    .checked_sub(post_balances[0])
                    .ok_or("unexpected fee-payer credit")?;
                let status = if error.is_null() {
                    json!({"Ok":null})
                } else {
                    json!({"Err":error})
                };
                let receipt = json!({"slot":self.slot,"confirmations":null,"err":error,"status":status,"confirmationStatus":"finalized","logs":logs});
                self.transactions.insert(signature.clone(), json!({
                    "slot":self.slot,"blockTime":null,"version":0,"transaction":[STANDARD.encode(&wire),"base64"],
                    "meta":{"err":error,"status":status,"fee":fee,"preBalances":pre_balances,"postBalances":post_balances,
                        "preTokenBalances":pre_tokens,"postTokenBalances":post_tokens,"loadedAddresses":loaded,
                        "logMessages":logs,"computeUnitsConsumed":units}
                }));
                self.history.push(signature.clone());
                self.receipts.insert(signature.clone(), receipt);
                Ok(json!(signature))
            }
            "getSignaturesForAddress" => {
                let address = Pubkey::from_str(params[0].as_str().ok_or("missing address")?)?;
                let before = params[1]["before"].as_str();
                let until = params[1]["until"].as_str();
                let limit = params[1]["limit"].as_u64().unwrap_or(1000) as usize;
                let mut started = before.is_none();
                let mut result = Vec::new();
                for signature in self.history.iter().rev() {
                    if !started {
                        if Some(signature.as_str()) == before {
                            started = true;
                        }
                        continue;
                    }
                    if Some(signature.as_str()) == until {
                        break;
                    }
                    let receipt = &self.transactions[signature];
                    let wire = STANDARD.decode(
                        receipt["transaction"][0]
                            .as_str()
                            .ok_or("missing recorded wire")?,
                    )?;
                    let transaction: VersionedTransaction = bincode::deserialize(&wire)?;
                    let (keys, _) = transaction_keys(self.svm.as_ref().unwrap(), &transaction)?;
                    if keys.contains(&address) {
                        result.push(json!({"signature":signature,"slot":receipt["slot"],"err":receipt["meta"]["err"],"memo":null,"blockTime":null,"confirmationStatus":"finalized"}));
                        if result.len() == limit {
                            break;
                        }
                    }
                }
                Ok(json!(result))
            }
            "getSignatureStatuses" => {
                let values: Vec<Value> = params[0]
                    .as_array()
                    .ok_or("missing signatures")?
                    .iter()
                    .map(|sig| {
                        self.receipts
                            .get(sig.as_str().unwrap_or(""))
                            .cloned()
                            .unwrap_or(Value::Null)
                    })
                    .collect();
                Ok(json!({"context":{"slot":self.slot},"value":values}))
            }
            "getTransaction" => {
                if params[1]["encoding"].as_str() != Some("base64") {
                    return Err(
                        "local receipt transport requires exact base64 wire encoding".into(),
                    );
                }
                Ok(self
                    .transactions
                    .get(params[0].as_str().ok_or("missing signature")?)
                    .cloned()
                    .unwrap_or(Value::Null))
            }
            "getFeeForMessage" => {
                let message: VersionedMessage = bincode::deserialize(
                    &STANDARD.decode(params[0].as_str().ok_or("missing message")?)?,
                )?;
                Ok(
                    json!({"context":{"slot":self.slot},"value":u64::from(message.header().num_required_signatures)*5000}),
                )
            }
            "getTokenSupply" => {
                let key = Pubkey::from_str(params[0].as_str().ok_or("missing mint")?)?;
                let account = self
                    .svm
                    .as_ref()
                    .unwrap()
                    .get_account(&key)
                    .ok_or("mint missing")?;
                if account.owner != spl_token::id() {
                    return Err("local supply requires classic SPL mint".into());
                }
                let mint = spl_token::state::Mint::unpack(&account.data)?;
                Ok(
                    json!({"context":{"slot":self.slot},"value":{"amount":mint.supply.to_string(),"decimals":mint.decimals,"uiAmount":null,"uiAmountString":format!("{}", mint.supply as f64 / 10_f64.powi(i32::from(mint.decimals)))}}),
                )
            }
            "getBalance" => {
                let key = Pubkey::from_str(params[0].as_str().ok_or("missing account")?)?;
                Ok(
                    json!({"context":{"slot":self.slot},"value":self.svm.as_ref().unwrap().get_account(&key).map(|a| a.lamports).unwrap_or(0)}),
                )
            }
            "executionEvidence" => {
                let mut accounts = BTreeMap::new();
                for receipt in self.transactions.values() {
                    let wire = STANDARD.decode(
                        receipt["transaction"][0]
                            .as_str()
                            .ok_or("missing recorded wire")?,
                    )?;
                    let transaction: VersionedTransaction = bincode::deserialize(&wire)?;
                    let (keys, _) = transaction_keys(self.svm.as_ref().unwrap(), &transaction)?;
                    for key in keys {
                        accounts.insert(key.to_string(), self.svm.as_ref().unwrap().get_account(&key).map(|a|json!({"lamports":a.lamports,"owner":a.owner.to_string(),"data":STANDARD.encode(a.data)})));
                    }
                }
                Ok(
                    json!({"submissionAttempts":self.submissions,"receipts":self.receipts,"transactions":self.transactions,"accounts":accounts}),
                )
            }
            _ => Err(format!("unsupported local RPC method: {method}").into()),
        }
    }
}

fn transaction_keys(svm: &LiteSVM, tx: &VersionedTransaction) -> Result<(Vec<Pubkey>, Value)> {
    let mut keys = tx.message.static_account_keys().to_vec();
    let mut writable = Vec::new();
    let mut readonly = Vec::new();
    for lookup in tx.message.address_table_lookups().unwrap_or(&[]) {
        let account = svm
            .get_account(&lookup.account_key)
            .ok_or("missing transaction ALT")?;
        for (indexes, output) in [
            (&lookup.writable_indexes, &mut writable),
            (&lookup.readonly_indexes, &mut readonly),
        ] {
            for index in indexes {
                let start = 56 + usize::from(*index) * 32;
                let key: [u8; 32] = account
                    .data
                    .get(start..start + 32)
                    .ok_or("invalid ALT index")?
                    .try_into()?;
                output.push(Pubkey::new_from_array(key));
            }
        }
    }
    keys.extend_from_slice(&writable);
    keys.extend_from_slice(&readonly);
    Ok((
        keys,
        json!({"writable":writable.iter().map(ToString::to_string).collect::<Vec<_>>(),"readonly":readonly.iter().map(ToString::to_string).collect::<Vec<_>>()}),
    ))
}

fn lamport_balances(svm: &LiteSVM, keys: &[Pubkey]) -> Vec<u64> {
    keys.iter()
        .map(|key| svm.get_account(key).map(|a| a.lamports).unwrap_or(0))
        .collect()
}

fn token_balances(svm: &LiteSVM, keys: &[Pubkey]) -> Result<Vec<Value>> {
    let mut balances = Vec::new();
    for (index, key) in keys.iter().enumerate() {
        let Some(account) = svm.get_account(key) else {
            continue;
        };
        if account.owner != spl_token::id() || account.data.len() != spl_token::state::Account::LEN
        {
            continue;
        }
        let token = spl_token::state::Account::unpack(&account.data)?;
        let mint_account = svm.get_account(&token.mint).ok_or("receipt mint missing")?;
        let mint = spl_token::state::Mint::unpack(&mint_account.data)?;
        let scale = 10u64
            .checked_pow(u32::from(mint.decimals))
            .ok_or("receipt decimals overflow")?;
        let amount_string = if mint.decimals == 0 {
            token.amount.to_string()
        } else {
            format!(
                "{}.{:0width$}",
                token.amount / scale,
                token.amount % scale,
                width = usize::from(mint.decimals)
            )
        };
        balances.push(json!({"accountIndex":index,"mint":token.mint.to_string(),"owner":token.owner.to_string(),"programId":account.owner.to_string(),
            "uiTokenAmount":{"amount":token.amount.to_string(),"decimals":mint.decimals,"uiAmount":null,"uiAmountString":amount_string}}));
    }
    Ok(balances)
}

fn main() -> Result<()> {
    let mut chain = LocalChain::new()?;
    let mut stdout = io::stdout().lock();
    for line in io::stdin().lock().lines() {
        let request: Value = serde_json::from_str(&line?)?;
        let response = match chain.handle(&request) {
            Ok(result) => json!({"jsonrpc":"2.0","id":request["id"],"result":result}),
            Err(error) => {
                json!({"jsonrpc":"2.0","id":request["id"],"error":{"code":-32000,"message":error.to_string()}})
            }
        };
        serde_json::to_writer(&mut stdout, &response)?;
        writeln!(stdout)?;
        stdout.flush()?;
    }
    Ok(())
}

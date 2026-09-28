//! Adaptor v3 matrix on LiteSVM against the REAL deployed binaries:
//! Voltr `vVoLTR…` (ProgramData slot 445,223,838) and Squads `SMRTz…`, with the
//! custom adaptor `FSj27…` loaded from the mainnet dump (v2) for the repair and
//! then swapped IN PLACE for the v3 ELF (fixtures/voltr-repair/adaptor-v3.so).
//!
//! Mainnet order mirrored:
//!   0. repair strategy one on the CURRENT (v2) adaptor -> tv == idle == 3,793,417
//!   1. swap the adaptor program for the v3.1 ELF (104-byte v2-format report
//!      ticket with last_consumed_slot, writable receipt accepted at capital
//!      index 11, interval gated on Clock.slot at arm AND consume, flow-adjusted
//!      NAV step bound)
//!   2. bootstrap strategy two on v3 in three transactions
//!      (initialize_config 41B / initialize_report_ticket / custody+holding ATA
//!       create -> initializeStrategy; the manager round-trip was dropped — the
//!       updateVaultConfig Manager field could not be identified and
//!       initializeStrategy succeeds without it under direct signers)
//!   3. seed the vault with a 1,000,000 admin deposit (as R5a does)
//!   V1..V13 case matrix, each case on a clone of the post-V1 state unless the
//!   case is part of the main line (V1 -> V2 -> V3 -> V10).
//!
//! Book identity asserted after every successful step (receipt1 is the orphaned
//! strategy-one phantom 3,793,536 and is excluded):
//!     tv == idle + custody2 + receipt2
//! Conserved quantity (Voltr keeps it under EVERY instruction, orphan included):
//!     D = tv - idle - (receipt1 + receipt2) == -3,793,536
//!
//! Nothing is signed or broadcast: LiteSVM sigverify is off and the privileged
//! signers (admin BAqg…, manager ST999…, config keypair) are marked signers
//! directly. Overrides are listed in the emitted JSON.

#![allow(dead_code, clippy::identity_op, clippy::too_many_arguments)]
use base64::{engine::general_purpose::STANDARD, Engine};
use litesvm::LiteSVM;
use serde_json::{json, Value};
use solana_sdk::{
    account::Account,
    clock::Clock,
    instruction::{AccountMeta, Instruction},
    message::Message,
    pubkey::Pubkey,
    signature::Signature,
    transaction::Transaction,
};
use spl_token::{
    solana_program::{program_option::COption, program_pack::Pack},
    state::{Account as SplAccount, AccountState},
};
use std::{fs, path::PathBuf, str::FromStr};

// ---- deployed program ids -------------------------------------------------
const VOLTR: &str = "vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8";
const ADAPTOR: &str = "FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW";
const SQUADS: &str = "SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG";
const TOKEN: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const ATA_PROG: &str = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL";
const SYS: &str = "11111111111111111111111111111111";
const RENT_SYSVAR: &str = "SysvarRent111111111111111111111111111111111";

// ---- state accounts (mainnet dump, slot 445235325) -------------------------
const VAULT: &str = "HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA";
const RECEIPT: &str = "3GHLmyTTGH9ZfQqb3YCo9xKjpPhMLvHsq2JSYzCnk9U6";
const STRATEGY: &str = "9hDH4acTDrSjg9d5n8c1g53jMTonaDAUesp1diCWuuhj"; // v2 config (orphan)
const TICKET: &str = "C71BFjq6PfgcWV4geoRudheupKnQBv6yN6uzYKthgAt5";
const IDLE_ATA: &str = "6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh";
const CUSTODY_ATA: &str = "FTDWN5Ay8tzYPJBJT4s2oZaHRQ7jKPo8XP2ZRWb5GP3M";
const SQUADS_USDC_ATA: &str = "EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe";
const SETTINGS: &str = "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6";
const SQUADS_VAULT: &str = "ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"; // manager
const USDC: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const LP_MINT: &str = "6tNheTBYSpQkfMLhcczKgmTLSGffK54npKMG1WQR2tvb";
const ADMIN: &str = "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ";
const ADMIN_LP_ATA: &str = "ZvsW29zAXZwMayzP9jAVBryRi5rt6X7em5vYdhKGbvZ";
const PROTOCOL: &str = "4sycXz9Xwevedo6eiXR8QEhY8yrQrkNS4G1deY9tAD2Y";
const IDLE_AUTH: &str = "EoHz6FHTL34F6HjuJmb5EceaRqxRG1RMYwYWKtWkGBFb";
const STRATEGY_AUTH: &str = "8fLTf2ufePttZW3Es1xVoW3ows3WjXcuHQkkBCVvHsdH";
const LP_MINT_AUTH: &str = "HHM86gQUM7rN8bz2VPWhqNn7ZcfznLC5KyT7kLs569yq";
const ADAPTOR_ADD_RECEIPT: &str = "AsfkxMdVYjMnr2fdTBMUXhq81hgi2hbENXCy9WhUQF7u";

// ---- v3 ELF under test ------------------------------------------------------
const V3_ELF_FILE: &str = "adaptor-v3.so";
const V3_ELF_SHA256: &str = "836ded9ff4e79cda9fafbafcffcf9f2e9f395c762c69ba5af4630e82a9d8a4d0";
const V3_ELF_BYTES: usize = 107_832;
const HOLDING_SEED: &[u8] = b"withdrawal_holding";

// ---- v3 bootstrap config (frozen at initialize_config) ----------------------
const V3_VAULT_INDEX: u8 = 0;
const V3_MAX_NAV: u64 = 1_000_000_000_000;
const V3_MAX_AGE: u64 = 32;
const V3_MAX_STEP_BPS: u64 = 500;
const V3_STEP_FLOOR: u64 = 1_000_000;
const V3_MIN_INTERVAL: u64 = 60;

// ---- instruction discriminators -------------------------------------------
const VOLTR_DEPOSIT_STRATEGY: [u8; 8] = [246, 82, 57, 226, 131, 222, 253, 249];
const VOLTR_WITHDRAW_STRATEGY: [u8; 8] = [31, 45, 162, 5, 193, 217, 134, 188];
const VOLTR_DEPOSIT_VAULT: [u8; 8] = [126, 224, 21, 255, 228, 53, 117, 33];
const VOLTR_REQUEST_WITHDRAW_VAULT: [u8; 8] = [248, 225, 47, 22, 116, 144, 23, 143];
const VOLTR_WITHDRAW_VAULT: [u8; 8] = [135, 7, 237, 120, 149, 94, 95, 7];
const VOLTR_INITIALIZE_STRATEGY: [u8; 8] = [208, 119, 144, 145, 178, 57, 105, 252];
const VOLTR_UPDATE_VAULT_CONFIG: [u8; 8] = [122, 3, 21, 222, 158, 255, 238, 157];
const ADAPTOR_DEPOSIT: [u8; 8] = [242, 35, 198, 137, 82, 225, 242, 182];
const ADAPTOR_WITHDRAW: [u8; 8] = [183, 18, 70, 156, 148, 109, 161, 34];
const ADAPTOR_ARM_REPORT: [u8; 8] = [164, 175, 246, 41, 178, 140, 35, 3];
const ADAPTOR_INITIALIZE_CONFIG: [u8; 8] = [208, 127, 21, 1, 194, 190, 196, 70];
const ADAPTOR_INITIALIZE_REPORT_TICKET: [u8; 8] = [124, 41, 223, 13, 165, 246, 70, 62];
const ADAPTOR_INITIALIZE: [u8; 8] = [175, 175, 109, 31, 13, 152, 155, 237];

const WAIT_SECS: i64 = 600;
/// the stale mainnet withdrawal request owned by admin BAqg (drained in R3)
const PENDING_RECEIPT: &str = "8eufrxGC9Djf7ekcoWnyewKvYz4GgjtmLLpB8HBji99e";
const PENDING_ESCROW: &str = "C35aUCiMtQa7Zou8Jag5qRbMHqvnVarPwkSwYRgshcC1";
/// updateVaultConfig field ids (reset sequence R0)
const FIELD_LOCKED_PROFIT_DEGRADATION_DURATION: u8 = 2;
const FIELD_ADMIN_PERFORMANCE_FEE: u8 = 5;
const PROTOCOL_TREASURY: &str = "C7sE3MjSAqqF7TgXn1VsNQPWem1gdhqv3ZYV9TNfSjY9";
const VOLTR_HARVEST_FEE: [u8; 8] = [32, 59, 42, 128, 246, 73, 255, 47];
const VOLTR_CANCEL_REQUEST_WITHDRAW_VAULT: [u8; 8] = [231, 54, 14, 6, 223, 124, 127, 238];
const SLOTS_PER_EPOCH: u64 = 432_000;
/// min_report_interval_slots = 60: a report may only move the NAV once per 60
/// slots, so every interval-gated step advances 61 slots.
const INTERVAL_GAP: u64 = 61;
const INTERVAL_SECS: i64 = 30;
/// v2-adaptor cranks (the repair) are only ticket-gated: 10 slots.
const V2_GAP: u64 = 10;
const V2_SECS: i64 = 4;

fn key(s: &str) -> Pubkey {
    Pubkey::from_str(s).unwrap()
}

fn fixtures_dir() -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("fixtures/voltr-repair")
}

fn load_fixture(addr: &str) -> Value {
    let path = fixtures_dir().join(format!("{addr}.json"));
    serde_json::from_slice(&fs::read(&path).unwrap_or_else(|_| panic!("missing fixture {addr}")))
        .unwrap()
}

fn fixture_data(addr: &str) -> Vec<u8> {
    STANDARD
        .decode(load_fixture(addr)["dataBase64"].as_str().unwrap())
        .unwrap()
}

fn set_account_from_fixture(svm: &mut LiteSVM, addr: &str) {
    let v = load_fixture(addr);
    let data = STANDARD.decode(v["dataBase64"].as_str().unwrap()).unwrap();
    svm.set_account(
        key(addr),
        Account {
            lamports: v["lamports"].as_u64().unwrap(),
            data,
            owner: key(v["owner"].as_str().unwrap()),
            executable: false,
            rent_epoch: 0,
        },
    )
    .unwrap();
}

fn program_elf(programdata_addr: &str) -> Vec<u8> {
    let raw = fixture_data(programdata_addr);
    assert_eq!(&raw[45..49], b"\x7fELF", "programdata ELF magic");
    raw[45..].to_vec()
}

fn u16_le(b: &[u8], o: usize) -> u16 {
    u16::from_le_bytes(b[o..o + 2].try_into().unwrap())
}
fn u64_le(b: &[u8], o: usize) -> u64 {
    u64::from_le_bytes(b[o..o + 8].try_into().unwrap())
}
fn u128_le(b: &[u8], o: usize) -> u128 {
    u128::from_le_bytes(b[o..o + 16].try_into().unwrap())
}

fn token_amount_pk(svm: &LiteSVM, addr: &Pubkey) -> u64 {
    match svm.get_account(addr) {
        Some(a) if a.data.len() >= 72 => u64_le(&a.data, 64),
        _ => 0,
    }
}
fn token_amount(svm: &LiteSVM, addr: &str) -> u64 {
    token_amount_pk(svm, &key(addr))
}
fn mint_supply(svm: &LiteSVM, addr: &str) -> u64 {
    u64_le(&svm.get_account(&key(addr)).unwrap().data, 36)
}
fn account_exists(svm: &LiteSVM, addr: &Pubkey) -> bool {
    svm.get_account(addr)
        .map(|a| a.lamports > 0 && !a.data.is_empty())
        .unwrap_or(false)
}
fn account_data(svm: &LiteSVM, addr: &Pubkey) -> Vec<u8> {
    svm.get_account(addr).map(|a| a.data).unwrap_or_default()
}

fn pda(seeds: &[&[u8]], program: &str) -> Pubkey {
    Pubkey::find_program_address(seeds, &key(program)).0
}

fn ata_for(owner: &Pubkey, mint: &Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[owner.as_ref(), key(TOKEN).as_ref(), mint.as_ref()],
        &key(ATA_PROG),
    )
    .0
}

// ---- strategy handle --------------------------------------------------------
#[derive(Clone, Debug)]
struct Strat {
    config: Pubkey,
    receipt: Pubkey,
    auth: Pubkey,
    custody: Pubkey,
    ticket: Pubkey,
    holding_auth: Pubkey,
    holding: Pubkey,
}

fn strat1() -> Strat {
    Strat {
        config: key(STRATEGY),
        receipt: key(RECEIPT),
        auth: key(STRATEGY_AUTH),
        custody: key(CUSTODY_ATA),
        ticket: key(TICKET),
        holding_auth: pda(&[HOLDING_SEED, key(STRATEGY).as_ref()], ADAPTOR),
        holding: ata_for(&pda(&[HOLDING_SEED, key(STRATEGY).as_ref()], ADAPTOR), &key(USDC)),
    }
}

/// Derive every account of a strategy from its (arbitrary) config key.
fn strat_for(config: Pubkey) -> Strat {
    let vault = key(VAULT);
    let receipt = pda(&[b"strategy_init_receipt", vault.as_ref(), config.as_ref()], VOLTR);
    let auth = pda(&[b"vault_strategy_auth", vault.as_ref(), config.as_ref()], VOLTR);
    let custody = ata_for(&auth, &key(USDC));
    let ticket = pda(&[b"report_ticket", config.as_ref()], ADAPTOR);
    let holding_auth = pda(&[HOLDING_SEED, config.as_ref()], ADAPTOR);
    let holding = ata_for(&holding_auth, &key(USDC));
    Strat { config, receipt, auth, custody, ticket, holding_auth, holding }
}

fn strat_json(s: &Strat) -> Value {
    json!({
        "config": s.config.to_string(), "strategyInitReceipt": s.receipt.to_string(),
        "vaultStrategyAuth": s.auth.to_string(), "custodyAta": s.custody.to_string(),
        "reportTicket": s.ticket.to_string(),
        "withdrawalHoldingAuth": s.holding_auth.to_string(), "withdrawalHoldingAta": s.holding.to_string(),
    })
}

// ---- state snapshot ---------------------------------------------------------
#[derive(Clone, Debug)]
struct Snap {
    tv: u64,
    idle: u64,
    receipt1: u64,
    custody1: u64,
    receipt2: Option<u64>,
    receipt2_tracked: Option<u64>,
    custody2: Option<u64>,
    holding2: Option<u64>,
    squads_usdc: u64,
    lp_supply: u64,
    dead_weight: u64,
    fee_manager: u64,
    fee_admin: u64,
    fee_protocol: u64,
    locked_deg: u64,
    locked_raw: u64,
    admin_perf_bps: u16,
    admin_lp: u64,
    clock_slot: u64,
    clock_epoch: u64,
    clock_ts: i64,
}

fn receipt_value(svm: &LiteSVM, receipt: &Pubkey) -> Option<u64> {
    svm.get_account(receipt)
        .filter(|a| a.owner == key(VOLTR) && a.data.len() >= 112)
        .map(|a| u64_le(&a.data, 104))
}

/// Voltr-tracked custody for strategy two: receipt bytes 128..136. Voltr must
/// always leave it zero at an instruction boundary on this binary.
fn receipt_tracked_custody(svm: &LiteSVM, receipt: &Pubkey) -> Option<u64> {
    svm.get_account(receipt)
        .filter(|a| a.owner == key(VOLTR) && a.data.len() >= 136)
        .map(|a| u64_le(&a.data, 128))
}

fn snap(svm: &LiteSVM, s2: Option<&Strat>) -> Snap {
    let v = svm.get_account(&key(VAULT)).unwrap().data;
    let clock: Clock = svm.get_sysvar();
    let receipt2_tracked = s2.and_then(|s| receipt_tracked_custody(svm, &s.receipt));
    if let Some(t) = receipt2_tracked {
        assert_eq!(
            t, 0,
            "receipt2TrackedCustody (strategy-two receipt bytes 128..136, Voltr-tracked custody) must be 0 at every snapshot; got {t}"
        );
    }
    Snap {
        tv: u64_le(&v, 168),
        idle: token_amount(svm, IDLE_ATA),
        receipt1: receipt_value(svm, &key(RECEIPT)).unwrap_or(0),
        custody1: token_amount(svm, CUSTODY_ATA),
        receipt2: s2.and_then(|s| receipt_value(svm, &s.receipt)),
        receipt2_tracked,
        custody2: s2.map(|s| token_amount_pk(svm, &s.custody)),
        holding2: s2.map(|s| token_amount_pk(svm, &s.holding)),
        squads_usdc: token_amount(svm, SQUADS_USDC_ATA),
        lp_supply: mint_supply(svm, LP_MINT),
        dead_weight: u64_le(&v, 616),
        fee_manager: u64_le(&v, 576),
        fee_admin: u64_le(&v, 584),
        fee_protocol: u64_le(&v, 592),
        locked_deg: u64_le(&v, 448),
        locked_raw: u64_le(&v, 672),
        admin_perf_bps: u16::from_le_bytes(v[514..516].try_into().unwrap()),
        admin_lp: token_amount(svm, ADMIN_LP_ATA),
        clock_slot: clock.slot,
        clock_epoch: clock.epoch,
        clock_ts: clock.unix_timestamp,
    }
}

impl Snap {
    fn lp_incl_fees(&self) -> u64 {
        self.lp_supply + self.fee_manager + self.fee_admin + self.fee_protocol + self.dead_weight
    }
    /// Book identity for the live v3 strategy (orphan receipt1 excluded).
    fn book_ok(&self) -> bool {
        match (self.custody2, self.receipt2) {
            (Some(c), Some(r)) => self.tv as u128 == self.idle as u128 + c as u128 + r as u128,
            _ => self.tv == self.idle,
        }
    }
    /// Conserved Voltr quantity, orphan included: idle + receipts - tv.
    fn gap(&self) -> i128 {
        self.idle as i128 + self.receipt1 as i128 + self.receipt2.unwrap_or(0) as i128
            - self.tv as i128
    }
}

fn snap_json(s: &Snap) -> Value {
    json!({
        "vaultTotalValue": s.tv,
        "idleAta": s.idle,
        "strategy1ReceiptPositionValue": s.receipt1,
        "strategy1CustodyAta": s.custody1,
        "strategy2ReceiptPositionValue": s.receipt2,
        "receipt2TrackedCustody": s.receipt2_tracked,
        "strategy2CustodyAta": s.custody2,
        "strategy2HoldingAta": s.holding2,
        "squadsUsdcAta": s.squads_usdc,
        "lpSupply": s.lp_supply,
        "lpSupplyInclFees": s.lp_incl_fees(),
        "feeStateAccumulatedLpAdminFees": s.fee_admin,
        "feeStateAccumulatedLpManagerFees": s.fee_manager,
        "feeStateAccumulatedLpProtocolFees": s.fee_protocol,
        "lockedProfitDegradationDuration": s.locked_deg,
        "lockedProfitRaw": s.locked_raw,
        "adminPerformanceFeeBps": s.admin_perf_bps,
        "adminLpAta": s.admin_lp,
        "deadWeight": s.dead_weight,
        "bookIdentity_tvEqualsIdlePlusCustody2PlusReceipt2": s.book_ok(),
        "conservedGap_idlePlusReceiptsMinusTv": s.gap().to_string(),
        "clock": {"slot": s.clock_slot, "epoch": s.clock_epoch, "unixTimestamp": s.clock_ts},
    })
}

// ---- LiteSVM base -------------------------------------------------------------
struct Base {
    svm: LiteSVM,
    slot0: u64,
    ts0: i64,
}

fn build_base() -> Base {
    let mut svm = LiteSVM::new()
        .with_sigverify(false)
        .with_blockhash_check(false)
        .with_transaction_history(0);

    svm.add_program(key(VOLTR), &program_elf("3fiAyUjktZkZf6hcbBPy6U6UdkMdEFoToS4sjtzAd5az"))
        .unwrap();
    // deployed v2 adaptor (mainnet ProgramData fixture) — replaced by v3 below
    svm.add_program(key(ADAPTOR), &program_elf("DrvzixaVmAuPVVJPtP5wykb9mvgDWqZbvZau9oiCUpHu"))
        .unwrap();
    svm.add_program(key(SQUADS), &program_elf("2g3u9qgz4adKQVN1TUoh7bbBKqaSsjXtz1yX2ptagW5T"))
        .unwrap();

    for addr in [
        VAULT,
        RECEIPT,
        STRATEGY,
        TICKET,
        IDLE_ATA,
        CUSTODY_ATA,
        SQUADS_USDC_ATA,
        SETTINGS,
        USDC,
        LP_MINT,
        PROTOCOL,
        ADAPTOR_ADD_RECEIPT,
        ADMIN_LP_ATA,
        PENDING_RECEIPT,
        PENDING_ESCROW,
    ] {
        set_account_from_fixture(&mut svm, addr);
    }
    svm.set_account(
        key(SQUADS_VAULT),
        Account {
            lamports: 1_000_000,
            data: vec![],
            owner: key(SYS),
            executable: false,
            rent_epoch: 0,
        },
    )
    .unwrap();

    let clock_raw = fixture_data("SysvarC1ock11111111111111111111111111111111");
    let mut clock: Clock = svm.get_sysvar();
    clock.slot = u64_le(&clock_raw, 0);
    clock.epoch_start_timestamp = i64::from_le_bytes(clock_raw[8..16].try_into().unwrap());
    clock.epoch = u64_le(&clock_raw, 16);
    clock.leader_schedule_epoch = u64_le(&clock_raw, 24);
    clock.unix_timestamp = i64::from_le_bytes(clock_raw[32..40].try_into().unwrap());
    svm.set_sysvar(&clock);
    svm.set_sysvar(&solana_sdk::epoch_schedule::EpochSchedule::custom(
        SLOTS_PER_EPOCH,
        SLOTS_PER_EPOCH,
        false,
    ));
    Base { svm, slot0: clock.slot, ts0: clock.unix_timestamp }
}

fn advance_time(svm: &mut LiteSVM, slots: u64, secs: i64) {
    let mut clock: Clock = svm.get_sysvar();
    clock.slot += slots;
    clock.unix_timestamp += secs;
    svm.set_sysvar(&clock);
}

fn set_clock_ts(svm: &mut LiteSVM, ts: i64) {
    let mut clock: Clock = svm.get_sysvar();
    clock.slot += 100;
    clock.unix_timestamp = ts;
    svm.set_sysvar(&clock);
}

fn fund(svm: &mut LiteSVM, who: &Pubkey, lamports: u64) {
    svm.airdrop(who, lamports).unwrap();
}

fn new_funded(svm: &mut LiteSVM) -> Pubkey {
    let k = Pubkey::new_unique();
    fund(svm, &k, 1_000_000_000);
    k
}

type TxResult = Result<(u64, Vec<String>), (String, Vec<String>)>;

fn send(svm: &mut LiteSVM, ixs: &[Instruction], fee_payer: &Pubkey) -> TxResult {
    let msg = Message::new_with_blockhash(ixs, Some(fee_payer), &svm.latest_blockhash());
    let num = msg.header.num_required_signatures as usize;
    let tx = Transaction { signatures: vec![Signature::default(); num], message: msg };
    match svm.send_transaction(tx) {
        Ok(m) => Ok((m.compute_units_consumed, m.logs)),
        Err(f) => Err((format!("{:?}", f.err), f.meta.logs)),
    }
}

fn tx_json(r: &TxResult) -> Value {
    match r {
        Ok((cu, logs)) => json!({"ok": true, "computeUnits": cu,
            "logsTail": logs.iter().rev().take(8).rev().collect::<Vec<_>>()}),
        Err((e, logs)) => json!({"ok": false, "error": e,
            "logsTail": logs.iter().rev().take(14).rev().collect::<Vec<_>>()}),
    }
}

fn err_of(r: &TxResult) -> Option<String> {
    r.as_ref().err().map(|(e, _)| e.clone())
}

fn custom_code(r: &TxResult) -> Option<u32> {
    err_of(&r).and_then(|e| {
        let i = e.find("Custom(")?;
        e[i + 7..].split(|c: char| !c.is_ascii_digit()).next()?.parse().ok()
    })
}

fn err_name(code: u32) -> &'static str {
    match code {
        0 => "InvalidInstruction",
        1 => "InvalidAccountCount",
        2 => "InvalidAccount",
        3 => "InvalidConfig",
        4 => "InvalidAuthority",
        5 => "InvalidSquadsVault",
        6 => "InvalidTokenAccount",
        7 => "InvalidReport",
        8 => "ReportSequence",
        9 => "ReportSlot",
        10 => "ReportCap",
        11 => "DuplicateMutableAccount",
        12 => "InsufficientBridgeLiquidity",
        13 => "InvalidTicket",
        14 => "InvalidTicketWritable",
        15 => "TicketAlreadyArmed",
        16 => "TicketNotArmed",
        17 => "TicketMismatch",
        18 => "TicketReplay",
        19 => "ReportStep",
        20 => "ReportInterval",
        21 => "CustodyNotEmpty",
        _ => "unknown",
    }
}

fn meta(am: bool, aw: bool, addr: &str) -> AccountMeta {
    AccountMeta { pubkey: key(addr), is_signer: am, is_writable: aw }
}
fn m(pk: Pubkey, am: bool, aw: bool) -> AccountMeta {
    AccountMeta { pubkey: pk, is_signer: am, is_writable: aw }
}

fn cu_ix() -> Instruction {
    solana_sdk::compute_budget::ComputeBudgetInstruction::set_compute_unit_limit(1_400_000)
}

fn set_token_account(svm: &mut LiteSVM, addr: Pubkey, mint: Pubkey, owner: Pubkey, amount: u64) {
    let mut data = vec![0u8; SplAccount::LEN];
    SplAccount {
        mint,
        owner,
        amount,
        delegate: COption::None,
        state: AccountState::Initialized,
        is_native: COption::None,
        delegated_amount: 0,
        close_authority: COption::None,
    }
    .pack_into_slice(&mut data);
    svm.set_account(
        addr,
        Account { lamports: 2_039_280, data, owner: key(TOKEN), executable: false, rent_epoch: 0 },
    )
    .unwrap();
}

/// spl-associated-token-account CreateIdempotent (permissionless on mainnet).
fn create_ata_idempotent_ix(payer: &Pubkey, owner: &Pubkey, mint: &Pubkey) -> Instruction {
    Instruction {
        program_id: key(ATA_PROG),
        accounts: vec![
            m(*payer, true, true),
            m(ata_for(owner, mint), false, true),
            m(*owner, false, false),
            m(*mint, false, false),
            meta(false, false, SYS),
            meta(false, false, TOKEN),
        ],
        data: vec![1],
    }
}

fn transfer_checked_ix(source: &Pubkey, dest: &Pubkey, authority: &Pubkey, amount: u64) -> Instruction {
    let mut d = vec![12u8];
    d.extend_from_slice(&amount.to_le_bytes());
    d.push(6); // USDC decimals
    Instruction {
        program_id: key(TOKEN),
        accounts: vec![
            m(*source, false, true),
            meta(false, false, USDC),
            m(*dest, false, true),
            m(*authority, true, false),
        ],
        data: d,
    }
}

// ---- report / crank builders ---------------------------------------------------
fn report_v1(seq: u64, nav: u64) -> Vec<u8> {
    let mut r = vec![1u8];
    r.extend_from_slice(&seq.to_le_bytes());
    r.extend_from_slice(&seq.to_le_bytes());
    r.extend_from_slice(&nav.to_le_bytes());
    r.extend_from_slice(&[7u8; 32]);
    r
}

/// Voltr outer instruction: amount + Borsh Some(Vec<ReportV1>).
fn voltr_capital_data(outer: [u8; 8], amount: u64, inner: [u8; 8], seq: u64, nav: u64) -> Vec<u8> {
    let mut d = Vec::with_capacity(91);
    d.extend_from_slice(&outer);
    d.extend_from_slice(&amount.to_le_bytes());
    d.push(1);
    d.extend_from_slice(&8u32.to_le_bytes());
    d.extend_from_slice(&inner);
    d.push(1);
    d.extend_from_slice(&57u32.to_le_bytes());
    d.extend_from_slice(&report_v1(seq, nav));
    assert_eq!(d.len(), 91);
    d
}

/// The 78-byte adaptor capital wire Voltr unwraps and forwards.
fn adaptor_capital_wire(inner: [u8; 8], amount: u64, seq: u64, nav: u64) -> Vec<u8> {
    let mut d = Vec::with_capacity(78);
    d.extend_from_slice(&inner);
    d.extend_from_slice(&amount.to_le_bytes());
    d.push(1);
    d.extend_from_slice(&57u32.to_le_bytes());
    d.extend_from_slice(&report_v1(seq, nav));
    assert_eq!(d.len(), 78);
    d
}

fn arm_report_data(operation: u8, amount: u64, seq: u64, nav: u64) -> Vec<u8> {
    let mut d = Vec::with_capacity(79);
    d.extend_from_slice(&ADAPTOR_ARM_REPORT);
    d.push(operation);
    d.extend_from_slice(&amount.to_le_bytes());
    d.push(1);
    d.extend_from_slice(&57u32.to_le_bytes());
    d.extend_from_slice(&report_v1(seq, nav));
    assert_eq!(d.len(), 79);
    d
}

fn arm_report_ix(s: &Strat, operation: u8, amount: u64, seq: u64, nav: u64) -> Instruction {
    Instruction {
        program_id: key(ADAPTOR),
        accounts: vec![
            m(s.config, false, false),
            m(s.ticket, false, true),
            meta(false, false, SETTINGS),
            meta(true, false, SQUADS_VAULT),
            meta(false, false, SQUADS),
        ],
        data: arm_report_data(operation, amount, seq, nav),
    }
}

fn deposit_strategy_v2_ix(s: &Strat, amount: u64, seq: u64, nav: u64) -> Instruction {
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            meta(true, false, SQUADS_VAULT),         // 0 manager (signer)
            meta(false, false, PROTOCOL),            // 1 protocol
            meta(false, true, VAULT),                // 2 vault (w)
            m(s.config, false, false),               // 3 strategy (adaptor config)
            meta(false, false, ADAPTOR_ADD_RECEIPT), // 4 adaptor add receipt
            m(s.receipt, false, true),               // 5 strategy init receipt (w)
            meta(false, true, IDLE_AUTH),            // 6 idle auth (w)
            m(s.auth, false, true),                  // 7 strategy auth (w)
            meta(false, true, USDC),                 // 8 asset mint (w)
            meta(false, false, LP_MINT),             // 9 lp mint
            meta(false, true, IDLE_ATA),             // 10 idle ata (w)
            m(s.custody, false, true),               // 11 strategy asset ata (w)
            meta(false, false, TOKEN),               // 12 asset token program
            meta(false, false, ADAPTOR),             // 13 adaptor program
            meta(false, false, SETTINGS),            // remaining -> adaptor CPI
            meta(true, false, SQUADS_VAULT),         //   squads vault (signer)
            meta(false, true, SQUADS_USDC_ATA),      //   squads asset ata (w)
            m(s.ticket, false, true),                //   report ticket (w)
        ],
        data: voltr_capital_data(VOLTR_DEPOSIT_STRATEGY, amount, ADAPTOR_DEPOSIT, seq, nav),
    }
}

fn withdraw_strategy_v2_ix(s: &Strat, amount: u64, seq: u64, nav: u64) -> Instruction {
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            meta(true, false, SQUADS_VAULT),         // 0 manager (signer)
            meta(false, false, PROTOCOL),            // 1 protocol
            meta(false, true, VAULT),                // 2 vault (w)
            meta(false, false, ADAPTOR_ADD_RECEIPT), // 3 adaptor add receipt
            m(s.receipt, false, true),               // 4 strategy init receipt (w)
            m(s.config, false, false),               // 5 strategy (adaptor config)
            meta(false, false, ADAPTOR),             // 6 adaptor program
            meta(false, true, IDLE_AUTH),            // 7 idle auth (w)
            m(s.auth, false, true),                  // 8 strategy auth (w)
            meta(false, true, USDC),                 // 9 asset mint (w)
            meta(false, false, LP_MINT),             // 10 lp mint
            meta(false, true, IDLE_ATA),             // 11 idle ata (w)
            m(s.custody, false, true),               // 12 strategy asset ata (w)
            meta(false, false, TOKEN),               // 13 token program
            meta(false, false, SETTINGS),            // remaining -> adaptor CPI
            meta(true, false, SQUADS_VAULT),         //   squads vault (signer)
            meta(false, true, SQUADS_USDC_ATA),      //   squads asset ata (w)
            m(s.ticket, false, true),                //   ticket (w)
        ],
        data: voltr_capital_data(VOLTR_WITHDRAW_STRATEGY, amount, ADAPTOR_WITHDRAW, seq, nav),
    }
}

/// v3 adaptor CPI tail: 12 accounts = the v2 four (settings, squads vault,
/// squads asset ATA, ticket) + holding ATA (w) + holding auth (ro) + receipt (ro).
fn v3_tail(s: &Strat) -> Vec<AccountMeta> {
    vec![
        meta(false, false, SETTINGS),
        meta(true, false, SQUADS_VAULT),
        meta(false, true, SQUADS_USDC_ATA),
        m(s.ticket, false, true),
        m(s.holding, false, true),
        m(s.holding_auth, false, false),
        m(s.receipt, false, false),
    ]
}

fn deposit_strategy_v3_ix(s: &Strat, amount: u64, seq: u64, nav: u64) -> Instruction {
    let mut accounts = vec![
        meta(true, false, SQUADS_VAULT),
        meta(false, false, PROTOCOL),
        meta(false, true, VAULT),
        m(s.config, false, false),
        meta(false, false, ADAPTOR_ADD_RECEIPT),
        m(s.receipt, false, true),
        meta(false, true, IDLE_AUTH),
        m(s.auth, false, true),
        meta(false, true, USDC),
        meta(false, false, LP_MINT),
        meta(false, true, IDLE_ATA),
        m(s.custody, false, true),
        meta(false, false, TOKEN),
        meta(false, false, ADAPTOR),
    ];
    accounts.extend(v3_tail(s));
    Instruction {
        program_id: key(VOLTR),
        accounts,
        data: voltr_capital_data(VOLTR_DEPOSIT_STRATEGY, amount, ADAPTOR_DEPOSIT, seq, nav),
    }
}

fn withdraw_strategy_v3_ix(s: &Strat, amount: u64, seq: u64, nav: u64) -> Instruction {
    let mut accounts = vec![
        meta(true, false, SQUADS_VAULT),
        meta(false, false, PROTOCOL),
        meta(false, true, VAULT),
        meta(false, false, ADAPTOR_ADD_RECEIPT),
        m(s.receipt, false, true),
        m(s.config, false, false),
        meta(false, false, ADAPTOR),
        meta(false, true, IDLE_AUTH),
        m(s.auth, false, true),
        meta(false, true, USDC),
        meta(false, false, LP_MINT),
        meta(false, true, IDLE_ATA),
        m(s.custody, false, true),
        meta(false, false, TOKEN),
    ];
    accounts.extend(v3_tail(s));
    Instruction {
        program_id: key(VOLTR),
        accounts,
        data: voltr_capital_data(VOLTR_WITHDRAW_STRATEGY, amount, ADAPTOR_WITHDRAW, seq, nav),
    }
}

/// [arm_report, deposit_strategy] one interval-window later (seq == Clock.slot).
fn crank_deposit(svm: &mut LiteSVM, s: &Strat, amount: u64, nav: u64, v3: bool, gap: u64, secs: i64) -> (TxResult, u64) {
    advance_time(svm, gap, secs);
    let clock: Clock = svm.get_sysvar();
    let seq = clock.slot;
    let payer = new_funded(svm);
    let dep = if v3 { deposit_strategy_v3_ix(s, amount, seq, nav) } else { deposit_strategy_v2_ix(s, amount, seq, nav) };
    let ixs = vec![cu_ix(), arm_report_ix(s, 0, amount, seq, nav), dep];
    (send(svm, &ixs, &payer), seq)
}

fn crank_withdraw(svm: &mut LiteSVM, s: &Strat, amount: u64, nav: u64, v3: bool, gap: u64, secs: i64) -> (TxResult, u64) {
    advance_time(svm, gap, secs);
    let clock: Clock = svm.get_sysvar();
    let seq = clock.slot;
    let payer = new_funded(svm);
    let wd = if v3 { withdraw_strategy_v3_ix(s, amount, seq, nav) } else { withdraw_strategy_v2_ix(s, amount, seq, nav) };
    let ixs = vec![cu_ix(), arm_report_ix(s, 1, amount, seq, nav), wd];
    (send(svm, &ixs, &payer), seq)
}

// ---- user-side builders ----------------------------------------------------------
struct User {
    key: Pubkey,
    usdc: Pubkey,
    lp: Pubkey,
}

fn deposit_vault_ix(user: &Pubkey, user_usdc: &Pubkey, user_lp: &Pubkey, amount: u64) -> Instruction {
    let mut data = VOLTR_DEPOSIT_VAULT.to_vec();
    data.extend_from_slice(&amount.to_le_bytes());
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            m(*user, true, false),
            meta(false, false, PROTOCOL),
            meta(false, true, VAULT),
            meta(false, false, USDC),
            meta(false, true, LP_MINT),
            m(*user_usdc, false, true),
            meta(false, true, IDLE_ATA),
            meta(false, false, IDLE_AUTH),
            m(*user_lp, false, true),
            meta(false, false, LP_MINT_AUTH),
            meta(false, false, TOKEN),
            meta(false, false, TOKEN),
            meta(false, false, SYS),
        ],
        data,
    }
}

fn request_withdraw_vault_ix(
    user: &Pubkey,
    user_lp_ata: &Pubkey,
    receipt: &Pubkey,
    escrow: &Pubkey,
    amount_lp: u64,
) -> Instruction {
    let mut data = VOLTR_REQUEST_WITHDRAW_VAULT.to_vec();
    data.extend_from_slice(&amount_lp.to_le_bytes());
    data.push(1); // isAmountInLp
    data.push(1); // isWithdrawAll
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            m(*user, true, true),
            m(*user, true, false),
            meta(false, false, PROTOCOL),
            meta(false, false, VAULT),
            meta(false, false, LP_MINT),
            m(*user_lp_ata, false, true),
            m(*escrow, false, true),
            m(*receipt, false, true),
            meta(false, false, TOKEN),
            meta(false, false, SYS),
        ],
        data,
    }
}

fn withdraw_vault_ix(user: &Pubkey, receipt: &Pubkey, escrow: &Pubkey, user_asset: &Pubkey) -> Instruction {
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            m(*user, true, true),
            meta(false, false, PROTOCOL),
            meta(false, true, VAULT),
            meta(false, false, USDC),
            meta(false, true, LP_MINT),
            m(*escrow, false, true),
            meta(false, true, IDLE_ATA),
            meta(false, true, IDLE_AUTH),
            m(*user_asset, false, true),
            m(*receipt, false, true),
            meta(false, false, TOKEN),
            meta(false, false, TOKEN),
            meta(false, false, SYS),
        ],
        data: VOLTR_WITHDRAW_VAULT.to_vec(),
    }
}

/// Fresh wallet with `amount` USDC (balance override) deposits it all.
fn user_deposit(svm: &mut LiteSVM, amount: u64) -> (User, TxResult, u64) {
    let usdc = key(USDC);
    let lp = key(LP_MINT);
    let user = new_funded(svm);
    let u = User { key: user, usdc: ata_for(&user, &usdc), lp: ata_for(&user, &lp) };
    set_token_account(svm, u.usdc, usdc, user, amount);
    let r = send(
        svm,
        &[cu_ix(), create_ata_idempotent_ix(&user, &user, &lp), deposit_vault_ix(&user, &u.usdc, &u.lp, amount)],
        &user,
    );
    let minted = token_amount_pk(svm, &u.lp);
    (u, r, minted)
}

/// Request ALL LP, wait the withdrawal period, claim. Returns (request, claim, payout).
fn user_exit(svm: &mut LiteSVM, u: &User) -> (TxResult, TxResult, u64) {
    let lp = key(LP_MINT);
    let bal = token_amount_pk(svm, &u.lp);
    let receipt = pda(&[b"request_withdraw_vault_receipt", key(VAULT).as_ref(), u.key.as_ref()], VOLTR);
    let escrow = ata_for(&receipt, &lp);
    let r_req = send(
        svm,
        &[cu_ix(), create_ata_idempotent_ix(&u.key, &receipt, &lp), request_withdraw_vault_ix(&u.key, &u.lp, &receipt, &escrow, bal)],
        &u.key,
    );
    let rr = account_data(svm, &receipt);
    let wf = if rr.len() >= 104 { u64_le(&rr, 96) } else { 0 };
    let now: Clock = svm.get_sysvar();
    set_clock_ts(svm, (wf as i64).max(now.unix_timestamp + WAIT_SECS) + 5);
    let before = token_amount_pk(svm, &u.usdc);
    let r_claim = send(svm, &[cu_ix(), withdraw_vault_ix(&u.key, &receipt, &escrow, &u.usdc)], &u.key);
    let payout = token_amount_pk(svm, &u.usdc) - before;
    (r_req, r_claim, payout)
}

// ---- admin-side builders -----------------------------------------------------------
fn update_vault_config_ix(field: u8, data: &[u8]) -> Instruction {
    let mut d = VOLTR_UPDATE_VAULT_CONFIG.to_vec();
    d.push(field);
    d.extend_from_slice(&(data.len() as u32).to_le_bytes());
    d.extend_from_slice(data);
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            meta(true, false, ADMIN),
            meta(false, false, PROTOCOL),
            meta(false, true, VAULT),
            meta(false, false, RENT_SYSVAR),
        ],
        data: d,
    }
}

fn harvest_fee_ix(harvester: &Pubkey) -> Instruction {
    let lp = key(LP_MINT);
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            m(*harvester, true, false),                            // 0 harvester (signer)
            meta(false, false, SQUADS_VAULT),                      // 1 vault manager
            meta(false, false, ADMIN),                             // 2 vault admin
            meta(false, false, PROTOCOL_TREASURY),                 // 3 protocol treasury
            meta(false, false, PROTOCOL),                          // 4 protocol
            meta(false, true, VAULT),                              // 5 vault (w)
            meta(false, true, LP_MINT),                            // 6 lp mint (w)
            meta(false, false, LP_MINT_AUTH),                      // 7 lp mint auth
            m(ata_for(&key(SQUADS_VAULT), &lp), false, true),      // 8 manager lp ata (w)
            meta(false, true, ADMIN_LP_ATA),                       // 9 admin lp ata (w)
            m(ata_for(&key(PROTOCOL_TREASURY), &lp), false, true), // 10 treasury lp ata (w)
            meta(false, false, TOKEN),                             // 11 lp token program
        ],
        data: VOLTR_HARVEST_FEE.to_vec(),
    }
}

fn cancel_request_withdraw_vault_ix(user: &Pubkey, user_lp_ata: &Pubkey, receipt: &Pubkey, escrow: &Pubkey) -> Instruction {
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            m(*user, true, true),          // 0 userTransferAuthority (w,signer)
            meta(false, false, PROTOCOL),  // 1 protocol
            meta(false, true, VAULT),      // 2 vault (w)
            meta(false, true, LP_MINT),    // 3 vault lp mint (w)
            m(*user_lp_ata, false, true),  // 4 user lp ata (w)
            m(*escrow, false, true),       // 5 request withdraw lp ata (w)
            m(*receipt, false, true),      // 6 request withdraw vault receipt (w)
            meta(false, false, TOKEN),     // 7 lp token program
            meta(false, false, SYS),       // 8 system program
        ],
        data: VOLTR_CANCEL_REQUEST_WITHDRAW_VAULT.to_vec(),
    }
}

/// Overwrite a Voltr strategy receipt's booked `position_value` (104..112) —
/// models a re-derived position for step-bound and withdrawal boundary tests.
fn patch_receipt_position(svm: &mut LiteSVM, receipt: &Pubkey, value: u64) {
    let mut a = svm.get_account(receipt).expect("receipt account to patch");
    a.data[104..112].copy_from_slice(&value.to_le_bytes());
    svm.set_account(*receipt, a).unwrap();
}

/// Overwrite a Voltr strategy receipt's Voltr-tracked custody word (128..136).
fn patch_receipt_tracked(svm: &mut LiteSVM, receipt: &Pubkey, value: u64) {
    let mut a = svm.get_account(receipt).expect("receipt account to patch");
    a.data[128..136].copy_from_slice(&value.to_le_bytes());
    svm.set_account(*receipt, a).unwrap();
}

/// v3 initialize_config: 13 accounts, exactly 41 data bytes.
#[allow(clippy::too_many_arguments)]
fn adaptor_initialize_config_v3_ix(payer: &Pubkey, s: &Strat) -> Instruction {
    let mut d = ADAPTOR_INITIALIZE_CONFIG.to_vec();
    d.push(V3_VAULT_INDEX);
    d.extend_from_slice(&V3_MAX_NAV.to_le_bytes());
    d.extend_from_slice(&V3_MAX_AGE.to_le_bytes());
    d.extend_from_slice(&V3_MAX_STEP_BPS.to_le_bytes());
    d.extend_from_slice(&V3_STEP_FLOOR.to_le_bytes());
    d.extend_from_slice(&V3_MIN_INTERVAL.to_le_bytes());
    assert_eq!(d.len(), 8 + 41);
    Instruction {
        program_id: key(ADAPTOR),
        accounts: vec![
            m(*payer, true, true),           // 0 payer
            m(s.config, true, true),         // 1 config keypair (IS the strategy key)
            meta(false, false, VOLTR),       // 2 voltr program
            meta(false, false, VAULT),       // 3 voltr vault
            m(s.auth, false, false),         // 4 vault_strategy_auth PDA
            meta(false, false, SQUADS),      // 5 squads program
            meta(false, false, SETTINGS),    // 6 settings
            meta(false, false, ADMIN),       // 7 settings signer
            meta(false, false, SQUADS_VAULT),// 8 squads vault PDA
            meta(false, false, USDC),        // 9 asset mint
            meta(false, false, TOKEN),       // 10 token program
            meta(false, false, SQUADS_USDC_ATA), // 11 squads asset ata
            meta(false, false, SYS),         // 12 system program
        ],
        data: d,
    }
}

fn adaptor_initialize_report_ticket_ix(payer: &Pubkey, s: &Strat) -> Instruction {
    Instruction {
        program_id: key(ADAPTOR),
        accounts: vec![
            m(*payer, true, true),
            m(s.config, false, false),
            m(s.ticket, false, true),
            meta(false, false, SYS),
        ],
        data: ADAPTOR_INITIALIZE_REPORT_TICKET.to_vec(),
    }
}

/// Voltr initializeStrategy (manager signs) + the 6 adaptor remaining accounts.
fn voltr_initialize_strategy_ix(payer: &Pubkey, s: &Strat) -> Instruction {
    let mut d = VOLTR_INITIALIZE_STRATEGY.to_vec();
    d.push(1); // Some(instructionDiscriminator)
    d.extend_from_slice(&8u32.to_le_bytes());
    d.extend_from_slice(&ADAPTOR_INITIALIZE);
    d.push(0); // additionalArgs = None
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            m(*payer, true, true),
            meta(true, false, SQUADS_VAULT),
            meta(false, false, PROTOCOL),
            meta(false, false, VAULT),
            m(s.config, false, false),
            meta(false, false, ADAPTOR_ADD_RECEIPT),
            m(s.receipt, false, true),
            m(s.auth, false, true),
            meta(false, false, ADAPTOR),
            meta(false, false, SYS),
            meta(false, false, SETTINGS),
            meta(false, false, SQUADS_VAULT),
            meta(false, false, USDC),
            meta(false, false, TOKEN),
            meta(false, false, SQUADS_USDC_ATA),
            meta(false, false, SQUADS),
        ],
        data: d,
    }
}

// ---- step recorder -----------------------------------------------------------------
struct Rec {
    steps: Vec<Value>,
    all_ok: bool,
}

impl Rec {
    fn push(&mut self, id: &str, name: &str, checks: &[(String, bool)], body: Value) -> bool {
        let ok = checks.iter().all(|(_, p)| *p);
        self.all_ok &= ok;
        let mut v = json!({
            "id": id, "name": name,
            "verdict": if ok { "PASS" } else { "FAIL" },
            "checks": checks.iter().map(|(n, p)| json!({"check": n, "pass": p})).collect::<Vec<_>>(),
        });
        if let (Some(dst), Some(src)) = (v.as_object_mut(), body.as_object()) {
            for (k, val) in src {
                dst.insert(k.clone(), val.clone());
            }
        }
        eprintln!("{id} {} — {name}", if ok { "PASS" } else { "FAIL" });
        for (n, p) in checks {
            if !p {
                eprintln!("    FAILED CHECK: {n}");
            }
        }
        if !ok {
            eprintln!("    step body: {}", serde_json::to_string_pretty(&v).unwrap());
        }
        self.steps.push(v);
        ok
    }
}

fn chk(v: &mut Vec<(String, bool)>, name: impl Into<String>, pass: bool) {
    v.push((name.into(), pass));
}

// =========================================================================================
/// Which manager field index updateVaultConfig uses: probe on clones and detect
/// the vault word that held ST999 flipping to BAqg.
fn probe_manager_field(svm: &LiteSVM, admin: &Pubkey) -> (Option<u8>, Vec<Value>) {
    let before = account_data(svm, &key(VAULT));
    let mgr_off = before
        .windows(32)
        .position(|w| w == key(SQUADS_VAULT).as_ref());
    let mut out = vec![];
    let mut found = None;
    for field in 0u8..=8 {
        let mut clone = svm.clone();
        let r = send(&mut clone, &[cu_ix(), update_vault_config_ix(field, key(ADMIN).as_ref())], admin);
        let after = account_data(&clone, &key(VAULT));
        let changed = matches!(mgr_off, Some(o) if o + 32 <= after.len() && after[o..o + 32] == key(ADMIN).to_bytes());
        if changed && found.is_none() {
            found = Some(field);
        }
        out.push(json!({"field": field, "txOk": r.is_ok(), "managerChangedToAdmin": changed, "error": err_of(&r)}));
    }
    (found, out)
}

/// Swap the loaded v2 adaptor ELF for the v3 ELF in place.
fn swap_adaptor_v3(svm: &mut LiteSVM) -> Value {
    let elf = fs::read(fixtures_dir().join(V3_ELF_FILE)).expect("adaptor-v3.so fixture");
    assert_eq!(elf.len(), V3_ELF_BYTES, "v3 ELF size");
    let added = svm.add_program(key(ADAPTOR), &elf);
    if added.is_err() {
        let lamports = svm.get_account(&key(ADAPTOR)).map(|a| a.lamports).unwrap_or(0);
        svm.set_account(
            key(ADAPTOR),
            Account {
                lamports: lamports.max(1_000_000_000),
                data: elf.clone(),
                owner: solana_sdk::bpf_loader_upgradeable::id(),
                executable: true,
                rent_epoch: 0,
            },
        )
        .expect("set adaptor program account to v3 ELF");
    }
    let a = svm.get_account(&key(ADAPTOR)).expect("adaptor program account after swap");
    let swapped = a.data == elf && a.executable;
    json!({
        "elfFile": V3_ELF_FILE, "elfSha256": V3_ELF_SHA256, "elfBytes": elf.len(),
        "swapMethod": if added.is_ok() { "litesvm add_program (replaces in place)" } else { "set_account (bpf_loader_upgradeable)" },
        "swapOk": swapped, "programAccountOwner": a.owner.to_string(), "executable": a.executable,
    })
}

/// The full main line: dump -> P0 config (degradation/perf-fee 0) -> repair (v2
/// adaptor) -> harvest -> drain to tv == idle == 23 -> swap to v3 -> bootstrap
/// strategy two (3 txs) -> seed 1,000,000 -> V1 allocate -> V2 dust -> V3 restore.
/// Returns the live instance plus every recorded step.
#[allow(clippy::type_complexity)]
fn main_line(run_v1: bool) -> (LiteSVM, Strat, Pubkey, Rec, Base, Snap, LiteSVM) {
    let base = build_base();
    let mut svm = base.svm.clone();
    let mut rec = Rec { steps: vec![], all_ok: true };
    let s1 = strat1();
    let admin = key(ADMIN);
    fund(&mut svm, &admin, 1_000_000_000);
    let s_init = snap(&svm, None);
    assert_eq!(s_init.dead_weight, 1_000, "deadWeight offset sanity");

    // ---------------------------------------------------------------- P0 config (reset-sequence R0 / mainnet P1.1)
    // The matrix base must equal the proven reset sequence: LockedProfit
    // DegradationDuration (86_400 -> 0) and AdminPerformanceFee are cleared
    // BEFORE the repair. Without this the repair's +3,793,536 book jump sits in
    // the 86_400 s locked-profit unlock and a redeemer is paid on the degraded
    // NAV (the T10 NAV-sniper haircut behind v3.1's 827,489 payout).
    {
        let before = snap(&svm, None);
        let r_lock = send(&mut svm, &[cu_ix(), update_vault_config_ix(FIELD_LOCKED_PROFIT_DEGRADATION_DURATION, &0u64.to_le_bytes())], &admin);
        let r_fee = send(&mut svm, &[cu_ix(), update_vault_config_ix(FIELD_ADMIN_PERFORMANCE_FEE, &0u16.to_le_bytes())], &admin);
        let after = snap(&svm, None);
        let mut c = vec![];
        chk(&mut c, "updateVaultConfig(LockedProfitDegradationDuration=0) executed", r_lock.is_ok());
        chk(&mut c, format!("lockedProfitDegradationDuration {} -> 0", before.locked_deg), after.locked_deg == 0);
        chk(&mut c, "updateVaultConfig(AdminPerformanceFee=0) executed", r_fee.is_ok());
        chk(&mut c, format!("adminPerformanceFee {} -> 0", before.admin_perf_bps), after.admin_perf_bps == 0);
        chk(&mut c, "books untouched (tv, receipt1, idle)", after.tv == before.tv && after.receipt1 == before.receipt1 && after.idle == before.idle);
        rec.push("P0-config", "admin updateVaultConfig: LockedProfitDegradationDuration=0, AdminPerformanceFee=0 (the reset sequence's R0 / mainnet P1.1)", &c, json!({
            "signer": ADMIN,
            "instructions": [
                {"ix": "updateVaultConfig", "field": "LockedProfitDegradationDuration(2)", "data": "u64 0", "tx": tx_json(&r_lock)},
                {"ix": "updateVaultConfig", "field": "AdminPerformanceFee(5)", "data": "u16 0", "tx": tx_json(&r_fee)},
            ],
            "before": snap_json(&before), "after": snap_json(&after)}));
        assert!(r_lock.is_ok() && after.locked_deg == 0, "P0 config failed");
    }

    // ---------------------------------------------------------------- M0 repair (v2 adaptor)
    let (repair_body, repair_checks, repair_ok) = {
        let before = snap(&svm, None);
        let nav = before.idle + (before.receipt1 - before.tv);
        let (r, seq) = crank_deposit(&mut svm, &s1, 0, nav, false, V2_GAP, V2_SECS);
        let after = snap(&svm, None);
        let mut c = vec![];
        chk(&mut c, "repair crank (v2 adaptor) executed", r.is_ok());
        chk(&mut c, format!("tv == idle == {}", before.idle), after.tv == before.idle && after.idle == before.idle);
        chk(&mut c, format!("receipt1 == {nav} (orphan value)"), after.receipt1 == nav);
        let b = json!({"reportedNav": nav, "sequence": seq, "tx": tx_json(&r),
            "before": snap_json(&before), "after": snap_json(&after)});
        (b, c, r.is_ok())
    };
    rec.push("M0-repair", "repair strategy one on the DEPLOYED v2 adaptor: arm_report + deposit_strategy(0, nav = idle + receipt1 - tv)", &repair_checks, repair_body);
    assert!(repair_ok, "repair on the v2 adaptor failed");

    // ---------------------------------------------------------------- R2 harvest (reset-sequence / P1.3)
    // Mint the accumulated admin-fee LP (the pending accumulator from the first
    // crank on near-zero supply) so no user deposit is priced against it.
    {
        let before = snap(&svm, None);
        let lp = key(LP_MINT);
        let payer = new_funded(&mut svm);
        let r_atas = send(
            &mut svm,
            &[
                create_ata_idempotent_ix(&payer, &key(SQUADS_VAULT), &lp),
                create_ata_idempotent_ix(&payer, &key(PROTOCOL_TREASURY), &lp),
                create_ata_idempotent_ix(&payer, &admin, &lp),
            ],
            &payer,
        );
        let mut attempts = vec![];
        let mut applied: Option<(&str, TxResult)> = None;
        for (label, who) in [("anyone(random)", Pubkey::new_unique()), ("vaultAdmin", admin), ("vaultManager", key(SQUADS_VAULT))] {
            let mut probe = svm.clone();
            fund(&mut probe, &who, 100_000_000);
            let r = send(&mut probe, &[cu_ix(), harvest_fee_ix(&who)], &who);
            attempts.push(json!({"harvester": label, "key": who.to_string(), "tx": tx_json(&r)}));
            if r.is_ok() && applied.is_none() {
                svm = probe;
                applied = Some((label, r));
            }
        }
        let after = snap(&svm, None);
        let expect = before.fee_admin;
        let mut c = vec![];
        chk(&mut c, "LP ATAs created idempotently", r_atas.is_ok());
        chk(&mut c, "harvestFee executed", applied.is_some());
        chk(&mut c, "feeState.accumulatedLpAdminFees -> 0", after.fee_admin == 0);
        chk(&mut c, format!("admin LP ATA (ZvsW) += {expect}"), after.admin_lp == before.admin_lp + expect);
        chk(&mut c, format!("lp supply += {expect}"), after.lp_supply == before.lp_supply + expect);
        chk(&mut c, "lpSupplyInclFees unchanged (the fee LP was already counted)", after.lp_incl_fees() == before.lp_incl_fees());
        chk(&mut c, "tv/idle untouched", after.tv == before.tv && after.idle == before.idle);
        rec.push("R2-harvest", "harvestFee: mint the accumulated admin-fee LP to the admin LP ATA (clears the pending accumulator before any user deposit)", &c, json!({
            "harvesterThatWorked": applied.as_ref().map(|(l, _)| *l),
            "attempts": attempts,
            "mintedAdminFeeLp": expect,
            "before": snap_json(&before), "after": snap_json(&after)}));
        assert!(applied.is_some() && after.fee_admin == 0, "harvestFee failed");
    }

    // ---------------------------------------------------------------- R3 drain (reset-sequence / P1.4 + P1.5)
    // Cancel the stale pre-repair request, re-request ALL LP at the reset
    // price, wait the 600 s window, claim — ending at tv == idle == 23.
    {
        let before = snap(&svm, None);
        let usdc = key(USDC);
        let lp = key(LP_MINT);
        let admin_usdc = ata_for(&admin, &usdc);
        let pending_receipt = pda(&[b"request_withdraw_vault_receipt", key(VAULT).as_ref(), admin.as_ref()], VOLTR);
        let escrow = ata_for(&pending_receipt, &lp);
        let r_cancel = send(&mut svm, &[cu_ix(), cancel_request_withdraw_vault_ix(&admin, &key(ADMIN_LP_ATA), &pending_receipt, &escrow)], &admin);
        let after_cancel = snap(&svm, None);
        let lp_to_burn = after_cancel.admin_lp;
        let ts_req: Clock = svm.get_sysvar();
        let r_req = send(
            &mut svm,
            &[
                cu_ix(),
                create_ata_idempotent_ix(&admin, &pending_receipt, &lp),
                request_withdraw_vault_ix(&admin, &key(ADMIN_LP_ATA), &pending_receipt, &escrow, lp_to_burn),
            ],
            &admin,
        );
        let rr = account_data(&svm, &pending_receipt);
        let wf = if rr.len() >= 104 { u64_le(&rr, 96) } else { 0 };
        set_clock_ts(&mut svm, (wf as i64).max(ts_req.unix_timestamp + WAIT_SECS) + 5);
        let r_ata2 = send(&mut svm, &[create_ata_idempotent_ix(&admin, &admin, &usdc)], &admin);
        let usdc_before_claim = token_amount_pk(&svm, &admin_usdc);
        let r_claim = send(&mut svm, &[cu_ix(), withdraw_vault_ix(&admin, &pending_receipt, &escrow, &admin_usdc)], &admin);
        let after = snap(&svm, None);
        let payout = token_amount_pk(&svm, &admin_usdc) - usdc_before_claim;
        let fair = (before.tv as u128 * lp_to_burn as u128 / after_cancel.lp_incl_fees().max(1) as u128) as u64;
        let mut c = vec![];
        chk(&mut c, "cancelRequestWithdrawVault executed (admin BAqg)", r_cancel.is_ok());
        chk(&mut c, "after cancel the admin LP ATA holds 100% of LP supply", lp_to_burn > 0 && lp_to_burn == after_cancel.lp_supply);
        chk(&mut c, "cancel left tv/idle untouched", after_cancel.tv == before.tv && after_cancel.idle == before.idle);
        chk(&mut c, "requestWithdrawVault(all) executed", r_req.is_ok());
        chk(&mut c, format!("withdrawableFromTs == requestTs + {WAIT_SECS}"), wf == ts_req.unix_timestamp as u64 + WAIT_SECS as u64);
        chk(&mut c, "withdrawVault claim executed", r_claim.is_ok());
        chk(&mut c, format!("payout == floor(tv*lp/lpInclFees) == {fair}"), payout == fair);
        chk(&mut c, "LP supply == 0 (only the virtual deadWeight 1_000 remains)", after.lp_supply == 0 && after.lp_incl_fees() == after.dead_weight);
        chk(&mut c, "tv == idle (book == real after the drain)", after.tv == after.idle);
        chk(&mut c, "residual tv == idle == 23 (the proven reset end state)", after.tv == 23 && after.idle == 23);
        chk(&mut c, "admin USDC ATA created idempotently", r_ata2.is_ok());
        rec.push("R3-drain", "drain: cancel the stale mainnet request, re-request ALL LP at the reset price, wait 600 s, claim", &c, json!({
            "cancel": {"tx": tx_json(&r_cancel), "after": snap_json(&after_cancel)},
            "request": {"tx": tx_json(&r_req), "lpRequested": lp_to_burn, "withdrawableFromTs": wf},
            "claim": {"ataCreate": tx_json(&r_ata2), "tx": tx_json(&r_claim), "payoutRaw": payout, "fairPayout": fair},
            "before": snap_json(&before), "after": snap_json(&after),
            "lpPriceAfterDrain": format!("{}/{} = {:.9}", after.tv, after.lp_incl_fees(), after.tv as f64 / after.lp_incl_fees().max(1) as f64)}));
        assert!(
            r_cancel.is_ok() && r_req.is_ok() && r_claim.is_ok() && after.lp_supply == 0 && after.tv == after.idle,
            "drain legs differ from the proven reset sequence (voltr_reset_sequence R3) — STOP; see the R3-drain step JSON"
        );
    }

    // ---------------------------------------------------------------- M1 swap to v3
    let swap_info = swap_adaptor_v3(&mut svm);
    rec.push("M1-swap", "replace the adaptor program FSj27… with the v3 ELF in place (Voltr + Squads stay mainnet)", &[("v3 ELF loaded at FSj27…".to_string(), swap_info["swapOk"] == true)], swap_info.clone());
    assert_eq!(swap_info["swapOk"], true, "v3 ELF swap failed");

    // ---------------------------------------------------------------- M2 bootstrap strategy two (3 txs)
    let s2 = strat_for(Pubkey::new_unique());
    let (boot_checks, boot_body) = {
        let before = snap(&svm, None);
        let payer = new_funded(&mut svm);
        // tx 1/3: initialize_config (41 bytes, v3 fields frozen)
        let r_cfg = send(&mut svm, &[cu_ix(), adaptor_initialize_config_v3_ix(&payer, &s2)], &payer);
        // tx 2/3: initialize_report_ticket
        let r_tk = send(&mut svm, &[cu_ix(), adaptor_initialize_report_ticket_ix(&payer, &s2)], &payer);
        // manager field probe (which updateVaultConfig field is Manager?)
        let (mgr_field, mgr_probe) = probe_manager_field(&svm, &admin);
        // tx 3/3: custody + holding ATA create, manager round-trip, initializeStrategy
        let tx3: Vec<Instruction> = match mgr_field {
            Some(f) => vec![
                cu_ix(),
                create_ata_idempotent_ix(&payer, &s2.auth, &key(USDC)),
                create_ata_idempotent_ix(&payer, &s2.holding_auth, &key(USDC)),
                update_vault_config_ix(f, key(ADMIN).as_ref()),
                voltr_initialize_strategy_ix(&payer, &s2),
                update_vault_config_ix(f, key(SQUADS_VAULT).as_ref()),
            ],
            None => vec![
                cu_ix(),
                create_ata_idempotent_ix(&payer, &s2.auth, &key(USDC)),
                create_ata_idempotent_ix(&payer, &s2.holding_auth, &key(USDC)),
                voltr_initialize_strategy_ix(&payer, &s2),
            ],
        };
        let r_tx3 = send(&mut svm, &tx3, &payer);
        let mut tx3_json = tx_json(&r_tx3);
        if r_tx3.is_err() && mgr_field.is_some() {
            // diagnostic fallback on a clone: plain initializeStrategy, no round-trip
            let mut probe = svm.clone();
            let p = new_funded(&mut probe);
            let rp = send(&mut probe, &[cu_ix(), create_ata_idempotent_ix(&p, &s2.auth, &key(USDC)), create_ata_idempotent_ix(&p, &s2.holding_auth, &key(USDC)), voltr_initialize_strategy_ix(&p, &s2)], &p);
            tx3_json["roundTripFallbackProbe"] = json!({"shape": "no manager round-trip", "tx": tx_json(&rp)});
        }
        let after = snap(&svm, Some(&s2));
        let cfg_data = account_data(&svm, &s2.config);
        let cfg_ok = !cfg_data.is_empty()
            && svm.get_account(&s2.config).unwrap().owner == key(ADAPTOR)
            && cfg_data.len() == 472;
        let tk_ok = svm.get_account(&s2.ticket).map(|a| a.owner == key(ADAPTOR) && a.data.len() == 104).unwrap_or(false);
        let rcpt_ok = svm.get_account(&s2.receipt).map(|a| a.owner == key(VOLTR)).unwrap_or(false);
        let mut c = vec![];
        chk(&mut c, "tx 1/3 adaptor initialize_config executed (472-byte v3 config, owner FSj27…)", r_cfg.is_ok() && cfg_ok);
        chk(&mut c, "tx 2/3 adaptor initialize_report_ticket executed (104-byte v3.1 ticket: + last_consumed_slot)", r_tk.is_ok() && tk_ok);
        chk(&mut c, "tx 3/3 custody + holding ATA create + initializeStrategy executed (manager ST999 signer)", r_tx3.is_ok() && rcpt_ok);
        chk(&mut c, "receipt2.positionValue == 0", after.receipt2 == Some(0));
        chk(&mut c, "custody2 and holding ATAs exist and are empty", after.custody2 == Some(0) && after.holding2 == Some(0));
        chk(&mut c, "books untouched by setup (tv, idle, receipt1)", after.tv == before.tv && after.idle == before.idle && after.receipt1 == before.receipt1);
        chk(&mut c, "book identity holds after setup", after.book_ok());
        let c2 = c;
        (c2, json!({
            "strategy2": strat_json(&s2),
            "managerFieldProbe": {"found": mgr_field, "probes": mgr_probe},
            "tx1InitializeConfig": {"tx": tx_json(&r_cfg), "args": {"vaultIndex": V3_VAULT_INDEX, "maxReportNavRaw": V3_MAX_NAV,
                "maxReportAgeSlots": V3_MAX_AGE, "maxStepBps": V3_MAX_STEP_BPS, "stepFloorRaw": V3_STEP_FLOOR,
                "minReportIntervalSlots": V3_MIN_INTERVAL}, "configLen": cfg_data.len(),
                "configOwner": svm.get_account(&s2.config).map(|a| a.owner.to_string()).unwrap_or_default()},
            "tx2InitializeReportTicket": tx_json(&r_tk),
            "tx3InitializeStrategy": tx3_json,
            "before": snap_json(&before), "after": snap_json(&after),
        }))
    };
    let boot_ok = rec.push("M2-bootstrap", "bootstrap strategy two on v3 in three transactions", &boot_checks, boot_body);

    // ---------------------------------------------------------------- M3 seed 1,000,000
    let seed_ok = if boot_ok {
        let usdc = key(USDC);
        let lp = key(LP_MINT);
        let admin_usdc = ata_for(&admin, &usdc);
        let seed = 1_000_000u64;
        set_token_account(&mut svm, admin_usdc, usdc, admin, seed);
        let before = snap(&svm, Some(&s2));
        let r_seed = send(&mut svm, &[cu_ix(), create_ata_idempotent_ix(&admin, &admin, &lp), deposit_vault_ix(&admin, &admin_usdc, &key(ADMIN_LP_ATA), seed)], &admin);
        let after = snap(&svm, Some(&s2));
        let mut c = vec![];
        chk(&mut c, format!("admin deposit_vault({seed}) executed"), r_seed.is_ok());
        chk(&mut c, format!("tv == idle == {}", before.tv + seed), after.tv == before.tv + seed && after.idle == after.tv);
        chk(&mut c, "book identity holds", after.book_ok());
        rec.push("M3-seed", "seed the vault with a 1,000,000 admin deposit (as R5a does)", &c, json!({
            "tx": tx_json(&r_seed), "before": snap_json(&before), "after": snap_json(&after)}));
        r_seed.is_ok()
    } else {
        false
    };

    if !run_v1 {
        let s_now = snap(&svm, Some(&s2));
        let post_v1 = svm.clone();
        return (svm, s2, admin, rec, base, s_now, post_v1);
    }

    // ---------------------------------------------------------------- M4 V1 allocate
    // v3.1 accepts the receipt at capital index 11 WRITABLE (only a signer at
    // [11] is rejected), so the Voltr capital path executes end to end.
    assert!(seed_ok, "bootstrap/seed failed; V1 cannot run");
    let (v1_checks, v1_body) = {
        let alloc = 500_000u64;
        let before = snap(&svm, Some(&s2));
        let (r, seq) = crank_deposit(&mut svm, &s2, alloc, alloc, true, INTERVAL_GAP, INTERVAL_SECS);
        let after = snap(&svm, Some(&s2));
        let mut c = vec![];
        chk(&mut c, "V1 arm_report + deposit_strategy(500_000, nav 500_000) executed through Voltr", r.is_ok());
        chk(&mut c, "custody2 swept to 0", after.custody2 == Some(0));
        chk(&mut c, "EBG2 += 500_000", after.squads_usdc == before.squads_usdc + alloc);
        chk(&mut c, "idle == before - 500_000", after.idle == before.idle - alloc);
        chk(&mut c, "receipt2 == 500_000 (reported NAV booked)", after.receipt2 == Some(alloc));
        chk(&mut c, "tv unchanged (the allocation books as idle + receipt2)", after.tv == before.tv);
        chk(&mut c, "book identity tv == idle + custody2 + receipt2", after.book_ok());
        (c, json!({
            "allocateExecuted": r.is_ok(),
            "sequence": seq, "tx": tx_json(&r), "error": err_of(&r),
            "before": snap_json(&before), "after": snap_json(&after),
        }))
    };
    rec.push(
        "V1-allocate",
        "V1 allocate: arm_report + deposit_strategy(500_000, nav 500_000) through Voltr on the v3.3 ELF (writable receipt at capital index 11 accepted)",
        &v1_checks,
        v1_body,
    );
    assert!(
        v1_checks.iter().all(|(_, p)| *p),
        "V1 allocate failed; if the adaptor (not the harness) rejected it, STOP and report the error and logs"
    );
    // Frozen reference for V4..V9: receipt2 500_000, EBG2 500_000, custody2 0.
    let post_v1 = svm.clone();

    // ---------------------------------------------------------------- M5 V2 dust refresh
    {
        let dust = 100u64;
        let mut clone = svm.clone();
        set_token_account(&mut clone, s2.custody, key(USDC), s2.auth, dust);
        let before = snap(&clone, Some(&s2));
        let (r, seq) = crank_deposit(&mut clone, &s2, 0, before.receipt2.unwrap_or(0) + dust, true, INTERVAL_GAP, INTERVAL_SECS);
        let after = snap(&clone, Some(&s2));
        let mut c = vec![];
        chk(&mut c, "V2 refresh (deposit_strategy(0, nav receipt2+100)) with 100 raw dust in custody2 executed", r.is_ok());
        chk(&mut c, "custody2 swept to 0", after.custody2 == Some(0));
        chk(&mut c, "EBG2 += 100", after.squads_usdc == before.squads_usdc + dust);
        chk(&mut c, "receipt2 == 500_100", after.receipt2 == Some(before.receipt2.unwrap_or(0) + dust));
        chk(&mut c, "tv += 100", after.tv == before.tv + dust);
        chk(&mut c, "book identity holds", after.book_ok());
        rec.push("V2-dust-refresh", "V2 dust on refresh: 100 raw third-party dust in custody2 is swept and credited by deposit_strategy(0)", &c, json!({
            "sequence": seq, "tx": tx_json(&r), "error": err_of(&r),
            "before": snap_json(&before), "after": snap_json(&after)}));
    }

    // ---------------------------------------------------------------- M6 V3 restore via holding
    {
        let alloc = 500_000u64;
        let before = snap(&svm, Some(&s2));
        let r_stage = send(&mut svm, &[transfer_checked_ix(&key(SQUADS_USDC_ATA), &s2.holding, &key(SQUADS_VAULT), alloc)], &key(SQUADS_VAULT));
        let staged = snap(&svm, Some(&s2));
        let (r_w, seq) = crank_withdraw(&mut svm, &s2, alloc, 0, true, INTERVAL_GAP, INTERVAL_SECS);
        let after = snap(&svm, Some(&s2));
        let mut c = vec![];
        chk(&mut c, "V3 manager SPL transfer 500_000 EBG2 -> holding ATA executed", r_stage.is_ok() && staged.holding2 == Some(alloc));
        chk(&mut c, "V3 arm + withdraw_strategy(500_000, nav 0) executed", r_w.is_ok());
        chk(&mut c, "custody2 == 0 before and after", staged.custody2 == Some(0) && after.custody2 == Some(0));
        chk(&mut c, "holding == 0 after (exact pull)", after.holding2 == Some(0));
        chk(&mut c, "idle += 500_000", after.idle == before.idle + alloc);
        chk(&mut c, "receipt2 == 0", after.receipt2 == Some(0));
        chk(&mut c, "tv unchanged", after.tv == before.tv);
        chk(&mut c, "book identity tv == idle + custody2 + receipt2", after.book_ok());
        rec.push("V3-restore-holding", "V3 restore via holding: stage 500_000 EBG2 -> holding, then arm + withdraw_strategy(500_000, nav 0)", &c, json!({
            "stage": tx_json(&r_stage), "sequence": seq, "tx": tx_json(&r_w), "error": err_of(&r_w),
            "before": snap_json(&before), "afterStage": snap_json(&staged), "after": snap_json(&after)}));
    }

    (svm, s2, admin, rec, base, s_init, post_v1)
}

// =========================================================================================
/// V4..V9 + V11 on clones of the post-V1 state; V10 on the main line after V3.
fn extra_cases(svm: &mut LiteSVM, post_v1: &LiteSVM, s2: &Strat, admin: &Pubkey, rec: &mut Rec) {
    let usdc = key(USDC);
    let squads_vault = key(SQUADS_VAULT);
    let squads_ata = key(SQUADS_USDC_ATA);
    let alloc = 500_000u64;

    // post-V1 reference state: receipt2 500_000, EBG2 500_000, custody2 0, holding 0
    let post_v1_snap = snap(post_v1, Some(s2));

    // ---------------------------------------------------------------- V4 over-staged holding
    {
        let mut clone = post_v1.clone();
        let r_stage = send(&mut clone, &[transfer_checked_ix(&squads_ata, &s2.holding, &squads_vault, alloc)], &squads_vault);
        // +100_000 extra staged (balance override: modelled extra manager cash)
        set_token_account(&mut clone, squads_ata, usdc, squads_vault, 100_000);
        let r_stage2 = send(&mut clone, &[transfer_checked_ix(&squads_ata, &s2.holding, &squads_vault, 100_000)], &squads_vault);
        let before = snap(&clone, Some(s2));
        let (r_w, seq) = crank_withdraw(&mut clone, s2, alloc, 0, true, INTERVAL_GAP, INTERVAL_SECS);
        let after = snap(&clone, Some(s2));
        let mut c = vec![];
        chk(&mut c, "V4 staging 600_000 into holding executed", r_stage.is_ok() && r_stage2.is_ok() && before.holding2 == Some(600_000));
        chk(&mut c, "V4 withdraw_strategy(500_000, nav 0) executed", r_w.is_ok());
        chk(&mut c, "100_000 remains in holding", after.holding2 == Some(100_000));
        chk(&mut c, "idle += 500_000", after.idle == before.idle + alloc);
        chk(&mut c, "receipt2 == 0, custody2 == 0", after.receipt2 == Some(0) && after.custody2 == Some(0));
        chk(&mut c, "tv unchanged", after.tv == before.tv);
        chk(&mut c, "book identity holds (over-staged 100_000 is unbooked adaptor cash)", after.book_ok());
        rec.push("V4-over-staged-holding", "V4 over-staged holding: 600_000 in holding, withdraw_strategy(500_000, nav 0) leaves 100_000 unbooked in holding", &c, json!({
            "staging": {"leg1": tx_json(&r_stage), "leg2": tx_json(&r_stage2),
                "note": "leg2 tops the Squads USDC ATA up by balance override (+100_000 modelled cash) before transferring"},
            "sequence": seq, "tx": tx_json(&r_w), "error": err_of(&r_w),
            "before": snap_json(&before), "after": snap_json(&after)}));
    }

    // ---------------------------------------------------------------- V5 custody residue at withdraw entry (v3.3)
    // v3.3 sweeps residue WHOLE to Squads before pulling and prices only the
    // Voltr-explained flow, so a third-party donation can never fail an honest
    // withdraw; the residue is unreported until the next report (safe
    // direction). Error 21 exists only as a post-sweep postcondition.
    {
        let mut clone = post_v1.clone();
        set_token_account(&mut clone, s2.custody, usdc, s2.auth, 100);
        let r_stage = send(&mut clone, &[transfer_checked_ix(&squads_ata, &s2.holding, &squads_vault, alloc)], &squads_vault);
        let b1 = snap(&clone, Some(s2));
        let (r_w1, seq1) = crank_withdraw(&mut clone, s2, alloc, 0, true, INTERVAL_GAP, INTERVAL_SECS);
        let a1 = snap(&clone, Some(s2));
        let mut c = vec![];
        chk(&mut c, "V5 pre-state: 100 raw third-party dust in custody2 + 500_000 staged in holding", r_stage.is_ok() && b1.custody2 == Some(100) && b1.holding2 == Some(alloc));
        chk(&mut c, "V5 withdraw(500_000, nav 0) with residue SUCCEEDS (a donation never blocks an honest report)", r_w1.is_ok());
        chk(&mut c, "V5 residue swept whole to Squads first: custody2 0, EBG2 +100", a1.custody2 == Some(0) && a1.squads_usdc == b1.squads_usdc + 100);
        chk(&mut c, "V5 exact pull from holding: holding 0, idle +500_000", a1.holding2 == Some(0) && a1.idle == b1.idle + alloc);
        chk(&mut c, "V5 receipt2 == nav (0), tv unchanged", a1.receipt2 == Some(0) && a1.tv == b1.tv);
        chk(&mut c, "V5 book identity holds (the 100 raw sits in EBG2 outside the book)", a1.book_ok());
        // the swept residue becomes reportable on the NEXT report: refresh nav = receipt + 100
        let (r_ref, seq2) = crank_deposit(&mut clone, s2, 0, 100, true, INTERVAL_GAP, INTERVAL_SECS);
        let a2 = snap(&clone, Some(s2));
        chk(&mut c, "V5 next refresh reporting nav +100 books the residue: receipt2 100, tv +100", r_ref.is_ok() && a2.receipt2 == Some(100) && a2.tv == a1.tv + 100);
        chk(&mut c, "V5 book identity holds after the refresh", a2.book_ok());
        rec.push("V5-custody-residue", "V5 custody residue (v3.3): withdraw sweeps 100 raw to Squads, then pulls exactly 500_000 from holding — SUCCEEDS; a follow-up refresh reporting nav +100 books the residue (tv +100)", &c, json!({
            "withdraw": {"sequence": seq1, "tx": tx_json(&r_w1), "error": err_of(&r_w1), "adaptorErrorName": custom_code(&r_w1).map(err_name)},
            "refresh": {"sequence": seq2, "tx": tx_json(&r_ref)},
            "before": snap_json(&b1), "afterWithdraw": snap_json(&a1), "afterRefresh": snap_json(&a2)}));
    }

    // ---------------------------------------------------------------- V6 step bound
    {
        let step = V3_STEP_FLOOR.max(post_v1_snap.receipt2.unwrap_or(0) * V3_MAX_STEP_BPS / 10_000);
        let ceiling = post_v1_snap.receipt2.unwrap_or(0) + step;
        let mut cases = vec![];
        let mut c = vec![];
        for (label, nav, expect_ok) in [
            ("nav 1_000_000 (inside the window; bps alone would cap it at 525_000)", 1_000_000u64, true),
            ("nav 1_500_000 (exact boundary: receipt2 + step)", ceiling, true),
            ("nav 1_500_001 (one raw above the boundary)", ceiling + 1, false),
        ] {
            let mut clone = post_v1.clone();
            let b = snap(&clone, Some(s2));
            let (r, seq) = crank_deposit(&mut clone, s2, 0, nav, true, INTERVAL_GAP, INTERVAL_SECS);
            let a = snap(&clone, Some(s2));
            let code = custom_code(&r);
            let matches = r.is_ok() == expect_ok && if expect_ok { a.book_ok() } else { true };
            chk(&mut c, format!("V6 {label}: executed={} (expected ok={expect_ok}, code={})", r.is_ok(), code.map(err_name).unwrap_or("none")), matches);
            cases.push(json!({"case": label, "reportedNav": nav, "step": step, "boundary": ceiling,
                "sequence": seq, "ok": r.is_ok(), "error": err_of(&r), "adaptorErrorName": code.map(err_name),
                "tx": tx_json(&r), "tvBefore": b.tv, "tvAfter": a.tv, "receipt2After": a.receipt2, "bookOkAfter": a.book_ok()}));
        }
        rec.push("V6-step-bound", "V6 step bound on a zero-flow refresh: |nav - receipt2| <= step = max(receipt2 * 500 / 10_000, 1_000_000) = 1_000_000 at receipt2 500_000; pass edge nav 1_500_000", &c, json!({
            "config": {"maxStepBps": V3_MAX_STEP_BPS, "stepFloorRaw": V3_STEP_FLOOR},
            "stepAtReceipt2_500_000": step, "navCeiling": ceiling,
            "cases": cases}));
    }

    // ---------------------------------------------------------------- V6b deposit-side step bound
    // Flow-adjusted equation: |(nav_after + outflow) - (receipt + inflow)| <= step
    // with inflow = the executed custody sweep. A 100_000 deposit onto receipt2
    // 500_000 books 600_000, so the pass edge is 600_000 + 1_000_000 = 1_600_000.
    {
        let receipt2 = post_v1_snap.receipt2.unwrap_or(0);
        let step = V3_STEP_FLOOR.max(receipt2 * V3_MAX_STEP_BPS / 10_000);
        let booked = receipt2 + 100_000u64;
        let pass_edge = booked + step;
        let mut cases = vec![];
        let mut c = vec![];
        for (label, nav, expect_ok) in [
            ("deposit 100_000 reporting nav 1_600_000 (exact pass edge: booked + step)", pass_edge, true),
            ("deposit 100_000 reporting nav 1_600_001 (one raw above the boundary)", pass_edge + 1, false),
        ] {
            let mut clone = post_v1.clone();
            let b = snap(&clone, Some(s2));
            let (r, seq) = crank_deposit(&mut clone, s2, 100_000, nav, true, INTERVAL_GAP, INTERVAL_SECS);
            let a = snap(&clone, Some(s2));
            let code = custom_code(&r);
            let matches =
                r.is_ok() == expect_ok && if expect_ok { a.book_ok() } else { a.tv == b.tv && a.receipt2 == b.receipt2 };
            chk(&mut c, format!("V6b {label}: executed={} (expected ok={expect_ok}, code={})", r.is_ok(), code.map(err_name).unwrap_or("none")), matches);
            cases.push(json!({"case": label, "depositAmount": 100_000u64, "reportedNav": nav, "step": step,
                "bookedWithSweep": booked, "passEdge": pass_edge, "sequence": seq, "ok": r.is_ok(),
                "error": err_of(&r), "adaptorErrorName": code.map(err_name), "tx": tx_json(&r),
                "tvBefore": b.tv, "tvAfter": a.tv, "receipt2After": a.receipt2, "bookOkAfter": a.book_ok()}));
        }
        rec.push("V6b-deposit-step-bound", "V6b deposit-side step bound: deposit 100_000 onto receipt2 500_000 -> booked 600_000; pass edge nav 1_600_000, nav 1_600_001 fails ReportStep(19)", &c, json!({
            "equation": "|(nav_after + outflow) - (receipt + inflow)| <= step; inflow = the executed custody sweep",
            "config": {"maxStepBps": V3_MAX_STEP_BPS, "stepFloorRaw": V3_STEP_FLOOR},
            "stepAtReceipt2_500_000": step, "bookedWithSweep": booked, "passEdge": pass_edge,
            "cases": cases}));
    }

    // ---------------------------------------------------------------- V7 interval
    {
        let mut clone = post_v1.clone();
        let (r1, seq1) = crank_deposit(&mut clone, s2, 0, 500_000, true, INTERVAL_GAP, INTERVAL_SECS);
        let a1 = snap(&clone, Some(s2));
        // second refresh only 10 slots later -> ReportInterval at arm time
        let (r2, seq2) = crank_deposit(&mut clone, s2, 0, 500_000, true, 10, 4);
        let code2 = custom_code(&r2);
        // third refresh 61+ slots after the consumed sequence -> succeeds
        let (r3, seq3) = crank_deposit(&mut clone, s2, 0, 500_100, true, INTERVAL_GAP, INTERVAL_SECS);
        let a3 = snap(&clone, Some(s2));
        let mut c = vec![];
        chk(&mut c, "V7 first refresh executed", r1.is_ok() && a1.receipt2 == Some(500_000));
        chk(&mut c, format!("V7 second refresh 10 slots later rejected with adaptor error 20 ReportInterval (got {})", code2.map(err_name).unwrap_or("none")), r2.is_err() && code2 == Some(20));
        chk(&mut c, "V7 third refresh 61 slots later executed", r3.is_ok());
        chk(&mut c, "V7 book identity holds after each successful step", a1.book_ok() && a3.book_ok());
        rec.push("V7-interval", "V7 minimum report interval (60 slots): second refresh 10 slots later fails error 20, a refresh 61 slots later succeeds", &c, json!({
            "config": {"minReportIntervalSlots": V3_MIN_INTERVAL},
            "first": {"sequence": seq1, "tx": tx_json(&r1), "after": snap_json(&a1)},
            "second": {"sequence": seq2, "tx": tx_json(&r2), "error": err_of(&r2), "adaptorErrorName": code2.map(err_name)},
            "third": {"sequence": seq3, "tx": tx_json(&r3), "after": snap_json(&a3)}}));
    }

    // ---------------------------------------------------------------- V8 third-party direct call
    {
        let mut clone = post_v1.clone();
        let clock: Clock = clone.get_sysvar();
        let seq = clock.slot + INTERVAL_GAP;
        advance_time(&mut clone, INTERVAL_GAP, INTERVAL_SECS);
        let payer = new_funded(&mut clone);
        // arm through the normal path first, so the ONLY difference is the caller
        let r_arm = send(&mut clone, &[cu_ix(), arm_report_ix(s2, 0, 0, seq, 500_000)], &payer);
        let intruder = Pubkey::new_unique();
        let ix = Instruction {
            program_id: key(ADAPTOR),
            accounts: vec![
                m(intruder, true, true),                  // 0 "strategy auth" = random signer
                m(s2.config, false, false),               // 1 config
                meta(false, true, USDC),                  // 2 mint
                m(s2.custody, false, true),               // 3 custody
                meta(false, false, TOKEN),                // 4 token program
                meta(false, false, SETTINGS),             // 5 settings
                meta(false, false, SQUADS_VAULT),         // 6 squads vault
                meta(false, true, SQUADS_USDC_ATA),       // 7 squads asset ata
                m(s2.ticket, false, true),                // 8 ticket
                m(s2.holding, false, true),               // 9 holding
                m(s2.holding_auth, false, false),         // 10 holding auth
                m(s2.receipt, false, false),              // 11 receipt
            ],
            data: adaptor_capital_wire(ADAPTOR_DEPOSIT, 0, seq, 500_000),
        };
        // fund the intruder so the fee payer exists; the caller under test is
        // still the random key at accounts[0]
        fund(&mut clone, &intruder, 1_000_000);
        let r = send(&mut clone, &[cu_ix(), ix], &payer);
        let code = custom_code(&r);
        let mut c = vec![];
        chk(&mut c, "V8 ticket armed through the normal path", r_arm.is_ok());
        chk(&mut c, format!("V8 direct third-party call rejected (code {})", code.map(err_name).unwrap_or("none")), r.is_err() && matches!(code, Some(4) | Some(5) | Some(2)));
        rec.push("V8-direct-call", "V8 third-party direct call: the adaptor capital path invoked directly (not via Voltr) with a random signer at accounts[0]", &c, json!({
            "arm": tx_json(&r_arm), "caller": intruder.to_string(),
            "tx": tx_json(&r), "error": err_of(&r), "adaptorErrorName": code.map(err_name)}));
    }

    // ---------------------------------------------------------------- V9 v2 config rejected
    {
        let s1 = strat1();
        let mut clone = post_v1.clone();
        let b = snap(&clone, Some(s2));
        let (r, seq) = crank_deposit(&mut clone, &s1, 0, b.receipt1, false, V2_GAP, V2_SECS);
        let a = snap(&clone, Some(s2));
        let code = custom_code(&r);
        let mut c = vec![];
        chk(
            &mut c,
            format!("V9 strategy-one crank on the v3 binary rejected (code {})", code.map(err_name).unwrap_or("none")),
            // The realistic mistake is the old 4-account tail, which trips the
            // 12-account count first (InvalidInstruction 0); a v3-shaped crank
            // on the v2 config would trip the config version (InvalidConfig 3).
            // Either rejection closes the v2 path.
            r.is_err() && matches!(code, Some(0) | Some(3)),
        );
        chk(&mut c, "V9 books untouched (v2 config cannot be driven by v3)", a.tv == b.tv && a.receipt1 == b.receipt1);
        rec.push("V9-v2-config-rejected", "V9 v2 config rejected: a strategy-one (9hDH…) deposit_strategy(0) crank against the v3 binary", &c, json!({
            "sequence": seq, "tx": tx_json(&r), "error": err_of(&r), "adaptorErrorName": code.map(err_name),
            "before": snap_json(&b), "after": snap_json(&a)}));
    }

    // ---------------------------------------------------------------- V11 invariant sweep (main line)
    {
        // gap recorded at every main-line snapshot so far
        let now = snap(svm, Some(s2));
        let g = now.gap();
        let mut c = vec![];
        chk(&mut c, format!("V11 D = tv - idle - (receipt1 + receipt2) == {} conserved on the main line", -g), g == post_v1_snap.gap());
        chk(&mut c, "V11 final book identity tv == idle + custody2 + receipt2", now.book_ok());
        rec.push("V11-invariants", "V11 invariant sweep: D conserved across the whole main line (dump -> config -> repair -> harvest -> drain -> swap -> bootstrap -> seed -> V1 -> V2 -> V3)", &c, json!({
            "conservedGapIdlePlusReceiptsMinusTv": g.to_string(),
            "conservedDTvMinusIdleMinusReceipts": (-g).to_string(),
            "snapshotNow": snap_json(&now), "snapshotAtPostV1": snap_json(&post_v1_snap)}));
    }

    // ---------------------------------------------------------------- V14 donations never block (v3.3)
    // The step bound prices ONLY Voltr-explained flows: deposit inflow = the
    // capital amount, refresh inflow = 0, withdraw outflow = the pulled amount.
    // Swept custody residue is NOT priced, so no donation — however large — can
    // fail an honest report; the residue sits in Squads EBG2 outside the Voltr
    // book until the next report (safe direction).
    {
        // (a) 999_999 raw donated to custody2 before a nav-500_000 refresh
        let mut clone = post_v1.clone();
        set_token_account(&mut clone, s2.custody, usdc, s2.auth, 999_999);
        let b = snap(&clone, Some(s2));
        let (r, seq) = crank_deposit(&mut clone, s2, 0, 500_000, true, INTERVAL_GAP, INTERVAL_SECS);
        let a = snap(&clone, Some(s2));
        let mut c = vec![];
        chk(&mut c, "V14a refresh nav 500_000 with 999_999 raw donated to custody2 SUCCEEDS", r.is_ok());
        chk(&mut c, "V14a residue swept whole to Squads: custody2 0, EBG2 +999_999", a.custody2 == Some(0) && a.squads_usdc == b.squads_usdc + 999_999);
        chk(&mut c, "V14a donation unreported: receipt2 500_000, tv unchanged", a.receipt2 == Some(500_000) && a.tv == b.tv);
        chk(&mut c, "V14a book identity holds", a.book_ok());
        rec.push("V14a-donation-refresh", "V14a donation 999_999 raw before a nav-500_000 refresh: passes, swept whole to EBG2, unreported (tv unchanged)", &c, json!({
            "donatedRaw": 999_999u64, "sequence": seq, "tx": tx_json(&r), "error": err_of(&r),
            "before": snap_json(&b), "after": snap_json(&a)}));

        // (b) 5_000_000_000 raw (5_000 USDC) donated — far above the 1_000_000 step floor
        let step = V3_STEP_FLOOR.max(500_000u64 * V3_MAX_STEP_BPS / 10_000);
        let mut clone = post_v1.clone();
        set_token_account(&mut clone, s2.custody, usdc, s2.auth, 5_000_000_000);
        let b = snap(&clone, Some(s2));
        let (r, seq) = crank_deposit(&mut clone, s2, 0, 500_000, true, INTERVAL_GAP, INTERVAL_SECS);
        let a = snap(&clone, Some(s2));
        let mut c = vec![];
        chk(&mut c, "V14b refresh nav 500_000 with 5_000_000_000 raw donated (> step floor) still SUCCEEDS", r.is_ok());
        chk(&mut c, "V14b EBG2 +5_000_000_000, custody2 0, tv unchanged (unpriced)", a.custody2 == Some(0) && a.squads_usdc == b.squads_usdc + 5_000_000_000 && a.tv == b.tv);
        chk(&mut c, "V14b book identity holds", a.book_ok());
        // the donation is unreported, so the NEXT report is still bounded at receipt + step
        let (r_pass, seq_p) = crank_deposit(&mut clone, s2, 0, 500_000 + step, true, INTERVAL_GAP, INTERVAL_SECS);
        let a_pass = snap(&clone, Some(s2));
        chk(&mut c, "V14b next refresh nav 1_500_000 (receipt + step) passes", r_pass.is_ok() && a_pass.book_ok());
        let mut clone2 = post_v1.clone();
        set_token_account(&mut clone2, s2.custody, usdc, s2.auth, 5_000_000_000);
        let _ = crank_deposit(&mut clone2, s2, 0, 500_000, true, INTERVAL_GAP, INTERVAL_SECS);
        let (r_fail, seq_f) = crank_deposit(&mut clone2, s2, 0, 500_000 + step + 1, true, INTERVAL_GAP, INTERVAL_SECS);
        let code_f = custom_code(&r_fail);
        chk(
            &mut c,
            format!("V14b next refresh nav 1_500_001 rejected with error 19 ReportStep (got {})", code_f.map(err_name).unwrap_or("none")),
            r_fail.is_err() && code_f == Some(19),
        );
        rec.push("V14b-donation-over-step", "V14b donation 5_000_000_000 raw (5_000 USDC, > step): still passes; the next report stays bounded at receipt + step (nav 1_500_000 passes, 1_500_001 fails 19)", &c, json!({
            "donatedRaw": 5_000_000_000u64, "step": step,
            "refresh": {"sequence": seq, "tx": tx_json(&r), "error": err_of(&r)},
            "nextPass": {"sequence": seq_p, "tx": tx_json(&r_pass), "after": snap_json(&a_pass)},
            "nextFail": {"sequence": seq_f, "tx": tx_json(&r_fail), "error": err_of(&r_fail), "adaptorErrorName": code_f.map(err_name)},
            "before": snap_json(&b), "after": snap_json(&a)}));

        // (c) astra's case on a book-consistent state: allocate 1_000_000_000 for
        // real (receipt2 == 1e9 with tv following), stage 500_000_000 of the swept
        // EBG2 into holding, donate 1 raw, withdraw 500_000_000 at nav 499_000_000
        let mut clone = post_v1.clone();
        // Seed enough idle through the actual Voltr deposit path before the
        // 1e9 allocation. A balance override on IDLE_ATA would create cash
        // outside totalValue and make the subsequent Voltr accounting fail
        // with 6004, which is a harness fixture error rather than the astra
        // counterexample.
        let seed = 1_000_000_000u64;
        let admin_usdc = ata_for(admin, &usdc);
        set_token_account(&mut clone, admin_usdc, usdc, *admin, seed);
        let r_seed = send(
            &mut clone,
            &[
                cu_ix(),
                create_ata_idempotent_ix(admin, admin, &usdc),
                deposit_vault_ix(admin, &admin_usdc, &key(ADMIN_LP_ATA), seed),
            ],
            admin,
        );
        let a_seed = snap(&clone, Some(s2));
        let (r_alloc, seq_a) = crank_deposit(&mut clone, s2, 1_000_000_000, 1_000_000_000, true, INTERVAL_GAP, INTERVAL_SECS);
        let a_alloc = snap(&clone, Some(s2));
        let r_stage = send(&mut clone, &[transfer_checked_ix(&squads_ata, &s2.holding, &squads_vault, 500_000_000)], &squads_vault);
        set_token_account(&mut clone, s2.custody, usdc, s2.auth, 1);
        let b = snap(&clone, Some(s2));
        let (r, seq) = crank_withdraw(&mut clone, s2, 500_000_000, 499_000_000, true, INTERVAL_GAP, INTERVAL_SECS);
        let a = snap(&clone, Some(s2));
        let step_c = V3_STEP_FLOOR.max(1_000_000_000u64 * V3_MAX_STEP_BPS / 10_000);
        let mut c = vec![];
        chk(
            &mut c,
            "V14c admin deposit 1_000_000_000 seeds enough accounted idle",
            r_seed.is_ok() && a_seed.tv == post_v1_snap.tv + seed && a_seed.idle == post_v1_snap.idle + seed && a_seed.book_ok(),
        );
        chk(
            &mut c,
            "V14c allocate 1_000_000_000 (nav 1e9) executed: receipt2 == 1e9 and the book is consistent",
            r_alloc.is_ok() && a_alloc.receipt2 == Some(1_000_000_000) && a_alloc.book_ok(),
        );
        chk(
            &mut c,
            "V14c staging 500_000_000 EBG2 -> holding and 1 raw donated to custody2",
            r_stage.is_ok() && b.holding2 == Some(500_000_000) && b.custody2 == Some(1),
        );
        chk(&mut c, "V14c withdraw(500_000_000, nav 499_000_000) with the donation SUCCEEDS", r.is_ok());
        chk(&mut c, "V14c 1 raw swept to EBG2, custody2 0", a.custody2 == Some(0) && a.squads_usdc == b.squads_usdc + 1);
        chk(&mut c, "V14c exact pull: holding 0, idle +500_000_000", a.holding2 == Some(0) && a.idle == b.idle + 500_000_000);
        chk(&mut c, "V14c receipt2 == 499_000_000 (the reported loss is booked)", a.receipt2 == Some(499_000_000));
        chk(&mut c, "V14c book identity holds", a.book_ok());
        chk(&mut c, "V14c D conserved (the donation stays outside the Voltr book)", a.gap() == b.gap() && a.gap() == post_v1_snap.gap());
        rec.push("V14c-donation-withdraw", "V14c astra case: position 1e9 (really allocated), holding 5e8, withdraw 5e8 reporting nav 499_000_000 with 1 raw donated before execution — passes; the bound |(499e6 + 500e6) - 1e9| = 1_000_000 sits inside step = max(1e9*500/10_000, 1_000_000)", &c, json!({
            "donatedRaw": 1u64, "positionValue": 1_000_000_000u64, "withdrawAmount": 500_000_000u64,
            "reportedNav": 499_000_000u64, "stepAtPosition1e9": step_c,
            "seed": {"amount": seed, "tx": tx_json(&r_seed), "after": snap_json(&a_seed)},
            "allocate": {"sequence": seq_a, "tx": tx_json(&r_alloc), "after": snap_json(&a_alloc)},
            "sequence": seq, "tx": tx_json(&r), "error": err_of(&r),
            "before": snap_json(&b), "after": snap_json(&a)}));
    }

    // ---------------------------------------------------------------- V15 tracked-custody guard (v3.3)
    {
        let mut c = vec![];
        let poison = |svm: &mut LiteSVM, value: u64| patch_receipt_tracked(svm, &s2.receipt, value);
        let mut clone = post_v1.clone();
        poison(&mut clone, 42);
        let tracked = receipt_tracked_custody(&clone, &s2.receipt);
        let (r, seq) = crank_deposit(&mut clone, s2, 0, 500_000, true, INTERVAL_GAP, INTERVAL_SECS);
        let code = custom_code(&r);
        chk(&mut c, "V15 receipt2 bytes 128..136 overwritten with 42", tracked == Some(42));
        chk(
            &mut c,
            format!("V15 refresh with tracked custody 42 rejected with adaptor error 22 TrackedCustodyNonZero (got {})", code.map(err_name).unwrap_or("none")),
            r.is_err() && code == Some(22),
        );
        let mut clone2 = post_v1.clone();
        poison(&mut clone2, 42);
        let (r2, seq2) = crank_withdraw(&mut clone2, s2, alloc, 0, true, INTERVAL_GAP, INTERVAL_SECS);
        let code2 = custom_code(&r2);
        chk(
            &mut c,
            format!("V15 withdraw path equally gated (got {})", code2.map(err_name).unwrap_or("none")),
            r2.is_err() && code2 == Some(22),
        );
        poison(&mut clone, 0);
        let restored = receipt_tracked_custody(&clone, &s2.receipt);
        let (r3, seq3) = crank_deposit(&mut clone, s2, 0, 500_000, true, INTERVAL_GAP, INTERVAL_SECS);
        let a3 = snap(&clone, Some(s2));
        chk(&mut c, "V15 tracked custody restored to 0", restored == Some(0));
        chk(&mut c, "V15 refresh executes again after the restore", r3.is_ok() && a3.book_ok());
        rec.push("V15-tracked-custody-guard", "V15 tracked-custody guard: receipt2 bytes 128..136 = 42 -> error 22 on both the deposit and withdraw capital paths; restored to 0 -> executes again", &c, json!({
            "depositPoisoned": {"sequence": seq, "tx": tx_json(&r), "error": err_of(&r), "adaptorErrorName": code.map(err_name)},
            "withdrawPoisoned": {"sequence": seq2, "tx": tx_json(&r2), "error": err_of(&r2), "adaptorErrorName": code2.map(err_name)},
            "restored": {"sequence": seq3, "tx": tx_json(&r3), "after": snap_json(&a3)}}));
    }

    // ---------------------------------------------------------------- V15b resized receipt (v3.3)
    for len in [191usize, 193] {
        let mut clone = post_v1.clone();
        let mut a = clone.get_account(&s2.receipt).expect("receipt2").clone();
        a.data.resize(len, 0);
        clone.set_account(s2.receipt, a).unwrap();
        let before = snap(&clone, Some(s2));
        let (r, seq) = crank_deposit(&mut clone, s2, 0, 500_000, true, INTERVAL_GAP, INTERVAL_SECS);
        let after = snap(&clone, Some(s2));
        let code = custom_code(&r);
        let err = err_of(&r);
        let loader_panic = match &r {
            Err((_, logs)) => logs.iter().any(|line| {
                line.contains("src/accounts/account_loader.rs")
                    && line.contains("range end index 192 out of range for slice of length 191")
            }),
            Ok(_) => false,
        };
        let mut c = vec![];
        if len == 191 {
            // observed on this binary: VOLTR's own account loader slices the
            // strategy receipt at a fixed 192 bytes and the SBF program aborts
            // before the adaptor is ever CPI'd — the path still fails closed.
            chk(
                &mut c,
                "V15b receipt2 at 191 bytes fails CLOSED (Voltr's fixed-192 account loader aborts before the adaptor CPI)",
                r.is_err() && loader_panic,
            );
        } else {
            chk(
                &mut c,
                format!("V15b receipt2 at 193 bytes -> adaptor InvalidAccount 2 (got {})", code.map(err_name).unwrap_or("none")),
                r.is_err() && code == Some(2),
            );
        }
        rec.push(
            "V15b-resized-receipt",
            "V15b resized strategy receipt: 191 B aborts in Voltr's fixed-192 account loader (fails closed), 193 B -> adaptor InvalidAccount 2",
            &c,
            json!({
                "bytes": len, "sequence": seq, "before": snap_json(&before),
                "after": snap_json(&after), "tx": tx_json(&r), "error": err,
                "adaptorErrorName": code.map(err_name),
                "failureOwner": if len == 191 { "Voltr loader" } else { "adaptor" },
                "adaptorReached": len != 191,
                "loaderPanicObserved": loader_panic}),
        );
    }
}

// =========================================================================================
/// V12 + V13 on pristine base clones with the v3.1 ELF swapped in: a pre-funded
/// ticket PDA must not brick initialize_report_ticket (V12), and the adaptor's
/// settings-graph check must tolerate Squads governance hardening while still
/// failing closed on a weakened pinned signer (V13).
fn robustness_cases(base: &Base, rec: &mut Rec) {
    // ---------------------------------------------------------------- V12 prefunded ticket
    for (label, lamports) in [("1 lamport", 1u64), ("1 SOL", 1_000_000_000u64)] {
        let mut clone = base.svm.clone();
        let _ = swap_adaptor_v3(&mut clone);
        let s = strat_for(Pubkey::new_unique());
        let payer = new_funded(&mut clone);
        let r_cfg = send(&mut clone, &[cu_ix(), adaptor_initialize_config_v3_ix(&payer, &s)], &payer);
        fund(&mut clone, &s.ticket, lamports);
        let pre = clone.get_account(&s.ticket);
        let r_tk = send(&mut clone, &[cu_ix(), adaptor_initialize_report_ticket_ix(&payer, &s)], &payer);
        let tk = clone.get_account(&s.ticket);
        let mut c = vec![];
        chk(&mut c, "V12 initialize_config executed", r_cfg.is_ok());
        chk(
            &mut c,
            "V12 ticket PDA pre-funded and still system-owned with no data",
            pre.map(|a| a.owner == key(SYS) && a.data.is_empty() && a.lamports == lamports).unwrap_or(false),
        );
        chk(&mut c, format!("V12 initialize_report_ticket executed with {label} pre-staged"), r_tk.is_ok());
        chk(
            &mut c,
            "V12 ticket is 104 bytes owned by FSj27…",
            tk.as_ref().map(|a| a.owner == key(ADAPTOR) && a.data.len() == 104).unwrap_or(false),
        );
        chk(
            &mut c,
            if lamports == 1 { "V12 payer topped the ticket up (1 lamport grew to rent)" } else { "V12 over-funded ticket left untouched (exactly 1 SOL, no transfer)" },
            tk.as_ref().map(|a| if lamports == 1 { a.lamports > 1 } else { a.lamports == 1_000_000_000 }).unwrap_or(false),
        );
        rec.push(
            "V12-prefunded-ticket",
            "V12 prefunded-ticket griefing: lamports on the ticket PDA before initialize_report_ticket",
            &c,
            json!({
                "prefundedLamports": lamports, "configTx": tx_json(&r_cfg), "ticketTx": tx_json(&r_tk),
                "ticket": s.ticket.to_string(),
                "ticketAfter": tk.as_ref().map(|a| json!({"lamports": a.lamports, "bytes": a.data.len(), "owner": a.owner.to_string()})),
            }),
        );
    }

    // ---------------------------------------------------------------- V13 settings graph
    for (label, relax) in [
        ("relaxed: second member (mask 7) added and threshold raised to 2, BAqg kept at mask 7", true),
        ("weakened: BAqg permission mask 7 -> 6", false),
    ] {
        let mut clone = base.svm.clone();
        let _ = swap_adaptor_v3(&mut clone);
        let s = strat_for(Pubkey::new_unique());
        let payer = new_funded(&mut clone);
        let r_cfg = send(&mut clone, &[cu_ix(), adaptor_initialize_config_v3_ix(&payer, &s)], &payer);
        let r_tk = send(&mut clone, &[cu_ix(), adaptor_initialize_report_ticket_ix(&payer, &s)], &payer);
        let r_tx3 = send(
            &mut clone,
            &[
                cu_ix(),
                create_ata_idempotent_ix(&payer, &s.auth, &key(USDC)),
                create_ata_idempotent_ix(&payer, &s.holding_auth, &key(USDC)),
                voltr_initialize_strategy_ix(&payer, &s),
            ],
            &payer,
        );
        let boot = snap(&clone, Some(&s));
        let member_off = mutate_settings(&mut clone, relax);
        let b = snap(&clone, Some(&s));
        let (r, seq) = crank_deposit(&mut clone, &s, 0, 1_000, true, INTERVAL_GAP, INTERVAL_SECS);
        let a = snap(&clone, Some(&s));
        let code = custom_code(&r);
        let mut c = vec![];
        chk(
            &mut c,
            "V13 bootstrap (config, ticket, ATAs, initializeStrategy) executed on the unmutated settings",
            r_cfg.is_ok() && r_tk.is_ok() && r_tx3.is_ok() && boot.receipt2 == Some(0),
        );
        chk(&mut c, format!("V13 settings mutated locally (BAqg member at offset {member_off})"), true);
        if relax {
            // The pristine base still carries live strategy-one state, so the
            // strategy-two-only book_ok does not apply here; assert what the
            // refresh itself must do: credit receipt2 and leave custody empty.
            chk(
                &mut c,
                "V13 refresh executes with a second mask-7 member and threshold 2 (governance hardening tolerated)",
                r.is_ok() && a.receipt2 == Some(1_000) && a.custody2 == Some(0) && a.tv == b.tv + 1_000,
            );
        } else {
            chk(
                &mut c,
                format!("V13 refresh with BAqg mask 6 rejected (code {})", code.map(err_name).unwrap_or("none")),
                r.is_err() && code == Some(5),
            );
            chk(&mut c, "V13 books untouched by the rejected refresh", a.tv == b.tv && a.receipt2 == b.receipt2);
        }
        rec.push("V13-settings-graph", "V13 settings-graph relaxation: the adaptor only requires the pinned signer present with mask 7", &c, json!({
            "mutation": label, "baqgMemberOffset": member_off,
            "bootstrap": {"config": tx_json(&r_cfg), "ticket": tx_json(&r_tk), "initializeStrategy": tx_json(&r_tx3)},
            "refresh": {"sequence": seq, "tx": tx_json(&r), "error": err_of(&r), "adaptorErrorName": code.map(err_name)},
            "before": snap_json(&b), "after": snap_json(&a),
        }));
    }
}

/// Rewrite the Squads Settings account bytes in place. The adaptor's reader
/// (valid_settings_authority_graph) walks: discriminator 0..8, reserved zeros
/// 24..56, threshold u16 56..58, authority tag at 78 (0 -> member count u32 at
/// 88 and members from 92; 1 -> count at 120 and members from 124), each member
/// 32 pubkey bytes + 1 permission byte. The Squads program itself is never
/// invoked in this harness, so the mutation only exercises the adaptor's check.
fn mutate_settings(svm: &mut LiteSVM, relax: bool) -> usize {
    let fixture = load_fixture(SETTINGS);
    let mut data = STANDARD.decode(fixture["dataBase64"].as_str().unwrap()).unwrap();
    let baqg = key(ADMIN);
    let off = data
        .windows(32)
        .position(|w| w == baqg.as_ref())
        .expect("BAqg must appear in the settings fixture member list");
    let (count_off, members_off) = match data[78] {
        0 => (88usize, 92usize),
        1 => (120, 124),
        t => panic!("unexpected settings authority tag {t}"),
    };
    assert_eq!(off, members_off, "BAqg should be the first settings member");
    if relax {
        let count = u32::from_le_bytes(data[count_off..count_off + 4].try_into().unwrap());
        data[count_off..count_off + 4].copy_from_slice(&(count + 1).to_le_bytes());
        data[56..58].copy_from_slice(&2u16.to_le_bytes()); // threshold 1 -> 2
        data.extend_from_slice(Pubkey::new_unique().as_ref());
        data.push(7);
    } else {
        data[off + 32] = 6; // mask 7 -> 6 on the pinned signer
    }
    svm.set_account(
        key(SETTINGS),
        Account {
            lamports: fixture["lamports"].as_u64().unwrap(),
            data,
            owner: key(fixture["owner"].as_str().unwrap()),
            executable: false,
            rent_epoch: 0,
        },
    )
    .unwrap();
    off
}

// =========================================================================================
fn run_matrix(write_json: bool) {
    let (mut svm, s2, admin, mut rec, base, s_init, post_v1) = main_line(true);
    assert!(
        snap(&svm, Some(&s2)).receipt2 == Some(0),
        "the main line must land post-V3 (receipt2 restored to 0, custody2 0) before the matrix; \
         check the ELF sha in V3_ELF_SHA256 and the V1-V3 step JSONs above"
    );

    // ---------------------------------------------------------------- V10 user lifecycle post-restore
    {
        let before = snap(&svm, Some(&s2));
        let (u, r_dep, minted) = user_deposit(&mut svm, 1_000_000);
        let after_dep = snap(&svm, Some(&s2));
        let expected_lp = (1_000_000u128 * before.lp_incl_fees() as u128 / before.tv.max(1) as u128) as u64;
        let (r_req, r_claim, payout) = user_exit(&mut svm, &u);
        let after = snap(&svm, Some(&s2));
        let round_trip_bps = (payout as u128 * 10_000 / 1_000_000u128) as u64;
        let fair_redeem =
            (minted as u128 * after_dep.tv.max(1) as u128 / after_dep.lp_incl_fees().max(1) as u128) as u64;
        let mut c = vec![];
        chk(&mut c, "V10 fresh user deposit_vault(1_000_000) executed", r_dep.is_ok());
        chk(&mut c, format!("V10 LP minted == floor(amount * lpInclFees / tv) == {expected_lp}"), minted == expected_lp);
        chk(&mut c, "V10 requestWithdrawVault(all) executed", r_req.is_ok());
        chk(&mut c, "V10 withdrawVault executed after the 600 s wait", r_claim.is_ok());
        // With the full reset sequence as the base (degradation 0 + perf fee 0
        // before the repair, then harvest -> cancel -> request -> 600 s ->
        // claim), the vault carries no locked-profit accumulator and no pending
        // fee LP when the user deposits, so the round trip is fair: payout is
        // 1_000_000 minus the mint's floor dust (about 1 raw). The v3.1 run's
        // 827,489 payout was the T10 NAV-sniper haircut: that base skipped
        // P1.1, so the repair's +3,793,536 book jump sat in the 86_400 s
        // locked-profit unlock and the redeemer was paid on the degraded NAV.
        chk(
            &mut c,
            format!("V10 payout == 1_000_000 +/- 2 raw on the clean base (got {payout}; round trip {round_trip_bps} bps)"),
            (999_997..=1_000_001).contains(&payout),
        );
        chk(&mut c, "V10 tv == idle throughout (custody2 0, receipt2 0)", before.book_ok() && after_dep.book_ok() && after.book_ok() && after.receipt2 == Some(0) && after.custody2 == Some(0));
        rec.push("V10-user-lifecycle", "V10 user lifecycle post-restore: fresh user deposit 1_000_000 -> request all -> 600 s -> claim", &c, json!({
            "user": u.key.to_string(), "lpMinted": minted, "expectedLp": expected_lp,
            "deposit": tx_json(&r_dep), "request": tx_json(&r_req), "claim": tx_json(&r_claim), "payoutRaw": payout,
            "roundTripBps": round_trip_bps, "payoutFairValueInclFees": fair_redeem,
            "before": snap_json(&before), "afterDeposit": snap_json(&after_dep), "after": snap_json(&after)}));
    }

    // ---------------------------------------------------------------- V4..V9, V11
    extra_cases(&mut svm, &post_v1, &s2, &admin, &mut rec);

    // ---------------------------------------------------------------- V12, V13 (pristine base clones)
    robustness_cases(&base, &mut rec);

    let final_state = snap(&svm, Some(&s2));
    {
        let mut c = vec![];
        chk(&mut c, format!("D conserved end to end ({} -> {})", s_init.gap(), final_state.gap()), s_init.gap() == final_state.gap());
        chk(&mut c, "final book identity tv == idle + custody2 + receipt2", final_state.book_ok());
        chk(&mut c, "final: strategy-2 custody2 and holding are zero", final_state.custody2 == Some(0) && final_state.holding2 == Some(0));
        rec.push("FINAL-invariants", "end-to-end invariants on the main-line instance", &c, json!({
            "initial": snap_json(&s_init), "final": snap_json(&final_state)}));
    }

    let manifest: Value =
        serde_json::from_slice(&fs::read(fixtures_dir().join("_manifest.json")).unwrap()).unwrap();
    let report = json!({
        "schema": "voltr-adaptor-v3-litesvm/v1",
        "generatedBy": "crates/squads-test-harness/tests/voltr_adaptor_v3_matrix.rs",
        "broadcast": false,
        "signatureProof": false,
        "squadsPolicyExecutionProof": false,
        "cluster": "mainnet-beta (accounts + program binaries cloned read-only)",
        "dumpSlot": base.slot0,
        "dumpClockUnixTimestamp": base.ts0,
        "dumpGenesis": manifest["genesis"],
        "programs": {"voltr": VOLTR, "adaptorUnderTest": ADAPTOR, "squadsSmartAccount": SQUADS},
        "voltrProgramDataDeployedSlot": u64_le(&fixture_data("3fiAyUjktZkZf6hcbBPy6U6UdkMdEFoToS4sjtzAd5az"), 4),
        "adaptorV2ProgramDataDeployedSlot": u64_le(&fixture_data("DrvzixaVmAuPVVJPtP5wykb9mvgDWqZbvZau9oiCUpHu"), 4),
        "adaptorV3Elf": {"file": "crates/squads-test-harness/fixtures/voltr-repair/adaptor-v3.so",
            "sha256": V3_ELF_SHA256, "bytes": V3_ELF_BYTES},
        "vault": VAULT,
        "vaultAdmin": ADMIN,
        "vaultManager": SQUADS_VAULT,
        "strategy1": strat_json(&strat1()),
        "strategy2": strat_json(&s2),
        "v3Config": {"vaultIndex": V3_VAULT_INDEX, "maxReportNavRaw": V3_MAX_NAV, "maxReportAgeSlots": V3_MAX_AGE,
            "maxStepBps": V3_MAX_STEP_BPS, "stepFloorRaw": V3_STEP_FLOOR, "minReportIntervalSlots": V3_MIN_INTERVAL},
        "initialState": snap_json(&s_init),
        "finalState": snap_json(&final_state),
        "overallVerdict": if rec.all_ok { "PASS" } else { "FAIL" },
        "signerOverridesAndCaveats": [
            "LiteSVM built with with_sigverify(false) + with_blockhash_check(false); all transactions carry default (zero) signatures. Nothing was signed or broadcast.",
            "Vault admin BAqg… is marked a transaction signer directly for the seed deposit_vault and the manager round-trip updateVaultConfig legs; its lamports are airdropped locally.",
            "The Squads vault PDA ST999… (= vault.manager) is marked a transaction signer directly for arm_report + capital cranks, initializeStrategy, and the EBG2 -> holding SPL transfers. On mainnet these run through the Squads smart-account program's policies.",
            "The strategy-two config key is a fresh Pubkey::new_unique() acting as the initialize_config keypair signer (on mainnet: a fresh keypair).",
            "Token-balance overrides only (no program/authority/policy changes): V2 injects 100 raw dust into custody2; V4 tops the Squads USDC ATA up by 100_000 before staging 600_000 into holding; V5 injects 100 raw dust into custody2; M3/V10 give wallets USDC balances.",
            "The adaptor program account FSj27… is swapped from the mainnet v2 ELF to the v3 ELF (fixtures/voltr-repair/adaptor-v3.so) after the M0 repair; Voltr and Squads remain the deployed mainnet binaries.",
            "Custody residues, dust and holding over-staging model third-party transfers that LiteSVM cannot produce otherwise.",
            "V12 prefunds the strategy-two ticket PDA (1 lamport / 1 SOL) before initialize_report_ticket; V13 rewrites the Squads Settings account bytes locally (extra mask-7 member + threshold 2, or BAqg mask 6) — the Squads program itself is never invoked, so only the adaptor's settings-graph check sees the mutation.",
            "The matrix base is now the full proven reset sequence (P0 degradation/perf-fee 0 -> repair -> harvest -> cancel/request/claim to tv == idle == 23) before the v3.3 ELF swap; V10's payout is therefore fair (1_000_000 +/- 2 raw). The earlier 827,489 payout was the T10 NAV-sniper haircut from skipping P1.1, not a Voltr redeem asymmetry.",
            "v3.3 semantics under test: the step bound prices only Voltr-explained flows (deposit inflow = amount, refresh inflow = 0, withdraw outflow = amount); custody residue is swept whole to Squads and is unreported until the next report (V5, V14); the withdraw path sweeps-then-pulls with a post-pull custody == amount assertion (error 23); receipt bytes 128..136 must be zero or every capital path fails error 22 (V15); a resized receipt fails closed (191 B aborts in Voltr's fixed-192 account loader before the CPI, 193 B -> adaptor InvalidAccount 2) (V15b).",
        ],
        "steps": rec.steps,
    });
    eprintln!("final state: {}", serde_json::to_string_pretty(&report["finalState"]).unwrap());
    if write_json {
        let out = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
            .join("../../docs/evidence/voltr-adaptor-v3-litesvm-2026-09-08.results.json");
        fs::write(&out, serde_json::to_string_pretty(&report).unwrap()).unwrap();
        eprintln!("wrote {}", out.display());
    }
    assert!(rec.all_ok, "one or more matrix steps failed; see the step JSON above");
}

#[test]
#[ignore = "clones deployed mainnet programs+accounts into LiteSVM; run explicitly with --ignored"]
fn voltr_adaptor_v3_phase1() {
    // Phase 1 only: ELF load + repair + bootstrap + V1..V3 (asserts live in main_line()).
    let (_, _, _, rec, _, _, _) = main_line(true);
    let summary = rec.steps.iter().map(|s| format!("{} {}", s["id"], s["verdict"])).collect::<Vec<_>>().join(" | ");
    eprintln!("PHASE 1 (v3 ELF sha {}…): {summary}", &V3_ELF_SHA256[..12]);
    assert!(rec.all_ok, "phase 1 failed");
}

/// Diagnostic: is the v3 12-account capital wire executable at all, and which
/// account privileges does it require? Calls the adaptor DIRECTLY (no Voltr)
/// with the strategy authority PDA marked signer, on clones of the post-seed
/// state, over meta variants of the three v3 tail accounts.
///
/// Observed on v3.0 (sha 0063193490e1a26d…, 2026-09-08): only
/// "holding w / auth ro / receipt ro" executed; every variant that made the
/// receipt (capital account 11) or the holding auth writable, or the holding
/// read-only, failed with AdaptorError::InvalidAccount (2), while Voltr's
/// capital instruction always hands the CPI a writable receipt — the phase-1
/// blocker. On v3.1 (sha 14073d33…) a writable receipt at index 11 is
/// accepted, so the receipt-writable rows execute. Writes a separate
/// diagnostic JSON; the authoritative results JSON comes from the full matrix.
#[test]
#[ignore = "clones deployed mainnet programs+accounts into LiteSVM; run explicitly with --ignored"]
fn voltr_adaptor_v3_cpi_probe() {
    let (svm, s2, _, rec, base, s_post, _) = main_line(false);
    eprintln!("post-seed: {}", serde_json::to_string_pretty(&snap_json(&s_post)).unwrap());
    let mut direct_rows: Vec<Value> = vec![];
    let variants: [(&str, bool, bool, bool); 5] = [
        ("holding w / auth ro / receipt ro", true, false, false),
        ("holding w / auth ro / receipt w", true, false, true),
        ("holding w / auth w  / receipt ro", true, true, false),
        ("holding w / auth w  / receipt w", true, true, true),
        ("holding ro / auth ro / receipt ro", false, false, false),
    ];
    for (label, hw, aw, rw) in variants {
        let mut clone = svm.clone();
        advance_time(&mut clone, INTERVAL_GAP, INTERVAL_SECS);
        let clock: Clock = clone.get_sysvar();
        let seq = clock.slot;
        let payer = new_funded(&mut clone);
        let r_arm = send(&mut clone, &[cu_ix(), arm_report_ix(&s2, 0, 0, seq, 0)], &payer);
        let ix = Instruction {
            program_id: key(ADAPTOR),
            accounts: vec![
                m(s2.auth, true, true),
                m(s2.config, false, false),
                meta(false, true, USDC),
                m(s2.custody, false, true),
                meta(false, false, TOKEN),
                meta(false, false, SETTINGS),
                meta(false, false, SQUADS_VAULT),
                meta(false, true, SQUADS_USDC_ATA),
                m(s2.ticket, false, true),
                m(s2.holding, false, hw),
                m(s2.holding_auth, false, aw),
                m(s2.receipt, false, rw),
            ],
            data: adaptor_capital_wire(ADAPTOR_DEPOSIT, 0, seq, 0),
        };
        let r = send(&mut clone, &[cu_ix(), ix], &payer);
        let row = json!({
            "tailMetas": label, "armOk": r_arm.is_ok(), "callOk": r.is_ok(),
            "error": err_of(&r), "adaptorErrorName": custom_code(&r).map(err_name),
        });
        eprintln!("DIRECT {row}");
        direct_rows.push(row);
    }

    // ---- how does Voltr forward the v3 tail? permute the tail of the VOLTR ix
    let fixed: Vec<AccountMeta> = vec![
        meta(true, false, SQUADS_VAULT),
        meta(false, false, PROTOCOL),
        meta(false, true, VAULT),
        m(s2.config, false, false),
        meta(false, false, ADAPTOR_ADD_RECEIPT),
        m(s2.receipt, false, true),
        meta(false, true, IDLE_AUTH),
        m(s2.auth, false, true),
        meta(false, true, USDC),
        meta(false, false, LP_MINT),
        meta(false, true, IDLE_ATA),
        m(s2.custody, false, true),
        meta(false, false, TOKEN),
        meta(false, false, ADAPTOR),
    ];
    let voltr_variants: Vec<(&str, Vec<AccountMeta>)> = vec![
        ("A 7-tail as designed (settings,vault,ata,ticket,holding w,auth ro,receipt ro)", v3_tail(&s2)),
        ("B same but receipt WRITABLE in the tail", vec![
            meta(false, false, SETTINGS),
            meta(true, false, SQUADS_VAULT),
            meta(false, true, SQUADS_USDC_ATA),
            m(s2.ticket, false, true),
            m(s2.holding, false, true),
            m(s2.holding_auth, false, false),
            m(s2.receipt, false, true),
        ]),
        ("C 6-tail WITHOUT the receipt", vec![
            meta(false, false, SETTINGS),
            meta(true, false, SQUADS_VAULT),
            meta(false, true, SQUADS_USDC_ATA),
            m(s2.ticket, false, true),
            m(s2.holding, false, true),
            m(s2.holding_auth, false, false),
        ]),
        ("D receipt BEFORE holding (ro)", vec![
            meta(false, false, SETTINGS),
            meta(true, false, SQUADS_VAULT),
            meta(false, true, SQUADS_USDC_ATA),
            m(s2.ticket, false, true),
            m(s2.receipt, false, false),
            m(s2.holding, false, true),
            m(s2.holding_auth, false, false),
        ]),
        ("E holding auth WRITABLE in the tail", vec![
            meta(false, false, SETTINGS),
            meta(true, false, SQUADS_VAULT),
            meta(false, true, SQUADS_USDC_ATA),
            m(s2.ticket, false, true),
            m(s2.holding, false, true),
            m(s2.holding_auth, false, true),
            m(s2.receipt, false, false),
        ]),
    ];
    // control: the receipt READ-ONLY in Voltr's own fixed accounts (index 5) —
    // does the CPI privilege flag follow Voltr's fixed-account metadata?
    let mut fixed_ro_receipt = fixed.clone();
    fixed_ro_receipt[5] = m(s2.receipt, false, false);
    let mut accounts = fixed_ro_receipt.clone();
    accounts.extend(v3_tail(&s2));
    let mut voltr_rows: Vec<Value> = vec![];
    {
        let mut clone = svm.clone();
        advance_time(&mut clone, INTERVAL_GAP, INTERVAL_SECS);
        let clock: Clock = clone.get_sysvar();
        let seq = clock.slot;
        let payer = new_funded(&mut clone);
        let dep = Instruction {
            program_id: key(VOLTR),
            accounts,
            data: voltr_capital_data(VOLTR_DEPOSIT_STRATEGY, 0, ADAPTOR_DEPOSIT, seq, 0),
        };
        let r = send(&mut clone, &[cu_ix(), arm_report_ix(&s2, 0, 0, seq, 0), dep], &payer);
        let row = json!({
            "variant": "F receipt READ-ONLY in Voltr fixed accounts + 7-tail as designed",
            "ok": r.is_ok(), "error": err_of(&r),
            "errorName": if custom_code(&r) == Some(2000) { Some("Voltr custom error 2000 (unnamed; fires only when the receipt is read-only in Voltr's own fixed accounts)") } else { custom_code(&r).map(err_name) },
        });
        eprintln!("VOLTR {row}");
        voltr_rows.push(row);
    }
    for (label, tail) in voltr_variants {
        let mut clone = svm.clone();
        advance_time(&mut clone, INTERVAL_GAP, INTERVAL_SECS);
        let clock: Clock = clone.get_sysvar();
        let seq = clock.slot;
        let payer = new_funded(&mut clone);
        let mut accounts = fixed.clone();
        accounts.extend(tail);
        let dep = Instruction {
            program_id: key(VOLTR),
            accounts,
            data: voltr_capital_data(VOLTR_DEPOSIT_STRATEGY, 0, ADAPTOR_DEPOSIT, seq, 0),
        };
        let r = send(&mut clone, &[cu_ix(), arm_report_ix(&s2, 0, 0, seq, 0), dep], &payer);
        let row = json!({
            "variant": label, "ok": r.is_ok(), "error": err_of(&r),
            "adaptorErrorName": custom_code(&r).map(err_name),
        });
        eprintln!("VOLTR {row}");
        voltr_rows.push(row);
    }

    // ---- V1 failure evidence: the designed Voltr-path allocate crank
    let v1_fail = {
        let mut clone = svm.clone();
        let alloc = 500_000u64;
        let before = snap(&clone, Some(&s2));
        let (r, seq) = crank_deposit(&mut clone, &s2, alloc, alloc, true, INTERVAL_GAP, INTERVAL_SECS);
        json!({
            "case": "V1 allocate: arm + deposit_strategy(500_000, nav 500_000) on the v3 adaptor through Voltr",
            "sequence": seq, "tx": tx_json(&r), "error": err_of(&r),
            "adaptorErrorName": custom_code(&r).map(err_name),
            "before": snap_json(&before), "after": snap_json(&snap(&clone, Some(&s2))),
        })
    };
    let out = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../docs/evidence/voltr-adaptor-v3-cpi-probe-2026-09-08.json");
    let report = json!({
        "schema": "voltr-adaptor-v3-litesvm/v1",
        "generatedBy": "crates/squads-test-harness/tests/voltr_adaptor_v3_matrix.rs",
        "broadcast": false,
        "signatureProof": false,
        "cluster": "mainnet-beta (accounts + program binaries cloned read-only)",
        "dumpSlot": base.slot0,
        "dumpGenesis": serde_json::from_slice::<Value>(&fs::read(fixtures_dir().join("_manifest.json")).unwrap()).unwrap()["genesis"],
        "programs": {"voltr": VOLTR, "adaptorUnderTest": ADAPTOR, "squadsSmartAccount": SQUADS},
        "voltrProgramDataDeployedSlot": u64_le(&fixture_data("3fiAyUjktZkZf6hcbBPy6U6UdkMdEFoToS4sjtzAd5az"), 4),
        "adaptorV2ProgramDataDeployedSlot": u64_le(&fixture_data("DrvzixaVmAuPVVJPtP5wykb9mvgDWqZbvZau9oiCUpHu"), 4),
        "adaptorV3Elf": {"file": "crates/squads-test-harness/fixtures/voltr-repair/adaptor-v3.so",
            "sha256": V3_ELF_SHA256, "bytes": V3_ELF_BYTES},
        "overallVerdict": "DIAGNOSTIC — receipt-privilege probe; on v3.1 the Voltr capital path executes (authoritative results: voltr-adaptor-v3-litesvm-2026-09-08.results.json)",
        "blocker": "HISTORY (v3.0, sha 0063193490e1a26d…): every Voltr-path capital call failed AdaptorError::InvalidAccount (custom error 2) at the adaptor CPI because Voltr's capital instruction hands the CPI the strategy-init receipt (adaptor capital account 11) WRITABLE, and v3.0 required that account read-only. Probe C proved verbatim tail forwarding (6-tail -> InvalidInstruction 0); probe F (receipt read-only in Voltr's own fixed accounts) removed the adaptor error and surfaced Voltr custom error 2000. v3.1 (sha 14073d33…) accepts a writable receipt at index 11 (only a signer is rejected), so the receipt-writable rows now execute; this probe is kept as the recorded demonstration and runs against whatever ELF V3_ELF_SHA256 pins.",
        "managerRoundTripNote": "The task specified a manager round-trip (updateVaultConfig Manager=BAqg -> initializeStrategy -> Manager=ST999) inside tx 3. A probe of updateVaultConfig fields 0..=8 with 32-byte values was rejected by Voltr for every field (BorshIoError(\"Unknown\") for 0-5 and 8, ProgramFailedToComplete for 6-7), so no Manager field could be identified and the round-trip was NOT performed. Voltr initializeStrategy succeeded with vault.manager = ST999 unchanged (tx 3 = custody ATA create + holding ATA create + initializeStrategy), i.e. under direct-signer LiteSVM the round-trip is not required; whether the real Squads-signed flow needs it is NOT proven here.",
        "v3Config": {"vaultIndex": V3_VAULT_INDEX, "maxReportNavRaw": V3_MAX_NAV, "maxReportAgeSlots": V3_MAX_AGE,
            "maxStepBps": V3_MAX_STEP_BPS, "stepFloorRaw": V3_STEP_FLOOR, "minReportIntervalSlots": V3_MIN_INTERVAL},
        "strategy2": strat_json(&s2),
        "postSeedState": snap_json(&s_post),
        "mainLineStepsThroughSeed": rec.steps,
        "v1Crank": v1_fail,
        "probeDirectCallNoVoltr": direct_rows,
        "probeVoltrPathVariants": voltr_rows,
        "signerOverridesAndCaveats": [
            "LiteSVM built with with_sigverify(false) + with_blockhash_check(false); all transactions carry default (zero) signatures. Nothing was signed or broadcast.",
            "The Squads vault PDA ST999… (= vault.manager) and the vault admin BAqg… are marked transaction signers directly; the strategy-two config key is a fresh Pubkey::new_unique() acting as the initialize_config keypair signer.",
            "The adaptor program account FSj27… is swapped from the mainnet v2 ELF to the v3 ELF after the M0 repair; Voltr and Squads remain the deployed mainnet binaries.",
            "The direct-call probe marks the Voltr vault_strategy_auth PDA as a transaction signer (sigverify off), which is what Voltr's CPI does on the real path.",
            "LiteSVM's CPI privilege resolution and Voltr's explicit CPI metas are indistinguishable here; on mainnet the receipt must be writable in Voltr's instruction either way, because Voltr writes positionValue into it.",
        ],
    });
    fs::write(&out, serde_json::to_string_pretty(&report).unwrap()).unwrap();
    eprintln!("wrote {}", out.display());
    eprintln!("PROBE COMPLETE (diagnostic artifact only; the authoritative matrix JSON is written by voltr_adaptor_v3_full_matrix)");
}

#[test]
#[ignore = "clones deployed mainnet programs+accounts into LiteSVM; run explicitly with --ignored"]
fn voltr_adaptor_v3_full_matrix() {
    run_matrix(true);
}

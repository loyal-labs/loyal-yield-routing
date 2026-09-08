//! Voltr bricked-vault RESET sequence proof, run end-to-end on ONE LiteSVM
//! instance against the REAL deployed program binaries (Voltr `vVoLTR…`,
//! custom adaptor `FSj27…`, Squads smart-account `SMRTz…`) cloned from
//! mainnet `getAccountInfo` dumps (fixtures/voltr-repair/).
//!
//! Sequence (all vault-admin / vault-manager actions only):
//!   R0 admin updateVaultConfig: lockedProfitDegradationDuration=0, adminPerformanceFee=0
//!   R1 repair crank deposit_strategy(0) with nav = idle + receipt - tv
//!   R2 harvestFee: mint the accumulated admin-fee LP to the admin LP ATA
//!   R3 drain: cancel the stale pending request, re-request ALL LP, wait, claim
//!   R4 fresh-user deposit / withdraw lifecycle at the reset price
//!   R5 second strategy on the same custom adaptor (initialize_config +
//!      initialize_report_ticket + Voltr initializeStrategy) + one truthful
//!      allocation; withdraw failure mode demonstrated on a CLONE
//!   R6 orphan check on strategy 1 (clone) + one more fresh-user lifecycle
//!
//! Nothing here is broadcast; nothing is signed. LiteSVM sigverify is
//! disabled and every privileged signer (admin BAqg…, manager ST999…) is
//! marked a transaction signer directly. Every override is listed in the
//! emitted results JSON under `signerOverridesAndCaveats`.
//!
//! Builders are copied from `voltr_repair_paths.rs` and generalised over the
//! strategy (config / receipt / auth / custody / ticket) so the same code
//! drives strategy 1 (9hDH…) and the new strategy 2.

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
const TRUSTFUL_ADAPTOR: &str = "3pnpK9nrs1R65eMV1wqCXkDkhSgN18xb1G5pgYPwoZjJ";

// ---- state accounts (mainnet) ---------------------------------------------
const VAULT: &str = "HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA";
const RECEIPT: &str = "3GHLmyTTGH9ZfQqb3YCo9xKjpPhMLvHsq2JSYzCnk9U6";
const STRATEGY: &str = "9hDH4acTDrSjg9d5n8c1g53jMTonaDAUesp1diCWuuhj"; // v2 adaptor config
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
const PENDING_RECEIPT: &str = "8eufrxGC9Djf7ekcoWnyewKvYz4GgjtmLLpB8HBji99e";
const PENDING_ESCROW: &str = "C35aUCiMtQa7Zou8Jag5qRbMHqvnVarPwkSwYRgshcC1";
const PROTOCOL_TREASURY: &str = "C7sE3MjSAqqF7TgXn1VsNQPWem1gdhqv3ZYV9TNfSjY9";
// PDAs (derived + verified against on-chain owners)
const PROTOCOL: &str = "4sycXz9Xwevedo6eiXR8QEhY8yrQrkNS4G1deY9tAD2Y";
const IDLE_AUTH: &str = "EoHz6FHTL34F6HjuJmb5EceaRqxRG1RMYwYWKtWkGBFb";
const STRATEGY_AUTH: &str = "8fLTf2ufePttZW3Es1xVoW3ows3WjXcuHQkkBCVvHsdH";
const LP_MINT_AUTH: &str = "HHM86gQUM7rN8bz2VPWhqNn7ZcfznLC5KyT7kLs569yq";
const ADAPTOR_ADD_RECEIPT: &str = "AsfkxMdVYjMnr2fdTBMUXhq81hgi2hbENXCy9WhUQF7u";

// ---- instruction discriminators -------------------------------------------
const VOLTR_DEPOSIT_STRATEGY: [u8; 8] = [246, 82, 57, 226, 131, 222, 253, 249];
const VOLTR_WITHDRAW_STRATEGY: [u8; 8] = [31, 45, 162, 5, 193, 217, 134, 188];
const VOLTR_DEPOSIT_VAULT: [u8; 8] = [126, 224, 21, 255, 228, 53, 117, 33];
const VOLTR_REQUEST_WITHDRAW_VAULT: [u8; 8] = [248, 225, 47, 22, 116, 144, 23, 143];
const VOLTR_WITHDRAW_VAULT: [u8; 8] = [135, 7, 237, 120, 149, 94, 95, 7];
const VOLTR_CANCEL_REQUEST_WITHDRAW_VAULT: [u8; 8] = [231, 54, 14, 6, 223, 124, 127, 238];
const VOLTR_INITIALIZE_STRATEGY: [u8; 8] = [208, 119, 144, 145, 178, 57, 105, 252];
const VOLTR_UPDATE_VAULT_CONFIG: [u8; 8] = [122, 3, 21, 222, 158, 255, 238, 157];
const VOLTR_HARVEST_FEE: [u8; 8] = [32, 59, 42, 128, 246, 73, 255, 47];
const ADAPTOR_DEPOSIT: [u8; 8] = [242, 35, 198, 137, 82, 225, 242, 182];
const ADAPTOR_WITHDRAW: [u8; 8] = [183, 18, 70, 156, 148, 109, 161, 34];
const ADAPTOR_ARM_REPORT: [u8; 8] = [164, 175, 246, 41, 178, 140, 35, 3];
const ADAPTOR_INITIALIZE_CONFIG: [u8; 8] = [208, 127, 21, 1, 194, 190, 196, 70];
const ADAPTOR_INITIALIZE_REPORT_TICKET: [u8; 8] = [124, 41, 223, 13, 165, 246, 70, 62];
const ADAPTOR_INITIALIZE: [u8; 8] = [175, 175, 109, 31, 13, 152, 155, 237];

// VaultConfigField enum indexes (SDK generated/types/vaultConfigField.ts)
const FIELD_LOCKED_PROFIT_DEGRADATION_DURATION: u8 = 2;
const FIELD_ADMIN_PERFORMANCE_FEE: u8 = 5;

const WAIT_SECS: i64 = 600;
const SLOTS_PER_EPOCH: u64 = 432_000;

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

fn account_exists(svm: &LiteSVM, addr: &Pubkey) -> bool {
    svm.get_account(addr)
        .map(|a| a.lamports > 0 && !a.data.is_empty())
        .unwrap_or(false)
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
}

fn strat1() -> Strat {
    Strat {
        config: key(STRATEGY),
        receipt: key(RECEIPT),
        auth: key(STRATEGY_AUTH),
        custody: key(CUSTODY_ATA),
        ticket: key(TICKET),
    }
}

/// Derive every account of a strategy from its (arbitrary) config key.
fn strat_for(config: Pubkey) -> Strat {
    let vault = key(VAULT);
    let receipt = pda(&[b"strategy_init_receipt", vault.as_ref(), config.as_ref()], VOLTR);
    let auth = pda(&[b"vault_strategy_auth", vault.as_ref(), config.as_ref()], VOLTR);
    let custody = ata_for(&auth, &key(USDC));
    let ticket = pda(&[b"report_ticket", config.as_ref()], ADAPTOR);
    Strat { config, receipt, auth, custody, ticket }
}

fn strat_json(s: &Strat) -> Value {
    json!({
        "config": s.config.to_string(), "strategyInitReceipt": s.receipt.to_string(),
        "vaultStrategyAuth": s.auth.to_string(), "custodyAta": s.custody.to_string(),
        "reportTicket": s.ticket.to_string(),
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
    custody2: Option<u64>,
    squads_usdc: u64,
    lp_supply: u64,
    fee_manager: u64,
    fee_admin: u64,
    fee_protocol: u64,
    dead_weight: u64,
    locked_deg: u64,
    admin_perf_bps: u16,
    manager_perf_bps: u16,
    withdraw_wait: u64,
    locked_raw: u64,
    locked_last_report: u64,
    hwm: u128,
    admin_lp: u64,
    clock_slot: u64,
    clock_epoch: u64,
    clock_ts: i64,
}

fn receipt_value(svm: &LiteSVM, receipt: &Pubkey) -> Option<u64> {
    svm.get_account(receipt)
        .filter(|a| a.data.len() >= 112)
        .map(|a| u64_le(&a.data, 104))
}

fn snap(svm: &LiteSVM, s2: Option<&Strat>) -> Snap {
    let v = svm.get_account(&key(VAULT)).unwrap().data;
    let clock: Clock = svm.get_sysvar();
    Snap {
        tv: u64_le(&v, 168),
        idle: token_amount(svm, IDLE_ATA),
        receipt1: receipt_value(svm, &key(RECEIPT)).unwrap_or(0),
        custody1: token_amount(svm, CUSTODY_ATA),
        receipt2: s2.and_then(|s| receipt_value(svm, &s.receipt)),
        custody2: s2.map(|s| token_amount_pk(svm, &s.custody)),
        squads_usdc: token_amount(svm, SQUADS_USDC_ATA),
        lp_supply: mint_supply(svm, LP_MINT),
        fee_manager: u64_le(&v, 576),
        fee_admin: u64_le(&v, 584),
        fee_protocol: u64_le(&v, 592),
        dead_weight: u64_le(&v, 616),
        locked_deg: u64_le(&v, 448),
        admin_perf_bps: u16_le(&v, 514),
        manager_perf_bps: u16_le(&v, 512),
        withdraw_wait: u64_le(&v, 456),
        locked_raw: u64_le(&v, 672),
        locked_last_report: u64_le(&v, 680),
        hwm: u128_le(&v, 624),
        admin_lp: token_amount(svm, ADMIN_LP_ATA),
        clock_slot: clock.slot,
        clock_epoch: clock.epoch,
        clock_ts: clock.unix_timestamp,
    }
}

impl Snap {
    /// Vault::get_total_lp_supply_incl_fees (SDK math.ts).
    fn lp_incl_fees(&self) -> u64 {
        self.lp_supply + self.fee_manager + self.fee_admin + self.fee_protocol + self.dead_weight
    }
    /// Effective locked profit right now (SDK calculateLockedProfit).
    fn locked_effective(&self) -> u64 {
        if self.locked_deg == 0 {
            return 0;
        }
        let dur = self.clock_ts - self.locked_last_report as i64;
        if dur <= 0 {
            return self.locked_raw;
        }
        if dur as u64 >= self.locked_deg {
            return 0;
        }
        ((self.locked_raw as u128 * (self.locked_deg - dur as u64) as u128) / self.locked_deg as u128)
            as u64
    }
    fn price(&self) -> f64 {
        self.tv as f64 / self.lp_incl_fees().max(1) as f64
    }
    /// Voltr conserves D = tv - idle - sum(receipts) under EVERY instruction
    /// (deposit_strategy: dtv = nav-old-a, didle = -a; withdraw_strategy:
    /// dtv = nav-old+swept, didle = +swept; deposit/withdraw_vault: dtv = didle).
    /// On the bricked vault -D = 3_793_536: the strategy-1 phantom.
    fn gap1(&self) -> i128 {
        self.idle as i128 + self.receipt1 as i128 + self.receipt2.unwrap_or(0) as i128 - self.tv as i128
    }
    /// Book-vs-real for the live strategy 2: tv - (idle + receipt2).
    fn book_minus_real2(&self) -> Option<i128> {
        self.receipt2.map(|r| self.tv as i128 - self.idle as i128 - r as i128)
    }
}

fn snap_json(s: &Snap) -> Value {
    json!({
        "vaultTotalValue": s.tv,
        "idleAta": s.idle,
        "strategy1ReceiptPositionValue": s.receipt1,
        "strategy1CustodyAta": s.custody1,
        "strategy2ReceiptPositionValue": s.receipt2,
        "strategy2CustodyAta": s.custody2,
        "squadsUsdcAta": s.squads_usdc,
        "lpSupply": s.lp_supply,
        "feeStateAccumulatedLpManagerFees": s.fee_manager,
        "feeStateAccumulatedLpAdminFees": s.fee_admin,
        "feeStateAccumulatedLpProtocolFees": s.fee_protocol,
        "deadWeight": s.dead_weight,
        "lpSupplyInclFees": s.lp_incl_fees(),
        "lpPriceAssetPerLp": format!("{:.9}", s.price()),
        "lpPriceRational": format!("{}/{}", s.tv, s.lp_incl_fees()),
        "lockedProfitDegradationDuration": s.locked_deg,
        "adminPerformanceFeeBps": s.admin_perf_bps,
        "managerPerformanceFeeBps": s.manager_perf_bps,
        "withdrawalWaitingPeriod": s.withdraw_wait,
        "lockedProfitState": {"lastUpdatedLockedProfit": s.locked_raw, "lastReport": s.locked_last_report, "effectiveLockedNow": s.locked_effective()},
        "highWaterMarkAssetPerLpBits": s.hwm.to_string(),
        "highWaterMarkAssetPerLp": dec48(s.hwm),
        "adminLpAta": s.admin_lp,
        "tvEqualsIdle": s.tv == s.idle,
        "tvEqualsIdlePlusReceipt2": s.receipt2.map(|r| s.tv == s.idle + r),
        "conservedGap_idlePlusReceiptsMinusTv": s.gap1().to_string(),
        "strategy2BookMinusReal": s.book_minus_real2().map(|v| v.to_string()),
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
        PENDING_ESCROW,
        PENDING_RECEIPT,
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
    Base { svm, slot0: clock.slot, ts0: clock.unix_timestamp }
}

/// New epoch (slot + 432k, epoch + 1): Voltr stamps adaptorAddReceipt
/// .lastUpdatedEpoch on every strategy crank and rejects a repeat in the same
/// epoch (6010), so each deposit/withdraw_strategy needs a fresh one. NOTE: the
/// add-receipt is per (vault, adaptor program), i.e. shared by strategy 1 and 2.
fn advance_epoch(svm: &mut LiteSVM) {
    let mut clock: Clock = svm.get_sysvar();
    clock.slot += SLOTS_PER_EPOCH;
    clock.epoch += 1;
    clock.leader_schedule_epoch = clock.epoch + 1;
    clock.unix_timestamp += 172_800;
    clock.epoch_start_timestamp = clock.unix_timestamp;
    svm.set_sysvar(&clock);
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

// ---- crank builders (generalised over the strategy) ----------------------------
fn report_v1(seq: u64, nav: u64) -> Vec<u8> {
    let mut r = vec![1u8];
    r.extend_from_slice(&seq.to_le_bytes());
    r.extend_from_slice(&seq.to_le_bytes());
    r.extend_from_slice(&nav.to_le_bytes());
    r.extend_from_slice(&[7u8; 32]);
    r
}

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

fn deposit_strategy_ix(s: &Strat, amount: u64, seq: u64, nav: u64) -> Instruction {
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
            meta(false, false, SETTINGS),            // 14 settings (remaining)
            meta(true, false, SQUADS_VAULT),         // 15 squads vault (signer)
            meta(false, true, SQUADS_USDC_ATA),      // 16 squads asset ata (w)
            m(s.ticket, false, true),                // 17 report ticket (w)
        ],
        data: voltr_capital_data(VOLTR_DEPOSIT_STRATEGY, amount, ADAPTOR_DEPOSIT, seq, nav),
    }
}

fn withdraw_strategy_ix(s: &Strat, amount: u64, seq: u64, nav: u64) -> Instruction {
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
            meta(false, false, SETTINGS),            // 14 settings
            meta(true, false, SQUADS_VAULT),         // 15 squads vault (signer)
            meta(false, true, SQUADS_USDC_ATA),      // 16 squads asset ata (w)
            m(s.ticket, false, true),                // 17 ticket (w)
        ],
        data: voltr_capital_data(VOLTR_WITHDRAW_STRATEGY, amount, ADAPTOR_WITHDRAW, seq, nav),
    }
}

/// [arm_report, deposit_strategy] in one tx after advancing to a fresh epoch.
fn crank_deposit(svm: &mut LiteSVM, s: &Strat, amount: u64, nav: u64) -> (TxResult, u64) {
    advance_epoch(svm);
    let clock: Clock = svm.get_sysvar();
    let seq = clock.slot;
    let payer = new_funded(svm);
    let ixs = vec![cu_ix(), arm_report_ix(s, 0, amount, seq, nav), deposit_strategy_ix(s, amount, seq, nav)];
    (send(svm, &ixs, &payer), seq)
}

fn crank_withdraw(svm: &mut LiteSVM, s: &Strat, amount: u64, nav: u64) -> (TxResult, u64) {
    advance_epoch(svm);
    let clock: Clock = svm.get_sysvar();
    let seq = clock.slot;
    let payer = new_funded(svm);
    let ixs = vec![cu_ix(), arm_report_ix(s, 1, amount, seq, nav), withdraw_strategy_ix(s, amount, seq, nav)];
    (send(svm, &ixs, &payer), seq)
}

// ---- user-side builders ----------------------------------------------------------
fn request_withdraw_vault_ix(
    user: &Pubkey,
    user_lp_ata: &Pubkey,
    receipt: &Pubkey,
    escrow: &Pubkey,
    amount_lp: u64,
    withdraw_all: bool,
) -> Instruction {
    let mut data = VOLTR_REQUEST_WITHDRAW_VAULT.to_vec();
    data.extend_from_slice(&amount_lp.to_le_bytes());
    data.push(1); // isAmountInLp
    data.push(u8::from(withdraw_all)); // isWithdrawAll
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            m(*user, true, true),           // 0 payer (w,signer)
            m(*user, true, false),          // 1 userTransferAuthority (signer)
            meta(false, false, PROTOCOL),   // 2 protocol
            meta(false, false, VAULT),      // 3 vault
            meta(false, false, LP_MINT),    // 4 vault lp mint
            m(*user_lp_ata, false, true),   // 5 user lp ata (w)
            m(*escrow, false, true),        // 6 request withdraw lp ata (w)
            m(*receipt, false, true),       // 7 request withdraw vault receipt (w)
            meta(false, false, TOKEN),      // 8 lp token program
            meta(false, false, SYS),        // 9 system program
        ],
        data,
    }
}

fn withdraw_vault_ix(user: &Pubkey, receipt: &Pubkey, escrow: &Pubkey, user_asset: &Pubkey) -> Instruction {
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            m(*user, true, true),           // 0 userTransferAuthority (w,signer)
            meta(false, false, PROTOCOL),   // 1 protocol
            meta(false, true, VAULT),       // 2 vault (w)
            meta(false, false, USDC),       // 3 vault asset mint
            meta(false, true, LP_MINT),     // 4 vault lp mint (w)
            m(*escrow, false, true),        // 5 request withdraw lp ata (w)
            meta(false, true, IDLE_ATA),    // 6 vault asset idle ata (w)
            meta(false, true, IDLE_AUTH),   // 7 vault asset idle auth (w)
            m(*user_asset, false, true),    // 8 user asset ata (w)
            m(*receipt, false, true),       // 9 request withdraw vault receipt (w)
            meta(false, false, TOKEN),      // 10 asset token program
            meta(false, false, TOKEN),      // 11 lp token program
            meta(false, false, SYS),        // 12 system program
        ],
        data: VOLTR_WITHDRAW_VAULT.to_vec(),
    }
}

fn cancel_request_withdraw_vault_ix(user: &Pubkey, user_lp_ata: &Pubkey, receipt: &Pubkey, escrow: &Pubkey) -> Instruction {
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            m(*user, true, true),           // 0 userTransferAuthority (w,signer)
            meta(false, false, PROTOCOL),   // 1 protocol
            meta(false, true, VAULT),       // 2 vault (w)
            meta(false, true, LP_MINT),     // 3 vault lp mint (w)
            m(*user_lp_ata, false, true),   // 4 user lp ata (w)
            m(*escrow, false, true),        // 5 request withdraw lp ata (w)
            m(*receipt, false, true),       // 6 request withdraw vault receipt (w)
            meta(false, false, TOKEN),      // 7 lp token program
            meta(false, false, SYS),        // 8 system program
        ],
        data: VOLTR_CANCEL_REQUEST_WITHDRAW_VAULT.to_vec(),
    }
}

fn deposit_vault_ix(user: &Pubkey, user_usdc: &Pubkey, user_lp: &Pubkey, amount: u64) -> Instruction {
    let mut data = VOLTR_DEPOSIT_VAULT.to_vec();
    data.extend_from_slice(&amount.to_le_bytes());
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            m(*user, true, false),           // 0 userTransferAuthority (signer)
            meta(false, false, PROTOCOL),    // 1 protocol
            meta(false, true, VAULT),        // 2 vault (w)
            meta(false, false, USDC),        // 3 asset mint
            meta(false, true, LP_MINT),      // 4 lp mint (w)
            m(*user_usdc, false, true),      // 5 user asset ata (w)
            meta(false, true, IDLE_ATA),     // 6 vault idle ata (w)
            meta(false, false, IDLE_AUTH),   // 7 idle auth
            m(*user_lp, false, true),        // 8 user lp ata (w)
            meta(false, false, LP_MINT_AUTH),// 9 lp mint auth
            meta(false, false, TOKEN),       // 10 asset token program
            meta(false, false, TOKEN),       // 11 lp token program
            meta(false, false, SYS),         // 12 system program
        ],
        data,
    }
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
            meta(true, false, ADMIN),        // 0 admin (signer)
            meta(false, false, PROTOCOL),    // 1 protocol
            meta(false, true, VAULT),        // 2 vault (w)
            meta(false, false, RENT_SYSVAR), // 3 rent
        ],
        data: d,
    }
}

fn harvest_fee_ix(harvester: &Pubkey) -> Instruction {
    let lp = key(LP_MINT);
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            m(*harvester, true, false),                          // 0 harvester (signer)
            meta(false, false, SQUADS_VAULT),                    // 1 vault manager
            meta(false, false, ADMIN),                           // 2 vault admin
            meta(false, false, PROTOCOL_TREASURY),               // 3 protocol treasury
            meta(false, false, PROTOCOL),                        // 4 protocol
            meta(false, true, VAULT),                            // 5 vault (w)
            meta(false, true, LP_MINT),                          // 6 lp mint (w)
            meta(false, false, LP_MINT_AUTH),                    // 7 lp mint auth
            m(ata_for(&key(SQUADS_VAULT), &lp), false, true),    // 8 manager lp ata (w)
            meta(false, true, ADMIN_LP_ATA),                     // 9 admin lp ata (w)
            m(ata_for(&key(PROTOCOL_TREASURY), &lp), false, true), // 10 treasury lp ata (w)
            meta(false, false, TOKEN),                           // 11 lp token program
        ],
        data: VOLTR_HARVEST_FEE.to_vec(),
    }
}

/// Custom adaptor initialize_config (13 accounts, 17 data bytes). The config
/// account is a fresh keypair signer; its key IS the Voltr strategy key.
fn adaptor_initialize_config_ix(payer: &Pubkey, s: &Strat, squads_vault_index: u8, max_nav: u64, max_age_slots: u64) -> Instruction {
    let mut d = ADAPTOR_INITIALIZE_CONFIG.to_vec();
    d.push(squads_vault_index);
    d.extend_from_slice(&max_nav.to_le_bytes());
    d.extend_from_slice(&max_age_slots.to_le_bytes());
    Instruction {
        program_id: key(ADAPTOR),
        accounts: vec![
            m(*payer, true, true),
            m(s.config, true, true),
            meta(false, false, VOLTR),
            meta(false, false, VAULT),
            m(s.auth, false, false),
            meta(false, false, SQUADS),
            meta(false, false, SETTINGS),
            meta(false, false, ADMIN), // squads settings signer (= BAqg on-chain config 9hDH)
            meta(false, false, SQUADS_VAULT),
            meta(false, false, USDC),
            meta(false, false, TOKEN),
            meta(false, false, SQUADS_USDC_ATA),
            meta(false, false, SYS),
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

/// Voltr initializeStrategy (manager signs) + the 6 adaptor remaining accounts
/// exactly as tools/backyard-voltr initializeRemainingAccounts() appends them.
fn voltr_initialize_strategy_ix(payer: &Pubkey, s: &Strat) -> Instruction {
    let mut d = VOLTR_INITIALIZE_STRATEGY.to_vec();
    d.push(1); // Some(instructionDiscriminator)
    d.extend_from_slice(&8u32.to_le_bytes());
    d.extend_from_slice(&ADAPTOR_INITIALIZE);
    d.push(0); // additionalArgs = None
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            m(*payer, true, true),                   // 0 payer
            meta(true, false, SQUADS_VAULT),         // 1 manager (signer)
            meta(false, false, PROTOCOL),            // 2 protocol
            meta(false, false, VAULT),               // 3 vault
            m(s.config, false, false),               // 4 strategy
            meta(false, false, ADAPTOR_ADD_RECEIPT), // 5 adaptor add receipt
            m(s.receipt, false, true),               // 6 strategy init receipt (w)
            m(s.auth, false, true),                  // 7 vault strategy auth (w)
            meta(false, false, ADAPTOR),             // 8 adaptor program
            meta(false, false, SYS),                 // 9 system program
            meta(false, false, SETTINGS),            // remaining: settings
            meta(false, false, SQUADS_VAULT),        //   squads vault
            meta(false, false, USDC),                //   asset mint
            meta(false, false, TOKEN),               //   token program
            meta(false, false, SQUADS_USDC_ATA),     //   squads asset ata
            meta(false, false, SQUADS),              //   squads program
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
        self.steps.push(v);
        ok
    }
}

fn chk(v: &mut Vec<(String, bool)>, name: impl Into<String>, pass: bool) {
    v.push((name.into(), pass));
}

// ---- pending-request receipt decode ---------------------------------------------------
/// Voltr "Decimal" fixed point: 48 fractional bits (SDK extensions/decimal.ts).
const DECIMAL_FRACTIONAL_BITS: u32 = 48;
fn dec48(bits: u128) -> String {
    let int = bits >> DECIMAL_FRACTIONAL_BITS;
    let frac = (bits & ((1u128 << DECIMAL_FRACTIONAL_BITS) - 1)) as f64 / (1u128 << DECIMAL_FRACTIONAL_BITS) as f64;
    format!("{int}.{:06}", (frac * 1_000_000.0).round() as u64)
}

fn decode_request_receipt(svm: &LiteSVM, receipt: &Pubkey) -> Value {
    match svm.get_account(receipt) {
        Some(a) if a.data.len() >= 106 => {
            let d = &a.data;
            let bits = u128_le(d, 80);
            json!({
                "present": true,
                "user": Pubkey::new_from_array(d[40..72].try_into().unwrap()).to_string(),
                "amountLpEscrowed": u64_le(d, 72),
                "amountAssetToWithdrawDecimalBits": bits.to_string(),
                "amountAssetToWithdrawAtRequest": dec48(bits),
                "withdrawableFromTs": u64_le(d, 96),
            })
        }
        _ => json!({"present": false}),
    }
}

/// SPL Token TransferChecked built by hand (source, mint, dest, authority).
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

fn close_strategy_ix(payer: &Pubkey, s: &Strat) -> Instruction {
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            m(*payer, true, true),            // 0 payer (w, signer)
            meta(true, false, SQUADS_VAULT),  // 1 manager (signer)
            meta(false, false, PROTOCOL),     // 2 protocol
            meta(false, false, VAULT),        // 3 vault
            m(s.config, false, false),        // 4 strategy
            m(s.receipt, false, true),        // 5 strategy init receipt (w)
            meta(false, false, SYS),          // 6 system program
        ],
        data: [56, 247, 170, 246, 89, 221, 134, 200].to_vec(), // closeStrategy
    }
}

// =========================================================================================
#[test]
#[ignore = "clones deployed mainnet programs+accounts into LiteSVM; run explicitly with --ignored"]
fn voltr_reset_sequence() {
    let base = build_base();
    let mut svm = base.svm.clone();
    let mut rec = Rec { steps: vec![], all_ok: true };
    let s1 = strat1();
    let admin = key(ADMIN);
    fund(&mut svm, &admin, 1_000_000_000);

    let s_init = snap(&svm, None);
    assert_eq!(s_init.dead_weight, 1_000, "deadWeight offset sanity");
    assert_eq!(s_init.withdraw_wait, WAIT_SECS as u64, "withdrawalWaitingPeriod");
    // The mainnet pending receipt must be the PDA for admin BAqg; its escrow is C35a.
    let pending_receipt = pda(&[b"request_withdraw_vault_receipt", key(VAULT).as_ref(), admin.as_ref()], VOLTR);
    assert_eq!(pending_receipt, key(PENDING_RECEIPT), "pending request receipt PDA");
    assert_eq!(ata_for(&pending_receipt, &key(LP_MINT)), key(PENDING_ESCROW), "pending escrow ATA");
    let pending_before = decode_request_receipt(&svm, &pending_receipt);

    // ---------------------------------------------------------------- R0
    {
        let before = snap(&svm, None);
        let r_lock = send(&mut svm, &[cu_ix(), update_vault_config_ix(FIELD_LOCKED_PROFIT_DEGRADATION_DURATION, &0u64.to_le_bytes())], &admin);
        let mid = snap(&svm, None);
        // AdminPerformanceFee is a u16 in FeeConfiguration; probe the 2-byte
        // encoding on a clone first and fall back to 8 bytes if rejected.
        let mut fee_encoding = "u16";
        let mut probe = svm.clone();
        let mut r_fee = send(&mut probe, &[cu_ix(), update_vault_config_ix(FIELD_ADMIN_PERFORMANCE_FEE, &0u16.to_le_bytes())], &admin);
        if r_fee.is_ok() && snap(&probe, None).admin_perf_bps == 0 {
            svm = probe;
        } else {
            fee_encoding = "u64";
            r_fee = send(&mut svm, &[cu_ix(), update_vault_config_ix(FIELD_ADMIN_PERFORMANCE_FEE, &0u64.to_le_bytes())], &admin);
        }
        let after = snap(&svm, None);
        let mut c = vec![];
        chk(&mut c, "updateVaultConfig(LockedProfitDegradationDuration=0) executed", r_lock.is_ok());
        chk(&mut c, format!("lockedProfitDegradationDuration {} -> 0", before.locked_deg), after.locked_deg == 0);
        chk(&mut c, "updateVaultConfig(AdminPerformanceFee=0) executed", r_fee.is_ok());
        chk(&mut c, format!("adminPerformanceFee {} -> 0", before.admin_perf_bps), after.admin_perf_bps == 0);
        chk(&mut c, "books untouched (tv, receipt1, idle)", after.tv == before.tv && after.receipt1 == before.receipt1 && after.idle == before.idle);
        rec.push("R0", "admin updateVaultConfig: LockedProfitDegradationDuration=0, AdminPerformanceFee=0", &c, json!({
            "signer": ADMIN,
            "instructions": [
                {"ix": "updateVaultConfig", "field": "LockedProfitDegradationDuration(2)", "data": "u64 0", "tx": tx_json(&r_lock)},
                {"ix": "updateVaultConfig", "field": "AdminPerformanceFee(5)", "data": format!("{fee_encoding} 0"), "tx": tx_json(&r_fee)},
            ],
            "before": snap_json(&before), "afterFirst": snap_json(&mid), "after": snap_json(&after),
        }));
    }

    // ---------------------------------------------------------------- R1
    {
        let before = snap(&svm, None);
        let nav = before.idle + (before.receipt1 - before.tv);
        let (r, seq) = crank_deposit(&mut svm, &s1, 0, nav);
        let after = snap(&svm, None);
        let mut c = vec![];
        chk(&mut c, "repair crank executed", r.is_ok());
        chk(&mut c, format!("tv == idle == {}", before.idle), after.tv == before.idle && after.idle == before.idle);
        chk(&mut c, format!("receipt1 == {nav}"), after.receipt1 == nav);
        chk(&mut c, "effective lockedProfit == 0 (degradation duration 0)", after.locked_effective() == 0);
        chk(&mut c, "no performance-fee LP accrued (fee 0)", after.fee_admin == before.fee_admin && after.fee_manager == before.fee_manager && after.fee_protocol == before.fee_protocol);
        chk(&mut c, "lp supply unchanged", after.lp_supply == before.lp_supply);
        rec.push("R1", "repair crank: arm_report + deposit_strategy(0) nav = idle + receipt1 - tv", &c, json!({
            "reportedNav": nav, "sequence": seq, "signer": SQUADS_VAULT, "tx": tx_json(&r),
            "before": snap_json(&before), "after": snap_json(&after),
            "lockedProfitRawAfter": after.locked_raw,
        }));
    }

    // ---------------------------------------------------------------- R2
    {
        let before = snap(&svm, None);
        let lp = key(LP_MINT);
        // manager + treasury LP ATAs must exist (permissionless ATA creation on mainnet)
        let payer = new_funded(&mut svm);
        let r_atas = send(&mut svm, &[
            create_ata_idempotent_ix(&payer, &key(SQUADS_VAULT), &lp),
            create_ata_idempotent_ix(&payer, &key(PROTOCOL_TREASURY), &lp),
            create_ata_idempotent_ix(&payer, &admin, &lp),
        ], &payer);
        // who may harvest? probe candidates on clones, apply the first that works
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
        chk(&mut c, "lpSupplyInclFees unchanged (fee LP was already counted)", after.lp_incl_fees() == before.lp_incl_fees());
        chk(&mut c, "tv/idle untouched", after.tv == before.tv && after.idle == before.idle);
        rec.push("R2", "harvestFee: mint accumulated admin-fee LP to the admin LP ATA", &c, json!({
            "harvesterThatWorked": applied.as_ref().map(|(l, _)| *l),
            "attempts": attempts,
            "mintedAdminFeeLp": expect,
            "before": snap_json(&before), "after": snap_json(&after),
        }));
    }

    // ---------------------------------------------------------------- R3
    {
        let before = snap(&svm, None);
        let usdc = key(USDC);
        let lp = key(LP_MINT);
        let admin_usdc = ata_for(&admin, &usdc);
        let escrow = key(PENDING_ESCROW);
        let receipt = pending_receipt;

        // (branch) claim the stale mainnet request AS-IS on a clone: it was priced
        // while profit was locked, so amountAssetToWithdraw is tiny — min(atRequest, atPresent).
        let mut branch = svm.clone();
        let r_ata = send(&mut branch, &[create_ata_idempotent_ix(&admin, &admin, &usdc)], &admin);
        let r_asis = send(&mut branch, &[cu_ix(), withdraw_vault_ix(&admin, &receipt, &escrow, &admin_usdc)], &admin);
        let asis_after = snap(&branch, None);
        let asis_payout = token_amount_pk(&branch, &admin_usdc);
        let asis = json!({
            "path": "claim pending request as-is (CLONE, discarded)",
            "ataCreate": tx_json(&r_ata), "claim": tx_json(&r_asis),
            "pendingReceiptBefore": pending_before,
            "payoutRaw": asis_payout, "lpBurned": before.lp_supply - asis_after.lp_supply,
            "after": snap_json(&asis_after),
            "verdict": if r_asis.is_ok() { "EXECUTES_BUT_UNFAIR" } else { "REJECTED" },
            "note": "amountAssetToWithdrawDecimalBits (2^48 fixed point) was frozen at request time under the pre-repair price; withdrawVault pays min(atRequest, atPresent) and burns ALL escrowed LP, so claiming as-is forfeits the difference to nobody (it stays in the vault). Cancel + re-request instead (main line below).",
        });

        // (main) cancel -> LP back to ZvsW -> request ALL -> wait 600s -> claim
        let r_cancel = send(&mut svm, &[cu_ix(), cancel_request_withdraw_vault_ix(&admin, &key(ADMIN_LP_ATA), &receipt, &escrow)], &admin);
        let after_cancel = snap(&svm, None);
        let escrow_after_cancel = account_exists(&svm, &escrow);
        let receipt_after_cancel = account_exists(&svm, &receipt);
        let lp_to_burn = after_cancel.admin_lp;
        let r_req = send(&mut svm, &[
            cu_ix(),
            create_ata_idempotent_ix(&admin, &receipt, &lp),
            request_withdraw_vault_ix(&admin, &key(ADMIN_LP_ATA), &receipt, &escrow, lp_to_burn, true),
        ], &admin);
        let req_receipt = decode_request_receipt(&svm, &receipt);
        let wf = req_receipt["withdrawableFromTs"].as_u64().unwrap_or(0);
        let ts_req: Clock = svm.get_sysvar();
        set_clock_ts(&mut svm, (wf as i64).max(ts_req.unix_timestamp + WAIT_SECS) + 5);
        let r_ata2 = send(&mut svm, &[create_ata_idempotent_ix(&admin, &admin, &usdc)], &admin);
        let r_claim = send(&mut svm, &[cu_ix(), withdraw_vault_ix(&admin, &receipt, &escrow, &admin_usdc)], &admin);
        let after = snap(&svm, None);
        let payout = token_amount_pk(&svm, &admin_usdc);
        // cancelRequestWithdrawVault refunds LP worth the request-time asset amount
        // at the post-cancel price and BURNS the rest (event amountLpBurned); the
        // burned value accrues to the remaining LP holders (= the same admin here).
        let burned_on_cancel = before.lp_supply - after_cancel.lp_supply;
        let refunded_on_cancel = after_cancel.admin_lp - before.admin_lp;
        let fair = (before.tv as u128 * lp_to_burn as u128 / after_cancel.lp_incl_fees() as u128) as u64;
        let mut c = vec![];
        chk(&mut c, "cancelRequestWithdrawVault executed (admin BAqg)", r_cancel.is_ok());
        chk(&mut c, "after cancel the admin LP ATA holds 100% of LP supply", lp_to_burn > 0 && lp_to_burn == after_cancel.lp_supply);
        chk(&mut c, "cancel left tv/idle untouched", after_cancel.tv == before.tv && after_cancel.idle == before.idle);
        chk(&mut c, "requestWithdrawVault(all) executed", r_req.is_ok());
        chk(&mut c, format!("withdrawableFromTs == requestTs + {WAIT_SECS}"), wf == ts_req.unix_timestamp as u64 + WAIT_SECS as u64);
        chk(&mut c, "withdrawVault claim executed", r_claim.is_ok());
        chk(&mut c, format!("payout == floor(tv*lp/lpInclFees) == {fair}"), payout == fair);
        chk(&mut c, "LP supply == 0 (only virtual deadWeight 1000 remains)", after.lp_supply == 0 && after.lp_incl_fees() == after.dead_weight);
        chk(&mut c, "tv > 0", after.tv > 0);
        chk(&mut c, "tv == idle (book == real)", after.tv == after.idle);
        rec.push("R3", "drain: cancel stale request, re-request ALL LP at reset price, wait 600s, claim", &c, json!({
            "asIsClaimBranch": asis,
            "cancel": {"tx": tx_json(&r_cancel), "escrowAtaStillExists": escrow_after_cancel, "receiptStillExists": receipt_after_cancel,
                       "lpRefunded": refunded_on_cancel, "lpBurnedByCancel": burned_on_cancel,
                       "why": "Voltr refunds LP worth amountAssetToWithdrawAtRequest at the post-cancel price and burns the remainder (CancelRequestWithdrawVaultEvent.amountLpBurned); the burned LP's value accrues to the other LP holders, which is the same admin wallet here, so the total drain below is unaffected.",
                       "after": snap_json(&after_cancel)},
            "request": {"tx": tx_json(&r_req), "lpRequested": lp_to_burn, "receipt": req_receipt, "requestClockTs": ts_req.unix_timestamp},
            "claim": {"ataCreate": tx_json(&r_ata2), "tx": tx_json(&r_claim), "payoutRaw": payout, "fairPayout": fair,
                      "residualTv": after.tv, "residualIdle": after.idle},
            "before": snap_json(&before), "after": snap_json(&after),
            "lpPriceAfterDrain": format!("{}/{} = {:.9}", after.tv, after.lp_incl_fees(), after.price()),
        }));
    }

    // ---------------------------------------------------------------- R4
    let lifecycle = |svm: &mut LiteSVM, amount: u64, s2: Option<&Strat>| -> (Vec<(String, bool)>, Value) {
        let usdc = key(USDC);
        let lp = key(LP_MINT);
        let user = new_funded(svm);
        let user_usdc = ata_for(&user, &usdc);
        let user_lp = ata_for(&user, &lp);
        set_token_account(svm, user_usdc, usdc, user, amount);
        let before = snap(svm, s2);
        let r_dep = send(svm, &[cu_ix(), create_ata_idempotent_ix(&user, &user, &lp), deposit_vault_ix(&user, &user_usdc, &user_lp, amount)], &user);
        let after_dep = snap(svm, s2);
        let minted = token_amount_pk(svm, &user_lp);
        let expected_lp = (amount as u128 * before.lp_incl_fees() as u128 / before.tv.max(1) as u128) as u64;
        let receipt = pda(&[b"request_withdraw_vault_receipt", key(VAULT).as_ref(), user.as_ref()], VOLTR);
        let escrow = ata_for(&receipt, &lp);
        let r_req = send(svm, &[cu_ix(), create_ata_idempotent_ix(&user, &receipt, &lp), request_withdraw_vault_ix(&user, &user_lp, &receipt, &escrow, minted, true)], &user);
        let req_receipt = decode_request_receipt(svm, &receipt);
        let wf = req_receipt["withdrawableFromTs"].as_u64().unwrap_or(0);
        let ts_req: Clock = svm.get_sysvar();
        set_clock_ts(svm, (wf as i64).max(ts_req.unix_timestamp + WAIT_SECS) + 5);
        let r_claim = send(svm, &[cu_ix(), withdraw_vault_ix(&user, &receipt, &escrow, &user_usdc)], &user);
        let after = snap(svm, s2);
        let payout = token_amount_pk(svm, &user_usdc);
        let mut c = vec![];
        chk(&mut c, format!("deposit_vault({amount}) executed"), r_dep.is_ok());
        chk(&mut c, format!("tv += {amount} and idle += {amount}"), after_dep.tv == before.tv + amount && after_dep.idle == before.idle + amount);
        chk(&mut c, format!("LP minted == floor(amount*lpInclFees/tv) == {expected_lp}"), minted == expected_lp);
        chk(&mut c, "requestWithdrawVault(all) executed", r_req.is_ok());
        chk(&mut c, "withdrawVault executed after 600s", r_claim.is_ok());
        chk(&mut c, format!("payout within 5 raw of {amount}"), (payout as i128 - amount as i128).abs() <= 5);
        let consistent = |s: &Snap| match s.receipt2 { Some(r) => s.tv == s.idle + r, None => s.tv == s.idle };
        chk(&mut c, "tv == idle (+ receipt2) before, after deposit and after claim", consistent(&before) && consistent(&after_dep) && consistent(&after));
        chk(&mut c, "user LP fully burned", token_amount_pk(svm, &user_lp) == 0 && after.lp_supply == before.lp_supply);
        (c, json!({
            "user": user.to_string(), "amount": amount,
            "deposit": {"tx": tx_json(&r_dep), "lpMinted": minted, "expectedLp": expected_lp,
                        "impliedPriceAssetPerLp": format!("{:.9}", amount as f64 / minted.max(1) as f64)},
            "request": {"tx": tx_json(&r_req), "receipt": req_receipt},
            "claim": {"tx": tx_json(&r_claim), "payoutRaw": payout, "deltaFromDeposit": payout as i128 - amount as i128},
            "before": snap_json(&before), "afterDeposit": snap_json(&after_dep), "after": snap_json(&after),
        }))
    };
    {
        let (c, body) = lifecycle(&mut svm, 1_000_000, None);
        rec.push("R4", "fresh user lifecycle at reset price: deposit_vault(1_000_000) -> request all -> wait -> claim", &c, body);
    }

    // ---------------------------------------------------------------- R5 (a)
    let s2 = strat_for(Pubkey::new_unique());
    let s2_live: bool;
    {
        let before = snap(&svm, None);
        let payer = new_funded(&mut svm);
        // 1. adaptor initialize_config (config keypair signs; same bounds as 9hDH)
        let r_cfg = send(&mut svm, &[cu_ix(), adaptor_initialize_config_ix(&payer, &s2, 0, 2_000_000_000_000, 32)], &payer);
        // 2. adaptor initialize_report_ticket
        let r_tk = send(&mut svm, &[cu_ix(), adaptor_initialize_report_ticket_ix(&payer, &s2)], &payer);
        // 3. Voltr initializeStrategy (manager ST999 signs) -> CPI adaptor initialize
        let r_init = send(&mut svm, &[cu_ix(), voltr_initialize_strategy_ix(&payer, &s2)], &payer);
        // 4. custody ATA for the new strategy auth (permissionless)
        let r_cust = send(&mut svm, &[create_ata_idempotent_ix(&payer, &s2.auth, &key(USDC))], &payer);
        let after_setup = snap(&svm, Some(&s2));
        let cfg_ok = svm.get_account(&s2.config).map(|a| a.owner == key(ADAPTOR) && a.data.len() == 16 + 12 * 32 + 40 + 32).unwrap_or(false);
        let tk_ok = svm.get_account(&s2.ticket).map(|a| a.owner == key(ADAPTOR) && a.data.len() == 96).unwrap_or(false);
        let rcpt_ok = svm.get_account(&s2.receipt).map(|a| a.owner == key(VOLTR)).unwrap_or(false);
        let mut c = vec![];
        chk(&mut c, "adaptor initialize_config executed (new config = new strategy key)", r_cfg.is_ok() && cfg_ok);
        chk(&mut c, "adaptor initialize_report_ticket executed", r_tk.is_ok() && tk_ok);
        chk(&mut c, "Voltr initializeStrategy executed (manager ST999 signer)", r_init.is_ok() && rcpt_ok);
        chk(&mut c, "receipt2.positionValue == 0", after_setup.receipt2 == Some(0));
        chk(&mut c, "custody2 ATA created", r_cust.is_ok() && account_exists(&svm, &s2.custody));
        chk(&mut c, "books untouched by setup", after_setup.tv == before.tv && after_setup.idle == before.idle);
        s2_live = rec.push("R5a-setup", "second strategy on the same custom adaptor FSj27: initialize_config + initialize_report_ticket + Voltr initializeStrategy", &c, json!({
            "strategy2": strat_json(&s2),
            "initializeConfig": {"tx": tx_json(&r_cfg), "args": {"squadsVaultIndex": 0, "maxReportNavRaw": 2_000_000_000_000u64, "maxReportAgeSlots": 32}},
            "initializeReportTicket": tx_json(&r_tk),
            "voltrInitializeStrategy": tx_json(&r_init),
            "custodyAtaCreate": tx_json(&r_cust),
            "before": snap_json(&before), "after": snap_json(&after_setup),
        }));
    }
    if s2_live {
        // seed capital so idle can fund a 500_000 allocation (admin deposit_vault)
        let usdc = key(USDC);
        let lp = key(LP_MINT);
        let admin_usdc = ata_for(&admin, &usdc);
        let seed = 1_000_000u64;
        set_token_account(&mut svm, admin_usdc, usdc, admin, seed);
        let before_seed = snap(&svm, Some(&s2));
        let r_seed = send(&mut svm, &[cu_ix(), create_ata_idempotent_ix(&admin, &admin, &lp), deposit_vault_ix(&admin, &admin_usdc, &key(ADMIN_LP_ATA), seed)], &admin);
        let before = snap(&svm, Some(&s2));
        // truthful allocation: cash leaves idle -> custody2 -> Squads USDC ATA inside the CPI; nav = 500_000
        let alloc = 500_000u64;
        let (r_alloc, seq) = crank_deposit(&mut svm, &s2, alloc, alloc);
        let after = snap(&svm, Some(&s2));
        let mut c = vec![];
        chk(&mut c, format!("seed deposit_vault({seed}) by admin executed"), r_seed.is_ok() && before.tv == before_seed.tv + seed);
        chk(&mut c, "deposit_strategy(500_000, nav=500_000) on strategy 2 executed", r_alloc.is_ok());
        chk(&mut c, "receipt2 == 500_000", after.receipt2 == Some(alloc));
        chk(&mut c, "tv unchanged", after.tv == before.tv);
        chk(&mut c, "idle -= 500_000", after.idle == before.idle - alloc);
        chk(&mut c, "tv == idle + receipt2", after.tv == after.idle + alloc);
        chk(&mut c, "custody2 == 0", after.custody2 == Some(0));
        chk(&mut c, "Squads USDC ATA (EBG2) += 500_000", after.squads_usdc == before.squads_usdc + alloc);
        chk(&mut c, "receipt1 untouched", after.receipt1 == before.receipt1);
        rec.push("R5a-allocate", "strategy 2 allocation: arm_report + deposit_strategy(500_000) with truthful nav_after = 500_000", &c, json!({
            "seedDeposit": tx_json(&r_seed), "sequence": seq, "tx": tx_json(&r_alloc),
            "before": snap_json(&before), "after": snap_json(&after),
        }));

        // NAV/cash mismatch hazards on CLONES (discarded). The hypothesised
        // "pre-staged custody is not credited" failure mode does NOT reproduce
        // against the deployed program (see R5a-withdraw below): Voltr credits
        // whatever custody balance it sweeps. What does hurt is a NAV that does
        // not match the cash movement.
        {
            let mut cases = vec![];
            let mut expectations_hold = true;
            // (i) cash staged into custody but NAV NOT reduced -> tv inflates by 500_000 (book > real)
            {
                let mut clone = svm.clone();
                let r_stage = send(&mut clone, &[transfer_checked_ix(&key(SQUADS_USDC_ATA), &s2.custody, &key(SQUADS_VAULT), alloc)], &key(SQUADS_VAULT));
                let b = snap(&clone, Some(&s2));
                let (r_w, seq_w) = crank_withdraw(&mut clone, &s2, alloc, alloc);
                let a = snap(&clone, Some(&s2));
                let inflated = r_w.is_ok() && a.tv == b.tv + alloc && a.idle == b.idle + alloc && a.receipt2 == Some(alloc);
                expectations_hold &= r_stage.is_ok() && inflated;
                cases.push(json!({"case": "(i) custody staged 500_000, withdraw_strategy(500_000, nav = old = 500_000)", "sequence": seq_w,
                    "stage": tx_json(&r_stage), "tx": tx_json(&r_w), "before": snap_json(&b), "after": snap_json(&a),
                    "observed": "tv inflated by the swept cash while receipt2 stayed 500_000 -> book exceeds real by 500_000 (phantom NAV, the receipt>tv seed of the original brick)",
                    "matchesExpectation": inflated}));
            }
            // (ii) nothing staged, withdraw_strategy(0, nav = 0): pure write-down; books stay consistent but the 500_000 is stranded in the Squads ATA
            {
                let mut clone = svm.clone();
                let b = snap(&clone, Some(&s2));
                let (r_w, seq_w) = crank_withdraw(&mut clone, &s2, 0, 0);
                let a = snap(&clone, Some(&s2));
                let written_down = r_w.is_ok() && a.tv + alloc == b.tv && a.idle == b.idle && a.receipt2 == Some(0) && a.squads_usdc == b.squads_usdc;
                expectations_hold &= written_down;
                cases.push(json!({"case": "(ii) no cash staged, withdraw_strategy(0, nav = 0)", "sequence": seq_w,
                    "tx": tx_json(&r_w), "before": snap_json(&b), "after": snap_json(&a),
                    "observed": "tv written down by 500_000, receipt2 -> 0, idle unchanged; books consistent but the 500_000 USDC stays stranded in EBG2 (LP holders eat the loss)",
                    "matchesExpectation": written_down}));
            }
            // (iii) withdraw_strategy(500_000, nav = 0) with EMPTY custody -> adaptor rejects (InsufficientBridgeLiquidity = 12)
            {
                let mut clone = svm.clone();
                let b = snap(&clone, Some(&s2));
                let (r_w, seq_w) = crank_withdraw(&mut clone, &s2, alloc, 0);
                let a = snap(&clone, Some(&s2));
                let rejected = r_w.is_err() && a.tv == b.tv && a.receipt2 == b.receipt2;
                expectations_hold &= rejected;
                cases.push(json!({"case": "(iii) custody empty, withdraw_strategy(500_000, nav = 0)", "sequence": seq_w,
                    "tx": tx_json(&r_w), "error": err_of(&r_w), "observed": "rejected before any book change", "matchesExpectation": rejected}));
            }
            let mut c = vec![];
            chk(&mut c, "all three mismatch cases behave as recorded (i inflates, ii writes down, iii rejected)", expectations_hold);
            rec.push("R5a-hazards", "CLONE ONLY: NAV/cash mismatch hazards on strategy 2 (the real failure modes; never do (i) on mainnet)", &c, json!({
                "cases": cases,
                "stateRestored": "clones discarded; main line continues from the post-allocation state",
            }));
        }

        // correct withdraw shape for the custom adaptor (MAIN LINE): the manager
        // moves exactly `amount` from the Squads USDC ATA into the strategy custody
        // ATA (a Squads-signed SPL transfer), then withdraw_strategy(amount,
        // nav = old - amount). Voltr credits the custody balance it sweeps, so
        // tv' = tv - old + new + swept == tv, idle += amount, receipt2 -> 0.
        {
            let before = snap(&svm, Some(&s2));
            let r_stage = send(&mut svm, &[transfer_checked_ix(&key(SQUADS_USDC_ATA), &s2.custody, &key(SQUADS_VAULT), alloc)], &key(SQUADS_VAULT));
            let staged = snap(&svm, Some(&s2));
            let (r_w, seq_w) = crank_withdraw(&mut svm, &s2, alloc, 0);
            let after = snap(&svm, Some(&s2));
            let mut c = vec![];
            chk(&mut c, "manager SPL transfer EBG2 -> custody2 (500_000) executed", r_stage.is_ok() && staged.custody2 == Some(alloc) && staged.squads_usdc + alloc == before.squads_usdc);
            chk(&mut c, "withdraw_strategy(500_000, nav = old - 500_000 = 0) executed", r_w.is_ok());
            chk(&mut c, "receipt2 -> 0", after.receipt2 == Some(0));
            chk(&mut c, "idle += 500_000", after.idle == before.idle + alloc);
            chk(&mut c, "tv unchanged", after.tv == before.tv);
            chk(&mut c, "custody2 swept to 0", after.custody2 == Some(0));
            chk(&mut c, "tv == idle + receipt2 (book == real)", after.tv == after.idle + 0);
            rec.push("R5a-withdraw", "strategy 2 withdraw shape for the custom adaptor: stage 500_000 into custody, withdraw_strategy(500_000, nav=0)", &c, json!({
                "stage": tx_json(&r_stage), "sequence": seq_w, "tx": tx_json(&r_w),
                "before": snap_json(&before), "afterStage": snap_json(&staged), "after": snap_json(&after),
                "why": "Voltr sweeps the strategy custody ATA to idle after the adaptor CPI and credits the swept amount: tv' = tv - old_receipt + new_receipt + swept. With swept == amount and new = old - amount the book is unchanged. The custom adaptor only checks custody >= amount (InsufficientBridgeLiquidity otherwise) and moves nothing itself, so the cash must be staged by the manager (Squads) BEFORE the crank — that is the production VOLTR_RESTORE_IDLE shape.",
            }));
        }
    } else {
        rec.push("R5b", "fallback (Trustful adaptor 3pnpK…) — NOT ATTEMPTED because R5a succeeded/failed as recorded", &[("R5a setup failed; Trustful fallback not implemented in this run".into(), false)], json!({"trustfulAdaptor": TRUSTFUL_ADAPTOR}));
    }

    // ---------------------------------------------------------------- R6
    {
        // orphan check on clones: strategy 1's receipt (3_793_536 phantom).
        // Voltr applies tv' = tv - old + new with checked math, so a refresh at
        // nav == receipt1 is a harmless no-op, nav = 0 underflows (6004), and any
        // nav below receipt1 that tv can absorb is a silent WRITE-DOWN of tv.
        let mut orphan = vec![];
        let mut ok_all = true;
        let b0 = snap(&svm, Some(&s2));
        for (label, nav, expect_ok, expect_tv_delta) in [
            ("refresh nav = receipt1 (unchanged)", b0.receipt1, true, 0i128),
            ("refresh nav = 0 (try to zero the phantom)", 0u64, false, 0i128),
            ("HAZARD refresh nav = receipt1 - 100", b0.receipt1 - 100, true, -100i128),
        ] {
            let mut clone = svm.clone();
            let b = snap(&clone, Some(&s2));
            let (r, seq) = crank_deposit(&mut clone, &s1, 0, nav);
            let a = snap(&clone, Some(&s2));
            let matches = r.is_ok() == expect_ok && (a.tv as i128 - b.tv as i128) == expect_tv_delta;
            ok_all &= matches;
            orphan.push(json!({"case": label, "reportedNav": nav, "sequence": seq, "tx": tx_json(&r),
                "tvBefore": b.tv, "tvAfter": a.tv, "receipt1Before": b.receipt1, "receipt1After": a.receipt1,
                "executed": r.is_ok(), "error": err_of(&r), "matchesExpectation": matches}));
        }
        let mut c = vec![];
        chk(&mut c, "nav == receipt1 is a no-op; nav = 0 rejected with 6004 (MathOverflow); nav = receipt1-100 silently writes tv down by 100", ok_all);
        rec.push("R6-orphan", "CLONE ONLY: strategy 1 (old receipt 3GHL…) after the reset — never lower its NAV", &c, json!({
            "cases": orphan,
            "note": "receipt1.positionValue (3_793_536) is a phantom: tv' = tv - old + new underflows for nav = 0 (Voltr MathOverflow 6004 at vault.rs:603) and closeStrategy is rejected (2003, positionValue != 0). Any nav in (tv-shortfall, receipt1) is accepted and moves depositor value out of tv, so the REPORT_NAV policy must never target strategy 1 again.",
        }));

        // optional full close of the orphan on a CLONE: costs exactly receipt1 in
        // real USDC (deposit it, write the phantom down to 0, closeStrategy).
        {
            let mut clone = svm.clone();
            let usdc = key(USDC);
            let lp = key(LP_MINT);
            let admin_usdc = ata_for(&admin, &usdc);
            let cost = b0.receipt1;
            set_token_account(&mut clone, admin_usdc, usdc, admin, cost);
            let b = snap(&clone, Some(&s2));
            let r_dep = send(&mut clone, &[cu_ix(), create_ata_idempotent_ix(&admin, &admin, &lp), deposit_vault_ix(&admin, &admin_usdc, &key(ADMIN_LP_ATA), cost)], &admin);
            let mid = snap(&clone, Some(&s2));
            let (r_zero, seq_z) = crank_deposit(&mut clone, &s1, 0, 0);
            let after_zero = snap(&clone, Some(&s2));
            let payer = new_funded(&mut clone);
            let r_close = send(&mut clone, &[cu_ix(), close_strategy_ix(&payer, &s1)], &payer);
            let a = snap(&clone, Some(&s2));
            let receipt_gone = !account_exists(&clone, &s1.receipt);
            let stranded = a.idle as i128 - a.tv as i128 - a.receipt2.unwrap_or(0) as i128;
            let mut c = vec![];
            chk(&mut c, format!("admin deposit_vault({cost}) executed"), r_dep.is_ok() && mid.tv == b.tv + cost);
            chk(&mut c, "refresh strategy 1 with nav = 0 executed (tv back to pre-deposit level, receipt1 = 0)", r_zero.is_ok() && after_zero.tv == b.tv && after_zero.receipt1 == 0);
            chk(&mut c, "closeStrategy(strategy 1) executed and receipt account closed", r_close.is_ok() && receipt_gone);
            chk(&mut c, format!("the {cost} deposited USDC is now UNBOOKED idle (idle - tv - receipt2 == {cost}); D = tv - idle - receipts is conserved"), stranded == cost as i128 && a.gap1() == b.gap1());
            rec.push("R6-orphan-close", "CLONE ONLY (optional): retire strategy 1 by writing the phantom down — converts it into stranded unbooked idle, not into value", &c, json!({
                "costRawUsdc": cost, "deposit": tx_json(&r_dep), "zeroRefresh": {"sequence": seq_z, "tx": tx_json(&r_zero)}, "closeStrategy": tx_json(&r_close),
                "before": snap_json(&b), "afterDeposit": snap_json(&mid), "afterZeroRefresh": snap_json(&after_zero), "after": snap_json(&a),
                "unbookedIdleAfter": stranded.to_string(),
                "adminLpAfter": a.admin_lp, "adminLpValueAfter": format!("{:.0}", a.admin_lp as f64 * a.price()),
                "note": "Voltr conserves D = tv - idle - sum(receipts) (= -3_793_536 on this vault) under every instruction, so the phantom can only change form: a receipt-1 position that must never be lowered, or (after this write-down) 3_793_536 raw USDC sitting in the idle ATA that no LP claims. The write-down needs tv >= receipt1 first (hence the deposit) and its cost is socialised across LP holders at that moment. NOT recommended; leaving strategy 1 untouched is strictly better.",
            }));
        }
        let (c, body) = lifecycle(&mut svm, 1_000_000, Some(&s2));
        rec.push("R6-lifecycle", "fresh user deposit_vault + withdraw_vault still work with strategy 2 live", &c, body);
    }

    let final_state = snap(&svm, Some(&s2));
    {
        let mut c = vec![];
        chk(&mut c, format!("D = tv - idle - receipts conserved end to end ({} -> {})", -s_init.gap1(), -final_state.gap1()), s_init.gap1() == final_state.gap1());
        chk(&mut c, "final: tv == idle + receipt2 (book == real for the live strategy)", final_state.tv == final_state.idle + final_state.receipt2.unwrap_or(0));
        chk(&mut c, "final: no accumulated fee LP, fee 0, locked-profit degradation 0", final_state.fee_admin == 0 && final_state.admin_perf_bps == 0 && final_state.locked_deg == 0);
        chk(&mut c, "final: tv > 0 and lpSupplyInclFees > 0", final_state.tv > 0 && final_state.lp_incl_fees() > 0);
        rec.push("R7-invariants", "end-to-end invariants on the main-line instance", &c, json!({"initial": snap_json(&s_init), "final": snap_json(&final_state)}));
    }

    // ---------------------------------------------------------------- report
    let manifest: Value =
        serde_json::from_slice(&fs::read(fixtures_dir().join("_manifest.json")).unwrap()).unwrap();
    let pending_fx = load_fixture(PENDING_RECEIPT);
    let report = json!({
        "schema": "voltr-reset-litesvm/v1",
        "generatedBy": "crates/squads-test-harness/tests/voltr_reset_sequence.rs",
        "broadcast": false,
        "signatureProof": false,
        "squadsPolicyExecutionProof": false,
        "cluster": "mainnet-beta (accounts + program binaries cloned read-only)",
        "dumpSlot": base.slot0,
        "dumpClockUnixTimestamp": base.ts0,
        "dumpGenesis": manifest["genesis"],
        "pendingReceiptFixtureFetchedSlot": pending_fx["fetchedSlot"],
        "programs": {"voltr": VOLTR, "adaptor": ADAPTOR, "squadsSmartAccount": SQUADS},
        "vault": VAULT,
        "vaultAdmin": ADMIN,
        "vaultManager": SQUADS_VAULT,
        "protocolTreasury": PROTOCOL_TREASURY,
        "strategy1": strat_json(&s1),
        "strategy2": strat_json(&s2),
        "initialState": snap_json(&s_init),
        "finalState": snap_json(&final_state),
        "overallVerdict": if rec.all_ok { "PASS" } else { "FAIL" },
        "signerOverridesAndCaveats": [
            "LiteSVM built with with_sigverify(false) + with_blockhash_check(false); all transactions carry default (zero) signatures. Nothing was signed or broadcast.",
            "Vault admin BAqg… is marked a transaction signer directly for updateVaultConfig (R0), cancel/request/withdraw of its own LP (R3) and the seed deposit_vault (R5); its lamports are airdropped locally.",
            "The Squads vault PDA ST999… (= vault.manager) is marked a transaction signer directly for arm_report + deposit_strategy/withdraw_strategy cranks (R1, R5, R6) and Voltr initializeStrategy (R5). On mainnet these go through the Squads smart-account program: the existing REPORT_NAV policy covers the amount=0 refresh (R1); the allocation (R5) and initializeStrategy need the allocation policy / a Squads proposal.",
            "harvestFee (R2): the harvester that worked is recorded in R2.harvesterThatWorked; the manager (ST999) and protocol-treasury LP ATAs were created with permissionless ATA CreateIdempotent instructions.",
            "R3 as-is claim of the mainnet pending request is executed on a CLONE only; the main line cancels it (cancelRequestWithdrawVault) and re-requests all LP at the reset price.",
            "R4/R6 fresh users and the R5 seed deposit get their USDC via a token-balance override (set_account); no program/authority/policy changes.",
            "R5 strategy-2 config key is a fresh Pubkey::new_unique() acting as the config keypair signer (on mainnet: a fresh keypair). The custody ATA for the new strategy auth is created via ATA CreateIdempotent.",
            "R5 withdraw failure mode and R6 orphan probes run on CLONES of the main LiteSVM instance and are discarded; the main line is a single continuous instance.",
            "Every strategy crank advances the LiteSVM clock to a new epoch because Voltr stamps adaptorAddReceipt.lastUpdatedEpoch per (vault, adaptor) and rejects a second crank in the same epoch (6010). On mainnet that means at most one deposit/withdraw_strategy per epoch (~2 days) across BOTH strategies of this adaptor.",
            "The external OnRe RWA leg is not present in LiteSVM; the strategy-2 allocation's cash lands in the Squads USDC ATA (EBG2…) exactly as the production bridge does, but no onward RWA leg is modelled.",
        ],
        "steps": rec.steps,
    });

    let out = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../docs/evidence/voltr-reset-litesvm-2026-09-08.results.json");
    fs::write(&out, serde_json::to_string_pretty(&report).unwrap()).unwrap();
    eprintln!("wrote {}", out.display());
    eprintln!("final state: {}", serde_json::to_string_pretty(&report["finalState"]).unwrap());

    assert!(rec.all_ok, "one or more reset steps failed; see {}", out.display());
}

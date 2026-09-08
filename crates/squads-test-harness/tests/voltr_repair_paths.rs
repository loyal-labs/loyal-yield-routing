//! Voltr bricked-vault repair-path probes against the REAL deployed program
//! binaries (Voltr `vVoLTR…`, custom adaptor `FSj27…`, Squads smart-account
//! `SMRTz…`) cloned into LiteSVM from fresh mainnet `getAccountInfo` dumps.
//!
//! This is NOT a mainnet action, signature proof, or Squads-policy execution
//! proof. The NAV-refresh crank is built exactly as production builds it (the
//! 91-byte Voltr `deposit_strategy` v2 adaptor envelope + the 79-byte adaptor
//! `ArmReport`, see `tools/backyard-voltr/src/integrations/rwa-multiply-voltr.ts`
//! and `crates/loyal-actions/src/autonomous_vaults/voltr_custom.rs`), but the
//! Squads authorization that production routes through the Squads program +
//! installed REPORT_NAV policy is instead supplied by marking the Squads vault
//! PDA a transaction signer directly (LiteSVM sigverify disabled). Every such
//! override is recorded in the emitted results JSON.
//!
//! Fixtures live in `fixtures/voltr-repair/` (dumped read-only from
//! api.mainnet-beta at the slot recorded in `_manifest.json`).

#![allow(dead_code, clippy::identity_op)]
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
use std::{fs, path::PathBuf, str::FromStr};

// ---- deployed program ids -------------------------------------------------
const VOLTR: &str = "vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8";
const ADAPTOR: &str = "FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW";
const SQUADS: &str = "SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG";
const TOKEN: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const ATA_PROG: &str = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL";
const SYS: &str = "11111111111111111111111111111111";

// ---- state accounts (mainnet) ---------------------------------------------
const VAULT: &str = "HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA";
const RECEIPT: &str = "3GHLmyTTGH9ZfQqb3YCo9xKjpPhMLvHsq2JSYzCnk9U6";
const STRATEGY: &str = "9hDH4acTDrSjg9d5n8c1g53jMTonaDAUesp1diCWuuhj"; // v2 adaptor config
const TICKET: &str = "C71BFjq6PfgcWV4geoRudheupKnQBv6yN6uzYKthgAt5";
const IDLE_ATA: &str = "6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh";
const CUSTODY_ATA: &str = "FTDWN5Ay8tzYPJBJT4s2oZaHRQ7jKPo8XP2ZRWb5GP3M"; // strategy asset ATA
const SQUADS_USDC_ATA: &str = "EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe";
const SETTINGS: &str = "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6";
const SQUADS_VAULT: &str = "ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"; // manager
const USDC: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const LP_MINT: &str = "6tNheTBYSpQkfMLhcczKgmTLSGffK54npKMG1WQR2tvb";
const ADMIN: &str = "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ";
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
const VOLTR_CLOSE_STRATEGY: [u8; 8] = [56, 247, 170, 246, 89, 221, 134, 200];
const VOLTR_INITIALIZE_STRATEGY: [u8; 8] = [208, 119, 144, 145, 178, 57, 105, 252];
const VOLTR_REMOVE_ADAPTOR: [u8; 8] = [161, 199, 99, 22, 25, 193, 61, 193];
const VOLTR_UPDATE_ADAPTOR_POLICY: [u8; 8] = [20, 101, 232, 123, 189, 18, 133, 230];
const VOLTR_INSTANT_WITHDRAW_STRATEGY: [u8; 8] = [105, 57, 166, 130, 147, 221, 250, 189];
const ADAPTOR_DEPOSIT: [u8; 8] = [242, 35, 198, 137, 82, 225, 242, 182];
const ADAPTOR_WITHDRAW: [u8; 8] = [183, 18, 70, 156, 148, 109, 161, 34];
const ADAPTOR_ARM_REPORT: [u8; 8] = [164, 175, 246, 41, 178, 140, 35, 3];

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

/// UpgradeableLoaderState::ProgramData header is 45 bytes; the ELF follows.
fn program_elf(programdata_addr: &str) -> Vec<u8> {
    let raw = fixture_data(programdata_addr);
    assert_eq!(&raw[45..49], b"\x7fELF", "programdata ELF magic");
    raw[45..].to_vec()
}

fn u64_le(b: &[u8], o: usize) -> u64 {
    u64::from_le_bytes(b[o..o + 8].try_into().unwrap())
}
fn u128_le(b: &[u8], o: usize) -> u128 {
    u128::from_le_bytes(b[o..o + 16].try_into().unwrap())
}

// SPL token account amount at offset 64.
fn token_amount(svm: &LiteSVM, addr: &str) -> u64 {
    match svm.get_account(&key(addr)) {
        Some(a) if a.data.len() >= 72 => u64_le(&a.data, 64),
        _ => 0,
    }
}
// SPL mint supply at offset 36.
fn mint_supply(svm: &LiteSVM, addr: &str) -> u64 {
    u64_le(&svm.get_account(&key(addr)).unwrap().data, 36)
}

#[derive(Clone, Debug)]
struct State {
    tv: u64,
    receipt_pv: u64,
    idle: u64,
    custody: u64,
    squads_usdc: u64,
    lp_supply: u64,
    fee_manager: u64,
    fee_admin: u64,
    fee_protocol: u64,
    hwm: u128,
}

fn read_state(svm: &LiteSVM) -> State {
    let vault = svm.get_account(&key(VAULT)).unwrap().data;
    let receipt = svm.get_account(&key(RECEIPT)).unwrap().data;
    State {
        tv: u64_le(&vault, 168),
        receipt_pv: u64_le(&receipt, 104),
        idle: token_amount(svm, IDLE_ATA),
        custody: token_amount(svm, CUSTODY_ATA),
        squads_usdc: token_amount(svm, SQUADS_USDC_ATA),
        lp_supply: mint_supply(svm, LP_MINT),
        fee_manager: u64_le(&vault, 576),
        fee_admin: u64_le(&vault, 584),
        fee_protocol: u64_le(&vault, 592),
        hwm: u128_le(&vault, 624),
    }
}

/// (lockedProfitState.lastUpdatedLockedProfit@672, lastReport@680)
fn locked_profit(svm: &LiteSVM) -> (u64, u64) {
    let v = svm.get_account(&key(VAULT)).unwrap().data;
    (u64_le(&v, 672), u64_le(&v, 680))
}

fn state_json(s: &State) -> Value {
    json!({
        "vaultTotalValue": s.tv,
        "receiptPositionValue": s.receipt_pv,
        "idleAta": s.idle,
        "custodyAta": s.custody,
        "squadsUsdcAta": s.squads_usdc,
        "lpSupply": s.lp_supply,
        "feeStateAccumulatedLpManagerFees": s.fee_manager,
        "feeStateAccumulatedLpAdminFees": s.fee_admin,
        "feeStateAccumulatedLpProtocolFees": s.fee_protocol,
        "highWaterMarkAssetPerLpBits": s.hwm.to_string(),
    })
}

/// LiteSVM loaded with the three deployed programs + all cloned state accounts,
/// clock advanced to the dump slot. `slot0` is the dump slot.
struct Base {
    svm: LiteSVM,
    slot0: u64,
    ts0: i64,
}

fn build_base() -> Base {
    let manifest: Value =
        serde_json::from_slice(&fs::read(fixtures_dir().join("_manifest.json")).unwrap()).unwrap();
    let slot0 = manifest["slot"].as_u64().unwrap();

    let mut svm = LiteSVM::new()
        .with_sigverify(false)
        .with_blockhash_check(false)
        .with_transaction_history(0);

    // deployed programs from their programdata ELFs
    svm.add_program(key(VOLTR), &program_elf("3fiAyUjktZkZf6hcbBPy6U6UdkMdEFoToS4sjtzAd5az"))
        .unwrap();
    svm.add_program(
        key(ADAPTOR),
        &program_elf("DrvzixaVmAuPVVJPtP5wykb9mvgDWqZbvZau9oiCUpHu"),
    )
    .unwrap();
    svm.add_program(
        key(SQUADS),
        &program_elf("2g3u9qgz4adKQVN1TUoh7bbBKqaSsjXtz1yX2ptagW5T"),
    )
    .unwrap();

    // cloned state accounts (non-executable)
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
    ] {
        set_account_from_fixture(&mut svm, addr);
    }
    // Squads vault PDA (system-owned, 0 data) — give it rent lamports so it can
    // exist as a signer account.
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

    // Clone the real mainnet Clock sysvar so slot AND epoch are consistent.
    // Voltr's deposit_strategy rejects with AdaptorEpochInvalid (6010) when
    // clock.epoch == adaptor_add_receipt.lastUpdatedEpoch (0 on-chain); the real
    // epoch (1030) clears that gate, exactly as the 11 live phase-2 refreshes did.
    let clock_raw = fixture_data("SysvarC1ock11111111111111111111111111111111");
    let mut clock: Clock = svm.get_sysvar();
    clock.slot = u64_le(&clock_raw, 0);
    clock.epoch_start_timestamp = i64::from_le_bytes(clock_raw[8..16].try_into().unwrap());
    clock.epoch = u64_le(&clock_raw, 16);
    clock.leader_schedule_epoch = u64_le(&clock_raw, 24);
    clock.unix_timestamp = i64::from_le_bytes(clock_raw[32..40].try_into().unwrap());
    svm.set_sysvar(&clock);
    let slot0 = clock.slot;
    let ts0 = clock.unix_timestamp;

    Base { svm, slot0, ts0 }
}

const SLOTS_PER_EPOCH: u64 = 432_000;

/// Advance the clock to a brand-new epoch (slot + 432k, epoch + 1). Voltr's
/// deposit_strategy stamps adaptor_add_receipt.lastUpdatedEpoch = clock.epoch and
/// rejects a repeat in the same epoch, so every strategy crank needs a fresh one.
fn advance_epoch(svm: &mut LiteSVM) {
    let mut clock: Clock = svm.get_sysvar();
    clock.slot += SLOTS_PER_EPOCH;
    clock.epoch += 1;
    clock.leader_schedule_epoch = clock.epoch + 1;
    clock.unix_timestamp += 172_800;
    clock.epoch_start_timestamp = clock.unix_timestamp;
    svm.set_sysvar(&clock);
}

fn set_clock(svm: &mut LiteSVM, slot: u64, ts: i64) {
    let mut clock: Clock = svm.get_sysvar();
    clock.slot = slot;
    clock.unix_timestamp = ts;
    svm.set_sysvar(&clock);
}

fn fund(svm: &mut LiteSVM, who: &Pubkey, lamports: u64) {
    svm.airdrop(who, lamports).unwrap();
}

/// Send a transaction with `fee_payer` first and all-default signatures
/// (sigverify disabled). Returns Ok((compute_units, logs)) or Err((error,logs)).
fn send(
    svm: &mut LiteSVM,
    ixs: &[Instruction],
    fee_payer: &Pubkey,
) -> Result<(u64, Vec<String>), (String, Vec<String>)> {
    let msg = Message::new_with_blockhash(ixs, Some(fee_payer), &svm.latest_blockhash());
    let num = msg.header.num_required_signatures as usize;
    let tx = Transaction {
        signatures: vec![Signature::default(); num],
        message: msg,
    };
    match svm.send_transaction(tx) {
        Ok(m) => Ok((m.compute_units_consumed, m.logs)),
        Err(f) => Err((format!("{:?}", f.err), f.meta.logs)),
    }
}

fn meta(am: bool, aw: bool, addr: &str) -> AccountMeta {
    AccountMeta {
        pubkey: key(addr),
        is_signer: am,
        is_writable: aw,
    }
}

// ---- crank builders -------------------------------------------------------

/// The 57-byte ReportV1 tail (version || sequence || observed_slot || nav ||
/// digest). `seq` == `observed_slot` == the clock slot the adaptor authorises.
fn report_v1(seq: u64, nav: u64) -> Vec<u8> {
    let mut r = vec![1u8]; // version
    r.extend_from_slice(&seq.to_le_bytes());
    r.extend_from_slice(&seq.to_le_bytes()); // observed_slot == sequence
    r.extend_from_slice(&nav.to_le_bytes());
    r.extend_from_slice(&[7u8; 32]); // nonzero snapshot digest
    let _ = nav;
    r
}

/// Voltr deposit_strategy / withdraw_strategy 91-byte "v2 adaptor envelope".
fn voltr_capital_data(outer: [u8; 8], amount: u64, inner: [u8; 8], seq: u64, nav: u64) -> Vec<u8> {
    let mut d = Vec::with_capacity(91);
    d.extend_from_slice(&outer);
    d.extend_from_slice(&amount.to_le_bytes());
    d.push(1); // Some(instructionDiscriminator)
    d.extend_from_slice(&8u32.to_le_bytes());
    d.extend_from_slice(&inner);
    d.push(1); // Some(additionalArgs)
    d.extend_from_slice(&57u32.to_le_bytes());
    d.extend_from_slice(&report_v1(seq, nav));
    assert_eq!(d.len(), 91);
    d
}

/// Adaptor ArmReport 79-byte wire.
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

fn arm_report_ix(operation: u8, amount: u64, seq: u64, nav: u64) -> Instruction {
    Instruction {
        program_id: key(ADAPTOR),
        accounts: vec![
            meta(false, false, STRATEGY),     // 0 config
            meta(false, true, TICKET),        // 1 ticket (w)
            meta(false, false, SETTINGS),     // 2 settings
            meta(true, false, SQUADS_VAULT),  // 3 squads vault (signer)
            meta(false, false, SQUADS),       // 4 squads program
        ],
        data: arm_report_data(operation, amount, seq, nav),
    }
}

/// Voltr deposit_strategy (18 accounts) as production builds it.
fn deposit_strategy_ix(amount: u64, seq: u64, nav: u64) -> Instruction {
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            meta(true, false, SQUADS_VAULT),       // 0 manager (signer)
            meta(false, false, PROTOCOL),          // 1 protocol
            meta(false, true, VAULT),              // 2 vault (w)
            meta(false, false, STRATEGY),          // 3 strategy (adaptor config)
            meta(false, false, ADAPTOR_ADD_RECEIPT), // 4 adaptor add receipt
            meta(false, true, RECEIPT),            // 5 strategy init receipt (w)
            meta(false, true, IDLE_AUTH),          // 6 idle auth (w)
            meta(false, true, STRATEGY_AUTH),      // 7 strategy auth (w)
            meta(false, true, USDC),               // 8 asset mint (w)
            meta(false, false, LP_MINT),           // 9 lp mint
            meta(false, true, IDLE_ATA),           // 10 idle ata (w)
            meta(false, true, CUSTODY_ATA),        // 11 strategy asset ata (w)
            meta(false, false, TOKEN),             // 12 asset token program
            meta(false, false, ADAPTOR),           // 13 adaptor program
            meta(false, false, SETTINGS),          // 14 settings (remaining)
            meta(true, false, SQUADS_VAULT),       // 15 squads vault (signer)
            meta(false, true, SQUADS_USDC_ATA),    // 16 squads asset ata (w)
            meta(false, true, TICKET),             // 17 report ticket (w)
        ],
        data: voltr_capital_data(VOLTR_DEPOSIT_STRATEGY, amount, ADAPTOR_DEPOSIT, seq, nav),
    }
}

/// Voltr withdraw_strategy (18 accounts).
fn withdraw_strategy_ix(amount: u64, seq: u64, nav: u64) -> Instruction {
    // withdraw account order differs from deposit (from voltr_custom bound
    // indexes: 0 manager,2 vault,5 strategy,6 adaptor,9 mint,12 strat ata,
    // 13 token,14 settings,15 manager,16 squads ata,17 ticket).
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            meta(true, false, SQUADS_VAULT),       // 0 manager (signer)
            meta(false, false, PROTOCOL),          // 1 protocol
            meta(false, true, VAULT),              // 2 vault (w)
            meta(false, false, ADAPTOR_ADD_RECEIPT), // 3 adaptor add receipt
            meta(false, true, RECEIPT),            // 4 strategy init receipt (w)
            meta(false, false, STRATEGY),          // 5 strategy (adaptor config)
            meta(false, false, ADAPTOR),           // 6 adaptor program
            meta(false, true, IDLE_AUTH),          // 7 idle auth (w)
            meta(false, true, STRATEGY_AUTH),      // 8 strategy auth (w)
            meta(false, true, USDC),               // 9 asset mint (w)
            meta(false, false, LP_MINT),           // 10 lp mint
            meta(false, true, IDLE_ATA),           // 11 idle ata (w)
            meta(false, true, CUSTODY_ATA),        // 12 strategy asset ata (w)
            meta(false, false, TOKEN),             // 13 token program
            meta(false, false, SETTINGS),          // 14 settings
            meta(true, false, SQUADS_VAULT),       // 15 squads vault (signer)
            meta(false, true, SQUADS_USDC_ATA),    // 16 squads asset ata (w)
            meta(false, true, TICKET),             // 17 ticket (w)
        ],
        data: voltr_capital_data(VOLTR_WITHDRAW_STRATEGY, amount, ADAPTOR_WITHDRAW, seq, nav),
    }
}

/// Build the NAV-refresh (or allocation) crank = [arm_report, deposit_strategy]
/// in one transaction. `operation` 0=deposit path.
fn crank_refresh(svm: &mut LiteSVM, payer: &Pubkey, amount: u64, nav: u64) -> Value {
    advance_epoch(svm);
    let clock: Clock = svm.get_sysvar();
    let seq = clock.slot;
    let ixs = vec![
        solana_sdk::compute_budget::ComputeBudgetInstruction::set_compute_unit_limit(1_400_000),
        arm_report_ix(0, amount, seq, nav),
        deposit_strategy_ix(amount, seq, nav),
    ];
    let before = read_state(svm);
    let result = send(svm, &ixs, payer);
    let after = read_state(svm);
    match result {
        Ok((cu, logs)) => json!({
            "ok": true, "seq": seq, "amount": amount, "reportedNav": nav,
            "computeUnits": cu, "before": state_json(&before), "after": state_json(&after),
            "logsTail": logs.iter().rev().take(6).rev().collect::<Vec<_>>(),
        }),
        Err((err, logs)) => json!({
            "ok": false, "seq": seq, "amount": amount, "reportedNav": nav,
            "error": err, "before": state_json(&before), "after": state_json(&after),
            "logsTail": logs.iter().rev().take(10).rev().collect::<Vec<_>>(),
        }),
    }
}

// ---- token / pda helpers --------------------------------------------------
use spl_token::{
    solana_program::{program_option::COption, program_pack::Pack},
    state::{Account as SplAccount, AccountState},
};

fn ata_for(owner: &Pubkey, mint: &Pubkey) -> Pubkey {
    Pubkey::find_program_address(
        &[owner.as_ref(), key(TOKEN).as_ref(), mint.as_ref()],
        &key(ATA_PROG),
    )
    .0
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
        Account {
            lamports: 2_039_280,
            data,
            owner: key(TOKEN),
            executable: false,
            rent_epoch: 0,
        },
    )
    .unwrap();
}

fn cu_ix() -> Instruction {
    solana_sdk::compute_budget::ComputeBudgetInstruction::set_compute_unit_limit(1_400_000)
}

// ---- T0: calibration ------------------------------------------------------
fn t0(base: &Base) -> Value {
    let mut cases = vec![];
    for nav in [0u64, 118, 119] {
        let mut svm = base.svm.clone();
        let payer = Pubkey::new_unique();
        fund(&mut svm, &payer, 100_000_000);
        let res = crank_refresh(&mut svm, &payer, 0, nav);
        let ok = res["ok"] == true;
        let after_tv = res["after"]["vaultTotalValue"].as_u64().unwrap();
        let after_pv = res["after"]["receiptPositionValue"].as_u64().unwrap();
        let expect_pass = nav >= 119;
        let verdict = if ok == expect_pass
            && (!ok || (nav == 119 && after_tv == 0 && after_pv == 119))
        {
            "PASS"
        } else {
            "FAIL"
        };
        cases.push(json!({
            "reportedNav": nav, "succeeded": ok, "expectSucceed": expect_pass,
            "resultTv": after_tv, "resultReceipt": after_pv, "verdict": verdict,
            "error": res.get("error").cloned().unwrap_or(Value::Null),
            "execution": res,
        }));
    }
    json!({"name": "T0 calibration (nav boundary)", "cases": cases})
}

// ---- T1: THE REPAIR -------------------------------------------------------
/// Repair nav so tv_new == idle: nav = idle + (receipt - tv).
fn repair_nav(base: &Base) -> u64 {
    let s = read_state(&base.svm);
    s.idle + (s.receipt_pv - s.tv)
}

fn apply_repair(base: &Base) -> (LiteSVM, Value) {
    let mut svm = base.svm.clone();
    let payer = Pubkey::new_unique();
    fund(&mut svm, &payer, 100_000_000);
    let nav = repair_nav(base);
    let res = crank_refresh(&mut svm, &payer, 0, nav);
    (svm, res)
}

fn t1(base: &Base) -> (Value, LiteSVM) {
    let before = read_state(&base.svm);
    let (svm, res) = apply_repair(base);
    let after = read_state(&svm);
    let nav = repair_nav(base);
    let tv_ok = after.tv == before.idle;
    let receipt_ok = after.receipt_pv == nav;
    let lp_delta = after.lp_supply as i128 - before.lp_supply as i128;
    let fee_admin_delta = after.fee_admin as i128 - before.fee_admin as i128;
    let fee_mgr_delta = after.fee_manager as i128 - before.fee_manager as i128;
    let hwm_changed = after.hwm != before.hwm;
    let verdict = if res["ok"] == true && tv_ok && receipt_ok {
        "PASS"
    } else {
        "FAIL"
    };
    let value = json!({
        "name": "T1 THE REPAIR (deposit_strategy(0) report nav = idle + deficit)",
        "reportedNav": nav,
        "verdict": verdict,
        "expected": {"tv": before.idle, "receipt": nav},
        "actual": {"tv": after.tv, "receipt": after.receipt_pv},
        "feeAndHwmSideEffects": {
            "lpSupplyDelta": lp_delta.to_string(),
            "feeStateAdminDelta": fee_admin_delta.to_string(),
            "feeStateManagerDelta": fee_mgr_delta.to_string(),
            "hwmBefore": before.hwm.to_string(),
            "hwmAfter": after.hwm.to_string(),
            "hwmChanged": hwm_changed,
            "performanceFeeFired": lp_delta != 0 || fee_admin_delta != 0 || fee_mgr_delta != 0,
        },
        "execution": res,
    });
    (value, svm)
}

// ---- T2: fair drain after repair -----------------------------------------
const LP_HOLDER_ATA: &str = "C35aUCiMtQa7Zou8Jag5qRbMHqvnVarPwkSwYRgshcC1";
const LP_HOLDER_OWNER: &str = "8eufrxGC9Djf7ekcoWnyewKvYz4GgjtmLLpB8HBji99e";

fn request_withdraw_vault_ix(
    user: &Pubkey,
    receipt: &Pubkey,
    escrow: &Pubkey,
    amount_lp: u64,
) -> Instruction {
    let mut data = VOLTR_REQUEST_WITHDRAW_VAULT.to_vec();
    data.extend_from_slice(&amount_lp.to_le_bytes());
    data.push(1); // isAmountInLp = true
    data.push(1); // isWithdrawAll = true
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            AccountMeta::new(*user, true),              // 0 payer (w,signer)
            AccountMeta::new_readonly(*user, true),     // 1 userTransferAuthority (signer)
            meta(false, false, PROTOCOL),               // 2 protocol
            meta(false, false, VAULT),                  // 3 vault (ro)
            meta(false, false, LP_MINT),                // 4 vault lp mint
            meta(false, true, LP_HOLDER_ATA),           // 5 user lp ata (w)
            AccountMeta::new(*escrow, false),           // 6 request withdraw lp ata (w)
            AccountMeta::new(*receipt, false),          // 7 request withdraw vault receipt (w)
            meta(false, false, TOKEN),                  // 8 lp token program
            meta(false, false, SYS),                    // 9 system program
        ],
        data,
    }
}

fn withdraw_vault_ix(user: &Pubkey, receipt: &Pubkey, escrow: &Pubkey, user_asset: &Pubkey) -> Instruction {
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            AccountMeta::new(*user, true),              // 0 userTransferAuthority (w,signer)
            meta(false, false, PROTOCOL),               // 1 protocol
            meta(false, true, VAULT),                   // 2 vault (w)
            meta(false, false, USDC),                   // 3 vault asset mint
            meta(false, true, LP_MINT),                 // 4 vault lp mint (w)
            AccountMeta::new(*escrow, false),           // 5 request withdraw lp ata (w)
            meta(false, true, IDLE_ATA),                // 6 vault asset idle ata (w)
            meta(false, true, IDLE_AUTH),               // 7 vault asset idle auth (w)
            AccountMeta::new(*user_asset, false),       // 8 user asset ata (w)
            AccountMeta::new(*receipt, false),          // 9 request withdraw vault receipt (w)
            meta(false, false, TOKEN),                  // 10 asset token program
            meta(false, false, TOKEN),                  // 11 lp token program
            meta(false, false, SYS),                    // 12 system program
        ],
        data: VOLTR_WITHDRAW_VAULT.to_vec(),
    }
}

fn t2(base: &Base) -> Value {
    let (mut svm, repair) = apply_repair(base);
    if repair["ok"] != true {
        return json!({"name":"T2 fair drain","verdict":"BLOCKED","reason":"repair (T1) failed","repair":repair});
    }
    // clone the sole LP holder token account (owner 8eufrx, 100% of supply)
    set_account_from_fixture(&mut svm, LP_HOLDER_ATA);
    let user = key(LP_HOLDER_OWNER);
    fund(&mut svm, &user, 1_000_000_000);
    let lp_amount = token_amount(&svm, LP_HOLDER_ATA);
    let s_before = read_state(&svm);

    let (receipt, _) = Pubkey::find_program_address(
        &[b"request_withdraw_vault_receipt", key(VAULT).as_ref(), user.as_ref()],
        &key(VOLTR),
    );
    let escrow = ata_for(&receipt, &key(LP_MINT));
    // idempotent escrow: seed empty LP token account owned by the receipt PDA
    set_token_account(&mut svm, escrow, key(LP_MINT), receipt, 0);
    // user USDC destination ATA
    let user_asset = ata_for(&user, &key(USDC));
    set_token_account(&mut svm, user_asset, key(USDC), user, 0);

    // Unlock any locked profit from the repair: advance clock past the locked
    // profit degradation window BEFORE requesting, so the request prices at the
    // full repaired NAV (no wall-time claim, only the local clock advances).
    let locked_after_repair = locked_profit(&svm);
    let clock_at_repair: Clock = svm.get_sysvar();
    let cur: Clock = svm.get_sysvar();
    set_clock(&mut svm, cur.slot + 1_000, cur.unix_timestamp + 500_000);
    let locked_after_wait = locked_profit(&svm);
    let clock_at_request: Clock = svm.get_sysvar();

    let req = send(
        &mut svm,
        &[cu_ix(), request_withdraw_vault_ix(&user, &receipt, &escrow, lp_amount)],
        &user,
    );
    let req_json = match &req {
        Ok((cu, logs)) => json!({"ok": true, "computeUnits": cu,
            "escrowLpAfter": token_amount(&svm, &escrow.to_string()),
            "userLpAfter": token_amount(&svm, LP_HOLDER_ATA),
            "logs": logs}),
        Err((e, logs)) => json!({"ok": false, "error": e, "logs": logs}),
    };
    if req.is_err() {
        return json!({"name":"T2 fair drain","verdict":"FAIL","stage":"requestWithdrawVault",
            "lpAmount": lp_amount, "request": req_json});
    }
    // decode receipt for the escrowed amounts
    let receipt_data = svm.get_account(&receipt).map(|a| a.data).unwrap_or_default();
    // RequestWithdrawVaultReceipt: amountLpEscrowed@72, withdrawableFromTs@96.
    let escrowed_lp = if receipt_data.len() >= 80 { u64_le(&receipt_data, 72) } else { 0 };
    // amountAssetToWithdrawDecimalBits is Q64.64 fixed point (integer part >> 64)
    let asset_bits = if receipt_data.len() >= 96 { u128_le(&receipt_data, 80) } else { 0 };
    let asset_int = (asset_bits >> 64) as u64;
    let withdrawable_from_ts = if receipt_data.len() >= 104 { u64_le(&receipt_data, 96) } else { 0 };

    // advance past the withdrawal waiting period (600s) measured from request
    let cur2: Clock = svm.get_sysvar();
    set_clock(&mut svm, cur2.slot + 100, withdrawable_from_ts as i64 + 5);

    let idle_before_claim = token_amount(&svm, IDLE_ATA);
    let claim = send(
        &mut svm,
        &[cu_ix(), withdraw_vault_ix(&user, &receipt, &escrow, &user_asset)],
        &user,
    );
    let payout = token_amount(&svm, &user_asset.to_string());
    let s_after = read_state(&svm);
    let claim_json = match &claim {
        Ok((cu, logs)) => json!({"ok": true, "computeUnits": cu, "logs": logs}),
        Err((e, logs)) => json!({"ok": false, "error": e, "logs": logs}),
    };
    // fair price: holder has 100% of LP, so payout should be ~ full tv at repair
    let fair = s_before.tv;
    let within = claim.is_ok() && payout > 0 && (payout as i128 - fair as i128).abs() as u64 * 10_000 / fair.max(1) <= 100; // <=1%
    json!({
        "name": "T2 fair drain after repair (requestWithdrawVault -> wait -> withdrawVault)",
        "verdict": if within {"PASS"} else if claim.is_ok() {"PARTIAL"} else {"FAIL"},
        "lpAmountBurned": lp_amount,
        "lpShareBps": 10_000u64, // sole holder = 100%
        "escrowedLp": escrowed_lp,
        "receiptAmountAssetToWithdrawInt": asset_int,
        "lockedProfit": {
            "afterRepair": {"lastUpdatedLockedProfit": locked_after_repair.0, "lastReport": locked_after_repair.1},
            "afterClockAdvance": {"lastUpdatedLockedProfit": locked_after_wait.0, "lastReport": locked_after_wait.1},
            "repairClockTs": clock_at_repair.unix_timestamp,
            "requestClockTs": clock_at_request.unix_timestamp,
            "degradationSeconds": 86_400,
        },
        "repairedTv": s_before.tv,
        "payoutRaw": payout,
        "expectedFairPayout": fair,
        "idleBeforeClaim": idle_before_claim,
        "idleAfterClaim": s_after.idle,
        "lpSupplyBefore": s_before.lp_supply,
        "lpSupplyAfter": s_after.lp_supply,
        "noUnderflow": claim.is_ok(),
        "request": req_json,
        "claim": claim_json,
    })
}

// ---- T3: continued operation after repair (offset discipline) -------------
fn deposit_vault_ix(user: &Pubkey, user_usdc: &Pubkey, user_lp: &Pubkey, amount: u64) -> Instruction {
    let mut data = VOLTR_DEPOSIT_VAULT.to_vec();
    data.extend_from_slice(&amount.to_le_bytes());
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            AccountMeta::new_readonly(*user, true),  // 0 userTransferAuthority (signer)
            meta(false, false, PROTOCOL),            // 1 protocol
            meta(false, true, VAULT),                // 2 vault (w)
            meta(false, false, USDC),                // 3 asset mint
            meta(false, true, LP_MINT),              // 4 lp mint (w)
            AccountMeta::new(*user_usdc, false),     // 5 user asset ata (w)
            meta(false, true, IDLE_ATA),             // 6 vault idle ata (w)
            meta(false, false, IDLE_AUTH),           // 7 idle auth
            AccountMeta::new(*user_lp, false),       // 8 user lp ata (w)
            meta(false, false, LP_MINT_AUTH),        // 9 lp mint auth
            meta(false, false, TOKEN),               // 10 asset token program
            meta(false, false, TOKEN),               // 11 lp token program
            meta(false, false, SYS),                 // 12 system program
        ],
        data,
    }
}

fn t3(base: &Base) -> Value {
    let (mut svm, repair) = apply_repair(base);
    if repair["ok"] != true {
        return json!({"name":"T3 continued operation","verdict":"BLOCKED","reason":"repair failed"});
    }
    let mut steps = vec![];
    let mut all_ok = true;

    // step A: fresh-user deposit_vault(1_000_000)
    let user = Pubkey::new_unique();
    fund(&mut svm, &user, 1_000_000_000);
    let user_usdc = ata_for(&user, &key(USDC));
    let user_lp = ata_for(&user, &key(LP_MINT));
    set_token_account(&mut svm, user_usdc, key(USDC), user, 1_000_000);
    set_token_account(&mut svm, user_lp, key(LP_MINT), user, 0);
    let before_a = read_state(&svm);
    let res_a = send(&mut svm, &[cu_ix(), deposit_vault_ix(&user, &user_usdc, &user_lp, 1_000_000)], &user);
    let after_a = read_state(&svm);
    let a_ok = res_a.is_ok()
        && after_a.tv == before_a.tv + 1_000_000
        && after_a.idle == before_a.idle + 1_000_000
        && after_a.receipt_pv == before_a.receipt_pv;
    all_ok &= a_ok;
    steps.push(json!({"step":"A deposit_vault(1_000_000)","ok":res_a.is_ok(),"verdict":if a_ok {"PASS"} else {"FAIL"},
        "expectedTv":before_a.tv+1_000_000,"tv":after_a.tv,"idle":after_a.idle,"receipt":after_a.receipt_pv,
        "userLpMinted":token_amount(&svm,&user_lp.to_string()),
        "error":res_a.as_ref().err().map(|(e,_)|e.clone())}));

    // step B: allocate deposit_strategy(500_000). Voltr moves the 500_000 from
    // the vault idle ATA into the strategy custody ATA and the custom adaptor
    // forwards it custody -> Squads USDC ATA inside the CPI. Voltr books
    // tv' = tv - old_receipt + new_receipt - amount, so with nav =
    // old_receipt + 500_000 the total value is UNCHANGED (idle -500_000,
    // receipt +500_000) — that is the correct Voltr behaviour, not a rise.
    // The Squads ATA is pre-funded only to mirror the production staging.
    if all_ok {
        advance_epoch(&mut svm);
        let clock: Clock = svm.get_sysvar();
        // stage 500_000 into the squads USDC ATA (models the allocate step)
        set_token_account(&mut svm, key(SQUADS_USDC_ATA), key(USDC), key(SQUADS_VAULT), 500_000);
        let before_b = read_state(&svm);
        let nav_b = before_b.receipt_pv + 500_000;
        let ixs = vec![cu_ix(), arm_report_ix(0, 500_000, clock.slot, nav_b), deposit_strategy_ix(500_000, clock.slot, nav_b)];
        let pb = Pubkey::new_unique();
        fund(&mut svm, &pb, 100_000_000);
        let res_b = send(&mut svm, &ixs, &pb);
        let after_b = read_state(&svm);
        let b_ok = res_b.is_ok()
            && after_b.custody == 0
            && after_b.tv == before_b.tv
            && after_b.idle == before_b.idle - 500_000
            && after_b.squads_usdc == before_b.squads_usdc + 500_000
            && after_b.receipt_pv == nav_b;
        all_ok &= b_ok;
        steps.push(json!({"step":"B allocation deposit_strategy(500_000) nav=old_receipt+500_000","ok":res_b.is_ok(),
            "verdict":if b_ok {"PASS"} else {"FAIL"},"reportedNav":nav_b,
            "expectedTv":before_b.tv,"tv":after_b.tv,"idle":after_b.idle,"custody":after_b.custody,
            "squadsUsdc":after_b.squads_usdc,"receipt":after_b.receipt_pv,
            "error":res_b.as_ref().err().map(|(e,_)|e.clone())}));
    }

    // step C: withdraw_strategy(200_000) with custody pre-loaded to exactly
    // 200_000 (the manager's Squads -> custody restore transfer). Voltr sweeps
    // custody to idle after the adaptor CPI and credits the swept amount:
    // tv' = tv - old_receipt + new_receipt + swept, so with nav =
    // old_receipt - 200_000 the total value is again UNCHANGED (idle
    // +200_000, receipt -200_000, custody 0).
    if all_ok {
        advance_epoch(&mut svm);
        let clock: Clock = svm.get_sysvar();
        set_token_account(&mut svm, key(CUSTODY_ATA), key(USDC), key(STRATEGY_AUTH), 200_000);
        let before_c = read_state(&svm);
        let nav_c = before_c.receipt_pv - 200_000;
        let ixs = vec![cu_ix(), arm_report_ix(1, 200_000, clock.slot, nav_c), withdraw_strategy_ix(200_000, clock.slot, nav_c)];
        let pc = Pubkey::new_unique();
        fund(&mut svm, &pc, 100_000_000);
        let res_c = send(&mut svm, &ixs, &pc);
        let after_c = read_state(&svm);
        let c_ok = res_c.is_ok()
            && after_c.custody == 0
            && after_c.tv == before_c.tv
            && after_c.idle == before_c.idle + 200_000
            && after_c.receipt_pv == nav_c;
        all_ok &= c_ok;
        steps.push(json!({"step":"C withdraw_strategy(200_000) nav=old_receipt-200_000","ok":res_c.is_ok(),
            "verdict":if c_ok {"PASS"} else {"FAIL"},"reportedNav":nav_c,
            "expectedTv":before_c.tv,"tv":after_c.tv,"idle":after_c.idle,"custody":after_c.custody,
            "squadsUsdc":after_c.squads_usdc,"receipt":after_c.receipt_pv,
            "error":res_c.as_ref().err().map(|(e,_)|e.clone())}));
    }

    json!({"name":"T3 continued operation after repair (offset discipline)",
        "verdict": if all_ok {"PASS"} else {"FAIL"},
        "note":"external OnRe RWA leg is not present in LiteSVM; allocation is modeled by pre-staging the Squads USDC ATA and custody, exactly as the production bridge stages funds. Voltr books deposit as tv' = tv - old + new - amount and withdraw as tv' = tv - old + new + swept_custody, so a truthful nav (old +/- amount) keeps tv unchanged on both legs.",
        "steps": steps})
}

// ---- T4: closeStrategy reset (on CURRENT unrepaired state) -----------------
fn close_strategy_ix(payer: &Pubkey) -> Instruction {
    Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            AccountMeta::new(*payer, true),           // 0 payer (w, signer)
            meta(true, false, SQUADS_VAULT),          // 1 manager (signer) = vault.manager
            meta(false, false, PROTOCOL),             // 2 protocol
            meta(false, false, VAULT),                // 3 vault (ro)
            meta(false, false, STRATEGY),             // 4 strategy (adaptor config)
            meta(false, true, RECEIPT),               // 5 strategy init receipt (w)
            meta(false, false, SYS),                  // 6 system program
        ],
        data: VOLTR_CLOSE_STRATEGY.to_vec(),
    }
}

fn t4(base: &Base) -> Value {
    let mut svm = base.svm.clone();
    let payer = Pubkey::new_unique();
    fund(&mut svm, &payer, 100_000_000);
    let before = read_state(&svm);
    let res = send(&mut svm, &[cu_ix(), close_strategy_ix(&payer)], &payer);
    let after_receipt = svm.get_account(&key(RECEIPT));
    let after = read_state(&svm);
    let receipt_present = after_receipt.as_ref().map(|a| a.lamports > 0 && !a.data.is_empty()).unwrap_or(false);
    let (ok, err, logs) = match &res {
        Ok((_cu, l)) => (true, Value::Null, l.clone()),
        Err((e, l)) => (false, json!(e), l.clone()),
    };
    json!({
        "name": "T4 closeStrategy on CURRENT (unrepaired) state",
        "vaultAdmin": ADMIN,
        "vaultManager": SQUADS_VAULT,
        "closeSucceeded": ok,
        "receiptPositionValueBefore": before.receipt_pv,
        "receiptStillPresent": receipt_present,
        "tvBefore": before.tv,
        "tvAfter": after.tv,
        "tvUntouched": before.tv == after.tv,
        "verdict": if ok && before.tv == after.tv {
            "RESET_PATH_VIABLE"
        } else if !ok {
            "CLOSE_REJECTED"
        } else {
            "CLOSE_MUTATED_TV"
        },
        "error": err,
        "logsTail": logs.iter().rev().take(12).rev().collect::<Vec<_>>(),
        "note": "If close is rejected because positionValue != 0, that is the blocker; re-initializeStrategy + refresh is only reachable after a successful close.",
    })
}

// ---- T5: tolerance / instant / direct strategy withdrawals ---------------
fn t5(base: &Base) -> Value {
    // Best-effort probe: attempt instantWithdrawStrategy on current state with a
    // fresh LP-less user and capture the exact program error / semantics. These
    // paths CPI the same custom adaptor withdraw and require an armed ticket +
    // funded custody; without those they are rejected before any book change.
    let mut svm = base.svm.clone();
    let user = Pubkey::new_unique();
    fund(&mut svm, &user, 100_000_000);
    let user_lp = ata_for(&user, &key(LP_MINT));
    let user_usdc = ata_for(&user, &key(USDC));
    set_token_account(&mut svm, user_lp, key(LP_MINT), user, 1_000);
    set_token_account(&mut svm, user_usdc, key(USDC), user, 0);
    // directWithdrawInitReceipt PDA (may be absent) — pass its derived address.
    let (dw_receipt, _) = Pubkey::find_program_address(
        &[b"direct_withdraw_init_receipt", key(VAULT).as_ref(), key(STRATEGY).as_ref()],
        &key(VOLTR),
    );
    // instantWithdrawStrategy: 17 base accounts + adaptor remaining. Data = disc
    // + amount(u64) + additionalArgs option (report). We reuse a report envelope.
    let clock: Clock = svm.get_sysvar();
    let mut data = VOLTR_INSTANT_WITHDRAW_STRATEGY.to_vec();
    data.extend_from_slice(&1_000u64.to_le_bytes()); // amount (lp)
    // Option<additionalArgs>=Some(report) mirroring the withdraw envelope tail.
    data.push(1);
    data.extend_from_slice(&57u32.to_le_bytes());
    data.extend_from_slice(&report_v1(clock.slot, base_state_receipt(&svm)));
    let ix = Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            AccountMeta::new(user, true),                 // 0 userTransferAuthority (w,signer)
            meta(false, false, PROTOCOL),                 // 1 protocol
            meta(false, true, VAULT),                     // 2 vault (w)
            meta(false, false, ADAPTOR_ADD_RECEIPT),      // 3 adaptor add receipt
            meta(false, true, RECEIPT),                   // 4 strategy init receipt (w)
            AccountMeta::new_readonly(dw_receipt, false), // 5 direct withdraw init receipt
            meta(false, false, STRATEGY),                 // 6 strategy
            meta(false, true, USDC),                      // 7 asset mint (w)
            meta(false, true, LP_MINT),                   // 8 lp mint (w)
            AccountMeta::new(user_lp, false),             // 9 user lp ata (w)
            meta(false, true, STRATEGY_AUTH),             // 10 strategy auth (w)
            AccountMeta::new(user_usdc, false),           // 11 user asset ata (w)
            meta(false, true, CUSTODY_ATA),               // 12 strategy asset ata (w)
            meta(false, false, ADAPTOR),                  // 13 adaptor program
            meta(false, false, TOKEN),                    // 14 asset token program
            meta(false, false, TOKEN),                    // 15 lp token program
            meta(false, false, SYS),                      // 16 system program
            // adaptor remaining accounts (settings, squads vault, squads ata, ticket)
            meta(false, false, SETTINGS),
            meta(false, false, SQUADS_VAULT),
            meta(false, true, SQUADS_USDC_ATA),
            meta(false, true, TICKET),
        ],
        data,
    };
    let before = read_state(&svm);
    let res = send(&mut svm, &[cu_ix(), ix], &user);
    let after = read_state(&svm);
    let (ok, err, logs) = match &res {
        Ok((_c, l)) => (true, Value::Null, l.clone()),
        Err((e, l)) => (false, json!(e), l.clone()),
    };
    json!({
        "name": "T5 instant/direct strategy withdrawal (tolerance) probe on current state",
        "executed": ok,
        "receiptBefore": before.receipt_pv,
        "receiptAfter": after.receipt_pv,
        "tvBefore": before.tv,
        "tvAfter": after.tv,
        "custodyBefore": before.custody,
        "error": err,
        "logsTail": logs.iter().rev().take(14).rev().collect::<Vec<_>>(),
        "note": "instantWithdrawStrategy / directWithdrawStrategy (and their WithTolerance variants) CPI the same custom adaptor withdraw path; with empty custody (0) they cannot reduce the receipt without the same underflow, and the adaptor additionally requires an armed one-use ticket authorised by the Squads vault. Exact observed error above.",
    })
}

fn base_state_receipt(svm: &LiteSVM) -> u64 {
    u64_le(&svm.get_account(&key(RECEIPT)).unwrap().data, 104)
}

// ---- T6: removeAdaptor / updateVaultAdaptorPolicy -------------------------
fn t6(base: &Base) -> Value {
    // updateVaultAdaptorPolicy (admin sets allowAnyAdaptor) — must not touch books
    let mut svm1 = base.svm.clone();
    let admin = key(ADMIN);
    fund(&mut svm1, &admin, 100_000_000);
    let before1 = read_state(&svm1);
    let mut data = VOLTR_UPDATE_ADAPTOR_POLICY.to_vec();
    data.push(1); // allowAnyAdaptor = 1
    let upd = Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            AccountMeta::new_readonly(admin, true), // 0 admin (signer)
            meta(false, false, PROTOCOL),           // 1 protocol
            meta(false, true, VAULT),               // 2 vault (w)
        ],
        data,
    };
    let res_upd = send(&mut svm1, &[cu_ix(), upd], &admin);
    let after1 = read_state(&svm1);

    // removeAdaptor — likely rejected while a strategy still references it
    let mut svm2 = base.svm.clone();
    fund(&mut svm2, &admin, 100_000_000);
    let before2 = read_state(&svm2);
    let rem = Instruction {
        program_id: key(VOLTR),
        accounts: vec![
            AccountMeta::new(admin, true),              // 0 admin (w, signer)
            meta(false, false, PROTOCOL),               // 1 protocol
            meta(false, true, VAULT),                   // 2 vault (w)
            meta(false, true, ADAPTOR_ADD_RECEIPT),     // 3 adaptor add receipt (w)
            meta(false, false, ADAPTOR),                // 4 adaptor program
            meta(false, false, SYS),                    // 5 system program
        ],
        data: VOLTR_REMOVE_ADAPTOR.to_vec(),
    };
    let res_rem = send(&mut svm2, &[cu_ix(), rem], &admin);
    let after2 = read_state(&svm2);

    json!({
        "name": "T6 removeAdaptor / updateVaultAdaptorPolicy",
        "updateVaultAdaptorPolicy": {
            "executed": res_upd.is_ok(),
            "tvBefore": before1.tv, "tvAfter": after1.tv, "receiptBefore": before1.receipt_pv, "receiptAfter": after1.receipt_pv,
            "touchesBooks": before1.tv != after1.tv || before1.receipt_pv != after1.receipt_pv,
            "error": res_upd.as_ref().err().map(|(e,_)|e.clone()),
        },
        "removeAdaptor": {
            "executed": res_rem.is_ok(),
            "tvBefore": before2.tv, "tvAfter": after2.tv, "receiptBefore": before2.receipt_pv, "receiptAfter": after2.receipt_pv,
            "touchesBooks": before2.tv != after2.tv || before2.receipt_pv != after2.receipt_pv,
            "error": res_rem.as_ref().err().map(|(e,l)|{let _=l; e.clone()}),
        },
        "usefulAsRepair": false,
        "note": "Neither instruction rewrites asset.totalValue or receipt.positionValue, so neither reconciles the 119 discrepancy on its own.",
    })
}

// ---- Squads seed-63 nav_refresh policy constraint verification ------------
fn policy_check(base: &Base) -> Value {
    // Build the exact refresh envelope T1 sends and locate the NAV field vs the
    // installed nav_refresh policy's constrained offsets (voltr_custom.rs).
    let nav = repair_nav(base);
    let clock: Clock = base.svm.get_sysvar();
    let dep = voltr_capital_data(VOLTR_DEPOSIT_STRATEGY, 0, ADAPTOR_DEPOSIT, clock.slot, nav);
    let arm = arm_report_data(0, 0, clock.slot, nav);

    // deposit envelope the policy pins at offset 16 (14 bytes):
    // [1, 8u32, ADAPTOR_DEPOSIT(8), 1, 57u32, 1]
    let mut expected_envelope = vec![1u8];
    expected_envelope.extend_from_slice(&8u32.to_le_bytes());
    expected_envelope.extend_from_slice(&ADAPTOR_DEPOSIT);
    expected_envelope.push(1);
    expected_envelope.extend_from_slice(&57u32.to_le_bytes());
    expected_envelope.push(1);

    let amount_is_zero = u64_le(&dep, 8) == 0; // refresh_constraint: offset 8 == 0
    let envelope_ok = dep[16..16 + expected_envelope.len()] == expected_envelope[..];
    // NAV byte range inside the deposit data: version@34, seq@35, slot@43, nav@51
    let nav_offset = 51usize;
    let nav_in_data = u64_le(&dep, nav_offset) == nav;
    // arm nav_refresh constraint: disc||op at 0, amount(off 9)==0, [1,57,0,0,0,1] at 17
    let arm_amount_zero = u64_le(&arm, 9) == 0;
    let arm_tag_ok = arm[17..23] == [1u8, 57, 0, 0, 0, 1];

    json!({
        "name": "Squads seed-63 nav_refresh policy — would the T1 crank pass the installed policy?",
        "wouldPass": amount_is_zero && envelope_ok && nav_in_data && arm_amount_zero && arm_tag_ok,
        "depositAmountOffset8IsZero": amount_is_zero,
        "depositEnvelopeOffset16Matches": envelope_ok,
        "navValueLocation": {"offsetInDepositData": nav_offset, "value": nav,
            "constrainedByPolicy": false,
            "reason": "policy pins offsets 0..8 (disc), 8..16 (amount==0) and 16..30 (adaptor envelope); the ReportV1 body (version@34, sequence@35, observed_slot@43, nav@51, digest@59) is deliberately unconstrained"},
        "armAmountOffset9IsZero": arm_amount_zero,
        "armReportTagOffset17Matches": arm_tag_ok,
        "installedNavRefreshPolicyAccount": "41nzu42c3KPgJfWhnV5jbfxjHbvVU6HXaiJmzzYNqvBP",
        "quotedConstraint": "voltr_custom.rs refresh_constraint = voltr_constraint(refresh, VOLTR_DEPOSIT, CUSTOM_ADAPTOR_DEPOSIT_DISCRIMINATOR, vec![SquadsDataConstraint{ data_offset: 8, data_value: U64Le(0), operator: Equals }], DEPOSIT_BOUND_ACCOUNT_INDEXES); validate_envelope comment: \"Only the report version is static; the sequence, slot, NAV, and digest are deliberately left unconstrained for the adaptor to authenticate and order.\"",
        "conclusion": "The T1 repair crank reports amount=0 (satisfying the zero-amount refresh constraint) and carries the repair NAV in the policy-unconstrained ReportV1 body, so it passes the installed seed-63 nav_refresh ProgramInteraction policy. The adaptor (not the policy) bounds NAV to max_report_nav_raw = 2_000_000_000_000, which 3_793_536 satisfies.",
    })
}

// ---- master probe ---------------------------------------------------------
#[test]
#[ignore = "clones deployed mainnet programs+accounts into LiteSVM; run explicitly with --ignored"]
fn voltr_repair_paths() {
    let base = build_base();
    let s0 = read_state(&base.svm);

    let t0v = t0(&base);
    let (t1v, _repaired) = t1(&base);
    let t2v = t2(&base);
    let t3v = t3(&base);
    let t4v = t4(&base);
    let t5v = t5(&base);
    let t6v = t6(&base);
    let policyv = policy_check(&base);

    let manifest: Value =
        serde_json::from_slice(&fs::read(fixtures_dir().join("_manifest.json")).unwrap()).unwrap();

    let report = json!({
        "schema": "voltr-repair-litesvm/v1",
        "generatedBy": "crates/squads-test-harness/tests/voltr_repair_paths.rs",
        "broadcast": false,
        "signatureProof": false,
        "squadsPolicyExecutionProof": false,
        "cluster": "mainnet-beta (accounts + program binaries cloned read-only)",
        "dumpSlot": base.slot0,
        "dumpGenesis": manifest["genesis"],
        "programs": {
            "voltr": VOLTR, "adaptor": ADAPTOR, "squadsSmartAccount": SQUADS,
        },
        "vaultAdmin": ADMIN,
        "vaultManager": SQUADS_VAULT,
        "delegatedExecutor": "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5",
        "initialState": state_json(&s0),
        "deficit": s0.receipt_pv as i128 - s0.tv as i128,
        "signerOverridesAndCaveats": [
            "LiteSVM built with with_sigverify(false) + with_blockhash_check(false); all transactions carry default (zero) signatures.",
            "The Squads vault PDA (ST999…, = vault.manager) is marked a transaction signer directly for the arm_report + deposit_strategy/withdraw_strategy crank, replicating the privilege the Squads smart-account program's CPI grants after the installed REPORT_NAV / allocation / withdraw policies authorise it. The real Squads policy execution is NOT performed; it is instead verified structurally (see policyCheck).",
            "The v2 adaptor config (9hDH…) has NO delegate field; arming is authorised by the Squads vault signer, so no delegate override was needed.",
            "T2 fake-signs the LP holder wallet (8eufrx…, userTransferAuthority) — sigverify disabled.",
            "T4/T6 fake-sign the vault admin (BAqgb…) / manager (ST999…); admin lamports are airdropped locally as fee payer.",
            "T3 pre-stages the Squads USDC ATA and the strategy custody ATA to model the external OnRe allocate/sweep legs, which are not present in LiteSVM (token-balance overrides only; no policy/authority/program changes).",
            "T5 pre-seeds a synthetic LP token account for a fresh user.",
        ],
        "tests": {
            "T0_calibration": t0v,
            "T1_repair": t1v,
            "T2_fair_drain": t2v,
            "T3_continued_operation": t3v,
            "T4_close_strategy": t4v,
            "T5_tolerance_withdrawals": t5v,
            "T6_remove_adaptor_update_policy": t6v,
        },
        "squadsPolicyCheck": policyv,
    });

    // Always write the evidence JSON first, then assert the core proof.
    let out = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../docs/evidence/voltr-repair-litesvm-2026-09-07.results.json");
    fs::write(&out, serde_json::to_string_pretty(&report).unwrap()).unwrap();
    eprintln!("wrote {}", out.display());
    eprintln!("{}", serde_json::to_string_pretty(&report["tests"]).unwrap());

    // Core proof gates: T0 boundary + T1 repair must hold.
    for case in t0v["cases"].as_array().unwrap() {
        assert_eq!(case["verdict"], "PASS", "T0 case {case}");
    }
    assert_eq!(t1v["verdict"], "PASS", "T1 repair must succeed: {t1v}");
}

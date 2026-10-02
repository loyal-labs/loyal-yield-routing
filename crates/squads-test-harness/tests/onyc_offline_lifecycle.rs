//! Opt-in current-native ONyc proof. No RPC, signatures, validators or daemons.
//! Every executed wire comes from the current Go compiler and the prior bank.
use base64::{engine::general_purpose::STANDARD, Engine};
use litesvm::LiteSVM;
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use solana_sdk::{
    account::Account, clock::Clock, message::VersionedMessage, pubkey::Pubkey,
    transaction::VersionedTransaction,
};
use std::{
    fs,
    path::{Path, PathBuf},
    process::Command,
    str::FromStr,
};

const CASH: &str = "EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe";
const ONYC: &str = "AVX9wxDTk639eZ4KaiMA7LrLhXe7Lg6DaDDVRa1Q7Ji3";
const OBLIGATION: &str = "4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei";
const EQUITY: u64 = 100_000_000_000;
fn key(s: &str) -> Pubkey {
    Pubkey::from_str(s).unwrap()
}
fn sha(b: &[u8]) -> String {
    format!("{:x}", Sha256::digest(b))
}
fn bytes(v: &Value, field: &str) -> Vec<u8> {
    STANDARD.decode(v[field].as_str().unwrap()).unwrap()
}
fn read(path: impl AsRef<Path>) -> Value {
    serde_json::from_slice(&fs::read(path).unwrap()).unwrap()
}
fn write(path: impl AsRef<Path>, value: &Value) {
    fs::write(path, serde_json::to_vec_pretty(value).unwrap()).unwrap();
}
fn amount(svm: &LiteSVM, address: &str) -> u64 {
    u64::from_le_bytes(
        svm.get_account(&key(address)).unwrap().data[64..72]
            .try_into()
            .unwrap(),
    )
}

struct Proof {
    svm: LiteSVM,
    addresses: Vec<Pubkey>,
    output: PathBuf,
    compiler: PathBuf,
    sequence: usize,
    partial_target: Option<u64>,
}
impl Proof {
    fn load(directory: &Path, output: PathBuf, compiler: PathBuf) -> Self {
        let snapshot = read(directory.join("snapshot.json"));
        assert_eq!(snapshot["broadcast"], false);
        assert_eq!(
            snapshot["genesis"],
            "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
        );
        let mut svm = LiteSVM::new()
            .with_sigverify(false)
            .with_blockhash_check(false)
            .with_transaction_history(0);
        for p in snapshot["programs"].as_array().unwrap() {
            let code = fs::read(directory.join(p["file"].as_str().unwrap())).unwrap();
            assert_eq!(sha(&code), p["elfSha256"].as_str().unwrap());
            svm.add_program(key(p["program"].as_str().unwrap()), &code)
                .unwrap();
        }
        let mut addresses = vec![];
        for a in snapshot["accounts"].as_array().unwrap() {
            let address = key(a["address"].as_str().unwrap());
            addresses.push(address);
            if a["present"] != true || a["executable"] == true {
                continue;
            }
            let data = bytes(a, "dataBase64");
            assert_eq!(sha(&data), a["dataSha256"].as_str().unwrap());
            if address == solana_sdk::sysvar::clock::ID {
                let clock: Clock = bincode::deserialize(&data).unwrap();
                assert_eq!(clock.slot, snapshot["slot"].as_u64().unwrap());
                svm.set_sysvar(&clock);
            } else {
                svm.set_account(
                    address,
                    Account {
                        lamports: a["lamports"].as_u64().unwrap(),
                        data,
                        owner: key(a["owner"].as_str().unwrap()),
                        executable: false,
                        rent_epoch: 0,
                    },
                )
                .unwrap();
            }
        }
        let holder_path = PathBuf::from(
            std::env::var("ONYC_PROOF_LP_HOLDERS").expect("explicit public LP holder capture"),
        );
        let holders = read(&holder_path);
        let mut circulating = 0u64;
        for row in holders["response"]["result"]["value"].as_array().unwrap() {
            let address = key(row["pubkey"].as_str().unwrap());
            let a = &row["account"];
            let data = STANDARD.decode(a["data"][0].as_str().unwrap()).unwrap();
            assert_eq!(data.len(), 165);
            assert_eq!(
                &data[..32],
                key("6tNheTBYSpQkfMLhcczKgmTLSGffK54npKMG1WQR2tvb").as_ref()
            );
            assert_eq!(a["owner"], "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA");
            circulating = circulating
                .checked_add(u64::from_le_bytes(data[64..72].try_into().unwrap()))
                .unwrap();
            if let Some(existing) = svm.get_account(&address) {
                assert_eq!(
                    existing.data, data,
                    "holder capture differs from main snapshot"
                );
            } else {
                svm.set_account(
                    address,
                    Account {
                        lamports: a["lamports"].as_u64().unwrap(),
                        data,
                        owner: key(a["owner"].as_str().unwrap()),
                        executable: false,
                        rent_epoch: 0,
                    },
                )
                .unwrap();
            }
            if !addresses.contains(&address) {
                addresses.push(address);
            }
        }
        let mint = svm
            .get_account(&key("6tNheTBYSpQkfMLhcczKgmTLSGffK54npKMG1WQR2tvb"))
            .unwrap();
        assert_eq!(
            circulating,
            u64::from_le_bytes(mint.data[36..44].try_into().unwrap())
        );
        fs::create_dir_all(&output).unwrap();
        write(
            output.join("lp-holder-source.json"),
            &json!({"captureSha256":sha(&fs::read(holder_path).unwrap()),"captureSlot":holders["response"]["result"]["context"]["slot"],"bankSlot":snapshot["slot"],"circulatingRaw":circulating,"sameSlotClaim":false}),
        );
        write(
            output.join("source.json"),
            &json!({"snapshotSha256":sha(&fs::read(directory.join("snapshot.json")).unwrap()),"source":directory,"signatureProof":false,"workerAdmissionProof":false,"clockProgression":"one slot and one second per executed transaction; no oracle rewrites"}),
        );
        Self {
            svm,
            addresses,
            output,
            compiler,
            sequence: 0,
            partial_target: None,
        }
    }
    fn synthetic_equity(&mut self) {
        assert_eq!(amount(&self.svm, CASH), 0);
        assert_eq!(amount(&self.svm, ONYC), 0);
        assert_eq!(
            amount(&self.svm, "6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh"),
            0,
            "synthetic setup requires captured idle0"
        );
        assert_eq!(
            amount(&self.svm, "EPCVCLY5wfumf6yPvqu7zuEB4WnnXbnPsy7JrKoAWcqC"),
            0
        );
        let obligation = self.svm.get_account(&key(OBLIGATION)).unwrap();
        for i in 0..8 {
            assert!(obligation.data[128 + i * 136..136 + i * 136]
                .iter()
                .all(|b| *b == 0));
        }
        for i in 0..5 {
            assert!(obligation.data[1296 + i * 200..1312 + i * 200]
                .iter()
                .all(|b| *b == 0));
        }
        let before = self.accounts();
        let mut vault = self
            .svm
            .get_account(&key("HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA"))
            .unwrap();
        let lp = self
            .svm
            .get_account(&key("6tNheTBYSpQkfMLhcczKgmTLSGffK54npKMG1WQR2tvb"))
            .unwrap();
        let mut effective = u64::from_le_bytes(lp.data[36..44].try_into().unwrap());
        for offset in [576, 584, 592, 616] {
            effective = effective
                .checked_add(u64::from_le_bytes(
                    vault.data[offset..offset + 8].try_into().unwrap(),
                ))
                .unwrap();
        }
        assert!(effective > 0);
        vault.data[168..176].copy_from_slice(&EQUITY.to_le_bytes());
        vault.data[624..640]
            .copy_from_slice(&((u128::from(EQUITY) << 48) / u128::from(effective)).to_le_bytes());
        vault.data[672..680].fill(0);
        self.svm
            .set_account(key("HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA"), vault)
            .unwrap();
        let mut idle = self
            .svm
            .get_account(&key("6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh"))
            .unwrap();
        idle.data[64..72].copy_from_slice(&EQUITY.to_le_bytes());
        self.svm
            .set_account(key("6LATwaB4yRwGURCBDyFeJGqofaXxb6xXws9wBGbr3RBh"), idle)
            .unwrap();
        let mut receipt = self
            .svm
            .get_account(&key("5bw4VYzpZXsk9SUNyWwJkb4fEx1DS8eNMFB6Qb4MUfhE"))
            .unwrap();
        receipt.data[104..112].fill(0);
        receipt.data[128..136].fill(0);
        self.svm
            .set_account(key("5bw4VYzpZXsk9SUNyWwJkb4fEx1DS8eNMFB6Qb4MUfhE"), receipt)
            .unwrap();
        write(
            self.output.join("initial-overrides.json"),
            &json!({"kind":"SYNTHETIC_FLAT_100K_NOT_LIVE_AUTO_ROTATION","before":before,"after":self.accounts(),"effectiveLPSupplyUnchanged":effective,"lpMintAndHoldersUnchanged":true,"syntheticUSDCCreationRaw":EQUITY,"fields":["vault.totalValue","vault.highWaterMark","vault.lockedProfit","idle.amount","currentReceipt.positionValue","currentReceipt.custodyTracked"]}),
        );
    }
    fn accounts(&self) -> Value {
        // Executable ELFs are pinned once at load; Go consumes data accounts only.
        Value::Array(self.addresses.iter().filter(|address| !self.svm.get_account(address).is_some_and(|a|a.executable)).map(|address| match self.svm.get_account(address) {
   Some(a) => json!({"Address":address.to_string(),"Owner":a.owner.to_string(),"Lamports":a.lamports,"Data":STANDARD.encode(&a.data),"Executable":a.executable,"dataSha256":sha(&a.data)}),
   None => json!({"Address":address.to_string(),"Owner":"11111111111111111111111111111111","Lamports":0,"Data":"","Executable":false}),
  }).collect())
    }
    fn compile(&mut self, operation: &str, raw: u64, swap: Option<Value>) -> Result<Value, String> {
        self.sequence += 1;
        let input = json!({"operation":operation,"amountRaw":raw,"slot":self.svm.get_sysvar::<Clock>().slot,"accounts":self.accounts(),"swap":swap,"partialTargetLTVBPS":self.partial_target});
        let file = self
            .output
            .join(format!("{:03}-{operation}-input.json", self.sequence));
        write(&file, &input);
        // The enclosing test runs inside the documented hard resource cgroup.
        let output = Command::new(&self.compiler)
            .arg("-test.run=^TestExportONycOfflineProof$")
            .env("ONYC_PROOF_INPUT", &file)
            .output()
            .unwrap();
        fs::write(file.with_extension("log"), &output.stdout).unwrap();
        if !output.status.success() {
            return Err(String::from_utf8_lossy(&output.stdout).into_owned());
        }
        let stdout = String::from_utf8(output.stdout).unwrap();
        let result: Value = serde_json::from_str(
            stdout
                .lines()
                .find_map(|s| s.strip_prefix("ONYC_PROOF_JSON="))
                .expect("native exporter result"),
        )
        .unwrap();
        assert_eq!(result["requestSHA256"], sha(&fs::read(&file).unwrap()));
        assert_eq!(result["signatureProof"], false);
        assert_eq!(result["broadcast"], false);
        write(file.with_extension("compiled.json"), &result);
        Ok(result)
    }
    fn execute(&mut self, operation: &str, raw: u64, swap: Option<Value>) -> Value {
        let execution_clock = self.svm.get_sysvar::<Clock>();
        let compiled = self
            .compile(operation, raw, swap)
            .unwrap_or_else(|e| panic!("native {operation} construction: {e}"));
        let wire = bytes(&compiled, "wireBase64");
        assert_eq!(sha(&wire), compiled["wireSha256"]);
        assert!(wire.len() <= 1232 && wire[0] == 1 && wire[1..65].iter().all(|b| *b == 0));
        let tx: VersionedTransaction = bincode::deserialize(&wire).unwrap();
        let (static_accounts, alt_count, top_level) = match &tx.message {
            VersionedMessage::Legacy(m) => (m.account_keys.len(), 0, m.instructions.len()),
            VersionedMessage::V0(m) => (
                m.account_keys.len(),
                m.address_table_lookups.len(),
                m.instructions.len(),
            ),
        };
        let before = self.accounts();
        let pool_before = self
            .svm
            .get_account(&key("7jhhyxPUKpu42hPGSYwgMXbR2dtVJHKhs8DW3sAAgAvX"));
        let (meta, error) = match self.svm.send_transaction(tx) {
            Ok(m) => (m, None),
            Err(e) => (e.meta, Some(format!("{:?}", e.err))),
        };
        let mut next_clock = execution_clock.clone();
        next_clock.slot += 1;
        next_clock.unix_timestamp += 1;
        self.svm.set_sysvar(&next_clock);
        let result = json!({"executionClock":{"slot":execution_clock.slot,"unixTimestamp":execution_clock.unix_timestamp},"nextObservationClock":{"slot":next_clock.slot,"unixTimestamp":next_clock.unix_timestamp},"operation":operation,"amountRaw":raw,"wireSha256":sha(&wire),"packetBytes":wire.len(),"staticAccountCount":static_accounts,"lookupTableCount":alt_count,"topLevelInstructionCount":top_level,"before":before,"after":self.accounts(),"computeUnits":meta.compute_units_consumed,"logs":meta.logs,"error":error,"pass":error.is_none()});
        write(
            self.output
                .join(format!("{:03}-{operation}-execution.json", self.sequence)),
            &result,
        );
        assert!(
            error.is_none(),
            "{operation} failed: {error:?}; see execution artifact"
        );
        if operation == "swap" {
            assert_ne!(
                self.svm
                    .get_account(&key("7jhhyxPUKpu42hPGSYwgMXbR2dtVJHKhs8DW3sAAgAvX")),
                pool_before,
                "executed pool state must evolve"
            );
        }
        // Mirror the worker's mandatory NAV barrier after every capital mutation.
        if matches!(
            operation,
            "swap" | "deposit" | "borrow" | "repay" | "withdraw"
        ) {
            self.execute("REPORT_NAV", 0, None);
        }
        result
    }
    fn swap(&mut self, template: &Value, raw: u64, negative: bool) -> u64 {
        let main_before = self.accounts();
        let quote_directory = self
            .output
            .join(format!("{:03}-clone-quote", self.sequence + 1));
        fs::create_dir_all(&quote_directory).unwrap();
        let mut probe = Self {
            svm: self.svm.clone(),
            addresses: self.addresses.clone(),
            output: quote_directory,
            compiler: self.compiler.clone(),
            sequence: 0,
            partial_target: self.partial_target,
        };
        let forward = template["Action"] == "SWAP_STABLE_TO_COLLATERAL_STEP";
        let destination = if forward { ONYC } else { CASH };
        let initial = amount(&probe.svm, destination);
        probe.execute("swap", raw, Some(swap_instruction(template, raw, 1, 0)));
        let quoted = amount(&probe.svm, destination)
            .checked_sub(initial)
            .unwrap();
        assert!(quoted > 0);
        assert_eq!(
            self.accounts(),
            main_before,
            "quote probe changed main bank"
        );
        drop(probe);
        if negative {
            let request = swap_instruction(template, raw, quoted.checked_mul(2).unwrap(), 50);
            let compiled = self.compile("swap", raw, Some(request)).unwrap();
            let tx: VersionedTransaction =
                bincode::deserialize(&bytes(&compiled, "wireBase64")).unwrap();
            let mut rejected_bank = self.svm.clone();
            let failed = rejected_bank
                .send_transaction(tx)
                .expect_err("min-out above real output must fail");
            assert_eq!(amount(&rejected_bank, CASH), amount(&self.svm, CASH));
            assert_eq!(amount(&rejected_bank, ONYC), amount(&self.svm, ONYC));
            // Transaction fees may debit only the fee payer; all program state rolls back.
            for a in &self.addresses {
                if *a != key("62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5") {
                    assert_eq!(
                        rejected_bank.get_account(a),
                        self.svm.get_account(a),
                        "negative mutated {a}"
                    );
                }
            }
            write(
                self.output
                    .join(format!("{:03}-negative-minimum.json", self.sequence)),
                &json!({"mainBankUnchanged":self.accounts()==main_before,"capitalAndProgramsUnchanged":true,"error":format!("{:?}",failed.err),"logs":failed.meta.logs,"quotedOutputRaw":quoted,"impossibleQuotedOutputRaw":quoted*2}),
            );
        }
        let request = swap_instruction(template, raw, quoted, 50);
        let minimum = request["MinimumOutputRaw"].as_u64().unwrap();
        let pre = amount(&self.svm, destination);
        self.execute("swap", raw, Some(request));
        let output = amount(&self.svm, destination).checked_sub(pre).unwrap();
        assert_eq!(
            output, quoted,
            "same bank and exact native swap must reproduce quote"
        );
        assert!(output >= minimum);
        output
    }
    fn restore(&mut self) {
        let cash = amount(&self.svm, CASH);
        assert!(cash > 0);
        self.execute("STAGE_SQUADS_TO_VOLTR", cash, None);
        assert_eq!(amount(&self.svm, CASH), 0);
        self.execute("VOLTR_RESTORE_IDLE", cash, None);
        assert_eq!(
            amount(&self.svm, "EPCVCLY5wfumf6yPvqu7zuEB4WnnXbnPsy7JrKoAWcqC"),
            0
        );
    }
}

// Fresh Borsh construction for the captured one-hop WhirlpoolV2 topology.
// The HTTP instruction is decoded only as a topology reference; no wire is resized.
fn swap_instruction(template: &Value, raw: u64, quoted: u64, slippage: u16) -> Value {
    let original = bytes(&template["Instruction"], "data");
    assert_eq!(original.len(), 38);
    assert_eq!(&original[..8], &[193, 32, 155, 51, 65, 214, 156, 129]);
    assert_eq!(&original[9..14], &[1, 0, 0, 0, 47]); // one WhirlpoolV2 route
    assert!(original[14] <= 1);
    assert_eq!(&original[15..19], &[0, 100, 0, 1]);
    assert_eq!(original[37], 0);
    assert!(raw > 0 && quoted > 0 && slippage <= 50);
    let mut data = vec![193, 32, 155, 51, 65, 214, 156, 129, original[8]];
    data.extend(1u32.to_le_bytes());
    data.extend([47, original[14], 0, 100, 0, 1]);
    data.extend(raw.to_le_bytes());
    data.extend(quoted.to_le_bytes());
    data.extend(slippage.to_le_bytes());
    data.push(0);
    let mut request = template.clone();
    request["AmountRaw"] = json!(raw);
    request["QuotedOutputRaw"] = json!(quoted);
    request["MinimumOutputRaw"] =
        json!((u128::from(quoted) * u128::from(10_000 - slippage) / 10_000) as u64);
    request["Instruction"]["data"] = json!(STANDARD.encode(data));
    request
}

#[test]
#[ignore = "explicit current public snapshot and bounded native Go compiler required"]
fn onyc_current_native_connected_lifecycle() {
    let dir = PathBuf::from(std::env::var("ONYC_PROOF_DIR").expect("explicit public snapshot"));
    let output =
        PathBuf::from(std::env::var("ONYC_PROOF_OUTPUT").expect("explicit output directory"));
    let compiler = PathBuf::from(
        std::env::var("ONYC_PROOF_COMPILER").expect("explicit local Go test compiler"),
    );
    let mut proof = Proof::load(&dir, output, compiler);
    let observed = proof
        .compile("observe", 0, None)
        .expect("current policies and actual prestate must validate");
    write(proof.output.join("baseline.json"), &observed);
    proof.synthetic_equity();
    let observed = proof
        .compile("observe", 0, None)
        .expect("synthetic book must validate");
    assert_eq!(observed["nav"]["Voltr"]["TotalValueRaw"], EQUITY);
    assert_eq!(observed["nav"]["TotalVaultNAVRaw"], EQUITY);
    let initial_book = observed["nav"].clone();
    let initial_lp_accounts: Vec<_> = proof
        .addresses
        .iter()
        .filter_map(|a| {
            proof
                .svm
                .get_account(a)
                .filter(|v| {
                    v.data.len() == 165
                        && v.data[..32]
                            == key("6tNheTBYSpQkfMLhcczKgmTLSGffK54npKMG1WQR2tvb").to_bytes()
                })
                .map(|v| (*a, v))
        })
        .collect();
    // First connected slice: explicit initial funding only, no obligation reset.
    assert_eq!(observed["nav"]["PositionDebtValue"], 0);
    assert_eq!(observed["nav"]["PositionCollateralValue"], 0);
    assert!(
        observed["nav"]["VaultIdleRaw"].as_u64().unwrap() >= EQUITY,
        "captured idle cannot fund100k; requires explicit coherent initial equity setup"
    );
    assert_eq!(amount(&proof.svm, CASH), 0);
    assert_eq!(amount(&proof.svm, ONYC), 0);
    assert!(
        proof.svm.get_account(&key(OBLIGATION)).is_some(),
        "native obligation initializer required"
    );
    proof.execute("VOLTR_ALLOCATE_TO_SQUADS", EQUITY, None);
    let swap = read(dir.join("entry-swap-request.json"));
    proof.execute("swap", EQUITY, Some(swap.clone()));
    assert_eq!(
        amount(&proof.svm, CASH),
        0,
        "all initial equity must enter ONyc"
    );
    let collateral = amount(&proof.svm, ONYC);
    assert!(collateral > 0);
    proof.execute("deposit", collateral, None);
    assert_eq!(amount(&proof.svm, ONYC), 0);
    let observed = proof.compile("observe", 0, None).unwrap();
    assert!(observed["nav"]["PositionCollateralValue"].as_u64().unwrap() > 0);
    assert_eq!(observed["nav"]["PositionDebtValue"], 0);
    let exit = read(dir.join("exit-swap-request.json"));
    let mode = std::env::var("ONYC_PROOF_CASE").unwrap_or_else(|_| "1x".into());
    assert!(matches!(mode.as_str(), "1x" | "1.75x" | "fractional"));
    let mut total_borrowed = 0u64;
    if mode != "1x" {
        for level in [150, 175] {
            let observed = proof.compile("observe", 0, None).unwrap();
            let mut loan = observed[format!("borrow{level}Raw")]
                .as_u64()
                .expect("native capacity evidence");
            if mode == "fractional" {
                loan = loan.min(20_000_000_000);
            }
            if loan < 10_000_000 {
                break;
            }
            assert_eq!(amount(&proof.svm, CASH), 0, "no unrelated repayment cash");
            proof.execute("borrow", loan, None);
            total_borrowed += loan;
            assert_eq!(amount(&proof.svm, CASH), loan);
            let instant = proof.compile("observe", 0, None).unwrap();
            assert!(
                u128::from(instant["nav"]["PositionDebtValue"].as_u64().unwrap()) * 10_000
                    <= u128::from(instant["nav"]["PositionCollateralValue"].as_u64().unwrap())
                        * 5_000,
                "instant borrow LTV exceeds50%"
            );
            proof.swap(&swap, loan, false);
            assert_eq!(
                amount(&proof.svm, CASH),
                0,
                "loan proceeds must be exhausted by reinvestment"
            );
            proof.execute("deposit", amount(&proof.svm, ONYC), None);
            let observed = proof.compile("observe", 0, None).unwrap();
            let c = observed["nav"]["PositionCollateralValue"].as_u64().unwrap();
            let d = observed["nav"]["PositionDebtValue"].as_u64().unwrap();
            assert!(
                u128::from(d) * 10_000 <= u128::from(c) * 4_500,
                "completed LTV exceeds45%"
            );
            if mode == "fractional" {
                break;
            }
        }
        assert!(
            total_borrowed >= 10_000_000_000,
            "loan must be economically material, not1000raw"
        );
    }
    let leveraged = proof.compile("observe", 0, None).unwrap();
    assert_eq!(amount(&proof.svm, CASH), 0);
    let partial_mode = std::env::var("ONYC_PROOF_PARTIAL").unwrap_or_else(|_| "required".into());
    assert!(matches!(partial_mode.as_str(), "required" | "skip"));
    let partial_enabled = partial_mode == "required";
    let mut partial_payout = 0;
    let mut target = 0;
    let mut after_partial = Value::Null;
    if partial_enabled {
        let partial = proof.compile("partial", 10_000_000_000, None).unwrap();
        target = partial["partialTargetLTVBPS"]
            .as_u64()
            .expect("capture actual partial LTV");
        proof.partial_target = Some(target);
        let mut release = partial["partialReceiptsRaw"]
            .as_u64()
            .expect("native partial withdrawal receipt sizing");
        if total_borrowed > 0 {
            release = release.min(
                partial["partialRelease"]["receiptRaw"]
                    .as_u64()
                    .expect("50% partial release bound"),
            );
        }
        assert!(release > 0);
        proof.execute("withdraw", release, None);
        let after_release = proof.compile("observe", 0, None).unwrap();
        let c = after_release["nav"]["PositionCollateralValue"]
            .as_u64()
            .unwrap();
        let d = after_release["nav"]["PositionDebtValue"].as_u64().unwrap();
        assert!(
            u128::from(d) * 10_000 <= u128::from(c) * 5_000,
            "partial instant LTV exceeds50%"
        );
        proof.swap(&exit, amount(&proof.svm, ONYC), true);
        if total_borrowed > 0 {
            let observed = proof.compile("observe", 0, None).unwrap();
            let repayment = observed["partialRepayRaw"]
                .as_u64()
                .expect("native actual-ratio partial repayment");
            assert!(repayment > 0 && repayment < observed["position"]["DebtRaw"].as_u64().unwrap());
            assert!(
                repayment <= amount(&proof.svm, CASH),
                "partial repayment must use sold ONyc only"
            );
            proof.execute("repay", repayment, None);
        }
        partial_payout = amount(&proof.svm, CASH);
        assert!(partial_payout >= 10_000_000_000);
        proof.restore();
        after_partial = proof.compile("observe", 0, None).unwrap();
        assert!(
            after_partial["position"]["CollateralDepositedRaw"]
                .as_u64()
                .unwrap()
                > 0
        );
        if total_borrowed > 0 {
            let c = after_partial["nav"]["PositionCollateralValue"]
                .as_u64()
                .unwrap();
            let d = after_partial["nav"]["PositionDebtValue"].as_u64().unwrap();
            let actual = (u128::from(d) * 10_000 / u128::from(c)) as u64;
            assert!(
                actual.abs_diff(target) <= 2,
                "partial must retain actual captured LTV, not snap target"
            );
        }
        proof.partial_target = None;
    }
    assert_eq!(
        amount(&proof.svm, CASH),
        0,
        "full unwind cannot use partial payout already restored to idle"
    );
    let mut partial_repayments = 0;
    loop {
        let observed = proof.compile("observe", 0, None).unwrap();
        if observed["position"]["DebtRaw"].as_u64().unwrap() == 0 {
            break;
        }
        assert!(partial_repayments <= 2, "current two-partial-cycle ceiling");
        let release = observed["release"]["receiptRaw"]
            .as_u64()
            .expect("native safe collateral release");
        proof.execute("withdraw", release, None);
        let released_position = proof.compile("observe", 0, None).unwrap();
        let released_c = released_position["nav"]["PositionCollateralValue"]
            .as_u64()
            .unwrap();
        let released_d = released_position["nav"]["PositionDebtValue"]
            .as_u64()
            .unwrap();
        assert!(
            u128::from(released_d) * 10_000 <= u128::from(released_c) * 5_500,
            "native full-release ceiling55% exceeded"
        );
        assert!(
            u128::from(released_d) * 10_000 < u128::from(released_c) * 6_000,
            "hard LTV60% reached"
        );
        let released = amount(&proof.svm, ONYC);
        assert!(released > 0);
        proof.swap(&exit, released, partial_repayments == 0);
        let observed = proof.compile("observe", 0, None).unwrap();
        let payoff = observed["payoff"]["upperDebtRaw"]
            .as_u64()
            .expect("native finite payoff bound");
        let available = amount(&proof.svm, CASH);
        let payment = available.min(payoff);
        proof.execute("repay", payment, None);
        if payment < payoff {
            partial_repayments += 1;
            assert_eq!(amount(&proof.svm, CASH), 0);
        }
    }
    let observed = proof.compile("observe", 0, None).unwrap();
    let receipts = observed["position"]["CollateralDepositedRaw"]
        .as_u64()
        .unwrap();
    assert!(receipts > 0);
    proof.execute("withdraw", receipts, None);
    let remaining = amount(&proof.svm, ONYC);
    assert!(remaining > 0);
    proof.swap(&exit, remaining, mode == "1x");
    proof.restore();
    proof.execute("REPORT_NAV", 0, None);
    let terminal = proof.compile("observe", 0, None).unwrap();
    assert_eq!(terminal["position"]["DebtRaw"], 0);
    assert_eq!(terminal["position"]["CollateralDepositedRaw"], 0);
    assert_eq!(amount(&proof.svm, CASH), 0);
    assert_eq!(amount(&proof.svm, ONYC), 0);
    assert_eq!(terminal["nav"]["StrategyNAVRaw"], 0);
    assert_eq!(terminal["nav"]["Receipt"]["PositionValueRaw"], 0);
    assert_eq!(
        terminal["nav"]["Voltr"]["TotalValueRaw"],
        terminal["nav"]["VaultIdleRaw"]
    );
    assert!(
        terminal["nav"]["VaultIdleRaw"].as_u64().unwrap() > EQUITY * 95 / 100,
        "unexpected loss exceeds5%"
    );
    for (address, before) in initial_lp_accounts {
        assert_eq!(
            proof.svm.get_account(&address).unwrap(),
            before,
            "LP holder mutated"
        );
    }
    assert_eq!(terminal["nav"]["LPSupplyRaw"], initial_book["LPSupplyRaw"]);
    for field in [
        "ManagerPerformanceFeeBPS",
        "AdminPerformanceFeeBPS",
        "ManagerManagementFeeBPS",
        "AdminManagementFeeBPS",
        "ProtocolPerformanceFeeBPS",
        "ProtocolManagementFeeBPS",
        "IssuanceFeeBPS",
        "RedemptionFeeBPS",
        "LPSupplyDeadWeightRaw",
    ] {
        assert_eq!(
            terminal["nav"]["Voltr"][field], initial_book["Voltr"][field],
            "fee configuration changed: {field}"
        );
    }
    let circulating_equity = |nav: &Value| -> u64 {
        let supply = nav["LPSupplyRaw"].as_u64().unwrap();
        let effective = supply
            + [
                "FeeAccumulatorManagerRaw",
                "FeeAccumulatorAdminRaw",
                "FeeAccumulatorProtocolRaw",
                "LPSupplyDeadWeightRaw",
            ]
            .iter()
            .map(|field| nav["Voltr"][field].as_u64().unwrap())
            .sum::<u64>();
        (u128::from(nav["Voltr"]["TotalValueRaw"].as_u64().unwrap()) * u128::from(supply)
            / u128::from(effective)) as u64
    };
    let initial_user_equity = circulating_equity(&initial_book);
    let terminal_user_equity = circulating_equity(&terminal["nav"]);
    let effective = |nav: &Value| -> u64 {
        nav["LPSupplyRaw"].as_u64().unwrap()
            + [
                "FeeAccumulatorManagerRaw",
                "FeeAccumulatorAdminRaw",
                "FeeAccumulatorProtocolRaw",
                "LPSupplyDeadWeightRaw",
            ]
            .iter()
            .map(|field| nav["Voltr"][field].as_u64().unwrap())
            .sum::<u64>()
    };
    let normalized_wealth = (u128::from(terminal["nav"]["VaultIdleRaw"].as_u64().unwrap())
        * u128::from(effective(&initial_book))
        / u128::from(effective(&terminal["nav"]))) as u64;
    write(
        proof.output.join("result.json"),
        &json!({"clockElapsedSeconds":proof.svm.get_sysvar::<Clock>().unix_timestamp-read(dir.join("snapshot.json"))["accounts"].as_array().unwrap().iter().find(|a|a["address"]==solana_sdk::sysvar::clock::ID.to_string()).map(|a|bincode::deserialize::<Clock>(&bytes(a,"dataBase64")).unwrap().unix_timestamp).unwrap(),"normalizedIncumbentLPWealthRaw":normalized_wealth,"normalizedIncumbentLPCostRaw":EQUITY-normalized_wealth,"feePayerSOLAndRentExcluded":true,"trueReserveCapacityClipProved":false,"fractionalCase":"explicit20k request below native cap; not reserve exhaustion","initialBook":initial_book,"initialCirculatingLPEquityRaw":initial_user_equity,"terminalCirculatingLPEquityRaw":terminal_user_equity,"circulatingLPEquityLossRaw":i128::from(initial_user_equity)-i128::from(terminal_user_equity),"case":mode,"entrySlicePass":true,"fullExitPass":true,"fullLifecyclePass":partial_enabled,"partialWithdrawalProved":partial_enabled,"partialPayoutRaw":partial_payout,"capturedPartialLTVBPS":target,"afterPartial":after_partial,"borrowedRaw":total_borrowed,"partialRepaymentCycles":partial_repayments,"leveragedObservation":leveraged,"terminalObservation":terminal,"workerAdmissionProof":false,"signatureProof":false,"broadcast":false}),
    );
}

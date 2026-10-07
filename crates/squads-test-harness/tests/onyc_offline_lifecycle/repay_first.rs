//! Existing-policy mechanics only. Synthetic external capital is not exit proceeds.
#[path = "settlement.rs"]
mod settlement;
use super::*;
use klend_interface::helpers::{flash::flash_loan, ObligationContext, ReserveInfo};
use solana_sdk::{
    address_lookup_table::{state::AddressLookupTable, AddressLookupTableAccount},
    instruction::{AccountMeta, Instruction, InstructionError},
    message::v0,
    program_pack::Pack,
    signature::Signature,
};

const EXECUTOR: &str = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5";
const VAULT: &str = "ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh";
const DEBT: &str = "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z";
const COLLATERAL: &str = "6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ";
const STAGE: &str = "7EW76UaxsNTnLG931HTNSteRjhR3s9rcKVJ7UtqyN6e3";

fn fork(proof: &Proof, label: &str) -> Proof {
    let output = proof.output.join(label);
    fs::create_dir_all(&output).unwrap();
    Proof {
        svm: proof.svm.clone(),
        addresses: proof.addresses.clone(),
        output,
        compiler: proof.compiler.clone(),
        sequence: 0,
        partial_target: None,
    }
}

// The sole additional bank override: initial EOA-owned USDC, before execution.
fn seed_external(proof: &mut Proof, raw: u64) -> Pubkey {
    assert!(key(EXECUTOR).is_on_curve());
    assert!(!key(VAULT).is_on_curve());
    let cash = proof.svm.get_account(&key(CASH)).unwrap();
    let mut token = spl_token::state::Account::unpack(&cash.data).unwrap();
    assert_eq!(token.owner, key(VAULT));
    let ata =
        spl_associated_token_account::get_associated_token_address(&key(EXECUTOR), &token.mint);
    assert!(
        proof.svm.get_account(&ata).is_none(),
        "initial EOA ATA must be absent"
    );
    let before = proof.accounts();
    token.owner = key(EXECUTOR);
    token.amount = raw;
    token.delegate = None.into();
    token.delegated_amount = 0;
    token.close_authority = None.into();
    let mut data = vec![0; spl_token::state::Account::LEN];
    spl_token::state::Account::pack(token, &mut data).unwrap();
    proof
        .svm
        .set_account(
            ata,
            Account {
                lamports: cash.lamports,
                data,
                owner: cash.owner,
                executable: false,
                rent_epoch: 0,
            },
        )
        .unwrap();
    proof.addresses.push(ata);
    write(
        proof.output.join("synthetic-external-capital.json"),
        &json!({
        "before":before,"after":proof.accounts(),"owner":EXECUTOR,"ata":ata.to_string(),
        "syntheticUSDCraw":raw,"syntheticRentLamports":cash.lamports,
        "includedInSelfFinancingExitClaim":false,"signatureProof":false,
        "authorityOnCurve":true,"vaultTopLevelSigner":false}),
    );
    ata
}

// Legacy reserves store their vault addresses; current helper PDA derivation
// does not describe this captured reserve. Keep dependency wire builders, bind
// their account metas to the actual captured reserve (no state mutation).
fn flash_pair(proof: &Proof, ata: Pubkey, raw: u64) -> (Instruction, Instruction) {
    let account = proof.svm.get_account(&key(DEBT)).unwrap();
    let state =
        klend_interface::state::from_account_data::<klend_interface::state::Reserve>(&account.data)
            .unwrap();
    let info = reserve(proof);
    let (mut b, mut r) = flash_loan(key(EXECUTOR), &info, ata, ata, raw, 0, None);
    for ix in [&mut b, &mut r] {
        ix.accounts[5].pubkey = state.liquidity.supply_vault;
        ix.accounts[7].pubkey = state.liquidity.fee_vault;
        assert!(proof.svm.get_account(&ix.accounts[5].pubkey).is_some());
        assert!(proof.svm.get_account(&ix.accounts[7].pubkey).is_some());
    }
    (b, r)
}

fn reserve(proof: &Proof) -> ReserveInfo {
    ReserveInfo::from_account_data(key(DEBT), &proof.svm.get_account(&key(DEBT)).unwrap().data)
        .unwrap()
}

// Test-local composer only. Production native admission still requires exactly
// refresh+policy. No signer/production compiler boundary is broadened here.
fn probe(
    proof: &mut Proof,
    label: &str,
    ixs: &[Instruction],
    failure: Option<(usize, u32)>,
) -> Value {
    probe_with_tables(
        proof,
        label,
        ixs,
        failure,
        &[key("FyJcsUtFnzisYya2y82qSuUTTo8gm2gyc8MokSKfWnnY")],
    )
}

fn lookup_tables(proof: &Proof, keys: &[Pubkey]) -> Vec<AddressLookupTableAccount> {
    keys.iter()
        .map(|address| {
            let account = proof.svm.get_account(address).unwrap();
            let table = AddressLookupTable::deserialize(&account.data).unwrap();
            AddressLookupTableAccount {
                key: *address,
                addresses: table.addresses.to_vec(),
            }
        })
        .collect()
}

fn probe_with_tables(
    proof: &mut Proof,
    label: &str,
    ixs: &[Instruction],
    failure: Option<(usize, u32)>,
    table_keys: &[Pubkey],
) -> Value {
    let executor = key(EXECUTOR);
    for ix in ixs {
        for a in &ix.accounts {
            assert!(
                !a.is_signer || a.pubkey == executor,
                "non-EOA top-level signer"
            );
            if !proof.addresses.contains(&a.pubkey) {
                proof.addresses.push(a.pubkey);
            }
        }
    }
    let message = v0::Message::try_compile(
        &executor,
        ixs,
        &lookup_tables(proof, table_keys),
        proof.svm.latest_blockhash(),
    )
    .unwrap();
    assert_eq!(message.header.num_required_signatures, 1);
    assert_eq!(message.account_keys[0], executor);
    let tx = VersionedTransaction {
        signatures: vec![Signature::default()],
        message: VersionedMessage::V0(message),
    };
    let wire = bincode::serialize(&tx).unwrap();
    assert!(
        wire.len() <= 1232,
        "{label} packet overflow: {}",
        wire.len()
    );
    let before = proof.accounts();
    let bank = proof.svm.clone();
    let (meta, error) = match proof.svm.send_transaction(tx) {
        Ok(m) => (m, None),
        Err(e) => (e.meta, Some(e.err)),
    };
    let result = json!({"label":label,"before":before,"after":proof.accounts(),
        "wireBase64":STANDARD.encode(&wire),"wireSha256":sha(&wire),"packetBytes":wire.len(),
        "logs":meta.logs,"computeUnits":meta.compute_units_consumed,"error":error.as_ref().map(|e|format!("{e:?}")),
        "signatureProof":false,"broadcast":false,"topLevelSigners":[EXECUTOR],
        "clock":proof.svm.get_sysvar::<Clock>().slot});
    write(proof.output.join(format!("{label}.json")), &result);
    match failure {
        None => assert!(error.is_none(), "{label}: {error:?}; logs: {:?}", meta.logs),
        Some((index, code)) => {
            assert_eq!(
                error,
                Some(solana_sdk::transaction::TransactionError::InstructionError(
                    index as u8,
                    InstructionError::Custom(code)
                )),
                "{label}: wrong rejection"
            );
            for a in &proof.addresses {
                if *a == executor {
                    let mut after = proof.svm.get_account(a).unwrap();
                    let previous = bank.get_account(a).unwrap();
                    assert_eq!(previous.lamports - after.lamports, 5000);
                    after.lamports = previous.lamports;
                    assert_eq!(after, previous);
                } else {
                    assert_eq!(
                        proof.svm.get_account(a),
                        bank.get_account(a),
                        "{label} rollback: {a}"
                    );
                }
            }
        }
    }
    result
}

fn reimbursement(destination: Pubkey, mint: Pubkey, raw: u64) -> Instruction {
    let inner = spl_token::instruction::transfer_checked(
        &spl_token::id(),
        &key(CASH),
        &mint,
        &destination,
        &key(VAULT),
        &[],
        raw,
        6,
    )
    .unwrap();
    let mut accounts = vec![];
    let compiled = loyal_actions::compile_squads_inner_instruction(&mut accounts, inner);
    loyal_actions::execute_program_interaction_policy_instruction(
        key(STAGE),
        key(EXECUTOR),
        0,
        vec![compiled],
        vec![0],
        accounts,
    )
}

#[test]
#[ignore = "explicit current public snapshot, native Go compiler and bounded cgroup required"]
fn onyc_existing_boundary_repay_first() {
    let dir = PathBuf::from(std::env::var("ONYC_PROOF_DIR").unwrap());
    let output = PathBuf::from(std::env::var("ONYC_PROOF_OUTPUT").unwrap());
    let compiler = PathBuf::from(std::env::var("ONYC_PROOF_COMPILER").unwrap());
    let mut proof = Proof::load(&dir, output, compiler);
    proof
        .compile("observe", 0, None)
        .expect("current policies must validate");
    write(
        proof.output.join("probe-scope.json"),
        &json!({
        "clockProgression":"native Proof.execute advances one slot/second; dependency-built probe transactions hold the current clock fixed; no oracle rewrites",
        "composer":"test-only v0 with captured ALT; not production admission",
        "atomicCoverage":"financing/repay/denied-return prefix; not a complete atomic withdrawal/swap/return exit",
        "signatureProof":false,"vaultTopLevelSigner":false}),
    );

    // Isolated flash-only bank: $1 synthetic fee reserve, $100 borrowed/returned.
    let mut flash = fork(&proof, "flash-only");
    let flash_ata = seed_external(&mut flash, 1_000_000);
    let (borrow, repay) = flash_pair(&flash, flash_ata, 100_000_000);
    for (label, instructions) in [
        ("missing-repay", vec![borrow.clone()]),
        ("wrong-amount", {
            let mut r = repay.clone();
            r.data[8..16].copy_from_slice(&99_999_999u64.to_le_bytes());
            vec![borrow.clone(), r]
        }),
        ("wrong-index", {
            let mut r = repay.clone();
            r.data[16] = 1;
            vec![borrow.clone(), r]
        }),
        ("wrong-custody", {
            let mut r = repay.clone();
            r.accounts[6].pubkey = key(CASH);
            vec![borrow.clone(), r]
        }),
    ] {
        let mut negative = fork(&flash, label);
        let result = probe(
            &mut negative,
            label,
            &instructions,
            Some((0, if label == "missing-repay" { 6032 } else { 6033 })),
        );
        let expected = if label == "missing-repay" {
            "NoFlashRepayFound"
        } else {
            "InvalidFlashRepay"
        };
        assert!(
            result["logs"]
                .as_array()
                .unwrap()
                .iter()
                .any(|l| l.as_str().unwrap().contains(expected)),
            "{label}: not a pairing rejection"
        );
    }
    let initial_cash = amount(&flash.svm, CASH);
    probe(&mut flash, "flash-pair", &[borrow, repay], None);
    let fee = 1_000_000 - amount(&flash.svm, &flash_ata.to_string());
    let debt_account = flash.svm.get_account(&key(DEBT)).unwrap();
    let state = klend_interface::state::from_account_data::<klend_interface::state::Reserve>(
        &debt_account.data,
    )
    .unwrap();
    assert_eq!(
        state.config.fees.flash_loan_fee_sf, 0,
        "this snapshot is a zero-fee flash case"
    );
    assert_eq!(fee, 0);
    assert_eq!(amount(&flash.svm, CASH), initial_cash);
    write(
        flash.output.join("result.json"),
        &json!({"mechanicsPass":true,"flashPrincipalRaw":100_000_000,
        "feePaidFromSyntheticCapitalRaw":fee,"initialSyntheticFeeFundingRaw":1_000_000,"capturedFlashFeeSF":0,"positiveFlashFeeProved":false,
        "repaymentPairNegativesPass":true,"fullExitProved":false,"signatureProof":false}),
    );

    proof.synthetic_equity();
    let external = seed_external(&mut proof, EQUITY);
    let initial = proof.compile("observe", 0, None).unwrap();
    proof.execute("VOLTR_ALLOCATE_TO_SQUADS", EQUITY, None);
    let entry = read(dir.join("entry-swap-request.json"));
    proof.execute("swap", EQUITY, Some(entry.clone()));
    proof.execute("deposit", amount(&proof.svm, ONYC), None);
    let observed = proof.compile("observe", 0, None).unwrap();
    let loan = observed["borrow150Raw"]
        .as_u64()
        .unwrap()
        .min(20_000_000_000);
    assert_eq!(loan, 20_000_000_000);
    proof.execute("borrow", loan, None); // unchanged native capacity admission
    proof.swap(&entry, loan, false);
    proof.execute("deposit", amount(&proof.svm, ONYC), None);
    let leveraged = proof.compile("observe", 0, None).unwrap();
    assert!(
        u128::from(leveraged["nav"]["PositionDebtValue"].as_u64().unwrap()) * 10_000
            <= u128::from(
                leveraged["nav"]["PositionCollateralValue"]
                    .as_u64()
                    .unwrap()
            ) * 4_500
    );
    assert_eq!(amount(&proof.svm, CASH), 0);
    let payoff = leveraged["payoff"]["upperDebtRaw"].as_u64().unwrap();
    let obligation = proof.svm.get_account(&key(OBLIGATION)).unwrap();
    let debt = proof.svm.get_account(&key(DEBT)).unwrap();
    let collateral = proof.svm.get_account(&key(COLLATERAL)).unwrap();
    let ctx = ObligationContext::from_account_data(
        key(OBLIGATION),
        &obligation.data,
        &[(key(DEBT), &debt.data), (key(COLLATERAL), &collateral.data)],
    )
    .unwrap();
    let mut ixs = ctx
        .repay(key(EXECUTOR), &key(DEBT), external, payoff)
        .unwrap();
    let state =
        klend_interface::state::from_account_data::<klend_interface::state::Reserve>(&debt.data)
            .unwrap();
    ixs.last_mut().unwrap().accounts[5].pubkey = state.liquidity.supply_vault;
    let native = proof.compile("repay", payoff, None).unwrap();
    for (index, account) in native["request"]["Accounts"]
        .as_array()
        .unwrap()
        .iter()
        .enumerate()
    {
        if index != 0 && index != 6 {
            assert_eq!(
                ixs.last().unwrap().accounts[index].pubkey,
                key(account["Address"].as_str().unwrap())
            );
        }
    }
    assert_eq!(ixs.last().unwrap().accounts[0].pubkey, key(EXECUTOR));
    assert_ne!(key(EXECUTOR), key(VAULT));
    let info = reserve(&proof);
    let denied_return = reimbursement(external, info.liquidity_mint, payoff);

    // Both external prefunding and flash financing roll back when the later
    // current-policy return is denied. No debt or reserve state is assigned.
    for financing in [false, true] {
        let label = if financing {
            "atomic-flash-denied-return"
        } else {
            "atomic-prefunded-denied-return"
        };
        let mut negative = fork(&proof, label);
        let mut atomic = ixs.clone();
        let fail_index;
        if financing {
            let (b, r) = flash_pair(&negative, external, payoff);
            atomic.insert(0, b);
            fail_index = atomic.len();
            atomic.push(denied_return.clone());
            atomic.push(r);
        } else {
            fail_index = atomic.len();
            atomic.push(denied_return.clone());
        }
        let result = probe(&mut negative, label, &atomic, Some((fail_index, 6069)));
        let logs = result["logs"].as_array().unwrap();
        assert!(
            logs.iter().any(|l| l
                .as_str()
                .unwrap()
                .contains("Repaying obligation liquidity")),
            "repayment must execute before rejection"
        );
        assert!(
            logs.iter().any(|l| l
                .as_str()
                .unwrap()
                .contains("ProgramInteractionAccountConstraintViolated")),
            "must be policy rejection"
        );
    }
    let cash_before = proof.svm.get_account(&key(CASH)).unwrap();
    probe(&mut proof, "external-prefunded-repay", &ixs, None);
    assert_eq!(
        proof.svm.get_account(&key(CASH)).unwrap(),
        cash_before,
        "no inbound vault transfer"
    );
    let external_paid = EQUITY - amount(&proof.svm, &external.to_string());
    assert!(external_paid >= loan && external_paid <= payoff);
    proof.execute("REPORT_NAV", 0, None);
    let repaid = proof.compile("observe", 0, None).unwrap();
    assert_eq!(repaid["position"]["DebtRaw"], 0);
    let receipts = repaid["position"]["CollateralDepositedRaw"]
        .as_u64()
        .unwrap();
    proof.execute("withdraw", receipts, None);
    let exit = read(dir.join("exit-swap-request.json"));
    // Same source balance and valid route bytes, only the Jupiter destination
    // changes. Squads must reject before entering Jupiter, not at token debit.
    let request = swap_instruction(&exit, amount(&proof.svm, ONYC), 1, 0);
    let mut inner = Instruction {
        program_id: key(request["Instruction"]["programId"].as_str().unwrap()),
        data: bytes(&request["Instruction"], "data"),
        accounts: request["Instruction"]["accounts"]
            .as_array()
            .unwrap()
            .iter()
            .map(|a| AccountMeta {
                pubkey: key(a["pubkey"].as_str().unwrap()),
                is_signer: a["isSigner"].as_bool().unwrap(),
                is_writable: a["isWritable"].as_bool().unwrap(),
            })
            .collect(),
    };
    assert_eq!(inner.accounts[6].pubkey, key(CASH));
    inner.accounts[6].pubkey = external;
    let mut accounts = vec![];
    let compiled = loyal_actions::compile_squads_inner_instruction(&mut accounts, inner);
    let redirected = loyal_actions::execute_program_interaction_policy_instruction(
        key(exit["Policy"].as_str().unwrap()),
        key(EXECUTOR),
        0,
        vec![compiled],
        vec![exit["PolicyConstraintIndex"].as_u64().unwrap() as u8],
        accounts,
    );
    let mut negative = fork(&proof, "jupiter-external-destination");
    probe(
        &mut negative,
        "jupiter-external-destination",
        &[redirected],
        Some((0, 6069)),
    );
    proof.swap(&exit, amount(&proof.svm, ONYC), false);
    // Positive control: exact current stage policy permits only strategy custody.
    let mut allowed = fork(&proof, "allowed-stage-control");
    let canonical = reimbursement(
        key("EPCVCLY5wfumf6yPvqu7zuEB4WnnXbnPsy7JrKoAWcqC"),
        info.liquidity_mint,
        payoff,
    );
    probe(&mut allowed, "allowed-stage", &[canonical], None);
    let mut denied = fork(&proof, "denied-return-funded-vault");
    probe(
        &mut denied,
        "denied-return",
        &[denied_return],
        Some((0, 6069)),
    );
    let terminal = proof.compile("observe", 0, None).unwrap();
    assert_eq!(terminal["position"]["DebtRaw"], 0);
    assert_eq!(terminal["position"]["CollateralDepositedRaw"], 0);
    assert_eq!(
        terminal["nav"]["LPSupplyRaw"],
        initial["nav"]["LPSupplyRaw"]
    );
    write(
        proof.output.join("result.json"),
        &json!({"mechanicsPass":true,"fullExitProved":false,
        "selfFinancingExitProved":false,"signatureProof":false,"workerAdmissionProof":false,"broadcast":false,
        "initialSyntheticVaultEquityRaw":EQUITY,"initialSyntheticExternalUSDCraw":EQUITY,
        "loanRaw":loan,"externalPayerSpentRaw":external_paid,"financierReimbursedRaw":0,
        "externalUSDCremainingRaw":amount(&proof.svm,&external.to_string()),"vaultCashRaw":amount(&proof.svm,CASH),
        "flashPairPass":true,"externalRepayPass":true,"currentPolicyReturnRejected":true,
        "atomicPrefundingAndFlashRollbackPass":true,"jupiterExternalDestinationRejected":true,"requestedPayoffRaw":payoff,"initial":initial,"leveraged":leveraged,"repaid":repaid,"terminal":terminal,
        "limits":"20k under-cap loan; no higher leverage; no policy or risk change; synthetic external capital excluded from self-financing claim"}),
    );
}

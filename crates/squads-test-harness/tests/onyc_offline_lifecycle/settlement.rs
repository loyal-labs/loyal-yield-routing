//! Opt-in candidate composition. No deployed authority is changed.
use super::*;

fn instructions(proof: &Proof, compiled: &Value) -> Vec<Instruction> {
    let tx: VersionedTransaction = bincode::deserialize(&bytes(compiled, "wireBase64")).unwrap();
    let mut keys = tx.message.static_account_keys().to_vec();
    if let VersionedMessage::V0(message) = &tx.message {
        let mut readonly = Vec::new();
        for lookup in &message.address_table_lookups {
            let account = proof.svm.get_account(&lookup.account_key).unwrap();
            let table = AddressLookupTable::deserialize(&account.data).unwrap();
            keys.extend(
                lookup
                    .writable_indexes
                    .iter()
                    .map(|i| table.addresses[*i as usize]),
            );
            readonly.extend(
                lookup
                    .readonly_indexes
                    .iter()
                    .map(|i| table.addresses[*i as usize]),
            );
        }
        keys.extend(readonly);
    }
    tx.message
        .instructions()
        .iter()
        .map(|ix| Instruction {
            program_id: keys[ix.program_id_index as usize],
            accounts: ix
                .accounts
                .iter()
                .map(|i| AccountMeta {
                    pubkey: keys[*i as usize],
                    is_signer: tx.message.is_signer(*i as usize),
                    is_writable: tx.message.is_maybe_writable(*i as usize, None),
                })
                .collect(),
            data: ix.data.clone(),
        })
        .collect()
}

fn composition(proof: &Proof, ixs: &[Instruction], table_key: Pubkey) -> Value {
    let account = proof.svm.get_account(&table_key).unwrap();
    let table = AddressLookupTable::deserialize(&account.data).unwrap();
    let message = v0::Message::try_compile(
        &key(EXECUTOR),
        ixs,
        &[AddressLookupTableAccount {
            key: table_key,
            addresses: table.addresses.to_vec(),
        }],
        proof.svm.latest_blockhash(),
    )
    .unwrap();
    assert_eq!(message.header.num_required_signatures, 1);
    assert_eq!(message.account_keys[0], key(EXECUTOR));
    for ix in ixs {
        assert!(ix
            .accounts
            .iter()
            .all(|a| !a.is_signer || a.pubkey == key(EXECUTOR)));
    }
    let static_keys = message.account_keys.clone();
    let loaded: usize = message
        .address_table_lookups
        .iter()
        .map(|l| l.writable_indexes.len() + l.readonly_indexes.len())
        .sum();
    let tx = VersionedTransaction {
        signatures: vec![Signature::default()],
        message: VersionedMessage::V0(message),
    };
    let wire = bincode::serialize(&tx).unwrap();
    json!({"wireBase64": STANDARD.encode(&wire), "wireSha256": sha(&wire),
        "packetBytes": wire.len(), "packetLimit": 1232, "fits": wire.len() <= 1232,
        "staticAccounts": static_keys.iter().map(ToString::to_string).collect::<Vec<_>>(),
        "loadedAccountCount": loaded, "totalAccountCount": static_keys.len()+loaded,
        "topLevelInstructionCount": ixs.len(), "topLevelSigners": [EXECUTOR],
        "lookupTable": table_key.to_string(), "lookupTableDataSHA256": sha(&account.data),
        "signatureProof": false, "scope": "pre-execution wire sizing; no outcome claim"})
}

// Candidate infrastructure is created by the real ALT program before the
// capital scenario. No captured lookup bytes are edited or relabeled as live.
fn create_candidate_table(proof: &mut Proof) -> Pubkey {
    use solana_sdk::{address_lookup_table::instruction as alt, slot_hashes::SlotHashes};
    let before_clock = proof.svm.get_sysvar::<Clock>();
    let hashes = proof.svm.get_sysvar::<SlotHashes>();
    let recent = hashes.first().expect("local runtime SlotHashes").0;
    let (create, address) = alt::create_lookup_table(key(EXECUTOR), key(EXECUTOR), recent);
    assert!(proof.svm.get_account(&address).is_none());
    let mut addresses = proof.addresses.clone();
    addresses.retain(|a| *a != key(EXECUTOR));
    addresses.sort();
    addresses.dedup();
    assert!(addresses.len() <= 256);
    probe(proof, "candidate-alt-create", &[create], None);
    for (i, chunk) in addresses.chunks(20).enumerate() {
        let extend =
            alt::extend_lookup_table(address, key(EXECUTOR), Some(key(EXECUTOR)), chunk.to_vec());
        probe(proof, &format!("candidate-alt-extend-{i}"), &[extend], None);
    }
    proof.svm.warp_to_slot(before_clock.slot + 1);
    let account = proof.svm.get_account(&address).unwrap();
    let table = AddressLookupTable::deserialize(&account.data).unwrap();
    assert_eq!(table.addresses.as_ref(), addresses.as_slice());
    assert!(table.meta.last_extended_slot < proof.svm.get_sysvar::<Clock>().slot);
    write(
        proof.output.join("candidate-alt.json"),
        &json!({
        "address": address.to_string(), "authority": EXECUTOR,
        "recentSlotFromLocalRuntime": recent, "createdAtSlot": before_clock.slot,
        "warmedAtSlot": proof.svm.get_sysvar::<Clock>().slot,
        "accountDataSHA256": sha(&account.data), "addressCount": addresses.len(),
        "createdByActualInstructions": true, "capturedTableModified": false,
        "installedOnChain": false, "signatureProof": false,
        "clockChange": "one local slot for ALT warmup before position entry; timestamp/oracle bytes unchanged"}),
    );
    address
}

#[test]
#[ignore = "explicit current snapshot and resource-bounded native compiler required"]
fn onyc_atomic_exit_prefunding_feasibility() {
    run_prefunded(false, 0);
}

fn run_prefunded(guarded: bool, retained_cash: u64) {
    let dir = PathBuf::from(std::env::var("ONYC_PROOF_DIR").unwrap());
    let output = PathBuf::from(std::env::var("ONYC_PROOF_OUTPUT").unwrap());
    let compiler = PathBuf::from(std::env::var("ONYC_PROOF_COMPILER").unwrap());
    let mut proof = Proof::load(&dir, output, compiler);
    proof
        .compile("observe", 0, None)
        .expect("current policies must validate");
    proof.synthetic_equity();
    let external = seed_external(&mut proof, EQUITY);
    let settlement = guarded.then(|| setup_adapter(&mut proof, external));
    let candidate_table = create_candidate_table(&mut proof);
    let initial_book = proof.compile("observe", 0, None).unwrap()["nav"].clone();
    // Snapshot the actual LP mint and all holders from the inherited fixture.
    let lp_mint = key("6tNheTBYSpQkfMLhcczKgmTLSGffK54npKMG1WQR2tvb");
    let lp_accounts: Vec<_> = proof
        .addresses
        .iter()
        .filter_map(|address| {
            proof
                .svm
                .get_account(address)
                .filter(|account| {
                    *address == lp_mint
                        || (account.data.len() == 165 && account.data[..32] == lp_mint.to_bytes())
                })
                .map(|account| (*address, account))
        })
        .collect();
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
    proof.execute("borrow", loan, None);
    proof.swap(&entry, loan, false);
    proof.execute("deposit", amount(&proof.svm, ONYC), None);
    let leveraged = proof.compile("observe", 0, None).unwrap();
    assert_eq!(amount(&proof.svm, CASH), 0);
    if retained_cash > 0 {
        let contribution = spl_token::instruction::transfer_checked(
            &spl_token::id(),
            &external,
            &key("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"),
            &key(CASH),
            &key(EXECUTOR),
            &[],
            retained_cash,
            6,
        )
        .unwrap();
        probe(
            &mut proof,
            "declared-existing-cash-contribution",
            &[contribution],
            None,
        );
    }
    let external_before = amount(&proof.svm, &external.to_string());
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
    let mut atomic = ctx
        .repay(key(EXECUTOR), &key(DEBT), external, payoff)
        .unwrap();
    let state =
        klend_interface::state::from_account_data::<klend_interface::state::Reserve>(&debt.data)
            .unwrap();
    atomic.last_mut().unwrap().accounts[5].pubkey = state.liquidity.supply_vault;

    // Size each following leg against real prior mutation, at the same Clock as
    // the eventual atomic bundle. This clone performs no intermediate NAV writes.
    let mut planning = fork(&proof, "planning");
    probe(&mut planning, "repay", &atomic, None);
    let after_repay = planning.compile("observe", 0, None).unwrap();
    assert_eq!(after_repay["position"]["DebtRaw"], 0);
    let receipts = after_repay["position"]["CollateralDepositedRaw"]
        .as_u64()
        .unwrap();
    let withdrawal = planning.compile("withdraw", receipts, None).unwrap();
    let withdraw_ixs = instructions(&planning, &withdrawal);
    probe(&mut planning, "withdraw", &withdraw_ixs, None);
    atomic.extend(withdraw_ixs);
    let exit = read(dir.join("exit-swap-request.json"));
    let raw = amount(&planning.svm, ONYC);
    let mut quote = fork(&planning, "sale-quote");
    let request = swap_instruction(&exit, raw, 1, 0);
    let compiled = quote.compile("swap", raw, Some(request)).unwrap();
    let quote_ixs = instructions(&quote, &compiled);
    let cash_before = amount(&quote.svm, CASH);
    probe(&mut quote, "sale", &quote_ixs, None);
    let quoted = amount(&quote.svm, CASH) - cash_before;
    let request = swap_instruction(&exit, raw, quoted, 50);
    let compiled = planning.compile("swap", raw, Some(request)).unwrap();
    let swap_ixs = instructions(&planning, &compiled);
    probe(&mut planning, "sale", &swap_ixs, None);
    atomic.extend(swap_ixs);
    let actual_paid = external_before - amount(&planning.svm, &external.to_string());
    if let Some(settlement) = settlement {
        atomic[3] = begin_instruction(&atomic[3]);
        atomic.push(settlement);
    }

    let captured_sizing = composition(
        &proof,
        &atomic,
        key("FyJcsUtFnzisYya2y82qSuUTTo8gm2gyc8MokSKfWnnY"),
    );
    write(
        proof.output.join("captured-alt-prefix-composition.json"),
        &captured_sizing,
    );
    let sizing = composition(&proof, &atomic, candidate_table);
    write(proof.output.join("prefix-composition.json"), &sizing);
    assert_eq!(
        sizing["fits"], true,
        "complete native exit prefix exceeds packet limit: {sizing}"
    );
    let negatives = if guarded {
        negative_cases(&proof, &atomic, candidate_table)
    } else {
        Value::Null
    };
    let mut replay_bank = guarded.then(|| fork(&proof, "consumed-receipt-replay"));
    let execution = probe_with_tables(
        &mut proof,
        "atomic-prefix",
        &atomic,
        None,
        &[candidate_table],
    );
    let reimbursed = if guarded { actual_paid } else { 0 };
    assert_eq!(
        amount(&proof.svm, CASH),
        amount(&planning.svm, CASH) - reimbursed
    );
    assert_eq!(
        amount(&proof.svm, &external.to_string()),
        amount(&planning.svm, &external.to_string()) + reimbursed
    );
    assert_eq!(amount(&proof.svm, ONYC), 0);
    let terminal = proof.compile("observe", 0, None).unwrap();
    assert_eq!(terminal["position"]["DebtRaw"], 0);
    assert_eq!(terminal["position"]["CollateralDepositedRaw"], 0);
    let settled_cash = amount(&proof.svm, CASH);
    let mut replay_results = Value::Null;
    if let Some(ref mut replay) = replay_bank {
        let consumed = proof.svm.get_account(&candidate_receipt()).unwrap();
        assert_eq!(consumed.data.len(), 64);
        assert_eq!(consumed.data[8], 2);
        assert_eq!(
            u64::from_le_bytes(consumed.data[19..27].try_into().unwrap()),
            actual_paid
        );
        // Adversarial historical receipt fixture only: financial/protocol
        // prestate stays unchanged. A used record cannot pay a fresh position.
        replay
            .svm
            .set_account(candidate_receipt(), consumed.clone())
            .unwrap();
        replay.svm.expire_blockhash();
        let used = probe_with_tables(
            replay,
            "consumed-receipt-replay",
            &atomic,
            Some((3, 7004)),
            &[candidate_table],
        );
        let mut stale = fork(replay, "stale-active-receipt");
        let mut stale_record = consumed;
        stale_record.data[8] = 1;
        stale
            .svm
            .set_account(candidate_receipt(), stale_record)
            .unwrap();
        let active = probe_with_tables(
            &mut stale,
            "stale-active-receipt",
            &atomic,
            Some((3, 7004)),
            &[candidate_table],
        );
        replay_results = json!({"consumed":used["error"],"staleActive":active["error"],"historicalReceiptFixtureOnly":true,"rollbackPass":true});
    }
    let mut accounting = Value::Null;
    if guarded {
        proof.execute("REPORT_NAV", 0, None);
        proof.restore();
        proof.execute("REPORT_NAV", 0, None);
        let terminal = proof.compile("observe", 0, None).unwrap();
        assert_eq!(amount(&proof.svm, CASH), 0);
        assert_eq!(amount(&proof.svm, ONYC), 0);
        assert_eq!(terminal["position"]["DebtRaw"], 0);
        assert_eq!(terminal["position"]["CollateralDepositedRaw"], 0);
        assert_eq!(terminal["nav"]["StrategyNAVRaw"], 0);
        assert_eq!(terminal["nav"]["Receipt"]["PositionValueRaw"], 0);
        assert_eq!(terminal["nav"]["VaultIdleRaw"], settled_cash);
        assert_eq!(terminal["nav"]["Voltr"]["TotalValueRaw"], settled_cash);
        assert_eq!(terminal["nav"]["LPSupplyRaw"], initial_book["LPSupplyRaw"]);
        for (address, initial) in lp_accounts {
            assert_eq!(
                proof.svm.get_account(&address).unwrap(),
                initial,
                "LP mint/holder changed"
            );
        }
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
                "fee config changed: {field}"
            );
        }
        let effective = |nav: &Value| -> u64 {
            nav["LPSupplyRaw"].as_u64().unwrap()
                + [
                    "FeeAccumulatorManagerRaw",
                    "FeeAccumulatorAdminRaw",
                    "FeeAccumulatorProtocolRaw",
                    "LPSupplyDeadWeightRaw",
                ]
                .iter()
                .map(|f| nav["Voltr"][f].as_u64().unwrap())
                .sum::<u64>()
        };
        let normalized = (u128::from(settled_cash) * u128::from(effective(&initial_book))
            / u128::from(effective(&terminal["nav"]))) as u64;
        accounting = json!({"pass":true,"initialBook":initial_book,"terminal":terminal,
            "normalizedIncumbentLPWealthRaw":normalized,"normalizedIncumbentLPCostRaw":i128::from(EQUITY+retained_cash)-i128::from(normalized),
            "feePayerSOLAndRentExcluded":true,"declaredExistingCashContributionRaw":retained_cash});
    }
    write(
        proof.output.join("result.json"),
        &json!({
        "prefixPass": true, "guardedPrefundingPass": guarded, "prefundedCandidateFullExitProved": guarded,
        "flashFinancedExitProved": false, "positiveFinancingFeeProved": false, "candidatePolicyOnly": guarded, "fullExitProved": false, "financierReimbursedRaw": reimbursed,
        "externalPayerSpentRaw": actual_paid, "externalNetUSDCSpentRaw": external_before - amount(&proof.svm, &external.to_string()), "negativeCases": negatives,
        "replayCases":replay_results,"accounting":accounting,"initialVaultCashRaw":retained_cash,"vaultCashAfterSettlementRaw":settled_cash,
        "vaultCashRaw": amount(&proof.svm, CASH), "quotedSaleOutputRaw": quoted,
        "packetBytes": sizing["packetBytes"], "computeUnits": execution["computeUnits"],
        "signatureProof": false, "workerAdmissionProof": false, "broadcast": false,
        "clock": "planning and atomic bundle use identical Clock; no oracle rewrites",
        "scope": if guarded { "offline prefunded repay-release-sale-settlement with native terminal NAV/LP; no flash funding, signature or production-admission proof" } else { "prefunded repay-release-sale without reimbursement" }}),
    );
}

const CANDIDATE_PROGRAM: Pubkey = Pubkey::new_from_array([73; 32]);
const OPERATION: u64 = 1;

fn candidate_receipt() -> Pubkey {
    Pubkey::find_program_address(
        &[
            b"onyc-repay-v1",
            key(OBLIGATION).as_ref(),
            key(EXECUTOR).as_ref(),
            &OPERATION.to_le_bytes(),
        ],
        &CANDIDATE_PROGRAM,
    )
    .0
}

fn settle_inner(external: Pubkey) -> Instruction {
    let mut data = b"ONYCEND1".to_vec();
    data.extend(OPERATION.to_le_bytes());
    Instruction {
        program_id: CANDIDATE_PROGRAM,
        accounts: vec![
            AccountMeta::new(candidate_receipt(), false),
            AccountMeta::new_readonly(key(VAULT), true),
            AccountMeta::new(key(CASH), false),
            AccountMeta::new(external, false),
            AccountMeta::new_readonly(key("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"), false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new_readonly(key(OBLIGATION), false),
            AccountMeta::new_readonly(key(ONYC), false),
            AccountMeta::new_readonly(solana_sdk::sysvar::instructions::ID, false),
            AccountMeta::new_readonly(key(EXECUTOR), false),
        ],
        data,
    }
}

fn setup_adapter(proof: &mut Proof, external: Pubkey) -> Instruction {
    use loyal_actions::{
        create_deployed_semantic_program_interaction_policy_instruction, derive_action_account,
        SemanticProgramInteractionConstraint, SemanticProgramInteractionDataConstraint,
    };
    use solana_sdk::message::Message;
    let elf_path =
        PathBuf::from(std::env::var("ONYC_SETTLEMENT_PROGRAM_SO").expect("explicit candidate SBF"));
    let elf = fs::read(&elf_path).unwrap();
    proof.svm.add_program(CANDIDATE_PROGRAM, &elf).unwrap();
    let settings = key("5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6");
    let admin = key("BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ");
    let settings_data = proof.svm.get_account(&settings).unwrap().data;
    assert_eq!(settings_data[158], 1);
    let seed = u64::from_le_bytes(settings_data[159..167].try_into().unwrap())
        .checked_add(1)
        .unwrap();
    let policy = derive_action_account(&settings, seed).0;
    assert!(proof.svm.get_account(&policy).is_none());
    let before = proof.accounts();
    let admin_before = proof.svm.get_account(&admin);
    let mut account = admin_before.clone().unwrap_or_default();
    assert!(account.data.is_empty() && account.owner == solana_sdk::system_program::ID);
    account.lamports = 100_000_000;
    proof.svm.set_account(admin, account).unwrap();
    for address in [admin, policy, candidate_receipt(), CANDIDATE_PROGRAM] {
        if !proof.addresses.contains(&address) {
            proof.addresses.push(address);
        }
    }
    let inner = settle_inner(external);
    let spec = SemanticProgramInteractionConstraint {
        program_id: CANDIDATE_PROGRAM,
        account_pubkeys: inner
            .accounts
            .iter()
            .enumerate()
            .map(|(i, a)| (i as u8, vec![a.pubkey]))
            .collect(),
        account_data: vec![],
        data: vec![SemanticProgramInteractionDataConstraint::SliceEquals {
            offset: 0,
            value: b"ONYCEND1".to_vec(),
        }],
    };
    let create = create_deployed_semantic_program_interaction_policy_instruction(
        settings,
        admin,
        key(EXECUTOR),
        seed,
        0,
        vec![spec],
    )
    .unwrap();
    let tx = VersionedTransaction {
        signatures: vec![Signature::default()],
        message: VersionedMessage::Legacy(Message::new_with_blockhash(
            &[create],
            Some(&admin),
            &proof.svm.latest_blockhash(),
        )),
    };
    let wire = bincode::serialize(&tx).unwrap();
    assert!(wire.len() <= 1232);
    let meta = proof
        .svm
        .send_transaction(tx)
        .expect("real local Squads PolicyCreate");
    write(
        proof.output.join("candidate-policy.json"),
        &json!({
        "policy":policy.to_string(),"seed":seed,"before":before,"after":proof.accounts(),
        "wireBase64":STANDARD.encode(&wire),"packetBytes":wire.len(),"logs":meta.logs,
        "program":CANDIDATE_PROGRAM.to_string(),"elfSHA256":sha(&elf),"elfPath":elf_path,
        "admin":admin.to_string(),"adminLamportsBefore":admin_before.map(|a|a.lamports),
        "syntheticAdminLamports":100_000_000u64,"signatureProof":false,"installedOnChain":false,
        "authority":"local unsigned administrator creates only candidate settlement policy; production permissions unchanged"}),
    );
    let mut accounts = vec![];
    let compiled = loyal_actions::compile_squads_inner_instruction(&mut accounts, inner);
    loyal_actions::execute_program_interaction_policy_instruction(
        policy,
        key(EXECUTOR),
        0,
        vec![compiled],
        vec![0],
        accounts,
    )
}

fn begin_instruction(repay: &Instruction) -> Instruction {
    assert_eq!(
        repay.program_id,
        key("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD")
    );
    assert_eq!(repay.accounts.len(), 14);
    assert_eq!(repay.data.len(), 16);
    let mut data = b"ONYCBEG1".to_vec();
    data.extend(OPERATION.to_le_bytes());
    data.extend(&repay.data[8..16]);
    data.extend(0u64.to_le_bytes()); // First candidate authorizes no financing fee.
    data.extend(0u64.to_le_bytes()); // Preserve all pre-existing vault cash, plus sale surplus.
    let mut accounts = repay.accounts.clone();
    accounts.extend([
        AccountMeta::new(candidate_receipt(), false),
        AccountMeta::new_readonly(key(CASH), false),
        AccountMeta::new_readonly(key(VAULT), false),
        AccountMeta::new_readonly(key(ONYC), false),
        AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
        AccountMeta::new_readonly(repay.program_id, false),
    ]);
    Instruction {
        program_id: CANDIDATE_PROGRAM,
        accounts,
        data,
    }
}

#[test]
#[ignore = "explicit candidate SBF, public snapshot and bounded native compiler required"]
fn onyc_guarded_prefunded_exit() {
    run_prefunded(true, 0);
}

#[test]
#[ignore = "explicit candidate SBF, public snapshot and bounded native compiler required"]
fn onyc_guard_preserves_existing_cash() {
    run_prefunded(true, 100_000_000);
}

fn negative_cases(proof: &Proof, atomic: &[Instruction], table: Pubkey) -> Value {
    let mut cases: Vec<(&str, Vec<Instruction>, usize, u32, bool)> = Vec::new();
    for label in [
        "standalone-settlement",
        "missing-settlement",
        "duplicate-settlement",
        "wrong-policy",
        "wrong-recipient",
        "wrong-settlement-nonce",
        "extra-inner",
        "extra-outer-account",
        "duplicate-outer-account",
        "fee",
        "zero-payment",
        "partial-payment",
        "wrong-payer-custody",
        "wrong-obligation",
        "wrong-reserve",
        "wrong-vault",
        "wrong-token-program",
        "wrong-sysvar",
        "wrong-operation",
        "above-principal-cap",
        "impossible-swap-minimum",
        "changed-swap-route-id",
        "changed-swap-direction",
        "insufficient-proceeds",
    ] {
        let mut bad = atomic.to_vec();
        let mut index = 3;
        let mut code = 7002;
        let mut paid_before_rejection = false;
        match label {
            "standalone-settlement" => {
                bad = vec![bad[9].clone()];
                index = 0;
            }
            "missing-settlement" => {
                bad.pop();
            }
            "duplicate-settlement" => bad.push(bad[9].clone()),
            "wrong-policy" => bad[9].accounts[0].pubkey = key(STAGE),
            "wrong-recipient" => {
                let x = bad[3].accounts[6].pubkey;
                bad[9]
                    .accounts
                    .iter_mut()
                    .find(|a| a.pubkey == x)
                    .unwrap()
                    .pubkey = key(CASH);
            }
            "wrong-settlement-nonce" => {
                let n = bad[9].data.len();
                bad[9].data[n - 8..].copy_from_slice(&2u64.to_le_bytes());
            }
            "extra-inner" => {
                let inner = settle_inner(bad[3].accounts[6].pubkey);
                let mut accounts = vec![];
                let compiled =
                    loyal_actions::compile_squads_inner_instruction(&mut accounts, inner);
                bad[9] = loyal_actions::execute_program_interaction_policy_instruction(
                    bad[9].accounts[0].pubkey,
                    key(EXECUTOR),
                    0,
                    vec![compiled.clone(), compiled],
                    vec![0, 0],
                    accounts,
                );
            }
            "extra-outer-account" => bad[9]
                .accounts
                .push(AccountMeta::new_readonly(key(STAGE), false)),
            "duplicate-outer-account" => {
                let duplicate = bad[9].accounts[3].clone();
                bad[9].accounts.push(duplicate);
            }
            "fee" => {
                bad[3].data[24..32].copy_from_slice(&1u64.to_le_bytes());
                code = 7007;
            }
            "zero-payment" => {
                bad[3].data[16..24].fill(0);
                code = 7006;
            }
            "partial-payment" => {
                bad[3].data[16..24].copy_from_slice(&10_000_000_000u64.to_le_bytes());
                code = 7005;
                paid_before_rejection = true;
            }
            "wrong-payer-custody" => {
                bad[3].accounts[6].pubkey = key(CASH);
                code = 7001;
            }
            "wrong-obligation" => {
                bad[3].accounts[1].pubkey = key(CASH);
                code = 7001;
            }
            "wrong-reserve" => {
                bad[3].accounts[3].pubkey = key(COLLATERAL);
                code = 7001;
            }
            "wrong-vault" => {
                bad[3].accounts[16].pubkey = key(EXECUTOR);
                code = 7001;
            }
            "wrong-token-program" => {
                bad[3].accounts[7].pubkey = key(DEBT);
                code = 7001;
            }
            "wrong-sysvar" => {
                bad[3].accounts[8].pubkey = solana_sdk::sysvar::clock::ID;
                code = 7001;
            }
            "wrong-operation" => {
                bad[3].data[8..16].copy_from_slice(&2u64.to_le_bytes());
                code = 7000;
            }
            "above-principal-cap" => {
                bad[3].data[16..24].copy_from_slice(&100_000_000_001u64.to_le_bytes());
                code = 7006;
            }
            "impossible-swap-minimum" => {
                let start = bad[8].data.len() - 38;
                let quoted =
                    u64::from_le_bytes(bad[8].data[start + 27..start + 35].try_into().unwrap());
                bad[8].data[start + 27..start + 35].copy_from_slice(&(quoted * 2).to_le_bytes());
                index = 8;
                code = 6001;
                paid_before_rejection = true;
            }
            "changed-swap-route-id" => {
                let start = bad[8].data.len() - 38;
                bad[8].data[start + 8] = 6;
            }
            "changed-swap-direction" => {
                let start = bad[8].data.len() - 38;
                bad[8].data[start + 14] ^= 1;
            }
            "insufficient-proceeds" => {
                bad[3].data[32..40].copy_from_slice(&100_000_000_000u64.to_le_bytes());
                index = 9;
                code = 7008;
                paid_before_rejection = true;
            }
            _ => unreachable!(),
        }
        cases.push((label, bad, index, code, paid_before_rejection));
    }
    let mut results = vec![];
    for (label, ixs, index, code, paid) in cases {
        let mut rejected = fork(proof, label);
        let result = probe_with_tables(&mut rejected, label, &ixs, Some((index, code)), &[table]);
        let logs = result["logs"].as_array().unwrap();
        assert_eq!(
            logs.iter().any(|l| l
                .as_str()
                .unwrap()
                .contains("Repaying obligation liquidity")),
            paid,
            "{label}: repayment execution ordering"
        );
        if index == 9 {
            assert!(logs
                .iter()
                .any(|l| l.as_str().unwrap().contains("Closing account")));
            assert!(
                logs.iter().any(|l| l
                    .as_str()
                    .unwrap()
                    .contains("Program SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG failed")),
                "settlement error must propagate through Squads"
            );
        }
        results.push(json!({"label":label,"error":result["error"],"packetBytes":result["packetBytes"],"rollbackPass":true,"repaymentExecutedBeforeRejection":paid}));
    }
    json!(results)
}

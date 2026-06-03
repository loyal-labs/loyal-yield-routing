#[allow(deprecated)]
use solana_sdk::address_lookup_table::{
    self,
    state::{AddressLookupTable, LookupTableMeta},
    AddressLookupTableAccount,
};
use solana_sdk::{
    account::Account,
    compute_budget::ComputeBudgetInstruction,
    instruction::{AccountMeta, Instruction},
    message::{v0, Message, VersionedMessage},
    pubkey::Pubkey,
    signature::Keypair,
    signer::Signer,
    transaction::{Transaction, VersionedTransaction},
};
use squads_test_harness::prelude::*;
use std::{borrow::Cow, fmt::Write as _};

const SWAP_AMOUNT: u64 = 1_000;
const MAX_BATCH_PROBE: usize = 16;
const MAX_COMPACT_BATCH_PROBE: usize = 96;
const COMPACT_BATCH_DUMP_SWAP_COUNT: usize = 20;
const SOLANA_PACKET_DATA_SIZE: usize = 1_232;
const JUPITER_BATCH_PROGRAM_ID: Pubkey = Pubkey::new_from_array([66; 32]);
const MOCK_JUPITER_BATCH_EXACT_IN: [u8; 8] = [4, 0, 0, 0, 0, 0, 0, 0];

#[derive(Clone, Debug)]
struct BatchAttempt {
    swap_count: usize,
    serialized_bytes: usize,
    account_count: usize,
    static_account_count: usize,
    lookup_account_count: usize,
    signer_count: usize,
}

#[test]
#[ignore = "capacity probe for local LiteSVM transaction limits"]
fn probes_max_direct_mock_jupiter_swaps_for_multiple_users_in_one_transaction() {
    let mut last_success = None;
    let mut first_failure = None;

    for swap_count in 1..=MAX_BATCH_PROBE {
        let attempt = attempt_multi_user_jupiter_swap_batch(swap_count);
        match attempt {
            Ok(attempt) => {
                eprintln!(
                    "ok swaps={} bytes={} accounts={} signers={}",
                    attempt.swap_count,
                    attempt.serialized_bytes,
                    attempt.account_count,
                    attempt.signer_count
                );
                last_success = Some(attempt);
            }
            Err((attempt, error)) => {
                eprintln!(
                    "fail swaps={} bytes={} accounts={} signers={} error={}",
                    attempt.swap_count,
                    attempt.serialized_bytes,
                    attempt.account_count,
                    attempt.signer_count,
                    error
                );
                first_failure = Some((attempt, error));
                break;
            }
        }
    }

    let last_success =
        last_success.expect("at least one direct mock Jupiter swap should fit in one transaction");
    let (first_failure, first_failure_error) =
        first_failure.expect("probe should find the first batch size that no longer fits");

    assert_eq!(
        last_success.swap_count + 1,
        first_failure.swap_count,
        "probe should stop at the first failing batch size"
    );
    eprintln!(
        "max direct mock Jupiter multi-user swaps in one transaction: {} (next={} failed: {})",
        last_success.swap_count, first_failure.swap_count, first_failure_error
    );
}

#[test]
#[ignore = "capacity probe for local LiteSVM transaction limits"]
fn probes_max_v0_alt_mock_jupiter_swaps_for_multiple_users_in_one_transaction() {
    let mut last_success = None;
    let mut first_failure = None;

    for swap_count in 1..=MAX_BATCH_PROBE {
        let attempt = attempt_multi_user_jupiter_swap_batch_v0_alt(swap_count);
        match attempt {
            Ok(attempt) => {
                eprintln!(
                    "ok v0_alt swaps={} bytes={} static_accounts={} lookup_accounts={} total_accounts={} signers={}",
                    attempt.swap_count,
                    attempt.serialized_bytes,
                    attempt.static_account_count,
                    attempt.lookup_account_count,
                    attempt.account_count,
                    attempt.signer_count
                );
                last_success = Some(attempt);
            }
            Err((attempt, error)) => {
                eprintln!(
                    "fail v0_alt swaps={} bytes={} static_accounts={} lookup_accounts={} total_accounts={} signers={} error={}",
                    attempt.swap_count,
                    attempt.serialized_bytes,
                    attempt.static_account_count,
                    attempt.lookup_account_count,
                    attempt.account_count,
                    attempt.signer_count,
                    error
                );
                first_failure = Some((attempt, error));
                break;
            }
        }
    }

    let last_success =
        last_success.expect("at least one v0 ALT mock Jupiter swap should fit in one transaction");
    let (first_failure, first_failure_error) =
        first_failure.expect("probe should find the first v0 ALT batch size that no longer fits");

    assert_eq!(
        last_success.swap_count + 1,
        first_failure.swap_count,
        "probe should stop at the first failing v0 ALT batch size"
    );
    eprintln!(
        "max v0 ALT mock Jupiter multi-user swaps in one transaction: {} (next={} failed: {})",
        last_success.swap_count, first_failure.swap_count, first_failure_error
    );
}

#[test]
#[ignore = "capacity probe for local LiteSVM transaction limits"]
fn probes_max_v0_alt_mock_jupiter_swaps_with_single_batch_signer_in_one_transaction() {
    let mut last_success = None;
    let mut first_failure = None;

    for swap_count in 1..=MAX_BATCH_PROBE {
        let attempt = attempt_single_signer_jupiter_swap_batch_v0_alt(swap_count);
        match attempt {
            Ok(attempt) => {
                eprintln!(
                    "ok v0_alt_single_signer swaps={} bytes={} static_accounts={} lookup_accounts={} total_accounts={} signers={}",
                    attempt.swap_count,
                    attempt.serialized_bytes,
                    attempt.static_account_count,
                    attempt.lookup_account_count,
                    attempt.account_count,
                    attempt.signer_count
                );
                last_success = Some(attempt);
            }
            Err((attempt, error)) => {
                eprintln!(
                    "fail v0_alt_single_signer swaps={} bytes={} static_accounts={} lookup_accounts={} total_accounts={} signers={} error={}",
                    attempt.swap_count,
                    attempt.serialized_bytes,
                    attempt.static_account_count,
                    attempt.lookup_account_count,
                    attempt.account_count,
                    attempt.signer_count,
                    error
                );
                first_failure = Some((attempt, error));
                break;
            }
        }
    }

    let last_success = last_success.expect(
        "at least one single-signer v0 ALT mock Jupiter swap should fit in one transaction",
    );
    let (first_failure, first_failure_error) = first_failure
        .expect("probe should find the first single-signer v0 ALT batch size that no longer fits");

    assert_eq!(
        last_success.swap_count + 1,
        first_failure.swap_count,
        "probe should stop at the first failing single-signer v0 ALT batch size"
    );
    eprintln!(
        "max single-signer v0 ALT mock Jupiter swaps in one transaction: {} (next={} failed: {})",
        last_success.swap_count, first_failure.swap_count, first_failure_error
    );
}

#[test]
#[ignore = "capacity probe for local LiteSVM transaction limits"]
fn probes_max_v0_alt_compact_batch_jupiter_cpi_swaps_in_one_transaction() {
    let mut last_success = None;
    let mut first_failure = None;

    for swap_count in 1..=MAX_COMPACT_BATCH_PROBE {
        let attempt = attempt_compact_batch_jupiter_cpi_swaps_v0_alt(swap_count);
        match attempt {
            Ok(attempt) => {
                eprintln!(
                    "ok compact_batch swaps={} bytes={} static_accounts={} lookup_accounts={} total_accounts={} signers={}",
                    attempt.swap_count,
                    attempt.serialized_bytes,
                    attempt.static_account_count,
                    attempt.lookup_account_count,
                    attempt.account_count,
                    attempt.signer_count
                );
                last_success = Some(attempt);
            }
            Err((attempt, error)) => {
                eprintln!(
                    "fail compact_batch swaps={} bytes={} static_accounts={} lookup_accounts={} total_accounts={} signers={} error={}",
                    attempt.swap_count,
                    attempt.serialized_bytes,
                    attempt.static_account_count,
                    attempt.lookup_account_count,
                    attempt.account_count,
                    attempt.signer_count,
                    error
                );
                first_failure = Some((attempt, error));
                break;
            }
        }
    }

    let last_success =
        last_success.expect("at least one compact batch swap should fit in one transaction");
    let (first_failure, first_failure_error) =
        first_failure.expect("probe should find the first compact batch size that no longer fits");

    assert_eq!(
        last_success.swap_count + 1,
        first_failure.swap_count,
        "probe should stop at the first failing compact batch size"
    );
    eprintln!(
        "max compact batch Jupiter CPI swaps in one transaction: {} (next={} failed: {})",
        last_success.swap_count, first_failure.swap_count, first_failure_error
    );
}

#[test]
#[ignore = "prints a deterministic serialized transaction byte layout for docs"]
fn dumps_serialized_v0_alt_compact_batch_transaction_layout() {
    let fixture = setup_compact_batch_jupiter_cpi_swaps_with_key_source(
        COMPACT_BATCH_DUMP_SWAP_COUNT,
        CompactBatchKeySource::Deterministic,
    );
    let built =
        build_compact_batch_jupiter_cpi_transaction_v0_alt(fixture, deterministic_pubkey(0x90, 0))
            .expect("build deterministic compact batch transaction");

    eprintln!(
        "{}",
        serialized_transaction_dump(&built.transaction, COMPACT_BATCH_DUMP_SWAP_COUNT)
    );

    let CompactBatchTransaction {
        mut svm,
        transaction,
        attempt,
    } = built;
    svm.send_transaction(transaction)
        .expect("deterministic compact batch transaction should execute");
    assert_eq!(attempt.serialized_bytes, 690);
}

fn attempt_multi_user_jupiter_swap_batch(
    swap_count: usize,
) -> Result<BatchAttempt, (BatchAttempt, String)> {
    let BatchFixture {
        mut svm,
        users,
        instructions,
        ..
    } = setup_multi_user_jupiter_swap_batch(swap_count);

    svm.expire_blockhash();
    let message = Message::new_with_blockhash(
        &instructions,
        Some(&users[0].pubkey()),
        &svm.latest_blockhash(),
    );
    let account_count = message.account_keys.len();
    let signer_count = users.len();
    let signers: Vec<&Keypair> = users.iter().collect();
    let transaction = Transaction::new(&signers, message, svm.latest_blockhash());
    let serialized_bytes = bincode::serialized_size(&transaction)
        .expect("measure serialized transaction size") as usize;
    let attempt = BatchAttempt {
        swap_count,
        serialized_bytes,
        account_count,
        static_account_count: account_count,
        lookup_account_count: 0,
        signer_count,
    };
    if serialized_bytes > SOLANA_PACKET_DATA_SIZE {
        return Err((
            attempt,
            format!("serialized transaction exceeds {SOLANA_PACKET_DATA_SIZE} byte packet limit"),
        ));
    }

    svm.send_transaction(transaction)
        .map(|_| attempt.clone())
        .map_err(|error| (attempt, format!("{:?}", error.err)))
}

fn attempt_multi_user_jupiter_swap_batch_v0_alt(
    swap_count: usize,
) -> Result<BatchAttempt, (BatchAttempt, String)> {
    let BatchFixture {
        mut svm,
        users,
        instructions,
        lookup_addresses,
    } = setup_multi_user_jupiter_swap_batch(swap_count);
    let lookup_table_key = Pubkey::new_unique();
    seed_address_lookup_table(&mut svm, lookup_table_key, lookup_addresses.clone());
    svm.warp_to_slot(1);
    svm.expire_blockhash();

    let lookup_table = AddressLookupTableAccount {
        key: lookup_table_key,
        addresses: lookup_addresses,
    };
    let message = match v0::Message::try_compile(
        &users[0].pubkey(),
        &instructions,
        &[lookup_table],
        svm.latest_blockhash(),
    ) {
        Ok(message) => message,
        Err(error) => {
            return Err((
                BatchAttempt::empty(swap_count, users.len()),
                format!("v0 message compile failed: {error:?}"),
            ))
        }
    };
    let static_account_count = message.account_keys.len();
    let lookup_account_count = message
        .address_table_lookups
        .iter()
        .map(|lookup| lookup.writable_indexes.len() + lookup.readonly_indexes.len())
        .sum::<usize>();
    let account_count = static_account_count + lookup_account_count;
    let signer_count = users.len();
    let versioned_message = VersionedMessage::V0(message);
    let signers: Vec<&Keypair> = users.iter().collect();
    let transaction = match VersionedTransaction::try_new(versioned_message, &signers) {
        Ok(transaction) => transaction,
        Err(error) => {
            return Err((
                BatchAttempt {
                    swap_count,
                    serialized_bytes: 0,
                    account_count,
                    static_account_count,
                    lookup_account_count,
                    signer_count,
                },
                format!("v0 transaction signing failed: {error:?}"),
            ))
        }
    };
    let serialized_bytes = bincode::serialized_size(&transaction)
        .expect("measure serialized v0 transaction size") as usize;
    let attempt = BatchAttempt {
        swap_count,
        serialized_bytes,
        account_count,
        static_account_count,
        lookup_account_count,
        signer_count,
    };
    if serialized_bytes > SOLANA_PACKET_DATA_SIZE {
        return Err((
            attempt,
            format!("serialized transaction exceeds {SOLANA_PACKET_DATA_SIZE} byte packet limit"),
        ));
    }

    svm.send_transaction(transaction)
        .map(|_| attempt.clone())
        .map_err(|error| (attempt, format!("{:?}", error.err)))
}

fn attempt_single_signer_jupiter_swap_batch_v0_alt(
    swap_count: usize,
) -> Result<BatchAttempt, (BatchAttempt, String)> {
    let BatchFixture {
        mut svm,
        users,
        instructions,
        lookup_addresses,
    } = setup_single_signer_jupiter_swap_batch(swap_count);
    let lookup_table_key = Pubkey::new_unique();
    seed_address_lookup_table(&mut svm, lookup_table_key, lookup_addresses.clone());
    svm.warp_to_slot(1);
    svm.expire_blockhash();

    let lookup_table = AddressLookupTableAccount {
        key: lookup_table_key,
        addresses: lookup_addresses,
    };
    let message = match v0::Message::try_compile(
        &users[0].pubkey(),
        &instructions,
        &[lookup_table],
        svm.latest_blockhash(),
    ) {
        Ok(message) => message,
        Err(error) => {
            return Err((
                BatchAttempt::empty(swap_count, users.len()),
                format!("v0 message compile failed: {error:?}"),
            ))
        }
    };
    let static_account_count = message.account_keys.len();
    let lookup_account_count = message
        .address_table_lookups
        .iter()
        .map(|lookup| lookup.writable_indexes.len() + lookup.readonly_indexes.len())
        .sum::<usize>();
    let account_count = static_account_count + lookup_account_count;
    let signer_count = users.len();
    let versioned_message = VersionedMessage::V0(message);
    let signers: Vec<&Keypair> = users.iter().collect();
    let transaction = match VersionedTransaction::try_new(versioned_message, &signers) {
        Ok(transaction) => transaction,
        Err(error) => {
            return Err((
                BatchAttempt {
                    swap_count,
                    serialized_bytes: 0,
                    account_count,
                    static_account_count,
                    lookup_account_count,
                    signer_count,
                },
                format!("v0 transaction signing failed: {error:?}"),
            ))
        }
    };
    let serialized_bytes = bincode::serialized_size(&transaction)
        .expect("measure serialized single-signer v0 transaction size")
        as usize;
    let attempt = BatchAttempt {
        swap_count,
        serialized_bytes,
        account_count,
        static_account_count,
        lookup_account_count,
        signer_count,
    };
    if serialized_bytes > SOLANA_PACKET_DATA_SIZE {
        return Err((
            attempt,
            format!("serialized transaction exceeds {SOLANA_PACKET_DATA_SIZE} byte packet limit"),
        ));
    }

    svm.send_transaction(transaction)
        .map(|_| attempt.clone())
        .map_err(|error| (attempt, format!("{:?}", error.err)))
}

fn attempt_compact_batch_jupiter_cpi_swaps_v0_alt(
    swap_count: usize,
) -> Result<BatchAttempt, (BatchAttempt, String)> {
    let fixture = setup_compact_batch_jupiter_cpi_swaps(swap_count);
    let built = build_compact_batch_jupiter_cpi_transaction_v0_alt(fixture, Pubkey::new_unique())?;
    let CompactBatchTransaction {
        mut svm,
        transaction,
        attempt,
    } = built;

    svm.send_transaction(transaction)
        .map(|_| attempt.clone())
        .map_err(|error| (attempt, format!("{:?}", error.err)))
}

fn build_compact_batch_jupiter_cpi_transaction_v0_alt(
    fixture: CompactBatchFixture,
    lookup_table_key: Pubkey,
) -> Result<CompactBatchTransaction, (BatchAttempt, String)> {
    let CompactBatchFixture {
        mut svm,
        swap_count,
        signer,
        instruction,
        lookup_addresses,
    } = fixture;
    seed_address_lookup_table(&mut svm, lookup_table_key, lookup_addresses.clone());
    svm.warp_to_slot(1);
    svm.expire_blockhash();

    let lookup_table = AddressLookupTableAccount {
        key: lookup_table_key,
        addresses: lookup_addresses,
    };
    let instructions = vec![
        ComputeBudgetInstruction::set_compute_unit_limit(1_400_000),
        instruction,
    ];
    let message = match v0::Message::try_compile(
        &signer.pubkey(),
        &instructions,
        &[lookup_table],
        svm.latest_blockhash(),
    ) {
        Ok(message) => message,
        Err(error) => {
            return Err((
                BatchAttempt::empty(swap_count, 1),
                format!("v0 compact batch message compile failed: {error:?}"),
            ))
        }
    };
    let static_account_count = message.account_keys.len();
    let lookup_account_count = message
        .address_table_lookups
        .iter()
        .map(|lookup| lookup.writable_indexes.len() + lookup.readonly_indexes.len())
        .sum::<usize>();
    let account_count = static_account_count + lookup_account_count;
    let versioned_message = VersionedMessage::V0(message);
    let transaction = match VersionedTransaction::try_new(versioned_message, &[&signer]) {
        Ok(transaction) => transaction,
        Err(error) => {
            return Err((
                BatchAttempt {
                    swap_count,
                    serialized_bytes: 0,
                    account_count,
                    static_account_count,
                    lookup_account_count,
                    signer_count: 1,
                },
                format!("v0 compact batch transaction signing failed: {error:?}"),
            ))
        }
    };
    let serialized_bytes = bincode::serialized_size(&transaction)
        .expect("measure serialized compact batch v0 transaction size")
        as usize;
    let attempt = BatchAttempt {
        swap_count,
        serialized_bytes,
        account_count,
        static_account_count,
        lookup_account_count,
        signer_count: 1,
    };
    if serialized_bytes > SOLANA_PACKET_DATA_SIZE {
        return Err((
            attempt,
            format!("serialized transaction exceeds {SOLANA_PACKET_DATA_SIZE} byte packet limit"),
        ));
    }

    Ok(CompactBatchTransaction {
        svm,
        transaction,
        attempt,
    })
}

struct BatchFixture {
    svm: litesvm::LiteSVM,
    users: Vec<Keypair>,
    instructions: Vec<Instruction>,
    lookup_addresses: Vec<Pubkey>,
}

struct CompactBatchFixture {
    svm: litesvm::LiteSVM,
    swap_count: usize,
    signer: Keypair,
    instruction: Instruction,
    lookup_addresses: Vec<Pubkey>,
}

struct CompactBatchTransaction {
    svm: litesvm::LiteSVM,
    transaction: VersionedTransaction,
    attempt: BatchAttempt,
}

#[derive(Clone, Copy)]
enum CompactBatchKeySource {
    Random,
    Deterministic,
}

fn setup_multi_user_jupiter_swap_batch(swap_count: usize) -> BatchFixture {
    let mut svm = new_litesvm();
    add_mock_jupiter_program(&mut svm).expect("load mock Jupiter program");

    let users: Vec<Keypair> = (0..swap_count).map(|_| Keypair::new()).collect();
    svm.airdrop(&users[0].pubkey(), LAMPORTS_PER_SOL)
        .expect("fund transaction payer");

    seed_spl_mint_if_missing(&mut svm, USDC_MINT, None, USDC_DECIMALS, 0);
    seed_spl_mint_if_missing(&mut svm, PYUSD_MINT, None, PYUSD_DECIMALS, 0);
    seed_mock_jupiter_stable_reserve_spl_accounts(
        &mut svm,
        &[
            MockJupiterStableReserveTokenAccount {
                mint: USDC_MINT,
                reserve: mock_jupiter_stable_reserve_token_account(USDC_MINT),
            },
            MockJupiterStableReserveTokenAccount {
                mint: PYUSD_MINT,
                reserve: mock_jupiter_stable_reserve_token_account(PYUSD_MINT),
            },
        ],
        SWAP_AMOUNT * swap_count as u64,
    );

    let mut instructions = Vec::with_capacity(swap_count);
    let mut lookup_addresses = vec![
        JUPITER_V6_PROGRAM_ID,
        USDC_MINT,
        PYUSD_MINT,
        spl_token::id(),
        mock_jupiter_stable_reserve_token_account(USDC_MINT),
        mock_jupiter_stable_reserve_token_account(PYUSD_MINT),
        derive_mock_jupiter_swap_authority(),
    ];
    for user in &users {
        let user_usdc = Keypair::new().pubkey();
        let user_pyusd = Keypair::new().pubkey();
        seed_spl_token_account(&mut svm, user_usdc, USDC_MINT, user.pubkey(), SWAP_AMOUNT);
        seed_spl_token_account(&mut svm, user_pyusd, PYUSD_MINT, user.pubkey(), 0);
        lookup_addresses.push(user_usdc);
        lookup_addresses.push(user_pyusd);
        instructions.push(direct_mock_jupiter_stable_swap_instruction(
            user.pubkey(),
            user_usdc,
            user_pyusd,
        ));
    }

    BatchFixture {
        svm,
        users,
        instructions,
        lookup_addresses,
    }
}

fn setup_single_signer_jupiter_swap_batch(swap_count: usize) -> BatchFixture {
    let mut svm = new_litesvm();
    add_mock_jupiter_program(&mut svm).expect("load mock Jupiter program");

    let batch_signer = Keypair::new();
    svm.airdrop(&batch_signer.pubkey(), LAMPORTS_PER_SOL)
        .expect("fund transaction payer");

    seed_spl_mint_if_missing(&mut svm, USDC_MINT, None, USDC_DECIMALS, 0);
    seed_spl_mint_if_missing(&mut svm, PYUSD_MINT, None, PYUSD_DECIMALS, 0);
    seed_mock_jupiter_stable_reserve_spl_accounts(
        &mut svm,
        &[
            MockJupiterStableReserveTokenAccount {
                mint: USDC_MINT,
                reserve: mock_jupiter_stable_reserve_token_account(USDC_MINT),
            },
            MockJupiterStableReserveTokenAccount {
                mint: PYUSD_MINT,
                reserve: mock_jupiter_stable_reserve_token_account(PYUSD_MINT),
            },
        ],
        SWAP_AMOUNT * swap_count as u64,
    );

    let mut instructions = Vec::with_capacity(swap_count);
    let mut lookup_addresses = vec![
        JUPITER_V6_PROGRAM_ID,
        USDC_MINT,
        PYUSD_MINT,
        spl_token::id(),
        mock_jupiter_stable_reserve_token_account(USDC_MINT),
        mock_jupiter_stable_reserve_token_account(PYUSD_MINT),
        derive_mock_jupiter_swap_authority(),
    ];
    for _ in 0..swap_count {
        let user_usdc = Keypair::new().pubkey();
        let user_pyusd = Keypair::new().pubkey();
        seed_spl_token_account(
            &mut svm,
            user_usdc,
            USDC_MINT,
            batch_signer.pubkey(),
            SWAP_AMOUNT,
        );
        seed_spl_token_account(&mut svm, user_pyusd, PYUSD_MINT, batch_signer.pubkey(), 0);
        lookup_addresses.push(user_usdc);
        lookup_addresses.push(user_pyusd);
        instructions.push(direct_mock_jupiter_stable_swap_instruction(
            batch_signer.pubkey(),
            user_usdc,
            user_pyusd,
        ));
    }

    BatchFixture {
        svm,
        users: vec![batch_signer],
        instructions,
        lookup_addresses,
    }
}

fn setup_compact_batch_jupiter_cpi_swaps(swap_count: usize) -> CompactBatchFixture {
    setup_compact_batch_jupiter_cpi_swaps_with_key_source(swap_count, CompactBatchKeySource::Random)
}

fn setup_compact_batch_jupiter_cpi_swaps_with_key_source(
    swap_count: usize,
    key_source: CompactBatchKeySource,
) -> CompactBatchFixture {
    let mut svm = new_litesvm();
    add_mock_jupiter_program(&mut svm).expect("load mock Jupiter program");
    add_mock_yield_protocols_program(&mut svm, JUPITER_BATCH_PROGRAM_ID)
        .expect("load mock Jupiter batch program");

    let batch_signer = match key_source {
        CompactBatchKeySource::Random => Keypair::new(),
        CompactBatchKeySource::Deterministic => deterministic_keypair(0x51),
    };
    svm.airdrop(&batch_signer.pubkey(), LAMPORTS_PER_SOL)
        .expect("fund transaction payer");

    seed_spl_mint_if_missing(&mut svm, USDC_MINT, None, USDC_DECIMALS, 0);
    seed_spl_mint_if_missing(&mut svm, PYUSD_MINT, None, PYUSD_DECIMALS, 0);
    seed_mock_jupiter_stable_reserve_spl_accounts(
        &mut svm,
        &[
            MockJupiterStableReserveTokenAccount {
                mint: USDC_MINT,
                reserve: mock_jupiter_stable_reserve_token_account(USDC_MINT),
            },
            MockJupiterStableReserveTokenAccount {
                mint: PYUSD_MINT,
                reserve: mock_jupiter_stable_reserve_token_account(PYUSD_MINT),
            },
        ],
        SWAP_AMOUNT * swap_count as u64,
    );

    let mut accounts = vec![
        AccountMeta::new_readonly(batch_signer.pubkey(), true),
        AccountMeta::new_readonly(JUPITER_V6_PROGRAM_ID, false),
        AccountMeta::new_readonly(spl_token::id(), false),
        AccountMeta::new_readonly(USDC_MINT, false),
        AccountMeta::new_readonly(PYUSD_MINT, false),
        AccountMeta::new(mock_jupiter_stable_reserve_token_account(USDC_MINT), false),
        AccountMeta::new(mock_jupiter_stable_reserve_token_account(PYUSD_MINT), false),
        AccountMeta::new_readonly(derive_mock_jupiter_swap_authority(), false),
    ];
    let mut lookup_addresses = vec![
        JUPITER_V6_PROGRAM_ID,
        spl_token::id(),
        USDC_MINT,
        PYUSD_MINT,
        mock_jupiter_stable_reserve_token_account(USDC_MINT),
        mock_jupiter_stable_reserve_token_account(PYUSD_MINT),
        derive_mock_jupiter_swap_authority(),
    ];
    let mut data = Vec::with_capacity(9 + swap_count * 17);
    data.extend_from_slice(&MOCK_JUPITER_BATCH_EXACT_IN);
    data.push(swap_count as u8);

    for index in 0..swap_count {
        let (user_usdc, user_pyusd) = match key_source {
            CompactBatchKeySource::Random => (Keypair::new().pubkey(), Keypair::new().pubkey()),
            CompactBatchKeySource::Deterministic => (
                deterministic_pubkey(0xa0, index),
                deterministic_pubkey(0xb0, index),
            ),
        };
        let direction = if index % 2 == 0 { 0 } else { 1 };
        let (usdc_amount, pyusd_amount) = if direction == 0 {
            (SWAP_AMOUNT, 0)
        } else {
            (0, SWAP_AMOUNT)
        };
        seed_spl_token_account(
            &mut svm,
            user_usdc,
            USDC_MINT,
            batch_signer.pubkey(),
            usdc_amount,
        );
        seed_spl_token_account(
            &mut svm,
            user_pyusd,
            PYUSD_MINT,
            batch_signer.pubkey(),
            pyusd_amount,
        );
        accounts.push(AccountMeta::new(user_usdc, false));
        accounts.push(AccountMeta::new(user_pyusd, false));
        lookup_addresses.push(user_usdc);
        lookup_addresses.push(user_pyusd);
        data.push(direction);
        data.extend_from_slice(&SWAP_AMOUNT.to_le_bytes());
        data.extend_from_slice(&SWAP_AMOUNT.to_le_bytes());
    }

    CompactBatchFixture {
        svm,
        swap_count,
        signer: batch_signer,
        instruction: Instruction {
            program_id: JUPITER_BATCH_PROGRAM_ID,
            accounts,
            data,
        },
        lookup_addresses,
    }
}

fn deterministic_keypair(seed_prefix: u8) -> Keypair {
    let mut secret = [0u8; 32];
    for (index, byte) in secret.iter_mut().enumerate() {
        *byte = seed_prefix.wrapping_add(index as u8);
    }
    Keypair::new_from_array(secret)
}

fn deterministic_pubkey(prefix: u8, index: usize) -> Pubkey {
    let mut bytes = [prefix; 32];
    bytes[30] = (index >> 8) as u8;
    bytes[31] = index as u8;
    Pubkey::new_from_array(bytes)
}

fn serialized_transaction_dump(transaction: &VersionedTransaction, swap_count: usize) -> String {
    let bytes = bincode::serialize(transaction).expect("serialize versioned transaction");
    let mut output = String::new();
    writeln!(output, "serialized_transaction_len={}", bytes.len()).unwrap();
    writeln!(output, "swap_count={swap_count}").unwrap();
    writeln!(output).unwrap();
    writeln!(output, "hex_dump:").unwrap();
    for (offset, chunk) in bytes.chunks(16).enumerate() {
        writeln!(
            output,
            "{:04x}: {}",
            offset * 16,
            chunk
                .iter()
                .map(|byte| format!("{byte:02x}"))
                .collect::<Vec<_>>()
                .join(" ")
        )
        .unwrap();
    }

    writeln!(output).unwrap();
    writeln!(output, "chunks:").unwrap();
    let mut cursor = 0;
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        1,
        "signature_count shortvec = 1",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        64,
        "signature[0] batch signer",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        1,
        "message version prefix: 0x80 = v0",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        3,
        "message header: required signatures, readonly signed, readonly unsigned",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        1,
        "static account key count shortvec = 3",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        32,
        "static account[0] batch signer / fee payer",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        32,
        "static account[1] Compute Budget program id",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        32,
        "static account[2] batch program id",
    );
    push_chunk(&mut output, &bytes, &mut cursor, 32, "recent blockhash");
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        1,
        "compiled instruction count shortvec = 2",
    );

    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        1,
        "ix[0] program_id_index = 1 (Compute Budget)",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        1,
        "ix[0] account index count shortvec = 0",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        1,
        "ix[0] data length shortvec = 5",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        5,
        "ix[0] data: SetComputeUnitLimit(1,400,000)",
    );

    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        1,
        "ix[1] program_id_index = 2 (batch program)",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        1,
        "ix[1] account index count shortvec = 48",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        48,
        "ix[1] account indexes: signer, CPI programs/mints/reserves/authority, user token accounts",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        2,
        "ix[1] data length shortvec = 349",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        8,
        "ix[1] data discriminator",
    );
    push_chunk(&mut output, &bytes, &mut cursor, 1, "ix[1] data swap count");
    for index in 0..swap_count {
        push_chunk(
            &mut output,
            &bytes,
            &mut cursor,
            1,
            &format!("swap[{index}] direction"),
        );
        push_chunk(
            &mut output,
            &bytes,
            &mut cursor,
            8,
            &format!("swap[{index}] input amount"),
        );
        push_chunk(
            &mut output,
            &bytes,
            &mut cursor,
            8,
            &format!("swap[{index}] output amount"),
        );
    }

    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        1,
        "address table lookup count shortvec = 1",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        32,
        "lookup[0] table account key",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        1,
        "lookup[0] writable index count shortvec = 42",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        42,
        "lookup[0] writable indexes: reserves plus 40 user token accounts",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        1,
        "lookup[0] readonly index count shortvec = 5",
    );
    push_chunk(
        &mut output,
        &bytes,
        &mut cursor,
        5,
        "lookup[0] readonly indexes: Jupiter, SPL Token, mints, authority",
    );
    assert_eq!(
        cursor,
        bytes.len(),
        "dump should consume every serialized byte"
    );

    output
}

fn push_chunk(output: &mut String, bytes: &[u8], cursor: &mut usize, len: usize, label: &str) {
    let start = *cursor;
    let end = start + len;
    writeln!(
        output,
        "{start:04x}..{:04x} len={len:03} | {label} | {}",
        end - 1,
        bytes[start..end]
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect::<Vec<_>>()
            .join(" ")
    )
    .unwrap();
    *cursor = end;
}

fn seed_address_lookup_table(
    svm: &mut litesvm::LiteSVM,
    lookup_table_key: Pubkey,
    addresses: Vec<Pubkey>,
) {
    let data = AddressLookupTable {
        meta: LookupTableMeta {
            last_extended_slot: 0,
            last_extended_slot_start_index: 0,
            ..LookupTableMeta::default()
        },
        addresses: Cow::Owned(addresses),
    }
    .serialize_for_tests()
    .expect("serialize lookup table account");

    svm.set_account(
        lookup_table_key,
        Account {
            lamports: LAMPORTS_PER_SOL,
            data,
            owner: address_lookup_table::program::id(),
            executable: false,
            rent_epoch: 0,
        },
    )
    .expect("seed lookup table account");
}

impl BatchAttempt {
    fn empty(swap_count: usize, signer_count: usize) -> Self {
        Self {
            swap_count,
            serialized_bytes: 0,
            account_count: 0,
            static_account_count: 0,
            lookup_account_count: 0,
            signer_count,
        }
    }
}

fn direct_mock_jupiter_stable_swap_instruction(
    user: Pubkey,
    user_input: Pubkey,
    user_output: Pubkey,
) -> Instruction {
    Instruction {
        program_id: JUPITER_V6_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new_readonly(user, true),
            AccountMeta::new(user_input, false),
            AccountMeta::new(user_output, false),
            AccountMeta::new_readonly(USDC_MINT, false),
            AccountMeta::new_readonly(PYUSD_MINT, false),
            AccountMeta::new_readonly(spl_token::id(), false),
            AccountMeta::new(mock_jupiter_stable_reserve_token_account(USDC_MINT), false),
            AccountMeta::new(mock_jupiter_stable_reserve_token_account(PYUSD_MINT), false),
            AccountMeta::new_readonly(derive_mock_jupiter_swap_authority(), false),
        ],
        data: mock_jupiter_stable_exact_in_swap_data(
            SWAP_AMOUNT,
            SWAP_AMOUNT,
            USDC_MINT,
            PYUSD_MINT,
        ),
    }
}

use borsh::BorshSerialize;
use litesvm::LiteSVM;
use solana_sdk::{
    account::Account,
    hash::hashv,
    instruction::{AccountMeta, Instruction},
    message::Message,
    pubkey,
    pubkey::Pubkey,
    signature::Keypair,
    signer::Signer,
    transaction::Transaction,
};
use solana_system_interface::instruction as system_instruction;
use std::{env, fs, path::PathBuf};

pub const SQUADS_SMART_ACCOUNT_PROGRAM_ID: Pubkey =
    pubkey!("SMRTzfY6DfH5ik3TKiyLFfXexV8uSG3d2UksSCYdunG");
pub const SQUADS_SEED_PREFIX: &[u8] = b"smart_account";
pub const SQUADS_SEED_SETTINGS: &[u8] = b"settings";
pub const SQUADS_SEED_SMART_ACCOUNT: &[u8] = b"smart_account";
pub const SQUADS_SEED_POLICY: &[u8] = b"policy";
pub const SQUADS_PROGRAM_CONFIG_SEED: &[u8] = b"program_config";
pub const SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR: [u8; 8] =
    [90, 81, 187, 81, 39, 70, 128, 78];
pub const SQUADS_CREATE_SMART_ACCOUNT_DISCRIMINATOR: [u8; 8] = [197, 102, 253, 231, 77, 84, 50, 17];
pub const SQUADS_PROGRAM_CONFIG_DISCRIMINATOR: [u8; 8] = [196, 210, 90, 231, 144, 149, 140, 63];
pub const SQUADS_FULL_PERMISSIONS_MASK: u8 = 7;
pub const SQUADS_SYNC_SIGNER_COUNT: u8 = 1;
pub const SQUADS_ONE_SIGNER_SETTINGS_SPACE: usize = 168;
pub const DEFAULT_WALLET_AIRDROP_LAMPORTS: u64 = 1_000_000_000;
pub const LAMPORTS_PER_SOL: u64 = 1_000_000_000;
pub const SOL_DECIMALS: u8 = 9;

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsSettingsAction {
    AddSigner {
        new_signer: SquadsSmartAccountSigner,
    },
    RemoveSigner {
        old_signer: Pubkey,
    },
    ChangeThreshold {
        new_threshold: u16,
    },
    SetTimeLock {
        new_time_lock: u32,
    },
    AddSpendingLimit {
        seed: Pubkey,
        account_index: u8,
        mint: Pubkey,
        amount: u64,
        period: LegacyPeriod,
        signers: Vec<Pubkey>,
        destinations: Vec<Pubkey>,
        expiration: i64,
    },
    RemoveSpendingLimit {
        spending_limit: Pubkey,
    },
    SetArchivalAuthority {
        new_archival_authority: Option<Pubkey>,
    },
    PolicyCreate {
        seed: u64,
        policy_creation_payload: SquadsPolicyCreationPayload,
        signers: Vec<SquadsSmartAccountSigner>,
        threshold: u16,
        time_lock: u32,
        start_timestamp: Option<i64>,
        expiration_args: Option<SquadsPolicyExpirationArgs>,
    },
    PolicyUpdate {
        policy: Pubkey,
        signers: Vec<SquadsSmartAccountSigner>,
        threshold: u16,
        time_lock: u32,
        policy_update_payload: SquadsPolicyCreationPayload,
        expiration_args: Option<SquadsPolicyExpirationArgs>,
    },
    PolicyRemove {
        policy: Pubkey,
    },
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum LegacyPeriod {
    OneTime,
    Day,
    Week,
    Month,
}

#[derive(BorshSerialize)]
struct SquadsSmartAccountSigner {
    key: Pubkey,
    permissions: SquadsPermissions,
}

#[derive(BorshSerialize)]
struct SquadsPermissions {
    mask: u8,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsPolicyExpirationArgs {
    Timestamp(i64),
    SettingsState,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsPolicyCreationPayload {
    InternalFundTransfer(Vec<u8>),
    SpendingLimit(SquadsSpendingLimitPolicyCreationPayload),
    SettingsChange(Vec<u8>),
    LegacyProgramInteraction(Vec<u8>),
    ProgramInteraction(Vec<u8>),
}

#[derive(BorshSerialize)]
struct SquadsSpendingLimitPolicyCreationPayload {
    mint: Pubkey,
    source_account_index: u8,
    time_constraints: SquadsTimeConstraints,
    quantity_constraints: SquadsQuantityConstraints,
    usage_state: Option<SquadsUsageState>,
    destinations: Vec<Pubkey>,
}

#[derive(BorshSerialize)]
struct SquadsTimeConstraints {
    start: i64,
    expiration: Option<i64>,
    period: SquadsPeriodV2,
    accumulate_unused: bool,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsPeriodV2 {
    OneTime,
    Daily,
    Weekly,
    Monthly,
    Custom(i64),
}

#[derive(BorshSerialize)]
struct SquadsQuantityConstraints {
    max_per_period: u64,
    max_per_use: u64,
    enforce_exact_quantity: bool,
}

#[derive(BorshSerialize)]
struct SquadsUsageState {
    remaining_in_period: u64,
    last_reset: i64,
}

#[derive(BorshSerialize)]
struct SquadsSyncSettingsTransactionArgs {
    num_signers: u8,
    actions: Vec<SquadsSettingsAction>,
    memo: Option<String>,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsSyncPayload {
    Transaction(Vec<u8>),
    Policy(SquadsPolicyPayload),
}

#[derive(BorshSerialize)]
struct SquadsSyncTransactionArgs {
    account_index: u8,
    num_signers: u8,
    payload: SquadsSyncPayload,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsPolicyPayload {
    InternalFundTransfer(Vec<u8>),
    ProgramInteraction(Vec<u8>),
    SpendingLimit(SquadsSpendingLimitPayload),
    SettingsChange(Vec<u8>),
}

#[derive(BorshSerialize)]
struct SquadsSpendingLimitPayload {
    amount: u64,
    destination: Pubkey,
    decimals: u8,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SquadsPool {
    pub seed: u128,
    pub settings: Pubkey,
    pub settings_bump: u8,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct FundedSquadsTestConfig {
    pub smart_account_seed: u128,
    pub vault_index: u8,
    pub wallet_airdrop_lamports: u64,
    pub vault_funding_lamports: u64,
}

impl Default for FundedSquadsTestConfig {
    fn default() -> Self {
        Self {
            smart_account_seed: 1,
            vault_index: 0,
            wallet_airdrop_lamports: DEFAULT_WALLET_AIRDROP_LAMPORTS,
            vault_funding_lamports: DEFAULT_WALLET_AIRDROP_LAMPORTS / 2,
        }
    }
}

pub struct FundedSquadsTestContext {
    pub svm: LiteSVM,
    pub wallet: Keypair,
    pub pool: SquadsPool,
    pub vault_index: u8,
    pub vault: Pubkey,
    pub wallet_airdrop_lamports: u64,
    pub vault_funding_lamports: u64,
    pub loaded_program_path: PathBuf,
}

impl FundedSquadsTestContext {
    pub fn wallet_pubkey(&self) -> Pubkey {
        self.wallet.pubkey()
    }

    pub fn wallet_balance(&self) -> u64 {
        self.svm
            .get_account(&self.wallet.pubkey())
            .map(|account| account.lamports)
            .unwrap_or(0)
    }

    pub fn vault_balance(&self) -> u64 {
        self.svm
            .get_account(&self.vault)
            .map(|account| account.lamports)
            .unwrap_or(0)
    }

    pub fn sync_transfer_from_vault_to_wallet(&mut self, lamports: u64) {
        let instruction = execute_squads_sync_transfer_instruction(
            self.pool.settings,
            self.wallet.pubkey(),
            self.vault_index,
            self.wallet.pubkey(),
            lamports,
        );
        send_instructions(&mut self.svm, &[instruction], &self.wallet);
    }
}

pub fn new_litesvm() -> LiteSVM {
    LiteSVM::new()
}

pub fn derive_squads_settings(seed: u128) -> (Pubkey, u8) {
    Pubkey::find_program_address(
        &[
            SQUADS_SEED_PREFIX,
            SQUADS_SEED_SETTINGS,
            &seed.to_le_bytes(),
        ],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
}

pub fn derive_squads_pool(seed: u128) -> SquadsPool {
    let (settings, settings_bump) = derive_squads_settings(seed);
    SquadsPool {
        seed,
        settings,
        settings_bump,
    }
}

pub fn derive_squads_vault(squads_settings: &Pubkey, vault_index: u8) -> (Pubkey, u8) {
    Pubkey::find_program_address(
        &[
            SQUADS_SEED_PREFIX,
            squads_settings.as_ref(),
            SQUADS_SEED_SMART_ACCOUNT,
            &[vault_index],
        ],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
}

pub fn derive_squads_policy(squads_settings: &Pubkey, policy_seed: u64) -> (Pubkey, u8) {
    Pubkey::find_program_address(
        &[
            SQUADS_SEED_PREFIX,
            SQUADS_SEED_POLICY,
            squads_settings.as_ref(),
            &policy_seed.to_le_bytes(),
        ],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
}

pub fn derive_squads_program_config() -> Pubkey {
    Pubkey::find_program_address(
        &[SQUADS_SEED_PREFIX, SQUADS_PROGRAM_CONFIG_SEED],
        &SQUADS_SMART_ACCOUNT_PROGRAM_ID,
    )
    .0
}

pub fn anchor_instruction_discriminator(name: &str) -> [u8; 8] {
    let preimage = format!("global:{name}");
    let hash = hashv(&[preimage.as_bytes()]).to_bytes();
    hash[..8].try_into().unwrap()
}

pub fn squads_test_treasury() -> Pubkey {
    Pubkey::new_from_array(hash32(b"loyal-yield-routing-squads-treasury"))
}

pub fn serialize_squads_program_config(
    authority: Pubkey,
    treasury: Pubkey,
    smart_account_index: u128,
) -> Vec<u8> {
    let mut data = Vec::with_capacity(160);
    data.extend_from_slice(&SQUADS_PROGRAM_CONFIG_DISCRIMINATOR);
    smart_account_index.serialize(&mut data).unwrap();
    authority.serialize(&mut data).unwrap();
    0u64.serialize(&mut data).unwrap();
    treasury.serialize(&mut data).unwrap();
    [0u8; 64].serialize(&mut data).unwrap();
    data
}

pub fn seed_squads_program_config(
    svm: &mut LiteSVM,
    authority: Pubkey,
    treasury: Pubkey,
    smart_account_index: u128,
) -> Pubkey {
    let program_config = derive_squads_program_config();
    let data = serialize_squads_program_config(authority, treasury, smart_account_index);

    svm.set_account(
        program_config,
        Account {
            lamports: 1_000_000_000,
            data,
            owner: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
            executable: false,
            rent_epoch: 0,
        },
    )
    .expect("seed Squads program config account");

    program_config
}

pub fn serialize_squads_create_smart_account_args(verifier: Pubkey) -> Vec<u8> {
    let mut data = Vec::from(SQUADS_CREATE_SMART_ACCOUNT_DISCRIMINATOR);
    Option::<Pubkey>::None.serialize(&mut data).unwrap();
    1u16.serialize(&mut data).unwrap();
    1u32.serialize(&mut data).unwrap();
    verifier.serialize(&mut data).unwrap();
    SQUADS_FULL_PERMISSIONS_MASK.serialize(&mut data).unwrap();
    0u32.serialize(&mut data).unwrap();
    Option::<Pubkey>::None.serialize(&mut data).unwrap();
    Option::<String>::None.serialize(&mut data).unwrap();
    data
}

pub fn create_squads_smart_account_instruction(
    payer: Pubkey,
    verifier: Pubkey,
    seed: u128,
) -> Instruction {
    assert!(seed > 0, "Squads smart-account seed starts at 1");
    let program_config = derive_squads_program_config();
    let treasury = squads_test_treasury();
    let (settings, _) = derive_squads_settings(seed);

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(program_config, false),
            AccountMeta::new(treasury, false),
            AccountMeta::new(payer, true),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new(settings, false),
        ],
        data: serialize_squads_create_smart_account_args(verifier),
    }
}

pub fn squads_system_transfer_payload(lamports: u64) -> Vec<u8> {
    let mut transfer_data =
        system_instruction::transfer(&Pubkey::default(), &Pubkey::default(), lamports).data;
    let mut payload = Vec::with_capacity(7 + transfer_data.len());
    payload.push(1);
    payload.push(2);
    payload.push(2);
    payload.push(0);
    payload.push(1);
    payload.extend_from_slice(&(transfer_data.len() as u16).to_le_bytes());
    payload.append(&mut transfer_data);
    payload
}

pub fn serialize_squads_sync_transaction_args(account_index: u8, payload: Vec<u8>) -> Vec<u8> {
    let mut data = Vec::from(SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR);
    account_index.serialize(&mut data).unwrap();
    SQUADS_SYNC_SIGNER_COUNT.serialize(&mut data).unwrap();
    0u8.serialize(&mut data).unwrap();
    payload.serialize(&mut data).unwrap();
    data
}

fn serialize_squads_sync_settings_transaction_args(actions: Vec<SquadsSettingsAction>) -> Vec<u8> {
    let mut data = Vec::from(anchor_instruction_discriminator(
        "execute_settings_transaction_sync",
    ));
    SquadsSyncSettingsTransactionArgs {
        num_signers: SQUADS_SYNC_SIGNER_COUNT,
        actions,
        memo: None,
    }
    .serialize(&mut data)
    .unwrap();
    data
}

fn serialize_squads_sync_policy_payload_args(
    account_index: u8,
    policy_payload: SquadsPolicyPayload,
) -> Vec<u8> {
    let mut data = Vec::from(SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR);
    SquadsSyncTransactionArgs {
        account_index,
        num_signers: SQUADS_SYNC_SIGNER_COUNT,
        payload: SquadsSyncPayload::Policy(policy_payload),
    }
    .serialize(&mut data)
    .unwrap();
    data
}

pub fn execute_squads_sync_transfer_instruction(
    squads_settings: Pubkey,
    signer: Pubkey,
    account_index: u8,
    recipient: Pubkey,
    lamports: u64,
) -> Instruction {
    let (vault, _) = derive_squads_vault(&squads_settings, account_index);

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(squads_settings, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(signer, true),
            AccountMeta::new(vault, false),
            AccountMeta::new(recipient, false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
        ],
        data: serialize_squads_sync_transaction_args(
            account_index,
            squads_system_transfer_payload(lamports),
        ),
    }
}

pub fn create_squads_spending_limit_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    source_account_index: u8,
    destination: Pubkey,
    max_per_period_lamports: u64,
    max_per_use_lamports: u64,
) -> Instruction {
    let (policy, _) = derive_squads_policy(&squads_settings, policy_seed);
    let action = SquadsSettingsAction::PolicyCreate {
        seed: policy_seed,
        policy_creation_payload: SquadsPolicyCreationPayload::SpendingLimit(
            SquadsSpendingLimitPolicyCreationPayload {
                mint: Pubkey::default(),
                source_account_index,
                time_constraints: SquadsTimeConstraints {
                    start: 0,
                    expiration: None,
                    period: SquadsPeriodV2::OneTime,
                    accumulate_unused: false,
                },
                quantity_constraints: SquadsQuantityConstraints {
                    max_per_period: max_per_period_lamports,
                    max_per_use: max_per_use_lamports,
                    enforce_exact_quantity: false,
                },
                usage_state: None,
                destinations: vec![destination],
            },
        ),
        signers: vec![SquadsSmartAccountSigner {
            key: delegated_signer,
            permissions: SquadsPermissions {
                mask: SQUADS_FULL_PERMISSIONS_MASK,
            },
        }],
        threshold: 1,
        time_lock: 0,
        start_timestamp: None,
        expiration_args: None,
    };

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(squads_settings, false),
            AccountMeta::new(authority, true),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(authority, true),
            AccountMeta::new(policy, false),
        ],
        data: serialize_squads_sync_settings_transaction_args(vec![action]),
    }
}

pub fn remove_squads_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    policy: Pubkey,
) -> Instruction {
    let action = SquadsSettingsAction::PolicyRemove { policy };

    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(squads_settings, false),
            AccountMeta::new(authority, true),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(authority, true),
            AccountMeta::new(policy, false),
        ],
        data: serialize_squads_sync_settings_transaction_args(vec![action]),
    }
}

pub fn execute_squads_spending_limit_withdrawal_instruction(
    policy: Pubkey,
    signer: Pubkey,
    squads_settings: Pubkey,
    source_account_index: u8,
    destination: Pubkey,
    lamports: u64,
) -> Instruction {
    let (vault, _) = derive_squads_vault(&squads_settings, source_account_index);
    Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts: vec![
            AccountMeta::new(policy, false),
            AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
            AccountMeta::new_readonly(signer, true),
            AccountMeta::new(vault, false),
            AccountMeta::new(destination, false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
        ],
        data: serialize_squads_sync_policy_payload_args(
            source_account_index,
            SquadsPolicyPayload::SpendingLimit(SquadsSpendingLimitPayload {
                amount: lamports,
                destination,
                decimals: SOL_DECIMALS,
            }),
        ),
    }
}

pub fn hash_squads_account_metas(accounts: &[AccountMeta]) -> [u8; 32] {
    let mut bytes = Vec::with_capacity(accounts.len() * 34);
    for account in accounts {
        bytes.extend_from_slice(account.pubkey.as_ref());
        bytes.push(u8::from(account.is_writable));
        bytes.push(u8::from(account.is_signer));
    }

    hashv(&[&bytes]).to_bytes()
}

pub fn add_squads_program_from_env(svm: &mut LiteSVM) -> std::io::Result<Option<PathBuf>> {
    let Some(path) = env::var_os("SQUADS_SMART_ACCOUNT_PROGRAM_SO").map(PathBuf::from) else {
        return Ok(None);
    };
    let program = fs::read(&path)?;
    svm.add_program(SQUADS_SMART_ACCOUNT_PROGRAM_ID, &program)
        .map_err(|error| std::io::Error::other(format!("add Squads program failed: {error}")))?;
    Ok(Some(path))
}

pub fn add_squads_program_from_env_or_sibling_checkout(
    svm: &mut LiteSVM,
) -> std::io::Result<Option<PathBuf>> {
    if let Some(path) = add_squads_program_from_env(svm)? {
        return Ok(Some(path));
    }

    let sibling_path =
        PathBuf::from("../passkey-work/target/deploy/squads_smart_account_program.so");
    if !sibling_path.exists() {
        return Ok(None);
    }

    let program = fs::read(&sibling_path)?;
    svm.add_program(SQUADS_SMART_ACCOUNT_PROGRAM_ID, &program)
        .map_err(|error| std::io::Error::other(format!("add Squads program failed: {error}")))?;
    Ok(Some(sibling_path))
}

pub fn create_funded_squads_test_context() -> std::io::Result<Option<FundedSquadsTestContext>> {
    create_funded_squads_test_context_with_config(FundedSquadsTestConfig::default())
}

pub fn create_funded_squads_test_context_with_config(
    config: FundedSquadsTestConfig,
) -> std::io::Result<Option<FundedSquadsTestContext>> {
    assert!(
        config.vault_funding_lamports < config.wallet_airdrop_lamports,
        "vault funding should leave the wallet funded for later operations"
    );

    let mut svm = new_litesvm();
    let Some(loaded_program_path) = add_squads_program_from_env_or_sibling_checkout(&mut svm)?
    else {
        return Ok(None);
    };

    let wallet = Keypair::new();
    svm.airdrop(&wallet.pubkey(), config.wallet_airdrop_lamports)
        .expect("airdrop test wallet");
    seed_squads_program_config(&mut svm, wallet.pubkey(), squads_test_treasury(), 0);

    let pool = derive_squads_pool(config.smart_account_seed);
    let create_smart_account_ix = create_squads_smart_account_instruction(
        wallet.pubkey(),
        wallet.pubkey(),
        config.smart_account_seed,
    );
    send_instructions(&mut svm, &[create_smart_account_ix], &wallet);

    let settings_account = svm
        .get_account(&pool.settings)
        .expect("Squads settings account created");
    assert_eq!(settings_account.owner, SQUADS_SMART_ACCOUNT_PROGRAM_ID);

    let (vault, _) = derive_squads_vault(&pool.settings, config.vault_index);
    let fund_vault_ix =
        system_instruction::transfer(&wallet.pubkey(), &vault, config.vault_funding_lamports);
    send_instructions(&mut svm, &[fund_vault_ix], &wallet);

    let context = FundedSquadsTestContext {
        svm,
        wallet,
        pool,
        vault_index: config.vault_index,
        vault,
        wallet_airdrop_lamports: config.wallet_airdrop_lamports,
        vault_funding_lamports: config.vault_funding_lamports,
        loaded_program_path,
    };
    assert_eq!(context.vault_balance(), config.vault_funding_lamports);
    assert!(context.wallet_balance() > 0);

    Ok(Some(context))
}

pub fn send_instructions(svm: &mut LiteSVM, instructions: &[Instruction], payer: &Keypair) {
    try_send_instructions(svm, instructions, payer, &[]).unwrap();
}

pub fn try_send_instructions(
    svm: &mut LiteSVM,
    instructions: &[Instruction],
    payer: &Keypair,
    additional_signers: &[&Keypair],
) -> Result<(), String> {
    let message =
        Message::new_with_blockhash(instructions, Some(&payer.pubkey()), &svm.latest_blockhash());
    let mut signers = Vec::with_capacity(additional_signers.len() + 1);
    signers.push(payer);
    signers.extend_from_slice(additional_signers);
    let transaction = Transaction::new(&signers, message, svm.latest_blockhash());
    svm.send_transaction(transaction)
        .map(|_| ())
        .map_err(|error| format!("{error:?}"))
}

pub fn hash32(value: &[u8]) -> [u8; 32] {
    hashv(&[value]).to_bytes()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn litesvm_can_fund_a_test_payer() {
        let mut svm = new_litesvm();
        let payer = Keypair::new();

        svm.airdrop(&payer.pubkey(), 1_000_000_000).unwrap();

        let account = svm.get_account(&payer.pubkey()).unwrap();
        assert_eq!(account.lamports, 1_000_000_000);
    }

    #[test]
    fn derives_squads_settings_and_vault_namespaces() {
        let pool = derive_squads_pool(1);
        let (settings_again, _) = derive_squads_settings(1);
        let (vault_0, _) = derive_squads_vault(&pool.settings, 0);
        let (vault_255, _) = derive_squads_vault(&pool.settings, u8::MAX);

        assert_eq!(pool.settings, settings_again);
        assert_ne!(vault_0, vault_255);
    }

    #[test]
    fn seeds_squads_program_config_in_litesvm() {
        let mut svm = new_litesvm();
        let authority = Pubkey::new_unique();
        let treasury = squads_test_treasury();

        let program_config = seed_squads_program_config(&mut svm, authority, treasury, 0);

        let account = svm.get_account(&program_config).unwrap();
        assert_eq!(account.owner, SQUADS_SMART_ACCOUNT_PROGRAM_ID);
        assert_eq!(&account.data[..8], &SQUADS_PROGRAM_CONFIG_DISCRIMINATOR);
    }

    #[test]
    fn builds_squads_create_smart_account_instruction() {
        let payer = Pubkey::new_unique();
        let verifier = Pubkey::new_unique();
        let instruction = create_squads_smart_account_instruction(payer, verifier, 1);
        let (settings, _) = derive_squads_settings(1);

        assert_eq!(instruction.program_id, SQUADS_SMART_ACCOUNT_PROGRAM_ID);
        assert_eq!(instruction.accounts[2], AccountMeta::new(payer, true));
        assert_eq!(instruction.accounts[5], AccountMeta::new(settings, false));
        assert_eq!(
            &instruction.data[..8],
            &SQUADS_CREATE_SMART_ACCOUNT_DISCRIMINATOR
        );
    }

    #[test]
    fn hashes_squads_payload_and_accounts_for_authorization() {
        let settings = derive_squads_pool(1).settings;
        let (vault, _) = derive_squads_vault(&settings, 0);
        let recipient = Pubkey::new_unique();
        let accounts = vec![
            AccountMeta::new(vault, false),
            AccountMeta::new(recipient, false),
            AccountMeta::new_readonly(solana_sdk::system_program::ID, false),
        ];

        let payload = squads_system_transfer_payload(500_000);
        let accounts_hash = hash_squads_account_metas(&accounts);

        assert!(!payload.is_empty());
        assert_ne!(accounts_hash, [0; 32]);
    }
}

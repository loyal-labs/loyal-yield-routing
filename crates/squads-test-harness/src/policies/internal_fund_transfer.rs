use crate::types::*;
use crate::*;
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};

/// Squads derives policy PDAs from monotonically assigned seeds starting at 1.
pub const FIRST_SQUADS_POLICY_SEED: u64 = 1;
pub const DEFAULT_INTERNAL_FUND_TRANSFER_SOURCE_INDICES: [u8; 2] = [0, 1];
pub const DEFAULT_INTERNAL_FUND_TRANSFER_DESTINATION_INDICES: [u8; 2] = [0, 1];

pub fn supported_internal_fund_transfer_stable_mints() -> Vec<Pubkey> {
    loyal_actions::supported_yield_route_stable_mints()
}

pub fn create_first_squads_internal_fund_transfer_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
) -> (Pubkey, Instruction) {
    let policy_seed = FIRST_SQUADS_POLICY_SEED;
    let (policy, _) = derive_squads_policy(&squads_settings, policy_seed);
    let instruction = create_squads_internal_fund_transfer_policy_instruction(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
    );
    (policy, instruction)
}

pub fn create_squads_internal_fund_transfer_policy_instruction(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
) -> Instruction {
    create_squads_internal_fund_transfer_policy_instruction_with_mints(
        squads_settings,
        authority,
        delegated_signer,
        policy_seed,
        supported_internal_fund_transfer_stable_mints(),
    )
}

pub fn create_squads_internal_fund_transfer_policy_instruction_with_mints(
    squads_settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    policy_seed: u64,
    allowed_mints: Vec<Pubkey>,
) -> Instruction {
    let (policy, _) = derive_squads_policy(&squads_settings, policy_seed);
    let action = SquadsSettingsAction::PolicyCreate {
        seed: policy_seed,
        policy_creation_payload: SquadsPolicyCreationPayload::InternalFundTransfer(
            SquadsInternalFundTransferPolicyCreationPayload {
                source_account_indices: DEFAULT_INTERNAL_FUND_TRANSFER_SOURCE_INDICES.to_vec(),
                destination_account_indices: DEFAULT_INTERNAL_FUND_TRANSFER_DESTINATION_INDICES
                    .to_vec(),
                allowed_mints,
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

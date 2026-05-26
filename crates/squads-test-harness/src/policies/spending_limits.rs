use crate::types::*;
use crate::*;
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};

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

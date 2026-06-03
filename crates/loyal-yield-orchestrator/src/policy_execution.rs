use borsh::BorshSerialize;
use loyal_actions::{SameMintRoute, SQUADS_SMART_ACCOUNT_PROGRAM_ID};
use solana_sdk::{
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
};

use crate::OrchestratorError;

const SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR: [u8; 8] =
    [90, 81, 187, 81, 39, 70, 128, 78];
const SQUADS_SYNC_SIGNER_COUNT: u8 = 1;

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SquadsCompiledInstruction {
    pub program_id_index: usize,
    pub accounts: Vec<usize>,
    pub data: Vec<u8>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CompiledInstructionSet {
    pub instructions: Vec<SquadsCompiledInstruction>,
    pub accounts: Vec<AccountMeta>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum RouteInstructionPart {
    Compiled(CompiledInstructionSet),
    Instruction(Instruction),
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct SameMintPolicyRoute {
    pub action_account: Pubkey,
    pub instruction_constraint_indexes: [u8; 2],
}

impl From<SameMintRoute> for SameMintPolicyRoute {
    fn from(route: SameMintRoute) -> Self {
        Self {
            action_account: route.action_account(),
            instruction_constraint_indexes: *route.instruction_constraint_indexes(),
        }
    }
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsSyncPayload {
    Transaction(Vec<u8>),
    Policy(SquadsPolicyPayload),
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsPolicyPayload {
    InternalFundTransfer(()),
    ProgramInteraction(SquadsProgramInteractionPayload),
}

#[derive(BorshSerialize)]
struct SquadsSyncTransactionArgs {
    account_index: u8,
    num_signers: u8,
    payload: SquadsSyncPayload,
}

#[derive(BorshSerialize)]
struct SquadsProgramInteractionPayload {
    instruction_constraint_indices: Option<Vec<u8>>,
    transaction_payload: SquadsProgramInteractionTransactionPayload,
}

#[derive(BorshSerialize)]
#[allow(dead_code)]
enum SquadsProgramInteractionTransactionPayload {
    AsyncTransaction(Vec<u8>),
    SyncTransaction(SquadsProgramInteractionSyncPayload),
}

#[derive(BorshSerialize)]
struct SquadsProgramInteractionSyncPayload {
    account_index: u8,
    instructions: Vec<u8>,
}

pub fn compile_inner_instruction(instruction: Instruction) -> CompiledInstructionSet {
    let mut accounts = Vec::with_capacity(instruction.accounts.len() + 1);
    let compiled = compile_inner_instruction_into(&mut accounts, instruction);
    CompiledInstructionSet {
        instructions: vec![compiled],
        accounts,
    }
}

pub fn execute_same_mint_policy_route(
    route: SameMintRoute,
    signer: Pubkey,
    vault_index: u8,
    withdraw: CompiledInstructionSet,
    deposit: CompiledInstructionSet,
) -> Result<Instruction, OrchestratorError> {
    execute_same_mint_policy_route_from_policy(route.into(), signer, vault_index, withdraw, deposit)
}

pub fn execute_same_mint_policy_route_from_policy(
    route: SameMintPolicyRoute,
    signer: Pubkey,
    vault_index: u8,
    withdraw: CompiledInstructionSet,
    deposit: CompiledInstructionSet,
) -> Result<Instruction, OrchestratorError> {
    execute_loyal_action_route(
        route.action_account,
        signer,
        vault_index,
        route.instruction_constraint_indexes.to_vec(),
        vec![
            RouteInstructionPart::Compiled(withdraw),
            RouteInstructionPart::Compiled(deposit),
        ],
    )
}

pub fn execute_loyal_action_route(
    action_account: Pubkey,
    signer: Pubkey,
    vault_index: u8,
    instruction_constraint_indexes: Vec<u8>,
    parts: Vec<RouteInstructionPart>,
) -> Result<Instruction, OrchestratorError> {
    let mut transaction_accounts = Vec::new();
    let mut compiled_instructions = Vec::new();

    for part in parts {
        match part {
            RouteInstructionPart::Compiled(compiled) => {
                compiled_instructions.extend(merge_compiled_instructions(
                    &mut transaction_accounts,
                    compiled.instructions,
                    compiled.accounts,
                )?);
            }
            RouteInstructionPart::Instruction(instruction) => {
                compiled_instructions.push(compile_inner_instruction_into(
                    &mut transaction_accounts,
                    instruction,
                ));
            }
        }
    }

    execute_squads_program_interaction_instruction(
        action_account,
        signer,
        vault_index,
        compiled_instructions,
        instruction_constraint_indexes,
        transaction_accounts,
    )
}

pub fn execute_squads_program_interaction_instruction(
    policy: Pubkey,
    signer: Pubkey,
    account_index: u8,
    compiled_instructions: Vec<SquadsCompiledInstruction>,
    instruction_constraint_indices: Vec<u8>,
    mut transaction_accounts: Vec<AccountMeta>,
) -> Result<Instruction, OrchestratorError> {
    let mut accounts = vec![
        AccountMeta::new(policy, false),
        AccountMeta::new_readonly(SQUADS_SMART_ACCOUNT_PROGRAM_ID, false),
        AccountMeta::new_readonly(signer, true),
    ];
    accounts.append(&mut transaction_accounts);

    Ok(Instruction {
        program_id: SQUADS_SMART_ACCOUNT_PROGRAM_ID,
        accounts,
        data: serialize_squads_sync_policy_payload_args(
            account_index,
            instruction_constraint_indices,
            squads_compiled_instruction_payload(&compiled_instructions)?,
        )?,
    })
}

pub fn squads_compiled_instruction_payload(
    instructions: &[SquadsCompiledInstruction],
) -> Result<Vec<u8>, OrchestratorError> {
    let mut payload = Vec::new();
    payload.push(to_u8(instructions.len(), "instruction count")?);

    for instruction in instructions {
        payload.push(to_u8(instruction.program_id_index, "program id index")?);
        payload.push(to_u8(instruction.accounts.len(), "account index count")?);
        for account in &instruction.accounts {
            payload.push(to_u8(*account, "account index")?);
        }
        payload.extend_from_slice(
            &to_u16(instruction.data.len(), "instruction data length")?.to_le_bytes(),
        );
        payload.extend_from_slice(&instruction.data);
    }

    Ok(payload)
}

fn serialize_squads_sync_policy_payload_args(
    account_index: u8,
    instruction_constraint_indices: Vec<u8>,
    instructions: Vec<u8>,
) -> Result<Vec<u8>, OrchestratorError> {
    let mut data = Vec::from(SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR);
    SquadsSyncTransactionArgs {
        account_index,
        num_signers: SQUADS_SYNC_SIGNER_COUNT,
        payload: SquadsSyncPayload::Policy(SquadsPolicyPayload::ProgramInteraction(
            SquadsProgramInteractionPayload {
                instruction_constraint_indices: Some(instruction_constraint_indices),
                transaction_payload: SquadsProgramInteractionTransactionPayload::SyncTransaction(
                    SquadsProgramInteractionSyncPayload {
                        account_index,
                        instructions,
                    },
                ),
            },
        )),
    }
    .serialize(&mut data)
    .map_err(|error| OrchestratorError::Execution(error.to_string()))?;
    Ok(data)
}

fn merge_compiled_instructions(
    transaction_accounts: &mut Vec<AccountMeta>,
    compiled_instructions: Vec<SquadsCompiledInstruction>,
    source_accounts: Vec<AccountMeta>,
) -> Result<Vec<SquadsCompiledInstruction>, OrchestratorError> {
    compiled_instructions
        .into_iter()
        .map(|instruction| {
            let program_id_index = remap_account_index(
                transaction_accounts,
                &source_accounts,
                instruction.program_id_index,
            )?;
            let accounts = instruction
                .accounts
                .into_iter()
                .map(|index| remap_account_index(transaction_accounts, &source_accounts, index))
                .collect::<Result<Vec<_>, _>>()?;
            Ok(SquadsCompiledInstruction {
                program_id_index,
                accounts,
                data: instruction.data,
            })
        })
        .collect()
}

fn compile_inner_instruction_into(
    transaction_accounts: &mut Vec<AccountMeta>,
    instruction: Instruction,
) -> SquadsCompiledInstruction {
    let accounts = instruction
        .accounts
        .into_iter()
        .map(|account| push_or_update_account_meta(transaction_accounts, account))
        .collect();
    let program_id_index = push_or_update_account_meta(
        transaction_accounts,
        AccountMeta::new_readonly(instruction.program_id, false),
    );

    SquadsCompiledInstruction {
        program_id_index,
        accounts,
        data: instruction.data,
    }
}

fn remap_account_index(
    transaction_accounts: &mut Vec<AccountMeta>,
    source_accounts: &[AccountMeta],
    index: usize,
) -> Result<usize, OrchestratorError> {
    let account = source_accounts.get(index).ok_or_else(|| {
        OrchestratorError::Execution(format!(
            "compiled instruction account index {index} is out of bounds"
        ))
    })?;
    Ok(push_or_update_account_meta(
        transaction_accounts,
        account.clone(),
    ))
}

fn push_or_update_account_meta(accounts: &mut Vec<AccountMeta>, meta: AccountMeta) -> usize {
    if let Some(index) = accounts
        .iter()
        .position(|existing| existing.pubkey == meta.pubkey)
    {
        accounts[index].is_writable |= meta.is_writable;
        accounts[index].is_signer |= meta.is_signer;
        return index;
    }

    let index = accounts.len();
    accounts.push(meta);
    index
}

fn to_u8(value: usize, label: &'static str) -> Result<u8, OrchestratorError> {
    value
        .try_into()
        .map_err(|_| OrchestratorError::Execution(format!("{label} exceeds u8")))
}

fn to_u16(value: usize, label: &'static str) -> Result<u16, OrchestratorError> {
    value
        .try_into()
        .map_err(|_| OrchestratorError::Execution(format!("{label} exceeds u16")))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn compiled_instruction_payload_serializes_indices_and_data() {
        let payload = squads_compiled_instruction_payload(&[SquadsCompiledInstruction {
            program_id_index: 2,
            accounts: vec![0, 1],
            data: vec![9, 8, 7],
        }])
        .unwrap();

        assert_eq!(payload, vec![1, 2, 2, 0, 1, 3, 0, 9, 8, 7]);
    }

    #[test]
    fn compile_inner_instruction_deduplicates_program_account() {
        let program = Pubkey::new_unique();
        let compiled = compile_inner_instruction(Instruction {
            program_id: program,
            accounts: vec![
                AccountMeta::new(program, false),
                AccountMeta::new_readonly(Pubkey::new_unique(), false),
            ],
            data: vec![1],
        });

        assert_eq!(compiled.accounts.len(), 2);
        assert_eq!(compiled.instructions[0].program_id_index, 0);
    }

    #[test]
    fn program_interaction_instruction_uses_squads_program_and_signer() {
        let signer = Pubkey::new_unique();
        let policy = Pubkey::new_unique();
        let program = Pubkey::new_unique();
        let ix = execute_squads_program_interaction_instruction(
            policy,
            signer,
            0,
            vec![SquadsCompiledInstruction {
                program_id_index: 0,
                accounts: vec![],
                data: vec![],
            }],
            vec![0],
            vec![AccountMeta::new_readonly(program, false)],
        )
        .unwrap();

        assert_eq!(ix.program_id, SQUADS_SMART_ACCOUNT_PROGRAM_ID);
        assert_eq!(ix.accounts[0].pubkey, policy);
        assert_eq!(ix.accounts[2].pubkey, signer);
        assert!(ix.accounts[2].is_signer);
        assert_eq!(
            &ix.data[..8],
            &SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR
        );
    }

    #[test]
    fn same_mint_policy_route_keeps_action_and_constraint_indexes() {
        let route = SameMintPolicyRoute {
            action_account: Pubkey::new_unique(),
            instruction_constraint_indexes: [0, 2],
        };
        let signer = Pubkey::new_unique();
        let withdraw_program = Pubkey::new_unique();
        let deposit_program = Pubkey::new_unique();
        let withdraw = compile_inner_instruction(Instruction {
            program_id: withdraw_program,
            accounts: vec![AccountMeta::new(Pubkey::new_unique(), false)],
            data: vec![1],
        });
        let deposit = compile_inner_instruction(Instruction {
            program_id: deposit_program,
            accounts: vec![AccountMeta::new(Pubkey::new_unique(), false)],
            data: vec![2],
        });

        let ix = execute_same_mint_policy_route_from_policy(route, signer, 0, withdraw, deposit)
            .unwrap();

        assert_eq!(ix.accounts[0].pubkey, route.action_account);
        assert_eq!(ix.accounts[2].pubkey, signer);
        assert!(ix.data.len() > SQUADS_EXECUTE_TRANSACTION_SYNC_V2_DISCRIMINATOR.len());
    }
}

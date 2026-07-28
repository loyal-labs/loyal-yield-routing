use crate::squads::{
    create_program_interaction_action_instruction, SquadsAccountConstraint,
    SquadsAccountConstraintType, SquadsDataConstraint, SquadsDataOperator, SquadsDataValue,
    SquadsInstructionConstraint,
};
use crate::{
    derive_action_account, derive_kamino_user_metadata, derive_kamino_vanilla_obligation,
    KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR, KAMINO_INIT_OBLIGATION_DISCRIMINATOR,
    KAMINO_LEND_PROGRAM_ID, KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR, USDC_MINT,
};
use solana_sdk::{instruction::Instruction, pubkey::Pubkey, sysvar};
use std::fmt;

const KAMINO_V2_ACCOUNT_COUNT: usize = 17;

#[derive(Clone, Debug)]
pub struct KaminoReservePolicyTemplate {
    pub market: Pubkey,
    pub reserve: Pubkey,
    pub vault_usdc: Pubkey,
    pub deposit_instruction: Instruction,
    pub withdraw_instruction: Instruction,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct KaminoPolicyConstraintIndexes {
    pub market: Pubkey,
    pub reserve: Pubkey,
    pub deposit: u8,
    pub withdraw: u8,
}

#[derive(Clone, Debug)]
pub struct KaminoPolicyPlan {
    pub policy: Pubkey,
    pub policy_seed: u64,
    pub create_instruction: Instruction,
    pub constraint_indexes: Vec<KaminoPolicyConstraintIndexes>,
}

#[derive(Clone, Debug)]
pub struct AutonomousKaminoPolicies {
    pub operations: KaminoPolicyPlan,
    pub init_obligation: KaminoPolicyPlan,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum AutonomousVaultError {
    ExpectedTwoKaminoReserves {
        actual: usize,
    },
    DuplicateKaminoMarket,
    DuplicateKaminoReserve,
    DuplicatePolicySeeds,
    InvalidKaminoInstruction {
        operation: &'static str,
        field: &'static str,
    },
    Squads(crate::LoyalActionError),
}

impl fmt::Display for AutonomousVaultError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::ExpectedTwoKaminoReserves { actual } => {
                write!(
                    formatter,
                    "expected exactly two Kamino reserves, got {actual}"
                )
            }
            Self::DuplicateKaminoMarket => formatter.write_str("Kamino markets must be unique"),
            Self::DuplicateKaminoReserve => formatter.write_str("Kamino reserves must be unique"),
            Self::DuplicatePolicySeeds => {
                formatter.write_str("Kamino policy seeds must be distinct")
            }
            Self::InvalidKaminoInstruction { operation, field } => {
                write!(formatter, "invalid Kamino {operation} {field}")
            }
            Self::Squads(error) => write!(formatter, "Squads policy encoding failed: {error}"),
        }
    }
}

impl std::error::Error for AutonomousVaultError {}

impl From<crate::LoyalActionError> for AutonomousVaultError {
    fn from(error: crate::LoyalActionError) -> Self {
        Self::Squads(error)
    }
}

pub fn create_kamino_policies(
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    vault: Pubkey,
    vault_index: u8,
    operations_seed: u64,
    init_obligation_seed: u64,
    templates: Vec<KaminoReservePolicyTemplate>,
) -> Result<AutonomousKaminoPolicies, AutonomousVaultError> {
    validate_templates(vault, &templates)?;
    if operations_seed == init_obligation_seed {
        return Err(AutonomousVaultError::DuplicatePolicySeeds);
    }

    let operation_constraints = vec![
        protected_instruction_constraint(&templates, false),
        protected_instruction_constraint(&templates, true),
    ];
    let init_constraints = vec![init_obligation_constraint(
        vault,
        templates.iter().map(|template| template.market).collect(),
    )];

    Ok(AutonomousKaminoPolicies {
        operations: policy_plan(
            settings,
            authority,
            delegated_signer,
            vault_index,
            operations_seed,
            operation_constraints,
            &templates,
            false,
        )?,
        init_obligation: policy_plan(
            settings,
            authority,
            delegated_signer,
            vault_index,
            init_obligation_seed,
            init_constraints,
            &templates,
            true,
        )?,
    })
}

fn policy_plan(
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    vault_index: u8,
    policy_seed: u64,
    constraints: Vec<SquadsInstructionConstraint>,
    templates: &[KaminoReservePolicyTemplate],
    init_only: bool,
) -> Result<KaminoPolicyPlan, AutonomousVaultError> {
    let create_instruction = create_program_interaction_action_instruction(
        settings,
        authority,
        delegated_signer,
        policy_seed,
        vault_index,
        constraints,
    )?;
    Ok(KaminoPolicyPlan {
        policy: derive_action_account(&settings, policy_seed).0,
        policy_seed,
        create_instruction,
        constraint_indexes: templates
            .iter()
            .map(|template| KaminoPolicyConstraintIndexes {
                market: template.market,
                reserve: template.reserve,
                deposit: 0,
                withdraw: if init_only { 0 } else { 1 },
            })
            .collect(),
    })
}

fn validate_templates(
    vault: Pubkey,
    templates: &[KaminoReservePolicyTemplate],
) -> Result<(), AutonomousVaultError> {
    if templates.len() != 2 {
        return Err(AutonomousVaultError::ExpectedTwoKaminoReserves {
            actual: templates.len(),
        });
    }
    if templates[0].market == templates[1].market {
        return Err(AutonomousVaultError::DuplicateKaminoMarket);
    }
    if templates[0].reserve == templates[1].reserve {
        return Err(AutonomousVaultError::DuplicateKaminoReserve);
    }
    for template in templates {
        let obligation = derive_kamino_vanilla_obligation(vault, template.market);
        validate_shared_reserve_graph(template)?;
        validate_protected_instruction(
            "deposit",
            &template.deposit_instruction,
            KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
            &[
                (0, vault, "owner"),
                (1, obligation, "obligation"),
                (2, template.market, "market"),
                (4, template.reserve, "reserve"),
                (5, USDC_MINT, "liquidity mint"),
                (9, template.vault_usdc, "vault USDC"),
                (11, spl_token::id(), "token program"),
                (12, spl_token::id(), "liquidity token program"),
            ],
        )?;
        validate_protected_instruction(
            "withdraw",
            &template.withdraw_instruction,
            KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
            &[
                (0, vault, "owner"),
                (1, obligation, "obligation"),
                (2, template.market, "market"),
                (4, template.reserve, "reserve"),
                (5, USDC_MINT, "liquidity mint"),
                (9, template.vault_usdc, "vault USDC"),
                (11, spl_token::id(), "token program"),
                (12, spl_token::id(), "liquidity token program"),
            ],
        )?;
    }
    Ok(())
}

fn validate_shared_reserve_graph(
    template: &KaminoReservePolicyTemplate,
) -> Result<(), AutonomousVaultError> {
    for (deposit_index, withdraw_index, field) in [
        (3, 3, "lending market authority"),
        (6, 8, "reserve liquidity supply"),
        (7, 7, "reserve collateral mint"),
        (8, 6, "reserve collateral supply"),
        (10, 10, "placeholder collateral destination"),
        (13, 13, "instructions sysvar"),
        (14, 14, "obligation farm state"),
        (15, 15, "reserve farm state"),
        (16, 16, "farms program"),
    ] {
        if template
            .deposit_instruction
            .accounts
            .get(deposit_index)
            .map(|account| account.pubkey)
            != template
                .withdraw_instruction
                .accounts
                .get(withdraw_index)
                .map(|account| account.pubkey)
        {
            return Err(invalid("reserve pair", field));
        }
    }
    Ok(())
}

fn validate_protected_instruction(
    operation: &'static str,
    instruction: &Instruction,
    discriminator: [u8; 8],
    expected_accounts: &[(usize, Pubkey, &'static str)],
) -> Result<(), AutonomousVaultError> {
    if instruction.program_id != KAMINO_LEND_PROGRAM_ID {
        return Err(invalid(operation, "program"));
    }
    if instruction.accounts.len() != KAMINO_V2_ACCOUNT_COUNT {
        return Err(invalid(operation, "account count"));
    }
    if !instruction.data.starts_with(&discriminator) {
        return Err(invalid(operation, "discriminator"));
    }
    for (index, expected, field) in expected_accounts {
        if instruction
            .accounts
            .get(*index)
            .map(|account| account.pubkey)
            != Some(*expected)
        {
            return Err(invalid(operation, field));
        }
    }
    Ok(())
}

fn invalid(operation: &'static str, field: &'static str) -> AutonomousVaultError {
    AutonomousVaultError::InvalidKaminoInstruction { operation, field }
}

fn protected_instruction_constraint(
    templates: &[KaminoReservePolicyTemplate],
    withdraw: bool,
) -> SquadsInstructionConstraint {
    // K-Lend validates the reserve-owned vaults, collateral mint, market authority,
    // optional farm state, sysvar, and Farms program against the pinned reserve and
    // market. Encoding those program-enforced accounts again makes the policy-create
    // transaction exceed Solana's packet limit. These are the user-controlled
    // security edges: vault authority, allowed obligations, markets/reserves,
    // liquidity mint, and vault source/destination. K-Lend binds each obligation
    // and reserve to its market, validates reserve-owned supply accounts, requires
    // the classic collateral-token program, and binds the liquidity-token program
    // to the pinned mint owner. One allowlisted deposit and one allowlisted
    // withdrawal constraint therefore preserve exact pair safety without
    // exceeding the packet limit.
    const PROTECTED_ACCOUNT_INDEXES: [usize; 6] = [0, 1, 2, 4, 5, 9];
    let instructions = templates
        .iter()
        .map(|template| {
            if withdraw {
                &template.withdraw_instruction
            } else {
                &template.deposit_instruction
            }
        })
        .collect::<Vec<_>>();
    let instruction = instructions[0];
    SquadsInstructionConstraint {
        program_id: instruction.program_id,
        account_constraints: PROTECTED_ACCOUNT_INDEXES
            .iter()
            .map(|index| {
                pubkey_allowlist_constraint(
                    *index as u8,
                    instructions
                        .iter()
                        .map(|instruction| instruction.accounts[*index].pubkey)
                        .collect(),
                )
            })
            .collect(),
        data_constraints: vec![data_slice_equals(0, instruction.data[..8].to_vec())],
    }
}

fn init_obligation_constraint(vault: Pubkey, markets: Vec<Pubkey>) -> SquadsInstructionConstraint {
    let mut data_prefix = KAMINO_INIT_OBLIGATION_DISCRIMINATOR.to_vec();
    data_prefix.extend_from_slice(&[0, 0]);
    SquadsInstructionConstraint {
        program_id: KAMINO_LEND_PROGRAM_ID,
        account_constraints: vec![
            pubkey_constraint(0, vault),
            pubkey_constraint(1, vault),
            pubkey_allowlist_constraint(
                2,
                markets
                    .iter()
                    .map(|market| derive_kamino_vanilla_obligation(vault, *market))
                    .collect(),
            ),
            pubkey_allowlist_constraint(3, markets),
            pubkey_constraint(4, Pubkey::default()),
            pubkey_constraint(5, Pubkey::default()),
            pubkey_constraint(6, derive_kamino_user_metadata(vault)),
            pubkey_constraint(7, sysvar::rent::id()),
            pubkey_constraint(8, solana_sdk::system_program::ID),
        ],
        data_constraints: vec![data_slice_equals(0, data_prefix)],
    }
}

fn pubkey_constraint(account_index: u8, pubkey: Pubkey) -> SquadsAccountConstraint {
    pubkey_allowlist_constraint(account_index, vec![pubkey])
}

fn pubkey_allowlist_constraint(
    account_index: u8,
    mut pubkeys: Vec<Pubkey>,
) -> SquadsAccountConstraint {
    pubkeys.sort();
    pubkeys.dedup();
    SquadsAccountConstraint {
        account_index,
        account_constraint: SquadsAccountConstraintType::Pubkey(pubkeys),
        owner: None,
    }
}

fn data_slice_equals(data_offset: u64, value: Vec<u8>) -> SquadsDataConstraint {
    SquadsDataConstraint {
        data_offset,
        data_value: SquadsDataValue::U8Slice(value),
        operator: SquadsDataOperator::Equals,
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use solana_sdk::instruction::AccountMeta;

    fn template(vault: Pubkey, market: Pubkey, reserve: Pubkey) -> KaminoReservePolicyTemplate {
        let obligation = derive_kamino_vanilla_obligation(vault, market);
        let vault_usdc = Pubkey::new_unique();
        let deposit_instruction = protected_instruction(
            vault,
            obligation,
            market,
            reserve,
            vault_usdc,
            KAMINO_DEPOSIT_RESERVE_LIQUIDITY_DISCRIMINATOR,
        );
        let mut withdraw_instruction = protected_instruction(
            vault,
            obligation,
            market,
            reserve,
            vault_usdc,
            KAMINO_WITHDRAW_RESERVE_LIQUIDITY_DISCRIMINATOR,
        );
        align_shared_reserve_graph(&deposit_instruction, &mut withdraw_instruction);
        KaminoReservePolicyTemplate {
            market,
            reserve,
            vault_usdc,
            deposit_instruction,
            withdraw_instruction,
        }
    }

    fn align_shared_reserve_graph(deposit: &Instruction, withdraw: &mut Instruction) {
        for (deposit_index, withdraw_index) in [
            (3, 3),
            (6, 8),
            (7, 7),
            (8, 6),
            (10, 10),
            (13, 13),
            (14, 14),
            (15, 15),
            (16, 16),
        ] {
            withdraw.accounts[withdraw_index].pubkey = deposit.accounts[deposit_index].pubkey;
        }
    }

    fn protected_instruction(
        vault: Pubkey,
        obligation: Pubkey,
        market: Pubkey,
        reserve: Pubkey,
        vault_usdc: Pubkey,
        discriminator: [u8; 8],
    ) -> Instruction {
        let mut accounts = (0..KAMINO_V2_ACCOUNT_COUNT)
            .map(|_| AccountMeta::new(Pubkey::new_unique(), false))
            .collect::<Vec<_>>();
        for (index, pubkey) in [
            (0, vault),
            (1, obligation),
            (2, market),
            (4, reserve),
            (5, USDC_MINT),
            (9, vault_usdc),
            (11, spl_token::id()),
            (12, spl_token::id()),
        ] {
            accounts[index].pubkey = pubkey;
        }
        let mut data = discriminator.to_vec();
        data.extend_from_slice(&1u64.to_le_bytes());
        Instruction {
            program_id: KAMINO_LEND_PROGRAM_ID,
            accounts,
            data,
        }
    }

    fn templates(vault: Pubkey) -> Vec<KaminoReservePolicyTemplate> {
        vec![
            template(vault, Pubkey::new_unique(), Pubkey::new_unique()),
            template(vault, Pubkey::new_unique(), Pubkey::new_unique()),
        ]
    }

    #[test]
    fn creates_split_deployed_kamino_policy_plans() {
        let settings = Pubkey::new_unique();
        let vault = Pubkey::new_unique();
        let templates = templates(vault);
        let policies = create_kamino_policies(
            settings,
            Pubkey::new_unique(),
            Pubkey::new_unique(),
            vault,
            0,
            1,
            2,
            templates.clone(),
        )
        .expect("build policies");

        assert_eq!(
            policies.operations.policy,
            derive_action_account(&settings, 1).0
        );
        assert_eq!(
            policies.init_obligation.policy,
            derive_action_account(&settings, 2).0
        );
        assert_eq!(policies.operations.create_instruction.data[22], 3);
        assert_eq!(policies.init_obligation.create_instruction.data[22], 3);
        assert_eq!(
            policies.operations.constraint_indexes,
            vec![
                KaminoPolicyConstraintIndexes {
                    market: templates[0].market,
                    reserve: templates[0].reserve,
                    deposit: 0,
                    withdraw: 1,
                },
                KaminoPolicyConstraintIndexes {
                    market: templates[1].market,
                    reserve: templates[1].reserve,
                    deposit: 0,
                    withdraw: 1,
                },
            ]
        );
    }

    #[test]
    fn rejects_mutated_protected_account_graphs() {
        let vault = Pubkey::new_unique();
        for (index, expected_field) in [
            (0, "owner"),
            (1, "obligation"),
            (2, "market"),
            (4, "reserve"),
            (5, "liquidity mint"),
            (9, "vault USDC"),
            (11, "token program"),
            (12, "liquidity token program"),
        ] {
            let mut templates = templates(vault);
            templates[0].deposit_instruction.accounts[index].pubkey = Pubkey::new_unique();
            assert_eq!(
                validate_templates(vault, &templates),
                Err(invalid("deposit", expected_field))
            );
        }
    }

    #[test]
    fn rejects_wrong_program_discriminator_and_pair_count() {
        let vault = Pubkey::new_unique();
        let mut wrong_program = templates(vault);
        wrong_program[0].withdraw_instruction.program_id = Pubkey::new_unique();
        assert_eq!(
            validate_templates(vault, &wrong_program),
            Err(invalid("withdraw", "program"))
        );

        let mut wrong_discriminator = templates(vault);
        wrong_discriminator[0].withdraw_instruction.data[0] ^= 1;
        assert_eq!(
            validate_templates(vault, &wrong_discriminator),
            Err(invalid("withdraw", "discriminator"))
        );

        assert_eq!(
            validate_templates(vault, &templates(vault)[..1]),
            Err(AutonomousVaultError::ExpectedTwoKaminoReserves { actual: 1 })
        );
    }

    #[test]
    fn allowlists_only_the_two_approved_markets_reserves_and_obligations() {
        let vault = Pubkey::new_unique();
        let templates = templates(vault);
        let deposit = protected_instruction_constraint(&templates, false);

        assert_pubkey_allowlist(
            account_constraint(&deposit, 1),
            templates
                .iter()
                .map(|template| derive_kamino_vanilla_obligation(vault, template.market))
                .collect(),
        );
        assert_pubkey_allowlist(
            account_constraint(&deposit, 2),
            templates.iter().map(|template| template.market).collect(),
        );
        assert_pubkey_allowlist(
            account_constraint(&deposit, 4),
            templates.iter().map(|template| template.reserve).collect(),
        );
    }

    fn account_constraint(
        constraint: &SquadsInstructionConstraint,
        account_index: u8,
    ) -> &SquadsAccountConstraint {
        constraint
            .account_constraints
            .iter()
            .find(|constraint| constraint.account_index == account_index)
            .expect("account constraint")
    }

    fn assert_pubkey_allowlist(constraint: &SquadsAccountConstraint, mut expected: Vec<Pubkey>) {
        expected.sort();
        expected.dedup();
        match &constraint.account_constraint {
            SquadsAccountConstraintType::Pubkey(pubkeys) => {
                assert_eq!(pubkeys, &expected);
            }
            SquadsAccountConstraintType::AccountData(_) => {
                panic!("expected a pubkey account constraint")
            }
        }
    }
}

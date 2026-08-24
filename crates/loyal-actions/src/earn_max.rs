use crate::{
    jupiter::JupiterBuildError, LoyalActionError, Result,
    SemanticProgramInteractionConstraint as Constraint,
    SemanticProgramInteractionDataConstraint as DataConstraint,
};
use solana_sdk::{instruction::Instruction, pubkey::Pubkey};

pub const EARN_MAX_SHARED_ACCOUNTS_ROUTE: [u8; 8] =
    [0xc1, 0x20, 0x9b, 0x33, 0x41, 0xd6, 0x9c, 0x81];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum EarnMaxPolicyFamily {
    Collateral,
    Debt,
    Swap,
}

#[derive(Clone, Copy, Debug)]
pub struct EarnMaxPolicyLane {
    pub obligation: Pubkey,
    pub collateral_reserve: Pubkey,
    pub collateral_custody: Pubkey,
    pub debt_reserve: Pubkey,
    pub debt_custody: Pubkey,
    pub debt_token_program: Pubkey,
}

#[derive(Clone, Debug)]
pub struct EarnMaxPolicyBoundary {
    pub vault: Pubkey,
    pub klend_program: Pubkey,
    pub farms_program: Pubkey,
    pub jupiter_program: Pubkey,
    pub classic_token_program: Pubkey,
    pub deposit_discriminator: [u8; 8],
    pub withdraw_discriminator: [u8; 8],
    pub borrow_discriminator: [u8; 8],
    pub repay_discriminator: [u8; 8],
    pub lanes: Vec<EarnMaxPolicyLane>,
}

#[derive(Clone, Copy, Debug)]
pub struct EarnMaxJupiterRouteExpectation {
    pub jupiter_program: Pubkey,
    pub vault: Pubkey,
    pub source: Pubkey,
    pub destination: Pubkey,
    pub input_mint: Pubkey,
    pub output_mint: Pubkey,
    pub input_amount: u64,
    pub quoted_output_amount: u64,
    pub minimum_output_amount: u64,
    pub slippage_bps: u16,
}

pub fn validate_earn_max_jupiter_route(
    instruction: &Instruction,
    expected: EarnMaxJupiterRouteExpectation,
) -> std::result::Result<(), JupiterBuildError> {
    if instruction.program_id != expected.jupiter_program {
        return Err(JupiterBuildError::UnexpectedProgram("swapInstruction"));
    }
    let expected_accounts = [
        (2, expected.vault, true, false),
        (3, expected.source, false, true),
        (6, expected.destination, false, true),
        (7, expected.input_mint, false, false),
        (8, expected.output_mint, false, false),
    ];
    for (index, pubkey, signer, writable) in expected_accounts {
        let account = instruction
            .accounts
            .get(index)
            .ok_or(JupiterBuildError::InvalidSwapAccounts)?;
        if account.pubkey != pubkey
            || account.is_signer != signer
            || account.is_writable != writable
        {
            return Err(JupiterBuildError::InvalidSwapAccounts);
        }
    }
    if instruction
        .accounts
        .iter()
        .filter(|account| account.is_signer)
        .count()
        != 1
    {
        return Err(JupiterBuildError::InvalidAuthority("swapInstruction"));
    }
    let data = instruction.data.as_slice();
    if data.get(..8) != Some(EARN_MAX_SHARED_ACCOUNTS_ROUTE.as_slice()) || data.len() < 32 {
        return Err(JupiterBuildError::UnsupportedJupiterDialect);
    }
    let route_count = read_u32(data, 9).ok_or(JupiterBuildError::InvalidRoutePlan)?;
    if !(1..=4).contains(&route_count) {
        return Err(JupiterBuildError::InvalidRoutePlan);
    }
    let tail = data.len() - 19;
    let input = read_u64(data, tail).ok_or(JupiterBuildError::InvalidSwapInstructionData)?;
    let output = read_u64(data, tail + 8).ok_or(JupiterBuildError::InvalidSwapInstructionData)?;
    let slippage =
        read_u16(data, tail + 16).ok_or(JupiterBuildError::InvalidSwapInstructionData)?;
    if input != expected.input_amount
        || output != expected.quoted_output_amount
        || slippage != expected.slippage_bps
    {
        return Err(JupiterBuildError::InvalidSwapInstructionData);
    }
    if data.last() != Some(&0) {
        return Err(JupiterBuildError::PlatformFeeNotZero);
    }
    let minimum = expected
        .quoted_output_amount
        .saturating_mul(u64::from(10_000 - expected.slippage_bps))
        .saturating_add(9_999)
        / 10_000;
    if expected.minimum_output_amount != minimum {
        return Err(JupiterBuildError::InvalidMinimumOutput);
    }
    Ok(())
}

pub fn earn_max_policy_constraints(
    boundary: &EarnMaxPolicyBoundary,
    family: EarnMaxPolicyFamily,
) -> Result<Vec<Constraint>> {
    if boundary.lanes.is_empty() {
        return Err(LoyalActionError::InvalidPolicyConstraint);
    }
    match family {
        EarnMaxPolicyFamily::Collateral => Ok(vec![
            collateral_constraint(boundary, boundary.deposit_discriminator),
            collateral_constraint(boundary, boundary.withdraw_discriminator),
        ]),
        EarnMaxPolicyFamily::Debt => Ok(vec![
            Constraint {
                program_id: boundary.klend_program,
                account_pubkeys: vec![
                    (0, vec![boundary.vault]),
                    (
                        1,
                        boundary.lanes.iter().map(|lane| lane.obligation).collect(),
                    ),
                    (
                        4,
                        unique(boundary.lanes.iter().map(|lane| lane.debt_reserve)),
                    ),
                    (
                        8,
                        unique(boundary.lanes.iter().map(|lane| lane.debt_custody)),
                    ),
                    (
                        10,
                        unique(boundary.lanes.iter().map(|lane| lane.debt_token_program)),
                    ),
                    (14, vec![boundary.farms_program]),
                ],
                data: vec![slice_equals(boundary.borrow_discriminator)],
            },
            Constraint {
                program_id: boundary.klend_program,
                account_pubkeys: vec![
                    (0, vec![boundary.vault]),
                    (
                        1,
                        boundary.lanes.iter().map(|lane| lane.obligation).collect(),
                    ),
                    (
                        3,
                        unique(boundary.lanes.iter().map(|lane| lane.debt_reserve)),
                    ),
                    (
                        6,
                        unique(boundary.lanes.iter().map(|lane| lane.debt_custody)),
                    ),
                    (
                        7,
                        unique(boundary.lanes.iter().map(|lane| lane.debt_token_program)),
                    ),
                    (12, vec![boundary.farms_program]),
                ],
                data: vec![slice_equals(boundary.repay_discriminator)],
            },
        ]),
        EarnMaxPolicyFamily::Swap => Ok(boundary
            .lanes
            .iter()
            .flat_map(|lane| {
                [
                    swap_constraint(boundary, lane.debt_custody, lane.collateral_custody),
                    swap_constraint(boundary, lane.collateral_custody, lane.debt_custody),
                ]
            })
            .collect()),
    }
}

fn collateral_constraint(boundary: &EarnMaxPolicyBoundary, discriminator: [u8; 8]) -> Constraint {
    Constraint {
        program_id: boundary.klend_program,
        account_pubkeys: vec![
            (0, vec![boundary.vault]),
            (
                1,
                boundary.lanes.iter().map(|lane| lane.obligation).collect(),
            ),
            (
                4,
                unique(boundary.lanes.iter().map(|lane| lane.collateral_reserve)),
            ),
            (
                9,
                unique(boundary.lanes.iter().map(|lane| lane.collateral_custody)),
            ),
            (11, vec![boundary.classic_token_program]),
            (12, vec![boundary.classic_token_program]),
            (14, vec![boundary.klend_program]),
            (15, vec![boundary.klend_program]),
            (16, vec![boundary.farms_program]),
        ],
        data: vec![slice_equals(discriminator)],
    }
}

fn swap_constraint(
    boundary: &EarnMaxPolicyBoundary,
    source: Pubkey,
    destination: Pubkey,
) -> Constraint {
    Constraint {
        program_id: boundary.jupiter_program,
        account_pubkeys: vec![
            (2, vec![boundary.vault]),
            (3, vec![source]),
            (6, vec![destination]),
        ],
        data: vec![slice_equals(EARN_MAX_SHARED_ACCOUNTS_ROUTE)],
    }
}

fn slice_equals(value: [u8; 8]) -> DataConstraint {
    DataConstraint::SliceEquals {
        offset: 0,
        value: value.to_vec(),
    }
}

fn unique(values: impl IntoIterator<Item = Pubkey>) -> Vec<Pubkey> {
    let mut unique = Vec::new();
    for value in values {
        if !unique.contains(&value) {
            unique.push(value);
        }
    }
    unique
}

fn read_u16(data: &[u8], offset: usize) -> Option<u16> {
    data.get(offset..offset + 2)
        .and_then(|bytes| <[u8; 2]>::try_from(bytes).ok())
        .map(u16::from_le_bytes)
}

fn read_u32(data: &[u8], offset: usize) -> Option<u32> {
    data.get(offset..offset + 4)
        .and_then(|bytes| <[u8; 4]>::try_from(bytes).ok())
        .map(u32::from_le_bytes)
}

fn read_u64(data: &[u8], offset: usize) -> Option<u64> {
    data.get(offset..offset + 8)
        .and_then(|bytes| <[u8; 8]>::try_from(bytes).ok())
        .map(u64::from_le_bytes)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        derive_action_account, derive_associated_token_account, derive_kamino_obligation,
        derive_squads_vault, update_semantic_program_interaction_policy_instruction,
    };
    use sha2::{Digest, Sha256};
    use solana_sdk::instruction::{AccountMeta, Instruction};
    use std::str::FromStr;

    fn key(value: &str) -> Pubkey {
        Pubkey::from_str(value).unwrap()
    }

    fn boundary(settings: Pubkey) -> EarnMaxPolicyBoundary {
        let vault = derive_squads_vault(&settings, 0).0;
        let token = spl_token::id();
        let token_2022 = spl_token_2022::id();
        let templates = [
            (
                "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8",
                "6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ",
                "5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5",
                "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z",
                "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
                token,
            ),
            (
                "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8",
                "6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ",
                "5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5",
                "3yDc9ARvtPLhYxZLgucZGuBtZ9bHshBvXTwHxGe3nhmC",
                "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA",
                token,
            ),
            (
                "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA",
                "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh",
                "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7",
                "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu",
                "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
                token,
            ),
            (
                "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA",
                "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh",
                "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7",
                "3ZUAwhEtK8XWfK4fy98z4yoptm4GeyeAu21L11HPXaZ5",
                "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo",
                token_2022,
            ),
            (
                "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA",
                "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh",
                "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7",
                "7SzMWArC8WAenndXFmRyfvcvrNPodqUFkmPrmmoRZvn4",
                "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA",
                token,
            ),
            (
                "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y",
                "AwCyCPZYJSZ93xcVKNK7jR8e1BHzJXq1D4bReNuh9woY",
                "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj",
                "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo",
                "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v",
                token,
            ),
            (
                "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y",
                "AwCyCPZYJSZ93xcVKNK7jR8e1BHzJXq1D4bReNuh9woY",
                "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj",
                "92qeAka3ZzCGPfJriDXrE7tiNqfATVCAM6ZjjctR3TrS",
                "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo",
                token_2022,
            ),
        ];
        let lanes = templates
            .map(
                |(
                    market,
                    collateral_reserve,
                    collateral_mint,
                    debt_reserve,
                    debt_mint,
                    debt_token_program,
                )| {
                    let market = key(market);
                    let collateral_mint = key(collateral_mint);
                    let debt_mint = key(debt_mint);
                    EarnMaxPolicyLane {
                        obligation: derive_kamino_obligation(
                            vault,
                            market,
                            1,
                            0,
                            collateral_mint,
                            debt_mint,
                        ),
                        collateral_reserve: key(collateral_reserve),
                        collateral_custody: derive_associated_token_account(
                            vault,
                            collateral_mint,
                            token,
                        ),
                        debt_reserve: key(debt_reserve),
                        debt_custody: derive_associated_token_account(
                            vault,
                            debt_mint,
                            debt_token_program,
                        ),
                        debt_token_program,
                    }
                },
            )
            .to_vec();
        EarnMaxPolicyBoundary {
            vault,
            klend_program: key("KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD"),
            farms_program: key("FarmsPZpWu9i7Kky8tPN37rs2TpmMrAZrC7S7vJa91Hr"),
            jupiter_program: key("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"),
            classic_token_program: token,
            deposit_discriminator: [216, 224, 191, 27, 204, 151, 102, 175],
            withdraw_discriminator: [235, 52, 119, 152, 149, 197, 20, 7],
            borrow_discriminator: [161, 128, 143, 245, 171, 199, 194, 6],
            repay_discriminator: [116, 174, 213, 76, 180, 53, 210, 144],
            lanes,
        }
    }

    fn instruction_fingerprint(instruction: &Instruction) -> String {
        let mut digest = Sha256::new();
        digest.update(instruction.program_id.as_ref());
        digest.update((instruction.accounts.len() as u32).to_le_bytes());
        for account in &instruction.accounts {
            digest.update(account.pubkey.as_ref());
            digest.update([account.is_signer as u8, account.is_writable as u8]);
        }
        digest.update((instruction.data.len() as u32).to_le_bytes());
        digest.update(&instruction.data);
        digest
            .finalize()
            .iter()
            .map(|byte| format!("{byte:02x}"))
            .collect()
    }

    #[test]
    fn three_policy_wire_contract_matches_the_typescript_sdk() {
        let settings = key("11111111111111111111111111111112");
        let authority = key("11111111111111111111111111111113");
        let delegate = key("11111111111111111111111111111114");
        let boundary = boundary(settings);
        let fingerprints = [
            (EarnMaxPolicyFamily::Collateral, 234),
            (EarnMaxPolicyFamily::Debt, 235),
            (EarnMaxPolicyFamily::Swap, 236),
        ]
        .map(|(family, seed)| {
            let policy = derive_action_account(&settings, seed).0;
            let instruction = update_semantic_program_interaction_policy_instruction(
                settings,
                authority,
                policy,
                delegate,
                0,
                earn_max_policy_constraints(&boundary, family).unwrap(),
            )
            .unwrap();
            instruction_fingerprint(&instruction)
        });
        assert_eq!(
            fingerprints,
            [
                "ef32ee403e4a472b19f927e14a224e318b8572073c7bd40260b6c4b1be45e224",
                "ad1b0ca8316a1e03644b27c0e6050dc58703326f9f095db2697e58de5df72f5c",
                "88320a43b00cafad090780a28ec23c773e5ef595621d4262ba08fc208ebbc2af",
            ]
        );
        assert_eq!(
            [
                EarnMaxPolicyFamily::Collateral,
                EarnMaxPolicyFamily::Debt,
                EarnMaxPolicyFamily::Swap
            ]
            .map(|family| earn_max_policy_constraints(&boundary, family)
                .unwrap()
                .len()),
            [2, 2, 14]
        );
    }

    #[test]
    fn jupiter_mutation_boundary_rejects_value_redirection_and_quote_drift() {
        let jupiter = key("JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4");
        let vault = Pubkey::new_unique();
        let source = Pubkey::new_unique();
        let destination = Pubkey::new_unique();
        let input_mint = Pubkey::new_unique();
        let output_mint = Pubkey::new_unique();
        let expected = EarnMaxJupiterRouteExpectation {
            jupiter_program: jupiter,
            vault,
            source,
            destination,
            input_mint,
            output_mint,
            input_amount: 1_000,
            quoted_output_amount: 2_001,
            minimum_output_amount: 1_991,
            slippage_bps: 50,
        };
        let mut accounts = (0..9)
            .map(|_| AccountMeta::new_readonly(Pubkey::new_unique(), false))
            .collect::<Vec<_>>();
        accounts[2] = AccountMeta::new_readonly(vault, true);
        accounts[3] = AccountMeta::new(source, false);
        accounts[6] = AccountMeta::new(destination, false);
        accounts[7] = AccountMeta::new_readonly(input_mint, false);
        accounts[8] = AccountMeta::new_readonly(output_mint, false);
        let mut data = Vec::from(EARN_MAX_SHARED_ACCOUNTS_ROUTE);
        data.push(0);
        data.extend(1_u32.to_le_bytes());
        data.extend(expected.input_amount.to_le_bytes());
        data.extend(expected.quoted_output_amount.to_le_bytes());
        data.extend(expected.slippage_bps.to_le_bytes());
        data.push(0);
        let instruction = Instruction {
            program_id: jupiter,
            accounts,
            data,
        };
        validate_earn_max_jupiter_route(&instruction, expected).unwrap();

        for index in [2, 3, 6, 7, 8] {
            let mut mutation = instruction.clone();
            mutation.accounts[index].pubkey = Pubkey::new_unique();
            assert!(validate_earn_max_jupiter_route(&mutation, expected).is_err());
        }
        let mut extra_signer = instruction.clone();
        extra_signer.accounts[0].is_signer = true;
        assert!(validate_earn_max_jupiter_route(&extra_signer, expected).is_err());

        for offset in [0, 9, 13, 21, 29, 31] {
            let mut mutation = instruction.clone();
            mutation.data[offset] ^= 1;
            assert!(validate_earn_max_jupiter_route(&mutation, expected).is_err());
        }
        let wrong_threshold = EarnMaxJupiterRouteExpectation {
            minimum_output_amount: 1_990,
            ..expected
        };
        assert!(validate_earn_max_jupiter_route(&instruction, wrong_threshold).is_err());
    }
}

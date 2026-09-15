//! The four-policy, owner-approved basic policy boundary for Backyard RWA.
//!
//! This module intentionally owns only the semantic boundary and its offline
//! PolicyCreate artifacts. It does not discover accounts, query RPC, sign, or
//! submit transactions.

use crate::backyard_policy_catalog::signed_policy_create_packet_bytes;
use crate::{
    create_deployed_semantic_program_interaction_policy_instruction, derive_action_account,
    derive_associated_token_account, derive_kamino_obligation, derive_squads_vault,
    earn_max_policy_constraints, swap_constraint, EarnMaxPolicyBoundary, EarnMaxPolicyFamily,
    EarnMaxPolicyLane, EARN_MAX_SHARED_ACCOUNTS_ROUTE,
    SemanticProgramInteractionConstraint as Constraint,
};
use serde::Serialize;
use sha2::{Digest, Sha256};
use solana_sdk::{
    hash::Hash,
    instruction::{AccountMeta, Instruction},
    pubkey::Pubkey,
    signature::{Keypair, Signer},
};
use std::{error::Error, str::FromStr};

const KLEND_PROGRAM: &str = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD";
const JUPITER_PROGRAM: &str = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4";
const TOKEN_PROGRAM: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const TOKEN_2022_PROGRAM: &str = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb";
const MULTIPLY_OBLIGATION_TAG: u8 = 1;
const MULTIPLY_OBLIGATION_ID: u8 = 0;

const DEPOSIT_COLLATERAL: [u8; 8] = [216, 224, 191, 27, 204, 151, 102, 175];
const WITHDRAW_COLLATERAL: [u8; 8] = [235, 52, 119, 152, 149, 197, 20, 7];
const BORROW_DEBT: [u8; 8] = [161, 128, 143, 245, 171, 199, 194, 6];
const REPAY_DEBT: [u8; 8] = [116, 174, 213, 76, 180, 53, 210, 144];

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum BasicPolicyFamily {
    CollateralLifecycle,
    DebtLifecycle,
    SwapRoutesA,
    SwapRoutesB,
}

impl BasicPolicyFamily {
    const ALL: [Self; 4] = [
        Self::CollateralLifecycle,
        Self::DebtLifecycle,
        Self::SwapRoutesA,
        Self::SwapRoutesB,
    ];

    const fn label(self) -> &'static str {
        match self {
            Self::CollateralLifecycle => "CollateralLifecycle",
            Self::DebtLifecycle => "DebtLifecycle",
            Self::SwapRoutesA => "SwapRoutesA",
            Self::SwapRoutesB => "SwapRoutesB",
        }
    }
}

/// A human-readable account predicate attached to one semantic constraint.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct PolicyAccountConditionSummary {
    pub account_index: u8,
    pub condition: String,
}

/// A Privy-style summary of one independently matchable policy rule.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct PolicyConstraintSummary {
    pub program: String,
    pub instruction: String,
    pub account_conditions: Vec<PolicyAccountConditionSummary>,
    pub data_conditions: Vec<String>,
}

/// One complete, unsigned legacy PolicyCreate plan.
#[derive(Clone, Debug)]
pub struct BackyardBasicPolicyPlan {
    pub family: &'static str,
    pub seed: u64,
    pub account: Pubkey,
    pub bump: u8,
    pub constraints: Vec<Constraint>,
    pub instruction: Instruction,
    pub signed_packet_bytes: usize,
    pub data_sha256: String,
    pub summary: Vec<PolicyConstraintSummary>,
}

/// Build the four consecutive basic policies for a known settings account.
///
/// The seed base is deliberately supplied by the caller: live seed-counter
/// verification belongs to the installer, while this function remains a pure
/// deterministic compiler boundary.
pub fn compile_backyard_basic_policy_set(
    settings: Pubkey,
    authority: Pubkey,
    delegate: Pubkey,
    seed_base: u64,
) -> std::result::Result<Vec<BackyardBasicPolicyPlan>, Box<dyn Error>> {
    let boundary = canonical_boundary(settings)?;
    let measurement_authority = Keypair::new();
    let mut plans = Vec::with_capacity(BasicPolicyFamily::ALL.len());

    for (offset, family) in BasicPolicyFamily::ALL.into_iter().enumerate() {
        let seed = seed_base
            .checked_add(offset as u64)
            .ok_or("basic policy seed overflow")?;
        let account = derive_action_account(&settings, seed);
        let constraints = match family {
            BasicPolicyFamily::CollateralLifecycle => {
                earn_max_policy_constraints(&boundary, EarnMaxPolicyFamily::Collateral)?
            }
            BasicPolicyFamily::DebtLifecycle => {
                earn_max_policy_constraints(&boundary, EarnMaxPolicyFamily::Debt)?
            }
            BasicPolicyFamily::SwapRoutesA => vec![
                swap_constraint(
                    &boundary,
                    vec![boundary.usdc_custody, boundary.usds_custody],
                    vec![boundary.onyc_custody, boundary.prime_custody],
                ),
                swap_constraint(
                    &boundary,
                    vec![boundary.usdc_custody, boundary.pyusd_custody],
                    vec![boundary.prime_custody, boundary.syrup_usdc_custody],
                ),
            ],
            BasicPolicyFamily::SwapRoutesB => vec![
                swap_constraint(
                    &boundary,
                    vec![boundary.onyc_custody, boundary.prime_custody],
                    vec![boundary.usdc_custody, boundary.usds_custody],
                ),
                swap_constraint(
                    &boundary,
                    vec![boundary.prime_custody, boundary.syrup_usdc_custody],
                    vec![boundary.usdc_custody, boundary.pyusd_custody],
                ),
            ],
        };
        let instruction = create_deployed_semantic_program_interaction_policy_instruction(
            settings,
            authority,
            delegate,
            seed,
            0,
            constraints.clone(),
        )?;
        let mut digest = Sha256::new();
        digest.update(&instruction.data);
        let data_sha256 = hex_digest(digest.finalize().as_slice());
        let measurement_instruction = Instruction {
            program_id: instruction.program_id,
            accounts: instruction
                .accounts
                .iter()
                .map(|account| {
                    if account.pubkey == authority {
                        AccountMeta {
                            pubkey: measurement_authority.pubkey(),
                            is_signer: account.is_signer,
                            is_writable: account.is_writable,
                        }
                    } else {
                        account.clone()
                    }
                })
                .collect(),
            data: instruction.data.clone(),
        };
        let signed_packet_bytes = signed_policy_create_packet_bytes(
            &measurement_instruction,
            &measurement_authority,
            Hash::default(),
        );
        let summary = summarize_constraints(family, &boundary, &constraints);
        plans.push(BackyardBasicPolicyPlan {
            family: family.label(),
            seed,
            account: account.0,
            bump: account.1,
            constraints,
            instruction,
            signed_packet_bytes,
            data_sha256,
            summary,
        });
    }

    Ok(plans)
}

fn canonical_boundary(
    settings: Pubkey,
) -> std::result::Result<EarnMaxPolicyBoundary, Box<dyn Error>> {
    let vault = derive_squads_vault(&settings, 0).0;
    let token = Pubkey::from_str(TOKEN_PROGRAM)?;
    let token_2022 = Pubkey::from_str(TOKEN_2022_PROGRAM)?;
    let templates = [
        (
            "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8",
            crate::EARN_MAX_ONYC_COLLATERAL_RESERVE,
            crate::EARN_MAX_ONYC_MINT,
            crate::EARN_MAX_ONYC_USDC_DEBT_RESERVE,
            crate::EARN_MAX_USDC_MINT,
            token,
        ),
        (
            "47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8",
            crate::EARN_MAX_ONYC_COLLATERAL_RESERVE,
            crate::EARN_MAX_ONYC_MINT,
            crate::EARN_MAX_ONYC_USDS_DEBT_RESERVE,
            crate::EARN_MAX_USDS_MINT,
            token,
        ),
        (
            "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA",
            crate::EARN_MAX_PRIME_COLLATERAL_RESERVE,
            crate::EARN_MAX_PRIME_MINT,
            crate::EARN_MAX_PRIME_USDC_DEBT_RESERVE,
            crate::EARN_MAX_USDC_MINT,
            token,
        ),
        (
            "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA",
            crate::EARN_MAX_PRIME_COLLATERAL_RESERVE,
            crate::EARN_MAX_PRIME_MINT,
            crate::EARN_MAX_PRIME_PYUSD_DEBT_RESERVE,
            crate::EARN_MAX_PYUSD_MINT,
            token_2022,
        ),
        (
            "CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA",
            crate::EARN_MAX_PRIME_COLLATERAL_RESERVE,
            crate::EARN_MAX_PRIME_MINT,
            crate::EARN_MAX_PRIME_USDS_DEBT_RESERVE,
            crate::EARN_MAX_USDS_MINT,
            token,
        ),
        (
            "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y",
            crate::EARN_MAX_SYRUP_COLLATERAL_RESERVE,
            crate::EARN_MAX_SYRUP_USDC_MINT,
            crate::EARN_MAX_SYRUP_USDC_DEBT_RESERVE,
            crate::EARN_MAX_USDC_MINT,
            token,
        ),
        (
            "6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y",
            crate::EARN_MAX_SYRUP_COLLATERAL_RESERVE,
            crate::EARN_MAX_SYRUP_USDC_MINT,
            crate::EARN_MAX_SYRUP_PYUSD_DEBT_RESERVE,
            crate::EARN_MAX_PYUSD_MINT,
            token_2022,
        ),
    ];
    let lanes = templates
        .into_iter()
        .map(
            |(
                market,
                collateral_reserve,
                collateral_mint,
                debt_reserve,
                debt_mint,
                debt_token_program,
            )| {
                let market = Pubkey::from_str(market)?;
                let collateral_reserve = Pubkey::from_str(collateral_reserve)?;
                let collateral_mint = Pubkey::from_str(collateral_mint)?;
                let debt_reserve = Pubkey::from_str(debt_reserve)?;
                let debt_mint = Pubkey::from_str(debt_mint)?;
                Ok(EarnMaxPolicyLane {
                    market,
                    obligation: derive_kamino_obligation(
                        vault,
                        market,
                        MULTIPLY_OBLIGATION_TAG,
                        MULTIPLY_OBLIGATION_ID,
                        collateral_mint,
                        debt_mint,
                    ),
                    collateral_reserve,
                    collateral_custody: derive_associated_token_account(
                        vault,
                        collateral_mint,
                        token,
                    ),
                    debt_reserve,
                    debt_custody: derive_associated_token_account(
                        vault,
                        debt_mint,
                        debt_token_program,
                    ),
                    debt_token_program,
                })
            },
        )
        .collect::<std::result::Result<Vec<_>, Box<dyn Error>>>()?;

    Ok(EarnMaxPolicyBoundary {
        vault,
        klend_program: Pubkey::from_str(KLEND_PROGRAM)?,
        jupiter_program: Pubkey::from_str(JUPITER_PROGRAM)?,
        usdc_custody: lanes[0].debt_custody,
        pyusd_custody: lanes[3].debt_custody,
        usds_custody: lanes[1].debt_custody,
        onyc_custody: lanes[0].collateral_custody,
        prime_custody: lanes[2].collateral_custody,
        syrup_usdc_custody: lanes[5].collateral_custody,
        deposit_discriminator: DEPOSIT_COLLATERAL,
        withdraw_discriminator: WITHDRAW_COLLATERAL,
        borrow_discriminator: BORROW_DEBT,
        repay_discriminator: REPAY_DEBT,
        lanes,
    })
}

fn summarize_constraints(
    family: BasicPolicyFamily,
    boundary: &EarnMaxPolicyBoundary,
    constraints: &[Constraint],
) -> Vec<PolicyConstraintSummary> {
    constraints
        .iter()
        .enumerate()
        .map(|(index, constraint)| {
            let instruction = match family {
                BasicPolicyFamily::CollateralLifecycle => {
                    if index == 0 {
                        "depositReserveLiquidityAndObligationCollateralV2"
                    } else {
                        "withdrawObligationCollateralAndRedeemReserveCollateralV2"
                    }
                }
                BasicPolicyFamily::DebtLifecycle => {
                    if index == 0 {
                        "borrowObligationLiquidityV2"
                    } else {
                        "repayObligationLiquidityV2"
                    }
                }
                BasicPolicyFamily::SwapRoutesA | BasicPolicyFamily::SwapRoutesB => {
                    "sharedAccountsRoute"
                }
            };
            let mut account_conditions: Vec<_> = constraint
                .account_pubkeys
                .iter()
                .map(|(account_index, pubkeys)| PolicyAccountConditionSummary {
                    account_index: *account_index,
                    condition: if pubkeys.len() == 1 {
                        format!("equals {}", pubkeys[0])
                    } else {
                        format!(
                            "one of {{{}}}",
                            pubkeys
                                .iter()
                                .map(ToString::to_string)
                                .collect::<Vec<_>>()
                                .join(", ")
                        )
                    },
                })
                .chain(constraint.account_data.iter().map(|account_data| {
                    PolicyAccountConditionSummary {
                        account_index: account_data.account_index,
                        condition: format!(
                            "owner {} and data[64..96] equals vault {}",
                            account_data
                                .owner
                                .map_or_else(|| "any".to_owned(), |owner| owner.to_string()),
                            boundary.vault
                        ),
                    }
                }))
                .collect();
            account_conditions.sort_by_key(|condition| condition.account_index);
            let data_conditions = match family {
                BasicPolicyFamily::CollateralLifecycle => vec![format!(
                    "data[0..8] equals {}",
                    hex_bytes(if index == 0 {
                        DEPOSIT_COLLATERAL
                    } else {
                        WITHDRAW_COLLATERAL
                    })
                )],
                BasicPolicyFamily::DebtLifecycle => vec![format!(
                    "data[0..8] equals {}",
                    hex_bytes(if index == 0 { BORROW_DEBT } else { REPAY_DEBT })
                )],
                BasicPolicyFamily::SwapRoutesA | BasicPolicyFamily::SwapRoutesB => {
                    vec![format!(
                        "data[0..8] equals {} (SharedAccountsRoute)",
                        hex_bytes(EARN_MAX_SHARED_ACCOUNTS_ROUTE)
                    )]
                }
            };
            PolicyConstraintSummary {
                program: constraint.program_id.to_string(),
                instruction: instruction.to_owned(),
                account_conditions,
                data_conditions,
            }
        })
        .collect()
}

fn hex_bytes<const N: usize>(bytes: [u8; N]) -> String {
    bytes.iter().map(|byte| format!("{byte:02x}")).collect()
}

fn hex_digest(bytes: &[u8]) -> String {
    bytes.iter().map(|byte| format!("{byte:02x}")).collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{derive_associated_token_account, derive_kamino_obligation};
    use sha2::{Digest, Sha256};

    const SETTINGS: &str = "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6";
    const AUTHORITY: &str = "BAqgbERmvUViqDSx961xpRBHGt68SpACiWL4t9696qZZ";
    const DELEGATE: &str = "62JLkPeE4oG65LRB3W3m52RVicmYq3xFHdv7TecCsPj5";

    fn key(value: &str) -> Pubkey {
        Pubkey::from_str(value).unwrap()
    }

    fn fingerprint(instruction: &Instruction) -> String {
        let mut digest = Sha256::new();
        digest.update(instruction.program_id.as_ref());
        digest.update((instruction.accounts.len() as u32).to_le_bytes());
        for account in &instruction.accounts {
            digest.update(account.pubkey.as_ref());
            digest.update([account.is_signer as u8, account.is_writable as u8]);
        }
        digest.update((instruction.data.len() as u32).to_le_bytes());
        digest.update(&instruction.data);
        hex_digest(digest.finalize().as_slice())
    }

    #[test]
    fn known_obligations_and_custodies_match_mainnet_identities() {
        let settings = key(SETTINGS);
        let vault = derive_squads_vault(&settings, 0).0;
        let token = key(TOKEN_PROGRAM);
        let obligation = derive_kamino_obligation(
            vault,
            key("47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8"),
            1,
            0,
            key(crate::EARN_MAX_ONYC_MINT),
            key(crate::EARN_MAX_USDC_MINT),
        );
        assert_eq!(
            obligation.to_string(),
            "4LnCFir7Qc99GhjGHLcwtkfweyAMu37u5QE1zTupKsei"
        );
        assert_eq!(
            derive_associated_token_account(vault, key(crate::EARN_MAX_ONYC_MINT), token)
                .to_string(),
            "AVX9wxDTk639eZ4KaiMA7LrLhXe7Lg6DaDDVRa1Q7Ji3"
        );
        assert_eq!(
            derive_associated_token_account(vault, key(crate::EARN_MAX_USDC_MINT), token)
                .to_string(),
            "EBG2iYrcXttDy9FpWDeNVL8uaCLRCkevrpRyrAhvVYKe"
        );
        let maple_obligation = derive_kamino_obligation(
            vault,
            key("6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y"),
            1,
            0,
            key(crate::EARN_MAX_SYRUP_USDC_MINT),
            key(crate::EARN_MAX_USDC_MINT),
        );
        assert_eq!(
            maple_obligation.to_string(),
            "Gtwj2FNuiPoV2mGLC5SpHZ9PCmDrHHKaHXtacRaqm8vT"
        );
    }

    #[test]
    fn four_policy_shape_packets_and_fingerprints_are_stable() {
        let plans =
            compile_backyard_basic_policy_set(key(SETTINGS), key(AUTHORITY), key(DELEGATE), 141)
                .unwrap();
        assert_eq!(
            plans.iter().map(|plan| plan.seed).collect::<Vec<_>>(),
            [141, 142, 143, 144]
        );
        assert_eq!(
            plans
                .iter()
                .map(|plan| plan.constraints.len())
                .collect::<Vec<_>>(),
            [2, 2, 2, 2]
        );
        assert!(plans.iter().all(|plan| plan.signed_packet_bytes <= 1232));
        assert_eq!(
            plans
                .iter()
                .map(|plan| plan.signed_packet_bytes)
                .collect::<Vec<_>>(),
            [1136, 1136, 916, 916]
        );
        assert_eq!(
            plans
                .iter()
                .map(|plan| fingerprint(&plan.instruction))
                .collect::<Vec<_>>(),
            [
                "9ea2f745123519ecb3a5f080dffd409408924d6f1f0b28c0a71994855e37f0b7",
                "0630789eb9294d4bcb49df0271fb2e8af995c03824c4912ad86498761935ff11",
                "a5b356277a427638bdabd80b015ecbc4c852a8c8cced59df23db754213f7cdb9",
                "0d319b3e37f7c88b1c1ff453bbc519dcd6eeb8a1f71a3cfd3241e24997df7f43",
            ]
        );
    }

    #[test]
    fn lifecycle_constraints_match_the_worker_boundary() {
        let settings = key(SETTINGS);
        let boundary = canonical_boundary(settings).unwrap();
        let collateral =
            compile_backyard_basic_policy_set(settings, key(AUTHORITY), key(DELEGATE), 141)
                .unwrap();
        assert_eq!(
            collateral[0].constraints,
            earn_max_policy_constraints(&boundary, EarnMaxPolicyFamily::Collateral).unwrap()
        );
        assert_eq!(
            collateral[1].constraints,
            earn_max_policy_constraints(&boundary, EarnMaxPolicyFamily::Debt).unwrap()
        );
    }
}

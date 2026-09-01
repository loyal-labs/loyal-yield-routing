//! Exact, offline semantic contract for the Backyard RWA Squads policy catalog.
//!
//! This deliberately contains no discovery addresses.  A compiler must replace every
//! [`CatalogAccount::Unresolved`] value from a confirmed mainnet graph before it can
//! emit a policy instruction.  In particular, Maple/USDG must never be guessed.

use crate::squads::{
    create_semantic_program_interaction_policy_instruction, LoyalActionError,
    SemanticProgramInteractionConstraint, SemanticProgramInteractionDataConstraint,
};
use solana_sdk::{
    hash::Hash,
    instruction::Instruction,
    pubkey::Pubkey,
    signature::{Keypair, Signer},
    transaction::Transaction,
};
use std::collections::{BTreeMap, BTreeSet};

pub const SOLANA_PACKET_BYTES: usize = 1232;
/// Five market policies, one six-constraint swap policy, and two bridge policies.
pub const BEST_CASE_PHYSICAL_POLICY_COUNT: usize = 8;

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum CatalogAccount<T> {
    Resolved(T),
    Unresolved {
        label: &'static str,
        resume_condition: &'static str,
    },
}

impl<T> CatalogAccount<T> {
    pub fn resolved(&self) -> Option<&T> {
        match self {
            Self::Resolved(value) => Some(value),
            Self::Unresolved { .. } => None,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum Stablecoin {
    Usdc,
    Usdg,
    Usds,
    Pyusd,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum RwaCollateral {
    Onyc,
    Prime,
    SyrupUsdc,
    Auto,
    Usde,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub enum Market {
    Onre,
    Prime,
    Maple,
    Auto,
    Ethena,
}

#[repr(u8)]
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum KaminoOperation {
    Deposit,
    Withdraw,
    Borrow,
    Repay,
}

#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
pub struct Lane {
    pub market: Market,
    pub collateral: RwaCollateral,
    pub debt: Stablecoin,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct KaminoPermission {
    pub lane: Lane,
    pub operation: KaminoOperation,
    /// The policy compiler binds this to the lane's own reserve graph, never a mint-only lookup.
    pub debt_reserve: CatalogAccount<String>,
    pub collateral_reserve: CatalogAccount<String>,
    pub obligation: CatalogAccount<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub struct SwapEdge {
    pub from: Asset,
    pub to: Asset,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum Asset {
    Stable(Stablecoin),
    Rwa(RwaCollateral),
}

/// The immutable semantic expansion required by verifier V03.
pub fn lanes() -> Vec<Lane> {
    vec![
        Lane {
            market: Market::Onre,
            collateral: RwaCollateral::Onyc,
            debt: Stablecoin::Usdc,
        },
        Lane {
            market: Market::Onre,
            collateral: RwaCollateral::Onyc,
            debt: Stablecoin::Usdg,
        },
        Lane {
            market: Market::Onre,
            collateral: RwaCollateral::Onyc,
            debt: Stablecoin::Usds,
        },
        Lane {
            market: Market::Prime,
            collateral: RwaCollateral::Prime,
            debt: Stablecoin::Usdc,
        },
        Lane {
            market: Market::Prime,
            collateral: RwaCollateral::Prime,
            debt: Stablecoin::Pyusd,
        },
        Lane {
            market: Market::Prime,
            collateral: RwaCollateral::Prime,
            debt: Stablecoin::Usds,
        },
        Lane {
            market: Market::Maple,
            collateral: RwaCollateral::SyrupUsdc,
            debt: Stablecoin::Usdc,
        },
        Lane {
            market: Market::Maple,
            collateral: RwaCollateral::SyrupUsdc,
            debt: Stablecoin::Usdg,
        },
        Lane {
            market: Market::Maple,
            collateral: RwaCollateral::SyrupUsdc,
            debt: Stablecoin::Pyusd,
        },
        Lane {
            market: Market::Auto,
            collateral: RwaCollateral::Auto,
            debt: Stablecoin::Pyusd,
        },
        Lane {
            market: Market::Ethena,
            collateral: RwaCollateral::Usde,
            debt: Stablecoin::Pyusd,
        },
    ]
}

pub fn swap_edges() -> BTreeSet<SwapEdge> {
    let stable = [
        Stablecoin::Usdc,
        Stablecoin::Usdg,
        Stablecoin::Usds,
        Stablecoin::Pyusd,
    ];
    let rwa = [
        RwaCollateral::Onyc,
        RwaCollateral::Prime,
        RwaCollateral::SyrupUsdc,
        RwaCollateral::Auto,
        RwaCollateral::Usde,
    ];
    let mut out = BTreeSet::new();
    for s in stable {
        for r in rwa {
            out.insert(SwapEdge {
                from: Asset::Stable(s),
                to: Asset::Rwa(r),
            });
            out.insert(SwapEdge {
                from: Asset::Rwa(r),
                to: Asset::Stable(s),
            });
        }
    }
    for from in stable {
        for to in stable {
            if from != to {
                out.insert(SwapEdge {
                    from: Asset::Stable(from),
                    to: Asset::Stable(to),
                });
            }
        }
    }
    out
}

/// Expand only with a lane-keyed graph.  The map key makes a cartesian market/custody
/// authorization impossible to express in this compiler boundary.
pub fn expand_permissions(
    graph: &BTreeMap<
        Lane,
        (
            CatalogAccount<String>,
            CatalogAccount<String>,
            CatalogAccount<String>,
        ),
    >,
) -> Result<Vec<KaminoPermission>, CatalogError> {
    let expected = lanes();
    if graph.len() != expected.len() || expected.iter().any(|lane| !graph.contains_key(lane)) {
        return Err(CatalogError::IncompleteLaneGraph);
    }
    let mut out = Vec::with_capacity(44);
    for lane in expected {
        let (collateral_reserve, debt_reserve, obligation) =
            graph.get(&lane).expect("checked").clone();
        for operation in [
            KaminoOperation::Deposit,
            KaminoOperation::Withdraw,
            KaminoOperation::Borrow,
            KaminoOperation::Repay,
        ] {
            out.push(KaminoPermission {
                lane: lane.clone(),
                operation,
                debt_reserve: debt_reserve.clone(),
                collateral_reserve: collateral_reserve.clone(),
                obligation: obligation.clone(),
            });
        }
    }
    Ok(out)
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub enum CatalogError {
    IncompleteLaneGraph,
    UnresolvedAccount {
        label: &'static str,
        resume_condition: &'static str,
    },
    NoSafePackingRung,
    Squads(LoyalActionError),
}

impl From<LoyalActionError> for CatalogError {
    fn from(value: LoyalActionError) -> Self {
        Self::Squads(value)
    }
}

/// Confirmed, position-specific accounts for one K-Lend lane.  The graph is keyed
/// by [`Lane`], so a debt reserve or obligation cannot be reused by a different
/// same-mint lane unless the caller explicitly makes that (reviewable) binding.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ResolvedLaneAccounts {
    pub klend_program: Pubkey,
    pub vault: Pubkey,
    pub lending_market: Pubkey,
    pub collateral_reserve: Pubkey,
    pub debt_reserve: Pubkey,
    pub obligation: Pubkey,
    pub lending_market_authority: Pubkey,
    pub collateral_supply: Pubkey,
    pub debt_liquidity_supply: Pubkey,
    pub collateral_custody: Pubkey,
    pub debt_custody: Pubkey,
    pub collateral_token_program: Pubkey,
    pub debt_token_program: Pubkey,
}

/// Instruction offsets are manifest-owned rather than inferred from an asset
/// symbol.  The compiler records them in its canonical output alongside the
/// resolved graph.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct KaminoInstructionLayout {
    pub tag: u8,
    pub market_index: u8,
    pub collateral_reserve_index: u8,
    pub debt_reserve_index: u8,
    pub obligation_index: u8,
    pub vault_index: u8,
    pub authority_index: u8,
    pub collateral_supply_index: u8,
    pub debt_supply_index: u8,
    pub collateral_custody_index: u8,
    pub debt_custody_index: u8,
    pub collateral_token_program_index: u8,
    pub debt_token_program_index: u8,
}

fn lane_constraints(
    accounts: &ResolvedLaneAccounts,
    layouts: &[(KaminoOperation, KaminoInstructionLayout)],
) -> Result<Vec<SemanticProgramInteractionConstraint>, CatalogError> {
    if layouts.is_empty() {
        return Err(CatalogError::IncompleteLaneGraph);
    }
    let mut seen = BTreeSet::new();
    layouts
        .iter()
        .map(|(operation, layout)| {
            if !seen.insert(*operation as u8) {
                return Err(CatalogError::IncompleteLaneGraph);
            }
            Ok(SemanticProgramInteractionConstraint {
                program_id: accounts.klend_program,
                account_pubkeys: vec![
                    (layout.vault_index, vec![accounts.vault]),
                    (layout.market_index, vec![accounts.lending_market]),
                    (
                        layout.collateral_reserve_index,
                        vec![accounts.collateral_reserve],
                    ),
                    (layout.debt_reserve_index, vec![accounts.debt_reserve]),
                    (layout.obligation_index, vec![accounts.obligation]),
                    (
                        layout.authority_index,
                        vec![accounts.lending_market_authority],
                    ),
                    (
                        layout.collateral_supply_index,
                        vec![accounts.collateral_supply],
                    ),
                    (
                        layout.debt_supply_index,
                        vec![accounts.debt_liquidity_supply],
                    ),
                    (
                        layout.collateral_custody_index,
                        vec![accounts.collateral_custody],
                    ),
                    (layout.debt_custody_index, vec![accounts.debt_custody]),
                    (
                        layout.collateral_token_program_index,
                        vec![accounts.collateral_token_program],
                    ),
                    (
                        layout.debt_token_program_index,
                        vec![accounts.debt_token_program],
                    ),
                ],
                account_data: Vec::new(),
                data: vec![SemanticProgramInteractionDataConstraint::U8Equals {
                    offset: 0,
                    value: layout.tag,
                }],
            })
        })
        .collect()
}

/// Build exactly one physical policy per market. Each lane contributes four
/// operation-specific constraints, preserving its own indexed pins. The five
/// canonical markets therefore have 12/12/12/4/4 constraints respectively.
pub fn create_market_policies(
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    first_seed: u64,
    account_index: u8,
    graph: &BTreeMap<
        Lane,
        (
            ResolvedLaneAccounts,
            Vec<(KaminoOperation, KaminoInstructionLayout)>,
        ),
    >,
) -> Result<Vec<Instruction>, CatalogError> {
    if graph.len() != 11 || lanes().iter().any(|lane| !graph.contains_key(lane)) {
        return Err(CatalogError::IncompleteLaneGraph);
    }
    let markets = [
        Market::Onre,
        Market::Prime,
        Market::Maple,
        Market::Auto,
        Market::Ethena,
    ];
    let mut out = Vec::with_capacity(5);
    for (offset, market) in markets.iter().enumerate() {
        out.push(create_market_policy(
            settings,
            authority,
            delegated_signer,
            first_seed + offset as u64,
            account_index,
            *market,
            &[
                KaminoOperation::Deposit,
                KaminoOperation::Withdraw,
                KaminoOperation::Borrow,
                KaminoOperation::Repay,
            ],
            graph,
        )?);
    }
    Ok(out)
}

/// Compile one complete market direction/lifecycle slice.  The caller must use
/// this only as one member of a packing layout that covers every operation for
/// the market; the returned instruction carries no wildcard account pins.
pub fn create_market_policy(
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    seed: u64,
    account_index: u8,
    market: Market,
    operations: &[KaminoOperation],
    graph: &BTreeMap<
        Lane,
        (
            ResolvedLaneAccounts,
            Vec<(KaminoOperation, KaminoInstructionLayout)>,
        ),
    >,
) -> Result<Instruction, CatalogError> {
    if operations.is_empty()
        || graph.len() != 11
        || lanes().iter().any(|lane| !graph.contains_key(lane))
    {
        return Err(CatalogError::IncompleteLaneGraph);
    }
    let allowed = operations
        .iter()
        .map(|operation| *operation as u8)
        .collect::<BTreeSet<_>>();
    if allowed.len() != operations.len() {
        return Err(CatalogError::IncompleteLaneGraph);
    }
    let mut constraints = Vec::new();
    for lane in lanes().into_iter().filter(|lane| lane.market == market) {
        let (accounts, layouts) = graph.get(&lane).expect("checked");
        let selected = layouts
            .iter()
            .copied()
            .filter(|(operation, _)| allowed.contains(&(*operation as u8)))
            .collect::<Vec<_>>();
        if selected.len() != operations.len() {
            return Err(CatalogError::IncompleteLaneGraph);
        }
        constraints.extend(lane_constraints(accounts, &selected)?);
    }
    create_semantic_program_interaction_policy_instruction(
        settings,
        authority,
        delegated_signer,
        seed,
        account_index,
        constraints,
    )
    .map_err(Into::into)
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ResolvedSwapBiclique {
    pub program: Pubkey,
    pub authority: Pubkey,
    pub source_mints: Vec<Pubkey>,
    pub destination_mints: Vec<Pubkey>,
    pub source_custodies: Vec<Pubkey>,
    pub destination_custodies: Vec<Pubkey>,
    pub source_token_programs: Vec<Pubkey>,
    pub destination_token_programs: Vec<Pubkey>,
    pub source_index: u8,
    pub destination_index: u8,
    pub authority_index: u8,
    pub source_mint_index: u8,
    pub destination_mint_index: u8,
    pub source_token_program_index: u8,
    pub destination_token_program_index: u8,
    pub tag: u8,
}

/// Build one physical swap policy with exactly six biclique constraints:
/// stable->RWA, RWA->stable, plus one source-specific stable->stable constraint
/// per stable. Squads pins custody addresses, while the external token-account
/// mint validator remains the owner of custody/mint correlation.
pub fn create_swap_policy(
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    seed: u64,
    account_index: u8,
    constraints: Vec<ResolvedSwapBiclique>,
) -> Result<Instruction, CatalogError> {
    if constraints.len() != 6
        || constraints
            .iter()
            .map(|edge| edge.source_mints.len() * edge.destination_mints.len())
            .sum::<usize>()
            != 52
    {
        return Err(CatalogError::IncompleteLaneGraph);
    }
    create_swap_policy_slice(
        settings,
        authority,
        delegated_signer,
        seed,
        account_index,
        constraints,
    )
}

/// Compile one non-empty directed swap slice.  Callers that split the 52-edge
/// graph must prove their slices are disjoint and complete against
/// [`swap_edges`] before using this instruction for an installed policy.
pub fn create_swap_policy_slice(
    settings: Pubkey,
    authority: Pubkey,
    delegated_signer: Pubkey,
    seed: u64,
    account_index: u8,
    constraints: Vec<ResolvedSwapBiclique>,
) -> Result<Instruction, CatalogError> {
    if constraints.is_empty() {
        return Err(CatalogError::IncompleteLaneGraph);
    }
    Ok(create_semantic_program_interaction_policy_instruction(
        settings,
        authority,
        delegated_signer,
        seed,
        account_index,
        constraints
            .into_iter()
            .map(|edge| SemanticProgramInteractionConstraint {
                program_id: edge.program,
                account_pubkeys: vec![
                    (edge.authority_index, vec![edge.authority]),
                    (edge.source_index, edge.source_custodies),
                    (edge.destination_index, edge.destination_custodies),
                    (edge.source_mint_index, edge.source_mints),
                    (edge.destination_mint_index, edge.destination_mints),
                    (edge.source_token_program_index, edge.source_token_programs),
                    (
                        edge.destination_token_program_index,
                        edge.destination_token_programs,
                    ),
                ],
                account_data: Vec::new(),
                data: vec![SemanticProgramInteractionDataConstraint::U8Equals {
                    offset: 0,
                    value: edge.tag,
                }],
            })
            .collect(),
    )?)
}

/// Reject unresolved values before policy bytes are constructed.
pub fn require_resolved(permissions: &[KaminoPermission]) -> Result<(), CatalogError> {
    for permission in permissions {
        for account in [
            &permission.debt_reserve,
            &permission.collateral_reserve,
            &permission.obligation,
        ] {
            if let CatalogAccount::Unresolved {
                label,
                resume_condition,
            } = account
            {
                return Err(CatalogError::UnresolvedAccount {
                    label,
                    resume_condition,
                });
            }
        }
    }
    Ok(())
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub enum PackingRung {
    Market,
    RiskDirection,
    Lifecycle,
    Lane,
    SplitSwapOrBridge,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SignedPacket {
    pub policy: String,
    pub bytes: usize,
}
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PackingSelection {
    pub rung: PackingRung,
    pub packets: Vec<SignedPacket>,
}

/// Measures the actual legacy wire that would carry one policy-create instruction.
///
/// This is deliberately a packet measurement primitive, not a policy installer:
/// callers choose an ephemeral authority and recent blockhash, and no RPC call is
/// made.  Packet length is invariant under the key/signature values themselves;
/// callers must still use a current resolved account graph before treating an
/// artifact as installable.
pub fn signed_policy_create_packet_bytes(
    instruction: &Instruction,
    authority: &Keypair,
    recent_blockhash: Hash,
) -> usize {
    let transaction = Transaction::new_signed_with_payer(
        std::slice::from_ref(instruction),
        Some(&authority.pubkey()),
        &[authority],
        recent_blockhash,
    );
    bincode::serialize(&transaction)
        .expect("legacy Solana transaction serialization cannot fail")
        .len()
}

/// Select the first supplied complete layout whose *signed* create/update packets fit.
/// Callers must provide every policy in every rung; this API cannot silently drop a pin.
pub fn select_first_fitting_layout(
    layouts: &[(PackingRung, Vec<SignedPacket>)],
) -> Result<PackingSelection, CatalogError> {
    for (rung, packets) in layouts {
        if !packets.is_empty()
            && packets
                .iter()
                .all(|packet| packet.bytes <= SOLANA_PACKET_BYTES)
        {
            return Ok(PackingSelection {
                rung: *rung,
                packets: packets.clone(),
            });
        }
    }
    Err(CatalogError::NoSafePackingRung)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn v03_has_exact_lane_permission_and_swap_counts() {
        assert_eq!(lanes().len(), 11);
        let edges = swap_edges();
        assert_eq!(edges.len(), 52);
        assert!(edges.iter().all(|edge| edge.from != edge.to
            && !matches!((edge.from, edge.to), (Asset::Rwa(_), Asset::Rwa(_)))));
    }
    #[test]
    fn graph_is_lane_keyed_and_rejects_missing_maple_usdg() {
        let mut graph = BTreeMap::new();
        for lane in lanes() {
            graph.insert(
                lane.clone(),
                (
                    CatalogAccount::Resolved(format!("c:{:?}", lane)),
                    CatalogAccount::Resolved(format!("d:{:?}", lane)),
                    CatalogAccount::Resolved(format!("o:{:?}", lane)),
                ),
            );
        }
        assert_eq!(expand_permissions(&graph).unwrap().len(), 44);
        graph.remove(&Lane {
            market: Market::Maple,
            collateral: RwaCollateral::SyrupUsdc,
            debt: Stablecoin::Usdg,
        });
        assert_eq!(
            expand_permissions(&graph),
            Err(CatalogError::IncompleteLaneGraph)
        );
    }
    #[test]
    fn unresolved_maple_usdg_is_a_typed_blocker() {
        let mut graph = BTreeMap::new();
        for lane in lanes() {
            let debt = if lane.market == Market::Maple && lane.debt == Stablecoin::Usdg {
                CatalogAccount::Unresolved {
                    label: "maple/usdg debt reserve",
                    resume_condition: "resolve and confirm the current Maple/USDG reserve graph",
                }
            } else {
                CatalogAccount::Resolved("d".to_owned())
            };
            graph.insert(
                lane.clone(),
                (
                    CatalogAccount::Resolved("c".to_owned()),
                    debt,
                    CatalogAccount::Resolved("o".to_owned()),
                ),
            );
        }
        assert!(matches!(
            require_resolved(&expand_permissions(&graph).unwrap()),
            Err(CatalogError::UnresolvedAccount { .. })
        ));
    }
    #[test]
    fn chooses_first_signed_packet_layout_that_fits() {
        let result = select_first_fitting_layout(&[
            (
                PackingRung::Market,
                vec![SignedPacket {
                    policy: "maple".into(),
                    bytes: 1233,
                }],
            ),
            (
                PackingRung::RiskDirection,
                vec![
                    SignedPacket {
                        policy: "maple-increase".into(),
                        bytes: 1232,
                    },
                    SignedPacket {
                        policy: "maple-reduce".into(),
                        bytes: 1200,
                    },
                ],
            ),
        ])
        .unwrap();
        assert_eq!(result.rung, PackingRung::RiskDirection);
    }

    #[test]
    fn same_mint_lanes_emit_different_exact_reserve_and_obligation_constraints() {
        let key = |byte| Pubkey::new_from_array([byte; 32]);
        let layout = |tag| KaminoInstructionLayout {
            tag,
            market_index: 1,
            collateral_reserve_index: 2,
            debt_reserve_index: 3,
            obligation_index: 4,
            vault_index: 0,
            authority_index: 5,
            collateral_supply_index: 6,
            debt_supply_index: 7,
            collateral_custody_index: 8,
            debt_custody_index: 9,
            collateral_token_program_index: 10,
            debt_token_program_index: 11,
        };
        let layouts = [
            (KaminoOperation::Deposit, layout(1)),
            (KaminoOperation::Withdraw, layout(2)),
            (KaminoOperation::Borrow, layout(3)),
            (KaminoOperation::Repay, layout(4)),
        ];
        let accounts = |debt_reserve, obligation| ResolvedLaneAccounts {
            klend_program: key(1),
            vault: key(2),
            lending_market: key(3),
            collateral_reserve: key(4),
            debt_reserve,
            obligation,
            lending_market_authority: key(5),
            collateral_supply: key(6),
            debt_liquidity_supply: key(7),
            collateral_custody: key(8),
            debt_custody: key(9),
            collateral_token_program: key(10),
            debt_token_program: key(11),
        };
        let graph = |debt: u8, obligation: u8| {
            lanes()
                .into_iter()
                .enumerate()
                .map(|(index, lane)| {
                    (
                        lane,
                        (
                            accounts(
                                key(debt.wrapping_add(index as u8)),
                                key(obligation.wrapping_add(index as u8)),
                            ),
                            layouts.to_vec(),
                        ),
                    )
                })
                .collect::<BTreeMap<_, _>>()
        };
        let first =
            create_market_policies(key(11), key(12), key(13), 1, 0, &graph(14, 40)).unwrap();
        let second =
            create_market_policies(key(11), key(12), key(13), 1, 0, &graph(16, 60)).unwrap();
        assert_eq!(
            first.len() + 1 + 2,
            BEST_CASE_PHYSICAL_POLICY_COUNT,
            "five market policies plus swap and two bridges is the best-case eight-policy layout"
        );
        assert_eq!(
            [
                Market::Onre,
                Market::Prime,
                Market::Maple,
                Market::Auto,
                Market::Ethena
            ]
            .map(|market| lanes()
                .into_iter()
                .filter(|lane| lane.market == market)
                .count()
                * 4),
            [12, 12, 12, 4, 4]
        );
        assert_ne!(
            first[0].data, second[0].data,
            "same mint cannot collapse distinct reserve/obligation lanes"
        );
    }

    #[test]
    fn swap_policy_has_exactly_six_biclique_constraints() {
        let key = |byte| Pubkey::new_from_array([byte; 32]);
        let edge = |byte: u8, source_count: u8, destination_count: u8| ResolvedSwapBiclique {
            program: key(1),
            authority: key(2),
            source_mints: (0..source_count).map(|i| key(byte + i)).collect(),
            destination_mints: (0..destination_count).map(|i| key(byte + 10 + i)).collect(),
            source_custodies: (0..source_count).map(|i| key(byte + 20 + i)).collect(),
            destination_custodies: (0..destination_count).map(|i| key(byte + 30 + i)).collect(),
            source_token_programs: vec![key(4)],
            destination_token_programs: vec![key(5)],
            source_index: 0,
            destination_index: 1,
            authority_index: 2,
            source_mint_index: 3,
            destination_mint_index: 4,
            source_token_program_index: 5,
            destination_token_program_index: 6,
            tag: 9,
        };
        let policy = create_swap_policy(
            key(10),
            key(11),
            key(12),
            20,
            0,
            vec![
                edge(20, 4, 5),
                edge(30, 5, 4),
                edge(40, 1, 3),
                edge(50, 1, 3),
                edge(60, 1, 3),
                edge(70, 1, 3),
            ],
        )
        .unwrap();
        assert!(!policy.data.is_empty());
    }

    #[test]
    fn structural_signed_packet_measurement_exposes_the_first_safe_rung() {
        // These addresses model only equality relationships in the complete
        // catalog. They are deliberately non-installable: current account
        // identities and layouts must come from a fresh chain read before a
        // policy may be created. Legacy packet length, however, is determined
        // by the compiled account/message shape, not the public-key bytes.
        let mut next = 1u8;
        let mut key = || {
            let out = Pubkey::new_from_array([next; 32]);
            next = next.checked_add(1).expect("measurement key range");
            out
        };
        let settings = key();
        let authority = Keypair::new_from_array([7; 32]);
        let delegated_signer = key();
        let vault = key();
        let klend = key();
        let mut market_accounts = BTreeMap::new();
        let mut collateral_accounts = BTreeMap::new();
        let mut graph = BTreeMap::new();
        let layout = |tag| KaminoInstructionLayout {
            tag,
            market_index: 1,
            collateral_reserve_index: 2,
            debt_reserve_index: 3,
            obligation_index: 4,
            vault_index: 0,
            authority_index: 5,
            collateral_supply_index: 6,
            debt_supply_index: 7,
            collateral_custody_index: 8,
            debt_custody_index: 9,
            collateral_token_program_index: 10,
            debt_token_program_index: 11,
        };
        for lane in lanes() {
            let market = *market_accounts.entry(lane.market).or_insert_with(&mut key);
            let (collateral_reserve, collateral_supply) = *collateral_accounts
                .entry(lane.collateral)
                .or_insert_with(|| (key(), key()));
            graph.insert(
                lane,
                (
                    ResolvedLaneAccounts {
                        klend_program: klend,
                        vault,
                        lending_market: market,
                        collateral_reserve,
                        debt_reserve: key(),
                        obligation: key(),
                        lending_market_authority: key(),
                        collateral_supply,
                        debt_liquidity_supply: key(),
                        collateral_custody: key(),
                        debt_custody: key(),
                        collateral_token_program: key(),
                        debt_token_program: key(),
                    },
                    vec![
                        (KaminoOperation::Deposit, layout(1)),
                        (KaminoOperation::Withdraw, layout(2)),
                        (KaminoOperation::Borrow, layout(3)),
                        (KaminoOperation::Repay, layout(4)),
                    ],
                ),
            );
        }
        let market_packets =
            create_market_policies(settings, authority.pubkey(), delegated_signer, 1, 0, &graph)
                .expect("complete structural graph compiles")
                .into_iter()
                .map(|instruction| {
                    signed_policy_create_packet_bytes(&instruction, &authority, Hash::new_unique())
                })
                .collect::<Vec<_>>();
        assert_eq!(market_packets, vec![2163, 2163, 2163, 1059, 1059]);

        let mut split_packets = Vec::new();
        let mut seed = 20;
        for market in [Market::Onre, Market::Prime, Market::Maple] {
            for operations in [
                &[KaminoOperation::Deposit, KaminoOperation::Borrow][..],
                &[KaminoOperation::Withdraw, KaminoOperation::Repay][..],
            ] {
                let instruction = create_market_policy(
                    settings,
                    authority.pubkey(),
                    delegated_signer,
                    seed,
                    0,
                    market,
                    operations,
                    &graph,
                )
                .expect("risk-direction structural graph compiles");
                split_packets.push(signed_policy_create_packet_bytes(
                    &instruction,
                    &authority,
                    Hash::new_unique(),
                ));
                seed += 1;
            }
        }
        for market in [Market::Auto, Market::Ethena] {
            let instruction = create_market_policy(
                settings,
                authority.pubkey(),
                delegated_signer,
                seed,
                0,
                market,
                &[
                    KaminoOperation::Deposit,
                    KaminoOperation::Withdraw,
                    KaminoOperation::Borrow,
                    KaminoOperation::Repay,
                ],
                &graph,
            )
            .expect("singleton market structural graph compiles");
            split_packets.push(signed_policy_create_packet_bytes(
                &instruction,
                &authority,
                Hash::new_unique(),
            ));
            seed += 1;
        }
        assert_eq!(
            split_packets,
            vec![1719, 1719, 1719, 1719, 1719, 1719, 1059, 1059]
        );
        assert!(split_packets
            .iter()
            .any(|bytes| *bytes > SOLANA_PACKET_BYTES));

        let lifecycle_packets = [Market::Onre, Market::Prime, Market::Maple]
            .into_iter()
            .flat_map(|market| {
                [
                    KaminoOperation::Deposit,
                    KaminoOperation::Withdraw,
                    KaminoOperation::Borrow,
                    KaminoOperation::Repay,
                ]
                .into_iter()
                .map(move |operation| (market, operation))
            })
            .enumerate()
            .map(|(offset, (market, operation))| {
                let instruction = create_market_policy(
                    settings,
                    authority.pubkey(),
                    delegated_signer,
                    50 + offset as u64,
                    0,
                    market,
                    &[operation],
                    &graph,
                )
                .expect("lifecycle structural graph compiles");
                signed_policy_create_packet_bytes(&instruction, &authority, Hash::new_unique())
            })
            .collect::<Vec<_>>();
        assert_eq!(lifecycle_packets, vec![1497; 12]);
        assert!(lifecycle_packets
            .iter()
            .any(|bytes| *bytes > SOLANA_PACKET_BYTES));

        let lane_packets = lanes()
            .into_iter()
            .enumerate()
            .map(|(offset, lane)| {
                let (accounts, layouts) = graph.get(&lane).expect("complete graph");
                let instruction = create_semantic_program_interaction_policy_instruction(
                    settings,
                    authority.pubkey(),
                    delegated_signer,
                    100 + offset as u64,
                    0,
                    lane_constraints(accounts, layouts).expect("complete lane layouts"),
                )
                .expect("lane structural graph compiles");
                signed_policy_create_packet_bytes(&instruction, &authority, Hash::new_unique())
            })
            .collect::<Vec<_>>();
        assert_eq!(lane_packets, vec![1059; 11]);
        assert!(lane_packets
            .iter()
            .all(|bytes| *bytes <= SOLANA_PACKET_BYTES));

        let swap_program = key();
        let stable_mints = (0..4).map(|_| key()).collect::<Vec<_>>();
        let rwa_mints = (0..5).map(|_| key()).collect::<Vec<_>>();
        let stable_custodies = (0..4).map(|_| key()).collect::<Vec<_>>();
        let rwa_custodies = (0..5).map(|_| key()).collect::<Vec<_>>();
        let token_programs = vec![key(), key()];
        let biclique =
            |source_mints: Vec<Pubkey>,
             destination_mints: Vec<Pubkey>,
             source_custodies: Vec<Pubkey>,
             destination_custodies: Vec<Pubkey>| ResolvedSwapBiclique {
                program: swap_program,
                authority: delegated_signer,
                source_mints,
                destination_mints,
                source_custodies,
                destination_custodies,
                source_token_programs: token_programs.clone(),
                destination_token_programs: token_programs.clone(),
                source_index: 0,
                destination_index: 1,
                authority_index: 2,
                source_mint_index: 3,
                destination_mint_index: 4,
                source_token_program_index: 5,
                destination_token_program_index: 6,
                tag: 9,
            };
        let mut swap_constraints = vec![
            biclique(
                stable_mints.clone(),
                rwa_mints.clone(),
                stable_custodies.clone(),
                rwa_custodies.clone(),
            ),
            biclique(
                rwa_mints.clone(),
                stable_mints.clone(),
                rwa_custodies.clone(),
                stable_custodies.clone(),
            ),
        ];
        for source in 0..4 {
            swap_constraints.push(biclique(
                vec![stable_mints[source]],
                stable_mints
                    .iter()
                    .enumerate()
                    .filter_map(|(index, mint)| (index != source).then_some(*mint))
                    .collect(),
                vec![stable_custodies[source]],
                stable_custodies
                    .iter()
                    .enumerate()
                    .filter_map(|(index, custody)| (index != source).then_some(*custody))
                    .collect(),
            ));
        }
        let swap_instruction = create_swap_policy(
            settings,
            authority.pubkey(),
            delegated_signer,
            200,
            0,
            swap_constraints.clone(),
        )
        .expect("52-edge structural swap graph compiles");
        let swap_packet =
            signed_policy_create_packet_bytes(&swap_instruction, &authority, Hash::new_unique());
        assert_eq!(swap_packet, 1401);

        let swap_split_packets = [
            vec![swap_constraints[0].clone()],
            vec![swap_constraints[1].clone()],
            swap_constraints[2..].to_vec(),
        ]
        .into_iter()
        .enumerate()
        .map(|(offset, constraints)| {
            let instruction = create_swap_policy_slice(
                settings,
                authority.pubkey(),
                delegated_signer,
                201 + offset as u64,
                0,
                constraints,
            )
            .expect("structural swap slice compiles");
            signed_policy_create_packet_bytes(&instruction, &authority, Hash::new_unique())
        })
        .collect::<Vec<_>>();
        assert_eq!(swap_split_packets, vec![1116, 1116, 951]);
        assert!(swap_split_packets
            .iter()
            .all(|bytes| *bytes <= SOLANA_PACKET_BYTES));
    }
}

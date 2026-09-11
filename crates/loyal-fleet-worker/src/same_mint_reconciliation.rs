//! Exact finalized effects for reserve routes whose source is no longer empty.
//!
//! A later balance is not a transaction receipt: a successful full withdrawal
//! can be followed by another deposit. Never erase that balance or replay the
//! route. Only the exact durably signed atomic route can authorize this fallback.

use super::*;
use solana_client::rpc_config::RpcTransactionConfig;
use solana_transaction_status_client_types::{
    EncodedConfirmedTransactionWithStatusMeta, UiTransactionEncoding,
};

pub(super) struct FinalizedSameMintRouteProof {
    decision_id: DecisionId,
    finalized_slot: i64,
}

impl FinalizedSameMintRouteProof {
    pub(super) fn covers(&self, decision_id: DecisionId, observed_slot: i64) -> bool {
        self.decision_id == decision_id && observed_slot >= self.finalized_slot
    }
}

pub(super) fn verify_finalized_signed_route(
    rpc: &RpcClient,
    lease: &SignedRouteSubmissionLease,
) -> Result<FinalizedSameMintRouteProof, Box<dyn Error>> {
    let submission = &lease.submission;
    if submission.state != SignedRouteSubmissionState::ReconciliationPending
        || submission.movement_leg != "route"
    {
        return Err("same-mint finalized proof requires a confirmed reserve route".into());
    }
    let decision_id = submission
        .decision_id
        .ok_or("same-mint finalized proof is missing decision_id")?;
    let confirmed_slot = submission
        .confirmed_slot
        .ok_or("same-mint finalized proof is missing confirmed_slot")?;
    let signature = Signature::from_str(&submission.transaction_signature)?;
    // The runtime RPC client has a 10s request timeout. Fail closed and use the
    // existing durable reconciliation backoff if history/finality is unavailable.
    let receipt = rpc.get_transaction_with_config(
        &signature,
        RpcTransactionConfig {
            encoding: Some(UiTransactionEncoding::Base64),
            commitment: Some(CommitmentConfig::finalized()),
            max_supported_transaction_version: Some(0),
        },
    )?;
    verify_finalized_receipt(
        &receipt,
        confirmed_slot,
        &signature,
        &submission.signed_transaction,
    )?;
    Ok(FinalizedSameMintRouteProof {
        decision_id,
        finalized_slot: confirmed_slot,
    })
}

fn verify_finalized_receipt(
    receipt: &EncodedConfirmedTransactionWithStatusMeta,
    confirmed_slot: i64,
    signature: &Signature,
    signed_wire: &[u8],
) -> Result<(), Box<dyn Error>> {
    if i64::try_from(receipt.slot)? != confirmed_slot {
        return Err("same-mint finalized transaction slot differs from confirmed slot".into());
    }
    let transaction = receipt
        .transaction
        .transaction
        .decode()
        .ok_or("same-mint finalized transaction bytes did not decode")?;
    if bincode::serialize(&transaction)? != signed_wire {
        return Err("same-mint finalized transaction differs from persisted signed wire".into());
    }
    if transaction.signatures.first() != Some(signature) {
        return Err(
            "same-mint finalized transaction signature differs from persisted identity".into(),
        );
    }
    let meta = receipt
        .transaction
        .meta
        .as_ref()
        .ok_or("same-mint finalized transaction omitted status metadata")?;
    if meta.err.is_some() || meta.status.is_err() {
        return Err("same-mint finalized transaction failed".into());
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use solana_sdk::message::Message;
    use solana_sdk::transaction::TransactionError;
    use solana_transaction_status_client_types::{
        EncodedTransaction, EncodedTransactionWithStatusMeta, TransactionBinaryEncoding,
        TransactionStatusMeta,
    };

    // Exercise the external receipt/signed-wire contract with an actual signed
    // transaction, not balance thresholds or a mocked claim of success.
    fn signed_receipt() -> (
        EncodedConfirmedTransactionWithStatusMeta,
        Vec<u8>,
        Signature,
    ) {
        let payer = Keypair::new();
        let message = Message::new_with_blockhash(
            &[system_instruction::transfer(
                &payer.pubkey(),
                &Pubkey::new_unique(),
                17,
            )],
            Some(&payer.pubkey()),
            &Hash::new_unique(),
        );
        let transaction =
            VersionedTransaction::try_new(VersionedMessage::Legacy(message), &[&payer]).unwrap();
        let wire = bincode::serialize(&transaction).unwrap();
        let receipt = EncodedConfirmedTransactionWithStatusMeta {
            slot: 443023824,
            block_time: None,
            transaction: EncodedTransactionWithStatusMeta {
                transaction: EncodedTransaction::Binary(
                    BASE64_STANDARD.encode(&wire),
                    TransactionBinaryEncoding::Base64,
                ),
                meta: Some(TransactionStatusMeta::default().into()),
                version: None,
            },
        };
        (receipt, wire, transaction.signatures[0])
    }

    #[test]
    fn finalized_proof_requires_exact_signed_identity_and_slot() {
        let (receipt, wire, signature) = signed_receipt();
        verify_finalized_receipt(&receipt, 443023824, &signature, &wire).unwrap();
        let (_, other_wire, other_signature) = signed_receipt();
        assert!(verify_finalized_receipt(&receipt, 443023824, &signature, &other_wire).is_err());
        assert!(verify_finalized_receipt(&receipt, 443023824, &other_signature, &wire).is_err());
        assert!(verify_finalized_receipt(&receipt, 443023823, &signature, &wire).is_err());
    }

    #[test]
    fn residual_source_needs_exact_finality_and_keeps_post_state_guards() {
        let (receipt, wire, signature) = signed_receipt();
        verify_finalized_receipt(&receipt, 443023824, &signature, &wire).unwrap();
        let proof = FinalizedSameMintRouteProof {
            decision_id: DecisionId(14973),
            finalized_slot: 443023824,
        };
        let decision = PreparedSameMintDecision {
            id: DecisionId(14973),
            vault_id: VaultId(4132),
            source_snapshot_id: SnapshotId(25417925),
            source_reserve: "source".to_owned(),
            target_reserve: "target".to_owned(),
            liquidity_mint: "usdc".to_owned(),
            source_liquidity_mint: "usdc".to_owned(),
            target_liquidity_mint: "usdc".to_owned(),
            amount_raw: 501835024,
            source_apy_bps: 0,
            target_apy_bps: 0,
            estimated_edge_bps: 0,
            estimated_cost_lamports: 5000,
            execution_plan: json!({}),
            idempotency_key: "receipt-contract-test".to_owned(),
        };
        let mut state = ReconciledVaultState {
            observed_slot: 446211888,
            observed_at: None,
            chain_slot: Some(446211888),
            lock_attempt_id: None,
            context: json!({}),
            positions: [("source", 2), ("target", 290)]
                .into_iter()
                .map(|(reserve, amount_raw)| ReconciledReservePosition {
                    reserve: reserve.to_owned(),
                    market: None,
                    liquidity_mint: "usdc".to_owned(),
                    amount_raw,
                    supply_apy_bps: None,
                    borrow_apy_bps: None,
                    planning_metadata: json!({}),
                })
                .collect(),
        };
        assert!(ensure_post_confirm_chain_reconcile_state(&decision, &state, None).is_err());
        ensure_post_confirm_chain_reconcile_state(&decision, &state, Some(&proof)).unwrap();
        state.observed_slot = 443023823;
        assert!(
            ensure_post_confirm_chain_reconcile_state(&decision, &state, Some(&proof)).is_err()
        );
        state.observed_slot = 446211888;
        let other_decision = PreparedSameMintDecision {
            id: DecisionId(14974),
            ..decision.clone()
        };
        assert!(
            ensure_post_confirm_chain_reconcile_state(&other_decision, &state, Some(&proof))
                .is_err()
        );
        state.positions[1].amount_raw = 0;
        assert!(
            ensure_post_confirm_chain_reconcile_state(&decision, &state, Some(&proof)).is_err()
        );
        state.positions[1].amount_raw = 290;
        state.positions[0].liquidity_mint = "other-mint".to_owned();
        assert!(
            ensure_post_confirm_chain_reconcile_state(&decision, &state, Some(&proof)).is_err()
        );
    }

    #[test]
    fn failed_or_missing_receipt_cannot_release_a_residual_source() {
        let (mut receipt, wire, signature) = signed_receipt();
        receipt.transaction.meta = None;
        assert!(verify_finalized_receipt(&receipt, 443023824, &signature, &wire).is_err());
        receipt.transaction.meta = Some(
            TransactionStatusMeta {
                status: Err(TransactionError::AccountNotFound),
                ..TransactionStatusMeta::default()
            }
            .into(),
        );
        assert!(verify_finalized_receipt(&receipt, 443023824, &signature, &wire).is_err());
        receipt.transaction.transaction = EncodedTransaction::Binary(
            BASE64_STANDARD.encode([0]),
            TransactionBinaryEncoding::Base64,
        );
        assert!(verify_finalized_receipt(&receipt, 443023824, &signature, &wire).is_err());
    }
}

package backyard

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

func (d *Database) recoverDisclosedWithdrawalCost(ctx context.Context, manifest RouteManifest, client *chain.Client, operation PersistedOperation) (bool, error) {
	return d.recoverDisclosedWithdrawalCostForDelegate(ctx, manifest, client, operation, mustKey(backyardDelegate))
}

func (d *Database) recoverDisclosedWithdrawalCostForDelegate(ctx context.Context, manifest RouteManifest, client *chain.Client, operation PersistedOperation, delegate publicKey) (bool, error) {
	var available bool
	if err := d.pool.QueryRow(ctx, `SELECT to_regclass('loyal_yield.backyard_withdrawal_cost_operations') IS NOT NULL`).Scan(&available); err != nil {
		return false, err
	}
	if !available {
		return false, nil
	}
	var signed SignedTransaction
	var effects []byte
	err := d.pool.QueryRow(ctx, `SELECT signed_wire,signed_wire_sha256,message_sha256,transaction_signature,recent_blockhash,last_valid_block_height,expected_effects FROM loyal_yield.backyard_withdrawal_cost_operations WHERE operation_id=$1 AND signed_wire IS NOT NULL`, operation.ID).Scan(&signed.signedWire, &signed.signedWireSHA256, &signed.messageSHA256, &signed.transactionSignature, &signed.recentBlockhash, &signed.lastValidBlockHeight, &effects)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	message, signature, err := splitSignedWire(signed.signedWire)
	if err != nil || sha256Bytes(signed.signedWire) != signed.signedWireSHA256 || sha256Bytes(message) != signed.messageSHA256 || encodeBase58(signature) != signed.transactionSignature || signed.lastValidBlockHeight <= 0 || !ed25519.Verify(delegate[:], message, signature) {
		return true, staleHold("withdrawal_signed_wire_journal_invalid")
	}
	signed.message = message
	m, err := decodeV1Message(message)
	if err != nil || m.RecentBlockhash.String() != signed.recentBlockhash || !json.Valid(effects) {
		return true, staleHold("withdrawal_signed_wire_journal_invalid")
	}
	out, err := chain.Observe(ctx, client, chain.Attempt{Wire: signed.signedWire, Signature: signed.transactionSignature, LastValidBlockHeight: uint64(signed.lastValidBlockHeight), Required: chain.Finalized})
	if err != nil {
		return true, err
	}
	if out.Kind == chain.Expired {
		valid, _, err := client.BlockhashValid(ctx, m.RecentBlockhash, rpc.CommitmentFinalized, out.ContextSlot)
		if err != nil {
			return true, err
		}
		if valid {
			return true, nil
		}
		tag, err := d.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='failed',recovery_reason='signature_absent_after_blockhash_expiry',updated_at=clock_timestamp() WHERE operation_id=$1 AND status='decided' AND signed_wire IS NULL`, operation.ID)
		if err != nil {
			return true, err
		}
		if tag.RowsAffected() != 1 {
			return true, staleHold("withdrawal_signed_wire_operation_changed")
		}
		return true, d.releaseWithdrawalCost(ctx, operation.ID)
	}
	if out.Kind == chain.Failed && out.Commitment < chain.Finalized {
		return true, nil
	}
	if out.Kind == chain.Landed || out.Kind == chain.Failed {
		if out.Slot == 0 || out.Slot > math.MaxInt64 {
			return true, staleHold("withdrawal_finalized_slot_unavailable")
		}
		if out.Kind == chain.Failed {
			receipt, err := readFailureReceipt(ctx, client, signed.transactionSignature)
			if err != nil {
				return true, err
			}
			if err := d.settleDisclosedWithdrawalFailure(ctx, operation, signed, effects, receipt, delegate); err != nil {
				return true, err
			}
			return true, d.settleWithdrawalCost(ctx, client, operation.ID)
		}
		if err := d.hydrateDisclosedWithdrawalOperation(ctx, &operation, signed, effects, ManualRecovery, int64(out.Slot)); err != nil {
			return true, err
		}
		return true, d.settleWithdrawalCost(ctx, client, operation.ID)
	}
	simulation, err := simulateSigned(ctx, client, signed.signedWire)
	if err != nil {
		return true, fmt.Errorf("withdrawal_signed_wire_recovery_required: %v", err)
	}
	return true, d.PersistSigned(ctx, operation.ID, signed, simulation, effects)
}

func (d *Database) settleDisclosedWithdrawalFailure(ctx context.Context, operation PersistedOperation, signed SignedTransaction, effects []byte, receipt finalizedFailureReceipt, delegate publicKey) error {
	if err := validateFinalizedFailureReceipt(receipt, signed.signedWire, signed.transactionSignature, signed.signedWireSHA256, signed.messageSHA256, delegate); err != nil {
		return staleHoldFrom(err, "withdrawal_disclosed_failure_receipt_unproven")
	}
	reason := confirmedFailureReason(receipt.Meta.Err, receipt.Meta.LogMessages)
	proof := finalizedFailureSettlement{Schema: "backyard-finalized-failure/v1", Signature: signed.transactionSignature, SignedWireSHA256: signed.signedWireSHA256, MessageSHA256: signed.messageSHA256, Slot: receipt.Slot, Reason: reason, AtomicNoCapitalMovement: true, FeeLamports: *receipt.Meta.Fee, Receipt: receipt}
	body, err := json.Marshal(proof)
	if err != nil {
		return err
	}
	tag, err := d.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations o SET status='failed',message_sha256=$2,signed_wire=$3,signed_wire_sha256=$4,transaction_signature=$5,recent_blockhash=$6,last_valid_block_height=$7,expected_effects=jsonb_set(expected_effects,'{expectedEffects}',$8::jsonb),confirmed_slot=$9,confirmation_status='finalized',reconciliation_sha256=$10,reconciled_effects=$11::jsonb,recovery_reason=$12,updated_at=clock_timestamp() WHERE operation_id=$1 AND status='decided' AND signed_wire IS NULL AND EXISTS(SELECT 1 FROM loyal_yield.backyard_withdrawal_cost_operations c WHERE c.operation_id=o.operation_id AND c.signed_wire_sha256=$4 AND c.signed_wire=$3)`, operation.ID, signed.messageSHA256, signed.signedWire, signed.signedWireSHA256, signed.transactionSignature, signed.recentBlockhash, signed.lastValidBlockHeight, effects, receipt.Slot, sha256Bytes(body), body, reason)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return staleHold("withdrawal_signed_wire_operation_changed")
	}
	return nil
}

func (d *Database) hydrateDisclosedWithdrawalOperation(ctx context.Context, operation *PersistedOperation, signed SignedTransaction, effects []byte, status OperationStatus, slot int64) error {
	tag, err := d.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status=$2,message_sha256=$3,signed_wire=$4,signed_wire_sha256=$5,transaction_signature=$6,recent_blockhash=$7,last_valid_block_height=$8,expected_effects=jsonb_set(expected_effects,'{expectedEffects}',$9::jsonb),confirmed_slot=$10,confirmation_status='finalized',recovery_reason=CASE WHEN $2='manual_recovery' THEN 'signed_wire_landed_after_simulation_disclosure' ELSE 'signed_wire_disclosed_by_simulation' END,updated_at=clock_timestamp() WHERE operation_id=$1 AND status='decided' AND (signed_wire IS NULL OR signed_wire_sha256=$5)`, operation.ID, string(status), signed.messageSHA256, signed.signedWire, signed.signedWireSHA256, signed.transactionSignature, signed.recentBlockhash, signed.lastValidBlockHeight, effects, slot)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return staleHold("withdrawal_signed_wire_operation_changed")
	}
	operation.Status = status
	operation.SignedWire = signed.signedWire
	operation.SignedWireSHA256 = signed.signedWireSHA256
	operation.TransactionSignature = signed.transactionSignature
	operation.RecentBlockhash = signed.recentBlockhash
	operation.LastValidBlockHeight = signed.lastValidBlockHeight
	operation.ConfirmedSlot = slot
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(operation.ExpectedEffects, &envelope); err != nil {
		return err
	}
	envelope["expectedEffects"] = effects
	operation.ExpectedEffects, err = json.Marshal(envelope)
	return err
}

package backyardrwa

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

// Prepare only: no queue registration, readiness assertion or broadcast. The
// future setup coordinator must establish repaired-farm/deployment readiness
// and signed-expiry recovery before enabling this in the worker. Exact setup
// bytes use the Settings admin, not the delegate-only PersistSigned path.
func buildSimulateAndPersistPolicySetup(ctx context.Context, d *Database, rpc *RPCClient, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	payment, err := d.authorizePolicySetupPayment(ctx, rpc, id)
	if err != nil {
		return err
	}
	admin, err := loadPinnedPolicySetupSigner()
	if err != nil {
		return err
	}
	build, err := signPolicySetupPayment(payment, admin)
	if err != nil {
		return err
	}
	// Persist identity before exposing valid signed bytes even to simulation.
	// Any timeout/crash retains Built with its wire and reservation; it cannot
	// masquerade as a never-signed intent or enter the send path.
	if err = d.persistPolicySetupPreparation(ctx, rpc, id, build, nil); err != nil {
		return err
	}
	simulation, err := rpc.SimulateSignedTransaction(ctx, build.SignedWire)
	if err != nil {
		return err
	}
	build.SimulationSlot = simulation.Slot
	return d.persistPolicySetupPreparation(ctx, rpc, id, build, &simulation)
}

func signPolicySetupPayment(payment policySetupPayment, admin ed25519.PrivateKey) (BuildResult, error) {
	var out BuildResult
	if len(admin) != ed25519.PrivateKeySize || publicKeyFromBytes(admin.Public().(ed25519.PublicKey)) != mustKey(bridgeSettingsSigner) {
		return out, budgetHold("setup_signer_mismatch")
	}
	// A caller cannot supply a forged public half and make the key appear pinned.
	if !ed25519.NewKeyFromSeed(admin.Seed()).Public().(ed25519.PublicKey).Equal(admin.Public()) {
		return out, budgetHold("setup_signer_mismatch")
	}
	if _, err := checkedUnsignedMessage(payment.Message); err != nil {
		return out, err
	}
	if sha256Bytes(payment.Message) != payment.Cost.MessageSHA256 || payment.Request.LastValidBlockHeight <= 0 {
		return out, budgetHold("setup_signed_intent_mismatch")
	}
	signature := ed25519.Sign(admin, payment.Message)
	wire := append([]byte{1}, signature...)
	wire = append(wire, payment.Message...)
	return BuildResult{MessageSHA256: sha256Bytes(payment.Message), SignedWire: wire,
		SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(signature),
		RecentBlockhash: payment.Request.RecentBlockhash, LastValidBlockHeight: payment.Request.LastValidBlockHeight}, nil
}

func (d *Database) persistPolicySetupPreparation(ctx context.Context, rpc *RPCClient, id string, build BuildResult, simulation *SimulationResult) error {
	if d == nil || d.pool == nil || rpc == nil || id == "" {
		return budgetHold("invalid_setup_signed_persistence")
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	budget, auth, err := d.readPhase3BudgetTx(ctx, tx, id)
	if err != nil {
		return err
	}
	from := Decided
	if simulation != nil {
		from = Built
	}
	action, err := d.validatePolicySetupReservationTx(ctx, tx, id, budget, auth, from)
	if err != nil {
		return err
	}
	// Match the currently reserved request, not the caller's pre-simulation
	// generation. An unsigned refresh racing simulation invalidates this wire.
	bound := auth
	bound.SignedWireSHA256 = build.SignedWireSHA256
	op := PersistedOperation{Operation: Operation{Decision: Decision{Action: action, StrategyKey: "OnRe/ONyc/USDC"}},
		SignedWire: build.SignedWire, SignedWireSHA256: build.SignedWireSHA256,
		TransactionSignature: build.TransactionSignature, RecentBlockhash: build.RecentBlockhash,
		LastValidBlockHeight: build.LastValidBlockHeight}
	if err = validatePolicySetupSignedPayment(bound, op); err != nil {
		return err
	}
	if err = d.persistValidatedPolicySetupSignedTx(ctx, tx, rpc, id, budget, auth, build, simulation); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Private journal half of persistPolicySetupPreparation. Its caller has locked and
// validated the reservation and verified the exact admin signature. Keeping the
// wire and budget binding atomic fences unsigned refresh before simulation RPC.
// Only the same persisted wire with successful simulation may become Signed.
func (d *Database) persistValidatedPolicySetupSignedTx(ctx context.Context, tx pgx.Tx, rpc *RPCClient, id string, budget Phase3Budget, auth phase3OperationAuthorization, build BuildResult, simulation *SimulationResult) error {
	cost := auth.SetupBuildCost
	if cost == nil || auth.SendKnownCost != nil || len(build.SignedWire) <= 65 || cost.MessageSHA256 != build.MessageSHA256 || build.MessageSHA256 != sha256Bytes(build.SignedWire[65:]) {
		return budgetHold("setup_payment_not_build_authorized")
	}
	if simulation == nil {
		if auth.SignedWireSHA256 != "" || build.SimulationSlot != 0 {
			return budgetHold("setup_payment_not_build_authorized")
		}
	} else if auth.SignedWireSHA256 != build.SignedWireSHA256 || simulation.Slot <= 0 || simulation.Slot != build.SimulationSlot {
		return budgetHold("setup_simulation_wire_mismatch")
	}
	minimum, expiry := cost.ObservationSlot, cost.ValidThroughSlot
	if auth.SetupCompletionCost != nil {
		minimum = max(minimum, auth.SetupCompletionCost.ObservationSlot)
		expiry = min(expiry, auth.SetupCompletionCost.ValidThroughSlot)
	}
	if simulation != nil && (simulation.Slot < minimum || simulation.Slot > expiry) {
		return budgetHold("setup_simulation_outside_valuation")
	}
	slot, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		return err
	}
	if slot < minimum || slot > expiry || (simulation != nil && slot < simulation.Slot) {
		return budgetHold("policy_setup_valuation_expired")
	}
	// The setup Built CHECK requires this binding on the same row. Write it
	// while still Decided, then transition within this transaction; an immediate
	// PostgreSQL constraint cannot wait for a later authorization update.
	auth.SignedWireSHA256 = build.SignedWireSHA256
	if err = d.writePhase3BudgetTx(ctx, tx, id, budget, auth); err != nil {
		return err
	}
	if simulation != nil {
		encoded, err := json.Marshal(simulation)
		if err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET
		 status='signed',simulation_slot=$8,simulation_result=$9::jsonb,updated_at=now()
		 WHERE operation_id=$1 AND status='built' AND message_sha256=$2 AND signed_wire=$3
		 AND signed_wire_sha256=$4 AND transaction_signature=$5 AND recent_blockhash=$6
		 AND last_valid_block_height=$7 AND broadcast_intent_at IS NULL`,
			id, build.MessageSHA256, build.SignedWire, build.SignedWireSHA256, build.TransactionSignature,
			build.RecentBlockhash, build.LastValidBlockHeight, simulation.Slot, string(encoded))
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return budgetHold("setup_simulation_wire_mismatch")
		}
	} else {
		result, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET
	 status='built',message_sha256=$2,signed_wire=$3,signed_wire_sha256=$4,
	 transaction_signature=$5,recent_blockhash=$6,last_valid_block_height=$7,
	 updated_at=now()
	 WHERE operation_id=$1 AND status='decided' AND signed_wire IS NULL
	 AND signed_wire_sha256 IS NULL AND transaction_signature IS NULL AND broadcast_intent_at IS NULL`,
			id, build.MessageSHA256, build.SignedWire, build.SignedWireSHA256, build.TransactionSignature,
			build.RecentBlockhash, build.LastValidBlockHeight)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return budgetHold("setup_signed_persistence_lost_serialization")
		}
	}
	return nil
}

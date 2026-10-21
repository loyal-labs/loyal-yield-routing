package backyard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

func buildSimulatePersist(ctx context.Context, database *Database, rpc *chain.Client, manifest RouteManifest, operationID string, request any, effects ExpectedEffects, credentials Credentials) error {
	if database == nil || rpc == nil || operationID == "" {
		return fmt.Errorf("build runtime dependencies are required")
	}
	encoded, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		return err
	}
	message, blockhash, height, err := compileRequest(manifest, request)
	if err != nil {
		return err
	}
	signer, err := credentials.signer()
	if err != nil {
		return err
	}
	signed, err := signMessage(signer, mustKey(bridgeDelegate), message, blockhash, height)
	if err != nil {
		return err
	}
	start := time.Now()
	simulation, err := simulateSigned(ctx, rpc, signed.signedWire)
	logStage("build_simulate", start)
	if err != nil {
		return simulationRefused(err)
	}
	return database.PersistSigned(ctx, operationID, signed, simulation, encoded)
}

func simulationRefused(err error) error {
	if errors.Is(err, errConfirmedObservationUnavailable) {
		return err
	}
	return &BudgetHold{Reason: holdChainRefused, Details: map[string]string{"simulation": err.Error()}, cause: err}
}

func advanceNonterminalWithManifest(ctx context.Context, manifest RouteManifest, database *Database, rpc *chain.Client, operation PersistedOperation) error {
	if database == nil || rpc == nil || !IsNonterminal(operation.Status) {
		return fmt.Errorf("invalid nonterminal recovery input")
	}
	switch operation.Status {
	case Decided, Built, Simulated:
		return database.MarkPreBroadcastFailed(ctx, operation.ID, operation.Status, "prebroadcast_restart_reobserve_required")
	case Signed, BroadcastIntent:
		return database.land(ctx, rpc, operation)
	case Confirmed, Reconciling:
		return database.reconcile(ctx, manifest, rpc, operation)
	default:
		return fmt.Errorf("unsupported nonterminal status: %s", operation.Status)
	}
}

// land resends the persisted wire through chain.Land until it lands, fails on
// chain or its blockhash expires. The bytes are the row's, owned by their
// hash; a signed row records broadcast intent before its first send, and a
// row that already recorded it resends the same bytes, which cannot spend
// twice.
func (d *Database) land(ctx context.Context, rpc *chain.Client, operation PersistedOperation) error {
	wire, err := chain.OwnSignedWire(operation.SignedWire, operation.SignedWireSHA256)
	var signature []byte
	if err == nil {
		_, signature, err = splitSignedWire(wire.Bytes())
	}
	if err != nil || operation.LastValidBlockHeight <= 0 {
		return d.MarkManualRecovery(ctx, operation.ID, operation.Status, "incomplete_persisted_signed_wire")
	}
	out, err := chain.Land(ctx, rpc, chain.Attempt{
		Wire: wire.Bytes(), Signature: encodeBase58(signature),
		LastValidBlockHeight: uint64(operation.LastValidBlockHeight), Required: chain.Confirmed,
	}, chain.LandResendEvery, func(ctx context.Context) error {
		if operation.Status != Signed {
			return nil
		}
		if err := d.MarkBroadcastIntent(ctx, operation.ID); err != nil {
			return err
		}
		operation.Status = BroadcastIntent
		return nil
	})
	if err != nil {
		return unavailable(err)
	}
	switch {
	case out.Kind == chain.Expired:
		return d.MarkExpiredAbsentFailed(ctx, operation.ID, operation.Status)
	case operation.Status == Signed:
		return d.MarkManualRecovery(ctx, operation.ID, Signed, "signed_signature_found_before_broadcast_intent")
	case out.Kind == chain.Failed:
		return d.recoverConfirmedFailure(ctx, rpc, operation)
	}
	return d.MarkConfirmed(ctx, operation.ID, operation.Status, int64(out.Slot))
}

func (d *Database) reconcile(ctx context.Context, manifest RouteManifest, rpc *chain.Client, operation PersistedOperation) error {
	status, err := signatureStatus(ctx, rpc, operation.TransactionSignature)
	if err != nil {
		return err
	}
	if status.Failed {
		return d.MarkManualRecovery(ctx, operation.ID, operation.Status, "finalization_transaction_error")
	}
	if !status.Finalized {
		return nil
	}
	expected, err := DecodeExpectedEffects(manifest.vault, operation.ExpectedEffects)
	if err != nil {
		return d.MarkManualRecovery(ctx, operation.ID, operation.Status, "invalid_expected_effects")
	}
	var receipt ConfirmedTransactionEvidence
	if expected.Initialization != nil {
		receipt, err = manifest.observeFinalizedKaminoInitialization(ctx, rpc, *expected.Initialization, operation)
	} else {
		receipt, err = finalizedTransaction(ctx, rpc, operation.TransactionSignature)
	}
	if err != nil {
		return err
	}
	if receipt.Slot != operation.ConfirmedSlot {
		return d.MarkManualRecovery(ctx, operation.ID, operation.Status, "confirmed_transaction_slot_mismatch")
	}
	reconciliation, effects, err := ReconcileConfirmedTransaction(manifest.vault, expected, receipt)
	if err != nil {
		return d.MarkManualRecovery(ctx, operation.ID, operation.Status, "exact_effect_reconciliation_failed")
	}
	return d.MarkReconciled(ctx, operation.ID, reconciliation, effects)
}

func sha256Bytes(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func simulationJSON(simulation SimulationResult) (string, error) {
	encoded, err := json.Marshal(simulation)
	return string(encoded), err
}

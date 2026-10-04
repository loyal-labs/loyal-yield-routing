package fleetexec

import (
	"encoding/json"
	"fmt"
)

// legalTransition is the explicit durable state machine shared with the legacy
// Rust confirmer. Every edge carries its own proof requirement, enforced by
// the caller that builds the Advance: an expiry edge additionally requires the
// broadcast-count/absence-proof preconditions the legacy expiry advance gates
// on, and is only reached through expiryRecovery below.
func legalTransition(from, to SubmissionState) bool {
	switch from {
	case StateSigned:
		return to == StateSubmitted || to == StateConfirmed || to == StateEffectAmbiguous ||
			to == StateExpiryCheckPending || to == StateExpired || to == StateFailed
	case StateSubmitted:
		return to == StateConfirmed || to == StateEffectAmbiguous ||
			to == StateExpiryCheckPending || to == StateExpired || to == StateFailed
	case StateEffectAmbiguous:
		return to == StateEffectAmbiguous || to == StateConfirmed || to == StateExpiryCheckPending || to == StateExpired || to == StateFailed
	case StateExpiryCheckPending:
		return to == StateConfirmed || to == StateExpired || to == StateFailed
	case StateConfirmed:
		return to == StateReconciliationPending
	case StateReconciliationPending:
		return to == StateReconciled || to == StateFailed || to == StateReconciliationPending
	}
	return false
}

// EffectAnchors is the receipt contract persisted with the signed wire: exact
// pre->post raw token deltas per account and mint. Unknown is not zero; a
// missing anchor is a reconciliation failure, not a satisfied delta.
type EffectAnchors struct {
	SourceLiquidityVault string `json:"source_liquidity_vault"`
	TargetLiquidityVault string `json:"target_liquidity_vault"`
	Mint                 string `json:"mint"`
	Decimals             uint8  `json:"decimals"`
	// AmountRaw is the route amount that must leave the source liquidity
	// vault and arrive at the target liquidity vault in the same receipt.
	AmountRaw int64 `json:"amount_raw"`
}

func parseEffectAnchors(raw json.RawMessage) (EffectAnchors, error) {
	var anchors EffectAnchors
	if len(raw) == 0 {
		return anchors, fmt.Errorf("missing effect anchors")
	}
	if err := json.Unmarshal(raw, &anchors); err != nil {
		return anchors, fmt.Errorf("decode effect anchors: %w", err)
	}
	if anchors.SourceLiquidityVault == "" || anchors.TargetLiquidityVault == "" ||
		anchors.Mint == "" || anchors.AmountRaw <= 0 {
		return anchors, fmt.Errorf("incomplete effect anchors")
	}
	return anchors, nil
}

// BroadcastPlan decides what one tick may do with a claimed submission. It is
// pure so tests and the replay tool exercise the same decisions as the
// runtime. There is no general retry edge: an ambiguous broadcast is resolved
// only by its signature status or its protocol-specific no-effect proof.
type BroadcastPlan struct {
	// SendExactWire is true only for signed-but-never-broadcast routes.
	SendExactWire bool
	// CheckStatus resolves an already-broadcast uncertainty by signature.
	CheckStatus bool
	// AwaitConfirmation waits for required_commitment confirmation.
	AwaitConfirmation bool
	// ReconcileFinalized verifies the exact finalized receipt effects.
	ReconcileFinalized bool
}

func planFor(record SubmissionRecord) (BroadcastPlan, error) {
	switch record.State {
	case StateSigned:
		if record.BroadcastCount != 0 {
			// The durable broadcast intent was already counted: the send may
			// or may not have left this process. Resolve by signature; the
			// wire is never re-sent and never rebuilt.
			return BroadcastPlan{CheckStatus: true}, nil
		}
		return BroadcastPlan{SendExactWire: true}, nil
	case StateSubmitted, StateEffectAmbiguous:
		return BroadcastPlan{CheckStatus: true}, nil
	case StateConfirmed:
		return BroadcastPlan{AwaitConfirmation: true}, nil
	case StateReconciliationPending:
		return BroadcastPlan{ReconcileFinalized: true}, nil
	case StateExpiryCheckPending:
		return BroadcastPlan{CheckStatus: true}, nil
	}
	return BroadcastPlan{}, fmt.Errorf("%w: %s", ErrNotClaimable, record.State)
}

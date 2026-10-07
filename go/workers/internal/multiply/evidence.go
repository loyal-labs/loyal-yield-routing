package multiply

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"github.com/gagliardetto/solana-go"
)

// OperationPrestateEvidence supplements the unchanged Rust financial JSON.
// It is published atomically with signed bytes, never reconstructed after send.
type OperationPrestateEvidence struct {
	OperationID            string              `json:"operationId"`
	Signature              string              `json:"signature"`
	WireSHA256             string              `json:"wireSha256"`
	FinancialAnchorsSHA256 string              `json:"financialAnchorsSha256"`
	ObservedSlot           uint64              `json:"observedSlot"`
	TokenAmountsBefore     []TokenAmountBefore `json:"tokenAmountsBefore"`
	ObligationBefore       *ObligationBefore   `json:"obligationBefore,omitempty"`
}

func NewOperationPrestate(op *MultiplyOperation, before *ObservedRoute, topology *EarnMaxTopology) (*OperationPrestateEvidence, error) {
	if op == nil || before == nil || topology == nil || before.Slot == 0 {
		return nil, errors.New("operation prestate is missing")
	}
	effects := op.ExpectedEffects
	if len(effects.TokenDeltas) == 0 && effects.ObligationDelta == nil {
		return nil, errors.New("operation financial contract is missing")
	}
	seen := map[string]bool{}
	for _, anchor := range effects.TokenAmountsBefore {
		identity := anchor.Account + ":" + anchor.Mint
		if seen[identity] {
			return nil, errors.New("duplicate prestate anchor")
		}
		seen[identity] = true
		amount, known := observedTokenAmount(before, anchor.Account, anchor.Mint)
		if !known || amount != anchor.AmountRaw {
			return nil, errors.New("prestate token anchor disagrees with original read")
		}
	}
	for _, delta := range effects.TokenDeltas {
		if !seen[delta.Account+":"+delta.Mint] {
			return nil, errors.New("prestate token anchor is missing")
		}
	}
	if effects.ObligationDelta != nil {
		anchor := effects.ObligationBefore
		if anchor == nil || anchor.Obligation != effects.ObligationDelta.Obligation || anchor.DebtAmountSF == "" {
			return nil, errors.New("prestate obligation anchor is missing")
		}
		key, err := solana.PublicKeyFromBase58(anchor.Obligation)
		if err != nil {
			return nil, err
		}
		post := positionForObligation(before, key, topology)
		if post == nil || post.CollateralDepositedRaw != anchor.CollateralRaw || post.DebtRaw != anchor.DebtRaw || post.DebtAmountSF != anchor.DebtAmountSF {
			return nil, errors.New("prestate obligation anchor disagrees with original read")
		}
	}
	hash, err := financialAnchorsHash(effects)
	if err != nil {
		return nil, err
	}
	evidence := &OperationPrestateEvidence{OperationID: op.OperationID, FinancialAnchorsSHA256: hash, ObservedSlot: before.Slot, TokenAmountsBefore: append([]TokenAmountBefore(nil), effects.TokenAmountsBefore...)}
	if effects.ObligationBefore != nil {
		copy := *effects.ObligationBefore
		evidence.ObligationBefore = &copy
	}
	return evidence, nil
}

func validatePrestateIdentity(op *MultiplyOperation, prestate *OperationPrestateEvidence) error {
	if op == nil || prestate == nil || prestate.ObservedSlot == 0 || prestate.OperationID != op.OperationID {
		return errors.New("operation has no original prestate evidence")
	}
	hash, err := financialAnchorsHash(op.ExpectedEffects)
	if err != nil {
		return err
	}
	if hash != prestate.FinancialAnchorsSHA256 || op.TransactionSignature == nil || prestate.Signature != *op.TransactionSignature || op.SignedWireSHA256 == nil || prestate.WireSHA256 != *op.SignedWireSHA256 {
		return errors.New("prestate evidence identity drifted")
	}
	// The standalone financial JSON digest binds every anchor and effect; the
	// explicit side evidence must carry exactly those same original anchors.
	recorded, err := json.Marshal(struct {
		Tokens     []TokenAmountBefore
		Obligation *ObligationBefore
	}{prestate.TokenAmountsBefore, prestate.ObligationBefore})
	if err != nil {
		return err
	}
	original, err := json.Marshal(struct {
		Tokens     []TokenAmountBefore
		Obligation *ObligationBefore
	}{op.ExpectedEffects.TokenAmountsBefore, op.ExpectedEffects.ObligationBefore})
	if err != nil {
		return err
	}
	if string(recorded) != string(original) {
		return errors.New("prestate financial anchors drifted")
	}
	return nil
}

// ExpiredNoEffectEvidence is appended only after the immutable financial
// anchors have been proved unchanged and a historical signature query at a
// newer context reports absence. It records the original anchor digest.
type ExpiredNoEffectEvidence struct {
	OperationID            string              `json:"operationId"`
	Signature              string              `json:"signature"`
	WireSHA256             string              `json:"wireSha256"`
	FinancialAnchorsSHA256 string              `json:"financialAnchorsSha256"`
	LastValidBlockHeight   uint64              `json:"lastValidBlockHeight"`
	FinalizedHeight        uint64              `json:"finalizedHeight"`
	FinalizedSlot          uint64              `json:"finalizedSlot"`
	EffectSlot             uint64              `json:"effectSlot"`
	HistorySlot            uint64              `json:"historySlot"`
	FirstAvailableBlock    uint64              `json:"firstAvailableBlock"`
	TokenAmountsAfter      []TokenAmountBefore `json:"tokenAmountsAfter"`
	ObligationAfter        *ObligationBefore   `json:"obligationAfter,omitempty"`
}

type NoEffectProof struct{ evidence ExpiredNoEffectEvidence }

type expiryRPC interface {
	FinalizedBlockHeight(context.Context) (uint64, error)
	FinalizedSlot(context.Context) (uint64, error)
	HistoricalSignature(context.Context, string) (*SignatureObservation, uint64, error)
	FirstAvailableBlock(context.Context) (uint64, error)
}

// ExpiredFinalizedBoundary establishes irreversible expiry before the worker
// reads the effect accounts. A confirmed height cannot establish this boundary.
func (e *Executor) ExpiredFinalizedBoundary(ctx context.Context, op *MultiplyOperation) (uint64, error) {
	if _, err := PersistedTransaction(op); err != nil {
		return 0, err
	}
	if op.LastValidBlockHeight == nil {
		return 0, errors.New("operation omitted blockhash expiry")
	}
	rpc, ok := e.RPC.(expiryRPC)
	if !ok {
		return 0, errors.New("RPC has no finalized historical recovery capability")
	}
	height, err := rpc.FinalizedBlockHeight(ctx)
	if err != nil {
		return 0, err
	}
	if height <= *op.LastValidBlockHeight {
		return 0, errors.New("stored blockhash has not expired at finalized height")
	}
	slot, err := rpc.FinalizedSlot(ctx)
	if err != nil {
		return 0, err
	}
	if slot == 0 {
		return 0, errors.New("finalized slot is missing")
	}
	return slot, nil
}

// ProveExpiredNoEffect is an intentional stricter correction to the Rust
// cache/height-only expiry path. No state transition is justified by current
// balances alone: historical signature absence is rechecked after the complete
// effect read, at a context at least as new as that read, after finalized expiry.
func (e *Executor) ProveExpiredNoEffect(ctx context.Context, op *MultiplyOperation, after *ObservedRoute, topology *EarnMaxTopology, boundarySlot uint64, prestate *OperationPrestateEvidence) (*NoEffectProof, error) {
	if _, err := PersistedTransaction(op); err != nil {
		return nil, err
	}
	if after == nil || topology == nil || boundarySlot == 0 || after.Slot < boundarySlot {
		return nil, errors.New("effect snapshot predates finalized expiry boundary")
	}
	rpc, ok := e.RPC.(expiryRPC)
	if !ok {
		return nil, errors.New("RPC has no historical recovery capability")
	}
	height, err := rpc.FinalizedBlockHeight(ctx)
	if err != nil {
		return nil, err
	}
	finalizedSlot, err := rpc.FinalizedSlot(ctx)
	if err != nil {
		return nil, err
	}
	if finalizedSlot < boundarySlot || after.Slot < finalizedSlot {
		return nil, errors.New("effect read predates live finalized expiry slot")
	}
	boundarySlot = finalizedSlot
	if op.LastValidBlockHeight == nil || height <= *op.LastValidBlockHeight {
		return nil, errors.New("blockhash expiry is not finalized")
	}
	anchors := op.ExpectedEffects
	if err := validatePrestateIdentity(op, prestate); err != nil {
		return nil, err
	}
	if prestate == nil || prestate.ObservedSlot == 0 {
		return nil, errors.New("legacy operation omitted immutable observation slot; absence cannot be proved")
	}

	if len(anchors.TokenDeltas) == 0 && anchors.ObligationDelta == nil {
		return nil, errors.New("operation has no financial effect contract")
	}
	tokenAfter := make([]TokenAmountBefore, 0, len(anchors.TokenAmountsBefore))
	seen := map[string]bool{}
	for _, before := range anchors.TokenAmountsBefore {
		identity := before.Account + ":" + before.Mint
		if seen[identity] {
			return nil, errors.New("duplicate financial anchor")
		}
		seen[identity] = true
		amount, known := observedTokenAmount(after, before.Account, before.Mint)
		if !known || amount != before.AmountRaw {
			return nil, errors.New("token effect absence is not proved")
		}
		tokenAfter = append(tokenAfter, TokenAmountBefore{Account: before.Account, Mint: before.Mint, AmountRaw: amount})
	}
	for _, delta := range anchors.TokenDeltas {
		if !seen[delta.Account+":"+delta.Mint] {
			return nil, errors.New("token effect omitted immutable pre-state")
		}
	}
	var obligationAfter *ObligationBefore
	if anchors.ObligationDelta != nil {
		before := anchors.ObligationBefore
		if before == nil || before.Obligation != anchors.ObligationDelta.Obligation || before.DebtAmountSF == "" {
			return nil, errors.New("obligation effect omitted immutable pre-state")
		}
		key, err := solana.PublicKeyFromBase58(before.Obligation)
		if err != nil {
			return nil, err
		}
		position := positionForObligation(after, key, topology)
		if position == nil || position.CollateralDepositedRaw != before.CollateralRaw || position.DebtRaw != before.DebtRaw || position.DebtAmountSF != before.DebtAmountSF {
			return nil, errors.New("obligation effect absence is not proved")
		}
		copy := *before
		obligationAfter = &copy
	}
	firstAvailable, err := rpc.FirstAvailableBlock(ctx)
	if err != nil {
		return nil, err
	}
	if firstAvailable > prestate.ObservedSlot {
		return nil, errors.New("historical signature range was pruned")
	}
	observation, historySlot, err := rpc.HistoricalSignature(ctx, *op.TransactionSignature)
	if err != nil {
		return nil, err
	}
	if observation != nil || historySlot < after.Slot {
		return nil, errors.New("historical signature absence is not proved after effect read")
	}
	digest, err := financialAnchorsHash(anchors)
	if err != nil {
		return nil, err
	}
	return &NoEffectProof{evidence: ExpiredNoEffectEvidence{OperationID: op.OperationID, Signature: *op.TransactionSignature, WireSHA256: *op.SignedWireSHA256, FinancialAnchorsSHA256: digest, LastValidBlockHeight: *op.LastValidBlockHeight, FinalizedHeight: height, FinalizedSlot: boundarySlot, EffectSlot: after.Slot, HistorySlot: historySlot, FirstAvailableBlock: firstAvailable, TokenAmountsAfter: tokenAfter, ObligationAfter: obligationAfter}}, nil
}

// financialAnchorsHash digests the anchors in a fixed shape of their own (the
// one persisted prestate evidence was first hashed with: absent obligation
// fields omitted), so the row encoding of ExpectedEffects can match Rust
// without invalidating evidence already published.
func financialAnchorsHash(effects ExpectedEffects) (string, error) {
	anchors := struct {
		TokenAmountsBefore []TokenAmountBefore `json:"tokenAmountsBefore"`
		TokenDeltas        []TokenDelta        `json:"tokenDeltas"`
		ObligationBefore   *ObligationBefore   `json:"obligationBefore,omitempty"`
		ObligationDelta    *ObligationDelta    `json:"obligationDelta,omitempty"`
	}{effects.TokenAmountsBefore, effects.TokenDeltas, effects.ObligationBefore, effects.ObligationDelta}
	if anchors.TokenAmountsBefore == nil {
		anchors.TokenAmountsBefore = []TokenAmountBefore{}
	}
	if anchors.TokenDeltas == nil {
		anchors.TokenDeltas = []TokenDelta{}
	}
	raw, err := json.Marshal(anchors)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hexEncode(digest[:]), nil
}

func (c *LiveRPCSurface) FinalizedBlockHeight(ctx context.Context) (uint64, error) {
	var height uint64
	err := c.call(ctx, "getBlockHeight", []any{map[string]string{"commitment": "finalized"}}, &height)
	return height, err
}
func (c *LiveRPCSurface) FinalizedSlot(ctx context.Context) (uint64, error) {
	var slot uint64
	err := c.call(ctx, "getSlot", []any{map[string]string{"commitment": "finalized"}}, &slot)
	return slot, err
}
func (c *LiveRPCSurface) HistoricalSignature(ctx context.Context, signature string) (*SignatureObservation, uint64, error) {
	var raw struct {
		Context struct {
			Slot uint64 `json:"slot"`
		} `json:"context"`
		Value []*struct {
			Slot               int64            `json:"slot"`
			ConfirmationStatus string           `json:"confirmationStatus"`
			Err                *json.RawMessage `json:"err"`
		} `json:"value"`
	}
	if err := c.call(ctx, "getSignatureStatuses", []any{[]string{signature}, map[string]bool{"searchTransactionHistory": true}}, &raw); err != nil {
		return nil, 0, err
	}
	if raw.Context.Slot == 0 || len(raw.Value) != 1 {
		return nil, 0, errors.New("historical signature response is incomplete")
	}
	if raw.Value[0] == nil {
		return nil, raw.Context.Slot, nil
	}
	value := raw.Value[0]
	if value.Slot <= 0 {
		return nil, 0, errors.New("historical signature slot is invalid")
	}
	observed := &SignatureObservation{Slot: value.Slot, ConfirmationState: value.ConfirmationStatus}
	if value.Err != nil {
		message := string(*value.Err)
		observed.Err = &message
	}
	return observed, raw.Context.Slot, nil
}

func (c *LiveRPCSurface) FirstAvailableBlock(ctx context.Context) (uint64, error) {
	var slot uint64
	err := c.call(ctx, "getFirstAvailableBlock", nil, &slot)
	return slot, err
}

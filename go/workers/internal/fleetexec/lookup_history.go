package fleetexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	sdk "github.com/gagliardetto/solana-go"
)

const lookupHistoryMaximumBlocks = 256

// Finalized parent links cover produced blocks even when numeric slots were
// skipped. The bounded proof holds if history is pruned or the landing window
// is too old. An absent signature index alone never supplies this evidence.
type lookupHistoryBlock struct {
	Slot              int64  `json:"slot"`
	ParentSlot        int64  `json:"parent_slot"`
	Blockhash         string `json:"blockhash"`
	PreviousBlockhash string `json:"previous_blockhash"`
	SignatureCount    int    `json:"signature_count"`
	SignaturesSHA256  string `json:"signatures_sha256"`
}
type lookupHistory struct {
	firstSlot, lastSlot, blockHeight int64
	blocks                           []lookupHistoryBlock
}

func (r *LookupRPC) lookupHistory(ctx context.Context, attempt LookupAttempt, snapshot LookupSnapshot) (*lookupHistory, error) {
	if attempt.SigningContextSlot <= 0 || snapshot.Slot < attempt.SigningContextSlot || snapshot.Slot-attempt.SigningContextSlot > 100000 {
		return nil, errors.New("lookup original landing window is unavailable or too wide")
	}
	out := &lookupHistory{firstSlot: attempt.SigningContextSlot, lastSlot: snapshot.Slot}
	slot := snapshot.Slot
	var expectedHash string
	for len(out.blocks) < lookupHistoryMaximumBlocks {
		var block *struct {
			Blockhash         string    `json:"blockhash"`
			PreviousBlockhash string    `json:"previousBlockhash"`
			ParentSlot        *int64    `json:"parentSlot"`
			Height            *int64    `json:"blockHeight"`
			Signatures        *[]string `json:"signatures"`
		}
		if err := r.call(ctx, &block, "getBlock", slot, map[string]any{"commitment": "finalized", "transactionDetails": "signatures", "rewards": false, "maxSupportedTransactionVersion": 0}); err != nil {
			return nil, err
		}
		if block == nil || block.ParentSlot == nil || *block.ParentSlot < 0 || *block.ParentSlot >= slot || block.Signatures == nil || len(*block.Signatures) > 100000 {
			return nil, errors.New("lookup finalized block history is incomplete")
		}
		if _, err := sdk.HashFromBase58(block.Blockhash); err != nil {
			return nil, errors.New("lookup finalized block hash missing")
		}
		if _, err := sdk.HashFromBase58(block.PreviousBlockhash); err != nil {
			return nil, errors.New("lookup finalized parent hash missing")
		}
		if expectedHash != "" && expectedHash != block.Blockhash {
			return nil, errors.New("lookup finalized block ancestry is inconsistent")
		}
		if len(out.blocks) == 0 {
			if block.Height == nil || *block.Height <= attempt.Wire.LastValidBlockHeight {
				return nil, errors.New("lookup finalized history has not passed packet expiry")
			}
			out.blockHeight = *block.Height
		}
		seen := make(map[string]bool, len(*block.Signatures))
		for _, sig := range *block.Signatures {
			if _, err := sdk.SignatureFromBase58(sig); err != nil || seen[sig] {
				return nil, errors.New("lookup block signature list is malformed")
			}
			seen[sig] = true
			if sig == attempt.Wire.TransactionSignature {
				return nil, errors.New("lookup owned signature exists in finalized block history")
			}
		}
		signatureBytes, _ := json.Marshal(block.Signatures)
		signatureHash := sha256.Sum256(signatureBytes)
		out.blocks = append(out.blocks, lookupHistoryBlock{Slot: slot, ParentSlot: *block.ParentSlot, Blockhash: block.Blockhash, PreviousBlockhash: block.PreviousBlockhash, SignatureCount: len(*block.Signatures), SignaturesSHA256: hex.EncodeToString(signatureHash[:])})
		if slot == attempt.SigningContextSlot {
			return out, nil
		}
		if *block.ParentSlot < attempt.SigningContextSlot {
			return nil, errors.New("lookup signing bank is absent from finalized ancestry")
		}
		expectedHash, slot = block.PreviousBlockhash, *block.ParentSlot
	}
	return nil, errors.New("lookup finalized landing window exceeds bounded history proof")
}
func expireLookup(attempt LookupAttempt, status SignatureStatus, snapshot LookupSnapshot, history *lookupHistory) (*lookupProof, error) {
	if err := proveLookupWire(attempt.Intent, attempt.Wire); err != nil {
		return nil, err
	}
	if status.Found || status.BlockHeight <= attempt.Wire.LastValidBlockHeight || snapshot.Address != attempt.Intent.TableAddress || !lookupUnchanged(attempt.Intent, snapshot) {
		return nil, errors.New("lookup expiry lacks signature absence and unchanged account proof")
	}
	if history == nil || history.firstSlot != attempt.SigningContextSlot || history.lastSlot != snapshot.Slot || len(history.blocks) == 0 || history.blocks[0].Slot != snapshot.Slot || history.blocks[len(history.blocks)-1].Slot != attempt.SigningContextSlot || history.blockHeight <= attempt.Wire.LastValidBlockHeight || status.BlockHeight < history.blockHeight {
		return nil, errors.New("lookup expiry lacks complete finalized landing-window history")
	}
	var evidence map[string]json.RawMessage
	if err := json.Unmarshal(lookupReadback(snapshot, "expired_exact_no_effect"), &evidence); err != nil {
		return nil, err
	}
	evidence["history_first_slot"], _ = json.Marshal(history.firstSlot)
	evidence["history_last_slot"], _ = json.Marshal(history.lastSlot)
	evidence["history_produced_blocks"], _ = json.Marshal(history.blocks)
	readback, err := json.Marshal(evidence)
	if err != nil {
		return nil, err
	}
	return &lookupProof{binding: lookupProofBinding(attempt), state: LookupExpired, readbackSlot: snapshot.Slot, historySlot: history.lastSlot, blockHeight: history.blockHeight, historyComplete: true, readback: readback}, nil
}

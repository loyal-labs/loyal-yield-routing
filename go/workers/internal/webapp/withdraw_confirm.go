package webapp

import (
	"errors"
	"math/big"
	"strconv"
	"time"
)

// POST /api/smart-accounts/yield-optimization/withdrawals/confirm ports
// web/apps/web/src/app/api/smart-accounts/yield-optimization/withdrawals/confirm/route.ts
// plus verifyConfirmedEarnWithdrawal (earn-withdraw-confirm.server.ts:564-759):
// after the client sends the withdrawal from withdrawals/prepare, it confirms
// the signature here. The route verifies the chain evidence and returns the
// verified position; it never writes (single-writer rule,
// web/apps/web/scripts/verify-earn-confirm-single-writer.ts), so a repeat run
// answers with the same shape and a crash mid-way leaves no state.
const earnWithdrawConfirmPath = "/api/smart-accounts/yield-optimization/withdrawals/confirm"

// EarnWithdrawConfirmRequest documents the request body for OpenAPI; the
// handler parses the raw bytes so the TS error messages carry over verbatim.
type EarnWithdrawConfirmRequest struct {
	AccountingReserve          *string         `json:"accountingReserve,omitempty"`
	AutodepositClose           *struct{}       `json:"autodepositClose,omitempty"`
	Cluster                    *string         `json:"cluster,omitempty"`
	ConfirmedSlot              *string         `json:"confirmedSlot,omitempty"`
	DelegatedSigner            *string         `json:"delegatedSigner,omitempty"`
	ExecutionReserve           *string         `json:"executionReserve,omitempty"`
	IsFinalStep                *bool           `json:"isFinalStep,omitempty"`
	LiquidityMint              *string         `json:"liquidityMint,omitempty"`
	Market                     *string         `json:"market,omitempty"`
	Mode                       *string         `json:"mode,omitempty"`
	PolicyAccount              *string         `json:"policyAccount,omitempty"`
	PolicyID                   *string         `json:"policyId,omitempty"`
	PolicySeed                 *string         `json:"policySeed,omitempty"`
	RequestedWithdrawAmountRaw *string         `json:"requestedWithdrawAmountRaw,omitempty"`
	ReserveWithdrawals         *[]struct{}     `json:"reserveWithdrawals,omitempty"`
	Settings                   *string         `json:"settings,omitempty"`
	SetupPolicyAccount         *string         `json:"setupPolicyAccount,omitempty"`
	SetupPolicyID              *string         `json:"setupPolicyId,omitempty"`
	SetupPolicySeed            *string         `json:"setupPolicySeed,omitempty"`
	SmartAccountAddress        *string         `json:"smartAccountAddress,omitempty"`
	SourceAmountRaw            *string         `json:"sourceAmountRaw,omitempty"`
	SourceID                   *string         `json:"sourceId,omitempty"`
	SourceMetadata             *map[string]any `json:"sourceMetadata,omitempty"`
	SourceMint                 *string         `json:"sourceMint,omitempty"`
	SourceTokenAccount         *string         `json:"sourceTokenAccount,omitempty"`
	SourceType                 *string         `json:"sourceType,omitempty"`
	StepCount                  *int            `json:"stepCount,omitempty"`
	StepIndex                  *int            `json:"stepIndex,omitempty"`
	TargetReserve              *string         `json:"targetReserve,omitempty"`
	VaultIndex                 *int            `json:"vaultIndex,omitempty"`
	VaultPubkey                *string         `json:"vaultPubkey,omitempty"`
	WalletAddress              *string         `json:"walletAddress,omitempty"`
	WithdrawalSignature        *string         `json:"withdrawalSignature,omitempty"`
	WithdrawnAmountRaw         *string         `json:"withdrawnAmountRaw,omitempty"`
}

// EarnWithdrawConfirmPosition is serializeVerifiedWithdrawPosition
// (earn-withdraw-confirm.server.ts:497-532): the read-model row the
// LaserStream projection would hold for the withdrawal, with ids and slots as
// decimal strings and market null on idle withdrawals.
type EarnWithdrawConfirmPosition struct {
	CurrentHolding        EarnWithdrawConfirmedHolding `json:"currentHolding"`
	ID                    string                       `json:"id"`
	InitialHolding        EarnWithdrawConfirmedInitial `json:"initialHolding"`
	CurrentTotalAmountRaw string                       `json:"currentTotalAmountRaw"`
	PrincipalAmountRaw    string                       `json:"principalAmountRaw"`
	Status                string                       `json:"status"`
}

type EarnWithdrawConfirmedHolding struct {
	AmountRaw     string                          `json:"amountRaw"`
	LiquidityMint string                          `json:"liquidityMint"`
	Market        *string                         `json:"market"`
	ObservedAt    string                          `json:"observedAt"`
	ObservedSlot  string                          `json:"observedSlot"`
	Provenance    EarnWithdrawConfirmedProvenance `json:"provenance"`
	Reserve       string                          `json:"reserve"`
}

type EarnWithdrawConfirmedProvenance struct {
	LastHoldingEventID      *string `json:"lastHoldingEventId"`
	LastRebalanceDecisionID *string `json:"lastRebalanceDecisionId"`
}

type EarnWithdrawConfirmedInitial struct {
	LiquidityMint string  `json:"liquidityMint"`
	Market        *string `json:"market"`
	Reserve       string  `json:"reserve"`
	SupplyApyBps  *string `json:"supplyApyBps"`
}

// EarnWithdrawConfirmRemainingHolding is the response's remainingHoldings entry
// (:727-733): the five fields the route maps out of the holdings snapshot.
type EarnWithdrawConfirmRemainingHolding struct {
	AmountRaw     string  `json:"amountRaw"`
	Kind          string  `json:"kind"`
	LiquidityMint string  `json:"liquidityMint"`
	Market        *string `json:"market"`
	Reserve       *string `json:"reserve"`
}

// EarnWithdrawConfirmResponse is EarnWithdrawConfirmationResult
// (earn-withdraw-confirm.server.ts:539-554). The zero proof's blocking entries
// serialize as they are, tokenProgramId included (:725), so the field names
// come from the Next runtime objects, not the wider result type comment.
type EarnWithdrawConfirmResponse struct {
	BlockingTokenAccounts []withdrawBlockingTokenAccount        `json:"blockingTokenAccounts"`
	Position              EarnWithdrawConfirmPosition           `json:"position"`
	RemainingHoldings     []EarnWithdrawConfirmRemainingHolding `json:"remainingHoldings"`
	Status                string                                `json:"status"`
}

// withdrawVerifiedPosition ports serializeVerifiedWithdrawPosition
// (:497-532): a full final withdrawal empties the holding; a partial one
// leaves source minus what the transaction actually transferred.
func withdrawVerifiedPosition(in withdrawCanonicalInput, verifiedAt time.Time) EarnWithdrawConfirmPosition {
	sourceAmountRaw := in.SourceAmountRaw
	if sourceAmountRaw == nil {
		sourceAmountRaw = in.WithdrawnAmountRaw
	}
	current := new(big.Int)
	if in.Mode != "full" || (in.IsFinalStep != nil && !*in.IsFinalStep) {
		if sourceAmountRaw.Cmp(in.WithdrawnAmountRaw) > 0 {
			current.Sub(sourceAmountRaw, in.WithdrawnAmountRaw)
		}
	}
	currentRaw := current.String()
	position := EarnWithdrawConfirmPosition{
		CurrentHolding: EarnWithdrawConfirmedHolding{
			AmountRaw:     currentRaw,
			LiquidityMint: in.LiquidityMint,
			Market:        in.Market,
			ObservedAt:    verifiedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			ObservedSlot:  in.ConfirmedSlot.String(),
			Provenance:    EarnWithdrawConfirmedProvenance{},
			Reserve:       in.TargetReserve,
		},
		ID:                    in.WithdrawalSignature,
		InitialHolding:        EarnWithdrawConfirmedInitial{LiquidityMint: in.LiquidityMint, Market: in.Market, Reserve: in.TargetReserve},
		CurrentTotalAmountRaw: currentRaw,
		PrincipalAmountRaw:    currentRaw,
		Status:                "active",
	}
	return position
}

// withdrawSafeContextSlot ports toSafeContextSlot (:556-562): the zero proof
// demands the slot as a safe RPC integer.
func withdrawSafeContextSlot(slot *big.Int) (uint64, error) {
	if !slot.IsUint64() || slot.Cmp(new(big.Int).SetUint64(1<<53-1)) > 0 {
		return 0, errors.New("Confirmed withdrawal slot is outside the safe RPC range.")
	}
	return slot.Uint64(), nil
}

// withdrawFullExitResponse serializes the zero proof the way :724-735 does:
// the proof's blocking entries verbatim, its holdings mapped to the five
// response fields, nil slices as empty arrays.
func withdrawFullExitResponse(proof withdrawFullExitZeroProof, position EarnWithdrawConfirmPosition) *EarnWithdrawConfirmResponse {
	blocking := proof.BlockingTokenAccounts
	if blocking == nil {
		blocking = []withdrawBlockingTokenAccount{}
	}
	remaining := make([]EarnWithdrawConfirmRemainingHolding, 0, len(proof.RemainingHoldings))
	for _, holding := range proof.RemainingHoldings {
		entry := EarnWithdrawConfirmRemainingHolding{
			AmountRaw:     strconv.FormatUint(holding.AmountRaw, 10),
			Kind:          holding.Kind,
			LiquidityMint: holding.LiquidityMint.String(),
		}
		if holding.Market != nil {
			text := holding.Market.String()
			entry.Market = &text
		}
		if holding.Reserve != nil {
			text := holding.Reserve.String()
			entry.Reserve = &text
		}
		remaining = append(remaining, entry)
	}
	return &EarnWithdrawConfirmResponse{
		BlockingTokenAccounts: blocking,
		Position:              position,
		RemainingHoldings:     remaining,
		Status:                proof.Status,
	}
}

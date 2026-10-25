package webapp

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// POST /api/smart-accounts/yield-optimization/withdrawals/confirm ports
// web/apps/web/src/app/api/smart-accounts/yield-optimization/withdrawals/confirm/route.ts
// plus verifyConfirmedEarnWithdrawal:
// after the client sends the withdrawal from withdrawals/prepare, it confirms
// the signature here. The route verifies the chain evidence and returns the
// verified position; it never writes (single-writer rule,
// web/apps/web/scripts/verify-earn-confirm-single-writer.ts), so a repeat run
// answers with the same shape and a crash mid-way leaves no state.
const earnWithdrawConfirmPath = "/api/smart-accounts/yield-optimization/withdrawals/confirm"

// EarnWithdrawConfirmRequest documents the request body for OpenAPI; the
// handler parses the raw bytes so the TS error messages carry over verbatim.
type EarnWithdrawConfirmRequest struct {
	AccountingReserve          *string                                 `json:"accountingReserve,omitempty"`
	AutodepositClose           *struct{}                               `json:"autodepositClose,omitempty"`
	Cluster                    *string                                 `json:"cluster,omitempty"`
	ConfirmedSlot              *string                                 `json:"confirmedSlot,omitempty"`
	DelegatedSigner            *string                                 `json:"delegatedSigner,omitempty"`
	ExecutionReserve           *string                                 `json:"executionReserve,omitempty"`
	IsFinalStep                *bool                                   `json:"isFinalStep,omitempty"`
	LiquidityMint              *string                                 `json:"liquidityMint,omitempty"`
	Market                     *string                                 `json:"market,omitempty"`
	Mode                       *string                                 `json:"mode,omitempty"`
	PolicyAccount              *string                                 `json:"policyAccount,omitempty"`
	PolicyID                   *string                                 `json:"policyId,omitempty"`
	PolicySeed                 *string                                 `json:"policySeed,omitempty"`
	RequestedWithdrawAmountRaw *string                                 `json:"requestedWithdrawAmountRaw,omitempty"`
	ReserveWithdrawals         *[]EarnWithdrawConfirmReserveWithdrawal `json:"reserveWithdrawals,omitempty"`
	Settings                   *string                                 `json:"settings,omitempty"`
	SetupPolicyAccount         *string                                 `json:"setupPolicyAccount,omitempty"`
	SetupPolicyID              *string                                 `json:"setupPolicyId,omitempty"`
	SetupPolicySeed            *string                                 `json:"setupPolicySeed,omitempty"`
	SmartAccountAddress        *string                                 `json:"smartAccountAddress,omitempty"`
	SourceAmountRaw            *string                                 `json:"sourceAmountRaw,omitempty"`
	SourceID                   *string                                 `json:"sourceId,omitempty"`
	SourceMetadata             *map[string]any                         `json:"sourceMetadata,omitempty"`
	SourceMint                 *string                                 `json:"sourceMint,omitempty"`
	SourceTokenAccount         *string                                 `json:"sourceTokenAccount,omitempty"`
	SourceType                 *string                                 `json:"sourceType,omitempty"`
	StepCount                  *int                                    `json:"stepCount,omitempty"`
	StepIndex                  *int                                    `json:"stepIndex,omitempty"`
	TargetReserve              *string                                 `json:"targetReserve,omitempty"`
	VaultIndex                 *int                                    `json:"vaultIndex,omitempty"`
	VaultPubkey                *string                                 `json:"vaultPubkey,omitempty"`
	WalletAddress              *string                                 `json:"walletAddress,omitempty"`
	WithdrawalSignature        *string                                 `json:"withdrawalSignature,omitempty"`
	WithdrawnAmountRaw         *string                                 `json:"withdrawnAmountRaw,omitempty"`
}

// EarnWithdrawConfirmPosition is serializeVerifiedWithdrawPosition
// : the read-model row the
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
// . The zero proof's blocking entries
// serialize as they are, tokenProgramId included (:725), so the field names
// come from the Next runtime objects, not the wider result type comment.
type EarnWithdrawConfirmResponse struct {
	BlockingTokenAccounts []withdrawBlockingTokenAccount        `json:"blockingTokenAccounts"`
	Position              EarnWithdrawConfirmPosition           `json:"position"`
	RemainingHoldings     []EarnWithdrawConfirmRemainingHolding `json:"remainingHoldings"`
	Status                string                                `json:"status"`
}

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

func withdrawSafeContextSlot(slot *big.Int) (uint64, error) {
	if !slot.IsUint64() || slot.Cmp(new(big.Int).SetUint64(1<<53-1)) > 0 {
		return 0, errors.New("Confirmed withdrawal slot is outside the safe RPC range.")
	}
	return slot.Uint64(), nil
}

func withdrawFullExitResponse(proof withdrawFullExitZeroProof, position EarnWithdrawConfirmPosition) *EarnWithdrawConfirmResponse {
	blocking := proof.BlockingTokenAccounts
	if blocking == nil {
		blocking = []withdrawBlockingTokenAccount{}
	}
	remaining := make([]EarnWithdrawConfirmRemainingHolding, 0, len(proof.RemainingHoldings))
	for _, holding := range proof.RemainingHoldings {
		entry := EarnWithdrawConfirmRemainingHolding{
			AmountRaw:     strconv.FormatUint(holding.AmountRaw, 10),
			Kind:          string(holding.Kind),
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

type earnWithdrawConfirmInput struct {
	SessionInput
	Raw rawJSON
}

type earnWithdrawConfirmOutput struct {
	Body EarnWithdrawConfirmResponse
}

func (h earnRoutes) registerWithdrawConfirm(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-earn-withdraw-confirm",
		Method:      http.MethodPost,
		Path:        earnWithdrawConfirmPath,
		Summary:     "Confirm an Earn withdrawal transaction",
		Responses:   errorResponses[routeError](api, 400, 401, 403, 500, 503),
	}, h.earnWithdrawConfirm)
	documentJSONBody[EarnWithdrawConfirmRequest](api, earnWithdrawConfirmPath)
}

func (h earnRoutes) earnWithdrawConfirm(ctx context.Context, in *earnWithdrawConfirmInput) (*earnWithdrawConfirmOutput, error) {
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		if refused, ok := authRefusal(err); ok {
			return nil, refused
		}
		return nil, earnUnavailable("withdraw confirm principal", err, "withdraw_confirm_unavailable", "The Earn withdrawal confirm is temporarily unavailable.")
	}
	body, err := parseWithdrawConfirmBody(in.Raw.data)
	if err != nil {
		var envelope *routeError
		if errors.As(err, &envelope) {
			return nil, envelope
		}
		return nil, earnError(http.StatusBadRequest, "invalid_request", err.Error())
	}
	response, err := withdrawConfirmVerify(ctx, h.Chain, account, body, time.Now)
	if err != nil {
		var envelope *routeError
		if errors.As(err, &envelope) {
			return nil, envelope
		}
		return nil, earnError(http.StatusInternalServerError, "record_failed", confirmErrorText(err, "Confirmed yield withdrawal could not be recorded."))
	}
	return &earnWithdrawConfirmOutput{Body: *response}, nil
}

func withdrawConfirmVerify(ctx context.Context, client withdrawConfirmChain, account EarnAccount, body withdrawConfirmInput, now func() time.Time) (*EarnWithdrawConfirmResponse, error) {
	if body.WalletAddress != account.Wallet.String() || body.Settings != account.Settings.String() {
		return nil, earnError(http.StatusForbidden, "principal_mismatch", "Confirmed yield withdrawal does not match the authenticated wallet.")
	}
	canonical, err := createWithdrawCanonicalInput(body)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "metadata_mismatch", confirmErrorText(err, "Confirmed yield withdrawal metadata is invalid."))
	}
	if canonical.Cluster != string(account.Cluster) {
		return nil, earnError(http.StatusBadRequest, "cluster_mismatch", "Confirmed yield withdrawal cluster does not match the configured Solana environment.")
	}
	slot, err := withdrawConfirmedSignatureSlot(ctx, client, withdrawSleep, "withdrawal", canonical.WithdrawalSignature)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "unconfirmed_signature", confirmErrorText(err, "Withdrawal transaction is not confirmed."))
	}
	if canonical.ConfirmedSlot.Cmp(slot) != 0 {
		// The client-supplied slot can be its confirmation context slot
		// (RPC-lag fallback), not the landing slot; the server-resolved status
		// slot is canonical (:635-646). Rejecting here would strand the
		// withdrawal out of the read-model.
		slog.Warn("[earn-withdraw-confirm] corrected client slot",
			"clientSlot", canonical.ConfirmedSlot.String(),
			"resolvedSlot", slot.String(),
			"settings", canonical.Settings,
			"walletAddress", canonical.WalletAddress,
			"withdrawalSignature", canonical.WithdrawalSignature)
		canonical.ConfirmedSlot = slot
	}
	if err := withdrawTransactionProof(ctx, client, withdrawSleep, canonical.Cluster, &canonical); err != nil {
		return nil, earnError(http.StatusBadRequest, "invalid_transaction_proof", confirmErrorText(err, "Confirmed withdrawal transaction proof is invalid."))
	}

	position := withdrawVerifiedPosition(canonical, now())
	if canonical.Mode != "full" {
		return &EarnWithdrawConfirmResponse{
			BlockingTokenAccounts: []withdrawBlockingTokenAccount{},
			Position:              position,
			RemainingHoldings:     []EarnWithdrawConfirmRemainingHolding{},
			Status:                "withdrawal_recorded",
		}, nil
	}
	if canonical.IsFinalStep != nil && !*canonical.IsFinalStep {
		return &EarnWithdrawConfirmResponse{
			BlockingTokenAccounts: []withdrawBlockingTokenAccount{},
			Position:              position,
			RemainingHoldings:     []EarnWithdrawConfirmRemainingHolding{},
			Status:                "full_exit_incomplete",
		}, nil
	}

	minContextSlot, err := withdrawSafeContextSlot(canonical.ConfirmedSlot)
	if err == nil {
		var proof withdrawFullExitZeroProof
		proof, err = verifyWithdrawFullExitZeroBalances(ctx, client, withdrawSleep, now, canonical.Cluster, canonical, minContextSlot)
		if err == nil {
			return withdrawFullExitResponse(proof, position), nil
		}
	}
	return nil, earnError(http.StatusServiceUnavailable, "full_exit_verification_retryable", confirmErrorText(err, "Post-withdraw balance verification failed."))
}

// EarnWithdrawConfirmReserveWithdrawal documents one reserveWithdrawals entry
// of the confirm body: the SDK's SmartAccountEarnUsdcReserveWithdrawalMetadata
// , echoed from prepare.
type EarnWithdrawConfirmReserveWithdrawal struct {
	AccountingReserve       string  `json:"accountingReserve"`
	CollateralAta           string  `json:"collateralAta"`
	ExecutionMarket         string  `json:"executionMarket"`
	ExecutionReserve        string  `json:"executionReserve"`
	KaminoWithdrawAmountRaw string  `json:"kaminoWithdrawAmountRaw"`
	LiquidityMint           string  `json:"liquidityMint"`
	Market                  *string `json:"market"`
	Reserve                 string  `json:"reserve"`
	WithdrawnAmountRaw      string  `json:"withdrawnAmountRaw"`
}

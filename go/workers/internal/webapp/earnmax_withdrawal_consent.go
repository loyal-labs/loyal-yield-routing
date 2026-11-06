package webapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/releasecontrol"
)

const withdrawalConsentPath = "/api/smart-accounts/earn-max/withdrawal-consent"

type WithdrawalClaimAttempts interface {
	ExistingExact(context.Context, earnmax.WithdrawalConsent, []byte) (string, bool, error)
	Reserve(context.Context, earnmax.WithdrawalConsent, []byte, string, string, uint64, uint64) (string, error)
}

type withdrawalClaimAttempt struct {
	Consent              earnmax.WithdrawalConsent `json:"consent"`
	WireBase64           string                    `json:"wireBase64"`
	Signature            string                    `json:"signature"`
	Blockhash            string                    `json:"blockhash"`
	LastValidBlockHeight uint64                    `json:"lastValidBlockHeight"`
	FeeLamports          string                    `json:"feeLamports"`
}

type withdrawalAttemptOutput struct {
	EarnMaxHeaders
	Body struct {
		AttemptID string `json:"attemptId"`
	}
}

type withdrawalConsentView struct {
	CostStatus *earnmax.WithdrawalCostStatus `json:"costStatus"`
	Draft      earnmax.WithdrawalConsent     `json:"draft"`
	Approved   *earnmax.WithdrawalConsent    `json:"approved"`
	Reason     earnmax.WithdrawalWaitReason  `json:"reason,omitempty"`
}
type withdrawalConsentOutput struct {
	EarnMaxHeaders
	Body withdrawalConsentView
}
type withdrawalApprovalOutput struct {
	EarnMaxHeaders
	Body struct {
		ConsentID string `json:"consentId"`
	}
}

func (h earnMaxRoutes) registerWithdrawalConsents(api huma.API) {
	h.registerWithdrawalClaimAttempts(api)
	huma.Register(api, huma.Operation{OperationID: "get-earn-max-withdrawal-consent", Method: http.MethodGet, Path: withdrawalConsentPath, Summary: "Review the active withdrawal minimum", Responses: errorResponses[routeError](api, 401, 409, 503)}, func(ctx context.Context, in *sessionRequest) (*withdrawalConsentOutput, error) {
		p, err := h.auth.Principal(ctx, in.SessionInput)
		if err != nil {
			return nil, h.refused(err)
		}
		if err := h.admit(); err != nil {
			return nil, err
		}
		if h.cfg.Consents == nil {
			return nil, h.fail(http.StatusServiceUnavailable, "withdrawal_consent_unavailable", "Withdrawal approvals are temporarily unavailable.")
		}
		draft, err := h.cfg.Reader.WithdrawalConsentDraft(ctx, p.Wallet, p.Settings)
		if err != nil {
			return nil, h.fail(http.StatusConflict, "withdrawal_generation_unavailable", "The active withdrawal could not be verified. Refresh before approving.")
		}
		approved, err := h.cfg.Consents.Latest(ctx, draft.Authority, draft.WithdrawalGeneration)
		if err != nil {
			return nil, h.unavailable("withdrawal consent", err)
		}
		view := withdrawalConsentView{Draft: draft, Approved: approved, Reason: earnmax.WithdrawalNeedsConsent}
		if approved != nil {
			if h.cfg.WithdrawalStatus != nil {
				status, err := h.cfg.WithdrawalStatus.Read(ctx, *approved)
				if err != nil {
					slog.Warn("withdrawal cost status unavailable", "error", err)
				} else {
					view.CostStatus = status
				}
			}
			version, err := strconv.ParseUint(approved.Version, 10, 63)
			if err != nil {
				return nil, h.unavailable("withdrawal consent version", err)
			}
			view.Draft.Version = strconv.FormatUint(version+1, 10)
			view.Reason = ""
			if approved.PolicySeed != draft.PolicySeed || approved.Owner != draft.Owner {
				view.Reason = earnmax.WithdrawalNeedsConsent
			} else if !earnmax.ConsentExpiry(*approved).After(time.Now()) {
				view.Reason = earnmax.WithdrawalExpired
			} else if approved.MinClaimUSDC != "" {
				floor, _ := strconv.ParseUint(approved.MinClaimUSDC, 10, 64)
				current, _ := strconv.ParseUint(draft.ReferenceUSDC, 10, 64)
				if current < floor {
					view.Reason = earnmax.WithdrawalWaitingPrice
				}
			}
		}
		return &withdrawalConsentOutput{h.headers, view}, nil
	})
	huma.Register(api, huma.Operation{OperationID: "post-earn-max-withdrawal-consent", Method: http.MethodPost, Path: withdrawalConsentPath, Summary: "Record an owner-signed withdrawal minimum", Responses: errorResponses[routeError](api, 400, 401, 409, 503)}, func(ctx context.Context, in *redeemRequest) (*withdrawalApprovalOutput, error) {
		p, err := h.auth.Principal(ctx, in.SessionInput)
		if err != nil {
			return nil, h.refused(err)
		}
		if err := h.admit(); err != nil {
			return nil, err
		}
		if h.cfg.Consents == nil {
			return nil, h.fail(http.StatusServiceUnavailable, "withdrawal_consent_unavailable", "Withdrawal approvals are temporarily unavailable.")
		}
		var consent earnmax.WithdrawalConsent
		if json.Unmarshal(in.Raw.data, &consent) != nil {
			return nil, h.fail(http.StatusBadRequest, "invalid_withdrawal_consent", "Invalid withdrawal approval.")
		}
		if err := consent.Validate(p.Wallet, p.Settings, time.Now()); err != nil {
			return nil, h.fail(http.StatusBadRequest, "invalid_withdrawal_consent", err.Error())
		}
		if err := h.cfg.Reader.VerifyWithdrawalGeneration(ctx, consent); err != nil {
			return nil, h.fail(http.StatusConflict, "withdrawal_generation_changed", "Withdrawal changed. Refresh and approve again.")
		}
		id, err := h.cfg.Consents.Accept(ctx, consent)
		if err != nil {
			return nil, h.fail(http.StatusConflict, "withdrawal_consent_conflict", "Approval changed. Refresh and sign again.")
		}
		out := &withdrawalApprovalOutput{EarnMaxHeaders: h.headers}
		out.Body.ConsentID = id
		return out, nil
	})
	documentJSONBody[earnmax.WithdrawalConsent](api, withdrawalConsentPath)
}

func (h earnMaxRoutes) registerWithdrawalClaimAttempts(api huma.API) {
	const path = "/api/smart-accounts/earn-max/withdrawal-claim-attempt"
	huma.Register(api, huma.Operation{OperationID: "post-earn-max-withdrawal-claim-attempt", Method: http.MethodPost, Path: path, Summary: "Reserve a signed protected withdrawal claim before broadcast", Responses: errorResponses[routeError](api, 400, 401, 409, 503)}, func(ctx context.Context, in *redeemRequest) (*withdrawalAttemptOutput, error) {
		p, err := h.auth.Principal(ctx, in.SessionInput)
		if err != nil {
			return nil, h.refused(err)
		}
		if err := h.admit(); err != nil {
			return nil, err
		}
		if h.cfg.Consents == nil || h.cfg.Attempts == nil {
			return nil, h.fail(http.StatusServiceUnavailable, "withdrawal_claim_accounting_unavailable", "Withdrawal claim accounting is temporarily unavailable.")
		}
		var body withdrawalClaimAttempt
		if json.Unmarshal(in.Raw.data, &body) != nil {
			return nil, h.fail(http.StatusBadRequest, "invalid_claim_attempt", "Invalid withdrawal claim.")
		}
		wire, err := base64.StdEncoding.Strict().DecodeString(body.WireBase64)
		if err != nil || len(wire) == 0 || len(wire) > 1232 {
			return nil, h.fail(http.StatusBadRequest, "invalid_claim_wire", "Invalid withdrawal transaction size.")
		}
		fee, err := strconv.ParseUint(body.FeeLamports, 10, 64)
		if err != nil || strconv.FormatUint(fee, 10) != body.FeeLamports {
			return nil, h.fail(http.StatusBadRequest, "invalid_claim_fee", "Invalid withdrawal fee.")
		}
		if body.Consent.Owner != p.Wallet.String() || body.Consent.Settings != p.Settings.String() {
			return nil, h.fail(http.StatusForbidden, "withdrawal_owner_mismatch", "Withdrawal claim does not match the authenticated account.")
		}
		if id, found, err := h.cfg.Attempts.ExistingExact(ctx, body.Consent, wire); err != nil {
			return nil, h.fail(http.StatusConflict, "withdrawal_claim_recovery_unavailable", "The stored withdrawal claim could not be verified. Check status before signing again.")
		} else if found {
			out := &withdrawalAttemptOutput{EarnMaxHeaders: h.headers}
			out.Body.AttemptID = id
			return out, nil
		}
		if err := body.Consent.Validate(p.Wallet, p.Settings, time.Now()); err != nil {
			return nil, h.fail(http.StatusBadRequest, "invalid_withdrawal_consent", err.Error())
		}
		latest, err := h.cfg.Consents.Latest(ctx, body.Consent.Authority, body.Consent.WithdrawalGeneration)
		if err != nil {
			return nil, h.unavailable("claim consent", err)
		}
		id, _ := body.Consent.Digest()
		if latest == nil {
			return nil, h.fail(http.StatusConflict, "withdrawal_needs_consent", "Approve this withdrawal first.")
		}
		latestID, _ := latest.Digest()
		if latestID != id {
			return nil, h.fail(http.StatusConflict, "withdrawal_consent_changed", "Approval changed. Refresh and sign again.")
		}
		if err := h.cfg.Reader.VerifyWithdrawalGeneration(ctx, body.Consent); err != nil {
			return nil, h.fail(http.StatusConflict, "withdrawal_generation_changed", "Withdrawal changed. Refresh and approve again.")
		}
		attemptID, err := h.cfg.Attempts.Reserve(ctx, body.Consent, wire, body.Signature, body.Blockhash, body.LastValidBlockHeight, fee)
		if err != nil {
			if errors.Is(err, earnmax.ErrWithdrawalFeeBudget) {
				return nil, h.fail(http.StatusConflict, "fee_budget_exhausted", "The approved network-fee budget is used. Review and approve a higher budget before signing again.")
			}
			if errors.Is(err, earnmax.ErrWithdrawalClaimPending) {
				return nil, h.fail(http.StatusConflict, "withdrawal_claim_pending", "A claim you already signed is still pending. Check status before signing another transaction.")
			}
			if refusal := (*releasecontrol.Refusal)(nil); errors.As(err, &refusal) {
				return nil, h.fail(http.StatusConflict, refusal.Reason, "Withdrawal claims are paused for this vault.")
			}
			return nil, h.fail(http.StatusConflict, "withdrawal_claim_not_reserved", "This claim could not be reserved. Check status before signing another transaction.")
		}
		out := &withdrawalAttemptOutput{EarnMaxHeaders: h.headers}
		out.Body.AttemptID = attemptID
		return out, nil
	})
	documentJSONBody[withdrawalClaimAttempt](api, path)
}

package webapp

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// POST /api/smart-accounts/yield-optimization/autodeposit/setup/confirm ports
// web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/setup/confirm/route.ts.
// The device echoes its prepared metadata; the server rebuilds every address
// canonically, asserts the on-chain artifacts, records the intent
// (autodeposit_repository.go) and, for an already-active target, schedules the
// bootstrap sweep. Mobile's sendWalletPush stays inside the op (pathmap.md:14).
// Remaining family paths (floor/toggle/close confirms, sweeps GET) are units 2-3.
const autodepositSetupConfirmPath = "/api/smart-accounts/yield-optimization/autodeposit/setup/confirm"

// AutodepositSetupConfirmRequest is the documented body: every unsigned
// integer arrives as a decimal string, as the web client sends it.
type AutodepositSetupConfirmRequest struct {
	AmountPerPeriodRaw                  string `json:"amountPerPeriodRaw"`
	Cluster                             string `json:"cluster"`
	ConfirmedSlot                       string `json:"confirmedSlot"`
	DelegatedSigner                     string `json:"delegatedSigner"`
	ExpiryTimestamp                     string `json:"expiryTimestamp"`
	LiquidityMint                       string `json:"liquidityMint"`
	Nonce                               string `json:"nonce"`
	PeriodLengthSeconds                 string `json:"periodLengthSeconds"`
	PolicyAccount                       string `json:"policyAccount"`
	PolicyID                            string `json:"policyId"`
	PolicySeed                          string `json:"policySeed"`
	RecurringDelegation                 string `json:"recurringDelegation"`
	Settings                            string `json:"settings"`
	SetupSignature                      string `json:"setupSignature"`
	SetupStage                          string `json:"setupStage"`
	StartTimestamp                      string `json:"startTimestamp"`
	SubscriptionAuthority               string `json:"subscriptionAuthority"`
	SubscriptionAuthorityInitialization string `json:"subscriptionAuthorityInitialization"`
	SubscriptionDelegatee               string `json:"subscriptionDelegatee"`
	VaultIndex                          int    `json:"vaultIndex"`
	VaultPubkey                         string `json:"vaultPubkey"`
	VaultUsdcAta                        string `json:"vaultUsdcAta"`
	WalletAddress                       string `json:"walletAddress"`
	WalletBalanceFloorRaw               string `json:"walletBalanceFloorRaw"`
	WalletUsdcAta                       string `json:"walletUsdcAta"`
}

type AutodepositSetupConfirmSweep struct {
	Classification        string  `json:"classification"`
	Confidence            string  `json:"confidence"`
	EligibleAfter         string  `json:"eligibleAfter"`
	ExecuteNowAvailableAt *string `json:"executeNowAvailableAt"`
	ID                    string  `json:"id"`
	LotCount              int64   `json:"lotCount"`
	OriginalAmountRaw     string  `json:"originalAmountRaw"`
	Reason                string  `json:"reason"`
	RemainingAmountRaw    string  `json:"remainingAmountRaw"`
	SlotID                string  `json:"slotId"`
	Status                string  `json:"status"`
}

type AutodepositSetupConfirmBootstrapSweep struct {
	Reason *string                       `json:"reason,omitempty"`
	Status string                        `json:"status"`
	Sweep  *AutodepositSetupConfirmSweep `json:"sweep,omitempty"`
}

type AutodepositSetupConfirmTarget struct {
	Active                bool    `json:"active"`
	BalanceSweepPolicyID  *string `json:"balanceSweepPolicyId"`
	ID                    string  `json:"id"`
	LifecycleStatus       string  `json:"lifecycleStatus"`
	PolicyAccount         string  `json:"policyAccount"`
	RecurringDelegation   *string `json:"recurringDelegation"`
	WalletBalanceFloorRaw *string `json:"walletBalanceFloorRaw"`
}

type AutodepositSetupConfirmResponse struct {
	ConfirmedSlot  string                                 `json:"confirmedSlot"`
	BootstrapSweep *AutodepositSetupConfirmBootstrapSweep `json:"bootstrapSweep,omitempty"`
	Target         *AutodepositSetupConfirmTarget         `json:"target,omitempty"`
}

// parseAutodepositSetupConfirmBody ports
// parseEarnAutodepositSetupConfirmRequestBody
// (earn-autodeposit-prepare-contracts.shared.ts:466): fields evaluate in the
// TS object-literal order and every failure names its key, so the first bad
// field matches Next's error message exactly.
func parseAutodepositSetupConfirmBody(raw []byte) (autodepositSetupInput, error) {
	object, err := autodepositBodyObject(raw)
	if err != nil {
		return autodepositSetupInput{}, err
	}
	in := autodepositSetupInput{}
	read := func() error {
		if err := readAutodepositUint(object, "amountPerPeriodRaw", &in.AmountPerPeriodRaw); err != nil {
			return err
		}
		if err := readAutodepositString(object, "cluster", &in.Cluster); err != nil {
			return err
		}
		if err := readAutodepositUint(object, "confirmedSlot", &in.ConfirmedSlot); err != nil {
			return err
		}
		if err := readAutodepositString(object, "delegatedSigner", &in.DelegatedSigner); err != nil {
			return err
		}
		if err := readAutodepositUint(object, "expiryTimestamp", &in.ExpiryTimestamp); err != nil {
			return err
		}
		if err := readAutodepositString(object, "liquidityMint", &in.LiquidityMint); err != nil {
			return err
		}
		if err := readAutodepositUint(object, "nonce", &in.Nonce); err != nil {
			return err
		}
		if err := readAutodepositUint(object, "periodLengthSeconds", &in.PeriodLengthSeconds); err != nil {
			return err
		}
		if err := readAutodepositString(object, "policyAccount", &in.PolicyAccount); err != nil {
			return err
		}
		if err := readAutodepositUint(object, "policyId", &in.PolicyID); err != nil {
			return err
		}
		if err := readAutodepositUint(object, "policySeed", &in.PolicySeed); err != nil {
			return err
		}
		if err := readAutodepositString(object, "recurringDelegation", &in.RecurringDelegation); err != nil {
			return err
		}
		if err := readAutodepositString(object, "settings", &in.Settings); err != nil {
			return err
		}
		if err := readAutodepositString(object, "setupSignature", &in.SetupSignature); err != nil {
			return err
		}
		stage, err := requireAutodepositString(object, "setupStage")
		if err != nil {
			return err
		}
		switch stage {
		case "initialize_subscription_authority", "approve_token_delegate", "create_policy", "create_recurring_delegation":
			in.SetupStage = stage
		default:
			return errors.New("setupStage is not a supported autodeposit stage.")
		}
		if err := readAutodepositUint(object, "startTimestamp", &in.StartTimestamp); err != nil {
			return err
		}
		if err := readAutodepositString(object, "subscriptionAuthority", &in.SubscriptionAuthority); err != nil {
			return err
		}
		initialization, err := requireAutodepositString(object, "subscriptionAuthorityInitialization")
		if err != nil {
			return err
		}
		if initialization != "exists" && initialization != "required" {
			return errors.New("subscriptionAuthorityInitialization must be exists or required.")
		}
		in.SubscriptionAuthorityInitialization = initialization
		if err := readAutodepositString(object, "subscriptionDelegatee", &in.SubscriptionDelegatee); err != nil {
			return err
		}
		var vaultIndex int
		if err := readAutodepositVaultIndex(object, &vaultIndex); err != nil {
			return err
		}
		in.VaultIndex = vaultIndex
		if err := readAutodepositString(object, "vaultPubkey", &in.VaultPubkey); err != nil {
			return err
		}
		if err := readAutodepositString(object, "vaultUsdcAta", &in.VaultUsdcAta); err != nil {
			return err
		}
		if err := readAutodepositString(object, "walletAddress", &in.WalletAddress); err != nil {
			return err
		}
		if err := readAutodepositUint(object, "walletBalanceFloorRaw", &in.WalletBalanceFloorRaw); err != nil {
			return err
		}
		return readAutodepositString(object, "walletUsdcAta", &in.WalletUsdcAta)
	}
	if err := read(); err != nil {
		return autodepositSetupInput{}, err
	}
	return in, nil
}

func requireAutodepositString(object map[string]any, key string) (string, error) {
	value, ok := object[key].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s must be a non-empty string.", key)
	}
	return strings.TrimSpace(value), nil
}

func readAutodepositString(object map[string]any, key string, into *string) error {
	value, err := requireAutodepositString(object, key)
	if err != nil {
		return err
	}
	*into = value
	return nil
}

// readAutodepositUint ports readBigIntString: a trimmed decimal string, parsed
// strictly. A value beyond uint64 is rejected with the same message the TS
// reader uses for malformed input.
func readAutodepositUint(object map[string]any, key string, into *uint64) error {
	value, ok := object[key].(string)
	if !ok {
		return fmt.Errorf("%s must be an unsigned integer string.", key)
	}
	trimmed := strings.TrimSpace(value)
	for _, digit := range trimmed {
		if digit < '0' || digit > '9' {
			return fmt.Errorf("%s must be an unsigned integer string.", key)
		}
	}
	if trimmed == "" {
		return fmt.Errorf("%s must be an unsigned integer string.", key)
	}
	parsed, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil {
		return fmt.Errorf("%s must be an unsigned integer string.", key)
	}
	*into = parsed
	return nil
}

func serializeAutodepositSweep(sweep *autodepositSweepRow) *AutodepositSetupConfirmSweep {
	if sweep == nil {
		return nil
	}
	out := &AutodepositSetupConfirmSweep{
		Classification:     sweep.Classification,
		Confidence:         sweep.Confidence,
		EligibleAfter:      isoMilliseconds(sweep.EligibleAfter),
		ID:                 strconv.FormatInt(sweep.ID, 10),
		LotCount:           sweep.LotCount,
		OriginalAmountRaw:  strconv.FormatUint(sweep.OriginalAmountRaw, 10),
		Reason:             sweep.Reason,
		RemainingAmountRaw: strconv.FormatUint(sweep.RemainingAmountRaw, 10),
		SlotID:             strconv.FormatInt(sweep.SlotID, 10),
		Status:             sweep.Status,
	}
	if sweep.ExecuteNowAvailableAt != nil {
		at := isoMilliseconds(*sweep.ExecuteNowAvailableAt)
		out.ExecuteNowAvailableAt = &at
	}
	return out
}

// serializeAutodepositTarget ports serializeTarget (route.ts:265).
func serializeAutodepositTarget(target autodepositTargetRow) *AutodepositSetupConfirmTarget {
	out := &AutodepositSetupConfirmTarget{
		Active:              target.Active,
		ID:                  strconv.FormatInt(target.ID, 10),
		LifecycleStatus:     target.LifecycleStatus,
		PolicyAccount:       target.PolicyAccount,
		RecurringDelegation: target.RecurringDelegation,
	}
	if target.BalanceSweepPolicyID != nil {
		id := strconv.FormatInt(*target.BalanceSweepPolicyID, 10)
		out.BalanceSweepPolicyID = &id
	}
	if target.WalletBalanceFloorRaw != nil {
		floor := strconv.FormatUint(*target.WalletBalanceFloorRaw, 10)
		out.WalletBalanceFloorRaw = &floor
	}
	return out
}

// readAutodepositBootstrapSnapshot ports readBootstrapWalletBalanceSnapshot
// (route.ts:309): the wallet USDC balance at confirm time, or a skip reason.
func (h earnRoutes) readAutodepositBootstrapSnapshot(ctx context.Context, in autodepositSetupInput) (autodepositBootstrapSnapshot, *string, error) {
	skip := func(reason string) (autodepositBootstrapSnapshot, *string, error) {
		return autodepositBootstrapSnapshot{}, &reason, nil
	}
	walletAta, err := solana.PublicKeyFromBase58(in.WalletUsdcAta)
	if err != nil {
		return autodepositBootstrapSnapshot{}, nil, err
	}
	slot, accounts, err := h.Chain.Accounts(ctx, []solana.PublicKey{walletAta}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return autodepositBootstrapSnapshot{}, nil, err
	}
	account := accounts[0]
	if account == nil {
		return skip("wallet_usdc_ata_missing")
	}
	if account.Owner != solana.TokenProgramID {
		return skip("wallet_usdc_ata_invalid_owner")
	}
	data := account.Data
	if len(data) < 165 {
		return skip("wallet_usdc_ata_invalid_data")
	}
	mint := solana.PublicKeyFromBytes(data[0:32])
	if mint.String() != in.LiquidityMint {
		return skip("wallet_usdc_ata_non_usdc")
	}
	owner := solana.PublicKeyFromBytes(data[32:64])
	if owner.String() != in.WalletAddress {
		return skip("wallet_usdc_ata_wallet_mismatch")
	}
	hash := sha256.Sum256(data)
	return autodepositBootstrapSnapshot{
		AmountRaw:        binary.LittleEndian.Uint64(data[64:72]),
		ObservedSlot:     slot,
		ObservedAt:       time.Now(),
		Mint:             mint.String(),
		Owner:            owner.String(),
		AccountDataHash:  fmt.Sprintf("%x", hash[:]),
		AccountLamports:  strconv.FormatUint(account.Lamports, 10),
		AccountOwner:     account.Owner.String(),
		SetupSignature:   in.SetupSignature,
		Source:           autodepositBootstrapEventSource,
		SourceCommitment: "confirmed",
	}, nil, nil
}

type autodepositSetupConfirmInput struct {
	SessionInput
	Raw rawJSON
}

type autodepositSetupConfirmOutput struct {
	Body AutodepositSetupConfirmResponse
}

func (h earnRoutes) registerAutodepositSetup(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-autodeposit-setup-confirm",
		Method:      http.MethodPost,
		Path:        autodepositSetupConfirmPath,
		Summary:     "Confirm an Earn autodeposit setup stage",
		Responses:   errorResponses[routeError](api, 400, 401, 403, 409),
	}, h.autodepositSetupConfirm)
	documentJSONBody[AutodepositSetupConfirmRequest](api, autodepositSetupConfirmPath)
}

func (h earnRoutes) autodepositSetupConfirm(ctx context.Context, in *autodepositSetupConfirmInput) (*autodepositSetupConfirmOutput, error) {
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		return nil, earnUnavailable("load Earn account", err, "account_unavailable", "Earn account is unavailable.")
	}

	// route.ts:415-424 — parse errors pass their message through under
	// invalid_request.
	input, err := parseAutodepositSetupConfirmBody(in.Raw.data)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "invalid_request", err.Error())
	}

	// route.ts:426-435 — the echoed wallet/settings must be the session's.
	if input.WalletAddress != account.Wallet.String() || input.Settings != account.Settings.String() {
		return nil, earnError(http.StatusForbidden, "principal_mismatch",
			"Confirmed autodeposit setup does not match the authenticated wallet session.")
	}

	// route.ts:437-447 — rebuild every address canonically.
	canonical, err := canonicalizeAutodepositSetupInput(input, h.PolicySigner)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "metadata_mismatch", err.Error())
	}

	// route.ts:449-459.
	if canonical.Cluster != string(account.Cluster) {
		return nil, earnError(http.StatusBadRequest, "cluster_mismatch",
			"Confirmed autodeposit setup cluster does not match the configured Solana environment.")
	}

	// route.ts:461-487 — the setup transaction must be confirmed; the client's
	// non-zero slot must match the observed one.
	confirmedSlot, err := autodepositConfirmedSlot(ctx, h.Chain, canonical.SetupSignature)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "unconfirmed_signature", err.Error())
	}
	if canonical.ConfirmedSlot != 0 && canonical.ConfirmedSlot != confirmedSlot {
		return nil, earnError(http.StatusBadRequest, "slot_mismatch",
			"Confirmed autodeposit setup slot does not match the transaction status.")
	}
	canonical.ConfirmedSlot = confirmedSlot

	requirePolicy := canonical.SetupStage == "create_policy" || canonical.SetupStage == "approve_token_delegate"
	requireDelegation := canonical.SetupStage == "create_recurring_delegation" || canonical.SetupStage == "approve_token_delegate"

	// route.ts:489-541 — the created policy and delegation must exist, be
	// owned correctly and match the canonical layout byte for byte.
	if requirePolicy || requireDelegation {
		policyKey, keyErr := solana.PublicKeyFromBase58(canonical.PolicyAccount)
		if keyErr != nil {
			return nil, earnError(http.StatusBadRequest, "metadata_mismatch", keyErr.Error())
		}
		delegationKey, keyErr := solana.PublicKeyFromBase58(canonical.RecurringDelegation)
		if keyErr != nil {
			return nil, earnError(http.StatusBadRequest, "metadata_mismatch", keyErr.Error())
		}
		err := withAutodepositArtifactRetry(ctx, func() error {
			accounts, readErr := autodepositAccountAt(ctx, h.Chain, policyKey, delegationKey)
			if readErr != nil {
				return readErr
			}
			if err := assertAutodepositArtifactsExist(accounts, requirePolicy, requireDelegation); err != nil {
				return err
			}
			return assertAutodepositCanonicalArtifacts(accounts, canonical, h.PolicySigner, requirePolicy, requireDelegation)
		})
		if err != nil {
			return nil, earnError(http.StatusConflict, "artifact_missing", err.Error())
		}
	}

	// route.ts:543-560 — the wallet must have approved the delegation pull.
	if requireDelegation {
		walletAta, keyErr := solana.PublicKeyFromBase58(canonical.WalletUsdcAta)
		if keyErr != nil {
			return nil, earnError(http.StatusBadRequest, "metadata_mismatch", keyErr.Error())
		}
		err := withAutodepositArtifactRetry(ctx, func() error {
			accounts, readErr := autodepositAccountAt(ctx, h.Chain, walletAta)
			if readErr != nil {
				return readErr
			}
			return assertAutodepositWalletTokenApproval(accounts[0], canonical)
		})
		if err != nil {
			return nil, earnError(http.StatusConflict, "token_approval_missing", err.Error())
		}
	}

	// route.ts:562-566 — the first stage only proves the authority landed.
	if canonical.SetupStage == "initialize_subscription_authority" {
		return &autodepositSetupConfirmOutput{Body: AutodepositSetupConfirmResponse{
			ConfirmedSlot: strconv.FormatUint(confirmedSlot, 10),
		}}, nil
	}

	// route.ts:568-585.
	target, err := h.recordAutodepositSetupIntent(ctx, canonical)
	if err != nil {
		if strings.Contains(err.Error(), "Closed autodeposit targets cannot be reactivated") {
			return nil, earnError(http.StatusConflict, "autodeposit_target_closed",
				"Autodeposit was already closed. Start Autodeposit again to create a fresh policy.")
		}
		return nil, earnError(http.StatusBadRequest, "record_failed", err.Error())
	}

	// route.ts:586-625 — only an already-active target bootstraps a sweep;
	// any failure is reported in the response, never the status.
	var bootstrapSweep *AutodepositSetupConfirmBootstrapSweep
	var scheduledSweep *autodepositSweepRow
	if target.Active && target.LifecycleStatus == "active" {
		snapshot, skipReason, err := h.readAutodepositBootstrapSnapshot(ctx, canonical)
		switch {
		case err != nil:
			slog.WarnContext(ctx, "[autodeposit setup] bootstrap scheduling failed",
				"errorMessage", err.Error(), "policyAccount", canonical.PolicyAccount,
				"setupSignature", canonical.SetupSignature, "walletAddress", canonical.WalletAddress)
			reason := err.Error()
			bootstrapSweep = &AutodepositSetupConfirmBootstrapSweep{Reason: &reason, Status: "failed"}
		case skipReason != nil:
			bootstrapSweep = &AutodepositSetupConfirmBootstrapSweep{Reason: skipReason, Status: "skipped"}
		default:
			result, scheduleErr := h.scheduleBootstrapAutodepositSweep(ctx, snapshot, target)
			if scheduleErr != nil {
				slog.WarnContext(ctx, "[autodeposit setup] bootstrap scheduling failed",
					"errorMessage", scheduleErr.Error(), "policyAccount", canonical.PolicyAccount,
					"setupSignature", canonical.SetupSignature, "walletAddress", canonical.WalletAddress)
				reason := scheduleErr.Error()
				bootstrapSweep = &AutodepositSetupConfirmBootstrapSweep{Reason: &reason, Status: "failed"}
			} else if result.Status == "skipped" {
				bootstrapSweep = &AutodepositSetupConfirmBootstrapSweep{Reason: &result.Reason, Status: "skipped"}
			} else {
				bootstrapSweep = &AutodepositSetupConfirmBootstrapSweep{Status: result.Status, Sweep: serializeAutodepositSweep(result.Sweep)}
				if result.Status == "scheduled" {
					scheduledSweep = result.Sweep
				}
			}
		}
	}

	// Mobile parity (M/autodeposit/setup/confirm/route.ts:713-731, DECIDED
	// keep in pathmap.md:14): push exactly when a delegation-bearing stage
	// confirms — the scheduled-sweep copy when one was scheduled, the enabled
	// copy otherwise.
	if requireDelegation {
		payload := autodepositEnabledPush()
		if scheduledSweep != nil {
			payload = autodepositSweepScheduledPush(scheduledSweep.RemainingAmountRaw)
		}
		sendAutodepositWalletPush(ctx, canonical.WalletAddress, payload)
	}

	return &autodepositSetupConfirmOutput{Body: AutodepositSetupConfirmResponse{
		ConfirmedSlot:  strconv.FormatUint(confirmedSlot, 10),
		BootstrapSweep: bootstrapSweep,
		Target:         serializeAutodepositTarget(target),
	}}, nil
}

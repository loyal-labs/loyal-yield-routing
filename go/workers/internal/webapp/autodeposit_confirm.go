package webapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// Confirm ops for the remaining autodeposit writes, ported from
// web/apps/web/src/app/api/smart-accounts/yield-optimization/autodeposit/{close,floor,toggle}/confirm/route.ts.
// Mobile's wallet pushes stay inside the ops (pathmap.md:15-16): floor pushes
// the scheduled-sweep copy when a rebaseline sweep was scheduled, toggle
// pushes the enabled copy when the target turns active.

const (
	autodepositCloseConfirmPath  = "/api/smart-accounts/yield-optimization/autodeposit/close/confirm"
	autodepositFloorConfirmPath  = "/api/smart-accounts/yield-optimization/autodeposit/floor/confirm"
	autodepositToggleConfirmPath = "/api/smart-accounts/yield-optimization/autodeposit/toggle/confirm"
)

// AutodepositCloseConfirmRequest is the documented body: every unsigned
// integer arrives as a decimal string, as the web client sends it.
type AutodepositCloseConfirmRequest struct {
	Cluster             string `json:"cluster"`
	CloseSignature      string `json:"closeSignature"`
	ConfirmedSlot       string `json:"confirmedSlot"`
	DelegatedSigner     string `json:"delegatedSigner"`
	PolicyAccount       string `json:"policyAccount"`
	RecurringDelegation string `json:"recurringDelegation"`
	Settings            string `json:"settings"`
	VaultIndex          int    `json:"vaultIndex"`
	VaultPubkey         string `json:"vaultPubkey"`
	WalletAddress       string `json:"walletAddress"`
}

type AutodepositFloorConfirmRequest struct {
	PolicyAccount         string `json:"policyAccount"`
	RecurringDelegation   string `json:"recurringDelegation"`
	VaultIndex            int    `json:"vaultIndex"`
	WalletBalanceFloorRaw string `json:"walletBalanceFloorRaw"`
}

type AutodepositToggleConfirmRequest struct {
	Active              bool   `json:"active"`
	PolicyAccount       string `json:"policyAccount"`
	RecurringDelegation string `json:"recurringDelegation"`
	VaultIndex          int    `json:"vaultIndex"`
}

// autodepositCloseInput is the parsed close body, mirroring
// ConfirmedEarnAutodepositCloseInput (BigInt fields become uint64).
type autodepositCloseInput struct {
	Cluster             string
	CloseSignature      string
	ConfirmedSlot       uint64
	DelegatedSigner     string
	PolicyAccount       string
	RecurringDelegation string
	Settings            string
	VaultIndex          int
	VaultPubkey         string
	WalletAddress       string
}

type autodepositFloorInput struct {
	PolicyAccount         string
	RecurringDelegation   string
	Settings              string
	VaultIndex            int
	WalletAddress         string
	WalletBalanceFloorRaw uint64
}

type autodepositToggleInput struct {
	Active              bool
	PolicyAccount       string
	RecurringDelegation string
	Settings            string
	VaultIndex          int
	WalletAddress       string
}

// autodepositBodyObject ports assertRequestObject
// (prepare-contracts.shared.ts:190): anything but a JSON object fails with
// the same message.
func autodepositBodyObject(raw []byte) (map[string]any, error) {
	parsed, err := parseJSONBody(raw)
	if err != nil {
		return nil, errors.New("Request body must be an object.")
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return nil, errors.New("Request body must be an object.")
	}
	return object, nil
}

// readAutodepositVaultIndex ports readVaultIndex
// (prepare-contracts.shared.ts:252): the JSON number must be exactly 1.
func readAutodepositVaultIndex(object map[string]any, into *int) error {
	if value, ok := object["vaultIndex"]; !ok || value != float64(1) {
		return errors.New("vaultIndex must be 1 for Earn autodeposit.")
	}
	*into = 1
	return nil
}

// parseAutodepositCloseConfirmBody ports
// parseEarnAutodepositCloseConfirmRequestBody
// (earn-autodeposit-prepare-contracts.shared.ts:500): fields evaluate in the
// TS object-literal order and every failure names its key, so the first bad
// field matches Next's error message exactly.
func parseAutodepositCloseConfirmBody(raw []byte) (autodepositCloseInput, error) {
	object, err := autodepositBodyObject(raw)
	if err != nil {
		return autodepositCloseInput{}, err
	}
	in := autodepositCloseInput{}
	read := func() error {
		if err := readAutodepositString(object, "cluster", &in.Cluster); err != nil {
			return err
		}
		if err := readAutodepositString(object, "closeSignature", &in.CloseSignature); err != nil {
			return err
		}
		if err := readAutodepositUint(object, "confirmedSlot", &in.ConfirmedSlot); err != nil {
			return err
		}
		if err := readAutodepositString(object, "delegatedSigner", &in.DelegatedSigner); err != nil {
			return err
		}
		if err := readAutodepositString(object, "policyAccount", &in.PolicyAccount); err != nil {
			return err
		}
		if err := readAutodepositString(object, "recurringDelegation", &in.RecurringDelegation); err != nil {
			return err
		}
		if err := readAutodepositString(object, "settings", &in.Settings); err != nil {
			return err
		}
		if err := readAutodepositVaultIndex(object, &in.VaultIndex); err != nil {
			return err
		}
		if err := readAutodepositString(object, "vaultPubkey", &in.VaultPubkey); err != nil {
			return err
		}
		return readAutodepositString(object, "walletAddress", &in.WalletAddress)
	}
	if err := read(); err != nil {
		return autodepositCloseInput{}, err
	}
	return in, nil
}

// parseAutodepositFloorConfirmBody ports
// parseEarnAutodepositFloorUpdateConfirmRequestBody
// (earn-autodeposit-prepare-contracts.shared.ts:370).
func parseAutodepositFloorConfirmBody(raw []byte) (autodepositFloorInput, error) {
	object, err := autodepositBodyObject(raw)
	if err != nil {
		return autodepositFloorInput{}, err
	}
	in := autodepositFloorInput{}
	read := func() error {
		if err := readAutodepositString(object, "policyAccount", &in.PolicyAccount); err != nil {
			return err
		}
		if err := readAutodepositString(object, "recurringDelegation", &in.RecurringDelegation); err != nil {
			return err
		}
		if err := readAutodepositVaultIndex(object, &in.VaultIndex); err != nil {
			return err
		}
		return readAutodepositUint(object, "walletBalanceFloorRaw", &in.WalletBalanceFloorRaw)
	}
	if err := read(); err != nil {
		return autodepositFloorInput{}, err
	}
	return in, nil
}

// parseAutodepositToggleConfirmBody ports
// parseEarnAutodepositToggleConfirmRequestBody
// (earn-autodeposit-prepare-contracts.shared.ts:387).
func parseAutodepositToggleConfirmBody(raw []byte) (autodepositToggleInput, error) {
	object, err := autodepositBodyObject(raw)
	if err != nil {
		return autodepositToggleInput{}, err
	}
	in := autodepositToggleInput{}
	read := func() error {
		// readRequiredBoolean (prepare-contracts.shared.ts:241).
		active, isBool := object["active"].(bool)
		if !isBool {
			return errors.New("active must be a boolean.")
		}
		in.Active = active
		if err := readAutodepositString(object, "policyAccount", &in.PolicyAccount); err != nil {
			return err
		}
		if err := readAutodepositString(object, "recurringDelegation", &in.RecurringDelegation); err != nil {
			return err
		}
		return readAutodepositVaultIndex(object, &in.VaultIndex)
	}
	if err := read(); err != nil {
		return autodepositToggleInput{}, err
	}
	return in, nil
}

// ---- canonical close rebuild (close/confirm/route.ts:53-96) ----

// canonicalizeAutodepositCloseInput mirrors
// createCanonicalAutodepositCloseInput: the cluster, deployment signer, vault
// index and vault address are rebuilt server-side and asserted in the TS
// order; the policy and delegation addresses are only checked to parse.
func canonicalizeAutodepositCloseInput(in autodepositCloseInput, policySigner solana.PublicKey) (autodepositCloseInput, error) {
	cluster, err := normalizeLoyalCluster(in.Cluster)
	if err != nil {
		return in, err
	}
	settings, err := solana.PublicKeyFromBase58(in.Settings)
	if err != nil {
		return in, fmt.Errorf("settings is not a valid public key: %w", err)
	}
	vault, _, err := squads.SmartAccountAddress(settings, earnVaultIndex)
	if err != nil {
		return in, err
	}
	assert := func(actual, expected, label string) error {
		if actual != expected {
			return fmt.Errorf("%s does not match canonical Earn autodeposit metadata.", label)
		}
		return nil
	}
	// TS's cluster assert compares the normalized request cluster against the
	// canonical one — tautological after normalizeLoyalCluster accepted it —
	// so only the signer, index and vault asserts can fire here.
	if err := assert(in.DelegatedSigner, policySigner.String(), "delegatedSigner"); err != nil {
		return in, err
	}
	if in.VaultIndex != int(earnVaultIndex) {
		return in, errors.New("vaultIndex does not match canonical Earn autodeposit metadata.")
	}
	if err := assert(in.VaultPubkey, vault.String(), "vaultPubkey"); err != nil {
		return in, err
	}
	if _, err := solana.PublicKeyFromBase58(in.PolicyAccount); err != nil {
		return in, fmt.Errorf("policyAccount is not a valid public key: %w", err)
	}
	if _, err := solana.PublicKeyFromBase58(in.RecurringDelegation); err != nil {
		return in, fmt.Errorf("recurringDelegation is not a valid public key: %w", err)
	}
	in.Cluster = cluster
	in.DelegatedSigner = policySigner.String()
	in.VaultIndex = int(earnVaultIndex)
	in.VaultPubkey = vault.String()
	return in, nil
}

// autodepositCloseConfirmedSlot ports the close route's
// resolveConfirmedSignatureSlot (close/confirm/route.ts:115-141): the close
// transaction must be confirmed or finalized and its slot is what we record.
func autodepositCloseConfirmedSlot(ctx context.Context, client *chain.Client, signature string) (uint64, error) {
	state, err := client.SignatureState(ctx, signature)
	if err != nil {
		return 0, err
	}
	if !state.Found || state.Err != "" || state.Commitment == chain.Processed {
		return 0, errors.New("Autodeposit close transaction is not confirmed.")
	}
	if state.Slot == 0 {
		return 0, errors.New("Confirmed transaction slot is unavailable.")
	}
	return state.Slot, nil
}

// ---- response bodies ----

// AutodepositFloorRebaselineSweep keeps both TS key orders byte-identical:
// the repository's skipped literal is {reason, status} and the route's
// scheduled literal is {status, sweep} — omitted fields leave no gap.
type AutodepositFloorRebaselineSweep struct {
	Reason *string                       `json:"reason,omitempty"`
	Status string                        `json:"status"`
	Sweep  *AutodepositSetupConfirmSweep `json:"sweep,omitempty"`
}

type AutodepositFloorConfirmResponse struct {
	RebaselineSweep *AutodepositFloorRebaselineSweep `json:"rebaselineSweep,omitempty"`
	Target          *AutodepositSetupConfirmTarget   `json:"target"`
}

// AutodepositTargetResponse is the close and toggle body:
// NextResponse.json({ target }).
type AutodepositTargetResponse struct {
	Target *AutodepositSetupConfirmTarget `json:"target"`
}

// serializeAutodepositFloorSweep ports the web floor route's
// serializeScheduledSweep (floor/confirm/route.ts:36-55): id and slotId both
// carry the slot id, lotCount is the CTE's fixed 1, and
// executeNowAvailableAt renders as null while the delegation is not ready.
func serializeAutodepositFloorSweep(sweep *autodepositFloorSweep) *AutodepositSetupConfirmSweep {
	out := &AutodepositSetupConfirmSweep{
		Classification:     sweep.Classification,
		Confidence:         sweep.Confidence,
		EligibleAfter:      isoMilliseconds(sweep.EligibleAfter),
		ID:                 strconv.FormatInt(sweep.SlotID, 10),
		LotCount:           1,
		OriginalAmountRaw:  strconv.FormatInt(sweep.OriginalAmountRaw, 10),
		Reason:             sweep.Reason,
		RemainingAmountRaw: strconv.FormatInt(sweep.RemainingAmountRaw, 10),
		SlotID:             strconv.FormatInt(sweep.SlotID, 10),
		Status:             sweep.Status,
	}
	if sweep.ExecuteNowAvailableAt != nil {
		at := isoMilliseconds(*sweep.ExecuteNowAvailableAt)
		out.ExecuteNowAvailableAt = &at
	}
	return out
}

type autodepositTargetOutput struct {
	Body AutodepositTargetResponse
}

func newAutodepositTargetOutput(target autodepositTargetRow) *autodepositTargetOutput {
	return &autodepositTargetOutput{Body: AutodepositTargetResponse{Target: serializeAutodepositTarget(target)}}
}

// ---- handlers ----

type autodepositCloseConfirmInput struct {
	SessionInput
	Raw rawJSON
}

func (h earnRoutes) registerAutodepositClose(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-autodeposit-close-confirm",
		Method:      http.MethodPost,
		Path:        autodepositCloseConfirmPath,
		Summary:     "Confirm an Earn autodeposit close",
		Responses:   errorResponses[routeError](api, 400, 401, 403, 503),
	}, h.autodepositCloseConfirm)
	documentJSONBody[AutodepositCloseConfirmRequest](api, autodepositCloseConfirmPath)
}

func (h earnRoutes) autodepositCloseConfirm(ctx context.Context, in *autodepositCloseConfirmInput) (*autodepositTargetOutput, error) {
	// route.ts:158-162.
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		return nil, earnUnavailable("load Earn account", err, "account_unavailable", "Earn account is unavailable.")
	}

	// route.ts:164-173 — parse errors pass their message through under
	// invalid_request.
	input, err := parseAutodepositCloseConfirmBody(in.Raw.data)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "invalid_request", err.Error())
	}

	// route.ts:175-184.
	if input.WalletAddress != account.Wallet.String() || input.Settings != account.Settings.String() {
		return nil, earnError(http.StatusForbidden, "principal_mismatch",
			"Confirmed autodeposit close does not match the authenticated wallet session.")
	}

	// route.ts:186-196.
	canonical, err := canonicalizeAutodepositCloseInput(input, h.PolicySigner)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "metadata_mismatch", err.Error())
	}

	// route.ts:198-206.
	if canonical.Cluster != string(account.Cluster) {
		return nil, earnError(http.StatusBadRequest, "cluster_mismatch",
			"Confirmed autodeposit close cluster does not match the configured Solana environment.")
	}

	// route.ts:208-222.
	confirmedSlot, err := autodepositCloseConfirmedSlot(ctx, h.Chain, canonical.CloseSignature)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "unconfirmed_signature", err.Error())
	}

	// route.ts:224-230.
	if canonical.ConfirmedSlot != confirmedSlot {
		return nil, earnError(http.StatusBadRequest, "slot_mismatch",
			"Confirmed autodeposit close slot does not match the transaction status.")
	}

	// route.ts:232-243.
	target, err := h.recordAutodepositCloseIntent(ctx, canonical)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "record_failed", err.Error())
	}
	return newAutodepositTargetOutput(target), nil
}

type autodepositFloorConfirmInput struct {
	SessionInput
	Raw rawJSON
}

func (h earnRoutes) registerAutodepositFloor(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-autodeposit-floor-confirm",
		Method:      http.MethodPost,
		Path:        autodepositFloorConfirmPath,
		Summary:     "Confirm an Earn autodeposit wallet balance floor update",
		Responses:   errorResponses[routeError](api, 400, 401, 503),
	}, h.autodepositFloorConfirm)
	documentJSONBody[AutodepositFloorConfirmRequest](api, autodepositFloorConfirmPath)
}

func (h earnRoutes) autodepositFloorConfirm(ctx context.Context, in *autodepositFloorConfirmInput) (*autodepositFloorConfirmOutput, error) {
	// route.ts:58-62.
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		return nil, earnUnavailable("load Earn account", err, "account_unavailable", "Earn account is unavailable.")
	}

	// route.ts:64-77.
	input, err := parseAutodepositFloorConfirmBody(in.Raw.data)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "invalid_request", err.Error())
	}

	// route.ts:80-87 — the wallet and settings come from the principal, never
	// from the body.
	input.Settings = account.Settings.String()
	input.WalletAddress = account.Wallet.String()
	result, err := h.updateAutodepositWalletBalanceFloor(ctx, input)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "update_failed", err.Error())
	}

	// Mobile parity (M/autodeposit/floor/confirm/route.ts:144-151, DECIDED
	// keep in pathmap.md:15): push the scheduled-sweep copy when the floor
	// rebaseline scheduled a sweep.
	if result.Rebaseline.Status == "scheduled" && result.Rebaseline.Sweep != nil {
		h.Push.send(ctx, account.Wallet.String(),
			autodepositSweepScheduledPush(uint64(result.Rebaseline.Sweep.RemainingAmountRaw)))
	}

	rebaseline := &AutodepositFloorRebaselineSweep{Status: result.Rebaseline.Status}
	if result.Rebaseline.Reason != nil {
		rebaseline.Reason = result.Rebaseline.Reason
	}
	if result.Rebaseline.Sweep != nil {
		rebaseline.Sweep = serializeAutodepositFloorSweep(result.Rebaseline.Sweep)
	}
	return &autodepositFloorConfirmOutput{Body: AutodepositFloorConfirmResponse{
		RebaselineSweep: rebaseline,
		Target:          serializeAutodepositTarget(result.Target),
	}}, nil
}

type autodepositFloorConfirmOutput struct {
	Body AutodepositFloorConfirmResponse
}

type autodepositToggleConfirmInput struct {
	SessionInput
	Raw rawJSON
}

func (h earnRoutes) registerAutodepositToggle(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-autodeposit-toggle-confirm",
		Method:      http.MethodPost,
		Path:        autodepositToggleConfirmPath,
		Summary:     "Confirm an Earn autodeposit enable or disable",
		Responses:   errorResponses[routeError](api, 400, 401, 503),
	}, h.autodepositToggleConfirm)
	documentJSONBody[AutodepositToggleConfirmRequest](api, autodepositToggleConfirmPath)
}

func (h earnRoutes) autodepositToggleConfirm(ctx context.Context, in *autodepositToggleConfirmInput) (*autodepositTargetOutput, error) {
	// route.ts:36-40.
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		return nil, earnUnavailable("load Earn account", err, "account_unavailable", "Earn account is unavailable.")
	}

	// route.ts:42-51.
	input, err := parseAutodepositToggleConfirmBody(in.Raw.data)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "invalid_request", err.Error())
	}

	// route.ts:54-61 — the wallet and settings come from the principal.
	input.Settings = account.Settings.String()
	input.WalletAddress = account.Wallet.String()
	target, err := h.updateAutodepositTargetActive(ctx, input)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "toggle_failed", err.Error())
	}

	// Mobile parity (M/autodeposit/toggle/confirm/route.ts:124-126, DECIDED
	// keep in pathmap.md:16): push the enabled copy when the target turns on.
	if input.Active {
		h.Push.send(ctx, account.Wallet.String(), autodepositEnabledPush())
	}
	return newAutodepositTargetOutput(target), nil
}

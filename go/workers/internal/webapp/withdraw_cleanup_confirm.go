package webapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
)

// The route proves the cleanup landed — zero balances, every prepared policy
// account closed, the client's slot matching the chain — and records the one
// close intent for the LaserStream projection; it never writes a money row
// (the single-writer check forbids recordConfirmedEarnCleanup here).
const earnCleanupConfirmPath = "/api/smart-accounts/yield-optimization/withdrawals/cleanup/confirm"

type earnCleanupConfirmInput struct {
	SessionInput
	Raw rawJSON
}

type earnCleanupConfirmOutput struct {
	Body EarnCleanupConfirmResponse
}

func (h earnRoutes) registerCleanupConfirm(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-earn-withdrawals-cleanup-confirm",
		Method:      http.MethodPost,
		Path:        earnCleanupConfirmPath,
		Summary:     "Confirm a sent Earn cleanup transaction",
		Responses:   errorResponses[routeError](api, 400, 401, 403, 409, 500, 503),
	}, h.earnCleanupConfirm)
	documentJSONBody[EarnCleanupConfirmRequest](api, earnCleanupConfirmPath)
}

func (h earnRoutes) earnCleanupConfirm(ctx context.Context, in *earnCleanupConfirmInput) (*earnCleanupConfirmOutput, error) {
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		if refused, ok := authRefusal(err); ok {
			return nil, refused
		}
		return nil, cleanupConfirmFailed(err)
	}
	body, err := cleanupConfirmVerify(ctx, h.Chain, h, account, in.Raw.data, time.Now)
	if err != nil {
		return nil, err
	}
	return &earnCleanupConfirmOutput{Body: *body}, nil
}

func cleanupConfirmFailed(err error) error {
	slog.Error("[earn-withdraw-cleanup-confirm] failed", "error", err)
	return earnError(http.StatusInternalServerError, "confirm_failed", err.Error())
}

func cleanupConfirmAnswer(err error) error {
	var refused *cleanupConfirmError
	if errors.As(err, &refused) {
		return earnError(refused.status, refused.code, refused.message)
	}
	return cleanupConfirmFailed(err)
}

type cleanupConfirmError struct {
	status  int
	code    string
	message string
}

func (e *cleanupConfirmError) Error() string { return e.message }

func cleanupRetryable(err error) *cleanupConfirmError {
	return &cleanupConfirmError{http.StatusServiceUnavailable, "full_exit_verification_retryable", err.Error()}
}

// cleanupConfirmBody parses the confirm request body. preparedCleanup stays
// the raw object the client echoed; the route reads only the persistence
// fields it proves against.
type cleanupConfirmBody struct {
	CleanupSignature              string
	ConfirmedSlot                 string
	PreparedCleanup               any
	AutodepositCloseSignature     *string
	AutodepositCloseConfirmedSlot *string
}

// cleanupConfirmRecord applies JavaScript's typing: a top-level array is an
// object whose field reads all miss, like Next's.
func cleanupConfirmRecord(raw []byte) (map[string]any, error) {
	parsed, err := parseJSONBody(raw)
	if err != nil {
		return nil, errors.New("Invalid request body.")
	}
	switch typed := parsed.(type) {
	case map[string]any:
		return typed, nil
	case []any:
		return map[string]any{}, nil
	}
	return nil, errors.New("Request body must be an object.")
}

func cleanupNonEmptyString(record map[string]any, key string) (string, error) {
	text, ok := record[key].(string)
	if !ok || strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("%s must be a non-empty string.", key)
	}
	return strings.TrimSpace(text), nil
}

func cleanupUnsignedString(record map[string]any, key string) (string, error) {
	text, ok := record[key].(string)
	if !ok || !cleanupDigits.MatchString(strings.TrimSpace(text)) {
		return "", fmt.Errorf("%s must be an unsigned integer string.", key)
	}
	return strings.TrimSpace(text), nil
}

func parseCleanupConfirmBody(raw []byte) (cleanupConfirmBody, error) {
	record, err := cleanupConfirmRecord(raw)
	if err != nil {
		return cleanupConfirmBody{}, err
	}
	preparedAny := record["preparedCleanup"]
	if _, isObject := preparedAny.(map[string]any); !isObject {
		if _, isArray := preparedAny.([]any); !isArray {
			return cleanupConfirmBody{}, errors.New("preparedCleanup must be an object.")
		}
	}
	signature, err := cleanupNonEmptyString(record, "cleanupSignature")
	if err != nil {
		return cleanupConfirmBody{}, err
	}
	slot, err := cleanupUnsignedString(record, "confirmedSlot")
	if err != nil {
		return cleanupConfirmBody{}, err
	}
	body := cleanupConfirmBody{CleanupSignature: signature, ConfirmedSlot: slot, PreparedCleanup: preparedAny}
	if text, ok := record["autodepositCloseSignature"].(string); ok && strings.TrimSpace(text) != "" {
		trimmed := strings.TrimSpace(text)
		body.AutodepositCloseSignature = &trimmed
	}
	if text, ok := record["autodepositCloseConfirmedSlot"].(string); ok && cleanupDigits.MatchString(strings.TrimSpace(text)) {
		trimmed := strings.TrimSpace(text)
		body.AutodepositCloseConfirmedSlot = &trimmed
	}
	return body, nil
}

func cleanupWireText(object map[string]any, key string) (string, bool) {
	text, ok := object[key].(string)
	return text, ok
}

// cleanupWireOptionalText reads one optional persistence string: absent and
// null are the same null; a present non-string keeps its own type, which no
// DB string or null ever equals.
func cleanupWireOptionalText(object map[string]any, key string) *string {
	value, present := object[key]
	if !present || value == nil {
		return nil
	}
	if text, ok := value.(string); ok {
		return &text
	}
	alien := "\x00not-a-string:" + key
	return &alien
}

// cleanupConfirmPersistence is the echoed preparedCleanup.persistence; nil
// when the client sent a shape with no object there (Next dies on the same
// bodies with a TypeError, answered as the catch-all's 500).
func cleanupConfirmPersistence(body cleanupConfirmBody) map[string]any {
	prepared, _ := body.PreparedCleanup.(map[string]any)
	persistence, _ := prepared["persistence"].(map[string]any)
	return persistence
}

func cleanupConfirmedSignatureSlot(ctx context.Context, client withdrawConfirmChain, signature string) (uint64, error) {
	state, err := client.SignatureState(ctx, signature)
	if err != nil {
		return 0, cleanupRetryable(err)
	}
	if state.Found && state.Err != "" {
		return 0, &cleanupConfirmError{http.StatusBadRequest, "cleanup_transaction_failed", "Earn cleanup transaction failed on-chain."}
	}
	if state.Found && (state.Commitment == chain.Confirmed || state.Commitment == chain.Finalized) {
		return state.Slot, nil
	}
	sig, err := solana.SignatureFromBase58(signature)
	if err != nil {
		return 0, cleanupRetryable(err)
	}
	execution, err := client.Execution(ctx, sig, rpc.CommitmentConfirmed)
	if errors.Is(err, chain.ErrNotFound) {
		return 0, cleanupRetryable(errors.New("Confirmed transaction slot is unavailable."))
	}
	if err != nil {
		return 0, cleanupRetryable(err)
	}
	if execution.Err != nil {
		return 0, &cleanupConfirmError{http.StatusBadRequest, "cleanup_transaction_failed", "Earn cleanup transaction failed on-chain."}
	}
	if execution.Slot != 0 {
		return execution.Slot, nil
	}
	return 0, cleanupRetryable(errors.New("Confirmed transaction slot is unavailable."))
}

func cleanupPolicyAccountsClosed(ctx context.Context, client withdrawConfirmChain, accounts []string, minContextSlot uint64) error {
	keys := make([]solana.PublicKey, 0, len(accounts))
	for _, text := range accounts {
		key, err := solana.PublicKeyFromBase58(text)
		if err != nil {
			return cleanupRetryable(err)
		}
		keys = append(keys, key)
	}
	slot, infos, err := client.Accounts(ctx, keys, rpc.CommitmentConfirmed, minContextSlot)
	if err != nil {
		return cleanupRetryable(err)
	}
	if slot < minContextSlot {
		return cleanupRetryable(errors.New("Earn policy close proof was observed before the cleanup confirmation slot."))
	}
	for _, info := range infos {
		if info != nil {
			return cleanupRetryable(errors.New("One or more Earn policy accounts remain open on-chain."))
		}
	}
	return nil
}

func cleanupAssertFullExitProven(ctx context.Context, client withdrawConfirmChain, now func() time.Time, cluster string, state *RoutePolicyPair, settings solana.PublicKey, policyAccounts []string, minContextSlot uint64) error {
	proof, err := verifyFullExitZeroBalances(ctx, client, withdrawSleep, now, cluster, cleanupPolicyUniverse(cluster, state.Route), settings, minContextSlot)
	if err != nil {
		return cleanupRetryable(err)
	}
	if proof.Status != "policy_close_required" {
		return &cleanupConfirmError{http.StatusConflict, "full_exit_incomplete", "Earn balances remain after cleanup; the position stays active."}
	}
	return cleanupPolicyAccountsClosed(ctx, client, policyAccounts, minContextSlot)
}

func cleanupConfirmMetadataMatches(state *RoutePolicyPair, persistence map[string]any) bool {
	var setupAccount, setupSeed *string
	if state.Setup != nil {
		seed := strconv.FormatInt(state.Setup.PolicySeed, 10)
		setupAccount, setupSeed = &state.Setup.PolicyAccount, &seed
	}
	routeAccount, routeAccountOK := cleanupWireText(persistence, "policyAccount")
	routeSeed, routeSeedOK := cleanupWireText(persistence, "policySeed")
	return routeAccountOK && routeAccount == state.Route.PolicyAccount &&
		routeSeedOK && routeSeed == strconv.FormatInt(state.Route.PolicySeed, 10) &&
		cleanupOptionalTextEqual(cleanupWireOptionalText(persistence, "setupPolicyAccount"), setupAccount) &&
		cleanupOptionalTextEqual(cleanupWireOptionalText(persistence, "setupPolicySeed"), setupSeed)
}

func cleanupOptionalTextEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

type cleanupConfirmStore interface {
	findCleanupVaultState(ctx context.Context, includeInactive bool, authority, settings string, vault solana.PublicKey) (*RoutePolicyPair, error)
	recordAutodepositCloseIntent(ctx context.Context, in autodepositCloseInput) (autodepositTargetRow, error)
}

// cleanupConfirmSlotText compares the client's decimal slot with the
// resolved one at BigInt precision: digit strings beyond uint64 never match.
func cleanupConfirmSlotText(text string, slot uint64) int {
	client, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return -1
	}
	return client.Cmp(new(big.Int).SetUint64(slot))
}

func cleanupConfirmVerify(ctx context.Context, client withdrawConfirmChain, store cleanupConfirmStore, account EarnAccount, raw []byte, now func() time.Time) (*EarnCleanupConfirmResponse, error) {
	body, err := parseCleanupConfirmBody(raw)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "invalid_request", err.Error())
	}
	persistence := cleanupConfirmPersistence(body)
	if persistence == nil {
		return nil, cleanupConfirmFailed(errors.New("Prepared cleanup persistence is invalid."))
	}
	wallet, _ := cleanupWireText(persistence, "walletAddress")
	settings, _ := cleanupWireText(persistence, "settings")
	vaultIndex, isNumber := persistence["vaultIndex"].(float64)
	if wallet != account.Wallet.String() || settings != account.Settings.String() || !isNumber || vaultIndex != float64(earnprep.EarnVaultIndex) {
		return nil, earnError(http.StatusForbidden, "cleanup_owner_mismatch", "Prepared cleanup does not belong to the authenticated wallet.")
	}
	cluster, _ := cleanupWireText(persistence, "cluster")
	if cluster != string(account.Cluster) {
		return nil, earnError(http.StatusBadRequest, "cluster_mismatch", "Prepared cleanup cluster does not match the configured Solana environment.")
	}
	confirmedSlot, err := cleanupConfirmedSignatureSlot(ctx, client, body.CleanupSignature)
	if err != nil {
		return nil, cleanupConfirmAnswer(err)
	}
	if cleanupConfirmSlotText(body.ConfirmedSlot, confirmedSlot) != 0 {
		return nil, earnError(http.StatusBadRequest, "slot_mismatch", "Confirmed Earn cleanup slot does not match the transaction status.")
	}
	vaultPubkey, ok := cleanupWireText(persistence, "vaultPubkey")
	vault, parseErr := solana.PublicKeyFromBase58(vaultPubkey)
	if !ok || parseErr != nil {
		return nil, earnError(http.StatusConflict, "missing_earn_policy", "Earn policy state is unavailable for cleanup confirmation.")
	}
	state, err := store.findCleanupVaultState(ctx, true, wallet, settings, vault)
	if err != nil {
		return nil, cleanupConfirmFailed(err)
	}
	if state == nil {
		return nil, earnError(http.StatusConflict, "missing_earn_policy", "Earn policy state is unavailable for cleanup confirmation.")
	}
	if !cleanupConfirmMetadataMatches(state, persistence) {
		return nil, earnError(http.StatusConflict, "cleanup_policy_mismatch", "Prepared cleanup policy metadata does not match the persisted Earn policy.")
	}
	closeMeta, _ := persistence["autodepositClose"].(map[string]any)
	adSignature, adSlot := body.AutodepositCloseSignature, body.AutodepositCloseConfirmedSlot
	if closeMeta != nil && (adSignature == nil || adSlot == nil) {
		return nil, earnError(http.StatusBadRequest, "missing_autodeposit_close", "Autodeposit close confirmation is required before Earn cleanup.")
	}
	if closeMeta != nil && adSignature != nil && adSlot != nil {
		slot, err := cleanupConfirmedSignatureSlot(ctx, client, *adSignature)
		if err != nil {
			return nil, cleanupConfirmAnswer(err)
		}
		if cleanupConfirmSlotText(*adSlot, slot) != 0 {
			return nil, earnError(http.StatusBadRequest, "autodeposit_close_slot_mismatch", "Confirmed Autodeposit close slot does not match the transaction status.")
		}
	}
	if confirmedSlot > 1<<53-1 {
		return nil, earnError(http.StatusBadRequest, "invalid_confirmed_slot", "Confirmed Earn cleanup slot is outside the supported range.")
	}
	policyAccounts := []string{state.Route.PolicyAccount}
	if setupAccount := cleanupWireOptionalText(persistence, "setupPolicyAccount"); setupAccount != nil {
		policyAccounts = append(policyAccounts, *setupAccount)
	}
	if closeMeta != nil {
		if text, ok := cleanupWireText(closeMeta, "policyAccount"); ok {
			policyAccounts = append(policyAccounts, text)
		}
	}
	if err := cleanupAssertFullExitProven(ctx, client, now, cluster, state, account.Settings, policyAccounts, confirmedSlot); err != nil {
		return nil, cleanupConfirmAnswer(err)
	}
	if closeMeta != nil && adSignature != nil && adSlot != nil {
		closeInput, err := cleanupCloseIntentInput(cluster, closeMeta, *adSignature, *adSlot, settings, vaultPubkey, wallet)
		if err != nil {
			return nil, cleanupConfirmFailed(err)
		}
		if _, err := store.recordAutodepositCloseIntent(ctx, closeInput); err != nil {
			return nil, cleanupConfirmFailed(err)
		}
	}
	return &EarnCleanupConfirmResponse{OK: true, Status: "full_exit_closed"}, nil
}

// cleanupCloseIntentInput maps the echoed autodepositClose persistence into
// the close-intent write; a field that is not a string is a plain confirm
// failure, where Next feeds the undefined into the write and fails the same
// catch-all.
func cleanupCloseIntentInput(cluster string, closeMeta map[string]any, signature, slotText, settings, vaultPubkey, wallet string) (autodepositCloseInput, error) {
	delegatedSigner, ok := cleanupWireText(closeMeta, "delegatedSigner")
	if !ok {
		return autodepositCloseInput{}, errors.New("Prepared cleanup autodeposit close is missing delegatedSigner.")
	}
	policyAccount, ok := cleanupWireText(closeMeta, "policyAccount")
	if !ok {
		return autodepositCloseInput{}, errors.New("Prepared cleanup autodeposit close is missing policyAccount.")
	}
	recurringDelegation, ok := cleanupWireText(closeMeta, "recurringDelegation")
	if !ok {
		return autodepositCloseInput{}, errors.New("Prepared cleanup autodeposit close is missing recurringDelegation.")
	}
	slot, ok := new(big.Int).SetString(slotText, 10)
	if !ok || !slot.IsUint64() {
		return autodepositCloseInput{}, errors.New("Confirmed Autodeposit close slot is outside the supported range.")
	}
	return autodepositCloseInput{
		Cluster:             cluster,
		CloseSignature:      signature,
		ConfirmedSlot:       slot.Uint64(),
		DelegatedSigner:     delegatedSigner,
		PolicyAccount:       policyAccount,
		RecurringDelegation: recurringDelegation,
		Settings:            settings,
		VaultIndex:          int(earnprep.EarnVaultIndex),
		VaultPubkey:         vaultPubkey,
		WalletAddress:       wallet,
	}, nil
}

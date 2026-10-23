package webapp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// POST /api/smart-accounts/yield-optimization/policies/confirm ports
// web/apps/web/src/app/api/smart-accounts/yield-optimization/policies/confirm/route.ts:
// after the client sends the policy-create transactions from policies/prepare,
// it confirms one of them here. The route checks the body against the
// canonical policy metadata derived from the Settings, waits for the claimed
// signature to reach confirmed on RPC and returns the canonical policy
// reference. No database access, nothing signed, nothing sent.
const earnPoliciesConfirmPath = "/api/smart-accounts/yield-optimization/policies/confirm"

// CONFIRMED_SIGNATURE_STATUS_ATTEMPTS, _RETRY_MS and _RETRY_MAX_MS
// (route.ts:20-22): the RPC status index can trail the browser, so the
// request stays open through the same bounded lag window (ASK-2005).
const (
	confirmSignatureAttempts = 12
	confirmSignatureRetryMS  = 350 * time.Millisecond
	confirmSignatureMaxWait  = 5000 * time.Millisecond
)

const (
	confirmStageRoute = "route_policy"
	confirmStageSetup = "setup_policy"
)

// EarnPolicyConfirmPolicy is serializeVerifiedPolicy's shape
// (route.ts:252-273): ids and seeds are decimal strings, vaultIndex a number.
type EarnPolicyConfirmPolicy struct {
	Account     string `json:"account"`
	ID          string `json:"id"`
	Seed        string `json:"seed"`
	VaultIndex  uint8  `json:"vaultIndex"`
	VaultPubkey string `json:"vaultPubkey"`
}

type EarnPolicyConfirmResponse struct {
	Policy EarnPolicyConfirmPolicy `json:"policy"`
}

// EarnPolicyConfirmRequest documents the request body for OpenAPI; the
// handler parses the raw bytes so the TS error messages carry over verbatim.
type EarnPolicyConfirmRequest struct {
	Cluster                  *string `json:"cluster,omitempty"`
	ConfirmedSlot            *string `json:"confirmedSlot,omitempty"`
	DelegatedSigner          *string `json:"delegatedSigner,omitempty"`
	LiquidityMint            *string `json:"liquidityMint,omitempty"`
	Market                   *string `json:"market,omitempty"`
	PolicyAccount            *string `json:"policyAccount,omitempty"`
	PolicyID                 *string `json:"policyId,omitempty"`
	PolicySeed               *string `json:"policySeed,omitempty"`
	PolicySignature          *string `json:"policySignature,omitempty"`
	Stage                    *string `json:"stage,omitempty" enum:"route_policy,setup_policy"`
	SetupPolicyAccount       *string `json:"setupPolicyAccount,omitempty"`
	SetupPolicyConfirmedSlot *string `json:"setupPolicyConfirmedSlot,omitempty"`
	SetupPolicyID            *string `json:"setupPolicyId,omitempty"`
	SetupPolicySeed          *string `json:"setupPolicySeed,omitempty"`
	SetupPolicySignature     *string `json:"setupPolicySignature,omitempty"`
	Settings                 *string `json:"settings,omitempty"`
	TargetReserve            *string `json:"targetReserve,omitempty"`
	VaultIndex               *int    `json:"vaultIndex,omitempty"`
	VaultPubkey              *string `json:"vaultPubkey,omitempty"`
	WalletAddress            *string `json:"walletAddress,omitempty"`
}

type earnPoliciesConfirmInput struct {
	SessionInput
	Raw rawJSON
}

type earnPoliciesConfirmOutput struct {
	Body EarnPolicyConfirmResponse
}

func (h earnRoutes) registerPoliciesConfirm(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-earn-policies-confirm",
		Method:      http.MethodPost,
		Path:        earnPoliciesConfirmPath,
		Summary:     "Confirm an Earn policy creation transaction",
		Responses:   errorResponses[routeError](api, 400, 401, 403, 503),
	}, h.earnPoliciesConfirm)
	documentJSONBody[EarnPolicyConfirmRequest](api, earnPoliciesConfirmPath)
}

func (h earnRoutes) earnPoliciesConfirm(ctx context.Context, in *earnPoliciesConfirmInput) (*earnPoliciesConfirmOutput, error) {
	// Next order (route.ts:275-329): principal, body, principal match,
	// canonical metadata, configured cluster.
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		if refused, ok := authRefusal(err); ok {
			return nil, refused
		}
		return nil, earnUnavailable("policy confirm principal", err, "policy_confirm_unavailable", "The Earn policy confirm is temporarily unavailable.")
	}
	body, err := parseEarnPolicyConfirmBody(in.Raw.data)
	if err != nil {
		return nil, err
	}
	if body.WalletAddress != account.Wallet.String() || body.Settings != account.Settings.String() {
		return nil, earnError(http.StatusForbidden, "principal_mismatch", "Confirmed yield policy does not match the authenticated wallet session.")
	}
	canonical, err := depositCanonicalPolicyInput(body)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "metadata_mismatch", confirmErrorText(err, "Confirmed yield policy metadata is invalid."))
	}
	if canonical.Cluster != string(account.Cluster) {
		return nil, earnError(http.StatusBadRequest, "cluster_mismatch", "Confirmed yield policy cluster does not match the configured Solana environment.")
	}

	signatureToVerify := body.PolicySignature
	expectedSlot := body.ConfirmedSlot
	operation := "route policy setup"
	unconfirmedCode := "unconfirmed_signature"
	slotMismatchCode := "slot_mismatch"
	slotMismatchMessage := "Confirmed yield policy slot does not match the transaction status."
	if body.Stage == confirmStageSetup {
		signatureToVerify = *body.SetupPolicySignature
		expectedSlot = body.SetupPolicyConfirmedSlot
		operation = "setup policy setup"
		unconfirmedCode = "unconfirmed_setup_policy_signature"
		slotMismatchCode = "setup_policy_slot_mismatch"
		slotMismatchMessage = "Confirmed setup policy slot does not match the transaction status."
	}
	confirmedSlot, err := depositConfirmedSignatureSlot(ctx, h.Chain, operation, signatureToVerify)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, unconfirmedCode, confirmErrorText(err, "Policy setup transaction is not confirmed."))
	}
	if expectedSlot.Cmp(confirmedSlot) != 0 {
		return nil, earnError(http.StatusBadRequest, slotMismatchCode, slotMismatchMessage)
	}

	setup := body.Stage == confirmStageSetup
	policy := EarnPolicyConfirmPolicy{
		Account:     canonical.PolicyAccount,
		ID:          canonical.PolicyID.String(),
		Seed:        canonical.PolicySeed.String(),
		VaultIndex:  canonical.VaultIndex,
		VaultPubkey: canonical.VaultPubkey,
	}
	if setup {
		policy.Account = canonical.SetupPolicyAccount
		policy.ID = canonical.SetupPolicyID.String()
		policy.Seed = canonical.SetupPolicySeed.String()
	}
	return &earnPoliciesConfirmOutput{Body: EarnPolicyConfirmResponse{Policy: policy}}, nil
}

// confirmErrorText mirrors `error instanceof Error ? error.message : fallback`
// at the route's two catch sites.
func confirmErrorText(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	if message := err.Error(); message != "" {
		return message
	}
	return fallback
}

// earnPolicyConfirmBody is parseEarnPolicyConfirmRequestBody's output
// (earn-confirm-contracts.shared.ts:447-479) plus the stage it derives. Whole
// numbers ride in big.Int because the TS body carries BigInts that only later
// steps bound to the safe-integer range.
type earnPolicyConfirmBody struct {
	Cluster                  string
	ConfirmedSlot            *big.Int
	DelegatedSigner          string
	LiquidityMint            string
	Market                   *string
	PolicyAccount            string
	PolicyID                 *big.Int
	PolicySeed               *big.Int
	PolicySignature          string
	Stage                    string
	SetupPolicyAccount       *string
	SetupPolicyConfirmedSlot *big.Int
	SetupPolicyID            *big.Int
	SetupPolicySeed          *big.Int
	SetupPolicySignature     *string
	Settings                 string
	TargetReserve            string
	VaultIndex               float64
	VaultPubkey              string
	WalletAddress            string
}

var confirmUintPattern = regexp.MustCompile(`^\d+$`)

// confirmInvalidBody is the shared 400 envelope every parser refusal below
// maps to (route.ts:285-293).
func confirmInvalidBody(message string) error {
	return earnError(http.StatusBadRequest, "invalid_request", message)
}

// confirmRequiredString is readRequiredString (contracts.shared.ts:120-129).
func confirmRequiredString(record map[string]any, key string) (string, error) {
	value, ok := record[key].(string)
	if !ok || strings.TrimSpace(value) == "" {
		return "", confirmInvalidBody(key + " must be a non-empty string.")
	}
	return strings.TrimSpace(value), nil
}

// confirmOptionalString is readOptionalString (contracts.shared.ts:131-144):
// absent, null and blank all collapse to null.
func confirmOptionalString(record map[string]any, key string) (*string, error) {
	value, present := record[key]
	if !present || value == nil {
		return nil, nil
	}
	text, ok := value.(string)
	if !ok {
		return nil, confirmInvalidBody(key + " must be a string when provided.")
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, nil
	}
	return &trimmed, nil
}

func confirmBigInt(text, key string) (*big.Int, error) {
	if !confirmUintPattern.MatchString(text) {
		return nil, confirmInvalidBody(key + " must be an unsigned integer string.")
	}
	value, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return nil, confirmInvalidBody(key + " must be an unsigned integer string.")
	}
	return value, nil
}

// confirmBigIntString is readBigIntString (contracts.shared.ts:146-152).
func confirmBigIntString(record map[string]any, key string) (*big.Int, error) {
	text, err := confirmRequiredString(record, key)
	if err != nil {
		return nil, err
	}
	return confirmBigInt(text, key)
}

// confirmOptionalBigIntString is readOptionalBigIntString
// (contracts.shared.ts:154-166).
func confirmOptionalBigIntString(record map[string]any, key string) (*big.Int, error) {
	text, err := confirmOptionalString(record, key)
	if err != nil || text == nil {
		return nil, err
	}
	return confirmBigInt(*text, key)
}

// confirmVaultIndex is readVaultIndex (contracts.shared.ts:168-179); JSON
// numbers decode as float64, and Number.isInteger rejects the fractions.
func confirmVaultIndex(record map[string]any) (float64, error) {
	value, ok := record["vaultIndex"].(float64)
	if !ok || value != math.Trunc(value) || value < 0 || value > 32767 {
		return 0, confirmInvalidBody("vaultIndex must be an integer between 0 and 32767.")
	}
	return value, nil
}

// confirmStage is readPolicyConfirmStage (contracts.shared.ts:191-204): the
// explicit stage first, else the setup signature's presence decides.
func confirmStage(record map[string]any) (string, error) {
	value, err := confirmOptionalString(record, "stage")
	if err != nil {
		return "", err
	}
	if value == nil {
		setupSignature, err := confirmOptionalString(record, "setupPolicySignature")
		if err != nil {
			return "", err
		}
		if setupSignature != nil {
			return confirmStageSetup, nil
		}
		return confirmStageRoute, nil
	}
	if *value != confirmStageRoute && *value != confirmStageSetup {
		return "", confirmInvalidBody("stage must be route_policy or setup_policy.")
	}
	return *value, nil
}

// parseEarnPolicyConfirmBody ports parseEarnPolicyConfirmRequestBody: fields
// validate in the TS object literal's evaluation order, stage first, so the
// first refusal's message matches what Next returns.
func parseEarnPolicyConfirmBody(raw []byte) (earnPolicyConfirmBody, error) {
	parsed, err := parseJSONBody(raw)
	if err != nil {
		return earnPolicyConfirmBody{}, err
	}
	record := map[string]any{}
	switch value := parsed.(type) {
	case map[string]any:
		record = value
	case []any:
		// An array passes TS's typeof-object guard and then misses every
		// field, so it dies on the first required-string check like Next.
	default:
		return earnPolicyConfirmBody{}, confirmInvalidBody("Request body must be an object.")
	}

	var body earnPolicyConfirmBody
	if body.Stage, err = confirmStage(record); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.Cluster, err = confirmRequiredString(record, "cluster"); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.ConfirmedSlot, err = confirmBigIntString(record, "confirmedSlot"); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.DelegatedSigner, err = confirmRequiredString(record, "delegatedSigner"); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.LiquidityMint, err = confirmRequiredString(record, "liquidityMint"); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.Market, err = confirmOptionalString(record, "market"); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.PolicyAccount, err = confirmRequiredString(record, "policyAccount"); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.PolicyID, err = confirmBigIntString(record, "policyId"); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.PolicySeed, err = confirmBigIntString(record, "policySeed"); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.PolicySignature, err = confirmRequiredString(record, "policySignature"); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.SetupPolicyAccount, err = confirmOptionalString(record, "setupPolicyAccount"); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.SetupPolicyConfirmedSlot, err = confirmOptionalBigIntString(record, "setupPolicyConfirmedSlot"); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.SetupPolicyID, err = confirmOptionalBigIntString(record, "setupPolicyId"); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.SetupPolicySeed, err = confirmOptionalBigIntString(record, "setupPolicySeed"); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.SetupPolicySignature, err = confirmOptionalString(record, "setupPolicySignature"); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.Settings, err = confirmRequiredString(record, "settings"); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.TargetReserve, err = confirmRequiredString(record, "targetReserve"); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.VaultIndex, err = confirmVaultIndex(record); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.VaultPubkey, err = confirmRequiredString(record, "vaultPubkey"); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	if body.WalletAddress, err = confirmRequiredString(record, "walletAddress"); err != nil {
		return earnPolicyConfirmBody{}, err
	}
	return body, nil
}

// earnPolicyCanonical is the canonical input createCanonicalPolicyInput
// returns (route.ts:93-107): the body fields replaced by what the Settings
// PDA actually derives.
type earnPolicyCanonical struct {
	Cluster            string
	PolicyAccount      string
	PolicyID           *big.Int
	PolicySeed         *big.Int
	SetupPolicyAccount string
	SetupPolicyID      *big.Int
	SetupPolicySeed    *big.Int
	VaultIndex         uint8
	VaultPubkey        string
}

// confirmPublicKey is `new PublicKey(...)`: any decode failure carries web3's
// own message, which the route surfaces inside metadata_mismatch.
func confirmPublicKey(raw string) (solana.PublicKey, error) {
	key, err := solana.PublicKeyFromBase58(raw)
	if err != nil {
		return solana.PublicKey{}, errors.New("Invalid public key input")
	}
	return key, nil
}

// confirmNormalizeCluster is normalizeLoyalCluster (loyal-actions cluster.ts:46-58).
func confirmNormalizeCluster(value string) (string, error) {
	switch value {
	case "devnet":
		return string(LoyalDevnet), nil
	case "mainnet-beta", "mainnet":
		return string(LoyalMainnetBeta), nil
	}
	return "", fmt.Errorf("unsupported Loyal cluster: %s", value)
}

// confirmSafePolicySeed is toSafePolicySeed (route.ts:50-56): the wire and
// the PDA seeds cap a policy seed at the JSON safe-integer range.
func confirmSafePolicySeed(seed *big.Int) (uint64, error) {
	maxSafe := new(big.Int).SetUint64(depositMaxPolicySeed)
	if seed.Sign() <= 0 || seed.Cmp(maxSafe) > 0 {
		return 0, errors.New("policySeed is outside the supported earn policy range.")
	}
	return seed.Uint64(), nil
}

// confirmCanonicalMismatch is assertCanonicalField (route.ts:38-48).
func confirmCanonicalMismatch(label string) error {
	return fmt.Errorf("%s does not match the canonical earn policy metadata.", label)
}

// depositCanonicalPolicyInput ports createCanonicalPolicyInput
// (route.ts:58-183): derive every account from the Settings, validate the
// reserve the way deposit confirm does, then assert each claimed field.
func depositCanonicalPolicyInput(request earnPolicyConfirmBody) (earnPolicyCanonical, error) {
	cluster, err := confirmNormalizeCluster(request.Cluster)
	if err != nil {
		return earnPolicyCanonical{}, err
	}
	settings, err := confirmPublicKey(request.Settings)
	if err != nil {
		return earnPolicyCanonical{}, err
	}
	expectedSetupPolicySeed := new(big.Int).Add(request.PolicySeed, big.NewInt(1))
	seed, err := confirmSafePolicySeed(request.PolicySeed)
	if err != nil {
		return earnPolicyCanonical{}, err
	}
	setupSeed, err := confirmSafePolicySeed(expectedSetupPolicySeed)
	if err != nil {
		return earnPolicyCanonical{}, err
	}
	expectedPolicyAccount, _, err := squads.PolicyAddress(settings, seed)
	if err != nil {
		return earnPolicyCanonical{}, err
	}
	expectedSetupPolicyAccount, _, err := squads.PolicyAddress(settings, setupSeed)
	if err != nil {
		return earnPolicyCanonical{}, err
	}
	expectedVault, _, err := squads.SmartAccountAddress(settings, earnVaultIndex)
	if err != nil {
		return earnPolicyCanonical{}, err
	}

	// Prepare routes mint to the best Safe reserve rather than one fixed
	// market, so the reserve validates like deposit confirm: a supported
	// product mint and a Safe-universe market (route.ts:78-92).
	universe, err := depositEarnUniverseFor(LoyalCluster(cluster))
	if err != nil {
		return earnPolicyCanonical{}, err
	}
	productMint, err := confirmPublicKey(request.LiquidityMint)
	if err != nil {
		return earnPolicyCanonical{}, err
	}
	if !depositConfirmContainsKey(universe.StableMints, productMint) {
		return earnPolicyCanonical{}, errors.New("mint is not a supported Earn product mint.")
	}
	if request.Market == nil || !depositConfirmContainsString(depositKeyStrings(universe.Markets), *request.Market) {
		return earnPolicyCanonical{}, errors.New("Earn reserve market is not in the Safe universe.")
	}
	if _, err := confirmPublicKey(request.TargetReserve); err != nil {
		return earnPolicyCanonical{}, err
	}

	canonical := earnPolicyCanonical{
		Cluster:            cluster,
		PolicyAccount:      expectedPolicyAccount.String(),
		PolicyID:           new(big.Int).Set(request.PolicySeed),
		PolicySeed:         new(big.Int).Set(request.PolicySeed),
		SetupPolicyAccount: expectedSetupPolicyAccount.String(),
		SetupPolicyID:      new(big.Int).Set(expectedSetupPolicySeed),
		SetupPolicySeed:    new(big.Int).Set(expectedSetupPolicySeed),
		VaultIndex:         earnVaultIndex,
		VaultPubkey:        expectedVault.String(),
	}

	// assertCanonicalField order (route.ts:109-180). The liquidityMint,
	// market and targetReserve assertions compare the request value against
	// itself (assertSafeEarnReserveMetadata passes them through unchanged),
	// so unlike the rest they cannot fire.
	if request.Cluster != canonical.Cluster {
		return earnPolicyCanonical{}, confirmCanonicalMismatch("cluster")
	}
	if request.PolicyAccount != canonical.PolicyAccount {
		return earnPolicyCanonical{}, confirmCanonicalMismatch("policyAccount")
	}
	if request.PolicyID.Cmp(request.PolicySeed) != 0 {
		return earnPolicyCanonical{}, confirmCanonicalMismatch("policyId")
	}
	if request.PolicyID.Cmp(canonical.PolicyID) != 0 {
		return earnPolicyCanonical{}, confirmCanonicalMismatch("policyId")
	}
	if request.PolicySeed.Cmp(canonical.PolicySeed) != 0 {
		return earnPolicyCanonical{}, confirmCanonicalMismatch("policySeed")
	}
	if request.Stage == confirmStageSetup {
		if request.SetupPolicySignature == nil {
			return earnPolicyCanonical{}, errors.New("setupPolicySignature is required for policy setup.")
		}
		if request.SetupPolicyConfirmedSlot == nil {
			return earnPolicyCanonical{}, errors.New("setupPolicyConfirmedSlot is required for policy setup.")
		}
		if request.SetupPolicyAccount == nil || *request.SetupPolicyAccount != canonical.SetupPolicyAccount {
			return earnPolicyCanonical{}, confirmCanonicalMismatch("setupPolicyAccount")
		}
		if request.SetupPolicyID == nil || request.SetupPolicyID.Cmp(canonical.SetupPolicyID) != 0 {
			return earnPolicyCanonical{}, confirmCanonicalMismatch("setupPolicyId")
		}
		if request.SetupPolicySeed == nil || request.SetupPolicySeed.Cmp(canonical.SetupPolicySeed) != 0 {
			return earnPolicyCanonical{}, confirmCanonicalMismatch("setupPolicySeed")
		}
	}
	if request.VaultIndex != float64(canonical.VaultIndex) {
		return earnPolicyCanonical{}, confirmCanonicalMismatch("vaultIndex")
	}
	if request.VaultPubkey != canonical.VaultPubkey {
		return earnPolicyCanonical{}, confirmCanonicalMismatch("vaultPubkey")
	}
	return canonical, nil
}

func depositConfirmContainsKey(keys []solana.PublicKey, key solana.PublicKey) bool {
	for _, candidate := range keys {
		if candidate == key {
			return true
		}
	}
	return false
}

func depositConfirmContainsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// depositConfirmedSignatureSlot is resolveConfirmedSignatureSlot
// (route.ts:202-250): poll getSignatureStatuses until the transaction reaches
// confirmed, backoff capped at 5s, refusing a transaction that failed or
// never landed inside the lag window.
func depositConfirmedSignatureSlot(ctx context.Context, client *chain.Client, operation, signature string) (*big.Int, error) {
	for attempt := 0; attempt < confirmSignatureAttempts; attempt++ {
		state, err := client.SignatureState(ctx, signature)
		if err != nil {
			return nil, err
		}
		if state.Err != "" {
			return nil, fmt.Errorf("%s transaction failed on-chain.", operation)
		}
		if state.Found && (state.Commitment == chain.Confirmed || state.Commitment == chain.Finalized) {
			return new(big.Int).SetUint64(state.Slot), nil
		}
		if attempt < confirmSignatureAttempts-1 {
			delay := confirmSignatureRetryMS << uint(attempt)
			if delay > confirmSignatureMaxWait || delay <= 0 {
				delay = confirmSignatureMaxWait
			}
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, fmt.Errorf("%s transaction is not confirmed.", operation)
}

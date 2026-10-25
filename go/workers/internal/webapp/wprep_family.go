package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
)

// POST /api/smart-accounts/yield-optimization/withdrawals/prepare ports
// web/apps/web/src/app/api/smart-accounts/yield-optimization/withdrawals/prepare/route.ts
// for mainnet idle and partial reserve withdrawals. Devnet and the legacy
// full exit stay on Next and answer 503 earn_withdraw_unported here.
const earnWprepPath = "/api/smart-accounts/yield-optimization/withdrawals/prepare"

var earnWprepFamily = earnFamily{
	name:        "earn withdraw prepare",
	appGrants:   AuthGrants,
	yieldGrants: EarnPolicyGrants,
	ready: func(h earnRoutes) bool {
		return h.signedIn() && h.YieldDB != nil && h.Chain != nil && h.HTTP != nil && !h.PolicySigner.IsZero()
	},
	register: earnRoutes.registerWprep,
}

// EarnWithdrawPrepareRequest documents the body; parseWprepBody reads it with
// the TS parser's own coercions (earn-withdraw-prepare-contracts.shared.ts).
type EarnWithdrawPrepareRequest struct {
	AmountRaw string                    `json:"amountRaw" doc:"Unsigned integer string, or \"max\""`
	SourceID  *string                   `json:"sourceId,omitempty"`
	Mode      *string                   `json:"mode,omitempty" enum:"partial,full"`
	Source    *EarnWithdrawLegacySource `json:"source,omitempty"`
}

type EarnWithdrawLegacySource struct {
	AmountRaw     *string `json:"amountRaw,omitempty"`
	ID            string  `json:"id"`
	LiquidityMint *string `json:"liquidityMint,omitempty"`
	Market        *string `json:"market,omitempty"`
	Mint          *string `json:"mint,omitempty"`
	Reserve       *string `json:"reserve,omitempty"`
	TokenAccount  *string `json:"tokenAccount,omitempty"`
	Type          string  `json:"type" enum:"reserve,idle"`
}

type wprepRequestInput struct {
	SessionInput
	Raw rawJSON
}

type wprepOutput struct {
	Body EarnWithdrawPrepareResponse
}

func (h earnRoutes) registerWprep(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-earn-withdrawals-prepare",
		Method:      http.MethodPost,
		Path:        earnWprepPath,
		Summary:     "Prepare an Earn withdrawal for the signed-in wallet",
		Responses:   errorResponses[routeError](api, 400, 401, 409, 500, 503),
	}, h.earnWprep)
	documentJSONBody[EarnWithdrawPrepareRequest](api, earnWprepPath)
}

func (h earnRoutes) earnWprep(ctx context.Context, in *wprepRequestInput) (*wprepOutput, error) {
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		var refused *routeError
		if errors.As(err, &refused) {
			return nil, refused
		}
		return nil, wprepFailed(account, err)
	}
	body, err := wprepPrepare(ctx, h, wprepDeps{Chain: h.Chain, HTTP: h.HTTP, KaminoURL: earnprep.KaminoWithdrawInstructionsURL}, account, h.PolicySigner, in.Raw.data)
	if err != nil {
		return nil, err
	}
	return &wprepOutput{Body: *body}, nil
}

// wprepPolicies are the two SQL reads resolution makes; earnRoutes serves them.
type wprepPolicies interface {
	activePolicyPair(ctx context.Context, account EarnAccount) (*RoutePolicyPair, error)
	inactivePolicy(ctx context.Context, account EarnAccount) (bool, error)
}

// inactivePolicy is hasInactiveYieldRoutePolicyForVault.
func (h earnRoutes) inactivePolicy(ctx context.Context, account EarnAccount) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var id int64
	err := h.YieldDB.QueryRow(ctx, `SELECT mv.id
FROM loyal_yield.managed_vaults mv
JOIN loyal_yield.route_policies rp ON rp.id = mv.active_policy_id
WHERE mv.settings = $1 AND mv.vault_index = $2 AND mv.vault_pubkey = $3
  AND rp.authority = $4 AND rp.cluster = $5 AND rp.settings = $1 AND rp.vault_index = $2 AND rp.vault_pubkey = $3
  AND (NOT mv.active OR NOT rp.active)
LIMIT 1`, account.Settings.String(), int16(earnprep.EarnVaultIndex), account.Vault.String(), account.Wallet.String(), string(account.Cluster)).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

var wprepProjectionDelays = []time.Duration{0, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond, time.Second}

type wprepRetryError struct{ routeError }

func (e *wprepRetryError) GetHeaders() http.Header { return http.Header{"Retry-After": {"1"}} }

const (
	wprepSourceChanged    = "The selected Earn source changed. Refresh and choose it again."
	wprepSourceRequired   = "Select an Earn source before withdrawing."
	wprepExceedsSource    = "Withdrawal exceeds the selected Earn source amount."
	wprepAccountMissing   = "A required Earn withdrawal transaction account is unavailable. Refresh Earn and prepare the withdrawal again."
	wprepUnportedMessage  = "This Earn withdrawal is not served by Go yet."
	wprepUnderfilledError = "Kamino withdrawal simulation produced less liquidity than requested."
)

func wprepPrepare(ctx context.Context, policies wprepPolicies, deps wprepDeps, account EarnAccount, policySigner solana.PublicKey, raw []byte) (*EarnWithdrawPrepareResponse, error) {
	request, err := parseWprepBody(raw)
	if err != nil {
		return nil, earnError(http.StatusBadRequest, "invalid_request", err.Error())
	}
	if account.Cluster != LoyalMainnetBeta || (request.legacy != nil && request.legacy.mode == "full") {
		return nil, earnError(http.StatusServiceUnavailable, "earn_withdraw_unported", wprepUnportedMessage)
	}
	input, err := wprepResolve(ctx, policies, deps.Chain, account, policySigner, request)
	if err != nil {
		return nil, wprepNormalize(account, err)
	}
	var wire EarnWithdrawPrepareWire
	if input.Source.idle() {
		wire, err = input.prepareIdle()
	} else {
		wire, err = input.prepareReserve(ctx, deps)
	}
	if err != nil {
		return nil, wprepNormalize(account, err)
	}
	return &EarnWithdrawPrepareResponse{PreparedWithdraw: wire}, nil
}

var wprepSourceChangedPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)KLEND_(MARKET|OBLIGATION)_NOT_FOUND`),
	regexp.MustCompile(`(?i)Kamino (reserve account|vault collateral token account) (was not found|is unavailable)`),
	regexp.MustCompile(`(?i)Selected Kamino reserve account was not found`),
}

var wprepAccountNotFound = regexp.MustCompile(`(?i)\bAccountNotFound\b`)

// wprepNormalize is normalizeEarnWithdrawPreparationError plus the route's
// fallthrough to 500 prepare_failed.
func wprepNormalize(account EarnAccount, err error) error {
	var refused *routeError
	var retry *wprepRetryError
	switch {
	case errors.As(err, &retry):
		return retry
	case errors.As(err, &refused):
		return refused
	case errors.Is(err, earnprep.ErrWithdrawRequiredAccountMissing):
		return earnError(http.StatusConflict, "earn_withdraw_required_account_missing", wprepAccountMissing)
	}
	text := err.Error()
	for _, pattern := range wprepSourceChangedPatterns {
		if pattern.MatchString(text) {
			return earnError(http.StatusConflict, "earn_withdraw_source_changed", "The selected Earn source changed. Refresh Earn and choose it again.")
		}
	}
	if wprepAccountNotFound.MatchString(text) {
		return earnError(http.StatusConflict, "earn_withdraw_required_account_missing", wprepAccountMissing)
	}
	return wprepFailed(account, err)
}

func wprepFailed(account EarnAccount, err error) error {
	if errors.Is(err, errWprepUnderfilled) {
		err = fmt.Errorf("%s: %w", wprepUnderfilledError, err)
	}
	slog.Error("earn withdraw prepare failed", "wallet", account.Wallet.String(), "settings", account.Settings.String(), "error", err)
	return earnError(http.StatusInternalServerError, "prepare_failed", "Failed to prepare Earn withdrawal. Refresh Earn and try again.")
}

type wprepLegacySource struct {
	amountRaw                                         *string
	id                                                string
	liquidityMint, mint, reserve, tokenAccount, typed string
}

type wprepLegacy struct {
	mode   string
	source *wprepLegacySource
}

type wprepRequest struct {
	max       bool
	amountRaw *big.Int
	sourceID  string
	legacy    *wprepLegacy
}

var (
	wprepUnsigned     = regexp.MustCompile(`^[0-9]+$`)
	wprepSourcePrefix = regexp.MustCompile(`^(?:reserve|idle):`)
)

// parseWprepBody is parseEarnWithdrawPrepareRequestBody with JavaScript's
// typing: a non-object body is refused, an array reads as an empty object.
func parseWprepBody(raw []byte) (wprepRequest, error) {
	var body any
	if json.Unmarshal(raw, &body) != nil {
		return wprepRequest{}, errors.New("Invalid request body.")
	}
	record, ok := wprepRecord(body)
	if !ok {
		return wprepRequest{}, errors.New("Request body must be an object.")
	}
	var request wprepRequest
	if amount, isString := record["amountRaw"].(string); isString && amount == "max" {
		request.max = true
	} else {
		trimmed := strings.TrimSpace(amount)
		if !isString || !wprepUnsigned.MatchString(trimmed) {
			return wprepRequest{}, errors.New("amountRaw must be an unsigned integer string.")
		}
		request.amountRaw, _ = new(big.Int).SetString(trimmed, 10)
		if request.amountRaw.Sign() <= 0 {
			return wprepRequest{}, errors.New("amountRaw must be greater than 0.")
		}
	}
	_, hasSourceID := record["sourceId"]
	if mode, _ := record["mode"].(string); !hasSourceID && (mode == "partial" || mode == "full") {
		if request.max {
			return wprepRequest{}, errors.New("amountRaw must be an unsigned integer string.")
		}
		source, err := parseWprepLegacySource(record["source"])
		if err != nil {
			return wprepRequest{}, err
		}
		request.legacy = &wprepLegacy{mode: mode, source: source}
		return request, nil
	}
	sourceID, _ := record["sourceId"].(string)
	if strings.TrimSpace(sourceID) == "" {
		return wprepRequest{}, errors.New("sourceId must be a non-empty string.")
	}
	request.sourceID = strings.TrimSpace(sourceID)
	return request, nil
}

func wprepRecord(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	case []any:
		return map[string]any{}, true
	}
	return nil, false
}

func parseWprepLegacySource(value any) (*wprepLegacySource, error) {
	if value == nil {
		return nil, nil
	}
	record, ok := wprepRecord(value)
	if !ok {
		return nil, errors.New("source must be an object when provided.")
	}
	typed, _ := record["type"].(string)
	if typed != "reserve" && typed != "idle" {
		return nil, errors.New("source.type must be reserve or idle.")
	}
	id, _ := record["id"].(string)
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("source.id must be a non-empty string.")
	}
	text := func(key string) string {
		value, _ := record[key].(string)
		return strings.TrimSpace(value)
	}
	source := &wprepLegacySource{
		id: strings.TrimSpace(id), liquidityMint: text("liquidityMint"), mint: text("mint"),
		reserve: text("reserve"), tokenAccount: text("tokenAccount"), typed: typed,
	}
	if amount, ok := record["amountRaw"].(string); ok && wprepUnsigned.MatchString(amount) {
		source.amountRaw = &amount
	}
	return source, nil
}

// wprepResolve is resolveEarnUsdcWithdrawInput for the sourceId and legacy
// partial shapes.
func wprepResolve(ctx context.Context, policies wprepPolicies, chain earnprep.WithdrawChain, account EarnAccount, policySigner solana.PublicKey, request wprepRequest) (wprepInput, error) {
	var pair *RoutePolicyPair
	for _, delay := range wprepProjectionDelays {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return wprepInput{}, ctx.Err()
			case <-time.After(delay):
			}
		}
		var err error
		if pair, err = policies.activePolicyPair(ctx, account); err != nil {
			return wprepInput{}, err
		}
		if pair != nil {
			break
		}
	}
	if pair == nil {
		inactive, err := policies.inactivePolicy(ctx, account)
		if err != nil {
			return wprepInput{}, err
		}
		if inactive {
			return wprepInput{}, earnError(http.StatusConflict, "earn_policy_inactive", "This Earn policy is no longer active. Refresh Earn before withdrawing.")
		}
		return wprepInput{}, &wprepRetryError{routeError{Detail: EarnMaxErrorDetail{Code: "earn_policy_projection_pending", Message: "The confirmed Earn policy is still updating. Retry this withdrawal."}, status: http.StatusServiceUnavailable}}
	}
	snapshot, err := earnprep.FetchWithdrawHoldingsSnapshot(ctx, chain, earnprep.WithdrawHoldingsArgs{
		Cluster: string(account.Cluster),
		Policy: &earnprep.WithdrawPolicyUniverse{
			VaultIndex:           earnprep.EarnVaultIndex,
			StableMints:          pair.Route.StableMints,
			KaminoLiquidityMints: pair.Route.KaminoLiquidityMints,
			KaminoMarkets:        pair.Route.KaminoMarkets,
		},
		SettingsPda: account.Settings,
	})
	if err != nil {
		return wprepInput{}, err
	}
	sources := wprepSources(snapshot.Holdings)
	if len(sources) == 0 {
		return wprepInput{}, earnError(http.StatusConflict, "missing_earn_withdraw_source", "No active Earn withdrawal source was found.")
	}
	var selected wprepCandidate
	if request.legacy != nil {
		found, ok := wprepLegacyMatch(sources, request.legacy.source)
		if !ok {
			return wprepInput{}, earnError(http.StatusBadRequest, "earn_withdraw_source_required", wprepSourceRequired)
		}
		selected = found
	} else {
		var matches []wprepCandidate
		for _, source := range sources {
			if source.sourceID == request.sourceID {
				matches = append(matches, source)
			}
		}
		if len(matches) != 1 {
			return wprepInput{}, earnError(http.StatusConflict, "earn_withdraw_source_changed", wprepSourceChanged)
		}
		selected = matches[0]
	}
	amount := selected.AmountRaw
	if !request.max {
		if request.amountRaw.Cmp(new(big.Int).SetUint64(selected.AmountRaw)) > 0 {
			return wprepInput{}, earnError(http.StatusConflict, "earn_withdraw_amount_exceeds_source", wprepExceedsSource)
		}
		amount = request.amountRaw.Uint64()
	}
	policyAccount, err := solana.PublicKeyFromBase58(pair.Route.PolicyAccount)
	if err != nil {
		return wprepInput{}, err
	}
	policy := wprepPolicy{Account: policyAccount, Seed: pair.Route.PolicySeed}
	if pair.Setup != nil {
		if policy.SetupAccount, err = solana.PublicKeyFromBase58(pair.Setup.PolicyAccount); err != nil {
			return wprepInput{}, err
		}
		policy.SetupSeed = pair.Setup.PolicySeed
	}
	return wprepInput{Account: account, AmountRaw: amount, PolicySigner: policySigner, Policy: policy, Source: selected.wprepSource}, nil
}

type wprepCandidate struct {
	wprepSource
	sourceID string
}

// wprepSources is buildSnapshotWithdrawSources.
func wprepSources(holdings []earnprep.WithdrawHolding) []wprepCandidate {
	var sources []wprepCandidate
	for _, holding := range holdings {
		if holding.AmountRaw == 0 {
			continue
		}
		source := wprepSource{AmountRaw: holding.AmountRaw, Mint: holding.LiquidityMint, TokenProgram: holding.TokenProgram}
		if holding.Kind == "idle" {
			tokenAccount, err := solana.PublicKeyFromBase58(holding.Provenance["tokenAccount"])
			if err != nil {
				continue
			}
			source.ID, source.TokenAccount = tokenAccount.String(), tokenAccount
		} else {
			if holding.Reserve == nil || holding.Market == nil {
				continue
			}
			source.ID, source.Reserve, source.Market = holding.Reserve.String(), *holding.Reserve, *holding.Market
		}
		sources = append(sources, wprepCandidate{wprepSource: source, sourceID: holding.SourceID})
	}
	return sources
}

func (c wprepCandidate) typed() string {
	if c.idle() {
		return "idle"
	}
	return "reserve"
}

// wprepLegacyMatch is selectLegacyEarnWithdrawSource: a direct identifier
// first, then a unique stable-mint match, then a unique amount match among
// the stable-mint matches.
func wprepLegacyMatch(sources []wprepCandidate, request *wprepLegacySource) (wprepCandidate, bool) {
	if request == nil {
		if len(sources) == 1 {
			return sources[0], true
		}
		return wprepCandidate{}, false
	}
	for _, source := range sources {
		if source.typed() != request.typed {
			continue
		}
		for _, identifier := range []string{request.id, request.reserve, request.tokenAccount} {
			if identifier == "" {
				continue
			}
			identifier = wprepSourcePrefix.ReplaceAllString(identifier, "")
			if identifier == source.ID || (!source.idle() && identifier == source.Reserve.String()) {
				return source, true
			}
		}
	}
	var mintMatches []wprepCandidate
	for _, source := range sources {
		if source.typed() != request.typed {
			continue
		}
		for _, identifier := range []string{request.id, request.liquidityMint, request.mint} {
			if identifier != "" && identifier == source.Mint.String() {
				mintMatches = append(mintMatches, source)
				break
			}
		}
	}
	if len(mintMatches) == 1 {
		return mintMatches[0], true
	}
	var amountMatches []wprepCandidate
	for _, source := range mintMatches {
		if request.amountRaw != nil && *request.amountRaw == fmt.Sprint(source.AmountRaw) {
			amountMatches = append(amountMatches, source)
		}
	}
	if len(amountMatches) == 1 {
		return amountMatches[0], true
	}
	return wprepCandidate{}, false
}

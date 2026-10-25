package webapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// POST /api/smart-accounts/yield-optimization/deposits/confirm. The route is
// verification-only: the LaserStream projection is the sole writer of Earn
// deposit state (scripts/web/verify-earn-confirm-single-writer.ts forbids
// recordConfirmedYieldDeposit here), so this endpoint validates the client's
// confirmed deposit against the principal, the canonical policy metadata and
// the on-chain transaction, then acknowledges it — no transaction is built,
// nothing signed, nothing sent. The op reports quest completion itself
// (EARN.md, Decided).
const earnDepositsConfirmPath = "/api/smart-accounts/yield-optimization/deposits/confirm"

// RPC_VERIFY_ATTEMPTS and RPC_VERIFY_DELAY_MS bound verification: reads can
// land on lagging RPC nodes, so each read polls six times half a second apart
// before the deposit is rejected.
const (
	depositConfirmVerifyAttempts = 6
	depositConfirmVerifyRetry    = 500 * time.Millisecond
)

// MIN_EARN_DEPOSIT_QUEST_USDC_RAW: deposits
// below $5 USDC never create a quest completion row.
var depositQuestMinUsdcRaw = big.NewInt(5_000_000)

const (
	depositQuestKind          = "earn_deposit"
	depositQuestSource        = "mobile-earn-deposit-confirm"
	depositQuestRetiredReason = "Seeker Summer round 1 ended — Solana-side reporting retired"
)

// EarnDepositConfirmPosition is the acknowledged deposit as the active
// position the LaserStream projection will soon record.
type EarnDepositConfirmPosition struct {
	CurrentHolding EarnDepositConfirmCurrentHolding `json:"currentHolding"`
	ID             string                           `json:"id"`
	InitialHolding EarnDepositConfirmInitialHolding `json:"initialHolding"`
	Principal      string                           `json:"principalAmountRaw"`
	Status         string                           `json:"status"`
}

type EarnDepositConfirmCurrentHolding struct {
	AmountRaw     string                               `json:"amountRaw"`
	LiquidityMint string                               `json:"liquidityMint"`
	Market        string                               `json:"market"`
	ObservedAt    string                               `json:"observedAt"`
	ObservedSlot  string                               `json:"observedSlot"`
	Provenance    EarnDepositConfirmPositionProvenance `json:"provenance"`
	Reserve       string                               `json:"reserve"`
}

type EarnDepositConfirmPositionProvenance struct {
	LastHoldingEventID      *string `json:"lastHoldingEventId"`
	LastRebalanceDecisionID *string `json:"lastRebalanceDecisionId"`
}

type EarnDepositConfirmInitialHolding struct {
	LiquidityMint string  `json:"liquidityMint"`
	Market        string  `json:"market"`
	Reserve       string  `json:"reserve"`
	SupplyApyBps  *string `json:"supplyApyBps"`
}

type EarnDepositConfirmResponse struct {
	Position EarnDepositConfirmPosition `json:"position"`
}

// EarnDepositConfirmRequest documents the request body for OpenAPI; the
// handler parses the raw bytes so the TS error messages carry over verbatim.
type EarnDepositConfirmRequest struct {
	Cluster                  *string `json:"cluster,omitempty"`
	ConfirmedSlot            *string `json:"confirmedSlot,omitempty"`
	DelegatedSigner          *string `json:"delegatedSigner,omitempty"`
	DepositMint              *string `json:"depositMint,omitempty"`
	DepositSignature         *string `json:"depositSignature,omitempty"`
	LiquidityMint            *string `json:"liquidityMint,omitempty"`
	Market                   *string `json:"market,omitempty"`
	PolicyAccount            *string `json:"policyAccount,omitempty"`
	PolicyConfirmedSlot      *string `json:"policyConfirmedSlot,omitempty"`
	PolicyID                 *string `json:"policyId,omitempty"`
	PolicyInitialization     *string `json:"policyInitialization,omitempty" enum:"create,reuse"`
	PolicySeed               *string `json:"policySeed,omitempty"`
	PolicySignature          *string `json:"policySignature,omitempty"`
	PrincipalAmountRaw       *string `json:"principalAmountRaw,omitempty"`
	Settings                 *string `json:"settings,omitempty"`
	SetupPolicyAccount       *string `json:"setupPolicyAccount,omitempty"`
	SetupPolicyConfirmedSlot *string `json:"setupPolicyConfirmedSlot,omitempty"`
	SetupPolicyID            *string `json:"setupPolicyId,omitempty"`
	SetupPolicySeed          *string `json:"setupPolicySeed,omitempty"`
	SetupPolicySignature     *string `json:"setupPolicySignature,omitempty"`
	SmartAccountAddress      *string `json:"smartAccountAddress,omitempty"`
	TargetReserve            *string `json:"targetReserve,omitempty"`
	TargetSupplyApyBps       *string `json:"targetSupplyApyBps,omitempty"`
	VaultIndex               *int    `json:"vaultIndex,omitempty"`
	VaultPubkey              *string `json:"vaultPubkey,omitempty"`
	WalletAddress            *string `json:"walletAddress,omitempty"`
}

type earnDepositConfirmInput struct {
	SessionInput
	Raw rawJSON
}

type earnDepositConfirmOutput struct {
	Body EarnDepositConfirmResponse
}

func (h earnRoutes) registerDepositsConfirm(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-earn-deposits-confirm",
		Method:      http.MethodPost,
		Path:        earnDepositsConfirmPath,
		Summary:     "Confirm a deposited Earn deposit transaction",
		Responses:   errorResponses[routeError](api, 400, 401, 403, 503),
	}, h.earnDepositsConfirm)
	documentJSONBody[EarnDepositConfirmRequest](api, earnDepositsConfirmPath)
}

func (h earnRoutes) earnDepositsConfirm(ctx context.Context, in *earnDepositConfirmInput) (*earnDepositConfirmOutput, error) {
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		if refused, ok := authRefusal(err); ok {
			return nil, refused
		}
		return nil, earnUnavailable("deposit confirm principal", err, "deposit_confirm_unavailable", "The Earn deposit confirm is temporarily unavailable.")
	}
	body, err := parseEarnDepositConfirmBody(in.Raw.data)
	if err != nil {
		return nil, err
	}
	input, err := h.depositCiteEarnPolicies(ctx, account, body)
	if err != nil {
		return nil, err
	}
	position, principalAmount, err := h.depositVerifyConfirmed(ctx, account, input)
	if err != nil {
		return nil, err
	}
	h.depositReportQuestCompletion(ctx, account, principalAmount)
	return &earnDepositConfirmOutput{Body: EarnDepositConfirmResponse{Position: *position}}, nil
}

func depositCiteUnresolved(policyAccount string, body earnDepositConfirmBody, wallet string) error {
	slog.Error("earn deposit confirm policy citation unresolved",
		"depositSignature", body.DepositSignature,
		"policyAccount", policyAccount,
		"policyInitialization", body.PolicyInitialization,
		"walletAddress", wallet)
	return earnError(http.StatusBadRequest, "policy_signature_unresolved",
		"We couldn't verify your Earn policy on-chain. Your deposit is safe — please try again in a moment.")
}

// earnDepositConfirmBody is the parsed confirm body. Whole numbers ride in
// big.Int because the TS body carries BigInts that only later steps bound.
type earnDepositConfirmBody struct {
	Cluster                  string
	ConfirmedSlot            *big.Int
	DelegatedSigner          string
	DepositMint              string
	DepositSignature         string
	LiquidityMint            string
	Market                   *string
	PolicyAccount            string
	PolicyConfirmedSlot      *big.Int
	PolicyID                 *big.Int
	PolicyInitialization     string
	PolicySeed               *big.Int
	PolicySignature          string
	PrincipalAmountRaw       *big.Int
	Settings                 string
	SetupPolicyAccount       *string
	SetupPolicyConfirmedSlot *big.Int
	SetupPolicyID            *big.Int
	SetupPolicySeed          *big.Int
	SetupPolicySignature     *string
	SmartAccountAddress      string
	TargetReserve            string
	TargetSupplyApyBps       *big.Int
	VaultIndex               float64
	VaultPubkey              string
	WalletAddress            string
}

func depositConfirmPolicyInitialization(record map[string]any) (string, error) {
	value, err := confirmRequiredString(record, "policyInitialization")
	if err != nil {
		return "", err
	}
	if value != "create" && value != "reuse" {
		return "", confirmInvalidBody("policyInitialization must be create or reuse.")
	}
	return value, nil
}

// parseEarnDepositConfirmBody ports parseEarnDepositConfirmRequestBody: fields
// validate in the TS object literal's evaluation order, so the first refusal's
// message matches what Next returns. Like the policy parser, an array body
// passes TS's typeof-object guard and dies on the first required-string check.
func parseEarnDepositConfirmBody(raw []byte) (earnDepositConfirmBody, error) {
	parsed, err := parseJSONBody(raw)
	if err != nil {
		return earnDepositConfirmBody{}, err
	}
	record := map[string]any{}
	switch value := parsed.(type) {
	case map[string]any:
		record = value
	case []any:
	default:
		return earnDepositConfirmBody{}, confirmInvalidBody("Request body must be an object.")
	}

	var body earnDepositConfirmBody
	if body.Cluster, err = confirmRequiredString(record, "cluster"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.ConfirmedSlot, err = confirmBigIntString(record, "confirmedSlot"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.DelegatedSigner, err = confirmRequiredString(record, "delegatedSigner"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.DepositMint, err = confirmRequiredString(record, "depositMint"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.DepositSignature, err = confirmRequiredString(record, "depositSignature"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.LiquidityMint, err = confirmRequiredString(record, "liquidityMint"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.Market, err = confirmOptionalString(record, "market"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.PolicyAccount, err = confirmRequiredString(record, "policyAccount"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.PolicyID, err = confirmBigIntString(record, "policyId"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.PolicyConfirmedSlot, err = confirmOptionalBigIntString(record, "policyConfirmedSlot"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.PolicyInitialization, err = depositConfirmPolicyInitialization(record); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.PolicySeed, err = confirmBigIntString(record, "policySeed"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.PolicySignature, err = confirmRequiredString(record, "policySignature"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.PrincipalAmountRaw, err = confirmBigIntString(record, "principalAmountRaw"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.Settings, err = confirmRequiredString(record, "settings"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.SetupPolicyAccount, err = confirmOptionalString(record, "setupPolicyAccount"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.SetupPolicyConfirmedSlot, err = confirmOptionalBigIntString(record, "setupPolicyConfirmedSlot"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.SetupPolicyID, err = confirmOptionalBigIntString(record, "setupPolicyId"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.SetupPolicySeed, err = confirmOptionalBigIntString(record, "setupPolicySeed"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.SetupPolicySignature, err = confirmOptionalString(record, "setupPolicySignature"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.SmartAccountAddress, err = confirmRequiredString(record, "smartAccountAddress"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.TargetReserve, err = confirmRequiredString(record, "targetReserve"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.TargetSupplyApyBps, err = confirmOptionalBigIntString(record, "targetSupplyApyBps"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.VaultIndex, err = confirmVaultIndex(record); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.VaultPubkey, err = confirmRequiredString(record, "vaultPubkey"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	if body.WalletAddress, err = confirmRequiredString(record, "walletAddress"); err != nil {
		return earnDepositConfirmBody{}, err
	}
	return body, nil
}

type depositConfirmCanonical struct {
	Cluster               string
	DepositMint           string
	LiquidityMint         string
	Market                string
	PolicyAccount         string
	PolicyID              *big.Int
	PolicySeed            *big.Int
	RequiresSetupMetadata bool
	SetupPolicyAccount    string
	SetupPolicyID         *big.Int
	SetupPolicySeed       *big.Int
	SmartAccountAddress   string
	TargetReserve         string
	TargetSupplyApyBps    *big.Int
	VaultIndex            uint8
	VaultPubkey           string
}

func depositCanonicalMismatch(label string) error {
	return fmt.Errorf("%s does not match the canonical earn deposit metadata.", label)
}

func depositCanonicalInput(request earnDepositConfirmBody) (depositConfirmCanonical, error) {
	cluster, err := confirmNormalizeCluster(request.Cluster)
	if err != nil {
		return depositConfirmCanonical{}, err
	}
	settings, err := confirmPublicKey(request.Settings)
	if err != nil {
		return depositConfirmCanonical{}, err
	}
	expectedSetupSeed := new(big.Int).Add(request.PolicySeed, big.NewInt(1))
	seed, err := confirmSafePolicySeed(request.PolicySeed)
	if err != nil {
		return depositConfirmCanonical{}, err
	}
	setupSeed, err := confirmSafePolicySeed(expectedSetupSeed)
	if err != nil {
		return depositConfirmCanonical{}, err
	}
	expectedPolicyAccount, _, err := squads.PolicyAddress(settings, seed)
	if err != nil {
		return depositConfirmCanonical{}, err
	}
	expectedSetupPolicyAccount, _, err := squads.PolicyAddress(settings, setupSeed)
	if err != nil {
		return depositConfirmCanonical{}, err
	}
	expectedVault, _, err := squads.SmartAccountAddress(settings, earnprep.EarnVaultIndex)
	if err != nil {
		return depositConfirmCanonical{}, err
	}
	hasSetupPolicyMetadata := request.SetupPolicyID != nil || request.SetupPolicyAccount != nil || request.SetupPolicySeed != nil
	requiresSetupPolicyMetadata := request.PolicyInitialization == "create" || hasSetupPolicyMetadata
	universe, err := depositEarnUniverseFor(LoyalCluster(cluster))
	if err != nil {
		return depositConfirmCanonical{}, err
	}
	liquidityMint, err := confirmPublicKey(request.LiquidityMint)
	if err != nil {
		return depositConfirmCanonical{}, err
	}
	if !depositConfirmContainsKey(universe.StableMints, liquidityMint) {
		return depositConfirmCanonical{}, errors.New("mint is not a supported Earn product mint.")
	}
	if request.Market == nil || !depositConfirmContainsString(depositKeyStrings(universe.Markets), *request.Market) {
		return depositConfirmCanonical{}, errors.New("Earn reserve market is not in the Safe universe.")
	}
	if _, err := confirmPublicKey(request.TargetReserve); err != nil {
		return depositConfirmCanonical{}, err
	}
	if request.TargetSupplyApyBps != nil && request.TargetSupplyApyBps.Sign() < 0 {
		return depositConfirmCanonical{}, errors.New("Earn target APY evidence cannot be negative.")
	}

	canonical := depositConfirmCanonical{
		Cluster:               cluster,
		DepositMint:           liquidityMint.String(),
		LiquidityMint:         liquidityMint.String(),
		Market:                *request.Market,
		PolicyAccount:         expectedPolicyAccount.String(),
		PolicyID:              new(big.Int).Set(request.PolicySeed),
		PolicySeed:            new(big.Int).Set(request.PolicySeed),
		RequiresSetupMetadata: requiresSetupPolicyMetadata,
		SetupPolicyAccount:    expectedSetupPolicyAccount.String(),
		SetupPolicyID:         new(big.Int).Set(expectedSetupSeed),
		SetupPolicySeed:       new(big.Int).Set(expectedSetupSeed),
		SmartAccountAddress:   expectedVault.String(),
		TargetReserve:         request.TargetReserve,
		TargetSupplyApyBps:    request.TargetSupplyApyBps,
		VaultIndex:            earnprep.EarnVaultIndex,
		VaultPubkey:           expectedVault.String(),
	}

	if request.DepositMint != canonical.DepositMint {
		return depositConfirmCanonical{}, depositCanonicalMismatch("depositMint")
	}
	if request.PolicyAccount != canonical.PolicyAccount {
		return depositConfirmCanonical{}, depositCanonicalMismatch("policyAccount")
	}
	if request.PolicyID.Cmp(request.PolicySeed) != 0 {
		return depositConfirmCanonical{}, depositCanonicalMismatch("policyId")
	}
	if request.PolicyInitialization == "create" {
		if request.PolicyConfirmedSlot == nil {
			return depositConfirmCanonical{}, errors.New("policyConfirmedSlot is required for policy creation.")
		}
		if request.SetupPolicySignature == nil {
			return depositConfirmCanonical{}, errors.New("setupPolicySignature is required for policy creation.")
		}
		if request.SetupPolicyConfirmedSlot == nil {
			return depositConfirmCanonical{}, errors.New("setupPolicyConfirmedSlot is required for policy creation.")
		}
	}
	if requiresSetupPolicyMetadata {
		if request.SetupPolicyAccount == nil || *request.SetupPolicyAccount != canonical.SetupPolicyAccount {
			return depositConfirmCanonical{}, depositCanonicalMismatch("setupPolicyAccount")
		}
		if request.SetupPolicyID == nil || request.SetupPolicyID.Cmp(canonical.SetupPolicyID) != 0 {
			return depositConfirmCanonical{}, depositCanonicalMismatch("setupPolicyId")
		}
		if request.SetupPolicySeed == nil || request.SetupPolicySeed.Cmp(canonical.SetupPolicySeed) != 0 {
			return depositConfirmCanonical{}, depositCanonicalMismatch("setupPolicySeed")
		}
	}
	hasSetupPolicyConfirmation := request.SetupPolicySignature != nil || request.SetupPolicyConfirmedSlot != nil
	if hasSetupPolicyConfirmation && (request.SetupPolicySignature == nil || request.SetupPolicyConfirmedSlot == nil) {
		return depositConfirmCanonical{}, errors.New("Setup policy confirmation metadata is incomplete.")
	}
	if request.SmartAccountAddress != canonical.SmartAccountAddress {
		return depositConfirmCanonical{}, depositCanonicalMismatch("smartAccountAddress")
	}
	if request.VaultIndex != float64(canonical.VaultIndex) {
		return depositConfirmCanonical{}, depositCanonicalMismatch("vaultIndex")
	}
	if request.VaultPubkey != canonical.VaultPubkey {
		return depositConfirmCanonical{}, depositCanonicalMismatch("vaultPubkey")
	}
	return canonical, nil
}

type depositPolicyCitation struct {
	Signature string
	Slot      *big.Int
}

func depositCitePolicy(ctx context.Context, client *chain.Client, policyAccount string, recorded *RoutePolicy) *depositPolicyCitation {
	if recorded != nil && recorded.PolicyAccount == policyAccount {
		return &depositPolicyCitation{Signature: recorded.LastSeenSignature, Slot: big.NewInt(recorded.LastSeenSlot)}
	}
	for attempt := 1; attempt <= depositConfirmVerifyAttempts; attempt++ {
		if cited := depositCiteCreationFromChain(ctx, client, policyAccount); cited != nil {
			return cited
		}
		if attempt < depositConfirmVerifyAttempts {
			select {
			case <-time.After(depositConfirmVerifyRetry):
			case <-ctx.Done():
				return nil
			}
		}
	}
	return nil
}

func depositCiteCreationFromChain(ctx context.Context, client *chain.Client, policyAccount string) *depositPolicyCitation {
	account, err := solana.PublicKeyFromBase58(policyAccount)
	if err != nil {
		return nil
	}
	history, err := client.History(ctx, account, 1000, solana.Signature{}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil
	}
	for index := len(history) - 1; index >= 0; index-- {
		if history[index].Failed {
			continue
		}
		return &depositPolicyCitation{
			Signature: history[index].Signature.String(),
			Slot:      new(big.Int).SetUint64(history[index].Slot),
		}
	}
	return nil
}

func (h earnRoutes) depositCiteEarnPolicies(ctx context.Context, account EarnAccount, body earnDepositConfirmBody) (earnDepositConfirmBody, error) {
	pair, err := h.activePolicyPair(ctx, account)
	if err != nil {
		return earnDepositConfirmBody{}, earnUnavailable("deposit confirm policy pair", err, "deposit_confirm_unavailable", "The Earn deposit confirm is temporarily unavailable.")
	}
	var routeRecord, setupRecord *RoutePolicy
	if pair != nil {
		routeRecord = &pair.Route
		setupRecord = pair.Setup
	}
	route := depositCitePolicy(ctx, h.Chain, body.PolicyAccount, routeRecord)
	if route == nil {
		return earnDepositConfirmBody{}, depositCiteUnresolved(body.PolicyAccount, body, account.Wallet.String())
	}
	setupAccount := body.PolicyAccount
	if body.SetupPolicyAccount != nil {
		setupAccount = *body.SetupPolicyAccount
	}
	var setup *depositPolicyCitation
	if body.SetupPolicyAccount != nil {
		setup = depositCitePolicy(ctx, h.Chain, *body.SetupPolicyAccount, setupRecord)
	}
	if setup == nil && body.PolicyInitialization == "create" {
		return earnDepositConfirmBody{}, depositCiteUnresolved(setupAccount, body, account.Wallet.String())
	}

	cited := body
	cited.PolicyConfirmedSlot = route.Slot
	cited.PolicySignature = route.Signature
	cited.SetupPolicyConfirmedSlot = nil
	cited.SetupPolicySignature = nil
	if setup != nil {
		cited.SetupPolicyConfirmedSlot = setup.Slot
		cited.SetupPolicySignature = &setup.Signature
	}
	return cited, nil
}

func depositVerifySignatureSlot(ctx context.Context, client *chain.Client, operation, signature string) (*big.Int, error) {
	for attempt := 1; attempt <= depositConfirmVerifyAttempts; attempt++ {
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
		if attempt < depositConfirmVerifyAttempts {
			select {
			case <-time.After(depositConfirmVerifyRetry):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	return nil, fmt.Errorf("%s transaction is not confirmed.", operation)
}

func depositConfirmTransactionProof(ctx context.Context, client *chain.Client, input earnDepositConfirmBody, mint solana.PublicKey) (*big.Int, error) {
	signature, err := solana.SignatureFromBase58(input.DepositSignature)
	if err != nil {
		return nil, err
	}
	var receipt *chain.Receipt
	for attempt := 1; attempt <= depositConfirmVerifyAttempts; attempt++ {
		execution, err := client.Execution(ctx, signature, rpc.CommitmentConfirmed)
		switch {
		case errors.Is(err, chain.ErrNotFound):
			// Parsed transactions propagate later than signature statuses — a
			// miss here is usually lag, not a missing transaction.
		case err != nil:
			return nil, err
		default:
			parsed := execution.Receipt
			receipt = &parsed
		}
		if receipt != nil || attempt == depositConfirmVerifyAttempts {
			break
		}
		select {
		case <-time.After(depositConfirmVerifyRetry):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if receipt == nil {
		return nil, errors.New("Confirmed deposit transaction details are unavailable.")
	}
	if receipt.Err != nil {
		return nil, errors.New("Deposit transaction proof has an execution error.")
	}
	if new(big.Int).SetUint64(receipt.Slot).Cmp(input.ConfirmedSlot) != 0 {
		return nil, errors.New("Confirmed deposit transaction slot does not match the recorded slot.")
	}

	deltas := depositTokenDeltasByOwner(*receipt, mint)
	funding := map[solana.PublicKey]bool{}
	for _, raw := range []string{input.WalletAddress, input.SmartAccountAddress, input.VaultPubkey} {
		key, err := solana.PublicKeyFromBase58(raw)
		if err != nil {
			continue
		}
		funding[key] = true
	}
	principal := new(big.Int)
	for owner, delta := range deltas {
		if !funding[owner] || delta.Sign() >= 0 {
			continue
		}
		principal.Sub(principal, delta)
	}
	if principal.Sign() <= 0 {
		return nil, errors.New("Confirmed deposit transaction does not debit the deposit mint from the wallet or Earn vault.")
	}
	return principal, nil
}

func depositTokenDeltasByOwner(receipt chain.Receipt, mint solana.PublicKey) map[solana.PublicKey]*big.Int {
	type snapshot struct {
		amount uint64
		owner  solana.PublicKey
		exists bool
	}
	pre := map[solana.PublicKey]snapshot{}
	for account, balance := range receipt.Pre {
		if balance.Mint == mint {
			pre[account] = snapshot{amount: balance.Amount, owner: balance.Owner, exists: true}
		}
	}
	post := map[solana.PublicKey]snapshot{}
	for account, balance := range receipt.Post {
		if balance.Mint == mint {
			post[account] = snapshot{amount: balance.Amount, owner: balance.Owner, exists: true}
		}
	}
	accounts := map[solana.PublicKey]bool{}
	for account := range pre {
		accounts[account] = true
	}
	for account := range post {
		accounts[account] = true
	}
	deltas := map[solana.PublicKey]*big.Int{}
	for account := range accounts {
		before, after := pre[account], post[account]
		if !before.exists && !after.exists {
			continue
		}
		owner := before.owner
		if after.exists {
			owner = after.owner
		}
		delta := new(big.Int)
		if after.exists {
			delta.SetUint64(after.amount)
		}
		if before.exists {
			delta.Sub(delta, new(big.Int).SetUint64(before.amount))
		}
		if existing, ok := deltas[owner]; ok {
			existing.Add(existing, delta)
		} else {
			deltas[owner] = delta
		}
	}
	return deltas
}

func (h earnRoutes) depositVerifyConfirmed(ctx context.Context, account EarnAccount, input earnDepositConfirmBody) (*EarnDepositConfirmPosition, *big.Int, error) {
	if input.WalletAddress != account.Wallet.String() || input.Settings != account.Settings.String() {
		return nil, nil, earnError(http.StatusForbidden, "principal_mismatch",
			"Confirmed yield deposit does not match the authenticated wallet session.")
	}
	canonical, err := depositCanonicalInput(input)
	if err != nil {
		return nil, nil, earnError(http.StatusBadRequest, "metadata_mismatch", confirmErrorText(err, "Confirmed yield deposit metadata is invalid."))
	}
	if canonical.Cluster != string(account.Cluster) {
		return nil, nil, earnError(http.StatusBadRequest, "cluster_mismatch",
			"Confirmed yield deposit cluster does not match the configured Solana environment.")
	}

	confirmedSlot, err := depositVerifySignatureSlot(ctx, h.Chain, "deposit", input.DepositSignature)
	if err != nil {
		return nil, nil, earnError(http.StatusBadRequest, "unconfirmed_signature", confirmErrorText(err, "Deposit transaction is not confirmed."))
	}
	if input.ConfirmedSlot.Cmp(confirmedSlot) != 0 {
		// The client-supplied slot can be its confirmation context slot, not
		// the landing slot; the server-resolved one is canonical
		slog.Warn("earn deposit confirm corrected client slot",
			"depositSignature", input.DepositSignature, "settings", input.Settings,
			"walletAddress", input.WalletAddress,
			"clientSlot", input.ConfirmedSlot.String(), "resolvedSlot", confirmedSlot.String())
		input.ConfirmedSlot = confirmedSlot
	}

	mint, err := confirmPublicKey(canonical.DepositMint)
	if err != nil {
		return nil, nil, err
	}
	principalAmount, err := depositConfirmTransactionProof(ctx, h.Chain, input, mint)
	if err != nil {
		return nil, nil, earnError(http.StatusBadRequest, "invalid_deposit_proof", confirmErrorText(err, "Deposit transaction amount proof is invalid."))
	}

	if input.PolicyInitialization == "create" {
		policySlot, err := depositVerifySignatureSlot(ctx, h.Chain, "route policy setup", input.PolicySignature)
		if err != nil {
			return nil, nil, earnError(http.StatusBadRequest, "unconfirmed_policy_signature", confirmErrorText(err, "Route policy setup transaction is not confirmed."))
		}
		if input.PolicyConfirmedSlot == nil || input.PolicyConfirmedSlot.Cmp(policySlot) != 0 {
			slog.Warn("earn deposit confirm corrected client policy slot",
				"depositSignature", input.DepositSignature, "settings", input.Settings,
				"walletAddress", input.WalletAddress,
				"clientSlot", depositBigIntText(input.PolicyConfirmedSlot), "resolvedSlot", policySlot.String())
			input.PolicyConfirmedSlot = policySlot
		}

		setupSignature := ""
		if input.SetupPolicySignature != nil {
			setupSignature = *input.SetupPolicySignature
		}
		setupSlot, err := depositVerifySignatureSlot(ctx, h.Chain, "setup policy setup", setupSignature)
		if err != nil {
			return nil, nil, earnError(http.StatusBadRequest, "unconfirmed_setup_policy_signature", confirmErrorText(err, "Setup policy transaction is not confirmed."))
		}
		if input.SetupPolicyConfirmedSlot == nil || input.SetupPolicyConfirmedSlot.Cmp(setupSlot) != 0 {
			slog.Warn("earn deposit confirm corrected client setup policy slot",
				"depositSignature", input.DepositSignature, "settings", input.Settings,
				"walletAddress", input.WalletAddress,
				"clientSlot", depositBigIntText(input.SetupPolicyConfirmedSlot), "resolvedSlot", setupSlot.String())
			input.SetupPolicyConfirmedSlot = setupSlot
		}
	}

	position := depositSerializeVerifiedPosition(canonical, input.DepositSignature, input.ConfirmedSlot, principalAmount, time.Now())
	return position, principalAmount, nil
}

func depositBigIntText(value *big.Int) string {
	if value == nil {
		return ""
	}
	return value.String()
}

// depositSerializeVerifiedPosition: observedAt keeps toISOString's
// millisecond precision.
func depositSerializeVerifiedPosition(canonical depositConfirmCanonical, depositSignature string, confirmedSlot *big.Int, principalAmount *big.Int, verifiedAt time.Time) *EarnDepositConfirmPosition {
	var supplyApyBps *string
	if canonical.TargetSupplyApyBps != nil {
		text := canonical.TargetSupplyApyBps.String()
		supplyApyBps = &text
	}
	amount := principalAmount.String()
	return &EarnDepositConfirmPosition{
		CurrentHolding: EarnDepositConfirmCurrentHolding{
			AmountRaw:     amount,
			LiquidityMint: canonical.LiquidityMint,
			Market:        canonical.Market,
			ObservedAt:    verifiedAt.UTC().Format("2006-01-02T15:04:05.000Z07:00"),
			ObservedSlot:  confirmedSlot.String(),
			Provenance:    EarnDepositConfirmPositionProvenance{LastHoldingEventID: nil, LastRebalanceDecisionID: nil},
			Reserve:       canonical.TargetReserve,
		},
		ID:             depositSignature,
		InitialHolding: EarnDepositConfirmInitialHolding{LiquidityMint: canonical.LiquidityMint, Market: canonical.Market, Reserve: canonical.TargetReserve, SupplyApyBps: supplyApyBps},
		Principal:      amount,
		Status:         "active",
	}
}

// depositReportQuestCompletion runs inside the op per the decided behavior
// (EARN.md, Decided). Reporting is retired: rows stay pending and "reported"
// is unreachable.
func (h earnRoutes) depositReportQuestCompletion(ctx context.Context, account EarnAccount, principalAmount *big.Int) {
	if h.YieldDB == nil || principalAmount == nil || principalAmount.Cmp(depositQuestMinUsdcRaw) < 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	wallet := account.Wallet.String()
	metadata := fmt.Sprintf(`{"depositUsdcRaw":"%s","solanaEnv":"%s","source":"%s"}`,
		principalAmount.String(), string(h.Cluster), depositQuestSource)
	if _, err := h.YieldDB.Exec(ctx, `INSERT INTO loyal_yield.solana_week_quest_completions
  (wallet_address, quest_kind, status, metadata)
  VALUES ($1, $2, 'pending', $3::jsonb)
  ON CONFLICT (wallet_address, quest_kind) DO NOTHING`, wallet, depositQuestKind, metadata); err != nil {
		slog.Warn("earn deposit confirm quest row insert failed", "error", err, "walletAddress", wallet)
		return
	}
	var rowID int64
	var status string
	if err := h.YieldDB.QueryRow(ctx, `SELECT id, status FROM loyal_yield.solana_week_quest_completions
  WHERE wallet_address = $1 AND quest_kind = $2 LIMIT 1`, wallet, depositQuestKind).Scan(&rowID, &status); err != nil {
		slog.Warn("earn deposit confirm quest row read failed", "error", err, "walletAddress", wallet)
		return
	}
	if status == "reported" {
		return
	}
	// The retired reporter's row contract: bump attempts, keep the status
	// pending, record the retired reason.
	if _, err := h.YieldDB.Exec(ctx, `UPDATE loyal_yield.solana_week_quest_completions
  SET attempts = attempts + 1, updated_at = now(), status = 'pending', last_error_code = $2
  WHERE id = $1`, rowID, depositQuestRetiredReason); err != nil {
		slog.Warn("earn deposit confirm quest report failed", "error", err, "walletAddress", wallet)
	}
}

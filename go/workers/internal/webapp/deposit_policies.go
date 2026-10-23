package webapp

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// POST /api/smart-accounts/yield-optimization/policies/prepare ports
// web/apps/web/src/app/api/smart-accounts/yield-optimization/policies/prepare/route.ts
// (prepareEarnUsdcYieldRoutingPolicy, client.ts:8266-8347): the two unsigned
// Squads ops that create the Earn route policy at the Settings' next seed and
// its setup policy at seed+1. No database access, nothing signed, nothing
// sent; the client compiles both ops against a fresh blockhash.
const earnPoliciesPreparePath = "/api/smart-accounts/yield-optimization/policies/prepare"

const depositExecuteSettingsOperation = "executeSettingsTransactionSync"

// depositMaxPolicySeed is Number.MAX_SAFE_INTEGER: the wire carries seeds as
// JSON numbers on the TS side, so a seed beyond 2^53-1 cannot round-trip
// (resolveNextPolicySeed, client.ts:4482-4484).
const depositMaxPolicySeed = 1<<53 - 1

const (
	depositPolicyStageRoute = "route"
	depositPolicyStageSetup = "setup"

	depositEarnRiskProfile    = "safe"
	depositEarnUniversePreset = "canonical_stable_kamino"
)

type EarnPolicySeedRef struct {
	Account string `json:"account"`
	ID      string `json:"id"`
	Seed    string `json:"seed"`
}

type EarnSetupPolicyRef struct {
	Account                                  string `json:"account"`
	ID                                       string `json:"id"`
	InitObligationInstructionConstraintIndex uint8  `json:"initObligationInstructionConstraintIndex"`
	Seed                                     string `json:"seed"`
}

type EarnPolicyTargetReserve struct {
	LiquidityMint         string `json:"liquidityMint"`
	LiquidityTokenProgram string `json:"liquidityTokenProgram"`
	Market                string `json:"market"`
	Obligation            string `json:"obligation"`
	Reserve               string `json:"reserve"`
}

type EarnPolicyVaultRef struct {
	AccountIndex uint8  `json:"accountIndex"`
	Pubkey       string `json:"pubkey"`
}

// EarnPolicySwapLanePersistence is one route plan swap lane; Earn prepares
// with no swap lanes, so it only fixes the empty array's element shape.
type EarnPolicySwapLanePersistence struct {
	Lane                         string  `json:"lane"`
	ActionAccount                string  `json:"actionAccount"`
	InstructionConstraintIndexes []uint8 `json:"instructionConstraintIndexes"`
}

// EarnPolicyPersistence is the persistence object the client stores with the
// confirmed policies (prepareEarnUsdcYieldRoutingPolicy, client.ts:8328-8345,
// spreading createYieldRoutePolicyPlan's persistence, sdk.ts:203-212).
type EarnPolicyPersistence struct {
	Cluster              string                          `json:"cluster"`
	WalletAddress        string                          `json:"walletAddress"`
	DelegatedSigner      string                          `json:"delegatedSigner"`
	Settings             string                          `json:"settings"`
	VaultIndex           uint8                           `json:"vaultIndex"`
	VaultPubkey          string                          `json:"vaultPubkey"`
	PolicyID             string                          `json:"policyId"`
	PolicyAccount        string                          `json:"policyAccount"`
	PolicySeed           string                          `json:"policySeed"`
	SetupPolicyID        string                          `json:"setupPolicyId"`
	SetupPolicyAccount   string                          `json:"setupPolicyAccount"`
	SetupPolicySeed      string                          `json:"setupPolicySeed"`
	TargetReserve        string                          `json:"targetReserve"`
	Market               string                          `json:"market"`
	LiquidityMint        string                          `json:"liquidityMint"`
	RiskProfile          string                          `json:"riskProfile"`
	UniversePreset       string                          `json:"universePreset"`
	RouteModes           []string                        `json:"routeModes"`
	StableMints          []string                        `json:"stableMints"`
	KaminoMarkets        []string                        `json:"kaminoMarkets"`
	KaminoLiquidityMints []string                        `json:"kaminoLiquidityMints"`
	SwapLanes            []EarnPolicySwapLanePersistence `json:"swapLanes"`
	Threshold            int                             `json:"threshold"`
}

// EarnPolicyPrepareWire is serializePreparedEarnUsdcYieldRoutingPolicy's
// shape (earn-policy-prepare-contracts.shared.ts:9-76).
type EarnPolicyPrepareWire struct {
	FinalizePrepared *DepositWirePreparedOperation `json:"finalizePrepared,omitempty"`
	Persistence      EarnPolicyPersistence         `json:"persistence"`
	Policy           EarnPolicySeedRef             `json:"policy"`
	SetupPolicy      EarnSetupPolicyRef            `json:"setupPolicy"`
	Prepared         DepositWirePreparedOperation  `json:"prepared"`
	TargetReserve    EarnPolicyTargetReserve       `json:"targetReserve"`
	Vault            EarnPolicyVaultRef            `json:"vault"`
}

type EarnPolicyPrepareResponse struct {
	PreparedPolicy EarnPolicyPrepareWire `json:"preparedPolicy"`
}

type earnPoliciesPrepareInput struct {
	SessionInput
}

type earnPoliciesPrepareOutput struct {
	Body EarnPolicyPrepareResponse
}

func (h earnRoutes) registerPoliciesPrepare(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-earn-policies-prepare",
		Method:      http.MethodPost,
		Path:        earnPoliciesPreparePath,
		Summary:     "Prepare the Earn route and setup policy creation operations",
		Responses:   errorResponses[routeError](api, 401, 409, 500),
	}, h.earnPoliciesPrepare)
}

func (h earnRoutes) earnPoliciesPrepare(ctx context.Context, in *earnPoliciesPrepareInput) (*earnPoliciesPrepareOutput, error) {
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		if refused, ok := authRefusal(err); ok {
			return nil, refused
		}
		return nil, depositPrepareFailed(err)
	}

	// fetchSettings (client.ts:8282-8285): getAccountInfo at confirmed.
	_, read, err := h.Chain.Accounts(ctx, []solana.PublicKey{account.Settings}, rpc.CommitmentConfirmed, 0)
	if err != nil {
		return nil, depositPrepareUnavailable(account, err)
	}
	settings, err := squads.DecodeSettings(read[0])
	if err != nil {
		return nil, depositPrepareUnavailable(account, err)
	}
	response, err := depositPreparePolicyResponse(account, settings, h.PolicySigner)
	if err != nil {
		return nil, depositPrepareUnavailable(account, err)
	}
	return &earnPoliciesPrepareOutput{Body: *response}, nil
}

func depositPrepareUnavailable(account EarnAccount, err error) error {
	slog.Warn("earn policy prepare failed",
		"cluster", string(account.Cluster),
		"error", err,
		"settings", account.Settings.String(),
		"wallet", account.Wallet.String())
	return depositPrepareFailed(err)
}

// depositPrepareFailed is route.ts:101-105: an unexpected failure becomes 500
// prepare_failed carrying the error's own message.
func depositPrepareFailed(err error) error {
	message := err.Error()
	if message == "" {
		message = "Failed to prepare Earn policy."
	}
	return earnError(http.StatusInternalServerError, "prepare_failed", message)
}

// depositPreparePolicyResponse is prepareEarnUsdcYieldRoutingPolicy minus the
// auth and RPC reads: seed resolution, both ops, the packet guards and the
// serialized response.
func depositPreparePolicyResponse(account EarnAccount, settings squads.Settings, policySigner solana.PublicKey) (*EarnPolicyPrepareResponse, error) {
	seed := squads.NextPolicySeed(settings)
	if seed > depositMaxPolicySeed {
		return nil, earnError(http.StatusInternalServerError, "prepare_failed", "Policy seed is too large for this client.")
	}
	setupSeed := seed + 1
	if setupSeed > depositMaxPolicySeed {
		return nil, earnError(http.StatusInternalServerError, "prepare_failed", "Policy seed is too large for this client.")
	}

	target, err := depositEarnTargetFor(account.Cluster)
	if err != nil {
		return nil, err
	}
	universe, err := depositEarnUniverseFor(account.Cluster)
	if err != nil {
		return nil, err
	}
	routeConstraints, err := EarnRoutePayload(account.Cluster, account.Vault, nil)
	if err != nil {
		return nil, err
	}
	setupConstraints, err := EarnSetupPayload(account.Cluster, account.Vault)
	if err != nil {
		return nil, err
	}

	routeApply := squads.PolicyApply{
		Settings:    account.Settings,
		RentPayer:   account.Wallet,
		Signer:      account.Wallet,
		Delegate:    policySigner,
		Seed:        seed,
		VaultIndex:  earnVaultIndex,
		Constraints: routeConstraints,
	}
	setupApply := squads.PolicyApply{
		Settings:    account.Settings,
		RentPayer:   account.Wallet,
		Signer:      account.Wallet,
		Delegate:    policySigner,
		Seed:        setupSeed,
		VaultIndex:  earnVaultIndex,
		Constraints: setupConstraints,
	}
	routeInstruction, err := routeApply.Instruction()
	if err != nil {
		return nil, err
	}
	setupInstruction, err := setupApply.Instruction()
	if err != nil {
		return nil, err
	}
	routePolicy, err := routeApply.Policy()
	if err != nil {
		return nil, err
	}
	setupPolicy, err := setupApply.Policy()
	if err != nil {
		return nil, err
	}

	// Packet guards (client.ts:6531-6555): each op must compile into one
	// Solana packet against the all-ones blockhash.
	routeLength, err := depositPacketLength(account.Wallet, routeInstruction)
	if err != nil || depositPacketOverLimit(routeLength) {
		return nil, earnError(http.StatusInternalServerError, "prepare_failed", "Earn route policy setup exceeds the Solana transaction size limit.")
	}
	setupLength, err := depositPacketLength(account.Wallet, setupInstruction)
	if err != nil || depositPacketOverLimit(setupLength) {
		return nil, earnError(http.StatusInternalServerError, "prepare_failed", "Earn init-obligation policy setup exceeds the Solana transaction size limit.")
	}

	obligation, err := kamino.VanillaObligation(account.Vault, target.Market)
	if err != nil {
		return nil, err
	}

	prepared := depositWireOperation(depositExecuteSettingsOperation, account.Wallet, routeInstruction,
		depositEarnPolicyCreateDiagnostics(routePolicy, seed, depositPolicyStageRoute, account.Settings, routeInstruction))
	finalizePrepared := depositWireOperation(depositExecuteSettingsOperation, account.Wallet, setupInstruction,
		depositEarnPolicyCreateDiagnostics(setupPolicy, setupSeed, depositPolicyStageSetup, account.Settings, setupInstruction))

	seedText := strconv.FormatUint(seed, 10)
	setupSeedText := strconv.FormatUint(setupSeed, 10)
	persistence := EarnPolicyPersistence{
		Cluster:              string(account.Cluster),
		WalletAddress:        account.Wallet.String(),
		DelegatedSigner:      policySigner.String(),
		Settings:             account.Settings.String(),
		VaultIndex:           earnVaultIndex,
		VaultPubkey:          account.Vault.String(),
		PolicyID:             seedText,
		PolicyAccount:        routePolicy.String(),
		PolicySeed:           seedText,
		SetupPolicyID:        setupSeedText,
		SetupPolicyAccount:   setupPolicy.String(),
		SetupPolicySeed:      setupSeedText,
		TargetReserve:        target.Reserve.String(),
		Market:               target.Market.String(),
		LiquidityMint:        target.LiquidityMint.String(),
		RiskProfile:          depositEarnRiskProfile,
		UniversePreset:       depositEarnUniversePreset,
		RouteModes:           []string{"same_mint_kamino"},
		StableMints:          depositKeyStrings(universe.StableMints),
		KaminoMarkets:        depositKeyStrings(universe.Markets),
		KaminoLiquidityMints: depositKeyStrings(universe.StableMints),
		SwapLanes:            []EarnPolicySwapLanePersistence{},
		Threshold:            1,
	}
	return &EarnPolicyPrepareResponse{PreparedPolicy: EarnPolicyPrepareWire{
		FinalizePrepared: &finalizePrepared,
		Persistence:      persistence,
		Policy:           EarnPolicySeedRef{Account: routePolicy.String(), ID: seedText, Seed: seedText},
		SetupPolicy: EarnSetupPolicyRef{
			Account:                                  setupPolicy.String(),
			ID:                                       setupSeedText,
			InitObligationInstructionConstraintIndex: 0,
			Seed:                                     setupSeedText,
		},
		Prepared: prepared,
		TargetReserve: EarnPolicyTargetReserve{
			LiquidityMint:         target.LiquidityMint.String(),
			LiquidityTokenProgram: target.LiquidityTokenProgram.String(),
			Market:                target.Market.String(),
			Obligation:            obligation.String(),
			Reserve:               target.Reserve.String(),
		},
		Vault: EarnPolicyVaultRef{AccountIndex: earnVaultIndex, Pubkey: account.Vault.String()},
	}}, nil
}

func depositKeyStrings(keys []solana.PublicKey) []string {
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key.String())
	}
	return out
}

package backyard

import (
	"encoding/binary"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// All identities are fixed by the lane and the account its initializer
// policy literal is installed on (Policy). Rent and network fees are native SOL
// flows, separate from user USDC principal.
type KaminoInitializationRequest struct {
	RouteLane            string `json:"routeLane"`
	Policy               string `json:"policy"`
	RecentBlockhash      string `json:"recentBlockhash"`
	LastValidBlockHeight int64  `json:"lastValidBlockHeight"`
	RentLamports         uint64 `json:"rentLamports"`
	MaximumFeeLamports   uint64 `json:"maximumFeeLamports"`
}

func CompileKaminoInitializationMessage(r KaminoInitializationRequest) ([]byte, error) {
	if r.LastValidBlockHeight <= 0 || r.RentLamports == 0 || r.MaximumFeeLamports == 0 {
		return nil, fmt.Errorf("incomplete Multiply initialization admission")
	}
	policy, err := decodeKey(r.Policy)
	if err != nil {
		return nil, err
	}
	blockhash, err := decodeKey(r.RecentBlockhash)
	if err != nil {
		return nil, err
	}
	inner, err := kaminoMultiplyInitializer(r.RouteLane)
	if err != nil {
		return nil, err
	}
	_, index := initializerPolicyLeg(r.RouteLane)
	outer, err := wrapSquadsKaminoPolicy(policy, mustKey(bridgeDelegate), mustKey(bridgeDelegate), index, inner)
	if err != nil {
		return nil, err
	}
	message, err := compileLegacyMessage(mustKey(bridgeDelegate), blockhash, []compiledInstruction{outer})
	if err != nil {
		return nil, err
	}
	return checkedUnsignedMessage(message)
}

// compileKaminoInitializationMessage is the form that also admits the AUTO
// lane, which initializes under its KLend policy's initializer leg. Installed
// selector lanes keep the exact public path above. Any other lane holds.
func (m RouteManifest) compileKaminoInitializationMessage(r KaminoInitializationRequest) ([]byte, error) {
	if basicLane(r.RouteLane) {
		return CompileKaminoInitializationMessage(r)
	}
	if !earnInitializerLane(r.RouteLane) {
		return nil, budgetHold("initializer_lane_unreviewed")
	}
	if r.LastValidBlockHeight <= 0 || r.RentLamports == 0 || r.MaximumFeeLamports == 0 {
		return nil, fmt.Errorf("incomplete Multiply initialization admission")
	}
	policy, err := decodeKey(r.Policy)
	if err != nil {
		return nil, err
	}
	blockhash, err := decodeKey(r.RecentBlockhash)
	if err != nil {
		return nil, err
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return nil, err
	}
	inner, err := kaminoRouteInitializer(route)
	if err != nil {
		return nil, err
	}
	_, index := initializerPolicyLeg(r.RouteLane)
	outer, err := wrapSquadsKaminoPolicy(policy, mustKey(bridgeDelegate), mustKey(bridgeDelegate), index, inner)
	if err != nil {
		return nil, err
	}
	// The eight-constraint installed policy needs the reviewed 64 KiB heap frame
	// during execute as well as during create, so the AUTO initializer leg goes
	// through the closed resource wrapper; the public compiler above keeps the
	// exact installed selector-lane bytes.
	message, err := compileAutoResourceLegacyMessage(mustKey(bridgeDelegate), blockhash, outer)
	if err != nil {
		return nil, err
	}
	return checkedUnsignedMessage(message)
}

func validateInitializedKaminoObligation(r KaminoInitializationRequest, a ConfirmedAccount) error {
	route, err := runtimeRoute(r.RouteLane)
	if err != nil || !basicLane(route.Lane) {
		return fmt.Errorf("unreviewed initialized obligation")
	}
	return validateInitializedObligationOnRoute(route, r, a)
}

// validateInitializedKaminoObligation is the form that also admits the AUTO
// lane; the empty-state checks below are the exact installed checks.
func (m RouteManifest) validateInitializedKaminoObligation(r KaminoInitializationRequest, a ConfirmedAccount) error {
	if basicLane(r.RouteLane) {
		return validateInitializedKaminoObligation(r, a)
	}
	if !earnInitializerLane(r.RouteLane) {
		return budgetHold("initializer_lane_unreviewed")
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return err
	}
	return validateInitializedObligationOnRoute(route, r, a)
}

func validateInitializedObligationOnRoute(route RuntimeRoute, r KaminoInitializationRequest, a ConfirmedAccount) error {
	o, err := decodeKaminoObligation(a, route.Kamino)
	if err != nil {
		return err
	}
	if a.Lamports != r.RentLamports || binary.LittleEndian.Uint64(a.Data[8:16]) != 1 || o.hasPosition ||
		!allZero(a.Data[96:1184]) || !allZero(a.Data[1208:2208]) ||
		a.Data[kaminoObligationElevationGroupOffset] != 0 || !allZero(a.Data[2288:2320]) {
		return fmt.Errorf("initialized obligation state differs from admitted empty Multiply account")
	}
	return nil
}

// Construct only the three reviewed Multiply PDAs. Policy installation, rent
// admission and journal execution are separate; this function grants no authority.
func kaminoMultiplyInitializer(lane string) (compiledInstruction, error) {
	route, err := runtimeRoute(lane)
	if err != nil || !basicLane(route.Lane) || route.Kamino.DebtMint != bridgeUSDC {
		return compiledInstruction{}, fmt.Errorf("unreviewed Multiply initializer lane")
	}
	return kaminoRouteInitializer(route)
}

// kaminoRouteInitializer is the lane-independent checked core: the identical
// nine-account topology with the route's own debt mint, and the obligation PDA
// re-derived from the route identities and compared to the reviewed route
// obligation, so a candidate lane can never drift to a synthetic authority.
func kaminoRouteInitializer(route RuntimeRoute) (compiledInstruction, error) {
	owner, market := kaminoKey(bridgeVault), kaminoKey(route.Kamino.Market)
	collateral, debt := kaminoKey(route.Kamino.CollateralMint), kaminoKey(route.Kamino.DebtMint)
	obligation, err := kamino.ObligationAddress(1, 0, owner, market, collateral, debt)
	if err != nil || obligation.String() != route.Kamino.Obligation {
		return compiledInstruction{}, fmt.Errorf("Multiply obligation PDA drifted")
	}
	metadata, err := kamino.UserMetadataAddress(owner)
	if err != nil {
		return compiledInstruction{}, err
	}
	return kaminoCompiled(kamino.InitObligation(kamino.ObligationInitAccounts{Owner: owner, FeePayer: owner, Obligation: obligation,
		LendingMarket: market, Seed1: collateral, Seed2: debt, OwnerUserMetadata: metadata}, 1, 0)), nil
}

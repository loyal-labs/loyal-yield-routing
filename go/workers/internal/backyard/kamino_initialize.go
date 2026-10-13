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

// compileKaminoInitializationMessage is the initializer of a lane whose
// obligation Backyard creates (earnInitializerLane): the basic lanes'
// split initializer policies and the AUTO lanes' KLend policy initializer
// leg. Any other lane holds.
func (m RouteManifest) compileKaminoInitializationMessage(r KaminoInitializationRequest) ([]byte, error) {
	v := m.vault
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
	route, err := v.route(r.RouteLane)
	if err != nil {
		return nil, err
	}
	inner, err := kaminoRouteInitializer(route)
	if err != nil {
		return nil, err
	}
	_, index := initializerPolicyLeg(r.RouteLane)
	outer, err := wrapSquadsKaminoPolicy(v, policy, mustKey(bridgeDelegate), mustKey(bridgeDelegate), index, inner)
	if err != nil {
		return nil, err
	}
	// The eight-constraint installed policy needs the reviewed 64 KiB heap frame
	// during execute as well as during create (executionBudget).
	message, err := compileV1Message(mustKey(bridgeDelegate), blockhash, []compiledInstruction{outer}, executionBudget(r.RouteLane, 1))
	if err != nil {
		return nil, err
	}
	return checkedUnsignedMessage(message)
}

// validateInitializedKaminoObligation checks a created obligation is the
// admitted empty Multiply account.
func (m RouteManifest) validateInitializedKaminoObligation(r KaminoInitializationRequest, a ConfirmedAccount) error {
	if !earnInitializerLane(r.RouteLane) {
		return budgetHold("initializer_lane_unreviewed")
	}
	route, err := m.route(r.RouteLane)
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

// kaminoRouteInitializer is the lane-independent core: the identical
// nine-account topology with the route's own debt mint and its obligation, a
// PDA of the vault and the route identities (earnLane.routeFor).
func kaminoRouteInitializer(route RuntimeRoute) (compiledInstruction, error) {
	owner, market, obligation := kaminoKey(route.Kamino.Vault), kaminoKey(route.Kamino.Market), kaminoKey(route.Kamino.Obligation)
	collateral, debt := kaminoKey(route.Kamino.CollateralMint), kaminoKey(route.Kamino.DebtMint)
	metadata, err := kamino.UserMetadataAddress(owner)
	if err != nil {
		return compiledInstruction{}, err
	}
	return kaminoCompiled(kamino.InitObligation(kamino.ObligationInitAccounts{Owner: owner, FeePayer: owner, Obligation: obligation,
		LendingMarket: market, Seed1: collateral, Seed2: debt, OwnerUserMetadata: metadata}, 1, 0)), nil
}

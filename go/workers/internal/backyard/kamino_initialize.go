package backyard

import (
	"encoding/binary"
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

type KaminoInitializationRequest struct {
	RouteLane            string `json:"routeLane"`
	Policy               string `json:"policy"`
	RecentBlockhash      string `json:"recentBlockhash"`
	LastValidBlockHeight int64  `json:"lastValidBlockHeight"`
	RentLamports         uint64 `json:"rentLamports"`
	MaximumFeeLamports   uint64 `json:"maximumFeeLamports"`
}

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
	// The created obligation registers with its reserves' farms in the same
	// transaction: KLend refuses a leg on a farmed reserve without it.
	instructions := []compiledInstruction{outer}
	for _, f := range obligationFarms(route) {
		instructions = append(instructions, kaminoCompiled(kamino.InitObligationFarmsForReserve(f.accounts, f.mode)))
	}
	message, err := compileV1Message(mustKey(bridgeDelegate), blockhash, instructions, executionBudget(r.RouteLane, len(instructions)))
	if err != nil {
		return nil, err
	}
	return checkedUnsignedMessage(message)
}

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
		!plainCrossModeObligation(o, a) {
		return fmt.Errorf("initialized obligation state differs from admitted empty Multiply account")
	}
	return nil
}

const kaminoObligationReferrerOffset = 2288

func plainCrossModeObligation(o decodedKaminoObligation, a ConfirmedAccount) bool {
	return o.elevationGroup == 0 && allZero(a.Data[kaminoObligationReferrerOffset:kaminoObligationReferrerOffset+32])
}

// obligationFarm is one farm the lane's obligation registers with: its
// reserve's farm in mode, and the obligation's user state in it.
type obligationFarm struct {
	accounts kamino.ObligationFarmsInitAccounts
	mode     kamino.FarmMode
}

// obligationFarms are the farms of route's reserves, collateral first. The
// registration is permissionless: the delegate pays the user state's rent,
// outside the vault's policy.
func obligationFarms(route RuntimeRoute) []obligationFarm {
	k := route.Kamino
	var out []obligationFarm
	for _, f := range []struct {
		reserve, farm, user string
		mode                kamino.FarmMode
	}{{k.CollateralReserve, route.CollateralFarm, route.ObligationCollateralFarm, kamino.FarmCollateral}, {k.DebtReserve, route.DebtFarm, route.ObligationDebtFarm, kamino.FarmDebt}} {
		if f.farm == "" {
			continue
		}
		out = append(out, obligationFarm{mode: f.mode, accounts: kamino.ObligationFarmsInitAccounts{Payer: kaminoKey(bridgeDelegate), Owner: kaminoKey(k.Vault),
			Obligation: kaminoKey(k.Obligation), LendingMarketAuthority: kaminoKey(k.MarketAuthority), Reserve: kaminoKey(f.reserve),
			ReserveFarmState: kaminoKey(f.farm), ObligationFarmUserState: kaminoKey(f.user), LendingMarket: kaminoKey(k.Market)}})
	}
	return out
}

func kaminoRouteInitializer(route RuntimeRoute) (compiledInstruction, error) {
	owner, market, obligation := kaminoKey(route.Kamino.Vault), kaminoKey(route.Kamino.Market), kaminoKey(route.Kamino.Obligation)
	collateral, debt := kaminoKey(route.Kamino.CollateralMint), kaminoKey(route.Kamino.DebtMint)
	metadata, err := kamino.UserMetadataAddress(owner)
	if err != nil {
		return compiledInstruction{}, err
	}
	return kaminoCompiled(kamino.InitObligation(kamino.ObligationInitAccounts{Owner: owner, FeePayer: owner, Obligation: obligation,
		LendingMarket: market, Seed1: collateral, Seed2: debt, OwnerUserMetadata: metadata}, kamino.MultiplyObligationTag, kamino.DefaultObligationID)), nil
}

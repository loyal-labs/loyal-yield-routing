package backyardrwa

import "fmt"

// Observe all admitted pilot ownership accounts in the same confirmed batch.
// Desired allocation never determines which assets disappear from accounting.
// This pilot cannot report multiple simultaneous positions: keep them visible
// as an explicit hold until a multi-position accounting path is proven.
func observedSelectorRoute(accounts []ConfirmedAccount, preferred string) (RuntimeRoute, error) {
	active := ""
	for _, lane := range selectorLanes {
		route, err := runtimeRoute(lane)
		if err != nil {
			return RuntimeRoute{}, err
		}
		account := accountAt(accounts, route.Kamino.Obligation)
		exposed := false
		if account.Address != route.Kamino.Obligation {
			return RuntimeRoute{}, fmt.Errorf("selector_obligation_observation_missing")
		}
		if account.Lamports != 0 {
			position, err := decodeKaminoObligation(account, route.Kamino)
			if err != nil {
				return RuntimeRoute{}, fmt.Errorf("selector_obligation_invalid: %w", err)
			}
			exposed = position.hasPosition || position.collateralDepositedRaw > 0 || position.debtRaw > 0
		}
		mint, err := decodeBase58PublicKey(route.Kamino.CollateralMint)
		if err != nil {
			return RuntimeRoute{}, err
		}
		owner, err := decodeBase58PublicKey(bridgeVault)
		if err != nil {
			return RuntimeRoute{}, err
		}
		custodyAccount := accountAt(accounts, route.CollateralCustody)
		if custodyAccount.Address != route.CollateralCustody || custodyAccount.Executable || custodyAccount.Lamports == 0 || custodyAccount.Owner != route.CollateralTokenProgram {
			return RuntimeRoute{}, fmt.Errorf("selector_custody_observation_invalid")
		}
		custody, err := DecodeTokenCustody(custodyAccount.Owner, custodyAccount.Data, mint, owner)
		if err != nil {
			return RuntimeRoute{}, err
		}
		exposed = exposed || custody.Raw > 0
		if exposed {
			if active != "" {
				return RuntimeRoute{}, fmt.Errorf("multiple_selector_lanes_have_exposure")
			}
			active = lane
		}
	}
	if active == "" {
		active = preferred
	}
	if !selectorLane(active) {
		return RuntimeRoute{}, fmt.Errorf("selector_preferred_lane_invalid")
	}
	return runtimeRoute(active)
}

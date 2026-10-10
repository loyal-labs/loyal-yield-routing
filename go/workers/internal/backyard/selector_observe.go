package backyard

import "fmt"

// observedSelectorRoute is the lane holding exposure in one confirmed batch,
// scanned over every registry lane (exit-only lanes included, so a held
// position is never hidden), or preferred when every lane is flat.
func observedSelectorRoute(accounts []ConfirmedAccount, preferred string) (RuntimeRoute, error) {
	active, err := observedSelectorActiveExposure(accounts, earnLaneIDs(true), preferred)
	if err != nil {
		return RuntimeRoute{}, err
	}
	if active == "" {
		active = preferred
	}
	if !earnHeldLane(active) {
		return RuntimeRoute{}, fmt.Errorf("selector_preferred_lane_invalid")
	}
	return runtimeRoute(active)
}

// observedSelectorActiveExposure scans one fixed lane inventory over one
// confirmed batch: each lane's obligation decode and collateral custody must
// be present and valid, and at most one lane may hold exposure. Lanes can
// share one collateral custody (the Prime lanes share the vault's PRIME
// account), so a custody balance is exposure of the sharing lane whose
// obligation holds a position, else of the preferred lane when it shares the
// custody, else of the first sharing lane in registry order.
func observedSelectorActiveExposure(accounts []ConfirmedAccount, lanes []string, preferred string) (string, error) {
	exposed := map[string]bool{}
	custodyLanes := map[string][]string{}
	custodyHeld := map[string]bool{}
	for _, lane := range lanes {
		route, err := runtimeRoute(lane)
		if err != nil {
			return "", err
		}
		account := accountAt(accounts, route.Kamino.Obligation)
		if account.Address != route.Kamino.Obligation {
			return "", fmt.Errorf("selector_obligation_observation_missing")
		}
		if account.Lamports != 0 {
			position, err := decodeKaminoObligation(account, route.Kamino)
			if err != nil {
				return "", fmt.Errorf("selector_obligation_invalid: %w", err)
			}
			exposed[lane] = position.hasPosition || position.collateralDepositedRaw > 0 || position.debtRaw > 0
		}
		mint, err := decodeBase58PublicKey(route.Kamino.CollateralMint)
		if err != nil {
			return "", err
		}
		owner, err := decodeBase58PublicKey(bridgeVault)
		if err != nil {
			return "", err
		}
		custodyAccount := accountAt(accounts, route.CollateralCustody)
		if custodyAccount.Address != route.CollateralCustody || custodyAccount.Executable || custodyAccount.Lamports == 0 || custodyAccount.Owner != route.CollateralTokenProgram {
			return "", fmt.Errorf("selector_custody_observation_invalid")
		}
		custody, err := DecodeTokenCustody(custodyAccount.Owner, custodyAccount.Data, mint, owner)
		if err != nil {
			return "", err
		}
		custodyLanes[route.CollateralCustody] = append(custodyLanes[route.CollateralCustody], lane)
		custodyHeld[route.CollateralCustody] = custody.Raw > 0
	}
	for custody, sharing := range custodyLanes {
		if !custodyHeld[custody] {
			continue
		}
		owner := ""
		for _, lane := range sharing {
			if exposed[lane] {
				owner = lane
			}
		}
		for _, lane := range sharing {
			if owner == "" && lane == preferred {
				owner = lane
			}
		}
		if owner == "" {
			owner = sharing[0]
		}
		exposed[owner] = true
	}
	active := ""
	for _, lane := range lanes {
		if exposed[lane] {
			if active != "" {
				return "", fmt.Errorf("multiple_selector_lanes_have_exposure")
			}
			active = lane
		}
	}
	return active, nil
}

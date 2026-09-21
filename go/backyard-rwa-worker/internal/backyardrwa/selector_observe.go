package backyardrwa

import "fmt"

// Observe all admitted pilot ownership accounts in the same confirmed batch.
// Desired allocation never determines which assets disappear from accounting.
// This pilot cannot report multiple simultaneous positions: keep them visible
// as an explicit hold until a multi-position accounting path is proven.
func observedSelectorRoute(accounts []ConfirmedAccount, preferred string) (RuntimeRoute, error) {
	return observedSelectorRouteWithLane(accounts, preferred, selectorLane)
}

// observedSelectorRouteWithLane is the identical ownership observation with
// the preferred-lane authority parameterized: an explicit reviewed manifest
// may observe its candidate initializer lane, the installed closure is
// unchanged for every embedded caller.
func observedSelectorRouteWithLane(accounts []ConfirmedAccount, preferred string, laneAllowed func(string) bool) (RuntimeRoute, error) {
	active, err := observedSelectorActiveExposure(accounts, selectorLanes)
	if err != nil {
		return RuntimeRoute{}, err
	}
	if active == "" {
		active = preferred
	}
	if !laneAllowed(active) {
		return RuntimeRoute{}, fmt.Errorf("selector_preferred_lane_invalid")
	}
	return runtimeRoute(active)
}

// observedSelectorRouteForManifest is the same single confirmed batch read
// with the ownership inventory scoped by the explicit reviewed manifest:
// while that manifest admits the candidate AUTO lane, the candidate's
// obligation and collateral custody join the SAME-BATCH scan, so a funded
// candidate tranche can never hide behind an all-flat installed scan. The
// installed lane set, its checks and every embedded caller stay byte-for-byte
// unchanged; the global lane list is never mutated.
func observedSelectorRouteForManifest(accounts []ConfirmedAccount, preferred string, manifest RouteManifest) (RuntimeRoute, error) {
	active, err := observedSelectorActiveExposure(accounts, selectorObservationLanes(manifest))
	if err != nil {
		return RuntimeRoute{}, err
	}
	if active == "" {
		active = preferred
	}
	if !manifest.selectorEntryLaneAllowed(active) {
		return RuntimeRoute{}, fmt.Errorf("selector_preferred_lane_invalid")
	}
	return runtimeRoute(active)
}

// selectorObservationLanes is the single manifest-scoped lane inventory both
// sides of the observation seam share: the installed lanes always, plus the
// candidate AUTO lane only while this explicit manifest admits it. The global
// lane list is never mutated.
func selectorObservationLanes(manifest RouteManifest) []string {
	if !manifest.selectorEntryLaneAllowed(autoAUTOPYUSD.Lane) {
		return selectorLanes
	}
	return append(append([]string{}, selectorLanes...), autoAUTOPYUSD.Lane)
}

// observedSelectorActiveExposure scans one fixed lane inventory over one
// confirmed batch: each lane's obligation decode and collateral custody must
// be present and valid, and at most one lane may hold exposure.
func observedSelectorActiveExposure(accounts []ConfirmedAccount, lanes []string) (string, error) {
	active := ""
	for _, lane := range lanes {
		route, err := runtimeRoute(lane)
		if err != nil {
			return "", err
		}
		account := accountAt(accounts, route.Kamino.Obligation)
		exposed := false
		if account.Address != route.Kamino.Obligation {
			return "", fmt.Errorf("selector_obligation_observation_missing")
		}
		if account.Lamports != 0 {
			position, err := decodeKaminoObligation(account, route.Kamino)
			if err != nil {
				return "", fmt.Errorf("selector_obligation_invalid: %w", err)
			}
			exposed = position.hasPosition || position.collateralDepositedRaw > 0 || position.debtRaw > 0
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
		exposed = exposed || custody.Raw > 0
		if exposed {
			if active != "" {
				return "", fmt.Errorf("multiple_selector_lanes_have_exposure")
			}
			active = lane
		}
	}
	return active, nil
}

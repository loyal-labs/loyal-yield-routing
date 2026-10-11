package backyard

import "fmt"

// observedSelectorRoute is the working lane of one confirmed batch (see
// observedSelectorLanes) for callers that need only the lane.
func observedSelectorRoute(accounts []ConfirmedAccount, preferred string) (RuntimeRoute, error) {
	route, _, err := observedSelectorLanes(accounts, preferred, "")
	return route, err
}

// observedSelectorLanes scans every registry lane (exit-only lanes included,
// so a held position is never hidden) and returns the working lane and the
// held set. Several held lanes are a valid state. The working lane is the
// durable working lane when one is recorded; else the preferred lane when it
// is held; else the only held lane; else, with nothing held, the preferred
// lane; else the first held lane in registry order.
func observedSelectorLanes(accounts []ConfirmedAccount, preferred, working string) (RuntimeRoute, []string, error) {
	held, err := observedSelectorHeldLanes(accounts, earnLaneIDs(true), preferred, working)
	if err != nil {
		return RuntimeRoute{}, nil, err
	}
	active := working
	if active == "" {
		active = preferred
		if !containsLane(held, preferred) && len(held) > 0 {
			active = held[0]
		}
	}
	if !earnHeldLane(active) {
		return RuntimeRoute{}, nil, fmt.Errorf("selector_preferred_lane_invalid")
	}
	route, err := runtimeRoute(active)
	return route, held, err
}

func containsLane(lanes []string, lane string) bool {
	for _, l := range lanes {
		if l == lane {
			return true
		}
	}
	return false
}

// observedSelectorActiveExposure is the single held lane of one batch, ""
// when none; several held lanes are refused. Kept for the one-lane callers.
func observedSelectorActiveExposure(accounts []ConfirmedAccount, lanes []string, preferred string) (string, error) {
	held, err := observedSelectorHeldLanes(accounts, lanes, preferred, "")
	if err != nil {
		return "", err
	}
	switch len(held) {
	case 0:
		return "", nil
	case 1:
		return held[0], nil
	}
	return "", fmt.Errorf("multiple_selector_lanes_have_exposure")
}

// observedSelectorHeldLanes scans one fixed lane inventory over one
// confirmed batch: each lane's obligation decode and collateral custody must
// be present and valid. A lane is held when its obligation holds a position
// or a custody balance is attributed to it. Lanes share custodies (the Prime
// lanes share the PRIME account; AUTO, Ethena and Prime/PYUSD share PYUSD),
// so a custody balance belongs to the one sharing lane that holds a
// position; with several, to the working (else preferred) lane among them,
// else it is unattributable and refused; with none, to the working (else
// preferred) lane when it shares the custody, else to the first sharing lane
// in registry order. The Squads USDC ATA is bridge cash, never lane exposure.
func observedSelectorHeldLanes(accounts []ConfirmedAccount, lanes []string, preferred, working string) ([]string, error) {
	exposed := map[string]bool{}
	custodyLanes := map[string][]string{}
	custodyHeld := map[string]bool{}
	var custodyOrder []string
	owner, err := decodeBase58PublicKey(bridgeVault)
	if err != nil {
		return nil, err
	}
	addCustody := func(address, mint, program, lane string) error {
		account := accountAt(accounts, address)
		if account.Address != address || account.Executable || account.Lamports == 0 || account.Owner != program {
			return fmt.Errorf("selector_custody_observation_invalid")
		}
		key, err := decodeBase58PublicKey(mint)
		if err != nil {
			return err
		}
		custody, err := DecodeTokenCustody(account.Owner, account.Data, key, owner)
		if err != nil {
			return err
		}
		if _, seen := custodyLanes[address]; !seen {
			custodyOrder = append(custodyOrder, address)
		}
		custodyLanes[address] = append(custodyLanes[address], lane)
		custodyHeld[address] = custody.Raw > 0
		return nil
	}
	for _, lane := range lanes {
		route, err := runtimeRoute(lane)
		if err != nil {
			return nil, err
		}
		account := accountAt(accounts, route.Kamino.Obligation)
		if account.Address != route.Kamino.Obligation {
			return nil, fmt.Errorf("selector_obligation_observation_missing")
		}
		if account.Lamports != 0 {
			position, err := decodeKaminoObligation(account, route.Kamino)
			if err != nil {
				return nil, fmt.Errorf("selector_obligation_invalid: %w", err)
			}
			exposed[lane] = position.hasPosition || position.collateralDepositedRaw > 0 || position.debtRaw > 0
		}
		if err := addCustody(route.CollateralCustody, route.Kamino.CollateralMint, route.CollateralTokenProgram, lane); err != nil {
			return nil, err
		}
		if route.Kamino.DebtMint != bridgeUSDC && route.DebtCustody != "" && route.DebtCustody != bridgeSquadsATA {
			// Only a debt custody the batch carries is scanned: a caller's
			// reduced inventory may omit it, and the NAV requires it anyway.
			if accountAt(accounts, route.DebtCustody).Address == route.DebtCustody {
				if err := addCustody(route.DebtCustody, route.Kamino.DebtMint, route.DebtTokenProgram, lane); err != nil {
					return nil, err
				}
			}
		}
	}
	positioned := map[string]bool{}
	for lane, held := range exposed {
		positioned[lane] = held
	}
	for _, custody := range custodyOrder {
		if !custodyHeld[custody] {
			continue
		}
		sharing := custodyLanes[custody]
		var candidates []string
		for _, lane := range sharing {
			if positioned[lane] {
				candidates = append(candidates, lane)
			}
		}
		owner := ""
		switch {
		case len(candidates) == 1:
			owner = candidates[0]
		case len(candidates) > 1:
			for _, lane := range []string{working, preferred} {
				if lane != "" && owner == "" && containsLane(candidates, lane) {
					owner = lane
				}
			}
			if owner == "" {
				return nil, fmt.Errorf("selector_shared_custody_unattributable")
			}
		default:
			for _, lane := range []string{working, preferred} {
				if lane != "" && owner == "" && containsLane(sharing, lane) {
					owner = lane
				}
			}
			if owner == "" {
				owner = sharing[0]
			}
		}
		exposed[owner] = true
	}
	var held []string
	for _, lane := range lanes {
		if exposed[lane] {
			held = append(held, lane)
		}
	}
	return held, nil
}

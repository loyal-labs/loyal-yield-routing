package backyard

import "fmt"

func containsLane(lanes []string, lane string) bool {
	for _, l := range lanes {
		if l == lane {
			return true
		}
	}
	return false
}

// observedSelectorHeldLanes scans one fixed lane inventory over one
// confirmed batch: each lane's obligation decode and collateral custody must
// be present and valid. A lane is held when its obligation holds a position
// or a custody balance is attributed to it. Lanes share custodies (the Prime
// lanes share the PRIME account; AUTO, Ethena and Prime/PYUSD share PYUSD),
// so a custody balance belongs to the first sharing lane in registry order
// that holds a position; with none, to the preferred lane when it shares the
// custody, else to the first sharing lane. The Squads USDC ATA is bridge
// cash, never lane exposure.
func observedSelectorHeldLanes(accounts []ConfirmedAccount, lanes []string, preferred string) ([]string, error) {
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
			// Lanes that share a custody and both hold positions (a lane and its
			// overflow lane) are both held already.
			owner = candidates[0]
		default:
			owner = sharing[0]
			if containsLane(sharing, preferred) {
				owner = preferred
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

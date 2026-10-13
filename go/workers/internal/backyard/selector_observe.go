package backyard

import (
	"fmt"
	"slices"
)

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
// or collateral waits in its collateral custody for the lane's next leg (an
// entry's deposit, a release's or an exit's conversion). Lanes share
// collateral custodies (the Prime lanes share PRIME, the AUTO lanes AUTO):
// collateral there is the first positioned sharer's, else the preferred
// lane's when it shares the custody, else the first sharer's. Debt-mint
// custody is vault cash and makes no lane held (navLanes values it).
func observedSelectorHeldLanes(accounts []ConfirmedAccount, lanes []string, preferred string) ([]string, error) {
	exposed := map[string]bool{}
	sharers := map[string][]string{}
	var custodies []string
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
		if _, seen := sharers[route.CollateralCustody]; !seen {
			custodies = append(custodies, route.CollateralCustody)
		}
		sharers[route.CollateralCustody] = append(sharers[route.CollateralCustody], lane)
	}
	for _, custody := range custodies {
		sharing := sharers[custody]
		route, _ := runtimeRoute(sharing[0])
		raw, err := vaultCustodyRaw(accounts, custody, route.Kamino.CollateralMint, route.CollateralTokenProgram)
		if err != nil {
			return nil, err
		}
		// A positioned sharer is held already.
		if raw == 0 || slices.ContainsFunc(sharing, func(lane string) bool { return exposed[lane] }) {
			continue
		}
		owner := sharing[0]
		if containsLane(sharing, preferred) {
			owner = preferred
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

// navLanes is every lane the vault's NAV values: the held lanes, then, for
// each debt-mint custody holding a balance that no earlier lane names, the
// first registry lane naming it. Debt-mint custody is vault cash: each
// account is counted once, and the lane naming it carries the view that
// converts it when no held lane owes it (target rule 4a).
func navLanes(accounts []ConfirmedAccount, held []string) ([]string, error) {
	named := map[string]bool{}
	for _, lane := range held {
		route, err := runtimeRoute(lane)
		if err != nil {
			return nil, err
		}
		named[route.DebtCustody] = true
	}
	lanes := append([]string(nil), held...)
	for _, l := range earnLanes {
		r := l.route
		// Only a debt custody the batch carries is read: a caller's reduced
		// inventory may omit it, and the NAV requires it anyway.
		if r.Kamino.DebtMint == bridgeUSDC || named[r.DebtCustody] || accountAt(accounts, r.DebtCustody).Address != r.DebtCustody {
			continue
		}
		named[r.DebtCustody] = true
		raw, err := vaultCustodyRaw(accounts, r.DebtCustody, r.Kamino.DebtMint, r.DebtTokenProgram)
		if err != nil {
			return nil, err
		}
		if raw > 0 {
			lanes = append(lanes, r.Lane)
		}
	}
	return lanes, nil
}

// vaultCustodyRaw is the vault's balance in one pinned custody account.
func vaultCustodyRaw(accounts []ConfirmedAccount, address, mint, program string) (uint64, error) {
	account := accountAt(accounts, address)
	if account.Address != address || account.Executable || account.Lamports == 0 || account.Owner != program {
		return 0, fmt.Errorf("custody_observation_invalid: %s", address)
	}
	key, err := decodeBase58PublicKey(mint)
	if err != nil {
		return 0, err
	}
	owner, err := decodeBase58PublicKey(bridgeVault)
	if err != nil {
		return 0, err
	}
	custody, err := DecodeTokenCustody(account.Owner, account.Data, key, owner)
	return custody.Raw, err
}

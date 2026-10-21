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

func observedSelectorHeldLanes(v *Vault, accounts []ConfirmedAccount, lanes []string, preferred string) ([]string, error) {
	exposed := map[string]bool{}
	sharers := map[string][]string{}
	var custodies []string
	for _, lane := range lanes {
		route, err := v.route(lane)
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
		route, _ := v.route(sharing[0])
		raw, err := vaultCustodyRaw(v, accounts, custody, route.Kamino.CollateralMint, route.CollateralTokenProgram)
		if err != nil {
			return nil, err
		}
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

func navLanes(v *Vault, accounts []ConfirmedAccount, held []string) ([]string, error) {
	named := map[string]bool{}
	for _, lane := range held {
		route, err := v.route(lane)
		if err != nil {
			return nil, err
		}
		named[route.DebtCustody] = true
	}
	lanes := append([]string(nil), held...)
	for _, r := range v.routes {
		if r.Kamino.DebtMint == bridgeUSDC || named[r.DebtCustody] || accountAt(accounts, r.DebtCustody).Address != r.DebtCustody {
			continue
		}
		named[r.DebtCustody] = true
		raw, err := vaultCustodyRaw(v, accounts, r.DebtCustody, r.Kamino.DebtMint, r.DebtTokenProgram)
		if err != nil {
			return nil, err
		}
		if raw > 0 {
			lanes = append(lanes, r.Lane)
		}
	}
	return lanes, nil
}

func vaultCustodyRaw(v *Vault, accounts []ConfirmedAccount, address, mint, program string) (uint64, error) {
	account := accountAt(accounts, address)
	if account.Address != address || account.Executable || account.Lamports == 0 || account.Owner != program {
		return 0, fmt.Errorf("custody_observation_invalid: %s", address)
	}
	key, err := decodeBase58PublicKey(mint)
	if err != nil {
		return 0, err
	}
	owner, err := decodeBase58PublicKey(v.squads)
	if err != nil {
		return 0, err
	}
	custody, err := DecodeTokenCustody(account.Owner, account.Data, key, owner)
	return custody.Raw, err
}

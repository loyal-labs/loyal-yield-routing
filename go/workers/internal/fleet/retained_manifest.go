package fleet

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

// retainedSameMintRequirementsFingerprint implements loyal-actions' v1 typed
// manifest wire contract. Account access comes from the exact outer instructions;
// roles come from decoded protocol accounts, never from an ALT's contents.
func retainedSameMintRequirementsFingerprint(input KaminoSameMintRouteRequest, policy, payer string, instructions []RouteInstruction) (string, error) {
	type requirement struct {
		key      [32]byte
		class    byte
		writable bool
		tags     map[byte]bool
	}
	roles := map[string]*requirement{}
	add := func(address string, class, tag byte) error {
		if address == "" {
			return nil
		}
		key, err := decodePublicKey(address)
		if err != nil {
			return fmt.Errorf("typed provenance for %q: %w", address, err)
		}
		r := roles[address]
		if r == nil {
			r = &requirement{key: key, class: class, tags: map[byte]bool{}}
			roles[address] = r
		}
		if r.class != class {
			return fmt.Errorf("conflicting typed provenance for %s", address)
		}
		r.tags[tag] = true
		return nil
	}
	for _, x := range []struct {
		key string
		tag byte
	}{{input.Vault, 1}, {policy, 3}} {
		if err := add(x.key, 2, x.tag); err != nil {
			return "", err
		}
	}
	for _, p := range []KaminoPositionAccounts{input.Source, input.Target} {
		for tag, key := range []string{p.Market, p.MarketAuthority, p.Reserve, p.LiquidityMint, p.LiquiditySupply, p.CollateralMint, p.CollateralSupply} {
			if err := add(key, 1, byte(tag)); err != nil {
				return "", err
			}
		}
		for _, x := range []struct {
			key        string
			class, tag byte
		}{{p.Obligation, 2, 2}, {p.VaultLiquidityATA, 2, 5}, {p.LiquidityTokenProgram, 1, 10}} {
			if err := add(x.key, x.class, x.tag); err != nil {
				return "", err
			}
		}
		// The reserve decoder normalizes only default optional pubkeys to None.
		// KLend is an instruction placeholder, not an absent decoded oracle/farm.
		for _, x := range []struct {
			key        string
			class, tag byte
		}{{p.ObligationFarmUserState, 2, 7}, {p.ReserveFarmState, 1, 9}, {p.PythOracle, 1, 7}, {p.SwitchboardPriceOracle, 1, 7}, {p.SwitchboardTWAPOracle, 1, 7}, {p.ScopePrices, 1, 8}} {
			if x.key == "" || x.key == "11111111111111111111111111111111" {
				continue
			}
			if err := add(x.key, x.class, x.tag); err != nil {
				return "", err
			}
		}
		for _, key := range append(append([]string{}, p.ObligationDepositReserves...), p.ObligationBorrowReserves...) {
			if err := add(key, 1, 2); err != nil {
				return "", err
			}
		}
	}
	for _, key := range []string{"11111111111111111111111111111111", "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA", "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL", "Sysvar1nstructions1111111111111111111111111", "SysvarRent111111111111111111111111111111111", KLendProgram, farmsProgram} {
		if err := add(key, 1, 10); err != nil {
			return "", err
		}
	}
	actual := map[string]*requirement{}
	touch := func(key string, writable bool, reason *byte) error {
		r := actual[key]
		if r == nil {
			decoded, err := decodePublicKey(key)
			if err != nil {
				return fmt.Errorf("manifest account %q: %w", key, err)
			}
			r = &requirement{key: decoded, class: 255, tags: map[byte]bool{}}
			actual[key] = r
		}
		r.writable = r.writable || writable
		if reason != nil {
			r.class = 0
			r.tags[*reason] = true
		}
		return nil
	}
	fee, signer, program := byte(0), byte(1), byte(3)
	if err := touch(payer, true, &fee); err != nil {
		return "", err
	}
	if err := touch(payer, true, &signer); err != nil {
		return "", err
	}
	for _, ix := range instructions {
		if err := touch(ix.Program, false, &program); err != nil {
			return "", err
		}
		for _, account := range ix.Accounts {
			var reason *byte
			if account.Signer {
				reason = &signer
			}
			if err := touch(account.Address, account.Writable, reason); err != nil {
				return "", err
			}
		}
	}
	// Match loyal-actions::lookup_tables::nonce_pubkey: only the first
	// instruction, the System program, and the four-byte advance prefix count.
	if len(instructions) > 0 {
		ix := instructions[0]
		if ix.Program == "11111111111111111111111111111111" && len(ix.Data) >= 4 &&
			bytes.Equal(ix.Data[:4], []byte{4, 0, 0, 0}) && len(ix.Accounts) > 0 {
			nonce := byte(2)
			if err := touch(ix.Accounts[0].Address, false, &nonce); err != nil {
				return "", err
			}
		}
	}
	groups := [3][]*requirement{}
	for key, r := range actual {
		if r.class != 0 {
			role := roles[key]
			if role == nil {
				return "", fmt.Errorf("missing typed provenance for %s", key)
			}
			r.class = role.class
			r.tags = role.tags
		}
		groups[r.class] = append(groups[r.class], r)
	}
	encoded := append([]byte("loyal-route-alt-manifest-v1"), 0)
	for _, group := range groups {
		sort.Slice(group, func(i, j int) bool { return bytes.Compare(group[i].key[:], group[j].key[:]) < 0 })
		encoded = appendU64x(encoded, uint64(len(group)))
		for _, r := range group {
			encoded = append(encoded, r.class)
			encoded = append(encoded, r.key[:]...)
			access := byte(0)
			if r.writable {
				access = 1
			}
			encoded = append(encoded, access)
			encoded = appendU64x(encoded, uint64(len(r.tags)))
			for tag := 0; tag < 256; tag++ {
				if r.tags[byte(tag)] {
					encoded = append(encoded, byte(tag))
				}
			}
		}
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

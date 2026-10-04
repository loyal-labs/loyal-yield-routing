package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// retainedSameMintRequirementsFingerprint implements loyal-actions' v1 typed
// manifest wire contract. Account access comes from the exact outer instructions;
// roles come from decoded protocol accounts, never from an ALT's contents.
// ALTManifestAddress is the retained durable provisioning row. Its ordinal
// follows base58 address order, separately from the v1 raw-pubkey hash order.
type ALTManifestAddress struct {
	Address, SemanticClass, AccountRole string
	Ordinal                             int32
	Writable                            bool
}

type ALTManifest struct {
	Fingerprint                     string
	SharedAddresses, VaultAddresses []ALTManifestAddress
	fingerprintMaterial             []byte
	verifiedShared, verifiedVault   []ALTManifestAddress
	sourceShared, sourceVault       []ALTManifestAddress
	externalSnapshots               []CrossMintExternalALT
	boundRequirementsFingerprint    string
}

// ValidateALTManifestIntegrity accepts only the unchanged output of the typed
// source builder, including the static account hash material omitted from the
// durable provisioning rows. It does not certify RPC freshness or chain state.
func ValidateALTManifestIntegrity(manifest *ALTManifest) error {
	if manifest == nil || len(manifest.fingerprintMaterial) == 0 {
		return fmt.Errorf("ALT manifest lacks typed source builder provenance")
	}
	digest := sha256.Sum256(manifest.fingerprintMaterial)
	if manifest.Fingerprint != hex.EncodeToString(digest[:]) || !reflect.DeepEqual(manifest.SharedAddresses, manifest.verifiedShared) || !reflect.DeepEqual(manifest.VaultAddresses, manifest.verifiedVault) {
		return fmt.Errorf("ALT manifest changed after typed source construction")
	}
	if manifest.boundRequirementsFingerprint != "" {
		fp, err := crossMintExternalRequirementsFingerprint(manifest.Fingerprint, manifest.externalSnapshots)
		if err != nil || fp != manifest.boundRequirementsFingerprint {
			return fmt.Errorf("ALT manifest lost finalized external requirement binding")
		}
	}
	return nil
}

// ALTManifestRequirementsFingerprint exposes only builder-bound requirements.
// A caller-computed composite hash cannot create finalized external provenance.
func ALTManifestRequirementsFingerprint(manifest *ALTManifest) (string, error) {
	if err := ValidateALTManifestIntegrity(manifest); err != nil {
		return "", err
	}
	if manifest.boundRequirementsFingerprint != "" {
		return manifest.boundRequirementsFingerprint, nil
	}
	return manifest.Fingerprint, nil
}

func ALTManifestExternalSnapshots(manifest *ALTManifest) ([]CrossMintExternalALT, error) {
	if err := ValidateALTManifestIntegrity(manifest); err != nil {
		return nil, err
	}
	if manifest.boundRequirementsFingerprint == "" {
		return nil, nil
	}
	return cloneCrossMintExternalALTs(manifest.externalSnapshots), nil
}

// ALTManifestSourceAddresses returns the original full source identities for
// store binding checks. Durable demand may exclude keys proven covered by an
// actual finalized external table; the original v1 fingerprint still binds all
// typed source accounts. Returned slices cannot mutate that private provenance.
func ALTManifestSourceAddresses(manifest *ALTManifest) (shared, vault []ALTManifestAddress, err error) {
	if err := ValidateALTManifestIntegrity(manifest); err != nil {
		return nil, nil, err
	}
	return append([]ALTManifestAddress{}, manifest.sourceShared...), append([]ALTManifestAddress{}, manifest.sourceVault...), nil
}

// filterFinalizedExternalALTManifest ports the retained verifier's durable
// manifest projection. Coverage is accepted only after the concrete shared
// reader proves the exact table vectors, active chain state and warm slot.
// This projection does not register tables or grant execution usage leases.
func (r *Revalidator) filterFinalizedExternalALTManifest(ctx context.Context, manifest ALTManifest, tables []LookupTable, minimumSlot int64) (ALTManifest, error) {
	if err := ValidateALTManifestIntegrity(&manifest); err != nil {
		return ALTManifest{}, err
	}
	if len(tables) == 0 {
		return manifest, nil
	}
	if r == nil || r.rpc == nil || minimumSlot <= 0 {
		return ALTManifest{}, fmt.Errorf("external manifest coverage requires actual finalized ALT reader")
	}
	for _, table := range tables {
		if !table.Active || len(table.Addresses) == 0 || table.Generation < 0 || table.MutationEpoch < 0 {
			return ALTManifest{}, fmt.Errorf("external manifest coverage lacks active full table vectors")
		}
	}
	names := make([]string, len(tables))
	for i, table := range tables {
		names[i] = table.Address
	}
	observedSlot, accounts, err := r.rpc.FinalizedAccounts(ctx, names, minimumSlot)
	if err != nil {
		return ALTManifest{}, err
	}
	if observedSlot < minimumSlot || len(accounts) != len(tables) {
		return ALTManifest{}, fmt.Errorf("external ALT readback is incomplete or stale")
	}
	var snapshots []CrossMintExternalALT
	external := map[string]bool{}
	for i, account := range accounts {
		table, err := decodeLookupTable(account, observedSlot)
		if err != nil || table.Address != tables[i].Address || !equalStrings(table.Addresses, tables[i].Addresses) {
			return ALTManifest{}, fmt.Errorf("external ALT finalized vector differs from supplied source input")
		}
		hash, err := CrossMintExternalAddressHash(table.Addresses)
		if err != nil {
			return ALTManifest{}, err
		}
		snapshots = append(snapshots, CrossMintExternalALT{Address: table.Address, Addresses: append([]string{}, table.Addresses...), OrderedAddressHash: hash, ObservedSlot: table.LastVerifiedSlot, UsableAfterSlot: table.UsableAfterSlot})
		for _, address := range table.Addresses {
			external[address] = true
		}
	}
	filter := func(original []ALTManifestAddress) []ALTManifestAddress {
		rows := []ALTManifestAddress{}
		for _, record := range original {
			if !external[record.Address] {
				record.Ordinal = int32(len(rows))
				rows = append(rows, record)
			}
		}
		return rows
	}
	manifest.SharedAddresses = filter(manifest.sourceShared)
	manifest.VaultAddresses = filter(manifest.sourceVault)
	manifest.verifiedShared = append([]ALTManifestAddress{}, manifest.SharedAddresses...)
	manifest.verifiedVault = append([]ALTManifestAddress{}, manifest.VaultAddresses...)
	manifest.externalSnapshots = snapshots
	manifest.boundRequirementsFingerprint = ""
	return manifest, nil
}

func (r *Revalidator) bindFinalizedCrossMintALTManifest(ctx context.Context, manifest ALTManifest, tables []LookupTable, minimumSlot int64) (ALTManifest, error) {
	manifest, err := r.filterFinalizedExternalALTManifest(ctx, manifest, tables, minimumSlot)
	if err != nil {
		return ALTManifest{}, err
	}
	manifest.boundRequirementsFingerprint, err = crossMintExternalRequirementsFingerprint(manifest.Fingerprint, manifest.externalSnapshots)
	if err != nil {
		return ALTManifest{}, err
	}
	return manifest, nil
}

func retainedSameMintRequirementsFingerprint(input KaminoSameMintRouteRequest, policy, payer string, instructions []RouteInstruction) (string, error) {
	manifest, err := BuildRouteALTManifest(input, "", policy, payer, instructions, nil)
	return manifest.Fingerprint, err
}

// BuildRouteALTManifest derives typed provenance from decoded KLend positions
// and exact builder instructions. For a strictly validated direct Jupiter
// instruction it ports fleet-worker's jupiter_swap_lookup_table_requirements:
// route mints are liquidity_mint; canonical vault ATAs are vault_token_account;
// remaining validated swap accounts are infrastructure. Unknown outer accounts
// still fail. No table membership or user-supplied role can invent provenance.
func BuildRouteALTManifest(input KaminoSameMintRouteRequest, settings, policy, payer string, instructions []RouteInstruction, validatedSwap *RouteInstruction) (ALTManifest, error) {
	return buildRouteALTManifest(input, settings, []string{policy}, payer, instructions, validatedSwap, validatedSwap == nil)
}

// BuildCrossMintPreflightALTManifest merges both actual builder provenance
// sets for the atomic verifier. It never describes an executable movement leg.
func BuildCrossMintPreflightALTManifest(input KaminoSameMintRouteRequest, settings, withdrawPolicy, swapPolicy, payer string, instructions []RouteInstruction, validatedSwap RouteInstruction) (ALTManifest, error) {
	return buildRouteALTManifest(input, settings, []string{withdrawPolicy, swapPolicy}, payer, instructions, &validatedSwap, true)
}

func buildRouteALTManifest(input KaminoSameMintRouteRequest, settings string, policies []string, payer string, instructions []RouteInstruction, validatedSwap *RouteInstruction, includeKLend bool) (ALTManifest, error) {
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
	if err := add(settings, 2, 0); err != nil {
		return ALTManifest{}, err
	}
	for _, x := range []struct {
		key string
		tag byte
	}{{input.Vault, 1}} {
		if err := add(x.key, 2, x.tag); err != nil {
			return ALTManifest{}, err
		}
	}
	for _, policy := range policies {
		if err := add(policy, 2, 3); err != nil {
			return ALTManifest{}, err
		}
	}
	if includeKLend {
		for _, p := range []KaminoPositionAccounts{input.Source, input.Target} {
			for tag, key := range []string{p.Market, p.MarketAuthority, p.Reserve, p.LiquidityMint, p.LiquiditySupply, p.CollateralMint, p.CollateralSupply} {
				if err := add(key, 1, byte(tag)); err != nil {
					return ALTManifest{}, err
				}
			}
			for _, x := range []struct {
				key        string
				class, tag byte
			}{{p.Obligation, 2, 2}, {p.VaultLiquidityATA, 2, 5}, {p.LiquidityTokenProgram, 1, 10}} {
				if err := add(x.key, x.class, x.tag); err != nil {
					return ALTManifest{}, err
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
					return ALTManifest{}, err
				}
			}
			for _, key := range append(append([]string{}, p.ObligationDepositReserves...), p.ObligationBorrowReserves...) {
				if err := add(key, 1, 2); err != nil {
					return ALTManifest{}, err
				}
			}
		}
	}
	if validatedSwap != nil {
		routeMints := map[string]bool{input.Source.LiquidityMint: true, input.Target.LiquidityMint: true}
		for mint := range routeMints {
			if err := add(mint, 1, 3); err != nil {
				return ALTManifest{}, err
			}
		}
		vaultATAs := map[string]bool{}
		for _, mint := range earnStableMints {
			ata, err := deriveATA(input.Vault, mint, mustStableProgram(mint))
			if err != nil {
				return ALTManifest{}, err
			}
			vaultATAs[ata] = true
			if err := add(ata, 2, 5); err != nil {
				return ALTManifest{}, err
			}
		}
		for _, account := range validatedSwap.Accounts {
			if account.Address != input.Vault && !vaultATAs[account.Address] && !routeMints[account.Address] {
				if err := add(account.Address, 1, 10); err != nil {
					return ALTManifest{}, err
				}
			}
		}
		if err := add(validatedSwap.Program, 1, 10); err != nil {
			return ALTManifest{}, err
		}
	}

	for _, key := range []string{"11111111111111111111111111111111", "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA", "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL", "Sysvar1nstructions1111111111111111111111111", "SysvarRent111111111111111111111111111111111", KLendProgram, farmsProgram} {
		if err := add(key, 1, 10); err != nil {
			return ALTManifest{}, err
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
		return ALTManifest{}, err
	}
	if err := touch(payer, true, &signer); err != nil {
		return ALTManifest{}, err
	}
	for _, ix := range instructions {
		if err := touch(ix.Program, false, &program); err != nil {
			return ALTManifest{}, err
		}
		for _, account := range ix.Accounts {
			var reason *byte
			if account.Signer {
				reason = &signer
			}
			if err := touch(account.Address, account.Writable, reason); err != nil {
				return ALTManifest{}, err
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
				return ALTManifest{}, err
			}
		}
	}
	groups := [3][]*requirement{}
	for key, r := range actual {
		if r.class != 0 {
			role := roles[key]
			if role == nil {
				return ALTManifest{}, fmt.Errorf("missing typed provenance for %s", key)
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
	manifest := ALTManifest{Fingerprint: hex.EncodeToString(digest[:]), SharedAddresses: []ALTManifestAddress{}, VaultAddresses: []ALTManifestAddress{}}
	roleNames := [3][]string{nil, {"market", "market_authority", "reserve", "liquidity_mint", "liquidity_supply", "collateral_mint", "collateral_supply", "oracle", "scope_prices", "reserve_farm_state", "infrastructure"}, {"settings", "vault", "obligation", "policy", "action_account", "vault_token_account", "metadata", "farm_user_state"}}
	for class := 1; class <= 2; class++ {
		records := make([]ALTManifestAddress, 0, len(groups[class]))
		for _, r := range groups[class] {
			var tags []string
			for tag, name := range roleNames[class] {
				if r.tags[byte(tag)] {
					tags = append(tags, name)
				}
			}
			semanticClass := "shared_market"
			if class == 2 {
				semanticClass = "vault"
			}
			records = append(records, ALTManifestAddress{Address: encodeBase58(r.key[:]), SemanticClass: semanticClass, AccountRole: strings.Join(tags, ","), Writable: r.writable})
		}
		sort.Slice(records, func(i, j int) bool { return records[i].Address < records[j].Address })
		for i := range records {
			records[i].Ordinal = int32(i)
		}
		if class == 1 {
			manifest.SharedAddresses = records
		} else {
			manifest.VaultAddresses = records
		}
	}
	manifest.fingerprintMaterial = bytes.Clone(encoded)
	manifest.verifiedShared = append([]ALTManifestAddress{}, manifest.SharedAddresses...)
	manifest.verifiedVault = append([]ALTManifestAddress{}, manifest.VaultAddresses...)
	manifest.sourceShared = append([]ALTManifestAddress{}, manifest.SharedAddresses...)
	manifest.sourceVault = append([]ALTManifestAddress{}, manifest.VaultAddresses...)
	return manifest, nil
}

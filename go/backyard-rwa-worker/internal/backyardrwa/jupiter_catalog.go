package backyardrwa

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Extracted from retained compiled+installed policy evidence, not fresh quotes.
// The live policy hash and current account state are still checked at prepare.
//
//go:embed manifest/catalog-jupiter-v1.json
var catalogJupiterJSON []byte

type catalogJupiterBinding struct {
	From, To, Policy, PolicySHA256                                                       string
	ConstraintIndex                                                                      byte
	Authority, SourceCustody, DestinationCustody, SourceMint, DestinationMint            string
	SourceTokenProgram, DestinationTokenProgram                                          string
	AuthorityIndex, SourceIndex, DestinationIndex, SourceMintIndex, DestinationMintIndex int
	SourceTokenProgramIndex, DestinationTokenProgramIndex                                int
	DiscriminatorHex                                                                     string
	AmountOffset, SlippageOffset, FeeOffset                                              int
	MaxInputRaw                                                                          uint64
	MaxSlippageBPS                                                                       uint16
}

func catalogJupiterRoute(lane string) bool {
	return lane == "AUTO/AUTO/PYUSD" || lane == "Ethena/USDe/PYUSD" || lane == "Prime/PRIME/PYUSD" || lane == "Prime/PRIME/USDS"
}

// Policy availability only. Quotes, packet/compute fit, setup and reserved
// exits must still be established by production admission before signing.
func catalogRoutePolicyHashes(route RuntimeRoute, manifest RouteManifest) (map[string]string, error) {
	wanted := map[string]string{}
	add := func(address, hash string) error {
		if _, err := decodeKey(address); err != nil || !validSHA256(hash) {
			return fmt.Errorf("invalid catalog policy identity")
		}
		if old, ok := wanted[address]; ok && old != hash {
			return fmt.Errorf("conflicting catalog policy hashes")
		}
		wanted[address] = hash
		return nil
	}
	if len(route.KaminoPolicies) != 4 || len(manifest.RuntimeBindings.BridgePolicies) != 4 {
		return nil, fmt.Errorf("incomplete catalog policy graph")
	}
	for _, b := range route.KaminoPolicies {
		if err := add(b.Policy, b.DataSHA256); err != nil {
			return nil, err
		}
	}
	for _, b := range manifest.RuntimeBindings.BridgePolicies {
		if b.NormalizedDigest == "" {
			return nil, fmt.Errorf("unbound bridge policy")
		}
		if err := add(b.Account, b.NormalizedDigest); err != nil {
			return nil, err
		}
	}
	for _, action := range []Action{SwapStableToCollateralStep, SwapCollateralToStableStep, SwapDebtToCollateralStep, SwapCollateralToDebtStep, SwapUSDCToDebtStep, SwapDebtToUSDCStep} {
		b, err := catalogJupiterBindingForRoute(action, route.Lane)
		if err != nil {
			return nil, err
		}
		if err = add(b.Policy, b.PolicySHA256); err != nil {
			return nil, err
		}
	}
	return wanted, nil
}

func catalogJupiterBindingForRoute(action Action, lane string) (catalogJupiterBinding, error) {
	if !catalogJupiterRoute(lane) {
		return catalogJupiterBinding{}, fmt.Errorf("unregistered Jupiter catalog lane")
	}
	route, err := runtimeRoute(lane)
	if err != nil {
		return catalogJupiterBinding{}, err
	}
	type asset struct{ symbol, mint, custody, program string }
	usdc := asset{"USDC", bridgeUSDC, bridgeSquadsATA, classicTokenProgram}
	collateral := asset{route.CollateralSymbol, route.Kamino.CollateralMint, route.CollateralCustody, route.CollateralTokenProgram}
	debt := asset{route.DebtSymbol, route.Kamino.DebtMint, route.DebtCustody, route.DebtTokenProgram}
	var from, to asset
	switch action {
	case SwapStableToCollateralStep:
		from, to = usdc, collateral
	case SwapCollateralToStableStep:
		from, to = collateral, usdc
	case SwapDebtToCollateralStep:
		from, to = debt, collateral
	case SwapCollateralToDebtStep:
		from, to = collateral, debt
	case SwapUSDCToDebtStep:
		from, to = usdc, debt
	case SwapDebtToUSDCStep:
		from, to = debt, usdc
	default:
		return catalogJupiterBinding{}, fmt.Errorf("action is not a catalog conversion")
	}
	var entries []catalogJupiterBinding
	if err = json.Unmarshal(catalogJupiterJSON, &entries); err != nil {
		return catalogJupiterBinding{}, err
	}
	var matches []catalogJupiterBinding
	for _, b := range entries {
		if b.From == from.symbol && b.To == to.symbol {
			matches = append(matches, b)
		}
	}
	if len(matches) != 1 {
		return catalogJupiterBinding{}, fmt.Errorf("missing or duplicate catalog conversion")
	}
	b := matches[0]
	if b.Authority != bridgeVault || b.SourceMint != from.mint || b.DestinationMint != to.mint ||
		b.SourceCustody != from.custody || b.DestinationCustody != to.custody ||
		b.SourceTokenProgram != from.program || b.DestinationTokenProgram != to.program ||
		!validSHA256(b.PolicySHA256) || b.AmountOffset < 8 || b.SlippageOffset != b.AmountOffset+16 || b.FeeOffset != b.SlippageOffset+2 ||
		b.MaxInputRaw == 0 || b.MaxSlippageBPS > jupiterMaxSlippageBPS {
		return catalogJupiterBinding{}, fmt.Errorf("catalog conversion identity or layout drifted")
	}
	return b, nil
}

func (b catalogJupiterBinding) fixedPrefixV2() bool {
	return b.DiscriminatorHex == "d19853937cfed8e9"
}

// V2 places economics before its variable route vector. This only recognizes
// a reviewed binding; it never upgrades the installed catalog implicitly.
func (b catalogJupiterBinding) matchesData(data []byte) bool {
	discriminator, err := hex.DecodeString(b.DiscriminatorHex)
	if err != nil || len(discriminator) != 8 || len(data) < 8 || !bytes.Equal(data[:8], discriminator) {
		return false
	}
	if b.fixedPrefixV2() {
		return b.AmountOffset == 9 && b.SlippageOffset == 25 && b.FeeOffset == 27 &&
			b.AuthorityIndex == 1 && b.SourceIndex == 2 && b.DestinationIndex == 5 &&
			b.SourceMintIndex == 6 && b.DestinationMintIndex == 7 &&
			b.SourceTokenProgramIndex == 8 && b.DestinationTokenProgramIndex == 9 &&
			len(data) >= 40 && len(data) <= 1232 && allZero(data[27:31]) &&
			binary.LittleEndian.Uint32(data[31:35]) > 0 && binary.LittleEndian.Uint32(data[31:35]) <= jupiterMaxRoutePlanLeg
	}
	return b.AmountOffset >= 8 && b.SlippageOffset == b.AmountOffset+16 &&
		b.FeeOffset == b.SlippageOffset+2 && len(data) == b.FeeOffset+1 && data[b.FeeOffset] == 0
}

func validateCatalogJupiterInstruction(value JupiterSwapInstruction, action Action, amount, out, minimum uint64, lane string) (compiledInstruction, error) {
	b, err := catalogJupiterBindingForRoute(action, lane)
	if err != nil {
		return compiledInstruction{}, err
	}
	data, err := base64.StdEncoding.Strict().DecodeString(value.Data)
	if err != nil || !b.matchesData(data) || value.ProgramID != jupiterV6Program || len(value.Accounts) > 64 ||
		amount == 0 || amount > b.MaxInputRaw || minimum == 0 || minimum > out ||
		readU64(data[b.AmountOffset:]) != amount || readU64(data[b.AmountOffset+8:]) != out ||
		uint16(data[b.SlippageOffset])|uint16(data[b.SlippageOffset+1])<<8 > b.MaxSlippageBPS || data[b.FeeOffset] != 0 {
		return compiledInstruction{}, fmt.Errorf("Jupiter instruction does not match installed edge economics or layout")
	}
	for _, boundary := range []struct {
		index            int
		key              string
		signer, writable bool
	}{
		{b.AuthorityIndex, b.Authority, true, false}, {b.SourceIndex, b.SourceCustody, false, true}, {b.DestinationIndex, b.DestinationCustody, false, true},
		{b.SourceMintIndex, b.SourceMint, false, false}, {b.DestinationMintIndex, b.DestinationMint, false, false},
		{b.SourceTokenProgramIndex, b.SourceTokenProgram, false, false}, {b.DestinationTokenProgramIndex, b.DestinationTokenProgram, false, false},
	} {
		if boundary.index < 0 || boundary.index >= len(value.Accounts) {
			return compiledInstruction{}, fmt.Errorf("Jupiter catalog account boundary missing")
		}
		got := value.Accounts[boundary.index]
		if got.Pubkey != boundary.key || got.IsSigner != boundary.signer || got.IsWritable != boundary.writable {
			return compiledInstruction{}, fmt.Errorf("Jupiter catalog account boundary %d drifted", boundary.index)
		}
	}
	accounts := make([]accountMeta, len(value.Accounts))
	for i, input := range value.Accounts {
		if input.Pubkey == previousBackyardVault || (input.IsSigner && input.Pubkey != bridgeVault) {
			return compiledInstruction{}, fmt.Errorf("Jupiter authority drifted")
		}
		key, err := decodeKey(input.Pubkey)
		if err != nil {
			return compiledInstruction{}, err
		}
		accounts[i] = accountMeta{key: key, signer: input.IsSigner, writable: input.IsWritable}
	}
	return compiledInstruction{program: mustKey(jupiterV6Program), accounts: accounts, data: data}, nil
}

package backyardrwa

import (
	"encoding/binary"
	"fmt"
)

// All identities are fixed by the lane and a reviewed, installed policy. Rent
// and network fees are native SOL flows, separate from user USDC principal.
type KaminoInitializationRequest struct {
	RouteLane               string `json:"routeLane"`
	PolicySeed              uint64 `json:"policySeed"`
	PolicyAccountDataSHA256 string `json:"policyAccountDataSha256"`
	RecentBlockhash         string `json:"recentBlockhash"`
	LastValidBlockHeight    int64  `json:"lastValidBlockHeight"`
	RentLamports            uint64 `json:"rentLamports"`
	MaximumFeeLamports      uint64 `json:"maximumFeeLamports"`
}

func CompileKaminoInitializationMessage(r KaminoInitializationRequest) ([]byte, error) {
	if r.LastValidBlockHeight <= 0 || r.RentLamports == 0 || r.MaximumFeeLamports == 0 || !validSHA256(r.PolicyAccountDataSHA256) {
		return nil, fmt.Errorf("incomplete Multiply initialization admission")
	}
	policy, err := policySetupAddress(r.PolicySeed)
	if err != nil {
		return nil, err
	}
	blockhash, err := decodeKey(r.RecentBlockhash)
	if err != nil {
		return nil, err
	}
	inner, err := kaminoMultiplyInitializer(r.RouteLane)
	if err != nil {
		return nil, err
	}
	outer, err := wrapSquadsKaminoPolicy(policy, mustKey(bridgeDelegate), mustKey(bridgeDelegate), 0, inner)
	if err != nil {
		return nil, err
	}
	message, err := compileLegacyMessage(mustKey(bridgeDelegate), blockhash, []compiledInstruction{outer})
	if err != nil {
		return nil, err
	}
	return checkedUnsignedMessage(message)
}

func validateInitializedKaminoObligation(r KaminoInitializationRequest, a ConfirmedAccount) error {
	route, err := runtimeRoute(r.RouteLane)
	if err != nil || !selectorLane(route.Lane) {
		return fmt.Errorf("unreviewed initialized obligation")
	}
	o, err := decodeKaminoObligation(a, route.Kamino)
	if err != nil {
		return err
	}
	if a.Lamports != r.RentLamports || binary.LittleEndian.Uint64(a.Data[8:16]) != 1 || o.hasPosition ||
		!allZero(a.Data[96:1184]) || !allZero(a.Data[1208:2208]) ||
		a.Data[kaminoObligationElevationGroupOffset] != 0 || !allZero(a.Data[2288:2320]) {
		return fmt.Errorf("initialized obligation state differs from admitted empty Multiply account")
	}
	return nil
}

// Construct only the three reviewed Multiply PDAs. Policy installation, rent
// admission and journal execution are separate; this function grants no authority.
func kaminoMultiplyInitializer(lane string) (compiledInstruction, error) {
	route, err := runtimeRoute(lane)
	if err != nil || !selectorLane(route.Lane) || route.Kamino.DebtMint != bridgeUSDC {
		return compiledInstruction{}, fmt.Errorf("unreviewed Multiply initializer lane")
	}
	program, owner := mustKey(kaminoProgram), mustKey(bridgeVault)
	market, collateral, debt := mustKey(route.Kamino.Market), mustKey(route.Kamino.CollateralMint), mustKey(bridgeUSDC)
	obligation, err := findProgramDerivedAddress([]byte{1}, program[:], []byte{0}, owner[:], market[:], collateral[:], debt[:])
	if err != nil || obligation != route.Kamino.Obligation {
		return compiledInstruction{}, fmt.Errorf("Multiply obligation PDA drifted")
	}
	metadata, err := findProgramDerivedAddress([]byte("user_meta"), program[:], owner[:])
	if err != nil {
		return compiledInstruction{}, err
	}
	return compiledInstruction{program: program,
		accounts: []accountMeta{
			meta(bridgeVault, true, false), meta(bridgeVault, true, true),
			meta(obligation, false, true), meta(route.Kamino.Market, false, false),
			meta(route.Kamino.CollateralMint, false, false), meta(bridgeUSDC, false, false),
			meta(metadata, false, false), meta("SysvarRent111111111111111111111111111111111", false, false),
			meta("11111111111111111111111111111111", false, false),
		}, data: []byte{251, 10, 231, 76, 27, 11, 159, 96, 1, 0}}, nil
}

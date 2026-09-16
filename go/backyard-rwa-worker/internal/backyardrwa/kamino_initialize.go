package backyardrwa

import "fmt"

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

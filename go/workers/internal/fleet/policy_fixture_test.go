package fleet

import "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet/fleettest"

func buildExactPolicyFixture(settings, signer string, accountIndex uint8, instructions []RouteInstruction) ([]byte, error) {
	converted := make([]fleettest.Instruction, len(instructions))
	for i, ix := range instructions {
		converted[i] = fleettest.Instruction{Program: ix.Program, Data: ix.Data}
	}
	return fleettest.ExactPolicy(settings, signer, accountIndex, converted)
}

package autodeposit

import (
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet/fleettest"
)

func exactPolicyFixture(settings, signer string, accountIndex uint8, instructions []fleet.RouteInstruction) ([]byte, error) {
	converted := make([]fleettest.Instruction, len(instructions))
	for i, ix := range instructions {
		converted[i] = fleettest.Instruction{Program: ix.Program, Data: ix.Data}
	}
	return fleettest.ExactPolicy(settings, signer, accountIndex, converted)
}

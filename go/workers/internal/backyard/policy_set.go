package backyard

import (
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/solana-foundation/solana-go/v2"
)

// BasicPolicyFamily identifies one of the four shared ProgramInteraction
// policies installed by the Rust/TypeScript track. The family owns the policy
// account; each lifecycle leg selects a constraint inside that account.
type BasicPolicyFamily string

const (
	BasicCollateralLifecycle BasicPolicyFamily = "CollateralLifecycle"
	BasicDebtLifecycle       BasicPolicyFamily = "DebtLifecycle"
	BasicSwapRoutesA         BasicPolicyFamily = "SwapRoutesA"
	BasicSwapRoutesB         BasicPolicyFamily = "SwapRoutesB"
)

func deriveKaminoObligationFarmUserState(reserveFarmState, obligation string) (string, error) {
	farm, err := decodeKey(reserveFarmState)
	if err != nil {
		return "", fmt.Errorf("decode reserve farm state: %w", err)
	}
	obligationKey, err := decodeKey(obligation)
	if err != nil {
		return "", fmt.Errorf("decode obligation: %w", err)
	}
	user, err := kamino.ObligationFarmUserState(solana.PublicKey(farm), solana.PublicKey(obligationKey))
	return user.String(), err
}

const onreDebtFarmState = "7vNfe1qX8iDxP5p3A4fosrjLqdn1YjmmGcZZkG2b4APF"

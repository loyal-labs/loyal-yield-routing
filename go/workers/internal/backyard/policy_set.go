package backyard

import (
	"fmt"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/solana-foundation/solana-go/v2"
)

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

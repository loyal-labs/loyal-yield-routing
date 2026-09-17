package backyardrwa

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

type PilotBudgetActivationResult struct {
	ProofLevel string                `json:"proofLevel"`
	Activation pilotBudgetActivation `json:"activation"`
}

// Explicit operator bookkeeping only: no signer, policy installation, deposit
// switch or worker start. The fixed transition itself obtains finalized chain
// evidence under the route lock and preserves the archived prior budget.
func RunPilotBudgetActivation(ctx context.Context, databaseURL, rpcURL, routeKey string) (result PilotBudgetActivationResult, err error) {
	if routeKey != productionRouteKey || databaseURL == "" || rpcURL == "" {
		return result, budgetHold("invalid_pilot_activation_config")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rpc, err := NewRPCClient(rpcURL)
	if err != nil {
		return result, budgetHold("pilot_activation_rpc_unavailable")
	}
	db, err := OpenDatabase(ctx, databaseURL)
	if err != nil {
		return result, budgetHold("pilot_activation_database_unavailable")
	}
	defer db.Close()
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return result, budgetHold("pilot_activation_owner_unavailable")
	}
	if _, err = db.AcquireRouteLease(ctx, routeKey, "pilot-activation:"+hex.EncodeToString(nonce[:]), 45*time.Second); err != nil {
		return result, budgetHold("pilot_activation_lease_unavailable")
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		released, releaseErr := db.ReleaseRouteLease(releaseCtx)
		if err == nil && (releaseErr != nil || !released) {
			err = budgetHold("pilot_activation_lease_release_unconfirmed")
		}
	}()
	result.Activation, err = db.activatePilotBudget(ctx, rpc)
	if err != nil {
		var hold *BudgetHold
		if errors.As(err, &hold) {
			return result, hold
		}
		// SQL/RPC failures may embed credentials. Emit only a fixed stage error.
		return result, budgetHold("pilot_activation_not_confirmed")
	}
	result.ProofLevel = "PILOT_BUDGET_AUTHORITY_NOT_DEPOSIT_READINESS"
	return result, nil
}

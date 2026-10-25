package webapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
)

type dprepOnboardingAttempt struct {
	policyAccount      string
	policySeed         int64
	routeSignature     *string
	routeConfirmedSlot *int64
	setupSignature     *string
	setupAccount       *string
	setupSeed          *int64
}

func (h dprepRoutes) dprepOnboardingAttempt(ctx context.Context, account EarnAccount) (*dprepOnboardingAttempt, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var attempt dprepOnboardingAttempt
	err := h.YieldDB.QueryRow(ctx, `SELECT policy_account, policy_seed, route_policy_signature, route_policy_confirmed_slot,
  setup_policy_signature, setup_policy_account, setup_policy_seed
FROM loyal_yield.earn_deposit_onboarding_attempts
WHERE settings = $1 AND vault_index = $2 AND wallet_address = $3 AND status <> 'complete' AND vault_pubkey = $4
ORDER BY updated_at DESC, id DESC
LIMIT 1`, account.Settings.String(), int16(earnprep.EarnVaultIndex), account.Wallet.String(), account.Vault.String()).Scan(
		&attempt.policyAccount, &attempt.policySeed, &attempt.routeSignature, &attempt.routeConfirmedSlot,
		&attempt.setupSignature, &attempt.setupAccount, &attempt.setupSeed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read current Earn deposit onboarding attempt: %w", err)
	}
	return &attempt, nil
}

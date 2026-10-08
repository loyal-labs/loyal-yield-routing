package autodeposit

import (
	"errors"
	"testing"
	"time"
)

// Production delegations carry expiry 0, which the Subscriptions program
// reads as never expiring; reading it as long expired deferred every pull.
// A real expiry in the past still defers.
func TestAllowanceReadTreatsZeroExpiryAsOpen(t *testing.T) {
	controller := &Controller{chain: &scriptedControllerChain{}}
	period, start, nonce := int64(2592000), int64(1784019890), int64(1784019837377)
	window := func(expiry int64) *TargetExecutionContext {
		return &TargetExecutionContext{Wallet: "wallet", WalletUsdcAta: "wallet-ata", VaultPubkey: "vault", TokenMint: USDCMint, RecurringDelegation: "delegation",
			PeriodLengthSeconds: &period, StartTimestamp: &start, RecurringDelegationNonce: &nonce, ExpiryTimestamp: &expiry}
	}
	if allowance, err := controller.readRemainingAllowance(t.Context(), window(0)); err != nil || allowance == nil {
		t.Fatal("expiry 0 deferred", allowance, err)
	}
	if _, err := controller.readRemainingAllowance(t.Context(), window(time.Now().Add(-time.Hour).Unix())); !errors.Is(err, ErrAllowanceUnknown) {
		t.Fatal("expired delegation read", err)
	}
}

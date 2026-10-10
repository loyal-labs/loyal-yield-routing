package backyard

// Regression for the proven AUTO destination quote blocker (plan 48). The
// canary inner stage diagnostic pinned the failure to the oracle account
// fetch, and the root read-only reserve probe
// (/private/tmp/auto-oracle-account-probe-20260921-005521.json) showed both
// AUTO reserves' oracle config slots 5160/5192/5224 holding the pinned
// klend-sdk NULL_PUBKEY sentinel while slot 5112 holds the real scope
// oracle. uniqueNonzero only drops EMPTY keys, so the sentinel reached the
// oracle fetch as a required account and failed the whole observation.
// The fix filters exactly that documented sentinel value at the reserve
// oracle-key extraction (kamino_observe.go decodeKaminoReserve) — nothing
// else. These tests run the REAL observeKaminoFromFixedAccounts.

import "testing"

// oracleSentinelFixture shapes the route accounts exactly like the probed
// AUTO reserves: the real scope oracle in slot 5112 of BOTH reserves and the
// documented NULL_PUBKEY sentinel in every unused oracle slot.
func oracleSentinelFixture(t *testing.T) (RuntimeRoute, []ConfirmedAccount) {
	t.Helper()
	route, accounts := nonUSDCDebtNAVFixture(t)
	putKey(t, accountAt(accounts, route.Kamino.CollateralReserve).Data[5112:5144], kaminoScopePrices)
	putKey(t, accountAt(accounts, route.Kamino.DebtReserve).Data[5112:5144], kaminoScopePrices)
	for _, reserve := range []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve} {
		for _, offset := range []int{5160, 5192, 5224} {
			putKey(t, accountAt(accounts, reserve).Data[offset:offset+32], kaminoOracleSentinelPubkey)
		}
	}
	return route, accounts
}

// TestKaminoOracleSentinelIsNeverAConfiguredOracle is the primary
// regression: with the AUTO probe's account images the observation succeeds
// and only the real scope oracle is configured.
func TestKaminoOracleSentinelIsNeverAConfiguredOracle(t *testing.T) {
	route, accounts := oracleSentinelFixture(t)
	position, err := observeKaminoFromFixedAccounts(77, append(append([]ConfirmedAccount{}, accounts...), clockFixture()), route.Kamino)
	if err != nil {
		t.Fatalf("sentinel oracle config broke the real observation: %v", err)
	}
	if len(position.Oracles) != 1 || position.Oracles[0] != kaminoScopePrices || !position.ObligationPresent {
		t.Fatalf("configured oracles %v, want only the real scope oracle", position.Oracles)
	}
}

// TestKaminoOracleSentinelOnlyConfiguredStillFails keeps the
// no-configured-oracle refusal exact: a reserve pair configured with ONLY the
// sentinel fails with the unchanged fixed error.
func TestKaminoOracleSentinelOnlyConfiguredStillFails(t *testing.T) {
	route, accounts := oracleSentinelFixture(t)
	putKey(t, accountAt(accounts, route.Kamino.CollateralReserve).Data[5112:5144], kaminoOracleSentinelPubkey)
	putKey(t, accountAt(accounts, route.Kamino.DebtReserve).Data[5112:5144], kaminoOracleSentinelPubkey)
	_, err := observeKaminoFromFixedAccounts(77, append(append([]ConfirmedAccount{}, accounts...), clockFixture()), route.Kamino)
	if err == nil || err.Error() != "Kamino reserve has no configured oracle" {
		t.Fatalf("sentinel-only configuration: got %v, want the unchanged no-configured-oracle refusal", err)
	}
}

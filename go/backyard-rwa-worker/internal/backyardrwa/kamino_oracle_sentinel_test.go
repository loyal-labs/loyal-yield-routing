package backyardrwa

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
// else. These tests run the REAL observeKaminoFromFixedAccounts with a mock
// accounts reader (the same seam the production fixed-account path uses).

import (
	"context"
	"strings"
	"testing"
)

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

// TestKaminoOracleSentinelNeverRequestedWhileRealOracleIs is the primary
// regression: with the AUTO probe's account images the observation succeeds
// and the oracle fetch requests ONLY the real configured oracle — the
// sentinel is never requested (it failed the observation before the fix).
func TestKaminoOracleSentinelNeverRequestedWhileRealOracleIs(t *testing.T) {
	route, accounts := oracleSentinelFixture(t)
	requested := []string(nil)
	position, err := observeKaminoFromFixedAccounts(context.Background(),
		func(_ context.Context, addresses []string, slot int64) (int64, []ConfirmedAccount, error) {
			for _, address := range addresses {
				if address == kaminoOracleSentinelPubkey {
					t.Fatalf("documented oracle sentinel requested as an account: %s", address)
				}
			}
			requested = append(requested, addresses...)
			return slot, []ConfirmedAccount{{Address: kaminoScopePrices, Lamports: 1, Data: []byte{1}}}, nil
		}, 77, append(append([]ConfirmedAccount{}, accounts...), clockFixture()), route.Kamino)
	if err != nil {
		t.Fatalf("sentinel oracle config broke the real observation: %v", err)
	}
	if len(requested) != 1 || requested[0] != kaminoScopePrices {
		t.Fatalf("oracle fetch requested %v, want only the real configured oracle", requested)
	}
	if !position.ObligationPresent {
		t.Fatal("real observation lost the obligation")
	}
}

// TestKaminoOracleSentinelOnlyConfiguredStillFailsWithoutOracleFetch keeps
// the no-configured-oracle refusal exact: a reserve pair configured with
// ONLY the sentinel must fail with the unchanged fixed error and must not
// attempt any oracle account fetch at all.
func TestKaminoOracleSentinelOnlyConfiguredStillFailsWithoutOracleFetch(t *testing.T) {
	route, accounts := oracleSentinelFixture(t)
	putKey(t, accountAt(accounts, route.Kamino.CollateralReserve).Data[5112:5144], kaminoOracleSentinelPubkey)
	putKey(t, accountAt(accounts, route.Kamino.DebtReserve).Data[5112:5144], kaminoOracleSentinelPubkey)
	readerCalls := 0
	_, err := observeKaminoFromFixedAccounts(context.Background(),
		func(_ context.Context, _ []string, slot int64) (int64, []ConfirmedAccount, error) {
			readerCalls++
			return slot, nil, nil
		}, 77, append(append([]ConfirmedAccount{}, accounts...), clockFixture()), route.Kamino)
	if err == nil || err.Error() != "Kamino reserve has no configured oracle" {
		t.Fatalf("sentinel-only configuration: got %v, want the unchanged no-configured-oracle refusal", err)
	}
	if readerCalls != 0 {
		t.Fatalf("oracle fetch attempted %d times with only sentinel config", readerCalls)
	}
}

// TestKaminoGenuinelyMissingRealOracleStillFails proves the filter is not a
// generic skip: a REAL configured oracle whose fetched image is absent or
// empty still fails the observation exactly as before.
func TestKaminoGenuinelyMissingRealOracleStillFails(t *testing.T) {
	route, accounts := oracleSentinelFixture(t)
	_, err := observeKaminoFromFixedAccounts(context.Background(),
		func(_ context.Context, addresses []string, slot int64) (int64, []ConfirmedAccount, error) {
			for _, address := range addresses {
				if address == kaminoOracleSentinelPubkey {
					t.Fatalf("documented oracle sentinel requested as an account: %s", address)
				}
			}
			return slot, []ConfirmedAccount{{Address: kaminoScopePrices, Lamports: 0, Data: []byte{1}}}, nil
		}, 77, append(append([]ConfirmedAccount{}, accounts...), clockFixture()), route.Kamino)
	if err == nil || !strings.Contains(err.Error(), "invalid configured oracle") {
		t.Fatalf("genuinely unavailable real oracle accepted: %v", err)
	}
}

// TestKaminoOracleZeroSlotsStillCollapse keeps the existing all-zero-slot
// behavior byte-identical: zero bytes map to "" via keyString and never
// reach an oracle fetch.
func TestKaminoOracleZeroSlotsStillCollapse(t *testing.T) {
	route, accounts := nonUSDCDebtNAVFixture(t)
	// Both reserves keep all oracle slots zero; the real oracle still comes
	// only from the explicit collateral slot below.
	putKey(t, accountAt(accounts, route.Kamino.CollateralReserve).Data[5112:5144], kaminoScopePrices)
	requested := []string(nil)
	_, err := observeKaminoFromFixedAccounts(context.Background(),
		func(_ context.Context, addresses []string, slot int64) (int64, []ConfirmedAccount, error) {
			requested = append(requested, addresses...)
			return slot, []ConfirmedAccount{{Address: kaminoScopePrices, Lamports: 1, Data: []byte{1}}}, nil
		}, 77, append(append([]ConfirmedAccount{}, accounts...), clockFixture()), route.Kamino)
	if err != nil {
		t.Fatal(err)
	}
	if len(requested) != 1 || requested[0] != kaminoScopePrices {
		t.Fatalf("zero oracle slots changed the fetch set: %v", requested)
	}
}

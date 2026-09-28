package backyardrwa

import (
	"encoding/binary"
	"errors"
	"math/big"
	"testing"
	"time"
)

// decodedHealthFixtures decodes the fixture bytes through the real decoders so
// the health gates are exercised against the same offsets production reads.
func decodedHealthFixtures(t *testing.T, slot int64) (decodedKaminoObligation, decodedKaminoReserve, decodedKaminoReserve, []ConfirmedAccount) {
	t.Helper()
	config, err := pinnedKaminoObservationConfig()
	if err != nil {
		t.Fatal(err)
	}
	accounts := []ConfirmedAccount{
		obligationFixture(t, slot, 10, 7),
		reserveFixture(t, config.CollateralReserve, config.CollateralMint, slot, new(big.Int).Lsh(big.NewInt(1), 60), 200, 100),
		reserveFixture(t, config.DebtReserve, config.DebtMint, slot, new(big.Int).Lsh(big.NewInt(1), 60), 100, 100),
		marketFixture(t, config.Market),
	}
	obligation, err := decodeKaminoObligation(accountAt(accounts, config.Obligation), config)
	if err != nil {
		t.Fatal(err)
	}
	collateral, err := decodeKaminoReserve(accountAt(accounts, config.CollateralReserve), config.CollateralMint, config)
	if err != nil {
		t.Fatal(err)
	}
	debt, err := decodeKaminoReserve(accountAt(accounts, config.DebtReserve), config.DebtMint, config)
	if err != nil {
		t.Fatal(err)
	}
	return obligation, collateral, debt, accounts
}

// Audit U5 / monitor M5 (contract test): a stale reserve, a paused reserve, a
// stale oracle price, or an emergency market must hold the tick instead of
// reporting the last valuation, and a fresh healthy batch must pass.
func TestKaminoRefreshGateRejectsStaleOrPausedReserve(t *testing.T) {
	const slot = int64(5_000_000)
	config, err := pinnedKaminoObservationConfig()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("a fresh healthy batch passes every gate", func(t *testing.T) {
		obligation, collateral, debt, accounts := decodedHealthFixtures(t, slot)
		emergency, err := decodeKaminoMarketEmergency(accountAt(accounts, config.Market), config)
		if err != nil || emergency {
			t.Fatalf("healthy market decoded as emergency=%t err=%v", emergency, err)
		}
		if err := validateKaminoReserveHealth(slot, emergency, obligation, collateral, debt); err != nil {
			t.Fatalf("healthy reserves were refused: %v", err)
		}
		if err := validateKaminoOracleAge(kaminoFixtureUnix, collateral, debt); err != nil {
			t.Fatalf("fresh oracle prices were refused: %v", err)
		}
	})

	t.Run("boundary freshness passes", func(t *testing.T) {
		obligation, collateral, debt, accounts := decodedHealthFixtures(t, slot)
		// Both views age together, so the relative obligation check stays
		// satisfied exactly at the absolute window edge.
		binary.LittleEndian.PutUint64(accountAt(accounts, config.Obligation).Data[16:24], uint64(slot-32))
		binary.LittleEndian.PutUint64(accountAt(accounts, config.CollateralReserve).Data[16:24], uint64(slot-32))
		binary.LittleEndian.PutUint64(accountAt(accounts, config.CollateralReserve).Data[264:272], uint64(kaminoFixtureUnix-300))
		obligation, err = decodeKaminoObligation(accountAt(accounts, config.Obligation), config)
		collateral, err = decodeKaminoReserve(accountAt(accounts, config.CollateralReserve), config.CollateralMint, config)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateKaminoReserveHealth(slot, false, obligation, collateral, debt); err != nil {
			t.Fatalf("a refresh exactly at the window edge was refused: %v", err)
		}
		if err := validateKaminoOracleAge(kaminoFixtureUnix, collateral, debt); err != nil {
			t.Fatalf("an oracle price exactly at the window edge was refused: %v", err)
		}
	})

	cases := []struct {
		name     string
		mutate   func(t *testing.T, accounts []ConfirmedAccount)
		sentinel error
		reason   string
	}{
		{
			name: "a reserve refreshed past the report window is stale",
			mutate: func(t *testing.T, accounts []ConfirmedAccount) {
				binary.LittleEndian.PutUint64(accountAt(accounts, config.CollateralReserve).Data[16:24], uint64(slot-33))
			},
			sentinel: errKaminoReserveStale, reason: "kamino_stale",
		},
		{
			name: "a reserve claiming a future refresh is stale",
			mutate: func(t *testing.T, accounts []ConfirmedAccount) {
				binary.LittleEndian.PutUint64(accountAt(accounts, config.DebtReserve).Data[16:24], uint64(slot+1))
			},
			sentinel: errKaminoReserveStale, reason: "kamino_stale",
		},
		{
			name: "a paused reserve is refused",
			mutate: func(t *testing.T, accounts []ConfirmedAccount) {
				accountAt(accounts, config.CollateralReserve).Data[kaminoReserveStatusOffset] = 1
			},
			sentinel: errKaminoReservePaused, reason: "reserve_paused",
		},
		{
			name: "a reserve in emergency mode is refused",
			mutate: func(t *testing.T, accounts []ConfirmedAccount) {
				accountAt(accounts, config.DebtReserve).Data[kaminoReserveEmergencyModeOffset] = 1
			},
			sentinel: errKaminoReservePaused, reason: "reserve_paused",
		},
		{
			name: "an oracle price older than the window is stale",
			mutate: func(t *testing.T, accounts []ConfirmedAccount) {
				binary.LittleEndian.PutUint64(accountAt(accounts, config.DebtReserve).Data[264:272], uint64(kaminoFixtureUnix-301))
			},
			sentinel: errKaminoOracleStale, reason: "kamino_stale",
		},
		{
			name: "an oracle price stamped in the future is stale",
			mutate: func(t *testing.T, accounts []ConfirmedAccount) {
				binary.LittleEndian.PutUint64(accountAt(accounts, config.CollateralReserve).Data[264:272], uint64(kaminoFixtureUnix+1))
			},
			sentinel: errKaminoOracleStale, reason: "kamino_stale",
		},
		{
			name: "an unset oracle timestamp is stale",
			mutate: func(t *testing.T, accounts []ConfirmedAccount) {
				binary.LittleEndian.PutUint64(accountAt(accounts, config.CollateralReserve).Data[264:272], 0)
			},
			sentinel: errKaminoOracleStale, reason: "kamino_stale",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			obligation, collateral, debt, accounts := decodedHealthFixtures(t, slot)
			testCase.mutate(t, accounts)
			collateral, err = decodeKaminoReserve(accountAt(accounts, config.CollateralReserve), config.CollateralMint, config)
			if err != nil {
				t.Fatal(err)
			}
			debt, err = decodeKaminoReserve(accountAt(accounts, config.DebtReserve), config.DebtMint, config)
			if err != nil {
				t.Fatal(err)
			}
			healthErr := validateKaminoReserveHealth(slot, false, obligation, collateral, debt)
			oracleErr := validateKaminoOracleAge(kaminoFixtureUnix, collateral, debt)
			err := healthErr
			if testCase.sentinel == errKaminoOracleStale {
				if !errors.Is(oracleErr, errKaminoOracleStale) {
					t.Fatalf("oracle gate err = %v, want %v", oracleErr, errKaminoOracleStale)
				}
				err = oracleErr
			} else {
				if !errors.Is(healthErr, testCase.sentinel) {
					t.Fatalf("health gate err = %v, want %v", healthErr, testCase.sentinel)
				}
				if oracleErr != nil {
					t.Fatalf("oracle gate refused a fresh batch: %v", oracleErr)
				}
			}
			hold, ok := KaminoHealthHoldObservation(err, slot, time.Unix(kaminoFixtureUnix, 0).UTC())
			if !ok || hold.Snapshot.ManualReason != testCase.reason || hold.Snapshot.Slot != slot {
				t.Fatalf("hold observation = %+v ok=%t, want reason %q", hold.Snapshot, ok, testCase.reason)
			}
			decision := Decide(hold.Snapshot)
			if decision.Action != HoldManualRecovery {
				t.Fatalf("decision = %s, want %s", decision.Action, HoldManualRecovery)
			}
			// The worker substitutes the observer's audited reason for Decide's
			// generic hold reason before recording the decision.
			decision.Reason = hold.Snapshot.ManualReason
			if decision.Reason != testCase.reason {
				t.Fatalf("recorded reason = %q, want %q", decision.Reason, testCase.reason)
			}
		})
	}

	t.Run("an emergency market holds the tick", func(t *testing.T) {
		_, collateral, debt, accounts := decodedHealthFixtures(t, slot)
		accountAt(accounts, config.Market).Data[kaminoMarketEmergencyModeOffset] = 1
		emergency, err := decodeKaminoMarketEmergency(accountAt(accounts, config.Market), config)
		if err != nil || !emergency {
			t.Fatalf("emergency flag decoded as %t err=%v", emergency, err)
		}
		err = validateKaminoReserveHealth(slot, emergency, decodedKaminoObligation{}, collateral, debt)
		if !errors.Is(err, errKaminoMarketEmergency) {
			t.Fatalf("health gate err = %v, want %v", err, errKaminoMarketEmergency)
		}
		reason, ok := kaminoHealthReason(err)
		if !ok || reason != "reserve_paused" {
			t.Fatalf("emergency reason = %q ok=%t, want reserve_paused", reason, ok)
		}
	})

	t.Run("an unexpected market layout fails closed", func(t *testing.T) {
		_, _, _, accounts := decodedHealthFixtures(t, slot)
		market := accountAt(accounts, config.Market)
		market.Data = market.Data[:kaminoMarketLength-1]
		if emergency, err := decodeKaminoMarketEmergency(market, config); err == nil || emergency {
			t.Fatalf("a truncated market decoded as emergency=%t err=%v", emergency, err)
		}
	})

	t.Run("other observation errors are not health holds", func(t *testing.T) {
		if _, ok := KaminoHealthHoldObservation(errors.New("Kamino account envelope or layout drifted"), slot, time.Unix(kaminoFixtureUnix, 0).UTC()); ok {
			t.Fatal("an unrelated observation error was mapped to a health hold")
		}
	})
}

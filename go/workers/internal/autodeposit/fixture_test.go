package autodeposit

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

func mustReadFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(fmt.Sprintf("../../testdata/autodeposit/%s", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

func timeMustParse(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parse fixture time %q: %v", value, err)
	}
	return parsed
}

func ptrInt64(v int64) *int64 { return &v }

type lotProjectionFixture struct {
	Description string `json:"description"`
	ObservedAt  string `json:"observedAt"`
	Cases       []struct {
		Name           string `json:"name"`
		FloorRaw       *int64 `json:"floorRaw"`
		AmountAfterRaw int64  `json:"amountAfterRaw"`
		DeltaRaw       *int64 `json:"deltaRaw"`
		Expect         struct {
			Schedules bool  `json:"schedules"`
			AmountRaw int64 `json:"amountRaw"`
		} `json:"expect"`
	} `json:"cases"`
}

func TestLotProjectionFromFixture(t *testing.T) {
	var fixture lotProjectionFixture
	if err := json.Unmarshal(mustReadFixture(t, "lot_projection.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	observedAt := timeMustParse(t, fixture.ObservedAt)
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			if testCase.DeltaRaw == nil {
				amount, ok := InitialSurplusAmount(testCase.AmountAfterRaw, testCase.FloorRaw)
				if ok != testCase.Expect.Schedules {
					t.Fatalf("InitialSurplusAmount scheduled=%v, want %v", ok, testCase.Expect.Schedules)
				}
				if ok && amount != testCase.Expect.AmountRaw {
					t.Fatalf("InitialSurplusAmount scheduled %d raw, want %d raw", amount, testCase.Expect.AmountRaw)
				}
				if ok {
					lot, err := PositiveDeltaToLot(1, 100, nil, amount, observedAt)
					if err != nil {
						t.Fatalf("initial surplus lot: %v", err)
					}
					if want := observedAt.Add(time.Hour); !lot.EligibleAfter.Equal(want) {
						t.Fatalf("initial surplus lot eligible at %s, want the fixed one-hour delay %s", lot.EligibleAfter, want)
					}
					if lot.RemainingAmountRaw != lot.OriginalAmountRaw {
						t.Fatalf("fresh lot remaining %d does not equal original %d", lot.RemainingAmountRaw, lot.OriginalAmountRaw)
					}
				}
				return
			}
			amount, ok := PositiveDeltaSurplusAmount(testCase.AmountAfterRaw, *testCase.DeltaRaw, testCase.FloorRaw)
			if ok != testCase.Expect.Schedules {
				t.Fatalf("PositiveDeltaSurplusAmount scheduled=%v, want %v", ok, testCase.Expect.Schedules)
			}
			if ok && amount != testCase.Expect.AmountRaw {
				t.Fatalf("PositiveDeltaSurplusAmount scheduled %d raw, want %d raw", amount, testCase.Expect.AmountRaw)
			}
		})
	}
}

type outflowSweepFixture struct {
	Description string `json:"description"`
	Lots        []struct {
		ID            int64  `json:"id"`
		CreatedAt     string `json:"createdAt"`
		EligibleAfter string `json:"eligibleAfter"`
		RemainingRaw  int64  `json:"remainingRaw"`
	} `json:"lots"`
	ExternalOutflows []struct {
		Name      string `json:"name"`
		AmountRaw int64  `json:"amountRaw"`
		Expect    struct {
			ConsumedRaw      int64            `json:"consumedRaw"`
			RemainingByLotID map[string]int64 `json:"remainingByLotId"`
			DepletedLotIDs   []int64          `json:"depletedLotIds"`
		} `json:"expect"`
	} `json:"externalOutflows"`
	Sweeps []struct {
		Name                  string `json:"name"`
		Now                   string `json:"now"`
		WalletBalanceRaw      int64  `json:"walletBalanceRaw"`
		WalletBalanceFloorRaw int64  `json:"walletBalanceFloorRaw"`
		RemainingAllowanceRaw *int64 `json:"remainingAllowanceRaw"`
		Expect                struct {
			Kind                string `json:"kind"`
			AmountRaw           int64  `json:"amountRaw"`
			CappedByWalletFloor bool   `json:"cappedByWalletFloor"`
			CappedByAllowance   bool   `json:"cappedByRemainingAllowance"`
			Selected            []struct {
				LotID     int64 `json:"lotId"`
				AmountRaw int64 `json:"amountRaw"`
			} `json:"selected"`
		} `json:"expect"`
	} `json:"sweeps"`
	Consumption struct {
		Name                  string `json:"name"`
		Now                   string `json:"now"`
		WalletBalanceRaw      int64  `json:"walletBalanceRaw"`
		WalletBalanceFloorRaw int64  `json:"walletBalanceFloorRaw"`
		RemainingAllowanceRaw *int64 `json:"remainingAllowanceRaw"`
		Expect                struct {
			ConsumedRaw   int64             `json:"consumedRaw"`
			StatusByLotID map[string]string `json:"statusByLotId"`
		} `json:"expect"`
	} `json:"consumption"`
}

func fixtureLots(t *testing.T, fixture *outflowSweepFixture) []SurplusLot {
	t.Helper()
	lots := make([]SurplusLot, 0, len(fixture.Lots))
	for _, raw := range fixture.Lots {
		lots = append(lots, SurplusLot{
			ID:                 raw.ID,
			OriginalAmountRaw:  raw.RemainingRaw,
			RemainingAmountRaw: raw.RemainingRaw,
			EligibleAfter:      timeMustParse(t, raw.EligibleAfter),
			Status:             LotOpen,
			CreatedAt:          timeMustParse(t, raw.CreatedAt),
		})
	}
	return lots
}

func TestExternalOutflowConsumesNewestLotsFirst(t *testing.T) {
	var fixture outflowSweepFixture
	if err := json.Unmarshal(mustReadFixture(t, "outflows_and_sweeps.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	for _, outflow := range fixture.ExternalOutflows {
		t.Run(outflow.Name, func(t *testing.T) {
			lots := fixtureLots(t, &fixture)
			consumed, err := ApplyExternalOutflowNewestFirst(lots, outflow.AmountRaw)
			if err != nil {
				t.Fatalf("apply external outflow: %v", err)
			}
			if consumed != outflow.Expect.ConsumedRaw {
				t.Fatalf("consumed %d raw, want %d raw", consumed, outflow.Expect.ConsumedRaw)
			}
			for lotID, wantRemaining := range outflow.Expect.RemainingByLotID {
				var lot SurplusLot
				for _, candidate := range lots {
					if fmt.Sprint(candidate.ID) == lotID {
						lot = candidate
						break
					}
				}
				if lot.ID == 0 {
					t.Fatalf("fixture references unknown lot %s", lotID)
				}
				if lot.RemainingAmountRaw != wantRemaining {
					t.Fatalf("lot %d remaining %d raw, want %d raw", lot.ID, lot.RemainingAmountRaw, wantRemaining)
				}
			}
			depleted := map[int64]bool{}
			for _, lot := range lots {
				if lot.Status == LotDepleted {
					depleted[lot.ID] = true
				}
			}
			if len(depleted) != len(outflow.Expect.DepletedLotIDs) {
				t.Fatalf("depleted lots %v, want %v", depleted, outflow.Expect.DepletedLotIDs)
			}
			for _, wantDepleted := range outflow.Expect.DepletedLotIDs {
				if !depleted[wantDepleted] {
					t.Fatalf("lot %d should be depleted by external spending", wantDepleted)
				}
			}
		})
	}
}

func TestSweepSelectionAndConsumption(t *testing.T) {
	var fixture outflowSweepFixture
	if err := json.Unmarshal(mustReadFixture(t, "outflows_and_sweeps.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	for _, sweep := range fixture.Sweeps {
		t.Run(sweep.Name, func(t *testing.T) {
			lots := fixtureLots(t, &fixture)
			decision, selection, err := SelectEligibleLots(lots, timeMustParse(t, sweep.Now), sweep.WalletBalanceRaw, sweep.WalletBalanceFloorRaw, sweep.RemainingAllowanceRaw)
			if err != nil {
				t.Fatalf("select eligible lots: %v", err)
			}
			if string(decision.Kind) != sweep.Expect.Kind {
				t.Fatalf("decision kind %q, want %q", decision.Kind, sweep.Expect.Kind)
			}
			if decision.AmountRaw != sweep.Expect.AmountRaw {
				t.Fatalf("decision amount %d raw, want %d raw", decision.AmountRaw, sweep.Expect.AmountRaw)
			}
			if decision.CappedByWalletFloor != sweep.Expect.CappedByWalletFloor {
				t.Fatalf("cappedByWalletFloor=%v, want %v", decision.CappedByWalletFloor, sweep.Expect.CappedByWalletFloor)
			}
			if decision.CappedByRemainingAllowance != sweep.Expect.CappedByAllowance {
				t.Fatalf("cappedByRemainingAllowance=%v, want %v", decision.CappedByRemainingAllowance, sweep.Expect.CappedByAllowance)
			}
			if sweep.Expect.Kind == "sweep" {
				if selection == nil {
					t.Fatal("sweep decision produced no lot selection")
				}
				if len(selection.Lots) != len(sweep.Expect.Selected) {
					t.Fatalf("selected %d lots (%v), want %d", len(selection.Lots), selection.Lots, len(sweep.Expect.Selected))
				}
				for index, wantSelected := range sweep.Expect.Selected {
					got := selection.Lots[index]
					if got.LotID != wantSelected.LotID || got.AmountRaw != wantSelected.AmountRaw {
						t.Fatalf("selection[%d] = (lot %d, %d raw), want (lot %d, %d raw)", index, got.LotID, got.AmountRaw, wantSelected.LotID, wantSelected.AmountRaw)
					}
				}
			} else if selection != nil {
				t.Fatalf("non-sweep decision %q produced a selection", decision.Kind)
			}
		})
	}

	consumption := fixture.Consumption
	t.Run(consumption.Name, func(t *testing.T) {
		lots := fixtureLots(t, &fixture)
		_, selection, err := SelectEligibleLots(lots, timeMustParse(t, consumption.Now), consumption.WalletBalanceRaw, consumption.WalletBalanceFloorRaw, consumption.RemainingAllowanceRaw)
		if err != nil {
			t.Fatal(err)
		}
		consumed, err := ApplyAutodepositConsumption(lots, selection)
		if err != nil {
			t.Fatalf("apply autodeposit consumption: %v", err)
		}
		if consumed != consumption.Expect.ConsumedRaw {
			t.Fatalf("consumed %d raw, want %d raw", consumed, consumption.Expect.ConsumedRaw)
		}
		for lotID, wantStatus := range consumption.Expect.StatusByLotID {
			for _, lot := range lots {
				if fmt.Sprint(lot.ID) != lotID {
					continue
				}
				if string(lot.Status) != wantStatus {
					t.Fatalf("lot %d status %q after sweep, want %q", lot.ID, lot.Status, wantStatus)
				}
				break
			}
		}
	})
}

func TestSweepRejectsFinanciallyImpossibleInput(t *testing.T) {
	now := timeMustParse(t, "2026-06-16T12:00:00Z")
	corrupt := SurplusLot{ID: 5, OriginalAmountRaw: 1000, RemainingAmountRaw: -1, Status: LotOpen, EligibleAfter: now.Add(-time.Minute), CreatedAt: now}
	healthy := SurplusLot{ID: 6, OriginalAmountRaw: 700, RemainingAmountRaw: 700, Status: LotOpen, EligibleAfter: now.Add(-time.Minute), CreatedAt: now}
	decision, selection, err := SelectEligibleLots([]SurplusLot{corrupt, healthy}, now, 1000, 0, nil)
	if err != nil {
		t.Fatalf("select with a corrupted lot: %v", err)
	}
	// A corrupt remaining amount carries no custody information; it must never be
	// swept and must not inflate the eligible total the sweep is sized from.
	if decision.AmountRaw != 700 {
		t.Fatalf("sweep sized %d raw with a corrupted lot present, want only the healthy 700 raw", decision.AmountRaw)
	}
	if selection == nil || len(selection.Lots) != 1 || selection.Lots[0].LotID != 6 {
		t.Fatalf("selection %v must fund only from the healthy lot", selection)
	}
	if _, err := ApplyExternalOutflowNewestFirst([]SurplusLot{corrupt}, 0); err == nil {
		t.Fatal("zero outflow must be rejected: it carries no financial information")
	}
	if _, err := ApplyExternalOutflowNewestFirst([]SurplusLot{corrupt}, -5); err == nil {
		t.Fatal("negative outflow must be rejected")
	}
	// Depleting into a corrupted lot must refuse to record a balance it cannot
	// trust rather than write a made-up remaining amount.
	if _, err := ApplyExternalOutflowNewestFirst([]SurplusLot{corrupt}, 5); err == nil {
		t.Fatal("outflow into a negative lot remaining must be rejected")
	}
	if _, err := LotFromPositiveDelta(1, PositiveDelta{AmountRaw: 0}); err == nil {
		t.Fatal("non-positive delta must not become a lot")
	}
}

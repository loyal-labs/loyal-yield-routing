package autodeposit

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func mustReadFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(fmt.Sprintf("../../testdata/autodeposit/%s", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
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

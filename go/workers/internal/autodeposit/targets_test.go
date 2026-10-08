package autodeposit

import (
	"encoding/json"
	"reflect"
	"testing"
)

type dispatchOrderFixture struct {
	Description string `json:"description"`
	Cases       []struct {
		Name    string `json:"name"`
		Targets []struct {
			TargetID        int64  `json:"targetId"`
			ScheduledSlotID int64  `json:"scheduledSlotId"`
			ClaimToken      string `json:"claimToken"`
		} `json:"targets"`
		HintedSlotIDs []int64 `json:"hintedSlotIds"`
		Limit         int     `json:"limit"`
		ExpectOrder   []struct {
			TargetID        int64  `json:"targetId"`
			ScheduledSlotID int64  `json:"scheduledSlotId"`
			ClaimToken      string `json:"claimToken"`
		} `json:"expectOrder"`
	} `json:"cases"`
}

func TestDispatchOrderFromFixture(t *testing.T) {
	var fixture dispatchOrderFixture
	if err := json.Unmarshal(mustReadFixture(t, "dispatch_order.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			targets := make([]ExecutableTarget, 0, len(testCase.Targets))
			for _, raw := range testCase.Targets {
				targets = append(targets, ExecutableTarget{
					TargetID:        raw.TargetID,
					ScheduledSlotID: raw.ScheduledSlotID,
					ClaimToken:      raw.ClaimToken,
				})
			}
			ordered := PrioritizeExecutableTargets(targets, testCase.HintedSlotIDs, testCase.Limit)
			expected := make([]ExecutableTarget, 0, len(testCase.ExpectOrder))
			for _, raw := range testCase.ExpectOrder {
				expected = append(expected, ExecutableTarget{
					TargetID:        raw.TargetID,
					ScheduledSlotID: raw.ScheduledSlotID,
					ClaimToken:      raw.ClaimToken,
				})
			}
			if !reflect.DeepEqual(ordered, expected) {
				t.Fatalf("dispatch order %+v, want %+v", ordered, expected)
			}
		})
	}
}

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

func TestSlotHintQueueIsBoundedDeduplicatedAndFIFO(t *testing.T) {
	queue := NewSlotHintQueue(3)
	queue.Push(11)
	queue.Push(12)
	queue.Push(11)
	queue.Push(13)
	if got := queue.Drain(2); !reflect.DeepEqual(got, []int64{11, 12}) {
		t.Fatalf("drained %v, want the first two arrivals", got)
	}
	queue.Push(14)
	queue.Push(15)
	if got := queue.Drain(4); !reflect.DeepEqual(got, []int64{13, 14, 15}) {
		t.Fatalf("drained %v after eviction, want remaining arrivals in order", got)
	}
	if queue.Len() != 0 {
		t.Fatalf("queue still holds %d hints after a full drain", queue.Len())
	}
	if got := queue.Drain(2); len(got) != 0 {
		t.Fatalf("empty queue drained %v", got)
	}
	// Realtime wakeup slot ids are strictly positive; anything else is a payload
	// defect, not a hint.
	queue.Push(0)
	queue.Push(-7)
	if queue.Len() != 0 {
		t.Fatalf("non-positive slot ids entered the hint queue: %v", queue.Drain(2))
	}
}

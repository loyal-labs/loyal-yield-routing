package autodeposit

import (
	"testing"
)

func TestTopUpRecoveryClassification(t *testing.T) {
	holds := AttemptConfirmed
	failed := AttemptFailed
	unknown := AttemptUnknown
	ambiguous := AttemptAmbiguous
	five, three, four := int64(5_000_000), int64(3_000_000), int64(4_000_000)

	cases := []struct {
		name     string
		state    *AttemptState
		vault    int64
		planned  int64
		snapshot *int64
		siblings int64
		want     TopUpRecoveryAction
	}{
		{"live attempt owns the deposit even when the balance looks short", &holds, 0, four, ptrInt64(1_000_000), 0, TopUpReconcilePersisted},
		{"ambiguous attempt is reconciled, never re-prepared", &ambiguous, 0, four, nil, 0, TopUpReconcilePersisted},
		{"unknown attempt is reconciled, not replaced", &unknown, 0, four, nil, 0, TopUpReconcilePersisted},
		{"vault below the planned amount cannot be explained", &failed, three, four, nil, 0, TopUpEffectAmbiguous},
		{"vault short of the snapshot with no sibling deposit is ambiguous", &failed, three, four, ptrInt64(five), 0, TopUpEffectAmbiguous},
		{"a confirmed sibling deposit explains the missing snapshot balance", &failed, three, three, ptrInt64(five), 2_000_000, TopUpPrepareOrRequeue},
		{"vault holding the plan with no prior attempt prepares", nil, four, four, nil, 0, TopUpPrepareOrRequeue},
		{"vault holding more than the plan prepares", nil, five, four, nil, 0, TopUpPrepareOrRequeue},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := ClassifyDirectTopUpRecovery(testCase.state, testCase.vault, testCase.planned, testCase.snapshot, testCase.siblings)
			if got != testCase.want {
				t.Fatalf("classified %q, want %q", got, testCase.want)
			}
		})
	}
}

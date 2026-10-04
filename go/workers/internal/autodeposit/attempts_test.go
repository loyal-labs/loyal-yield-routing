package autodeposit

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// scriptedAttempts is a consumer-style implementation of the settlement
// dependencies: it records the order of effects so the test can assert the
// protocol, not the implementation's internal helpers.
type scriptedAttempts struct {
	attempt        DurableAttempt
	observations   []AttemptObservation
	broadcastErr   error
	broadcastReply string
	calls          []string
	lastObserved   DurableAttempt
}

func (s *scriptedAttempts) Observe(ctx context.Context, attempt DurableAttempt) (AttemptObservation, error) {
	s.calls = append(s.calls, "observe")
	s.lastObserved = attempt
	if len(s.observations) == 0 {
		return AttemptObservation{State: AttemptUnknown}, nil
	}
	observation := s.observations[0]
	s.observations = s.observations[1:]
	return observation, nil
}

func (s *scriptedAttempts) BroadcastExact(ctx context.Context, attempt DurableAttempt) (string, error) {
	s.calls = append(s.calls, "broadcast")
	if s.broadcastErr != nil {
		return "", s.broadcastErr
	}
	if s.broadcastReply != "" {
		return s.broadcastReply, nil
	}
	return attempt.Signature, nil
}

func (s *scriptedAttempts) RecordBroadcast(ctx context.Context, attempt DurableAttempt) (DurableAttempt, error) {
	s.calls = append(s.calls, "record_broadcast")
	attempt.State = AttemptSubmitted
	attempt.BroadcastCount++
	s.attempt = attempt
	return attempt, nil
}

func (s *scriptedAttempts) RecordObservation(ctx context.Context, attempt DurableAttempt, observation AttemptObservation) (DurableAttempt, error) {
	s.calls = append(s.calls, "record_observation")
	attempt.State = observation.State
	if observation.ConfirmedSlot != nil {
		slot := *observation.ConfirmedSlot
		attempt.ConfirmedSlot = &slot
	}
	s.attempt = attempt
	return attempt, nil
}

func persistedPullAttempt() DurableAttempt {
	slot := int64(870640)
	return DurableAttempt{
		ID:                       11,
		ClaimToken:               "claim-1",
		OperationKind:            OperationPull,
		AttemptNumber:            1,
		AmountRaw:                4_000_000,
		SourcePreBalanceRaw:      9_000_000,
		DestinationPreBalanceRaw: 1_000_000,
		Signature:                "sig-pull-1",
		SignedTransactionBase64:  "base64-wire",
		SignedTransactionSHA256:  strings.Repeat("a", 64),
		RecentBlockhash:          "blockhash-1",
		LastValidBlockHeight:     500,
		State:                    AttemptPrepared,
		ConfirmedSlot:            &slot,
	}
}

func TestSettlementObservesBeforeAnyBroadcast(t *testing.T) {
	ctx := context.Background()
	attempt := persistedPullAttempt()
	slot := *attempt.ConfirmedSlot
	scripted := &scriptedAttempts{attempt: attempt, observations: []AttemptObservation{
		{State: AttemptConfirmed, ConfirmedSlot: &slot},
	}}

	settlement, err := SettleDurableAttempt(ctx, attempt, scripted)
	if err != nil {
		t.Fatalf("settle confirmed attempt: %v", err)
	}
	if settlement.Broadcasted {
		t.Fatal("a terminal observation must never re-broadcast the signed wire")
	}
	if settlement.Attempt.State != AttemptConfirmed || settlement.Attempt.ConfirmedSlot == nil || *settlement.Attempt.ConfirmedSlot != slot {
		t.Fatalf("settled attempt %+v lost the confirmed slot", settlement.Attempt)
	}
	if len(scripted.calls) != 2 || scripted.calls[0] != "observe" || scripted.calls[1] != "record_observation" {
		t.Fatalf("settlement call order %v must be observe then record_observation", scripted.calls)
	}
}

func TestSettlementRebroadcastsExactBytesOnlyWhenUnresolved(t *testing.T) {
	ctx := context.Background()
	attempt := persistedPullAttempt()
	confirmedSlot := int64(870_777)
	scripted := &scriptedAttempts{attempt: attempt, observations: []AttemptObservation{
		{State: AttemptUnknown},
		{State: AttemptConfirmed, ConfirmedSlot: &confirmedSlot},
	}}

	settlement, err := SettleDurableAttempt(ctx, attempt, scripted)
	if err != nil {
		t.Fatalf("settle unresolved attempt: %v", err)
	}
	if !settlement.Broadcasted {
		t.Fatal("unresolved prepared attempt must be broadcast before it can confirm")
	}
	if settlement.Attempt.State != AttemptConfirmed || settlement.Attempt.BroadcastCount != 1 {
		t.Fatalf("settled attempt state=%s broadcasts=%d, want confirmed after one exact rebroadcast", settlement.Attempt.State, settlement.Attempt.BroadcastCount)
	}
	want := []string{"observe", "record_broadcast", "broadcast", "observe", "record_observation"}
	if strings.Join(scripted.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("settlement call order %v, want %v: the durable broadcast intent must precede the send", scripted.calls, want)
	}
}

func TestSettlementRecordsIntentBeforeSendEvenWhenSendNeverHappens(t *testing.T) {
	ctx := context.Background()
	attempt := persistedPullAttempt()
	unknown := AttemptObservation{State: AttemptUnknown}
	scripted := &scriptedAttempts{attempt: attempt, broadcastReply: "a-different-signature", observations: []AttemptObservation{unknown, unknown}}

	settlement, err := SettleDurableAttempt(ctx, attempt, scripted)
	if err != nil {
		t.Fatalf("a contradicting signature is recorded evidence, not a panic: %v", err)
	}
	// The cluster was asked to accept the exact bytes before the contradiction
	// was visible, so the intent is durable evidence; it is never an observed
	// submit and must not read as broadcast progress.
	if settlement.Broadcasted {
		t.Fatal("contradicting signature must not be recorded as an observed broadcast")
	}
	if settlement.Attempt.BroadcastCount != 1 {
		t.Fatalf("durable broadcast intent broadcast_count %d, want 1 recorded before the send", settlement.Attempt.BroadcastCount)
	}
	if settlement.Attempt.State != AttemptUnknown {
		t.Fatalf("contradicting signature left state %s, want unknown pending reconciliation", settlement.Attempt.State)
	}
	if settlement.Observation.Err == nil {
		t.Fatal("the signature contradiction must be preserved on the recorded observation")
	}
	if !AttemptHoldsClaim(settlement.Attempt.State) {
		t.Fatal("an unproven attempt must keep holding the claim")
	}
}

func TestSettlementBroadcastFailureRethinksInsteadOfFailing(t *testing.T) {
	ctx := context.Background()
	// The RPC error may mean the submission landed anyway; the settlement must
	// observe again and record the truth rather than a transport verdict.
	attempt := persistedPullAttempt()
	confirmedSlot := int64(870_900)
	landed := &scriptedAttempts{attempt: attempt, broadcastErr: errors.New("context deadline exceeded"), observations: []AttemptObservation{
		{State: AttemptUnknown},
		{State: AttemptConfirmed, ConfirmedSlot: &confirmedSlot},
	}}
	settlement, err := SettleDurableAttempt(ctx, attempt, landed)
	if err != nil {
		t.Fatalf("settle after broadcast error: %v", err)
	}
	if settlement.Attempt.State != AttemptConfirmed {
		t.Fatalf("a landed submission was recorded as %s; transport failure is not chain truth", settlement.Attempt.State)
	}
	if settlement.Broadcasted {
		t.Fatal("a rejected broadcast must not be counted as broadcast work")
	}

	expired := &scriptedAttempts{attempt: attempt, broadcastErr: errors.New("node unreachable"), observations: []AttemptObservation{
		{State: AttemptUnknown},
		{State: AttemptExpired},
	}}
	settlement, err = SettleDurableAttempt(ctx, attempt, expired)
	if err != nil {
		t.Fatalf("settle expired attempt: %v", err)
	}
	if settlement.Attempt.State != AttemptExpired || !AttemptAllowsSafeRequeue(settlement.Attempt.State) {
		t.Fatalf("expired attempt state %s must conclusively release the claim for a fresh attempt", settlement.Attempt.State)
	}
}

func TestSettlementRefusesToInventWire(t *testing.T) {
	ctx := context.Background()
	bare := persistedPullAttempt()
	bare.Signature = ""
	scripted := &scriptedAttempts{attempt: bare}
	if _, err := SettleDurableAttempt(ctx, bare, scripted); err == nil {
		t.Fatal("settlement without persisted wire identity would fabricate a spend")
	}
	if len(scripted.calls) != 0 {
		t.Fatalf("settlement touched dependencies %v before checking its own wire identity", scripted.calls)
	}
}

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

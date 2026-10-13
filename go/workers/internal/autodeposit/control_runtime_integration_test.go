package autodeposit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

type runtimeControlReader func(context.Context, ControlTarget, int64) (ControlObservation, error)

func (f runtimeControlReader) ObserveControl(ctx context.Context, target ControlTarget, slot int64) (ControlObservation, error) {
	return f(ctx, target, slot)
}

// These are explicit synthetic persisted projection fields, exercising the
// registered schema census. They are not a canonical creator receipt proof or
// authorization to send a transaction; artifact verification has separate tests.
func seedRuntimeHealthyTarget(t *testing.T, store *Store, suffix string) ControlTarget {
	t.Helper()
	target := seedControlTarget(t, store, suffix)
	_, err := store.pool.Exec(context.Background(), `UPDATE loyal_yield.balance_sweep_targets SET
 chain_observation_slot=100,bootstrap_generation=setup_generation,
 period_length_seconds=86400,start_timestamp=0,recurring_delegation_expiry_timestamp=0,
 policy_signature=$2,policy_confirmed_slot=90,
 recurring_delegation_signature=$3,recurring_delegation_confirmed_slot=91 WHERE id=$1`,
		target.TargetID, "runtime-policy-"+suffix, "runtime-delegation-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(context.Background(), `UPDATE loyal_yield.autodeposit_reconciliation_requests SET processed_slot=requested_slot,claim_owner=NULL,claim_expires_at=NULL,last_error=NULL WHERE target_id=$1`, target.TargetID); err != nil {
		t.Fatal(err)
	}
	return target
}

func TestSignedCustodyCannotBeAdoptedByRepeatedTicks(t *testing.T) {
	for _, state := range []AttemptState{AttemptPrepared, AttemptSubmitted} {
		t.Run(string(state), func(t *testing.T) {
			s := integrationStore(t)
			seeded, claim, _, _ := seedRecoveryClaim(t, s, "runtime-unknown")
			ctx := context.Background()
			if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET desired_active=false WHERE id=$1`, seeded.TargetID); err != nil {
				t.Fatal(err)
			}
			before, err := s.LoadLatestAttempt(ctx, claim, OperationPull)
			if err != nil || before == nil {
				t.Fatalf("attempt=%v error=%v", before, err)
			}
			if state != AttemptPrepared {
				if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_transaction_attempts SET attempt_state=$2,broadcast_count=1 WHERE claim_token=$1`, claim, string(state)); err != nil {
					t.Fatal(err)
				}
				if before, err = s.LoadLatestAttempt(ctx, claim, OperationPull); err != nil || before == nil {
					t.Fatalf("attempt=%v error=%v", before, err)
				}
			}
			// Each fresh worker models a restart; none may adopt the signed intent.
			for i := 0; i < 2; i++ {
				w, err := NewWorker(WorkerDependencies{Store: s, Executor: &scriptedExecutor{results: []ExecutorResult{ResultRecoveryPending}}, Facts: testFacts(), FeePayer: fundedPayer{}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := w.Tick(ctx); err != nil {
					t.Fatal(err)
				}
			}
			after, err := s.LoadLatestAttempt(ctx, claim, OperationPull)
			if err != nil || *after != *before {
				t.Fatalf("restart changed immutable custody: error=%v", err)
			}
		})
	}
}

func TestControlPersistsRetryAndRecovers(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	target := seedRuntimeHealthyTarget(t, s, "runtime-retry")
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET desired_active=false WHERE id=$1`, target.TargetID); err != nil {
		t.Fatal(err)
	}
	enqueueReconciliationRequest(t, s, target.TargetID, 200)
	readerCalls := 0
	r := &ControlReconciler{Store: s}
	r.Reader = runtimeControlReader(func(_ context.Context, target ControlTarget, min int64) (ControlObservation, error) {
		readerCalls++
		if readerCalls == 1 {
			return ControlObservation{}, errors.New("private provider URL must not reach runtime logs")
		}
		return ControlObservation{Target: target, ObservedSlot: min + 1,
			PolicyExists: true, DelegationExists: true, PolicyValid: true, AuthorityValid: true, DelegationValid: true, TokenDelegateValid: true,
			WalletBalanceRaw: 4_000_000, WalletAccountDataSHA256: strings.Repeat("a", 64)}, nil
	})
	if worked, err := r.Tick(ctx); !worked || err == nil {
		t.Fatalf("failed control read acknowledged: worked=%v err=%v", worked, err)
	}
	var requested, processed int64
	var owner, lastError *string
	if err := s.pool.QueryRow(ctx, `SELECT requested_slot,processed_slot,claim_owner,last_error FROM loyal_yield.autodeposit_reconciliation_requests WHERE target_id=$1`, target.TargetID).Scan(&requested, &processed, &owner, &lastError); err != nil {
		t.Fatal(err)
	}
	if requested <= processed || owner != nil || lastError == nil || *lastError != "control reconciliation proof unavailable" || readerCalls != 1 {
		t.Fatalf("failed control read acknowledged or skipped retry: requested=%d processed=%d owner=%v lastError=%v reader=%d", requested, processed, owner, lastError, readerCalls)
	}
	// Advance the durable retry clock explicitly; no sleep or omitted backoff.
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.autodeposit_reconciliation_requests SET next_attempt_at=now() WHERE target_id=$1`, target.TargetID); err != nil {
		t.Fatal(err)
	}
	if worked, err := r.Tick(ctx); !worked || err != nil {
		t.Fatalf("retry worked=%v err=%v", worked, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT requested_slot,processed_slot FROM loyal_yield.autodeposit_reconciliation_requests WHERE target_id=$1`, target.TargetID).Scan(&requested, &processed); err != nil || processed < requested {
		t.Fatalf("retry not acknowledged: requested=%d processed=%d err=%v", requested, processed, err)
	}
	if readerCalls != 2 {
		t.Fatalf("reader=%d", readerCalls)
	}
}

func TestRuntimeControlAppliesKnownClosed(t *testing.T) {
	s := integrationStore(t)
	target := seedRuntimeHealthyTarget(t, s, "runtime-closed")
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET desired_active=false WHERE id=$1`, target.TargetID); err != nil {
		t.Fatal(err)
	}
	enqueueReconciliationRequest(t, s, target.TargetID, 200)
	r := &ControlReconciler{Store: s,
		Reader: runtimeControlReader(func(_ context.Context, target ControlTarget, min int64) (ControlObservation, error) {
			return ControlObservation{Target: target, ObservedSlot: min + 1}, nil
		}),
	}
	if worked, err := r.Tick(ctx); err != nil || !worked {
		t.Fatalf("closed reconciliation worked=%v error=%v", worked, err)
	}
	var status string
	var pending bool
	if err := s.pool.QueryRow(ctx, `SELECT t.chain_status,r.requested_slot>r.processed_slot FROM loyal_yield.balance_sweep_targets t JOIN loyal_yield.autodeposit_reconciliation_requests r ON r.target_id=t.id WHERE t.id=$1`, target.TargetID).Scan(&status, &pending); err != nil || status != "closed" || pending {
		t.Fatalf("status=%s pending=%v error=%v", status, pending, err)
	}
}

// A node behind the requested slot is not yet: the tick succeeds and the
// request waits the base delay, its attempt uncounted and no error recorded.
func TestControlBehindNodeReschedulesWithoutAnAttempt(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	target := seedRuntimeHealthyTarget(t, s, "runtime-behind")
	enqueueReconciliationRequest(t, s, target.TargetID, 200)
	reads := 0
	r := &ControlReconciler{Store: s, Reader: runtimeControlReader(func(context.Context, ControlTarget, int64) (ControlObservation, error) {
		reads++
		return ControlObservation{}, fmt.Errorf("getMultipleAccounts: %w", chain.ErrBehind)
	})}
	read := func() (attempts int, owner, lastError *string, wait float64) {
		t.Helper()
		if err := s.pool.QueryRow(ctx, `SELECT attempt_count,claim_owner,last_error,EXTRACT(EPOCH FROM next_attempt_at-now())::float8 FROM loyal_yield.autodeposit_reconciliation_requests WHERE target_id=$1`, target.TargetID).Scan(&attempts, &owner, &lastError, &wait); err != nil {
			t.Fatal(err)
		}
		return
	}
	before, _, _, _ := read()
	if worked, err := r.Tick(ctx); !worked || err != nil || reads != 1 {
		t.Fatalf("behind node failed the tick: worked=%v reads=%d err=%v", worked, reads, err)
	}
	if attempts, owner, lastError, wait := read(); attempts != before || owner != nil || lastError != nil || wait < 14 || wait > 15 {
		t.Fatalf("behind node was counted or recorded: attempts=%d/%d owner=%v lastError=%v wait=%.1fs", attempts, before, owner, lastError, wait)
	}
}

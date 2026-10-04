package autodeposit

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func runtimeRPCFixture(t *testing.T, slot int64) *RPCChain {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Method != "getSlot" {
			t.Errorf("readiness performed unexpected RPC: method=%q error=%v", request.Method, err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": slot})
	}))
	t.Cleanup(server.Close)
	chain, err := NewRPCChain(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	return chain
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

func TestRuntimeHealthRegisteredBacklogAndPause(t *testing.T) {
	s := integrationStore(t)
	ctx := context.Background()
	chain := runtimeRPCFixture(t, 777)
	target := seedRuntimeHealthyTarget(t, s, "runtime-census")
	assertHealth := func(want bool) {
		t.Helper()
		slot, err := runtimeRecoveryHealth(ctx, s, chain, true)
		if want && (err != nil || slot != 777) {
			t.Fatalf("healthy census slot=%d error=%v", slot, err)
		}
		if !want && (err == nil || slot != 0) {
			t.Fatalf("held census slot=%d error=%v", slot, err)
		}
	}
	assertHealth(true)
	// An already completed control ask does not hide a newer high-water ask.
	if _, err := s.EnqueueAutodepositReconciliationRequest(ctx, target.TargetID, 200); err != nil {
		t.Fatal(err)
	}
	assertHealth(false)
	r, err := s.ClaimAutodepositReconciliationRequest(ctx, "runtime-proof", 120)
	if err != nil || r == nil {
		t.Fatalf("claim=%v error=%v", r, err)
	}
	loaded, err := s.LoadControlTarget(ctx, target.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	o := ControlObservation{Target: *loaded, ObservedSlot: 201, PolicyExists: true, DelegationExists: true, PolicyValid: true, AuthorityValid: true, DelegationValid: true, TokenDelegateValid: true, WalletBalanceRaw: 4_000_000, WalletAccountDataSHA256: strings.Repeat("a", 64)}
	if err := s.ApplyControlObservation(ctx, *r, "runtime-proof", o); err != nil {
		t.Fatal(err)
	}
	assertHealth(true)
	// Desired pause is not an operational failure. Paused wallet events still
	// must be projected, but do not cause a new pull or active bootstrap demand.
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET desired_active=false WHERE id=$1`, target.TargetID); err != nil {
		t.Fatal(err)
	}
	assertHealth(true)
	s.insertIntegrationEvent(t, target.TargetID, 99_000_001, 5_000_000, nil, time.Now())
	assertHealth(false)
	if _, err := s.ProjectSurplusLotsOnce(ctx, 100); err != nil {
		t.Fatal(err)
	}
	assertHealth(true)
	// The actual endpoint must cover the already published observation slot.
	if _, err := runtimeRecoveryHealth(ctx, s, runtimeRPCFixture(t, 199), true); err == nil {
		t.Fatal("lagging RPC frontier accepted")
	}
}

func TestRuntimeHealthCreatorAndGenerationDemand(t *testing.T) {
	for _, mutation := range []struct{ name, sql string }{
		{"creator_pair", `policy_confirmed_slot=NULL`},
		{"bootstrap_generation", `bootstrap_generation=NULL`},
		{"delegation_nonce", `recurring_delegation_nonce=NULL`},
		{"budget", `max_amount_per_period=0`},
		{"pending_accounts", `chain_status='pending'`},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			s := integrationStore(t)
			target := seedRuntimeHealthyTarget(t, s, "runtime-"+mutation.name)
			chain := runtimeRPCFixture(t, 777)
			if _, err := runtimeRecoveryHealth(context.Background(), s, chain, true); err != nil {
				t.Fatal(err)
			}
			if _, err := s.pool.Exec(context.Background(), `UPDATE loyal_yield.balance_sweep_targets SET `+mutation.sql+` WHERE id=$1`, target.TargetID); err != nil {
				t.Fatal(err)
			}
			if _, err := runtimeRecoveryHealth(context.Background(), s, chain, true); err == nil {
				t.Fatal("incomplete active demand accepted")
			}
		})
	}
}

func TestRuntimeSignedCustodyCannotBeAdoptedByRestart(t *testing.T) {
	for _, state := range []AttemptState{AttemptPrepared, AttemptUnknown, AttemptAmbiguous} {
		t.Run(string(state), func(t *testing.T) {
			s := integrationStore(t)
			seeded, claim, _, _ := seedRecoveryClaim(t, s, "runtime-unknown")
			ctx := context.Background()
			if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET desired_active=false WHERE id=$1`, seeded.TargetID); err != nil {
				t.Fatal(err)
			}
			chain := runtimeRPCFixture(t, 777)
			before, err := s.LoadLatestAttempt(ctx, claim, OperationPull)
			if err != nil || before == nil {
				t.Fatalf("attempt=%v error=%v", before, err)
			}
			if state != AttemptPrepared {
				const owner = "runtime-census-owner"
				owned, err := s.AcquireClaimLease(ctx, claim, seeded.TargetID, owner)
				if err != nil || !owned {
					t.Fatalf("lease=%v error=%v", owned, err)
				}
				recorded, err := s.RecordAttemptObservation(ctx, *before, AttemptObservation{State: state}, owner)
				if err != nil {
					t.Fatal(err)
				}
				before = &recorded
				if err := s.ReleaseClaimLease(ctx, claim, owner); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				w, err := NewWorker(WorkerDependencies{Store: s, Executor: &scriptedExecutor{exits: []*int{exitCodePtr(ExitRecoveryPending)}}, RuntimeChain: chain, PollInterval: time.Millisecond})
				if err != nil {
					t.Fatal(err)
				}
				run, cancel := context.WithTimeout(ctx, 2*time.Second)
				cycles := 0
				w.SetRuntimeReporter(func(ready bool, slot uint64) {
					if ready {
						t.Error("restart adopted a signed intent without fenced proof")
					}
					cycles++
					if cycles == 2 {
						cancel()
					}
				})
				if err := w.Run(run); !errors.Is(err, context.Canceled) {
					t.Fatalf("exit=%v", err)
				}
				cancel()
			}
			after, err := s.LoadLatestAttempt(ctx, claim, OperationPull)
			if err != nil || *after != *before {
				t.Fatalf("readiness changed immutable custody: error=%v", err)
			}
		})
	}
}

func TestRuntimeWorkerReportsCompletedTickAndFreshChain(t *testing.T) {
	s := integrationStore(t)
	chain := runtimeRPCFixture(t, 889)
	w, err := NewWorker(WorkerDependencies{Store: s, Executor: &scriptedExecutor{}, RuntimeChain: chain})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	type report struct {
		ready bool
		slot  uint64
	}
	var reports []report
	w.SetRuntimeReporter(func(ready bool, slot uint64) {
		reports = append(reports, report{ready, slot})
		if ready {
			cancel()
		}
	})
	if err := w.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("exit=%v", err)
	}
	if len(reports) != 3 || reports[0].ready || !reports[1].ready || reports[1].slot != 889 || reports[2].ready {
		t.Fatalf("runtime callbacks=%v", reports)
	}
	var offset int64
	if err := s.pool.QueryRow(context.Background(), `SELECT last_event_id FROM loyal_yield.projection_offsets WHERE consumer_name=$1`, ConsumerName).Scan(&offset); err != nil {
		t.Fatalf("ready without completed projection tick: %v", err)
	}
}

type runtimeArtifacts func(context.Context, ReconciliationRequest, string) error

func (f runtimeArtifacts) ReconcileArtifacts(ctx context.Context, r ReconciliationRequest, owner string) error {
	return f(ctx, r, owner)
}

func TestRuntimeControlPersistsRetryAndRecoversWithoutExit(t *testing.T) {
	s := integrationStore(t)
	target := seedRuntimeHealthyTarget(t, s, "runtime-retry")
	if _, err := s.pool.Exec(context.Background(), `UPDATE loyal_yield.balance_sweep_targets SET desired_active=false WHERE id=$1`, target.TargetID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueAutodepositReconciliationRequest(context.Background(), target.TargetID, 200); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	artifactCalls, readerCalls, errorCalls, readyCalls := 0, 0, 0, 0
	r := &ControlReconciler{Store: s, RuntimeChain: runtimeRPCFixture(t, 777), PollInterval: time.Millisecond}
	r.Artifacts = runtimeArtifacts(func(context.Context, ReconciliationRequest, string) error {
		artifactCalls++
		if artifactCalls == 1 {
			return errors.New("private provider URL must not reach runtime logs")
		}
		return nil
	})
	r.Reader = runtimeControlReader(func(_ context.Context, target ControlTarget, min int64) (ControlObservation, error) {
		readerCalls++
		return ControlObservation{Target: target, ObservedSlot: min + 1,
			PolicyExists: true, DelegationExists: true, PolicyValid: true, AuthorityValid: true, DelegationValid: true, TokenDelegateValid: true,
			WalletBalanceRaw: 4_000_000, WalletAccountDataSHA256: strings.Repeat("a", 64)}, nil
	})
	r.OnError = func(err error) {
		errorCalls++
		if err.Error() != errRuntimeProofUnavailable.Error() {
			t.Errorf("unsanitized runtime callback: %v", err)
		}
		var requested, processed int64
		var owner *string
		var lastError *string
		if err := s.pool.QueryRow(context.Background(), `SELECT requested_slot,processed_slot,claim_owner,last_error FROM loyal_yield.autodeposit_reconciliation_requests WHERE target_id=$1`, target.TargetID).Scan(&requested, &processed, &owner, &lastError); err != nil {
			t.Error(err)
		}
		if requested <= processed || owner != nil || lastError == nil || *lastError != "control reconciliation proof unavailable" || readerCalls != 1 {
			t.Errorf("failed creator proof acknowledged or skipped retry: requested=%d processed=%d owner=%v lastError=%v reader=%d", requested, processed, owner, lastError, readerCalls)
		}
		// Advance the durable retry clock explicitly; no sleep or omitted backoff.
		if _, err := s.pool.Exec(context.Background(), `UPDATE loyal_yield.autodeposit_reconciliation_requests SET next_attempt_at=now() WHERE target_id=$1`, target.TargetID); err != nil {
			t.Error(err)
		}
	}
	r.SetRuntimeReporter(func(ready bool, slot uint64) {
		if ready {
			readyCalls++
			if slot != 777 {
				t.Errorf("slot=%d", slot)
			}
			cancel()
		}
	})
	if err := r.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("retryable proof outage killed runtime: %v", err)
	}
	if artifactCalls != 2 || readerCalls != 2 || errorCalls != 1 || readyCalls != 1 {
		t.Fatalf("artifact=%d reader=%d errors=%d ready=%d", artifactCalls, readerCalls, errorCalls, readyCalls)
	}
}

func TestRuntimeControlKnownClosedDoesNotDemandCreatorHistory(t *testing.T) {
	s := integrationStore(t)
	target := seedRuntimeHealthyTarget(t, s, "runtime-closed")
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET desired_active=false WHERE id=$1`, target.TargetID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueAutodepositReconciliationRequest(ctx, target.TargetID, 200); err != nil {
		t.Fatal(err)
	}
	r := &ControlReconciler{Store: s,
		Reader: runtimeControlReader(func(_ context.Context, target ControlTarget, min int64) (ControlObservation, error) {
			return ControlObservation{Target: target, ObservedSlot: min + 1}, nil
		}),
		Artifacts: runtimeArtifacts(func(context.Context, ReconciliationRequest, string) error {
			t.Error("known closed state required creator history")
			return ErrArtifactCreationProofPending
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

func TestRuntimeLiveUnsignedLeaseIsHealthyButExpiredWorkHolds(t *testing.T) {
	s := integrationStore(t)
	target := seedRuntimeHealthyTarget(t, s, "runtime-unsigned")
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `INSERT INTO loyal_yield.balance_sweep_lot_claims(claim_token,target_id,amount_raw,status,stale_check_event_id,autodeposit_executor_lease_token,autodeposit_executor_lease_expires_at) VALUES('runtime-unsigned-claim',$1,2000000,'selected',0,'runtime-live-fence',now()+interval '2 minutes')`, target.TargetID); err != nil {
		t.Fatal(err)
	}
	chain := runtimeRPCFixture(t, 777)
	if _, err := runtimeRecoveryHealth(ctx, s, chain, true); err != nil {
		t.Fatalf("normal owned unsigned work held readiness: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE loyal_yield.balance_sweep_lot_claims SET autodeposit_executor_lease_expires_at=now()-interval '1 second' WHERE claim_token='runtime-unsigned-claim'`); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeRecoveryHealth(ctx, s, chain, true); err == nil {
		t.Fatal("unproven expired claim disappeared from health census")
	}
}

func TestRuntimeExecutorFailuresHoldDespiteHealthyDurableCensus(t *testing.T) {
	for _, test := range []struct {
		name string
		code *int
		err  error
	}{
		{"alert", nil, errors.New("private RPC provider failure")},
		{"nonfatal_exit_error", exitCodePtr(ExitRecoveryPending), errors.New("proof not yet available")},
		{"unclassified_process_success", exitCodePtr(0), nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := integrationStore(t)
			target := seedRuntimeHealthyTarget(t, s, "runtime-failure-"+test.name)
			ctx := context.Background()
			s.insertIntegrationEvent(t, target.TargetID, 99_100_001, 9_000_000, nil, time.Now().Add(-2*time.Hour))
			if _, err := s.pool.Exec(ctx, `INSERT INTO loyal_yield.balance_sweep_wallet_balances_current(target_id,wallet,wallet_usdc_ata,wallet_token_ata,amount_raw,mint,observed_slot,source,source_commitment) VALUES($1,'itest-wallet','itest-wallet-usdc','itest-wallet-usdc',9000000,$2,100,'itest','confirmed')`, target.TargetID, USDCMint); err != nil {
				t.Fatal(err)
			}
			executor := &scriptedExecutor{exits: []*int{test.code}, errs: []error{test.err}}
			chain := runtimeRPCFixture(t, 777)
			w, err := NewWorker(WorkerDependencies{Store: s, Executor: executor, RuntimeChain: chain})
			if err != nil {
				t.Fatal(err)
			}
			run, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			cycles := 0
			w.SetRuntimeReporter(func(ready bool, slot uint64) {
				cycles++
				if ready {
					t.Error("failed executor made runtime ready")
				}
				if cycles == 2 {
					cancel()
				}
			})
			if err := w.Run(run); !errors.Is(err, context.Canceled) {
				t.Fatalf("exit=%v", err)
			}
			if executor.calls != 1 {
				t.Fatalf("eligible target was not dispatched: calls=%d", executor.calls)
			}
			// This hold comes from the tick outcome, independently of backlog.
			if _, err := runtimeRecoveryHealth(ctx, s, chain, true); err != nil {
				t.Fatalf("fixture did not isolate tick failure from DB census: %v", err)
			}
		})
	}
}

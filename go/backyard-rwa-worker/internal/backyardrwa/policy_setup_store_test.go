package backyardrwa

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func observedSetupFixture(t *testing.T, operation string) policySetupObservation {
	t.Helper()
	plan, err := observePolicySetup(context.Background(), setupObservationRPC(t, &setupRPCScenario{rent: 10_634_880}), operation)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestPolicySetupIntentRejectsUnpricedOrChangedPlan(t *testing.T) {
	for name, mutate := range map[string]func(*policySetupObservation){
		"total":              func(p *policySetupObservation) { p.TotalMicros-- },
		"remaining":          func(p *policySetupObservation) { p.CompletionReserveMicros-- },
		"rent":               func(p *policySetupObservation) { p.RentLamports-- },
		"seed":               func(p *policySetupObservation) { p.Request.Seed++ },
		"policy":             func(p *policySetupObservation) { p.Policy = bridgeVault },
		"authority evidence": func(p *policySetupObservation) { p.SettingsSHA256 = "" },
		"expiry":             func(p *policySetupObservation) { p.ValidThroughSlot++ },
		"allocated bytes":    func(p *policySetupObservation) { p.AllocatedBytes-- },
		"message hash":       func(p *policySetupObservation) { p.Payments[0].MessageSHA256 = strings.Repeat("a", 64) },
		"omitted fee":        func(p *policySetupObservation) { p.Payments[0].Fee.Lamports = 0 },
		"claimed admission":  func(p *policySetupObservation) { p.ProductionSetupAdmission = true },
	} {
		t.Run(name, func(t *testing.T) {
			plan := observedSetupFixture(t, "borrow")
			mutate(&plan)
			_, err := validatePolicySetupPlan(plan)
			assertBudgetHold(t, err, "invalid_policy_setup_plan")
		})
	}
	plan := observedSetupFixture(t, "borrow")
	if _, err := validatePolicySetupPlan(plan); err != nil {
		t.Fatal(err)
	}
	d := Decision{Action: PolicySetupPrefund, StrategyKey: "OnRe/ONyc/USDC", Reason: "phase3_policy_setup", AmountRaw: 1, IdempotencyKey: "setup"}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := runtimeRoute(d.StrategyKey); err == nil {
		t.Fatal("setup metadata activated the rejected runtime lane")
	}
	_, err := (&Database{}).RecordDecision(context.Background(), "route", Observation{}, d, "", "")
	assertBudgetHold(t, err, "policy_setup_requires_atomic_intent")
}

func setupGuardRPC(t *testing.T, slot int64, delay time.Duration) *RPCClient {
	t.Helper()
	rpc, _ := NewRPCClient("https://rpc.invalid")
	rpc.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body struct {
			Method string
			Params []json.RawMessage
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Method != "getMultipleAccounts" {
			t.Fatal("setup persistence attempted non-read RPC")
		}
		var addresses []string
		var config map[string]any
		_ = json.Unmarshal(body.Params[0], &addresses)
		_ = json.Unmarshal(body.Params[1], &config)
		policy, _ := policySetupAddress(140)
		if len(addresses) != 3 || addresses[0] != bridgeSettings || addresses[1] != bridgeSettingsSigner || addresses[2] != encodeBase58(policy[:]) || config["commitment"] != "confirmed" {
			t.Fatal("setup guard identity drift")
		}
		if delay > 0 {
			time.Sleep(delay)
		}
		a := setupSettingsAccount(t)
		encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]int64{"slot": slot}, "value": []any{
			map[string]any{"owner": a.Owner, "lamports": a.Lamports, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}},
			map[string]any{"owner": "11111111111111111111111111111111", "lamports": 1_000_000_000, "data": []string{"", "base64"}}, nil}}})
		return response(string(encoded)), nil
	})
	return rpc
}

// Parent test enforces the disposable database URL. No production credentials.
func testPolicySetupDurability(t *testing.T, url string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.pool.Exec(ctx, `ALTER TABLE loyal_yield.multiply_operations
	 ADD COLUMN IF NOT EXISTS cycle bigint DEFAULT 1,
	 ADD COLUMN IF NOT EXISTS engine_version text,
	 ADD COLUMN IF NOT EXISTS idempotency_key text,
	 ADD COLUMN IF NOT EXISTS created_at timestamptz DEFAULT now(),
	 ADD COLUMN IF NOT EXISTS signed_wire_sha256 text,
	 ADD COLUMN IF NOT EXISTS message_sha256 text,
	 ADD COLUMN IF NOT EXISTS simulation_slot bigint,
	 ADD COLUMN IF NOT EXISTS simulation_result jsonb,
	 ADD COLUMN IF NOT EXISTS recent_blockhash text,
	 ADD COLUMN IF NOT EXISTS last_valid_block_height bigint;`); err != nil {
		t.Fatal(err)
	}
	newRoute := func(t *testing.T, b Phase3Budget, ttl time.Duration) string {
		t.Helper()
		key := fmt.Sprintf("phase3-setup-%d", time.Now().UnixNano())
		raw, _ := json.Marshal(map[string]any{"generation": 1, "phase3": b, "unrelated": "preserve"})
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,$2::jsonb)`, key, string(raw)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AcquireRouteLease(ctx, key, "setup", ttl); err != nil {
			t.Fatal(err)
		}
		return key
	}
	read := func(t *testing.T, key string) (string, int) {
		t.Helper()
		var state string
		var n int
		if err := db.pool.QueryRow(ctx, `SELECT state::text,(SELECT count(*) FROM loyal_yield.multiply_operations WHERE route_key=$1) FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&state, &n); err != nil {
			t.Fatal(err)
		}
		return state, n
	}
	plan := observedSetupFixture(t, "borrow")
	t.Run("setup signed wire and simulation commit atomically", func(t *testing.T) {
		testPolicySetupSignedPersistence(t, ctx, db, newRoute, plan)
	})
	t.Run("expired initial setup wire preserves evidence and budget", func(t *testing.T) {
		for _, operation := range []string{"borrow", "repay", "onre-entry-swap", "onre-return-swap"} {
			t.Run(operation, func(t *testing.T) {
				testPolicySetupExpiredWire(t, ctx, db, func(t *testing.T) PersistedOperation {
					initial := plan
					if operation == "repay" {
						initial = *setupPaymentAuth(t, "direct").PolicySetup
					} else if operation != "borrow" {
						initial = observedSetupFixture(t, operation)
					}
					budget := emptyTestBudget()
					budget.Families["OnRe"] = FamilyBudget{SpentMicros: 3_000_000}
					key := newRoute(t, budget, time.Minute)
					if _, err := db.persistPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), key, initial); err != nil {
						t.Fatal(err)
					}
					op, err := db.LoadNonterminal(ctx, key)
					if err != nil || op == nil {
						t.Fatal("missing initial setup", err)
					}
					return *op
				})
			})
		}
	})
	t.Run("payment authorization preserves setup budget", func(t *testing.T) {
		key := newRoute(t, emptyTestBudget(), time.Minute)
		r, err := db.persistPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), key, plan)
		if err != nil {
			t.Fatal(err)
		}
		testPolicySetupPaymentAuthorization(t, ctx, db, r.OperationID)
	})
	t.Run("unsigned refresh is atomic and preserves lifetime spend", func(t *testing.T) {
		testPolicySetupUnsignedRefresh(t, ctx, db, newRoute, plan)
	})
	t.Run("finalized prefund advances atomically without duplicate funding", func(t *testing.T) {
		prepare := func(t *testing.T) PersistedOperation {
			b := emptyTestBudget()
			b.Families["OnRe"] = FamilyBudget{SpentMicros: 3_000_000}
			key := newRoute(t, b, time.Minute)
			r, err := db.persistPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), key, plan)
			if err != nil {
				t.Fatal(err)
			}
			op := setupPrefundOperation(t, plan)
			op.ID, op.RouteKey = r.OperationID, key
			digest, _ := validatePolicySetupPlan(plan)
			auth := phase3OperationAuthorization{GoalID: Phase3GoalID, IntentSHA256: digest, PolicySetup: &plan, SignedWireSHA256: op.SignedWireSHA256, SendKnownCost: &plan.Payments[0]}
			encoded, _ := json.Marshal(auth)
			// Synthetic persisted send state for the existing recovery entrypoint;
			// no signer or broadcast is used to prepare this disposable fixture.
			_, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='submitted',signed_wire=$2,signed_wire_sha256=$3,transaction_signature=$4,recent_blockhash=$5,last_valid_block_height=$6,expected_effects=jsonb_set(expected_effects,'{phase3}',$7::jsonb) WHERE operation_id=$1`, op.ID, op.SignedWire, op.SignedWireSHA256, op.TransactionSignature, op.RecentBlockhash, op.LastValidBlockHeight, string(encoded))
			if err != nil {
				t.Fatal(err)
			}
			return op
		}
		t.Run("expired creation wire preserves finalized prefund", func(t *testing.T) {
			testPolicySetupExpiredWire(t, ctx, db, func(t *testing.T) PersistedOperation {
				parent := prepare(t)
				if err := AdvanceNonterminal(ctx, db, setupCompletionRPC(t, plan, parent, ""), parent); err != nil {
					t.Fatal(err)
				}
				child, err := db.LoadNonterminal(ctx, parent.RouteKey)
				if err != nil || child == nil || child.Status != Decided {
					t.Fatal("missing reserved creation", err)
				}
				return *child
			})
		})
		for _, drift := range []string{"unfinalized", "target credit", "settings", "over cap", "lease expired"} {
			t.Run(drift, func(t *testing.T) {
				op := prepare(t)
				before, _ := read(t, op.RouteKey)
				rpc := setupCompletionRPC(t, plan, op, drift)
				if drift == "lease expired" {
					if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET lease_expires_at=clock_timestamp()+interval '100 milliseconds' WHERE route_key=$1`, op.RouteKey); err != nil {
						t.Fatal(err)
					}
					base := rpc.client.Transport
					rpc.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
						time.Sleep(30 * time.Millisecond)
						return base.RoundTrip(r)
					})
				}
				if err := AdvanceNonterminal(ctx, db, rpc, op); err == nil {
					t.Fatal("unsafe continuation advanced")
				}
				after, count := read(t, op.RouteKey)
				if drift == "lease expired" {
					if _, err := db.AcquireRouteLease(ctx, op.RouteKey, "restart-expired", time.Minute); err != nil {
						t.Fatal(err)
					}
				}
				pending, err := db.LoadNonterminal(ctx, op.RouteKey)
				if err != nil || pending == nil || pending.ID != op.ID || pending.Status != Submitted || after != before || count != 1 {
					t.Fatalf("failed recovery lost intent/reserve: %+v %v", pending, err)
				}
			})
		}
		t.Run("unpaid creation refresh preserves finalized prefund", func(t *testing.T) {
			testPolicySetupCompletionRefresh(t, ctx, db, plan, prepare)
		})
		op := prepare(t)
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); errs <- AdvanceNonterminal(ctx, db, setupCompletionRPC(t, plan, op, ""), op) }()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.ReleaseRouteLease(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := db.AcquireRouteLease(ctx, op.RouteKey, "creation-restart", time.Minute); err != nil {
			t.Fatal(err)
		}
		child, err := db.LoadNonterminal(ctx, op.RouteKey)
		if err != nil || child == nil || child.ID == op.ID || child.Decision.Action != PolicySetupCreate || child.Status != Decided {
			t.Fatalf("restart did not load creation continuation: %+v %v", child, err)
		}
		assertBudgetHold(t, AdvanceNonterminal(ctx, db, setupCompletionRPC(t, plan, op, ""), *child), "policy_setup_execution_not_enabled")
		before, count := read(t, op.RouteKey)
		var state struct {
			Budget  Phase3Budget `json:"phase3"`
			Pointer string       `json:"phase3SetupIntent"`
		}
		if json.Unmarshal([]byte(before), &state) != nil || count != 2 || state.Pointer != op.ID || len(state.Budget.Reservations) != 1 || state.Budget.Families["OnRe"].SpentMicros != 3_000_000+plan.Payments[0].TotalMicros {
			t.Fatal("prefund not booked exactly once or journal duplicated")
		}
		reservation := state.Budget.Reservations[child.ID]
		if !reservation.Recovery || reservation.ExitBeforeMicros != Phase3TransactionCapMicros || reservation.UpperMicros != plan.Payments[1].TotalMicros {
			t.Fatal("completion did not consume the original reserved headroom")
		}
		var finalized bool
		var effects []byte
		if err := db.pool.QueryRow(ctx, `SELECT status='reconciled' AND confirmation_status='finalized',reconciled_effects FROM loyal_yield.multiply_operations WHERE operation_id=$1`, op.ID).Scan(&finalized, &effects); err != nil || !finalized || !json.Valid(effects) {
			t.Fatal("finalized prefund receipt was not retained")
		}
		// Replay the old recovery request after the atomic transition. It must
		// return the same child without RPC repricing, duplicate spend or funding.
		retry, err := db.continuePolicySetupPrefund(ctx, setupCompletionRPC(t, plan, op, "over cap"), op.ID)
		after, count := read(t, op.RouteKey)
		if err != nil || retry.OperationID != child.ID || after != before || count != 2 {
			t.Fatalf("continuation retry changed state: %+v %v", retry, err)
		}
		assertBudgetHold(t, db.cancelUnsentPolicySetupIntent(ctx, op.ID), "policy_setup_not_proven_unsent")
		assertBudgetHold(t, db.cancelUnsentPolicySetupIntent(ctx, child.ID), "policy_setup_not_proven_unsent")
		t.Run("creation settles and releases fence", func(t *testing.T) {
			testPolicySetupCreationSettlement(t, ctx, db, child.ID, op.RouteKey, 3_000_000+plan.TotalMicros)
		})
	})
	t.Run("direct creation settles and releases fence", func(t *testing.T) {
		direct, err := observePolicySetup(ctx, setupObservationRPC(t, &setupRPCScenario{rent: 8_000_000}), "repay")
		if err != nil {
			t.Fatal(err)
		}
		b := emptyTestBudget()
		b.Families["OnRe"] = FamilyBudget{SpentMicros: 3_000_000}
		key := newRoute(t, b, time.Minute)
		r, err := db.persistPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), key, direct)
		if err != nil {
			t.Fatal(err)
		}
		testPolicySetupCreationSettlement(t, ctx, db, r.OperationID, key, 3_000_000+direct.TotalMicros)
	})
	t.Run("persisted completion headroom tolerates repricing within existing cap", func(t *testing.T) {
		b := emptyTestBudget()
		b.Families["OnRe"] = FamilyBudget{SpentMicros: 3_000_000}
		key := newRoute(t, b, time.Minute)
		record, err := db.persistPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), key, plan)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := read(t, key)
		var restored struct {
			Budget Phase3Budget `json:"phase3"`
		}
		if err := json.Unmarshal([]byte(raw), &restored); err != nil {
			t.Fatal(err)
		}
		first := restored.Budget.Reservations[record.OperationID]
		if first.ExitAfterMicros != Phase3TransactionCapMicros || plan.CompletionReserveMicros >= first.ExitAfterMicros {
			t.Fatal("durable reserve did not retain repricing headroom")
		}
		// Exercise the existing reducer on the persisted budget. This is not
		// production prefund reconciliation or a creation continuation sender.
		if err := restored.Budget.Settle(first.OperationID, first.IntentSHA256, first.UpperMicros); err != nil {
			t.Fatal(err)
		}
		second := BudgetReservation{OperationID: "repriced-create", Family: "OnRe", IntentSHA256: sha256Bytes([]byte("fresh creation payment")), UpperMicros: Phase3TransactionCapMicros + 1, Recovery: true}
		assertBudgetHold(t, restored.Budget.Admit(second), "transaction_cap_exceeded")
		second.UpperMicros = plan.CompletionReserveMicros + 1
		if err := restored.Budget.Admit(second); err != nil {
			t.Fatal(err)
		}
		if err := restored.Budget.Settle(second.OperationID, second.IntentSHA256, second.UpperMicros); err != nil {
			t.Fatal(err)
		}
		if restored.Budget.Families["OnRe"] != (FamilyBudget{SpentMicros: 3_000_000 + first.UpperMicros + second.UpperMicros}) {
			t.Fatal("unused headroom was spent, history reset, or reserve retained")
		}
	})
	t.Run("concurrent retry restart and safe unsent cancellation", func(t *testing.T) {
		b := emptyTestBudget()
		b.Families["OnRe"] = FamilyBudget{SpentMicros: 3_000_000}
		key := newRoute(t, b, time.Minute)
		var wg sync.WaitGroup
		records := make(chan DecisionRecord, 2)
		errs := make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r, e := db.persistPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), key, plan)
				records <- r
				errs <- e
			}()
		}
		wg.Wait()
		close(records)
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		id := ""
		for r := range records {
			if id != "" && id != r.OperationID {
				t.Fatal("duplicate setup identity")
			}
			id = r.OperationID
		}
		before, count := read(t, key)
		if count != 1 {
			t.Fatal("more than one setup journal row")
		}
		other, err := OpenDatabase(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		if _, err = other.AcquireRouteLease(ctx, key, "restart", time.Minute); err != ErrRouteLeaseUnavailable {
			t.Fatalf("lease preemption: %v", err)
		}
		if _, err = db.ReleaseRouteLease(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err = other.AcquireRouteLease(ctx, key, "restart", time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err = other.persistPolicySetupIntent(ctx, setupGuardRPC(t, 75, 0), key, plan); err != nil {
			t.Fatal(err)
		} // passive exact retry, not fresh send authority
		after, _ := read(t, key)
		if before != after {
			t.Fatal("retry changed budget or original plan")
		}
		op, err := other.LoadNonterminal(ctx, key)
		if err != nil || op == nil || op.ID != id || op.Decision.Action != PolicySetupPrefund {
			t.Fatalf("restart lost pending setup: %+v %v", op, err)
		}
		assertBudgetHold(t, AdvanceNonterminal(ctx, other, setupGuardRPC(t, 42, 0), *op), "policy_setup_execution_not_enabled")
		o, d, _ := bridgeAdmissionFixture(t, VoltrAllocateToSquads, 100_000, 200_000, 0, 0)
		_, err = other.RecordDecision(ctx, key, o, d, strings.Repeat("a", 64), strings.Repeat("b", 64))
		assertBudgetHold(t, err, "policy_setup_in_progress")
		if err = other.cancelUnsentPolicySetupIntent(ctx, id); err != nil {
			t.Fatal(err)
		}
		after, _ = read(t, key)
		var state struct {
			Budget    Phase3Budget `json:"phase3"`
			Pointer   *string      `json:"phase3SetupIntent"`
			Unrelated string       `json:"unrelated"`
		}
		_ = json.Unmarshal([]byte(after), &state)
		if state.Pointer != nil || state.Unrelated != "preserve" || state.Budget.Families["OnRe"] != (FamilyBudget{SpentMicros: 3_000_000}) || len(state.Budget.Reservations) != 0 {
			t.Fatal("cancel lost history or retained a phantom reserve")
		}
	})
	for name, setup := range map[string]func(*Phase3Budget){"family cap": func(b *Phase3Budget) { b.Families["OnRe"] = FamilyBudget{SpentMicros: Phase3FamilyCapMicros - 1} }, "reserved exit": func(b *Phase3Budget) { b.Families["AUTO"] = FamilyBudget{ExitMicros: 1} }, "closed": func(b *Phase3Budget) { b.Closed = true }} {
		t.Run(name, func(t *testing.T) {
			b := emptyTestBudget()
			setup(&b)
			key := newRoute(t, b, time.Minute)
			before, _ := read(t, key)
			_, err := db.persistPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), key, plan)
			assertBudgetHold(t, err, map[string]string{"family cap": "family_cap_exceeded", "reserved exit": "setup_conflicts_with_reserved_exit", "closed": "goal_envelope_expired"}[name])
			after, n := read(t, key)
			if before != after || n != 0 {
				t.Fatal("rejection partially persisted setup")
			}
		})
	}
	t.Run("lease expires while guard is in flight", func(t *testing.T) {
		key := newRoute(t, emptyTestBudget(), 100*time.Millisecond)
		before, _ := read(t, key)
		_, err := db.persistPolicySetupIntent(ctx, setupGuardRPC(t, 42, 180*time.Millisecond), key, plan)
		if err != ErrRouteLeaseLost {
			t.Fatalf("expired lease committed: %v", err)
		}
		after, n := read(t, key)
		if before != after || n != 0 {
			t.Fatal("lease failure partially committed")
		}
	})
	t.Run("freshness failure rolls back", func(t *testing.T) {
		key := newRoute(t, emptyTestBudget(), time.Minute)
		_, err := db.persistPolicySetupIntent(ctx, setupGuardRPC(t, 75, 0), key, plan)
		assertBudgetHold(t, err, "policy_setup_valuation_expired")
		_, n := read(t, key)
		if n != 0 {
			t.Fatal("stale plan persisted")
		}
	})
	t.Run("changed and corrupt intents cannot reset admission", func(t *testing.T) {
		key := newRoute(t, emptyTestBudget(), time.Minute)
		r, err := db.persistPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), key, plan)
		if err != nil {
			t.Fatal(err)
		}
		before, _ := read(t, key)
		_, err = db.persistPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), key, observedSetupFixture(t, "repay"))
		assertBudgetHold(t, err, "policy_setup_in_progress")
		if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=jsonb_set(expected_effects,'{phase3,policySetup,totalMicros}','1') WHERE operation_id=$1`, r.OperationID); err != nil {
			t.Fatal(err)
		}
		_, err = db.persistPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), key, plan)
		assertBudgetHold(t, err, "invalid_persisted_setup_intent")
		after, n := read(t, key)
		if before != after || n != 1 {
			t.Fatal("corrupt or changed intent rewrote budget")
		}
	})
	t.Run("orphaned setup prevents budget recreation", func(t *testing.T) {
		key := newRoute(t, emptyTestBudget(), time.Minute)
		if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state-'phase3','{phase3SetupIntent}','"missing-operation"') WHERE route_key=$1`, key); err != nil {
			t.Fatal(err)
		}
		before, _ := read(t, key)
		_, err = db.initializePhase3Budget(ctx, key)
		assertBudgetHold(t, err, "orphaned_policy_setup_intent")
		after, _ := read(t, key)
		if before != after {
			t.Fatal("orphaned setup got a new budget")
		}
	})
	for _, column := range []string{"signed_wire='\\x01'::bytea", "transaction_signature='observed-signature'", "broadcast_intent_at=clock_timestamp()", "status='reconciled'"} {
		t.Run("cannot cancel "+column, func(t *testing.T) {
			key := newRoute(t, emptyTestBudget(), time.Minute)
			r, err := db.persistPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), key, plan)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET `+column+` WHERE operation_id=$1`, r.OperationID); err != nil {
				t.Fatal(err)
			}
			before, _ := read(t, key)
			assertBudgetHold(t, db.cancelUnsentPolicySetupIntent(ctx, r.OperationID), "policy_setup_not_proven_unsent")
			after, _ := read(t, key)
			if before != after {
				t.Fatal("uncertain setup released budget")
			}
		})
	}
	t.Run("migration executes exact action and strategy constraints", func(t *testing.T) {
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if _, err = tx.Exec(ctx, `CREATE TEMP TABLE phase3_actions(action text NOT NULL,engine_version text NOT NULL,strategy_key text) ON COMMIT DROP`); err != nil {
			t.Fatal(err)
		}
		sql, err := os.ReadFile("../../../../crates/loyal-yield-store/migrations/0074_backyard_rwa_phase3_journal_actions.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(ctx, strings.ReplaceAll(string(sql), "loyal_yield.multiply_operations", "pg_temp.phase3_actions")); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			action, engine, lane string
			pass                 bool
		}{
			{"OPEN_ROUTE_STEP", "backyard_rwa_v1", "Ethena/USDe/PYUSD", true},
			{"SWAP_USDC_TO_DEBT_STEP", "backyard_rwa_v1", "AUTO/AUTO/PYUSD", true},
			{"POLICY_SETUP_PREFUND", "backyard_rwa_v1", "OnRe/ONyc/USDC", true},
			{"POLICY_SETUP_CREATE", "backyard_rwa_v1", "OnRe/ONyc/USDC", true},
			{"claim", "linus_v1", "legacy", true},
			{"OPEN_ROUTE_STEP", "backyard_rwa_v1", "OnRe/ONyc/USDC", false},
			{"POLICY_SETUP_CREATE", "backyard_rwa_v1", "Ethena/USDe/PYUSD", false},
			{"POLICY_SETUP_CREATE", "earn_max_v1", "OnRe/ONyc/USDC", false},
			{"UNREVIEWED_ACTION", "backyard_rwa_v1", "AUTO/AUTO/PYUSD", false},
		} {
			nested, err := tx.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = nested.Exec(ctx, `INSERT INTO pg_temp.phase3_actions VALUES($1,$2,$3)`, tc.action, tc.engine, tc.lane)
			if (err == nil) != tc.pass {
				t.Fatalf("migration verdict %s %s: %v", tc.action, tc.lane, err)
			}
			_ = nested.Rollback(ctx)
		}
	})
}

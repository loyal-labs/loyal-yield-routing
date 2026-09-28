package backyardrwa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPhase3BudgetInitializationConfig(t *testing.T) {
	for _, route := range []string{"", "another-route"} {
		_, err := RunPhase3BudgetInitialization(context.Background(), "not-a-database-secret", route)
		assertBudgetHold(t, err, "invalid_phase3_initialization_config")
	}
	_, err := RunPhase3BudgetInitialization(context.Background(), "", productionRouteKey)
	assertBudgetHold(t, err, "invalid_phase3_initialization_config")
	_, err = RunPhase3BudgetInitialization(context.Background(), "postgres://%not-a-database-secret", productionRouteKey)
	assertBudgetHold(t, err, "phase3_initialization_database_unavailable")
	if strings.Contains(err.Error(), "not-a-database-secret") {
		t.Fatal("initialization leaked database input")
	}
}

// Called only after the parent test enforces a disposable Unix-socket database
// and creates the existing route/journal schema. All mutations are test-local.
func testPhase3BudgetInitialization(t *testing.T, url string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	newRoute := func(t *testing.T) (*Database, string) {
		t.Helper()
		db, err := OpenDatabase(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(db.Close)
		key := fmt.Sprintf("phase3-init-test-%d", time.Now().UnixNano())
		if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state) VALUES($1,'{"generation":1,"unrelated":{"keep":true}}')`, key); err != nil {
			t.Fatal(err)
		}
		if _, err = db.AcquireRouteLease(ctx, key, "initializer", time.Minute); err != nil {
			t.Fatal(err)
		}
		return db, key
	}
	readState := func(t *testing.T, db *Database, key string) string {
		t.Helper()
		var state string
		if err := db.pool.QueryRow(ctx, `SELECT state::text FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	insertOperation := func(t *testing.T, db *Database, key, lane, status, effects string, wire []byte) {
		t.Helper()
		if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,strategy_key,status,action,expected_effects,signed_wire)
		 VALUES($1,$2,$3,$4,'OPEN_ROUTE_STEP',$5::jsonb,$6)`, key+"-op", key, lane, status, effects, wire); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("create once and preserve state through restart", func(t *testing.T) {
		db, key := newRoute(t)
		var wg sync.WaitGroup
		results := make(chan Phase3BudgetInitializationResult, 2)
		errResults := make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				result, err := db.initializePhase3Budget(ctx, key)
				results <- result
				errResults <- err
			}()
		}
		wg.Wait()
		close(results)
		close(errResults)
		for err := range errResults {
			if err != nil {
				t.Fatal(err)
			}
		}
		created := 0
		var marker phase3Initialization
		for result := range results {
			if result.Created {
				created++
			}
			if result.ProofLevel != "BOOKKEEPING_NOT_ACTIVATION" || result.Closed || result.Initialization.GoalID != Phase3GoalID || result.Initialization.Generation != 2 {
				t.Fatalf("incorrect initialization result: %+v", result)
			}
			if !marker.CreatedAt.IsZero() && marker != result.Initialization {
				t.Fatal("concurrent initialization changed marker")
			}
			marker = result.Initialization
		}
		if created != 1 {
			t.Fatalf("created %d budgets", created)
		}
		var state struct {
			Generation int64           `json:"generation"`
			Budget     Phase3Budget    `json:"phase3"`
			Unrelated  map[string]bool `json:"unrelated"`
		}
		if err := json.Unmarshal([]byte(readState(t, db, key)), &state); err != nil {
			t.Fatal(err)
		}
		if state.Generation != 2 || !state.Unrelated["keep"] || state.Budget.validate() != nil || len(state.Budget.Reservations) != 0 {
			t.Fatal("initialization damaged existing state or created an invalid budget")
		}
		for _, family := range state.Budget.Families {
			if family != (FamilyBudget{}) {
				t.Fatal("initial budget was not zero")
			}
		}
		// Model persisted spending and a reserved operation, then reopen the DB.
		state.Budget.Families["OnRe"] = FamilyBudget{SpentMicros: 1000, ExitMicros: 2_000_000}
		r := testReservation()
		state.Budget.Reservations[r.OperationID] = r
		state.Budget.Closed = true
		budget, _ := json.Marshal(state.Budget)
		if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{phase3}',$2::jsonb) WHERE route_key=$1`, key, string(budget)); err != nil {
			t.Fatal(err)
		}
		before := readState(t, db, key)
		other, err := OpenDatabase(ctx, url)
		if err != nil {
			t.Fatal(err)
		}
		defer other.Close()
		if _, err = other.AcquireRouteLease(ctx, key, "initializer", time.Minute); !errors.Is(err, ErrRouteLeaseUnavailable) {
			t.Fatalf("initializer preempted existing lease: %v", err)
		}
		if released, err := db.ReleaseRouteLease(ctx); err != nil || !released {
			t.Fatalf("release: %v", err)
		}
		if _, err = other.AcquireRouteLease(ctx, key, "restart", time.Minute); err != nil {
			t.Fatal(err)
		}
		result, err := other.initializePhase3Budget(ctx, key)
		if err != nil || result.Created || !result.Closed || result.Initialization != marker || readState(t, other, key) != before {
			t.Fatalf("restart replenished or reopened budget: %+v %v", result, err)
		}
		var operations int
		if err = other.pool.QueryRow(ctx, `SELECT count(*) FROM loyal_yield.multiply_operations WHERE route_key=$1`, key).Scan(&operations); err != nil || operations != 0 {
			t.Fatalf("bookkeeping created journal/wire work: %d %v", operations, err)
		}
	})
	for _, mutation := range []struct{ name, expression, reason string }{
		{"missing budget", "state-'phase3'", "incomplete_phase3_initialization"},
		{"missing marker", "state-'phase3Initialization'", "incomplete_phase3_initialization"},
		{"null budget", "jsonb_set(state,'{phase3}','null')", "invalid_phase3_initialization"},
		{"wrong goal", "jsonb_set(state,'{phase3,goalId}','\"another-goal\"')", "invalid_phase3_initialization"},
		{"wrong marker", "jsonb_set(state,'{phase3Initialization,goalId}','\"another-goal\"')", "invalid_phase3_initialization"},
		{"future generation", "jsonb_set(state,'{phase3Initialization,generation}','999')", "invalid_phase3_initialization"},
		{"null marker", "jsonb_set(state,'{phase3Initialization}','null')", "invalid_phase3_initialization"},
		{"over cap", "jsonb_set(state,'{phase3,families,AUTO,spentMicros}','20000001')", "persisted_budget_exceeds_cap"},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			db, key := newRoute(t)
			if _, err := db.initializePhase3Budget(ctx, key); err != nil {
				t.Fatal(err)
			}
			if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=`+mutation.expression+` WHERE route_key=$1`, key); err != nil {
				t.Fatal(err)
			}
			before := readState(t, db, key)
			_, err := db.initializePhase3Budget(ctx, key)
			assertBudgetHold(t, err, mutation.reason)
			if readState(t, db, key) != before {
				t.Fatal("rejection mutated state")
			}
		})
	}
	for _, lane := range []string{"OnRe/ONyc/USDC", "OnRe/ONyc/USDG", "OnRe/ONyc/USDS", "AUTO/AUTO/PYUSD", "Ethena/USDe/PYUSD"} {
		t.Run("untagged signed history "+lane, func(t *testing.T) {
			db, key := newRoute(t)
			insertOperation(t, db, key, lane, "failed", "{}", []byte{1})
			_, err := db.initializePhase3Budget(ctx, key)
			assertBudgetHold(t, err, "phase3_history_prevents_budget_creation")
		})
	}
	for _, status := range []string{"failed", "reconciled"} {
		t.Run("authorization history "+status, func(t *testing.T) {
			db, key := newRoute(t)
			insertOperation(t, db, key, "", status, `{"phase3":{}}`, nil)
			_, err := db.initializePhase3Budget(ctx, key)
			assertBudgetHold(t, err, "phase3_history_prevents_budget_creation")
		})
	}
	for _, column := range []string{"transaction_signature='retained-signature'", "broadcast_intent_at=clock_timestamp()"} {
		t.Run("submission history "+column, func(t *testing.T) {
			db, key := newRoute(t)
			insertOperation(t, db, key, "Ethena/USDe/PYUSD", "failed", "{}", nil)
			if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET `+column+` WHERE operation_id=$1`, key+"-op"); err != nil {
				t.Fatal(err)
			}
			_, err := db.initializePhase3Budget(ctx, key)
			assertBudgetHold(t, err, "phase3_history_prevents_budget_creation")
		})
	}
	for _, status := range []string{"prepared", "signed_persisted", "broadcast_intent", "confirmed", "reconciliation_pending", "decided", "built", "simulated", "signed", "submitted", "reconciling", "manual_recovery"} {
		t.Run("unresolved work "+status, func(t *testing.T) {
			db, key := newRoute(t)
			insertOperation(t, db, key, "Prime/PRIME/USDC", status, "{}", nil)
			_, err := db.initializePhase3Budget(ctx, key)
			assertBudgetHold(t, err, "unresolved_work_prevents_budget_creation")
		})
	}
	t.Run("unsubmitted missing budget HOLD permits first creation", func(t *testing.T) {
		db, key := newRoute(t)
		insertOperation(t, db, key, "Ethena/USDe/PYUSD", "failed", `{"budgetHold":{"reason":"missing_goal_budget"}}`, nil)
		result, err := db.initializePhase3Budget(ctx, key)
		if err != nil || !result.Created {
			t.Fatalf("never-submitted HOLD prevented bookkeeping: %+v %v", result, err)
		}
	})
	t.Run("lost expired and mismatched lease", func(t *testing.T) {
		db, key := newRoute(t)
		before := readState(t, db, key)
		_, err := db.initializePhase3Budget(ctx, key+"-other")
		if !errors.Is(err, ErrRouteLeaseLost) {
			t.Fatalf("wrong route: %v", err)
		}
		if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE route_key=$1`, key); err != nil {
			t.Fatal(err)
		}
		_, err = db.initializePhase3Budget(ctx, key)
		if !errors.Is(err, ErrRouteLeaseLost) || readState(t, db, key) != before {
			t.Fatalf("expired lease mutated budget: %v", err)
		}
		if _, err = db.AcquireRouteLease(ctx, key, "new-owner", time.Minute); err != nil {
			t.Fatal(err)
		}
		if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET fencing_token=fencing_token+1 WHERE route_key=$1`, key); err != nil {
			t.Fatal(err)
		}
		_, err = db.initializePhase3Budget(ctx, key)
		if !errors.Is(err, ErrRouteLeaseLost) || readState(t, db, key) != before {
			t.Fatalf("stale fence mutated budget: %v", err)
		}
	})
	t.Run("lease expiry after row lock rolls back creation", func(t *testing.T) {
		db, key := newRoute(t)
		before := readState(t, db, key)
		if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET lease_expires_at=clock_timestamp()+interval '1 second' WHERE route_key=$1`, key); err != nil {
			t.Fatal(err)
		}
		blocker, err := db.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer blocker.Rollback(ctx)
		if _, err = blocker.Exec(ctx, `LOCK loyal_yield.multiply_operations IN ACCESS EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := db.initializePhase3Budget(ctx, key); done <- err }()
		// Wait for the initializer to reach its history query AFTER locking the
		// route. The held journal lock makes the final-write fence test decisive.
		deadline := time.Now().Add(time.Second)
		for {
			var waiting bool
			if err = db.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE relation='loyal_yield.multiply_operations'::regclass AND mode='AccessShareLock' AND NOT granted)`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("initializer did not reach the locked journal")
			}
			time.Sleep(5 * time.Millisecond)
		}
		if _, err = blocker.Exec(ctx, `SELECT pg_sleep(1.05)`); err != nil {
			t.Fatal(err)
		}
		if err = blocker.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		if err = <-done; !errors.Is(err, ErrRouteLeaseLost) || readState(t, db, key) != before {
			t.Fatalf("expired final-write fence permitted initialization: %v", err)
		}
	})
}

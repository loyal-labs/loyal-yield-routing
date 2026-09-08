package backyardrwa

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func testPolicySetupUnsignedRefresh(t *testing.T, ctx context.Context, db *Database, newRoute func(*testing.T, Phase3Budget, time.Duration) string, original policySetupObservation) {
	fresh, err := observePolicySetup(ctx, setupObservationRPC(t, &setupRPCScenario{rent: 12_000_000, blockhash: bridgeUSDC}), "borrow")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Request.RecentBlockhash == original.Request.RecentBlockhash || fresh.Payments[0].TotalMicros <= original.Payments[0].TotalMicros {
		t.Fatal("refresh fixture must change the wire and raise its quoted reservation")
	}
	prepare := func(t *testing.T, limit bool) (string, DecisionRecord) {
		b := emptyTestBudget()
		b.Families["OnRe"] = FamilyBudget{SpentMicros: 3_000_000}
		b.Families["AUTO"] = FamilyBudget{SpentMicros: 2_000_000}
		if limit {
			b.Families["OnRe"] = FamilyBudget{SpentMicros: Phase3FamilyCapMicros - original.Payments[0].TotalMicros - Phase3TransactionCapMicros}
		}
		key := newRoute(t, b, time.Minute)
		r, err := db.persistPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), key, original)
		if err != nil {
			t.Fatal(err)
		}
		return key, r
	}
	snapshot := func(t *testing.T, key string) string {
		var value string
		err := db.pool.QueryRow(ctx, `SELECT jsonb_build_object('state',r.state,'operations',(SELECT jsonb_agg(to_jsonb(o) ORDER BY operation_id) FROM loyal_yield.multiply_operations o WHERE o.route_key=r.route_key))::text FROM loyal_yield.multiply_route_states r WHERE route_key=$1`, key).Scan(&value)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	for _, kind := range []string{"wire", "wire hash", "signature", "submitted", "continuation", "different operation", "expired observation", "cap", "closed goal", "lease expiry"} {
		t.Run(kind, func(t *testing.T) {
			key, record := prepare(t, kind == "cap")
			plan, rpc := fresh, setupGuardRPC(t, 42, 0)
			var update string
			switch kind {
			case "wire":
				update = `UPDATE loyal_yield.multiply_operations SET signed_wire='\x01'::bytea WHERE operation_id=$1`
			case "wire hash":
				update = `UPDATE loyal_yield.multiply_operations SET signed_wire_sha256=repeat('a',64) WHERE operation_id=$1`
			case "signature":
				update = `UPDATE loyal_yield.multiply_operations SET transaction_signature='recorded' WHERE operation_id=$1`
			case "submitted":
				update = `UPDATE loyal_yield.multiply_operations SET status='submitted' WHERE operation_id=$1`
			case "continuation":
				update = `UPDATE loyal_yield.multiply_operations SET expected_effects=jsonb_set(expected_effects,'{phase3,policySetupCompletion}','{}'::jsonb,true) WHERE operation_id=$1`
			case "different operation":
				plan, err = observePolicySetup(ctx, setupObservationRPC(t, &setupRPCScenario{rent: 8_000_000, blockhash: bridgeVault}), "repay")
				if err != nil {
					t.Fatal(err)
				}
			case "expired observation":
				rpc = setupGuardRPC(t, 75, 0)
			case "closed goal":
				if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{phase3,closed}','true'::jsonb) WHERE route_key=$1`, key); err != nil {
					t.Fatal(err)
				}
			case "lease expiry":
				if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET lease_expires_at=clock_timestamp()+interval '50 milliseconds' WHERE route_key=$1`, key); err != nil {
					t.Fatal(err)
				}
				rpc = setupGuardRPC(t, 42, 100*time.Millisecond)
			}
			if update != "" {
				if _, err = db.pool.Exec(ctx, update, record.OperationID); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshot(t, key)
			if _, err = db.refreshUnsentPolicySetupIntent(ctx, rpc, record.OperationID, plan); err == nil {
				t.Fatal("unsafe refresh accepted")
			}
			if after := snapshot(t, key); after != before {
				t.Fatal("failed refresh changed durable intent, reserve or evidence")
			}
		})
	}
	key, old := prepare(t, false)
	before := snapshot(t, key)
	if _, err = db.refreshUnsentPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), old.OperationID, original); err == nil {
		t.Fatal("unchanged intent treated as a refresh")
	}
	if snapshot(t, key) != before {
		t.Fatal("no-op refresh changed state")
	}
	// Duplicate callers racing the same refresh must observe one replacement.
	var wg sync.WaitGroup
	records := make(chan DecisionRecord, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := db.refreshUnsentPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), old.OperationID, fresh)
			records <- r
			errs <- e
		}()
	}
	wg.Wait()
	close(records)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var replacement DecisionRecord
	for r := range records {
		if replacement.OperationID != "" && replacement != r {
			t.Fatal("duplicate replacement")
		}
		replacement = r
	}
	if replacement.OperationID == old.OperationID || replacement.Status != Decided {
		t.Fatal("replacement identity/status missing")
	}
	var raw []byte
	if err = db.pool.QueryRow(ctx, `SELECT state FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var state struct {
		Budget    Phase3Budget `json:"phase3"`
		Pointer   string       `json:"phase3SetupIntent"`
		Unrelated string       `json:"unrelated"`
	}
	if json.Unmarshal(raw, &state) != nil || state.Pointer != replacement.OperationID || state.Unrelated != "preserve" || len(state.Budget.Reservations) != 1 || state.Budget.Families["OnRe"].SpentMicros != 3_000_000 || state.Budget.Families["AUTO"].SpentMicros != 2_000_000 {
		t.Fatal("refresh reset lifetime accounting or shared state")
	}
	digest, _ := validatePolicySetupPlan(fresh)
	want := BudgetReservation{OperationID: replacement.OperationID, Family: "OnRe", IntentSHA256: digest, UpperMicros: fresh.Payments[0].TotalMicros, ExitAfterMicros: Phase3TransactionCapMicros}
	if !reflect.DeepEqual(state.Budget.Reservations[replacement.OperationID], want) || state.Budget.Families["OnRe"].ExitMicros != Phase3TransactionCapMicros {
		t.Fatal("fresh payment and completion were not reserved together")
	}
	var count int
	var terminal, unsent bool
	var link, reason string
	if err = db.pool.QueryRow(ctx, `SELECT status='failed' AND (expected_effects->'phase3'->>'reservationReleased')::boolean,expected_effects->>'policySetupReplacementOperationId',recovery_reason FROM loyal_yield.multiply_operations WHERE operation_id=$1`, old.OperationID).Scan(&terminal, &link, &reason); err != nil {
		t.Fatal(err)
	}
	if !terminal || link != replacement.OperationID || reason != "policy_setup_unsigned_refresh" {
		t.Fatal("old intent was not retained with replacement lineage")
	}
	if err = db.pool.QueryRow(ctx, `SELECT count(*),bool_and(signed_wire IS NULL AND signed_wire_sha256 IS NULL AND transaction_signature IS NULL AND broadcast_intent_at IS NULL) FROM loyal_yield.multiply_operations WHERE route_key=$1`, key).Scan(&count, &unsent); err != nil || count != 2 || !unsent {
		t.Fatal("refresh lost journal history or created signing evidence", err)
	}
	if _, err = db.ReleaseRouteLease(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = db.AcquireRouteLease(ctx, key, "refresh-restart", time.Minute); err != nil {
		t.Fatal(err)
	}
	before = snapshot(t, key)
	retry, err := db.refreshUnsentPolicySetupIntent(ctx, setupGuardRPC(t, 75, 0), old.OperationID, fresh)
	if err != nil || retry != replacement || snapshot(t, key) != before {
		t.Fatal("restart retry repriced or replenished an existing intent", err)
	}
	if _, err = db.persistPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), key, original); err == nil {
		t.Fatal("superseded original intent was resurrected")
	}
	pending, err := db.LoadNonterminal(ctx, key)
	if err != nil || pending == nil || pending.ID != replacement.OperationID {
		t.Fatal("replacement did not resume through existing journal", err)
	}
	assertBudgetHold(t, AdvanceNonterminal(ctx, db, setupGuardRPC(t, 42, 0), *pending), "policy_setup_execution_not_enabled")
	// Refresh does not silently mutate an admitted plan or preserve a stale
	// pre-sign cost stamp from the canceled generation.
	if err = db.pool.QueryRow(ctx, `SELECT expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, replacement.OperationID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "setupBuildCost") || strings.Contains(string(raw), "signedWireSha256") {
		t.Fatal("old signing authorization leaked into replacement")
	}
	// A fresh lower rent may fit direct creation. Release only the canceled
	// initial plan's unspent completion reserve, never any lifetime spend.
	direct, err := observePolicySetup(ctx, setupObservationRPC(t, &setupRPCScenario{rent: 8_000_000, blockhash: bridgeDelegate}), "borrow")
	if err != nil || direct.Mode != "direct-create" {
		t.Fatal("direct refresh fixture", err)
	}
	last, err := db.refreshUnsentPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), replacement.OperationID, direct)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.pool.QueryRow(ctx, `SELECT state FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	// Unmarshal merges maps; clear the prior observation before reading the
	// replacement so canceled reservation keys cannot survive only in the test.
	state.Budget = Phase3Budget{}
	if json.Unmarshal(raw, &state) != nil || len(state.Budget.Reservations) != 1 || state.Pointer != last.OperationID || state.Budget.Families["OnRe"].ExitMicros != 0 || state.Budget.Families["OnRe"].SpentMicros != 3_000_000 || state.Budget.Families["AUTO"].SpentMicros != 2_000_000 || state.Budget.Reservations[last.OperationID].UpperMicros != direct.Payments[0].TotalMicros {
		t.Fatal("direct refresh reset spend or retained the wrong payment reserve")
	}
	before = snapshot(t, key)
	if _, err = db.refreshUnsentPolicySetupIntent(ctx, setupGuardRPC(t, 42, 0), old.OperationID, fresh); err == nil {
		t.Fatal("an older refresh resurrected its superseded generation")
	}
	if snapshot(t, key) != before {
		t.Fatal("superseded retry changed state")
	}
}

package backyardrwa

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"
)

func testPolicySetupCompletionRefresh(t *testing.T, ctx context.Context, db *Database, plan policySetupObservation, prepare func(*testing.T) PersistedOperation) {
	create := func(t *testing.T) (PersistedOperation, PersistedOperation, phase3OperationAuthorization) {
		parent := prepare(t)
		if err := AdvanceNonterminal(ctx, db, setupCompletionRPC(t, plan, parent, ""), parent); err != nil {
			t.Fatal(err)
		}
		child, err := db.LoadNonterminal(ctx, parent.RouteKey)
		if err != nil || child == nil {
			t.Fatal("missing creation continuation", err)
		}
		var raw []byte
		var auth phase3OperationAuthorization
		if err = db.pool.QueryRow(ctx, `SELECT expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, child.ID).Scan(&raw); err != nil || json.Unmarshal(raw, &auth) != nil {
			t.Fatal("missing authorization", err)
		}
		return parent, *child, auth
	}
	snapshot := func(t *testing.T, key string) string {
		var value string
		if err := db.pool.QueryRow(ctx, `SELECT jsonb_build_object('state',r.state,'operations',(SELECT jsonb_agg(to_jsonb(o) ORDER BY operation_id) FROM loyal_yield.multiply_operations o WHERE o.route_key=r.route_key))::text FROM loyal_yield.multiply_route_states r WHERE route_key=$1`, key).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	for _, kind := range []string{"wire", "wire hash", "signature", "signed", "parent finality", "target credit", "funded amount", "expired", "over cap", "lease expiry"} {
		t.Run(kind, func(t *testing.T) {
			parent, child, auth := create(t)
			var update string
			switch kind {
			case "wire":
				update = `UPDATE loyal_yield.multiply_operations SET signed_wire='\x01'::bytea WHERE operation_id=$1`
			case "wire hash":
				update = `UPDATE loyal_yield.multiply_operations SET signed_wire_sha256=repeat('a',64) WHERE operation_id=$1`
			case "signature":
				update = `UPDATE loyal_yield.multiply_operations SET transaction_signature='recorded' WHERE operation_id=$1`
			case "signed":
				update = `UPDATE loyal_yield.multiply_operations SET status='signed' WHERE operation_id=$1`
			case "parent finality":
				if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET confirmation_status='confirmed' WHERE operation_id=$1`, parent.ID); err != nil {
					t.Fatal(err)
				}
			}
			if update != "" {
				if _, err := db.pool.Exec(ctx, update, child.ID); err != nil {
					t.Fatal(err)
				}
			}
			rpc := setupCompletionRPC(t, plan, parent, kind)
			if kind == "lease expiry" {
				if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET lease_expires_at=clock_timestamp()+interval '50 milliseconds' WHERE route_key=$1`, parent.RouteKey); err != nil {
					t.Fatal(err)
				}
				rpc = setupCompletionRPC(t, plan, parent, "refresh")
				base := rpc.client.Transport
				rpc.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					time.Sleep(20 * time.Millisecond)
					return base.RoundTrip(r)
				})
			}
			before := snapshot(t, parent.RouteKey)
			if _, err := db.refreshPolicySetupCompletion(ctx, rpc, child.ID, auth.IntentSHA256); err == nil {
				t.Fatal("unsafe funded refresh accepted")
			}
			if snapshot(t, parent.RouteKey) != before {
				t.Fatal("failed refresh changed prefund, spend or outstanding reservation")
			}
		})
	}
	parent, child, auth := create(t)
	var parentBefore string
	if err := db.pool.QueryRow(ctx, `SELECT to_jsonb(o)::text FROM loyal_yield.multiply_operations o WHERE operation_id=$1`, parent.ID).Scan(&parentBefore); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan DecisionRecord, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := db.refreshPolicySetupCompletion(ctx, setupCompletionRPC(t, plan, parent, "refresh"), child.ID, auth.IntentSHA256)
			results <- r
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for r := range results {
		if r.OperationID != child.ID || r.Status != Decided {
			t.Fatal("refresh replaced the deterministic creation operation")
		}
	}
	if _, err := db.ReleaseRouteLease(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AcquireRouteLease(ctx, parent.RouteKey, "funded-refresh-restart", time.Minute); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, parent.RouteKey)
	if _, err := db.refreshPolicySetupCompletion(ctx, setupCompletionRPC(t, plan, parent, "over cap"), child.ID, auth.IntentSHA256); err != nil {
		t.Fatal("retry attempted another revaluation", err)
	}
	if snapshot(t, parent.RouteKey) != before {
		t.Fatal("restart duplicate changed state")
	}
	var raw []byte
	var fresh phase3OperationAuthorization
	if err := db.pool.QueryRow(ctx, `SELECT expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, child.ID).Scan(&raw); err != nil || json.Unmarshal(raw, &fresh) != nil {
		t.Fatal(err)
	}
	if fresh.IntentSHA256 == auth.IntentSHA256 || fresh.PolicySetupCompletion.Request.RecentBlockhash == auth.PolicySetupCompletion.Request.RecentBlockhash || !reflect.DeepEqual(fresh.PolicySetupCompletion.Prefund, auth.PolicySetupCompletion.Prefund) || fresh.PolicySetupCompletion.Cost.SetupLamports != auth.PolicySetupCompletion.Cost.SetupLamports+100_000 || fresh.SetupBuildCost != nil || fresh.SendKnownCost != nil {
		t.Fatal("refresh changed finalized funding or retained old message authorization")
	}
	// An older prefund recovery call still resolves this refreshed child.
	if r, err := db.continuePolicySetupPrefund(ctx, setupCompletionRPC(t, plan, parent, "over cap"), parent.ID); err != nil || r.OperationID != child.ID {
		t.Fatal("parent replay lost refreshed continuation", err)
	}
	if _, err := db.refreshPolicySetupCompletion(ctx, setupCompletionRPC(t, plan, parent, "refresh again"), child.ID, fresh.IntentSHA256); err != nil {
		t.Fatal(err)
	}
	before = snapshot(t, parent.RouteKey)
	if _, err := db.refreshPolicySetupCompletion(ctx, setupCompletionRPC(t, plan, parent, "refresh"), child.ID, auth.IntentSHA256); err == nil {
		t.Fatal("older generation overwrote the latest completion")
	}
	if snapshot(t, parent.RouteKey) != before {
		t.Fatal("superseded refresh changed state")
	}
	var state struct {
		Budget  Phase3Budget `json:"phase3"`
		Pointer string       `json:"phase3SetupIntent"`
	}
	if err := db.pool.QueryRow(ctx, `SELECT state FROM loyal_yield.multiply_route_states WHERE route_key=$1`, parent.RouteKey).Scan(&raw); err != nil || json.Unmarshal(raw, &state) != nil {
		t.Fatal(err)
	}
	var parentAfter string
	var count int
	if err := db.pool.QueryRow(ctx, `SELECT to_jsonb(o)::text,(SELECT count(*) FROM loyal_yield.multiply_operations WHERE route_key=$2) FROM loyal_yield.multiply_operations o WHERE operation_id=$1`, parent.ID, parent.RouteKey).Scan(&parentAfter, &count); err != nil {
		t.Fatal(err)
	}
	if parentAfter != parentBefore || count != 2 || state.Pointer != parent.ID || state.Budget.Families["OnRe"].SpentMicros != 3_000_000+plan.Payments[0].TotalMicros || state.Budget.Families["OnRe"].ExitMicros != 0 || len(state.Budget.Reservations) != 1 {
		t.Fatal("refresh repeated prefund or reset/duplicated accounting")
	}
	r := state.Budget.Reservations[child.ID]
	if !r.Recovery || r.ExitBeforeMicros != Phase3TransactionCapMicros || r.UpperMicros <= auth.PolicySetupCompletion.Cost.TotalMicros {
		t.Fatal("remaining creation reservation not repriced")
	}
	t.Run("refreshed creation settles", func(t *testing.T) {
		testPolicySetupCreationSettlement(t, ctx, db, child.ID, parent.RouteKey, 3_000_000+plan.Payments[0].TotalMicros+r.UpperMicros)
	})
}

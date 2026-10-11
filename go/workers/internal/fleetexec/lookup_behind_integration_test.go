package fleetexec

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// behindBankNode answers a finalized blockhash at slot 1000 and refuses every
// account read as a node that has not reached the asked slot (-32016).
func behindBankNode(t *testing.T, reads *atomic.Int32) *chain.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any
			Method string
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		answer := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "getLatestBlockhash":
			answer["result"] = map[string]any{"context": map[string]any{"slot": 1000}, "value": map[string]any{"blockhash": sdk.Hash{7}.String(), "lastValidBlockHeight": 1150}}
		case "getMultipleAccounts":
			reads.Add(1)
			answer["error"] = map[string]any{"code": -32016, "message": "Minimum context slot has not been reached"}
		default:
			t.Errorf("unexpected %s", req.Method)
		}
		_ = json.NewEncoder(w).Encode(answer)
	}))
	t.Cleanup(server.Close)
	client, err := chain.New(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// A node behind the signing bank is not yet: the tick succeeds, and the
// operation waits for a later tick, its attempt uncounted and no error
// recorded.
func TestLookupWriterHandsBackAnOperationABehindNodeCannotRead(t *testing.T) {
	pool := lookupRegisteredPool(t)
	store, op := seedLookupSource(t, t.Context(), pool, readLookupFixture(t))
	if _, err := pool.Exec(t.Context(), `UPDATE loyal_yield.lookup_table_operations SET operation_state='queued',lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`, op.Intent.OperationID); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	noKey := func(context.Context, string) (ed25519.PrivateKey, error) {
		return nil, errors.New("signed before the bank was read")
	}
	worker, err := NewLookupWorker(store, behindBankNode(t, &reads), LookupWorkerConfig{Cluster: "localnet", Owner: "lookup-behind", LeaseTTL: time.Minute, TickDeadline: time.Second, PollInterval: time.Second, Budget: LookupBudget{MaximumLamports: 10000000, RollingWindow: time.Hour}, Facts: testFacts()}, noKey)
	if err != nil {
		t.Fatal(err)
	}
	row := func() (state string, attempts int, owner, detail *string, later bool) {
		t.Helper()
		if err := pool.QueryRow(t.Context(), `SELECT operation_state,attempt_count,lease_owner,error_detail,COALESCE(next_attempt_at>clock_timestamp(),false) FROM loyal_yield.lookup_table_operations WHERE id=$1`, op.Intent.OperationID).Scan(&state, &attempts, &owner, &detail, &later); err != nil {
			t.Fatal(err)
		}
		return
	}
	_, attempts, _, _, _ := row()
	if worked, err := worker.Tick(t.Context()); err != nil || !worked || reads.Load() == 0 {
		t.Fatalf("behind node failed the tick: worked=%v reads=%d %v", worked, reads.Load(), err)
	}
	if s, a, owner, detail, later := row(); s != "retry_wait" || a != attempts || owner != nil || detail != nil || !later {
		t.Fatalf("operation not handed back uncounted: state=%s attempts=%d/%d owner=%v detail=%v later=%v", s, a, attempts, owner, detail, later)
	}
}

// The planner's reads at the blockhash bank wait on a behind node: the tick
// reports healthy and a later tick reads again.
func TestLookupPlannerTickWaitsOnABehindNode(t *testing.T) {
	pool := lookupRegisteredPool(t)
	store, op := seedLookupSource(t, t.Context(), pool, readLookupFixture(t))
	// A verified retiring table is a cleanup candidate, read at the bank.
	for _, statement := range []string{
		`UPDATE loyal_yield.lookup_table_operations SET operation_state='cancelled',lease_owner=NULL,lease_expires_at=NULL WHERE id=$1`,
		`UPDATE loyal_yield.route_lookup_tables SET desired_state='retiring',last_verified_slot=1 WHERE id=(SELECT route_lookup_table_id FROM loyal_yield.lookup_table_operations WHERE id=$1)`,
	} {
		if _, err := pool.Exec(t.Context(), statement, op.Intent.OperationID); err != nil {
			t.Fatal(err)
		}
	}
	var reads atomic.Int32
	var reported []error
	planner, err := NewLookupPlanner(store, behindBankNode(t, &reads), LookupPlannerConfig{Cluster: "localnet", Owner: "planner-behind", LeaseTTL: time.Minute, TickDeadline: time.Second, PollInterval: time.Second, CatalogInterval: time.Minute, GrowthReservation: 8, MaximumVaultCohort: 16, Facts: testFacts(), OnHealth: func(err error) { reported = append(reported, err) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planner.Tick(t.Context()); err != nil || reads.Load() == 0 || len(reported) != 1 || reported[0] != nil {
		t.Fatalf("behind node failed the planner tick: reads=%d reported=%v %v", reads.Load(), reported, err)
	}
}

package backyardrwa

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// Synthetic RPC and unsigned wire only. Actual Worker.Run/Tick, lifecycle,
// pgx lease/journal/reservation methods; no signer, observer or new admission.
func TestBackyardFinancialHandoffFixture(t *testing.T) {
	if os.Getenv("BACKYARD_HANDOFF_ISOLATED_FIXTURE") != "1" {
		t.Skip("use isolated handoff runner")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	url := os.Getenv("BACKYARD_RWA_TEST_DATABASE_URL")
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name, host string
	if err = db.pool.QueryRow(ctx, "SELECT current_database(), coalesce(inet_server_addr()::text,'socket')").Scan(&name, &host); err != nil || name != "backyard_handoff_fixture" || host != "socket" {
		t.Fatalf("private fixture required: %v", err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg := DefaultConfig()
	cfg.PollInterval = time.Hour
	owner := func(n int) string {
		return fmt.Sprintf("deployment:backyard:production:fixture-restart-%d:sha-%s", n, strings.Repeat("f", 40))
	}
	for i, mode := range []string{"context-cancellation", "lost-rpc-response"} {
		t.Run(mode, func(t *testing.T) {
			collectedStart := time.Now().UTC()
			opID := fmt.Sprintf("synthetic-handoff-%d", i)
			_, err = db.pool.Exec(ctx, "DELETE FROM loyal_yield.multiply_operations")
			must(err)
			b := emptyTestBudget()
			b.Families["OnRe"] = FamilyBudget{ExitMicros: 4000000}
			state, _ := json.Marshal(map[string]any{"generation": 1, "phase3": b})
			_, err = db.pool.Exec(ctx, "UPDATE loyal_yield.multiply_route_states SET state=$1,state_version=1", state)
			must(err)
			_, err = db.AcquireRouteLease(ctx, productionRouteKey, owner(i*3), time.Minute)
			must(err)
			request := bridgeTestRequest(ReportNAV, 0)
			request.Report.ObservedSlot = 47
			request.Report.Sequence = 47
			request.LastValidBlockHeight = 100
			effects, _, _, err := bridgeExpectedEffects(Decision{Action: ReportNAV}, 0, 0, 0)
			must(err)
			encoded, err := jsonMarshalExpectedEffects(effects)
			must(err)
			envelope, _ := json.Marshal(map[string]any{"decision": decisionEvidence{Reason: "synthetic-handoff", StrategyKey: "OnRe/ONyc/USDC"}})
			_, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,cycle,engine_version,action,status,idempotency_key,strategy_key,expected_effects) VALUES($1,$2,1,'backyard_rwa_v1',$3,'decided',$1,'OnRe/ONyc/USDC',$4)`, opID, productionRouteKey, ReportNAV, envelope)
			must(err)
			digest, err := Phase3IntentDigest(request, encoded)
			must(err)
			r := testReservation()
			r.OperationID = opID
			r.IntentSHA256 = digest
			r.Recovery = true
			must(db.ReservePhase3(ctx, r))
			must(db.AuthorizePhase3Build(ctx, opID, request, encoded))
			message, err := CompileBridgeMessage(request)
			must(err)
			wire := append(make([]byte, 65), message...)
			wire[0] = 1
			wire[1] = byte(i + 1) // unsigned fixture, not chain-valid
			_, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',signed_wire=$2,signed_wire_sha256=$3,transaction_signature=$4,recent_blockhash=$5,last_valid_block_height=100 WHERE operation_id=$1`, opID, wire, sha256Bytes(wire), encodeBase58(wire[1:65]), request.RecentBlockhash)
			must(err)
			tx, err := db.pool.Begin(ctx)
			must(err)
			must(db.bindPhase3WireTx(ctx, tx, opID, sha256Bytes(wire)))
			must(tx.Commit(ctx))
			old, err := db.currentLease()
			must(err)
			_, err = db.ReleaseRouteLease(ctx)
			must(err)
			entered := make(chan struct{})
			runCtx, stop := context.WithCancel(ctx)
			rpc := budgetBuildRPC(t, 5000, 42)
			base := rpc.client.Transport
			sends := 0
			rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				raw, _ := io.ReadAll(req.Body)
				req.Body = io.NopCloser(strings.NewReader(string(raw)))
				var call struct {
					Method string            `json:"method"`
					Params []json.RawMessage `json:"params"`
				}
				must(json.Unmarshal(raw, &call))
				switch call.Method {
				case "sendTransaction":
					sends++
					var sent string
					must(json.Unmarshal(call.Params[0], &sent))
					actual, err := base64.StdEncoding.DecodeString(sent)
					must(err)
					if string(actual) != string(wire) {
						t.Error("transport did not receive exact persisted wire")
					}
					var status string
					var intent bool
					must(db.pool.QueryRow(ctx, "SELECT status,broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_operations WHERE operation_id=$1", opID).Scan(&status, &intent))
					if status != "broadcast_intent" || !intent {
						t.Error("send preceded durable intent")
					}
					close(entered)
					if mode == "context-cancellation" {
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					return nil, io.ErrUnexpectedEOF
				case "getSignatureStatuses":
					return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":42},"value":[null]}}`), nil
				case "getBlockHeight":
					return response(`{"jsonrpc":"2.0","id":1,"result":42}`), nil
				}
				return base.RoundTrip(req)
			})
			worker := func(d *Database) *Worker {
				return &Worker{routeKey: productionRouteKey, interval: time.Hour, wake: make(chan struct{}, 1), runtime: tickRuntime{loadNonterminal: d.LoadNonterminal, advance: func(c context.Context, o PersistedOperation) error { return AdvanceNonterminal(c, d, rpc, o) }}}
			}
			done := make(chan error, 1)
			go func() { done <- worker(db).Run(runCtx, db, owner(i*3+1), cfg) }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("send not reached")
			}
			old, err = db.currentLease()
			must(err)
			start := time.Now()
			stop()
			select {
			case err = <-done:
				if err == nil {
					t.Fatal("ambiguous send should stop")
				}
			case <-ctx.Done():
				t.Fatal("shutdown exceeded bound")
			}
			stoppedMS := time.Since(start).Milliseconds()
			// Fresh DB connection/runtime and fresh deployment identity simulate restart;
			// this is not an OS process kill/reboot test.
			successor, err := OpenDatabase(ctx, url)
			must(err)
			defer successor.Close()
			lease, err := successor.AcquireRouteLease(ctx, productionRouteKey, owner(i*3+2), time.Minute)
			must(err)
			snapshot := func() string {
				var raw string
				must(successor.pool.QueryRow(ctx, `SELECT jsonb_build_object('status',o.status,'wire_sha256',o.signed_wire_sha256,'wire_bytes',octet_length(o.signed_wire),'broadcast_intent',o.broadcast_intent_at IS NOT NULL,'budget',r.state->'phase3','generation',r.state_version)::text FROM loyal_yield.multiply_operations o JOIN loyal_yield.multiply_route_states r USING(route_key) WHERE operation_id=$1`, opID).Scan(&raw))
				return raw
			}
			before := snapshot()
			var budgetRaw []byte
			must(successor.pool.QueryRow(ctx, "SELECT state->'phase3' FROM loyal_yield.multiply_route_states WHERE route_key=$1", productionRouteKey).Scan(&budgetRaw))
			var retained Phase3Budget
			must(json.Unmarshal(budgetRaw, &retained))
			if len(retained.Reservations) != 1 || retained.Reservations[opID].UpperMicros != r.UpperMicros || retained.Reservations[opID].IntentSHA256 != digest || retained.Families["OnRe"].SpentMicros != 0 {
				t.Fatal("unknown outcome released or booked reservation")
			}

			must(worker(successor).Tick(ctx))
			must(worker(successor).Tick(ctx))
			if after := snapshot(); after != before {
				t.Fatalf("unknown outcome changed retained state: %s -> %s", before, after)
			}
			loaded, err := successor.LoadNonterminal(ctx, productionRouteKey)
			must(err)
			if loaded == nil || string(loaded.SignedWire) != string(wire) || loaded.Status != BroadcastIntent || sends != 1 {
				t.Fatalf("wire/recovery mismatch: sends=%d", sends)
			}
			db.setLease(&old)
			if err = db.MarkSubmitted(ctx, opID); !errors.Is(err, ErrRouteLeaseLost) {
				t.Fatalf("returning predecessor write accepted: %v", err)
			}
			db.setLease(&old)
			released, err := db.ReleaseRouteLease(ctx)
			must(err)
			if released {
				t.Fatal("predecessor released successor")
			}
			must(successor.AssertRouteLease(ctx, productionRouteKey))
			t.Logf("RAW synthetic=true collection_start=%s collection_end=%s case=%s shutdown_ms=%d sends=%d predecessor_token=%d successor_token=%d retained=%s", collectedStart.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), mode, stoppedMS, sends, old.FencingToken, lease.FencingToken, before)
			_, err = successor.ReleaseRouteLease(ctx)
			must(err)
		})
	}
}

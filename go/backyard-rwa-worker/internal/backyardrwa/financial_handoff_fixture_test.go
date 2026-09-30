package backyardrwa

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
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
	url := isolatedHandoffDSN(t)
	db, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name, host string
	if err = db.pool.QueryRow(ctx, "SELECT current_database(), coalesce(inet_server_addr()::text,'socket')").Scan(&name, &host); err != nil || name != "backyard_handoff_fixture" || host != "socket" {
		t.Fatalf("private fixture required: %v", err)
	}
	cfg := DefaultConfig()
	cfg.PollInterval = time.Hour
	owner := func(n int) string {
		return fmt.Sprintf("deployment:backyard:production:fixture-restart-%d:sha-%s", n, strings.Repeat("f", 40))
	}
	for i, mode := range []string{"context-cancellation", "lost-rpc-response", "os-sigterm"} {
		t.Run(mode, func(t *testing.T) {
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}

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
			_, decoded, _, _, err := decodeExactLegacyWire(wire)
			must(err)
			if string(decoded) != string(message) {
				t.Fatal("synthetic serialization mismatch")
			}
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
			defer stop()
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
			stoppedMS := int64(0)
			if mode == "os-sigterm" {
				executable, err := os.Executable()
				must(err)
				command := exec.CommandContext(ctx, executable, "-test.run=^TestBackyardSIGTERMHelper$", "-test.v", "-test.timeout=15s")
				command.Env = []string{"BACKYARD_HANDOFF_ISOLATED_FIXTURE=1", "BACKYARD_RWA_TEST_DATABASE_URL=" + url, "BACKYARD_HANDOFF_CHILD=1", "BACKYARD_HANDOFF_OWNER=" + owner(i*3+1)}
				pipe, err := command.StdoutPipe()
				must(err)
				command.Stderr = os.Stderr
				must(command.Start())
				waited := false
				defer func() {
					if !waited {
						_ = command.Process.Kill()
						_ = command.Wait()
					}
				}()
				ready := make(chan string, 1)
				drained := make(chan handoffChildOutput, 1)
				go func() { drained <- drainHandoffChild(pipe, ready) }()
				logChild := func(output handoffChildOutput) {
					t.Logf("CHILD synthetic=true stdout_tail=%q scanner_error=%v", output.tail, output.err)
				}
				select {
				case raw := <-ready:
					must(json.Unmarshal([]byte(raw), &old))
				case output := <-drained:
					logChild(output)
					exitErr := command.Wait()
					waited = true
					t.Fatalf("child exited before blocked send: %v", exitErr)
				case <-ctx.Done():
					_ = command.Process.Kill()
					logChild(<-drained)
					_ = command.Wait()
					waited = true
					t.Fatal("child send not reached")
				}
				start := time.Now()
				must(command.Process.Signal(syscall.SIGTERM))
				// Drain to EOF before Wait closes StdoutPipe; retain failures after readiness too.
				output := <-drained
				logChild(output)
				exitErr := command.Wait()
				waited = true
				var expectedExit *exec.ExitError
				if output.err != nil || !errors.As(exitErr, &expectedExit) || expectedExit.ExitCode() != 1 {
					t.Fatalf("expected synthetic SIGTERM exit 1: %v", exitErr)
				}
				records := 0
				for _, line := range strings.Split(output.tail, "\n") {
					if line == handoffExpectedSIGTERMExit {
						records++
					}
				}
				if records != 1 || strings.Contains(output.tail, "--- FAIL:") || strings.Contains(output.tail, "panic:") {
					t.Fatal("missing or contradictory expected SIGTERM exit record")
				}
				t.Log("CHILD_RESULT synthetic=true process=worker_store_helper exit_code=1 expected_record_verified=true production_entrypoint_tested=false")
				stoppedMS = time.Since(start).Milliseconds()
				if stoppedMS > 5000 {
					t.Fatal("SIGTERM exit exceeded five seconds")
				}
				sends = 1
			} else {
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
				stoppedMS = time.Since(start).Milliseconds()
			}
			// Fresh DB connection/runtime and fresh deployment identity simulate restart;
			// SIGTERM case reaps a real child; successor remains a fresh in-process runtime.
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

func isolatedHandoffDSN(t *testing.T) string {
	t.Helper()
	if os.Getenv("BACKYARD_HANDOFF_ISOLATED_FIXTURE") != "1" {
		t.Fatal("isolated fixture required")
	}
	raw := os.Getenv("BACKYARD_RWA_TEST_DATABASE_URL")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "postgresql" || u.Host != "" || u.User != nil || u.Path != "/backyard_handoff_fixture" || len(u.Query()) != 1 {
		t.Fatal("private fixture DSN required")
	}
	socket := u.Query().Get("host")
	if !filepath.IsAbs(socket) || filepath.Base(socket) != "socket" || !strings.HasPrefix(filepath.Dir(socket), "/tmp/backyard-handoff-") || filepath.Clean(socket) != socket {
		t.Fatal("private runner socket required")
	}
	return raw
}

func TestBackyardSIGTERMHelper(t *testing.T) {
	if os.Getenv("BACKYARD_HANDOFF_CHILD") != "1" {
		t.Skip("subprocess only")
	}
	dsn := isolatedHandoffDSN(t)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	db, err := OpenDatabase(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var name, host string
	if err := db.pool.QueryRow(ctx, "SELECT current_database(), coalesce(inet_server_addr()::text,'socket')").Scan(&name, &host); err != nil || name != "backyard_handoff_fixture" || host != "socket" {
		t.Fatal("private database required")
	}
	// Worker.Run owns acquisition; LoadNonterminal must remain behind that fence.
	var op *PersistedOperation
	rpc := budgetBuildRPC(t, 5000, 42)
	base := rpc.client.Transport
	sends := 0
	requestCanceled := false
	rpc.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(strings.NewReader(string(raw)))
		var call struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(raw, &call); err != nil {
			return nil, err
		}
		switch call.Method {
		case "getSignatureStatuses":
			return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":42},"value":[null]}}`), nil
		case "getBlockHeight":
			return response(`{"jsonrpc":"2.0","id":1,"result":42}`), nil
		case "sendTransaction":
		default:
			return base.RoundTrip(req)
		}
		sends++
		var encoded string
		if len(call.Params) == 0 {
			t.Fatal("missing wire")
		}
		if err := json.Unmarshal(call.Params[0], &encoded); err != nil {
			t.Fatal(err)
		}
		actual, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || op == nil || string(actual) != string(op.SignedWire) || sends != 1 {
			t.Fatal("wire/send mismatch")
		}
		var status string
		var intent bool
		if err := db.pool.QueryRow(ctx, "SELECT status,broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_operations WHERE operation_id=$1", op.ID).Scan(&status, &intent); err != nil || status != "broadcast_intent" || !intent {
			t.Fatal("send preceded durable intent")
		}
		lease, err := db.currentLease()
		if err != nil {
			t.Fatal(err)
		}
		ready, _ := json.Marshal(lease)
		fmt.Printf("BLOCKED_SEND %s\n", ready)
		<-req.Context().Done()
		requestCanceled = req.Context().Err() == context.Canceled
		return nil, req.Context().Err()
	})
	w := &Worker{routeKey: productionRouteKey, interval: time.Hour, wake: make(chan struct{}, 1), runtime: tickRuntime{loadNonterminal: func(c context.Context, route string) (*PersistedOperation, error) {
		loaded, err := db.LoadNonterminal(c, route)
		op = loaded
		return loaded, err
	}, advance: func(c context.Context, o PersistedOperation) error { return AdvanceNonterminal(c, db, rpc, o) }}}
	err = w.Run(ctx, db, os.Getenv("BACKYARD_HANDOFF_OWNER"), DefaultConfig())
	var rpcFailure *rpcError
	if err == nil || !requestCanceled || ctx.Err() != context.Canceled || sends != 1 ||
		!errors.As(err, &rpcFailure) || rpcFailure.stage != rpcStageTransport ||
		rpcFailure.method != "sendTransaction" || rpcFailure.class != rpcClassContextCanceled ||
		err.Error() != "ambiguous send after durable broadcast intent: RPC sendTransaction request failed: stage=transport class=context_canceled" ||
		errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected signal shutdown: %v sends=%d request_canceled=%t outer_canceled=%t", err, sends, requestCanceled, ctx.Err() == context.Canceled)
	}
	// Worker.Run has completed its deferred lease release. Production Run
	// propagates this sanitized transport error and main log.Fatal exits 1.
	// This helper exercises real Worker/Store, not the production entrypoint.
	fmt.Println(handoffExpectedSIGTERMExit)
	os.Exit(1)
}

// Proves the fixture payload parses as the exact single-signature Solana wire;
// the placeholder signature is deterministic and deliberately not chain-valid.
func TestBackyardSyntheticHandoffWire(t *testing.T) {
	request := bridgeTestRequest(ReportNAV, 0)
	request.Report.ObservedSlot = 47
	request.Report.Sequence = 47
	request.LastValidBlockHeight = 100
	message, err := CompileBridgeMessage(request)
	if err != nil {
		t.Fatal(err)
	}
	wire := append(make([]byte, 65), message...)
	wire[0], wire[1] = 1, 3
	signature, decoded, _, _, err := decodeExactLegacyWire(wire)
	if err != nil || string(decoded) != string(message) || len(signature) != 64 || signature[0] != 3 {
		t.Fatalf("synthetic wire serialization failed: %v", err)
	}
}

// Child environment contains only fixed fixture bindings. Keep a bounded tail
// and continue draining after readiness so startup/test failures reach run.py.
type handoffChildOutput struct {
	tail string
	err  error
}

func drainHandoffChild(reader io.Reader, ready chan<- string) handoffChildOutput {
	scanner := bufio.NewScanner(reader)
	output := handoffChildOutput{}
	announced := false
	for scanner.Scan() {
		line := scanner.Text()
		output.tail += line + "\n"
		if len(output.tail) > 16384 {
			output.tail = output.tail[len(output.tail)-16384:]
		}
		if !announced && strings.HasPrefix(line, "BLOCKED_SEND ") {
			ready <- strings.TrimPrefix(line, "BLOCKED_SEND ")
			announced = true
		}
	}
	output.err = scanner.Err()
	return output
}

func TestBackyardChildDiagnostics(t *testing.T) {
	ready := make(chan string, 1)
	failed := drainHandoffChild(strings.NewReader("=== RUN child\nmissing route lease\n--- FAIL: child\n"), ready)
	if failed.err != nil || !strings.Contains(failed.tail, "missing route lease") || len(ready) != 0 {
		t.Fatal("startup diagnostics lost")
	}
	output := drainHandoffChild(strings.NewReader("BLOCKED_SEND {}\n"+strings.Repeat("x\n", 10000)+"--- FAIL: after readiness\n"), ready)
	if output.err != nil || len(output.tail) > 16384 || !strings.Contains(output.tail, "--- FAIL: after readiness") || <-ready != "{}" {
		t.Fatal("readiness/drain diagnostics lost")
	}
}

const handoffExpectedSIGTERMExit = "EXPECTED_SIGTERM_EXIT synthetic=true process=worker_store_helper stage=transport class=context_canceled request_canceled=true outer_canceled=true sends=1 worker_error=true exit_code=1"

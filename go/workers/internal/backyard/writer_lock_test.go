package backyard

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

// The writer is one session. It takes Backyard's one-writer lock when it
// opens; a second writer waits for it; a one-shot operator write refuses
// beside it. Once the first session ends, the second takes the lock, and the
// first can neither write, nor reconnect, nor send a signed row: the worker
// stops on its next read.
func TestWriterIsOneSession(t *testing.T) {
	ctx, cancel, admin, url := openManualRecoveryTestDatabase(t, 60*time.Second)
	defer cancel()
	defer admin.Close()
	if _, err := admin.pool.Exec(ctx, `ALTER TABLE loyal_yield.multiply_operations
		ADD COLUMN IF NOT EXISTS message_sha256 text, ADD COLUMN IF NOT EXISTS signed_wire_sha256 text,
		ADD COLUMN IF NOT EXISTS recent_blockhash text, ADD COLUMN IF NOT EXISTS last_valid_block_height bigint,
		ADD COLUMN IF NOT EXISTS simulation_slot bigint, ADD COLUMN IF NOT EXISTS simulation_result jsonb,
		ADD COLUMN IF NOT EXISTS confirmation_status text`); err != nil {
		t.Fatal(err)
	}
	resetManualRecoveryProductionRoute(t, ctx, admin, "writer-session")
	lockSessions := func(granted bool) []int32 {
		t.Helper()
		rows, err := admin.pool.Query(ctx, `SELECT pid FROM pg_locks WHERE locktype = 'advisory' AND granted = $1
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
			AND (classid::bigint << 32 | objid::bigint) = hashtextextended('loyal-family:backyard', 0) ORDER BY pid`, granted)
		if err != nil {
			t.Fatal(err)
		}
		var pids []int32
		for rows.Next() {
			var pid int32
			if err := rows.Scan(&pid); err != nil {
				t.Fatal(err)
			}
			pids = append(pids, pid)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return pids
	}
	backendPID := func(d *Database) int32 {
		t.Helper()
		var pid int32
		if err := d.pool.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		return pid
	}

	// (1) The writer's session holds the lock.
	first, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	firstPID := backendPID(first)
	if held := lockSessions(true); len(held) != 1 || held[0] != firstPID {
		t.Fatalf("lock holders %v, want the writer session %d", held, firstPID)
	}
	if _, err := holdWriterLock(ctx, url); !errors.Is(err, engine.ErrFamilyHeld) {
		t.Fatalf("an operator write took the lock beside the writer: %v", err)
	}

	// (2) A second writer waits for it.
	type opened struct {
		database *Database
		err      error
	}
	secondOpened := make(chan opened, 1)
	go func() {
		database, err := OpenDatabase(ctx, url)
		secondOpened <- opened{database, err}
	}()
	for deadline := time.Now().Add(10 * time.Second); len(lockSessions(false)) == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the second writer never waited on the lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case result := <-secondOpened:
		t.Fatalf("the second writer opened beside the first: %v", result.err)
	case <-time.After(500 * time.Millisecond):
	}

	// A decided operation and the exact wire a tick would persist for it.
	manifest := readyWorkerManifest(t)
	observation := tickObservation(Snapshot{ObservationID: "writer-session", Slot: 10, RouteKind: RouteKind, Fresh: true, VoltrIdleRaw: 7})
	decision := Decide(observation.Snapshot)
	if decision.Action != VoltrAllocateToSquads {
		t.Fatalf("fixture is not actionable: %+v", decision)
	}
	record, err := first.RecordDecisionOnManifest(ctx, manifest, productionRouteKey, observation, decision, manifest.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	request := bridgeTestRequest(VoltrAllocateToSquads, uint64(decision.AmountRaw))
	effects, _, _, err := bridgeExpectedEffects(decision, 7, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	encodedEffects, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{43}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	message, err := compileBridgeMessageForDelegate(request, delegate)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := signMessage(key, delegate, message, request.RecentBlockhash, request.LastValidBlockHeight)
	if err != nil {
		t.Fatal(err)
	}
	row := func() (status string, wire []byte, intent bool) {
		t.Helper()
		if err := admin.pool.QueryRow(ctx, `SELECT status, signed_wire, broadcast_intent_at IS NOT NULL FROM loyal_yield.multiply_operations WHERE operation_id = $1`,
			record.OperationID).Scan(&status, &wire, &intent); err != nil {
			t.Fatal(err)
		}
		return status, wire, intent
	}

	// (3) The first session ends; the second writer takes the lock.
	var terminated bool
	if err := admin.pool.QueryRow(ctx, `SELECT pg_terminate_backend($1)`, firstPID).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("terminate the first writer session: %v", err)
	}
	var second *Database
	select {
	case result := <-secondOpened:
		if result.err != nil {
			t.Fatal(result.err)
		}
		second = result.database
	case <-time.After(15 * time.Second):
		t.Fatal("the second writer never took the released lock")
	}
	defer second.Close()
	secondPID := backendPID(second)
	if held := lockSessions(true); len(held) != 1 || held[0] != secondPID {
		t.Fatalf("lock holders %v, want the second writer session %d", held, secondPID)
	}

	// (4) The first writer's write fails, and so does every later one: its
	// pool does not open another session.
	if err := first.PersistSigned(ctx, record.OperationID, signed, SimulationResult{Slot: 11}, encodedEffects); err == nil {
		t.Fatal("a writer without its session persisted a signed wire")
	}
	if err := first.PersistSigned(ctx, record.OperationID, signed, SimulationResult{Slot: 11}, encodedEffects); !errors.Is(err, errWriterSessionLost) {
		t.Fatalf("the lost writer reconnected or failed otherwise: %v", err)
	}
	if conns := first.pool.Stat().TotalConns(); conns != 0 {
		t.Fatalf("the lost writer holds %d connections", conns)
	}
	if status, wire, _ := row(); status != string(Decided) || len(wire) != 0 {
		t.Fatalf("the lost writer changed the row: status=%s", status)
	}
	if held := lockSessions(true); len(held) != 1 || held[0] != secondPID {
		t.Fatalf("lock holders %v after the lost writer's writes", held)
	}

	// (5) The second writer writes.
	if err := second.PersistSigned(ctx, record.OperationID, signed, SimulationResult{Slot: 11}, encodedEffects); err != nil {
		t.Fatal(err)
	}
	if status, wire, intent := row(); status != string(Signed) || !bytes.Equal(wire, signed.signedWire) || intent {
		t.Fatalf("the second writer's signed row: status=%s intent=%v", status, intent)
	}

	// (6) The first writer's land on that signed row sends nothing: broadcast
	// intent is written before a first send, and it cannot write.
	operation, err := second.LoadNonterminal(ctx, productionRouteKey)
	if err != nil || operation == nil || operation.Status != Signed {
		t.Fatalf("load the signed row: %+v %v", operation, err)
	}
	sends := 0
	rpc := newFakeChain(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), `"method":"sendTransaction"`):
			sends++
			return response(`{"jsonrpc":"2.0","id":1,"result":"` + encodeBase58(wireSig(signed.signedWire)) + `"}`), nil
		case strings.Contains(string(body), `"method":"getEpochInfo"`):
			return response(finalizedEpochJSON(5)), nil
		case strings.Contains(string(body), `"method":"getSignatureStatuses"`):
			return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":5000},"value":[null]}}`), nil
		}
		t.Errorf("unexpected RPC: %s", body)
		return nil, fmt.Errorf("unexpected RPC")
	}))
	if err := first.land(ctx, rpc, *operation); !errors.Is(err, errWriterSessionLost) {
		t.Fatalf("the lost writer's land: %v", err)
	}
	if sends != 0 {
		t.Fatalf("the lost writer sent the signed wire %d times", sends)
	}
	if status, _, intent := row(); status != string(Signed) || intent {
		t.Fatalf("the lost writer moved the row: status=%s intent=%v", status, intent)
	}

	// The lost writer's worker stops on its next tick instead of running on.
	worker := &Worker{routeKey: productionRouteKey, interval: time.Millisecond, manifest: manifest, runtime: tickRuntime{
		loadLatch:       first.ManualRecoveryLatch,
		loadNonterminal: first.LoadNonterminal,
	}}
	if err := worker.Run(ctx); !errors.Is(err, errWriterSessionLost) {
		t.Fatalf("the lost writer's worker did not stop: %v", err)
	}
}

// A caller's deadline cancels its statement and keeps the writer session,
// and the lock with it: a timeout is not a lost session.
func TestWriterSessionSurvivesCallerDeadlines(t *testing.T) {
	ctx, cancel, admin, url := openManualRecoveryTestDatabase(t, 30*time.Second)
	defer cancel()
	defer admin.Close()
	writer, err := OpenDatabase(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	var pid int32
	if err := writer.pool.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	sameSession := func(step string) {
		t.Helper()
		var now int32
		if err := writer.pool.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&now); err != nil || now != pid {
			t.Fatalf("%s: writer session %d, want %d: %v", step, now, pid, err)
		}
		var held bool
		if err := admin.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND granted AND pid = $1
			AND (classid::bigint << 32 | objid::bigint) = hashtextextended('loyal-family:backyard', 0))`, pid).Scan(&held); err != nil || !held {
			t.Fatalf("%s: the writer session lost the lock: %v", step, err)
		}
	}

	// A deadline during a statement.
	short, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	_, err = writer.pool.Exec(short, `SELECT pg_sleep(2)`)
	stop()
	if err == nil {
		t.Fatal("the statement outlived its deadline")
	}
	sameSession("statement deadline")

	// A deadline between two statements of a transaction, and one during a
	// statement in it.
	for _, between := range []bool{true, false} {
		short, stop := context.WithTimeout(ctx, 200*time.Millisecond)
		err = writer.inTx(short, func(tx pgx.Tx) error {
			if _, err := tx.Exec(short, `SELECT 1`); err != nil {
				return err
			}
			if between {
				<-short.Done()
				_, err := tx.Exec(short, `SELECT 1`)
				return err
			}
			_, err := tx.Exec(short, `SELECT pg_sleep(2)`)
			return err
		})
		stop()
		if err == nil {
			t.Fatal("the transaction outlived its deadline")
		}
		sameSession(fmt.Sprintf("transaction deadline (between statements %v)", between))
	}

	// A deadline that ends the body after its last statement rolls back.
	short, stop = context.WithTimeout(ctx, 200*time.Millisecond)
	err = writer.inTx(short, func(tx pgx.Tx) error {
		_, err := tx.Exec(short, `SELECT 1`)
		<-short.Done()
		return err
	})
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a transaction past its deadline committed: %v", err)
	}
	sameSession("deadline before commit")
}

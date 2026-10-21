package backyard

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The tick persists its signed wire before anything sends it. A process that
// dies after that write and before the send leaves a signed row; the next
// process lands exactly those bytes: broadcast intent first, then the same
// wire until it confirms. Nothing is rebuilt or re-signed.
func TestCrashBetweenPersistAndSendLandsThePersistedWire(t *testing.T) {
	ctx, cancel, db, url := openManualRecoveryTestDatabase(t, 60*time.Second)
	defer cancel()
	defer db.Close()
	if _, err := db.pool.Exec(ctx, `ALTER TABLE loyal_yield.multiply_operations
		ADD COLUMN IF NOT EXISTS message_sha256 text, ADD COLUMN IF NOT EXISTS signed_wire_sha256 text,
		ADD COLUMN IF NOT EXISTS recent_blockhash text, ADD COLUMN IF NOT EXISTS last_valid_block_height bigint,
		ADD COLUMN IF NOT EXISTS simulation_slot bigint, ADD COLUMN IF NOT EXISTS simulation_result jsonb,
		ADD COLUMN IF NOT EXISTS confirmation_status text`); err != nil {
		t.Fatal(err)
	}
	resetManualRecoveryProductionRoute(t, ctx, db, "write-ahead-crash")

	observation := tickObservation(Snapshot{ObservationID: "write-ahead", Slot: 10, RouteKind: RouteKind, Fresh: true, VoltrIdleRaw: 7})
	decision := Decide(observation.Snapshot)
	if decision.Action != VoltrAllocateToSquads {
		t.Fatalf("fixture is not actionable: %+v", decision)
	}
	request := bridgeTestRequest(VoltrAllocateToSquads, uint64(decision.AmountRaw))
	effects, _, _, _, err := bridgeExpectedEffects(rwa, decision, 7, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	encodedEffects, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		t.Fatal(err)
	}
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, ed25519.SeedSize))
	delegate := publicKeyFromBytes(key.Public().(ed25519.PublicKey))
	var persisted []byte
	manifest := readyWorkerManifest(t)
	first := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
		loadNonterminal: db.LoadNonterminal,
		observe:         func(context.Context) (Observation, error) { return observation, nil },
		prepareBridge: func(context.Context, RouteManifest, Decision, Observation) (Observation, BridgeExecutionEvidence, error) {
			return observation, BridgeExecutionEvidence{Request: request, ExpectedEffects: effects}, nil
		},
		recordDecision: func(ctx context.Context, routeKey string, o Observation, d Decision, sha string) (DecisionRecord, error) {
			return db.RecordDecisionOnManifest(ctx, manifest, routeKey, o, d, sha)
		},
		// The production build with a test signer: compile, sign, simulate
		// (here: a fixed slot), persist the exact wire.
		build: func(ctx context.Context, id string, r any, e ExpectedEffects) error {
			message, err := compileBridgeMessageForDelegate(rwa, r.(BridgeBuildRequest), delegate)
			if err != nil {
				return err
			}
			signed, err := signMessage(key, delegate, message, request.RecentBlockhash, request.LastValidBlockHeight)
			if err != nil {
				return err
			}
			persisted = signed.signedWire
			return db.PersistSigned(ctx, id, signed, SimulationResult{Slot: 11}, encodedEffects)
		},
		// The process dies before its first send.
		advance: func(context.Context, PersistedOperation) error {
			return errors.New("process killed before the send")
		},
	}}
	if err := first.Tick(ctx); err == nil || !strings.Contains(err.Error(), "process killed") {
		t.Fatalf("the crash did not happen after the persist: %v", err)
	}
	if len(persisted) == 0 {
		t.Fatal("the build persisted nothing")
	}
	var status string
	var intent bool
	var wire []byte
	if err := db.pool.QueryRow(ctx, `SELECT status, broadcast_intent_at IS NOT NULL, signed_wire FROM loyal_yield.multiply_operations WHERE route_key=$1`, productionRouteKey).Scan(&status, &intent, &wire); err != nil {
		t.Fatal(err)
	}
	if status != "signed" || intent || !bytes.Equal(wire, persisted) {
		t.Fatalf("the persisted row is not the signed, unsent wire: status=%s intent=%v", status, intent)
	}

	// A new process: its own database handle, the production advance.
	restarted, err := OpenDatabase(ctx, engine.DirectDSN(url))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	var sends [][]byte
	signature := encodeBase58(wireSig(persisted))
	rpc := newFakeChain(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), `"method":"sendTransaction"`):
			var sent struct {
				Params []any `json:"params"`
			}
			if err := json.Unmarshal(body, &sent); err != nil || len(sent.Params) == 0 {
				t.Errorf("send body: %s", body)
			} else if raw, err := base64.StdEncoding.DecodeString(fmt.Sprint(sent.Params[0])); err == nil {
				sends = append(sends, raw)
			}
			return response(`{"jsonrpc":"2.0","id":1,"result":"` + signature + `"}`), nil
		case strings.Contains(string(body), `"method":"getEpochInfo"`):
			return response(finalizedEpochJSON(5)), nil
		case strings.Contains(string(body), `"method":"getSignatureStatuses"`):
			status := "null"
			if len(sends) > 0 {
				status = `{"slot":45,"confirmations":1,"err":null,"confirmationStatus":"confirmed"}`
			}
			return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":5000},"value":[` + status + `]}}`), nil
		}
		t.Errorf("unexpected RPC while recovering: %s", body)
		return nil, fmt.Errorf("unexpected RPC")
	}))
	second := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
		loadNonterminal: restarted.LoadNonterminal,
		advance: func(ctx context.Context, op PersistedOperation) error {
			return advanceNonterminalWithManifest(ctx, manifest, restarted, rpc, op)
		},
		observe: func(context.Context) (Observation, error) {
			t.Fatal("recovery observed before landing the persisted wire")
			return Observation{}, nil
		},
	}}
	if err := second.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(sends) != 1 || !bytes.Equal(sends[0], persisted) {
		t.Fatalf("recovery did not send exactly the persisted wire: %d sends", len(sends))
	}
	var slot int64
	if err := restarted.pool.QueryRow(ctx, `SELECT status, broadcast_intent_at IS NOT NULL, confirmed_slot FROM loyal_yield.multiply_operations WHERE route_key=$1`, productionRouteKey).Scan(&status, &intent, &slot); err != nil {
		t.Fatal(err)
	}
	if status != "confirmed" || !intent || slot != 45 {
		t.Fatalf("recovered wire did not land: status=%s intent=%v slot=%d", status, intent, slot)
	}
}

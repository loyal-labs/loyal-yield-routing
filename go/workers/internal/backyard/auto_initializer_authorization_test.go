package backyard

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

// autoInitializerAuthorizationFixture recompiles the shared candidate fixture
// at an observed rent. The reviewed binding pins the policy seed and account
// digest, never the synthetically observed rent: the value below is an
// ordinary chain observable (per-byte rent times the exact obligation size),
// and the rent sysvar served by the transport serves it exactly.
func autoInitializerAuthorizationFixture(t *testing.T) autoInitializerRecoveryFixture {
	t.Helper()
	f := newAutoInitializerRecoveryFixture(t)
	f.request.RentLamports = 289 * (kamino.ObligationSize + 128)
	var err error
	f.effects.Initialization = &f.request
	f.raw, err = jsonMarshalExpectedEffects(f.effects)
	if err != nil {
		t.Fatal(err)
	}
	f.message, err = f.manifest.compileKaminoInitializationMessage(f.request)
	if err != nil {
		t.Fatal(err)
	}
	f.wire = signedWire(f.message, bytes.Repeat([]byte{9}, 64))
	return f
}

// autoInitializerAuthorizationRPC composes the shared budget-build transport
// (slot, message-fee, native-price and rent-exemption reads) with the shared
// candidate AUTO prestate account set from autoInitializerPrestateAccounts,
// which fills the account reads with the target obligation absent.
// The rent sysvar prices the fixture request's exact rent. sendTransaction is
// counted and refused, and the signature lands once it was attempted;
// simulateTransaction is refused outright: no signer exists in these tests.
func autoInitializerAuthorizationRPC(t *testing.T, f autoInitializerRecoveryFixture) (*chain.Client, *int) {
	t.Helper()
	accounts := autoInitializerPrestateAccounts(t, f.request)
	const rentAddress = "SysvarRent111111111111111111111111111111111"
	rent := accounts[rentAddress]
	binary.LittleEndian.PutUint64(rent.Data, f.request.RentLamports/(kamino.ObligationSize+128))
	accounts[rentAddress] = rent
	rpc := budgetBuildRPC(t, 5000, 42)
	base := fillAccounts(rpcOf(rpc).Transport, accounts)
	sends := 0
	rpcOf(rpc).Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct {
			Method string
			Params []json.RawMessage
			ID     any
		}
		if err = json.Unmarshal(raw, &body); err != nil {
			return nil, err
		}
		switch body.Method {
		case "sendTransaction":
			// The lifecycle attempts the broadcast only after durable
			// broadcast intent committed. Count the attempt and refuse it;
			// the status below lands the signature once it was sent.
			sends++
			return &http.Response{StatusCode: 500, Body: io.NopCloser(bytes.NewReader([]byte(`{}`))), Header: make(http.Header)}, nil
		case "getSignatureStatuses":
			status := `null`
			if sends > 0 {
				status = `{"slot":43,"confirmations":1,"err":null,"confirmationStatus":"confirmed"}`
			}
			return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":43},"value":[` + status + `]}}`), nil
		case "simulateTransaction":
			t.Fatalf("authorization must not simulate: no signer exists in this chain")
		}
		return base.RoundTrip(req)
	})
	return rpc, &sends
}

// seedAutoInitializerDecidedOperation seeds one decided candidate initializer
// operation on a test-owned route key.
func seedAutoInitializerDecidedOperation(t *testing.T, ctx context.Context, db *Database, f autoInitializerRecoveryFixture, key string) string {
	t.Helper()
	return seedDecidedOperation(t, ctx, db, key, InitializeKaminoObligation, f.request.RouteLane, f.raw)
}

// The real Signed transition: broadcast intent is recorded before the
// broadcast, the persisted wire lands and confirms at its slot, and a
// completed send cannot be replayed.
func TestAutoInitializerSignedTransitionThroughReviewedManifest(t *testing.T) {
	// A dedicated disposable database: nothing shared with other tests.
	t.Parallel()
	f := autoInitializerAuthorizationFixture(t)
	ctx, cancel, db := openInitializerAutoScopeServiceDatabase(t, "phase3_auto_signed_transition_test", 30*time.Second)
	defer cancel()
	rpc, sends := autoInitializerAuthorizationRPC(t, f)

	signedOperation := func(key string, decision Decision) PersistedOperation {
		t.Helper()
		id := seedAutoInitializerDecidedOperation(t, ctx, db, f, key)
		// A local unsigned wire fixture: no signer exists in this chain, the
		// exact compiled message is persisted like any wire.
		hash := sha256Bytes(f.wire)
		if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',signed_wire=$2,signed_wire_sha256=$3 WHERE operation_id=$1`, id, f.wire, hash); err != nil {
			t.Fatal(err)
		}
		return PersistedOperation{Operation: Operation{ID: id, RouteKey: key, StrategyKey: f.request.RouteLane, Decision: decision},
			Status: Signed, ExpectedEffects: f.raw, SignedWire: f.wire, SignedWireSHA256: hash,
			TransactionSignature: encodeBase58(wireSig(f.wire)), RecentBlockhash: f.request.RecentBlockhash, LastValidBlockHeight: f.request.LastValidBlockHeight}
	}
	op := signedOperation("auto-initializer-send-"+time.Now().Format("150405.000000000"),
		Decision{Action: InitializeKaminoObligation, Reason: "multiply_obligation_missing", StrategyKey: f.request.RouteLane, IdempotencyKey: "controlled-init"})
	// The decision slot the custody window starts from.
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=expected_effects || '{"decision":{"observationSlot":42}}'::jsonb WHERE operation_id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	op.ExpectedEffects = []byte(`{"decision":{"observationSlot":42}}`)

	// The internal lifecycle path: durable broadcast intent, then the
	// persisted wire lands at its slot.
	if err := advanceNonterminalWithManifest(ctx, f.manifest, db, rpc, op); err != nil {
		t.Fatal(err)
	}
	var confirmedSlot int64
	if err := db.pool.QueryRow(ctx, `SELECT confirmed_slot FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND status='confirmed' AND broadcast_intent_at IS NOT NULL`, op.ID).Scan(&confirmedSlot); err != nil || confirmedSlot != 43 {
		t.Fatalf("landed wire not confirmed at its slot: %d %v", confirmedSlot, err)
	}
	if *sends != 1 {
		t.Fatalf("broadcast attempted %d times before landing", *sends)
	}

	// A completed send cannot be replayed: the durable row is no longer
	// signed, so broadcast intent cannot be recorded again.
	if err := db.MarkBroadcastIntent(ctx, op.ID); err == nil {
		t.Fatal("completed send replayed")
	}
}

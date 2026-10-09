package backyard

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Historical receipts/signature bytes are seeded. Real prepared swap admission,
// locked build/restart/send fences run; this is NOT a full receipt lifecycle.
func TestAutoEmergencyFundingDB(t *testing.T) {
	if os.Getenv("PHASE3_TEST_DATABASE_URL") == "" {
		t.Skip("requires parent-owned disposable PostgreSQL")
	}
	ctx, cancel, db := openInitializerAutoScopeServiceDatabase(t, "phase3_topup_ledger_test", 5*time.Minute)
	defer cancel()
	o, d, evidence, m, rpc, client, accounts := autoEmergencyFundingFixture(t)
	installEmergencyFundingSimulation(t, rpc, m, accounts, evidence, o.Snapshot.Slot, nil)
	request, effects := evidence.Request, evidence.ExpectedEffects
	key, id := "funding", sha256Bytes([]byte("funding-current"))
	state := planningPilotState(t)
	budget := state["phase3"].(Phase3Budget)
	budget.Families["AUTO"] = FamilyBudget{SpentMicros: 10000, ExitMicros: 50_000_000}
	state["phase3"] = budget
	if o.Snapshot.TopupTranche != nil {
		state["topupTranche"] = o.Snapshot.TopupTranche
	}
	state["generation"] = 3
	raw, _ := json.Marshal(state)
	if _, err := db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2,3)`, key, raw); err != nil {
		t.Fatal(err)
	}
	lease, err := db.AcquireRouteLease(ctx, key, "capital-test", 3*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Seed finalized predecessor evidence, never a current authorization.
	if tr := o.Snapshot.TopupTranche; tr != nil {
		original, last := tr.OriginOperationID, tr.LastOperationID
		tr.OriginOperationID = sha256Bytes([]byte(key + original))
		tr.LastOperationID = sha256Bytes([]byte(key + last))
		raw, _ = json.Marshal(tr)
		if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{topupTranche}',$2::jsonb) WHERE route_key=$1`, key, raw); err != nil {
			t.Fatal(err)
		}
		for _, prior := range []string{tr.OriginOperationID, tr.LastOperationID} {
			wire := []byte("seeded-finalized-history-" + prior)
			auth := phase3OperationAuthorization{GoalID: Phase3GoalID, SignedWireSHA256: sha256Bytes(wire), Topup: &topupTrancheBinding{Loan: tr.Loan, OriginOperationID: tr.OriginOperationID, AllocatedUSDCRaw: tr.AllocatedUSDCRaw}, TopupResult: tr}
			body, _ := json.Marshal(map[string]any{"decision": map[string]any{"reason": topupAllocationReason}, "phase3": auth})
			if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,confirmed_slot,confirmation_status,reconciliation_sha256,signed_wire,signed_wire_sha256,expected_effects) VALUES($1,$2,'reconciled',$3,$4,$5,'finalized',$6,$7,$8,$9) ON CONFLICT DO NOTHING`, prior, key, string(VoltrAllocateToSquads), d.StrategyKey, tr.LastSlot, tr.LastEffectsSHA256, wire, sha256Bytes(wire), body); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = db.LoadTopupTranche(ctx, key); err != nil {
			t.Fatal("seeded ancestry", err)
		}
	}
	proof := custodyAdmissionProofFixture(t, autoSharedPYUSDAttributionConfig(autoAUTOPYUSD, key), key, effects, uint64(o.Snapshot.CollateralIdleRaw), o.Snapshot.Slot, 3, lease.FencingToken, "capital-test")
	o.custodyProof = &proof
	body, _ := json.Marshal(map[string]any{"decision": newDecisionEvidence(o, d, m.SHA256, *m.PolicyCatalog.SHA256), "topupTranche": o.Snapshot.TopupTranche})
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects) VALUES($1,$2,'decided',$3,$4,$5)`, id, key, string(d.Action), d.StrategyKey, body); err != nil {
		t.Fatal(err)
	}
	oldTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		clone := r.Clone(r.Context())
		clone.URL.Path = strings.TrimPrefix(clone.URL.Path, "/swap/v1")
		return client.http.Transport.RoundTrip(clone)
	})
	defer func() { http.DefaultTransport = oldTransport }()
	runtime := productionTickRuntime(db, rpc, m, Credentials{})
	admit := func() error { return runtime.admitJupiter(ctx, id, o, d, evidence) }
	read := func() string {
		t.Helper()
		var state, op string
		if err := db.pool.QueryRow(ctx, `SELECT s.state::text || s.state_version::text,o.expected_effects::text || o.status || COALESCE(encode(o.signed_wire,'hex'),'') || COALESCE(o.broadcast_intent_at::text,'') FROM loyal_yield.multiply_route_states s JOIN loyal_yield.multiply_operations o USING(route_key) WHERE operation_id=$1`, id).Scan(&state, &op); err != nil {
			t.Fatal(err)
		}
		return state + op
	}
	unchanged := func(label string, f func() error) {
		t.Helper()
		before := read()
		reason := "topup_loan_principal_changed"
		if strings.Contains(label, "image") {
			reason = "topup_kamino_program_identity_changed"
		}
		if strings.Contains(label, "predecessor") {
			reason = "topup_predecessor_changed"
		}
		if strings.Contains(label, "ancestry") {
			reason = "topup_origin_changed"
		}
		assertBudgetHold(t, f(), reason)
		if read() != before {
			t.Fatal(label, "mutated durable state")
		}
	}
	if o.Snapshot.TopupTranche != nil {
		saved := body
		if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=expected_effects-'topupTranche' WHERE operation_id=$1`, id); err != nil {
			t.Fatal(err)
		}
		unchanged("missing predecessor admission", admit)
		if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET expected_effects=$2 WHERE operation_id=$1`, id, saved); err != nil {
			t.Fatal(err)
		}
	}
	image := accountAt(accounts, reviewedTopupKaminoIdentity().programData).Data
	image[100] ^= 1
	unchanged("fresh image admission", admit)
	image[100] ^= 1
	if tr := o.Snapshot.TopupTranche; tr != nil {
		if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET confirmation_status='confirmed' WHERE operation_id=$1`, tr.OriginOperationID); err != nil {
			t.Fatal(err)
		}
		unchanged("drifted finalized ancestry admission", admit)
		if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET confirmation_status='finalized' WHERE operation_id=$1`, tr.OriginOperationID); err != nil {
			t.Fatal(err)
		}
	}
	// The existing return reservation, not this classification, must fund the
	// entire recovery. A smaller prior reservation refuses without any write.
	var priorBudget []byte
	if err = db.pool.QueryRow(ctx, `SELECT state->'phase3' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&priorBudget); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{phase3,families,AUTO,exitMicros}','5000000'::jsonb) WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	beforeInsufficient := read()
	assertBudgetHold(t, admit(), "recovery_exceeds_reserved_exit")
	if read() != beforeInsufficient {
		t.Fatal("insufficient reservation rejection wrote state")
	}
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{phase3}',$2::jsonb) WHERE route_key=$1`, key, priorBudget); err != nil {
		t.Fatal(err)
	}
	if err = admit(); err != nil {
		t.Fatal("production admission", err)
	}
	auth := loadAutoInitializerAuth(t, ctx, db, id)
	if auth.Topup == nil || auth.BuildInput == nil || auth.BridgeAdmission == nil || auth.PartialRisk == nil || auth.DebtClear != nil {
		t.Fatalf("missing real admission topup=%v input=%v plan=%v partial=%v full=%v", auth.Topup != nil, auth.BuildInput != nil, auth.BridgeAdmission != nil, auth.PartialRisk != nil, auth.DebtClear != nil)
	}
	_, _, expectedWire, err := auth.BuildInput.decodeWithManifest(m)
	if err != nil {
		t.Fatal(err)
	}
	got, gotEffects, _, err := auth.BuildInput.decodeWithManifest(m)
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(request)
	if err != nil || !bytes.Equal(gotJSON, wantJSON) || !reflect.DeepEqual(gotEffects, effects) {
		t.Fatal("current executable input changed", err)
	}
	loan := auth.Topup.Loan
	var admittedBudget []byte
	if err = db.pool.QueryRow(ctx, `SELECT state->'phase3' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&admittedBudget); err != nil {
		t.Fatal(err)
	}
	checkBudget := func() {
		t.Helper()
		var clear string
		if err := db.pool.QueryRow(ctx, `SELECT COALESCE(state->'debtClearAuthority','null'::jsonb)::text FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&clear); err != nil {
			t.Fatal(err)
		}
		if clear != "null" {
			t.Fatal("partial funding created route-level debt-clear authority")
		}

		var raw []byte
		if err := db.pool.QueryRow(ctx, `SELECT state->'phase3' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(raw, admittedBudget) {
			t.Fatal("build/send changed spent, exit or signed reservation")
		}
		var b Phase3Budget
		if json.Unmarshal(raw, &b) != nil {
			t.Fatal("budget decode")
		}
		if b.Families["AUTO"].SpentMicros != 10000 || b.Families["AUTO"].ExitMicros <= 0 || b.Reservations[id].UpperMicros <= 0 || b.Pilot == nil || *b.Pilot != *budget.Pilot || b.deploymentLimits() != budget.deploymentLimits() {
			t.Fatal("cumulative accounting or pilot changed")
		}
	}
	checkBudget()
	rawEffects, _ := json.Marshal(effects)
	build := func() error {
		return db.authorizePhase3BuildOnManifest(ctx, m, rpc, id, request, rawEffects, auth.BridgeAdmission.CurrentCost)
	}
	data := accountAt(accounts, autoAUTOPYUSD.Kamino.Obligation).Data
	data[1296]++
	unchanged("fresh principal build", build)
	data[1296]--
	image[100] ^= 1
	unchanged("fresh image build", build)
	image[100] ^= 1
	if err = build(); err != nil {
		t.Fatal("locked build", err)
	}
	// Reopen pool, expire old lease and obtain a new fence: no in-memory auth.
	if _, err = db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET lease_expires_at=clock_timestamp()-interval '1 second' WHERE route_key=$1`, key); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenDatabase(ctx, strings.Replace(os.Getenv("PHASE3_TEST_DATABASE_URL"), "/phase3_budget_test", "/phase3_topup_ledger_test", 1))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err = restarted.AcquireRouteLease(ctx, key, "capital-restart", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err = restarted.authorizePhase3BuildOnManifest(ctx, m, rpc, id, request, rawEffects, auth.BridgeAdmission.CurrentCost); err != nil {
		t.Fatal("restarted build", err)
	}
	auth = loadAutoInitializerAuth(t, ctx, restarted, id)
	_, _, wire, err := auth.BuildInput.decodeWithManifest(m)
	if err != nil || !bytes.Equal(wire, expectedWire) || auth.Topup.Loan != loan {
		t.Fatal("restart changed wire or origin", err)
	}
	// Synthetic signature only; use the real durable wire-binding transaction.
	signed := append(make([]byte, 65), wire...)
	signed[0] = 1
	hash := sha256Bytes(signed)
	tx, err := restarted.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.bindPhase3WireTx(ctx, tx, id, hash); err == nil {
		_, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',signed_wire=$2,signed_wire_sha256=$3 WHERE operation_id=$1`, id, signed, hash)
	}
	if err != nil {
		tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	auth = loadAutoInitializerAuth(t, ctx, restarted, id)
	op := PersistedOperation{Status: Signed, SignedWire: signed, SignedWireSHA256: hash, TransactionSignature: encodeBase58(signed[1:65]), RecentBlockhash: bridgeVault, LastValidBlockHeight: 99}
	send := func() error {
		cost, err := m.revaluePhase3SignedInput(ctx, rpc, auth, op)
		if err != nil {
			return err
		}
		tx, err := restarted.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if err = restarted.authorizePhase3SendTxOnManifest(ctx, m, tx, id, auth.IntentSHA256, hash, cost, o.Snapshot.Slot); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	data[1296]++
	unchanged("fresh principal send", send)
	data[1296]--
	image[100] ^= 1
	unchanged("fresh image send", send)
	image[100] ^= 1
	if tr := o.Snapshot.TopupTranche; tr != nil {
		if _, err = restarted.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET confirmation_status='confirmed' WHERE operation_id=$1`, tr.OriginOperationID); err != nil {
			t.Fatal(err)
		}
		unchanged("drifted finalized ancestry send", send)
		if _, err = restarted.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET confirmation_status='finalized' WHERE operation_id=$1`, tr.OriginOperationID); err != nil {
			t.Fatal(err)
		}
	}
	if err = send(); err != nil {
		t.Fatal("locked send", err)
	}
	checkBudget()
	var persisted []byte
	if err = restarted.pool.QueryRow(ctx, `SELECT signed_wire FROM loyal_yield.multiply_operations WHERE operation_id=$1 AND broadcast_intent_at IS NULL`, id).Scan(&persisted); err != nil || !bytes.Equal(persisted, signed) {
		t.Fatal("send fence changed wire or broadcast", err)
	}
}

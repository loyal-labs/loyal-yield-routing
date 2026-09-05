package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestPolicySetupCreatedStateMatchesSDKAndRejectsAuthorityDrift(t *testing.T) {
	var inputs []map[string]string
	for _, operation := range []string{"borrow", "repay"} {
		for _, seed := range []uint64{140, 256} {
			r := setupTestRequest(operation)
			r.Seed = seed
			data, err := policySetupExpectedAccount(r, 1000)
			if err != nil {
				t.Fatal(err)
			}
			inputs = append(inputs, map[string]string{"operation": operation, "seed": strconv.FormatUint(seed, 10), "start": "1000", "data": base64.StdEncoding.EncodeToString(data)})
			key, _ := policySetupAddress(seed)
			a := ConfirmedAccount{Address: encodeBase58(key[:]), Owner: bridgeSquadsProgram, Lamports: 10_000_000, Data: data}
			if err := validatePolicySetupCreatedAccount(r, a, a.Lamports, 1000); err != nil {
				t.Fatal(err)
			}
			for _, offset := range []int{0, 8, 40, 48, 49, 65, 69, 101, 102, 104, 108, 110, 150, len(data) - 1} {
				changed := a
				changed.Data = append([]byte(nil), a.Data...)
				changed.Data[offset] ^= 1
				if err := validatePolicySetupCreatedAccount(r, changed, a.Lamports, 1000); err == nil {
					t.Fatalf("policy mutation accepted at offset %d", offset)
				}
			}
			if err := validatePolicySetupCreatedAccount(r, a, a.Lamports, 999); err == nil {
				t.Fatal("future start accepted")
			}
		}
	}
	encoded, _ := json.Marshal(inputs)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bun", "testdata/policy-created-state-oracle.mjs")
	cmd.Stdin = bytes.NewReader(encoded)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("created state disagrees with SDK/chain: %v %s", err, output)
	}
	var result struct{ Passed int }
	if json.Unmarshal(output, &result) != nil || result.Passed != 4 {
		t.Fatal("state oracle did not cover both operations/seeds")
	}
}

func setupCreationOperation(t *testing.T, auth *phase3OperationAuthorization) PersistedOperation {
	t.Helper()
	r, _, _, err := policySetupCreationInput(*auth)
	if err != nil {
		t.Fatal(err)
	}
	ix, _, err := policySetupCreateInstruction(r)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := decodeKey(r.RecentBlockhash)
	message, err := compileLegacyMessage(mustKey(bridgeSettingsSigner), key, []compiledInstruction{ix})
	if err != nil {
		t.Fatal(err)
	}
	wire := append([]byte{1}, bytes.Repeat([]byte{9}, 64)...)
	wire = append(wire, message...)
	auth.SignedWireSHA256 = sha256Bytes(wire)
	return PersistedOperation{Operation: Operation{ID: "creation-fixture", Decision: Decision{Action: PolicySetupCreate, StrategyKey: "OnRe/ONyc/USDC"}}, Status: Submitted, SignedWire: wire, SignedWireSHA256: auth.SignedWireSHA256, TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: r.RecentBlockhash, LastValidBlockHeight: r.LastValidBlockHeight}
}

func setupCreatedRPC(t *testing.T, auth phase3OperationAuthorization, op PersistedOperation, drift string) *RPCClient {
	t.Helper()
	r, cost, prefund, err := policySetupCreationInput(auth)
	if err != nil {
		t.Fatal(err)
	}
	rpc := budgetBuildRPC(t, 5000, 42)
	rpc.retryBackoff = 0
	rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body struct {
			Method string
			Params []json.RawMessage
		}
		if json.NewDecoder(request.Body).Decode(&body) != nil {
			t.Fatal("invalid RPC input")
		}
		var result any
		switch body.Method {
		case "getGenesisHash":
			result = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
		case "getTransaction":
			var signature string
			var config map[string]any
			_ = json.Unmarshal(body.Params[0], &signature)
			_ = json.Unmarshal(body.Params[1], &config)
			if signature != op.TransactionSignature || config["commitment"] != "finalized" || config["encoding"] != "base64" {
				t.Fatal("wrong creation receipt request")
			}
			pre := []uint64{1_000_000_000, 2_060_160, prefund, 1, 1}
			post := []uint64{pre[0] - cost.SetupLamports - 5000, pre[1], prefund + cost.SetupLamports, 1, 1}
			wire := append([]byte(nil), op.SignedWire...)
			if drift == "wire" {
				wire[len(wire)-1] ^= 1
			}
			if drift == "payer debit" {
				post[0]--
			}
			if drift == "settings rent" {
				post[1]++
			}
			if drift == "wrong prefund" {
				pre[2]++
			}
			if drift == "wrong funding" {
				post[2]--
			}
			meta := map[string]any{"err": nil, "fee": 5000, "preBalances": pre, "postBalances": post}
			if drift == "failed" {
				meta["err"] = "failed"
			}
			result = map[string]any{"slot": 42, "transaction": []string{base64.StdEncoding.EncodeToString(wire), "base64"}, "meta": meta}
			if drift == "unfinalized" {
				result = nil
			}
		case "getMultipleAccounts":
			var addresses []string
			var config map[string]any
			_ = json.Unmarshal(body.Params[0], &addresses)
			_ = json.Unmarshal(body.Params[1], &config)
			if len(addresses) != 4 || addresses[0] != bridgeSettings || addresses[1] != bridgeSettingsSigner || addresses[2] != auth.PolicySetup.Policy || addresses[3] != budgetClockAddress || config["commitment"] != "finalized" || config["minContextSlot"] != float64(42) {
				t.Fatal("wrong finalized creation accounts")
			}
			settings := setupSettingsAccount(t)
			binary.LittleEndian.PutUint64(settings.Data[len(settings.Data)-9:len(settings.Data)-1], r.Seed)
			if drift == "settings authority" {
				settings.Data[24] = 1
			}
			if drift == "settings archival" {
				settings.Data[79] ^= 1
			}
			if drift == "settings counter" {
				settings.Data[62] ^= 1
			}
			if drift == "seed" {
				binary.LittleEndian.PutUint64(settings.Data[len(settings.Data)-9:len(settings.Data)-1], r.Seed+1)
			}
			data, err := policySetupExpectedAccount(r, 1000)
			if err != nil {
				t.Fatal(err)
			}
			if drift == "policy constraints" {
				data[150] ^= 1
			}
			policy := ConfirmedAccount{Address: auth.PolicySetup.Policy, Owner: bridgeSquadsProgram, Lamports: prefund + cost.SetupLamports, Data: data}
			if drift == "policy owner" {
				policy.Owner = classicTokenProgram
			}
			if drift == "policy rent" {
				policy.Lamports--
			}
			clock := ConfirmedAccount{Address: budgetClockAddress, Owner: "Sysvar1111111111111111111111111111111111111", Data: make([]byte, 40)}
			binary.LittleEndian.PutUint64(clock.Data[32:40], 1000)
			if drift == "future start" {
				binary.LittleEndian.PutUint64(clock.Data[32:40], 999)
			}
			values := []any{}
			for _, a := range []ConfirmedAccount{settings, {Address: bridgeSettingsSigner, Owner: "11111111111111111111111111111111", Lamports: 1_000_000_000}, policy, clock} {
				values = append(values, map[string]any{"owner": a.Owner, "lamports": a.Lamports, "data": []string{base64.StdEncoding.EncodeToString(a.Data), "base64"}})
			}
			result = map[string]any{"context": map[string]int{"slot": 42}, "value": values}
		default:
			t.Fatalf("unexpected setup reconciliation RPC: %s", body.Method)
		}
		encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
		return response(string(encoded)), nil
	})
	return rpc
}

func TestPolicySetupCreationReconcilesDirectAndPrefundedPayments(t *testing.T) {
	for _, staged := range []bool{false, true} {
		plan, err := observePolicySetup(context.Background(), setupObservationRPC(t, &setupRPCScenario{rent: 8_000_000}), "repay")
		if err != nil {
			t.Fatal(err)
		}
		digest, _ := validatePolicySetupPlan(plan)
		auth := phase3OperationAuthorization{GoalID: Phase3GoalID, IntentSHA256: digest, PolicySetup: &plan}
		if staged {
			plan = observedSetupFixture(t, "borrow")
			prefund := setupPrefundOperation(t, plan)
			completion, err := observePolicySetupCompletion(context.Background(), setupCompletionRPC(t, plan, prefund, ""), plan, prefund)
			if err != nil {
				t.Fatal(err)
			}
			auth.PolicySetupCompletion = &completion
			auth.IntentSHA256, err = validatePolicySetupCompletion(plan, completion)
			if err != nil {
				t.Fatal(err)
			}
		}
		op := setupCreationOperation(t, &auth)
		out, err := observePolicySetupCreated(context.Background(), setupCreatedRPC(t, auth, op, ""), auth, op)
		if err != nil || out.Slot != 42 || out.FinalizedAccountSlot != 42 {
			t.Fatalf("creation reconciliation: %+v %v", out, err)
		}
		for _, drift := range []string{"wire", "payer debit", "settings rent", "wrong prefund", "wrong funding", "failed", "unfinalized", "settings authority", "settings archival", "settings counter", "seed", "policy constraints", "policy owner", "policy rent", "future start"} {
			if _, err := observePolicySetupCreated(context.Background(), setupCreatedRPC(t, auth, op, drift), auth, op); err == nil {
				t.Fatalf("accepted %s staged=%v", drift, staged)
			}
		}
	}
}

func testPolicySetupCreationSettlement(t *testing.T, ctx context.Context, db *Database, id, route string, spent int64) {
	t.Helper()
	testPolicySetupPaymentAuthorization(t, ctx, db, id)
	var saved []byte
	if err := db.pool.QueryRow(ctx, `SELECT expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	var auth phase3OperationAuthorization
	if json.Unmarshal(saved, &auth) != nil {
		t.Fatal("invalid setup auth")
	}
	op := setupCreationOperation(t, &auth)
	op.ID, op.RouteKey = id, route
	_, cost, _, err := policySetupCreationInput(auth)
	if err != nil {
		t.Fatal(err)
	}
	auth.SendKnownCost = &cost
	encoded, _ := json.Marshal(auth)
	if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='submitted',signed_wire=$2,signed_wire_sha256=$3,transaction_signature=$4,recent_blockhash=$5,last_valid_block_height=$6,expected_effects=jsonb_set(expected_effects,'{phase3}',$7::jsonb) WHERE operation_id=$1`, id, op.SignedWire, op.SignedWireSHA256, op.TransactionSignature, op.RecentBlockhash, op.LastValidBlockHeight, string(encoded)); err != nil {
		t.Fatal(err)
	}
	read := func() string {
		var raw string
		if err := db.pool.QueryRow(ctx, `SELECT state::text FROM loyal_yield.multiply_route_states WHERE route_key=$1`, route).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := read()
	for _, drift := range []string{"unfinalized", "wrong funding", "policy constraints", "lease expired"} {
		rpc := setupCreatedRPC(t, auth, op, drift)
		if drift == "lease expired" {
			if _, err := db.pool.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET lease_expires_at=clock_timestamp()+interval '100 milliseconds' WHERE route_key=$1`, route); err != nil {
				t.Fatal(err)
			}
			base := rpc.client.Transport
			rpc.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				time.Sleep(60 * time.Millisecond)
				return base.RoundTrip(r)
			})
		}
		if err := AdvanceNonterminal(ctx, db, rpc, op); err == nil {
			t.Fatalf("creation reconciliation accepted %s", drift)
		}
		if read() != before {
			t.Fatalf("failed creation changed budget/fence: %s", drift)
		}
		if drift == "lease expired" {
			if _, err := db.AcquireRouteLease(ctx, route, "creation-lease-restart", time.Minute); err != nil {
				t.Fatal(err)
			}
		}
		pending, err := db.LoadNonterminal(ctx, route)
		if err != nil || pending == nil || pending.ID != id || pending.Status != Submitted {
			t.Fatalf("failed creation lost pending operation: %+v %v", pending, err)
		}
	}
	if err := AdvanceNonterminal(ctx, db, setupCreatedRPC(t, auth, op, ""), op); err != nil {
		t.Fatal(err)
	}
	var state struct {
		Budget  Phase3Budget `json:"phase3"`
		Pointer *string      `json:"phase3SetupIntent"`
	}
	after := read()
	if json.Unmarshal([]byte(after), &state) != nil || state.Pointer != nil || len(state.Budget.Reservations) != 0 || state.Budget.Families["OnRe"] != (FamilyBudget{SpentMicros: spent}) {
		t.Fatalf("setup fence/spend not settled: %+v", state)
	}
	pending, err := db.LoadNonterminal(ctx, route)
	if err != nil || pending != nil {
		t.Fatalf("terminal setup remained pending: %+v %v", pending, err)
	}
	if err := db.reconcilePolicySetupCreation(ctx, setupCreatedRPC(t, auth, op, "unfinalized"), id); err != nil {
		t.Fatal(err)
	}
	if read() != after {
		t.Fatal("terminal retry booked setup twice")
	}
	var finalized bool
	var raw []byte
	if err := db.pool.QueryRow(ctx, `SELECT status='reconciled' AND confirmation_status='finalized',reconciled_effects FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&finalized, &raw); err != nil || !finalized {
		t.Fatal("missing terminal receipt")
	}
	var receipt policySetupCreatedReceipt
	if json.Unmarshal(raw, &receipt) != nil || receipt.WireSHA256 != op.SignedWireSHA256 || len(receipt.Policy.Data) == 0 || len(receipt.PreBalances) != 5 || len(receipt.PostBalances) != 5 {
		t.Fatal("terminal receipt omitted policy/native evidence")
	}
}

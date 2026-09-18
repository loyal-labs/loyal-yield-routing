package backyardrwa

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"testing"
)

func setupPaymentAuth(t *testing.T, stage string) phase3OperationAuthorization {
	t.Helper()
	plan, err := observePolicySetup(context.Background(), setupObservationRPC(t, &setupRPCScenario{rent: 8_000_000}), "repay")
	if err != nil {
		t.Fatal(err)
	}
	if stage != "direct" {
		plan = observedSetupFixture(t, "borrow")
	}
	digest, _ := validatePolicySetupPlan(plan)
	auth := phase3OperationAuthorization{GoalID: Phase3GoalID, IntentSHA256: digest, PolicySetup: &plan}
	if stage == "continuation" {
		op := setupPrefundOperation(t, plan)
		completion, err := observePolicySetupCompletion(context.Background(), setupCompletionRPC(t, plan, op, ""), plan, op)
		if err != nil {
			t.Fatal(err)
		}
		auth.PolicySetupCompletion = &completion
		auth.IntentSHA256, err = validatePolicySetupCompletion(plan, completion)
		if err != nil {
			t.Fatal(err)
		}
	}
	return auth
}

func setupPaymentRPC(t *testing.T, auth phase3OperationAuthorization, drift string) *RPCClient {
	t.Helper()
	var extra []ConfirmedAccount
	if drift == "price up" || drift == "over cap" {
		price := int64(101)
		if drift == "over cap" {
			price = 201
		}
		sf := new(big.Int).Lsh(big.NewInt(1), 60)
		sol := reserveFixture(t, budgetSOLReserve, budgetWrappedSOLMint, 42, new(big.Int).Mul(sf, big.NewInt(price)), 1, 1)
		putKey(t, sol.Data[32:64], budgetSOLMarket)
		binary.LittleEndian.PutUint64(sol.Data[272:280], 9)
		binary.LittleEndian.PutUint64(sol.Data[264:272], 1000)
		extra = []ConfirmedAccount{sol}
	}
	rpc := budgetBuildRPCWithAccounts(t, 5000, 42, extra)
	rpc.retryBackoff = 0
	base := rpc.client.Transport
	fees := 0
	rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		request.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct {
			Method string
			Params []json.RawMessage
		}
		if json.Unmarshal(raw, &body) != nil {
			t.Fatal("invalid payment RPC")
		}
		var result any
		switch body.Method {
		case "getGenesisHash":
			result = "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d"
			if drift == "genesis" {
				result = "other"
			}
		case "getMinimumBalanceForRentExemption":
			rent := auth.PolicySetup.RentLamports
			if auth.PolicySetupCompletion != nil {
				rent = auth.PolicySetupCompletion.RentLamports
			}
			if drift == "rent" {
				rent++
			}
			result = rent
		case "getBlockHeight":
			result = 10
			if drift == "blockhash" {
				result = 1000
			}
		case "getFeeForMessage":
			fees++
			if drift == "completion over cap" && fees == 2 {
				result = map[string]any{"context": map[string]int{"slot": 42}, "value": 10_000_000}
			} else if drift == "fee" {
				result = map[string]any{"context": map[string]int{"slot": 42}, "value": 5001}
			} else {
				return base.RoundTrip(request)
			}
		case "getMultipleAccounts":
			var addresses []string
			var config map[string]any
			_ = json.Unmarshal(body.Params[0], &addresses)
			_ = json.Unmarshal(body.Params[1], &config)
			if len(addresses) == 0 || addresses[0] != bridgeSettings {
				return base.RoundTrip(request)
			}
			if len(addresses) != 3 || addresses[1] != bridgeSettingsSigner || addresses[2] != auth.PolicySetup.Policy {
				t.Fatal("setup payment requested wrong identities")
			}
			settings := setupSettingsAccount(t)
			if drift == "settings" {
				settings.Data[8] ^= 1
			}
			balance := uint64(1_000_000_000)
			if drift == "underfunded" {
				balance = 1
			}
			var target any
			if auth.PolicySetupCompletion != nil {
				target = map[string]any{"owner": "11111111111111111111111111111111", "lamports": auth.PolicySetupCompletion.Prefund.FundedLamports, "data": []string{"", "base64"}}
			}
			if drift == "target" {
				target = map[string]any{"owner": bridgeSquadsProgram, "lamports": 1, "data": []string{"AQ==", "base64"}}
			}
			slot := 42
			if drift == "expired" && config["commitment"] == "confirmed" {
				slot = 75
			}
			result = map[string]any{"context": map[string]int{"slot": slot}, "value": []any{
				map[string]any{"owner": settings.Owner, "lamports": settings.Lamports, "data": []string{base64.StdEncoding.EncodeToString(settings.Data), "base64"}},
				map[string]any{"owner": "11111111111111111111111111111111", "lamports": balance, "data": []string{"", "base64"}}, target}}
		default:
			return base.RoundTrip(request)
		}
		encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
		return response(string(encoded)), nil
	})
	return rpc
}

func TestPolicySetupPaymentRepricesEveryStageAndRejectsUnfitCompletion(t *testing.T) {
	for _, stage := range []string{"direct", "prefund", "continuation"} {
		t.Run(stage, func(t *testing.T) {
			auth := setupPaymentAuth(t, stage)
			action := PolicySetupCreate
			if stage == "prefund" {
				action = PolicySetupPrefund
			}
			input, err := policySetupPaymentInput(auth, action)
			if err != nil {
				t.Fatal(err)
			}
			out, err := observePolicySetupPayment(context.Background(), setupPaymentRPC(t, auth, ""), auth, action)
			if err != nil || !bytes.Equal(out.Message, input.Message) || out.Cost.TotalMicros != input.Cost.TotalMicros || (out.CompletionCost != nil) != (stage == "prefund") {
				t.Fatalf("payment changed frozen intent: %+v %v", out, err)
			}
			for _, drift := range []string{"genesis", "rent", "blockhash", "fee", "settings", "underfunded", "target", "expired", "over cap"} {
				if _, err := observePolicySetupPayment(context.Background(), setupPaymentRPC(t, auth, drift), auth, action); err == nil {
					t.Fatalf("payment accepted %s", drift)
				}
			}
			if stage == "prefund" {
				_, err := observePolicySetupPayment(context.Background(), setupPaymentRPC(t, auth, "completion over cap"), auth, action)
				assertBudgetHold(t, err, "setup_completion_exceeds_reserved_cap")
			}
		})
	}
}

func TestPolicySetupSignedIdentityRejectsSyntheticOrDifferentSignersBeforeRPC(t *testing.T) {
	auth := setupPaymentAuth(t, "prefund")
	op := setupPrefundOperation(t, *auth.PolicySetup)
	op.Status = Signed
	auth.SignedWireSHA256 = op.SignedWireSHA256
	_, err := revaluePhase3SignedInput(context.Background(), nil, auth, op)
	assertBudgetHold(t, err, "setup_signature_invalid")
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, 32))
	copy(op.SignedWire[1:65], ed25519.Sign(key, op.SignedWire[65:]))
	op.SignedWireSHA256 = sha256Bytes(op.SignedWire)
	op.TransactionSignature = encodeBase58(op.SignedWire[1:65])
	auth.SignedWireSHA256 = op.SignedWireSHA256
	_, err = revaluePhase3SignedInput(context.Background(), nil, auth, op)
	assertBudgetHold(t, err, "setup_signature_invalid")
	op.SignedWire[len(op.SignedWire)-1] ^= 1
	op.SignedWireSHA256 = sha256Bytes(op.SignedWire)
	auth.SignedWireSHA256 = op.SignedWireSHA256
	_, err = revaluePhase3SignedInput(context.Background(), nil, auth, op)
	assertBudgetHold(t, err, "setup_signed_intent_mismatch")
}

func testPolicySetupPaymentAuthorization(t *testing.T, ctx context.Context, db *Database, id string) {
	t.Helper()
	var saved []byte
	var route string
	var action Action
	if err := db.pool.QueryRow(ctx, `SELECT route_key,action,expected_effects->'phase3' FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&route, &action, &saved); err != nil {
		t.Fatal(err)
	}
	var auth phase3OperationAuthorization
	if json.Unmarshal(saved, &auth) != nil {
		t.Fatal("invalid payment auth")
	}
	read := func() (string, string) {
		var state, budget string
		if err := db.pool.QueryRow(ctx, `SELECT state::text,(state->'phase3')::text FROM loyal_yield.multiply_route_states WHERE route_key=$1`, route).Scan(&state, &budget); err != nil {
			t.Fatal(err)
		}
		return state, budget
	}
	before, budgetBefore := read()
	for _, drift := range []string{"price up", "over cap", "rent", "target", "expired"} {
		_, err := db.authorizePolicySetupPayment(ctx, setupPaymentRPC(t, auth, drift), id)
		if err == nil {
			t.Fatalf("durable gate accepted %s", drift)
		}
		if drift == "price up" {
			assertBudgetHold(t, err, "fresh_setup_cost_exceeds_reservation")
		}
		after, _ := read()
		if after != before {
			t.Fatal("rejected payment mutated state")
		}
	}
	payment, err := db.authorizePolicySetupPayment(ctx, setupPaymentRPC(t, auth, ""), id)
	if err != nil {
		t.Fatal(err)
	}
	_, afterBudget := read()
	if budgetBefore != afterBudget {
		t.Fatal("build authorization changed spend/reservations")
	}
	var neverSigned bool
	if err := db.pool.QueryRow(ctx, `SELECT expected_effects->'phase3',status='decided' AND signed_wire IS NULL AND transaction_signature IS NULL AND broadcast_intent_at IS NULL FROM loyal_yield.multiply_operations WHERE operation_id=$1`, id).Scan(&saved, &neverSigned); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(saved, &auth) != nil || auth.SetupBuildCost == nil || auth.SetupBuildCost.MessageSHA256 != payment.Cost.MessageSHA256 || !neverSigned {
		t.Fatal("payment proof did not remain pre-sign")
	}
	// Exercise only the locked DB gate with synthetic wire metadata. Production
	// revaluation independently rejects these signatures before it reaches here.
	var op PersistedOperation
	if action == PolicySetupPrefund {
		op = setupPrefundOperation(t, *auth.PolicySetup)
		auth.SignedWireSHA256 = op.SignedWireSHA256
	} else {
		op = setupCreationOperation(t, &auth)
	}
	op.ID, op.RouteKey, op.Status = id, route, Signed
	for _, mode := range []string{"valid DB reservation", "higher fresh cost", "missing build proof", "missing setup pointer"} {
		tx, err := db.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer tx.Rollback(ctx)
			value := auth
			if mode == "missing build proof" {
				value.SetupBuildCost = nil
			}
			encoded, _ := json.Marshal(value)
			if _, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_operations SET status='signed',signed_wire=$2,expected_effects=jsonb_set(expected_effects,'{phase3}',$3::jsonb) WHERE operation_id=$1`, id, op.SignedWire, string(encoded)); err != nil {
				t.Fatal(err)
			}
			if mode == "missing setup pointer" {
				if _, err := tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=state-'phase3SetupIntent' WHERE route_key=$1`, route); err != nil {
					t.Fatal(err)
				}
			}
			cost := payment.Cost
			if mode == "higher fresh cost" {
				cost.TotalMicros++
			}
			err = db.authorizePhase3SendTx(ctx, tx, id, auth.IntentSHA256, op.SignedWireSHA256, cost)
			if mode == "valid DB reservation" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatalf("final DB gate accepted %s", mode)
			}
		}()
	}
	_, afterBudget = read()
	if afterBudget != budgetBefore {
		t.Fatal("DB gate probe changed real fixture reservation")
	}
}

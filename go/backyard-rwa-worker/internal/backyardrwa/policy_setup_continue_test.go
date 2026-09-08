package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func setupPrefundOperation(t *testing.T, plan policySetupObservation) PersistedOperation {
	t.Helper()
	pair, err := compilePolicySetupMessages(plan.Request, plan.RentLamports/2)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately synthetic signature bytes: these tests exercise recovery of
	// controlled RPC fixtures, not possession of the pinned production signer.
	wire := append([]byte{1}, bytes.Repeat([]byte{7}, 64)...)
	wire = append(wire, pair[0]...)
	return PersistedOperation{Operation: Operation{ID: "prefund-fixture", Decision: Decision{Action: PolicySetupPrefund, StrategyKey: "OnRe/ONyc/USDC"}}, Status: Submitted, SignedWire: wire, SignedWireSHA256: sha256Bytes(wire), TransactionSignature: encodeBase58(wire[1:65]), RecentBlockhash: plan.Request.RecentBlockhash, LastValidBlockHeight: plan.Request.LastValidBlockHeight}
}

func setupCompletionRPC(t *testing.T, plan policySetupObservation, op PersistedOperation, drift string) *RPCClient {
	t.Helper()
	rpc := setupObservationRPC(t, &setupRPCScenario{rent: plan.RentLamports})
	rpc.retryBackoff = 0
	base := rpc.client.Transport
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
			t.Fatal("invalid RPC request")
		}
		var result any
		switch body.Method {
		case "getTransaction":
			var signature string
			var config map[string]any
			_ = json.Unmarshal(body.Params[0], &signature)
			_ = json.Unmarshal(body.Params[1], &config)
			if signature != op.TransactionSignature || config["commitment"] != "finalized" || config["encoding"] != "base64" {
				t.Fatal("recovery did not request the exact finalized wire")
			}
			pre := []uint64{1_000_000_000, 0, 1}
			post := []uint64{pre[0] - plan.Payments[0].SetupLamports - 5000, plan.Payments[0].SetupLamports, 1}
			meta := map[string]any{"err": nil, "fee": 5000, "preBalances": pre, "postBalances": post}
			wire := append([]byte(nil), op.SignedWire...)
			slot := int64(42)
			switch drift {
			case "wire":
				wire[len(wire)-1] ^= 1
			case "failed":
				meta["err"] = map[string]any{"InstructionError": []any{0, "Custom"}}
			case "missing error":
				delete(meta, "err")
			case "missing fee":
				delete(meta, "fee")
			case "excess fee":
				meta["fee"] = 5001
			case "payer debit":
				post[0]--
			case "target credit":
				post[1]--
			case "adopt funded":
				pre[1] = 1
			case "unrelated debit":
				post[2] = 0
			case "missing balances":
				meta["postBalances"] = post[:2]
			case "wrong slot":
				slot = 41
			}
			result = map[string]any{"slot": slot, "transaction": []string{base64.StdEncoding.EncodeToString(wire), "base64"}, "meta": meta}
			if drift == "missing meta" {
				result.(map[string]any)["meta"] = nil
			}
			if drift == "unfinalized" {
				result = nil
			}
		case "getLatestBlockhash":
			result = map[string]any{"context": map[string]int{"slot": 42}, "value": map[string]any{"blockhash": bridgeUSDC, "lastValidBlockHeight": 199}}
			if drift == "refresh" {
				result = map[string]any{"context": map[string]int{"slot": 42}, "value": map[string]any{"blockhash": bridgeDelegate, "lastValidBlockHeight": 299}}
			}
			if drift == "refresh again" {
				result = map[string]any{"context": map[string]int{"slot": 42}, "value": map[string]any{"blockhash": bridgeSettings, "lastValidBlockHeight": 399}}
			}
		case "getMultipleAccounts":
			var addresses []string
			var config map[string]any
			_ = json.Unmarshal(body.Params[0], &addresses)
			_ = json.Unmarshal(body.Params[1], &config)
			if len(addresses) == 0 || addresses[0] != bridgeSettings {
				return base.RoundTrip(request)
			}
			if len(addresses) != 3 || addresses[1] != bridgeSettingsSigner || addresses[2] != plan.Policy || config["minContextSlot"] != float64(42) {
				t.Fatal("recovery account identity drift")
			}
			settings := setupSettingsAccount(t)
			if drift == "settings" {
				settings.Data[8] ^= 1
			}
			target := map[string]any{"owner": "11111111111111111111111111111111", "lamports": plan.Payments[0].SetupLamports, "data": []string{"", "base64"}}
			switch drift {
			case "owner":
				target["owner"] = bridgeSquadsProgram
			case "funded amount":
				target["lamports"] = plan.Payments[0].SetupLamports + 1
			case "allocated":
				target["data"] = []string{"AQ==", "base64"}
			case "executable":
				target["executable"] = true
			}
			balance := uint64(1_000_000_000)
			if drift == "underfunded" {
				balance = 1
			}
			slot := 42
			if drift == "expired" && config["commitment"] == "confirmed" {
				slot = 75
			}
			result = map[string]any{"context": map[string]int{"slot": slot}, "value": []any{
				map[string]any{"owner": settings.Owner, "lamports": settings.Lamports, "data": []string{base64.StdEncoding.EncodeToString(settings.Data), "base64"}},
				map[string]any{"owner": "11111111111111111111111111111111", "lamports": balance, "data": []string{"", "base64"}}, target,
			}}
		case "getMinimumBalanceForRentExemption":
			if drift == "refresh" {
				result = plan.RentLamports + 100_000
			} else if drift == "refresh again" {
				result = plan.RentLamports + 200_000
			} else if drift != "over cap" {
				return base.RoundTrip(request)
			} else {
				result = uint64(30_000_000)
			}
		default:
			return base.RoundTrip(request)
		}
		encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
		return response(string(encoded)), nil
	})
	return rpc
}

func TestPolicySetupCompletionRequiresExactFinalizedPrefundAndFreshRemainingPayment(t *testing.T) {
	plan := observedSetupFixture(t, "borrow")
	op := setupPrefundOperation(t, plan)
	out, err := observePolicySetupCompletion(context.Background(), setupCompletionRPC(t, plan, op, ""), plan, op)
	if err != nil {
		t.Fatal(err)
	}
	if out.Prefund.FundedLamports != plan.Payments[0].SetupLamports || out.Request.Seed != plan.Request.Seed || out.Request.Operation != plan.Request.Operation || out.Request.RecentBlockhash == plan.Request.RecentBlockhash || out.Cost.SetupLamports != plan.RentLamports-plan.Payments[0].SetupLamports || out.Cost.TotalMicros != plan.Payments[1].TotalMicros || out.FinalizedAccountSlot != 42 {
		t.Fatalf("continuation repeated funding or changed intent: %+v", out)
	}
	for _, drift := range []string{"wire", "failed", "missing error", "missing fee", "excess fee", "payer debit", "target credit", "adopt funded", "unrelated debit", "missing balances", "wrong slot", "missing meta", "unfinalized", "settings", "owner", "funded amount", "allocated", "executable", "underfunded", "expired", "over cap"} {
		t.Run(drift, func(t *testing.T) {
			_, err := observePolicySetupCompletion(context.Background(), setupCompletionRPC(t, plan, op, drift), plan, op)
			if err == nil {
				t.Fatal("unproven/unsafe continuation accepted")
			}
		})
	}
	for _, mutate := range []func(*PersistedOperation){
		func(o *PersistedOperation) { o.TransactionSignature = "other" },
		func(o *PersistedOperation) { o.ConfirmedSlot = 43 },
		func(o *PersistedOperation) { o.SignedWireSHA256 = "" },
		func(o *PersistedOperation) { o.LastValidBlockHeight++ },
		func(o *PersistedOperation) { o.Decision.StrategyKey = "AUTO/AUTO/PYUSD" },
	} {
		changed := op
		mutate(&changed)
		if _, err := observePolicySetupCompletion(context.Background(), setupCompletionRPC(t, plan, op, ""), plan, changed); err == nil {
			t.Fatal("changed journal accepted")
		}
	}
}

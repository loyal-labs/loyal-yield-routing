package backyardrwa

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"testing"
)

// custodyBalanceRPC answers getMultipleAccounts for one account at a fixed
// slot, honoring minContextSlot like a real node.
func custodyBalanceRPC(t *testing.T, slot int64, account ConfirmedAccount) *RPCClient {
	t.Helper()
	rpc, _ := NewRPCClient("https://rpc.invalid")
	rpc.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.Method != "getMultipleAccounts" {
			t.Fatalf("unexpected custody RPC %s", body.Method)
		}
		var options struct {
			MinContextSlot int64 `json:"minContextSlot"`
		}
		_ = json.Unmarshal(body.Params[1], &options)
		if options.MinContextSlot > slot {
			return response(`{"jsonrpc":"2.0","id":1,"error":{"code":-32016,"message":"Minimum context slot has not been reached"}}`), nil
		}
		encoded, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]int64{"slot": slot},
			"value": []any{map[string]any{"owner": account.Owner, "lamports": account.Lamports, "executable": false, "data": []string{base64.StdEncoding.EncodeToString(account.Data), "base64"}}}}})
		return response(string(encoded)), nil
	})
	rpc.retryBackoff, rpc.fixedRetryBackoff = 0, true
	return rpc
}

// A5: the final-send journal snapshot is read in parallel with revaluation
// and the custody balance afterwards. A custody move between the two reads
// must still refuse, and the balance must be read at the revalued slot.
func TestSplitSendCustodyProofCatchesCustodyChangeAfterJournalRead(t *testing.T) {
	cfg := custodyAttributionConfig()
	spend := custodyAttributionRepayExpected(3_100_000_000, 600_000_000, 6_000_000_000, 8_500_000_000)
	lease := RouteLease{RouteKey: cfg.RouteKey, Owner: "owner", FencingToken: 7}
	send := sharedCustodySendProof{applies: true, cfg: cfg, inputs: sharedCustodyProofInputs{
		spend:    sharedCustodySpendRaw(spend, cfg),
		planning: &routePlanningState{routeKey: cfg.RouteKey, generation: 11, lease: &lease},
		probe:    &sharedCustodyCurrentOperation{OperationID: "signed-spend"},
		evidence: sharedCustodyAttributionEvidence{Rows: []custodyAttributionRow{
			custodyAttributionRepayRow(t, "auto-repay", "sig-repay", 200),
			custodyAttributionFundingRow(t, "auto-fund", "sig-fund", 100),
		}},
	}}
	custody := make([]byte, 165)
	putKey(t, custody[:32], cfg.Mint)
	putKey(t, custody[32:64], cfg.Authority)
	custody[108] = 1
	account := ConfirmedAccount{Address: cfg.Custody, Owner: cfg.Owner, Lamports: 1, Data: custody}
	// Journal rows sit at slots 100/200; the chain answers at slot 300.
	rpc := custodyBalanceRPC(t, 300, account)
	ctx := context.Background()

	// The journal tip says 3.1B and the chain still holds 3.1B: proven.
	binary.LittleEndian.PutUint64(custody[64:72], 3_100_000_000)
	proof, err := send.finish(ctx, rpc, spend, 300)
	if err != nil || proof == nil || proof.ObservedSlot != 300 || proof.ExcludedOperation != "signed-spend" || proof.Generation != 11 || proof.Digest != sharedCustodyAdmissionDigest(*proof) {
		t.Fatalf("coherent split proof refused or malformed: %v %+v", err, proof)
	}
	serial, err := finishSharedCustodySpendProof(ctx, cfg, spend, send.inputs, 3_100_000_000, 300, nil)
	if err != nil || serial.Digest != proof.Digest {
		t.Fatal("split proof differs from the serial proof", err)
	}
	// Custody moved after the journal read (an unjournaled top-up): refused.
	binary.LittleEndian.PutUint64(custody[64:72], 3_100_000_001)
	if _, err := send.finish(ctx, rpc, spend, 300); custodyAttributionHoldReason(t, err) != "custody_attribution_balance_mismatch" {
		t.Fatalf("custody change between the reads accepted: %v", err)
	}
	// The balance may not come from before the revalued cost slot.
	binary.LittleEndian.PutUint64(custody[64:72], 3_100_000_000)
	if _, err := send.finish(ctx, rpc, spend, 301); err == nil {
		t.Fatal("balance older than the revalued cost accepted")
	}
	// Journal inputs taken for other effects cannot finish this spend.
	other := custodyAttributionRepayExpected(3_100_000_000, 100_000_000, 6_000_000_000, 9_000_000_000)
	if _, err := send.finish(ctx, rpc, other, 300); err == nil {
		t.Fatal("journal inputs reused for a different spend")
	}
	// A zero-spend or other-lane operation takes no proof at all.
	if proof, err := (sharedCustodySendProof{}).finish(ctx, rpc, spend, 300); proof != nil || err != nil {
		t.Fatal("non-applying send produced a proof", err)
	}
}

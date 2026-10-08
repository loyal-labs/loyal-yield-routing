package fleet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// loadBalancedBlockhashRPC answers getLatestBlockhash like a load-balanced
// provider whose next backend lags: the confirmed bank is at confirmedSlot,
// the finalized bank at finalizedSlot, and minContextSlot is enforced against
// the bank of the requested commitment (-32016 when not reached). Only the
// finalized blockhash is known to every backend.
func loadBalancedBlockhashRPC(t *testing.T, confirmedSlot, finalizedSlot int64, confirmedHash, finalizedHash string) *RPCClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		var config struct {
			Commitment     string `json:"commitment"`
			MinContextSlot *int64 `json:"minContextSlot"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.Method != "getLatestBlockhash" || len(request.Params) != 1 || json.Unmarshal(request.Params[0], &config) != nil {
			t.Errorf("unexpected RPC request %q", request.Method)
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		slot, hash := confirmedSlot, confirmedHash
		if config.Commitment == "finalized" {
			slot, hash = finalizedSlot, finalizedHash
		}
		if config.MinContextSlot != nil && *config.MinContextSlot > slot {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32016, "message": "Minimum context slot has not been reached"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"context": map[string]any{"slot": slot}, "value": map[string]any{"blockhash": hash, "lastValidBlockHeight": 2_000}}})
	}))
	t.Cleanup(server.Close)
	return &RPCClient{url: server.URL, client: server.Client(), retryDelay: func(int) time.Duration { return 0 }}
}

// Rust compiles, simulates and prices every route with a finalized blockhash
// (46997c76): on the load-balanced RPC a confirmed blockhash failed 14% of
// prepares with BlockhashNotFound or a null fee on the next backend. The
// same-mint route's evidence slot is confirmed, which no finalized bank has
// reached, so it must not floor the finalized read.
func TestRouteBlockhashIsFinalizedOnALoadBalancedRPC(t *testing.T) {
	confirmedHash, finalizedHash := testPubkey(71), testPubkey(72)
	rpc := loadBalancedBlockhashRPC(t, 1_000, 968, confirmedHash, finalizedHash)
	ctx := context.Background()
	hash, height, err := rpc.finalizedBlockhash(ctx, 0)
	if err != nil || hash != finalizedHash || height != 2_000 {
		t.Fatalf("route blockhash is not the finalized one every backend knows: hash=%s height=%d err=%v", hash, height, err)
	}
	// Finalized cross-mint evidence floors the read at its own finalized slot.
	if hash, _, err = rpc.finalizedBlockhash(ctx, 968); err != nil || hash != finalizedHash {
		t.Fatalf("finalized evidence floor refused: hash=%s err=%v", hash, err)
	}
	if _, _, err = rpc.finalizedBlockhash(ctx, 1_000); err == nil {
		t.Fatal("a floor above the finalized bank was reported as satisfied")
	}
}

package fleetexec

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	sdk "github.com/gagliardetto/solana-go"
)

func TestLookupExpiryRequiresCompleteFinalizedAncestryAndNoEffect(t *testing.T) {
	a, _, _, snapshot := lookupUnitRecovery(t)
	a.SigningContextSlot = 8998
	snapshot.Slot = 9000
	snapshot.Absent = true
	snapshot.Owner = ""
	snapshot.Authority = ""
	snapshot.Addresses = nil
	status := SignatureStatus{BlockHeight: 1151}
	hash := func(slot int64) string { var b [32]byte; b[0] = byte(slot); return sdk.Hash(b).String() }
	for _, kind := range []string{"complete", "null", "signatures-missing", "signature-found", "wrong-parent-hash", "signing-bank-skipped"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Method string
					Params []json.RawMessage
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil || request.Method != "getBlock" {
					t.Error("invalid history request")
					return
				}
				var slot int64
				_ = json.Unmarshal(request.Params[0], &slot)
				calls++
				var result any
				if kind != "null" {
					block := map[string]any{"blockhash": hash(slot), "previousBlockhash": hash(slot - 1), "parentSlot": slot - 1, "blockHeight": 1151, "signatures": []string{}}
					if kind == "signatures-missing" {
						delete(block, "signatures")
					}
					if kind == "signature-found" {
						block["signatures"] = []string{a.Wire.TransactionSignature}
					}
					if kind == "wrong-parent-hash" && slot == 9000 {
						block["previousBlockhash"] = hash(123)
					}
					if kind == "signing-bank-skipped" && slot == 8999 {
						block["parentSlot"] = 8997
					}
					result = block
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
			}))
			defer server.Close()
			rpc, err := NewLookupRPC(server.URL, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			history, err := rpc.lookupHistory(context.Background(), a, snapshot)
			if kind != "complete" {
				if err == nil {
					t.Fatal("incomplete finalized landing-window accepted")
				}
				return
			}
			if err != nil || calls != 3 {
				t.Fatalf("complete proof missing: calls%d %v", calls, err)
			}
			proof, err := expireLookup(a, status, snapshot, history)
			if err != nil || proof.state != LookupExpired || !proof.historyComplete {
				t.Fatalf("exact expiry: %+v %v", proof, err)
			}
			if _, err = expireLookup(a, status, snapshot, nil); err == nil {
				t.Fatal("status absence substituted for history")
			}
			found := status
			found.Found = true
			if _, err = expireLookup(a, found, snapshot, history); err == nil {
				t.Fatal("found signature expired")
			}
			drift := snapshot
			drift.Absent = false
			if _, err = expireLookup(a, status, drift, history); err == nil {
				t.Fatal("chain effect expired")
			}
		})
	}
}

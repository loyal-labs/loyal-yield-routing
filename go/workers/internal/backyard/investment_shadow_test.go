package backyard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInvestmentShadowRejectsMissingReadCredentials(t *testing.T) {
	for _, vault := range []string{"rwa", "looping"} {
		err := CaptureInvestmentShadow(context.Background(), io.Discard, InvestmentShadowConfig{Vault: vault, Lane: onreONycUSDC})
		if err == nil || !strings.Contains(err.Error(), "credentials are required") {
			t.Fatalf("unexpected error for %s: %v", vault, err)
		}
	}
}

func TestInvestmentShadowCapturesProgramsAndAccountsAtOneSlot(t *testing.T) {
	const program = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"
	const dataKey = "So11111111111111111111111111111111111111112"
	programData := make([]byte, 36)
	programData[0] = 2
	key := mustKey(dataKey)
	copy(programData[4:], key[:])
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string
			Params []json.RawMessage
			ID     any
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request.Method != "getMultipleAccounts" {
			t.Errorf("unexpected method %s", request.Method)
		}
		var addresses []string
		var options map[string]any
		_ = json.Unmarshal(request.Params[0], &addresses)
		_ = json.Unmarshal(request.Params[1], &options)
		if options["commitment"] != "confirmed" || options["minContextSlot"] != float64(100) {
			t.Errorf("lost bank constraints: %v", options)
		}
		calls++
		values := make([]any, len(addresses))
		for i, address := range addresses {
			data := []byte{byte(calls)}
			if address == program {
				data = programData
			}
			values[i] = map[string]any{"owner": "BPFLoaderUpgradeab1e11111111111111111111111", "lamports": calls, "executable": address == program, "data": []string{base64.StdEncoding.EncodeToString(data), "base64"}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"context": map[string]int{"slot": 500 + calls}, "value": values}})
	}))
	defer server.Close()
	accounts, slots, err := captureShadowAccounts(context.Background(), server.URL, []string{program}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || len(accounts) != 2 || len(slots) != 1 || slots[0] != 503 {
		t.Fatalf("non-atomic capture: calls=%d accounts=%v slots=%v", calls, accounts, slots)
	}
	for _, account := range accounts {
		if account.Lamports != 3 {
			t.Fatal("retained an older account version")
		}
	}
}

func TestInvestmentShadowPreservesUnderlyingPriceFailure(t *testing.T) {
	err := costOutage(stagePrice, fmt.Errorf("observation: %w", errors.New("behind current slot")))
	if detail := shadowError(err); !strings.Contains(detail, "leverage_cost_unavailable(price)") || !strings.Contains(detail, "behind current slot") {
		t.Fatalf("lost diagnostic: %s", detail)
	}
}

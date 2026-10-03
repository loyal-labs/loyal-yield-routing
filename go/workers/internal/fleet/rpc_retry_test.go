package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRPCTransientRetriesPreserveConfirmedSlotFence(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req struct {
			Params []json.RawMessage `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var opts struct {
			Commitment string `json:"commitment"`
			Min        int64  `json:"minContextSlot"`
		}
		json.Unmarshal(req.Params[1], &opts)
		if opts.Min != 1000 || opts.Commitment != "confirmed" {
			t.Error("retry weakened observation fence")
		}
		if calls < 4 {
			fmt.Fprint(w, `{"error":{"code":-32016,"message":"provider-secret"}}`)
			return
		}
		fmt.Fprint(w, `{"result":{"context":{"slot":1000},"value":[{"owner":"owner","lamports":1,"data":["","base64"]}]}}`)
	}))
	defer server.Close()
	c := NewRPCClient(server.URL)
	c.retryDelay = func(int) time.Duration { return 0 }
	slot, accounts, err := c.ConfirmedAccounts(context.Background(), []string{"reserve"}, 1000)
	if err != nil || calls != 4 || slot != 1000 || len(accounts) != 1 {
		t.Fatalf("calls=%d slot=%d error=%v", calls, slot, err)
	}
}

func TestRPCRetryClassificationAndBound(t *testing.T) {
	for _, tc := range []struct {
		name, body, after string
		status, calls     int
	}{
		{"lag", `{"error":{"code":-32016,"message":"provider-secret"}}`, "", 200, 5},
		{"unhealthy", `{"error":{"code":-32005}}`, "", 200, 5},
		{"invalid_params", `{"error":{"code":-32602,"message":"provider-secret"}}`, "", 200, 1},
		{"rate_limit", "", "0", 429, 5},
		{"long_rate_limit", "", "60", 429, 1},
		{"unavailable", "", "", 503, 5},
		{"unauthorized", "", "", 401, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Retry-After", tc.after)
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer s.Close()
			c := NewRPCClient(s.URL)
			c.retryDelay = func(int) time.Duration { return 0 }
			_, err := c.ConfirmedSlot(context.Background())
			if err == nil || calls != tc.calls || strings.Contains(err.Error(), "provider-secret") {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
			if tc.name == "lag" && !strings.Contains(err.Error(), "-32016") {
				t.Fatal("lost safe provider error code")
			}
		})
	}
}

func TestRPCCancellationInterruptsBackoff(t *testing.T) {
	calls := 0
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; fmt.Fprint(w, `{"error":{"code":-32016}}`) }))
	defer s.Close()
	c := NewRPCClient(s.URL)
	c.retryDelay = func(int) time.Duration { return time.Hour }
	_, err := c.ConfirmedSlot(ctx)
	if err != context.DeadlineExceeded || calls != 1 {
		t.Fatalf("cancelled call made requests: calls=%d error=%v", calls, err)
	}
}

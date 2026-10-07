package fleetexec

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLookupRPCSanitizesQueryCredentialAndRedirectErrors(t *testing.T) {
	for _, kind := range []string{"provider", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			followed := false
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { followed = true; w.WriteHeader(200) }))
			defer destination.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if kind == "redirect" {
					http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
					return
				}
				_, _ = w.Write([]byte(`{"error":{"code":-32000,"message":"private-secret-credential"}}`))
			}))
			defer server.Close()
			rpc, err := NewLookupRPC(server.URL+"?api-key=private-secret-credential", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = rpc.LookupBalance(context.Background(), readLookupFixture(t).Manager)
			if err == nil || strings.Contains(err.Error(), "private-secret") || followed {
				t.Fatalf("credential rendered or redirect followed: %v %v", err, followed)
			}
		})
	}
}
func TestLookupSnapshotDecodesOfficialPaddedSlotHashes(t *testing.T) {
	f := readLookupFixture(t)
	var account struct {
		Owner      string
		Lamports   uint64
		Data       string
		Executable bool
	}
	if err := json.Unmarshal(f.Accounts["SysvarS1otHashes111111111111111111111111111"], &account); err != nil {
		t.Fatal(err)
	}
	falseValue := false
	makeHashes := func(data string) *lookupRPCAccount {
		return &lookupRPCAccount{Owner: account.Owner, Lamports: &account.Lamports, Executable: &falseValue, Data: []json.RawMessage{json.RawMessage(`"` + data + `"`), json.RawMessage(`"base64"`)}}
	}
	out, err := decodeLookupSnapshot(f.Table, 1000, nil, makeHashes(account.Data))
	if err != nil || !out.Absent || len(out.SlotHashes) != 2 || out.SlotHashes[0] != 999 {
		t.Fatalf("actual official fixture: %+v %v", out, err)
	}
	d, err := base64.StdEncoding.DecodeString(account.Data)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"future", "padding", "wrong-owner"} {
		t.Run(kind, func(t *testing.T) {
			copyData := append([]byte(nil), d...)
			if kind == "future" {
				binary.LittleEndian.PutUint64(copyData[8:], 1001)
			}
			if kind == "padding" {
				copyData[len(copyData)-1] = 1
			}
			a := makeHashes(base64.StdEncoding.EncodeToString(copyData))
			if kind == "wrong-owner" {
				a.Owner = f.Manager
			}
			if _, err := decodeLookupSnapshot(f.Table, 1000, nil, a); err == nil {
				t.Fatal("malformed cooldown proof accepted")
			}
		})
	}
}

// These are log/security contracts: provider messages and request credentials
// must not escape even through wrapping, debug formatting or structured logs.
func TestLookupRPCDiagnosticBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, body, class string
		status, code      int
	}{
		{"rpc", `{"error":{"code":-32016,"message":"private-secret-provider","data":{"url":"private-secret-url"}}}`, "json_rpc", 200, -32016},
		{"rate_limit", "private-secret-body", "http", 429, 0},
		{"decode", `{"error":{"code":"private-secret-code"}}`, "decode", 200, 0},
		{"result_decode", `{"result":{"value":{"private-secret-key":"private-secret-value"}}}`, "result_decode", 200, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			rpc, err := NewLookupRPC(server.URL+"/private-secret-path?api-key=private-secret-query", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = rpc.LookupBalance(context.Background(), readLookupFixture(t).Manager)
			var failure *LookupRPCError
			if !errors.As(err, &failure) || calls.Load() != 1 {
				t.Fatalf("missing failure or unexpected retry: %v", err)
			}
			wrapped := fmt.Errorf("lookup failed: %w", err)
			var unsafe *rpcError
			if errors.As(wrapped, &unsafe) || errors.Unwrap(failure) != nil {
				t.Fatal("unsafe provider cause retained")
			}
			var log bytes.Buffer
			slog.New(slog.NewJSONHandler(&log, nil)).Error("lookup", "error", wrapped, "rpc", failure)
			for _, rendered := range []string{fmt.Sprintf("%v %+v %#v %q", err, err, err, err), fmt.Sprintf("%v %+v %#v", wrapped, wrapped, wrapped), log.String()} {
				if strings.Contains(rendered, "private-secret") || strings.Contains(rendered, server.URL) {
					t.Fatal("lookup diagnostics exposed private data")
				}
			}
			var event struct {
				RPC struct {
					Method, Class string
					Status        int `json:"http_status"`
					Code          int
					Duration      int64 `json:"duration_ms"`
				}
			}
			if err := json.Unmarshal(log.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			if event.RPC.Method != "getBalance" || event.RPC.Class != tc.class || event.RPC.Status != tc.status || event.RPC.Code != tc.code || event.RPC.Duration < 0 {
				t.Fatalf("wrong safe log diagnosis: %s", log.String())
			}
		})
	}
}

func TestLookupRPCTimeoutsPreserveContextBoundary(t *testing.T) {
	for _, scope := range []string{"inner", "outer", "canceled", "transport", "unknown_method"} {
		t.Run(scope, func(t *testing.T) {
			rpc, err := NewLookupRPC("https://rpc.invalid/private-secret-path?key=private-secret-query", 10*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			var cancel context.CancelFunc
			switch scope {
			case "outer":
				ctx, cancel = context.WithTimeout(ctx, time.Millisecond)
			case "canceled":
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if cancel != nil {
				defer cancel()
			}
			rpc.adapter.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if scope == "transport" || scope == "unknown_method" {
					return nil, fmt.Errorf("private-secret-transport %s", req.URL)
				}
				<-req.Context().Done()
				return nil, fmt.Errorf("private-secret-transport: %w", req.Context().Err())
			})
			method := "getBalance"
			if scope == "unknown_method" {
				method = "private-secret-method"
			}
			err = rpc.call(ctx, nil, method)
			var failure *LookupRPCError
			if !errors.As(err, &failure) {
				t.Fatalf("missing diagnostic: %v", err)
			}
			wantClass := "timeout"
			if scope == "canceled" {
				wantClass = "canceled"
			}
			if scope == "transport" || scope == "unknown_method" {
				wantClass = "transport"
			}
			if failure.class != wantClass || errors.Is(err, context.DeadlineExceeded) != (scope == "outer") || errors.Is(err, context.Canceled) != (scope == "canceled") {
				t.Fatalf("context boundary changed: %v class=%s", err, failure.class)
			}
			if scope == "inner" && failure.durationMS < 1 {
				t.Fatal("RPC elapsed time missing")
			}
			if scope == "unknown_method" && failure.method != "unknown" {
				t.Fatal("unbounded method retained")
			}
			for current := err; current != nil; current = errors.Unwrap(current) {
				if strings.Contains(fmt.Sprintf("%v %+v %#v", current, current, current), "private-secret") {
					t.Fatal("transport secret escaped")
				}
			}
		})
	}
}

func TestLookupPlannerReportsRPCStageWithoutContinuing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }))
	defer server.Close()
	rpc, err := NewLookupRPC(server.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var reported error
	// No store: a failed blockhash read must return before any DB work.
	planner := &LookupPlanner{chain: rpc, gate: make(chan struct{}, 1), config: LookupPlannerConfig{TickDeadline: time.Second, OnHealth: func(err error) { reported = err }}}
	worked, err := planner.Tick(context.Background())
	var failure *LookupRPCError
	if worked || !errors.As(reported, &failure) || reported != err || failure.stage != "blockhash" {
		t.Fatalf("stage/report boundary changed: %v", err)
	}
}

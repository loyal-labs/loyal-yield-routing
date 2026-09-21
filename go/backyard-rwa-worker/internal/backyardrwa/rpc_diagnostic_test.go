package backyardrwa

// Focused regression coverage for the callOnce failure diagnostics: every
// failure stage renders only safe fixed tokens (stage, allowlisted method,
// transport class, numeric HTTP/JSON-RPC code, sanitized scheme://host) while
// credential-bearing transport errors, RPC error messages, bodies, and wire
// material are classified and then dropped. The tests also pin the exact
// retry counts, the single sendTransaction attempt, and the errors.Is
// semantics callers rely on — the diagnostic cause is inspected but never
// unwrapped, so context.Canceled and context.DeadlineExceeded match decisions
// are unchanged.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

const (
	diagSecretURL = "https://rpc-user:rpc-pass@rpc.example:8899/rpc?api-key=secret-token"
	diagSecretRPC = `{"jsonrpc":"2.0","id":1,"error":{"code":-32004,"message":"node said postgresql://admin:hunter2@db.internal/neon -- leaks"}}`
)

type diagTimeoutError struct{}

func (diagTimeoutError) Error() string {
	return `dial tcp "https://rpc-user:rpc-pass@rpc.example": timed out`
}
func (diagTimeoutError) Timeout() bool   { return true }
func (diagTimeoutError) Temporary() bool { return false }

type diagNetError struct{ timeout bool }

func (e diagNetError) Error() string   { return "read: connection reset by peer" }
func (e diagNetError) Timeout() bool   { return e.timeout }
func (e diagNetError) Temporary() bool { return false }

var _ net.Error = diagNetError{}
var _ net.Error = diagTimeoutError{}

func newDiagnosticClient(t *testing.T) *RPCClient {
	t.Helper()
	client, err := NewRPCClient("https://rpc.invalid")
	if err != nil {
		t.Fatal(err)
	}
	client.retryBackoff = 0
	return client
}

func assertNoLeak(t *testing.T, err error) {
	t.Helper()
	rendered := err.Error()
	for _, secret := range []string{
		"rpc-pass", "secret-token", "rpc-user", ":8899/rpc", "hunter2",
		"db.internal", "admin:", "timed out", "connection reset", "node said",
		"postgresql://", "https://rpc-user", "not-a-number",
		"rpc.invalid", "://",
	} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("rpc diagnostic leaked %q: %v", secret, rendered)
		}
	}
}

// diagErrorBody is a response body whose read fails with a caller-supplied
// error, modelling a response stream cut by the network or context.
type diagErrorBody struct{ err error }

func (b diagErrorBody) Read([]byte) (int, error) { return 0, b.err }
func (b diagErrorBody) Close() error             { return nil }

func TestRPCDiagnosticsTransportTimeoutClassifiesWithoutCauseText(t *testing.T) {
	client := newDiagnosticClient(t)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: diagSecretURL, Err: diagTimeoutError{}}
	})
	_, err := client.ConfirmedSlot(context.Background())
	if err == nil {
		t.Fatal("transport timeout unexpectedly succeeded")
	}
	assertNoLeak(t, err)
	var failure *rpcError
	if !errors.As(err, &failure) {
		t.Fatalf("transport failure is not an rpcError: %v", err)
	}
	if failure.stage != rpcStageTransport || failure.class != rpcClassNetworkTimeout {
		t.Fatalf("stage=%s class=%s", failure.stage, failure.class)
	}
	if failure.method != "getSlot" {
		t.Fatalf("method token %q is not the allowlisted literal", failure.method)
	}
	if !strings.Contains(err.Error(), "stage=transport class=network_timeout") {
		t.Fatalf("diagnostic tokens missing: %v", err)
	}
	// The cause was classified, not propagated: errors.Is decisions that were
	// false before diagnostics keep being false.
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		t.Fatalf("diagnostic error unwraps to the dropped transport cause: %v", err)
	}
}

func TestRPCDiagnosticsTransportContextCauseNeverUnwraps(t *testing.T) {
	client := newDiagnosticClient(t)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &url.Error{Op: "Post", URL: diagSecretURL, Err: context.Canceled}
	})
	_, err := client.ConfirmedSlot(context.Background())
	if err == nil {
		t.Fatal("canceled transport unexpectedly succeeded")
	}
	assertNoLeak(t, err)
	var failure *rpcError
	if !errors.As(err, &failure) || failure.class != rpcClassContextCanceled {
		t.Fatalf("class=%v", failure)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("canceled transport error now matches context.Canceled, changing caller semantics: %v", err)
	}
}

func TestRPCDiagnosticsTransportOtherIsFixedClass(t *testing.T) {
	client := newDiagnosticClient(t)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, diagNetError{timeout: false}
	})
	_, err := client.ConfirmedSlot(context.Background())
	if err == nil {
		t.Fatal("transport failure unexpectedly succeeded")
	}
	assertNoLeak(t, err)
	var failure *rpcError
	if !errors.As(err, &failure) || failure.class != rpcClassTransportOther {
		t.Fatalf("class=%v", failure)
	}
}

func TestRPCDiagnosticsHTTPStageCarriesNumericStatusOnly(t *testing.T) {
	client := newDiagnosticClient(t)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Body:       io.NopCloser(strings.NewReader("gateway error rpc-pass secret-token")),
			Header:     make(http.Header),
		}, nil
	})
	_, err := client.SignatureStatus(context.Background(), "sig")
	if err == nil {
		t.Fatal("HTTP failure unexpectedly succeeded")
	}
	assertNoLeak(t, err)
	if !strings.Contains(err.Error(), "returned HTTP 503: stage=http") {
		t.Fatalf("http tokens missing: %v", err)
	}
	var failure *rpcError
	if !errors.As(err, &failure) || failure.status != http.StatusServiceUnavailable {
		t.Fatalf("status=%v", failure)
	}
}

func TestRPCDiagnosticsEnvelopeKeepsCodeDropsMessage(t *testing.T) {
	client := newDiagnosticClient(t)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(diagSecretRPC), nil
	})
	_, err := client.ConfirmedBlockTime(context.Background(), 7)
	if err == nil {
		t.Fatal("envelope failure unexpectedly succeeded")
	}
	assertNoLeak(t, err)
	if !strings.Contains(err.Error(), "stage=rpc_envelope code=-32004") {
		t.Fatalf("envelope tokens missing: %v", err)
	}
	if !errors.Is(err, errConfirmedObservationUnavailable) {
		t.Fatalf("confirmed-read wrapper lost the transient sentinel: %v", err)
	}
}

func TestRPCDiagnosticsEnvelopeWithoutNumericCodeRendersNone(t *testing.T) {
	client := newDiagnosticClient(t)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(`{"jsonrpc":"2.0","id":1,"error":"flat string error rpc-pass"}`), nil
	})
	_, err := client.GenesisHash(context.Background())
	if err == nil {
		t.Fatal("envelope failure unexpectedly succeeded")
	}
	assertNoLeak(t, err)
	if strings.Contains(err.Error(), "code=") {
		t.Fatalf("non-numeric envelope rendered a code: %v", err)
	}
	if !strings.Contains(err.Error(), "stage=rpc_envelope") {
		t.Fatalf("stage token missing: %v", err)
	}
}

func TestRPCDiagnosticsNoResultAndDecodeStagesStaySafe(t *testing.T) {
	client := newDiagnosticClient(t)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(`{"jsonrpc":"2.0","id":1,"result":null}`), nil
	})
	_, err := client.FinalizedSlot(context.Background())
	if err == nil || !strings.Contains(err.Error(), "returned no result: stage=rpc_envelope") {
		t.Fatalf("no-result diagnostics wrong: %v", err)
	}
	assertNoLeak(t, err)

	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(`{"jsonrpc":"2.0","id":1,"resul postgresql://admin:hunter2@db.internal"`), nil
	})
	_, err = client.ConfirmedSlot(context.Background())
	if err == nil || !strings.Contains(err.Error(), "stage=decode") {
		t.Fatalf("decode diagnostics wrong: %v", err)
	}
	assertNoLeak(t, err)
}

func TestRPCDiagnosticsResultDecodeStageNeverQuotesShape(t *testing.T) {
	client := newDiagnosticClient(t)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(`{"jsonrpc":"2.0","id":1,"result":{"context":{"slot":"not-a-number rpc-pass"}}}`), nil
	})
	_, err := client.ConfirmedSlot(context.Background())
	if err == nil {
		t.Fatal("result decode failure unexpectedly succeeded")
	}
	assertNoLeak(t, err)
	if !strings.Contains(err.Error(), "stage=result_decode") {
		t.Fatalf("result decode token missing: %v", err)
	}
	// The result decode error was previously returned raw; its json identity
	// stays reachable through Unwrap even though Error() renders only fixed
	// tokens.
	var typeError *json.UnmarshalTypeError
	if !errors.As(err, &typeError) {
		t.Fatalf("result decode lost the json.UnmarshalTypeError identity: %v", err)
	}
}

func TestRPCDiagnosticsDecodeReaderCancellationKeepsSentinelIdentity(t *testing.T) {
	client := newDiagnosticClient(t)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: diagErrorBody{errors.New(
				`read tcp rpc-user:rpc-pass@rpc.example:8899: use of closed connection api-key=secret-token`)},
		}, nil
	})
	_, err := client.ConfirmedSlot(context.Background())
	if err == nil {
		t.Fatal("decode read failure unexpectedly succeeded")
	}
	assertNoLeak(t, err)
	if !strings.Contains(err.Error(), "stage=decode class=transport_other") {
		t.Fatalf("decode read tokens missing: %v", err)
	}
	if !errors.Is(err, errConfirmedObservationUnavailable) {
		t.Fatalf("decode failure lost the transient sentinel: %v", err)
	}
}

func TestRPCDiagnosticsDecodeReaderContextCancelIdentityAndClass(t *testing.T) {
	client := newDiagnosticClient(t)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: diagErrorBody{&url.Error{
				Op: "read", URL: diagSecretURL, Err: context.Canceled,
			}},
		}, nil
	})
	_, err := client.ConfirmedSlot(context.Background())
	if err == nil {
		t.Fatal("canceled decode read unexpectedly succeeded")
	}
	assertNoLeak(t, err)
	if !strings.Contains(err.Error(), "stage=decode class=context_canceled") {
		t.Fatalf("canceled decode classification missing: %v", err)
	}
	// These identities were raw before diagnostics: the context cause from
	// the failing response reader stays reachable, and the confirmed-read
	// wrapper still exposes the transient sentinel.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("decode lost the context.Canceled identity: %v", err)
	}
	if !errors.Is(err, errConfirmedObservationUnavailable) {
		t.Fatalf("decode lost the transient sentinel: %v", err)
	}
}

func TestRPCDiagnosticsDecodeReaderTimeoutIdentityAndClass(t *testing.T) {
	client := newDiagnosticClient(t)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       diagErrorBody{&url.Error{Op: "read", URL: diagSecretURL, Err: diagTimeoutError{}}},
		}, nil
	})
	_, err := client.ConfirmedBlockTime(context.Background(), 7)
	if err == nil {
		t.Fatal("timed-out decode read unexpectedly succeeded")
	}
	assertNoLeak(t, err)
	if !strings.Contains(err.Error(), "stage=decode class=network_timeout") {
		t.Fatalf("network timeout classification missing: %v", err)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("decode lost the net.Error identity: %v", err)
	}
}

func TestRPCDiagnosticsDecodeSyntaxErrorIdentityPreserved(t *testing.T) {
	client := newDiagnosticClient(t)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(`{postgresql://admin:hunter2@db.internal}`), nil
	})
	_, err := client.ConfirmedSlot(context.Background())
	if err == nil {
		t.Fatal("malformed envelope unexpectedly succeeded")
	}
	assertNoLeak(t, err)
	if !strings.Contains(err.Error(), "stage=decode") {
		t.Fatalf("decode token missing: %v", err)
	}
	// The raw decode path used to surface json.SyntaxError directly; the
	// identity survives while the rendered text stays fixed.
	var syntaxError *json.SyntaxError
	if !errors.As(err, &syntaxError) {
		t.Fatalf("decode lost the json.SyntaxError identity: %v", err)
	}
}

func TestRPCDiagnosticsMethodTokenIsAllowlisted(t *testing.T) {
	client := newDiagnosticClient(t)
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("transport down rpc-pass")
	})
	if _, err := client.ConfirmedSlot(context.Background()); err == nil {
		t.Fatal("expected failure")
	} else if !strings.Contains(err.Error(), "RPC getSlot ") {
		t.Fatalf("allowlisted method missing: %v", err)
	}
	// A method outside the fixed allowlist renders the fixed unknown token.
	output := struct {
		Optional *string `json:"optional"`
		Required int     `json:"required"`
	}{}
	if err := client.call(context.Background(), "getReadOnlyFixture", nil, &output); err == nil {
		t.Fatal("expected failure")
	} else if !strings.Contains(err.Error(), "RPC unknown ") || strings.Contains(err.Error(), "getReadOnlyFixture") {
		t.Fatalf("unknown method not tokenized: %v", err)
	}
}

func TestRPCDiagnosticsPreserveRetryCountsAndSingleSend(t *testing.T) {
	client := newDiagnosticClient(t)
	readAttempts := 0
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		readAttempts++
		return nil, &url.Error{Op: "Post", URL: diagSecretURL, Err: diagNetError{timeout: true}}
	})
	if _, err := client.ConfirmedSlot(context.Background()); err == nil {
		t.Fatal("exhausted transport unexpectedly succeeded")
	}
	if readAttempts != readOnlyRPCAttempts {
		t.Fatalf("read attempted %d times, want %d", readAttempts, readOnlyRPCAttempts)
	}

	sendAttempts := 0
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		sendAttempts++
		return nil, errors.New("connection refused rpc-pass")
	})
	_, err := client.SendSignedTransactionOnce(context.Background(), []byte{1}, "expected")
	if err == nil {
		t.Fatal("failed send unexpectedly succeeded")
	}
	if sendAttempts != 1 {
		t.Fatalf("sendTransaction was attempted %d times", sendAttempts)
	}
	assertNoLeak(t, err)
	var failure *rpcError
	if !errors.As(err, &failure) || failure.method != "sendTransaction" || failure.stage != rpcStageTransport {
		t.Fatalf("send diagnostics wrong: %v", err)
	}
}

func TestRPCDiagnosticsContextCancellationStillMatchesDirectly(t *testing.T) {
	client := newDiagnosticClient(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	_, err := client.ConfirmedSlot(ctx)
	// The pre-attempt ctx.Err() path is untouched: callers still match
	// context.Canceled on it, exactly as before diagnostics existed.
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("direct cancellation lost errors.Is semantics: %v", err)
	}
}

func TestRPCDiagnosticsRequestStageHidesUnusableURL(t *testing.T) {
	client := newDiagnosticClient(t)
	client.url = "http://\x7f"
	_, err := client.ConfirmedSlot(context.Background())
	if err == nil {
		t.Fatal("request construction unexpectedly succeeded")
	}
	assertNoLeak(t, err)
	if !strings.Contains(err.Error(), "stage=request") {
		t.Fatalf("request stage token missing: %v", err)
	}
	if strings.Contains(err.Error(), "\x7f") {
		t.Fatalf("request diagnostics echoed URL material: %v", err)
	}
}

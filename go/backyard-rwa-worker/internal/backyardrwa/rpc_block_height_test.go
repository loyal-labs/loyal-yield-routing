package backyardrwa

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFinalizedBlockHeightCachesOnlyPositiveAnswers(t *testing.T) {
	calls, answer := 0, `{"jsonrpc":"2.0","id":1,"result":{"blockHeight":0}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = w.Write([]byte(answer))
	}))
	defer server.Close()
	rpc := &RPCClient{url: server.URL, client: server.Client(), retryBackoff: time.Millisecond}
	ctx := context.Background()
	if _, err := rpc.FinalizedBlockHeightForSlot(ctx, 777); err == nil {
		t.Fatal("zero height accepted")
	}
	answer = `{"jsonrpc":"2.0","id":1,"result":{"blockHeight":4242}}`
	for i := 0; i < 3; i++ {
		if height, err := rpc.FinalizedBlockHeightForSlot(ctx, 777); err != nil || height != 4242 {
			t.Fatal(height, err)
		}
	}
	if calls != 2 {
		t.Fatalf("zero answer was cached or the height was refetched: %d calls", calls)
	}
	// Another slot is its own question.
	if _, err := rpc.FinalizedBlockHeightForSlot(ctx, 778); err != nil || calls != 3 {
		t.Fatal("other slot served from the cache", calls, err)
	}
}

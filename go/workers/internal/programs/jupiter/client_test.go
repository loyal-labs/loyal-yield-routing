package jupiter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestClientSharesRateLimitCooldownWithoutReturningStaleQuotes(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("X-Ratelimit-Reset", strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"limited"}`))
			return
		}
		_, _ = w.Write([]byte(`{"outAmount":"42"}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, "", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		quote, err := client.Quote(context.Background(), QuoteRequest{Amount: 1})
		var status *StatusError
		if !errors.As(err, &status) || status.Code != 429 || len(quote.Raw) != 0 {
			t.Fatalf("limited quote = %+v, %v", quote, err)
		}
	}
	if calls != 1 {
		t.Fatalf("cooldown sent %d requests", calls)
	}
	client.mu.Lock()
	client.retryAfter = time.Now().Add(-time.Second)
	client.mu.Unlock()
	quote, err := client.Quote(context.Background(), QuoteRequest{Amount: 1})
	if err != nil || quote.OutAmount != "42" || calls != 2 {
		t.Fatalf("fresh quote after cooldown = %+v, %v; calls %d", quote, err, calls)
	}
}

func TestRetryTime(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	for _, tc := range []struct {
		retry, reset string
		wait         time.Duration
	}{
		{"12", "", 12 * time.Second},
		{now.Add(15 * time.Second).Format(http.TimeFormat), "", 15 * time.Second},
		{"2", strconv.FormatInt(now.Add(20*time.Second).Unix(), 10), 20 * time.Second},
		{"bad", "bad", 5 * time.Second},
		{"-1", strconv.FormatInt(now.Add(-time.Second).Unix(), 10), 5 * time.Second},
	} {
		headers := http.Header{}
		headers.Set("Retry-After", tc.retry)
		headers.Set("X-Ratelimit-Reset", tc.reset)
		if got := retryTime(headers, now).Sub(now); got != tc.wait {
			t.Errorf("retry %q reset %q: wait %v, want %v", tc.retry, tc.reset, got, tc.wait)
		}
	}
}

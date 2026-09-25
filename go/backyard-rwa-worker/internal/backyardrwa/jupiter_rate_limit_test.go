package backyardrwa

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestJupiterRetriesRateLimitWithSameBody(t *testing.T) {
	jupiterRateLimitBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	for _, limited := range []int{2, 3} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			if body, _ := io.ReadAll(r.Body); string(body) != `{"q":1}` {
				t.Fatal("retry lost the request body", string(body))
			}
			if calls <= limited {
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		}))
		client, _ := newJupiterClient(server.URL, server.Client())
		request, _ := http.NewRequest(http.MethodPost, server.URL, bytes.NewReader([]byte(`{"q":1}`)))
		_, err := client.doJSON(request)
		server.Close()
		if limited == 2 && (err != nil || calls != 3) {
			t.Fatal("two rate limits were not retried", calls, err)
		}
		if limited == 3 && (err == nil || calls != 3) {
			t.Fatal("retries were not bounded", calls, err)
		}
	}
}

package webapp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestMobileEarnSigningRouteRefusesOneWalletPastItsBudget(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	upstream, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(Config{Upstream: upstream})
	body := `{"walletAddress":"7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU","signature":"s","issuedAt":"2026-10-22T00:00:00Z"}`
	for i := 1; i <= 13; i++ {
		req := httptest.NewRequest(http.MethodPost, "https://next.askloyal.com/api/smart-accounts/mobile/earn/deposit/prepare", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:40000"
		req.Header.Set("CF-Connecting-IP", fmt.Sprintf("198.51.100.%d", i))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		want := http.StatusOK
		if i == 13 {
			want = http.StatusTooManyRequests
		}
		if response.Code != want {
			t.Fatalf("request %d: status %d, want %d", i, response.Code, want)
		}
		if i == 13 && response.Header().Get("Retry-After") == "" {
			t.Fatal("429 without Retry-After")
		}
	}
}

func TestMobileEarnSigningRouteIgnoresWalletValuesThatAreNotSolanaAddresses(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	upstream, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(Config{Upstream: upstream})
	body := `{"walletAddress":"` + strings.Repeat("A", 60000) + `"}`
	for i := 1; i <= 13; i++ {
		req := httptest.NewRequest(http.MethodPost, "https://next.askloyal.com/api/smart-accounts/mobile/earn/deposit/prepare", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:40000"
		req.Header.Set("CF-Connecting-IP", fmt.Sprintf("198.51.100.%d", i))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusOK {
			t.Fatalf("request %d: status %d, want 200 (oversized wallet must not become a bucket key)", i, response.Code)
		}
	}
}

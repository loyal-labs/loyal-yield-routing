package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReadOnlyTransport(t *testing.T) {
	for _, test := range []struct {
		name, method, url, body string
		allowed                 bool
	}{
		{"accounts", "POST", "https://rpc.test", `{"method":"getProgramAccounts"}`, true},
		{"simulation", "POST", "https://rpc.test", `{"method":"simulateTransaction"}`, true},
		{"quote", "GET", "https://api.jup.ag/swap/v1/quote", "", true},
		{"instructions", "POST", "https://api.jup.ag/swap/v1/swap-instructions", `{}`, true},
		{"submit", "POST", "https://rpc.test", `{"method":"sendTransaction"}`, false},
		{"bundle", "POST", "https://rpc.test", `{"method":"sendBundle"}`, false},
		{"batch", "POST", "https://rpc.test", `[{"method":"getSlot"},{"method":"sendTransaction"}]`, false},
		{"other post", "POST", "https://rpc.test", `{}`, false},
		{"invalid", "POST", "https://rpc.test", `garbage`, false},
		{"write", "DELETE", "https://rpc.test", "", false},
		{"lookalike", "POST", "https://api.jup.ag.evil.test/swap/v1/swap-instructions", `{}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			guard := readOnlyTransport{base: transportFunc(func(r *http.Request) (*http.Response, error) {
				called = true
				if r.Body != nil {
					body, _ := io.ReadAll(r.Body)
					if string(body) != test.body {
						t.Fatal("guard changed request")
					}
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})}
			r, err := http.NewRequest(test.method, test.url, strings.NewReader(test.body))
			if err != nil {
				t.Fatal(err)
			}
			_, err = guard.RoundTrip(r)
			if called != test.allowed || (err == nil) != test.allowed {
				t.Fatalf("allowed=%v called=%v err=%v", test.allowed, called, err)
			}
		})
	}
}

package jupiter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The follower's select evaluates Done only after registering with singleflight.
type joiningContext struct {
	context.Context
	joined chan struct{}
	once   sync.Once
}

func (c *joiningContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.joined) })
	return c.Context.Done()
}

func TestBackgroundQuotesCoalesceWithoutCachingExecution(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		_, _ = w.Write([]byte(`{"outAmount":"42","contextSlot":99}`))
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "key", server.Client())
	ctx, cancel := context.WithTimeout(Background(t.Context()), 3*time.Second)
	defer cancel()
	results := make(chan Quote, 12)
	request := QuoteRequest{InputMint: "A", OutputMint: "B", Amount: 100, SlippageBPS: 10, MaxAccounts: 48, InstructionVersion: "V2", Dexes: "AlphaQ"}
	get := func(ctx context.Context) {
		quote, err := client.Quote(ctx, request)
		if err != nil {
			t.Error(err)
		}
		results <- quote
	}
	go get(ctx)
	<-started
	for range 11 {
		follower := &joiningContext{Context: ctx, joined: make(chan struct{})}
		go get(follower)
		<-follower.joined
	}
	// Fresh execution must not join the blocked background read.
	execution, err := client.Quote(t.Context(), request)
	if err != nil || execution.OutAmount != "42" || calls.Load() != 2 {
		t.Fatalf("execution shared discovery evidence: %v", err)
	}
	close(release)
	var first Quote
	for i := range 12 {
		q := <-results
		if q.OutAmount != "42" || !json.Valid(q.Raw) {
			t.Fatalf("shared quote corrupted: %+v", q)
		}
		if i == 0 {
			first = q
			first.Raw[0] = '!'
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("12 identical reads + execution made %d requests, want 2", calls.Load())
	}
	if _, err := client.Quote(ctx, request); err != nil || calls.Load() != 3 {
		t.Fatal("completed discovery quote was cached")
	}
	t.Log("12 simultaneous identical discovery reads made 1 HTTP request (11 saved); execution and subsequent discovery fetched fresh quotes")
}

func TestBackgroundQuoteKeysAndCancellation(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		_, _ = w.Write([]byte(`{"outAmount":"1"}`))
	}))
	defer server.Close()
	client, _ := NewClient(server.URL, "key", server.Client())
	ctx := Background(t.Context())
	base := QuoteRequest{InputMint: "A", OutputMint: "B", Amount: 100, SlippageBPS: 10, MaxAccounts: 48, InstructionVersion: "V2", Dexes: "AlphaQ"}
	done := make(chan error, 1)
	go func() { _, err := client.Quote(ctx, base); done <- err }()
	<-started
	variants := []QuoteRequest{base, base, base, base, base, base, base}
	variants[0].InputMint = "C"
	variants[1].OutputMint = "C"
	variants[2].Amount++
	variants[3].SlippageBPS++
	variants[4].MaxAccounts--
	variants[5].InstructionVersion = "V1"
	variants[6].Dexes = "Other"
	for _, request := range variants {
		bounded, cancel := context.WithTimeout(ctx, time.Second)
		_, err := client.Quote(bounded, request)
		cancel()
		if err != nil {
			t.Errorf("distinct parameters joined blocked request: %v", err)
		}
	}
	bounded, cancel := context.WithCancel(ctx)
	follower := &joiningContext{Context: bounded, joined: make(chan struct{})}
	canceled := make(chan error, 1)
	go func() { _, err := client.Quote(follower, base); canceled <- err }()
	<-follower.joined
	cancel()
	if err := <-canceled; !errors.Is(err, context.Canceled) {
		t.Fatalf("follower did not cancel: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 8 {
		t.Fatalf("key isolation/cancellation sent %d requests, want 8", calls.Load())
	}
}

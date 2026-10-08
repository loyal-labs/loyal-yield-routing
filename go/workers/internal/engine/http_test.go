package engine

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestHTTPServerCancellationWaitsForActiveRequest(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	s, err := ListenHTTP("127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	requestDone := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		response, err := client.Get("http://" + s.listener.Addr().String())
		if err == nil {
			_ = response.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-entered:
	case err := <-requestDone:
		t.Fatalf("request ended before handler: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	select {
	case err := <-done:
		close(release)
		t.Fatalf("server returned with active handler: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("shutdown result: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not join after handler finished")
	}
	if err := <-requestDone; err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Second}
	if response, err := client.Get("http://" + s.listener.Addr().String()); err == nil {
		_ = response.Body.Close()
		t.Fatal("listener survived shutdown")
	}
}

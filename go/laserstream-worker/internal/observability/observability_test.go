package observability

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFatalFailureImmediatelyFailsReadiness(t *testing.T) {
	health := NewHealth()
	health.SetConnected(true)
	health.SetReady(true)
	health.Progress(100)
	handler := health.Handler(time.Minute)

	before := httptest.NewRecorder()
	handler.ServeHTTP(before, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if before.Code != http.StatusOK {
		t.Fatalf("healthy readiness status = %d", before.Code)
	}

	health.Fatal(errors.New("confirmed verification stalled"))
	after := httptest.NewRecorder()
	handler.ServeHTTP(after, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if after.Code != http.StatusServiceUnavailable {
		t.Fatalf("fatal readiness status = %d, want %d", after.Code, http.StatusServiceUnavailable)
	}
}

package observability

import (
	"encoding/json"
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

func TestCaptureProgressAloneDoesNotOpenReadinessGate(t *testing.T) {
	health := NewHealth()
	health.SetConnected(true)
	health.SetReady(true)
	handler := health.Handler(time.Minute)

	// Capture advanced to slot 500 while the registered Earn application gate
	// is still closed (registered gates start closed).
	health.Progress(500)
	health.DomainProgress("earn", 500)
	health.SetDomainReady("earn", false)
	closed := httptest.NewRecorder()
	handler.ServeHTTP(closed, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if closed.Code != http.StatusServiceUnavailable {
		t.Fatalf("capture-only readiness status = %d, want %d", closed.Code, http.StatusServiceUnavailable)
	}

	// Applied progress plus a healthy backlog opens the gate and readiness.
	health.DomainAppliedProgress("earn", 480)
	health.SetDomainReady("earn", true)
	open := httptest.NewRecorder()
	handler.ServeHTTP(open, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if open.Code != http.StatusOK {
		t.Fatalf("applied readiness status = %d, want %d", open.Code, http.StatusOK)
	}
	var body struct {
		DomainApplied map[string]uint64 `json:"domainApplied"`
		DomainGates   map[string]bool   `json:"domainGates"`
		Frontier      uint64            `json:"frontier"`
	}
	if err := json.Unmarshal(open.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.DomainApplied["earn"] != 480 || !body.DomainGates["earn"] || body.Frontier != 500 {
		t.Fatalf("readiness body = %+v, want applied 480, gate true, capture 500", body)
	}

	// A gate that closes again (unhealthy backlog) must hold readiness closed
	// even though capture continues to advance.
	health.Progress(600)
	health.SetDomainReady("earn", false)
	regressed := httptest.NewRecorder()
	handler.ServeHTTP(regressed, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if regressed.Code != http.StatusServiceUnavailable {
		t.Fatalf("unhealthy backlog readiness status = %d, want %d", regressed.Code, http.StatusServiceUnavailable)
	}
}

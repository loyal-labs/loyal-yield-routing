package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func retailKeyMaterialForTest(seed []byte) string {
	key := ed25519.NewKeyFromSeed(seed)
	values := make([]int, len(key))
	for i, value := range key {
		values[i] = int(value)
	}
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

func configureRetailForTest(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		"RETAIL_MODE": "active", "RETAIL_DATABASE_URL": "postgresql://test:test-secret@db.invalid/yield", "RETAIL_TIMESCALE_DATABASE_URL": "postgresql://test:test-secret@db.invalid/market", "RETAIL_SOLANA_RPC_URL": "https://rpc.invalid/?api-key=test-secret", "RETAIL_TIMESCALE_SCHEMA": "kamino", "RETAIL_HTTP_ADDRESS": "127.0.0.1:0", "RETAIL_KLEND_PROXY_PATH": "/unused/test-helper", "RETAIL_KLEND_PROXY_SHA256": strings.Repeat("a", 64), "RETAIL_SLOT_DURATION": "400ms", "RETAIL_CROSS_MINT_ENABLED": "false", "EARN_ROUTER_ENABLE_CROSS_MINT_JUPITER": "false",
	} {
		t.Setenv(name, value)
	}
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 41
	key := retailKeyMaterialForTest(seed)
	t.Setenv("RETAIL_DELEGATE_KEYPAIR", key)
	t.Setenv("RETAIL_FEE_PAYER_KEYPAIR", key)
}

func TestRetailConfigurationDoesNotStartWritersByDefault(t *testing.T) {
	configureRetailForTest(t)
	owner := "worker:retail:test:sha-" + strings.Repeat("a", 40)
	for _, mode := range []string{"", "shadow", "publish"} {
		t.Run("mode-"+mode, func(t *testing.T) {
			t.Setenv("RETAIL_MODE", mode)
			err := runRetail(context.Background(), owner, "sha-"+strings.Repeat("a", 40))
			if err == nil || !strings.Contains(err.Error(), "RETAIL_MODE") {
				t.Fatalf("unapproved writer mode reached initialization: %v", err)
			}
		})
	}
	for _, name := range []string{"RETAIL_CROSS_MINT_ENABLED", "EARN_ROUTER_ENABLE_CROSS_MINT_JUPITER"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "true")
			err := runRetail(context.Background(), owner, "sha-"+strings.Repeat("a", 40))
			if err == nil || !strings.Contains(err.Error(), "cross-mint") {
				t.Fatalf("unwired cross-mint reached initialization: %v", err)
			}
		})
	}
}

func TestRetailConfigurationIsScopedBoundedAndSecretSafe(t *testing.T) {
	configureRetailForTest(t)
	cfg, err := loadRetailConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.slotDuration != 400*time.Millisecond || cfg.fleetConfig().SlotDuration != cfg.slotDuration || !cfg.fleetConfig().FusedExecute {
		t.Fatal("planner/executor clocks or fused preparation drifted")
	}
	cases := []struct{ name, value string }{{"RETAIL_SLOT_DURATION", "0s"}, {"RETAIL_SLOT_DURATION", "11s"}, {"RETAIL_TIMESCALE_SCHEMA", "kamino;test-secret"}, {"RETAIL_SOLANA_RPC_URL", "test-secret"}, {"RETAIL_KLEND_PROXY_SHA256", strings.Repeat("z", 64)}, {"RETAIL_DELEGATE_KEYPAIR", "test-secret"}, {"RETAIL_HTTP_ADDRESS", "test-secret"}, {"RETAIL_CROSS_MINT_ENABLED", "test-secret"}}
	for _, tc := range cases {
		t.Run(tc.name+tc.value, func(t *testing.T) {
			t.Setenv(tc.name, tc.value)
			if _, err := loadRetailConfig(); err == nil || strings.Contains(err.Error(), "test-secret") {
				t.Fatalf("invalid configuration leaked or was accepted: %v", err)
			}
		})
	}
	t.Setenv("RETAIL_DELEGATE_KEYPAIR", "")
	t.Setenv("POLICY_KEYPAIR", "test-secret")
	t.Setenv("BACKYARD_POLICY_KEYPAIR", "test-secret")
	if _, err := loadRetailConfig(); err == nil || !strings.Contains(err.Error(), "RETAIL_DELEGATE_KEYPAIR") {
		t.Fatalf("unscoped credential substituted: %v", err)
	}
}

func TestRetailRejectsTwoKeysWhileAutodepositAndFleetHaveOneSigner(t *testing.T) {
	configureRetailForTest(t)
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 99
	key := retailKeyMaterialForTest(seed)
	t.Setenv("RETAIL_FEE_PAYER_KEYPAIR", key)
	if _, err := loadRetailConfig(); err == nil || !strings.Contains(err.Error(), "same key") {
		t.Fatalf("unsupported signer graph accepted: %v", err)
	}
}

func TestRetailDiagnosticsRetainCancellationWithoutRenderingSecrets(t *testing.T) {
	source := fmt.Errorf("provider https://rpc.invalid/?api-key=test-secret: %w", context.Canceled)
	err := retailError("startup", source)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "test-secret") || strings.Contains(err.Error(), "rpc.invalid") {
		t.Fatalf("unsafe startup diagnostic: %v", err)
	}
	configureRetailForTest(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runRetail(ctx, "invalid-owner", "invalid-release"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled startup proceeded: %v", err)
	}
}

type retailDrainingLane struct{ started, release, drained chan struct{} }

func (l retailDrainingLane) Run(ctx context.Context) error {
	close(l.started)
	<-ctx.Done()
	<-l.release
	close(l.drained)
	return ctx.Err()
}
func TestRetailShutdownJoinsLanesBeforeReturning(t *testing.T) {
	lane := retailDrainingLane{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runRetailLanes(ctx, lane) }()
	select {
	case <-lane.started:
	case <-time.After(time.Second):
		t.Fatal("lane did not start")
	}
	cancel()
	select {
	case err := <-done:
		close(lane.release)
		t.Fatalf("runtime returned before drain: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(lane.release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel identity lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runtime failed to join")
	}
	select {
	case <-lane.drained:
	default:
		t.Fatal("dependencies could close before lane drained")
	}
}

func TestRetailReadinessDoesNotOpenOnConstruction(t *testing.T) {
	health := retailHealth()
	recorder := httptest.NewRecorder()
	health.Handler(time.Minute).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unobserved families reported ready: %d", recorder.Code)
	}
	var body struct {
		Gates map[string]bool `json:"domainGates"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, family := range []string{"autodeposit-control", "autodeposit", "fleet-planner", "fleet-executor", "multiply"} {
		value, known := body.Gates[family]
		if !known || value {
			t.Fatalf("family %s reported readiness without proof", family)
		}
	}
}

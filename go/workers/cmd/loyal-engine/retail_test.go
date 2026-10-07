package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"os"
	"path/filepath"
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

// setCredential stands in for systemd's LoadCredentialEncrypted= directory.
func setCredential(t *testing.T, name, value string) {
	t.Helper()
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" {
		dir = t.TempDir()
		t.Setenv("CREDENTIALS_DIRECTORY", dir)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func configureRetailForTest(t *testing.T) {
	t.Helper()
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	for name, value := range map[string]string{
		"RETAIL_MODE": "active", "RETAIL_TIMESCALE_SCHEMA": "kamino", "RETAIL_SLOT_DURATION": "400ms", "RETAIL_CROSS_MINT_ENABLED": "false", "EARN_ROUTER_ENABLE_CROSS_MINT_JUPITER": "false",
	} {
		t.Setenv(name, value)
	}
	for name, value := range map[string]string{
		"RETAIL_DATABASE_URL": "postgresql://test:test-secret@db.invalid/yield", "RETAIL_TIMESCALE_DATABASE_URL": "postgresql://test:test-secret@db.invalid/market", "RETAIL_SOLANA_RPC_URL": "https://rpc.invalid/?api-key=test-secret", "RETAIL_JUPITER_API_KEY": "test-secret",
	} {
		setCredential(t, name, value)
	}
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 41
	key := retailKeyMaterialForTest(seed)
	setCredential(t, "RETAIL_DELEGATE_KEYPAIR", key)
	setCredential(t, "RETAIL_FEE_PAYER_KEYPAIR", key)
}

// setRetailInput writes credentials to the credential directory and
// everything else to the environment, as the systemd unit does.
func setRetailInput(t *testing.T, name, value string) {
	t.Helper()
	switch name {
	case "RETAIL_DATABASE_URL", "RETAIL_TIMESCALE_DATABASE_URL", "RETAIL_SOLANA_RPC_URL", "RETAIL_JUPITER_API_KEY", "RETAIL_DELEGATE_KEYPAIR", "RETAIL_FEE_PAYER_KEYPAIR", "RETAIL_LOOKUP_MANAGER_KEYPAIR":
		setCredential(t, name, value)
	default:
		t.Setenv(name, value)
	}
}

func TestRetailConfigurationDoesNotStartWritersByDefault(t *testing.T) {
	configureRetailForTest(t)
	owner := "worker:retail:test:sha-" + strings.Repeat("a", 40)
	for _, mode := range []string{"", "shadow", "publish"} {
		t.Run("mode-"+mode, func(t *testing.T) {
			t.Setenv("RETAIL_MODE", mode)
			err := runRetail(context.Background(), owner, nil, nil)
			if err == nil || !strings.Contains(err.Error(), "RETAIL_MODE") {
				t.Fatalf("unapproved writer mode reached initialization: %v", err)
			}
		})
	}
	t.Setenv("EARN_ROUTER_ENABLE_CROSS_MINT_JUPITER", "true")
	if _, err := loadRetailConfig(); err == nil || !strings.Contains(err.Error(), "RETAIL_CROSS_MINT_ENABLED") {
		t.Fatalf("legacy flag granted fresh cross-mint authority: %v", err)
	}
}

func TestRetailCrossMintRequiresScopedAuthorityAndBoundedEconomics(t *testing.T) {
	configureRetailForTest(t)
	t.Setenv("RETAIL_CROSS_MINT_ENABLED", "true")
	t.Setenv("RETAIL_CROSS_MINT_MAX_SLIPPAGE_BPS", "75")
	t.Setenv("RETAIL_CROSS_MINT_MAX_VALUE_LOSS_BPS", "25")
	cfg, err := loadRetailConfig()
	if err != nil || !cfg.crossMintEnabled || !cfg.fleetConfig().CrossMintEnabled || cfg.crossMintMaxSlippageBPS != 75 || cfg.crossMintMaxValueLossBPS != 25 {
		t.Fatalf("scoped planner/controller economics drifted: %v", err)
	}
	for _, test := range []struct{ name, value string }{
		{"RETAIL_CROSS_MINT_MAX_SLIPPAGE_BPS", "0"},
		{"RETAIL_CROSS_MINT_MAX_VALUE_LOSS_BPS", "1001"},
		{"RETAIL_CROSS_MINT_MAX_SLIPPAGE_BPS", "65536"},
		{"RETAIL_JUPITER_BUILD_URL", "http://provider.invalid/test-secret"},
		{"RETAIL_JUPITER_BUILD_URL", "https://test-secret@provider.invalid/build"},
	} {
		t.Run(test.name+test.value, func(t *testing.T) {
			t.Setenv(test.name, test.value)
			if _, err := loadRetailConfig(); err == nil || strings.Contains(err.Error(), "test-secret") {
				t.Fatalf("unsafe cross-mint configuration accepted or leaked: %v", err)
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
	cases := []struct{ name, value string }{{"RETAIL_SLOT_DURATION", "0s"}, {"RETAIL_SLOT_DURATION", "11s"}, {"RETAIL_TIMESCALE_SCHEMA", "kamino;test-secret"}, {"RETAIL_SOLANA_RPC_URL", "test-secret"}, {"RETAIL_DELEGATE_KEYPAIR", "test-secret"}, {"RETAIL_CROSS_MINT_ENABLED", "test-secret"}}
	for _, tc := range cases {
		t.Run(tc.name+tc.value, func(t *testing.T) {
			configureRetailForTest(t)
			setRetailInput(t, tc.name, tc.value)
			if _, err := loadRetailConfig(); err == nil || strings.Contains(err.Error(), "test-secret") {
				t.Fatalf("invalid configuration leaked or was accepted: %v", err)
			}
		})
	}
	setCredential(t, "RETAIL_DELEGATE_KEYPAIR", "")
	t.Setenv("POLICY_KEYPAIR", "test-secret")
	setCredential(t, "POLICY_KEYPAIR", "test-secret")
	t.Setenv("BACKYARD_POLICY_KEYPAIR", "test-secret")
	if _, err := loadRetailConfig(); err == nil || !strings.Contains(err.Error(), "RETAIL_DELEGATE_KEYPAIR") {
		t.Fatalf("unscoped credential substituted: %v", err)
	}
}

// The TS executor's defaults hold when the host sets nothing: $25 of idle
// tolerance and no failed-sweep push. The push needs both credentials.
func TestRetailAutodepositSettingsDefaultToProduction(t *testing.T) {
	configureRetailForTest(t)
	cfg, err := loadRetailConfig()
	if err != nil || cfg.idleToleranceRaw != 25_000_000 || cfg.sweepNotifier != nil {
		t.Fatalf("defaults drifted: tolerance=%d notifier=%v err=%v", cfg.idleToleranceRaw, cfg.sweepNotifier, err)
	}
	t.Setenv("AUTODEPOSIT_IDLE_TOLERANCE_RAW", "1000000")
	setCredential(t, "RETAIL_SWEEP_NOTIFY_ENDPOINT", "https://app.invalid/api/solana-week/sweep-notify")
	setCredential(t, "RETAIL_SWEEP_NOTIFY_SECRET", "test-secret")
	if cfg, err = loadRetailConfig(); err != nil || cfg.idleToleranceRaw != 1_000_000 || cfg.sweepNotifier == nil {
		t.Fatalf("settings ignored: tolerance=%d notifier=%v err=%v", cfg.idleToleranceRaw, cfg.sweepNotifier, err)
	}
	for _, tc := range []struct{ name, value string }{{"AUTODEPOSIT_IDLE_TOLERANCE_RAW", "-1"}, {"RETAIL_SWEEP_NOTIFY_ENDPOINT", "http://app.invalid/test-secret"}, {"RETAIL_SWEEP_NOTIFY_SECRET", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			setCredential(t, "RETAIL_SWEEP_NOTIFY_ENDPOINT", "https://app.invalid/api/solana-week/sweep-notify")
			setCredential(t, "RETAIL_SWEEP_NOTIFY_SECRET", "test-secret")
			if strings.HasPrefix(tc.name, "RETAIL_") {
				setCredential(t, tc.name, tc.value)
			} else {
				t.Setenv(tc.name, tc.value)
			}
			if _, err := loadRetailConfig(); err == nil || strings.Contains(err.Error(), "test-secret") {
				t.Fatalf("invalid autodeposit setting accepted or leaked: %v", err)
			}
		})
	}
}

func TestRetailRejectsTwoKeysWhileAutodepositAndFleetHaveOneSigner(t *testing.T) {
	configureRetailForTest(t)
	seed := make([]byte, ed25519.SeedSize)
	seed[0] = 99
	key := retailKeyMaterialForTest(seed)
	setCredential(t, "RETAIL_FEE_PAYER_KEYPAIR", key)
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
	if err := runRetail(ctx, "invalid-owner", nil, nil); !errors.Is(err, context.Canceled) {
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
	go func() { done <- engine.Run(ctx, lane) }()
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

func TestRetailFamiliesNameEachWriterOnce(t *testing.T) {
	families, err := retailFamilies("fleet, lookup")
	if err != nil || len(families) != 2 || families[0] != engine.FamilyFleet || families[1] != engine.FamilyLookup {
		t.Fatalf("families %v %v", families, err)
	}
	for _, bad := range []string{"", "fleet,fleet", "observer", "backyard"} {
		if _, err := retailFamilies(bad); err == nil {
			t.Fatalf("RETAIL_FAMILIES=%q accepted", bad)
		}
	}
}

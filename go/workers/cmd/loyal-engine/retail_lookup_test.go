package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestRetailLookupAuthorityMatchesSource(t *testing.T) {
	source, err := os.ReadFile("../../../../crates/loyal-solana-env/src/signer.rs")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), `pub const STANDARD_POLICY_AUTHORITY: &str = "`+retailLookupAuthority+`";`) {
		t.Fatal("production lookup authority differs from the retained source")
	}
}

func TestRetailLookupDefaultHasNoKeyCapability(t *testing.T) {
	for _, mode := range []string{"", "reconcile-only"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("RETAIL_LOOKUP_MODE", mode)
			t.Setenv("CREDENTIALS_DIRECTORY", "")
			setCredential(t, "RETAIL_LOOKUP_MANAGER_KEYPAIR", "test-secret")
			t.Setenv("POLICY_KEYPAIR", "test-secret")
			cfg, err := loadRetailLookupConfig()
			if err != nil || cfg.active || cfg.managerKey() != nil || len(cfg.manager) != 0 {
				t.Fatalf("recovery loaded signing authority: %v", err)
			}
		})
	}
}

func TestRetailLookupActiveRequiresSourceAuthorityAndExplicitBudget(t *testing.T) {
	for _, test := range []struct{ mode, maximum, window, material string }{
		{"publish", "1", "24h", ""},
		{"active", "", "24h", ""},
		{"active", "0", "24h", ""},
		{"active", "9223372036854775808", "24h", ""},
		{"active", "1", "59s", ""},
		{"active", "1", "366d", ""},
		{"active", "1", "60.1s", ""},
		{"active", "1", "24h", ""},
		{"active", "1", "24h", "test-secret"},
		{"active", "1", "24h", retailKeyMaterialForTest(make([]byte, ed25519.SeedSize))},
	} {
		t.Run(test.mode+test.maximum+test.window+test.material, func(t *testing.T) {
			t.Setenv("RETAIL_LOOKUP_MODE", test.mode)
			t.Setenv("RETAIL_LOOKUP_MAX_LAMPORTS", test.maximum)
			t.Setenv("RETAIL_LOOKUP_BUDGET_WINDOW", test.window)
			t.Setenv("CREDENTIALS_DIRECTORY", "")
			setCredential(t, "RETAIL_LOOKUP_MANAGER_KEYPAIR", test.material)
			t.Setenv("POLICY_KEYPAIR", "test-secret")
			if _, err := loadRetailLookupConfig(); err == nil || strings.Contains(err.Error(), "test-secret") {
				t.Fatalf("invalid authority accepted or material leaked: %v", err)
			}
		})
	}
}

func TestRetailLookupCallbackRefusesWrongAuthorityAndCancellation(t *testing.T) {
	cfg := retailLookupConfig{active: true, manager: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))}
	callback := cfg.managerKey()
	if _, err := callback(context.Background(), retailLookupAuthority); err == nil {
		t.Fatal("nonstandard manager obtained a signing grant")
	}
	if _, err := callback(context.Background(), "foreign-authority"); err == nil {
		t.Fatal("foreign operation obtained a signing grant")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := callback(ctx, retailLookupAuthority); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled callback obtained signing authority")
	}
}

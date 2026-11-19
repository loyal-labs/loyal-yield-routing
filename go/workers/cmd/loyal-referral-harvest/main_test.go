package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/solana-foundation/solana-go/v2"
)

func TestSimulationDoesNotRequireSigningCredential(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	t.Setenv("REFERRAL_HARVEST_PAYER", solana.NewWallet().PublicKey().String())
	for _, tc := range []struct {
		args    []string
		missing string
	}{
		{nil, "REFERRAL_HARVEST_DATABASE_URL"},
		{[]string{"--execute"}, "REFERRAL_HARVEST_KEYPAIR"},
	} {
		if err := run(t.Context(), tc.args, io.Discard); err == nil || !strings.Contains(err.Error(), tc.missing) {
			t.Fatalf("%v: expected missing %s, got %v", tc.args, tc.missing, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "REFERRAL_HARVEST_KEYPAIR"), []byte("bad-key"), 0600); err != nil {
		t.Fatal(err)
	}
	err := run(t.Context(), []string{"--execute"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("invalid key: got %v", err)
	}
}

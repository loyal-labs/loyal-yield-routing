package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnwindExecutionRequiresExplicitConfirmationFile(t *testing.T) {
	args := []string{"--lane", "OnRe/ONyc/USDC", "--reason", "economic_rotation", "--observation-id", "observed", "--max-collateral-raw", "100", "--max-debt-raw", "50", "--cost-bound-raw", "1", "--evidence-id", strings.Repeat("a", 64)}
	if _, execute, err := parseUnwindIntentFlags(args); err != nil || execute {
		t.Fatalf("unconfirmed dry-run must remain available: %v", err)
	}
	if _, _, err := parseUnwindIntentFlags(append(append([]string{}, args...), "--execute")); err == nil {
		t.Fatal("unconfirmed execution accepted")
	}
	path := filepath.Join(t.TempDir(), "confirmation.json")
	body := `{"requestId":"` + strings.Repeat("b", 64) + `","confirmedBy":"operator","confirmationRecord":"` + strings.Repeat("c", 64) + `","acknowledgeUnavailableReborrow":true,"expiresAt":"2026-10-08T08:00:00Z"}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	confirmed := append(append([]string{}, args...), "--confirmation-file", path, "--execute")
	request, execute, err := parseUnwindIntentFlags(confirmed)
	if err != nil || !execute || request.Confirmation == nil || !request.Confirmation.AcknowledgeUnavailableReborrow {
		t.Fatalf("explicit attestation was not passed to the domain validator: %v", err)
	}
	if _, _, err := parseUnwindIntentFlags(append(confirmed, "--confirmation-file", path)); err == nil {
		t.Fatal("ambiguous duplicate confirmation accepted")
	}
	// A risk-looking CLI reason is not internal emergency authority.
	args[3] = "hard_ltv_reduction"
	if _, _, err := parseUnwindIntentFlags(append(args, "--execute")); err == nil {
		t.Fatal("operator risk label bypassed explicit confirmation")
	}
}

func TestConfirmationFileRejectsAmbiguousOrUnboundedInput(t *testing.T) {
	for _, body := range []string{"null", "{} {}", `{"unknown":true}`, `{"acknowledgeUnavailableReborrow":"yes"}`, "{}" + strings.Repeat(" ", 4096)} {
		path := filepath.Join(t.TempDir(), "confirmation.json")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readDebtClearConfirmation(path); err == nil {
			t.Fatalf("invalid confirmation accepted: %q", body[:min(len(body), 80)])
		}
	}
	if _, err := readDebtClearConfirmation(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("missing confirmation accepted")
	}
}

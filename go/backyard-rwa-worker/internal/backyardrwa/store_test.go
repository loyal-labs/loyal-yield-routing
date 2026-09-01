package backyardrwa

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEveryDecisionHasDurableInitialStatus(t *testing.T) {
	held, reason := initialDecisionStatus(Decision{Action: Hold, Reason: "no_action"})
	if held != Held || reason != nil || IsNonterminal(held) {
		t.Fatalf("HOLD was not terminal and durable: status=%s reason=%v", held, reason)
	}
	manual, reason := initialDecisionStatus(Decision{Action: HoldManualRecovery, Reason: "identity"})
	if manual != ManualRecovery || reason != "identity" || IsNonterminal(manual) {
		t.Fatalf("manual hold was not terminal and durable: status=%s reason=%v", manual, reason)
	}
	transactional, _ := initialDecisionStatus(Decision{Action: VoltrAllocateToSquads})
	if transactional != Decided || !IsNonterminal(transactional) {
		t.Fatalf("transactional decision did not enter decided: %s", transactional)
	}
}

func TestMigrationPreservesOneNonterminalAndTerminalHolds(t *testing.T) {
	path := filepath.Join("..", "..", "..", "..", "crates", "loyal-yield-store", "migrations", "0055_backyard_rwa_worker.sql")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sql := string(data)
	for _, required := range []string{
		"multiply_operations_one_nonterminal_per_route",
		"status = 'held'",
		"action = 'HOLD'",
		"status = 'manual_recovery' AND recovery_reason IS NOT NULL",
		"simulation_result JSONB",
		"reconciled_effects JSONB",
		"submitted_at IS NOT NULL",
		"confirmed_slot IS NOT NULL AND confirmation_status IN ('confirmed','finalized')",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("migration contract missing %q", required)
		}
	}
	indexStart := strings.Index(sql, "CREATE UNIQUE INDEX multiply_operations_one_nonterminal_per_route")
	if indexStart < 0 || strings.Contains(sql[indexStart:], "'held'") {
		t.Fatal("terminal HOLD entered the one-nonterminal index")
	}
}

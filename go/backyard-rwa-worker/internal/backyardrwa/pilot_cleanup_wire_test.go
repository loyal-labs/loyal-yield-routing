package backyardrwa

import (
	"testing"
)

// The deployed cleanup lifecycle walks one attributed residue of 214,921 raw
// USDC through exactly four custody states. The receipt custody-tracking
// value stays zero across the whole walk because staging uses plain SPL
// transfer and restore returns all staged cash. Each row states the cash state,
// the receipt view, the vault idle balance, and the vault book total that the
// deployed binary leaves behind after the previous wire.
const pilotCleanupAmountRaw uint64 = 214_921

type pilotCleanupLeg struct {
	name          string
	squads        uint64
	strategy      uint64
	position      uint64
	tracked       uint64
	idle          uint64
	book          uint64
	wantAction    Action
	wantOrder     int
	wantAmountRaw uint64
	wantReason    string
}

func pilotCleanupLifecycle() []pilotCleanupLeg {
	return []pilotCleanupLeg{
		{
			name: "initial_report_due", squads: pilotCleanupAmountRaw, idle: 23, book: 23,
			wantAction: ReportNAV, wantOrder: 1, wantReason: "cleanup_recovery_report_due",
		},
		{
			name: "reported_stage_due", squads: pilotCleanupAmountRaw,
			position: pilotCleanupAmountRaw, idle: 23, book: 23 + pilotCleanupAmountRaw,
			wantAction: StageSquadsToVoltr, wantOrder: 2, wantAmountRaw: pilotCleanupAmountRaw, wantReason: "cleanup_stage_reported_cash",
		},
		{
			name: "staged_restore_due", strategy: pilotCleanupAmountRaw,
			position: pilotCleanupAmountRaw, idle: 23, book: 23 + pilotCleanupAmountRaw,
			wantAction: VoltrRestoreIdle, wantOrder: 3, wantAmountRaw: pilotCleanupAmountRaw, wantReason: "cleanup_restore_staged_cash",
		},
		{
			name: "final_flat", idle: 23 + pilotCleanupAmountRaw, book: 23 + pilotCleanupAmountRaw,
			wantAction: "", wantReason: "cleanup_cash_flat",
		},
	}
}

func pilotCleanupState(leg pilotCleanupLeg) cleanupCustodyState {
	return cleanupCustodyState{
		IdleRaw: leg.idle, StrategyRaw: leg.strategy, SquadsRaw: leg.squads,
		ReceiptPositionValueRaw: leg.position, ReceiptCustodyTrackedRaw: leg.tracked,
	}
}

func TestClassifyCleanupDeployedLifecycle(t *testing.T) {
	for _, leg := range pilotCleanupLifecycle() {
		step, err := classifyCleanupStep(pilotCleanupState(leg), pilotCleanupMaxRaw)
		if err != nil {
			t.Fatalf("%s: deployed lifecycle state refused: %v", leg.name, err)
		}
		if step.Action != leg.wantAction || step.Order != leg.wantOrder || step.AmountRaw != leg.wantAmountRaw || step.Reason != leg.wantReason {
			t.Fatalf("%s: classified %+v, want action=%q order=%d amount=%d reason=%q",
				leg.name, step, leg.wantAction, leg.wantOrder, leg.wantAmountRaw, leg.wantReason)
		}
	}
}

// Any nonzero receipt custody tracking is outside this cleanup lifecycle.
func TestClassifyCleanupRefusesWrongTrackedValue(t *testing.T) {
	for _, leg := range []pilotCleanupLeg{
		{
			name: "reported", squads: pilotCleanupAmountRaw,
			position: pilotCleanupAmountRaw, tracked: 999, idle: 23, book: 23 + pilotCleanupAmountRaw,
		},
		{
			name: "staged", strategy: pilotCleanupAmountRaw,
			position: pilotCleanupAmountRaw, tracked: 999, idle: 23, book: 23 + pilotCleanupAmountRaw,
		},
	} {
		if _, err := classifyCleanupStep(pilotCleanupState(leg), pilotCleanupMaxRaw); err == nil {
			t.Fatalf("%s: wrong tracked value accepted", leg.name)
		} else {
			assertBudgetHold(t, err, "cleanup_state_incoherent")
		}
	}
}

func TestClassifyCleanupRefusesSplitCustody(t *testing.T) {
	state := cleanupCustodyState{SquadsRaw: 1_000, StrategyRaw: 1_000}
	_, err := classifyCleanupStep(state, pilotCleanupMaxRaw)
	assertBudgetHold(t, err, "cleanup_cash_split_across_custodies")
}

func TestClassifyCleanupRefusesOverBoundCash(t *testing.T) {
	state := cleanupCustodyState{SquadsRaw: pilotCleanupMaxRaw + 1}
	_, err := classifyCleanupStep(state, pilotCleanupMaxRaw)
	assertBudgetHold(t, err, "cleanup_bound_exceeded")
	// The bound is the parameter, not the constant: an otherwise valid
	// reported state over a tighter bound refuses too.
	state = cleanupCustodyState{SquadsRaw: pilotCleanupAmountRaw, ReceiptPositionValueRaw: pilotCleanupAmountRaw}
	_, err = classifyCleanupStep(state, pilotCleanupAmountRaw-1)
	assertBudgetHold(t, err, "cleanup_bound_exceeded")
}

func TestClassifyCleanupRefusesArmedTicket(t *testing.T) {
	state := cleanupCustodyState{SquadsRaw: pilotCleanupAmountRaw, TicketArmed: true}
	_, err := classifyCleanupStep(state, pilotCleanupMaxRaw)
	assertBudgetHold(t, err, "cleanup_ticket_armed")
	// The armed refusal wins even over the split-custody refusal.
	state = cleanupCustodyState{SquadsRaw: 1_000, StrategyRaw: 1_000, TicketArmed: true}
	_, err = classifyCleanupStep(state, pilotCleanupMaxRaw)
	assertBudgetHold(t, err, "cleanup_ticket_armed")
}

func cleanupBookAccounts(t *testing.T, idle, totalValue, position, tracked uint64) []ConfirmedAccount {
	t.Helper()
	return []ConfirmedAccount{
		tokenAccountFixture(t, bridgeIdleATA, bridgeUSDC, bridgeIdleAuthority, idle),
		voltrVaultFixture(t, totalValue),
		voltrLPMintFixture(t, 0),
		strategyReceiptWithCustodyFixture(t, position, tracked),
	}
}

// Every lifecycle leg obeys book = idle + reported position + tracked custody.
func TestCleanupVaultBookExplainsEveryLifecycleLeg(t *testing.T) {
	for _, leg := range pilotCleanupLifecycle() {
		if err := cleanupVaultBookHold(cleanupBookAccounts(t, leg.idle, leg.book, leg.position, leg.tracked)); err != nil {
			t.Fatalf("%s: lifecycle book idle=%d book=%d position=%d tracked=%d held: %v",
				leg.name, leg.idle, leg.book, leg.position, leg.tracked, err)
		}
	}
}

// A zero-cash outcome with a book the custody views cannot explain is
// unaccounted value and must hold, never pass.
func TestCleanupVaultBookHoldsOnZeroCashMismatch(t *testing.T) {
	total := 23 + pilotCleanupAmountRaw
	for _, book := range []uint64{total - 1, total + 1, 0, 999_999} {
		accounts := cleanupBookAccounts(t, total, book, 0, 0)
		err := cleanupVaultBookHold(accounts)
		assertBudgetHold(t, err, "cleanup_vault_book_unexplained")
	}
	// Idle over the pilot deposit cap is a residue-carrying book: the
	// compiler folds the cap breach into the unexplained-book hold.
	overCap := uint64(PilotDepositCapRaw) + 1
	err := cleanupVaultBookHold(cleanupBookAccounts(t, overCap, overCap, 0, 0))
	assertBudgetHold(t, err, "cleanup_vault_book_unexplained")
	// A nonzero LP supply on an otherwise flat zero-cash outcome means Voltr
	// priced shares that do not exist in this cleanup: refuse as not empty.
	accounts := cleanupBookAccounts(t, total, total, 0, 0)
	accounts[2] = voltrLPMintFixture(t, 1_000)
	err = cleanupVaultBookHold(accounts)
	assertBudgetHold(t, err, "cleanup_vault_book_not_empty")
}

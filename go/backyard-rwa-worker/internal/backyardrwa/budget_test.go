package backyardrwa

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func emptyTestBudget() Phase3Budget {
	return Phase3Budget{GoalID: Phase3GoalID, Families: map[string]FamilyBudget{
		"OnRe": {}, "AUTO": {}, "Ethena": {},
	}, Reservations: map[string]BudgetReservation{}}
}
func testReservation() BudgetReservation {
	return BudgetReservation{OperationID: "entry", Family: "OnRe", IntentSHA256: strings.Repeat("a", 64), UpperMicros: 900_000, ExitAfterMicros: 3_000_000}
}
func assertBudgetHold(t *testing.T, err error, reason string) {
	t.Helper()
	var hold *BudgetHold
	if !errors.As(err, &hold) || hold.Reason != reason {
		t.Fatalf("wanted HOLD %s; got %v", reason, err)
	}
}
func TestBudgetRejectsWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*Phase3Budget, *BudgetReservation)
		reason string
	}{
		{"transaction", func(b *Phase3Budget, r *BudgetReservation) { r.UpperMicros = 1_000_001 }, "transaction_cap_exceeded"},
		{"family", func(b *Phase3Budget, r *BudgetReservation) {
			b.Families["OnRe"] = FamilyBudget{SpentMicros: 17_000_000}
		}, "family_cap_exceeded"},
		{"goal", func(b *Phase3Budget, r *BudgetReservation) {
			b.Families["AUTO"] = FamilyBudget{SpentMicros: 30_000_000}
			b.Families["Ethena"] = FamilyBudget{SpentMicros: 30_000_000}
		}, "goal_cap_exceeded"},
		{"reserve", func(b *Phase3Budget, r *BudgetReservation) { b.Families["OnRe"] = FamilyBudget{ExitMicros: 4_000_000} }, "entry_consumes_exit_reserve"},
		{"unfit exit", func(b *Phase3Budget, r *BudgetReservation) {
			r.Recovery = true
			b.Families["OnRe"] = FamilyBudget{ExitMicros: 3_899_999}
		}, "recovery_exceeds_reserved_exit"},
		{"expired", func(b *Phase3Budget, r *BudgetReservation) { b.Closed = true }, "goal_envelope_expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, r := emptyTestBudget(), testReservation()
			tc.setup(&b, &r)
			before, _ := json.Marshal(b)
			assertBudgetHold(t, b.Admit(r), tc.reason)
			after, _ := json.Marshal(b)
			if string(before) != string(after) {
				t.Fatal("rejection mutated budget")
			}
		})
	}
}

func TestBudgetRestartAndAmbiguityRetainReservation(t *testing.T) {
	b, r := emptyTestBudget(), testReservation()
	if err := b.Admit(r); err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var restarted Phase3Budget
	if err = json.Unmarshal(wire, &restarted); err != nil {
		t.Fatal(err)
	}
	if err = restarted.Admit(r); err != nil {
		t.Fatal(err)
	}
	other := r
	other.OperationID = "retry-with-new-name"
	assertBudgetHold(t, restarted.Admit(other), "unresolved_submission_reservation")
	assertBudgetHold(t, restarted.AuthorizeIntent(r.OperationID, strings.Repeat("b", 64)), "unreserved_transaction")
	if err = restarted.AuthorizeIntent(r.OperationID, r.IntentSHA256); err != nil {
		t.Fatal(err)
	}
	family, goal, err := restarted.totals("OnRe")
	if err != nil || family != 3_900_000 || goal != family {
		t.Fatalf("reservation changed: %d %d %v", family, goal, err)
	}
}

func TestBudgetUnwindConsumesReservedHeadroom(t *testing.T) {
	b := emptyTestBudget()
	b.Families["OnRe"] = FamilyBudget{SpentMicros: 19_000_000, ExitMicros: 1_000_000}
	r := testReservation()
	r.Recovery = true
	r.UpperMicros = 900_000
	r.ExitAfterMicros = 100_000
	if err := b.Admit(r); err != nil {
		t.Fatal(err)
	}
	if err := b.Settle(r.OperationID, r.IntentSHA256, 850_000); err != nil {
		t.Fatal(err)
	}
	row := b.Families["OnRe"]
	if row.SpentMicros != 19_850_000 || row.ExitMicros != 100_000 || len(b.Reservations) != 0 {
		t.Fatalf("wrong settled budget: %+v", b)
	}
	// Returning principal and retrying do not reset gross spent value.
	next := testReservation()
	next.OperationID = "next-entry"
	next.ExitAfterMicros = 100_000
	assertBudgetHold(t, b.Admit(next), "family_cap_exceeded")
}

func TestDebitValuationUsesDecimalsAndRoundsUp(t *testing.T) {
	for _, tc := range []struct {
		raw      uint64
		decimals uint8
		price    uint64
		want     int64
	}{
		{1_000_000_000, 9, 1_020_000, 1_020_000},
		{1_000_000, 6, 1_020_000, 1_020_000},
		{1, 9, 1_000_000, 1},
		{5_000, 9, 150_000_000, 750},
	} {
		v, err := ValueDebitMicros(tc.raw, tc.decimals, tc.price)
		if err != nil || v != tc.want {
			t.Fatalf("valuation %+v got %d %v", tc, v, err)
		}
	}
}

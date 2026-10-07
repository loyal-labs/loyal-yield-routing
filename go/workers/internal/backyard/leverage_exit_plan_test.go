package backyard

import (
	"math/big"
	"testing"
)

func TestPlanLeverageExitCycles(t *testing.T) {
	v := func(x int64) *big.Int { return big.NewInt(x) }
	for _, tc := range []struct {
		name          string
		c, d, ceiling int64
		cycles        int
		err           string
	}{
		{"1x no debt", 1_000_000, 0, 5500, 0, ""},
		{"1.5x one release pays off", 1_500_000, 500_000, 5500, 0, ""},
		{"1.75x one cycle at 55%", 1_750_000, 750_000, 5500, 1, ""},
		{"1.75x two cycles at 50%", 1_750_000, 750_000, 5000, 2, ""},
		{"at the ceiling: no release", 1_000_000, 550_000, 5500, 0, "no_safe_repayment_collateral_release"},
		{"more than two cycles holds", 1_750_000, 750_000, 4500, 0, "leverage_exit_cycles_exceeded"},
	} {
		plan, err := planLeverageExit(v(tc.c), v(tc.d), tc.ceiling, 0)
		if tc.err != "" {
			assertBudgetHold(t, err, tc.err)
			continue
		}
		if err != nil || len(plan.Cycles) != tc.cycles || plan.FinalReleaseValue == nil {
			t.Fatalf("%s: %d cycles, err %v", tc.name, len(plan.Cycles), err)
		}
		// Every cycle repays only part of the debt, and LTV falls.
		c, d := v(tc.c), v(tc.d)
		for _, cycle := range plan.Cycles {
			c.Sub(c, cycle.ReleaseValue)
			d.Sub(d, cycle.RepayValue)
			if d.Sign() <= 0 || cycle.RepayValue.Cmp(cycle.ReleaseValue) > 0 {
				t.Fatalf("%s: cycle repaid the whole debt or more than released", tc.name)
			}
			if new(big.Int).Mul(d, v(10_000)).Cmp(new(big.Int).Mul(c, v(tc.ceiling))) > 0 {
				t.Fatalf("%s: LTV above the ceiling after a cycle", tc.name)
			}
		}
	}
	// Accrual makes the plan longer, never shorter.
	plan, err := planLeverageExit(v(1_750_000), v(750_000), 5500, 50)
	if err != nil || len(plan.Cycles) < 1 {
		t.Fatalf("accrual: %v %v", plan, err)
	}
	for _, bad := range [][4]int64{{0, 1, 5500, 0}, {1, -1, 5500, 0}, {1, 1, 0, 0}, {1, 1, 10_000, 0}, {1, 1, 5500, -1}} {
		if _, err := planLeverageExit(v(bad[0]), v(bad[1]), bad[2], bad[3]); err == nil {
			t.Fatalf("invalid input accepted: %v", bad)
		}
	}
}

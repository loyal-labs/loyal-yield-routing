package backyardrwa

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// A1: the observation window is ~13 s of measured slot time, clamped to
// [32, 64] slots, and 32 when unmeasured or unreadable.
func TestObservationWindowScalesWithSlotTimeAndIsClamped(t *testing.T) {
	for _, tc := range []struct {
		ms   float64
		want int64
	}{{0, 32}, {-1, 32}, {400, 33}, {500, 32}, {270, 49}, {203, 64}, {100, 64}} {
		if got := observationLagForSlotMillis(tc.ms); got != tc.want {
			t.Fatalf("slot %.0f ms: got %d slots, want %d", tc.ms, got, tc.want)
		}
	}
	reset := func() {
		observationLag.Store(0)
		observationLagChecked, observationLagMeasured = time.Time{}, time.Time{}
	}
	reset()
	t.Cleanup(reset)
	if observationLagSlots() != budgetMaxObservationLagSlots {
		t.Fatal("unmeasured window is not 32 slots")
	}
	client, err := NewRPCClient("https://rpc.invalid")
	if err != nil {
		t.Fatal(err)
	}
	calls, body := 0, `{"jsonrpc":"2.0","id":1,"result":[{"numSlots":222,"samplePeriodSecs":60},{"numSlots":222,"samplePeriodSecs":60}]}`
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return response(body), nil
	})
	client.refreshObservationLagSlots(context.Background())
	if got := observationLagSlots(); got != 49 { // 60 s / 222 slots = 270 ms
		t.Fatalf("measured 270 ms slots gave %d slots, want 49", got)
	}
	client.refreshObservationLagSlots(context.Background())
	if calls != 1 {
		t.Fatalf("slot time re-measured within a minute: %d calls", calls)
	}
	// An unreadable measurement keeps the last value for 10 minutes, then 32.
	body = `{"jsonrpc":"2.0","id":1,"result":[]}`
	observationLagChecked = time.Now().Add(-2 * time.Minute)
	client.refreshObservationLagSlots(context.Background())
	if observationLagSlots() != 49 {
		t.Fatal("one failed read dropped the measured window")
	}
	observationLagChecked, observationLagMeasured = time.Now().Add(-2*time.Minute), time.Now().Add(-11*time.Minute)
	client.refreshObservationLagSlots(context.Background())
	if observationLagSlots() != budgetMaxObservationLagSlots {
		t.Fatal("a stale measurement did not fall back to 32 slots")
	}
}

// A1: a price valued at the widened window still refuses a slot past it,
// and window-width sanity checks never admit more than the 64-slot ceiling.
func TestWidenedWindowStillBoundsPriceAge(t *testing.T) {
	observationLag.Store(49)
	t.Cleanup(func() { observationLag.Store(0) })
	p := BudgetPrice{Mint: "m", TokenProgram: "p", Decimals: 6, ObservedSlot: 100, ValidThroughSlot: 100 + observationLagSlots(), EvidenceSHA256: sha256Bytes([]byte("e"))}
	p.TokenUpperSF[7], p.USDCLowerSF[7] = 16, 16
	if _, err := p.valueUpper(1, "m", "p", 149); err != nil {
		t.Fatal("price inside the widened window was refused", err)
	}
	assertBudgetHoldReason := func(err error) {
		t.Helper()
		assertBudgetHold(t, err, "missing_stale_or_mismatched_usdc_valuation")
	}
	_, err := p.valueUpper(1, "m", "p", 150)
	assertBudgetHoldReason(err)
	p.ValidThroughSlot = 100 + budgetMaxObservationLagCeilingSlots + 1
	_, err = p.valueUpper(1, "m", "p", 101)
	assertBudgetHoldReason(err)
}

package referral

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
)

func TestReferralCapitalEarnsOnNetOnlyWhileDeposited(t *testing.T) {
	const year = 365 * 86400
	start := time.Unix(1700000000, 0)
	r := capitalReplay{At: start.Unix()}
	apply := func(before, after, heldBefore, heldAfter uint64, e earnmax.CapitalEvent) {
		t.Helper()
		m := earnmax.CapitalMovement{FreeBefore: before, FreeAfter: after, EscrowBefore: heldBefore, EscrowAfter: heldAfter, Events: []earnmax.CapitalEvent{e}}
		if err := r.apply(m, start); err != nil {
			t.Fatal(err)
		}
	}
	apply(0, 100, 0, 0, earnmax.CapitalEvent{Kind: "deposit", LP: 100, Asset: 10_000_000000, NetAssetNumerator: "996000000000", NetAssetDenominator: 100, UnixTime: start.Unix()})
	// $9,960, then half reserved for a quarter-year. Only 40 out of the 50
	// reserved shares return and resume; ten are burned in the cancellation.
	apply(100, 50, 0, 50, earnmax.CapitalEvent{Kind: "request", LP: 50, UnixTime: start.Unix() + year/2})
	apply(50, 90, 50, 0, earnmax.CapitalEvent{Kind: "cancel", LP: 40, BurnedLP: 10, UnixTime: start.Unix() + 3*year/4})
	raw, err := json.Marshal(&r)
	if err != nil {
		t.Fatal(err)
	}
	var resumed capitalReplay
	if err = json.Unmarshal(raw, &resumed); err != nil {
		t.Fatal(err)
	}
	if err = resumed.accrue(start.Unix() + year); err != nil {
		t.Fatal(err)
	}
	if got := resumed.Earned.RatString(); got != "25398000" {
		t.Fatalf("earned %s micro-USDC, want 25398000", got)
	}
}

func TestReferralCapitalDoesNotAccrueTransferredOrPreBindingPrincipal(t *testing.T) {
	start := time.Unix(1001, 0)
	r := capitalReplay{At: start.Unix()}
	pre := earnmax.CapitalEvent{Kind: "deposit", LP: 25, NetAssetNumerator: "100", NetAssetDenominator: 1, UnixTime: 1000}
	if err := r.apply(earnmax.CapitalMovement{FreeAfter: 25, Events: []earnmax.CapitalEvent{pre}}, start); err != nil {
		t.Fatal(err)
	}
	if err := r.apply(earnmax.CapitalMovement{FreeBefore: 25, FreeAfter: 50, Events: []earnmax.CapitalEvent{{Kind: "transfer_in", LP: 25, UnixTime: 1008}}}, start); err != nil {
		t.Fatal(err)
	}
	if r.Earned.Sign() != 0 || r.Active.Sign() != 0 {
		t.Fatal("pre-binding or transferred shares qualified")
	}
	pre.UnixTime = 1009
	if err := r.apply(earnmax.CapitalMovement{FreeBefore: 50, FreeAfter: 75, Events: []earnmax.CapitalEvent{pre}}, start); err == nil {
		t.Fatal("mixed principal origin was certified")
	}
}

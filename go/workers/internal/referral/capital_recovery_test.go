package referral

import (
	"math/big"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
)

func TestReferralCapitalRecoveryAfterMixedPositionCloses(t *testing.T) {
	start := time.Unix(1000, 0)
	const year = 365 * 86400
	gap := capitalFact{slot: 101, at: time.Unix(1010, 0), movement: earnmax.CapitalMovement{FreeBefore: 90, FreeAfter: 100, Events: []earnmax.CapitalEvent{{Kind: "transfer_in", LP: 10, UnixTime: 1010}}}}
	request := capitalFact{slot: 102, at: time.Unix(1020, 0), movement: earnmax.CapitalMovement{FreeBefore: 100, EscrowAfter: 100, Events: []earnmax.CapitalEvent{{Kind: "request", LP: 100, UnixTime: 1020}}}}
	claim := capitalFact{slot: 103, at: time.Unix(1030, 0), movement: earnmax.CapitalMovement{EscrowBefore: 100, Events: []earnmax.CapitalEvent{{Kind: "claim", LP: 100, UnixTime: 1030}}}}
	topUp := capitalFact{slot: 104, at: time.Unix(1040, 0), movement: earnmax.CapitalMovement{BoundWalletSigned: true, FreeAfter: 30, Events: []earnmax.CapitalEvent{{Kind: "deposit", LP: 30, NetAssetNumerator: "300000000", NetAssetDenominator: 1, UnixTime: 1040}}}}
	carry := big.NewRat(27, 2)
	end := time.Unix(1040+year, 0)
	reset, r := recoverCapital([]capitalFact{gap, request, claim, topUp}, 0, 30, 0, 110, end, start, carry)
	if reset == nil || reset.slot != 103 || r.Free != 30 || r.Earned.RatString() != "1800027/2" {
		t.Fatalf("reset=%+v replay=%+v", reset, r)
	}
	// An unreferenced canonical free account in the claim is not proof it was
	// empty. Reconcile current independent balances backwards or keep the gap.
	if reset, _ := recoverCapital([]capitalFact{gap, request, claim}, 0, 5, 0, 110, end, start, carry); reset != nil {
		t.Fatal("unobserved free LP treated as zero")
	}
	if reset, _ := recoverCapital([]capitalFact{gap, request}, 0, 0, 100, 110, end, start, carry); reset != nil {
		t.Fatal("accepted withdrawal request treated as empty")
	}
	if reset, _ := recoverCapital([]capitalFact{gap, request, claim, topUp}, -1, 30, 0, 110, end, start, carry); reset != nil {
		t.Fatal("uncertain history used to infer a historical reset")
	}
	reset, r = recoverCapital(nil, -1, 0, 0, 110, end, start, carry)
	if reset == nil || reset.slot != 110 || r.Earned.Cmp(carry) != 0 {
		t.Fatal("finalized empty snapshot did not preserve prior proven earnings")
	}
}

func TestReferralCapitalTopUpAndClaimPreserveUntouchedEscrowAndFree(t *testing.T) {
	start := time.Unix(1000, 0)
	r := capitalReplay{At: start.Unix(), Free: 50, Escrow: 50}
	r.Active.SetInt64(250)
	r.Reserved.SetInt64(250)
	topUp := earnmax.CapitalMovement{FreeBefore: 50, FreeAfter: 60, Events: []earnmax.CapitalEvent{{Kind: "deposit", LP: 10, NetAssetNumerator: "50", NetAssetDenominator: 1, UnixTime: 1100}}}
	claim := earnmax.CapitalMovement{EscrowBefore: 50, Events: []earnmax.CapitalEvent{{Kind: "claim", LP: 50, UnixTime: 1200}}}
	if err := r.apply(topUp, start); err != nil {
		t.Fatal(err)
	}
	if err := r.apply(claim, start); err != nil {
		t.Fatal(err)
	}
	if r.Free != 60 || r.Escrow != 0 || r.Active.RatString() != "300" || r.Reserved.Sign() != 0 {
		t.Fatalf("unexpected carry after partial request/topup/claim: %+v", r)
	}
}

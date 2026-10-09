package backyard

import (
	"context"
	"encoding/binary"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

// feePolicySnapshot keeps the real coherent decoder/merge and existing armed
// journal/program fixture. Only the supplied vault bytes differ from it.
func feePolicySnapshot(t *testing.T, offset int, bps uint16) Snapshot {
	t.Helper()
	s := monitorSnapshot(t, nil)
	accounts := routeNAVFixture(t, s.Slot)
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeStrategyATA).Data[64:72], 0)
	vault := accountAt(accounts, bridgeVoltrVault).Data
	binary.LittleEndian.PutUint64(vault[168:176], 53)
	binary.LittleEndian.PutUint16(vault[514:516], 2000)
	if offset != 0 {
		binary.LittleEndian.PutUint16(vault[offset:offset+2], bps)
	}
	nav, err := ComputeRouteNAV(s.Slot, accounts, readyWorkerManifest(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyRouteNAVSnapshot(&s, nav, time.Unix(kaminoFixtureUnix+3600, 0)); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVoltrApprovedFeeTupleAndEveryTermDrift(t *testing.T) {
	manifest := readyWorkerManifest(t)
	approved := feePolicySnapshot(t, 0, 0)
	for _, decide := range []func(Snapshot) Decision{Decide, manifest.DecideOnManifest} {
		if got := decide(approved); got.Action != ReportNAV {
			t.Fatalf("approved exact tuple blocked normal NAV: %+v", got)
		}
	}
	for _, tc := range []struct {
		offset int
		bps    uint16
	}{{512, 1}, {514, 0}, {514, 1999}, {514, 2001}, {516, 1}, {518, 1}, {520, 1}, {522, 1}, {524, 1}, {526, 1}} {
		t.Run(strconv.Itoa(tc.offset)+"/"+strconv.Itoa(int(tc.bps)), func(t *testing.T) {
			s := feePolicySnapshot(t, tc.offset, tc.bps)
			for _, lane := range []string{RouteID, primePRIMEPYUSD.Lane} {
				s.RouteLane = lane
				for _, decide := range []func(Snapshot) Decision{Decide, manifest.DecideOnManifest} {
					if got := decide(s); got.Action != HoldManualRecovery || got.Reason != "voltr_fee_terms_unapproved" {
						t.Fatalf("fee drift did not hold on %s: %+v", lane, got)
					}
				}
			}
			// The normal worker dispatch must use its durable stop recorder,
			// never preparation, for a decoded fee-policy fault.
			s.RouteLane = RouteID
			recorded := false
			w := &Worker{routeKey: productionRouteKey, manifest: manifest, runtime: tickRuntime{
				loadNonterminal: func(context.Context, string) (*PersistedOperation, error) { return nil, nil },
				observe:         func(context.Context) (Observation, error) { return tickObservation(s), nil },
				recordManualRecovery: func(_ context.Context, _ string, _ Observation, d Decision, _, _ string) (DecisionRecord, error) {
					recorded = d.Action == HoldManualRecovery && d.Reason == "voltr_fee_terms_unapproved"
					return DecisionRecord{Status: ManualRecovery}, nil
				},
			}}
			if err := w.Tick(context.Background()); err != nil || !recorded {
				t.Fatalf("fee drift did not reach durable manual-recovery recorder: %v recorded=%t", err, recorded)
			}
		})
	}
}

func TestVoltrLegitimateFeeRatioKeepsNAVAndQueueUnwindLive(t *testing.T) {
	var s Snapshot
	// Accrued fee LP is ~2% of effective supply. A withdrawal burns 1000
	// circulating LP and lifts the unchanged accumulator to ~4%. Decode each
	// coherent poststate before deciding; this is not a fee-rate change.
	for _, mint := range []uint64{1000, 0} {
		s = monitorSnapshot(t, func(accounts []ConfirmedAccount, s *Snapshot) {
			binary.LittleEndian.PutUint64(accountAt(accounts, bridgeVoltrVault).Data[584:592], 40)
			binary.LittleEndian.PutUint64(accountAt(accounts, bridgeLPMint).Data[36:44], mint)
			nav, err := ComputeRouteNAV(s.Slot, accounts, readyWorkerManifest(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := applyRouteNAVSnapshot(s, nav, time.Unix(kaminoFixtureUnix+3600, 0)); err != nil {
				t.Fatal(err)
			}
		})
		if s.FeeAccumulatorRaw != 40 || s.LPSupplyInclFeesRaw != int64(mint)+1040 {
			t.Fatalf("withdrawal poststate lost fee LP or dead weight: %+v", s)
		}
		if got := Decide(s); got.Action != ReportNAV {
			t.Fatalf("legitimate fee share blocked NAV at supply %d: %+v", s.LPSupplyInclFeesRaw, got)
		}
		underfunded := s
		underfunded.PostMutationNAVRequired = true
		underfunded.WithdrawalDemandRaw = 20
		if got := Decide(underfunded); got.Action != SwapPrimeToUSDCStep {
			t.Fatalf("legitimate fee share blocked underfunded queue unwind: %+v", got)
		}
	}
	for _, mutate := range []func(*Snapshot){
		func(s *Snapshot) { s.FeeAccumulatorRaw = -1 },
		func(s *Snapshot) { s.LPSupplyInclFeesRaw = -1 },
		func(s *Snapshot) { s.FeeAccumulatorRaw = s.LPSupplyInclFeesRaw + 1 },
	} {
		invalid := s
		mutate(&invalid)
		if got := Decide(invalid); got.Action != HoldManualRecovery || got.Reason != "fee_accounting_invalid" {
			t.Fatalf("invalid LP accounting did not hold: %+v", got)
		}
	}
}

func TestVoltrLPTotalsRejectWrapAndSignedOverflowAtBothBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name                                 string
		manager, admin, protocol, mint, dead uint64
	}{
		{"fee wrap", math.MaxUint64, 1, 0, 0, 0},
		{"fee signed", math.MaxInt64, 1, 0, 0, 0},
		{"supply wrap", 1, 0, 0, math.MaxUint64, 0},
		{"dead weight wrap", 0, 0, 0, math.MaxUint64, 1},
		{"supply signed", 1, 0, 0, math.MaxInt64, 0},
		{"dead weight signed", 0, 0, 0, math.MaxInt64, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accounts := routeNAVFixture(t, 77)
			vault := accountAt(accounts, bridgeVoltrVault).Data
			binary.LittleEndian.PutUint64(vault[576:584], tc.manager)
			binary.LittleEndian.PutUint64(vault[584:592], tc.admin)
			binary.LittleEndian.PutUint64(vault[592:600], tc.protocol)
			binary.LittleEndian.PutUint64(vault[616:624], tc.dead)
			binary.LittleEndian.PutUint64(accountAt(accounts, bridgeLPMint).Data[36:44], tc.mint)
			for _, post := range []*RouteNAVCustodies{nil, {VoltrIdleRaw: 11, SquadsUSDCraw: 6, SquadsPRIMEraw: 3}} {
				if _, err := ComputeRouteNAV(77, accounts, readyWorkerManifest(t), post); err == nil {
					t.Fatalf("invalid LP totals accepted with poststate=%t", post != nil)
				}
			}
			nav := cadenceNAV(77, 33, 42, time.Unix(kaminoFixtureUnix, 0))
			nav.Voltr = VoltrVaultBook{Vault: voltr.Vault{FeeAccumulatorManagerRaw: tc.manager, FeeAccumulatorAdminRaw: tc.admin,
				FeeAccumulatorProtocolRaw: tc.protocol, LPSupplyDeadWeightRaw: tc.dead}}
			nav.LPSupplyRaw = tc.mint
			s := Snapshot{Slot: 77}
			if err := applyRouteNAVSnapshot(&s, nav, time.Unix(kaminoFixtureUnix, 0)); err == nil || s.MonitorsArmed {
				t.Fatalf("invalid LP totals reached signed/armed snapshot: %+v err=%v", s, err)
			}
		})
	}
}

func TestVoltrFeeHarvestPreservesEffectiveSupplyAndGrossNAV(t *testing.T) {
	accounts := routeNAVFixture(t, 77)
	vault := accountAt(accounts, bridgeVoltrVault).Data
	binary.LittleEndian.PutUint16(vault[514:516], 2000)
	binary.LittleEndian.PutUint64(vault[576:584], 17)
	binary.LittleEndian.PutUint64(vault[584:592], 37)
	binary.LittleEndian.PutUint64(vault[592:600], 11)
	before, err := ComputeRouteNAV(77, accounts, readyWorkerManifest(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	var s Snapshot
	s.Slot = 77
	if err := applyRouteNAVSnapshot(&s, before, time.Unix(kaminoFixtureUnix, 0)); err != nil {
		t.Fatal(err)
	}
	if s.FeeAccumulatorRaw != 65 || s.LPSupplyInclFeesRaw != 2065 || before.StrategyNAVRaw != 33 || before.Report.NAVAfterRaw != 33 || before.TotalVaultNAVRaw != 44 {
		t.Fatalf("fee LP changed gross asset NAV or effective supply: %+v %+v", s, before)
	}
	// Projection only: harvested LP moves to mint, not USDC or strategy NAV.
	clear(vault[576:600])
	binary.LittleEndian.PutUint64(accountAt(accounts, bridgeLPMint).Data[36:44], 1065)
	after, err := ComputeRouteNAV(77, accounts, readyWorkerManifest(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyRouteNAVSnapshot(&s, after, time.Unix(kaminoFixtureUnix, 0)); err != nil {
		t.Fatal(err)
	}
	if s.FeeAccumulatorRaw != 0 || s.LPSupplyInclFeesRaw != 2065 || after.StrategyNAVRaw != 33 || after.Report.NAVAfterRaw != 33 || after.TotalVaultNAVRaw != 44 || after.Voltr.TotalValueRaw != before.Voltr.TotalValueRaw {
		t.Fatalf("harvest projection double-counted LP or altered gross NAV: %+v %+v", s, after)
	}
}

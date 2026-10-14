package backyard

import (
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
)

// liveCapture is one production observation's confirmed reads, recorded
// read-only from a public RPC: the view reads in order and the valuation
// refresh simulations (the valuation capture, then the overflow room) in
// order.
type liveCapture struct {
	Now       time.Time `json:"now"`
	Reads     []liveRead
	Refreshes []liveRead
}

type liveRead struct {
	Slot     int64
	Accounts []ConfirmedAccount
	Receipts []programAccount
}

// liveInputs is the live route state after migration 0103 (route state
// version 87096): both AUTO lanes at a stored 1.75 level, no exits. The
// approval column starts false; the next selector sample decides it.
func liveInputs(approved bool) targetInputs {
	return targetInputs{Levels: map[string]laneLevel{
		autoAUTOPYUSD.Lane: {Level: 1.75, UpApproved: approved},
		autoAUTOWYLDSLane:  {Level: 1.75, UpApproved: approved},
	}}
}

func loadLiveCapture(t *testing.T, path string) liveCapture {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	z, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var c liveCapture
	if err := json.NewDecoder(z).Decode(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

// observeLive replays the capture through the production observer with in.
func observeLive(t *testing.T, c liveCapture, in targetInputs) Observation {
	t.Helper()
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	runtime := routeObservationRuntime{state: testVaultState,
		inputs: in,
		read: func(_ context.Context, _ []string, _ int64) (int64, []ConfirmedAccount, []programAccount, error) {
			if reads >= len(c.Reads) {
				return 0, nil, nil, fmt.Errorf("replay read %d beyond the capture", reads)
			}
			r := c.Reads[reads]
			reads++
			return r.Slot, r.Accounts, r.Receipts, nil
		},
		now: func() time.Time { return c.Now },
	}
	refreshes := 0
	runtime.refreshValuation = func(context.Context, []RuntimeRoute, []string, int64) (int64, []ConfirmedAccount, error) {
		if refreshes >= len(c.Refreshes) {
			return 0, nil, fmt.Errorf("replay refresh %d beyond the capture", refreshes)
		}
		r := c.Refreshes[refreshes]
		refreshes++
		return r.Slot, r.Accounts, nil
	}
	o, _, err := observeConfirmedRouteSnapshotWithAccounts(context.Background(), manifest, runtime)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// observeLiveAtRest is observeLive through the production observer's journal
// and identity merge, with the journal at rest: reconciled through the live
// ticket and armed at the live reported NAV, as after the Oct 11 overflow's
// last report.
func observeLiveAtRest(t *testing.T, c liveCapture, in targetInputs) Observation {
	t.Helper()
	raw := observeLive(t, c, in)
	state := productionObserveState{
		routeKey: productionRouteKey,
		journal: &stubProductionJournal{journal: ReconciledBridgeJournalState{
			TicketSequenceKnown: true, TicketSequenceRaw: raw.Snapshot.TicketLastConsumedSequenceRaw,
			ArmedNAVKnown: true, ArmedNAVRaw: raw.Snapshot.PriorReportedNAVRaw,
		}},
		batch:    func(context.Context) (Observation, error) { return observeLive(t, c, in), nil },
		identity: pinnedIdentityObservation,
	}
	o, err := state.observe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// The live book read at 03:44 UTC Oct 12 (slot 455902661), after the Oct 11
// overflow: AUTO/AUTO/PYUSD and AUTO/AUTO/wYLDS both at 1.75x (LTV 4284 bps),
// PYUSD borrowing closed by reserve utilization, NAV ~$101.25k. A restart on
// the target binary computes exactly that state, right after the deploy
// (levels copied, no approval) and after the next selector sample approves
// both levels: no step, no overflow move, no release, no top-up.
func TestLiveStateRestartComputesTheCurrentState(t *testing.T) {
	t.Parallel()
	c := loadLiveCapture(t, "testdata/live_state_2026-10-12.json.gz")
	for _, approved := range []bool{false, true} {
		in := liveInputs(approved)
		o := observeLiveAtRest(t, c, in)
		s := o.Snapshot
		if s.HeldLanes != autoAUTOPYUSD.Lane+","+autoAUTOWYLDSLane || len(o.lanes) != 2 {
			t.Fatalf("approved=%v: held %q over %d views", approved, s.HeldLanes, len(o.lanes))
		}
		want := LaneTarget{Rule: holdAtTarget, Level: 1.75, UpApproved: approved}
		if s.Target != want {
			t.Fatalf("approved=%v: target %+v, want %+v", approved, s.Target, want)
		}
		if d := Decide(s); d.Action != Hold || d.Reason != holdAtTarget {
			t.Fatalf("approved=%v: the restart moved: %+v", approved, d)
		}
		lanes := make([]targetLane, len(o.lanes))
		for i, view := range o.lanes {
			lanes[i] = targetLane{s: view, held: true, level: in.Levels[view.RouteLane]}
		}
		for i, view := range o.lanes {
			t.Logf("approved=%v %s LTV %d bps, up level %v, borrow blocked %v", approved, view.RouteLane, view.LTVBPS, leverageUpLevel(view), view.BorrowUtilizationBlocked)
			if view.LTVBPS < 4200 || view.LTVBPS > 4300 || leverageUpLevel(view) != 0 {
				t.Fatalf("%s is not at its 1.75 band: LTV %d up %v", view.RouteLane, view.LTVBPS, leverageUpLevel(view))
			}
			// The overflow move needs a source below its stored level: PYUSD
			// sits on it, so the Oct 11 overflow neither repeats nor extends.
			if lane, e := overflowMove(lanes, i, in); e != 0 {
				t.Fatalf("%s would move %d to %s", view.RouteLane, e, lane)
			}
		}
	}
}

// Custody several lanes share belongs, while no lane holds a position on it,
// to the selector's entry lane: PRIME bought for Prime/PRIME/PYUSD is that
// lane's, not Prime/PRIME/USDC's (the first sharer in registry order).
func TestSharedCustodyIsTheEntryLanes(t *testing.T) {
	t.Parallel()
	c := loadLiveCapture(t, "testdata/live_state_2026-10-12.json.gz")
	route, err := runtimeRoute("Prime/PRIME/PYUSD")
	if err != nil {
		t.Fatal(err)
	}
	fund := func(accounts []ConfirmedAccount) {
		for i := range accounts {
			if accounts[i].Address == route.CollateralCustody {
				accounts[i].Data = append([]byte(nil), accounts[i].Data...)
				binary.LittleEndian.PutUint64(accounts[i].Data[64:72], 5_000_000_000)
			}
		}
	}
	accounts := c.Reads[0].Accounts
	fund(accounts)
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		t.Fatal(err)
	}
	in := liveInputs(false)
	in.Entry = &SelectorResult{Action: "ENTER", DestinationLane: route.Lane}
	// The observer's preferred lane for the batch.
	preferred := flatViewLane(manifest.RuntimeActivation.SelectedLane, in)
	held, err := observedSelectorHeldLanes(rwa, accounts, earnLaneIDs(true), preferred)
	if err != nil || !slices.Contains(held, route.Lane) || slices.Contains(held, PhaseOneLaneID) {
		t.Fatalf("shared PRIME custody held by %v (%v)", held, err)
	}
}

// withAccount is the capture with edit applied to address's data in every
// read and refresh.
func withAccount(c liveCapture, address string, edit func(data []byte)) liveCapture {
	apply := func(reads []liveRead) []liveRead {
		out := make([]liveRead, len(reads))
		for i, r := range reads {
			out[i] = r
			out[i].Accounts = append([]ConfirmedAccount(nil), r.Accounts...)
			for j := range out[i].Accounts {
				if out[i].Accounts[j].Address == address {
					out[i].Accounts[j].Data = append([]byte(nil), out[i].Accounts[j].Data...)
					edit(out[i].Accounts[j].Data)
				}
			}
		}
		return out
	}
	c.Reads, c.Refreshes = apply(c.Reads), apply(c.Refreshes)
	return c
}

// tokenBalance sets a token account's amount: raw residue in a custody.
func tokenBalance(raw uint64) func([]byte) {
	return func(data []byte) { binary.LittleEndian.PutUint64(data[64:72], raw) }
}

// emptyObligation clears a KLend obligation's borrows (its debt repaid) and,
// with deposits, its deposits too (its collateral withdrawn).
func emptyObligation(deposits bool) func([]byte) {
	return func(data []byte) {
		clear(data[1208 : 1208+5*200])
		clear(data[2208:2240])
		if deposits {
			clear(data[96 : 96+8*136])
			clear(data[1192:1208])
			clear(data[2240:2272])
		}
	}
}

// Custody is vault cash. PYUSD residue beside AUTO/AUTO/PYUSD, which owes
// PYUSD and sits at its target, is counted once in NAV and moves nothing:
// no lane changes, no step, no manual hold. The lane's next step takes the
// whole account. A 1-raw residue is the same.
func TestLivePYUSDResidueIsVaultCash(t *testing.T) {
	t.Parallel()
	c := loadLiveCapture(t, "testdata/live_state_2026-10-12.json.gz")
	in := liveInputs(true)
	base := observeLiveAtRest(t, c, in).Snapshot
	if base.TotalVaultNAVRaw != liveTotalVaultNAVRaw || base.StrategyNAVRaw != liveStrategyNAVRaw {
		t.Fatalf("live book changed: total %d strategy %d identity %s", base.TotalVaultNAVRaw, base.StrategyNAVRaw, base.ObservationID)
	}
	for _, residue := range []uint64{25_000_000, 1} {
		o := observeLiveAtRest(t, withAccount(c, pyusdCustody, tokenBalance(residue)), in)
		s := o.Snapshot
		if s.HeldLanes != base.HeldLanes || len(o.lanes) != 2 || s.Target != base.Target || s.RouteLane != base.RouteLane {
			t.Fatalf("residue %d moved the target: held %q views %d target %+v", residue, s.HeldLanes, len(o.lanes), s.Target)
		}
		if d := Decide(s); d.Action != Hold || d.Reason != holdAtTarget {
			t.Fatalf("residue %d: %+v", residue, d)
		}
		// Counted once: every view's NAV grows by the residue's value (PYUSD
		// near par, floored), never twice.
		for _, view := range o.lanes {
			grew, slack := uint64(view.StrategyNAVRaw-base.StrategyNAVRaw), residue/100+1
			if view.StrategyNAVRaw != s.StrategyNAVRaw || grew > residue+slack || grew+slack < residue {
				t.Fatalf("residue %d: %s NAV grew %d", residue, view.RouteLane, grew)
			}
			if want := map[bool]int64{true: int64(residue), false: 0}[view.RouteLane == autoAUTOPYUSD.Lane]; view.DebtIdleRaw != want {
				t.Fatalf("%s reads PYUSD custody %d", view.RouteLane, view.DebtIdleRaw)
			}
		}
		st, err := readStepState(rwa, o.routeBatch.Accounts, autoAUTOPYUSD, s.Slot)
		if err != nil || st.heldDebt != residue || st.debtBalance != residue {
			t.Fatalf("AUTO/AUTO/PYUSD's step does not take the whole account: held %d (%v)", st.heldDebt, err)
		}
	}
}

// The live capture's vault NAV and approved-level observation identity, as
// origin/main 84d0bb70 computed them.
const (
	liveTotalVaultNAVRaw = 101_250_158_835
	liveStrategyNAVRaw   = 101_249_158_835
)

// Collateral custody is vault cash; rule 4a claims it for a lane only when
// that lane's next leg takes it. On the live book that leg is the AUTO
// deposit, which KLend refuses only below one collateral receipt (3 raw AUTO
// here), far under dust: every residue above dust deposits whole, and toward
// a withdrawal it converts whole. Collateral the leg would refuse is vault
// cash the lane's next step takes whole: it does not hold the top-up.
func TestLiveCollateralResidueGoesWhereItsNextLegTakesIt(t *testing.T) {
	t.Parallel()
	c := loadLiveCapture(t, "testdata/live_state_2026-10-12.json.gz")
	in := liveInputs(true)
	for _, residue := range []uint64{uint64(partialWithdrawalDustRaw), uint64(partialWithdrawalDustRaw) + 1, 25_000_000} {
		o := observeLiveAtRest(t, withAccount(c, autoAUTOPYUSD.CollateralCustody, tokenBalance(residue)), in)
		s := o.Snapshot
		views := slices.Clone(o.lanes)
		for _, view := range views {
			if view.CollateralIdleRaw != int64(residue) || view.MinimumCollateralDepositRaw != 3 {
				t.Fatalf("%s reads AUTO custody %d, deposit minimum %d", view.RouteLane, view.CollateralIdleRaw, view.MinimumCollateralDepositRaw)
			}
		}
		d := Decide(s)
		if residue == uint64(partialWithdrawalDustRaw) {
			if s.Target.Rule != holdAtTarget || d.Action != Hold || d.Reason != holdAtTarget {
				t.Fatalf("dust moved: %+v %+v", s.Target, d)
			}
			continue
		}
		if s.RouteLane != autoAUTOPYUSD.Lane || s.Target.Rule != ruleCustody || d.Action != OpenRouteStep || d.Reason != topupDepositReason || d.AmountRaw != int64(residue) {
			t.Fatalf("residue %d: %s %+v %+v", residue, s.RouteLane, s.Target, d)
		}
		// Toward a withdrawal the same custody converts to USDC, whole.
		for i := range views {
			views[i].WithdrawalDemandRaw, views[i].MinimumCollateralDepositRaw = views[i].VoltrIdleRaw+50_000_000, 0
		}
		i, tgt := target(views, in)
		out := s
		out.WithdrawalDemandRaw, out.MinimumCollateralDepositRaw, out.Target = views[i].WithdrawalDemandRaw, 0, tgt
		if d := Decide(out); views[i].RouteLane != s.RouteLane || tgt.Rule != ruleCustody || !tgt.Outbound || d.Action != SwapCollateralToStableStep || d.AmountRaw != int64(residue) {
			t.Fatalf("residue %d toward a withdrawal: %+v %+v", residue, tgt, d)
		}
	}
}

// An equity-zero exit returns the lane's equity as USDC: its last step
// withdraws the collateral and swaps it to USDC in one transaction, so none
// of it waits in the AUTO custody AUTO/AUTO/PYUSD shares. On the live book,
// with AUTO/AUTO/wYLDS's debt repaid: the exit's step is that atomic step on
// the wYLDS lane; once it lands no AUTO is left for AUTO/AUTO/PYUSD, whose
// position is untouched. The custody a separate withdrawal left (the shape
// this replaces) was deposited into AUTO/AUTO/PYUSD instead.
func TestLiveEquityZeroExitLeavesNoCollateralForTheSharingLane(t *testing.T) {
	t.Parallel()
	c := loadLiveCapture(t, "testdata/live_state_2026-10-12.json.gz")
	in := liveInputs(true)
	in.Exits = map[string]laneExit{autoAUTOWYLDSLane: {EquityZero: true}}
	base := observeLiveAtRest(t, c, liveInputs(true)).lanes
	wylds, err := runtimeRoute(autoAUTOWYLDSLane)
	if err != nil {
		t.Fatal(err)
	}
	repaid := withAccount(c, wylds.Kamino.Obligation, emptyObligation(false))
	o := observeLiveAtRest(t, repaid, in)
	s := o.Snapshot
	d := Decide(s)
	if s.RouteLane != autoAUTOWYLDSLane || s.Target.Rule != ruleOperatorExit || !s.Target.EquityZero || s.PositionDebtRaw != 0 || s.PositionCollateralRaw == 0 {
		t.Fatalf("exit target %s %+v", s.RouteLane, s.Target)
	}
	if up, atomic := stepDecision(s, d); d.Action != DeleverRouteStep || d.Reason != exitWithdrawReason || d.StrategyKey != autoAUTOWYLDSLane || up || !atomic {
		t.Fatalf("exit step %+v", d)
	}
	// The step landed: the wYLDS obligation is empty, the AUTO custody as it
	// was, and the swap's USDC in the Squads bridge account.
	landed := withAccount(withAccount(c, wylds.Kamino.Obligation, emptyObligation(true)), bridgeSquadsATA, tokenBalance(33_718_911_211))
	after := observeLiveAtRest(t, landed, in)
	if after.Snapshot.HeldLanes != autoAUTOPYUSD.Lane {
		t.Fatalf("held after the exit: %q", after.Snapshot.HeldLanes)
	}
	for _, view := range after.lanes {
		if view.CollateralIdleRaw != 0 || laneInFlight(view) {
			t.Fatalf("%s sees AUTO custody %d after the exit", view.RouteLane, view.CollateralIdleRaw)
		}
		if view.RouteLane == autoAUTOPYUSD.Lane && (view.PositionCollateralRaw != base[0].PositionCollateralRaw || view.PositionDebtRaw != base[0].PositionDebtRaw) {
			t.Fatalf("AUTO/AUTO/PYUSD moved: %+v", view)
		}
	}
	if tgt := after.Snapshot.Target; tgt.Rule == ruleCustody || tgt.Rule == ruleOperatorExit {
		t.Fatalf("after the exit: %+v", tgt)
	}
	// The replaced shape: a lone withdrawal left the wYLDS lane's AUTO in the
	// shared custody, and AUTO/AUTO/PYUSD's custody rule deposited it.
	parked := withAccount(withAccount(c, wylds.Kamino.Obligation, emptyObligation(true)), autoAUTOPYUSD.CollateralCustody, tokenBalance(32_918_240_839))
	if p := observeLiveAtRest(t, parked, in).Snapshot; p.RouteLane != autoAUTOPYUSD.Lane || Decide(p).Reason != topupDepositReason {
		t.Fatalf("parked AUTO: %s %+v %+v", p.RouteLane, p.Target, Decide(p))
	}
}

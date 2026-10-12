package backyard

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	runtime := routeObservationRuntime{
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

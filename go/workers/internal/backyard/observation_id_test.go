package backyard

import "testing"

func TestTargetObservationIDIsPinned(t *testing.T) {
	target := LaneTarget{Rule: ruleRotate, Level: 1.75, UpApproved: true, KeepAlive: true, EquityZero: true, Outbound: true, ReleaseRaw: 1, RepayRaw: 2, WithdrawRaw: 3, MoveRaw: 4, MoveTo: "lane-b", EntryRaw: 5}
	got, err := targetObservationID("prior", target, "inputs")
	if err != nil {
		t.Fatal(err)
	}
	if want := "f6b427c248528e926847b73e4b7a0b9da0e3d6347182e726abde4d30209873f9"; got != want {
		t.Fatalf("observation id = %s, want %s", got, want)
	}
}

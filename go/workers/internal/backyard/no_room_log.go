package backyard

import (
	"fmt"
	"io"
	"os"
	"time"
)

// noRoomLog reports a step that holds because the chain has no room for the
// move its target wants (a full debt reserve, no withdraw room, a release a
// lane cannot make). Every one counts on /metrics, where the alert lives; the
// line prints once an hour per lane and rule so it stays visible.
type noRoomLog struct {
	out     io.Writer
	printed map[string]time.Time
}

func (l *noRoomLog) note(now time.Time, s Snapshot, d Decision) {
	if d.Action != Hold || d.Reason != holdNoRoom {
		return
	}
	backyardEvents.stepNoRoom(d.StrategyKey, s.Target.Rule)
	key := d.StrategyKey + "|" + s.Target.Rule
	if l.printed == nil {
		l.printed = map[string]time.Time{}
	}
	if now.Sub(l.printed[key]) < time.Hour {
		return
	}
	l.printed[key] = now
	out := l.out
	if out == nil {
		out = os.Stderr
	}
	cause := "the chain has no room for the target's move"
	if s.BorrowUtilizationBlocked {
		cause = "borrowing blocked by the debt reserve utilization limit"
	}
	_, _ = fmt.Fprintf(out, "backyard-rwa-worker: no_room lane=%s rule=%s: %s\n", d.StrategyKey, s.Target.Rule, cause)
}

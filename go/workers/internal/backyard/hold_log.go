package backyard

import (
	"fmt"
	"io"
	"os"
)

// holdLog reports why a tick did not move the vault. A hold is not a journal
// row: only transactions are. It is the loyal_backyard_hold{lane,reason}
// gauge, one log line when it changes, and, for no_room, the
// loyal_backyard_step_no_room_total counter the safety alert pages on.
type holdLog struct {
	out          io.Writer
	events       *events
	lane, reason string
}

// note takes one tick's outcome: a hold reason (the enum, hold.go), or ""
// when the tick acts. detail is why: a manual stop's own reason, a hold's
// cause or simulation error.
func (l *holdLog) note(s Snapshot, lane, reason, detail string) {
	if reason == holdNoRoom {
		l.events.stepNoRoom(lane, s.Target.Rule)
	}
	l.events.hold(lane, reason)
	if lane == l.lane && reason == l.reason {
		return
	}
	l.lane, l.reason = lane, reason
	if reason == "" {
		return
	}
	out := l.out
	if out == nil {
		out = os.Stderr
	}
	cause := detail
	switch {
	case cause != "":
	case reason == holdNoRoom && s.BorrowUtilizationBlocked:
		cause = "borrowing blocked by the debt reserve utilization limit"
	case reason == holdNoRoom:
		cause = "the chain has no room for the target's move"
	}
	_, _ = fmt.Fprintf(out, "backyard-rwa-worker: hold lane=%s reason=%s rule=%s %s\n", lane, reason, s.Target.Rule, cause)
}

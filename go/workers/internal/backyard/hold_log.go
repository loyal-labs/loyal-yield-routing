package backyard

import (
	"fmt"
	"io"
	"os"
)

// holdManualRecovery is the hold reason of a manual recovery stop: the
// operator's latch, lifted only by `loyal-engine backyard clear-hold`.
const holdManualRecovery = "manual_recovery"

// holdLog reports why a tick did not move the vault. A hold is not a journal
// row: only transactions are. It is the loyal_backyard_hold{lane,reason}
// gauge, one log line when it changes, and, for no_room, the
// loyal_backyard_step_no_room_total counter the safety alert pages on.
type holdLog struct {
	out          io.Writer
	lane, reason string
}

// note takes one tick's outcome: a hold reason, or "" when the tick acts.
// detail names a manual stop's own reason.
func (l *holdLog) note(s Snapshot, lane, reason, detail string) {
	if reason == holdNoRoom {
		backyardEvents.stepNoRoom(lane, s.Target.Rule)
	}
	backyardEvents.hold(lane, reason)
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
	case reason == holdNoRoom && s.BorrowUtilizationBlocked:
		cause = "borrowing blocked by the debt reserve utilization limit"
	case reason == holdNoRoom:
		cause = "the chain has no room for the target's move"
	}
	_, _ = fmt.Fprintf(out, "backyard-rwa-worker: hold lane=%s reason=%s rule=%s %s\n", lane, reason, s.Target.Rule, cause)
}

package backyard

import (
	"fmt"
	"io"
	"os"
)

type holdLog struct {
	out    io.Writer
	events *events
	lane   string
	reason HoldReason
}

func (l *holdLog) acted(lane string) {
	l.events.hold(lane, holdNone)
	l.lane, l.reason = lane, holdNone
}

func (l *holdLog) note(s Snapshot, lane string, reason HoldReason, detail string) {
	if reason == holdNoRoom {
		l.events.stepNoRoom(lane, string(s.Target.Rule))
	}
	l.events.hold(lane, reason)
	if lane == l.lane && reason == l.reason {
		return
	}
	l.lane, l.reason = lane, reason
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

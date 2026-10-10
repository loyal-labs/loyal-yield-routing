package backyard

import (
	"fmt"
	"io"
	"os"
	"time"
)

// borrowBlockedLog: while Kamino blocks new borrowing the route simply holds
// (no retry loop); the reason is printed once per hour so it stays visible.
type borrowBlockedLog struct {
	out     io.Writer
	printed time.Time
}

func (l *borrowBlockedLog) note(now time.Time, d Decision) {
	if d.Reason != "debt_reserve_utilization_blocks_borrow" || now.Sub(l.printed) < time.Hour {
		return
	}
	l.printed = now
	out := l.out
	if out == nil {
		out = os.Stderr
	}
	_, _ = fmt.Fprintf(out, "backyard-rwa-worker: borrowing blocked by the debt reserve utilization limit; holding lane=%s\n", d.StrategyKey)
}

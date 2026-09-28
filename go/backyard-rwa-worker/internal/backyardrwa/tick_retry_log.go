package backyardrwa

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// tickRetryLog makes retried (non-fatal) tick errors visible on stderr. The
// same error text prints at most once a minute; a new text prints at once.
// Without it a leg that fails every tick before recording left no trace
// (live 2026-09-28: the top-up residue swap).
type tickRetryLog struct {
	out     io.Writer
	last    string
	printed time.Time
}

const tickRetryLogInterval = time.Minute

func (l *tickRetryLog) note(now time.Time, err error) {
	text := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(text) > 600 {
		text = text[:600]
	}
	if text == l.last && now.Sub(l.printed) < tickRetryLogInterval {
		return
	}
	l.last, l.printed = text, now
	out := l.out
	if out == nil {
		out = os.Stderr
	}
	_, _ = fmt.Fprintf(out, "backyard-rwa-worker: tick retried error=%q\n", text)
}

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

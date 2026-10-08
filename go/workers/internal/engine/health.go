package engine

import (
	"fmt"
	"time"
)

// Progress keeps capture and application clocks distinct. An empty observation
// cannot report readiness merely because a process is running.
type Progress struct {
	CapturedAt, AppliedAt time.Time
	MaxLag                time.Duration
}

func (p Progress) Ready(now time.Time) error {
	if p.CapturedAt.IsZero() || p.AppliedAt.IsZero() || p.MaxLag <= 0 {
		return fmt.Errorf("worker progress unavailable")
	}
	if p.CapturedAt.After(now) || p.AppliedAt.After(now) {
		return fmt.Errorf("worker progress clock invalid")
	}
	if now.Sub(p.CapturedAt) > p.MaxLag || now.Sub(p.AppliedAt) > p.MaxLag {
		return fmt.Errorf("worker progress stale")
	}
	return nil
}

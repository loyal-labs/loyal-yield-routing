package backyardrwa

import (
	"fmt"
	"os"
	"time"
)

// ponytail: temporary latency diagnostics for the AUTO send-window misses
// (ASK-2297); delete once sign-to-send reliably lands inside the plan window.
func logStage(stage string, start time.Time) {
	_, _ = fmt.Fprintf(os.Stderr, "backyard-rwa-worker: timing stage=%s ms=%d\n", stage, time.Since(start).Milliseconds())
}

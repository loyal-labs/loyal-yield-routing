package backyard

import (
	"fmt"
	"os"
	"time"
)

func logStage(stage string, start time.Time) {
	_, _ = fmt.Fprintf(os.Stderr, "backyard-rwa-worker: timing stage=%s ms=%d\n", stage, time.Since(start).Milliseconds())
}

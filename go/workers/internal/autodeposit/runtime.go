package autodeposit

import (
	"errors"
	"time"
)

const (
	runtimeHealthTimeout = 5 * time.Second
	runtimeCycleTimeout  = 120 * time.Second
)

// errRuntimeProofUnavailable is the sanitized error handed to OnError.
var errRuntimeProofUnavailable = errors.New("autodeposit runtime proof unavailable")

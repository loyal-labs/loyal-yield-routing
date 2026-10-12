package backyard_test

import (
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

// The Backyard engine composes into the shared process runtime as a plain lane.
var _ engine.Lane = (*backyard.Engine)(nil)

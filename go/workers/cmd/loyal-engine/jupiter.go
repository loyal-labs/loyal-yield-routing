package main

import (
	"context"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

func openJupiterBudget(ctx context.Context, dsn string, facts *engine.Facts) (*jupiter.Budget, func(), error) {
	startup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := db.Open(startup, dsn, 2)
	if err != nil {
		return nil, nil, retailError("Jupiter budget database", err)
	}
	budget, err := jupiter.NewBudget(startup, pool, facts)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	return budget, pool.Close, nil
}

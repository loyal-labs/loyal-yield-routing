package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/webapp"
)

const cronUsage = "usage: loyal-web cron smart-account-reconcile | earn-yield-joy [--dry-run]"

// runCron runs one of the jobs vercel.json used to schedule and prints the
// summary the Next route answered with. A job that cannot start, or that is
// refused the lock, exits non-zero so the timer's unit shows failed.
func runCron(ctx context.Context, args []string) error {
	dryRun := len(args) == 2 && args[1] == "--dry-run"
	if len(args) == 0 || (len(args) == 2 && !dryRun) || len(args) > 2 || (dryRun && args[0] != "earn-yield-joy") {
		return errors.New(cronUsage)
	}
	appDB := openDB(ctx, "cron", "WEB_APP_DATABASE_URL")
	if appDB == nil {
		return errors.New("cron needs WEB_APP_DATABASE_URL")
	}
	defer appDB.Close()
	yieldDB := openDB(ctx, "cron", "WEB_YIELD_DATABASE_URL")
	if yieldDB != nil {
		defer yieldDB.Close()
	}
	timescaleDB := openDB(ctx, "cron", "TIMESCALEDB_URL")
	if timescaleDB != nil {
		defer timescaleDB.Close()
	}
	var rpcClient *chain.Client
	if endpoint, err := engine.OptionalCredential("WEB_SOLANA_RPC_URL"); err != nil {
		return err
	} else if endpoint != "" {
		if rpcClient, err = chain.New(endpoint, 10*time.Second); err != nil {
			return errors.New("WEB_SOLANA_RPC_URL is not a usable RPC URL")
		}
	}
	webEarn := earn(ctx, prometheus.NewRegistry(), rpcClient, appDB, yieldDB, timescaleDB)
	if webEarn == nil {
		return errors.New("cron needs a valid LOYAL_WEB_SOLANA_CLUSTER")
	}
	crons := webapp.NewCrons(appDB, *webEarn)
	var summary any
	var err error
	switch args[0] {
	case "smart-account-reconcile":
		summary, err = crons.SmartAccountReconcile(ctx)
	case "earn-yield-joy":
		if yieldDB == nil || timescaleDB == nil {
			return errors.New("earn-yield-joy needs WEB_YIELD_DATABASE_URL and TIMESCALEDB_URL")
		}
		summary, err = crons.EarnYieldJoy(ctx, dryRun)
	default:
		return errors.New(cronUsage)
	}
	if encodeErr := json.NewEncoder(os.Stdout).Encode(summary); encodeErr != nil && err == nil {
		err = encodeErr
	}
	if err != nil {
		return fmt.Errorf("cron %s: %w", args[0], err)
	}
	return nil
}

package backyard

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

type withdrawalSettlementWorker struct {
	database                  *Database
	chain                     *chain.Client
	attempts                  *earnmax.WithdrawalAttemptStore
	log                       *slog.Logger
	interval, attemptTimeout  time.Duration
	batchSize                 int
	attemptCursor, costCursor string
	lastWarnings              map[string]string
}

func newWithdrawalSettlementWorker(database *Database, client *chain.Client, log *slog.Logger) *withdrawalSettlementWorker {
	if log == nil {
		log = slog.Default()
	}
	return &withdrawalSettlementWorker{database: database, chain: client, attempts: earnmax.NewWithdrawalAttemptStore(database.pool, client), log: log, interval: 10 * time.Second, attemptTimeout: 3 * time.Second, batchSize: 32, lastWarnings: map[string]string{}}
}

func (w *withdrawalSettlementWorker) Run(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		w.sweep(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (w *withdrawalSettlementWorker) warning(ctx context.Context, route, code string) {
	if ctx.Err() != nil || w.lastWarnings[route] == code {
		return
	}
	vault := "unknown"
	if v, err := vaultOfRoute(route); err == nil {
		vault = v.Name
	}
	log := w.log.With("service", "backyard-rwa-worker", "family", string(engine.FamilyBackyard), "vault", vault, "route_key", route)
	if code != "" {
		log.WarnContext(ctx, "backyard_withdrawal_settlement_unavailable", "reason", code)
	} else {
		log.InfoContext(ctx, "backyard_withdrawal_settlement_recovered")
	}
	w.lastWarnings[route] = code
}

func withdrawalSettlementError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "settlement_timeout"
	}
	if errors.Is(err, chain.ErrUnavailable) {
		return "settlement_rpc_unavailable"
	}
	return "settlement_evidence_unavailable"
}

type withdrawalTerminalCost struct{ id, route string }

func (w *withdrawalSettlementWorker) sweep(ctx context.Context) {
	scan, cancel := context.WithTimeout(ctx, 2*time.Second)
	var ready bool
	err := w.database.pool.QueryRow(scan, `SELECT to_regclass('loyal_yield.earn_max_withdrawal_claim_attempts') IS NOT NULL AND to_regclass('loyal_yield.backyard_withdrawal_cost_operations') IS NOT NULL`).Scan(&ready)
	cancel()
	if err != nil {
		w.warning(ctx, rwa.RouteKey, withdrawalSettlementError(err))
		return
	}
	if !ready {
		w.warning(ctx, rwa.RouteKey, "withdrawal_accounting_schema_unavailable")
		return
	}
	failures := map[string]string{}
	observed := map[string]bool{}
	if w.lastWarnings[rwa.RouteKey] == "withdrawal_accounting_schema_unavailable" {
		w.warning(ctx, rwa.RouteKey, "")
	}
	scan, cancel = context.WithTimeout(ctx, 2*time.Second)
	ids, err := w.attempts.PendingAttempts(scan, w.attemptCursor, w.batchSize)
	cancel()
	if err != nil {
		observed[rwa.RouteKey] = true
		failures[rwa.RouteKey] = withdrawalSettlementError(err)
	} else {
		if len(ids) == 0 {
			w.attemptCursor = ""
		}
		for _, id := range ids {
			observed[rwa.RouteKey] = true
			if ctx.Err() != nil {
				return
			}
			attempt, stop := context.WithTimeout(ctx, w.attemptTimeout)
			err := w.attempts.Reconcile(attempt, id)
			stop()
			w.attemptCursor = id
			if err != nil {
				failures[rwa.RouteKey] = withdrawalSettlementError(err)
			}
		}
	}
	scan, cancel = context.WithTimeout(ctx, 2*time.Second)
	rows, err := w.database.pool.Query(scan, `SELECT c.operation_id,o.route_key FROM loyal_yield.backyard_withdrawal_cost_operations c JOIN loyal_yield.multiply_operations o USING(operation_id) WHERE c.state IN ('reserved','unknown') AND o.status IN ('failed','reconciled','manual_recovery') AND c.operation_id>$1 ORDER BY c.operation_id LIMIT $2`, w.costCursor, w.batchSize)
	var costs []withdrawalTerminalCost
	if err == nil {
		for rows.Next() {
			var item withdrawalTerminalCost
			if err = rows.Scan(&item.id, &item.route); err != nil {
				break
			}
			costs = append(costs, item)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
	}
	cancel()
	if err != nil {
		observed[rwa.RouteKey] = true
		failures[rwa.RouteKey] = withdrawalSettlementError(err)
	} else {
		if len(costs) == 0 {
			w.costCursor = ""
		}
		for _, cost := range costs {
			if ctx.Err() != nil {
				return
			}
			observed[cost.route] = true
			attempt, stop := context.WithTimeout(ctx, w.attemptTimeout)
			err := w.database.releaseWithdrawalCost(attempt, cost.id)
			if err == nil {
				err = w.database.settleWithdrawalCost(attempt, w.chain, cost.id)
			}
			if err == nil {
				var state string
				err = w.database.pool.QueryRow(attempt, `SELECT state FROM loyal_yield.backyard_withdrawal_cost_operations WHERE operation_id=$1`, cost.id).Scan(&state)
				if err == nil && state == "unknown" {
					failures[cost.route] = "withdrawal_pool_cost_unknown"
				}
			}
			stop()
			w.costCursor = cost.id
			if err != nil {
				failures[cost.route] = withdrawalSettlementError(err)
			}
		}
	}
	for route := range observed {
		w.warning(ctx, route, failures[route])
	}
}

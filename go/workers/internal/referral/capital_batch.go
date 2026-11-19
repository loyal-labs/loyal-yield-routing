package referral

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// Each collector has its own 90-second deadline. Four sequential reads leave
// headroom within the web cron service's ten-minute timeout.
const capitalBatchSize = 4

type CapitalBatchReport struct {
	Active     bool           `json:"active"`
	Selected   int            `json:"selected"`
	Complete   int            `json:"complete"`
	Incomplete map[string]int `json:"incomplete,omitempty"`
	Failed     int            `json:"failed"`
}

func (s *Store) CollectCapitalBatch(ctx context.Context, c *chain.Client, dryRun bool) (CapitalBatchReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 7*time.Minute)
	defer cancel()
	var out CapitalBatchReport
	ids, active, err := s.capitalBatchIDs(ctx, dryRun)
	if err != nil {
		return out, err
	}
	out.Active, out.Selected = active, len(ids)
	out.Incomplete = make(map[string]int)
	var failures []error
	for _, id := range ids {
		report, err := s.CollectCapital(ctx, c, id, dryRun)
		if err != nil {
			out.Failed++
			failures = append(failures, fmt.Errorf("referee %s: %w", id, err))
		} else if report.Coverage == "complete" {
			out.Complete++
		} else {
			out.Incomplete[report.Reason]++
		}
	}
	return out, errors.Join(failures...)
}

// Reserve just one page in a short transaction. Concurrent runs select the next
// page. A crash costs at most one rotation and never skips capital history.
func (s *Store) capitalBatchIDs(ctx context.Context, dryRun bool) ([]string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	opts := pgx.TxOptions{}
	query := `SELECT EXISTS(SELECT FROM loyal_yield.referral_earn_max_program WHERE singleton),after_referee::text FROM loyal_yield.referral_earn_max_batch_cursor WHERE singleton`
	if dryRun {
		opts.AccessMode = pgx.ReadOnly
	} else {
		query += " FOR UPDATE"
	}
	tx, err := s.yield.BeginTx(ctx, opts)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	var active bool
	var cursor *string
	if err := tx.QueryRow(ctx, query).Scan(&active, &cursor); err != nil {
		return nil, false, err
	}
	if !active {
		return nil, false, nil
	}
	page := func(after *string) ([]string, error) {
		rows, err := tx.Query(ctx, `SELECT referee_app_user_id::text FROM loyal_yield.referral_bindings WHERE ($1::uuid IS NULL OR referee_app_user_id>$1) ORDER BY referee_app_user_id LIMIT $2`, after, capitalBatchSize)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, pgx.RowTo[string])
	}
	ids, err := page(cursor)
	if err != nil {
		return nil, false, err
	}
	if len(ids) == 0 && cursor != nil {
		ids, err = page(nil)
		if err != nil {
			return nil, false, err
		}
	}
	if !dryRun {
		var next *string
		if len(ids) == capitalBatchSize {
			next = &ids[len(ids)-1]
		}
		if _, err = tx.Exec(ctx, `UPDATE loyal_yield.referral_earn_max_batch_cursor SET after_referee=$1 WHERE singleton`, next); err != nil {
			return nil, false, err
		}
	}
	return ids, true, tx.Commit(ctx)
}

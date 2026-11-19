package backyard

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

// Prefer the last sale into the cash quote's output mint. If there is no such
// sale, identify the stored benchmark so a debt redemption stays distinguishable.
const exitReferenceSaleSQL = `SELECT reconciled_effects->'sale', expected_effects #>> '{expectedEffects,accounts,1,mint}', updated_at
	FROM loyal_yield.multiply_operations
	WHERE route_key = $1 AND status = 'reconciled' AND confirmation_status = 'finalized'
	AND action <> 'REPORT_NAV' AND confirmed_slot <= $2
	AND reconciled_effects #>> '{sale,slot}' = confirmed_slot::text
	AND reconciled_effects #>> '{sale,lane}' = $3
	AND expected_effects #>> '{expectedEffects,accounts,0,mint}' = $5
	AND reconciled_effects #>> '{sale,outputAccount}' = expected_effects #>> '{expectedEffects,accounts,1,address}'
	AND (expected_effects #>> '{expectedEffects,accounts,1,mint}' = $6
		OR (expected_effects #>> '{expectedEffects,accounts,1,mint}' = $7
			AND confirmed_slot = $2 AND reconciled_effects #>> '{sale,soldRaw}' = $4))
	ORDER BY (expected_effects #>> '{expectedEffects,accounts,1,mint}' = $6) DESC NULLS LAST,
	         confirmed_slot DESC, operation_id DESC LIMIT 1`

func (d *Database) readExitReferenceSale(ctx context.Context, m RouteManifest, stored ExitCost) (ExitCost, error) {
	route, err := m.route(stored.Lane)
	if err != nil {
		return ExitCost{}, err
	}
	var data []byte
	var proceeds string
	var at time.Time
	err = d.pool.QueryRow(ctx, exitReferenceSaleSQL, m.vault.RouteKey, stored.Slot, stored.Lane,
		strconv.FormatUint(stored.QuotedCollateralRaw, 10), route.Kamino.CollateralMint, fleet.USDCMint, route.Kamino.DebtMint).Scan(&data, &proceeds, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return stored, nil
	}
	if err != nil {
		return ExitCost{}, err
	}
	var sale realizedSale
	if err := json.Unmarshal(data, &sale); err != nil {
		return ExitCost{}, err
	}
	cost, valid := sale.exitCost(at)
	if !valid || (proceeds != fleet.USDCMint && (cost.BPS != stored.BPS || cost.MarkSF != stored.MarkSF)) {
		return ExitCost{}, staleHold("exit_comparison_receipt_mismatch")
	}
	cost.ProceedsMint = proceeds
	return cost, nil
}

// Each vault has one recorded realization per lane. Read its journal history
// once per changed observation instead of adding a journal scan to every quote.
func (d *Database) exitReferenceReader(m RouteManifest) func(context.Context, ExitCost) (ExitCost, error) {
	type reference struct{ stored, resolved ExitCost }
	var mu sync.Mutex
	latest := make(map[string]reference)
	return func(ctx context.Context, sale ExitCost) (ExitCost, error) {
		mu.Lock()
		previous, ok := latest[sale.Lane]
		mu.Unlock()
		if ok && previous.stored == sale {
			return previous.resolved, nil
		}
		resolved, err := d.readExitReferenceSale(ctx, m, sale)
		if err != nil {
			return ExitCost{}, err
		}
		mu.Lock()
		latest[sale.Lane] = reference{sale, resolved}
		mu.Unlock()
		return resolved, nil
	}
}

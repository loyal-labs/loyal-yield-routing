package backyard

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// One admitted release sequence, stored atomically with its first decision.
// Generation and operation identity prevent a foreign lane/sequence from
// supplying the ratio. Repays and shrinking demand do not clear this state.
// Route state keeps one per lane under 'partialWithdrawals', keyed by lane;
// each operation carries its own lane's copy under 'partialWithdrawal'.
type partialWithdrawalState struct {
	Lane        string `json:"lane"`
	OperationID string `json:"operationId"`
	Generation  int64  `json:"generation"`
	LTVBPS      int64  `json:"ltvBps"`
}

func decodePartialWithdrawal(raw []byte) (*partialWithdrawalState, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var p partialWithdrawalState
	if json.Unmarshal(raw, &p) != nil || !earnActiveLane(p.Lane) || !sha256Pattern.MatchString(p.OperationID) || p.Generation <= 0 || p.LTVBPS < 0 || p.LTVBPS > leverageMaxLTVBPS {
		return nil, budgetHold("invalid_partial_withdrawal_state")
	}
	return &p, nil
}

// decodePartialWithdrawals decodes route state 'partialWithdrawals': one
// valid sequence per lane, each stored under its own lane.
func decodePartialWithdrawals(raw []byte) (map[string]partialWithdrawalState, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var entries map[string]json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return nil, budgetHold("invalid_partial_withdrawal_state")
	}
	out := make(map[string]partialWithdrawalState, len(entries))
	for lane, entry := range entries {
		p, err := decodePartialWithdrawal(entry)
		if err != nil || p == nil || p.Lane != lane {
			return nil, budgetHold("invalid_partial_withdrawal_state")
		}
		out[lane] = *p
	}
	return out, nil
}

func partialWithdrawalFor(entries map[string]partialWithdrawalState, lane string) *partialWithdrawalState {
	p, ok := entries[lane]
	if !ok {
		return nil
	}
	return &p
}

func applyPartialWithdrawal(s *Snapshot, p *partialWithdrawalState) error {
	s.PartialWithdrawalLTVBPS, s.PartialWithdrawalOperationID = 0, ""
	if p == nil {
		return nil
	}
	if p.Lane != s.RouteLane {
		return budgetHold("partial_withdrawal_lane_changed")
	}
	s.PartialWithdrawalLTVBPS, s.PartialWithdrawalOperationID = p.LTVBPS, p.OperationID
	return nil
}

// LoadPartialWithdrawal reads one lane's sequence and proves its origin.
func (d *Database) LoadPartialWithdrawal(ctx context.Context, key, lane string) (*partialWithdrawalState, error) {
	var raw []byte
	if err := d.pool.QueryRow(ctx, `SELECT state->'partialWithdrawals'->$2 FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key, lane).Scan(&raw); err != nil {
		return nil, err
	}
	p, err := decodePartialWithdrawal(raw)
	if err != nil || p == nil {
		return p, err
	}
	if p.Lane != lane {
		return nil, budgetHold("invalid_partial_withdrawal_state")
	}
	if err = d.validatePartialWithdrawalOrigin(ctx, d.pool, key, raw); err != nil {
		return nil, err
	}
	return p, nil
}

func partialWithdrawalInFlight(s Snapshot) bool {
	return s.PartialWithdrawalOperationID != "" && (s.CollateralIdleRaw > partialWithdrawalDustRaw || debtCashRaw(s) > partialWithdrawalDustRaw || s.SquadsIdleRaw > partialWithdrawalDustRaw || s.VoltrStrategyIdleRaw > 0)
}

func partialWithdrawalTerminal(s Snapshot) bool {
	return s.PartialWithdrawalOperationID != "" && !partialWithdrawalInFlight(s) && s.Nonterminal == "" && s.WithdrawalDemandRaw <= s.VoltrIdleRaw &&
		(s.PositionDebtRaw == 0 || s.LTVBPS <= s.PartialWithdrawalLTVBPS+leverageUpNearBPS)
}

type partialWithdrawalQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// validatePartialWithdrawalOrigin proves one lane's stored sequence (raw,
// whose own lane keys it) was opened by that lane's admitted release.
func (d *Database) validatePartialWithdrawalOrigin(ctx context.Context, q partialWithdrawalQuerier, key string, raw []byte) error {
	var valid bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_route_states s JOIN loyal_yield.multiply_operations o ON o.operation_id=$2::jsonb->>'operationId'
 WHERE s.route_key=$1 AND s.state->'partialWithdrawals'->($2::jsonb->>'lane')=$2::jsonb
 AND o.route_key=s.route_key AND o.strategy_key=$2::jsonb->>'lane'
 AND o.expected_effects->'decision'->>'reason'='withdrawal_partial_release'
 AND o.expected_effects->'partialWithdrawal'=$2::jsonb
 AND ($2::jsonb->>'generation')::bigint<=s.state_version)`, key, string(raw)).Scan(&valid)
	if err != nil {
		return err
	}
	if !valid {
		return budgetHold("partial_withdrawal_origin_changed")
	}
	return nil
}

// Called under the admission/send route lock before action-specific fences.
func (d *Database) authorizePartialWithdrawalTx(ctx context.Context, tx pgx.Tx, id string) error {
	var key string
	var current, bound []byte
	if err := tx.QueryRow(ctx, `SELECT s.route_key,s.state->'partialWithdrawals'->o.strategy_key,o.expected_effects->'partialWithdrawal' FROM loyal_yield.multiply_operations o JOIN loyal_yield.multiply_route_states s USING(route_key) WHERE o.operation_id=$1`, id).Scan(&key, &current, &bound); err != nil {
		return err
	}
	a, err := decodePartialWithdrawal(current)
	if err != nil {
		return err
	}
	b, err := decodePartialWithdrawal(bound)
	if err != nil {
		return err
	}
	if a == nil && b == nil {
		return nil
	}
	if a == nil || b == nil || *a != *b {
		return budgetHold("partial_withdrawal_state_changed")
	}
	return d.validatePartialWithdrawalOrigin(ctx, tx, key, current)
}

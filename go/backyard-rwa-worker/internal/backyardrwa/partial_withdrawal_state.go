package backyardrwa

import (
	"context"
	"encoding/json"
	"github.com/jackc/pgx/v5"
)

// One admitted release sequence, stored atomically with its first decision.
// Generation and operation identity prevent a foreign lane/sequence from
// supplying the ratio. Repays and shrinking demand do not clear this state.
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
	if json.Unmarshal(raw, &p) != nil || !leverageLane(p.Lane) || !sha256Pattern.MatchString(p.OperationID) || p.Generation <= 0 || p.LTVBPS < 0 || p.LTVBPS > leverageMaxLTVBPS {
		return nil, budgetHold("invalid_partial_withdrawal_state")
	}
	return &p, nil
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

func (d *Database) LoadPartialWithdrawal(ctx context.Context, key string) (*partialWithdrawalState, error) {
	var raw []byte
	if err := d.pool.QueryRow(ctx, `SELECT state->'partialWithdrawal' FROM loyal_yield.multiply_route_states WHERE route_key=$1`, key).Scan(&raw); err != nil {
		return nil, err
	}
	p, err := decodePartialWithdrawal(raw)
	if err != nil || p == nil {
		return p, err
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

func (d *Database) validatePartialWithdrawalOrigin(ctx context.Context, q partialWithdrawalQuerier, key string, raw []byte) error {
	var valid bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.multiply_route_states s JOIN loyal_yield.multiply_operations o ON o.operation_id=s.state->'partialWithdrawal'->>'operationId'
 WHERE s.route_key=$1 AND s.state->'partialWithdrawal'=$2::jsonb
 AND o.route_key=s.route_key AND o.strategy_key=s.state->'partialWithdrawal'->>'lane'
 AND o.expected_effects->'decision'->>'reason'='withdrawal_partial_release'
 AND o.expected_effects->'partialWithdrawal'=s.state->'partialWithdrawal'
 AND (s.state->'partialWithdrawal'->>'generation')::bigint<=s.state_version)`, key, string(raw)).Scan(&valid)
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
	if err := tx.QueryRow(ctx, `SELECT s.route_key,s.state->'partialWithdrawal',o.expected_effects->'partialWithdrawal' FROM loyal_yield.multiply_operations o JOIN loyal_yield.multiply_route_states s USING(route_key) WHERE o.operation_id=$1`, id).Scan(&key, &current, &bound); err != nil {
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

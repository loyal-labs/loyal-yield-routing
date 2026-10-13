package backyard

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

// WithdrawalHealth is display-only. No decision, admission or signer reads it.
// Its clock is the coherent observation clock, never the assessment/write time.
type WithdrawalHealth struct {
	// Assessment-only metadata, never serialized: routine NAV/recovery work
	// can wait normally but is not proof a prior intervention was resolved.
	preserveAttention bool
	Version           int        `json:"version"`
	Cluster           string     `json:"cluster"`
	Vault             string     `json:"vault"`
	Program           string     `json:"program"`
	RouteKey          string     `json:"routeKey"`
	Status            string     `json:"status"`
	Reason            string     `json:"reason"`
	ObservedAt        time.Time  `json:"observedAt"`
	ObservedSlot      int64      `json:"observedSlot"`
	BlockedSince      *time.Time `json:"blockedSince"`
}

func assessWithdrawalHealth(o Observation, decision Decision, tickErr error) (WithdrawalHealth, bool) {
	s := o.Snapshot
	// Health-hold snapshots carry no decoded custody/demand; zero is not evidence.
	if o.Validate() != nil || !s.Fresh || s.ManualReason != "" || s.StrategyReceiptIntegrityFault ||
		s.WithdrawalDemandRaw < 0 || s.VoltrIdleRaw < 0 {
		return WithdrawalHealth{}, false
	}
	h := WithdrawalHealth{Version: 1, Cluster: "mainnet-beta", Vault: bridgeVoltrVault,
		Program: voltr.ProgramID.String(), RouteKey: productionRouteKey,
		ObservedAt: o.ObservedAt.UTC(), ObservedSlot: s.Slot}
	switch {
	case s.WithdrawalDemandRaw == 0:
		h.Status, h.Reason = "none", "no_withdrawal_demand"
	case s.WithdrawalDemandRaw <= s.VoltrIdleRaw:
		h.Status, h.Reason = "waiting", "withdrawal_covered"
	default:
		switch {
		case decision.Action == HoldManualRecovery:
			h.Status, h.Reason = "operator_attention", "withdrawal_intervention_required"
		case tickErr != nil || decision.Action == Hold:
			h.Status, h.Reason = "unavailable", "withdrawal_evidence_unavailable"
		default:
			h.Status, h.Reason = "waiting", "withdrawal_in_progress"
			h.preserveAttention = decision.Action == ReportNAV || decision.Action == RecoverTransaction
		}
	}
	return h, true
}

// mergeWithdrawalHealth never reports recovery from an unavailable assessment.
// Keeping the old observation clock also keeps the freshness alert honest.
func mergeWithdrawalHealth(previous *WithdrawalHealth, next WithdrawalHealth) WithdrawalHealth {
	if previous != nil && previous.Version == next.Version && previous.Cluster == next.Cluster &&
		previous.Vault == next.Vault && previous.Program == next.Program && previous.RouteKey == next.RouteKey {
		if next.ObservedSlot < previous.ObservedSlot || next.ObservedAt.Before(previous.ObservedAt) ||
			(next.Status == "unavailable" || next.preserveAttention) && previous.Status == "operator_attention" {
			return *previous
		}
		if next.Status == "operator_attention" && previous.Status == "operator_attention" {
			next.BlockedSince = previous.BlockedSince
		}
	}
	if next.Status == "operator_attention" && next.BlockedSince == nil {
		since := next.ObservedAt
		next.BlockedSince = &since
	}
	return next
}

// RecordWithdrawalHealth stores this display projection under the route row
// lock. It changes no money.
func (d *Database) RecordWithdrawalHealth(ctx context.Context, next WithdrawalHealth) error {
	if next.RouteKey != productionRouteKey || next.Version != 1 || next.Cluster != "mainnet-beta" || next.Vault != bridgeVoltrVault || next.Program != voltr.ProgramID.String() {
		return fmt.Errorf("withdrawal health is not the production route's")
	}
	if err := d.inTx(ctx, func(tx pgx.Tx) error {
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT state->'withdrawalHealth' FROM loyal_yield.multiply_route_states WHERE route_key=$1 FOR UPDATE`, next.RouteKey).Scan(&raw); err != nil {
			return err
		}
		var previous *WithdrawalHealth
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &previous); err != nil {
				return err
			}
		}
		next = mergeWithdrawalHealth(previous, next)
		raw, err := json.Marshal(next)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{withdrawalHealth}',$2::jsonb,true), updated_at=clock_timestamp() WHERE route_key=$1`, next.RouteKey, string(raw))
		return err
	}); err != nil {
		return err
	}
	backyardEvents.withdrawalHealthStored(next)
	return nil
}

// Display failures never change execution or overwrite the last durable health.
func (w *Worker) publishWithdrawalHealth(ctx context.Context, o Observation, decision Decision, tickErr error) {
	if w.runtime.withdrawalHealth == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if o.Validate() != nil {
		// Never add network observation to signed-wire polling or failed
		// execution reads. Only a durable manual stop has no live work to delay.
		if decision.Action != HoldManualRecovery || w.runtime.observeWithdrawalHealth == nil {
			return
		}
		var err error
		o, err = w.runtime.observeWithdrawalHealth(ctx)
		if err != nil {
			return
		}
	}
	if h, ok := assessWithdrawalHealth(o, decision, tickErr); ok {
		_ = w.runtime.withdrawalHealth(ctx, h)
	}
}

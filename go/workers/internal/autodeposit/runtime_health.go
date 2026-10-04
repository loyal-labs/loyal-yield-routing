package autodeposit

import (
	"context"
	"errors"
	"time"
)

const (
	runtimeHealthTimeout = 5 * time.Second
	runtimeCycleTimeout  = 120 * time.Second
)

var errRuntimeProofUnavailable = errors.New("autodeposit runtime proof unavailable")

// ConfirmedSlotReader is a consumer capability, separate from financial chain
// effects. Production binds RPCChain; an unavailable frontier is never zero.
type ConfirmedSlotReader interface {
	ConfirmedSlot(context.Context) (int64, error)
}

// SetRuntimeReporter must be called before Run. A nil callback preserves the
// standalone worker API; reporting requires a genuine chain frontier.
func (w *Worker) SetRuntimeReporter(report func(bool, uint64)) { w.runtimeReporter = report }
func (r *ControlReconciler) SetRuntimeReporter(report func(bool, uint64)) {
	r.runtimeReporter = report
}
func (w *Worker) reportRuntime(ready bool, slot uint64) {
	if w.runtimeReporter != nil {
		w.runtimeReporter(ready, slot)
	}
}
func (r *ControlReconciler) reportRuntime(ready bool, slot uint64) {
	if r.runtimeReporter != nil {
		r.runtimeReporter(ready, slot)
	}
}

// runtimeRecoveryHealth only observes durable state. It does not release,
// adopt, replace, or classify a signed intent from a balance or lease timeout.
// Controller.Execute releases its lease before returning and exposes no
// fenced journal version. Therefore remaining signed custody conservatively
// holds readiness until settled; a post-return DB read cannot prove adoption.
// Ordinary live unsigned claims and future scheduled slots are healthy work.
func runtimeRecoveryHealth(ctx context.Context, store *Store, chain ConfirmedSlotReader, includeProjection bool) (uint64, error) {
	if store == nil || store.pool == nil || chain == nil {
		return 0, errRuntimeProofUnavailable
	}
	probe, cancel := context.WithTimeout(ctx, runtimeHealthTimeout)
	defer cancel()
	slot, err := chain.ConfirmedSlot(probe)
	if err != nil || slot <= 0 {
		return 0, errRuntimeProofUnavailable
	}
	var blocked bool
	err = store.pool.QueryRow(probe, `SELECT
 EXISTS(SELECT 1 FROM loyal_yield.autodeposit_reconciliation_requests
        WHERE requested_slot>processed_slot AND EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_targets target WHERE target.id=target_id AND target.cluster='mainnet-beta'))
 OR EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_targets t
   WHERE t.cluster='mainnet-beta' AND t.token_mint=$1 AND t.desired_active AND t.chain_status<>'closed' AND (
    t.chain_status<>'active' OR t.chain_observation_slot<=0
    OR t.wallet_balance_floor_raw IS NULL
    OR t.bootstrap_generation IS DISTINCT FROM t.setup_generation
    OR NULLIF(btrim(t.subscription_authority),'') IS NULL
    OR NULLIF(btrim(t.recurring_delegation),'') IS NULL
    OR t.recurring_delegation_nonce IS NULL
    OR t.max_amount_per_period IS NULL OR t.max_amount_per_period<=0
    OR t.period_length_seconds IS NULL OR t.period_length_seconds<=0
    OR t.start_timestamp IS NULL OR t.recurring_delegation_expiry_timestamp IS NULL
    OR NULLIF(btrim(t.policy_signature),'') IS NULL OR COALESCE(t.policy_confirmed_slot,0)<=0
    OR NULLIF(btrim(t.recurring_delegation_signature),'') IS NULL
    OR COALESCE(t.recurring_delegation_confirmed_slot,0)<=0))
 OR EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_targets target WHERE target.cluster='mainnet-beta' AND target.chain_status<>'closed' AND target.token_mint=$1 AND (`+mainnetSourceAheadSQL+`))
	OR EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_lot_claims c
	  WHERE c.status='selected' AND (
	   c.autodeposit_executor_lease_expires_at IS NULL
	   OR c.autodeposit_executor_lease_expires_at<=clock_timestamp()
	   OR (c.autodeposit_deposit_plan IS NOT NULL AND (
	    c.autodeposit_deposit_plan->>'version' IS DISTINCT FROM '1'
	    OR c.autodeposit_deposit_plan->>'amountRaw' IS DISTINCT FROM c.amount_raw::text
	    OR c.autodeposit_deposit_plan->'target'->>'id' IS DISTINCT FROM c.target_id::text))))
 OR EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_transaction_attempts a
   JOIN loyal_yield.balance_sweep_lot_claims c ON c.claim_token=a.claim_token
   WHERE c.status<>'executed' AND a.attempt_state IN('prepared','submitted','confirmed','unknown','ambiguous'))
 OR EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_destination_setup_attempts
   WHERE attempt_state IN('prepared','submitted','unknown','ambiguous'))
 OR ($3 AND EXISTS(SELECT 1 FROM loyal_yield.balance_sweep_wallet_balance_events e
   JOIN loyal_yield.balance_sweep_targets t ON t.id=e.target_id
   WHERE t.cluster='mainnet-beta' AND t.token_mint=$1 AND e.mint=t.token_mint AND e.event_id>COALESCE(
    (SELECT last_event_id FROM loyal_yield.projection_offsets WHERE consumer_name=$4),0)))`,
		USDCMint, slot, includeProjection, ConsumerName).Scan(&blocked)
	if err != nil || blocked || probe.Err() != nil {
		return 0, errRuntimeProofUnavailable
	}
	return uint64(slot), nil
}

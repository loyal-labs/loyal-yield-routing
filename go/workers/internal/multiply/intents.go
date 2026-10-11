package multiply

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Intent is one Earn MAX withdraw or cancel memo of a route. The observer
// records it in earn_max_intents; the worker decides it under the route lease.
type Intent struct {
	Signature        string
	InstructionIndex int32
	Kind             string
	RequestID        string
	// AmountRaw nil is a "max" withdrawal.
	AmountRaw   *uint64
	Destination *string
	Slot        uint64
	ObservedAt  time.Time
}

// pendingIntents lists the route's undecided intents in chain order.
func (s *Store) pendingIntents(ctx context.Context, settings, vault string) ([]Intent, error) {
	rows, err := s.pool.Query(ctx, `
        SELECT signature, instruction_index, kind, request_id, amount_raw::text, destination, slot, observed_at
        FROM loyal_yield.earn_max_intents
        WHERE settings=$1 AND vault=$2 AND status='pending'
        ORDER BY slot, transaction_index, instruction_index`, settings, vault)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var intents []Intent
	for rows.Next() {
		var intent Intent
		var amount *string
		var slot int64
		if err := rows.Scan(&intent.Signature, &intent.InstructionIndex, &intent.Kind, &intent.RequestID, &amount, &intent.Destination, &slot, &intent.ObservedAt); err != nil {
			return nil, err
		}
		if amount != nil {
			value, err := strconv.ParseUint(*amount, 10, 64)
			if err != nil {
				return nil, err
			}
			intent.AmountRaw = &value
		}
		intent.Slot = uint64(slot)
		intents = append(intents, intent)
	}
	return intents, rows.Err()
}

// decideIntent moves the route by one intent and returns its operation action
// and evidence, or the reason the route refuses it. A "max" withdrawal takes
// the route's equity.
func (s *RouteState) decideIntent(intent Intent, equityRaw uint64) (MultiplyAction, map[string]any, string) {
	switch intent.Kind {
	case "withdraw":
		amount := equityRaw
		if intent.AmountRaw != nil {
			amount = *intent.AmountRaw
		}
		switch {
		case s.WithdrawalMatches(intent.RequestID, *intent.Destination, amount):
			return "", nil, "no_action"
		case s.Withdrawal != nil && s.Withdrawal.RequestID == intent.RequestID:
			return "", nil, "request_id_reused"
		case s.Withdrawal != nil && s.Withdrawal.Status != WithdrawalClaimed:
			return "", nil, "withdrawal_already_pending"
		case amount == 0:
			return "", nil, "nothing_to_withdraw"
		}
		if _, err := s.RequestWithdrawal(intent.RequestID, *intent.Destination, amount, intent.ObservedAt); err != nil || s.ValidatePersisted() != nil {
			return "", nil, "invalid_state"
		}
		return ActionRequestWithdrawal, map[string]any{"kind": "withdraw", "requestId": intent.RequestID, "destinationAccount": *intent.Destination, "amountRaw": amount}, ""
	case "cancel":
		if s.Withdrawal == nil || s.Withdrawal.RequestID != intent.RequestID {
			return "", nil, "unknown_request"
		}
		if err := s.CancelWithdrawal(intent.RequestID); err != nil || s.ValidatePersisted() != nil {
			return "", nil, "invalid_state"
		}
		return ActionCancelWithdrawal, map[string]any{"kind": "cancel", "requestId": intent.RequestID}, ""
	}
	return "", nil, "invalid_state"
}

// applyIntent commits the route change, its request_withdrawal or
// cancel_withdrawal operation and the applied decision together.
func (s *Store) applyIntent(ctx context.Context, lease *Lease, route *RouteState, intent Intent, action MultiplyAction, evidence map[string]any) error {
	if err := validateNextRoute(lease, route); err != nil {
		return err
	}
	idempotencyKey := fmt.Sprintf("%s:intent:%s:%d", EngineVersion, intent.Signature, intent.InstructionIndex)
	evidenceBytes, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	effects, err := json.Marshal(map[string]any{"tokenDeltas": []any{}, "obligationDelta": nil, "intent": json.RawMessage(evidenceBytes)})
	if err != nil {
		return err
	}
	encoded, err := jsonMarshal(route)
	if err != nil {
		return err
	}
	reconciliation := sha256.Sum256(evidenceBytes)
	operationDigest := sha256.Sum256([]byte(idempotencyKey))
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	version, ok, err := updateRouteInTx(ctx, tx, lease, encoded)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("Earn MAX intent lost its route lease")
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO loyal_yield.multiply_operations (
            operation_id, route_key, cycle, engine_version, action, status,
            idempotency_key, expected_effects, transaction_signature,
            source_instruction_index, confirmed_slot, reconciliation_sha256,
            created_at, updated_at
        ) VALUES (
            $1, $2, $3, $4, $5, 'reconciled',
            $6, $7, $8, $9, $10, $11, $12, $12
        )`, "intent-"+hex.EncodeToString(operationDigest[:])[:32], route.RouteKey, int64(route.Cycle), EngineVersion, string(action),
		idempotencyKey, effects, intent.Signature, intent.InstructionIndex, int64(intent.Slot),
		hex.EncodeToString(reconciliation[:]), intent.ObservedAt); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
        UPDATE loyal_yield.earn_max_intents SET status='applied', decided_at=now()
        WHERE signature=$1 AND instruction_index=$2 AND status='pending'`, intent.Signature, intent.InstructionIndex); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	lease.Version = version
	return nil
}

// refuseIntent records that the route cannot take the intent; it is never
// retried.
func (s *Store) refuseIntent(ctx context.Context, intent Intent, reason string) error {
	_, err := s.pool.Exec(ctx, `
        UPDATE loyal_yield.earn_max_intents SET status='refused', refusal_reason=$3, decided_at=now()
        WHERE signature=$1 AND instruction_index=$2 AND status='pending'`, intent.Signature, intent.InstructionIndex, reason)
	return err
}

// ApplyPendingIntents decides the route's pending intents oldest first under
// the worker's lease, before planning. A "max" withdrawal resolves from the
// equity the worker observed this tick. It reports whether the route moved.
func (s *Store) ApplyPendingIntents(ctx context.Context, lease *Lease, route *RouteState, equityUSD string) (bool, error) {
	intents, err := s.pendingIntents(ctx, route.Settings, route.Vault)
	if err != nil || len(intents) == 0 {
		return false, err
	}
	// USDC raw units are USD micros.
	equity, err := strconv.ParseUint(equityUSD, 10, 64)
	if err != nil {
		return false, fmt.Errorf("route equity does not fit u64: %w", err)
	}
	next, moved := *route, false
	for _, intent := range intents {
		candidate := next
		action, evidence, reason := candidate.decideIntent(intent, equity)
		if reason != "" {
			if err := s.refuseIntent(ctx, intent, reason); err != nil {
				return moved, err
			}
			continue
		}
		if err := s.applyIntent(ctx, lease, &candidate, intent, action, evidence); err != nil {
			return moved, err
		}
		next, moved = candidate, true
	}
	return moved, nil
}

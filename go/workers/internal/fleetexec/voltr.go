package fleetexec

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/mr-tron/base58"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// The Backyard Voltr manager route on the generic signed-submission
// lifecycle, ported from loyal-fleet-worker voltr.rs and
// voltr_reconciliation.rs. Rows are the ones Rust writes: an execute lease on
// the opportunity, one policy-paid signed_route_submissions row with the
// vault and reserve semantic conflicts, and a planned
// voltr_manager_operation decision. Landing is the shared land().

type voltrLease struct {
	OpportunityID, VaultID, FencingToken, EpochID, AmountRaw, FeeCap int64
	Cluster, TargetReserve                                           string
	SourceReserve, RouteFingerprint, RequirementsFingerprint         *string
	ExpiresAt                                                        time.Time
	Plan                                                             json.RawMessage
}

type voltrPlan struct {
	Kind, RouteID, RouteSpec, Bundle, RouteFingerprint, Requirements string
	Manager, Guardian, Vault, Strategy, Operation, SourceKind        string
	TargetKind, Receipts, State, Addresses, Intent                   string
	Amount, MaxAmount, Slot                                          int64
	PreTotal, PreIdle, PrePosition                                   uint64
}

func decodeVoltrPlan(raw json.RawMessage) (voltrPlan, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return voltrPlan{}, err
	}
	var p voltrPlan
	str := func(key string, out *string) {
		_ = json.Unmarshal(m[key], out)
	}
	num := func(key string, out any) {
		_ = json.Unmarshal(m[key], out)
	}
	str("kind", &p.Kind)
	str("route_id", &p.RouteID)
	str("route_spec_sha256", &p.RouteSpec)
	str("route_bundle_sha256", &p.Bundle)
	str("route_fingerprint", &p.RouteFingerprint)
	str("requirements_fingerprint", &p.Requirements)
	str("manager", &p.Manager)
	str("guardian", &p.Guardian)
	str("vault", &p.Vault)
	str("strategy_id", &p.Strategy)
	str("operation", &p.Operation)
	str("source_kind", &p.SourceKind)
	str("target_kind", &p.TargetKind)
	str("receipt_set_fingerprint", &p.Receipts)
	str("protected_state_sha256", &p.State)
	str("protected_address_set_sha256", &p.Addresses)
	str("intent_sha256", &p.Intent)
	num("amount_raw", &p.Amount)
	num("max_operation_amount_raw", &p.MaxAmount)
	num("protected_context_slot", &p.Slot)
	num("pre_total_value_raw", &p.PreTotal)
	num("pre_idle_raw", &p.PreIdle)
	num("pre_position_raw", &p.PrePosition)
	if p.Kind != fleet.VoltrKind {
		return p, errors.New("not a voltr_kamino plan")
	}
	return p, nil
}

// admitVoltr is voltr.rs prepare(): every caller-controlled field must match
// the embedded bundle, and the intent hash must recompute.
func admitVoltr(r fleet.VoltrRoute, l voltrLease) (voltrPlan, int, error) {
	p, err := decodeVoltrPlan(l.Plan)
	if err != nil {
		return p, 0, err
	}
	strategy, ok := r.StrategyIndex(p.Strategy)
	if !ok || l.Cluster != r.Cluster || p.Bundle != r.BundleSHA256 || p.RouteFingerprint != r.BundleSHA256 || l.RouteFingerprint == nil || *l.RouteFingerprint != r.BundleSHA256 ||
		p.Manager != r.Manager || p.Guardian != r.Guardian || p.Vault != r.Vault || p.Amount <= 0 || uint64(p.Amount) > r.MaxOperationRaw || uint64(p.MaxAmount) != r.MaxOperationRaw || p.Amount != l.AmountRaw || p.Slot <= 0 {
		return p, 0, errors.New("voltr plan does not match the embedded route bundle")
	}
	requirements, err := r.RequirementsFingerprint(strategy, p.Operation)
	if err != nil || l.RequirementsFingerprint == nil || *l.RequirementsFingerprint != requirements {
		return p, 0, errors.New("voltr requirements fingerprint drifted")
	}
	reserve := r.Strategies[strategy].Reserve
	switch {
	case p.Operation == "deposit" && p.SourceKind == "voltr_idle" && p.TargetKind == "voltr_strategy" && l.TargetReserve == reserve:
	case p.Operation == "withdraw" && p.SourceKind == "voltr_strategy" && p.TargetKind == "voltr_idle" && l.SourceReserve != nil && *l.SourceReserve == reserve:
	default:
		return p, 0, errors.New("voltr source/target kinds or reserve drifted")
	}
	if p.Intent != r.IntentSHA256(strategy, p.Operation, uint64(p.Amount), p.Slot, p.Receipts, p.State, p.Addresses) {
		return p, 0, errors.New("voltr intent hash drifted")
	}
	return p, strategy, nil
}

type voltrSigned struct {
	payer                   string
	wire                    WireIdentity
	fee                     int64
	writable, conflicts     []string
	requirements, selection string
	epochs                  []byte
}

// executeVoltr claims one Voltr opportunity, builds and signs its manager
// transaction and hands it to the signed-submission lifecycle. Nothing is
// sent here; the next tick's claim lands it.
func (w *Worker) executeVoltr(ctx context.Context) error {
	if w.voltr == nil || !w.voltrGuardian {
		return nil
	}
	l, err := w.store.claimVoltr(ctx, w.config.Cluster, w.config.Owner, w.config.LeaseTTL)
	if err != nil || l == nil {
		return err
	}
	ctx, cancel := context.WithDeadline(ctx, leaseWorkDeadline(l.ExpiresAt, w.config.LeaseTTL))
	defer cancel()
	signed, err := w.signVoltr(ctx, *l)
	if err != nil {
		// The lease lapses and a later tick admits the opportunity afresh.
		return fmt.Errorf("voltr admission failed closed: %w", err)
	}
	return w.store.persistVoltr(ctx, w.config.Owner, *l, signed)
}

func (w *Worker) signVoltr(ctx context.Context, l voltrLease) (voltrSigned, error) {
	r := *w.voltr
	p, strategy, err := admitVoltr(r, l)
	if err != nil {
		return voltrSigned{}, err
	}
	slot, accounts, err := fleet.ReadAccounts(ctx, w.voltrChain, []string{fleet.VoltrLookupTable}, rpc.CommitmentConfirmed, p.Slot)
	if err != nil {
		return voltrSigned{}, err
	}
	table, err := fleet.VoltrLookupTableFromChain(accounts[0], slot)
	if err != nil {
		return voltrSigned{}, err
	}
	hash, lastValid, _, err := w.voltrChain.Blockhash(ctx, rpc.CommitmentConfirmed, uint64(p.Slot))
	if err != nil {
		return voltrSigned{}, err
	}
	blockhash := hash.String()
	ix, err := r.ManagerInstruction(strategy, p.Operation, uint64(p.Amount))
	if err != nil {
		return voltrSigned{}, err
	}
	tx, err := fleet.CompileVoltr(r.Guardian, blockhash, ix, table)
	if err != nil {
		return voltrSigned{}, err
	}
	simulation, err := fleet.SimulateExact(ctx, w.voltrChain, tx.UnsignedWire, rpc.CommitmentConfirmed, p.Slot)
	if err != nil || !simulation.Succeeded {
		return voltrSigned{}, fmt.Errorf("voltr unsigned simulation rejected: %v %s", err, simulation.Error)
	}
	fee, _, err := w.voltrChain.Fee(ctx, tx.Message, rpc.CommitmentConfirmed, uint64(p.Slot))
	if err != nil || int64(fee) > l.FeeCap {
		return voltrSigned{}, fmt.Errorf("voltr compiled fee %d exceeds the opportunity cap: %v", fee, err)
	}
	// The guardian is the only signer and pays the fee. The signature is
	// checked locally; Rust's second, signed simulation re-ran the same
	// message only to verify it.
	signature := ed25519.Sign(w.signer.FeePayer, tx.Message)
	wire := append(append([]byte{1}, signature...), tx.Message...)
	if tx.Message[1] != 1 || !ed25519.Verify(ed25519.PublicKey(w.signer.FeePayer[32:]), tx.Message, signature) {
		return voltrSigned{}, errors.New("voltr message is not signed by the guardian alone")
	}
	wireHash, messageHash := sha256.Sum256(wire), sha256.Sum256(tx.Message)
	requirements := *l.RequirementsFingerprint
	conflicts := []string{"voltr:vault:" + r.Vault, "kamino:reserve:" + r.Strategies[strategy].Reserve}
	sort.Strings(conflicts)
	epochs, err := json.Marshal(map[string]any{"routeBundleSha256": r.BundleSHA256, "lookupTable": fleet.VoltrLookupTable,
		"lookupTableOrderedAddressesSha256": fleet.VoltrLookupTableOrderedSHA256, "lookupTableAddressCount": fleet.VoltrLookupTableAddressCount})
	if err != nil {
		return voltrSigned{}, err
	}
	return voltrSigned{
		wire: WireIdentity{SignedTransaction: wire, SignedTransactionHash: hex.EncodeToString(wireHash[:]), MessageHash: hex.EncodeToString(messageHash[:]),
			TransactionSignature: base58.Encode(signature), RecentBlockhash: blockhash, LastValidBlockHeight: int64(lastValid)},
		payer: r.Guardian, fee: int64(fee), writable: tx.WritableAccounts, conflicts: conflicts, requirements: requirements, epochs: epochs,
		selection: stableFingerprint(requirements, "reusable", fleet.VoltrLookupTable, fleet.VoltrLookupTableOrderedSHA256, strconv.Itoa(fleet.VoltrLookupTableAddressCount)),
	}, nil
}

// stableFingerprint is voltr.rs stable_fingerprint: length-prefixed parts.
func stableFingerprint(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		var n [8]byte
		binary.LittleEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// claimVoltr is lease_rebalance_opportunity_batch for Voltr rows: restoration
// first, then allocation, then optimization, with the same epoch, active
// decision and nonterminal-submission exclusions.
func (s *Store) claimVoltr(ctx context.Context, cluster, owner string, ttl time.Duration) (*voltrLease, error) {
	var l voltrLease
	err := s.pool.QueryRow(ctx, `WITH candidate AS (
 SELECT o.id FROM loyal_yield.rebalance_opportunities o
 JOIN loyal_yield.optimizer_epochs e ON e.id=o.optimizer_epoch_id AND e.cluster=o.cluster
 JOIN loyal_yield.managed_vaults v ON v.id=o.vault_id AND v.active
 JOIN loyal_yield.route_policies p ON p.id=v.active_policy_id AND p.active
 WHERE o.cluster=$1 AND o.execution_plan->>'kind'='voltr_kamino'
  AND (o.opportunity_state IN ('revalidate','ready') OR o.opportunity_state='leased' AND o.lease_expires_at<=now())
  AND o.available_at<=now() AND o.expires_at>=clock_timestamp()+interval '65 seconds' AND e.expires_at>=clock_timestamp()+interval '65 seconds'
  AND NOT EXISTS(SELECT 1 FROM loyal_yield.rebalance_decisions d WHERE d.vault_id=o.vault_id AND d.status::text IN ('planned','simulating','ready','submitted','confirming'))
  AND NOT EXISTS(SELECT 1 FROM loyal_yield.signed_route_submissions s WHERE s.opportunity_id=o.id AND s.submission_state NOT IN ('reconciled','expired','failed'))
 ORDER BY CASE o.operation_class WHEN 'withdrawal_restoration' THEN 0 WHEN 'idle_allocation' THEN 1 ELSE 2 END,o.service_deadline_at ASC NULLS LAST,o.scheduler_priority_anchor DESC,o.economic_priority DESC,o.created_at,o.id
 FOR UPDATE OF o SKIP LOCKED LIMIT 1)
UPDATE loyal_yield.rebalance_opportunities o SET opportunity_state='leased',lease_kind='execute',lease_owner=$2,lease_expires_at=clock_timestamp()+$3::interval,fencing_token=o.fencing_token+1,attempt_count=o.attempt_count+1,updated_at=now()
FROM candidate WHERE o.id=candidate.id
RETURNING o.id,o.vault_id,o.fencing_token,o.optimizer_epoch_id,o.amount_raw,o.estimated_cost_lamports,o.cluster,o.target_reserve,o.source_reserve,o.route_fingerprint,o.requirements_fingerprint,o.lease_expires_at,o.execution_plan`,
		cluster, owner, formatInterval(ttl)).Scan(&l.OpportunityID, &l.VaultID, &l.FencingToken, &l.EpochID, &l.AmountRaw, &l.FeeCap, &l.Cluster, &l.TargetReserve, &l.SourceReserve, &l.RouteFingerprint, &l.RequirementsFingerprint, &l.ExpiresAt, &l.Plan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// persistVoltr is record_voltr_manager_decision_with_signed_submission plus
// the conflict lease acquisition before it, in one transaction.
func (s *Store) persistVoltr(ctx context.Context, owner string, l voltrLease, v voltrSigned) error {
	semantic := fmt.Sprintf("fleet-opportunity:%d", l.OpportunityID)
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var cluster string
		err := tx.QueryRow(ctx, `SELECT o.cluster FROM loyal_yield.rebalance_opportunities o JOIN loyal_yield.optimizer_epochs e ON e.id=o.optimizer_epoch_id AND e.cluster=o.cluster
  WHERE o.id=$1 AND o.opportunity_state='leased' AND o.lease_kind='execute' AND o.lease_owner=$3 AND o.fencing_token=$2 AND o.lease_expires_at>clock_timestamp()
   AND o.expires_at>=clock_timestamp()+interval '60 seconds' AND e.expires_at>=clock_timestamp()+interval '60 seconds' FOR UPDATE OF o FOR SHARE OF e`, l.OpportunityID, l.FencingToken, owner).Scan(&cluster)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleOwner
		}
		if err != nil {
			return err
		}
		var active, busy, bound bool
		if err := tx.QueryRow(ctx, `SELECT v.active,EXISTS(SELECT 1 FROM loyal_yield.rebalance_decisions d WHERE d.vault_id=v.id AND d.status::text IN ('planned','simulating','ready','submitted','confirming')) FROM loyal_yield.managed_vaults v WHERE v.id=$1 FOR UPDATE OF v`, l.VaultID).Scan(&active, &busy); err != nil || !active || busy {
			return fmt.Errorf("voltr vault %d is inactive or acquired an active decision: %v", l.VaultID, err)
		}
		if err := tx.QueryRow(ctx, crossMintPolicyPayerSQL, l.OpportunityID, cluster, v.payer, v.epochs).Scan(&bound); err != nil || !bound {
			return fmt.Errorf("voltr fee payer is not the vault's delegated guardian: %v", err)
		}
		var restoring bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.orchestration_outbox e WHERE e.cluster=$1 AND e.event_kind='backyard_voltr_manager_withdraw' AND e.aggregate_kind='voltr_withdrawal_restoration' AND e.processed_at IS NULL
  AND (e.payload->>'vaultId'=$2 OR e.payload->'managerRequest'->>'reserve'=ANY($3::text[])))`, cluster, strconv.FormatInt(l.VaultID, 10), v.conflicts).Scan(&restoring); err != nil || restoring {
			return fmt.Errorf("voltr execution is fenced by an active restoration outbox event: %v", err)
		}
		for _, key := range v.conflicts {
			tag, err := tx.Exec(ctx, `INSERT INTO loyal_yield.route_account_conflict_leases AS c(cluster,writable_account_key,opportunity_id,lease_owner,fencing_token,expires_at) VALUES($1,$2,$3,$4,$5,$6)
  ON CONFLICT(cluster,writable_account_key) DO UPDATE SET opportunity_id=EXCLUDED.opportunity_id,lease_owner=EXCLUDED.lease_owner,fencing_token=EXCLUDED.fencing_token,expires_at=EXCLUDED.expires_at,submission_id=NULL,updated_at=now()
  WHERE c.submission_id IS NULL AND (c.expires_at<=now() OR c.opportunity_id=EXCLUDED.opportunity_id AND c.lease_owner=EXCLUDED.lease_owner AND c.fencing_token=EXCLUDED.fencing_token)`, cluster, key, l.OpportunityID, owner, l.FencingToken, l.ExpiresAt)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return ErrConflictLeaseHeld
			}
		}
		var submissionID int64
		if err := tx.QueryRow(ctx, `INSERT INTO loyal_yield.signed_route_submissions
 (cluster,semantic_key,opportunity_id,signed_transaction,signed_transaction_hash,message_hash,transaction_signature,recent_blockhash,last_valid_block_height,
  optimizer_epoch_id,alt_requirements_fingerprint,alt_selection_fingerprint,alt_mutation_epochs,fee_payer,fee_payer_kind,compiled_fee_lamports,writable_account_keys,conflict_account_keys,executor_owner,executor_fencing_token)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'policy',$15,$16,$17,$18,$19) RETURNING id`,
			cluster, semantic, l.OpportunityID, v.wire.SignedTransaction, v.wire.SignedTransactionHash, v.wire.MessageHash, v.wire.TransactionSignature, v.wire.RecentBlockhash, v.wire.LastValidBlockHeight,
			l.EpochID, v.requirements, v.selection, v.epochs, v.payer, v.fee, v.writable, v.conflicts, owner, l.FencingToken).Scan(&submissionID); err != nil {
			return err
		}
		if tag, err := tx.Exec(ctx, `UPDATE loyal_yield.route_account_conflict_leases SET submission_id=$3,expires_at=GREATEST(expires_at,now()+interval '10 minutes'),updated_at=now() WHERE cluster=$1 AND opportunity_id=$2 AND writable_account_key=ANY($4) AND expires_at>now()`, cluster, l.OpportunityID, submissionID, v.conflicts); err != nil || tag.RowsAffected() != int64(len(v.conflicts)) {
			return ErrConflictLeaseHeld
		}
		// The registered trigger links this decision to the leased opportunity
		// and its one signed submission.
		var decisionID int64
		if err := tx.QueryRow(ctx, `INSERT INTO loyal_yield.rebalance_decisions
 (vault_id,source_snapshot_id,status,source_reserve,target_reserve,liquidity_mint,source_liquidity_mint,target_liquidity_mint,amount_raw,source_apy_bps,target_apy_bps,estimated_edge_bps,estimated_cost_lamports,decision_reason,execution_plan,idempotency_key)
 SELECT vault_id,source_snapshot_id,'planned',source_reserve,target_reserve,liquidity_mint,source_liquidity_mint,target_liquidity_mint,amount_raw,source_apy_bps,target_apy_bps,estimated_edge_bps,estimated_cost_lamports,'voltr_manager_operation',execution_plan,$2
 FROM loyal_yield.rebalance_opportunities WHERE id=$1 RETURNING id`, l.OpportunityID, semantic).Scan(&decisionID); err != nil {
			return err
		}
		var ready bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.rebalance_opportunities o ON o.id=s.opportunity_id JOIN loyal_yield.optimizer_epochs e ON e.id=o.optimizer_epoch_id AND e.cluster=o.cluster
  WHERE s.id=$1 AND s.decision_id=$2 AND o.decision_id=$2 AND o.opportunity_state='decision_created' AND s.submission_state='signed'
   AND o.expires_at>=clock_timestamp()+interval '60 seconds' AND e.expires_at>=clock_timestamp()+interval '60 seconds')`, submissionID, decisionID).Scan(&ready); err != nil || !ready {
			return ErrStaleOwner
		}
		return nil
	})
}

// reconcileVoltr is reconcile_if_voltr: a confirmed readback at or after the
// confirmed slot must show the exact manager leg's idle/strategy effect.
func (w *Worker) reconcileVoltr(ctx context.Context, lease SubmissionLease) error {
	record := lease.Submission
	if w.voltr == nil || record.ConfirmedSlot == nil || record.DecisionID == nil {
		return errors.New("voltr reconciliation lacks its route, confirmed slot or decision")
	}
	var raw json.RawMessage
	var decisionID *int64
	if err := w.store.pool.QueryRow(ctx, `SELECT execution_plan,decision_id FROM loyal_yield.rebalance_opportunities WHERE id=$1`, record.OpportunityID).Scan(&raw, &decisionID); err != nil {
		return err
	}
	p, err := decodeVoltrPlan(raw)
	if err != nil || decisionID == nil || *decisionID != *record.DecisionID {
		return errors.New("voltr opportunity, submission and decision diverged")
	}
	strategy, ok := w.voltr.StrategyIndex(p.Strategy)
	if !ok || p.Amount <= 0 {
		return errors.New("voltr reconciliation plan is invalid")
	}
	o, err := fleet.ObserveVoltr(ctx, w.voltrChain, *w.voltr, *record.ConfirmedSlot)
	if err != nil {
		return err
	}
	if !voltrEffectMatches(p, o, strategy) {
		return errors.New("voltr confirmed idle/strategy effect does not match the exact manager leg")
	}
	return db.WithTx(ctx, w.store.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE loyal_yield.rebalance_decisions SET status='confirmed',confirmed_slot=COALESCE(confirmed_slot,$2),updated_at=now() WHERE id=$1`, *record.DecisionID, *record.ConfirmedSlot); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions SET submission_state='reconciled',reconciled_slot=$4,reconciled_at=now(),confirmation_lease_owner=NULL,confirmation_lease_expires_at=NULL,error_detail=NULL,updated_at=now()
  WHERE id=$1 AND submission_state='reconciliation_pending' AND confirmed_slot IS NOT NULL AND $4>=confirmed_slot AND confirmation_lease_owner=$2 AND confirmation_fencing_token=$3 AND confirmation_lease_expires_at>now()`, record.ID, lease.Owner, lease.FencingToken, o.ContextSlot)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return nil
	})
}

// voltrEffectMatches is reconcile_if_voltr's exact-leg check: idle moved by
// exactly the amount, the strategy moved the right way, and total value did
// not lose more than the leg.
func voltrEffectMatches(p voltrPlan, o fleet.VoltrObservation, strategy int) bool {
	amount, post := uint64(p.Amount), o.PositionsRaw[strategy]
	switch p.Operation {
	case "deposit":
		return p.PreIdle >= amount && p.PreIdle-amount == o.IdleRaw && post >= p.PrePosition+amount && o.TotalValueRaw >= p.PreTotal
	case "withdraw":
		floor := uint64(0)
		if p.PreTotal > amount {
			floor = p.PreTotal - amount
		}
		return p.PreIdle+amount == o.IdleRaw && post < p.PrePosition && o.TotalValueRaw >= floor
	}
	return false
}

// isVoltrSubmission: only Voltr routes carry the voltr:vault conflict.
func isVoltrSubmission(r SubmissionRecord) bool {
	for _, key := range r.ConflictAccountKeys {
		if strings.HasPrefix(key, "voltr:vault:") {
			return true
		}
	}
	return false
}

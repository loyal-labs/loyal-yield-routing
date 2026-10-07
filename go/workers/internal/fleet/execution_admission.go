package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
)

// ExecutionAdmission is the result of fresh read-only preparation followed by
// locked capacity/conflict admission. It contains no signing capability. The
// executor must recheck these identities while publishing its immutable wire;
// receiving this value alone is never permission to broadcast.
type ExecutionAdmission struct {
	Lease                                                              RevalidationLease
	Preparation                                                        RoutePreparation
	LastValidBlockHeight                                               int64
	CapacityReservationID, ReservationGeneration, CapacityFencingToken int64
	SelectedALTs                                                       []ExecutionALT
	AltSelectionFingerprint                                            string
	ConflictKeys                                                       []string
	Evidence                                                           FreshRouteEvidence
	Anchors                                                            ExecutionBalanceAnchors
	// FeePayer is the policy signer or the vault's fee-only payer, with the
	// balance a fee-only payer's spend reservation records.
	FeePayer                             string
	FeePayerBalance, FeePayerBalanceSlot int64
}

type ExecutionALT struct {
	TableID, MutationEpoch, FamilyID, Generation int64
	BindingID                                    *int64
	Address                                      string
	Addresses                                    []string
}

// Amounts are protocol raw units, not USD or interchangeable collateral mints.
// The executor checks SQL BIGINT boundaries before persisting them.
type ExecutionBalanceAnchors struct {
	SourceObligation, TargetObligation, VaultLiquidityATA             string
	SourceReserve, TargetReserve, SourceMarket, TargetMarket          string
	SourceCollateralMint, TargetCollateralMint, LiquidityTokenProgram string
	Owner, Mint                                                       string
	SourceCollateralRaw, TargetCollateralRaw, IdleLiquidityRaw        uint64
	MinimumSlot                                                       int64
}

type executionAdmissionStore interface {
	CommitExecutionAdmission(context.Context, RevalidationLease, RevalidationCommit, *ExecutionAdmission) error
	RecoverUnsignedExecutionAdmissions(context.Context, string, int) (int64, error)
}

// PrepareExecution shares the actual policy, official builder, fee, simulation
// and ALT preparation with revalidation. A waiting ALT request is durable work,
// but yields no signable admission. Cross-mint legs use their separate custody
// protocol and cannot be passed through this same-mint API.
func (r *Revalidator) PrepareExecution(ctx context.Context, cluster string) (*ExecutionAdmission, bool, error) {
	store, ok := r.store.(executionAdmissionStore)
	if !ok || !r.fusedExecute {
		return nil, false, errors.New("fresh execution requires fused admission store")
	}
	if _, err := store.RecoverUnsignedExecutionAdmissions(ctx, cluster, 32); err != nil {
		return nil, false, err
	}
	lease, err := r.store.ClaimRevalidation(ctx, cluster, r.owner, r.leaseTTL, true, false, r.signer)
	if err != nil || lease == nil {
		return nil, false, err
	}
	ctx, cancel := context.WithDeadline(ctx, lease.ExpiresAt.Add(-5*time.Second))
	defer cancel()
	if lease.RouteKind != "same_mint" || !contains(lease.DelegatedSigners, r.signer) {
		return nil, true, errors.New("same-mint execution policy identity changed")
	}
	prepared, _, err := r.prepareSameMint(ctx, cluster, *lease, func(e FreshRouteEvidence) error {
		if err := r.store.RefreshTargetCapacity(ctx, cluster, lease.TargetReserve, lease.LiquidityMint, e.TargetObservedSupplyUSDMicros, e.Slot); err != nil {
			return err
		}
		return r.store.CheckRevalidationLease(ctx, *lease)
	})
	if err != nil {
		return nil, true, err
	}
	commit := RevalidationCommit{Disposition: "fused_execute", Preparation: &prepared.Preparation,
		ExpectedEpochFingerprint: lease.OptimizerEpochKey, ExpectedOpportunityKey: lease.IdempotencyKey,
		FreshEconomics: true, ObservedSourceAPYBPS: prepared.Evidence.ObservedSourceAPYBPS,
		ObservedTargetAPYBPS:          prepared.Evidence.ObservedTargetAPYBPS,
		TargetObservedSupplyUSDMicros: prepared.Evidence.TargetObservedSupplyUSDMicros, TargetObservedSlot: prepared.Evidence.Slot}
	if prepared.WaitingALT {
		commit.Disposition, commit.MissingAddresses = "waiting_alt", prepared.Missing
		return nil, true, r.store.CommitRevalidation(ctx, *lease, commit)
	}
	// These are the retained Rust semantic lanes, distinct from the physical
	// writable vector (which includes the shared fee payer and reserve accounts).
	commit.ConflictKeys = canonicalStrings([]string{"vault-write:" + lease.VaultPubkey, fmt.Sprintf("fleet-shared-write-lane:%02d", lease.VaultID%64)})
	admission := &ExecutionAdmission{Lease: *lease, Preparation: prepared.Preparation,
		LastValidBlockHeight: prepared.LastValidBlockHeight, ConflictKeys: append([]string(nil), commit.ConflictKeys...),
		Evidence: prepared.Evidence, Anchors: prepared.Evidence.Anchors,
		FeePayer: prepared.FeePayer, FeePayerBalance: prepared.FeePayerBalance, FeePayerBalanceSlot: prepared.FeePayerBalanceSlot}
	for _, address := range prepared.Preparation.Transaction.LookupTables {
		found := false
		for _, table := range prepared.Tables {
			if table.Address == address {
				admission.SelectedALTs = append(admission.SelectedALTs, ExecutionALT{TableID: table.ID, MutationEpoch: table.MutationEpoch, FamilyID: table.FamilyID, Generation: table.Generation, BindingID: table.BindingID, Address: table.Address, Addresses: append([]string(nil), table.Addresses...)})
				found = true
				break
			}
		}
		if !found {
			return nil, true, errors.New("compiled ALT has no verified database identity")
		}
	}
	if err := store.CommitExecutionAdmission(ctx, *lease, commit, admission); err != nil {
		return nil, true, err
	}
	return admission, true, nil
}

func (s *Store) CommitExecutionAdmission(ctx context.Context, lease RevalidationLease, commit RevalidationCommit, admission *ExecutionAdmission) error {
	if admission == nil || commit.Disposition != "fused_execute" || !commit.FreshEconomics ||
		admission.Lease.OpportunityID != lease.OpportunityID || admission.LastValidBlockHeight <= 0 ||
		admission.Anchors.MinimumSlot != admission.Evidence.Slot || admission.Evidence.Slot <= 0 ||
		admission.Evidence.OpportunityKey != lease.IdempotencyKey || admission.Evidence.EpochFingerprint != lease.OptimizerEpochKey ||
		admission.Evidence.ObservedAt.After(time.Now()) || time.Since(admission.Evidence.ObservedAt) > 15*time.Second ||
		len(admission.SelectedALTs) == 0 || len(admission.SelectedALTs) != len(admission.Preparation.Transaction.LookupTables) {
		return errors.New("fresh execution admission evidence is incomplete or stale")
	}
	a := admission.Anchors
	if a.SourceReserve != lease.SourceReserve || a.TargetReserve != lease.TargetReserve || a.Owner != lease.VaultPubkey || a.Mint != lease.LiquidityMint ||
		a.SourceObligation == "" || a.TargetObligation == "" || a.VaultLiquidityATA == "" || a.SourceCollateralRaw == 0 ||
		a.SourceCollateralRaw > math.MaxInt64 || a.TargetCollateralRaw > math.MaxInt64 || a.IdleLiquidityRaw > math.MaxInt64 {
		return errors.New("fresh execution balance anchors differ from route custody")
	}
	if commit.Preparation == nil || !reflect.DeepEqual(*commit.Preparation, admission.Preparation) || !reflect.DeepEqual(canonicalStrings(commit.ConflictKeys), admission.ConflictKeys) {
		return errors.New("execution admission differs from prepared transaction")
	}
	messageHash := sha256.Sum256(admission.Preparation.Transaction.Message)
	wireHash := sha256.Sum256(admission.Preparation.Transaction.UnsignedWire)
	if hex.EncodeToString(messageHash[:]) != admission.Preparation.Transaction.MessageSHA256 || hex.EncodeToString(wireHash[:]) != admission.Preparation.Transaction.WireSHA256 {
		return errors.New("execution preparation bytes changed after simulation")
	}
	deadline := admission.Evidence.ObservedAt.Add(15 * time.Second)
	if lease.ExpiresAt.Add(-5 * time.Second).Before(deadline) {
		deadline = lease.ExpiresAt.Add(-5 * time.Second)
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	return s.commitRevalidation(ctx, lease, commit, admission)
}

// lockExecutionALTs checks the same normalized membership and mutation epochs
// used by compilation, under the actual legacy parent-row locks. The executor
// later adopts prepared_transaction usage leases in its signed publication.
func lockExecutionALTs(ctx context.Context, tx pgx.Tx, lease RevalidationLease, admission *ExecutionAdmission) error {
	seen := map[int64]bool{}
	for i, expected := range admission.SelectedALTs {
		if expected.TableID <= 0 || expected.MutationEpoch < 0 || seen[expected.TableID] || expected.Address != admission.Preparation.Transaction.LookupTables[i] {
			return errors.New("invalid or duplicate selected ALT identity")
		}
		seen[expected.TableID] = true
		var address, kind string
		var familyID, generation, epoch int64
		var active bool
		err := tx.QueryRow(ctx, `SELECT t.table_address,t.family_id,t.generation,t.mutation_epoch,f.kind,
 t.cluster=$2 AND f.cluster=$2 AND t.durable AND t.status IN ('active','usable') AND t.desired_state='active'
 AND f.desired_state='active' AND t.generation=f.active_generation AND t.deactivated_slot IS NULL
 AND NOT EXISTS(SELECT 1 FROM loyal_yield.lookup_table_operations mutation WHERE mutation.route_lookup_table_id=t.id
 AND mutation.operation_kind IN ('create','extend','rollover','deactivate','close')
 AND (mutation.operation_state IN ('signed','submitted','confirmed','finalized','reconciled','needs_reconcile')
 OR (mutation.operation_state IN ('leased','retry_wait') AND mutation.transaction_signature IS NOT NULL)))
 FROM loyal_yield.route_lookup_tables t JOIN loyal_yield.lookup_table_families f ON f.id=t.family_id
 WHERE t.id=$1 FOR SHARE OF t,f`, expected.TableID, lease.Cluster).Scan(&address, &familyID, &generation, &epoch, &kind, &active)
		if err != nil {
			return fmt.Errorf("lock selected ALT: %w", err)
		}
		if !active || address != expected.Address || familyID != expected.FamilyID || generation != expected.Generation || epoch != expected.MutationEpoch {
			return errors.New("selected ALT generation or mutation fence changed")
		}
		if kind == "vault_shards" {
			if expected.BindingID == nil {
				return errors.New("vault ALT lacks exact binding")
			}
			var bindingActive bool
			if err := tx.QueryRow(ctx, `SELECT route_lookup_table_id=$2 AND vault_id=$3 AND lifecycle_state='active'
 FROM loyal_yield.lookup_table_vault_bindings WHERE id=$1 FOR SHARE`, *expected.BindingID, expected.TableID, lease.VaultID).Scan(&bindingActive); err != nil || !bindingActive {
				return errors.New("selected ALT vault binding changed")
			}
		} else if kind != "shared_market" || expected.BindingID != nil {
			return errors.New("selected ALT family differs from route scope")
		}
		var addresses []string
		var usable bool
		if err := tx.QueryRow(ctx, `SELECT array_agg(address ORDER BY ordinal),COALESCE(bool_and(usable_after_slot<=$2 AND last_verified_slot IS NOT NULL),false)
 FROM loyal_yield.lookup_table_addresses WHERE route_lookup_table_id=$1`, expected.TableID, admission.Evidence.Slot).Scan(&addresses, &usable); err != nil {
			return err
		}
		if !usable || !reflect.DeepEqual(addresses, expected.Addresses) {
			return errors.New("selected ALT usable member vector changed")
		}
	}
	// This fingerprint is deterministic Go selection evidence. Legacy adoption
	// preserves stored Rust fingerprints rather than recomputing this value.
	raw, err := json.Marshal(admission.SelectedALTs)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(bytes.Clone(raw))
	admission.AltSelectionFingerprint = hex.EncodeToString(hash[:])
	return nil
}

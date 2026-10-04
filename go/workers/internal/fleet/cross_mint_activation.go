package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// CrossMintActivationPreparation describes the atomic verifier and a separate
// independently simulated first withdrawal. No signed wire or ready decision
// is produced here. The custody owner locks capacity and the source lease.
type CrossMintActivationPreparation struct {
	Lease                           RevalidationLease
	Capacity                        CrossMintActivationCapacity
	Certificate                     CrossMintPreflightCertificate
	PreflightPreparation            RoutePreparation
	InitialWithdrawalPreparation    CrossMintLegPreparation
	ObservedAt                      time.Time
	ObservedSlot                    int64
	SourceAPYBPS, TargetAPYBPS      int64
	TargetObservedSupplyUSDMicros   int64
	ControlGeneration               int64
	WaitingALT                      bool
	MissingAddresses                []string
	SharedAddresses, VaultAddresses []ALTManifestAddress
}

// Capacity is the concrete frontier refreshed from this preparation's actual
// finalized reserve bank. The executor still locks and compares the row.
type CrossMintActivationCapacity struct {
	Cluster, TargetReserve, LiquidityMint                                             string
	ObservedSupplyUSDMicros, ObservedSlot, MaximumInflightUSDMicros, TelemetryVersion int64
}

type crossMintActivationStore interface {
	CheckCrossMintActivationLease(context.Context, RevalidationLease) (int64, error)
}

type crossMintPreflightStore interface {
	CheckCrossMintPreflightLease(context.Context, RevalidationLease) (int64, error)
}

type crossMintActivationSourceStore interface {
	crossMintActivationStore
	ClaimCrossMintPreflight(context.Context, string, string, time.Duration, string) (*RevalidationLease, error)
	ClaimCrossMintActivation(context.Context, string, string, time.Duration, string) (*RevalidationLease, error)
	QueueCrossMintActivationALT(context.Context, CrossMintActivationPreparation) error
}

// PrepareNextCrossMintActivation first prepares exact source fingerprints under
// a revalidation lease, then freshly certifies under a separate execute lease.
// Both phases share the same actual preflight calculation. It never claims a
// same-mint row, changes the fused configuration, signs or creates a decision.
func (r *Revalidator) PrepareNextCrossMintActivation(ctx context.Context, cluster string) (*CrossMintActivationPreparation, bool, error) {
	if r == nil || !r.crossMintEnabled || r.jupiter == nil || cluster == "" {
		return nil, false, errors.New("cross-mint source preparation is unavailable")
	}
	store, ok := r.store.(crossMintActivationSourceStore)
	if !ok {
		return nil, false, errors.New("cross-mint source requires dedicated durable claim and ALT handoff")
	}
	l, err := store.ClaimCrossMintActivation(ctx, cluster, r.owner, r.leaseTTL, r.signer)
	if err != nil {
		return nil, false, err
	}
	worked := l != nil
	if l == nil {
		preflight, err := store.ClaimCrossMintPreflight(ctx, cluster, r.owner, r.leaseTTL, r.signer)
		if err != nil || preflight == nil {
			return nil, false, err
		}
		worked = true
		if preflight.RouteKind != "cross_mint_jupiter" || preflight.Owner != r.owner || !contains(preflight.DelegatedSigners, r.signer) {
			return nil, true, errors.New("cross-mint preflight claim changed family or authority")
		}
		if err := r.cycleCrossMint(ctx, *preflight); err != nil {
			return nil, true, err
		}
		l, err = store.ClaimCrossMintActivation(ctx, cluster, r.owner, r.leaseTTL, r.signer)
		if err != nil || l == nil {
			return nil, true, err
		}
	}
	out, err := r.PrepareCrossMintActivation(ctx, *l)
	if err != nil {
		return nil, worked, err
	}
	if out.WaitingALT {
		return nil, true, store.QueueCrossMintActivationALT(ctx, out)
	}
	return &out, true, nil
}

func (r *Revalidator) PrepareCrossMintActivation(ctx context.Context, lease RevalidationLease) (CrossMintActivationPreparation, error) {
	var out CrossMintActivationPreparation
	if r == nil || !r.crossMintEnabled || r.jupiter == nil || lease.Owner != r.owner || lease.RouteKind != "cross_mint_jupiter" || lease.OpportunityID <= 0 || lease.FencingToken <= 0 || lease.FeeCapLamports <= 0 || lease.LiquidityAmountRaw == 0 || lease.LiquidityAmountRaw > math.MaxInt64 || !lease.ExpiresAt.After(time.Now().Add(5*time.Second)) || !contains(lease.DelegatedSigners, r.signer) {
		return out, errors.New("cross-mint activation requires actual delegated revalidation authority")
	}
	store, ok := r.store.(crossMintActivationStore)
	if !ok {
		return out, errors.New("cross-mint activation requires current source-plan/control checks")
	}
	ctx, cancel := context.WithDeadline(ctx, lease.ExpiresAt.Add(-5*time.Second))
	defer cancel()
	generation, err := store.CheckCrossMintActivationLease(ctx, lease)
	if err != nil {
		return out, err
	}
	out, err = r.prepareCrossMintPreflight(ctx, lease, true)
	if err != nil {
		return CrossMintActivationPreparation{}, err
	}
	if !out.WaitingALT {
		if err = validateCrossMintActivationObservation(out, lease, time.Now()); err != nil {
			return CrossMintActivationPreparation{}, err
		}
		// This private path is reached only from the complete source producer;
		// callers cannot supply an arbitrary DTO to publish telemetry.
		concrete, ok := r.store.(*Store)
		if !ok || concrete.pool == nil {
			return CrossMintActivationPreparation{}, errors.New("cross-mint activation requires concrete capacity store")
		}
		if err = concrete.RefreshTargetCapacity(ctx, lease.Cluster, lease.TargetReserve, lease.TargetLiquidityMint, out.TargetObservedSupplyUSDMicros, out.ObservedSlot); err != nil {
			return CrossMintActivationPreparation{}, err
		}
		out.Capacity = CrossMintActivationCapacity{Cluster: lease.Cluster, TargetReserve: lease.TargetReserve, LiquidityMint: lease.TargetLiquidityMint}
		p := &out.Capacity
		if err = concrete.pool.QueryRow(ctx, `SELECT observed_supply_usd_micros,observed_slot,maximum_inflight_usd_micros,telemetry_version FROM loyal_yield.target_capacity_frontiers WHERE cluster=$1 AND target_reserve=$2 AND liquidity_mint=$3`, p.Cluster, p.TargetReserve, p.LiquidityMint).Scan(&p.ObservedSupplyUSDMicros, &p.ObservedSlot, &p.MaximumInflightUSDMicros, &p.TelemetryVersion); err != nil {
			return CrossMintActivationPreparation{}, err
		}
		if p.ObservedSupplyUSDMicros != out.TargetObservedSupplyUSDMicros || p.ObservedSlot != out.ObservedSlot || p.MaximumInflightUSDMicros <= 0 || p.TelemetryVersion < 0 {
			return CrossMintActivationPreparation{}, errors.New("capacity telemetry changed after finalized observation")
		}
	}
	current, err := store.CheckCrossMintActivationLease(ctx, lease)
	if err != nil || current != generation || ctx.Err() != nil {
		if err == nil {
			err = errors.New("cross-mint control generation or actual lease deadline changed during preflight")
		}
		return CrossMintActivationPreparation{}, err
	}
	if !out.WaitingALT && time.Since(out.ObservedAt) > 15*time.Second {
		return CrossMintActivationPreparation{}, errors.New("cross-mint activation account evidence expired during preflight")
	}
	out.Lease, out.ControlGeneration = lease, generation
	return out, nil
}

func validateCrossMintActivationObservation(out CrossMintActivationPreparation, lease RevalidationLease, now time.Time) error {
	var economics struct {
		SourceAPYBPS *int64 `json:"source_apy_bps"`
		TargetAPYBPS *int64 `json:"observed_target_apy_bps"`
	}
	if out.WaitingALT || out.ObservedAt.IsZero() || out.ObservedAt.After(now) || now.Sub(out.ObservedAt) > 15*time.Second || out.ObservedSlot <= 0 || out.TargetObservedSupplyUSDMicros < 0 || json.Unmarshal(lease.ExecutionPlan, &economics) != nil || economics.SourceAPYBPS == nil || economics.TargetAPYBPS == nil || out.SourceAPYBPS != *economics.SourceAPYBPS || out.TargetAPYBPS != *economics.TargetAPYBPS {
		return errors.New("fresh activation reserve economics differ from immutable epoch or evidence expired")
	}
	return nil
}

func (s *Store) CheckCrossMintActivationLease(ctx context.Context, l RevalidationLease) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, errors.New("cross-mint activation requires source store")
	}
	return checkCrossMintSourceLease(ctx, s.pool, l, "execute", false)
}

func (s *Store) CheckCrossMintPreflightLease(ctx context.Context, l RevalidationLease) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, errors.New("cross-mint preflight requires source store")
	}
	return checkCrossMintSourceLease(ctx, s.pool, l, "revalidate", false)
}

type crossMintActivationQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func checkCrossMintSourceLease(ctx context.Context, query crossMintActivationQuery, l RevalidationLease, leaseKind string, lock bool) (int64, error) {
	if l.OpportunityID <= 0 || l.Owner == "" || l.Cluster == "" || l.FencingToken <= 0 || l.LiquidityAmountRaw == 0 || l.LiquidityAmountRaw > math.MaxInt64 || !json.Valid(l.ExecutionPlan) {
		return 0, errors.New("cross-mint activation lease identity is incomplete")
	}
	var generation int64
	var current bool
	sql := `SELECT COALESCE(c.generation,0),
 o.opportunity_state='leased' AND o.lease_kind=$19 AND o.lease_owner=$2 AND o.fencing_token=$3
 AND o.lease_expires_at=$4 AND o.lease_expires_at>clock_timestamp()+interval '5 seconds'
 AND o.expires_at>clock_timestamp()+interval '60 seconds' AND e.expires_at>clock_timestamp()+interval '60 seconds'
 AND o.optimizer_epoch_id=$5 AND e.epoch_key=$6 AND o.idempotency_key=$7 AND o.cluster=$8
 AND o.vault_id=$9 AND v.vault_pubkey=$10 AND v.vault_index=$11
 AND o.source_reserve=$12 AND o.target_reserve=$13 AND o.source_liquidity_mint=$14 AND o.target_liquidity_mint=$15
 AND o.amount_raw=$16 AND o.execution_plan=$17::jsonb AND o.source_snapshot_id IS NOT DISTINCT FROM $18::bigint
 AND o.decision_id IS NULL AND o.execution_plan->>'route_kind'='cross_mint_jupiter'
 AND o.execution_plan->>'source_kind'='reserve_position' AND COALESCE(c.start_new_movements,false)
 AND COALESCE(c.continue_or_recover_existing,true) AND v.active
 FROM loyal_yield.rebalance_opportunities o JOIN loyal_yield.optimizer_epochs e ON e.id=o.optimizer_epoch_id AND e.cluster=o.cluster
 JOIN loyal_yield.managed_vaults v ON v.id=o.vault_id
 LEFT JOIN loyal_yield.cross_mint_movement_controls c ON c.cluster=o.cluster WHERE o.id=$1`
	if lock {
		sql += ` FOR UPDATE OF o FOR SHARE OF e,v`
	}
	err := query.QueryRow(ctx, sql, l.OpportunityID, l.Owner, l.FencingToken, l.ExpiresAt, l.OptimizerEpochID, l.OptimizerEpochKey, l.IdempotencyKey, l.Cluster, l.VaultID, l.VaultPubkey, int16(l.VaultIndex), l.SourceReserve, l.TargetReserve, l.SourceLiquidityMint, l.TargetLiquidityMint, int64(l.LiquidityAmountRaw), string(l.ExecutionPlan), l.SourceSnapshotID, leaseKind).Scan(&generation, &current)
	if err != nil {
		return 0, err
	}
	if !current {
		return 0, errors.New("cross-mint source plan, control generation or " + leaseKind + " lease changed")
	}
	return generation, nil
}

// QueueCrossMintActivationALT publishes only typed, unsigned preparation demand.
// It holds the source execute fence and the retained control advisory lock while
// the shared provisioning implementation seals the full address manifest.
func (s *Store) QueueCrossMintActivationALT(ctx context.Context, p CrossMintActivationPreparation) error {
	return s.queueCrossMintSourceALT(ctx, p, "execute")
}

func (s *Store) QueueCrossMintPreflightALT(ctx context.Context, p CrossMintActivationPreparation) error {
	return s.queueCrossMintSourceALT(ctx, p, "revalidate")
}

func (s *Store) queueCrossMintSourceALT(ctx context.Context, p CrossMintActivationPreparation, leaseKind string) error {
	l, prep := p.Lease, p.PreflightPreparation
	if s == nil || s.pool == nil || !p.WaitingALT || len(p.MissingAddresses) == 0 || prep.Manifest == nil || p.Certificate.Kind != "" || len(prep.Transaction.Message) != 0 || len(prep.Transaction.UnsignedWire) != 0 || len(prep.RouteFingerprint) != 64 || prep.RequirementsFingerprint != prep.Manifest.Fingerprint || !bytes.Equal(prep.ExecutionPlan, l.ExecutionPlan) || !l.ExpiresAt.After(time.Now().Add(5*time.Second)) {
		return errors.New("cross-mint ALT handoff requires unsigned full manifest and actual source lease")
	}
	ctx, cancel := context.WithDeadline(ctx, l.ExpiresAt.Add(-5*time.Second))
	defer cancel()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "loyal-yield-cross-mint-control:"+l.Cluster); err != nil {
		return err
	}
	generation, err := checkCrossMintSourceLease(ctx, tx, l, leaseKind, true)
	if err != nil || generation != p.ControlGeneration {
		if err == nil {
			err = errors.New("cross-mint activation control generation changed before ALT handoff")
		}
		return err
	}
	requestID, err := upsertWaitingALTRequest(ctx, tx, l, prep, p.MissingAddresses)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.lookup_table_provisioning_request_consumers(opportunity_id,provisioning_request_id) VALUES($1,$2) ON CONFLICT(opportunity_id) DO UPDATE SET provisioning_request_id=EXCLUDED.provisioning_request_id`, l.OpportunityID, requestID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state='waiting_alt',lease_kind=NULL,lease_owner=NULL,lease_expires_at=NULL,route_fingerprint=$5,requirements_fingerprint=$6,updated_at=clock_timestamp() WHERE id=$1 AND opportunity_state='leased' AND lease_kind=$7 AND lease_owner=$2 AND fencing_token=$3 AND lease_expires_at=$4 AND lease_expires_at>clock_timestamp()+interval '5 seconds' AND decision_id IS NULL`, l.OpportunityID, l.Owner, l.FencingToken, l.ExpiresAt, prep.RouteFingerprint, prep.RequirementsFingerprint, leaseKind)
	if err != nil || tag.RowsAffected() != 1 {
		return errors.New("cross-mint activation ALT transition was fenced")
	}
	return tx.Commit(ctx)
}

func crossMintActivationRequest(l RevalidationLease) CrossMintPreparationRequest {
	return CrossMintPreparationRequest{Movement: CrossMintPreparationMovement{OpportunityID: l.OpportunityID, OptimizerEpochID: l.OptimizerEpochID, VaultID: l.VaultID, Cluster: l.Cluster, VaultPubkey: l.VaultPubkey, SourceSnapshotID: l.SourceSnapshotID, SourceReserve: l.SourceReserve, IntendedTargetReserve: l.TargetReserve, ActiveTargetReserve: l.TargetReserve, SourceMint: l.SourceLiquidityMint, TargetMint: l.TargetLiquidityMint, PlannedAmountRaw: int64(l.LiquidityAmountRaw), ExecutionPlan: bytes.Clone(l.ExecutionPlan), CustodyMint: l.SourceLiquidityMint, CustodyAmountRaw: int64(l.LiquidityAmountRaw), Phase: "source_reserve"}, Leg: "withdraw", Purpose: "optimize_yield", Generation: 1, RemainingFeeLamports: l.FeeCapLamports, ContinuationOwner: l.Owner, ContinuationFencingToken: l.FencingToken, ExpiresAt: l.ExpiresAt}
}

func (r *Revalidator) finishCrossMintInitialWithdrawal(ctx context.Context, l RevalidationLease, q CrossMintPreparationRequest, bank crossMintPreparationBank, instructions []RouteInstruction, tables []LookupTable) (CrossMintLegPreparation, error) {
	var out CrossMintLegPreparation
	manifest, err := BuildRouteALTManifest(KaminoSameMintRouteRequest{Vault: l.VaultPubkey, Source: bank.source.Position, Target: bank.target.Position}, planSettings(l.ExecutionPlan), l.PolicyAccount, r.signer, instructions, nil)
	if err != nil {
		return out, err
	}
	out.Preparation, out.LastValidBlockHeight, out.MissingAddresses, err = r.compileCrossMintIndependentLeg(ctx, q, instructions, tables, bank.slot)
	if err != nil {
		return out, err
	}
	out.Preparation.RequirementsFingerprint = manifest.Fingerprint
	out.Preparation.Manifest = &manifest
	out.SharedAddresses, out.VaultAddresses = manifest.SharedAddresses, manifest.VaultAddresses
	if len(out.MissingAddresses) > 0 {
		out.WaitingALT = true
		return out, nil
	}
	out.PolicyAccount = l.PolicyAccount
	for _, name := range out.Preparation.Transaction.LookupTables {
		found := false
		for _, table := range tables {
			if table.Address == name && table.ID > 0 && table.FamilyID > 0 && table.Generation >= 0 && table.MutationEpoch >= 0 {
				out.SelectedALTs = append(out.SelectedALTs, ExecutionALT{TableID: table.ID, MutationEpoch: table.MutationEpoch, FamilyID: table.FamilyID, Generation: table.Generation, BindingID: table.BindingID, Address: table.Address, Addresses: append([]string(nil), table.Addresses...)})
				found = true
				break
			}
		}
		if !found {
			return CrossMintLegPreparation{}, errors.New("first withdrawal selected ALT lacks actual registered identity")
		}
	}
	selected, _ := json.Marshal(out.SelectedALTs)
	hash := sha256.Sum256(selected)
	out.AltSelectionFingerprint = hex.EncodeToString(hash[:])
	out.ConflictKeys = canonicalStrings([]string{"vault-write:" + l.VaultPubkey, fmt.Sprintf("fleet-shared-write-lane:%02d", l.VaultID%64)})
	mint, ata, minimum := l.SourceLiquidityMint, bank.source.Position.VaultLiquidityATA, int64(1)
	out.ExpectedEffect, _ = json.Marshal(crossMintPreparationEffect{CreditMint: &mint, CreditAccount: &ata, MinimumCredit: &minimum})
	out.BalanceAnchors, _ = json.Marshal(crossMintPreparationAnchors{Credit: &crossMintPreparationToken{mint, ata, int64(binary.LittleEndian.Uint64(bank.accounts[ata].Data[64:72]))}, Position: &crossMintPreparationPosition{l.SourceReserve, bank.source.Position.Market, bank.source.Obligation, true, int64(bank.sourceCollateral)}})
	out.ObservedAt, out.ObservedSlot = bank.observedAt, bank.slot
	return out, nil
}

func planSettings(raw json.RawMessage) string {
	var plan crossMintPlan
	if json.Unmarshal(raw, &plan) != nil {
		return ""
	}
	return plan.Bindings.Settings
}

func crossMintSourceCertificate(l RevalidationLease, plan crossMintPlan, bank crossMintPreparationBank, build validatedJupiterBuild, policy DecodedSquadsPolicy, valueLoss uint16, tx PreparedTransaction, sim SimulationEvidence, jupiterTables []LookupTable) (CrossMintPreflightCertificate, error) {
	b := plan.Bindings
	readback := func(address string) CrossMintCertificatePolicy {
		hash := sha256.Sum256(bank.accounts[address].Data)
		return CrossMintCertificatePolicy{PolicyAccount: address, ContextSlot: bank.slot, DataSHA256: hex.EncodeToString(hash[:])}
	}
	c := CrossMintPreflightCertificate{Kind: "cross_mint_preflight", CertifiedAt: time.Now().UTC(), Cluster: l.Cluster, SourceMint: l.SourceLiquidityMint, TargetMint: l.TargetLiquidityMint, InputAmountRaw: strconv.FormatUint(l.LiquidityAmountRaw, 10), MinimumOutputAmountRaw: strconv.FormatUint(build.MinimumOutput, 10), EffectiveSlippageBPS: build.Slippage, EffectiveMaximumValueLossBPS: valueLoss,
		FinalizedPolicyReadbacks: CrossMintCertificatePolicies{Withdraw: readback(b.Withdraw.PolicyAccount), Deposit: readback(b.Deposit.PolicyAccount), Swap: CrossMintCertificateSwapPolicy{CrossMintCertificatePolicy: readback(b.Swap.PolicyAccount), PolicySeed: strconv.FormatUint(policy.PolicySeed, 10), SourceShard: b.Swap.SourceShard, ManifestFingerprint: b.Swap.ManifestFingerprint, Dialect: build.Dialect, ConstraintIndex: build.ConstraintIndex, DailySourceMintSpendingCap: strconv.FormatUint(b.Swap.DailySourceMintSpendingCap, 10)}},
		JupiterBuild:             CrossMintCertificateJupiter{ResponseSHA256: build.ResponseSHA256, RouteStepCount: build.RouteSteps, QuotedOutputAmountRaw: strconv.FormatUint(build.QuotedOutput, 10), SetupInstructionCount: 0, LookupTables: tableNames(jupiterTables), ComputeUnitLimit: tx.ComputeLimit, PacketSizeBytes: tx.PacketBytes, PacketDataSizeBytes: SolanaPacketLimit, FitsPacketDataSize: tx.PacketBytes <= SolanaPacketLimit, MessageSHA256: tx.MessageSHA256, LastValidBlockHeight: int64(build.LastValidBlockHeight), ObservedBlockHeight: int64(build.ObservedBlockHeight), InputPreBalanceRaw: strconv.FormatUint(binary.LittleEndian.Uint64(bank.accounts[bank.source.Position.VaultLiquidityATA].Data[64:72]), 10), OutputPreBalanceRaw: strconv.FormatUint(binary.LittleEndian.Uint64(bank.accounts[bank.target.Position.VaultLiquidityATA].Data[64:72]), 10), SimulationAttempted: true, SimulationUnits: sim.UnitsConsumed, SimulationTopology: "withdraw_then_swap_atomic_preflight_only", SimulationLookupTables: append([]string(nil), tx.LookupTables...), TargetDepositPolicyValidated: true, TargetReserve: l.TargetReserve, TargetObligation: bank.target.Obligation}}
	if build.LastValidBlockHeight > math.MaxInt64 || build.ObservedBlockHeight > math.MaxInt64 || !sim.Succeeded || sim.Slot < bank.slot || sim.WireSHA256 != tx.WireSHA256 {
		return c, errors.New("source verifier did not produce exact successful simulation")
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	m := crossMintActivationRequest(l).Movement
	m.PreflightCertification = raw
	if _, err := ValidateCrossMintPreflightCertificate(m, time.Now()); err != nil {
		return c, fmt.Errorf("actual prewithdraw source certificate: %w", err)
	}
	return c, nil
}

package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool     *pgxpool.Pool
	ownsPool bool
}

func OpenStore(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open fleet database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping fleet database: %w", err)
	}
	return &Store{pool: pool, ownsPool: true}, nil
}
func (s *Store) Close() {
	if s != nil && s.pool != nil && s.ownsPool {
		s.pool.Close()
	}
}

func NewStoreFromPool(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, errors.New("fleet requires a caller-owned pool")
	}
	return &Store{pool: pool}, nil
}

func (s *Store) RegisterFleetPlanningCluster(ctx context.Context, cluster string) error {
	if s == nil || s.pool == nil || cluster == "" || cluster != strings.TrimSpace(cluster) {
		return errors.New("fleet planning cluster requires a canonical cluster")
	}
	if _, err := s.pool.Exec(ctx, `
INSERT INTO loyal_yield.fleet_planning_clusters(cluster)
VALUES($1)
ON CONFLICT(cluster) DO NOTHING`, cluster); err != nil {
		return fmt.Errorf("register fleet planning cluster: %w", err)
	}
	return nil
}

func (s *Store) HeartbeatFleetPlanningCluster(ctx context.Context, cluster string) error {
	if s == nil || s.pool == nil || cluster == "" || cluster != strings.TrimSpace(cluster) {
		return errors.New("fleet planning heartbeat requires a canonical cluster")
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE loyal_yield.fleet_planning_clusters
SET last_seen_at=clock_timestamp()
WHERE cluster=$1`, cluster)
	if err != nil {
		return fmt.Errorf("heartbeat fleet planning cluster: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("fleet planning heartbeat has no registered cluster")
	}
	return nil
}

type FleetLoadOptions struct {
	DelegatedSigner          string
	EnableCrossMint          bool
	CrossMintMaxValueLossBPS uint16
}

type RegisteredVault struct {
	VaultID                        int64
	Settings                       string
	VaultIndex                     int16
	VaultPubkey                    string
	PolicyID                       int64
	PolicyAccount, PolicyAuthority string
	Markets, StableMints           []string
	KaminoMints, DelegatedSigners  []string
	EarnPolicies, SwapPolicies     json.RawMessage
}

func (s *Store) LoadRegistry(ctx context.Context, cluster string, options FleetLoadOptions) ([]RegisteredVault, error) {
	if cluster == "" {
		return nil, errors.New("cluster is required")
	}
	rows, err := s.pool.Query(ctx, `
SELECT vault.id, vault.settings, vault.vault_index, vault.vault_pubkey,
       policy.id, policy.policy_account, policy.authority, policy.kamino_markets,
       policy.stable_mints, policy.kamino_liquidity_mints, policy.delegated_signers,
       COALESCE((SELECT jsonb_agg(jsonb_build_object(
          'policy_account',ep.policy_account,'observed_slot',ep.last_seen_slot,
          'observed_signature',ep.last_seen_signature,'source_commitment',ep.source_commitment,
          'threshold',ep.threshold,'stable_mints',ep.stable_mints,
          'kamino_markets',ep.kamino_markets,'kamino_liquidity_mints',ep.kamino_liquidity_mints)
          ORDER BY ep.last_seen_slot DESC,ep.policy_account)
        FROM loyal_yield.route_policies ep
        WHERE $3 AND $2<>'' AND ep.active AND ep.finalized_eligible
          AND ep.source_commitment='finalized' AND ep.cluster=$1
          AND ep.settings=vault.settings AND ep.authority=policy.authority
          AND ep.vault_index=vault.vault_index AND ep.vault_pubkey=vault.vault_pubkey
          AND $2=ANY(ep.delegated_signers) AND 'same_mint_kamino'=ANY(ep.route_modes)), '[]'::jsonb),
       COALESCE((SELECT jsonb_agg(jsonb_build_object(
          'policy_account',sp.policy_account,'source_shard',sp.source_shard,
          'enrollment_generation',oi.generation,'observed_slot',sp.last_seen_slot,
          'observed_signature',sp.last_seen_signature,'source_commitment',sp.source_commitment,
          'max_slippage_bps',sp.max_slippage_bps,
          'daily_source_mint_spending_cap',sp.daily_source_mint_spending_cap,
          'manifest_fingerprint',sp.manifest_fingerprint)
          ORDER BY sp.source_shard,sp.last_seen_slot DESC,sp.policy_account)
        FROM loyal_yield.cross_mint_swap_policies sp
        JOIN loyal_yield.cross_mint_vault_opt_ins oi ON oi.cluster=sp.cluster
          AND oi.settings=sp.settings AND oi.vault_index=sp.vault_index
          AND oi.vault_pubkey=sp.vault_pubkey AND oi.enabled
        WHERE $3 AND $2<>'' AND sp.active AND sp.start_eligible
          AND sp.source_commitment='finalized' AND sp.last_mutation IN ('create','update')
          AND sp.cluster=$1 AND sp.settings=vault.settings AND sp.authority=policy.authority
          AND sp.vault_index=vault.vault_index AND sp.vault_pubkey=vault.vault_pubkey
          AND sp.delegated_signer=$2
          AND sp.max_slippage_bps=oi.max_slippage_bps
          AND sp.daily_source_mint_spending_cap=oi.daily_source_mint_spending_cap
          AND ((sp.source_shard='classic' AND sp.policy_account=oi.classic_policy_account AND sp.policy_seed=oi.classic_policy_seed)
            OR (sp.source_shard='token_2022' AND sp.policy_account=oi.token_2022_policy_account AND sp.policy_seed=oi.token_2022_policy_seed))), '[]'::jsonb)
FROM loyal_yield.managed_vaults vault
JOIN loyal_yield.route_policies policy ON policy.id=vault.active_policy_id
WHERE vault.active AND policy.active
  AND ($2='' OR (policy.cluster=$1 AND policy.source_commitment='finalized'
       AND policy.finalized_eligible AND $2=ANY(policy.delegated_signers)))
  AND 'same_mint_kamino'=ANY(policy.route_modes)
  AND NOT EXISTS (
    SELECT 1 FROM loyal_yield.rebalance_opportunities o
    WHERE o.cluster=$1 AND o.vault_id=vault.id
      AND (o.opportunity_state='decision_created'
        OR (o.opportunity_state='leased' AND o.lease_expires_at>clock_timestamp())))
  AND NOT EXISTS (SELECT 1 FROM loyal_yield.rebalance_decisions d WHERE d.vault_id=vault.id AND d.status::text IN ('planned','simulating','ready','submitted','confirming'))
ORDER BY vault.id`, cluster, options.DelegatedSigner, options.EnableCrossMint)
	if err != nil {
		return nil, fmt.Errorf("load fleet registry: %w", err)
	}
	defer rows.Close()
	var registry []RegisteredVault
	for rows.Next() {
		var v RegisteredVault
		if err := rows.Scan(&v.VaultID, &v.Settings, &v.VaultIndex, &v.VaultPubkey, &v.PolicyID, &v.PolicyAccount, &v.PolicyAuthority, &v.Markets, &v.StableMints, &v.KaminoMints, &v.DelegatedSigners, &v.EarnPolicies, &v.SwapPolicies); err != nil {
			return nil, err
		}
		registry = append(registry, v)
	}
	return registry, rows.Err()
}

func SnapshotVaults(registry []RegisteredVault) []SnapshotVault {
	out := make([]SnapshotVault, len(registry))
	for i, v := range registry {
		out[i] = SnapshotVault{Pubkey: v.VaultPubkey, PolicyAccount: v.PolicyAccount, Markets: v.Markets}
	}
	return out
}

type observedEarnPolicy struct {
	PolicyAccount        string   `json:"policy_account"`
	ObservedSlot         int64    `json:"observed_slot"`
	ObservedSignature    string   `json:"observed_signature"`
	SourceCommitment     string   `json:"source_commitment"`
	Threshold            int      `json:"threshold"`
	StableMints          []string `json:"stable_mints"`
	KaminoMarkets        []string `json:"kamino_markets"`
	KaminoLiquidityMints []string `json:"kamino_liquidity_mints"`
}

type observedSwapPolicy struct {
	PolicyAccount              string `json:"policy_account"`
	SourceShard                string `json:"source_shard"`
	EnrollmentGeneration       int64  `json:"enrollment_generation"`
	ObservedSlot               int64  `json:"observed_slot"`
	ObservedSignature          string `json:"observed_signature"`
	SourceCommitment           string `json:"source_commitment"`
	MaxSlippageBPS             int64  `json:"max_slippage_bps"`
	DailySourceMintSpendingCap int64  `json:"daily_source_mint_spending_cap"`
	ManifestFingerprint        string `json:"manifest_fingerprint"`
}

func PlanningVaults(snapshot Snapshot, catalog []ReserveIdentity, registry []RegisteredVault, inflow, outflow map[string]int64, options FleetLoadOptions) ([]FleetVault, error) {
	var fleet []FleetVault
	for _, v := range registry {
		facts := snapshot.Vaults[v.VaultPubkey]
		blocked := ""
		switch p := facts.Policy; {
		case p == nil:
			blocked = "route_policy_unreadable"
		case p.Settings != v.Settings || int16(p.AccountIndex) != v.VaultIndex:
			blocked = "route_policy_identity_mismatch"
		case options.DelegatedSigner != "" && (len(p.DelegatedSigners) != 1 || p.DelegatedSigners[0] != options.DelegatedSigner):
			blocked = "route_policy_not_delegated"
		}
		for _, h := range facts.Holdings {
			if !contains(v.Markets, h.Market) || !contains(v.StableMints, h.Mint) || !contains(v.KaminoMints, h.Mint) {
				continue
			}
			if h.LiquidityRaw > math.MaxInt64 || h.CollateralRaw > math.MaxInt64 {
				return nil, fmt.Errorf("vault %d holding in %s exceeds int64", v.VaultID, h.Reserve)
			}
			p := VaultPosition{
				VaultID: v.VaultID, Settings: v.Settings, VaultIndex: v.VaultIndex, VaultPubkey: v.VaultPubkey,
				PolicyID: v.PolicyID, PolicyAuthority: v.PolicyAuthority, PolicyAccount: v.PolicyAccount,
				SourceReserve: h.Reserve, Market: h.Market, Mint: h.Mint,
				AmountRaw: int64(h.LiquidityRaw), SourceCollateralAmountRaw: int64(h.CollateralRaw),
				SourceAmountSemantics: amountSemanticsKaminoCollateralDeposited,
				ObservedSlot:          snapshot.Slot, ObservedAt: snapshot.ObservedAt, BlockedReason: blocked,
			}
			if blocked != "" || p.AmountRaw <= 0 {
				if p.BlockedReason == "" {
					p.BlockedReason = "unsupported_source_amount_evidence"
				}
				fleet = append(fleet, FleetVault{Position: p})
				continue
			}
			allowed := []string{}
			for _, identity := range catalog {
				if identity.Mint == h.Mint && contains(v.Markets, identity.Market) {
					allowed = append(allowed, identity.Address)
				}
			}
			crossTargets, err := crossMintTargets(snapshot, catalog, p, v, options)
			if err != nil {
				return nil, fmt.Errorf("vault %d cross-mint policy evidence: %w", v.VaultID, err)
			}
			fleet = append(fleet, FleetVault{Position: p, AllowedTargets: canonicalStrings(allowed), CrossMintTargets: crossTargets, CrossMintMaxValueLossBPS: options.CrossMintMaxValueLossBPS, CommittedInflows: inflow, CommittedOutflows: outflow})
		}
	}
	return fleet, nil
}

func crossMintTargets(snapshot Snapshot, catalog []ReserveIdentity, position VaultPosition, v RegisteredVault, options FleetLoadOptions) (map[string]CrossMintPolicyBindings, error) {
	result := map[string]CrossMintPolicyBindings{}
	if !options.EnableCrossMint || options.DelegatedSigner == "" || !contains(v.DelegatedSigners, options.DelegatedSigner) {
		return result, nil
	}
	var earn []observedEarnPolicy
	var swaps []observedSwapPolicy
	if err := json.Unmarshal(v.EarnPolicies, &earn); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(v.SwapPolicies, &swaps); err != nil {
		return nil, err
	}
	if len(swaps) != 2 || position.VaultIndex < 0 || position.VaultIndex > 255 {
		return result, nil
	}
	byShard := map[string]observedSwapPolicy{}
	for _, policy := range swaps {
		if policy.SourceCommitment != "finalized" || policy.PolicyAccount == "" || policy.ObservedSlot <= 0 || policy.ObservedSignature == "" ||
			policy.MaxSlippageBPS <= 0 || policy.MaxSlippageBPS > 10_000 || policy.DailySourceMintSpendingCap <= 0 || len(policy.ManifestFingerprint) != 64 ||
			(policy.SourceShard != "classic" && policy.SourceShard != "token_2022") {
			return result, nil
		}
		if _, duplicate := byShard[policy.SourceShard]; duplicate {
			return result, nil
		}
		byShard[policy.SourceShard] = policy
	}
	classic, classicOK := byShard["classic"]
	token2022, token2022OK := byShard["token_2022"]
	if !classicOK || !token2022OK || classic.MaxSlippageBPS != token2022.MaxSlippageBPS ||
		classic.DailySourceMintSpendingCap != token2022.DailySourceMintSpendingCap || classic.EnrollmentGeneration != token2022.EnrollmentGeneration {
		return result, nil
	}
	swap := classic
	if earnTokenProgram(position.Mint) != earnTokenProgram(USDCMint) {
		swap = token2022
	}
	exactEarn := func(mint, market string) (observedEarnPolicy, bool) {
		for _, policy := range earn {
			if policy.Threshold == 1 && policy.SourceCommitment == "finalized" && policy.PolicyAccount != "" && policy.ObservedSlot > 0 &&
				policy.ObservedSignature != "" && contains(policy.StableMints, mint) && contains(policy.KaminoLiquidityMints, mint) && contains(policy.KaminoMarkets, market) {
				return policy, true
			}
		}
		return observedEarnPolicy{}, false
	}
	withdraw, ok := exactEarn(position.Mint, position.Market)
	if !ok {
		return result, nil
	}
	for _, target := range catalog {
		state, routable := snapshot.Reserves[target.Address]
		if !routable || !state.TargetEligible() || target.Mint == position.Mint || !isEarnStableMint(target.Mint) {
			continue
		}
		deposit, ok := exactEarn(target.Mint, target.Market)
		if !ok {
			continue
		}
		bindings := CrossMintPolicyBindings{
			Settings: position.Settings, VaultIndex: uint8(position.VaultIndex), VaultPubkey: position.VaultPubkey, DelegatedSigner: options.DelegatedSigner,
			Withdraw: CrossMintEarnPolicyBinding{PolicyAccount: withdraw.PolicyAccount, ObservedSlot: uint64(withdraw.ObservedSlot), ObservedSignature: withdraw.ObservedSignature, SourceCommitment: withdraw.SourceCommitment, ConstraintIndex: 0},
			Swap:     CrossMintSwapPolicyBinding{PolicyAccount: swap.PolicyAccount, SourceShard: swap.SourceShard, EnrollmentGeneration: swap.EnrollmentGeneration, ObservedSlot: uint64(swap.ObservedSlot), ObservedSignature: swap.ObservedSignature, SourceCommitment: swap.SourceCommitment, MaxSlippageBPS: uint16(swap.MaxSlippageBPS), DailySourceMintSpendingCap: uint64(swap.DailySourceMintSpendingCap), ManifestFingerprint: swap.ManifestFingerprint},
			Deposit:  CrossMintEarnPolicyBinding{PolicyAccount: deposit.PolicyAccount, ObservedSlot: uint64(deposit.ObservedSlot), ObservedSignature: deposit.ObservedSignature, SourceCommitment: deposit.SourceCommitment, ConstraintIndex: 1},
		}
		if bindings.Withdraw.PolicyAccount == bindings.Swap.PolicyAccount || bindings.Deposit.PolicyAccount == bindings.Swap.PolicyAccount {
			continue
		}
		result[target.Address] = bindings
	}
	return result, nil
}

type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

const inflightFlowsSQL = `
SELECT o.source_reserve, COALESCE(d.active_target_reserve, o.target_reserve), o.principal_usd_micros
FROM loyal_yield.rebalance_opportunities o
LEFT JOIN loyal_yield.rebalance_decisions d ON d.id=o.decision_id
WHERE o.cluster=$1 AND o.id<>$3
  AND ((o.opportunity_state='leased' AND o.lease_expires_at>clock_timestamp())
    OR (o.decision_id IS NOT NULL
        AND (d.status::text IN ('planned','simulating','ready','submitted','confirming')
          OR (d.status::text='confirmed' AND d.id IN (SELECT s.decision_id FROM loyal_yield.signed_route_submissions s
                WHERE s.movement_leg IN ('route','deposit') AND s.confirmed_slot>$2)))
        AND NOT EXISTS (SELECT 1 FROM loyal_yield.signed_route_submissions s
          WHERE s.decision_id=d.id AND s.movement_leg IN ('route','deposit')
            AND s.confirmed_slot IS NOT NULL AND s.confirmed_slot<=$2)))`

func LockedInflightFlows(ctx context.Context, tx pgx.Tx, cluster string, slot, exclude int64, reserves ...string) (inflow, outflow map[string]int64, err error) {
	for _, reserve := range canonicalStrings(reserves) {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "loyal-yield-fleet-reserve:"+cluster+":"+reserve); err != nil {
			return nil, nil, err
		}
	}
	return InflightFlows(ctx, tx, cluster, slot, exclude)
}

func InflightFlows(ctx context.Context, q Querier, cluster string, slot, exclude int64) (inflow, outflow map[string]int64, err error) {
	rows, err := q.Query(ctx, inflightFlowsSQL, cluster, slot, exclude)
	if err != nil {
		return nil, nil, fmt.Errorf("in-flight moves: %w", err)
	}
	defer rows.Close()
	inflow, outflow = map[string]int64{}, map[string]int64{}
	for rows.Next() {
		var source *string
		var target string
		var principal int64
		if err := rows.Scan(&source, &target, &principal); err != nil {
			return nil, nil, err
		}
		var ok bool
		if inflow[target], ok = sumInt64(inflow[target], principal); !ok || principal < 0 {
			return nil, nil, errors.New("in-flight inflow overflow")
		}
		if source != nil {
			if outflow[*source], ok = sumInt64(outflow[*source], principal); !ok {
				return nil, nil, errors.New("in-flight outflow overflow")
			}
		}
	}
	return inflow, outflow, rows.Err()
}

type PublishSummary struct {
	EpochID                                 int64
	Published, Kept, Readmitted, Superseded int64
	ActiveWork                              int64
}

const unclaimedSQL = `o.decision_id IS NULL AND (o.opportunity_state IN ('revalidate','waiting_alt','ready')
  OR (o.opportunity_state='leased' AND o.lease_expires_at<=clock_timestamp()))`

const keepMargin = "75 seconds"

func (s *Store) PublishPlan(ctx context.Context, cluster string, snapshot Snapshot, plan FleetPlan) (PublishSummary, error) {
	var summary PublishSummary
	record, err := recordOf(snapshot)
	if err != nil {
		return summary, err
	}
	if summary.EpochID, err = s.recordSnapshot(ctx, cluster, record); err != nil {
		return summary, fmt.Errorf("record snapshot: %w", err)
	}
	held := []int64{}
	for _, opportunity := range plan.Opportunities {
		id, outcome, err := s.publishOne(ctx, cluster, summary.EpochID, record, opportunity)
		if err != nil {
			return summary, fmt.Errorf("publish vault %d: %w", opportunity.Decision.VaultID, err)
		}
		switch outcome {
		case "published":
			summary.Published++
		case "kept":
			summary.Kept++
		case "alt_readmitted":
			summary.Readmitted++
		case "active_work":
			summary.ActiveWork++
		}
		if id > 0 {
			held = append(held, id)
		}
		logEvent(map[string]any{"event": "kamino_fleet_planner_opportunity", "vaultId": opportunity.Decision.VaultID, "sourceReserve": opportunity.Decision.SourceReserve, "targetReserve": opportunity.Decision.TargetReserve, "publishReason": outcome, "opportunityId": id})
	}
	err = s.pool.QueryRow(ctx, `
WITH superseded AS (
 UPDATE loyal_yield.rebalance_opportunities o
 SET opportunity_state='superseded',terminal_reason='replanned',lease_kind=NULL,lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp()
 WHERE o.cluster=$1 AND NOT (o.id=ANY($2::bigint[])) AND `+unclaimedSQL+`
 RETURNING o.id
), released AS (
 DELETE FROM loyal_yield.route_account_conflict_leases conflict USING superseded
 WHERE conflict.opportunity_id=superseded.id AND conflict.submission_id IS NULL
)
SELECT count(*) FROM superseded`, cluster, held).Scan(&summary.Superseded)
	if err != nil {
		return summary, fmt.Errorf("supersede unplanned moves: %w", err)
	}
	return summary, nil
}

// recordSnapshot stores the tick's reserves once; the same snapshot read
// twice is one row. The lookup is its own statement: a conflicting insert
// waits for the other writer's commit, and only a later statement's snapshot
// sees that row.
func (s *Store) recordSnapshot(ctx context.Context, cluster string, record snapshotRecord) (int64, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
INSERT INTO loyal_yield.optimizer_epochs(cluster,epoch_key,market_slot,observed_at,expires_at,market_state)
VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(cluster,epoch_key) DO NOTHING RETURNING id`, cluster, record.Key, record.Slot, record.ObservedAt, record.ExpiresAt, record.State).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		err = s.pool.QueryRow(ctx, `SELECT id FROM loyal_yield.optimizer_epochs WHERE cluster=$1 AND epoch_key=$2`, cluster, record.Key).Scan(&id)
	}
	return id, err
}

// LockVaultForDecision locks the vault row against opportunity publication and reports whether it is active (unknown is inactive).
func LockVaultForDecision(ctx context.Context, tx pgx.Tx, vaultID int64) (active bool, err error) {
	err = tx.QueryRow(ctx, `SELECT active FROM loyal_yield.managed_vaults WHERE id=$1 FOR UPDATE`, vaultID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return active, err
}

func (s *Store) publishOne(ctx context.Context, cluster string, epochID int64, record snapshotRecord, planned PlannedOpportunity) (int64, string, error) {
	d := planned.Decision
	if err := d.Validate(); err != nil {
		return 0, "", err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback(ctx)
	vaultActive, err := LockVaultForDecision(ctx, tx, d.VaultID)
	if err != nil {
		return 0, "", err
	}
	if !vaultActive {
		return 0, "inactive_vault", tx.Commit(ctx)
	}
	type row struct {
		id                    int64
		state, source, target string
		routeKind             string
		unclaimed, fresh, alt bool
	}
	rows, err := tx.Query(ctx, `
SELECT o.id,o.opportunity_state,COALESCE(o.source_reserve,''),o.target_reserve,COALESCE(o.execution_plan->>'route_kind',''),
       `+unclaimedSQL+`, o.expires_at>clock_timestamp()+interval '`+keepMargin+`',
       EXISTS (
         SELECT 1 FROM loyal_yield.lookup_table_provisioning_request_consumers consumer
         JOIN loyal_yield.lookup_table_provisioning_requests request ON request.id=consumer.provisioning_request_id
         WHERE consumer.opportunity_id=o.id AND request.cluster=o.cluster AND request.request_status='satisfied'
           AND request.sealed_at IS NOT NULL AND request.requirements_fingerprint=o.requirements_fingerprint)
FROM loyal_yield.rebalance_opportunities o
WHERE o.cluster=$1 AND o.vault_id=$2 AND o.opportunity_state IN ('waiting_alt','revalidate','ready','leased','decision_created')
FOR UPDATE OF o`, cluster, d.VaultID)
	if err != nil {
		return 0, "", err
	}
	var existing []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.state, &r.source, &r.target, &r.routeKind, &r.unclaimed, &r.fresh, &r.alt); err != nil {
			rows.Close()
			return 0, "", err
		}
		existing = append(existing, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, "", err
	}
	var deciding bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.rebalance_decisions WHERE vault_id=$1 AND status::text IN ('planned','simulating','ready','submitted','confirming'))`, d.VaultID).Scan(&deciding); err != nil {
		return 0, "", err
	}
	stale := []int64{}
	for _, r := range existing {
		if !r.unclaimed {
			deciding = true
			continue
		}
		if r.fresh && r.source == d.SourceReserve && r.target == d.TargetReserve && r.routeKind == d.RouteKind {
			outcome := "kept"
			if r.state == "waiting_alt" && r.alt {
				if _, err := tx.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state='revalidate',available_at=clock_timestamp(),terminal_reason=NULL,updated_at=clock_timestamp() WHERE id=$1`, r.id); err != nil {
					return 0, "", err
				}
				outcome = "alt_readmitted"
			}
			return r.id, outcome, tx.Commit(ctx)
		}
		stale = append(stale, r.id)
	}
	if deciding {
		return 0, "active_work", tx.Commit(ctx)
	}
	if len(stale) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE loyal_yield.rebalance_opportunities SET opportunity_state='superseded',terminal_reason='replanned',lease_kind=NULL,lease_owner=NULL,lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=ANY($1::bigint[])`, stale); err != nil {
			return 0, "", err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM loyal_yield.route_account_conflict_leases WHERE opportunity_id=ANY($1::bigint[]) AND submission_id IS NULL`, stale); err != nil {
			return 0, "", err
		}
	}
	sourceMint, targetMint := d.SourceMint, d.TargetMint
	key := opportunityIdentity(cluster, epochID, d, planned.ExecutionPlan, record.ExpiresAt)
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO loyal_yield.rebalance_opportunities
(cluster,idempotency_key,rediscovery_key,attempt_generation,vault_id,source_snapshot_id,optimizer_epoch_id,source_reserve,target_reserve,liquidity_mint,source_liquidity_mint,target_liquidity_mint,amount_raw,principal_usd_micros,source_apy_bps,target_apy_bps,estimated_edge_bps,estimated_cost_lamports,annual_yield_gain_usd_micros,expected_net_gain_usd_micros,economic_priority,priority_version,operation_class,opportunity_state,execution_plan,available_at,expires_at)
VALUES ($1,$2,$2,1,$3,NULL,$4,$5,$6,$7,$8,$7,$9,$10,$11,$12,$13,$14,$15,$16,$17,'lost-yield-service-net-reserve-capacity-v3','yield_optimization','revalidate',$18,clock_timestamp(),$19) RETURNING id`,
		cluster, key, d.VaultID, epochID, d.SourceReserve, d.TargetReserve, targetMint, sourceMint, d.AmountRaw, d.PrincipalUSDMicros, d.SourceAPYBPS, d.TargetAPYBPS, d.EdgeBPS, d.EstimatedCostLamports, d.AnnualYieldGainUSDMicros, d.ExpectedNetGainUSDMicros, d.EconomicPriority, planned.ExecutionPlan, record.ExpiresAt).Scan(&id)
	if err != nil {
		return 0, "", err
	}
	return id, "published", tx.Commit(ctx)
}

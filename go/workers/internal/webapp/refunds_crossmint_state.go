package webapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type crossMintScope struct {
	Authority   string
	Cluster     string
	Settings    string
	VaultIndex  int16
	VaultPubkey string
}

type crossMintBoundPolicy struct {
	Account     string
	Seed        uint64
	SourceShard string
}

type crossMintPolicyState struct {
	Bound             crossMintBoundPolicy
	LastSeenSignature string
	LastSeenSlot      string
}

type crossMintState struct {
	BoundPolicies              []crossMintBoundPolicy
	DailySourceMintSpendingCap string
	Enabled                    bool
	Generation                 string
	MaxSlippageBps             int32
	Policies                   []crossMintPolicyState
	Status                     string
}

type crossMintEnrollment struct {
	BoundPolicies              []crossMintBoundPolicy
	DailySourceMintSpendingCap string
	Enabled                    bool
	Generation                 string
	MaxSlippageBps             int32
}

type crossMintPolicyRow struct {
	Active            bool
	DailyCap          *int64
	DelegatedSigner   *string
	LastMutation      string
	LastSeenSignature string
	LastSeenSlot      int64
	MaxSlippageBps    *int32
	PolicyAccount     string
	PolicySeed        *int64
	SourceCommitment  string
	SourceShard       *string
	StartEligible     bool
}

type crossMintProjectedPolicy struct {
	Account                    string
	DailySourceMintSpendingCap string
	DelegatedSigner            string
	LastSeenSignature          string
	LastSeenSlot               string
	MaxSlippageBps             int32
	Seed                       uint64
	SourceCommitment           string
	SourceShard                string
}

type crossMintPolicyIndex struct {
	Policies []crossMintProjectedPolicy
	State    string // ambiguous, complete, empty, partial
}

func isCrossMintPolicyShard(value *string) bool {
	return value != nil && (*value == "classic" || *value == "token_2022")
}

func isCrossMintCommitment(value string) bool {
	return value == "confirmed" || value == "finalized"
}

func deriveCrossMintPolicyIndex(rows []crossMintPolicyRow) crossMintPolicyIndex {
	var active []crossMintPolicyRow
	for _, row := range rows {
		if row.Active && (row.LastMutation == "create" || row.LastMutation == "update") {
			active = append(active, row)
		}
	}
	if len(active) == 0 {
		return crossMintPolicyIndex{State: "empty"}
	}

	var candidates []crossMintProjectedPolicy
	for _, row := range active {
		if !row.StartEligible || !isCrossMintPolicyShard(row.SourceShard) || !isCrossMintCommitment(row.SourceCommitment) ||
			row.PolicySeed == nil || row.MaxSlippageBps == nil || row.DailyCap == nil ||
			row.DelegatedSigner == nil || *row.DelegatedSigner == "" {
			return crossMintPolicyIndex{State: "ambiguous"}
		}
		if *row.PolicySeed < 0 || *row.DailyCap < 0 {
			// TS carries these as bigint and keeps going; a negative seed or
			// cap never comes from the enrollment writer, and the uint64 views
			// below cannot state one.
			return crossMintPolicyIndex{State: "ambiguous"}
		}
		candidates = append(candidates, crossMintProjectedPolicy{
			Account:                    row.PolicyAccount,
			DailySourceMintSpendingCap: fmt.Sprintf("%d", *row.DailyCap),
			DelegatedSigner:            *row.DelegatedSigner,
			LastSeenSignature:          row.LastSeenSignature,
			LastSeenSlot:               fmt.Sprintf("%d", row.LastSeenSlot),
			MaxSlippageBps:             *row.MaxSlippageBps,
			Seed:                       uint64(*row.PolicySeed),
			SourceCommitment:           row.SourceCommitment,
			SourceShard:                *row.SourceShard,
		})
	}

	first := candidates[0]
	accounts := map[string]struct{}{}
	seeds := map[uint64]struct{}{}
	shards := map[string]struct{}{}
	parametersMatch := true
	for _, candidate := range candidates {
		accounts[candidate.Account] = struct{}{}
		seeds[candidate.Seed] = struct{}{}
		shards[candidate.SourceShard] = struct{}{}
		if candidate.DelegatedSigner != first.DelegatedSigner ||
			candidate.MaxSlippageBps != first.MaxSlippageBps ||
			candidate.DailySourceMintSpendingCap != first.DailySourceMintSpendingCap {
			parametersMatch = false
		}
	}
	if len(candidates) > 2 || len(accounts) != len(candidates) || len(seeds) != len(candidates) ||
		len(shards) != len(candidates) || !parametersMatch {
		return crossMintPolicyIndex{State: "ambiguous"}
	}

	sorted := make([]crossMintProjectedPolicy, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.SourceShard == "classic" {
			sorted = append(sorted, candidate)
		}
	}
	for _, candidate := range candidates {
		if candidate.SourceShard != "classic" {
			sorted = append(sorted, candidate)
		}
	}
	state := "partial"
	if len(sorted) == 2 {
		state = "complete"
	}
	return crossMintPolicyIndex{Policies: sorted, State: state}
}

func findEarnCrossMintOptIn(ctx context.Context, yieldDB *pgxpool.Pool, scope crossMintScope) (enabled bool, generation int64, found bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = yieldDB.QueryRow(ctx, `SELECT enabled, generation FROM loyal_yield.cross_mint_vault_opt_ins
WHERE cluster = $1 AND settings = $2 AND vault_index = $3 AND vault_pubkey = $4
LIMIT 1`, scope.Cluster, scope.Settings, scope.VaultIndex, scope.VaultPubkey).Scan(&enabled, &generation)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, 0, false, nil
		}
		return false, 0, false, fmt.Errorf("read cross-mint opt-in: %w", err)
	}
	return enabled, generation, true, nil
}

func findEarnCrossMintPolicyRows(ctx context.Context, yieldDB *pgxpool.Pool, scope crossMintScope) ([]crossMintPolicyRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := yieldDB.Query(ctx, `SELECT active, daily_source_mint_spending_cap, delegated_signer, last_mutation,
  last_seen_signature, last_seen_slot, max_slippage_bps, policy_account, policy_seed,
  source_commitment, source_shard, start_eligible
FROM loyal_yield.cross_mint_swap_policies
WHERE cluster = $1 AND settings = $2 AND authority = $3 AND vault_index = $4 AND vault_pubkey = $5
ORDER BY last_seen_slot DESC`, scope.Cluster, scope.Settings, scope.Authority, scope.VaultIndex, scope.VaultPubkey)
	if err != nil {
		return nil, fmt.Errorf("read cross-mint swap policies: %w", err)
	}
	defer rows.Close()
	var projected []crossMintPolicyRow
	for rows.Next() {
		var row crossMintPolicyRow
		if err := rows.Scan(&row.Active, &row.DailyCap, &row.DelegatedSigner, &row.LastMutation,
			&row.LastSeenSignature, &row.LastSeenSlot, &row.MaxSlippageBps, &row.PolicyAccount, &row.PolicySeed,
			&row.SourceCommitment, &row.SourceShard, &row.StartEligible); err != nil {
			return nil, fmt.Errorf("read cross-mint swap policies: %w", err)
		}
		projected = append(projected, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read cross-mint swap policies: %w", err)
	}
	return projected, nil
}

func findEarnCrossMintSnapshot(ctx context.Context, yieldDB *pgxpool.Pool, scope crossMintScope) (*crossMintState, crossMintPolicyIndex, error) {
	optInEnabled, optInGeneration, found, err := findEarnCrossMintOptIn(ctx, yieldDB, scope)
	if err != nil {
		return nil, crossMintPolicyIndex{}, err
	}
	policyRows, err := findEarnCrossMintPolicyRows(ctx, yieldDB, scope)
	if err != nil {
		return nil, crossMintPolicyIndex{}, err
	}
	index := deriveCrossMintPolicyIndex(policyRows)
	if !found || index.State != "complete" {
		return nil, index, nil
	}

	classic, token2022 := index.Policies[0], index.Policies[1]
	pairIsReady := true
	for _, policy := range index.Policies {
		if !isCrossMintCommitment(policy.SourceCommitment) {
			pairIsReady = false
		}
	}
	bound := []crossMintBoundPolicy{
		{Account: classic.Account, Seed: classic.Seed, SourceShard: "classic"},
		{Account: token2022.Account, Seed: token2022.Seed, SourceShard: "token_2022"},
	}
	var policies []crossMintPolicyState
	if pairIsReady {
		for i, policy := range []crossMintProjectedPolicy{classic, token2022} {
			policies = append(policies, crossMintPolicyState{
				Bound:             bound[i],
				LastSeenSignature: policy.LastSeenSignature,
				LastSeenSlot:      policy.LastSeenSlot,
			})
		}
	}
	status := "paused"
	if optInEnabled {
		if pairIsReady {
			status = "on"
		} else {
			status = "finalizing"
		}
	}
	return &crossMintState{
		BoundPolicies:              bound,
		DailySourceMintSpendingCap: classic.DailySourceMintSpendingCap,
		Enabled:                    optInEnabled,
		Generation:                 fmt.Sprintf("%d", optInGeneration),
		MaxSlippageBps:             classic.MaxSlippageBps,
		Policies:                   policies,
		Status:                     status,
	}, index, nil
}

func findEarnCrossMintState(ctx context.Context, yieldDB *pgxpool.Pool, scope crossMintScope) (*crossMintState, error) {
	state, _, err := findEarnCrossMintSnapshot(ctx, yieldDB, scope)
	return state, err
}

func setEarnCrossMintEnabled(ctx context.Context, yieldDB *pgxpool.Pool, scope crossMintScope, enabled bool, expectedGeneration int64) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var kind string
	err := yieldDB.QueryRow(ctx, `WITH current_enrollment AS (
  SELECT *
  FROM loyal_yield.cross_mint_vault_opt_ins
  WHERE cluster = $1 AND settings = $2 AND vault_index = $3 AND vault_pubkey = $4
  FOR UPDATE
),
updated_enrollment AS (
  UPDATE loyal_yield.cross_mint_vault_opt_ins AS enrollment
  SET enabled = $5,
      generation = enrollment.generation + 1,
      updated_at = CURRENT_TIMESTAMP
  FROM current_enrollment AS current
  WHERE enrollment.cluster = current.cluster
    AND enrollment.settings = current.settings
    AND enrollment.vault_index = current.vault_index
    AND enrollment.vault_pubkey = current.vault_pubkey
    AND current.generation = $6
    AND current.enabled <> $5
  RETURNING enrollment.*
)
SELECT
  CASE
    WHEN updated.generation IS NOT NULL THEN 'applied'
    WHEN current.enabled = $5
      AND current.generation IN ($6, $6 + 1) THEN 'idempotent'
    ELSE 'stale'
  END AS kind
FROM current_enrollment AS current
LEFT JOIN updated_enrollment AS updated ON true
UNION ALL
SELECT
  'missing' AS kind
WHERE NOT EXISTS (SELECT 1 FROM current_enrollment)`,
		scope.Cluster, scope.Settings, scope.VaultIndex, scope.VaultPubkey, enabled, expectedGeneration).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		// The UNION ALL always yields one row; a missing one means the
		// statement never ran to completion.
		return "missing", nil
	}
	if err != nil {
		return "", fmt.Errorf("write cross-mint opt-in: %w", err)
	}
	return kind, nil
}

// setEarnCrossMintEnabledHuge handles an expectedGeneration outside int64:
// the bigint column can never equal it nor sit in {expected, expected+1}, so
// the CTE can only answer stale, or missing when no enrollment row exists.
// Reading the row instead of sending an out-of-range parameter gives the same
// outcome without a Postgres overflow error.
func setEarnCrossMintEnabledHuge(ctx context.Context, yieldDB *pgxpool.Pool, scope crossMintScope) (string, error) {
	_, _, found, err := findEarnCrossMintOptIn(ctx, yieldDB, scope)
	if err != nil || !found {
		return "missing", err
	}
	return "stale", nil
}

func hasNonTerminalEarnCrossMintMovement(ctx context.Context, yieldDB *pgxpool.Pool, scope crossMintScope) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var has bool
	err := yieldDB.QueryRow(ctx, `SELECT EXISTS (SELECT 1
FROM loyal_yield.rebalance_decisions AS decisions
JOIN loyal_yield.managed_vaults AS vaults ON vaults.id = decisions.vault_id
WHERE vaults.settings = $1 AND vaults.vault_index = $2 AND vaults.vault_pubkey = $3
  AND decisions.movement_route = 'cross_mint_jupiter'
  AND decisions.terminal_outcome IS NULL
LIMIT 1)`, scope.Settings, scope.VaultIndex, scope.VaultPubkey).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("read cross-mint movement: %w", err)
	}
	return has, nil
}

func crossMintEnrollmentAfterTransition(ctx context.Context, yieldDB *pgxpool.Pool, scope crossMintScope) (*crossMintEnrollment, error) {
	state, err := findEarnCrossMintState(ctx, yieldDB, scope)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return nil, nil
	}
	return &crossMintEnrollment{
		BoundPolicies:              state.BoundPolicies,
		DailySourceMintSpendingCap: state.DailySourceMintSpendingCap,
		Enabled:                    state.Enabled,
		Generation:                 state.Generation,
		MaxSlippageBps:             state.MaxSlippageBps,
	}, nil
}

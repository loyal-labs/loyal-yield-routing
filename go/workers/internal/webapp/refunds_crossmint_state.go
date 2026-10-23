package webapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Ports web/apps/web/src/lib/yield-optimization/earn-cross-mint-repository.server.ts
// and earn-cross-mint-policy-index.shared.ts for the cross-mint toggle and
// delete-readiness routes.

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
	Bound crossMintBoundPolicy
	// LastSeen fields ride the projected policies, not the bound pair
	// (repository.server.ts:142-148).
	LastSeenSignature string
	LastSeenSlot      string
}

// crossMintState mirrors EarnCrossMintState; it is the repository's read
// model, not a wire type.
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

// crossMintPolicyRow mirrors EarnCrossMintPolicyProjectionRow for one
// cross_mint_swap_policies row; nullable columns stay pointers.
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

// crossMintPolicyIndex mirrors EarnCrossMintPolicyIndex.
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

// deriveCrossMintPolicyIndex ports deriveEarnCrossMintPolicyIndex
// (earn-cross-mint-policy-index.shared.ts:45-111), refusal for refusal.
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

	// candidates.sort(left => classic ? -1 : 1): shards are distinct here, so
	// this is the classic policy moving to the front.
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

// findEarnCrossMintSnapshot ports findEarnCrossMintSnapshot
// (repository.server.ts:93-166): nil autoswap when the opt-in row is missing
// or the policy index is not complete.
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

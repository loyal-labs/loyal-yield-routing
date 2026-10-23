package webapp

// POST /api/smart-accounts/yield-optimization/position/reconcile
// <- web/apps/web/src/app/api/smart-accounts/yield-optimization/position/reconcile/route.ts
// + L/earn-position-reconciliation.server.ts. Reads the managed vault with
// its active policy pair, then either answers from the five-minute
// reconciliation cache, or snapshots the vault's on-chain holdings through the
// RPC read and rewrites the current holdings tables inside one transaction.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnprep"
)

const stateReconcilePath = "/api/smart-accounts/yield-optimization/position/reconcile"

// stateReconcileCacheMS is RECONCILE_CACHE_MS
// (earn-position-reconciliation.server.ts:15).
const stateReconcileCacheMS = 5 * 60 * time.Second

type EarnPositionReconcileRequest struct {
	Force *bool `json:"force"`
}

type EarnPositionReconcileResponse struct {
	LastReconciledAt   *string `json:"lastReconciledAt"`
	LastReconciledSlot *string `json:"lastReconciledSlot"`
	PositionID         *string `json:"positionId"`
	Status             string  `json:"status"`
}

type stateReconcileRequest struct {
	SessionInput
	Raw rawJSON
}

type stateReconcileOutput struct {
	// Next's NextResponse.json sets no cache header on this route; no-store
	// is pinned because the write is per-wallet state.
	CacheControl string `header:"Cache-Control"`
	Body         EarnPositionReconcileResponse
}

func (h earnRoutes) registerReconcile(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "post-earn-position-reconcile",
		Method:      http.MethodPost,
		Path:        stateReconcilePath,
		Summary:     "Re-snapshot the Earn vault's on-chain holdings into the position tables",
		Responses:   errorResponses[routeError](api, 401, 409, 500),
	}, h.earnPositionReconcile)
	documentJSONBody[EarnPositionReconcileRequest](api, stateReconcilePath)
}

func (h earnRoutes) earnPositionReconcile(ctx context.Context, in *stateReconcileRequest) (*stateReconcileOutput, error) {
	account, err := h.account(ctx, in.SessionInput)
	if err != nil {
		return nil, earnUnavailable("earn-position-reconcile auth", err, "unauthenticated", "No active auth session.")
	}
	// request.json().catch(() => ({})): an unreadable body simply means
	// force stays false.
	var body EarnPositionReconcileRequest
	_ = json.Unmarshal(in.Raw.data, &body)

	result, err := h.reconcileEarnVaultPosition(ctx, account, body.Force != nil && *body.Force)
	if err != nil {
		slog.Warn("[earn-position] reconciliation failed", "error", err)
		return nil, earnError(http.StatusInternalServerError, "reconcile_failed", err.Error())
	}
	return &stateReconcileOutput{CacheControl: "no-store", Body: result}, nil
}

// stateReconcileMissing ports the two "missing" result literals
// (earn-position-reconciliation.server.ts:62-68 and :77-83).
func stateReconcileMissing(vault *stateManagedVaultRow) EarnPositionReconcileResponse {
	if vault == nil {
		return EarnPositionReconcileResponse{Status: "missing"}
	}
	return EarnPositionReconcileResponse{
		LastReconciledAt:   stateTimePtr(vault.LastReconciledAt),
		LastReconciledSlot: stateInt64Ptr(vault.LastReconciledSlot),
		Status:             "missing",
	}
}

// reconcileEarnVaultPosition ports reconcileEarnVaultPosition
// (earn-position-reconciliation.server.ts:49).
func (h earnRoutes) reconcileEarnVaultPosition(ctx context.Context, account EarnAccount, force bool) (EarnPositionReconcileResponse, error) {
	now := time.Now()
	vault, err := h.stateManagedVaultRow(ctx, account.Settings.String(), account.Vault.String())
	if err != nil {
		return EarnPositionReconcileResponse{}, err
	}
	var pair *RoutePolicyPair
	if vault != nil {
		pair, err = h.activePolicyPair(ctx, account)
		if err != nil {
			return EarnPositionReconcileResponse{}, err
		}
		if pair == nil {
			vault = nil
		}
	}
	if vault == nil {
		return stateReconcileMissing(nil), nil
	}

	position, err := h.reconciledActiveYieldPosition(ctx, account)
	if err != nil {
		return EarnPositionReconcileResponse{}, err
	}
	if position == nil {
		return stateReconcileMissing(vault), nil
	}
	if !force && vault.LastReconciledAt != nil && now.Sub(*vault.LastReconciledAt) < stateReconcileCacheMS {
		return EarnPositionReconcileResponse{
			LastReconciledAt:   stateTimePtr(vault.LastReconciledAt),
			LastReconciledSlot: stateInt64Ptr(vault.LastReconciledSlot),
			PositionID:         stateInt64Ptr(&position.ID),
			Status:             "cached",
		}, nil
	}

	if h.Chain == nil {
		return EarnPositionReconcileResponse{}, errors.New("the Solana read client is not configured.")
	}
	snapshot, err := earnprep.FetchWithdrawHoldingsSnapshot(ctx, h.Chain, earnprep.WithdrawHoldingsArgs{
		Cluster: string(account.Cluster),
		Now:     func() time.Time { return now },
		Policy: &earnprep.WithdrawPolicyUniverse{
			VaultIndex:           earnVaultIndex,
			StableMints:          pair.Route.StableMints,
			KaminoLiquidityMints: pair.Route.KaminoLiquidityMints,
			KaminoMarkets:        pair.Route.KaminoMarkets,
		},
		SettingsPda:                 account.Settings,
		RequireCompleteReserveReads: true,
	})
	if err != nil {
		return EarnPositionReconcileResponse{}, err
	}
	observedSlot := snapshot.ObservedSlot
	observedAt, err := time.Parse(time.RFC3339Nano, snapshot.ObservedAt)
	if err != nil {
		return EarnPositionReconcileResponse{}, err
	}
	positions, idleBalances, err := stateSnapshotBalances(snapshot.Holdings)
	if err != nil {
		return EarnPositionReconcileResponse{}, err
	}

	err = h.stateRecordReconciledSnapshot(ctx, stateReconciledSnapshot{
		VaultID:          vault.ID,
		PolicyID:         vault.ActivePolicyID,
		ObservedSlot:     int64(observedSlot),
		ObservedAt:       observedAt,
		Now:              now,
		AccountCount:     snapshot.Provenance.AccountCount,
		ChunkCount:       snapshot.Provenance.ChunkCount,
		Positions:        positions,
		IdleBalances:     idleBalances,
		SourceCommitment: "confirmed",
	})
	if err != nil {
		return EarnPositionReconcileResponse{}, err
	}

	reconciledAt := snapshot.ObservedAt
	slot := strconvFormatInt(int64(observedSlot))
	positionID := strconvFormatInt(position.ID)
	return EarnPositionReconcileResponse{
		LastReconciledAt:   &reconciledAt,
		LastReconciledSlot: &slot,
		PositionID:         &positionID,
		Status:             "refreshed",
	}, nil
}

// stateManagedVaultRow is the vault half of findActiveManagedYieldVaultWithPolicy
// (yield-deposit-repository.server.ts:3270): the active Earn vault, newest
// first. The policy half is the shared activePolicyPair read, whose filters
// match the route policy lookups exactly.
type stateManagedVaultRow struct {
	ID                 int64
	ActivePolicyID     int64
	LastReconciledAt   *time.Time
	LastReconciledSlot *int64
}

func (h earnRoutes) stateManagedVaultRow(ctx context.Context, settings, vaultPubkey string) (*stateManagedVaultRow, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var vault stateManagedVaultRow
	err := h.YieldDB.QueryRow(ctx, `SELECT id, active_policy_id, last_reconciled_at, last_reconciled_slot
FROM loyal_yield.managed_vaults
WHERE active = TRUE AND settings = $1 AND vault_index = $2 AND vault_pubkey = $3
ORDER BY last_seen_at DESC, id DESC
LIMIT 1`, settings, int16(earnVaultIndex), vaultPubkey).Scan(&vault.ID, &vault.ActivePolicyID,
		&vault.LastReconciledAt, &vault.LastReconciledSlot)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read managed yield vault: %w", err)
	}
	return &vault, nil
}

// stateSnapshotBalances ports the holdings mapping of
// reconcileEarnVaultPosition (:127-157): Kamino holdings become snapshot
// positions whose planning metadata is the RPC provenance plus the source id,
// and idle holdings become idle token balances that must carry their account
// identity.
func stateSnapshotBalances(holdings []earnprep.WithdrawHolding) ([]stateSnapshotPosition, []stateSnapshotIdleBalance, error) {
	var positions []stateSnapshotPosition
	var idle []stateSnapshotIdleBalance
	for _, holding := range holdings {
		metadata := make(map[string]any, len(holding.Provenance)+1)
		for key, value := range holding.Provenance {
			metadata[key] = value
		}
		metadata["sourceId"] = holding.SourceID
		switch holding.Kind {
		case "kamino":
			if holding.Reserve == nil {
				continue
			}
			market := statePubkeyOrNil(holding.Market)
			positions = append(positions, stateSnapshotPosition{
				Reserve:          holding.Reserve.String(),
				Market:           market,
				LiquidityMint:    holding.LiquidityMint.String(),
				AmountRaw:        int64(holding.AmountRaw),
				HasValue:         holding.AmountRaw > 0,
				PlanningMetadata: metadata,
			})
		case "idle":
			owner := holding.Provenance["owner"]
			tokenAccount := holding.Provenance["tokenAccount"]
			if owner == "" || tokenAccount == "" {
				return nil, nil, errors.New("Complete Earn idle holding lacks account identity.")
			}
			idle = append(idle, stateSnapshotIdleBalance{
				Mint:         holding.LiquidityMint.String(),
				AmountRaw:    int64(holding.AmountRaw),
				Owner:        owner,
				TokenAccount: tokenAccount,
			})
		}
	}
	return positions, idle, nil
}

func statePubkeyOrNil(key *solana.PublicKey) *string {
	if key == nil {
		return nil
	}
	s := key.String()
	return &s
}

// stateSnapshotPosition is one planned row for
// vault_position_snapshot_positions and vault_reserve_positions_current; the
// APY columns stay unset exactly as the RPC read leaves them unpriced.
type stateSnapshotPosition struct {
	Reserve          string
	Market           *string
	LiquidityMint    string
	AmountRaw        int64
	HasValue         bool
	PlanningMetadata map[string]any
}

// stateSnapshotIdleBalance is one planned row for
// vault_idle_token_balances_current.
type stateSnapshotIdleBalance struct {
	Mint         string
	AmountRaw    int64
	Owner        string
	TokenAccount string
}

// stateReconciledSnapshot carries recordReconciledYieldVaultSnapshot's input
// (yield-deposit-repository.server.ts:3413).
type stateReconciledSnapshot struct {
	VaultID          int64
	PolicyID         int64
	ObservedSlot     int64
	ObservedAt       time.Time
	Now              time.Time
	AccountCount     int
	ChunkCount       int
	Positions        []stateSnapshotPosition
	IdleBalances     []stateSnapshotIdleBalance
	SourceCommitment string
}

// stateRecordReconciledSnapshot ports recordReconciledYieldVaultSnapshot. The
// Neon db.batch (writeCurrentVaultSnapshot :712) becomes one SQL transaction
// opened with the same advisory lock key, so a crashed writer leaves the
// previous snapshot published and a repeated writer serializes behind the
// lock; the staged snapshot is only published (is_current, context) after its
// positions and idle balances are written.
func (h earnRoutes) stateRecordReconciledSnapshot(ctx context.Context, in stateReconciledSnapshot) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := h.YieldDB.Begin(ctx)
	if err != nil {
		return fmt.Errorf("record reconciled yield vault snapshot: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('loyal_yield.vault_position_snapshots.is_current' || ':' || $1::bigint::text, 0))`, in.VaultID); err != nil {
		return fmt.Errorf("lock reconciled yield vault snapshot: %w", err)
	}

	idleTotal := int64(0)
	for _, balance := range in.IdleBalances {
		idleTotal += balance.AmountRaw
	}
	// pendingContext: the context without publication_scope, which only the
	// publishing update below restores.
	pendingContext, err := json.Marshal(map[string]any{
		"accountCount":                    in.AccountCount,
		"chunkCount":                      in.ChunkCount,
		"idle_vault_liquidity_amount_raw": strconvFormatInt(idleTotal),
		"purpose":                         "routine",
		"source":                          "earn_rpc_holdings",
		"sourceCommitment":                in.SourceCommitment,
	})
	if err != nil {
		return fmt.Errorf("record reconciled yield vault snapshot: %w", err)
	}
	fullContext, err := json.Marshal(map[string]any{
		"accountCount":                    in.AccountCount,
		"chunkCount":                      in.ChunkCount,
		"idle_vault_liquidity_amount_raw": strconvFormatInt(idleTotal),
		"publication_scope":               "complete_product_vault",
		"purpose":                         "routine",
		"source":                          "earn_rpc_holdings",
		"sourceCommitment":                in.SourceCommitment,
	})
	if err != nil {
		return fmt.Errorf("record reconciled yield vault snapshot: %w", err)
	}

	var snapshotID int64
	if err := tx.QueryRow(ctx, `INSERT INTO loyal_yield.vault_position_snapshots (
  vault_id, policy_id, observed_slot, observed_at, chain_slot, is_current, context)
VALUES ($1, $2, $3, $4, $5, FALSE, $6)
RETURNING id`, in.VaultID, in.PolicyID, in.ObservedSlot, in.ObservedAt, in.ObservedSlot, pendingContext).Scan(&snapshotID); err != nil {
		return fmt.Errorf("record reconciled yield vault snapshot: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE loyal_yield.vault_position_snapshots
SET is_current = FALSE
WHERE vault_id = $1 AND is_current`, in.VaultID); err != nil {
		return fmt.Errorf("record reconciled yield vault snapshot: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM loyal_yield.vault_reserve_positions_current WHERE vault_id = $1`, in.VaultID); err != nil {
		return fmt.Errorf("record reconciled yield vault snapshot: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM loyal_yield.vault_idle_token_balances_current WHERE vault_id = $1`, in.VaultID); err != nil {
		return fmt.Errorf("record reconciled yield vault snapshot: %w", err)
	}

	for _, position := range in.Positions {
		if position.AmountRaw <= 0 {
			continue
		}
		metadata, err := json.Marshal(mergeStateSnapshotSource(position.PlanningMetadata))
		if err != nil {
			return fmt.Errorf("record reconciled yield vault snapshot: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.vault_position_snapshot_positions (
  snapshot_id, reserve, market, liquidity_mint, amount_raw, supply_apy_bps,
  borrow_apy_bps, has_value, planning_metadata)
VALUES ($1, $2, $3, $4, $5, NULL, NULL, $6, $7)`, snapshotID, position.Reserve, position.Market,
			position.LiquidityMint, position.AmountRaw, position.HasValue, metadata); err != nil {
			return fmt.Errorf("record reconciled yield vault snapshot: %w", err)
		}
	}
	for _, position := range in.Positions {
		metadata, err := json.Marshal(mergeStateSnapshotSource(position.PlanningMetadata))
		if err != nil {
			return fmt.Errorf("record reconciled yield vault snapshot: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.vault_reserve_positions_current (
  vault_id, reserve, market, liquidity_mint, amount_raw, has_value,
  supply_apy_bps, borrow_apy_bps, snapshot_id, observed_slot, observed_at,
  planning_metadata)
VALUES ($1, $2, $3, $4, $5, $6, NULL, NULL, $7, $8, $9, $10)`, in.VaultID, position.Reserve, position.Market,
			position.LiquidityMint, position.AmountRaw, position.HasValue, snapshotID,
			in.ObservedSlot, in.ObservedAt, metadata); err != nil {
			return fmt.Errorf("record reconciled yield vault snapshot: %w", err)
		}
	}
	for _, balance := range in.IdleBalances {
		if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.vault_idle_token_balances_current (
  vault_id, mint, amount_raw, owner, token_account, observed_slot, observed_at,
  source_commitment, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			in.VaultID, balance.Mint, balance.AmountRaw, balance.Owner, balance.TokenAccount,
			in.ObservedSlot, in.ObservedAt, in.SourceCommitment, in.Now); err != nil {
			return fmt.Errorf("record reconciled yield vault snapshot: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE loyal_yield.vault_position_snapshots
SET context = $2, is_current = TRUE
WHERE id = $1`, snapshotID, fullContext); err != nil {
		return fmt.Errorf("record reconciled yield vault snapshot: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE loyal_yield.managed_vaults
SET last_reconciled_at = $2, last_reconciled_slot = $3, last_seen_at = $4
WHERE id = $1`, in.VaultID, in.ObservedAt, in.ObservedSlot, in.Now); err != nil {
		return fmt.Errorf("record reconciled yield vault snapshot: %w", err)
	}
	return tx.Commit(ctx)
}

// mergeStateSnapshotSource ports snapshotPositionValues' planning metadata
// spread: the recorder's own source first, the RPC provenance over it.
func mergeStateSnapshotSource(metadata map[string]any) map[string]any {
	merged := make(map[string]any, len(metadata)+1)
	merged["source"] = "frontend_position_reconcile"
	for key, value := range metadata {
		merged[key] = value
	}
	return merged
}

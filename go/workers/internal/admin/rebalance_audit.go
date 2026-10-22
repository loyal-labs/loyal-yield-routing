package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type rebalanceAuditCursor struct {
	CreatedAt  string `json:"createdAt"`
	ID         string `json:"id"`
	SourceRank int64  `json:"sourceRank"`
}

var rebalanceDigits = regexp.MustCompile(`^\d+$`)

func rebalanceDecodeAuditCursor(value string) *rebalanceAuditCursor {
	if value == "" {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(value, "="))
	if err != nil {
		return nil
	}
	var decoded struct {
		CreatedAt  any `json:"createdAt"`
		ID         any `json:"id"`
		SourceRank any `json:"sourceRank"`
	}
	if json.Unmarshal(raw, &decoded) != nil {
		return nil
	}
	createdAt, ok := decoded.CreatedAt.(string)
	if !ok {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil
	}
	id, ok := decoded.ID.(string)
	if !ok || !rebalanceDigits.MatchString(id) {
		return nil
	}
	rank := 1.0
	if decoded.SourceRank != nil {
		if rank, ok = decoded.SourceRank.(float64); !ok {
			return nil
		}
	}
	if rank != math.Trunc(rank) || rank < 1 || rank > 3 {
		return nil
	}
	return &rebalanceAuditCursor{CreatedAt: rebalanceISOTime(parsed), ID: id, SourceRank: int64(rank)}
}

func (c rebalanceAuditCursor) encode() string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func rebalanceAuditRangePredicate(auditRange string) string {
	switch auditRange {
	case "24h":
		return "audit.event_at >= now() - interval '24 hours'"
	case "7d":
		return "audit.event_at >= now() - interval '7 days'"
	case "30d":
		return "audit.event_at >= now() - interval '30 days'"
	}
	return "TRUE"
}

func rebalanceAuditRouteModePredicate(mode RebalanceRouteMode) string {
	return `(
    audit.record_type <> 'decision'
    OR audit.movement_source <> 'rebalance'
    OR audit.execution_kind = '` + mode.executionKind() + `'
  )`
}

const (
	rebalanceAuditCompletedRebalances = `
    audit.record_type = 'decision'
    AND audit.movement_source = 'rebalance'
    AND audit.status = 'confirmed'
  `
	rebalanceAuditCompletedDeposits = `
    (
      audit.record_type = 'yield_deposit'
      AND audit.status = 'confirmed'
    )
    OR (
      audit.record_type = 'decision'
      AND audit.movement_source = 'idle_vault_deposit'
      AND audit.is_idle_fallback
      AND audit.status = 'confirmed'
    )
  `
	rebalanceAuditRebalanceErrors = `
    audit.record_type = 'decision'
    AND audit.movement_source = 'rebalance'
    AND audit.status IN ('failed', 'abandoned')
  `
	rebalanceAuditDepositErrors = `
    (
      audit.record_type = 'decision'
      AND audit.movement_source = 'idle_vault_deposit'
      AND audit.status IN ('failed', 'abandoned')
    )
    OR (
      audit.record_type = 'execution'
      AND audit.movement_source = 'autodeposit'
      AND audit.status = 'failed'
    )
  `
	rebalanceAuditNeedsReview = `
    audit.record_type = 'decision'
    AND (audit.movement_source = 'needs_review' OR audit.record_type = 'decision' AND audit.status NOT IN (
    'planned', 'simulating', 'ready', 'submitted', 'confirming',
    'confirmed', 'failed', 'abandoned', 'skipped'
  ))
  `
	rebalanceAuditActiveStatus = `audit.record_type = 'decision' AND audit.status IN (
  'planned', 'simulating', 'ready', 'submitted', 'confirming'
)`
)

func rebalanceAuditViewPredicate(view, errorFilter string) string {
	switch view {
	case "completed_rebalances":
		return rebalanceAuditCompletedRebalances
	case "completed_deposits":
		return rebalanceAuditCompletedDeposits
	}
	switch errorFilter {
	case "rebalance":
		return rebalanceAuditRebalanceErrors
	case "deposit":
		return rebalanceAuditDepositErrors
	case "needs_review":
		return rebalanceAuditNeedsReview
	}
	return "(" + rebalanceAuditRebalanceErrors + " OR " + rebalanceAuditDepositErrors + " OR " + rebalanceAuditNeedsReview + ")"
}

type RebalanceAuditRow struct {
	AbandonReason       *string `json:"abandonReason"`
	AmountRaw           *string `json:"amountRaw"`
	ConfirmedSlot       *string `json:"confirmedSlot"`
	CreatedAt           string  `json:"createdAt"`
	DecisionReason      *string `json:"decisionReason"`
	EstimatedEdgeBps    *int64  `json:"estimatedEdgeBps"`
	ID                  string  `json:"id"`
	Lane                string  `json:"lane" enum:"rebalance,deposit,needs_review"`
	LiquidityMint       *string `json:"liquidityMint"`
	MovementKind        *string `json:"movementKind"`
	Source              string  `json:"source" enum:"autodeposit,idle_vault_deposit,manual_deposit,needs_review,rebalance"`
	SourceLiquidityMint *string `json:"sourceLiquidityMint"`
	Signature           *string `json:"signature"`
	SecondarySignature  *string `json:"secondarySignature"`
	SourceApyBps        *int64  `json:"sourceApyBps"`
	SourceReserve       *string `json:"sourceReserve"`
	Status              string  `json:"status"`
	SubmittedSlot       *string `json:"submittedSlot"`
	TargetApyBps        *int64  `json:"targetApyBps"`
	TargetLiquidityMint *string `json:"targetLiquidityMint"`
	TargetReserve       *string `json:"targetReserve"`
	UpdatedAt           string  `json:"updatedAt"`
	VaultID             *string `json:"vaultId"`
	VaultIndex          *int64  `json:"vaultIndex"`
	VaultPubkey         *string `json:"vaultPubkey"`
}

type RebalanceAuditPage struct {
	NextCursor *string             `json:"nextCursor"`
	Rows       []RebalanceAuditRow `json:"rows"`
}

type rebalanceAuditSQLRow struct {
	RecordType          string     `db:"record_type"`
	ID                  string     `db:"id"`
	SortID              int64      `db:"sort_id"`
	SortSource          int32      `db:"sort_source"`
	EventAt             *time.Time `db:"event_at"`
	CreatedAt           *time.Time `db:"created_at"`
	UpdatedAt           *time.Time `db:"updated_at"`
	Lane                *string    `db:"lane"`
	MovementSource      *string    `db:"movement_source"`
	ExecutionKind       *string    `db:"execution_kind"`
	LiquidityMint       *string    `db:"liquidity_mint"`
	SourceLiquidityMint *string    `db:"source_liquidity_mint"`
	TargetLiquidityMint *string    `db:"target_liquidity_mint"`
	Status              string     `db:"status"`
	AbandonReason       *string    `db:"abandon_reason"`
	AmountRaw           *int64     `db:"amount_raw"`
	ConfirmedSlot       *int64     `db:"confirmed_slot"`
	SubmittedSlot       *int64     `db:"submitted_slot"`
	DecisionReason      *string    `db:"decision_reason"`
	EstimatedEdgeBps    *int64     `db:"estimated_edge_bps"`
	SourceApyBps        *int64     `db:"source_apy_bps"`
	SourceReserve       *string    `db:"source_reserve"`
	TargetApyBps        *int64     `db:"target_apy_bps"`
	TargetReserve       *string    `db:"target_reserve"`
	Signature           *string    `db:"signature"`
	SecondarySignature  *string    `db:"secondary_signature"`
	VaultID             *string    `db:"vault_id"`
	VaultIndex          *string    `db:"vault_index"`
	VaultPubkey         *string    `db:"vault_pubkey"`
	IsIdleFallback      *bool      `db:"is_idle_fallback"`
}

func rebalanceAuditLane(value *string) string {
	if value != nil && (*value == "rebalance" || *value == "deposit") {
		return *value
	}
	return "needs_review"
}

func rebalanceAuditSource(value *string) string {
	if value != nil {
		switch *value {
		case "autodeposit", "idle_vault_deposit", "manual_deposit", "rebalance":
			return *value
		}
	}
	return "needs_review"
}

func (row rebalanceAuditSQLRow) wire() RebalanceAuditRow {
	return RebalanceAuditRow{
		AbandonReason:       row.AbandonReason,
		AmountRaw:           rebalanceBigText(row.AmountRaw),
		ConfirmedSlot:       rebalanceBigText(row.ConfirmedSlot),
		CreatedAt:           rebalanceISOTimeOrEmpty(row.CreatedAt),
		DecisionReason:      row.DecisionReason,
		EstimatedEdgeBps:    row.EstimatedEdgeBps,
		ID:                  row.ID,
		Lane:                rebalanceAuditLane(row.Lane),
		LiquidityMint:       row.LiquidityMint,
		MovementKind:        row.ExecutionKind,
		Source:              rebalanceAuditSource(row.MovementSource),
		SourceLiquidityMint: row.SourceLiquidityMint,
		Signature:           row.Signature,
		SecondarySignature:  row.SecondarySignature,
		SourceApyBps:        row.SourceApyBps,
		SourceReserve:       row.SourceReserve,
		Status:              row.Status,
		SubmittedSlot:       rebalanceBigText(row.SubmittedSlot),
		TargetApyBps:        row.TargetApyBps,
		TargetLiquidityMint: row.TargetLiquidityMint,
		TargetReserve:       row.TargetReserve,
		UpdatedAt:           rebalanceISOTimeOrEmpty(row.UpdatedAt),
		VaultID:             row.VaultID,
		VaultIndex:          rebalanceIntPtr(row.VaultIndex),
		VaultPubkey:         row.VaultPubkey,
	}
}

const rebalanceAuditPageLimit = 25

func rebalanceAuditPage(ctx context.Context, d *Deps, predicate, auditRange string, mode RebalanceRouteMode, cursor *rebalanceAuditCursor) (RebalanceAuditPage, error) {
	query := rebalanceSQLAuditRowsCTE + `
      SELECT
        audit.*
      FROM audit_rows AS audit
      WHERE ` + rebalanceAuditRangePredicate(auditRange) + `
        AND ` + rebalanceAuditRouteModePredicate(mode) + `
        AND ` + predicate
	args := []any{}
	if cursor != nil {
		query += `
        AND (audit.event_at, audit.sort_source, audit.sort_id) < ($1::timestamptz, $2::integer, $3::bigint)`
		args = append(args, cursor.CreatedAt, cursor.SourceRank, cursor.ID)
	}
	query += `
      ORDER BY audit.event_at DESC, audit.sort_source DESC, audit.sort_id DESC
      LIMIT ` + strconv.Itoa(rebalanceAuditPageLimit+1)
	rows, err := rebalanceRows[rebalanceAuditSQLRow](ctx, d, query, args...)
	if err != nil {
		return RebalanceAuditPage{}, err
	}
	page := RebalanceAuditPage{Rows: []RebalanceAuditRow{}}
	if len(rows) > rebalanceAuditPageLimit {
		rows = rows[:rebalanceAuditPageLimit]
		last := rows[len(rows)-1]
		next := rebalanceAuditCursor{CreatedAt: rebalanceISOTimeOrEmpty(last.EventAt), ID: strconv.FormatInt(last.SortID, 10), SourceRank: int64(last.SortSource)}.encode()
		page.NextCursor = &next
	}
	for _, row := range rows {
		page.Rows = append(page.Rows, row.wire())
	}
	return page, nil
}

type RebalanceAuditSummary struct {
	Active              int64 `json:"active"`
	CompletedDeposits   int64 `json:"completedDeposits"`
	CompletedRebalances int64 `json:"completedRebalances"`
	DepositErrors       int64 `json:"depositErrors"`
	Errors              int64 `json:"errors"`
	NeedsReview         int64 `json:"needsReview"`
	RebalanceErrors     int64 `json:"rebalanceErrors"`
	StaleActive         int64 `json:"staleActive"`
}

func rebalanceAuditSummary(ctx context.Context, d *Deps, auditRange string, mode RebalanceRouteMode) (RebalanceAuditSummary, error) {
	query := rebalanceSQLAuditRowsCTE + rebalanceSQLAuditSummary + `
      WHERE ` + rebalanceAuditRangePredicate(auditRange) + `
        AND ` + rebalanceAuditRouteModePredicate(mode)
	rows, err := rebalanceRows[struct {
		CompletedRebalances string `db:"completed_rebalances"`
		CompletedDeposits   string `db:"completed_deposits"`
		RebalanceErrors     string `db:"rebalance_errors"`
		DepositErrors       string `db:"deposit_errors"`
		Errors              string `db:"errors"`
		NeedsReview         string `db:"needs_review"`
		Active              string `db:"active"`
		StaleActive         string `db:"stale_active"`
	}](ctx, d, query)
	if err != nil || len(rows) == 0 {
		return RebalanceAuditSummary{}, err
	}
	row := rows[0]
	return RebalanceAuditSummary{
		Active:              rebalanceInt(row.Active),
		CompletedDeposits:   rebalanceInt(row.CompletedDeposits),
		CompletedRebalances: rebalanceInt(row.CompletedRebalances),
		DepositErrors:       rebalanceInt(row.DepositErrors),
		Errors:              rebalanceInt(row.Errors),
		NeedsReview:         rebalanceInt(row.NeedsReview),
		RebalanceErrors:     rebalanceInt(row.RebalanceErrors),
		StaleActive:         rebalanceInt(row.StaleActive),
	}, nil
}

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

type auditCursor struct {
	CreatedAt  string `json:"createdAt"`
	ID         string `json:"id"`
	SourceRank int64  `json:"sourceRank"`
}

var digits = regexp.MustCompile(`^\d+$`)

func decodeAuditCursor(value string) *auditCursor {
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
	if !ok || !digits.MatchString(id) {
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
	return &auditCursor{CreatedAt: isoTime(parsed), ID: id, SourceRank: int64(rank)}
}

func (c auditCursor) encode() string {
	raw, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func auditRangePredicate(auditRange string) string {
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

func auditRouteModePredicate(mode RouteMode) string {
	return `(
    audit.record_type <> 'decision'
    OR audit.movement_source <> 'rebalance'
    OR audit.execution_kind = '` + mode.executionKind() + `'
  )`
}

const (
	auditCompletedRebalances = `
    audit.record_type = 'decision'
    AND audit.movement_source = 'rebalance'
    AND audit.status = 'confirmed'
  `
	auditCompletedDeposits = `
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
	auditRebalanceErrors = `
    audit.record_type = 'decision'
    AND audit.movement_source = 'rebalance'
    AND audit.status IN ('failed', 'abandoned')
  `
	auditDepositErrors = `
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
	auditNeedsReview = `
    audit.record_type = 'decision'
    AND (audit.movement_source = 'needs_review' OR audit.record_type = 'decision' AND audit.status NOT IN (
    'planned', 'simulating', 'ready', 'submitted', 'confirming',
    'confirmed', 'failed', 'abandoned', 'skipped'
  ))
  `
	auditActiveStatus = `audit.record_type = 'decision' AND audit.status IN (
  'planned', 'simulating', 'ready', 'submitted', 'confirming'
)`
)

func auditViewPredicate(view, errorFilter string) string {
	switch view {
	case "completed_rebalances":
		return auditCompletedRebalances
	case "completed_deposits":
		return auditCompletedDeposits
	}
	switch errorFilter {
	case "rebalance":
		return auditRebalanceErrors
	case "deposit":
		return auditDepositErrors
	case "needs_review":
		return auditNeedsReview
	}
	return "(" + auditRebalanceErrors + " OR " + auditDepositErrors + " OR " + auditNeedsReview + ")"
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

type auditSQLRow struct {
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

func auditLane(value *string) string {
	if value != nil && (*value == "rebalance" || *value == "deposit") {
		return *value
	}
	return "needs_review"
}

func auditSource(value *string) string {
	if value != nil {
		switch *value {
		case "autodeposit", "idle_vault_deposit", "manual_deposit", "rebalance":
			return *value
		}
	}
	return "needs_review"
}

func (row auditSQLRow) wire() RebalanceAuditRow {
	return RebalanceAuditRow{
		AbandonReason:       row.AbandonReason,
		AmountRaw:           bigText(row.AmountRaw),
		ConfirmedSlot:       bigText(row.ConfirmedSlot),
		CreatedAt:           isoTimeOrEmpty(row.CreatedAt),
		DecisionReason:      row.DecisionReason,
		EstimatedEdgeBps:    row.EstimatedEdgeBps,
		ID:                  row.ID,
		Lane:                auditLane(row.Lane),
		LiquidityMint:       row.LiquidityMint,
		MovementKind:        row.ExecutionKind,
		Source:              auditSource(row.MovementSource),
		SourceLiquidityMint: row.SourceLiquidityMint,
		Signature:           row.Signature,
		SecondarySignature:  row.SecondarySignature,
		SourceApyBps:        row.SourceApyBps,
		SourceReserve:       row.SourceReserve,
		Status:              row.Status,
		SubmittedSlot:       bigText(row.SubmittedSlot),
		TargetApyBps:        row.TargetApyBps,
		TargetLiquidityMint: row.TargetLiquidityMint,
		TargetReserve:       row.TargetReserve,
		UpdatedAt:           isoTimeOrEmpty(row.UpdatedAt),
		VaultID:             row.VaultID,
		VaultIndex:          intTextPtr(row.VaultIndex),
		VaultPubkey:         row.VaultPubkey,
	}
}

const auditPageLimit = 25

func rebalanceAuditPage(ctx context.Context, d *Deps, predicate, auditRange string, mode RouteMode, cursor *auditCursor) (RebalanceAuditPage, error) {
	query := sqlAuditRowsCTE + `
      SELECT
        audit.*
      FROM audit_rows AS audit
      WHERE ` + auditRangePredicate(auditRange) + `
        AND ` + auditRouteModePredicate(mode) + `
        AND ` + predicate
	args := []any{}
	if cursor != nil {
		query += `
        AND (audit.event_at, audit.sort_source, audit.sort_id) < ($1::timestamptz, $2::integer, $3::bigint)`
		args = append(args, cursor.CreatedAt, cursor.SourceRank, cursor.ID)
	}
	query += `
      ORDER BY audit.event_at DESC, audit.sort_source DESC, audit.sort_id DESC
      LIMIT ` + strconv.Itoa(auditPageLimit+1)
	rows, err := yieldRows[auditSQLRow](ctx, d, query, args...)
	if err != nil {
		return RebalanceAuditPage{}, err
	}
	page := RebalanceAuditPage{Rows: []RebalanceAuditRow{}}
	if len(rows) > auditPageLimit {
		rows = rows[:auditPageLimit]
		last := rows[len(rows)-1]
		next := auditCursor{CreatedAt: isoTimeOrEmpty(last.EventAt), ID: strconv.FormatInt(last.SortID, 10), SourceRank: int64(last.SortSource)}.encode()
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

func rebalanceAuditSummary(ctx context.Context, d *Deps, auditRange string, mode RouteMode) (RebalanceAuditSummary, error) {
	query := sqlAuditRowsCTE + sqlAuditSummary + `
      WHERE ` + auditRangePredicate(auditRange) + `
        AND ` + auditRouteModePredicate(mode)
	rows, err := yieldRows[struct {
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
		Active:              intText(row.Active),
		CompletedDeposits:   intText(row.CompletedDeposits),
		CompletedRebalances: intText(row.CompletedRebalances),
		DepositErrors:       intText(row.DepositErrors),
		Errors:              intText(row.Errors),
		NeedsReview:         intText(row.NeedsReview),
		RebalanceErrors:     intText(row.RebalanceErrors),
		StaleActive:         intText(row.StaleActive),
	}, nil
}

package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type RouteMode string

const (
	SameMint  RouteMode = "same_mint"
	CrossMint RouteMode = "cross_mint"
)

func routeModeOf(kind string) RouteMode {
	if kind == "cross_mint_jupiter" {
		return CrossMint
	}
	return SameMint
}

func (mode RouteMode) executionKind() string {
	if mode == CrossMint {
		return "cross_mint_jupiter"
	}
	return "same_mint"
}

func isoTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func isoTimeOrEmpty(t *time.Time) string {
	if t == nil {
		return ""
	}
	return isoTime(*t)
}

func isoTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := isoTime(*t)
	return &s
}

func intText(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func intTextPtr(s *string) *int64 {
	if s == nil || *s == "" {
		return nil
	}
	n := intText(*s)
	return &n
}

func bigText(n *int64) *string {
	if n == nil {
		return nil
	}
	s := strconv.FormatInt(*n, 10)
	return &s
}

type flexInt int64

func (n *flexInt) UnmarshalJSON(raw []byte) error {
	text := strings.Trim(string(raw), `"`)
	if text == "null" || text == "" {
		*n = 0
		return nil
	}
	v, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(text, 64)
		if ferr != nil {
			return err
		}
		v = int64(f)
	}
	*n = flexInt(v)
	return nil
}

type flexText string

func (s *flexText) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		*s = "0"
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		*s = flexText(text)
		return nil
	}
	*s = flexText(strings.TrimSpace(string(raw)))
	return nil
}

func yieldRows[T any](ctx context.Context, d *Deps, query string, args ...any) ([]T, error) {
	var out []T
	err := d.YieldRead(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowToStructByName[T])
		return err
	})
	if out == nil {
		out = []T{}
	}
	return out, err
}

type ActiveReserveRoute struct {
	ActiveAumRaw     string  `json:"activeAumRaw"`
	CurrentReserve   string  `json:"currentReserve"`
	LatestObservedAt *string `json:"latestObservedAt"`
	LiquidityMint    string  `json:"liquidityMint"`
	PositionCount    int64   `json:"positionCount"`
}

func activeReserveRoutes(ctx context.Context, d *Deps) ([]ActiveReserveRoute, error) {
	rows, err := yieldRows[struct {
		CurrentReserve   string     `db:"current_reserve"`
		LiquidityMint    string     `db:"liquidity_mint"`
		PositionCount    string     `db:"position_count"`
		ActiveAumRaw     string     `db:"active_aum_raw"`
		LatestObservedAt *time.Time `db:"latest_observed_at"`
	}](ctx, d, sqlActiveReserveRoutes)
	out := make([]ActiveReserveRoute, len(rows))
	for i, row := range rows {
		out[i] = ActiveReserveRoute{
			ActiveAumRaw:     row.ActiveAumRaw,
			CurrentReserve:   row.CurrentReserve,
			LatestObservedAt: isoTimePtr(row.LatestObservedAt),
			LiquidityMint:    row.LiquidityMint,
			PositionCount:    intText(row.PositionCount),
		}
	}
	return out, err
}

type RebalanceDecision struct {
	AbandonReason       *string   `json:"abandonReason"`
	AmountRaw           *string   `json:"amountRaw"`
	ConfirmedSlot       *string   `json:"confirmedSlot"`
	CreatedAt           string    `json:"createdAt"`
	DecisionReason      *string   `json:"decisionReason"`
	DecisionType        *string   `json:"decisionType" enum:"rebalance"`
	EstimatedEdgeBps    *int64    `json:"estimatedEdgeBps"`
	ID                  string    `json:"id"`
	LiquidityMint       *string   `json:"liquidityMint"`
	RouteMode           RouteMode `json:"routeMode" enum:"same_mint,cross_mint"`
	Signature           *string   `json:"signature"`
	SourceLiquidityMint *string   `json:"sourceLiquidityMint"`
	SourceApyBps        *int64    `json:"sourceApyBps"`
	SourceReserve       *string   `json:"sourceReserve"`
	Status              string    `json:"status"`
	TargetApyBps        *int64    `json:"targetApyBps"`
	TargetLiquidityMint *string   `json:"targetLiquidityMint"`
	TargetReserve       *string   `json:"targetReserve"`
	UpdatedAt           string    `json:"updatedAt"`
}

func recentRebalanceDecisions(ctx context.Context, d *Deps) ([]RebalanceDecision, error) {
	rows, err := yieldRows[struct {
		ID                  string    `db:"id"`
		Status              string    `db:"status"`
		SourceReserve       *string   `db:"source_reserve"`
		TargetReserve       *string   `db:"target_reserve"`
		LiquidityMint       *string   `db:"liquidity_mint"`
		SourceLiquidityMint *string   `db:"source_liquidity_mint"`
		TargetLiquidityMint *string   `db:"target_liquidity_mint"`
		AmountRaw           *string   `db:"amount_raw"`
		SourceApyBps        *string   `db:"source_apy_bps"`
		TargetApyBps        *string   `db:"target_apy_bps"`
		EstimatedEdgeBps    *string   `db:"estimated_edge_bps"`
		DecisionReason      *string   `db:"decision_reason"`
		AbandonReason       *string   `db:"abandon_reason"`
		Signature           *string   `db:"signature"`
		ConfirmedSlot       *string   `db:"confirmed_slot"`
		CreatedAt           time.Time `db:"created_at"`
		UpdatedAt           time.Time `db:"updated_at"`
		ExecutionKind       *string   `db:"execution_kind"`
		RouteMode           *string   `db:"route_mode"`
	}](ctx, d, sqlRecentDecisions)
	out := make([]RebalanceDecision, len(rows))
	for i, row := range rows {
		var decisionType *string
		if row.ExecutionKind != nil && (*row.ExecutionKind == "same_mint" || *row.ExecutionKind == "cross_mint_jupiter") {
			rebalance := "rebalance"
			decisionType = &rebalance
		}
		mode := ""
		if row.RouteMode != nil {
			mode = *row.RouteMode
		}
		out[i] = RebalanceDecision{
			AbandonReason:       row.AbandonReason,
			AmountRaw:           row.AmountRaw,
			ConfirmedSlot:       row.ConfirmedSlot,
			CreatedAt:           isoTime(row.CreatedAt),
			DecisionReason:      row.DecisionReason,
			DecisionType:        decisionType,
			EstimatedEdgeBps:    intTextPtr(row.EstimatedEdgeBps),
			ID:                  row.ID,
			LiquidityMint:       row.LiquidityMint,
			RouteMode:           routeModeOf(mode),
			Signature:           row.Signature,
			SourceLiquidityMint: row.SourceLiquidityMint,
			SourceApyBps:        intTextPtr(row.SourceApyBps),
			SourceReserve:       row.SourceReserve,
			Status:              row.Status,
			TargetApyBps:        intTextPtr(row.TargetApyBps),
			TargetLiquidityMint: row.TargetLiquidityMint,
			TargetReserve:       row.TargetReserve,
			UpdatedAt:           isoTime(row.UpdatedAt),
		}
	}
	return out, err
}

type RebalanceActivityPoint struct {
	BucketStartedAt     string    `json:"bucketStartedAt"`
	Confirmed           int64     `json:"confirmed"`
	FinalizedSwapLegs   int64     `json:"finalizedSwapLegs"`
	ExpiredSubmissions  int64     `json:"expiredSubmissions"`
	FailedDecisions     int64     `json:"failedDecisions"`
	FailedOpportunities int64     `json:"failedOpportunities"`
	FleetClaims         int64     `json:"fleetClaims"`
	MaxSwapFeeLamports  string    `json:"maxSwapFeeLamports"`
	RouteMode           RouteMode `json:"routeMode" enum:"same_mint,cross_mint"`
	SwapFeeLamports     string    `json:"swapFeeLamports"`
	TerminalAttempts    int64     `json:"terminalAttempts"`
}

func rebalanceActivity(ctx context.Context, d *Deps) ([]RebalanceActivityPoint, error) {
	rows, err := yieldRows[struct {
		RouteMode           string    `db:"route_mode"`
		BucketStartedAt     time.Time `db:"bucket_started_at"`
		Confirmed           string    `db:"confirmed"`
		TerminalAttempts    string    `db:"terminal_attempts"`
		FailedDecisions     string    `db:"failed_decisions"`
		FailedOpportunities string    `db:"failed_opportunities"`
		ExpiredSubmissions  string    `db:"expired_submissions"`
		FleetClaims         string    `db:"fleet_claims"`
		FinalizedSwapLegs   string    `db:"finalized_swap_legs"`
		SwapFeeLamports     string    `db:"swap_fee_lamports"`
		MaxSwapFeeLamports  string    `db:"max_swap_fee_lamports"`
	}](ctx, d, sqlRebalanceActivity)
	out := make([]RebalanceActivityPoint, len(rows))
	for i, row := range rows {
		out[i] = RebalanceActivityPoint{
			BucketStartedAt:     isoTime(row.BucketStartedAt),
			Confirmed:           intText(row.Confirmed),
			FinalizedSwapLegs:   intText(row.FinalizedSwapLegs),
			ExpiredSubmissions:  intText(row.ExpiredSubmissions),
			FailedDecisions:     intText(row.FailedDecisions),
			FailedOpportunities: intText(row.FailedOpportunities),
			FleetClaims:         intText(row.FleetClaims),
			MaxSwapFeeLamports:  row.MaxSwapFeeLamports,
			RouteMode:           routeModeOf(row.RouteMode),
			SwapFeeLamports:     row.SwapFeeLamports,
			TerminalAttempts:    intText(row.TerminalAttempts),
		}
	}
	return out, err
}

type autodepositPoint struct {
	accountNotFound       int64
	bucketStartedAt       string
	confirmationOrTimeout int64
	depositedAmountRaw    string
	insufficientRent      int64
	missingTokenDelegate  int64
	noLinkedError         int64
	otherPrePull          int64
	postPullKaminoTopUp   int64
	successful            int64
}

type autodepositRange struct {
	bucketHours int64
	key         string
	points      []autodepositPoint
}

func autodepositTimeSeries(ctx context.Context, d *Deps) ([]autodepositRange, error) {
	rows, err := yieldRows[struct {
		RangeKey              string    `db:"range_key"`
		BucketHours           string    `db:"bucket_hours"`
		BucketStartedAt       time.Time `db:"bucket_started_at"`
		Successful            string    `db:"successful"`
		DepositedAmountRaw    string    `db:"deposited_amount_raw"`
		AccountNotFound       string    `db:"account_not_found"`
		InsufficientRent      string    `db:"insufficient_rent"`
		MissingTokenDelegate  string    `db:"missing_token_delegate"`
		ConfirmationOrTimeout string    `db:"confirmation_or_timeout"`
		NoLinkedError         string    `db:"no_linked_error"`
		OtherPrePull          string    `db:"other_pre_pull"`
		PostPullKaminoTopUp   string    `db:"post_pull_kamino_top_up"`
	}](ctx, d, sqlAutodepositTimeSeries)
	ranges := []autodepositRange{{bucketHours: 2, key: "2d"}, {bucketHours: 6, key: "7d"}, {bucketHours: 24, key: "30d"}}
	for _, row := range rows {
		for i := range ranges {
			if ranges[i].key != row.RangeKey {
				continue
			}
			ranges[i].points = append(ranges[i].points, autodepositPoint{
				accountNotFound:       intText(row.AccountNotFound),
				bucketStartedAt:       isoTime(row.BucketStartedAt),
				confirmationOrTimeout: intText(row.ConfirmationOrTimeout),
				depositedAmountRaw:    row.DepositedAmountRaw,
				insufficientRent:      intText(row.InsufficientRent),
				missingTokenDelegate:  intText(row.MissingTokenDelegate),
				noLinkedError:         intText(row.NoLinkedError),
				otherPrePull:          intText(row.OtherPrePull),
				postPullKaminoTopUp:   intText(row.PostPullKaminoTopUp),
				successful:            intText(row.Successful),
			})
		}
	}
	return ranges, err
}

type OptimizationVolumePoint struct {
	ConfirmedCount      int64  `json:"confirmedCount"`
	CumulativeAmountRaw string `json:"cumulativeAmountRaw"`
	DailyAmountRaw      string `json:"dailyAmountRaw"`
	Date                string `json:"date"`
}

func optimizationVolumeSeries(ctx context.Context, d *Deps) ([]OptimizationVolumePoint, error) {
	rows, err := yieldRows[struct {
		Date                string `db:"date"`
		ConfirmedCount      string `db:"confirmed_count"`
		DailyAmountRaw      string `db:"daily_amount_raw"`
		CumulativeAmountRaw string `db:"cumulative_amount_raw"`
	}](ctx, d, sqlOptimizationVolume)
	out := make([]OptimizationVolumePoint, len(rows))
	for i, row := range rows {
		out[i] = OptimizationVolumePoint{
			ConfirmedCount:      intText(row.ConfirmedCount),
			CumulativeAmountRaw: row.CumulativeAmountRaw,
			DailyAmountRaw:      row.DailyAmountRaw,
			Date:                row.Date,
		}
	}
	return out, err
}

type Last30DaysRebalancePoint struct {
	Confirmed        int64     `json:"confirmed"`
	Date             string    `json:"date"`
	Failed           int64     `json:"failed"`
	RouteMode        RouteMode `json:"routeMode" enum:"same_mint,cross_mint"`
	TerminalAttempts int64     `json:"terminalAttempts"`
}

func last30DaysRebalanceSeries(ctx context.Context, d *Deps) ([]Last30DaysRebalancePoint, error) {
	rows, err := yieldRows[struct {
		RouteMode        string `db:"route_mode"`
		Date             string `db:"date"`
		Confirmed        string `db:"confirmed"`
		Failed           string `db:"failed"`
		TerminalAttempts string `db:"terminal_attempts"`
	}](ctx, d, sqlLast30DaysRebalances)
	out := make([]Last30DaysRebalancePoint, len(rows))
	for i, row := range rows {
		out[i] = Last30DaysRebalancePoint{
			Confirmed:        intText(row.Confirmed),
			Date:             row.Date,
			Failed:           intText(row.Failed),
			RouteMode:        routeModeOf(row.RouteMode),
			TerminalAttempts: intText(row.TerminalAttempts),
		}
	}
	return out, err
}

func jsonPayload(ctx context.Context, d *Deps, query string, args []any, into ...any) error {
	raws := make([][]byte, len(into))
	targets := make([]any, len(into))
	for i := range raws {
		targets[i] = &raws[i]
	}
	err := d.YieldRead(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, args...).Scan(targets...)
	})
	if err != nil {
		return err
	}
	for i, raw := range raws {
		if err := json.Unmarshal(raw, into[i]); err != nil {
			return fmt.Errorf("decode payload column %d: %w", i, err)
		}
	}
	return nil
}

type tuple []json.RawMessage

func (t tuple) text(i int) *string {
	if i >= len(t) || string(t[i]) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(t[i], &s) == nil {
		return &s
	}
	s = string(t[i])
	return &s
}

func (t tuple) str(i int) string {
	if s := t.text(i); s != nil {
		return *s
	}
	return ""
}

func (t tuple) big(i int) string {
	if s := t.text(i); s != nil && *s != "" {
		return *s
	}
	return "0"
}

func (t tuple) num(i int) int64 {
	var n flexInt
	if i < len(t) {
		_ = n.UnmarshalJSON(t[i])
	}
	return int64(n)
}

func (t tuple) iso(i int) string {
	s := t.text(i)
	if s == nil {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return *s
	}
	return isoTime(parsed)
}

type ExecutedRebalance struct {
	AmountRaw           string    `json:"amountRaw"`
	Authority           string    `json:"authority"`
	ConfirmedSlot       string    `json:"confirmedSlot"`
	CurrentDepositRaw   string    `json:"currentDepositRaw"`
	ExecutedAt          string    `json:"executedAt"`
	ID                  string    `json:"id"`
	LiquidityMint       *string   `json:"liquidityMint"`
	RouteMode           RouteMode `json:"routeMode" enum:"same_mint,cross_mint"`
	SourceReserve       string    `json:"sourceReserve"`
	SourceLiquidityMint *string   `json:"sourceLiquidityMint"`
	SwapFeeLamports     string    `json:"swapFeeLamports"`
	TargetReserve       string    `json:"targetReserve"`
	TargetLiquidityMint *string   `json:"targetLiquidityMint"`
	UserRank            int64     `json:"userRank"`
}

func executedFromTuple(t tuple) ExecutedRebalance {
	return ExecutedRebalance{
		AmountRaw:           t.big(0),
		Authority:           t.str(1),
		ConfirmedSlot:       t.big(2),
		CurrentDepositRaw:   t.big(3),
		ExecutedAt:          t.iso(4),
		ID:                  t.str(5),
		LiquidityMint:       t.text(6),
		RouteMode:           routeModeOf(t.str(7)),
		SourceReserve:       t.str(8),
		SourceLiquidityMint: t.text(9),
		SwapFeeLamports:     t.big(10),
		TargetReserve:       t.str(11),
		TargetLiquidityMint: t.text(12),
		UserRank:            t.num(13),
	}
}

type ExecutedRebalanceSummary struct {
	ExecutionCount         int64     `json:"executionCount"`
	ExecutionCount30d      int64     `json:"executionCount30d"`
	ExecutionCount7d       int64     `json:"executionCount7d"`
	FullyWithdrawnCount    int64     `json:"fullyWithdrawnCount"`
	FullyWithdrawnCount30d int64     `json:"fullyWithdrawnCount30d"`
	FullyWithdrawnCount7d  int64     `json:"fullyWithdrawnCount7d"`
	LiquidityMint          *string   `json:"liquidityMint"`
	RouteMode              RouteMode `json:"routeMode" enum:"same_mint,cross_mint"`
	SwapFeeLamports        string    `json:"swapFeeLamports"`
	UserCount              int64     `json:"userCount"`
}

type executedHistory struct {
	chartPoints []ExecutedRebalance
	details     []ExecutedRebalance
	generatedAt string
	summaries   []ExecutedRebalanceSummary
}

func executedRebalanceHistory(ctx context.Context, d *Deps, includeDetails bool) (executedHistory, error) {
	var summaries []struct {
		RouteMode              string   `json:"route_mode"`
		LiquidityMint          *string  `json:"liquidity_mint"`
		ExecutionCount         flexInt  `json:"execution_count"`
		ExecutionCount30d      flexInt  `json:"execution_count_30d"`
		ExecutionCount7d       flexInt  `json:"execution_count_7d"`
		FullyWithdrawnCount    flexInt  `json:"fully_withdrawn_count"`
		FullyWithdrawnCount30d flexInt  `json:"fully_withdrawn_count_30d"`
		FullyWithdrawnCount7d  flexInt  `json:"fully_withdrawn_count_7d"`
		UserCount              flexInt  `json:"user_count"`
		SwapFeeLamports        flexText `json:"swap_fee_lamports"`
	}
	var chart, details []tuple
	detailLimit := 0
	if includeDetails {
		detailLimit = 50
	}
	if err := jsonPayload(ctx, d, sqlExecutedHistory, []any{detailLimit}, &summaries, &chart, &details); err != nil {
		return executedHistory{}, err
	}
	history := executedHistory{
		chartPoints: make([]ExecutedRebalance, len(chart)),
		details:     make([]ExecutedRebalance, len(details)),
		generatedAt: isoTime(time.Now()),
		summaries:   make([]ExecutedRebalanceSummary, len(summaries)),
	}
	for i, row := range chart {
		history.chartPoints[i] = executedFromTuple(row)
	}
	for i, row := range details {
		history.details[i] = executedFromTuple(row)
	}
	for i, row := range summaries {
		history.summaries[i] = ExecutedRebalanceSummary{
			ExecutionCount:         int64(row.ExecutionCount),
			ExecutionCount30d:      int64(row.ExecutionCount30d),
			ExecutionCount7d:       int64(row.ExecutionCount7d),
			FullyWithdrawnCount:    int64(row.FullyWithdrawnCount),
			FullyWithdrawnCount30d: int64(row.FullyWithdrawnCount30d),
			FullyWithdrawnCount7d:  int64(row.FullyWithdrawnCount7d),
			LiquidityMint:          row.LiquidityMint,
			RouteMode:              routeModeOf(row.RouteMode),
			SwapFeeLamports:        string(row.SwapFeeLamports),
			UserCount:              int64(row.UserCount),
		}
	}
	return history, nil
}

type VaultRebalanceFrequency struct {
	AllCount          int64     `json:"allCount"`
	CurrentDepositRaw string    `json:"currentDepositRaw"`
	CurrentReserve    *string   `json:"currentReserve"`
	DepositRank       int64     `json:"depositRank"`
	Last12hCount      int64     `json:"last12hCount"`
	Last2hCount       int64     `json:"last2hCount"`
	Last7dCount       int64     `json:"last7dCount"`
	LiquidityMint     *string   `json:"liquidityMint"`
	PositionCount     int64     `json:"positionCount"`
	RouteMode         RouteMode `json:"routeMode" enum:"same_mint,cross_mint"`
	VaultID           string    `json:"vaultId"`
	VaultPubkey       string    `json:"vaultPubkey"`
}

func frequencyFromTuple(t tuple) VaultRebalanceFrequency {
	return VaultRebalanceFrequency{
		AllCount:          t.num(0),
		CurrentDepositRaw: t.big(1),
		CurrentReserve:    t.text(2),
		DepositRank:       t.num(3),
		Last12hCount:      t.num(4),
		Last2hCount:       t.num(5),
		Last7dCount:       t.num(6),
		LiquidityMint:     t.text(7),
		PositionCount:     t.num(8),
		RouteMode:         routeModeOf(t.str(9)),
		VaultID:           t.str(10),
		VaultPubkey:       t.str(11),
	}
}

type VaultRebalanceFrequencySummary struct {
	EligibleCount        int64     `json:"eligibleCount"`
	EligibleCount12h     int64     `json:"eligibleCount12h"`
	EligibleCount2h      int64     `json:"eligibleCount2h"`
	EligibleCount7d      int64     `json:"eligibleCount7d"`
	LiquidityMint        *string   `json:"liquidityMint"`
	PositionCount        int64     `json:"positionCount"`
	Rebalance12hCount    int64     `json:"rebalance12hCount"`
	Rebalance2hCount     int64     `json:"rebalance2hCount"`
	Rebalance7dCount     int64     `json:"rebalance7dCount"`
	RebalanceAllCount    int64     `json:"rebalanceAllCount"`
	RebalancedVaultCount int64     `json:"rebalancedVaultCount"`
	RouteMode            RouteMode `json:"routeMode" enum:"same_mint,cross_mint"`
	VaultCount           int64     `json:"vaultCount"`
}

type vaultFrequency struct {
	chartPoints []VaultRebalanceFrequency
	details     []VaultRebalanceFrequency
	generatedAt string
	summaries   []VaultRebalanceFrequencySummary
	vaultCount  int64
}

func vaultFrequencySQL(floor *string) string {
	gate := "FALSE"
	eligible := func(string, bool) string { return "TRUE" }
	if floor != nil {
		gate = "base.current_deposit_raw < " + *floor + "::bigint AND base.all_count = 0"
		eligible = func(column string, opportunity bool) string {
			clause := "(current_deposit_raw >= " + *floor + "::bigint OR " + column + " > 0"
			if opportunity {
				clause += " OR has_opportunity"
			}
			return clause + ")"
		}
	}
	return strings.NewReplacer(
		"{{opportunityGate}}", gate,
		"{{eligible:all_count:opportunity}}", eligible("all_count", true),
		"{{eligible:last_7d_count}}", eligible("last_7d_count", false),
		"{{eligible:last_12h_count}}", eligible("last_12h_count", false),
		"{{eligible:last_2h_count}}", eligible("last_2h_count", false),
	).Replace(sqlVaultFrequency)
}

func earnVaultRebalanceFrequency(ctx context.Context, d *Deps, floor *string, includeDetails bool) (vaultFrequency, error) {
	var summaries []struct {
		RouteMode            string  `json:"route_mode"`
		LiquidityMint        *string `json:"liquidity_mint"`
		VaultCount           flexInt `json:"vault_count"`
		EligibleCount        flexInt `json:"eligible_count"`
		EligibleCount7d      flexInt `json:"eligible_count_7d"`
		EligibleCount12h     flexInt `json:"eligible_count_12h"`
		EligibleCount2h      flexInt `json:"eligible_count_2h"`
		RebalancedVaultCount flexInt `json:"rebalanced_vault_count"`
		PositionCount        flexInt `json:"position_count"`
		RebalanceAllCount    flexInt `json:"rebalance_all_count"`
		Rebalance7dCount     flexInt `json:"rebalance_last_7d_count"`
		Rebalance12hCount    flexInt `json:"rebalance_last_12h_count"`
		Rebalance2hCount     flexInt `json:"rebalance_last_2h_count"`
	}
	var chart, details []tuple
	detailLimit := 0
	if includeDetails {
		detailLimit = 100
	}
	if err := jsonPayload(ctx, d, vaultFrequencySQL(floor), []any{detailLimit}, &summaries, &chart, &details); err != nil {
		return vaultFrequency{}, err
	}
	out := vaultFrequency{
		chartPoints: make([]VaultRebalanceFrequency, len(chart)),
		details:     make([]VaultRebalanceFrequency, len(details)),
		generatedAt: isoTime(time.Now()),
		summaries:   make([]VaultRebalanceFrequencySummary, len(summaries)),
	}
	for i, row := range chart {
		out.chartPoints[i] = frequencyFromTuple(row)
	}
	for i, row := range details {
		out.details[i] = frequencyFromTuple(row)
	}
	for i, row := range summaries {
		out.summaries[i] = VaultRebalanceFrequencySummary{
			EligibleCount:        int64(row.EligibleCount),
			EligibleCount12h:     int64(row.EligibleCount12h),
			EligibleCount2h:      int64(row.EligibleCount2h),
			EligibleCount7d:      int64(row.EligibleCount7d),
			LiquidityMint:        row.LiquidityMint,
			PositionCount:        int64(row.PositionCount),
			Rebalance12hCount:    int64(row.Rebalance12hCount),
			Rebalance2hCount:     int64(row.Rebalance2hCount),
			Rebalance7dCount:     int64(row.Rebalance7dCount),
			RebalanceAllCount:    int64(row.RebalanceAllCount),
			RebalancedVaultCount: int64(row.RebalancedVaultCount),
			RouteMode:            routeModeOf(row.RouteMode),
			VaultCount:           int64(row.VaultCount),
		}
		out.vaultCount += int64(row.VaultCount)
	}
	return out, nil
}

type VaultOpportunityCounts struct {
	AllCount     int64     `json:"allCount"`
	Last12hCount int64     `json:"last12hCount"`
	Last2hCount  int64     `json:"last2hCount"`
	Last7dCount  int64     `json:"last7dCount"`
	RouteMode    RouteMode `json:"routeMode" enum:"same_mint,cross_mint"`
	VaultID      string    `json:"vaultId"`
}

func earnVaultOpportunityCounts(ctx context.Context, d *Deps, vaultID string, mode RouteMode) (*VaultOpportunityCounts, error) {
	rows, err := yieldRows[struct {
		VaultID      string `db:"vault_id"`
		RouteMode    string `db:"route_mode"`
		AllCount     int32  `db:"all_count"`
		Last7dCount  int32  `db:"last_7d_count"`
		Last12hCount int32  `db:"last_12h_count"`
		Last2hCount  int32  `db:"last_2h_count"`
	}](ctx, d, sqlVaultOpportunityCounts, vaultID, mode.executionKind())
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	row := rows[0]
	return &VaultOpportunityCounts{
		AllCount:     int64(row.AllCount),
		Last12hCount: int64(row.Last12hCount),
		Last2hCount:  int64(row.Last2hCount),
		Last7dCount:  int64(row.Last7dCount),
		RouteMode:    routeModeOf(row.RouteMode),
		VaultID:      row.VaultID,
	}, nil
}

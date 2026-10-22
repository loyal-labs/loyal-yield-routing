package admin

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

func registerRebalance(m *Mux) {
	m.Router.Get("/api/earn/rebalance", m.rebalanceSection)
	m.Router.Get("/api/earn/rebalance/audit", m.rebalanceAudit)
	m.Router.Get("/api/earn/rebalance/details", m.rebalanceDetails)
	m.Router.Get("/api/earn/rebalance/vault-opportunities", m.rebalanceVaultOpportunities)
	m.Router.Get("/api/earn/rebalance/apy-samples", m.rebalanceApySamples)
}

func (m *Mux) rebalanceSection(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	section := query.Get("section")
	var (
		body any
		err  error
	)
	switch section {
	case "overview":
		body, err = m.rebalanceOverview(r)
	case "apy-history":
		body, err = m.rebalanceApyHistory(r)
	case "operations":
		body, err = rebalanceOperations(r, m.Deps)
	case "executions":
		body = map[string]any{"executedRebalances": rebalanceExecutedWire(r, m.Deps)}
	case "frequency":
		raw, present := query["eligibilityFloorRaw"]
		if !present || (raw[0] != "null" && !rebalanceDigits.MatchString(raw[0])) {
			err = huma.Error400BadRequest("eligibilityFloorRaw is required")
			break
		}
		var floor *string
		if raw[0] != "null" {
			floor = &raw[0]
		}
		body = map[string]any{"vaultRebalanceFrequency": rebalanceVaultFrequencyWire(r, m.Deps, floor)}
	case "":
		err = huma.Error400BadRequest("section is required")
	default:
		err = huma.Error400BadRequest("Unknown rebalance section: " + section)
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func (m *Mux) rebalanceOverview(r *http.Request) (any, error) {
	statuses, err := m.Deps.CurrentSafeReserveApyStatuses(r.Context())
	if err != nil {
		return nil, err
	}
	if statuses == nil {
		statuses = []SafeReserveApyStatusRow{}
	}
	routes, err := rebalanceActiveReserveRoutes(r.Context(), m.Deps)
	if err != nil {
		return nil, err
	}
	decisions, err := rebalanceRecentDecisions(r.Context(), m.Deps)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"decisions": decisions, "eligibilityFloorRaw": rebalanceEligibilityFloorRaw(statuses),
		"routes": routes, "statuses": statuses,
	}, nil
}

func (m *Mux) rebalanceApyHistory(r *http.Request) (any, error) {
	raw := r.URL.Query().Get("statuses")
	if raw == "" {
		return nil, huma.Error400BadRequest("statuses is required")
	}
	var statuses []SafeReserveApyStatusRow
	if err := json.Unmarshal([]byte(raw), &statuses); err != nil || statuses == nil {
		return nil, huma.Error400BadRequest("Invalid statuses payload")
	}
	data, err := m.Deps.SafeReserveApyMonitorDataFromStatuses(r.Context(), statuses, time.Now())
	if err != nil {
		return nil, err
	}
	return map[string]any{"apyData": rebalanceSerializeApyData(data)}, nil
}

func (m *Mux) rebalanceApySamples(w http.ResponseWriter, r *http.Request) {
	data, err := m.Deps.SafeReserveApyMonitorData(r.Context())
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func (m *Mux) rebalanceDetails(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	mint, filtered := query["liquidityMint"]
	if filtered {
		if _, ok := EarnStablecoinByMint(mint[0]); !ok {
			writeError(w, r, huma.Error400BadRequest("Invalid stablecoin mint."))
			return
		}
	}
	matches := func(mode RebalanceRouteMode, liquidityMint *string) bool {
		return !filtered || mode == rebalanceCrossMint || (liquidityMint != nil && *liquidityMint == mint[0])
	}
	switch query.Get("kind") {
	case "executed":
		history, err := rebalanceExecutedHistoryData(r.Context(), m.Deps, true)
		if err != nil {
			writeError(w, r, err)
			return
		}
		details := []RebalanceExecuted{}
		for _, row := range history.details {
			if matches(row.RouteMode, row.LiquidityMint) {
				details = append(details, row)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"details": details})
	case "frequency":
		frequency, err := rebalanceEarnVaultFrequency(r.Context(), m.Deps, nil, true)
		if err != nil {
			writeError(w, r, err)
			return
		}
		details := []RebalanceVaultFrequency{}
		for _, row := range frequency.details {
			if matches(row.RouteMode, row.LiquidityMint) {
				details = append(details, row)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"details": details})
	default:
		writeError(w, r, huma.Error400BadRequest("Invalid rebalance detail kind."))
	}
}

type rebalanceOperationsAutodepositPoint struct {
	AccountNotFound       int64  `json:"accountNotFound"`
	BucketStartedAt       string `json:"bucketStartedAt"`
	ConfirmationOrTimeout int64  `json:"confirmationOrTimeout"`
	InsufficientRent      int64  `json:"insufficientRent"`
	MissingTokenDelegate  int64  `json:"missingTokenDelegate"`
	NoLinkedError         int64  `json:"noLinkedError"`
	OtherPrePull          int64  `json:"otherPrePull"`
	PostPullKaminoTopUp   int64  `json:"postPullKaminoTopUp"`
}

type rebalanceOperationsAutodepositRange struct {
	BucketHours int64                                 `json:"bucketHours"`
	Key         string                                `json:"key"`
	Points      []rebalanceOperationsAutodepositPoint `json:"points"`
}

func rebalanceOperations(r *http.Request, d *Deps) (any, error) {
	activity, err := rebalanceActivity(r.Context(), d)
	if err != nil {
		return nil, err
	}
	last30, err := rebalanceLast30DaysSeries(r.Context(), d)
	if err != nil {
		return nil, err
	}
	ranges, err := rebalanceAutodepositTimeSeries(r.Context(), d)
	if err != nil {
		return nil, err
	}
	autodeposit := make([]rebalanceOperationsAutodepositRange, len(ranges))
	for i, rng := range ranges {
		points := make([]rebalanceOperationsAutodepositPoint, len(rng.points))
		for j, p := range rng.points {
			points[j] = rebalanceOperationsAutodepositPoint{
				AccountNotFound:       p.accountNotFound,
				BucketStartedAt:       p.bucketStartedAt,
				ConfirmationOrTimeout: p.confirmationOrTimeout,
				InsufficientRent:      p.insufficientRent,
				MissingTokenDelegate:  p.missingTokenDelegate,
				NoLinkedError:         p.noLinkedError,
				OtherPrePull:          p.otherPrePull,
				PostPullKaminoTopUp:   p.postPullKaminoTopUp,
			}
		}
		autodeposit[i] = rebalanceOperationsAutodepositRange{BucketHours: rng.bucketHours, Key: rng.key, Points: points}
	}
	return map[string]any{"activity": activity, "autodeposit": autodeposit, "last30DaysRebalances": last30}, nil
}

type rebalanceStringTable struct {
	strings []string
	index   map[string]int
}

func (t *rebalanceStringTable) of(value *string) *int {
	if value == nil {
		return nil
	}
	if i, ok := t.index[*value]; ok {
		return &i
	}
	i := len(t.strings)
	t.strings = append(t.strings, *value)
	t.index[*value] = i
	return &i
}

func (t *rebalanceStringTable) must(value string) int {
	return *t.of(&value)
}

func rebalanceExecutedWire(r *http.Request, d *Deps) map[string]any {
	history, err := rebalanceExecutedHistoryData(r.Context(), d, false)
	if err != nil {
		slog.Error("Executed Earn rebalance history query failed", "error", err)
		return map[string]any{
			"chartPoints": []any{}, "details": []any{}, "generatedAt": rebalanceISOTime(time.Now()),
			"status": "unavailable", "strings": []string{}, "summaries": []any{},
		}
	}
	table := &rebalanceStringTable{strings: []string{}, index: map[string]int{}}
	serialize := func(rows []RebalanceExecuted) [][]any {
		out := make([][]any, len(rows))
		for i, e := range rows {
			out[i] = []any{
				e.AmountRaw, table.must(e.Authority), e.ConfirmedSlot, e.CurrentDepositRaw, e.ExecutedAt, e.ID,
				table.of(e.LiquidityMint), e.RouteMode, table.must(e.SourceReserve), table.of(e.SourceLiquidityMint),
				e.SwapFeeLamports, table.must(e.TargetReserve), table.of(e.TargetLiquidityMint), e.UserRank,
			}
		}
		return out
	}
	chart := serialize(history.chartPoints)
	details := serialize(history.details)
	return map[string]any{
		"chartPoints": chart, "details": details, "generatedAt": history.generatedAt,
		"status": "available", "strings": table.strings, "summaries": history.summaries,
	}
}

func rebalanceVaultFrequencyWire(r *http.Request, d *Deps, floor *string) map[string]any {
	frequency, err := rebalanceEarnVaultFrequency(r.Context(), d, floor, false)
	if err != nil {
		slog.Error("Earn vault rebalance frequency query failed", "error", err)
		return map[string]any{
			"generatedAt": rebalanceISOTime(time.Now()), "status": "unavailable",
			"chartPoints": []any{}, "details": []any{}, "summaries": []any{}, "vaultCount": 0,
		}
	}
	serialize := func(rows []RebalanceVaultFrequency) [][]any {
		out := make([][]any, len(rows))
		for i, v := range rows {
			out[i] = []any{
				v.AllCount, v.CurrentDepositRaw, v.CurrentReserve, v.DepositRank, v.Last12hCount, v.Last2hCount,
				v.Last7dCount, v.LiquidityMint, v.PositionCount, v.RouteMode, v.VaultID, v.VaultPubkey,
			}
		}
		return out
	}
	return map[string]any{
		"chartPoints": serialize(frequency.chartPoints), "details": serialize(frequency.details),
		"generatedAt": frequency.generatedAt, "status": "available",
		"summaries": frequency.summaries, "vaultCount": frequency.vaultCount,
	}
}

func rebalanceQueryEnum(value, fallback string, allowed ...string) (string, bool) {
	if value == "" {
		return fallback, true
	}
	for _, a := range allowed {
		if value == a {
			return value, true
		}
	}
	return "", false
}

func (m *Mux) rebalanceAudit(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	view, okView := rebalanceQueryEnum(query.Get("view"), "errors", "completed_rebalances", "completed_deposits", "errors")
	auditRange, okRange := rebalanceQueryEnum(query.Get("range"), "24h", "24h", "7d", "30d", "all")
	mode, okMode := rebalanceQueryEnum(query.Get("routeMode"), "same_mint", "same_mint", "cross_mint")
	errorFilter, okFilter := rebalanceQueryEnum(query.Get("errorFilter"), "all", "all", "rebalance", "deposit", "needs_review")
	cursor := rebalanceDecodeAuditCursor(query.Get("cursor"))
	activeCursor := rebalanceDecodeAuditCursor(query.Get("activeCursor"))
	if !okView || !okRange || !okMode || !okFilter ||
		(query.Get("cursor") != "" && cursor == nil) || (query.Get("activeCursor") != "" && activeCursor == nil) {
		writeError(w, r, huma.Error400BadRequest("Invalid rebalance audit query."))
		return
	}
	routeMode := RebalanceRouteMode(mode)
	summary, err := rebalanceAuditSummary(r.Context(), m.Deps, auditRange, routeMode)
	if err != nil {
		writeError(w, r, err)
		return
	}
	page, err := rebalanceAuditPage(r.Context(), m.Deps, rebalanceAuditViewPredicate(view, errorFilter), auditRange, routeMode, cursor)
	if err != nil {
		writeError(w, r, err)
		return
	}
	activePage, err := rebalanceAuditPage(r.Context(), m.Deps, rebalanceAuditActiveStatus, auditRange, routeMode, activeCursor)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activePage": activePage, "generatedAt": rebalanceISOTime(time.Now()), "page": page, "summary": summary,
	})
}

func (m *Mux) rebalanceVaultOpportunities(w http.ResponseWriter, r *http.Request) {
	vaultID := r.URL.Query().Get("vaultId")
	mode := r.URL.Query().Get("routeMode")
	if !rebalanceDigits.MatchString(vaultID) || (mode != string(rebalanceSameMint) && mode != string(rebalanceCrossMint)) {
		writeError(w, r, huma.Error400BadRequest("Invalid vault opportunity query."))
		return
	}
	counts, err := rebalanceEarnVaultOpportunityCounts(r.Context(), m.Deps, vaultID, RebalanceRouteMode(mode))
	if err == nil && counts == nil {
		err = huma.Error404NotFound("Active Earn vault not found.")
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

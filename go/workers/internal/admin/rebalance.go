package admin

import (
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
		body = map[string]any{"executedRebalances": executedRebalancesWire(r, m.Deps)}
	case "frequency":
		raw, present := query["eligibilityFloorRaw"]
		if !present || (raw[0] != "null" && !digits.MatchString(raw[0])) {
			err = huma.Error400BadRequest("eligibilityFloorRaw is required")
			break
		}
		var floor *string
		if raw[0] != "null" {
			floor = &raw[0]
		}
		body = map[string]any{"vaultRebalanceFrequency": vaultFrequencyWire(r, m.Deps, floor)}
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

func (m *Mux) rebalanceOverview(*http.Request) (any, error) {
	return nil, Disabled("the reserve monitor")
}

func (m *Mux) rebalanceApyHistory(*http.Request) (any, error) {
	return nil, Disabled("the reserve monitor")
}

func (m *Mux) rebalanceApySamples(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, Disabled("the reserve monitor"))
}

func (m *Mux) rebalanceDetails(w http.ResponseWriter, r *http.Request) {
	writeError(w, r, Disabled("the reserve monitor"))
}

type operationsAutodepositPoint struct {
	AccountNotFound       int64  `json:"accountNotFound"`
	BucketStartedAt       string `json:"bucketStartedAt"`
	ConfirmationOrTimeout int64  `json:"confirmationOrTimeout"`
	InsufficientRent      int64  `json:"insufficientRent"`
	MissingTokenDelegate  int64  `json:"missingTokenDelegate"`
	NoLinkedError         int64  `json:"noLinkedError"`
	OtherPrePull          int64  `json:"otherPrePull"`
	PostPullKaminoTopUp   int64  `json:"postPullKaminoTopUp"`
}

type operationsAutodepositRange struct {
	BucketHours int64                        `json:"bucketHours"`
	Key         string                       `json:"key"`
	Points      []operationsAutodepositPoint `json:"points"`
}

func rebalanceOperations(r *http.Request, d *Deps) (any, error) {
	activity, err := rebalanceActivity(r.Context(), d)
	if err != nil {
		return nil, err
	}
	last30, err := last30DaysRebalanceSeries(r.Context(), d)
	if err != nil {
		return nil, err
	}
	ranges, err := autodepositTimeSeries(r.Context(), d)
	if err != nil {
		return nil, err
	}
	autodeposit := make([]operationsAutodepositRange, len(ranges))
	for i, rng := range ranges {
		points := make([]operationsAutodepositPoint, len(rng.points))
		for j, p := range rng.points {
			points[j] = operationsAutodepositPoint{
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
		autodeposit[i] = operationsAutodepositRange{BucketHours: rng.bucketHours, Key: rng.key, Points: points}
	}
	return map[string]any{"activity": activity, "autodeposit": autodeposit, "last30DaysRebalances": last30}, nil
}

type stringTable struct {
	strings []string
	index   map[string]int
}

func (t *stringTable) of(value *string) *int {
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

func (t *stringTable) must(value string) int {
	return *t.of(&value)
}

func executedRebalancesWire(r *http.Request, d *Deps) map[string]any {
	history, err := executedRebalanceHistory(r.Context(), d, false)
	if err != nil {
		slog.Error("Executed Earn rebalance history query failed", "error", err)
		return map[string]any{
			"chartPoints": []any{}, "details": []any{}, "generatedAt": isoTime(time.Now()),
			"status": "unavailable", "strings": []string{}, "summaries": []any{},
		}
	}
	table := &stringTable{strings: []string{}, index: map[string]int{}}
	serialize := func(rows []ExecutedRebalance) [][]any {
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

func vaultFrequencyWire(r *http.Request, d *Deps, floor *string) map[string]any {
	frequency, err := earnVaultRebalanceFrequency(r.Context(), d, floor, false)
	if err != nil {
		slog.Error("Earn vault rebalance frequency query failed", "error", err)
		return map[string]any{
			"generatedAt": isoTime(time.Now()), "status": "unavailable",
			"chartPoints": []any{}, "details": []any{}, "summaries": []any{}, "vaultCount": 0,
		}
	}
	serialize := func(rows []VaultRebalanceFrequency) [][]any {
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

func queryEnum(value, fallback string, allowed ...string) (string, bool) {
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
	view, okView := queryEnum(query.Get("view"), "errors", "completed_rebalances", "completed_deposits", "errors")
	auditRange, okRange := queryEnum(query.Get("range"), "24h", "24h", "7d", "30d", "all")
	mode, okMode := queryEnum(query.Get("routeMode"), "same_mint", "same_mint", "cross_mint")
	errorFilter, okFilter := queryEnum(query.Get("errorFilter"), "all", "all", "rebalance", "deposit", "needs_review")
	cursor := decodeAuditCursor(query.Get("cursor"))
	activeCursor := decodeAuditCursor(query.Get("activeCursor"))
	if !okView || !okRange || !okMode || !okFilter ||
		(query.Get("cursor") != "" && cursor == nil) || (query.Get("activeCursor") != "" && activeCursor == nil) {
		writeError(w, r, huma.Error400BadRequest("Invalid rebalance audit query."))
		return
	}
	routeMode := RouteMode(mode)
	summary, err := rebalanceAuditSummary(r.Context(), m.Deps, auditRange, routeMode)
	if err != nil {
		writeError(w, r, err)
		return
	}
	page, err := rebalanceAuditPage(r.Context(), m.Deps, auditViewPredicate(view, errorFilter), auditRange, routeMode, cursor)
	if err != nil {
		writeError(w, r, err)
		return
	}
	activePage, err := rebalanceAuditPage(r.Context(), m.Deps, auditActiveStatus, auditRange, routeMode, activeCursor)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"activePage": activePage, "generatedAt": isoTime(time.Now()), "page": page, "summary": summary,
	})
}

func (m *Mux) rebalanceVaultOpportunities(w http.ResponseWriter, r *http.Request) {
	vaultID := r.URL.Query().Get("vaultId")
	mode := r.URL.Query().Get("routeMode")
	if !digits.MatchString(vaultID) || (mode != string(SameMint) && mode != string(CrossMint)) {
		writeError(w, r, huma.Error400BadRequest("Invalid vault opportunity query."))
		return
	}
	counts, err := earnVaultOpportunityCounts(r.Context(), m.Deps, vaultID, RouteMode(mode))
	if err == nil && counts == nil {
		err = huma.Error404NotFound("Active Earn vault not found.")
	}
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, counts)
}

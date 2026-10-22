package admin

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

const earnSnapshotMaxAge = 5 * time.Minute

type EarnSnapshot struct {
	ActiveAutodepositPolicies int    `json:"activeAutodepositPolicies"`
	ActiveAumRaw              string `json:"activeAumRaw"`
	ActivePrincipalRaw        string `json:"activePrincipalRaw"`
	AgeMs                     int64  `json:"ageMs"`
	RefreshedAt               string `json:"refreshedAt"`
	State                     string `json:"state" enum:"current,stale"`
	UniqueEarnPolicies        int    `json:"uniqueEarnPolicies"`
	UniqueEarnUsers           int    `json:"uniqueEarnUsers"`
}

type EarnStablecoinHealthRow struct {
	EarnStablecoinSummary
	BestSupplyApyPercent *float64                  `json:"bestSupplyApyPercent"`
	EligibleReserveCount int                       `json:"eligibleReserveCount"`
	EligibilityReason    string                    `json:"eligibilityReason"`
	EligibilityStatus    string                    `json:"eligibilityStatus" enum:"eligible,stale,below-liquidity,apy-out-of-range,no-current-row,no-supported-reserve"`
	Warnings             []StablecoinHealthWarning `json:"warnings" nullable:"false"`
}

type EarnMonitoring struct {
	Data     EarnData                  `json:"data"`
	Rows     []EarnStablecoinHealthRow `json:"rows" nullable:"false"`
	Snapshot *EarnSnapshot             `json:"snapshot" required:"false"`
}

type EarnPageProps struct {
	Monitoring        *EarnMonitoring `json:"monitoring" required:"false"`
	MonitoringError   bool            `json:"monitoringError"`
	PositionDirection string          `json:"positionDirection,omitempty"`
	PositionSort      string          `json:"positionSort,omitempty"`
	SelectedMint      string          `json:"selectedMint,omitempty"`
}

func (d *Deps) EarnSnapshot(ctx context.Context) (*EarnSnapshot, error) {
	var snapshot *EarnSnapshot
	err := d.AppRead(ctx, func(tx pgx.Tx) error {
		var s EarnSnapshot
		var refreshed time.Time
		err := tx.QueryRow(ctx, `
			SELECT active_autodeposit_policies, total_aum_raw::text, active_principal_raw::text,
			       refreshed_at, unique_earn_policies, unique_earn_users
			FROM loyal_stats_snapshots
			WHERE snapshot_key = 'current'
			LIMIT 1`).
			Scan(&s.ActiveAutodepositPolicies, &s.ActiveAumRaw, &s.ActivePrincipalRaw, &refreshed, &s.UniqueEarnPolicies, &s.UniqueEarnUsers)
		if err == pgx.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		age := max(0, time.Since(refreshed))
		s.AgeMs, s.RefreshedAt, s.State = age.Milliseconds(), earnISO(refreshed), "current"
		if age > earnSnapshotMaxAge {
			s.State = "stale"
		}
		snapshot = &s
		return nil
	})
	return snapshot, err
}

func (d *Deps) EarnMonitoring(ctx context.Context) (EarnMonitoring, error) {
	var (
		data        EarnData
		eligibility []SafeReserveMintEligibilitySummary
		snapshot    *EarnSnapshot
		errs        [3]error
		wg          sync.WaitGroup
	)
	wg.Add(3)
	go func() { defer wg.Done(); data, errs[0] = d.EarnData(ctx) }()
	go func() { defer wg.Done(); eligibility, errs[1] = d.CurrentVerifiedReserveEligibilityByMint(ctx) }()
	go func() { defer wg.Done(); snapshot, errs[2] = d.EarnSnapshot(ctx) }()
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return EarnMonitoring{}, err
		}
	}

	rows := make([]EarnStablecoinHealthRow, 0, len(data.Stablecoins))
	for _, stablecoin := range data.Stablecoins {
		row := EarnStablecoinHealthRow{
			EarnStablecoinSummary: stablecoin,
			EligibilityReason:     "No supported Safe reserve",
			EligibilityStatus:     "no-supported-reserve",
		}
		for _, e := range eligibility {
			if e.LiquidityMint == stablecoin.LiquidityMint {
				row.BestSupplyApyPercent = e.BestSupplyApyPercent
				row.EligibleReserveCount = e.EligibleReserveCount
				row.EligibilityReason = e.Reason
				row.EligibilityStatus = e.Status
				break
			}
		}
		row.Warnings = DeriveStablecoinHealthWarnings(stablecoin.Symbol, row.EligibleReserveCount, row.EligibilityReason)
		rows = append(rows, row)
	}
	return EarnMonitoring{Data: data, Rows: rows, Snapshot: snapshot}, nil
}

func loadEarnPage(ctx context.Context, d *Deps, req PageRequest) (EarnPageProps, error) {
	props := EarnPageProps{
		PositionDirection: req.Query.Get("positionDirection"),
		PositionSort:      req.Query.Get("positionSort"),
		SelectedMint:      req.Query.Get("mint"),
	}
	monitoring, err := d.EarnMonitoring(ctx)
	if err != nil {
		slog.Error("admin earn monitoring section failed to load", "error", err)
		props.MonitoringError = true
		return props, nil
	}
	props.Monitoring = &monitoring
	return props, nil
}

func registerEarn(m *Mux) {
	Page(m, "/earn", loadEarnPage)
	m.Router.Get("/api/earn/progressive", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("section") {
		case "monitoring":
			monitoring, err := m.Deps.EarnMonitoring(r.Context())
			if err != nil {
				writeError(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, monitoring)
		case "funding":
			funding, err := m.Deps.EarnFundingData(r.Context())
			if err != nil {
				writeError(w, r, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]EarnFundingData{"data": funding})
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "section must be monitoring or funding"})
		}
	})
}

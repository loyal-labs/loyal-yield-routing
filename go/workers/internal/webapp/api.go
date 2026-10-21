package webapp

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgxpool"
)

type earnStatsOutput struct {
	CacheControl string `header:"Cache-Control"`
	Source       string `header:"X-Loyal-API-Source"`
	Body         *EarnPublicStats
}

type earnStatsUnavailable struct {
	Detail struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (e *earnStatsUnavailable) Error() string  { return e.Detail.Message }
func (e *earnStatsUnavailable) GetStatus() int { return http.StatusServiceUnavailable }

func RegisterAPI(api huma.API, appDB *pgxpool.Pool) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		huma.Register(api, huma.Operation{
			OperationID: method + "-earn-stats",
			Method:      method,
			Path:        "/api/earn/stats",
			Summary:     "Public Loyal Earn statistics",
		}, func(ctx context.Context, _ *struct{}) (*earnStatsOutput, error) {
			stats, err := loadEarnPublicStats(ctx, appDB)
			if err != nil {
				// Database errors can contain connection details; keep them out of logs.
				slog.Warn("earn stats unavailable")
				unavailable := &earnStatsUnavailable{}
				unavailable.Detail.Code = "earn_stats_unavailable"
				unavailable.Detail.Message = "Earn stats are unavailable."
				return nil, huma.ErrorWithHeaders(unavailable, http.Header{
					"Cache-Control":      {"no-store"},
					"X-Loyal-API-Source": {"go"},
				})
			}
			return &earnStatsOutput{
				CacheControl: "public, max-age=60, s-maxage=300, stale-while-revalidate=3600",
				Source:       "go",
				Body:         stats,
			}, nil
		})
	}
}

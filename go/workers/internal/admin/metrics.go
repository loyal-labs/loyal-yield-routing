package admin

import (
	"context"
	"net/http"
	"os"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

var metricsHTTP = &http.Client{Timeout: 20 * time.Second}

func registerMetrics(m *Mux) {
	m.API.OpenAPI().AddOperation(&huma.Operation{
		OperationID: "metrics-dashboard",
		Method:      http.MethodGet,
		Path:        "/api/metrics",
		Tags:        []string{"metrics"},
		Parameters:  []*huma.Param{{Name: "section", In: "query", Required: true, Schema: &huma.Schema{Type: "string", Enum: []any{"latency", "dashboard"}}}},
		Responses: map[string]*huma.Response{"200": {
			Description: "section=dashboard; section=latency returns EarnRebalanceLatencyData",
			Content: map[string]*huma.MediaType{"application/json": {
				Schema: m.API.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[MetricsDashboardData](), true, ""),
			}},
		}},
	})
	Page(m, "/metrics", func(ctx context.Context, d *Deps, _ PageRequest) (EarnRebalanceLatencyData, error) {
		return metricsLatency(ctx, d), nil
	})
	m.Router.Get("/api/metrics", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("section") {
		case "latency":
			writeJSON(w, http.StatusOK, metricsLatency(r.Context(), m.Deps))
		case "dashboard":
			url := os.Getenv("LOYAL_ADMIN_CLICKSTACK_URL")
			if err := m.Deps.Require("ClickStack", m.Deps.ClickStackReadKey); err != nil {
				writeError(w, r, err)
				return
			}
			if err := m.Deps.Require("ClickStack", url); err != nil {
				writeError(w, r, err)
				return
			}
			sourceID := os.Getenv("LOYAL_ADMIN_CLICKSTACK_METRICS_SOURCE_ID")
			if sourceID == "" {
				sourceID = metricsDefaultSourceID
			}
			writeJSON(w, http.StatusOK, metricsClickStack{client: metricsHTTP, baseURL: url, key: m.Deps.ClickStackReadKey, sourceID: sourceID}.dashboard(r.Context()))
		default:
			writeError(w, r, huma.Error400BadRequest("section must be latency or dashboard"))
		}
	})
}

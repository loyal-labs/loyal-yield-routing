package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// POST /api/csp-report ports web/apps/web/src/app/api/csp-report/route.ts:
// the browser's report-uri body is read as JSON, a legacy {"csp-report": ...}
// object becomes one WARN log record in the collector, and the answer is 204
// whatever the body, the format or the collector did.
const cspReportPath = "/api/csp-report"

type cspReportInput struct {
	Raw rawJSON
}

type cspReportOutput struct{}

type cspReports struct {
	routes earnRoutes
}

func registerCSPReport(api huma.API, earn *Earn) {
	reports := cspReports{routes: earnRoutes{Earn: Earn{HTTP: &http.Client{Timeout: obsExportBudget}}}}
	if earn != nil {
		reports.routes.OTLP = earn.OTLP
	}
	huma.Register(api, huma.Operation{
		OperationID:   "post-csp-report",
		Method:        http.MethodPost,
		Path:          cspReportPath,
		Summary:       "Record a browser CSP violation report",
		DefaultStatus: http.StatusNoContent,
	}, reports.post)
	api.OpenAPI().Paths[cspReportPath].Post.RequestBody = &huma.RequestBody{Content: map[string]*huma.MediaType{
		"application/csp-report": {Schema: &huma.Schema{Type: "object"}},
	}}
}

func (c cspReports) post(ctx context.Context, in *cspReportInput) (*cspReportOutput, error) {
	if event := cspViolationEvent(in.Raw.data, time.Now()); event != nil {
		c.routes.exportObsErrorEvent(ctx, event)
	}
	return &cspReportOutput{}, nil
}

func cspViolationEvent(raw []byte, now time.Time) *obsNormalizedError {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var body any
	if decoder.Decode(&body) != nil {
		return nil
	}
	object, _ := body.(map[string]any)
	report, present := object["csp-report"]
	if !present || !jsTruthy(report) {
		return nil
	}
	fields, _ := report.(map[string]any)
	blocked := jsString(jsCoalesce(fields, "blocked-uri"))
	directive := jsString(jsCoalesce(fields, "effective-directive", "violated-directive"))
	document := jsString(jsCoalesce(fields, "document-uri"))
	return &obsNormalizedError{
		deploymentEnvironment: observabilityDeploymentEnvironment(),
		exceptionMessage:      sanitizeTelemetryText("CSP "+directive+" blocked "+blocked+" on "+document, 512),
		exceptionName:         "Error",
		method:                http.MethodPost,
		operation:             "next.request.error",
		pathname:              normalizeTelemetryPathname(cspReportPath),
		release:               observabilityRelease(),
		runtime:               "node",
		serviceName:           "loyal-frontend",
		severity:              obsSeverityWarn,
		timestamp:             now.UTC().Format("2006-01-02T15:04:05.000Z"),
	}
}

func jsCoalesce(fields map[string]any, keys ...string) any {
	for _, key := range keys {
		if value := fields[key]; value != nil {
			return value
		}
	}
	return nil
}

func jsTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case json.Number:
		f, err := v.Float64()
		return err != nil || f != 0 && !math.IsNaN(f)
	}
	return true
}

func jsString(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return string(v)
		}
		if abs := math.Abs(f); abs != 0 && (abs >= 1e21 || abs < 1e-6) {
			mantissa, exponent, _ := strings.Cut(strconv.FormatFloat(f, 'e', -1, 64), "e")
			return mantissa + "e" + exponent[:1] + strings.TrimLeft(exponent[1:], "0")
		}
		return strconv.FormatFloat(f, 'f', -1, 64)
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = jsString(item)
		}
		return strings.Join(parts, ",")
	}
	return "[object Object]"
}

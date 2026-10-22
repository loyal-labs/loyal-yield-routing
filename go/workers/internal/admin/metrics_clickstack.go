package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	metricsDefaultSourceID = "6a58601284f8f1d4ff4d12ab"
	metricsRange           = 7 * 24 * time.Hour
	metricsResponseLimit   = 5 * 1024 * 1024
	metricsBucketMinutes   = 120
	metricsGranularity     = "2h"
	metricsQueryTimeout    = 15 * time.Second

	metricsOperationField = "arrayElement(Attributes, 'loyal.operation')"
	metricsPhaseField     = "arrayElement(Attributes, 'loyal.phase')"
	metricsDependencyKey  = "arrayElement(Attributes, 'loyal.dependency')"
	metricsOutcomeField   = "arrayElement(Attributes, 'loyal.outcome')"
	metricsPlatformField  = "arrayElement(Attributes, 'loyal.platform')"
	metricsBucketField    = "__hdx_time_bucket"
)

type MetricPoint struct {
	BucketStartedAt string  `json:"bucketStartedAt"`
	Dependency      *string `json:"dependency"`
	Operation       string  `json:"operation"`
	Outcome         string  `json:"outcome"`
	Phase           *string `json:"phase"`
	Platform        string  `json:"platform" enum:"android,desktop,ios,other"`
	P95Ms           float64 `json:"p95Ms"`
}

type OperationVolume struct {
	Completed float64 `json:"completed"`
	Failed    float64 `json:"failed"`
	Total     float64 `json:"total"`
}

type MetricsSurfaceStatus struct {
	Desktop string `json:"desktop" enum:"available,unavailable"`
	Mobile  string `json:"mobile" enum:"available,unavailable"`
}

type MetricsSurfaceVolume struct {
	Desktop map[string]OperationVolume `json:"desktop" nullable:"false"`
	Mobile  map[string]OperationVolume `json:"mobile" nullable:"false"`
}

type MetricsDashboardData struct {
	BucketMinutes  int32                `json:"bucketMinutes"`
	Desktop        []MetricPoint        `json:"desktop" nullable:"false"`
	FetchedAt      string               `json:"fetchedAt"`
	Mobile         []MetricPoint        `json:"mobile" nullable:"false"`
	RangeEndedAt   string               `json:"rangeEndedAt"`
	RangeStartedAt string               `json:"rangeStartedAt"`
	Status         MetricsSurfaceStatus `json:"status"`
	Volume         MetricsSurfaceVolume `json:"volume"`
}

type metricsQuery struct {
	environment string
	groupBy     []string
	latency     bool
	metricName  string
	serviceName string
	desktop     bool
}

var (
	metricsDesktopLatencyGroup = []string{"Attributes['loyal.operation']", "Attributes['loyal.phase']", "Attributes['loyal.dependency']", "Attributes['loyal.outcome']"}
	metricsMobileLatencyGroup  = []string{"Attributes['loyal.operation']", "Attributes['loyal.outcome']", "Attributes['loyal.platform']"}
	metricsVolumeGroup         = []string{"Attributes['loyal.operation']", "Attributes['loyal.outcome']"}

	metricsQueries = []metricsQuery{
		{environment: "production", metricName: "loyal.frontend.loading.duration", serviceName: "loyal-frontend", desktop: true, latency: true, groupBy: metricsDesktopLatencyGroup},
		{environment: "prod", metricName: "loyal.mobile.loading.duration", serviceName: "loyal-mobile", latency: true, groupBy: metricsMobileLatencyGroup},
		{environment: "production", metricName: "loyal.frontend.loading.duration", serviceName: "loyal-frontend", desktop: true, groupBy: metricsVolumeGroup},
		{environment: "prod", metricName: "loyal.mobile.loading.duration", serviceName: "loyal-mobile", groupBy: metricsVolumeGroup},
	}

	metricsDimension = regexp.MustCompile(`^[a-zA-Z0-9._:/ -]+$`)
)

type metricsClickStack struct {
	client   *http.Client
	baseURL  string
	key      string
	sourceID string
}

type metricsRawRow map[string]any

func metricsReadDimension(value any) *string {
	s, ok := value.(string)
	if !ok {
		return nil
	}
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 120 || !metricsDimension.MatchString(s) {
		return nil
	}
	return &s
}

func metricsReadPlatform(value any) string {
	if p := metricsReadDimension(value); p != nil {
		switch strings.ToLower(*p) {
		case "android":
			return "android"
		case "ios":
			return "ios"
		}
	}
	return "other"
}

func metricsSeries(row metricsRawRow) float64 {
	switch v := row["series_0"].(type) {
	case float64:
		return v
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	case nil:
		if _, present := row["series_0"]; present {
			return 0
		}
	case bool:
		if v {
			return 1
		}
		return 0
	}
	return math.NaN()
}

func metricsParseBucket(value string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999", "2006-01-02"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func metricsNormalizeRow(row metricsRawRow, desktop bool) (MetricPoint, bool) {
	operation := metricsReadDimension(row[metricsOperationField])
	outcome := "unknown"
	if o := metricsReadDimension(row[metricsOutcomeField]); o != nil {
		outcome = *o
	}
	p95 := metricsSeries(row)
	bucket := metricsReadDimension(row[metricsBucketField])
	if operation == nil || math.IsNaN(p95) || math.IsInf(p95, 0) || p95 < 0 || bucket == nil {
		return MetricPoint{}, false
	}
	at, ok := metricsParseBucket(*bucket)
	if !ok {
		return MetricPoint{}, false
	}
	platform := "desktop"
	if !desktop {
		platform = metricsReadPlatform(row[metricsPlatformField])
	}
	return MetricPoint{
		BucketStartedAt: at.UTC().Format("2006-01-02T15:04:05.000Z"),
		Dependency:      metricsReadDimension(row[metricsDependencyKey]),
		Operation:       *operation,
		Outcome:         outcome,
		Phase:           metricsReadDimension(row[metricsPhaseField]),
		Platform:        platform,
		P95Ms:           p95,
	}, true
}

func metricsLatencyPoints(rows []metricsRawRow, desktop bool) []MetricPoint {
	points := []MetricPoint{}
	for _, row := range rows {
		if p, ok := metricsNormalizeRow(row, desktop); ok {
			points = append(points, p)
		}
	}
	sort.SliceStable(points, func(i, j int) bool { return points[i].BucketStartedAt < points[j].BucketStartedAt })
	return points
}

func metricsVolumeByOperation(rows []metricsRawRow) map[string]OperationVolume {
	volume := map[string]OperationVolume{}
	for _, row := range rows {
		operation := metricsReadDimension(row[metricsOperationField])
		outcome := "unknown"
		if o := metricsReadDimension(row[metricsOutcomeField]); o != nil {
			outcome = *o
		}
		count := metricsSeries(row)
		if operation == nil || math.IsNaN(count) || math.IsInf(count, 0) || count <= 0 {
			continue
		}
		current := volume[*operation]
		current.Total += count
		switch outcome {
		case "completed":
			current.Completed += count
		case "failed":
			current.Failed += count
		}
		volume[*operation] = current
	}
	return volume
}

func (c metricsClickStack) query(ctx context.Context, q metricsQuery, start, end time.Time) ([]metricsRawRow, error) {
	series := map[string]any{
		"dataSource":     "metrics",
		"field":          "Value",
		"groupBy":        q.groupBy,
		"metricDataType": "gauge",
		"metricName":     q.metricName,
		"sourceId":       c.sourceID,
		"where":          fmt.Sprintf("ServiceName = '%s' AND ResourceAttributes['deployment.environment.name'] = '%s'", q.serviceName, q.environment),
		"whereLanguage":  "sql",
	}
	if q.latency {
		series["aggFn"] = "quantile"
		series["level"] = 0.95
	} else {
		series["aggFn"] = "count"
	}
	body, err := json.Marshal(map[string]any{
		"endTime":          end.UnixMilli(),
		"granularity":      metricsGranularity,
		"series":           []any{series},
		"seriesReturnType": "column",
		"startTime":        start.UnixMilli(),
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, metricsQueryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.baseURL, "/")+"/charts/series", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("authorization", "Bearer "+c.key)
	req.Header.Set("content-type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("ClickStack returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, metricsResponseLimit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > metricsResponseLimit {
		return nil, errors.New("ClickStack response exceeded the size limit")
	}
	var parsed struct {
		Data []any `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.Data == nil {
		return nil, errors.New("ClickStack returned an invalid metrics response")
	}
	rows := make([]metricsRawRow, 0, len(parsed.Data))
	for _, item := range parsed.Data {
		if row, ok := item.(map[string]any); ok {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (c metricsClickStack) dashboard(ctx context.Context) MetricsDashboardData {
	end := time.Now()
	start := end.Add(-metricsRange)
	results := make([][]metricsRawRow, len(metricsQueries))
	errs := make([]error, len(metricsQueries))
	var wg sync.WaitGroup
	for i, q := range metricsQueries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = c.query(ctx, q, start, end)
		}()
	}
	wg.Wait()

	data := MetricsDashboardData{
		BucketMinutes:  metricsBucketMinutes,
		Desktop:        []MetricPoint{},
		FetchedAt:      time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		Mobile:         []MetricPoint{},
		RangeEndedAt:   end.UTC().Format("2006-01-02T15:04:05.000Z"),
		RangeStartedAt: start.UTC().Format("2006-01-02T15:04:05.000Z"),
		Status:         MetricsSurfaceStatus{Desktop: "unavailable", Mobile: "unavailable"},
		Volume:         MetricsSurfaceVolume{Desktop: map[string]OperationVolume{}, Mobile: map[string]OperationVolume{}},
	}
	for i, q := range metricsQueries {
		if errs[i] != nil {
			slog.Error("ClickStack metrics query failed", "error", errs[i], "latency", q.latency, "desktop", q.desktop)
			continue
		}
		switch {
		case q.latency && q.desktop:
			data.Desktop = metricsLatencyPoints(results[i], true)
			data.Status.Desktop = "available"
		case q.latency:
			data.Mobile = metricsLatencyPoints(results[i], false)
			data.Status.Mobile = "available"
		case q.desktop:
			data.Volume.Desktop = metricsVolumeByOperation(results[i])
		default:
			data.Volume.Mobile = metricsVolumeByOperation(results[i])
		}
	}
	return data
}

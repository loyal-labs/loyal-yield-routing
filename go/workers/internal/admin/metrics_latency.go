package admin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

type EarnLatencyPoint struct {
	ConfirmedAt             string   `json:"confirmedAt"`
	DecisionAt              string   `json:"decisionAt"`
	DecisionID              string   `json:"decisionId"`
	DecisionToSignedMs      float64  `json:"decisionToSignedMs"`
	MonitorToSubmittedMs    float64  `json:"monitorToSubmittedMs"`
	ObservedToOpportunityMs float64  `json:"observedToOpportunityMs"`
	OpportunityAt           string   `json:"opportunityAt"`
	OpportunityToReadyMs    *float64 `json:"opportunityToReadyMs"`
	ReadyAt                 *string  `json:"readyAt"`
	ReadyToDecisionMs       *float64 `json:"readyToDecisionMs"`
	SignedAt                string   `json:"signedAt"`
	SignedToSubmittedMs     float64  `json:"signedToSubmittedMs"`
	LiquidityMint           *string  `json:"liquidityMint"`
	SourceReserve           string   `json:"sourceReserve"`
	SubmittedAt             string   `json:"submittedAt"`
	SubmittedToConfirmedMs  float64  `json:"submittedToConfirmedMs"`
	TargetObservedAt        string   `json:"targetObservedAt"`
	TargetReserve           string   `json:"targetReserve"`
}

type EarnLatencyChartPoint struct {
	BucketStartedAt         string   `json:"bucketStartedAt"`
	DecisionToSignedMs      *float64 `json:"decisionToSignedMs"`
	MonitorToSubmittedMs    *float64 `json:"monitorToSubmittedMs"`
	ObservedToOpportunityMs *float64 `json:"observedToOpportunityMs"`
	OpportunityToReadyMs    *float64 `json:"opportunityToReadyMs"`
	ReadyToDecisionMs       *float64 `json:"readyToDecisionMs"`
	SampleCount             int32    `json:"sampleCount"`
	SignedToSubmittedMs     *float64 `json:"signedToSubmittedMs"`
	SubmittedToConfirmedMs  *float64 `json:"submittedToConfirmedMs"`
}

type EarnLatencyStageSummary struct {
	Key           string  `json:"key" enum:"observedToOpportunityMs,opportunityToReadyMs,readyToDecisionMs,decisionToSignedMs,signedToSubmittedMs,submittedToConfirmedMs"`
	Label         string  `json:"label"`
	MeasuredCount int32   `json:"measuredCount"`
	P50Ms         float64 `json:"p50Ms"`
	P95Ms         float64 `json:"p95Ms"`
}

type EarnRebalanceLatencyData struct {
	FetchedAt                 string                    `json:"fetchedAt"`
	MeasuredCount             int32                     `json:"measuredCount"`
	MonitorToSubmittedP50Ms   float64                   `json:"monitorToSubmittedP50Ms"`
	MonitorToSubmittedP95Ms   float64                   `json:"monitorToSubmittedP95Ms"`
	LatestPoints              []EarnLatencyPoint        `json:"latestPoints" nullable:"false"`
	Points                    []EarnLatencyChartPoint   `json:"points" nullable:"false"`
	RangeEndedAt              *string                   `json:"rangeEndedAt"`
	RangeStartedAt            *string                   `json:"rangeStartedAt"`
	Stages                    []EarnLatencyStageSummary `json:"stages" nullable:"false"`
	Status                    string                    `json:"status" enum:"available,unavailable"`
	SubmittedToConfirmedP50Ms float64                   `json:"submittedToConfirmedP50Ms"`
	SubmittedToConfirmedP95Ms float64                   `json:"submittedToConfirmedP95Ms"`
	TotalConfirmed            int32                     `json:"totalConfirmed"`
}

type metricsSQLDetail struct {
	ConfirmedAt             string   `json:"confirmed_at"`
	DecisionAt              string   `json:"decision_at"`
	DecisionToSignedMs      *float64 `json:"decision_to_signed_ms"`
	ID                      string   `json:"id"`
	LiquidityMint           *string  `json:"liquidity_mint"`
	MonitorToSubmittedMs    *float64 `json:"monitor_to_submitted_ms"`
	ObservedToOpportunityMs *float64 `json:"observed_to_opportunity_ms"`
	OpportunityAt           string   `json:"opportunity_at"`
	OpportunityToReadyMs    *float64 `json:"opportunity_to_ready_ms"`
	ReadyAt                 *string  `json:"ready_at"`
	ReadyToDecisionMs       *float64 `json:"ready_to_decision_ms"`
	SignedAt                string   `json:"signed_at"`
	SignedToSubmittedMs     *float64 `json:"signed_to_submitted_ms"`
	SourceReserve           string   `json:"source_reserve"`
	SubmittedAt             string   `json:"submitted_at"`
	SubmittedToConfirmedMs  *float64 `json:"submitted_to_confirmed_ms"`
	TargetObservedAt        string   `json:"target_observed_at"`
	TargetReserve           string   `json:"target_reserve"`
}

type metricsSQLChartPoint struct {
	BucketStartedAt         string   `json:"bucket_started_at"`
	DecisionToSignedMs      *float64 `json:"decision_to_signed_ms"`
	MonitorToSubmittedMs    *float64 `json:"monitor_to_submitted_ms"`
	ObservedToOpportunityMs *float64 `json:"observed_to_opportunity_ms"`
	OpportunityToReadyMs    *float64 `json:"opportunity_to_ready_ms"`
	ReadyToDecisionMs       *float64 `json:"ready_to_decision_ms"`
	SampleCount             *float64 `json:"sample_count"`
	SignedToSubmittedMs     *float64 `json:"signed_to_submitted_ms"`
	SubmittedToConfirmedMs  *float64 `json:"submitted_to_confirmed_ms"`
}

type metricsSQLStage struct {
	Key           string   `json:"key"`
	Label         string   `json:"label"`
	MeasuredCount *float64 `json:"measured_count"`
	P50Ms         *float64 `json:"p50_ms"`
	P95Ms         *float64 `json:"p95_ms"`
}

var metricsStages = []EarnLatencyStageSummary{
	{Key: "observedToOpportunityMs", Label: "Observed \u2192 opportunity"},
	{Key: "opportunityToReadyMs", Label: "Opportunity \u2192 ready"},
	{Key: "readyToDecisionMs", Label: "Ready \u2192 decision"},
	{Key: "decisionToSignedMs", Label: "Decision \u2192 signed"},
	{Key: "signedToSubmittedMs", Label: "Signed \u2192 submitted"},
	{Key: "submittedToConfirmedMs", Label: "Submitted \u2192 confirmed"},
}

func metricsISO(value string) string {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999Z07", "2006-01-02 15:04:05.999999999"} {
		if t, err := time.Parse(layout, value); err == nil {
			return t.UTC().Format("2006-01-02T15:04:05.000Z")
		}
	}
	return ""
}

func metricsISOPtr(value *string) *string {
	if value == nil {
		return nil
	}
	iso := metricsISO(*value)
	return &iso
}

func metricsTimePtr(value *time.Time) *string {
	if value == nil {
		return nil
	}
	iso := value.UTC().Format("2006-01-02T15:04:05.000Z")
	return &iso
}

func metricsNumber(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

func metricsInt(value *float64) int32 { return int32(metricsNumber(value)) }

const metricsLatencySQL = `
    WITH confirmed_decisions AS MATERIALIZED (
      SELECT
        decision.id,
        decision.source_reserve,
        decision.target_reserve,
        decision.liquidity_mint,
        decision.created_at
      FROM loyal_yield.rebalance_decisions AS decision
      WHERE decision.status = 'confirmed'
        AND decision.source_reserve IS NOT NULL
        AND decision.target_reserve IS NOT NULL
        AND decision.created_at >= now() - interval '30 days'
    ),
    first_submission AS MATERIALIZED (
      SELECT
        submission.decision_id,
        MIN(submission.created_at) AS signed_at,
        MIN(submission.submitted_at) FILTER (
          WHERE submission.submitted_at IS NOT NULL
        ) AS submitted_at,
        MIN(submission.confirmed_at) FILTER (
          WHERE submission.confirmed_at IS NOT NULL
        ) AS confirmed_at
      FROM loyal_yield.signed_route_submissions AS submission
      INNER JOIN confirmed_decisions AS decision
        ON decision.id = submission.decision_id
      WHERE submission.decision_id IS NOT NULL
      GROUP BY submission.decision_id
    ),
    linked AS MATERIALIZED (
      SELECT
        decision.id,
        decision.source_reserve,
        decision.target_reserve,
        decision.liquidity_mint,
        opportunity.created_at AS opportunity_at,
        opportunity.ready_at,
        decision.created_at AS decision_at,
        first_submission.signed_at,
        first_submission.submitted_at,
        first_submission.confirmed_at,
        target_reserve.observed_at AS target_observed_at
      FROM confirmed_decisions AS decision
      INNER JOIN loyal_yield.rebalance_opportunities AS opportunity
        ON opportunity.decision_id = decision.id
      INNER JOIN loyal_yield.optimizer_epochs AS epoch
        ON epoch.id = opportunity.optimizer_epoch_id
      INNER JOIN LATERAL (
        SELECT (reserve.value->>'observedAt')::timestamptz AS observed_at
        FROM jsonb_array_elements(epoch.market_state->'reserves') AS reserve(value)
        WHERE reserve.value->>'reserve' = decision.target_reserve
          AND reserve.value->>'observedAt' IS NOT NULL
        LIMIT 1
      ) AS target_reserve ON true
      INNER JOIN first_submission
        ON first_submission.decision_id = decision.id
    ),
    valid AS MATERIALIZED (
      SELECT *
      FROM linked
      WHERE target_observed_at <= opportunity_at
        AND opportunity_at <= decision_at
        AND decision_at <= signed_at
        AND signed_at <= submitted_at
        AND submitted_at <= confirmed_at
    ),
    measured AS MATERIALIZED (
      SELECT
        valid.*,
        ROUND(EXTRACT(epoch FROM (
          valid.opportunity_at - valid.target_observed_at
        ))::numeric * 1000)::bigint AS observed_to_opportunity_ms,
      CASE
        WHEN valid.ready_at >= valid.opportunity_at
          AND valid.ready_at <= valid.decision_at
          THEN ROUND(EXTRACT(epoch FROM (
            valid.ready_at - valid.opportunity_at
          ))::numeric * 1000)::bigint
      END AS opportunity_to_ready_ms,
      CASE
        WHEN valid.ready_at >= valid.opportunity_at
          AND valid.ready_at <= valid.decision_at
          THEN ROUND(EXTRACT(epoch FROM (
            valid.decision_at - valid.ready_at
          ))::numeric * 1000)::bigint
      END AS ready_to_decision_ms,
      ROUND(EXTRACT(epoch FROM (
        valid.signed_at - valid.decision_at
        ))::numeric * 1000)::bigint AS decision_to_signed_ms,
      ROUND(EXTRACT(epoch FROM (
        valid.submitted_at - valid.signed_at
        ))::numeric * 1000)::bigint AS signed_to_submitted_ms,
      ROUND(EXTRACT(epoch FROM (
        valid.confirmed_at - valid.submitted_at
        ))::numeric * 1000)::bigint AS submitted_to_confirmed_ms,
      ROUND(EXTRACT(epoch FROM (
        valid.submitted_at - valid.target_observed_at
        ))::numeric * 1000)::bigint AS monitor_to_submitted_ms_number
      FROM valid
    ),
    summary AS (
      SELECT
        COUNT(*)::integer AS measured_count,
        percentile_cont(0.5) WITHIN GROUP (
          ORDER BY monitor_to_submitted_ms_number
        ) AS monitor_to_submitted_p50_ms,
        percentile_cont(0.95) WITHIN GROUP (
          ORDER BY monitor_to_submitted_ms_number
        ) AS monitor_to_submitted_p95_ms,
        percentile_cont(0.5) WITHIN GROUP (
          ORDER BY submitted_to_confirmed_ms
        ) AS submitted_to_confirmed_p50_ms,
        percentile_cont(0.95) WITHIN GROUP (
          ORDER BY submitted_to_confirmed_ms
        ) AS submitted_to_confirmed_p95_ms,
        MIN(submitted_at) AS range_started_at,
        MAX(submitted_at) AS range_ended_at
      FROM measured
    ),
    stage_definitions(position, key, label) AS (
      VALUES
        (1, 'observedToOpportunityMs'::text, 'Observed → opportunity'::text),
        (2, 'opportunityToReadyMs'::text, 'Opportunity → ready'::text),
        (3, 'readyToDecisionMs'::text, 'Ready → decision'::text),
        (4, 'decisionToSignedMs'::text, 'Decision → signed'::text),
        (5, 'signedToSubmittedMs'::text, 'Signed → submitted'::text),
        (6, 'submittedToConfirmedMs'::text, 'Submitted → confirmed'::text)
    ),
    stage_values AS (
      SELECT stage.position, stage.value
      FROM measured
      CROSS JOIN LATERAL (
        VALUES
          (1, measured.observed_to_opportunity_ms),
          (2, measured.opportunity_to_ready_ms),
          (3, measured.ready_to_decision_ms),
          (4, measured.decision_to_signed_ms),
          (5, measured.signed_to_submitted_ms),
          (6, measured.submitted_to_confirmed_ms)
      ) AS stage(position, value)
      WHERE stage.value IS NOT NULL
    ),
    stage_aggregates AS (
      SELECT
        definition.position,
        definition.key,
        definition.label,
      COUNT(stage_value.value)::integer AS measured_count,
      percentile_cont(0.5) WITHIN GROUP (
          ORDER BY stage_value.value
        ) AS p50_ms,
        percentile_cont(0.95) WITHIN GROUP (
          ORDER BY stage_value.value
        ) AS p95_ms
      FROM stage_definitions AS definition
      LEFT JOIN stage_values AS stage_value
        ON stage_value.position = definition.position
      GROUP BY definition.position, definition.key, definition.label
    ),
    stage_summaries AS (
      SELECT jsonb_agg(
        jsonb_build_object(
          'key', stage.key,
          'label', stage.label,
          'measured_count', stage.measured_count,
          'p50_ms', stage.p50_ms,
          'p95_ms', stage.p95_ms
        )
        ORDER BY stage.position
      ) AS stages
      FROM stage_aggregates AS stage
    ),
    chart_points AS (
      SELECT
        date_bin(
          interval '1 day',
          measured.submitted_at,
          timestamptz '1970-01-01 00:00:00+00'
        ) AS bucket_started_at,
        COUNT(*)::integer AS sample_count,
        percentile_cont(0.5) WITHIN GROUP (
          ORDER BY measured.monitor_to_submitted_ms_number
        ) AS monitor_to_submitted_ms,
        percentile_cont(0.5) WITHIN GROUP (
          ORDER BY measured.observed_to_opportunity_ms
        ) AS observed_to_opportunity_ms,
        percentile_cont(0.5) WITHIN GROUP (
          ORDER BY measured.opportunity_to_ready_ms
        ) AS opportunity_to_ready_ms,
        percentile_cont(0.5) WITHIN GROUP (
          ORDER BY measured.ready_to_decision_ms
        ) AS ready_to_decision_ms,
        percentile_cont(0.5) WITHIN GROUP (
          ORDER BY measured.decision_to_signed_ms
        ) AS decision_to_signed_ms,
        percentile_cont(0.5) WITHIN GROUP (
          ORDER BY measured.signed_to_submitted_ms
        ) AS signed_to_submitted_ms,
        percentile_cont(0.5) WITHIN GROUP (
          ORDER BY measured.submitted_to_confirmed_ms
        ) AS submitted_to_confirmed_ms
      FROM measured
      GROUP BY 1
    ),
    latest_points AS (
      SELECT measured.*
      FROM measured
      ORDER BY measured.submitted_at DESC, measured.id DESC
      LIMIT 50
    ),
    total_confirmed AS (
      SELECT COUNT(*)::integer AS total_confirmed
      FROM confirmed_decisions
    )
    SELECT
      summary.measured_count,
      summary.monitor_to_submitted_p50_ms,
      summary.monitor_to_submitted_p95_ms,
      summary.range_started_at,
      summary.range_ended_at,
      summary.submitted_to_confirmed_p50_ms,
      summary.submitted_to_confirmed_p95_ms,
      total_confirmed.total_confirmed,
      stage_summaries.stages,
      COALESCE(
        (
          SELECT jsonb_agg(
            jsonb_build_object(
              'bucket_started_at', chart.bucket_started_at,
              'decision_to_signed_ms', chart.decision_to_signed_ms,
              'monitor_to_submitted_ms', chart.monitor_to_submitted_ms,
              'observed_to_opportunity_ms', chart.observed_to_opportunity_ms,
              'opportunity_to_ready_ms', chart.opportunity_to_ready_ms,
              'ready_to_decision_ms', chart.ready_to_decision_ms,
              'sample_count', chart.sample_count,
              'signed_to_submitted_ms', chart.signed_to_submitted_ms,
              'submitted_to_confirmed_ms', chart.submitted_to_confirmed_ms
            )
            ORDER BY chart.bucket_started_at
          )
          FROM chart_points AS chart
        ),
        '[]'::jsonb
      ) AS chart_points,
      COALESCE(
        (
          SELECT jsonb_agg(
            jsonb_build_object(
              'confirmed_at', latest.confirmed_at,
              'decision_at', latest.decision_at,
              'decision_to_signed_ms', latest.decision_to_signed_ms,
              'id', latest.id::text,
              'liquidity_mint', latest.liquidity_mint,
              'monitor_to_submitted_ms', latest.monitor_to_submitted_ms_number,
              'observed_to_opportunity_ms', latest.observed_to_opportunity_ms,
              'opportunity_at', latest.opportunity_at,
              'opportunity_to_ready_ms', latest.opportunity_to_ready_ms,
              'ready_at', latest.ready_at,
              'ready_to_decision_ms', latest.ready_to_decision_ms,
              'signed_at', latest.signed_at,
              'signed_to_submitted_ms', latest.signed_to_submitted_ms,
              'source_reserve', latest.source_reserve,
              'submitted_at', latest.submitted_at,
              'submitted_to_confirmed_ms', latest.submitted_to_confirmed_ms,
              'target_observed_at', latest.target_observed_at,
              'target_reserve', latest.target_reserve
            )
            ORDER BY latest.submitted_at DESC, latest.id DESC
          )
          FROM latest_points AS latest
        ),
        '[]'::jsonb
      ) AS latest_points
    FROM summary
    CROSS JOIN stage_summaries
    CROSS JOIN total_confirmed
`

func metricsLatencyUnavailable() EarnRebalanceLatencyData {
	stages := make([]EarnLatencyStageSummary, len(metricsStages))
	copy(stages, metricsStages)
	return EarnRebalanceLatencyData{
		FetchedAt:    time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		LatestPoints: []EarnLatencyPoint{},
		Points:       []EarnLatencyChartPoint{},
		Stages:       stages,
		Status:       "unavailable",
	}
}

var metricsLatencyCache = &staleWhileRevalidate[EarnRebalanceLatencyData]{fresh: time.Minute, stale: 15 * time.Minute, wait: 4 * time.Second, budget: readTimeout}

func metricsLatency(d *Deps) EarnRebalanceLatencyData {
	data, have, err := metricsLatencyCache.get(func(ctx context.Context) (EarnRebalanceLatencyData, error) {
		return metricsLoadLatency(ctx, d)
	})
	if err != nil && !errors.Is(err, errStillRefreshing) {
		slog.Error("Earn rebalance latency query failed", "error", err)
	}
	if !have {
		return metricsLatencyUnavailable()
	}
	return data
}

func metricsLoadLatency(ctx context.Context, d *Deps) (EarnRebalanceLatencyData, error) {
	var (
		measured, total                   *int32
		mp50, mp95, sp50, sp95            *float64
		rangeStart, rangeEnd              *time.Time
		stagesJSON, chartJSON, latestJSON []byte
	)
	err := d.YieldRead(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, metricsLatencySQL).Scan(
			&measured, &mp50, &mp95, &rangeStart, &rangeEnd, &sp50, &sp95, &total, &stagesJSON, &chartJSON, &latestJSON,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	})
	if err != nil {
		return EarnRebalanceLatencyData{}, err
	}

	var stages []metricsSQLStage
	var chart []metricsSQLChartPoint
	var latest []metricsSQLDetail
	for _, part := range []struct {
		raw  []byte
		into any
	}{{stagesJSON, &stages}, {chartJSON, &chart}, {latestJSON, &latest}} {
		if len(part.raw) == 0 {
			continue
		}
		if err := json.Unmarshal(part.raw, part.into); err != nil {
			return EarnRebalanceLatencyData{}, err
		}
	}

	latestPoints := make([]EarnLatencyPoint, 0, len(latest))
	for _, p := range latest {
		latestPoints = append(latestPoints, EarnLatencyPoint{
			ConfirmedAt:             metricsISO(p.ConfirmedAt),
			DecisionAt:              metricsISO(p.DecisionAt),
			DecisionID:              p.ID,
			DecisionToSignedMs:      metricsNumber(p.DecisionToSignedMs),
			MonitorToSubmittedMs:    metricsNumber(p.MonitorToSubmittedMs),
			ObservedToOpportunityMs: metricsNumber(p.ObservedToOpportunityMs),
			OpportunityAt:           metricsISO(p.OpportunityAt),
			OpportunityToReadyMs:    p.OpportunityToReadyMs,
			ReadyAt:                 metricsISOPtr(p.ReadyAt),
			ReadyToDecisionMs:       p.ReadyToDecisionMs,
			SignedAt:                metricsISO(p.SignedAt),
			SignedToSubmittedMs:     metricsNumber(p.SignedToSubmittedMs),
			LiquidityMint:           p.LiquidityMint,
			SourceReserve:           p.SourceReserve,
			SubmittedAt:             metricsISO(p.SubmittedAt),
			SubmittedToConfirmedMs:  metricsNumber(p.SubmittedToConfirmedMs),
			TargetObservedAt:        metricsISO(p.TargetObservedAt),
			TargetReserve:           p.TargetReserve,
		})
	}
	points := make([]EarnLatencyChartPoint, 0, len(chart))
	for _, p := range chart {
		points = append(points, EarnLatencyChartPoint{
			BucketStartedAt:         metricsISO(p.BucketStartedAt),
			DecisionToSignedMs:      p.DecisionToSignedMs,
			MonitorToSubmittedMs:    p.MonitorToSubmittedMs,
			ObservedToOpportunityMs: p.ObservedToOpportunityMs,
			OpportunityToReadyMs:    p.OpportunityToReadyMs,
			ReadyToDecisionMs:       p.ReadyToDecisionMs,
			SampleCount:             metricsInt(p.SampleCount),
			SignedToSubmittedMs:     p.SignedToSubmittedMs,
			SubmittedToConfirmedMs:  p.SubmittedToConfirmedMs,
		})
	}
	stageSummaries := make([]EarnLatencyStageSummary, 0, len(stages))
	for _, s := range stages {
		stageSummaries = append(stageSummaries, EarnLatencyStageSummary{
			Key:           s.Key,
			Label:         s.Label,
			MeasuredCount: metricsInt(s.MeasuredCount),
			P50Ms:         metricsNumber(s.P50Ms),
			P95Ms:         metricsNumber(s.P95Ms),
		})
	}
	data := EarnRebalanceLatencyData{
		FetchedAt:                 time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		MonitorToSubmittedP50Ms:   metricsNumber(mp50),
		MonitorToSubmittedP95Ms:   metricsNumber(mp95),
		LatestPoints:              latestPoints,
		Points:                    points,
		RangeEndedAt:              metricsTimePtr(rangeEnd),
		RangeStartedAt:            metricsTimePtr(rangeStart),
		Stages:                    stageSummaries,
		Status:                    "available",
		SubmittedToConfirmedP50Ms: metricsNumber(sp50),
		SubmittedToConfirmedP95Ms: metricsNumber(sp95),
	}
	if measured != nil {
		data.MeasuredCount = *measured
	}
	if total != nil {
		data.TotalConfirmed = *total
	}
	return data, nil
}

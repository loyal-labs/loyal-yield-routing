package admin

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	journeysLogSourceID = "6a58601284f8f1d4ff4d12a5"
	journeysMaxRows     = 2000
	journeysWindow      = 7 * 24 * time.Hour
)

var journeysFields = []struct{ alias, key string }{
	{"trafficKind", "loyal.traffic.kind"},
	{"eventId", "loyal.event.id"},
	{"journeyId", "loyal.journey.id"},
	{"attemptId", "loyal.flow.id"},
	{"attemptNumber", "loyal.attempt.number"},
	{"flow", "loyal.flow.name"},
	{"variant", "loyal.flow.variant"},
	{"stage", "loyal.flow.stage"},
	{"outcome", "loyal.flow.outcome"},
	{"retryCount", "loyal.retry.count"},
	{"elapsedMs", "loyal.elapsed_ms"},
	{"errorCode", "loyal.error.code"},
	{"errorCategory", "loyal.error.category"},
	{"chainState", "loyal.chain.state"},
	{"persistenceState", "loyal.persistence.state"},
	{"release", "loyal.client.build_id"},
	{"userId", "loyal.user.id"},
	{"rumSessionId", "loyal.rum.session_id"},
}

type JourneysPage struct {
	Cohort      string         `json:"cohort" enum:"external,internal,synthetic"`
	Coverage    string         `json:"coverage" enum:"available,truncated,unavailable"`
	Events      []JourneyEvent `json:"events" nullable:"false"`
	EvidenceURL string         `json:"evidenceUrl"`
	FetchedAt   int64          `json:"fetchedAt"`
	LogSourceID string         `json:"logSourceId"`
}

type JourneyEvent struct {
	Time             int64  `json:"time"`
	TrafficKind      string `json:"trafficKind"`
	EventID          string `json:"eventId"`
	JourneyID        string `json:"journeyId"`
	AttemptID        string `json:"attemptId"`
	AttemptNumber    int    `json:"attemptNumber"`
	Flow             string `json:"flow"`
	Variant          string `json:"variant"`
	Stage            string `json:"stage"`
	Outcome          string `json:"outcome"`
	RetryCount       int    `json:"retryCount"`
	ElapsedMs        int64  `json:"elapsedMs"`
	ErrorCode        string `json:"errorCode"`
	ErrorCategory    string `json:"errorCategory"`
	ChainState       string `json:"chainState"`
	PersistenceState string `json:"persistenceState"`
	Release          string `json:"release"`
	UserID           string `json:"userId"`
	RumSessionID     string `json:"rumSessionId"`
}

func journeysWhere(cohort string) string {
	scope := "ResourceAttributes['deployment.environment.name'] IN ('production','prod') AND LogAttributes['loyal.traffic.kind'] IN ('external','internal')"
	if cohort == "synthetic" {
		scope = "LogAttributes['loyal.traffic.kind'] = 'synthetic'"
	}
	return "ServiceName = 'loyal-frontend' AND LogAttributes['loyal.event.kind'] = 'flow_lifecycle' AND LogAttributes['loyal.flow.source'] = 'browser' AND " + scope
}

func journeysSelect() string {
	parts := []string{"toUnixTimestamp64Milli(Timestamp) AS time"}
	for _, f := range journeysFields {
		parts = append(parts, fmt.Sprintf("LogAttributes['%s'] AS %s", f.key, f.alias))
	}
	return strings.Join(parts, ",")
}

func journeysNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil && strings.TrimSpace(n) != "" {
			return f, true
		}
	}
	return 0, false
}

func journeysEvents(rows []map[string]any) []JourneyEvent {
	events := make([]JourneyEvent, 0, len(rows))
	for _, row := range rows {
		text := func(key string) string {
			s, _ := row[key].(string)
			return s
		}
		number := func(key string) float64 {
			n, _ := journeysNumber(row[key])
			return n
		}
		at, ok := journeysNumber(row["time"])
		if !ok {
			continue
		}
		events = append(events, JourneyEvent{
			Time: int64(at), TrafficKind: text("trafficKind"), EventID: text("eventId"), JourneyID: text("journeyId"),
			AttemptID: text("attemptId"), AttemptNumber: int(number("attemptNumber")), Flow: text("flow"),
			Variant: text("variant"), Stage: text("stage"), Outcome: text("outcome"), RetryCount: int(number("retryCount")),
			ElapsedMs: int64(number("elapsedMs")), ErrorCode: text("errorCode"), ErrorCategory: text("errorCategory"),
			ChainState: text("chainState"), PersistenceState: text("persistenceState"), Release: text("release"),
			UserID: text("userId"), RumSessionID: text("rumSessionId"),
		})
	}
	return events
}

func loadJourneys(ctx context.Context, d *Deps, req PageRequest) (JourneysPage, error) {
	evidence := strings.TrimRight(os.Getenv("LOYAL_ADMIN_JOURNEYS_URL"), "/")
	if err := d.Require("the journeys evidence host (LOYAL_ADMIN_JOURNEYS_URL)", evidence); err != nil {
		return JourneysPage{}, err
	}
	cohort := req.Query.Get("cohort")
	if cohort != "internal" && cohort != "synthetic" {
		cohort = "external"
	}
	now := time.Now()
	page := JourneysPage{
		Cohort: cohort, Coverage: "unavailable", Events: []JourneyEvent{},
		EvidenceURL: evidence, FetchedAt: now.UnixMilli(), LogSourceID: journeysLogSourceID,
	}
	api := strings.TrimRight(os.Getenv("LOYAL_ADMIN_CLICKSTACK_URL"), "/")
	if api == "" || d.ClickStackReadKey == "" {
		return page, nil
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	err := readsFetchJSON(ctx, 15*time.Second, 4<<20, http.MethodPost, api+"/search",
		map[string]string{"authorization": "Bearer " + d.ClickStackReadKey, "content-type": "application/json"},
		map[string]any{
			"sourceId":      journeysLogSourceID,
			"startTime":     readsISO(now.Add(-journeysWindow)),
			"endTime":       readsISO(now),
			"select":        journeysSelect(),
			"where":         journeysWhere(cohort),
			"whereLanguage": "sql",
			"orderBy":       "Timestamp DESC",
			"maxResults":    journeysMaxRows,
		}, &body)
	if err != nil || body.Data == nil {
		return page, nil
	}
	page.Events = journeysEvents(body.Data)
	page.Coverage = "available"
	if len(body.Data) >= journeysMaxRows {
		page.Coverage = "truncated"
	}
	return page, nil
}

func registerJourneys(m *Mux) {
	Page(m, "/journeys", loadJourneys)
}

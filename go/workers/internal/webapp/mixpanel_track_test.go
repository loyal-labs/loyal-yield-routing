package webapp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// mixpanelStub records each /track body and answers with the next status.
func mixpanelStub(t *testing.T, statuses ...int) (*MixpanelTracker, func() [][]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var got [][]map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body []map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not a JSON array: %v", err)
		}
		mu.Lock()
		i := len(got)
		got = append(got, body)
		mu.Unlock()
		status := 200
		if len(statuses) > 0 {
			status = statuses[min(i, len(statuses)-1)]
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"status":1,"error":null}`))
	}))
	t.Cleanup(server.Close)
	m := NewMixpanelTracker("tok")
	m.url = server.URL + "/track?strict=1"
	return m, func() [][]map[string]any {
		mu.Lock()
		defer mu.Unlock()
		return append([][]map[string]any(nil), got...)
	}
}

func TestMixpanelTrackBody(t *testing.T) {
	m, requests := mixpanelStub(t)
	at := time.Date(2026, 11, 9, 15, 41, 0, 0, time.UTC)
	err := m.track(context.Background(), []mixpanelEvent{{
		Name:       "Earnings Push Sent",
		DistinctID: "mob:W1",
		InsertID:   "earnings:yield-digest:W1:2026-11-09",
		Time:       at,
		Properties: map[string]any{"type": "digest", "campaign": "yield-digest", "amount_usd": 1.37, "lifetime_usd": 48.2, "route": "/(tabs)/earn"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := requests()
	if len(got) != 1 || len(got[0]) != 1 {
		t.Fatalf("requests = %v", got)
	}
	event := got[0][0]
	props, _ := event["properties"].(map[string]any)
	if event["event"] != "Earnings Push Sent" || props["token"] != "tok" || props["distinct_id"] != "mob:W1" ||
		props["$insert_id"] != mixpanelInsertID("earnings:yield-digest:W1:2026-11-09") || props["time"] != float64(1794238860000) ||
		props["amount_usd"] != 1.37 || props["route"] != "/(tabs)/earn" {
		t.Fatalf("event = %v", event)
	}
}

func TestMixpanelTrackBatchesAndStopsOnError(t *testing.T) {
	events := make([]mixpanelEvent, 120)
	for i := range events {
		events[i] = mixpanelEvent{Name: "E", DistinctID: "mob:W", InsertID: "i", Time: time.Now()}
	}
	m, requests := mixpanelStub(t)
	if err := m.track(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	got := requests()
	if len(got) != 3 || len(got[0]) != 50 || len(got[1]) != 50 || len(got[2]) != 20 {
		t.Fatalf("batches = %d", len(got))
	}
	m, requests = mixpanelStub(t, 200, 400)
	err := m.track(context.Background(), events)
	if err == nil || len(requests()) != 2 {
		t.Fatalf("err = %v, requests = %d", err, len(requests()))
	}
	if text := err.Error(); len(text) > 600 || strings.Contains(text, "tok\"") {
		t.Fatalf("error = %q", text)
	}
}

func TestMixpanelNilTrackerIsNoOp(t *testing.T) {
	if NewMixpanelTracker("") != nil {
		t.Fatal("empty token should give a nil tracker")
	}
	var m *MixpanelTracker
	if err := m.track(context.Background(), []mixpanelEvent{{Name: "E"}}); err != nil {
		t.Fatal(err)
	}
}

// Mixpanel strict mode rejects a batch whose $insert_id is not 1-36 bytes of [A-Za-z0-9-].
func TestMixpanelInsertIDFitsMixpanelFormat(t *testing.T) {
	valid := regexp.MustCompile(`^[A-Za-z0-9-]{1,36}$`)
	long := "earnings:yield-digest:9xQeWvG816bUx9EPjHmaT23yvVM2ZWbrrpZb9PusVFin:2026-11-10"
	for _, name := range []string{"earn-deposit:123", long} {
		if id := mixpanelInsertID(name); !valid.MatchString(id) {
			t.Fatalf("%q -> %q", name, id)
		}
	}
	if mixpanelInsertID(long) != mixpanelInsertID(long) || mixpanelInsertID("earn-deposit:1") == mixpanelInsertID("earn-withdrawal:1") {
		t.Fatal("insert ids must be stable per name and distinct across names")
	}
}

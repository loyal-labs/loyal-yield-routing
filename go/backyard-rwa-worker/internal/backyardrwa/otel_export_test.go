package backyardrwa

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type otelTestPayload struct {
	ResourceLogs []struct {
		Resource struct {
			Attributes []otelTestAttr `json:"attributes"`
		} `json:"resource"`
		ScopeLogs []struct {
			LogRecords []struct {
				Attributes     []otelTestAttr    `json:"attributes"`
				Body           map[string]string `json:"body"`
				SeverityNumber int               `json:"severityNumber"`
				SeverityText   string            `json:"severityText"`
				TimeUnixNano   string            `json:"timeUnixNano"`
			} `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

type otelTestAttr struct {
	Key   string         `json:"key"`
	Value map[string]any `json:"value"`
}

func otelAttrMap(attrs []otelTestAttr) map[string]string {
	out := map[string]string{}
	for _, a := range attrs {
		for _, v := range a.Value {
			out[a.Key] = fmt.Sprint(v)
		}
	}
	return out
}

func TestOtelExportPayloadShape(t *testing.T) {
	type request struct {
		path, auth string
		body       []byte
	}
	requests := make(chan request, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- request{r.URL.Path, r.Header.Get("authorization"), body}
	}))
	defer server.Close()
	t.Setenv("OBSERVABILITY_OTLP_ENDPOINT", server.URL+"/ignored?x=1")
	t.Setenv("OBSERVABILITY_INGESTION_API_KEY", "test-key")
	e := newOtelExporterFromEnv("sha-abc")
	if e == nil {
		t.Fatal("exporter disabled with both env vars set")
	}
	e.latched("kamino_stale")
	e.emit("WARN", "ltv_warning", "ltv_warning", otelInt("loyal.ltv_bps", 4600))
	e.emit("INFO", "heartbeat", "")
	e.tickResult(errors.New("dial postgres://user:pw@db.example/x failed"), true)
	e.shutdown(2 * time.Second)

	got := <-requests
	if got.path != "/v1/logs" || got.auth != "test-key" {
		t.Fatalf("path=%q auth=%q", got.path, got.auth)
	}
	if strings.Contains(string(got.body), "postgres://") || strings.Contains(string(got.body), "pw@") {
		t.Fatalf("URL leaked into export: %s", got.body)
	}
	var payload otelTestPayload
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatal(err)
	}
	resource := otelAttrMap(payload.ResourceLogs[0].Resource.Attributes)
	if resource["service.name"] != "backyard-rwa-worker" || resource["service.version"] != "sha-abc" || resource["deployment.environment.name"] != "production" {
		t.Fatalf("resource attributes: %v", resource)
	}
	records := payload.ResourceLogs[0].ScopeLogs[0].LogRecords
	byBody := map[string]int{}
	for _, r := range records {
		byBody[r.Body["stringValue"]]++
		attrs := otelAttrMap(r.Attributes)
		if attrs["loyal.flow.name"] != "backyard_rwa.worker" || attrs["loyal.route.key"] != productionRouteKey ||
			"backyard_rwa."+attrs["loyal.flow.stage"] != r.Body["stringValue"] || r.TimeUnixNano == "" {
			t.Fatalf("record attributes: %v body=%v", attrs, r.Body)
		}
		switch r.SeverityText {
		case "ERROR":
			if r.SeverityNumber != 17 {
				t.Fatalf("ERROR severity %d", r.SeverityNumber)
			}
		case "WARN":
			if r.SeverityNumber != 13 {
				t.Fatalf("WARN severity %d", r.SeverityNumber)
			}
		case "INFO":
			if r.SeverityNumber != 9 {
				t.Fatalf("INFO severity %d", r.SeverityNumber)
			}
		default:
			t.Fatalf("severity %q", r.SeverityText)
		}
		switch r.Body["stringValue"] {
		case "backyard_rwa.latched":
			if attrs["loyal.error.code"] != "kamino_stale" {
				t.Fatalf("latched code: %v", attrs)
			}
		case "backyard_rwa.ltv_warning":
			if attrs["loyal.ltv_bps"] != "4600" {
				t.Fatalf("ltv attr: %v", attrs)
			}
		case "backyard_rwa.worker_exit":
			if attrs["loyal.error.code"] != "worker_fault" || !strings.Contains(attrs["loyal.error.detail"], "<url>") {
				t.Fatalf("worker_exit attrs: %v", attrs)
			}
		}
	}
	if byBody["backyard_rwa.latched"] != 1 || byBody["backyard_rwa.ltv_warning"] != 1 || byBody["backyard_rwa.worker_exit"] != 1 {
		t.Fatalf("records: %v", byBody)
	}
}

func TestOtelExportDisabledWithoutEnv(t *testing.T) {
	for _, env := range [][2]string{{"", "k"}, {"https://collector.example", ""}, {"http://collector.example", "k"}} {
		t.Setenv("OBSERVABILITY_OTLP_ENDPOINT", env[0])
		t.Setenv("OBSERVABILITY_INGESTION_API_KEY", env[1])
		if e := newOtelExporterFromEnv("sha-abc"); e != nil {
			t.Fatalf("exporter enabled for %v", env)
		}
	}
	var e *otelExporter // disabled exporter: every hook is a no-op
	e.latched("x")
	e.tickResult(errors.New("boom"), true)
	e.noteSnapshot(Snapshot{Fresh: true, HasPosition: true, LTVBPS: 9000})
	e.shutdown(time.Millisecond)
}

func TestOtelExportQueueFullDropsWithoutBlocking(t *testing.T) {
	e := newOtelExporter("http://127.0.0.1:1/v1/logs", "k", "sha-abc", 2) // sender not started
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			e.emit("INFO", "heartbeat", "")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("emit blocked on a full queue")
	}
	if len(e.queue) != 2 || e.dropped.Load() != 8 {
		t.Fatalf("queued=%d dropped=%d", len(e.queue), e.dropped.Load())
	}
}

func TestOtelAlertRepeatedFailureCrossing(t *testing.T) {
	e := newOtelExporter("http://127.0.0.1:1/v1/logs", "k", "sha-abc", 1024)
	hold := budgetHold("send_valuation_expired")
	fail := func(action Action, err error) {
		e.noteAction(action)
		e.tickResult(err, false)
	}
	repeated := func() int {
		n := 0
		for len(e.queue) > 0 {
			if r := <-e.queue; r.Body["stringValue"] == "backyard_rwa.repeated_failure" {
				n++
			}
		}
		return n
	}
	for i := 0; i < 9; i++ {
		fail(ReportNAV, hold)
	}
	if n := repeated(); n != 0 {
		t.Fatalf("emitted before 10: %d", n)
	}
	fail(ReportNAV, hold) // 10th in a row
	if n := repeated(); n != 1 {
		t.Fatalf("crossing 10 emitted %d", n)
	}
	for i := 0; i < 49; i++ {
		fail(ReportNAV, hold)
	}
	if n := repeated(); n != 0 {
		t.Fatalf("emitted between 10 and 60: %d", n)
	}
	fail(ReportNAV, hold) // 60th
	if n := repeated(); n != 1 {
		t.Fatalf("60th emitted %d", n)
	}
	// A different action+reason starts a new streak; a success resets it.
	for i := 0; i < 9; i++ {
		fail(VoltrRestoreIdle, hold)
	}
	e.tickResult(nil, false)
	for i := 0; i < 9; i++ {
		fail(VoltrRestoreIdle, hold)
	}
	if n := repeated(); n != 0 {
		t.Fatalf("streak survived reset: %d", n)
	}
}

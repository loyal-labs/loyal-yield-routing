package backyard

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// Backyard events are journald/ClickStack inputs: a fatal exit must carry its
// stable code without leaking RPC or database credentials, and the heartbeat
// must expose the fee-accumulator boundary the alert rule reads.
func TestEventsRedactCredentialsAndExposeHeartbeatFields(t *testing.T) {
	var out bytes.Buffer
	e := newEvents(slog.New(slog.NewJSONHandler(&out, nil)), nil)
	now := time.Unix(kaminoFixtureUnix, 0)
	e.now = func() time.Time { return now }
	s := feePolicySnapshot(t, 0, 0)
	s.FeeAccumulatorRaw, s.LPSupplyInclFeesRaw = 21, 2000
	e.noteSnapshot(s)
	e.tickResult(errors.New("dial https://rpc.example/?api-key=secret-value failed password=hunter2"), true)
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if len(records) != 2 || records[0]["msg"] != "backyard_heartbeat" || records[1]["msg"] != "backyard_worker_exit" {
		t.Fatalf("unexpected event sequence: %s", out.String())
	}
	if records[0]["fee_accumulator_warning"] != true || records[0]["family"] != "backyard" {
		t.Fatalf("heartbeat lost the fee boundary: %v", records[0])
	}
	if records[1]["code"] != "worker_fault" || strings.Contains(out.String(), "secret-value") || strings.Contains(out.String(), "hunter2") {
		t.Fatalf("exit event leaked credentials or lost its code: %v", records[1])
	}
	s.FeeAccumulatorRaw = 20
	e.noteSnapshot(s)
	out.Reset()
	now = now.Add(heartbeatInterval)
	e.tickResult(nil, false)
	if !strings.Contains(out.String(), `"fee_accumulator_warning":false`) {
		t.Fatalf("one-percent boundary reported as a warning: %s", out.String())
	}
}

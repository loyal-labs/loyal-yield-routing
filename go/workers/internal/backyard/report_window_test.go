package backyard

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"testing"
	"time"
)

// slotClock is a chain clock driven by round trips: every RPC or Jupiter
// exchange takes its latency in wall time, and the chain advances one slot per
// slotTime. getSlot and every response context answer the clock's slot, so the
// production slot windows judge exactly the time the reads took.
type slotClock struct {
	start    time.Time
	origin   int64
	slotTime time.Duration
}

func (c *slotClock) slot() int64 {
	return c.origin + int64(time.Since(c.start)/c.slotTime)
}

var fixtureContextSlot = regexp.MustCompile(`"context":\{"slot":42\}`)

func (c *slotClock) rpc(next http.RoundTripper, latency time.Duration) http.RoundTripper {
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(request.Body)
		request.Body = io.NopCloser(bytes.NewReader(raw))
		var body struct{ Method string }
		_ = json.Unmarshal(raw, &body)
		time.Sleep(latency)
		slot := c.slot()
		if body.Method == "getSlot" {
			return response(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":%d}`, slot)), nil
		}
		answer, err := next.RoundTrip(request)
		if err != nil {
			return nil, err
		}
		out, _ := io.ReadAll(answer.Body)
		out = fixtureContextSlot.ReplaceAll(out, []byte(fmt.Sprintf(`"context":{"slot":%d}`, slot)))
		if body.Method == "getMultipleAccounts" {
			out = clockAtSlot(out, raw, slot)
		}
		return response(string(out)), nil
	})
}

// clockAtSlot moves the Clock sysvar in a getMultipleAccounts answer to the
// answer's own slot, as a real bank would.
func clockAtSlot(answer, request []byte, slot int64) []byte {
	var call struct{ Params []json.RawMessage }
	var addresses []string
	var reply struct {
		Result struct {
			Context json.RawMessage `json:"context"`
			Value   []map[string]any
		}
	}
	if json.Unmarshal(request, &call) != nil || len(call.Params) == 0 || json.Unmarshal(call.Params[0], &addresses) != nil || json.Unmarshal(answer, &reply) != nil {
		return answer
	}
	for i, address := range addresses {
		if address != budgetClockAddress || i >= len(reply.Result.Value) {
			continue
		}
		data, _ := reply.Result.Value[i]["data"].([]any)
		if len(data) == 0 {
			return answer
		}
		encoded, _ := data[0].(string)
		clock, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(clock) != 40 {
			return answer
		}
		binary.LittleEndian.PutUint64(clock[:8], uint64(slot))
		data[0] = base64.StdEncoding.EncodeToString(clock)
	}
	out, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": reply.Result})
	if err != nil {
		return answer
	}
	return out
}

func (c *slotClock) jupiter(next http.RoundTripper, latency time.Duration) http.RoundTripper {
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		time.Sleep(latency)
		return next.RoundTrip(request)
	})
}

// Live 2026-10: the serial B2 exit pricer spent ~25 of the adaptor's 32
// report slots, so most hourly REPORT_NAVs reached simulation or send stale.
// This drives the real bind (the locked persist) and the real build gate on a
// chain clock where an RPC round trip costs 1/4 slot and a Jupiter round trip
// 7/8 slot (100/350 ms at 400 ms slots), then simulates and sends. Keep
// real-scale durations so host CPU and race instrumentation overhead are not
// magnified into simulated chain slots. The report must still be inside its
// age limit when it simulates and sends.
func TestLeverageNAVReportSendsInsideItsWindows(t *testing.T) {
	ctx, cancel, db, _ := openManualRecoveryTestDatabase(t, 60*time.Second)
	defer cancel()
	defer db.Close()
	o, m, rpc, client, _, route := leverage175Fixture(t)
	o.Snapshot.LastReportAgeSeconds = 3600
	s := o.Snapshot
	d := Decide(s)
	if d.Action != ReportNAV || d.StrategyKey != route.Lane {
		t.Fatalf("expected an hourly report on the 1.75x position: %+v", d)
	}
	nav := BridgeBuildRequest{Action: ReportNAV, Policy: testPolicyAccount(policyKey{action: ReportNAV}), AdaptorConfig: bridgeStrategy, Settings: bridgeSettings, RecentBlockhash: bridgeVault, LastValidBlockHeight: 99,
		Report: BridgeReport{Sequence: uint64(s.Slot), ObservedSlot: uint64(s.Slot), NAVAfterRaw: uint64(max(s.StrategyNAVRaw, 0)), SnapshotDigest: s.ReportSnapshotDigest}}
	effects, _, _, err := bridgeExpectedEffects(d, uint64(s.VoltrIdleRaw), uint64(s.VoltrStrategyIdleRaw), uint64(s.SquadsIdleRaw))
	if err != nil {
		t.Fatal(err)
	}
	effects.Kind, effects.ReturnData = "bridge", expectedAdaptorReturnData(uint64(max(s.StrategyNAVRaw, 0)))

	key := fmt.Sprintf("report-window-%d", time.Now().UnixNano())
	id := key + "-op"
	state := mustJSON(t, map[string]any{"generation": 2})
	envelope, _ := json.Marshal(map[string]any{"decision": newDecisionEvidence(o, d, m.SHA256)})
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_route_states(route_key,state,state_version) VALUES($1,$2::jsonb,2)`, key, string(state)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.pool.Exec(ctx, `INSERT INTO loyal_yield.multiply_operations(operation_id,route_key,status,action,strategy_key,expected_effects) VALUES($1,$2,'decided',$3,$4,$5::jsonb)`, id, key, d.Action, d.StrategyKey, string(envelope)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.AcquireRouteLease(ctx, key, "report-window", time.Minute); err != nil {
		t.Fatal(err)
	}

	const slotTime, rpcLatency, jupiterLatency = 400 * time.Millisecond, 100 * time.Millisecond, 350 * time.Millisecond
	// Observation, preparation, custody proof and decision record already
	// spent two slots of the tick when bind starts.
	clock := &slotClock{start: time.Now(), origin: s.Slot + 2, slotTime: slotTime}
	rpcOf(rpc).Transport = clock.rpc(rpcOf(rpc).Transport, rpcLatency)
	fixtureHTTP(client).Transport = clock.jupiter(fixtureHTTP(client).Transport, jupiterLatency)
	if err = db.bindOperation(ctx, rpc, m, id, o, d, nav, effects); err != nil {
		t.Fatalf("bind at slot S+%d: %v", clock.slot()-s.Slot, err)
	}
	bound := clock.slot()
	encoded, err := jsonMarshalExpectedEffects(effects)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.requireBoundIntent(ctx, id, nav, encoded); err != nil {
		t.Fatalf("bind was not persisted: %v", err)
	}
	if _, err = m.validateRequestPrestate(ctx, rpc, nav, effects); err != nil {
		t.Fatalf("build gate at slot S+%d: %v", clock.slot()-s.Slot, err)
	}
	// Sign locally and simulate (a heavier round trip), then the landing
	// status read and the broadcast itself.
	time.Sleep(rpcLatency)
	simulated, err := confirmedSlot(ctx, rpc)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := confirmedSlot(ctx, rpc)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(rpcLatency)
	t.Logf("S=%d bound=S+%d simulated=S+%d sent=S+%d", s.Slot, bound-s.Slot, simulated-s.Slot, sent-s.Slot)
	if ReportExpiredAtLanding(s.Slot, simulated) {
		t.Fatalf("report expired in simulation at S+%d", simulated-s.Slot)
	}
	if ReportExpiredAtLanding(s.Slot, sent) {
		t.Fatalf("report expired at send S+%d", sent-s.Slot)
	}
}

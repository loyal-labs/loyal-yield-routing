package backyard

import (
	"encoding/json"
	"testing"
	"time"
)

func TestCanaryRequestCannotTargetDeferredLane(t *testing.T) {
	request := func(lane string) string {
		raw, _ := json.Marshal(pilotCanaryEntryRequest{ID: sha256Bytes([]byte("canary-acceptance")), Lane: lane, EquityRaw: 1_000_000, ExpiresAt: time.Now().UTC().Add(10 * time.Minute)})
		return string(raw)
	}
	// The exit-only Ethena lane is never a canary destination.
	t.Setenv("BACKYARD_RWA_PILOT_CANARY_ENTRY", request(ethenaUSDePYUSD.Lane))
	_, err := readPilotCanaryEntryRequest(time.Now().UTC())
	assertPreSendHold(t, err, "invalid_pilot_canary_request")
	// Every active registry lane is an entry lane (Prime/PRIME/USDC again
	// since 2026-10-10).
	for _, lane := range earnLaneIDs(false) {
		t.Setenv("BACKYARD_RWA_PILOT_CANARY_ENTRY", request(lane))
		got, err := readPilotCanaryEntryRequest(time.Now().UTC())
		if err != nil || got == nil || got.Lane != lane {
			t.Fatal("permitted canary request rejected", lane, err, got)
		}
	}
	// An in-memory deferred request (prior durable state) is rejected by the
	// selection-time validation as well.
	in := selectorFixture()
	in.Policy = DefaultSelectorPolicy()
	in.Markets[0].Lane, in.Quotes[0].DestinationLane = ethenaUSDePYUSD.Lane, ethenaUSDePYUSD.Lane
	in.canaryRequest = &pilotCanaryEntryRequest{ID: sha256Bytes([]byte("deferred-acceptance")), Lane: in.Markets[0].Lane, EquityRaw: int64(in.Snapshot.TotalVaultNAVRaw), ExpiresAt: in.Now.Add(10 * time.Minute)}
	result, receipt, err := selectPilotCanaryEntry(in, SelectOpportunity(in, SelectorState{}), nil)
	assertPreSendHold(t, err, "invalid_pilot_canary_request")
	if receipt != nil || result.Action == "CANARY_ENTER" {
		t.Fatal("deferred canary accepted", result, receipt)
	}
}

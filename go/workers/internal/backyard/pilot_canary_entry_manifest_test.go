package backyard

import (
	"strings"
	"testing"
	"time"
)

// autoCanaryFixtureAt retargets the installed canary fixture onto the
// candidate AUTO lane: the same snapshot guards, quote identity, equity and
// expiry, with only the requested destination lane moved. The quote carries a
// real observed PYUSD debt price through the established budget valuation
// path — the bound evidence the installed non-USDC borrow check requires —
// with the quote's slot window lifted into the price evidence window. debtScale
// is millionths times parity (1e6 == 1 PYUSD/USDC), so values above 2e6 depeg
// the borrow receive above equity.
func autoCanaryFixtureAt(t *testing.T, debtScale int64) SelectorInput {
	t.Helper()
	in := pilotCanaryFixture()
	in.Markets[0].Lane = autoAUTOPYUSD.Lane
	in.Quotes[0].DestinationLane = autoAUTOPYUSD.Lane
	_, price, _ := autoDebtPriceFixture(t, debtScale)
	in.Snapshot.Slot = price.ObservedSlot
	quote := in.Quotes[0]
	quote.SampleSlot = price.ObservedSlot
	quote.ValidThroughSlot = price.ValidThroughSlot
	quote.DebtPrice = copyDebtPrice(&price)
	in.Quotes[0] = quote
	in.canaryRequest = &pilotCanaryEntryRequest{ID: sha256Bytes([]byte("auto-one-acceptance")), Lane: autoAUTOPYUSD.Lane, EquityRaw: 1_000_000, ExpiresAt: in.Now.Add(10 * time.Minute)}
	return in
}

func autoCanaryFixture(t *testing.T) SelectorInput {
	t.Helper()
	return autoCanaryFixtureAt(t, 1_100_000)
}

// TestPilotCanaryRequestValidateOnRegistry pins the request lane authority:
// every active registry lane (AUTO included) is admitted, the exit-only
// Ethena lane is refused, and every ID, equity and expiry negative holds.
func TestPilotCanaryRequestValidateOnRegistry(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC().Add(time.Minute)
	request := pilotCanaryEntryRequest{ID: sha256Bytes([]byte("auto-one-acceptance")), Lane: autoAUTOPYUSD.Lane, EquityRaw: 1_000, ExpiresAt: now.Add(5 * time.Minute)}
	if err := request.validate(now); err != nil {
		t.Fatal("registry refused the AUTO request:", err)
	}
	ethena := request
	ethena.Lane = ethenaUSDePYUSD.Lane
	if err := ethena.validate(now); err == nil {
		t.Fatal("exit-only lane accepted as a canary destination")
	}
	installed := pilotCanaryFixture()
	if err := installed.canaryRequest.validate(now); err != nil {
		t.Fatal("installed lane request refused under manifest scope:", err)
	}
	for name, change := range map[string]func(*pilotCanaryEntryRequest){
		"short id":        func(r *pilotCanaryEntryRequest) { r.ID = "deadbeef" },
		"zero equity":     func(r *pilotCanaryEntryRequest) { r.EquityRaw = 0 },
		"over cap":        func(r *pilotCanaryEntryRequest) { r.EquityRaw = int64(strategyTwoBridgeLegCapRaw) + 1 },
		"expired":         func(r *pilotCanaryEntryRequest) { r.ExpiresAt = now },
		"overlong window": func(r *pilotCanaryEntryRequest) { r.ExpiresAt = now.Add(16 * time.Minute) },
	} {
		bad := request
		change(&bad)
		if err := bad.validate(now); err == nil {
			t.Fatalf("manifest scope relaxed the %s negative", name)
		}
	}
}

// TestPilotCanaryReadOnRegistry pins the parser: environment variable,
// unknown-field, trailing-value and size rejections, with the registry lane
// authority.
func TestPilotCanaryReadOnRegistry(t *testing.T) {
	now := time.Now().UTC()
	valid := `{"id":"` + sha256Bytes([]byte("auto-one-acceptance")) + `","lane":"` + autoAUTOPYUSD.Lane + `","equityRaw":1000000,"expiresAt":"` + now.Add(10*time.Minute).UTC().Format(time.RFC3339Nano) + `"}`
	t.Setenv("BACKYARD_RWA_PILOT_CANARY_ENTRY", valid)
	got, err := readPilotCanaryEntryRequest(now)
	if err != nil || got == nil || got.Lane != autoAUTOPYUSD.Lane || got.EquityRaw != 1_000_000 || got.ID != sha256Bytes([]byte("auto-one-acceptance")) {
		t.Fatalf("valid binding refused a well-formed candidate request: %+v %v", got, err)
	}
	for name, raw := range map[string]string{
		"unknown field": strings.Replace(valid, `"equityRaw":1000000`, `"equityRaw":1000000,"extra":1`, 1),
		"trailing":      valid + " {}",
		"oversized":     `{"note":"` + strings.Repeat("x", 600) + `"}`,
	} {
		t.Setenv("BACKYARD_RWA_PILOT_CANARY_ENTRY", raw)
		if got, err := readPilotCanaryEntryRequest(now); err == nil || got != nil {
			t.Fatalf("manifest scope relaxed the %s rejection", name)
		}
	}
	t.Setenv("BACKYARD_RWA_PILOT_CANARY_ENTRY", "")
	if got, err := readPilotCanaryEntryRequest(now); got != nil || err != nil {
		t.Fatal("absent environment variable must stay a nil request")
	}
}

// TestPilotCanarySelectOnRegistry pins the forced-acceptance call: BOTH the
// request and the constructed SelectorEntry validate against the registry, and
// consumed-ID, capacity and receipt semantics are unchanged.
func TestPilotCanarySelectOnRegistry(t *testing.T) {
	t.Parallel()
	in := autoCanaryFixture(t)
	result := SelectorResult{Candidates: []CandidateForecast{{Lane: autoAUTOPYUSD.Lane, CostsKnown: true}}}
	installedAccepted, installedReceipt, err := selectPilotCanaryEntry(in, result, nil)
	if err != nil || installedAccepted.Action != "CANARY_ENTER" || installedReceipt == nil {
		t.Fatalf("installed manifest refused the canary acceptance: %+v %v %v", installedAccepted, installedReceipt, err)
	}
	if installedReceipt.Request != *in.canaryRequest || installedReceipt.QuoteEvidenceID != in.Quotes[0].EvidenceID {
		t.Fatalf("installed receipt identity drifted: %+v", installedReceipt)
	}
	accepted, receipt, err := selectPilotCanaryEntry(in, result, nil)
	if err != nil || accepted.Action != "CANARY_ENTER" || accepted.SelectedQuote == nil || receipt == nil {
		t.Fatalf("complete binding refused the canary acceptance: %+v %v %v", accepted, receipt, err)
	}
	// The installed non-USDC borrow checks gate the candidate lane exactly as
	// shipped: missing debt price evidence, a depegged price that values the
	// borrow above equity, and a stale price each refuse the constructed entry.
	missing := autoCanaryFixtureAt(t, 1_100_000)
	missing.Quotes[0].DebtPrice = nil
	if _, _, err := selectPilotCanaryEntry(missing, result, nil); err == nil || err.Error() != "invalid_selector_entry" {
		t.Fatalf("missing debt price did not refuse the entry exactly: %v", err)
	}
	depegged := autoCanaryFixtureAt(t, 3_000_000)
	if _, _, err := selectPilotCanaryEntry(depegged, result, nil); err == nil || err.Error() != "invalid_selector_entry" {
		t.Fatalf("depegged debt price did not refuse the entry exactly: %v", err)
	}
	stale := autoCanaryFixtureAt(t, 1_100_000)
	stalePrice := copyDebtPrice(stale.Quotes[0].DebtPrice)
	stalePrice.ObservedSlot = stale.Quotes[0].SampleSlot - 1
	stale.Quotes[0].DebtPrice = stalePrice
	if _, _, err := selectPilotCanaryEntry(stale, result, nil); err == nil || err.Error() != "invalid_selector_entry" {
		t.Fatalf("stale debt price did not refuse the entry exactly: %v", err)
	}
	if receipt.Request != *in.canaryRequest || receipt.QuoteEvidenceID != in.Quotes[0].EvidenceID || receipt.AcceptedAt != in.Now {
		t.Fatalf("receipt identity drifted: %+v", receipt)
	}
	history := map[string]pilotCanaryEntryReceipt{receipt.Request.ID: *receipt}
	replayed, again, err := selectPilotCanaryEntry(in, result, history)
	if err != nil || again != nil || replayed.Action != "KEEP" || replayed.Reason != "operator_canary_already_consumed" {
		t.Fatalf("consumed-ID check drifted under manifest scope: %+v %v %v", replayed, again, err)
	}
	reused := in
	reused.canaryRequest = &pilotCanaryEntryRequest{ID: receipt.Request.ID, Lane: autoAUTOPYUSD.Lane, EquityRaw: 999, ExpiresAt: in.Now.Add(10 * time.Minute)}
	if _, _, err = selectPilotCanaryEntry(reused, result, history); err == nil {
		t.Fatal("reused request ID with different content accepted")
	}
	full := pilotCanaryRetainedHistory(t, pilotCanaryReceiptCapacity, in)
	held, blocked, err := selectPilotCanaryEntry(in, result, full)
	if err == nil || blocked != nil || held.Reason != "operator_canary_waiting" {
		t.Fatalf("capacity hold drifted under manifest scope: %+v %v %v", held, blocked, err)
	}
}

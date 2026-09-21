package backyardrwa

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

// TestPilotCanaryRequestValidateOnManifest pins the request lane authority of
// doc 31: the candidate AUTO request is admitted exactly while the explicit
// manifest's plain binding resolves (selectorEntryFundingLane(lane,false)),
// the embedded wrapper keeps refusing it, and every ID, equity and expiry
// negative is identical under both scopes.
func TestPilotCanaryRequestValidateOnManifest(t *testing.T) {
	now := time.Now().UTC().Add(time.Minute)
	request := pilotCanaryEntryRequest{ID: sha256Bytes([]byte("auto-one-acceptance")), Lane: autoAUTOPYUSD.Lane, EquityRaw: 1_000, ExpiresAt: now.Add(5 * time.Minute)}
	if err := request.validateOnManifest(now, autoInitializerFixtureManifest(t)); err != nil {
		t.Fatal("initializer binding refused the candidate request:", err)
	}
	if err := request.validateOnManifest(now, autoFixtureManifest(t)); err != nil {
		t.Fatal("plain binding refused the candidate request:", err)
	}
	embedded := requireEmbeddedInstalledBinding(t)
	if err := request.validate(now); err == nil {
		t.Fatal("embedded wrapper accepted the candidate lane")
	}
	// Both binding states at the same seam: the explicit absent fixture (the
	// shipped pre-install state) refuses the request, while the embedded
	// manifest's installed binding admits it.
	if err := request.validateOnManifest(now, autoAbsentBindingManifest(t)); err == nil {
		t.Fatal("absent binding admitted the candidate request")
	}
	if err := request.validateOnManifest(now, embedded); err != nil {
		t.Fatalf("installed manifest refused the candidate request: %v", err)
	}
	malformed := embedded
	malformed.RuntimeBindings.AutoPolicy = &AutoPolicyBinding{Lane: autoAUTOPYUSD.Lane}
	if err := request.validateOnManifest(now, malformed); err == nil {
		t.Fatal("malformed binding admitted the candidate request")
	}
	installed := pilotCanaryFixture()
	if err := installed.canaryRequest.validateOnManifest(now, autoInitializerFixtureManifest(t)); err != nil {
		t.Fatal("installed lane request refused under manifest scope:", err)
	}
	for name, change := range map[string]func(*pilotCanaryEntryRequest){
		"short id":        func(r *pilotCanaryEntryRequest) { r.ID = "deadbeef" },
		"zero equity":     func(r *pilotCanaryEntryRequest) { r.EquityRaw = 0 },
		"over cap":        func(r *pilotCanaryEntryRequest) { r.EquityRaw = PilotWorkingTrancheCapRaw + 1 },
		"expired":         func(r *pilotCanaryEntryRequest) { r.ExpiresAt = now },
		"overlong window": func(r *pilotCanaryEntryRequest) { r.ExpiresAt = now.Add(16 * time.Minute) },
	} {
		bad := request
		change(&bad)
		if err := bad.validateOnManifest(now, autoFixtureManifest(t)); err == nil {
			t.Fatalf("manifest scope relaxed the %s negative", name)
		}
	}
}

// TestPilotCanaryReadOnManifest pins the parser: identical environment
// variable, unknown-field, trailing-value and size rejections, with only the
// lane authority moving to the manifest scope.
func TestPilotCanaryReadOnManifest(t *testing.T) {
	now := time.Now().UTC()
	valid := `{"id":"` + sha256Bytes([]byte("auto-one-acceptance")) + `","lane":"` + autoAUTOPYUSD.Lane + `","equityRaw":1000000,"expiresAt":"` + now.Add(10*time.Minute).UTC().Format(time.RFC3339Nano) + `"}`
	t.Setenv("BACKYARD_RWA_PILOT_CANARY_ENTRY", valid)
	if got, err := readPilotCanaryEntryRequest(now); err == nil || got != nil {
		t.Fatal("embedded wrapper accepted the candidate request")
	}
	embedded := requireEmbeddedInstalledBinding(t)
	// The explicit absent fixture (the shipped pre-install state) refuses; the
	// embedded manifest's installed binding admits the same well-formed bytes.
	if got, err := readPilotCanaryEntryRequestOnManifest(now, autoAbsentBindingManifest(t)); err == nil || got != nil {
		t.Fatal("absent binding accepted the candidate request")
	}
	if got, err := readPilotCanaryEntryRequestOnManifest(now, embedded); err != nil || got == nil || got.Lane != autoAUTOPYUSD.Lane || got.ID != sha256Bytes([]byte("auto-one-acceptance")) {
		t.Fatalf("installed manifest refused a well-formed candidate request: %+v %v", got, err)
	}
	got, err := readPilotCanaryEntryRequestOnManifest(now, autoFixtureManifest(t))
	if err != nil || got == nil || got.Lane != autoAUTOPYUSD.Lane || got.EquityRaw != 1_000_000 || got.ID != sha256Bytes([]byte("auto-one-acceptance")) {
		t.Fatalf("valid binding refused a well-formed candidate request: %+v %v", got, err)
	}
	for name, raw := range map[string]string{
		"unknown field": strings.Replace(valid, `"equityRaw":1000000`, `"equityRaw":1000000,"extra":1`, 1),
		"trailing":      valid + " {}",
		"oversized":     `{"note":"` + strings.Repeat("x", 600) + `"}`,
	} {
		t.Setenv("BACKYARD_RWA_PILOT_CANARY_ENTRY", raw)
		if got, err := readPilotCanaryEntryRequestOnManifest(now, autoFixtureManifest(t)); err == nil || got != nil {
			t.Fatalf("manifest scope relaxed the %s rejection", name)
		}
	}
	t.Setenv("BACKYARD_RWA_PILOT_CANARY_ENTRY", "")
	if got, err := readPilotCanaryEntryRequestOnManifest(now, autoFixtureManifest(t)); got != nil || err != nil {
		t.Fatal("absent environment variable must stay a nil request")
	}
}

// TestPilotCanarySelectOnManifest pins the forced-acceptance call: the
// manifest scope validates BOTH the request and the constructed SelectorEntry
// against the explicit manifest, the public wrapper keeps refusing the
// candidate lane, and consumed-ID, capacity and receipt semantics are the
// exact installed ones.
func TestPilotCanarySelectOnManifest(t *testing.T) {
	in := autoCanaryFixture(t)
	result := SelectorResult{Candidates: []CandidateForecast{{Lane: autoAUTOPYUSD.Lane, CostsKnown: true}}}
	if _, _, err := selectPilotCanaryEntry(in, result, nil); err == nil {
		t.Fatal("public wrapper accepted the candidate acceptance request")
	}
	embedded := requireEmbeddedInstalledBinding(t)
	// The explicit absent fixture (the shipped pre-install state) refuses the
	// acceptance; the embedded manifest's installed binding is
	// initializer-complete, so it produces the acceptance the release ships.
	if _, _, err := selectPilotCanaryEntryOnManifest(in, result, nil, autoAbsentBindingManifest(t)); err == nil {
		t.Fatal("absent binding accepted the candidate acceptance request")
	}
	installedAccepted, installedReceipt, err := selectPilotCanaryEntryOnManifest(in, result, nil, embedded)
	if err != nil || installedAccepted.Action != "CANARY_ENTER" || installedReceipt == nil {
		t.Fatalf("installed manifest refused the canary acceptance: %+v %v %v", installedAccepted, installedReceipt, err)
	}
	if installedReceipt.Request != *in.canaryRequest || installedReceipt.QuoteEvidenceID != in.Quotes[0].EvidenceID {
		t.Fatalf("installed receipt identity drifted: %+v", installedReceipt)
	}
	// Funding scope admits the REQUEST (plain binding), but the constructed
	// entry is still validated against the explicit manifest's initializer
	// scope, so a binding without the appended initialize constraint never
	// produces an entry.
	if _, _, err := selectPilotCanaryEntryOnManifest(in, result, nil, autoFixtureManifest(t)); err == nil || err.Error() != "invalid_selector_entry" {
		t.Fatalf("initializer-less binding did not refuse the entry exactly: %v", err)
	}
	accepted, receipt, err := selectPilotCanaryEntryOnManifest(in, result, nil, autoInitializerFixtureManifest(t))
	if err != nil || accepted.Action != "CANARY_ENTER" || accepted.SelectedQuote == nil || receipt == nil {
		t.Fatalf("complete binding refused the canary acceptance: %+v %v %v", accepted, receipt, err)
	}
	// The installed non-USDC borrow checks gate the candidate lane exactly as
	// shipped: missing debt price evidence, a depegged price that values the
	// borrow above equity, and a stale price each refuse the constructed entry.
	missing := autoCanaryFixtureAt(t, 1_100_000)
	missing.Quotes[0].DebtPrice = nil
	if _, _, err := selectPilotCanaryEntryOnManifest(missing, result, nil, autoInitializerFixtureManifest(t)); err == nil || err.Error() != "invalid_selector_entry" {
		t.Fatalf("missing debt price did not refuse the entry exactly: %v", err)
	}
	depegged := autoCanaryFixtureAt(t, 3_000_000)
	if _, _, err := selectPilotCanaryEntryOnManifest(depegged, result, nil, autoInitializerFixtureManifest(t)); err == nil || err.Error() != "invalid_selector_entry" {
		t.Fatalf("depegged debt price did not refuse the entry exactly: %v", err)
	}
	stale := autoCanaryFixtureAt(t, 1_100_000)
	stalePrice := copyDebtPrice(stale.Quotes[0].DebtPrice)
	stalePrice.ObservedSlot = stale.Quotes[0].SampleSlot - 1
	stale.Quotes[0].DebtPrice = stalePrice
	if _, _, err := selectPilotCanaryEntryOnManifest(stale, result, nil, autoInitializerFixtureManifest(t)); err == nil || err.Error() != "invalid_selector_entry" {
		t.Fatalf("stale debt price did not refuse the entry exactly: %v", err)
	}
	if receipt.Request != *in.canaryRequest || receipt.QuoteEvidenceID != in.Quotes[0].EvidenceID || receipt.AcceptedAt != in.Now {
		t.Fatalf("receipt identity drifted: %+v", receipt)
	}
	history := map[string]pilotCanaryEntryReceipt{receipt.Request.ID: *receipt}
	replayed, again, err := selectPilotCanaryEntryOnManifest(in, result, history, autoInitializerFixtureManifest(t))
	if err != nil || again != nil || replayed.Action != "KEEP" || replayed.Reason != "operator_canary_already_consumed" {
		t.Fatalf("consumed-ID check drifted under manifest scope: %+v %v %v", replayed, again, err)
	}
	reused := in
	reused.canaryRequest = &pilotCanaryEntryRequest{ID: receipt.Request.ID, Lane: autoAUTOPYUSD.Lane, EquityRaw: 999, ExpiresAt: in.Now.Add(10 * time.Minute)}
	if _, _, err = selectPilotCanaryEntryOnManifest(reused, result, history, autoInitializerFixtureManifest(t)); err == nil {
		t.Fatal("reused request ID with different content accepted")
	}
	full := pilotCanaryRetainedHistory(t, pilotCanaryReceiptCapacity, in)
	held, blocked, err := selectPilotCanaryEntryOnManifest(in, result, full, autoInitializerFixtureManifest(t))
	if err == nil || blocked != nil || held.Reason != "operator_canary_waiting" {
		t.Fatalf("capacity hold drifted under manifest scope: %+v %v %v", held, blocked, err)
	}
}

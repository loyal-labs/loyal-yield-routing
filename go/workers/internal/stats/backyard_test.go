package stats

import (
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// backyard.askloyal.com is public and partner-facing: it must serve only its
// page, its API and static files, never the stats page or other stats APIs.
func TestBackyardHostServesOnlyItsPage(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := backyardHostOnly(ok)
	for _, tc := range []struct {
		host, path string
		status     int
	}{
		{"backyard.askloyal.com", "/", 200},
		{"backyard.askloyal.com", "/api/backyard", 200},
		{"backyard.askloyal.com", "/assets/index-abc.js", 200},
		{"backyard.askloyal.com", "/app-icon.svg", 200},
		{"backyard.askloyal.com", "/api/earn/vaults", 404},
		{"backyard.askloyal.com", "/api/earn/stats", 404},
		{"backyard.askloyal.com", "/api/earn/growth-metrics", 404},
		{"backyard.askloyal.com", "/backyard", 404},
		{"stats.askloyal.com", "/api/earn/vaults", 200},
		{"stats.askloyal.com", "/backyard", 200},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, tc.path, nil)
		r.Host = tc.host
		handler.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s%s: got %d, want %d", tc.host, tc.path, w.Code, tc.status)
		}
	}
}

func TestBackyardRiskUsesMarketLimitsAndRejectsUnknown(t *testing.T) {
	now := time.Date(2026, 11, 9, 17, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		lane                      string
		borrow, liquidation, hard float64
	}{{"OnRe/ONyc/USDC", .66, .75, .63}, {"AUTO/AUTO/PYUSD", .78, .80, .70}, {"Maple/syrupUSDC/USDC", .88, .90, .80}} {
		point := backyardReserveDay{at: now.Add(-30 * time.Minute).Format(time.RFC3339), maxLtv: &tc.borrow, liquidation: &tc.liquidation}
		risk, err := backyardRiskFromPoint(tc.lane, point, now)
		if err != nil || risk.HardLtvRatio != tc.hard || risk.LiquidationRatio != tc.liquidation {
			t.Fatalf("%s: %+v, %v", tc.lane, risk, err)
		}
		if _, err := backyardRiskFromPoint(tc.lane, point, now.Add(2*time.Hour)); err == nil {
			t.Fatal("stale market limits must be unavailable")
		}
		point.liquidation = nil
		if _, err := backyardRiskFromPoint(tc.lane, point, now); err == nil {
			t.Fatal("missing liquidation threshold must not use a default")
		}
	}
}

func TestBackyardSourceMintDecimals(t *testing.T) {
	token, decimals := backyardToken("5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5")
	if token == nil || decimals == nil || *token != "ONyc" || *decimals != 9 {
		t.Fatal("ONyc transfers must be scaled by nine mint decimals")
	}
	if token, decimals := backyardToken("unrecognized-mint"); token != nil || decimals != nil {
		t.Fatal("unknown mints must not be scaled")
	}
}

func TestBackyardMoveInputKeepsAmountAndMintTogether(t *testing.T) {
	amount, mint := backyardMoveInput([]byte(`{"kind":"cross-mint-swap","accounts":[{"mint":"ONyc-mint","beforeRaw":2000000000,"afterRaw":1000000000},{"mint":"USDC-mint","beforeRaw":0,"afterRaw":1100000,"minimumAfterRaw":1100000}]}`))
	if amount == nil || mint == nil || *amount != "1000000000" || *mint != "ONyc-mint" {
		t.Fatalf("source transfer: %v %v", amount, mint)
	}
	amount, _ = backyardMoveInput([]byte(`{"accounts":[{"mint":"ONyc-mint","beforeRaw":18446744073709551615,"afterRaw":1}]}`))
	if amount == nil || *amount != "18446744073709551614" {
		t.Fatal("raw input must retain all uint64 digits")
	}
	// This live withdrawal's decision was 5054099 USDC micros, not ONyc raw.
	for _, raw := range []string{
		`{"kind":"atomic-step","accounts":[{"mint":"ONyc-mint","beforeRaw":0,"afterRaw":0,"minimumAfterRaw":0},{"mint":"USDC-mint","beforeRaw":0,"afterRaw":5017856,"minimumAfterRaw":5017856}]}`,
		`{"kind":"atomic-step","accounts":[{"mint":"ONyc-mint","beforeRaw":1000000000,"afterRaw":0}]}`,
		`{"accounts":[{"mint":"USDC-mint","beforeRaw":0,"afterRaw":5017856,"minimumAfterRaw":5017856}]}`,
		`null`, `invalid`,
	} {
		if amount, mint := backyardMoveInput([]byte(raw)); amount != nil || mint != nil {
			t.Fatal("unknown or multi-leg inputs must not become a single-token amount")
		}
	}
}

// A landed NAV report is read from the vault adaptor's verified return bytes,
// and otherwise from the built request; another program's bytes are ignored.
func TestBackyardReportNAV(t *testing.T) {
	bytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(bytes, 17_079_029)
	program, data := backyardNAVAdaptor, base64.StdEncoding.EncodeToString(bytes)
	if nav, ok := backyardReportNAV(&program, &data, nil); !ok || nav != 17_079_029 {
		t.Fatalf("adaptor return data: got %v %v", nav, ok)
	}
	request := base64.StdEncoding.EncodeToString([]byte(`{"Report":{"NAVAfterRaw":17066770}}`))
	other := "FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW" // the RWA vault's NAV adaptor
	if nav, ok := backyardReportNAV(&other, &data, &request); !ok || nav != 17_066_770 {
		t.Fatalf("request fallback: got %v %v", nav, ok)
	}
	if _, ok := backyardReportNAV(nil, nil, nil); ok {
		t.Fatal("a report with nothing to read must not yield a NAV")
	}
}

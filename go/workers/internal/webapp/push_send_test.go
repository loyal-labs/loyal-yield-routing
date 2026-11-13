package webapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type capturedPush struct {
	method string
	header http.Header
	body   map[string]any
}

// oneSignalStub answers each request with the next canned reply (the last one repeats).
func oneSignalStub(t *testing.T, replies ...struct {
	status int
	body   string
}) (*WalletPush, func() []capturedPush) {
	t.Helper()
	var mu sync.Mutex
	var got []capturedPush
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
		}
		mu.Lock()
		i := len(got)
		got = append(got, capturedPush{method: r.Method, header: r.Header.Clone(), body: body})
		mu.Unlock()
		reply := replies[min(i, len(replies)-1)]
		w.WriteHeader(reply.status)
		_, _ = w.Write([]byte(reply.body))
	}))
	t.Cleanup(server.Close)
	p := NewWalletPush(nil, OneSignalConfig{
		AppID:             "test-app",
		RESTAPIKey:        "test-key",
		EarningsChannelID: "chan-earnings",
		AlertsChannelID:   "chan-alerts",
	}, "secret")
	p.url = server.URL + "/notifications?c=push"
	p.expoURL = server.URL + "/expo"
	p.retryDelay = 0
	// Every wallet has one registered subscription, "sub-<wallet>".
	p.subscriptions = func(_ context.Context, wallet string) ([]string, error) {
		return []string{"sub-" + wallet}, nil
	}
	p.expoTokens = func(context.Context, string) ([]string, error) { return nil, nil }
	return p, func() []capturedPush {
		mu.Lock()
		defer mu.Unlock()
		return append([]capturedPush(nil), got...)
	}
}

type reply = struct {
	status int
	body   string
}

func TestOneSignalEarningsRequestShape(t *testing.T) {
	p, requests := oneSignalStub(t, reply{200, `{"id":"n-1"}`})
	ok := p.send(context.Background(), "W1", autodepositWalletPush{
		Title: "+$1.37 earned", Body: "$48.20 earned with Loyal so far.",
		Category: pushCategoryEarnings, Delivery: pushDeliveryIntelligent,
		Route: "/(tabs)/earn", IdempotencyName: "earnings:yield-digest:W1:2026-11-09",
	})
	if !ok {
		t.Fatal("send = false, want true")
	}
	got := requests()
	if len(got) != 1 {
		t.Fatalf("requests = %d, want 1", len(got))
	}
	r := got[0]
	if r.method != http.MethodPost {
		t.Errorf("method = %s", r.method)
	}
	if h := r.header.Get("Authorization"); h != "Key test-key" {
		t.Errorf("Authorization = %q", h)
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
	b := r.body
	if b["app_id"] != "test-app" || b["target_channel"] != "push" {
		t.Errorf("app_id/target_channel = %v/%v", b["app_id"], b["target_channel"])
	}
	if ids, _ := b["include_subscription_ids"].([]any); len(ids) != 1 || ids[0] != "sub-W1" {
		t.Errorf("include_subscription_ids = %v", b["include_subscription_ids"])
	}
	if _, present := b["include_aliases"]; present {
		t.Error("include_aliases present: the client-chosen external_id must never be a target")
	}
	if h, _ := b["headings"].(map[string]any); h["en"] != "+$1.37 earned" {
		t.Errorf("headings = %v", b["headings"])
	}
	if c, _ := b["contents"].(map[string]any); c["en"] != "$48.20 earned with Loyal so far." {
		t.Errorf("contents = %v", b["contents"])
	}
	if d, _ := b["data"].(map[string]any); d["route"] != "/(tabs)/earn" {
		t.Errorf("data = %v", b["data"])
	}
	if b["android_channel_id"] != "chan-earnings" {
		t.Errorf("android_channel_id = %v", b["android_channel_id"])
	}
	if b["delayed_option"] != "last-active" {
		t.Errorf("delayed_option = %v", b["delayed_option"])
	}
	if b["idempotency_key"] != pushIdempotencyKey("earnings:yield-digest:W1:2026-11-09") {
		t.Errorf("idempotency_key = %v", b["idempotency_key"])
	}
}

func TestOneSignalAlertDefaultsAreImmediateAlerts(t *testing.T) {
	p, requests := oneSignalStub(t, reply{200, `{"id":"n-1"}`})
	if !p.send(context.Background(), "W1", autodepositWalletPush{Title: "t", Body: "b"}) {
		t.Fatal("send = false")
	}
	b := requests()[0].body
	if b["android_channel_id"] != "chan-alerts" {
		t.Errorf("android_channel_id = %v", b["android_channel_id"])
	}
	for _, key := range []string{"delayed_option", "data"} {
		if _, present := b[key]; present {
			t.Errorf("%s present, want absent", key)
		}
	}
	key, _ := b["idempotency_key"].(string)
	if _, err := uuid.Parse(key); err != nil {
		t.Errorf("idempotency_key %q: %v", key, err)
	}
}

func TestOneSignalOmitsChannelWhenUnset(t *testing.T) {
	p, requests := oneSignalStub(t, reply{200, `{"id":"n-1"}`})
	p.cfg.EarningsChannelID = ""
	p.send(context.Background(), "W1", autodepositWalletPush{Title: "t", Body: "b", Category: pushCategoryEarnings})
	if _, present := requests()[0].body["android_channel_id"]; present {
		t.Error("android_channel_id present, want absent")
	}
}

func TestOneSignalNotSubscribedIsNotSent(t *testing.T) {
	p, _ := oneSignalStub(t, reply{200, `{"id":"","errors":["All included players are not subscribed"]}`})
	if p.send(context.Background(), "W1", autodepositWalletPush{Title: "t", Body: "b"}) {
		t.Error("send = true, want false")
	}
	sent, err := p.sendToWallet(context.Background(), "W1", autodepositWalletPush{Title: "t", Body: "b"}, nil)
	if sent || err != nil {
		t.Errorf("sendToWallet = (%v, %v), want (false, nil)", sent, err)
	}
}

func TestOneSignalSkipsWalletWithoutRegisteredSubscription(t *testing.T) {
	p, requests := oneSignalStub(t, reply{200, `{"id":"n-1"}`})
	p.subscriptions = func(context.Context, string) ([]string, error) { return nil, nil }
	sent, err := p.sendToWallet(context.Background(), "W1", autodepositWalletPush{Title: "t", Body: "b"}, nil)
	if sent || err != nil {
		t.Errorf("sendToWallet = (%v, %v), want (false, nil)", sent, err)
	}
	p.subscriptions = func(context.Context, string) ([]string, error) { return nil, errors.New("db down") }
	if _, err := p.sendToWallet(context.Background(), "W1", autodepositWalletPush{Title: "t", Body: "b"}, nil); err == nil {
		t.Error("lookup failure: want error")
	}
	if n := len(requests()); n != 0 {
		t.Errorf("requests = %d, want 0", n)
	}
}

func TestPushTokenProviderValidation(t *testing.T) {
	const sub = "3f2b8c1e-9d4a-4e7b-8c6f-1a2b3c4d5e6f"
	for _, tc := range []struct {
		raw, provider, token string
		refused              *PushError
	}{
		{`{"token":"ExponentPushToken[x]","platform":"ios"}`, "expo", "ExponentPushToken[x]", nil},
		{`{"token":"ExpoPushToken[x]","platform":"android","provider":"expo"}`, "expo", "ExpoPushToken[x]", nil},
		{`{"token":"` + sub + `","platform":"ios","provider":"expo"}`, "expo", sub, nil},
		{`{"token":"not-a-push-token","platform":"android","provider":"expo"}`, "", "", errPushToken},
		{`{"token":"ExpoPushToken[]","platform":"android"}`, "", "", errPushToken},
		{`{"token":"` + sub + `","platform":"android","provider":"onesignal"}`, "onesignal", sub, nil},
		{`{"token":"` + strings.ToUpper(sub) + `","platform":"android","provider":"onesignal"}`, "onesignal", sub, nil},
		{`{"token":"W1","platform":"android","provider":"onesignal"}`, "", "", errPushToken},
		{`{"token":"{` + sub + `}","platform":"android","provider":"onesignal"}`, "", "", errPushToken},
		{`{"token":"` + sub + `","platform":"android","provider":"fcm"}`, "", "", errPushToken},
	} {
		body, refused := pushFields([]byte(tc.raw))
		if refused != tc.refused {
			t.Errorf("%s: refused = %v, want %v", tc.raw, refused, tc.refused)
			continue
		}
		if refused == nil && (body.Provider != tc.provider || body.Token != tc.token) {
			t.Errorf("%s: got (%s, %s), want (%s, %s)", tc.raw, body.Provider, body.Token, tc.provider, tc.token)
		}
	}
}

func TestOneSignalRetriesOnceWithSameKey(t *testing.T) {
	p, requests := oneSignalStub(t, reply{503, `unavailable`}, reply{200, `{"id":"n-2"}`})
	if !p.send(context.Background(), "W1", autodepositWalletPush{Title: "t", Body: "b"}) {
		t.Fatal("send = false")
	}
	got := requests()
	if len(got) != 2 {
		t.Fatalf("requests = %d, want 2", len(got))
	}
	if got[0].body["idempotency_key"] != got[1].body["idempotency_key"] {
		t.Error("idempotency keys differ across retry")
	}

	p, requests = oneSignalStub(t, reply{400, `{"errors":["bad"]}`})
	_, err := p.sendToWallet(context.Background(), "W1", autodepositWalletPush{Title: "t", Body: "b"}, nil)
	if err == nil {
		t.Fatal("want error on 400")
	}
	if strings.Contains(err.Error(), "test-key") {
		t.Errorf("error leaks key: %v", err)
	}
	if n := len(requests()); n != 1 {
		t.Errorf("requests on 400 = %d, want 1", n)
	}
}

func TestOneSignalNeverFallsBackToExpo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		reply reply
	}{
		{"accepted", reply{200, `{"id":"accepted"}`}},
		{"unsubscribed", reply{200, `{"id":""}`}},
		{"provider failure", reply{503, `unavailable`}},
		{"ambiguous response", reply{200, `{`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			push, _ := oneSignalStub(t, tc.reply)
			expoLookups := 0
			push.expoTokens = func(context.Context, string) ([]string, error) {
				expoLookups++
				return []string{"ExpoPushToken[legacy]"}, nil
			}
			_, _ = push.sendToWallet(context.Background(), "W1", autodepositWalletPush{Title: "t", Body: "b"}, nil)
			if expoLookups != 0 {
				t.Fatalf("OneSignal result triggered %d Expo lookups", expoLookups)
			}
			push.subscriptions = func(context.Context, string) ([]string, error) { return nil, errors.New("lookup failed") }
			if _, err := push.sendToWallet(context.Background(), "W1", autodepositWalletPush{Title: "t", Body: "b"}, nil); err == nil || expoLookups != 0 {
				t.Fatalf("failed lookup: err=%v, Expo lookups=%d", err, expoLookups)
			}
		})
	}
}

func TestPushIdempotencyKeyIsStableUUID(t *testing.T) {
	if pushIdempotencyKey("a") != pushIdempotencyKey("a") {
		t.Error("not stable")
	}
	if pushIdempotencyKey("a") == pushIdempotencyKey("b") {
		t.Error("collision")
	}
	if _, err := uuid.Parse(pushIdempotencyKey("a")); err != nil {
		t.Error(err)
	}
}

func debugSend(t *testing.T, router http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, walletPushPath,
		strings.NewReader(`{"walletPublicKey":"W1","title":"t","body":"b","data":{"k":"v"}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestDebugSendGoesThroughOneSignalAsAlert(t *testing.T) {
	p, requests := oneSignalStub(t, reply{200, `{"id":"n-1"}`}, reply{200, `{"id":""}`})
	p.Secret = "s3cret"
	router := chi.NewRouter()
	registerWalletPush(humachi.New(router, apiConfig()), p)

	rec := debugSend(t, router)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"success":true,"sentTo":1}` {
		t.Fatalf("sent: %d %s", rec.Code, rec.Body.String())
	}
	b := requests()[0].body
	if b["android_channel_id"] != "chan-alerts" {
		t.Errorf("android_channel_id = %v", b["android_channel_id"])
	}
	if _, present := b["delayed_option"]; present {
		t.Error("delayed_option present, want absent")
	}
	if d, _ := b["data"].(map[string]any); d["k"] != "v" {
		t.Errorf("data = %v", b["data"])
	}

	rec = debugSend(t, router)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusNotFound || got["error"] != "No push tokens registered for this wallet" || got["sentTo"] != float64(0) {
		t.Fatalf("no subscription: %d %s", rec.Code, rec.Body.String())
	}
}

// Mixpanel cohorts for retired campaigns are refused before any wallet is pushed.
func TestRetiredCohortCampaignIsRefusedWithoutSending(t *testing.T) {
	push, requests := oneSignalStub(t, struct {
		status int
		body   string
	}{200, `{"id":"n1"}`})
	h := earnRoutes{Earn: Earn{MixpanelCohortSecret: "cohort-secret"}}
	h.CampaignPush = push
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(":cohort-secret"))
	for _, name := range []string{"earning-while-away", "still-earning"} {
		body := `{"action":"add_members","parameters":{"mixpanel_cohort_name":"push: ` + name + `","members":[{"mixpanel_distinct_id":"mob:W1"}]}}`
		_, err := h.mixpanelCohort(context.Background(), &mixpanelCohortInput{Authorization: auth, Raw: rawJSON{data: []byte(body)}})
		var rejected *campaignRejected
		if !errors.As(err, &rejected) || rejected.GetStatus() != http.StatusBadRequest {
			t.Fatalf("%s: err = %v, want a 400", name, err)
		}
	}
	if got := requests(); len(got) != 0 {
		t.Fatalf("sent %d pushes for retired campaigns", len(got))
	}
}

func TestCohortAndSweepPushesAreImmediateAlerts(t *testing.T) {
	all := map[string]autodepositWalletPush{
		"sweep-failed-0":    autodepositSweepFailedPush(0),
		"sweep-failed-5m":   autodepositSweepFailedPush(5_000_000),
		"enabled":           autodepositEnabledPush(),
		"sweep-scheduled-1": autodepositSweepScheduledPush(1),
	}
	for name, p := range mixpanelCohortCampaigns {
		all["cohort:"+name] = p
	}
	for name, p := range all {
		if p.Category != pushCategoryAlerts || p.Delivery != pushDeliveryNow {
			t.Errorf("%s: category=%v delivery=%v", name, p.Category, p.Delivery)
		}
	}
}

func TestCohortSendsCarryDistinctIdempotencyKeys(t *testing.T) {
	p, requests := oneSignalStub(t, reply{200, `{"id":"n-1"}`})
	shared := mixpanelCohortCampaigns["quest-1-nudge"]
	wallets := []string{"W1", "W2", "W3", "W4", "W5"}
	var wg sync.WaitGroup
	for _, w := range wallets {
		wg.Go(func() { p.send(context.Background(), w, cohortPush(shared, "quest-1-nudge", w)) })
	}
	wg.Wait()
	if shared.IdempotencyName != "" {
		t.Fatalf("shared payload mutated: %q", shared.IdempotencyName)
	}
	want := map[string]bool{}
	for _, w := range wallets {
		want[pushIdempotencyKey("cohort:quest-1-nudge:"+w)] = true
	}
	got := requests()
	if len(got) != len(wallets) {
		t.Fatalf("requests = %d", len(got))
	}
	for _, r := range got {
		key, _ := r.body["idempotency_key"].(string)
		if !want[key] {
			t.Errorf("unexpected or duplicate key %q", key)
		}
		delete(want, key)
	}
}

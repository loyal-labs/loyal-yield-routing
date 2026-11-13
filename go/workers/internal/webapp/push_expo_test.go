package webapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestExpoCompatibilityPayloadAndProviderIsolation(t *testing.T) {
	push, oneSignalRequests := oneSignalStub(t, reply{200, `{"id":"unused"}`})
	push.subscriptions = func(context.Context, string) ([]string, error) { return nil, nil }
	push.expoTokens = func(context.Context, string) ([]string, error) { return []string{"ExponentPushToken[legacy]"}, nil }
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" {
			t.Error("Expo request has wrong method or leaked OneSignal authorization")
		}
		var body []struct {
			To, Title, Body, Sound string
			Data                   map[string]string
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 {
			t.Errorf("Expo body = %+v, %v", body, err)
		} else if m := body[0]; m.To != "ExponentPushToken[legacy]" || m.Title != "+$2 earned" || m.Body != "Test earnings" || m.Sound != "default" || m.Data["route"] != "/(tabs)/earn" {
			t.Errorf("Expo message = %+v", m)
		}
		_, _ = w.Write([]byte(`{"data":[{"status":"ok","id":"receipt-1"}]}`))
	}))
	t.Cleanup(server.Close)
	push.expoURL = server.URL
	if registered, err := push.registered(context.Background(), "W1"); !registered || err != nil {
		t.Fatalf("Expo-only wallet registered = %v, %v", registered, err)
	}
	sent, err := push.sendToWallet(context.Background(), "W1", autodepositWalletPush{Title: "+$2 earned", Body: "Test earnings", Category: pushCategoryEarnings, Delivery: pushDeliveryIntelligent, Route: "/(tabs)/earn"}, nil)
	if !sent || err != nil || calls.Load() != 1 || len(oneSignalRequests()) != 0 {
		t.Fatalf("sent=%v err=%v Expo=%d OneSignal=%d", sent, err, calls.Load(), len(oneSignalRequests()))
	}
}

func TestExpoFailureNeverRetriesOrReportsAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"server failure", 503, `unavailable`},
		{"ambiguous JSON", 200, `{`},
		{"missing ticket", 200, `{"data":[]}`},
		{"missing receipt", 200, `{"data":[{"status":"ok"}]}`},
		{"invalid status", 200, `{"data":[{"status":"unknown"}]}`},
		{"rejected payload", 200, `{"data":[{"status":"error","details":{"error":"MessageTooBig"}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			push, oneSignalRequests := oneSignalStub(t, reply{200, `{"id":"unused"}`})
			push.subscriptions = func(context.Context, string) ([]string, error) { return nil, nil }
			push.expoTokens = func(context.Context, string) ([]string, error) { return []string{"ExpoPushToken[legacy]"}, nil }
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(server.Close)
			push.expoURL = server.URL
			sent, err := push.sendToWallet(context.Background(), "W1", autodepositWalletPush{Title: "t", Body: "b"}, nil)
			if sent || err == nil || calls.Load() != 1 || len(oneSignalRequests()) != 0 {
				t.Fatalf("sent=%v err=%v Expo=%d OneSignal=%d", sent, err, calls.Load(), len(oneSignalRequests()))
			}
		})
	}
}

func TestExpoPartialAcceptancePreservesAcceptedOutcome(t *testing.T) {
	push, _ := oneSignalStub(t, reply{200, `{"id":"unused"}`})
	push.subscriptions = func(context.Context, string) ([]string, error) { return nil, nil }
	push.expoTokens = func(context.Context, string) ([]string, error) {
		return []string{"ExpoPushToken[good]", "ExpoPushToken[bad]"}, nil
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"status":"ok","id":"accepted"},{"status":"error","details":{"error":"MessageRateExceeded"}}]}`))
	}))
	t.Cleanup(server.Close)
	push.expoURL = server.URL
	if !push.send(context.Background(), "W1", autodepositWalletPush{Title: "t", Body: "b"}) {
		t.Fatal("one rejected device hid the accepted device from the claim/analytics caller")
	}
}

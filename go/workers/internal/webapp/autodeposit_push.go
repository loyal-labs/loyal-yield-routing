package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// Wallet push bridge, ported from
// web/apps/web/src/lib/push-notifications/wallet-push.server.ts (sendWalletPush
// :18, autodepositEnabledPush :91, autodepositSweepScheduledPush :98). Expo
// push tokens register per wallet on the app backend, so Earn delivers pushes
// through its authenticated send endpoint. The DECIDED pathmap.md:14 row keeps
// the mobile route's sendWalletPush call inside the Go confirm op. Like the
// TS, a missing configuration or a failed send never fails the confirm.
type autodepositWalletPush struct {
	Body  string `json:"body"`
	Title string `json:"title"`
}

const autodepositPushSendTimeout = 5 * time.Second

func sendAutodepositWalletPush(ctx context.Context, walletAddress string, payload autodepositWalletPush) {
	baseURL := os.Getenv("MOBILE_PUSH_API_BASE_URL")
	secret := os.Getenv("MOBILE_PUSH_API_SECRET")
	if baseURL == "" || secret == "" {
		return
	}
	body, err := json.Marshal(map[string]string{
		"body":            payload.Body,
		"title":           payload.Title,
		"walletPublicKey": walletAddress,
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, autodepositPushSendTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/api/push-tokens/debug-send", bytes.NewReader(body))
	if err != nil {
		return
	}
	request.Header.Set("Authorization", "Bearer "+secret)
	request.Header.Set("Content-Type", "application/json")
	// A 404 means no devices registered for this wallet (web-only user).
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		slog.WarnContext(ctx, "[wallet-push] push send failed", "errorMessage", err.Error(), "walletAddress", walletAddress)
		return
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		if response.StatusCode != http.StatusNotFound {
			slog.WarnContext(ctx, "[wallet-push] push send rejected",
				"status", response.StatusCode, "walletAddress", walletAddress)
		}
	}
}

// autodepositEnabledPush copy comes from the mobile confirm route.
func autodepositEnabledPush() autodepositWalletPush {
	return autodepositWalletPush{
		Body:  "Your idle USDC will be moved to Earn automatically, roughly once an hour.",
		Title: "Auto-deposit is on.",
	}
}

// autodepositSweepScheduledPush announces the bootstrap sweep; a null or
// non-positive amount renders as the plain token symbol.
func autodepositSweepScheduledPush(amountRaw uint64) autodepositWalletPush {
	amount := "USDC"
	if amountRaw > 0 {
		amount = formatAutodepositUSDC(amountRaw)
	}
	return autodepositWalletPush{
		Body:  "Heading to yield in ~1 hour. Adjust autodeposit limits if you need it liquid.",
		Title: fmt.Sprintf("%s about to move to EARN.", amount),
	}
}

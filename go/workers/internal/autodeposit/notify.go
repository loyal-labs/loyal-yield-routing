package autodeposit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const sweepNotifyTimeout = 5 * time.Second

// SweepNotifier tells the app that a scheduled sweep did not land, so the app
// can push the wallet's devices (ASK-2091). The app's sent-log keeps one push
// per dedupeKey, which is what keeps every retry of one stuck slot down to a
// single push. A nil notifier is the disabled one.
type SweepNotifier struct {
	endpoint, secret string
	client           *http.Client
}

// NewSweepNotifier binds the app endpoint and its bearer secret.
func NewSweepNotifier(endpoint, secret string) (*SweepNotifier, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
		return nil, errors.New("sweep notify endpoint must be an absolute HTTPS URL without user info")
	}
	if secret == "" {
		return nil, errors.New("sweep notify secret is required")
	}
	return &SweepNotifier{endpoint: endpoint, secret: secret, client: &http.Client{Timeout: sweepNotifyTimeout}}, nil
}

// NotifyFailed reports one failed scheduled slot exactly as the TS executor's
// notifyFailedSweep did: {walletAddress, kind: "failed", dedupeKey: "slot-<id>"}
// with no amount. The result is logged and never changes the sweep's outcome;
// it reports whether the app accepted the push.
func (n *SweepNotifier) NotifyFailed(ctx context.Context, wallet string, scheduledSlotID int64) bool {
	if n == nil {
		return false
	}
	if scheduledSlotID <= 0 {
		slog.Warn("autodeposit sweep notify skipped", "event", "solana_week_sweep_notify", "status", "skipped", "reason", "no_scheduled_slot")
		return false
	}
	body, err := json.Marshal(map[string]string{"walletAddress": wallet, "kind": "failed", "dedupeKey": fmt.Sprintf("slot-%d", scheduledSlotID)})
	if err != nil {
		return false
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, n.endpoint, bytes.NewReader(body))
	if err != nil {
		return false
	}
	request.Header.Set("Authorization", "Bearer "+n.secret)
	request.Header.Set("Content-Type", "application/json")
	response, err := n.client.Do(request)
	if err != nil {
		// The endpoint is a credential: log the cause, never the URL.
		var urlErr *url.Error
		message := err.Error()
		if errors.As(err, &urlErr) {
			message = urlErr.Err.Error()
			if urlErr.Timeout() {
				message = fmt.Sprintf("notification timed out after %dms", sweepNotifyTimeout.Milliseconds())
			}
		}
		slog.Warn("autodeposit sweep notify failed", "event", "solana_week_sweep_notify", "status", "failed", "httpStatus", nil, "error", message)
		return false
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		text, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		message := strings.TrimSpace(string(text))
		if len(message) > 200 {
			message = message[:200]
		}
		if message == "" {
			message = http.StatusText(response.StatusCode)
		}
		slog.Warn("autodeposit sweep notify failed", "event", "solana_week_sweep_notify", "status", "failed", "httpStatus", response.StatusCode, "error", message)
		return false
	}
	slog.Info("autodeposit sweep notify sent", "event", "solana_week_sweep_notify", "status", "sent", "httpStatus", response.StatusCode)
	return true
}

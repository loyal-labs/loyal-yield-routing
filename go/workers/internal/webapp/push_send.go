package webapp

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

const (
	walletPushPath    = "/api/push-tokens/debug-send"
	walletPushTimeout = 30 * time.Second
	oneSignalSendURL  = "https://api.onesignal.com/notifications?c=push"
	oneSignalMaxBody  = 1 << 20
	oneSignalRetryGap = time.Second
)

// pushIdempotencyNamespace is the fixed namespace for deterministic idempotency keys.
var pushIdempotencyNamespace = uuid.MustParse("5b1f3c0e-7a52-4d8e-9c64-2f0a8e6b1d37")

type pushCategory int

const (
	pushCategoryAlerts pushCategory = iota
	pushCategoryEarnings
)

type pushDelivery int

const (
	pushDeliveryNow pushDelivery = iota
	pushDeliveryIntelligent
)

type OneSignalConfig struct {
	AppID, RESTAPIKey, EarningsChannelID, AlertsChannelID string
}

type WalletPush struct {
	Secret string
	HTTP   *http.Client

	cfg        OneSignalConfig
	url        string
	retryDelay time.Duration
}

func NewWalletPush(cfg OneSignalConfig, secret string) *WalletPush {
	return &WalletPush{Secret: secret, HTTP: http.DefaultClient, cfg: cfg, url: oneSignalSendURL, retryDelay: oneSignalRetryGap}
}

func pushIdempotencyKey(name string) string {
	if name == "" {
		return uuid.NewString()
	}
	return uuid.NewSHA1(pushIdempotencyNamespace, []byte(name)).String()
}

type oneSignalAliases struct {
	ExternalID []string `json:"external_id"`
}

type oneSignalText struct {
	EN string `json:"en"`
}

type oneSignalRequest struct {
	AppID            string           `json:"app_id"`
	TargetChannel    string           `json:"target_channel"`
	IncludeAliases   oneSignalAliases `json:"include_aliases"`
	Headings         oneSignalText    `json:"headings"`
	Contents         oneSignalText    `json:"contents"`
	Data             json.RawMessage  `json:"data,omitempty"`
	AndroidChannelID string           `json:"android_channel_id,omitempty"`
	DelayedOption    string           `json:"delayed_option,omitempty"`
	IdempotencyKey   string           `json:"idempotency_key"`
}

// sendToWallet reports whether OneSignal accepted the notification for at least one subscribed device.
func (p *WalletPush) sendToWallet(ctx context.Context, wallet string, msg autodepositWalletPush, data json.RawMessage) (bool, error) {
	if data == nil && msg.Route != "" {
		data, _ = json.Marshal(map[string]string{"route": msg.Route})
	}
	req := oneSignalRequest{
		AppID:          p.cfg.AppID,
		TargetChannel:  "push",
		IncludeAliases: oneSignalAliases{ExternalID: []string{wallet}},
		Headings:       oneSignalText{EN: msg.Title},
		Contents:       oneSignalText{EN: msg.Body},
		Data:           data,
		IdempotencyKey: pushIdempotencyKey(msg.IdempotencyName),
	}
	req.AndroidChannelID = p.cfg.AlertsChannelID
	if msg.Category == pushCategoryEarnings {
		req.AndroidChannelID = p.cfg.EarningsChannelID
	}
	if msg.Delivery == pushDeliveryIntelligent {
		req.DelayedOption = "last-active"
	}
	var payload bytes.Buffer
	encoder := json.NewEncoder(&payload)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(req); err != nil {
		return false, err
	}
	body := bytes.TrimRight(payload.Bytes(), "\n")

	id, err := p.post(ctx, body)
	if err != nil {
		return false, err
	}
	return id != "", nil
}

func (p *WalletPush) post(ctx context.Context, body []byte) (string, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(p.retryDelay):
			}
		}
		id, retryable, err := p.postOnce(ctx, body)
		if err == nil {
			return id, nil
		}
		lastErr = err
		if !retryable || ctx.Err() != nil {
			break
		}
	}
	return "", lastErr
}

func (p *WalletPush) postOnce(ctx context.Context, body []byte) (id string, retryable bool, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", "Key "+p.cfg.RESTAPIKey)
	response, err := p.HTTP.Do(request)
	if err != nil {
		// net/http errors wrap only the URL, never the headers.
		return "", true, fmt.Errorf("OneSignal request failed: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, oneSignalMaxBody))
	_ = response.Body.Close()
	if err != nil {
		return "", true, fmt.Errorf("OneSignal read response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		retry := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		return "", retry, fmt.Errorf("OneSignal API failed with %d: %s", response.StatusCode, raw)
	}
	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", false, fmt.Errorf("OneSignal response: %w", err)
	}
	return parsed.ID, false, nil
}

type WalletPushRequest struct {
	_               struct{}       `additionalProperties:"true"`
	WalletPublicKey string         `json:"walletPublicKey,omitempty"`
	Title           string         `json:"title,omitempty"`
	Body            string         `json:"body,omitempty"`
	Data            map[string]any `json:"data,omitempty"`
}

type WalletPushSent struct {
	Success bool `json:"success"`
	SentTo  int  `json:"sentTo"`
}

type WalletPushError struct {
	Message string `json:"error"`
	SentTo  *int   `json:"sentTo,omitempty"`
	status  int
}

func (e *WalletPushError) Error() string  { return e.Message }
func (e *WalletPushError) GetStatus() int { return e.status }

var (
	errWalletPushUnauthorized = &WalletPushError{Message: "Unauthorized", status: http.StatusUnauthorized}
	errWalletPushJSON         = &WalletPushError{Message: "Invalid JSON", status: http.StatusBadRequest}
	errWalletPushFields       = &WalletPushError{Message: "walletPublicKey, title, and body are required", status: http.StatusBadRequest}
	errWalletPushNoTokens     = &WalletPushError{Message: "No push tokens registered for this wallet", SentTo: new(int), status: http.StatusNotFound}
	errWalletPushFailed       = &WalletPushError{Message: "Failed to send push notification", status: http.StatusInternalServerError}
)

type walletPushRequest struct {
	Authorization string `header:"Authorization"`
	Raw           rawJSON
}

type walletPushOutput struct{ Body WalletPushSent }

func authorizedWalletPush(secret, header string) bool {
	if header == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte("Bearer "+secret), []byte(header)) == 1
}

func walletPushFields(raw []byte) (wallet, title, body string, data json.RawMessage, err *WalletPushError) {
	if !json.Valid(raw) {
		return "", "", "", nil, errWalletPushJSON
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	text := func(name string) string {
		var s string
		_ = json.Unmarshal(fields[name], &s)
		return s
	}
	wallet, title, body = text("walletPublicKey"), text("title"), text("body")
	if wallet == "" || title == "" || body == "" {
		return "", "", "", nil, errWalletPushFields
	}
	if trimmed := bytes.TrimSpace(fields["data"]); strings.HasPrefix(string(trimmed), "{") {
		var compact bytes.Buffer
		if json.Compact(&compact, trimmed) == nil {
			data = compact.Bytes()
		}
	}
	return wallet, title, body, data, nil
}

func registerWalletPush(api huma.API, push *WalletPush) {
	responses := errorResponses[WalletPushError](api, 400, 401, 404, 500)
	huma.Register(api, huma.Operation{
		OperationID: "post-wallet-push",
		Method:      http.MethodPost,
		Path:        walletPushPath,
		Summary:     "Send a push notification to every device registered for a wallet",
		Responses:   responses,
	}, func(ctx context.Context, in *walletPushRequest) (*walletPushOutput, error) {
		if !authorizedWalletPush(push.Secret, in.Authorization) {
			return nil, errWalletPushUnauthorized
		}
		wallet, title, body, data, refused := walletPushFields(in.Raw.data)
		if refused != nil {
			return nil, refused
		}
		ctx, cancel := context.WithTimeout(ctx, walletPushTimeout)
		defer cancel()
		sent, err := push.sendToWallet(ctx, wallet, autodepositWalletPush{Title: title, Body: body}, data)
		if err != nil {
			slog.WarnContext(ctx, "[wallet-push] debug-send failed", "error", err.Error(), "walletAddress", wallet)
			return nil, errWalletPushFailed
		}
		if !sent {
			return nil, errWalletPushNoTokens
		}
		return &walletPushOutput{WalletPushSent{Success: true, SentTo: 1}}, nil
	})
	documentJSONBody[WalletPushRequest](api, walletPushPath)
}

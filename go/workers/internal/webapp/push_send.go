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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	walletPushPath      = "/api/push-tokens/debug-send"
	walletPushSource    = "debug"
	walletPushTimeout   = 30 * time.Second
	expoSendURL         = "https://exp.host/--/api/v2/push/send"
	expoBatchSize       = 100
	pushRowBatchSize    = 1000
	deviceNotRegistered = "DeviceNotRegistered"
	expoMaxBody         = 1 << 20
)

type WalletPush struct {
	DB     *pgxpool.Pool
	Secret string
	HTTP   *http.Client

	expoURL string
}

func NewWalletPush(db *pgxpool.Pool, secret string) *WalletPush {
	return &WalletPush{DB: db, Secret: secret, HTTP: http.DefaultClient, expoURL: expoSendURL}
}

type expoMessage struct {
	To    string          `json:"to"`
	Title string          `json:"title"`
	Body  string          `json:"body"`
	Sound string          `json:"sound"`
	Data  json.RawMessage `json:"data,omitempty"`
}

type expoTicket struct {
	token   string
	id      *string
	status  string
	message *string
	err     *string
}

func (t expoTicket) deviceNotRegistered() bool {
	return t.status == "error" && t.err != nil && *t.err == deviceNotRegistered
}

func (p *WalletPush) sendToWallet(ctx context.Context, wallet, title, body string, data json.RawMessage) (int, error) {
	rows, err := p.DB.Query(ctx, `SELECT token FROM push_tokens WHERE wallet_public_key = $1`, wallet)
	if err != nil {
		return 0, fmt.Errorf("read push tokens: %w", err)
	}
	tokens, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("read push tokens: %w", err)
	}
	if len(tokens) == 0 {
		return 0, nil
	}
	messages := make([]expoMessage, len(tokens))
	for i, token := range tokens {
		messages[i] = expoMessage{To: token, Title: title, Body: body, Sound: "default", Data: data}
	}
	return len(tokens), p.dispatch(ctx, walletPushSource, messages)
}

func (p *WalletPush) dispatch(ctx context.Context, source string, messages []expoMessage) error {
	var sendID string
	if err := p.DB.QueryRow(ctx, `INSERT INTO push_notification_sends (source, audience, title, body, data, status, requested_count)
VALUES ($1, 'all', $2, $3, NULL, 'sending', $4) RETURNING id`,
		source, messages[0].Title, messages[0].Body, len(messages)).Scan(&sendID); err != nil {
		return fmt.Errorf("create push notification send record: %w", err)
	}
	if err := p.deliver(ctx, sendID, messages); err != nil {
		slog.WarnContext(ctx, "[push] Failed to send push notifications", "error", err.Error(), "sendId", sendID)
		failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := p.DB.Exec(failCtx, `UPDATE push_notification_sends SET status = 'failed', error_message = $2, updated_at = now() WHERE id = $1`,
			sendID, err.Error()); err != nil {
			return fmt.Errorf("mark push notification send failed: %w", err)
		}
	}
	return nil
}

func (p *WalletPush) deliver(ctx context.Context, sendID string, messages []expoMessage) error {
	tickets, err := p.expo(ctx, messages)
	if err != nil {
		return err
	}
	for start := 0; start < len(tickets); start += pushRowBatchSize {
		batch := tickets[start:min(start+pushRowBatchSize, len(tickets))]
		tokens := make([]string, len(batch))
		ids := make([]*string, len(batch))
		statuses := make([]string, len(batch))
		msgs := make([]*string, len(batch))
		errs := make([]*string, len(batch))
		for i, t := range batch {
			tokens[i], ids[i], statuses[i], msgs[i], errs[i] = t.token, t.id, t.status, t.message, t.err
		}
		if _, err := p.DB.Exec(ctx, `INSERT INTO push_notification_tickets (send_id, token, ticket_id, ticket_status, ticket_message, ticket_error)
SELECT $1, * FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[])`,
			sendID, tokens, ids, statuses, msgs, errs); err != nil {
			return err
		}
	}
	var stale []string
	seen := map[string]bool{}
	for _, t := range tickets {
		if t.deviceNotRegistered() && !seen[t.token] {
			seen[t.token] = true
			stale = append(stale, t.token)
		}
	}
	for start := 0; start < len(stale); start += pushRowBatchSize {
		if _, err := p.DB.Exec(ctx, `DELETE FROM push_tokens WHERE token = ANY($1)`, stale[start:min(start+pushRowBatchSize, len(stale))]); err != nil {
			return err
		}
	}
	_, err = p.DB.Exec(ctx, `UPDATE push_notification_sends SET status = 'sent', ticket_count = $2, device_not_registered_count = $3, sent_at = $4, updated_at = $4 WHERE id = $1`,
		sendID, len(tickets), len(stale), time.Now())
	return err
}

func (p *WalletPush) expo(ctx context.Context, messages []expoMessage) ([]expoTicket, error) {
	var tickets []expoTicket
	for start := 0; start < len(messages); start += expoBatchSize {
		batch := messages[start:min(start+expoBatchSize, len(messages))]
		var payload bytes.Buffer
		encoder := json.NewEncoder(&payload)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(batch); err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.expoURL, bytes.NewReader(bytes.TrimRight(payload.Bytes(), "\n")))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json")
		response, err := p.HTTP.Do(request)
		if err != nil {
			return nil, err
		}
		raw, err := io.ReadAll(io.LimitReader(response.Body, expoMaxBody))
		_ = response.Body.Close()
		if err != nil {
			return nil, err
		}
		if response.StatusCode < 200 || response.StatusCode > 299 {
			return nil, fmt.Errorf("Expo Push API failed with %d: %s", response.StatusCode, raw)
		}
		var envelope struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(raw, &envelope); err != nil {
			return nil, err
		}
		var items []json.RawMessage
		if json.Unmarshal(envelope.Data, &items) != nil {
			continue
		}
		for i, item := range items {
			if ticket, ok := parseExpoTicket(item); ok && i < len(batch) {
				ticket.token = batch[i].To
				tickets = append(tickets, ticket)
			}
		}
	}
	return tickets, nil
}

func parseExpoTicket(raw json.RawMessage) (expoTicket, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return expoTicket{}, false
	}
	var t expoTicket
	if json.Unmarshal(fields["status"], &t.status) != nil || (t.status != "ok" && t.status != "error") {
		return expoTicket{}, false
	}
	t.id = optionalString(fields["id"])
	t.message = optionalString(fields["message"])
	var details map[string]json.RawMessage
	if json.Unmarshal(fields["details"], &details) == nil {
		t.err = optionalString(details["error"])
	}
	return t, true
}

func optionalString(raw json.RawMessage) *string {
	var s *string
	if json.Unmarshal(raw, &s) != nil {
		return nil
	}
	return s
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
		sent, err := push.sendToWallet(ctx, wallet, title, body, data)
		if err != nil {
			slog.WarnContext(ctx, "[wallet-push] debug-send failed", "error", err.Error(), "walletAddress", wallet)
			return nil, errWalletPushFailed
		}
		if sent == 0 {
			return nil, errWalletPushNoTokens
		}
		return &walletPushOutput{WalletPushSent{Success: true, SentTo: sent}}, nil
	})
	documentJSONBody[WalletPushRequest](api, walletPushPath)
}

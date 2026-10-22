package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	pushModule     = "push-notifications/actions"
	pushChunkRows  = 1000
	pushBatchSize  = 100
	pushSendBudget = 5 * time.Minute
	pushLockKey    = "admin-push-send"
)

var pushExpoBase = "https://exp.host"

type PushPage struct {
	Counts        PushCounts         `json:"counts"`
	ActionMessage *PushActionMessage `json:"actionMessage"`
	RecentSends   []PushRecentSend   `json:"recentSends" nullable:"false"`
}

type PushCounts struct {
	All     int `json:"all"`
	Ios     int `json:"ios"`
	Android int `json:"android"`
}

type PushActionMessage struct {
	Kind    string `json:"kind" enum:"success,error"`
	Message string `json:"message"`
}

type PushRecentSend struct {
	ID                       string  `json:"id"`
	Source                   string  `json:"source"`
	Audience                 string  `json:"audience"`
	Platform                 *string `json:"platform"`
	Title                    string  `json:"title"`
	Status                   string  `json:"status"`
	RequestedCount           int     `json:"requestedCount"`
	TicketCount              int     `json:"ticketCount"`
	ReceiptOkCount           int     `json:"receiptOkCount"`
	ReceiptErrorCount        int     `json:"receiptErrorCount"`
	DeviceNotRegisteredCount int     `json:"deviceNotRegisteredCount"`
	SentAt                   *string `json:"sentAt"`
	ReceiptsCheckedAt        *string `json:"receiptsCheckedAt"`
	CreatedAt                string  `json:"createdAt"`
	CreatedBy                *string `json:"createdBy"`
	ReceiptIDCount           int     `json:"receiptIdCount"`
	LastTicketError          *string `json:"lastTicketError"`
}

type PushResult struct {
	Error   string `json:"error,omitempty"`
	Success bool   `json:"success,omitempty"`
	Message string `json:"message,omitempty"`
}

type pushTicket struct {
	Status  string `json:"status"`
	ID      string `json:"id"`
	Message string `json:"message"`
	Details struct {
		Error string `json:"error"`
	} `json:"details"`
}

type pushSentTicket struct {
	token  string
	ticket pushTicket
}

func pushFirst(query map[string][]string, key string) string {
	if values := query[key]; len(values) > 0 {
		return values[0]
	}
	return ""
}

func loadPush(ctx context.Context, d *Deps, req PageRequest) (PushPage, error) {
	page := PushPage{RecentSends: []PushRecentSend{}}
	if message := pushFirst(req.Query, "message"); message != "" {
		if kind := pushFirst(req.Query, "result"); kind == "success" || kind == "error" {
			page.ActionMessage = &PushActionMessage{Kind: kind, Message: message}
		}
	}
	err := d.AppRead(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT
count(*) FILTER (WHERE wallet_public_key IS NOT NULL)::int,
count(*) FILTER (WHERE wallet_public_key IS NOT NULL AND platform = 'ios')::int,
count(*) FILTER (WHERE wallet_public_key IS NOT NULL AND platform = 'android')::int
FROM push_tokens`).Scan(&page.Counts.All, &page.Counts.Ios, &page.Counts.Android); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id::text, source, audience, platform, title, status, requested_count, ticket_count,
receipt_ok_count, receipt_error_count, device_not_registered_count, sent_at, receipts_checked_at, created_at, created_by
FROM push_notification_sends WHERE source = 'admin' ORDER BY created_at DESC, id DESC LIMIT 20`)
		if err != nil {
			return err
		}
		byID := map[string]int{}
		ids := []string{}
		for rows.Next() {
			var s PushRecentSend
			var sent, checked *time.Time
			var created time.Time
			if err := rows.Scan(&s.ID, &s.Source, &s.Audience, &s.Platform, &s.Title, &s.Status, &s.RequestedCount, &s.TicketCount,
				&s.ReceiptOkCount, &s.ReceiptErrorCount, &s.DeviceNotRegisteredCount, &sent, &checked, &created, &s.CreatedBy); err != nil {
				return err
			}
			s.SentAt, s.ReceiptsCheckedAt, s.CreatedAt = libraryISOPtr(sent), libraryISOPtr(checked), libraryISO(created)
			byID[s.ID] = len(page.RecentSends)
			ids = append(ids, s.ID)
			page.RecentSends = append(page.RecentSends, s)
		}
		if err := rows.Err(); err != nil || len(ids) == 0 {
			return err
		}
		meta, err := tx.Query(ctx, `SELECT send_id::text,
count(*) FILTER (WHERE ticket_id IS NOT NULL)::int,
(ARRAY_AGG(NULLIF(CONCAT_WS(': ', ticket_error, ticket_message), '') ORDER BY updated_at DESC) FILTER (WHERE ticket_status = 'error'))[1]
FROM push_notification_tickets WHERE send_id = ANY($1::uuid[]) GROUP BY send_id`, ids)
		if err != nil {
			return err
		}
		defer meta.Close()
		for meta.Next() {
			var sendID string
			var count int
			var lastError *string
			if err := meta.Scan(&sendID, &count, &lastError); err != nil {
				return err
			}
			page.RecentSends[byID[sendID]].ReceiptIDCount, page.RecentSends[byID[sendID]].LastTicketError = count, lastError
		}
		return meta.Err()
	})
	return page, err
}

func pushRedirect(result PushResult) error {
	query := "result=success&message=" + url.QueryEscape(result.Message)
	if result.Error != "" {
		query = "result=error&message=" + url.QueryEscape(result.Error)
	}
	return Redirect("/push-notifications?" + query)
}

func pushChunks[T any](items []T, size int) [][]T {
	var chunks [][]T
	for len(items) > size {
		chunks, items = append(chunks, items[:size]), items[size:]
	}
	if len(items) > 0 {
		chunks = append(chunks, items)
	}
	return chunks
}

func pushExpoPost(ctx context.Context, d *Deps, endpoint string, body, into any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, pushExpoBase+endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if d.ExpoAccessToken != "" {
		request.Header.Set("Authorization", "Bearer "+d.ExpoAccessToken)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("Expo Push API failed with %d: %s", response.StatusCode, raw)
	}
	return json.Unmarshal(raw, into)
}

type pushDraft struct {
	title, body, confirmation, platform, wallet string
	createdBy                                   *string
	data                                        []byte
}

func readPushDraft(call Call) (pushDraft, string) {
	v := pushDraft{
		title:        strings.TrimSpace(call.FormValue("title")),
		body:         strings.TrimSpace(call.FormValue("body")),
		confirmation: strings.TrimSpace(call.FormValue("confirmation")),
		platform:     call.FormValue("platform"),
		wallet:       strings.TrimSpace(call.FormValue("walletPublicKey")),
	}
	if v.platform != "ios" && v.platform != "android" && v.platform != "wallet" {
		v.platform = "all"
	}
	if by := strings.TrimSpace(call.FormValue("createdBy")); by != "" {
		v.createdBy = &by
	}
	switch {
	case v.title == "" || v.body == "":
		return v, "Title and body are required"
	case v.confirmation != "SEND":
		return v, "Type SEND to confirm the broadcast"
	case v.platform == "wallet" && v.wallet == "":
		return v, "Wallet public key is required for wallet test sends"
	}
	if raw := call.FormValue("data"); strings.TrimSpace(raw) != "" {
		var parsed any
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			return v, "Invalid JSON data"
		}
		if object, ok := parsed.(map[string]any); !ok || object == nil {
			return v, "Data must be a JSON object"
		}
		v.data = []byte(raw)
	}
	return v, ""
}

func (v pushDraft) audience() string {
	switch v.platform {
	case "wallet":
		return "wallet"
	case "all":
		return "all"
	}
	return "platform"
}

func (v pushDraft) platformColumn() *string {
	if v.platform == "ios" || v.platform == "android" {
		return &v.platform
	}
	return nil
}

func pushReserve(ctx context.Context, d *Deps, v pushDraft) (sendID string, tokens []string, busy bool, err error) {
	err = d.AppWrite(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, pushLockKey); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM push_notification_sends
WHERE source = 'admin' AND status = 'sending' AND title = $1 AND body = $2 AND audience = $3
AND platform IS NOT DISTINCT FROM $4 AND created_at > now() - interval '1 hour')`,
			v.title, v.body, v.audience(), v.platformColumn()).Scan(&busy); err != nil || busy {
			return err
		}
		var query string
		var arg []any
		switch v.platform {
		case "wallet":
			query, arg = `SELECT DISTINCT token FROM push_tokens WHERE wallet_public_key = $1 ORDER BY token`, []any{v.wallet}
		case "all":
			query = `SELECT DISTINCT token FROM push_tokens WHERE wallet_public_key IS NOT NULL ORDER BY token`
		default:
			query, arg = `SELECT DISTINCT token FROM push_tokens WHERE wallet_public_key IS NOT NULL AND platform = $1 ORDER BY token`, []any{v.platform}
		}
		rows, err := tx.Query(ctx, query, arg...)
		if err != nil {
			return err
		}
		if tokens, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO push_notification_sends (source, audience, platform, title, body, data, created_by, status, requested_count)
VALUES ('admin', $1, $2, $3, $4, $5, $6, 'sending', $7) RETURNING id::text`,
			v.audience(), v.platformColumn(), v.title, v.body, v.data, v.createdBy, len(tokens)).Scan(&sendID); err != nil {
			return err
		}
		if len(tokens) == 0 {
			_, err = tx.Exec(ctx, `UPDATE push_notification_sends SET status = 'sent', sent_at = now(), updated_at = now() WHERE id = $1`, sendID)
		}
		return err
	})
	return sendID, tokens, busy, err
}

func pushStaleTokens(ctx context.Context, d *Deps, tokens []string) error {
	for _, chunk := range pushChunks(tokens, pushChunkRows) {
		if err := d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM push_tokens WHERE token = ANY($1)`, chunk)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

func pushDeliver(ctx context.Context, d *Deps, v pushDraft, sendID string, tokens []string) (PushResult, error) {
	var data any
	if v.data != nil {
		data = json.RawMessage(v.data)
	}
	type message struct {
		To    string `json:"to"`
		Title string `json:"title"`
		Body  string `json:"body"`
		Data  any    `json:"data,omitempty"`
		Sound string `json:"sound"`
	}
	var sent []pushSentTicket
	receiptIDs := 0
	var stale []string
	for _, batch := range pushChunks(tokens, pushBatchSize) {
		messages := make([]message, len(batch))
		for i, token := range batch {
			messages[i] = message{To: token, Title: v.title, Body: v.body, Data: data, Sound: "default"}
		}
		var reply struct {
			Data []json.RawMessage `json:"data"`
		}
		if err := pushExpoPost(ctx, d, "/--/api/v2/push/send", messages, &reply); err != nil {
			return PushResult{}, err
		}
		for i, raw := range reply.Data {
			var ticket pushTicket
			if json.Unmarshal(raw, &ticket) != nil || (ticket.Status != "ok" && ticket.Status != "error") || i >= len(batch) {
				continue
			}
			sent = append(sent, pushSentTicket{token: batch[i], ticket: ticket})
			if ticket.Status == "ok" && ticket.ID != "" {
				receiptIDs++
			}
			if ticket.Status == "error" && ticket.Details.Error == "DeviceNotRegistered" {
				stale = append(stale, batch[i])
			}
		}
	}

	firstError := ""
	for _, s := range sent {
		if s.ticket.Status != "error" {
			continue
		}
		firstError = s.ticket.Message
		if s.ticket.Details.Error != "" {
			message := s.ticket.Message
			if message == "" {
				message = "Expo rejected the ticket"
			}
			firstError = s.ticket.Details.Error + ": " + message
		}
		break
	}

	for _, batch := range pushChunks(sent, pushChunkRows) {
		var tokens, ids, statuses, messages, errs []*string
		for _, s := range batch {
			tokens = append(tokens, &s.token)
			ids, messages, errs = append(ids, pushNullable(s.ticket.ID)), append(messages, pushNullable(s.ticket.Message)), append(errs, pushNullable(s.ticket.Details.Error))
			statuses = append(statuses, &s.ticket.Status)
		}
		if err := d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO push_notification_tickets (send_id, token, ticket_id, ticket_status, ticket_message, ticket_error)
SELECT $1, t.* FROM unnest($2::text[], $3::text[], $4::text[], $5::text[], $6::text[]) AS t`, sendID, tokens, ids, statuses, messages, errs)
			return err
		}); err != nil {
			return PushResult{}, err
		}
	}

	stale = pushUnique(stale)
	if err := pushStaleTokens(ctx, d, stale); err != nil {
		return PushResult{}, err
	}
	status, errorMessage := "sent", (*string)(nil)
	if receiptIDs == 0 {
		status = "failed"
		if firstError != "" {
			errorMessage = &firstError
		}
	}
	if err := d.AppWrite(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE push_notification_sends SET status = $2, ticket_count = $3, device_not_registered_count = $4,
error_message = $5, sent_at = now(), updated_at = now() WHERE id = $1`, sendID, status, len(sent), len(stale), errorMessage)
		return err
	}); err != nil {
		return PushResult{}, err
	}
	if receiptIDs == 0 {
		if firstError == "" {
			firstError = "no receipt IDs were returned"
		}
		return PushResult{Error: "Expo rejected all push tickets: " + firstError}, nil
	}
	return PushResult{Success: true, Message: fmt.Sprintf("Accepted %d/%d push tickets. Pruned %d dead tokens.", receiptIDs, len(tokens), len(stale))}, nil
}

func pushNullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func pushUnique(items []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, item := range items {
		if !seen[item] {
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}

func pushSend(ctx context.Context, d *Deps, call Call) (PushResult, error) {
	v, invalid := readPushDraft(call)
	if invalid != "" {
		return PushResult{Error: invalid}, nil
	}
	sendID, tokens, busy, err := pushReserve(ctx, d, v)
	if err != nil {
		return PushResult{}, err
	}
	if busy {
		return PushResult{Error: "A push with this title and body is still sending. Check Recent sends before sending again."}, nil
	}
	if len(tokens) == 0 {
		return PushResult{Success: true, Message: "No registered mobile push tokens matched"}, nil
	}

	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushSendBudget)
	defer cancel()
	result, err := pushDeliver(work, d, v, sendID, tokens)
	if err != nil {
		_ = d.AppWrite(work, func(tx pgx.Tx) error {
			_, err := tx.Exec(work, `UPDATE push_notification_sends SET status = 'failed', error_message = $2, updated_at = now() WHERE id = $1`, sendID, err.Error())
			return err
		})
		return PushResult{Error: "Expo push send failed"}, nil
	}
	return result, nil
}

type pushTicketRow struct{ id, token, ticketID string }

func pushCheck(ctx context.Context, d *Deps, rawID string) (PushResult, error) {
	sendID, err := parseUUID(rawID)
	if err != nil {
		return PushResult{Error: "Invalid send ID"}, nil
	}
	var tickets []pushTicketRow
	if err := d.AppRead(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text, token, ticket_id FROM push_notification_tickets
WHERE send_id = $1 AND ticket_id IS NOT NULL AND ticket_id <> '' ORDER BY created_at, id`, sendID)
		if err != nil {
			return err
		}
		tickets, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (t pushTicketRow, err error) {
			err = row.Scan(&t.id, &t.token, &t.ticketID)
			return t, err
		})
		return err
	}); err != nil {
		return PushResult{}, err
	}
	if len(tickets) == 0 {
		return PushResult{Error: "No receipt IDs are available for this send"}, nil
	}

	work, cancel := context.WithTimeout(context.WithoutCancel(ctx), pushSendBudget)
	defer cancel()
	result, err := pushApplyReceipts(work, d, sendID, tickets)
	if err != nil {
		return PushResult{Error: err.Error()}, nil
	}
	return result, nil
}

type pushReceipt struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Details struct {
		Error string `json:"error"`
	} `json:"details"`
}

func pushApplyReceipts(ctx context.Context, d *Deps, sendID pgtype.UUID, tickets []pushTicketRow) (PushResult, error) {
	ticketIDs := make([]string, len(tickets))
	for i, t := range tickets {
		ticketIDs[i] = t.ticketID
	}
	receipts := map[string]pushReceipt{}
	for _, batch := range pushChunks(ticketIDs, pushChunkRows) {
		var reply struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := pushExpoPost(ctx, d, "/--/api/v2/push/getReceipts", map[string][]string{"ids": batch}, &reply); err != nil {
			return PushResult{}, fmt.Errorf("Expo Push receipts API failed: %w", err)
		}
		for id, raw := range reply.Data {
			var receipt pushReceipt
			if json.Unmarshal(raw, &receipt) == nil && (receipt.Status == "ok" || receipt.Status == "error") {
				receipts[id] = receipt
			}
		}
	}

	var okCount, errorCount int
	var stale []string
	var updateIDs, statuses, messages, errs []*string
	for i, ticketID := range ticketIDs {
		receipt, ok := receipts[ticketID]
		if !ok {
			continue
		}
		if receipt.Status == "ok" {
			okCount++
		} else {
			errorCount++
		}
		if receipt.Status == "error" && receipt.Details.Error == "DeviceNotRegistered" {
			stale = append(stale, tickets[i].token)
		}
		updateIDs = append(updateIDs, &tickets[i].id)
		statuses, messages, errs = append(statuses, &receipt.Status), append(messages, pushNullable(receipt.Message)), append(errs, pushNullable(receipt.Details.Error))
	}
	for start := 0; start < len(updateIDs); start += pushChunkRows {
		end := min(start+pushChunkRows, len(updateIDs))
		if err := d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE push_notification_tickets t SET receipt_status = u.status, receipt_message = u.message,
receipt_error = u.error, updated_at = now()
FROM unnest($1::uuid[], $2::text[], $3::text[], $4::text[]) AS u(id, status, message, error) WHERE t.id = u.id`,
				updateIDs[start:end], statuses[start:end], messages[start:end], errs[start:end])
			return err
		}); err != nil {
			return PushResult{}, err
		}
	}
	stale = pushUnique(stale)
	if err := pushStaleTokens(ctx, d, stale); err != nil {
		return PushResult{}, err
	}
	if err := d.AppWrite(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE push_notification_sends SET status = 'receipt_checked', receipt_ok_count = $2, receipt_error_count = $3,
device_not_registered_count = device_not_registered_count + $4, receipts_checked_at = now(), updated_at = now() WHERE id = $1`,
			sendID, okCount, errorCount, len(stale))
		return err
	}); err != nil {
		return PushResult{}, err
	}
	return PushResult{Success: true, Message: fmt.Sprintf("Checked %d receipts. Pruned %d dead tokens.", okCount+errorCount, len(stale))}, nil
}

func registerPush(m *Mux) {
	Page(m, "/push-notifications", loadPush)

	Action(m, pushModule, "sendManualPushNotification", func(ctx context.Context, d *Deps, call Call) (*PushResult, error) {
		result, err := pushSend(ctx, d, call)
		return &result, err
	})

	Action(m, pushModule, "checkPushReceipts", func(ctx context.Context, d *Deps, call Call) (*PushResult, error) {
		var sendID string
		if err := call.Arg(0, &sendID); err != nil {
			return nil, err
		}
		result, err := pushCheck(ctx, d, sendID)
		return &result, err
	})

	Action(m, pushModule, "submitManualPushNotification", func(ctx context.Context, d *Deps, call Call) (*struct{}, error) {
		result, err := pushSend(ctx, d, call)
		if err != nil {
			return nil, err
		}
		return nil, pushRedirect(result)
	})

	Action(m, pushModule, "submitPushReceiptCheck", func(ctx context.Context, d *Deps, call Call) (*struct{}, error) {
		result, err := pushCheck(ctx, d, call.FormValue("sendId"))
		if err != nil {
			return nil, err
		}
		return nil, pushRedirect(result)
	})
}

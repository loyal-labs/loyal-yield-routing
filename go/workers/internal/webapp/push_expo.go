package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

const (
	expoSendURL = "https://exp.host/--/api/v2/push/send"
	expoMaxBody = 1 << 20
)

// Match the Expo server SDK's legacy UUID and prefixed token formats. Provider
// selection still comes from the authenticated push_tokens row, never its shape.
func validExpoToken(token string) bool {
	for _, prefix := range []string{"ExponentPushToken[", "ExpoPushToken["} {
		if strings.HasPrefix(token, prefix) && strings.HasSuffix(token, "]") && len(token) > len(prefix)+1 {
			return true
		}
	}
	id, err := uuid.Parse(token)
	return err == nil && id.String() == strings.ToLower(token)
}

type expoMessage struct {
	To    string          `json:"to"`
	Title string          `json:"title"`
	Body  string          `json:"body"`
	Sound string          `json:"sound"`
	Data  json.RawMessage `json:"data,omitempty"`
}

type expoTicket struct {
	Status  string `json:"status"`
	ID      string `json:"id"`
	Details struct {
		Error string `json:"error"`
	} `json:"details"`
}

// sendExpo uses the previous Expo payload and ticket contract during adoption.
// Recipients are capped at 100 by the lookup, matching Expo's one-request limit.
// There is deliberately no retry: Expo has no equivalent to our OneSignal
// idempotency key, and a timeout may occur after the push was accepted.
func (p *WalletPush) sendExpo(ctx context.Context, wallet string, tokens []string, msg autodepositWalletPush, data json.RawMessage) (bool, error) {
	messages := make([]expoMessage, len(tokens))
	for i, token := range tokens {
		messages[i] = expoMessage{To: token, Title: msg.Title, Body: msg.Body, Sound: "default", Data: data}
	}
	body, err := json.Marshal(messages)
	if err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, p.expoURL, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := p.HTTP.Do(request)
	if err != nil {
		return false, fmt.Errorf("Expo request failed: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, expoMaxBody+1))
	_ = response.Body.Close()
	if err != nil {
		return false, fmt.Errorf("Expo read response: %w", err)
	}
	if len(raw) > expoMaxBody {
		return false, errors.New("Expo response exceeds size limit")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return false, fmt.Errorf("Expo Push API failed with %d", response.StatusCode)
	}
	var envelope struct {
		Data []expoTicket `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return false, fmt.Errorf("Expo response: %w", err)
	}
	if len(envelope.Data) != len(tokens) {
		return false, fmt.Errorf("Expo returned %d tickets for %d devices", len(envelope.Data), len(tokens))
	}
	var accepted bool
	var stale []string
	var failures []error
	for i, ticket := range envelope.Data {
		switch ticket.Status {
		case "ok":
			if ticket.ID == "" {
				failures = append(failures, errors.New("Expo accepted ticket has no receipt ID"))
			} else {
				accepted = true
			}
		case "error":
			if ticket.Details.Error == "DeviceNotRegistered" {
				stale = append(stale, tokens[i])
			} else {
				failures = append(failures, fmt.Errorf("Expo rejected push ticket: %s", ticket.Details.Error))
			}
		default:
			failures = append(failures, errors.New("Expo returned an invalid ticket status"))
		}
	}
	if len(stale) > 0 {
		if err := p.removeExpo(ctx, wallet, stale); err != nil {
			failures = append(failures, fmt.Errorf("remove unregistered Expo devices: %w", err))
		}
	}
	return accepted, errors.Join(failures...)
}

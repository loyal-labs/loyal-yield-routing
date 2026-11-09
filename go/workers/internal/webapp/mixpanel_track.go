package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	mixpanelTrackURL   = "https://api.mixpanel.com/track?strict=1"
	mixpanelBatchSize  = 50
	mixpanelErrorBytes = 512
)

type mixpanelEvent struct {
	Name, DistinctID, InsertID string
	Time                       time.Time
	Properties                 map[string]any
}

// MixpanelTracker posts events to Mixpanel's /track endpoint. A nil tracker
// drops events, so callers need no check.
type MixpanelTracker struct {
	token string
	url   string
	http  *http.Client
}

func NewMixpanelTracker(token string) *MixpanelTracker {
	if token == "" {
		return nil
	}
	return &MixpanelTracker{token: token, url: mixpanelTrackURL, http: &http.Client{Timeout: 10 * time.Second}}
}

// track posts the events in batches and stops at the first failed batch.
func (m *MixpanelTracker) track(ctx context.Context, events []mixpanelEvent) error {
	if m == nil {
		return nil
	}
	for start := 0; start < len(events); start += mixpanelBatchSize {
		batch := events[start:min(start+mixpanelBatchSize, len(events))]
		body := make([]map[string]any, len(batch))
		for i, event := range batch {
			properties := map[string]any{}
			for key, value := range event.Properties {
				properties[key] = value
			}
			properties["token"] = m.token
			properties["distinct_id"] = event.DistinctID
			properties["$insert_id"] = event.InsertID
			properties["time"] = event.Time.UnixMilli()
			body[i] = map[string]any{"event": event.Name, "properties": properties}
		}
		if err := m.post(ctx, body); err != nil {
			return err
		}
	}
	return nil
}

func (m *MixpanelTracker) post(ctx context.Context, body []map[string]any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.http.Do(req)
	if err != nil {
		// The request URL carries no secret, but keep the transport error terse.
		return fmt.Errorf("mixpanel track: %w", err)
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, mixpanelErrorBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("mixpanel track: status %d: %s", resp.StatusCode, reply)
	}
	return nil
}

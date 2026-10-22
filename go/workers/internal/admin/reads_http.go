package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

var readsClient = &http.Client{
	Timeout:       20 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func readsFetchJSON(ctx context.Context, timeout time.Duration, maxBytes int64, method, target string, headers map[string]string, body, out any) error {
	host := target
	if u, err := url.Parse(target); err == nil {
		host = u.Hostname()
	}
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return fmt.Errorf("%s request is invalid", host)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := readsClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s request failed", host)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s returned %d", host, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("%s response unreadable", host)
	}
	if int64(len(raw)) > maxBytes {
		return fmt.Errorf("%s response too large", host)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s returned invalid JSON", host)
	}
	return nil
}

func readsISO(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/engine"
)

const (
	privyAPI           = "https://api.privy.io"
	gasSpendInterval   = 5 * time.Minute
	gasSpendReadBudget = 45 * time.Second
	gasSpendWindow     = time.Hour
	privyPageSize      = 100
	privyMaxPages      = 50
)

type gasSpendReader struct {
	appID, appSecret string
	base             string
	interval         time.Duration
	client           *http.Client
	facts            *engine.Facts
}

// newGasSpendLane is nil unless this process holds the autodeposit family
// lock, which is what keeps Privy read by one process only.
func newGasSpendLane(families []engine.Family, facts *engine.Facts, appID, secret string) engine.Lane {
	if !slices.Contains(families, engine.FamilyAutodeposit) {
		return nil
	}
	if appID == "" {
		slog.Info("Privy gas spend off", "reason", "RETAIL_PRIVY_APP_ID and RETAIL_PRIVY_APP_SECRET are not installed")
		return nil
	}
	facts.PrivyGasSpendUnread()
	return &gasSpendReader{appID: appID, appSecret: secret, base: privyAPI, interval: gasSpendInterval, client: &http.Client{Timeout: 15 * time.Second}, facts: facts}
}

// Run never returns a read error: engine.Run stops every lane when one stops,
// and a Privy outage must only age the last-success timestamp. It reads one
// interval after start so a crash-looping process does not hammer Privy.
func (r *gasSpendReader) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		readCtx, cancel := context.WithTimeout(ctx, gasSpendReadBudget)
		usd, err := r.read(readCtx, time.Now())
		cancel()
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			slog.Error("Privy gas spend read failed", "error", engine.ErrorText(err))
		default:
			r.facts.PrivyGasSpendRead(usd)
		}
	}
}

func (r *gasSpendReader) read(ctx context.Context, now time.Time) (float64, error) {
	ids, err := r.walletIDs(ctx)
	if err != nil {
		return 0, err
	}
	var total float64
	for batch := range slices.Chunk(ids, privyPageSize) {
		query := url.Values{
			"wallet_ids":      batch,
			"start_timestamp": {strconv.FormatInt(now.Add(-gasSpendWindow).UnixMilli(), 10)},
			"end_timestamp":   {strconv.FormatInt(now.UnixMilli(), 10)},
		}
		var body struct {
			Value    string `json:"value"`
			Currency string `json:"currency"`
		}
		if err := r.get(ctx, "/v1/apps/gas_spend", query, &body); err != nil {
			return 0, err
		}
		if body.Currency != "usd" {
			return 0, fmt.Errorf("gas spend currency %q, want usd", body.Currency)
		}
		value, err := strconv.ParseFloat(body.Value, 64)
		if err != nil || value < 0 {
			return 0, errors.New("gas spend value is not a non-negative decimal")
		}
		total += value
	}
	return total, nil
}

func (r *gasSpendReader) walletIDs(ctx context.Context) ([]string, error) {
	var ids []string
	cursor := ""
	for range privyMaxPages {
		query := url.Values{"limit": {strconv.Itoa(privyPageSize)}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var page struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
			NextCursor string `json:"next_cursor"`
		}
		if err := r.get(ctx, "/v1/wallets", query, &page); err != nil {
			return nil, err
		}
		for _, wallet := range page.Data {
			ids = append(ids, wallet.ID)
		}
		if page.NextCursor == "" {
			return ids, nil
		}
		cursor = page.NextCursor
	}
	return nil, fmt.Errorf("more than %d wallets; the gas spend read needs a different shape", privyMaxPages*privyPageSize)
}

func (r *gasSpendReader) get(ctx context.Context, path string, query url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.base+path+"?"+query.Encode(), nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(r.appID, r.appSecret)
	req.Header.Set("privy-app-id", r.appID)
	req.Header.Set("privy-client", "go")
	res, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("privy %s: %w", path, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("privy %s: status %d", path, res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("privy %s: decode: %w", path, err)
	}
	return nil
}

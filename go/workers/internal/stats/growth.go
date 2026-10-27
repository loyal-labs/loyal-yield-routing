package stats

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	solana "github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
)

// Growth metrics for the 01Resolved feed (repo loyal-growth-metrics-feed, ASK-2356):
// current Earn + Earn MAX holdings and active users. The feed polls this every
// 15 minutes and publishes each UTC day from its last reading.
//
// Earn: redeemable reserves + idle balance of every vault a user deposited
// through, any stablecoin, whether or not the position is still active. This
// is the rule the feed's rebuilt history uses, so it runs slightly above
// /api/earn/stats (active positions only).
// Earn MAX: a pooled Voltr vault, so ownership is the LP token on-chain. Each
// holder is valued at the vault share price and mapped to its smart account
// through the Earn MAX authority PDA; other holders count as their own user.
const (
	growthFresh    = 5 * time.Minute
	growthTimeout  = 30 * time.Second
	growthMinRaw   = 1_000_000 // $1 in 6-decimal stablecoin units
	earnMaxVault   = "HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA"
	earnMaxLPMint  = "6tNheTBYSpQkfMLhcczKgmTLSGffK54npKMG1WQR2tvb"
	tokenProgramID = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	// Same mainnet node as loyal-app apps/web/src/lib/solana/rpc-endpoints.ts.
	defaultGrowthRPC = "https://fredra-z7l52f-fast-mainnet.helius-rpc.com"
)

// The planner expects one user vault instead of ~7.5k and re-scans both
// aggregates per vault in a nested loop (~15s); hash joins take ~2s.
const earnBySettingsSQL = `WITH user_vaults AS (
  SELECT DISTINCT vault.id AS vault_id, position.settings
  FROM loyal_yield.user_yield_positions AS position
  JOIN loyal_yield.managed_vaults AS vault
    ON vault.settings = position.settings
    AND vault.vault_index = position.vault_index
    AND vault.vault_pubkey = position.vault_pubkey
),
reserve AS (
  SELECT current.vault_id, SUM(
    CASE
      WHEN meta.semantics IS NULL
        OR meta.semantics IN ('kamino_redeemable_liquidity', 'redeemable_liquidity_amount')
        THEN current.amount_raw
      WHEN meta.semantics = 'kamino_obligation_collateral_deposited_amount'
        AND meta.redeemable ~ '^[0-9]+$'
        THEN meta.redeemable::numeric
      ELSE 0
    END
  ) AS raw
  FROM loyal_yield.vault_reserve_positions_current AS current
  CROSS JOIN LATERAL (
    SELECT
      COALESCE(current.planning_metadata ->> 'amountSemantics', current.planning_metadata ->> 'amount_semantics') AS semantics,
      COALESCE(current.planning_metadata ->> 'redeemable_liquidity_amount_raw', current.planning_metadata ->> 'redeemable_source_liquidity_amount_raw') AS redeemable
  ) AS meta
  GROUP BY current.vault_id
),
idle AS (
  SELECT vault_id, SUM(amount_raw) AS raw
  FROM loyal_yield.vault_idle_token_balances_current
  GROUP BY vault_id
)
SELECT user_vaults.settings, SUM(COALESCE(reserve.raw, 0) + COALESCE(idle.raw, 0))::text
FROM user_vaults
LEFT JOIN reserve ON reserve.vault_id = user_vaults.vault_id
LEFT JOIN idle ON idle.vault_id = user_vaults.vault_id
GROUP BY user_vaults.settings`

type growthBody struct {
	ObservedAt  string  `json:"observedAt"`
	TVLUSD      float64 `json:"tvlUsd"`
	ActiveUsers int     `json:"activeUsers"`
	EarnUSD     float64 `json:"earnUsd"`
	EarnMaxUSD  float64 `json:"earnMaxUsd"`
}

type growthMetrics struct {
	pool   *pgxpool.Pool
	client *http.Client
	rpcURL string
	mu     sync.Mutex
	body   []byte
	at     time.Time
}

func newGrowthMetrics(pool *pgxpool.Pool) *growthMetrics {
	rpc := os.Getenv("LOYAL_STATS_SOLANA_RPC_URL")
	if rpc == "" {
		rpc = defaultGrowthRPC
	}
	return &growthMetrics{pool: pool, client: &http.Client{Timeout: 15 * time.Second}, rpcURL: rpc}
}

func (g *growthMetrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.body == nil || time.Since(g.at) >= growthFresh {
		body, err := g.read(r.Context())
		if err != nil {
			slog.Warn("growth metrics unavailable", "error", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			if r.Method != http.MethodHead {
				_, _ = w.Write([]byte(`{"error":"growth metrics unavailable"}`))
			}
			return
		}
		g.body, g.at = body, time.Now()
	}
	h.Set("Content-Length", strconv.Itoa(len(g.body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(g.body)
	}
}

func (g *growthMetrics) read(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, growthTimeout)
	defer cancel()
	earn, err := g.earnBySettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("earn: %w", err)
	}
	holders, err := g.earnMaxByHolder(ctx)
	if err != nil {
		return nil, fmt.Errorf("earn max: %w", err)
	}
	return json.Marshal(combineGrowth(earn, holders, time.Now()))
}

// combineGrowth merges per-settings Earn and per-holder Earn MAX raw amounts.
func combineGrowth(earn, holders map[string]*big.Int, now time.Time) growthBody {
	settingsByVault := make(map[string]string, len(earn))
	for settings := range earn {
		key, err := solana.PublicKeyFromBase58(settings)
		if err != nil {
			continue
		}
		vault, err := earnmax.AuthorityOf(key)
		if err == nil {
			settingsByVault[vault.Key().String()] = settings
		}
	}
	perUser := make(map[string]*big.Int, len(earn)+len(holders))
	earnTotal, maxTotal := new(big.Int), new(big.Int)
	for settings, raw := range earn {
		perUser[settings] = new(big.Int).Set(raw)
		earnTotal.Add(earnTotal, raw)
	}
	for holder, raw := range holders {
		user, ok := settingsByVault[holder]
		if !ok {
			user = "holder:" + holder
		}
		if perUser[user] == nil {
			perUser[user] = new(big.Int)
		}
		perUser[user].Add(perUser[user], raw)
		maxTotal.Add(maxTotal, raw)
	}
	active := 0
	min := big.NewInt(growthMinRaw)
	for _, raw := range perUser {
		if raw.Cmp(min) > 0 {
			active++
		}
	}
	earnUSD, maxUSD := rawUSD(earnTotal), rawUSD(maxTotal)
	return growthBody{
		ObservedAt:  now.UTC().Format("2006-01-02T15:04:05.000Z"),
		TVLUSD:      earnUSD + maxUSD,
		ActiveUsers: active,
		EarnUSD:     earnUSD,
		EarnMaxUSD:  maxUSD,
	}
}

func rawUSD(raw *big.Int) float64 {
	usd, _ := new(big.Float).Quo(new(big.Float).SetInt(raw), big.NewFloat(1_000_000)).Float64()
	return usd
}

func (g *growthMetrics) earnBySettings(ctx context.Context) (map[string]*big.Int, error) {
	out := map[string]*big.Int{}
	err := db.WithTx(ctx, g.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = "+strconv.FormatInt(growthTimeout.Milliseconds(), 10)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "SET LOCAL enable_nestloop = off"); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, earnBySettingsSQL)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var settings, raw string
			if err := rows.Scan(&settings, &raw); err != nil {
				return err
			}
			amount, ok := new(big.Int).SetString(raw, 10)
			if !ok {
				return fmt.Errorf("bad amount %q for %s", raw, settings)
			}
			out[settings] = amount
		}
		return rows.Err()
	})
	return out, err
}

// earnMaxByHolder values each LP holder in raw USDC: lp * totalValue / supply.
func (g *growthMetrics) earnMaxByHolder(ctx context.Context) (map[string]*big.Int, error) {
	var price struct {
		Success bool `json:"success"`
		Data    struct {
			TotalValue json.Number `json:"totalValue"`
		} `json:"data"`
	}
	if err := g.getJSON(ctx, "https://api.voltr.xyz/vault/"+earnMaxVault+"/share-price", &price); err != nil {
		return nil, err
	}
	total, ok := new(big.Int).SetString(price.Data.TotalValue.String(), 10)
	if !price.Success || !ok || total.Sign() < 0 {
		return nil, errors.New("invalid Voltr valuation")
	}
	var supply struct {
		Value struct {
			Amount string `json:"amount"`
		} `json:"value"`
	}
	if err := g.rpc(ctx, "getTokenSupply", []any{earnMaxLPMint}, &supply); err != nil {
		return nil, err
	}
	lpSupply, ok := new(big.Int).SetString(supply.Value.Amount, 10)
	if !ok || lpSupply.Sign() <= 0 {
		return nil, errors.New("invalid LP supply")
	}
	var accounts []struct {
		Account struct {
			Data struct {
				Parsed struct {
					Info struct {
						Owner       string `json:"owner"`
						TokenAmount struct {
							Amount string `json:"amount"`
						} `json:"tokenAmount"`
					} `json:"info"`
				} `json:"parsed"`
			} `json:"data"`
		} `json:"account"`
	}
	filters := []any{map[string]any{"dataSize": 165}, map[string]any{"memcmp": map[string]any{"offset": 0, "bytes": earnMaxLPMint}}}
	if err := g.rpc(ctx, "getProgramAccounts", []any{tokenProgramID, map[string]any{"encoding": "jsonParsed", "filters": filters}}, &accounts); err != nil {
		return nil, err
	}
	out := map[string]*big.Int{}
	for _, a := range accounts {
		info := a.Account.Data.Parsed.Info
		lp, ok := new(big.Int).SetString(info.TokenAmount.Amount, 10)
		if !ok || lp.Sign() <= 0 {
			continue
		}
		value := new(big.Int).Div(new(big.Int).Mul(lp, total), lpSupply)
		if out[info.Owner] == nil {
			out[info.Owner] = new(big.Int)
		}
		out[info.Owner].Add(out[info.Owner], value)
	}
	return out, nil
}

func (g *growthMetrics) rpc(ctx context.Context, method string, params []any, result any) error {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := g.do(ctx, http.MethodPost, g.rpcURL, payload, &envelope); err != nil {
		return err
	}
	if envelope.Error != nil {
		return fmt.Errorf("%s: %s", method, envelope.Error.Message)
	}
	return json.Unmarshal(envelope.Result, result)
}

func (g *growthMetrics) getJSON(ctx context.Context, url string, result any) error {
	return g.do(ctx, http.MethodGet, url, nil, result)
}

func (g *growthMetrics) do(ctx context.Context, method, url string, payload []byte, result any) error {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: HTTP %d", method, url, res.StatusCode)
	}
	decoder := json.NewDecoder(res.Body)
	decoder.UseNumber()
	return decoder.Decode(result)
}

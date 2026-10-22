package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

var backyardManifest = []BackyardSetting{
	{Key: "Voltr vault", Value: "HXtk15EA5pBg3rSKxBm8sWPExScPkTknSRp37fXNHgNA"},
	{Key: "Voltr program", Value: "vVoLTRjQmtFpiYoegx285Ze4gsLJ8ZxgFKVcuvmG1a8"},
	{Key: "Squads vault", Value: "ST999VUTo5QExYEX9bz1oDDoKGkjXG9zpphy4Hj7VWh"},
	{Key: "Squads settings", Value: "5YQ78RwqukvCcykpmjmgRFmbEUeAgLpuVDxx1xNZnHD6"},
	{Key: "Adaptor", Value: "FSj27QT2PtP7365pQRtgSAwSwk5h2m2ATCBoXQjwTSxW"},
	{Key: "Strategy receipt", Value: "4sycXz9Xwevedo6eiXR8QEhY8yrQrkNS4G1deY9tAD2Y"},
}

const (
	backyardVaultCapRaw          = "1000000000000"
	backyardWithdrawalWaitSecond = 600
	backyardLoadFailure          = "The Backyard RWA projection could not be loaded."
	backyardNoRoute              = "No Backyard RWA route projection exists yet. The page will populate after the Go worker records its first observation."
)

type BackyardPage struct {
	Available             bool                `json:"available"`
	Error                 string              `json:"error"`
	ObservedAt            string              `json:"observedAt"`
	AumUsdMicros          *string             `json:"aumUsdMicros"`
	CurrentPosition       BackyardPosition    `json:"currentPosition"`
	History               []BackyardOperation `json:"history" nullable:"false"`
	NavUsdMicros          *string             `json:"navUsdMicros"`
	ProjectedApyBps       *string             `json:"projectedApyBps"`
	Report                BackyardReport      `json:"report"`
	RouteStatus           *string             `json:"routeStatus"`
	Settings              []BackyardSetting   `json:"settings" nullable:"false"`
	SquadsIdleRaw         *string             `json:"squadsIdleRaw"`
	StrategyIdleRaw       *string             `json:"strategyIdleRaw"`
	VoltrIdleRaw          *string             `json:"voltrIdleRaw"`
	VaultCapRaw           string              `json:"vaultCapRaw"`
	WithdrawalWaitSeconds int                 `json:"withdrawalWaitSeconds"`
}

type BackyardPosition struct {
	CollateralRaw *string `json:"collateralRaw"`
	DebtRaw       *string `json:"debtRaw"`
	LtvBps        *string `json:"ltvBps"`
	Strategy      *string `json:"strategy"`
}

type BackyardReport struct {
	Fresh      *bool   `json:"fresh"`
	ObservedAt *string `json:"observedAt"`
	Slot       *string `json:"slot"`
}

type BackyardSetting struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type BackyardOperation struct {
	Action               string  `json:"action"`
	AmountRaw            *string `json:"amount_raw"`
	CreatedAt            string  `json:"created_at"`
	Status               string  `json:"status"`
	TransactionSignature *string `json:"transaction_signature"`
}

func backyardObject(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func backyardGroups(state map[string]any) []map[string]any {
	groups := []map[string]any{}
	if state == nil {
		return groups
	}
	for _, g := range []any{state, state["snapshot"], state["observation"], state["balances"], state["report"]} {
		if m := backyardObject(g); m != nil {
			groups = append(groups, m)
		}
	}
	return groups
}

func backyardProjected(state map[string]any, keys []string) any {
	for _, group := range backyardGroups(state) {
		for _, key := range keys {
			switch v := group[key].(type) {
			case string, json.Number:
				return v
			}
		}
	}
	return nil
}

func backyardText(v any) *string {
	var s string
	switch v := v.(type) {
	case string:
		s = v
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			s = v.String()
		} else {
			s = strconv.FormatFloat(f, 'f', -1, 64)
		}
	}
	if s == "" {
		return nil
	}
	return &s
}

func backyardBigInt(state map[string]any, keys []string) *string {
	var n *big.Int
	switch v := backyardProjected(state, keys).(type) {
	case string:
		n = new(big.Int)
		if text := strings.TrimSpace(v); text != "" {
			if _, ok := n.SetString(text, 10); !ok {
				return nil
			}
		}
	case json.Number:
		f, err := v.Float64()
		if err != nil || math.IsInf(f, 0) || f != math.Trunc(f) {
			return nil
		}
		n, _ = big.NewFloat(f).Int(nil)
	default:
		return nil
	}
	s := n.String()
	return &s
}

func backyardBool(state map[string]any, keys []string) *bool {
	for _, group := range backyardGroups(state) {
		for _, key := range keys {
			if b, ok := group[key].(bool); ok {
				return &b
			}
		}
	}
	return nil
}

func backyardScalarSettings(state []byte) []BackyardSetting {
	settings := []BackyardSetting{}
	var holder struct {
		VaultSettings json.RawMessage `json:"vaultSettings"`
	}
	if json.Unmarshal(state, &holder) != nil {
		return settings
	}
	dec := json.NewDecoder(bytes.NewReader(holder.VaultSettings))
	dec.UseNumber()
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return settings
	}
	for dec.More() && len(settings) < 12 {
		keyToken, err := dec.Token()
		if err != nil {
			break
		}
		var nested any
		if dec.Decode(&nested) != nil {
			break
		}
		key, _ := keyToken.(string)
		switch v := nested.(type) {
		case string:
			settings = append(settings, BackyardSetting{Key: key, Value: v})
		case json.Number:
			settings = append(settings, BackyardSetting{Key: key, Value: *backyardText(v)})
		case bool:
			settings = append(settings, BackyardSetting{Key: key, Value: strconv.FormatBool(v)})
		}
	}
	return settings
}

func loadBackyard(ctx context.Context, d *Deps) (BackyardPage, error) {
	page := BackyardPage{
		ObservedAt: readsISO(time.Now()), History: []BackyardOperation{}, Settings: []BackyardSetting{},
		VaultCapRaw: backyardVaultCapRaw, WithdrawalWaitSeconds: backyardWithdrawalWaitSecond,
	}
	err := d.YieldRead(ctx, func(tx pgx.Tx) error {
		var routeKey string
		var strategy, collateral, debt, equity, collateralValue, debtValue, ltv, forecast *string
		var observed *time.Time
		var state []byte
		err := tx.QueryRow(ctx, `
      SELECT route.route_key, route.state,
        snapshot.strategy_key, snapshot.collateral_raw::text,
        snapshot.debt_raw::text, snapshot.equity_usd_micros::text,
        snapshot.collateral_value_usd_micros::text,
        snapshot.debt_value_usd_micros::text, snapshot.ltv_bps::text,
        snapshot.forecast_apy_bps::text, snapshot.observed_at
      FROM loyal_yield.multiply_route_states AS route
      LEFT JOIN LATERAL (
        SELECT * FROM loyal_yield.multiply_position_snapshots
        WHERE route_key = route.route_key
        ORDER BY observed_at DESC, id DESC
        LIMIT 1
      ) AS snapshot ON true
      WHERE route.state ->> 'engineVersion' = 'backyard_rwa_v1'
      ORDER BY snapshot.observed_at DESC NULLS LAST, route.updated_at DESC
      LIMIT 1
    `).Scan(&routeKey, &state, &strategy, &collateral, &debt, &equity, &collateralValue, &debtValue, &ltv, &forecast, &observed)
		if errors.Is(err, pgx.ErrNoRows) {
			page.Error = backyardNoRoute
			return nil
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
      SELECT action, status, expected_effects -> 'decision' ->> 'amountRaw' AS amount_raw,
        transaction_signature, created_at
      FROM loyal_yield.multiply_operations
      WHERE route_key = $1 AND engine_version = 'backyard_rwa_v1'
      ORDER BY created_at DESC, operation_id DESC
      LIMIT 20
    `, routeKey)
		if err != nil {
			return err
		}
		for rows.Next() {
			var op BackyardOperation
			var created time.Time
			if err := rows.Scan(&op.Action, &op.Status, &op.AmountRaw, &op.TransactionSignature, &created); err != nil {
				return err
			}
			op.CreatedAt = readsISO(created)
			page.History = append(page.History, op)
		}
		if err := rows.Err(); err != nil {
			return err
		}

		var decoded map[string]any
		dec := json.NewDecoder(bytes.NewReader(state))
		dec.UseNumber()
		if err := dec.Decode(&decoded); err != nil {
			return err
		}
		page.Available = true
		if observed != nil {
			page.ObservedAt = readsISO(*observed)
		}
		page.AumUsdMicros = backyardBigInt(decoded, []string{"aumUsdMicros", "aum_usd_micros", "aumRaw"})
		page.NavUsdMicros = backyardBigInt(decoded, []string{"navUsdMicros", "nav_usd_micros", "reportedNavRaw", "navRaw"})
		page.CurrentPosition = BackyardPosition{CollateralRaw: collateral, DebtRaw: debt, LtvBps: ltv, Strategy: strategy}
		page.ProjectedApyBps = forecast
		page.Report = BackyardReport{
			Fresh:      backyardBool(decoded, []string{"fresh", "navFresh"}),
			ObservedAt: backyardText(backyardProjected(decoded, []string{"reportObservedAt", "navObservedAt"})),
			Slot:       backyardBigInt(decoded, []string{"reportSlot", "navSlot", "slot"}),
		}
		page.RouteStatus = backyardText(backyardProjected(decoded, []string{"routeStatus", "status"}))
		page.Settings = append(append(page.Settings, backyardManifest...), backyardScalarSettings(state)...)
		page.SquadsIdleRaw = backyardBigInt(decoded, []string{"squadsIdleRaw", "squads_idle_raw"})
		page.StrategyIdleRaw = backyardBigInt(decoded, []string{"voltrStrategyIdleRaw", "voltr_strategy_idle_raw"})
		page.VoltrIdleRaw = backyardBigInt(decoded, []string{"voltrIdleRaw", "voltr_idle_raw"})
		return nil
	})
	var status huma.StatusError
	if errors.As(err, &status) {
		return page, err
	}
	if err != nil {
		slog.Error("backyard page load failed", "err", err)
		page = BackyardPage{
			ObservedAt: page.ObservedAt, Error: backyardLoadFailure, History: []BackyardOperation{}, Settings: []BackyardSetting{},
			VaultCapRaw: backyardVaultCapRaw, WithdrawalWaitSeconds: backyardWithdrawalWaitSecond,
		}
	}
	return page, nil
}

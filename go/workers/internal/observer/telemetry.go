package observer

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"
)

// These four Loyal product mints have six decimal liquidity units. Financial
// amounts remain integers; float64 is used only for the published price ratio.
const USDCMint = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"

var stableMints = []string{USDCMint, "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo", "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA"}

func supportedMint(mint string) bool {
	for _, known := range stableMints {
		if mint == known {
			return true
		}
	}
	return false
}

type FleetPosition struct {
	Reserve   string          `json:"reserve"`
	Market    string          `json:"market"`
	Mint      string          `json:"liquidityMint"`
	AmountRaw string          `json:"amountRaw"`
	Metadata  json.RawMessage `json:"metadata"`
}
type FleetIdle struct {
	Mint      string `json:"mint"`
	AmountRaw string `json:"amountRaw"`
	Slot      int64  `json:"slot"`
}
type FleetSnapshot struct {
	ObservedAt  time.Time
	Slot        int64
	ContextIdle *string
	Positions   []FleetPosition
}
type FleetVault struct {
	ID          int64
	Snapshot    *FleetSnapshot
	CurrentIdle []FleetIdle
}
type AllocationSample struct {
	ObservedAt, ObservedHour                 time.Time
	ReserveAmounts                           map[string]string
	IdleAmountRaw, ExcludedAmountRaw         string
	Total, Included, Missing, Invalid, Stale int
	OldestSourceAt, NewestSourceAt           *time.Time
	CalcVersion                              int
}
type SharePrice struct {
	Reserve, Market, Mint    string
	ObservedAt, ObservedHour time.Time
	Slot                     int64
	Price                    float64
}

func rawAmount(value string) (*big.Int, bool) {
	if value == "" {
		return nil, false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return nil, false
		}
	}
	n, ok := new(big.Int).SetString(value, 10)
	return n, ok && n.IsInt64() && n.Sign() >= 0
}
func metadataString(values map[string]json.RawMessage, keys ...string) (string, bool) {
	var value string
	found := false
	for _, key := range keys {
		if raw, exists := values[key]; exists {
			var s string
			if json.Unmarshal(raw, &s) != nil || s == "" || (found && value != s) {
				return "", false
			}
			value = s
			found = true
		}
	}
	return value, found
}
func positionAmounts(p FleetPosition) (liquidity, collateral *big.Int, ok bool) {
	if p.Reserve == "" || p.Market == "" || !supportedMint(p.Mint) {
		return nil, nil, false
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(p.Metadata, &metadata) != nil {
		return nil, nil, false
	}
	semantics, ok := metadataString(metadata, "amountSemantics", "amount_semantics")
	if !ok {
		return nil, nil, false
	}
	amount, ok := rawAmount(p.AmountRaw)
	if !ok {
		return nil, nil, false
	}
	switch semantics {
	case "kamino_redeemable_liquidity":
		return amount, nil, true
	case "kamino_obligation_collateral_deposited_amount":
		value, known := metadataString(metadata, "redeemable_liquidity_amount_raw", "redeemable_source_liquidity_amount_raw")
		if !known {
			return nil, nil, false
		}
		liquidity, known := rawAmount(value)
		return liquidity, amount, known
	default:
		return nil, nil, false
	}
}
func snapshotIdle(v FleetVault) (*big.Int, bool) {
	if v.Snapshot.ContextIdle != nil {
		return rawAmount(*v.Snapshot.ContextIdle)
	}
	if len(v.CurrentIdle) == 0 {
		return nil, false
	}
	total := new(big.Int)
	seen := map[string]bool{}
	for _, idle := range v.CurrentIdle {
		amount, ok := rawAmount(idle.AmountRaw)
		if !ok || !supportedMint(idle.Mint) || seen[idle.Mint] || idle.Slot != v.Snapshot.Slot {
			return nil, false
		}
		seen[idle.Mint] = true
		total.Add(total, amount)
	}
	return total, true
}
func numeric39(n *big.Int) bool { return n.Sign() >= 0 && len(n.String()) <= 39 }

// AggregateAllocation is a forward observation, never a historical backfill.
// Missing, invalid and stale vaults remain explicit coverage categories. An
// unknown unit never enters reserve or idle totals as zero-valued permission.
func AggregateAllocation(vaults []FleetVault, now time.Time) (AllocationSample, error) {
	if now.IsZero() {
		return AllocationSample{}, errors.New("allocation requires observation time")
	}
	sample := AllocationSample{ObservedAt: now.UTC(), ObservedHour: now.UTC().Truncate(time.Hour), ReserveAmounts: map[string]string{}, Total: len(vaults), CalcVersion: 1}
	totals := map[string]*big.Int{}
	idleTotal, excluded := new(big.Int), new(big.Int)
	seen := map[int64]bool{}
	for _, v := range vaults {
		if v.ID <= 0 || seen[v.ID] {
			return sample, errors.New("duplicate or invalid vault identity")
		}
		seen[v.ID] = true
		if v.Snapshot == nil {
			sample.Missing++
			continue
		}
		s := v.Snapshot
		if len(s.Positions) > 128 || len(v.CurrentIdle) > 128 {
			return sample, errors.New("vault telemetry exceeds bounded source count")
		}
		reserves := map[string]*big.Int{}
		known := new(big.Int)
		valid := !s.ObservedAt.IsZero() && !s.ObservedAt.After(now) && s.Slot > 0
		for _, p := range s.Positions {
			amount, _, ok := positionAmounts(p)
			if !ok {
				valid = false
				if diagnostic, ok := rawAmount(p.AmountRaw); ok {
					known.Add(known, diagnostic)
				}
				continue
			}
			known.Add(known, amount)
			if reserves[p.Reserve] == nil {
				reserves[p.Reserve] = new(big.Int)
			}
			reserves[p.Reserve].Add(reserves[p.Reserve], amount)
		}
		idle, ok := snapshotIdle(v)
		if !ok {
			valid = false
		} else {
			known.Add(known, idle)
		}
		if !valid {
			sample.Invalid++
			excluded.Add(excluded, known)
			continue
		}
		if known.Sign() > 0 && now.Sub(s.ObservedAt) > 6*time.Hour {
			sample.Stale++
			excluded.Add(excluded, known)
			continue
		}
		sample.Included++
		idleTotal.Add(idleTotal, idle)
		for reserve, amount := range reserves {
			if amount.Sign() > 0 {
				if totals[reserve] == nil {
					totals[reserve] = new(big.Int)
				}
				totals[reserve].Add(totals[reserve], amount)
			}
		}
		if known.Sign() > 0 {
			observed := s.ObservedAt.UTC()
			if sample.OldestSourceAt == nil || observed.Before(*sample.OldestSourceAt) {
				t := observed
				sample.OldestSourceAt = &t
			}
			if sample.NewestSourceAt == nil || observed.After(*sample.NewestSourceAt) {
				t := observed
				sample.NewestSourceAt = &t
			}
		}
	}
	if !numeric39(idleTotal) || !numeric39(excluded) {
		return sample, errors.New("allocation NUMERIC(39) overflow")
	}
	sample.IdleAmountRaw = idleTotal.String()
	sample.ExcludedAmountRaw = excluded.String()
	for reserve, amount := range totals {
		if !numeric39(amount) {
			return sample, errors.New("reserve allocation overflow")
		}
		sample.ReserveAmounts[reserve] = amount.String()
	}
	return sample, nil
}

// SnapshotSharePrices preserves the real snapshot's slot/hour. It chooses the
// largest recent collateral position to limit integer-rounding noise, exactly
// as earn-fleet-allocation.shared.ts; it never prices unknown conversion units.
func SnapshotSharePrices(vaults []FleetVault, now time.Time) []SharePrice {
	largest := map[string]*big.Int{}
	prices := map[string]SharePrice{}
	for _, v := range vaults {
		s := v.Snapshot
		if s == nil || s.Slot <= 0 || s.ObservedAt.IsZero() || s.ObservedAt.After(now) || now.Sub(s.ObservedAt) > time.Hour {
			continue
		}
		for _, p := range s.Positions {
			liquidity, collateral, ok := positionAmounts(p)
			if !ok || collateral == nil || collateral.Cmp(big.NewInt(1_000_000_000)) < 0 || liquidity.Sign() <= 0 {
				continue
			}
			if prior := largest[p.Reserve]; prior != nil && collateral.Cmp(prior) <= 0 {
				continue
			}
			scaled := new(big.Int).Mul(liquidity, big.NewInt(1_000_000_000_000))
			scaled.Quo(scaled, collateral)
			ratio, _ := new(big.Rat).SetFrac(scaled, big.NewInt(1_000_000_000_000)).Float64()
			if math.IsNaN(ratio) || math.IsInf(ratio, 0) || ratio <= 0 {
				continue
			}
			largest[p.Reserve] = collateral
			prices[p.Reserve] = SharePrice{Reserve: p.Reserve, Market: p.Market, Mint: p.Mint, ObservedAt: s.ObservedAt.UTC(), ObservedHour: s.ObservedAt.UTC().Truncate(time.Hour), Slot: s.Slot, Price: ratio}
		}
	}
	result := make([]SharePrice, 0, len(prices))
	for _, p := range prices {
		result = append(result, p)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Reserve < result[j].Reserve })
	return result
}

// ModelReserve and ModelPoint belong to the public unit-notional simulation.
// They are never raw money, spend decisions, realized returns or custody proof.
type ModelReserve struct {
	Reserve, Mint       string
	ObservedAt          time.Time
	Slot                int64
	APY, TotalSupplyUSD float64
	Active, Stale       bool
}
type ModelPoint struct {
	ObservedAt time.Time `json:"observedAt"`
	APYBPS     int32     `json:"apyBps"`
}
type PublicModel struct {
	GeneratedAt, WindowStartedAt, WindowEndedAt time.Time
	APYBPS, LowBPS, HighBPS                     int32
	Samples                                     []ModelPoint
	Benchmark                                   []ModelPoint
}

func modeledBPS(value float64, elapsed time.Duration) (int32, error) {
	if value <= 0 || elapsed <= 0 {
		return 0, errors.New("invalid modeled return")
	}
	rate := (math.Pow(value, float64(365*24*time.Hour)/float64(elapsed)) - 1) * 10000
	if math.IsNaN(rate) || math.IsInf(rate, 0) || rate > math.MaxInt32 {
		return 0, errors.New("modeled APY overflow")
	}
	return int32(math.Max(0, math.Floor(rate+0.5))), nil
}
func eligibleModelRow(r ModelReserve) bool {
	return r.Active && !r.Stale && supportedMint(r.Mint) && !math.IsNaN(r.APY) && !math.IsInf(r.APY, 0) && r.APY >= 0 && r.APY < .5 && !math.IsNaN(r.TotalSupplyUSD) && !math.IsInf(r.TotalSupplyUSD, 0) && r.TotalSupplyUSD > 100_000
}

func SimulatePublicModel(rows []ModelReserve, start, end time.Time) (PublicModel, error) {
	result := PublicModel{GeneratedAt: end, WindowStartedAt: start, WindowEndedAt: end}
	if start.IsZero() || !end.After(start) || end.Sub(start) > 30*24*time.Hour || len(rows) > 100_000 {
		return result, errors.New("public model window/source bound invalid")
	}
	rows = append([]ModelReserve(nil), rows...)
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].ObservedAt.Equal(rows[j].ObservedAt) {
			if rows[i].Reserve == rows[j].Reserve {
				return rows[i].Slot < rows[j].Slot
			}
			return rows[i].Reserve < rows[j].Reserve
		}
		return rows[i].ObservedAt.Before(rows[j].ObservedAt)
	})
	dates := []time.Time{start, end}
	valid := false
	for _, r := range rows {
		if r.ObservedAt.After(end) {
			continue
		}
		if eligibleModelRow(r) {
			valid = true
		}
		if r.ObservedAt.After(start) && r.ObservedAt.Before(end) {
			dates = append(dates, r.ObservedAt)
		}
	}
	if !valid {
		return result, errors.New("public model has no eligible observations")
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	unique := dates[:0]
	for _, d := range dates {
		if len(unique) == 0 || !d.Equal(unique[len(unique)-1]) {
			unique = append(unique, d)
		}
	}
	state := map[string]ModelReserve{}
	currentMint := USDCMint
	value := 1.0
	next := 0
	allBPS := []int32{}
	hourly := map[int64]ModelPoint{}
	for i := 0; i+1 < len(unique); i++ {
		at, until := unique[i], unique[i+1]
		for next < len(rows) && !rows[next].ObservedAt.After(at) {
			r := rows[next]
			if eligibleModelRow(r) {
				state[r.Reserve] = r
			} else {
				delete(state, r.Reserve)
			}
			next++
		}
		candidates := []ModelReserve{{Reserve: "cash:USDC", Mint: USDCMint}}
		keys := make([]string, 0, len(state))
		for key := range state {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			candidates = append(candidates, state[key])
		}
		best := -1.0
		chosen := USDCMint
		for _, p := range candidates {
			cost := 0.0
			if currentMint != p.Mint {
				cost = .0001
			}
			candidate := value * (1 - cost) * math.Pow(1+p.APY, float64(until.Sub(at))/float64(365*24*time.Hour))
			if candidate > best {
				best = candidate
				chosen = p.Mint
			}
		}
		value = best
		currentMint = chosen
		bps, err := modeledBPS(value, until.Sub(start))
		if err != nil {
			return result, err
		}
		allBPS = append(allBPS, bps)
		hourly[until.UTC().Truncate(time.Hour).Unix()] = ModelPoint{ObservedAt: until.UTC(), APYBPS: bps}
	}
	bps, err := modeledBPS(value, end.Sub(start))
	if err != nil {
		return result, err
	}
	result.APYBPS = bps
	sort.Slice(allBPS, func(i, j int) bool { return allBPS[i] < allBPS[j] })
	result.LowBPS = allBPS[int(math.Floor(float64(len(allBPS)-1)*.25+.5))]
	result.HighBPS = allBPS[int(math.Floor(float64(len(allBPS)-1)*.75+.5))]
	if result.LowBPS > bps {
		result.LowBPS = bps
	}
	if result.HighBPS < bps {
		result.HighBPS = bps
	}
	hours := make([]int64, 0, len(hourly))
	for hour := range hourly {
		hours = append(hours, hour)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i] < hours[j] })
	for _, hour := range hours {
		result.Samples = append(result.Samples, hourly[hour])
	}
	return result, nil
}

// BenchmarkModel follows timescaleReserveApySamplesToEarnHistorySamples: accrue
// the last actual rate over each interval, then annualize the cumulative return.
// Before the first supplied rate this public model holds cash (zero return).
func BenchmarkModel(rows []ModelReserve, start, end time.Time) ([]ModelPoint, error) {
	if !end.After(start) || len(rows) == 0 || len(rows) > 100_000 {
		return nil, errors.New("benchmark evidence/window missing")
	}
	rows = append([]ModelReserve(nil), rows...)
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].ObservedAt.Equal(rows[j].ObservedAt) {
			return rows[i].Slot < rows[j].Slot
		}
		return rows[i].ObservedAt.Before(rows[j].ObservedAt)
	})
	rate := 0.0
	next := 0
	valid := false
	for _, r := range rows {
		if math.IsNaN(r.APY) || math.IsInf(r.APY, 0) || r.APY < 0 || r.APY >= .5 || r.ObservedAt.IsZero() {
			return nil, errors.New("invalid benchmark rate")
		}
		if !r.ObservedAt.After(end) {
			valid = true
		}
	}
	if !valid {
		return nil, errors.New("benchmark has no actual observations in window")
	}
	for next < len(rows) && !rows[next].ObservedAt.After(start) {
		rate = rows[next].APY
		next++
	}
	value := 1.0
	previous := start
	hourly := map[int64]ModelPoint{}
	for {
		at := end
		if next < len(rows) && rows[next].ObservedAt.Before(end) {
			at = rows[next].ObservedAt
		}
		value *= math.Pow(1+rate, float64(at.Sub(previous))/float64(365*24*time.Hour))
		previous = at
		for next < len(rows) && !rows[next].ObservedAt.After(at) {
			rate = rows[next].APY
			next++
		}
		bps, err := modeledBPS(value, at.Sub(start))
		if err != nil {
			return nil, err
		}
		hourly[at.UTC().Truncate(time.Hour).Unix()] = ModelPoint{ObservedAt: at.UTC(), APYBPS: bps}
		if at.Equal(end) {
			break
		}
	}
	keys := make([]int64, 0, len(hourly))
	for key := range hourly {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]ModelPoint, 0, len(keys))
	for _, key := range keys {
		out = append(out, hourly[key])
	}
	return out, nil
}

type StageSignal struct {
	Stage           string     `json:"stage"`
	ActiveItemCount int64      `json:"activeItemCount"`
	StuckSince      *time.Time `json:"stuckSince"`
}
type StageDetection struct {
	Stage                 string    `json:"stage"`
	ActiveItemCount       int64     `json:"activeItemCount"`
	StuckSince            time.Time `json:"stuckSince"`
	DetectedAt            time.Time `json:"detectedAt"`
	DetectionMilliseconds int64     `json:"detectionMilliseconds"`
}
type StageHealthReport struct {
	Cluster                  string           `json:"cluster"`
	ObservedAt               time.Time        `json:"observedAt"`
	RecoveryPollMilliseconds int64            `json:"recoveryPollIntervalMilliseconds"`
	ObservationMilliseconds  int64            `json:"healthObservationIntervalMilliseconds"`
	Signals                  []StageSignal    `json:"signals"`
	StuckStages              []StageDetection `json:"stuckStages"`
}

// StageHealth is the Rust health.rs feedback detector over the first cluster
// aggregate row. Missing state-entry evidence with positive backlog is an
// immediate invariant violation; absent timestamps never become epoch zero.
func StageHealth(payload []byte, poll, observation time.Duration, now time.Time) (StageHealthReport, error) {
	var report StageHealthReport
	if poll < time.Millisecond || observation < time.Millisecond || poll > time.Duration(math.MaxInt64/2) || now.IsZero() {
		return report, errors.New("invalid health policy/clock")
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(payload, &rows); err != nil {
		return report, err
	}
	if len(rows) == 0 {
		return report, errors.New("empty health status")
	}
	readString := func(row map[string]json.RawMessage, key string) (string, error) {
		var v string
		err := json.Unmarshal(row[key], &v)
		return v, err
	}
	cluster, err := readString(rows[0], "cluster")
	if err != nil || cluster == "" {
		return report, errors.New("health cluster missing")
	}
	for _, r := range rows {
		v, err := readString(r, "cluster")
		if err != nil || v != cluster {
			return report, errors.New("mixed health clusters")
		}
	}
	row := rows[0]
	timestamp := func(key string) (*time.Time, error) {
		raw, ok := row[key]
		if !ok {
			return nil, fmt.Errorf("health timestamp %s missing", key)
		}
		if string(raw) == "null" {
			return nil, nil
		}
		var t time.Time
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, err
		}
		return &t, nil
	}
	count := func(key string) (int64, error) {
		if string(row[key]) == "null" {
			return 0, fmt.Errorf("health backlog %s unknown", key)
		}
		var n int64
		if err := json.Unmarshal(row[key], &n); err != nil {
			return 0, err
		}
		return n, nil
	}
	report = StageHealthReport{Cluster: cluster, ObservedAt: now.UTC(), RecoveryPollMilliseconds: poll.Milliseconds(), ObservationMilliseconds: observation.Milliseconds(), Signals: []StageSignal{}, StuckStages: []StageDetection{}}
	registered, err := timestamp("planner_registered_at")
	if err != nil {
		return report, err
	}
	seen, err := timestamp("planner_last_seen_at")
	if err != nil {
		return report, err
	}
	expiry, err := timestamp("latest_market_expires_at")
	if err != nil {
		return report, err
	}
	var market *time.Time
	minimum := func(t time.Time) {
		if market == nil || t.Before(*market) {
			value := t
			market = &value
		}
	}
	if expiry != nil && !expiry.After(now) {
		minimum(*expiry)
	}
	raw, ok := row["latest_market_epoch_id"]
	if !ok {
		return report, errors.New("market epoch identity missing")
	}
	if string(raw) != "null" {
		var epoch int64
		if err := json.Unmarshal(raw, &epoch); err != nil {
			return report, err
		}
	}
	if string(raw) == "null" && registered != nil {
		minimum(registered.Add(2 * poll))
	}
	if seen == nil {
		seen = registered
	}
	if seen != nil {
		deadline := seen.Add(2 * poll)
		if !deadline.After(now) {
			minimum(deadline)
		}
	}
	active := int64(0)
	if market != nil {
		active = 1
	}
	report.Signals = append(report.Signals, StageSignal{Stage: "market_epoch", ActiveItemCount: active, StuckSince: market})
	stages := []struct {
		stage, count, time string
		threshold          time.Duration
	}{{"ready", "ready_opportunity_count", "oldest_ready_state_entered_at", 10 * time.Second}, {"waiting_alt", "waiting_alt_opportunity_count", "oldest_waiting_alt_state_entered_at", 120 * time.Second}, {"sender", "sender_submission_count", "oldest_sender_state_entered_at", 10 * time.Second}, {"confirmer", "confirmer_submission_count", "oldest_confirmer_state_entered_at", 30 * time.Second}, {"reconciler", "reconciler_submission_count", "oldest_reconciler_state_entered_at", 30 * time.Second}}
	for _, s := range stages {
		n, err := count(s.count)
		if err != nil {
			return report, err
		}
		entered, err := timestamp(s.time)
		if err != nil {
			return report, err
		}
		var deadline *time.Time
		if n > 0 {
			at := now
			if entered != nil {
				threshold := s.threshold
				if poll > threshold {
					threshold = poll
				}
				at = entered.Add(threshold)
			}
			deadline = &at
		} else {
			n = 0
		}
		report.Signals = append(report.Signals, StageSignal{Stage: s.stage, ActiveItemCount: n, StuckSince: deadline})
	}
	for _, s := range report.Signals {
		if s.StuckSince != nil && !s.StuckSince.After(now) {
			report.StuckStages = append(report.StuckStages, StageDetection{Stage: s.Stage, ActiveItemCount: s.ActiveItemCount, StuckSince: s.StuckSince.UTC(), DetectedAt: now.UTC(), DetectionMilliseconds: now.Sub(*s.StuckSince).Milliseconds()})
		}
	}
	return report, nil
}

// FleetOrchestrationStatus JSON contract (Yield 0093 dropped the retired Rust
// planner's sweep columns); SQL view additions are not implicitly published.
const healthPayloadFields = "cluster opportunity_state opportunity_count principal_usd_micros annual_yield_gain_usd_micros yield_gain_usd_micros_per_hour oldest_created_at oldest_state_entered_at oldest_age_seconds oldest_state_age_seconds expired_lease_count pending_outbox_count pending_submission_count pending_compiled_fee_lamports expiry_check_pending_count effect_ambiguous_count oldest_pending_submission_at oldest_pending_submission_age_seconds sender_submission_count oldest_sender_state_entered_at oldest_sender_state_age_seconds confirmer_submission_count oldest_confirmer_state_entered_at oldest_confirmer_state_age_seconds reconciler_submission_count oldest_reconciler_state_entered_at oldest_reconciler_state_age_seconds planner_registered_at planner_last_seen_at planner_last_seen_age_seconds latest_market_epoch_id latest_market_epoch_key latest_market_slot latest_market_observed_at latest_market_expires_at latest_market_epoch_age_seconds latest_market_epoch_expires_in_seconds latest_market_epoch_expired waiting_alt_opportunity_count waiting_alt_principal_usd_micros waiting_alt_yield_gain_usd_micros_per_hour oldest_waiting_alt_state_entered_at oldest_waiting_alt_state_age_seconds ready_opportunity_count ready_principal_usd_micros ready_yield_gain_usd_micros_per_hour oldest_ready_state_entered_at oldest_ready_state_age_seconds current_epoch_opportunity_count current_epoch_principal_usd_micros current_epoch_recoverable_yield_usd_micros_per_hour current_epoch_submitted_within_10s_yield_ppm current_epoch_submitted_within_2m_yield_ppm current_epoch_submitted_within_10m_yield_ppm current_epoch_confirmed_within_30s_yield_ppm current_epoch_submission_p95_milliseconds current_epoch_confirmation_p95_milliseconds current_epoch_compiled_fee_lamports active_physical_writable_key_count top_physical_writable_key_congestion"

const healthOptionalFields = " opportunity_state oldest_created_at oldest_state_entered_at oldest_age_seconds oldest_state_age_seconds oldest_pending_submission_at oldest_pending_submission_age_seconds oldest_sender_state_entered_at oldest_sender_state_age_seconds oldest_confirmer_state_entered_at oldest_confirmer_state_age_seconds oldest_reconciler_state_entered_at oldest_reconciler_state_age_seconds planner_registered_at planner_last_seen_at planner_last_seen_age_seconds latest_market_epoch_id latest_market_epoch_key latest_market_slot latest_market_observed_at latest_market_expires_at latest_market_epoch_age_seconds latest_market_epoch_expires_in_seconds latest_market_epoch_expired oldest_waiting_alt_state_entered_at oldest_waiting_alt_state_age_seconds oldest_ready_state_entered_at oldest_ready_state_age_seconds current_epoch_submission_p95_milliseconds current_epoch_confirmation_p95_milliseconds "

func normalizeHealthPayload(payload []byte) ([]byte, error) {
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(payload, &rows); err != nil {
		return nil, err
	}
	out := make([]map[string]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		canonical := map[string]json.RawMessage{}
		for _, key := range strings.Fields(healthPayloadFields) {
			value, ok := row[key]
			if !ok {
				return nil, fmt.Errorf("health source field %s missing", key)
			}
			if string(value) == "null" {
				if !strings.Contains(healthOptionalFields, " "+key+" ") {
					return nil, fmt.Errorf("required health source field %s unknown", key)
				}
				canonical[key] = value
				continue
			}
			switch key {
			case "cluster", "opportunity_state", "latest_market_epoch_key":
				var v string
				if err := json.Unmarshal(value, &v); err != nil {
					return nil, err
				}
			case "latest_market_epoch_expired":
				var v bool
				if err := json.Unmarshal(value, &v); err != nil {
					return nil, err
				}
			case "top_physical_writable_key_congestion": // Validated below.
			default:
				if !strings.HasSuffix(key, "_at") {
					var v int64
					if err := json.Unmarshal(value, &v); err != nil {
						return nil, fmt.Errorf("health source %s: %w", key, err)
					}
				}
			}
			if strings.HasSuffix(key, "_at") && string(value) != "null" {
				var t time.Time
				if err := json.Unmarshal(value, &t); err != nil {
					return nil, err
				}
				value, _ = json.Marshal(t.UTC())
			}
			canonical[key] = value
		}
		var congestion []struct {
			Key       string `json:"writable_account_key"`
			Class     string `json:"classification"`
			Count     int64  `json:"active_submission_count"`
			Principal int64  `json:"principal_usd_micros"`
			Gain      int64  `json:"recoverable_yield_usd_micros_per_hour"`
		}
		if err := json.Unmarshal(canonical["top_physical_writable_key_congestion"], &congestion); err != nil {
			return nil, err
		}
		if len(congestion) > 16 {
			return nil, errors.New("health congestion bound exceeded")
		}
		for _, c := range congestion {
			if c.Key == "" || (c.Class != "payer" && c.Class != "target" && c.Class != "other") || c.Count <= 0 || c.Principal < 0 || c.Gain < 0 {
				return nil, errors.New("malformed physical congestion")
			}
		}
		out = append(out, canonical)
	}
	return json.Marshal(out)
}

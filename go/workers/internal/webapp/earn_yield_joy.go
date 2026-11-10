package webapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The daily Earn joy pushes <- web/apps/web/src/lib/yield-optimization/earn-yield-joy.server.ts
// and earn-yield-joy.shared.ts. At most one push per wallet per run.

const (
	joyDay                = 24 * time.Hour
	joyWalletConcurrency  = 5
	joyTimeBudget         = 240 * time.Second
	joyRuleVersion        = 2
	joyThresholdUSD       = 1.0
	joyWeeklyCap          = 4
	joyCapWindow          = 7 * 24 * time.Hour
	joyAnniversaryMinUSD  = 1.0
	joyDecisionSeed       = "seed"
	joyDecisionRebaseline = "rebaseline"
	joyDecisionPush       = "push"
	joyDecisionNone       = "none"
	joyKindMilestone      = "milestone"
	joyKindAnniversary    = "anniversary"
	joyKindDigest         = "digest"
	joyDigestCampaign     = "yield-digest"
	joyAnniversarySixMo   = "yield-six-months"
	joyAnniversaryOneYear = "yield-one-year"
)

var joyMilestonesUSD = [...]float64{1, 100, 500, 1000, 5000}

var joyAnniversaries = [...]struct {
	campaign string
	days     float64
	months   int
}{
	{joyAnniversaryOneYear, 365, 12},
	{joyAnniversarySixMo, 183, 6},
}

type joyState struct {
	lastPushedAt        *time.Time
	lastPushedEarnedUSD float64
	sentCampaigns       []string
	recentPushedAt      []time.Time
	ruleVersion         int
}

type joyPush struct {
	amountUSD   float64
	campaign    string
	months      int
	kind        string
	lifetimeUSD float64
	route       string
}

type joyDecision struct {
	kind           string
	campaigns      []string
	push           joyPush
	recentPushedAt []time.Time
	// baselineUSD is what a seed or rebaseline stores as the wallet's last
	// pushed total: never negative, so recovering from a loss is not earnings.
	baselineUSD float64
}

func joyMilestoneCampaign(milestone float64) string {
	return "yield-total-" + jsNumberString(milestone)
}

func joyQualifiedCampaigns(accountAgeDays, lifetimeEarnedUSD float64) []string {
	campaigns := []string{}
	for i := len(joyMilestonesUSD) - 1; i >= 0; i-- {
		if lifetimeEarnedUSD >= joyMilestonesUSD[i] {
			campaigns = append(campaigns, joyMilestoneCampaign(joyMilestonesUSD[i]))
			break
		}
	}
	if lifetimeEarnedUSD >= joyAnniversaryMinUSD {
		for _, entry := range joyAnniversaries {
			if accountAgeDays >= entry.days {
				campaigns = append(campaigns, entry.campaign)
				break
			}
		}
	}
	return campaigns
}

// joyMilestoneDone reports whether the milestone at index i, or any higher
// one, was already sent: a dip in earnings must not re-fire a lower milestone.
func joyMilestoneDone(sent []string, i int) bool {
	for j := i; j < len(joyMilestonesUSD); j++ {
		if slices.Contains(sent, joyMilestoneCampaign(joyMilestonesUSD[j])) {
			return true
		}
	}
	return false
}

// joyWithinWindow keeps the pushes that still count against the weekly cap:
// those on this UTC day or the six before it. Comparing days, like the daily
// cap, keeps a wallet processed a few minutes earlier than last week from
// still counting that push.
func joyWithinWindow(times []time.Time, now time.Time) []time.Time {
	today := now.UTC().Truncate(joyDay)
	kept := []time.Time{}
	for _, at := range times {
		if today.Sub(at.UTC().Truncate(joyDay)) < joyCapWindow {
			kept = append(kept, at)
		}
	}
	return kept
}

// selectJoyPush applies, in order: seed a new wallet, silently re-baseline a
// row written by an older rule, stay quiet for no earnings or a spent cap,
// then rank a milestone over an anniversary over the routine digest.
func selectJoyPush(accountAgeDays, lifetimeEarnedUSD float64, now time.Time, state *joyState) joyDecision {
	qualified := joyQualifiedCampaigns(accountAgeDays, lifetimeEarnedUSD)
	if state == nil {
		return joyDecision{kind: joyDecisionSeed, campaigns: qualified, recentPushedAt: []time.Time{}, baselineUSD: max(lifetimeEarnedUSD, 0)}
	}
	if state.ruleVersion < joyRuleVersion {
		campaigns := slices.Clone(state.sentCampaigns)
		for _, campaign := range qualified {
			if !slices.Contains(campaigns, campaign) {
				campaigns = append(campaigns, campaign)
			}
		}
		recent := []time.Time{}
		if state.lastPushedAt != nil {
			recent = joyWithinWindow([]time.Time{*state.lastPushedAt}, now)
		}
		return joyDecision{kind: joyDecisionRebaseline, campaigns: campaigns, recentPushedAt: recent, baselineUSD: max(lifetimeEarnedUSD, 0)}
	}
	if lifetimeEarnedUSD <= 0 {
		return joyDecision{kind: joyDecisionNone}
	}
	recent := joyWithinWindow(state.recentPushedAt, now)
	y, m, d := now.UTC().Date()
	for _, at := range state.recentPushedAt {
		ay, am, ad := at.UTC().Date()
		if ay == y && am == m && ad == d {
			return joyDecision{kind: joyDecisionNone}
		}
	}
	if len(recent) >= joyWeeklyCap {
		return joyDecision{kind: joyDecisionNone}
	}
	push := func(p joyPush) joyDecision {
		p.lifetimeUSD = lifetimeEarnedUSD
		return joyDecision{kind: joyDecisionPush, push: p, recentPushedAt: append(slices.Clone(recent), now)}
	}
	pending := func(campaign string) bool {
		return slices.Contains(qualified, campaign) && !slices.Contains(state.sentCampaigns, campaign)
	}
	for i := len(joyMilestonesUSD) - 1; i >= 0; i-- {
		campaign := joyMilestoneCampaign(joyMilestonesUSD[i])
		if pending(campaign) && !joyMilestoneDone(state.sentCampaigns, i) {
			return push(joyPush{amountUSD: joyMilestonesUSD[i], campaign: campaign, kind: joyKindMilestone})
		}
	}
	for _, entry := range joyAnniversaries {
		if pending(entry.campaign) {
			return push(joyPush{amountUSD: lifetimeEarnedUSD, campaign: entry.campaign, months: entry.months, kind: joyKindAnniversary})
		}
	}
	if earned := lifetimeEarnedUSD - state.lastPushedEarnedUSD; earned >= joyThresholdUSD {
		return push(joyPush{amountUSD: earned, campaign: joyDigestCampaign, kind: joyKindDigest})
	}
	return joyDecision{kind: joyDecisionNone}
}

// formatUSD2 is toLocaleString("en-US") with exactly two fraction digits,
// rounded half away from zero on the number's shortest decimal form.
func formatUSD2(amount float64) string {
	exact, _ := new(big.Rat).SetString(strconv.FormatFloat(math.Abs(amount), 'f', -1, 64))
	exact.Mul(exact, big.NewRat(100, 1))
	exact.Add(exact, big.NewRat(1, 2))
	cents := new(big.Int).Div(exact.Num(), exact.Denom())
	text := cents.String()
	for len(text) < 3 {
		text = "0" + text
	}
	whole, fraction := text[:len(text)-2], text[len(text)-2:]
	for end := len(whole) - 3; end > 0; end -= 3 {
		whole = whole[:end] + "," + whole[end:]
	}
	sign := ""
	if amount < 0 && cents.Sign() != 0 {
		sign = "-"
	}
	return sign + "$" + whole + "." + fraction
}

// formatUSDWhole is formatUSD2 without the cents, for whole milestone amounts.
func formatUSDWhole(amount float64) string {
	return strings.TrimSuffix(formatUSD2(amount), ".00")
}

func joyPushPayload(push joyPush, now time.Time, wallet string) autodepositWalletPush {
	payload := autodepositWalletPush{
		Category:        pushCategoryEarnings,
		Delivery:        pushDeliveryIntelligent,
		Route:           push.route,
		IdempotencyName: "earnings:" + push.campaign + ":" + wallet + ":" + now.UTC().Format("2006-01-02"),
	}
	switch push.kind {
	case joyKindMilestone:
		payload.Title = formatUSDWhole(push.amountUSD) + " earned with Loyal"
		payload.Body = "And it keeps going on its own."
		if push.amountUSD <= 1 {
			payload.Body = "Your first dollar, and it keeps going on its own."
		}
	case joyKindAnniversary:
		payload.Title = "Six months with Loyal"
		if push.months >= 12 {
			payload.Title = "A year with Loyal"
		}
		payload.Body = formatUSD2(push.lifetimeUSD) + " earned since you joined."
	default:
		payload.Title = "+" + formatUSD2(push.amountUSD) + " earned"
		payload.Body = formatUSD2(push.lifetimeUSD) + " earned with Loyal so far."
	}
	return payload
}

// numeric6 is Number.prototype.toFixed(6): ties go to the larger value.
func numeric6(value float64) string {
	scaled := new(big.Rat).SetFloat64(value)
	scaled.Mul(scaled, big.NewRat(1_000_000, 1))
	scaled.Add(scaled, big.NewRat(1, 2))
	micro := new(big.Int).Div(scaled.Num(), scaled.Denom())
	text := micro.String()
	for len(text) < 7 {
		text = "0" + text
	}
	return text[:len(text)-6] + "." + text[len(text)-6:]
}

type EarnYieldJoyOutcome struct {
	AmountUSD *float64 `json:"amountUsd,omitempty"`
	Campaign  string   `json:"campaign,omitempty"`
	Route     string   `json:"route,omitempty"`
	Status    string   `json:"status"`
	Wallet    string   `json:"wallet"`

	// event is set only when OneSignal accepted the push.
	event *mixpanelEvent
}

type EarnYieldJoySummary struct {
	Candidates  int                   `json:"candidates"`
	DryRun      bool                  `json:"dryRun"`
	Errors      int                   `json:"errors"`
	Processed   int                   `json:"processed"`
	Pushed      []EarnYieldJoyOutcome `json:"pushed"`
	Quiet       int                   `json:"quiet"`
	Rebaselined int                   `json:"rebaselined"`
	Seeded      int                   `json:"seeded"`
	Truncated   bool                  `json:"truncated"`
	Undelivered int                   `json:"undelivered"`
	Unavailable int                   `json:"unavailable"`
	// Unregistered counts wallets due a push that have no device registered through the app.
	Unregistered int `json:"unregistered"`
}

type joyCandidate struct {
	createdAt   time.Time
	settingsPda string
	wallet      string
}

func (c Crons) joyCandidates(ctx context.Context, after string) ([]joyCandidate, error) {
	rows, err := c.h.appDB.Query(ctx,
		`SELECT DISTINCT ON (u.subject_address COLLATE "C") u.created_at, s.settings_pda, u.subject_address
FROM app_user_smart_accounts s
JOIN app_users u ON s.user_id = u.id
WHERE s.state = 'ready' AND s.solana_env = $1
  AND u.subject_address COLLATE "C" > $2 AND s.settings_pda <> ''
ORDER BY u.subject_address COLLATE "C", s.updated_at DESC, s.settings_pda`, smartAccountsSolanaEnv, after)
	if err != nil {
		return nil, fmt.Errorf("list earn joy candidates: %w", err)
	}
	defer rows.Close()
	var out []joyCandidate
	for rows.Next() {
		var candidate joyCandidate
		var settings, wallet *string
		if err := rows.Scan(&candidate.createdAt, &settings, &wallet); err != nil {
			return nil, fmt.Errorf("list earn joy candidates: %w", err)
		}
		if settings == nil || *settings == "" || wallet == nil || *wallet == "" {
			continue
		}
		candidate.settingsPda, candidate.wallet = *settings, *wallet
		out = append(out, candidate)
	}
	return out, rows.Err()
}

func (c Crons) joyStates(ctx context.Context, wallets []string) (map[string]joyState, error) {
	states := map[string]joyState{}
	if len(wallets) == 0 {
		return states, nil
	}
	rows, err := c.h.appDB.Query(ctx,
		`SELECT wallet_public_key, last_pushed_at, last_pushed_earned_usd::text, sent_campaigns, recent_pushed_at, rule_version
FROM earn_yield_push_state
WHERE wallet_public_key = ANY($1)`, wallets)
	if err != nil {
		return nil, fmt.Errorf("load earn joy states: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var wallet, earned string
		var state joyState
		var sent []byte
		var ruleVersion int16
		if err := rows.Scan(&wallet, &state.lastPushedAt, &earned, &sent, &state.recentPushedAt, &ruleVersion); err != nil {
			return nil, fmt.Errorf("load earn joy states: %w", err)
		}
		if state.lastPushedEarnedUSD, err = strconv.ParseFloat(earned, 64); err != nil {
			return nil, fmt.Errorf("load earn joy states: %w", err)
		}
		if err := json.Unmarshal(sent, &state.sentCampaigns); err != nil {
			return nil, fmt.Errorf("load earn joy states: sent_campaigns: %w", err)
		}
		state.ruleVersion = int(ruleVersion)
		states[wallet] = state
	}
	return states, rows.Err()
}

// claimJoyState writes the wallet's new state only if the row still holds the
// state this run read (sent campaigns, last push and rule version), so the
// push that follows is sent at most once even if another run raced this one.
// Next sends first and upserts after.
func (c Crons) claimJoyState(ctx context.Context, wallet string, previous *joyState, decision joyDecision, now time.Time, lifetimeUSD float64) (bool, error) {
	recent := decision.recentPushedAt
	if recent == nil {
		recent = []time.Time{}
	}
	var tag pgconn.CommandTag
	var err error
	switch {
	case decision.kind == joyDecisionSeed && previous == nil:
		sent, marshalErr := json.Marshal(nonNilStrings(decision.campaigns))
		if marshalErr != nil {
			return false, marshalErr
		}
		tag, err = c.h.appDB.Exec(ctx,
			`INSERT INTO earn_yield_push_state (wallet_public_key, last_pushed_at, last_pushed_earned_usd, sent_campaigns, recent_pushed_at, rule_version, updated_at)
VALUES ($1, $2, $3::numeric, $4::jsonb, '{}'::timestamptz[], $5, now())
ON CONFLICT (wallet_public_key) DO NOTHING`,
			wallet, now, numeric6(decision.baselineUSD), string(sent), joyRuleVersion)
	case (decision.kind == joyDecisionRebaseline || decision.kind == joyDecisionPush) && previous != nil:
		campaigns := decision.campaigns
		lastPushedAt := previous.lastPushedAt
		baselineUSD := decision.baselineUSD
		if decision.kind == joyDecisionPush {
			baselineUSD = lifetimeUSD
			campaigns = slices.Clone(previous.sentCampaigns)
			if decision.push.kind != joyKindDigest {
				campaigns = append(campaigns, decision.push.campaign)
			}
			lastPushedAt = &now
		}
		sent, marshalErr := json.Marshal(nonNilStrings(campaigns))
		if marshalErr != nil {
			return false, marshalErr
		}
		previousSent, marshalErr := json.Marshal(nonNilStrings(previous.sentCampaigns))
		if marshalErr != nil {
			return false, marshalErr
		}
		tag, err = c.h.appDB.Exec(ctx,
			`UPDATE earn_yield_push_state
SET last_pushed_at = $2, last_pushed_earned_usd = $3::numeric, sent_campaigns = $4::jsonb, recent_pushed_at = $5::timestamptz[], rule_version = $6, updated_at = now()
WHERE wallet_public_key = $1 AND sent_campaigns = $7::jsonb AND last_pushed_at IS NOT DISTINCT FROM $8 AND rule_version = $9`,
			wallet, lastPushedAt, numeric6(baselineUSD), string(sent), recent, joyRuleVersion,
			string(previousSent), previous.lastPushedAt, previous.ruleVersion)
	default:
		return false, fmt.Errorf("save earn joy state: %s decision does not fit the stored state", decision.kind)
	}
	if err != nil {
		return false, fmt.Errorf("save earn joy state: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func (c Crons) processJoyCandidate(ctx context.Context, candidate joyCandidate, cluster string, dryRun bool, now time.Time, state *joyState) (EarnYieldJoyOutcome, error) {
	outcome := EarnYieldJoyOutcome{Wallet: candidate.wallet}
	earnings, available, err := c.walletEarnings(ctx, cluster, candidate)
	if err != nil {
		return outcome, err
	}
	if !available {
		outcome.Status = "unavailable"
		return outcome, nil
	}
	lifetime := earnings.lifetimeUSD()
	decision := selectJoyPush(float64(now.Sub(candidate.createdAt).Milliseconds())/float64(joyDay.Milliseconds()), lifetime, now, state)
	decision.push.route, decision.push.lifetimeUSD = earnings.route(), lifetime
	switch decision.kind {
	case joyDecisionNone:
		outcome.Status = "quiet"
		return outcome, nil
	case joyDecisionSeed, joyDecisionRebaseline:
		outcome.Status = "seeded"
		if decision.kind == joyDecisionRebaseline {
			outcome.Status = "rebaselined"
		}
		if !dryRun {
			claimed, err := c.claimJoyState(ctx, candidate.wallet, state, decision, now, lifetime)
			if err != nil {
				return outcome, err
			}
			if !claimed {
				outcome.Status = "quiet"
			}
		}
		return outcome, nil
	}
	// A wallet with no registered device keeps its claim, so it gets this push once it registers.
	if c.h.CampaignPush != nil {
		registered, err := c.h.CampaignPush.registered(ctx, candidate.wallet)
		if err != nil {
			return outcome, err
		}
		if !registered {
			outcome.Status = "unregistered"
			return outcome, nil
		}
	}
	delivered := true
	if !dryRun {
		claimed, err := c.claimJoyState(ctx, candidate.wallet, state, decision, now, lifetime)
		if err != nil {
			return outcome, err
		}
		if !claimed {
			outcome.Status = "quiet"
			return outcome, nil
		}
		payload := joyPushPayload(decision.push, now, candidate.wallet)
		delivered = c.h.CampaignPush.send(ctx, candidate.wallet, payload)
		if delivered {
			outcome.event = &mixpanelEvent{
				Name:       "Earnings Push Sent",
				DistinctID: "mob:" + candidate.wallet,
				InsertID:   payload.IdempotencyName,
				Time:       now,
				Properties: map[string]any{
					"type":         decision.push.kind,
					"campaign":     decision.push.campaign,
					"amount_usd":   decision.push.amountUSD,
					"lifetime_usd": decision.push.lifetimeUSD,
					"route":        decision.push.route,
				},
			}
		}
	}
	amount := decision.push.amountUSD
	outcome.AmountUSD, outcome.Campaign, outcome.Route, outcome.Status = &amount, decision.push.campaign, decision.push.route, "pushed"
	if !delivered {
		outcome.Status = "undelivered"
	}
	return outcome, nil
}

// EarnYieldJoy ports runEarnYieldJoy. A run that finds another run holding
// the lock does nothing and reports so.
func (c Crons) EarnYieldJoy(ctx context.Context, dryRun bool) (EarnYieldJoySummary, error) {
	var summary EarnYieldJoySummary
	if c.Earnings == nil && c.EarnMax == nil && c.EarnMaxActivity == nil {
		return summary, errEarnMaxReader
	}
	if !dryRun && c.h.CampaignPush == nil {
		return summary, errors.New("the wallet push sender is off (ONESIGNAL_APP_ID and ONESIGNAL_REST_API_KEY are not both installed): a run would spend the one-time campaigns without sending")
	}
	if !dryRun && !c.h.CampaignPush.earningsChannelSet() {
		return summary, errors.New("the earnings push channel is not configured (ONESIGNAL_EARNINGS_CHANNEL_ID): a run would spend the one-time campaigns on the wrong channel")
	}
	if c.h.YieldDB == nil {
		return summary, errors.New("earn-yield-joy needs the Yield database for resumable progress")
	}
	grants := []Grant{TableGrant("loyal_yield.earn_yield_joy_progress", "SELECT")}
	if !dryRun {
		grants = append(grants, TableGrant("loyal_yield.earn_yield_joy_progress", "INSERT"), TableGrant("loyal_yield.earn_yield_joy_progress", "UPDATE"))
	}
	if missing, err := MissingGrants(ctx, c.h.YieldDB, grants); err != nil {
		return summary, err
	} else if len(missing) != 0 {
		return summary, fmt.Errorf("earn-yield-joy progress grants missing: %v", missing)
	}
	task := func(ctx context.Context) error {
		var err error
		summary, err = c.earnYieldJoy(ctx, dryRun)
		return err
	}
	if dryRun {
		err := task(ctx)
		return summary, err
	}
	acquired, err := withRunLock(ctx, c.h.appDB, "earn-yield-joy", task)
	if err == nil && !acquired {
		return summary, errCronRunning
	}
	return summary, err
}

func (c Crons) earnYieldJoy(ctx context.Context, dryRun bool) (EarnYieldJoySummary, error) {
	return c.earnYieldJoyBatch(ctx, dryRun, joyTimeBudget)
}

// Continue unfinished cycles across timer ticks, even when they cross midnight.
// Completed cycles restart at the original daily notification time, 15:41 UTC.
func (c Crons) joyProgress(ctx context.Context, now time.Time) (cycle time.Time, after string, completed bool, err error) {
	day := now.UTC().Add(-15*time.Hour - 41*time.Minute).Truncate(joyDay).Add(15*time.Hour + 41*time.Minute)
	err = c.h.YieldDB.QueryRow(ctx, `SELECT cycle_started_at, last_wallet, completed
FROM loyal_yield.earn_yield_joy_progress WHERE cluster = $1`, string(c.h.Cluster.Loyal())).Scan(&cycle, &after, &completed)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && completed && cycle.Before(day)) {
		return day, "", false, nil
	}
	return
}

func (c Crons) saveJoyProgress(ctx context.Context, cycle time.Time, after string, completed bool) error {
	_, err := c.h.YieldDB.Exec(ctx, `INSERT INTO loyal_yield.earn_yield_joy_progress (cluster, cycle_started_at, last_wallet, completed)
VALUES ($1, $2, $3, $4) ON CONFLICT (cluster) DO UPDATE SET
cycle_started_at = EXCLUDED.cycle_started_at, last_wallet = EXCLUDED.last_wallet,
completed = EXCLUDED.completed, updated_at = now()`, string(c.h.Cluster.Loyal()), cycle, after, completed)
	return err
}

func (c Crons) earnYieldJoyBatch(ctx context.Context, dryRun bool, budget time.Duration) (EarnYieldJoySummary, error) {
	now := time.Now()
	deadline := now.Add(budget)
	cluster := string(c.h.Cluster.Loyal())
	summary := EarnYieldJoySummary{DryRun: dryRun, Pushed: []EarnYieldJoyOutcome{}}
	cycle, after, completed, err := c.joyProgress(ctx, now)
	if err != nil || completed {
		return summary, err
	}
	candidates, err := c.joyCandidates(ctx, after)
	if err != nil {
		return EarnYieldJoySummary{}, err
	}
	wallets := make([]string, len(candidates))
	for i, candidate := range candidates {
		wallets[i] = candidate.wallet
	}
	states, err := c.joyStates(ctx, wallets)
	if err != nil {
		return EarnYieldJoySummary{}, err
	}
	summary.Candidates = len(candidates)
	var events []mixpanelEvent
	// Track sent pushes even when saving progress fails afterwards.
	defer func() {
		if !dryRun && len(events) > 0 {
			if err := c.Mixpanel.track(ctx, events); err != nil {
				slog.WarnContext(ctx, "[earn-yield-joy] mixpanel track failed", "errorMessage", err.Error(), "events", len(events))
			}
		}
	}()
	for start := 0; start < len(candidates); start += joyWalletConcurrency {
		if time.Now().After(deadline) {
			summary.Truncated = true
			break
		}
		chunk := candidates[start:min(start+joyWalletConcurrency, len(candidates))]
		outcomes := make([]EarnYieldJoyOutcome, len(chunk))
		var wg sync.WaitGroup
		for i, candidate := range chunk {
			wg.Go(func() {
				var state *joyState
				if found, ok := states[candidate.wallet]; ok {
					state = &found
				}
				outcome, err := c.processJoyCandidate(ctx, candidate, cluster, dryRun, now, state)
				if err != nil {
					slog.WarnContext(ctx, "[earn-yield-joy] wallet failed", "errorMessage", err.Error(), "wallet", candidate.wallet)
					outcome = EarnYieldJoyOutcome{Status: "error", Wallet: candidate.wallet}
				}
				outcomes[i] = outcome
			})
		}
		wg.Wait()
		for _, outcome := range outcomes {
			if outcome.event != nil {
				events = append(events, *outcome.event)
			}
			summary.Processed++
			switch outcome.Status {
			case "pushed":
				summary.Pushed = append(summary.Pushed, outcome)
			case "undelivered":
				summary.Undelivered++
			case "seeded":
				summary.Seeded++
			case "rebaselined":
				summary.Rebaselined++
			case "quiet":
				summary.Quiet++
			case "unavailable":
				summary.Unavailable++
			case "unregistered":
				summary.Unregistered++
			default:
				summary.Errors++
			}
		}
		// A crash may replay this chunk; the existing per-wallet push claim prevents duplicates.
		after = chunk[len(chunk)-1].wallet
		if !dryRun {
			if err := c.saveJoyProgress(ctx, cycle, after, false); err != nil {
				return summary, fmt.Errorf("save earn joy progress: %w", err)
			}
		}
	}
	if !dryRun && !summary.Truncated {
		if err := c.saveJoyProgress(ctx, cycle, after, true); err != nil {
			return summary, fmt.Errorf("complete earn joy cycle: %w", err)
		}
	}
	return summary, nil
}

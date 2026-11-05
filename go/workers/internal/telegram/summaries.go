package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	minMessagesForSummary  = 3
	dailyTriggerType       = "daily"
	maxSummaryInputChars   = 12_000
	maxSummaryBullets      = 5
	maxSummaryBulletChars  = 450
	maxSummaryOGTextChars  = 110
	summaryBulletFallback  = "No detailed points available today."
	summaryBulletTag       = `<tg-emoji emoji-id="5255883309841422076">🔹</tg-emoji>`
	summaryQualityControl  = int64(-5173720920)
	summaryWindow          = 24 * time.Hour
	summaryDailyHourUTC    = 6
	summaryCommunityBudget = 5 * time.Minute
	summaryStartParamMax   = 64

	voteLikeEmoji      = "5447485069386090205"
	voteDislikeEmoji   = "5445146433923616423"
	voteOpenEmoji      = "5235480870860660944"
	voteScoreZero      = "5447522585925426434"
	voteScoreNegative  = "5447484068658714886"
	voteScorePositive  = "5445293605272984280"
	inlineResultsLimit = 5
)

type jsDate time.Time

func (d jsDate) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Time(d).UTC().Format("2006-01-02T15:04:05.000Z"))
}

type summaryRun struct {
	DayEndUTC   jsDate `json:"dayEndUtc"`
	DayStartUTC jsDate `json:"dayStartUtc"`
	WindowEnd   jsDate `json:"windowEnd"`
	WindowStart jsDate `json:"windowStart"`
}

func newSummaryRun(now time.Time) summaryRun {
	now = now.UTC().Truncate(time.Millisecond)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return summaryRun{
		DayEndUTC:   jsDate(dayStart.Add(summaryWindow - time.Millisecond)),
		DayStartUTC: jsDate(dayStart),
		WindowEnd:   jsDate(now),
		WindowStart: jsDate(now.Add(-summaryWindow)),
	}
}

func (r summaryRun) dayKey() string { return time.Time(r.DayStartUTC).Format("2006-01-02") }

type generationStatus string

const (
	generationCreated   generationStatus = "created"
	generationExisting  generationStatus = "existing"
	generationNotEnough generationStatus = "not_enough_messages"
)

type generation struct {
	status    generationStatus
	summaryID string
}

func (b *Bot) existingSummary(ctx context.Context, where string, args ...any) (string, error) {
	var id string
	err := b.db.QueryRow(ctx, `SELECT id::text FROM summaries WHERE community_id = $1::uuid AND `+where+` ORDER BY created_at DESC LIMIT 1`, args...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (b *Bot) generateOrGetSummary(ctx context.Context, communityID, chatTitle string, run summaryRun) (generation, error) {
	triggerKey := dailyTriggerType + ":" + run.dayKey()
	windowStart, windowEnd := time.Time(run.WindowStart), time.Time(run.WindowEnd)
	var messageCount int
	if err := b.db.QueryRow(ctx, `SELECT count(*) FROM messages WHERE community_id = $1::uuid AND created_at >= $2 AND created_at <= $3`,
		communityID, windowStart, windowEnd).Scan(&messageCount); err != nil {
		return generation{}, err
	}
	if messageCount < minMessagesForSummary {
		return generation{status: generationNotEnough}, nil
	}
	id, err := b.existingSummary(ctx, `trigger_key = $2`, communityID, triggerKey)
	if err == nil && id == "" {
		id, err = b.existingSummary(ctx, `created_at >= $2 AND created_at <= $3`, communityID, time.Time(run.DayStartUTC), time.Time(run.DayEndUTC))
	}
	if err != nil {
		return generation{}, err
	}
	if id != "" {
		return generation{status: generationExisting, summaryID: id}, nil
	}
	rows, err := b.db.Query(ctx, `
		SELECT m.telegram_message_id, m.content, u.display_name
		FROM messages m JOIN users u ON u.id = m.user_id
		WHERE m.community_id = $1::uuid AND m.created_at >= $2 AND m.created_at <= $3
		ORDER BY m.telegram_message_id`, communityID, windowStart, windowEnd)
	if err != nil {
		return generation{}, err
	}
	type line struct {
		messageID     int64
		content, name string
	}
	lines, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (line, error) {
		var l line
		return l, row.Scan(&l.messageID, &l.content, &l.name)
	})
	if err != nil {
		return generation{}, err
	}
	if len(lines) < minMessagesForSummary {
		return generation{status: generationNotEnough}, nil
	}
	var transcript strings.Builder
	participants := make([]string, len(lines))
	length := 0
	for i, l := range lines {
		participants[i] = l.name
	}
	for _, l := range lines {
		text := l.name + ": " + l.content
		next := length + jsLength(text)
		if length > 0 {
			next++
		}
		if next > maxSummaryInputChars {
			if length == 0 {
				transcript.WriteString(jsSlice(text, maxSummaryInputChars))
			}
			break
		}
		if length > 0 {
			transcript.WriteByte('\n')
		}
		transcript.WriteString(text)
		length = next
	}
	if transcript.Len() == 0 {
		return generation{status: generationNotEnough}, nil
	}
	result, err := b.summaries.generate(ctx, summaryRequest{ChatTitle: chatTitle, DayKeyUTC: run.dayKey(), Participants: participants, Transcript: transcript.String()})
	if err != nil {
		return generation{}, err
	}
	topics, err := json.Marshal(result.Topics)
	if err != nil {
		return generation{}, err
	}
	err = b.db.QueryRow(ctx, `
		INSERT INTO summaries (community_id, chat_title, from_message_id, message_count, oneliner, to_message_id, topics, trigger_key, trigger_type)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7::jsonb, $8, $9)
		ON CONFLICT DO NOTHING
		RETURNING id::text`, communityID, chatTitle, lines[0].messageID, messageCount, result.Oneliner, lines[len(lines)-1].messageID,
		string(topics), triggerKey, dailyTriggerType).Scan(&id)
	if err == nil {
		slog.Info("summary generated", "community_id", communityID, "attempts", result.Attempts, "model", b.summaries.model)
		return generation{status: generationCreated, summaryID: id}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return generation{}, err
	}
	if id, err = b.existingSummary(ctx, `trigger_key = $2`, communityID, triggerKey); err != nil {
		return generation{}, err
	}
	if id == "" {
		return generation{}, errors.New("Failed to create summary")
	}
	return generation{status: generationExisting, summaryID: id}, nil
}

type storedSummary struct {
	id        string
	oneliner  string
	topics    []summaryTopic
	createdAt time.Time
}

func scanSummary(row pgx.Row, extra ...any) (*storedSummary, error) {
	var s storedSummary
	var topics []byte
	err := row.Scan(append([]any{&s.id, &s.oneliner, &topics, &s.createdAt}, extra...)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(topics, &s.topics); err != nil {
		return nil, fmt.Errorf("summary %s topics: %w", s.id, err)
	}
	return &s, nil
}

func firstSentence(text string) string {
	text = normalizeWhitespace(text)
	for i, r := range text {
		if (r == '.' || r == '!' || r == '?') && (i+1 == len(text) || text[i+1] == ' ') {
			return text[:i+1]
		}
	}
	return text
}

func summaryBody(topics []summaryTopic) (body, first string) {
	var bullets []string
	for _, topic := range topics[:min(len(topics), maxSummaryBullets)] {
		sentence := firstSentence(topic.Content)
		if sentence == "" {
			continue
		}
		if jsLength(sentence) > maxSummaryBulletChars {
			sentence = strings.TrimRight(jsSlice(sentence, maxSummaryBulletChars-1), " ") + "…"
		}
		bullets = append(bullets, sentence)
	}
	if len(bullets) == 0 {
		return summaryBulletTag + " " + htmlEscaper.Replace(summaryBulletFallback), summaryBulletFallback
	}
	lines := make([]string, len(bullets))
	for i, bullet := range bullets {
		lines[i] = summaryBulletTag + " " + htmlEscaper.Replace(bullet)
	}
	return strings.Join(lines, "\n\n"), bullets[0]
}

func (b *Bot) publicOrigin() string {
	if b.publicBase == nil {
		return "http://localhost"
	}
	return b.publicBase.Scheme + "://" + b.publicBase.Host
}

func (b *Bot) summaryText(s *storedSummary) string {
	body, first := summaryBody(s.topics)
	ogText := normalizeWhitespace(s.oneliner)
	if ogText == "" {
		ogText = first
	}
	ogText = jsSlice(ogText, maxSummaryOGTextChars)
	image := b.publicOrigin() + "/api/og/share-summary?text=" + formEscape(ogText) + "&date=" + formEscape(s.createdAt.UTC().Format("January 2"))
	return body + `<a href="` + image + `">` + "​" + `</a>`
}

func canonicalChatID(raw string) string {
	n, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return raw
	}
	return n.String()
}

func summaryVoteKeyboard(groupChatID, summaryID string, likes, dislikes int) (*Keyboard, error) {
	startParam := "sf1_" + groupChatID + "_" + summaryID
	if len(startParam) > summaryStartParamMax {
		return nil, errors.New("Start param exceeds Telegram limits")
	}
	score := likes - dislikes
	scoreEmoji := voteScoreZero
	switch {
	case score > 0:
		scoreEmoji = voteScorePositive
	case score < 0:
		scoreEmoji = voteScoreNegative
	}
	data := func(action string) string { return "sv:" + action + ":" + summaryID + ":" + groupChatID }
	return &Keyboard{Rows: [][]Button{
		{
			{Text: "Like", IconCustomEmojiID: voteLikeEmoji, CallbackData: data("u")},
			{Text: "Score: " + strconv.Itoa(score), IconCustomEmojiID: scoreEmoji, CallbackData: data("s")},
			{Text: "Dislike", IconCustomEmojiID: voteDislikeEmoji, CallbackData: data("d")},
		},
		{{Style: "primary", Text: "Open", IconCustomEmojiID: voteOpenEmoji, URL: miniAppLink + "?startapp=" + startParam}},
	}}, nil
}

func (b *Bot) voteTotals(ctx context.Context, summaryID string) (likes, dislikes int, err error) {
	err = b.db.QueryRow(ctx, `
		SELECT coalesce(sum(CASE WHEN v.action = 'LIKE' THEN 1 ELSE 0 END), 0), coalesce(sum(CASE WHEN v.action = 'DISLIKE' THEN 1 ELSE 0 END), 0)
		FROM summaries s LEFT JOIN summary_votes v ON v.summary_id = s.id
		WHERE s.id = $1::uuid GROUP BY s.id`, summaryID).Scan(&likes, &dislikes)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	return likes, dislikes, err
}

func (b *Bot) summaryPayload(ctx context.Context, s *storedSummary, sourceChatID int64) (string, *Keyboard, error) {
	likes, dislikes, err := b.voteTotals(ctx, s.id)
	if err != nil {
		return "", nil, err
	}
	keyboard, err := summaryVoteKeyboard(strconv.FormatInt(sourceChatID, 10), s.id, likes, dislikes)
	if err != nil {
		return "", nil, err
	}
	return b.summaryText(s), keyboard, nil
}

type delivery struct {
	reason      string
	destination int64
	messageID   int64
}

func (b *Bot) deliverSummary(ctx context.Context, s *storedSummary, sourceChatID, destination, replyTo int64) (delivery, error) {
	text, keyboard, err := b.summaryPayload(ctx, s, sourceChatID)
	if err != nil {
		return delivery{}, err
	}
	opts := SendOptions{ParseMode: "HTML", PreferLargeMedia: true, Markup: keyboard}
	if replyTo != 0 {
		replyOpts := opts
		replyOpts.ReplyTo = replyTo
		messageID, err := b.api.Send(ctx, destination, text, replyOpts)
		if err == nil {
			return delivery{destination: destination, messageID: messageID}, nil
		}
		slog.Warn("summary reply failed, sending without reply", "chat_id", destination, "error", err)
	}
	messageID, err := b.api.Send(ctx, destination, text, opts)
	if err != nil {
		return delivery{}, err
	}
	return delivery{destination: destination, messageID: messageID}, nil
}

const summaryColumns = `s.id::text, s.oneliner, s.topics, s.created_at`

func (b *Bot) sendSummaryByID(ctx context.Context, summaryID string) (delivery, error) {
	var chatID int64
	var active, enabled bool
	s, err := scanSummary(b.db.QueryRow(ctx, `
		SELECT `+summaryColumns+`, c.chat_id, c.is_active, c.summary_notifications_enabled
		FROM summaries s JOIN communities c ON c.id = s.community_id WHERE s.id = $1::uuid`, summaryID), &chatID, &active, &enabled)
	switch {
	case err != nil:
		return delivery{}, err
	case s == nil:
		return delivery{reason: "no_summaries"}, nil
	case !active:
		return delivery{reason: "not_activated"}, nil
	case !enabled:
		return delivery{reason: "notifications_disabled"}, nil
	}
	return b.deliverSummary(ctx, s, chatID, chatID, 0)
}

func (b *Bot) sendLatestSummary(ctx context.Context, sourceChatID, destination, replyTo int64) (delivery, error) {
	var communityID string
	var enabled bool
	err := b.db.QueryRow(ctx, `SELECT id::text, summary_notifications_enabled FROM communities WHERE chat_id = $1 AND is_active LIMIT 1`, sourceChatID).Scan(&communityID, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return delivery{reason: "not_activated"}, nil
	}
	if err != nil {
		return delivery{}, err
	}
	if !enabled {
		return delivery{reason: "notifications_disabled"}, nil
	}
	s, err := scanSummary(b.db.QueryRow(ctx, `SELECT `+summaryColumns+` FROM summaries s WHERE s.community_id = $1::uuid ORDER BY s.created_at DESC LIMIT 1`, communityID))
	if err != nil {
		return delivery{}, err
	}
	if s == nil {
		return delivery{reason: "no_summaries"}, nil
	}
	return b.deliverSummary(ctx, s, sourceChatID, destination, replyTo)
}

type summaryStats struct {
	ActiveCommunities       int `json:"activeCommunities"`
	DeliveryAttempted       int `json:"deliveryAttempted"`
	DeliveryFailed          int `json:"deliveryFailed"`
	DeliverySucceeded       int `json:"deliverySucceeded"`
	Errors                  int `json:"errors"`
	ExistingToday           int `json:"existingToday"`
	Generated               int `json:"generated"`
	Processed               int `json:"processed"`
	SkippedByMasterSwitch   int `json:"skippedByMasterSwitch"`
	SkippedNotEnoughMessage int `json:"skippedNotEnoughMessages"`
}

type summaryFailure struct {
	ChatID         string   `json:"chatId"`
	CommunityID    string   `json:"communityId"`
	Error          string   `json:"error"`
	FailureReasons []string `json:"failureReasons,omitempty"`
	Scope          string   `json:"scope"`
	SummaryID      string   `json:"summaryId,omitempty"`
}

type SummaryReport struct {
	Errors  []summaryFailure `json:"errors"`
	OK      bool             `json:"ok"`
	Run     summaryRun       `json:"run"`
	Skipped bool             `json:"skipped"`
	Stats   summaryStats     `json:"stats"`
}

func (b *Bot) RunSummaries(ctx context.Context, now time.Time) (SummaryReport, error) {
	report := SummaryReport{Errors: []summaryFailure{}, Run: newSummaryRun(now)}
	rows, err := b.db.Query(ctx, `SELECT id::text, chat_id, chat_title, summary_notifications_enabled FROM communities WHERE is_active`)
	if err != nil {
		return report, err
	}
	type activeCommunity struct {
		id, title string
		chatID    int64
		enabled   bool
	}
	communities, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (activeCommunity, error) {
		var c activeCommunity
		return c, row.Scan(&c.id, &c.chatID, &c.title, &c.enabled)
	})
	if err != nil {
		return report, err
	}
	stats := &report.Stats
	stats.ActiveCommunities = len(communities)
	fail := func(c activeCommunity, scope, summaryID string, err error) {
		stats.Errors++
		failure := summaryFailure{ChatID: strconv.FormatInt(c.chatID, 10), CommunityID: c.id, Error: err.Error(), Scope: scope, SummaryID: summaryID}
		var exhausted *retryExhausted
		if errors.As(err, &exhausted) {
			failure.FailureReasons = exhausted.reasons
		}
		report.Errors = append(report.Errors, failure)
		slog.Error("community summary processing failed", "scope", scope, "community_id", c.id, "chat_id", c.chatID, "summary_id", summaryID, "error", err)
	}
	for _, c := range communities {
		stats.Processed++
		communityCtx, cancel := context.WithTimeout(ctx, summaryCommunityBudget)
		g, err := b.generateOrGetSummary(communityCtx, c.id, c.title, report.Run)
		if err != nil {
			cancel()
			fail(c, "generation", "", err)
			continue
		}
		switch g.status {
		case generationNotEnough:
			stats.SkippedNotEnoughMessage++
			cancel()
			continue
		case generationCreated:
			stats.Generated++
		case generationExisting:
			stats.ExistingToday++
		}
		if !c.enabled {
			stats.SkippedByMasterSwitch++
			cancel()
			continue
		}
		stats.DeliveryAttempted++
		d, err := b.sendSummaryByID(communityCtx, g.summaryID)
		switch {
		case err != nil:
			stats.DeliveryFailed++
			fail(c, "delivery", g.summaryID, err)
		case d.reason != "":
			stats.DeliveryFailed++
			fail(c, "delivery", g.summaryID, errors.New("Summary delivery rejected: "+d.reason))
		default:
			stats.DeliverySucceeded++
			if err := b.api.ForwardMessage(communityCtx, summaryQualityControl, d.destination, d.messageID); err != nil {
				slog.Error("summary quality control forward failed", "summary_id", g.summaryID, "chat_id", d.destination, "message_id", d.messageID, "error", err)
			} else {
				slog.Info("summary forwarded to quality control", "summary_id", g.summaryID, "chat_id", d.destination, "message_id", d.messageID)
			}
		}
		cancel()
	}
	report.OK = stats.Errors == 0
	return report, nil
}

func (b *Bot) RunDailySummaries(ctx context.Context) {
	for {
		now := time.Now().UTC()
		next := time.Date(now.Year(), now.Month(), now.Day(), summaryDailyHourUTC, 0, 0, 0, time.UTC)
		if !next.After(now) {
			next = next.AddDate(0, 0, 1)
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		report, err := b.RunSummaries(ctx, time.Now())
		if err != nil {
			slog.Error("daily summaries failed", "error", err)
			continue
		}
		slog.Info("daily summaries done", "ok", report.OK, "stats", report.Stats)
	}
}

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"
)

const (
	communitiesModule      = "communities/community-list-actions"
	communitiesSettingsMod = "communities/[communityId]/community-notification-settings-actions"
	communitiesPageSize    = 20
	communitiesSummarySize = 10
	communitiesWindowDays  = 30
)

var communitiesUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type CommunitiesPage struct {
	Rows                      []CommunityListRow    `json:"rows" nullable:"false"`
	SummaryCountByCommunityID map[string]int        `json:"summaryCountByCommunityId" nullable:"false"`
	TotalCountByParserType    CommunityParserCounts `json:"totalCountByParserType"`
	CurrentPage               int                   `json:"currentPage"`
	TotalPages                int                   `json:"totalPages"`
	HasPreviousPage           bool                  `json:"hasPreviousPage"`
	HasNextPage               bool                  `json:"hasNextPage"`
	ParserType                string                `json:"parserType" enum:"bot,userbot"`
	CommunityTrends           CommunityTrends       `json:"communityTrends"`
}

type CommunityListRow struct {
	ID                          string `json:"id"`
	ChatTitle                   string `json:"chatTitle"`
	ChatID                      string `json:"chatId"`
	IsActive                    bool   `json:"isActive"`
	SummaryNotificationsEnabled bool   `json:"summaryNotificationsEnabled"`
	IsPublic                    bool   `json:"isPublic"`
}

type CommunityParserCounts struct {
	Bot     int `json:"bot"`
	Userbot int `json:"userbot"`
}

type CommunityTrends struct {
	ChartPoints            []CommunitySummariesPoint `json:"chartPoints" nullable:"false"`
	Totals30d              CommunityActivityTotals   `json:"totals30d"`
	CommunitiesChartPoints []CommunityGrowthPoint    `json:"communitiesChartPoints" nullable:"false"`
	CommunitiesTotals30d   CommunityGrowthTotals     `json:"communitiesTotals30d"`
}

type CommunitySummariesPoint struct {
	Date      string `json:"date"`
	Summaries int    `json:"summaries"`
}

type CommunityGrowthPoint struct {
	Date        string `json:"date"`
	Communities int    `json:"communities"`
}

type CommunityActivityTotals struct {
	Summaries int `json:"summaries"`
	Messages  int `json:"messages"`
}

type CommunityGrowthTotals struct {
	Communities int `json:"communities"`
	Users       int `json:"users"`
}

type CommunityDetailPage struct {
	Community        CommunityDetail             `json:"community"`
	Summaries        CommunitySummariesPageData  `json:"summariesPage"`
	ChartPoints      []CommunitySummariesPoint   `json:"chartPoints" nullable:"false"`
	MessagesPoints   []CommunityMessagesPoint    `json:"messagesChartPoints" nullable:"false"`
	ActiveUsersPoint []CommunityActiveUsersPoint `json:"activeUsersChartPoints" nullable:"false"`
	TopUsersPoints   []CommunityTopUserPoint     `json:"topUsersChartPoints" nullable:"false"`
	Totals30d        CommunityActivityTotals     `json:"totals30d"`
	MessagesStats    CommunityDailyStats         `json:"messagesStats30d"`
	ActiveUsersStats CommunityDailyStats         `json:"activeUsersStats30d"`
}

type CommunityDetail struct {
	ID                              string `json:"id"`
	ChatTitle                       string `json:"chatTitle"`
	ChatID                          string `json:"chatId"`
	IsActive                        bool   `json:"isActive"`
	IsPublic                        bool   `json:"isPublic"`
	SummaryNotificationsEnabled     bool   `json:"summaryNotificationsEnabled"`
	SummaryNotificationTimeHours    *int   `json:"summaryNotificationTimeHours" enum:"24,48"`
	SummaryNotificationMessageCount *int   `json:"summaryNotificationMessageCount" enum:"500,1000"`
}

type CommunityMessagesPoint struct {
	Date     string `json:"date"`
	Messages int    `json:"messages"`
}

type CommunityActiveUsersPoint struct {
	Date        string `json:"date"`
	ActiveUsers int    `json:"activeUsers"`
}

type CommunityTopUserPoint struct {
	UserLabel    string `json:"userLabel"`
	MessageCount int    `json:"messageCount"`
}

type CommunityDailyStats struct {
	AveragePerDay float64 `json:"averagePerDay"`
	MedianPerDay  float64 `json:"medianPerDay"`
}

type CommunitySummariesPageData struct {
	Rows            []CommunitySummaryRow `json:"rows" nullable:"false"`
	TotalCount      int                   `json:"totalCount"`
	CurrentPage     int                   `json:"currentPage"`
	TotalPages      int                   `json:"totalPages"`
	HasPreviousPage bool                  `json:"hasPreviousPage"`
	HasNextPage     bool                  `json:"hasNextPage"`
}

type CommunitySummaryRow struct {
	ID            string           `json:"id"`
	CreatedAt     string           `json:"createdAt"`
	Oneliner      string           `json:"oneliner"`
	Topics        []CommunityTopic `json:"topics" nullable:"false"`
	LikesCount    int              `json:"likesCount"`
	DislikesCount int              `json:"dislikesCount"`
}

type CommunityTopic struct {
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Sources []string `json:"sources" nullable:"false"`
}

type CommunityResult struct {
	Error   string `json:"error,omitempty"`
	Success bool   `json:"success,omitempty"`
}

func communitiesPositive(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

func communitiesPaging(total, size, requested int) (totalPages, current, offset int) {
	totalPages = 1
	if total > 0 {
		totalPages = (total + size - 1) / size
	}
	current = min(max(requested, 1), totalPages)
	return totalPages, current, (current - 1) * size
}

func communitiesWindow(now time.Time) (start, end time.Time, days []string) {
	now = now.UTC()
	end = time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	start = end.AddDate(0, 0, -communitiesWindowDays)
	for i := range communitiesWindowDays {
		days = append(days, start.AddDate(0, 0, i).Format("2006-01-02"))
	}
	return start, end, days
}

func communitiesDayQuery(table, column, count, filter string, from int) string {
	return fmt.Sprintf(`SELECT to_char((date_trunc('day', %[2]s AT TIME ZONE 'UTC'))::date, 'YYYY-MM-DD') AS day, %[3]s
FROM %[1]s WHERE %[4]s%[2]s >= $%[5]d AND %[2]s < $%[6]d GROUP BY 1`, table, column, count, filter, from, from+1)
}

const communitiesScoped = "community_id = $1 AND "

func communitiesDailyCounts(ctx context.Context, d *Deps, query string, args ...any) (map[string]int, error) {
	counts := map[string]int{}
	err := d.AppRead(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var day string
			var n int
			if err := rows.Scan(&day, &n); err != nil {
				return err
			}
			counts[day] = n
		}
		return rows.Err()
	})
	return counts, err
}

func communitiesDailyCountsAll(ctx context.Context, d *Deps, queries []string, args ...any) ([]map[string]int, error) {
	byDay := make([]map[string]int, len(queries))
	group, ctx := errgroup.WithContext(ctx)
	for i, query := range queries {
		group.Go(func() (err error) {
			byDay[i], err = communitiesDailyCounts(ctx, d, query, args...)
			return err
		})
	}
	return byDay, group.Wait()
}

func loadCommunityTrends(ctx context.Context, d *Deps) (CommunityTrends, error) {
	start, end, days := communitiesWindow(time.Now())
	trends := CommunityTrends{ChartPoints: []CommunitySummariesPoint{}, CommunitiesChartPoints: []CommunityGrowthPoint{}}

	queries := []string{
		communitiesDayQuery("summaries", "created_at", "count(*)::int", "", 1),
		communitiesDayQuery("messages", "created_at", "count(*)::int", "", 1),
		communitiesDayQuery("communities", "updated_at", "count(*)::int", "", 1),
		communitiesDayQuery("users", "created_at", "count(*)::int", "", 1),
	}
	byDay, err := communitiesDailyCountsAll(ctx, d, queries, start, end)
	if err != nil {
		return trends, err
	}
	summariesByDay, messagesByDay, communitiesByDay, usersByDay := byDay[0], byDay[1], byDay[2], byDay[3]

	var runningSummaries, runningCommunities int
	for _, day := range days {
		runningSummaries += summariesByDay[day]
		trends.Totals30d.Messages += messagesByDay[day]
		runningCommunities += communitiesByDay[day]
		trends.CommunitiesTotals30d.Users += usersByDay[day]
		trends.ChartPoints = append(trends.ChartPoints, CommunitySummariesPoint{Date: day, Summaries: runningSummaries})
		trends.CommunitiesChartPoints = append(trends.CommunitiesChartPoints, CommunityGrowthPoint{Date: day, Communities: runningCommunities})
	}
	trends.Totals30d.Summaries = runningSummaries
	trends.CommunitiesTotals30d.Communities = runningCommunities
	return trends, nil
}

func loadCommunities(ctx context.Context, d *Deps, req PageRequest) (CommunitiesPage, error) {
	parserType := "bot"
	if raw := req.Query.Get("parserType"); raw == "bot" || raw == "userbot" {
		parserType = raw
	}
	page := CommunitiesPage{
		Rows:                      []CommunityListRow{},
		SummaryCountByCommunityID: map[string]int{},
		ParserType:                parserType,
	}
	err := d.AppRead(ctx, func(tx pgx.Tx) error {
		totals, err := tx.Query(ctx, `SELECT parser_type, count(*)::int FROM communities GROUP BY parser_type`)
		if err != nil {
			return err
		}
		for totals.Next() {
			var kind string
			var n int
			if err := totals.Scan(&kind, &n); err != nil {
				return err
			}
			switch kind {
			case "bot":
				page.TotalCountByParserType.Bot = n
			case "userbot":
				page.TotalCountByParserType.Userbot = n
			}
		}
		if err := totals.Err(); err != nil {
			return err
		}
		total := page.TotalCountByParserType.Bot
		if parserType == "userbot" {
			total = page.TotalCountByParserType.Userbot
		}
		var offset int
		page.TotalPages, page.CurrentPage, offset = communitiesPaging(total, communitiesPageSize, communitiesPositive(req.Query.Get("page")))
		page.HasPreviousPage, page.HasNextPage = page.CurrentPage > 1, page.CurrentPage < page.TotalPages

		rows, err := tx.Query(ctx, `SELECT id::text, chat_title, chat_id::text, is_active, summary_notifications_enabled, is_public
FROM communities WHERE parser_type = $1 ORDER BY activated_at DESC, id DESC LIMIT $2 OFFSET $3`, parserType, communitiesPageSize, offset)
		if err != nil {
			return err
		}
		page.Rows, err = pgx.CollectRows(rows, pgx.RowToStructByPos[CommunityListRow])
		if err != nil || len(page.Rows) == 0 {
			return err
		}
		ids := make([]string, len(page.Rows))
		for i, row := range page.Rows {
			ids[i] = row.ID
		}
		counts, err := tx.Query(ctx, `SELECT community_id::text, count(*)::int FROM summaries WHERE community_id = ANY($1::uuid[]) GROUP BY community_id`, ids)
		if err != nil {
			return err
		}
		defer counts.Close()
		for counts.Next() {
			var id string
			var n int
			if err := counts.Scan(&id, &n); err != nil {
				return err
			}
			page.SummaryCountByCommunityID[id] = n
		}
		return counts.Err()
	})
	if err != nil {
		return page, err
	}
	page.CommunityTrends, err = loadCommunityTrends(ctx, d)
	return page, err
}

func communitiesTopics(raw []byte) []CommunityTopic {
	topics := []CommunityTopic{}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return topics
	}
	for _, item := range items {
		var candidate struct {
			Title   *string         `json:"title"`
			Content *string         `json:"content"`
			Sources json.RawMessage `json:"sources"`
		}
		if json.Unmarshal(item, &candidate) != nil || candidate.Title == nil || candidate.Content == nil {
			continue
		}
		topic := CommunityTopic{Title: *candidate.Title, Content: *candidate.Content, Sources: []string{}}
		var sources []json.RawMessage
		if json.Unmarshal(candidate.Sources, &sources) == nil {
			for _, source := range sources {
				var s string
				if json.Unmarshal(source, &s) == nil && strings.TrimSpace(s) != "" {
					topic.Sources = append(topic.Sources, strings.TrimSpace(s))
				}
			}
		}
		topics = append(topics, topic)
	}
	return topics
}

func communitiesTopUserLabel(userID string, displayName, username *string) string {
	if displayName != nil && strings.TrimSpace(*displayName) != "" {
		return strings.TrimSpace(*displayName)
	}
	if username != nil {
		if name := strings.TrimLeft(strings.TrimSpace(*username), "@"); name != "" {
			return "@" + name
		}
	}
	return userID[:min(8, len(userID))]
}

func communitiesStats(daily []int) CommunityDailyStats {
	total := 0
	for _, n := range daily {
		total += n
	}
	sorted := slices.Sorted(slices.Values(daily))
	mid := len(sorted) / 2
	median := float64(sorted[mid])
	if len(sorted)%2 == 0 {
		median = float64(sorted[mid-1]+sorted[mid]) / 2
	}
	return CommunityDailyStats{AveragePerDay: float64(total) / float64(len(daily)), MedianPerDay: median}
}

func loadCommunityDetail(ctx context.Context, d *Deps, req PageRequest) (CommunityDetailPage, error) {
	id := req.Params["communityId"]
	page := CommunityDetailPage{
		ChartPoints:      []CommunitySummariesPoint{},
		MessagesPoints:   []CommunityMessagesPoint{},
		ActiveUsersPoint: []CommunityActiveUsersPoint{},
		TopUsersPoints:   []CommunityTopUserPoint{},
		Summaries:        CommunitySummariesPageData{Rows: []CommunitySummaryRow{}},
	}
	if !communitiesUUID.MatchString(id) {
		return page, huma.Error404NotFound("community not found")
	}
	found := false
	err := d.AppRead(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT id::text, chat_title, chat_id::text, is_active, is_public, summary_notifications_enabled,
summary_notification_time_hours, summary_notification_message_count FROM communities WHERE id = $1`, id).
			Scan(&page.Community.ID, &page.Community.ChatTitle, &page.Community.ChatID, &page.Community.IsActive, &page.Community.IsPublic,
				&page.Community.SummaryNotificationsEnabled, &page.Community.SummaryNotificationTimeHours, &page.Community.SummaryNotificationMessageCount)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true

		var total int
		if err := tx.QueryRow(ctx, `SELECT count(*)::int FROM summaries WHERE community_id = $1`, id).Scan(&total); err != nil {
			return err
		}
		s := &page.Summaries
		s.TotalCount = total
		var offset int
		s.TotalPages, s.CurrentPage, offset = communitiesPaging(total, communitiesSummarySize, communitiesPositive(req.Query.Get("page")))
		s.HasPreviousPage, s.HasNextPage = s.CurrentPage > 1, s.CurrentPage < s.TotalPages

		rows, err := tx.Query(ctx, `SELECT id::text, created_at, oneliner, topics FROM summaries
WHERE community_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`, id, communitiesSummarySize, offset)
		if err != nil {
			return err
		}
		byID := map[string]int{}
		ids := []string{}
		for rows.Next() {
			var row CommunitySummaryRow
			var created time.Time
			var topics []byte
			if err := rows.Scan(&row.ID, &created, &row.Oneliner, &topics); err != nil {
				return err
			}
			row.CreatedAt, row.Topics = libraryISO(created), communitiesTopics(topics)
			byID[row.ID] = len(s.Rows)
			ids = append(ids, row.ID)
			s.Rows = append(s.Rows, row)
		}
		if err := rows.Err(); err != nil || len(ids) == 0 {
			return err
		}
		votes, err := tx.Query(ctx, `SELECT summary_id::text, action, count(*)::int FROM summary_votes
WHERE summary_id = ANY($1::uuid[]) GROUP BY summary_id, action`, ids)
		if err != nil {
			return err
		}
		defer votes.Close()
		for votes.Next() {
			var summaryID, action string
			var n int
			if err := votes.Scan(&summaryID, &action, &n); err != nil {
				return err
			}
			switch strings.ToUpper(action) {
			case "LIKE":
				s.Rows[byID[summaryID]].LikesCount += n
			case "DISLIKE":
				s.Rows[byID[summaryID]].DislikesCount += n
			}
		}
		return votes.Err()
	})
	if err != nil {
		return page, err
	}
	if !found {
		return page, huma.Error404NotFound("community not found")
	}

	start, end, days := communitiesWindow(time.Now())
	queries := []string{
		communitiesDayQuery("summaries", "created_at", "count(*)::int", communitiesScoped, 2),
		communitiesDayQuery("messages", "created_at", "count(*)::int", communitiesScoped, 2),
		communitiesDayQuery("messages", "created_at", "count(DISTINCT user_id)::int", communitiesScoped, 2),
	}
	byDay, err := communitiesDailyCountsAll(ctx, d, queries, id, start, end)
	if err != nil {
		return page, err
	}
	summariesByDay, messagesByDay, activeByDay := byDay[0], byDay[1], byDay[2]

	var running int
	dailyMessages, dailyActive := []int{}, []int{}
	for _, day := range days {
		running += summariesByDay[day]
		page.Totals30d.Messages += messagesByDay[day]
		dailyMessages, dailyActive = append(dailyMessages, messagesByDay[day]), append(dailyActive, activeByDay[day])
		page.ChartPoints = append(page.ChartPoints, CommunitySummariesPoint{Date: day, Summaries: running})
		page.MessagesPoints = append(page.MessagesPoints, CommunityMessagesPoint{Date: day, Messages: messagesByDay[day]})
		page.ActiveUsersPoint = append(page.ActiveUsersPoint, CommunityActiveUsersPoint{Date: day, ActiveUsers: activeByDay[day]})
	}
	page.Totals30d.Summaries = running
	page.MessagesStats, page.ActiveUsersStats = communitiesStats(dailyMessages), communitiesStats(dailyActive)

	err = d.AppRead(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT m.user_id::text, u.display_name, u.username, count(m.id)::int
FROM messages m LEFT JOIN users u ON m.user_id = u.id
WHERE m.community_id = $1 AND m.created_at >= $2 AND m.created_at < $3
GROUP BY m.user_id, u.display_name, u.username
ORDER BY count(m.id) DESC, m.user_id ASC LIMIT 20`, id, start, end)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var userID string
			var displayName, username *string
			var n int
			if err := rows.Scan(&userID, &displayName, &username, &n); err != nil {
				return err
			}
			page.TopUsersPoints = append(page.TopUsersPoints, CommunityTopUserPoint{UserLabel: communitiesTopUserLabel(userID, displayName, username), MessageCount: n})
		}
		return rows.Err()
	})
	return page, err
}

func communitiesChoice(call Call, key string, allowed ...string) (string, bool) {
	value := "off"
	if call.Form != nil && len(call.Form.Value[key]) > 0 {
		value = call.Form.Value[key][0]
	}
	return value, slices.Contains(allowed, value)
}

func registerCommunities(m *Mux) {
	Page(m, "/communities", loadCommunities)
	Page(m, "/communities/{communityId}", loadCommunityDetail)

	Action(m, communitiesModule, "setCommunityActiveStatus", func(ctx context.Context, d *Deps, call Call) (*CommunityResult, error) {
		var id string
		var active bool
		if err := call.Arg(0, &id); err != nil {
			return nil, err
		}
		if err := call.Arg(1, &active); err != nil {
			return nil, err
		}
		if !communitiesUUID.MatchString(id) {
			return &CommunityResult{Error: "Invalid community ID"}, nil
		}
		var updated int64
		err := d.AppWrite(ctx, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE communities SET is_active = $2,
summary_notifications_enabled = summary_notifications_enabled AND $2, updated_at = now() WHERE id = $1`, id, active)
			updated = tag.RowsAffected()
			return err
		})
		switch {
		case err != nil:
			return &CommunityResult{Error: "Failed to update community status"}, nil
		case updated == 0:
			return &CommunityResult{Error: "Community not found"}, nil
		}
		return &CommunityResult{Success: true}, nil
	})

	Action(m, communitiesSettingsMod, "updateCommunityNotificationSettings", func(ctx context.Context, d *Deps, call Call) (*struct{}, error) {
		var id string
		if err := call.Arg(0, &id); err != nil {
			return nil, err
		}
		perDay, dayOK := communitiesChoice(call, "perDay", "off", "24", "48")
		perMessage, messageOK := communitiesChoice(call, "perMessage", "off", "500", "1000")
		if !communitiesUUID.MatchString(id) || !dayOK || !messageOK {
			return nil, nil
		}
		var hours, messages *int
		if n, err := strconv.Atoi(perDay); err == nil {
			hours = &n
		}
		if n, err := strconv.Atoi(perMessage); err == nil {
			messages = &n
		}
		master, active, public := call.FormValue("masterSwitch") == "on", call.FormValue("isActive") == "on", call.FormValue("isPublic") == "on"
		return nil, d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE communities SET summary_notifications_enabled = $2, summary_notification_time_hours = $3,
summary_notification_message_count = $4, is_active = $5, is_public = $6, updated_at = now() WHERE id = $1`,
				id, active && master, hours, messages, active, public)
			return err
		})
	})
}

package webapp

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const contentCacheControl = "public, max-age=60, s-maxage=300, stale-while-revalidate=600"

type ContentHeaders struct {
	Source       string `header:"X-Loyal-API-Source"`
	AllowOrigin  string `header:"Access-Control-Allow-Origin"`
	AllowMethods string `header:"Access-Control-Allow-Methods"`
	AllowHeaders string `header:"Access-Control-Allow-Headers"`
}

func newContentHeaders() ContentHeaders {
	return ContentHeaders{Source: "go", AllowOrigin: "*", AllowMethods: "GET, OPTIONS", AllowHeaders: "Content-Type"}
}

type contentError struct {
	Message string `json:"error"`
	status  int
}

func (e *contentError) Error() string  { return e.Message }
func (e *contentError) GetStatus() int { return e.status }

func contentFailure(status int, message string) error {
	return huma.ErrorWithHeaders(&contentError{Message: message, status: status}, http.Header{
		"X-Loyal-API-Source":           {"go"},
		"Access-Control-Allow-Origin":  {"*"},
		"Access-Control-Allow-Methods": {"GET, OPTIONS"},
		"Access-Control-Allow-Headers": {"Content-Type"},
	})
}

type preflightOutput struct{ ContentHeaders }

func registerPreflight(api huma.API, path string) {
	huma.Register(api, huma.Operation{
		OperationID:   "options" + path,
		Method:        http.MethodOptions,
		Path:          path,
		DefaultStatus: http.StatusNoContent,
		Hidden:        true,
	}, func(context.Context, *struct{}) (*preflightOutput, error) {
		return &preflightOutput{newContentHeaders()}, nil
	})
}

func registerReads[I, O any](api huma.API, id, path, summary string, handler func(context.Context, *I) (*O, error)) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		huma.Register(api, huma.Operation{
			OperationID: method + "-" + id,
			Method:      method,
			Path:        path,
			Summary:     summary,
		}, handler)
	}
	registerPreflight(api, path)
}

func readContent(ctx context.Context, pool *pgxpool.Pool, read func(context.Context, pgx.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = 10000"); err != nil {
		return err
	}
	return read(ctx, tx)
}

func isoMillis(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func isoMillisPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := isoMillis(*t)
	return &s
}

func registerContent(api huma.API, appDB *pgxpool.Pool) {
	registerReads(api, "summaries", "/api/summaries", "Public community summaries", func(ctx context.Context, in *summariesInput) (*summariesOutput, error) {
		var chatID *int64
		if in.GroupChatID != "" {
			id, err := strconv.ParseInt(in.GroupChatID, 10, 64)
			if err != nil {
				return nil, contentFailure(http.StatusBadRequest, "Invalid groupChatId")
			}
			chatID = &id
		}
		body, err := loadSummaries(ctx, appDB, chatID)
		if err != nil {
			slog.Warn("summaries unavailable", "error", err)
			return nil, contentFailure(http.StatusInternalServerError, "Failed to fetch summaries")
		}
		return &summariesOutput{ContentHeaders: newContentHeaders(), Body: body}, nil
	})
	registerReads(api, "mobile-dapps", "/api/mobile/dapps", "Trusted dApps for the mobile browser", func(ctx context.Context, _ *struct{}) (*dappsOutput, error) {
		body, err := loadTrustedDapps(ctx, appDB)
		if err != nil {
			slog.Warn("dapps unavailable", "error", err)
			return nil, contentFailure(http.StatusInternalServerError, "Failed to fetch dapps")
		}
		return &dappsOutput{ContentHeaders: newContentHeaders(), CacheControl: contentCacheControl, Body: body}, nil
	})
	registerReads(api, "mobile-library", "/api/mobile/library", "Mobile library sections and articles", func(ctx context.Context, _ *struct{}) (*libraryOutput, error) {
		body, err := loadLibrary(ctx, appDB)
		if err != nil {
			slog.Warn("library unavailable", "error", err)
			return nil, contentFailure(http.StatusInternalServerError, "Failed to fetch library")
		}
		return &libraryOutput{ContentHeaders: newContentHeaders(), CacheControl: contentCacheControl, Body: body}, nil
	})
}

type summariesInput struct {
	GroupChatID string `query:"groupChatId" doc:"Telegram chat id; omit for every public community"`
}

type summariesOutput struct {
	ContentHeaders
	Body *SummariesResponse
}

type SummariesResponse struct {
	Summaries []Summary `json:"summaries" nullable:"false"`
}

type Summary struct {
	ID           string         `json:"id"`
	ChatID       string         `json:"chatId"`
	Title        string         `json:"title"`
	MessageCount int            `json:"messageCount"`
	Oneliner     string         `json:"oneliner"`
	PhotoURL     *string        `json:"photoUrl,omitempty"`
	PhotoBase64  *string        `json:"photoBase64,omitempty" doc:"Deprecated compatibility field"`
	PhotoMime    *string        `json:"photoMimeType,omitempty" doc:"Deprecated compatibility field"`
	Topics       []SummaryTopic `json:"topics" nullable:"false"`
	CreatedAt    string         `json:"createdAt"`
}

type SummaryTopic struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Sources []string `json:"sources" nullable:"false"`
}

func loadSummaries(ctx context.Context, pool *pgxpool.Pool, chatID *int64) (*SummariesResponse, error) {
	body := &SummariesResponse{Summaries: []Summary{}}
	err := readContent(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT s.id::text, c.chat_id::text, s.chat_title, s.message_count, s.oneliner, c.settings, s.topics, s.created_at
FROM summaries s JOIN communities c ON c.id = s.community_id
WHERE c.is_public AND ($1::bigint IS NULL OR c.chat_id = $1)
ORDER BY s.created_at DESC, s.id`, chatID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s Summary
			var settings map[string]any
			var topics []SummaryTopic
			var created time.Time
			if err := rows.Scan(&s.ID, &s.ChatID, &s.Title, &s.MessageCount, &s.Oneliner, &settings, &topics, &created); err != nil {
				return err
			}
			s.PhotoURL, s.PhotoBase64, s.PhotoMime = settingString(settings, "photoUrl"), settingString(settings, "photoBase64"), settingString(settings, "photoMimeType")
			for i := range topics {
				topics[i].ID = s.ID + "-" + strconv.Itoa(i)
				if topics[i].Sources == nil {
					topics[i].Sources = []string{}
				}
			}
			s.Topics, s.CreatedAt = topics, isoMillis(created)
			body.Summaries = append(body.Summaries, s)
		}
		return rows.Err()
	})
	return body, err
}

func settingString(settings map[string]any, key string) *string {
	if v, ok := settings[key].(string); ok {
		return &v
	}
	return nil
}

type dappsOutput struct {
	ContentHeaders
	CacheControl string `header:"Cache-Control"`
	Body         *MobileTrustedDappsResponse
}

type MobileTrustedDappsResponse struct {
	Dapps []MobileTrustedDapp `json:"dapps" nullable:"false"`
}

type MobileTrustedDapp struct {
	ID           string  `json:"id"`
	Origin       string  `json:"origin"`
	Name         string  `json:"name"`
	StartURL     string  `json:"startUrl"`
	Category     *string `json:"category"`
	DisplayOrder int     `json:"displayOrder"`
}

func loadTrustedDapps(ctx context.Context, pool *pgxpool.Pool) (*MobileTrustedDappsResponse, error) {
	body := &MobileTrustedDappsResponse{Dapps: []MobileTrustedDapp{}}
	err := readContent(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text, origin, name, start_url, category, display_order
FROM trusted_dapps WHERE is_active ORDER BY display_order, name, id`)
		if err != nil {
			return err
		}
		body.Dapps, err = pgx.CollectRows(rows, pgx.RowToStructByPos[MobileTrustedDapp])
		return err
	})
	return body, err
}

type libraryOutput struct {
	ContentHeaders
	CacheControl string `header:"Cache-Control"`
	Body         *MobileLibraryResponse
}

type MobileLibraryResponse struct {
	Featured []MobileLibraryArticle `json:"featured" nullable:"false"`
	Sections []MobileLibrarySection `json:"sections" nullable:"false"`
}

type MobileLibrarySection struct {
	ID           string                 `json:"id"`
	Title        string                 `json:"title"`
	DisplayOrder int                    `json:"displayOrder"`
	Articles     []MobileLibraryArticle `json:"articles" nullable:"false"`
}

type MobileLibraryArticle struct {
	ID              string  `json:"id"`
	SectionID       string  `json:"sectionId"`
	Title           string  `json:"title"`
	CoverImageURL   string  `json:"coverImageUrl"`
	ContentMarkdown string  `json:"contentMarkdown"`
	ReadTime        string  `json:"readTime"`
	Excerpt         *string `json:"excerpt"`
	IsFeatured      bool    `json:"isFeatured"`
	DisplayOrder    int     `json:"displayOrder"`
	PublishedAt     *string `json:"publishedAt" doc:"ISO 8601 UTC"`
}

func loadLibrary(ctx context.Context, pool *pgxpool.Pool) (*MobileLibraryResponse, error) {
	body := &MobileLibraryResponse{Featured: []MobileLibraryArticle{}, Sections: []MobileLibrarySection{}}
	err := readContent(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text, title, display_order FROM library_sections
WHERE is_active ORDER BY display_order, title, id`)
		if err != nil {
			return err
		}
		body.Sections, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (MobileLibrarySection, error) {
			section := MobileLibrarySection{Articles: []MobileLibraryArticle{}}
			err := row.Scan(&section.ID, &section.Title, &section.DisplayOrder)
			return section, err
		})
		if err != nil {
			return err
		}
		bySection := make(map[string]*MobileLibrarySection, len(body.Sections))
		for i := range body.Sections {
			bySection[body.Sections[i].ID] = &body.Sections[i]
		}
		rows, err = tx.Query(ctx, `SELECT id::text, section_id::text, title, cover_image_url, content_markdown, read_time, excerpt,
is_featured, display_order, published_at
FROM library_articles WHERE is_active ORDER BY display_order, title, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a MobileLibraryArticle
			var published *time.Time
			if err := rows.Scan(&a.ID, &a.SectionID, &a.Title, &a.CoverImageURL, &a.ContentMarkdown, &a.ReadTime, &a.Excerpt,
				&a.IsFeatured, &a.DisplayOrder, &published); err != nil {
				return err
			}
			a.PublishedAt = isoMillisPtr(published)
			if a.IsFeatured {
				body.Featured = append(body.Featured, a)
			}
			if section := bySection[a.SectionID]; section != nil {
				section.Articles = append(section.Articles, a)
			}
		}
		return rows.Err()
	})
	return body, err
}

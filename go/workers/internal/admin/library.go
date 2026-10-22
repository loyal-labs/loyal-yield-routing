package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	libraryModule         = "library/actions"
	libraryForeignKeyCode = "23503"
	libraryUploadTimeout  = 60 * time.Second
	libraryUploadMaxBytes = 5 << 20
)

type LibraryPage struct {
	Sections []LibrarySection `json:"sections" nullable:"false"`
	Articles []LibraryArticle `json:"articles" nullable:"false"`
}

type LibrarySection struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	DisplayOrder int    `json:"displayOrder"`
	IsActive     bool   `json:"isActive"`
	CreatedAt    string `json:"createdAt"`
	UpdatedAt    string `json:"updatedAt"`
}

type LibraryArticle struct {
	ID              string  `json:"id"`
	SectionID       string  `json:"sectionId"`
	Title           string  `json:"title"`
	CoverImageURL   string  `json:"coverImageUrl"`
	ContentMarkdown string  `json:"contentMarkdown"`
	ReadTime        string  `json:"readTime"`
	Excerpt         *string `json:"excerpt"`
	IsFeatured      bool    `json:"isFeatured"`
	IsActive        bool    `json:"isActive"`
	DisplayOrder    int     `json:"displayOrder"`
	PublishedAt     *string `json:"publishedAt"`
	CreatedAt       string  `json:"createdAt"`
	UpdatedAt       string  `json:"updatedAt"`
}

type LibraryResult struct {
	Error   string `json:"error,omitempty"`
	Success bool   `json:"success,omitempty"`
}

type LibraryUploadResult struct {
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
}

func libraryISO(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func libraryISOPtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := libraryISO(*t)
	return &s
}

func libraryInt(raw string) int {
	s := strings.TrimSpace(raw)
	end := 0
	if end < len(s) && (s[end] == '+' || s[end] == '-') {
		end++
	}
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, err := strconv.ParseInt(s[:end], 10, 32)
	if err != nil {
		return 0
	}
	return int(n)
}

type libraryArticleFields struct {
	sectionID, title, coverImageURL, contentMarkdown, readTime string
	excerpt                                                    *string
	isFeatured, isActive                                       bool
	displayOrder                                               int
}

func readLibraryArticle(call Call) libraryArticleFields {
	v := libraryArticleFields{
		sectionID:       strings.TrimSpace(call.FormValue("sectionId")),
		title:           strings.TrimSpace(call.FormValue("title")),
		coverImageURL:   strings.TrimSpace(call.FormValue("coverImageUrl")),
		contentMarkdown: strings.TrimSpace(call.FormValue("contentMarkdown")),
		readTime:        strings.TrimSpace(call.FormValue("readTime")),
		isFeatured:      call.FormValue("isFeatured") == "on",
		isActive:        call.FormValue("isActive") == "on",
		displayOrder:    libraryInt(call.FormValue("displayOrder")),
	}
	if excerpt := strings.TrimSpace(call.FormValue("excerpt")); excerpt != "" {
		v.excerpt = &excerpt
	}
	return v
}

func (v libraryArticleFields) invalid() string {
	switch {
	case v.sectionID == "":
		return "Section is required"
	case v.title == "":
		return "Title is required"
	case v.coverImageURL == "":
		return "Cover image is required"
	case v.contentMarkdown == "":
		return "Content is required"
	case v.readTime == "":
		return "Read time is required"
	}
	return ""
}

func loadLibrary(ctx context.Context, d *Deps, _ PageRequest) (LibraryPage, error) {
	page := LibraryPage{Sections: []LibrarySection{}, Articles: []LibraryArticle{}}
	err := d.AppRead(ctx, func(tx pgx.Tx) error {
		sections, err := tx.Query(ctx, `SELECT id::text, title, display_order, is_active, created_at, updated_at
FROM library_sections ORDER BY display_order, title, id`)
		if err != nil {
			return err
		}
		for sections.Next() {
			var s LibrarySection
			var created, updated time.Time
			if err := sections.Scan(&s.ID, &s.Title, &s.DisplayOrder, &s.IsActive, &created, &updated); err != nil {
				return err
			}
			s.CreatedAt, s.UpdatedAt = libraryISO(created), libraryISO(updated)
			page.Sections = append(page.Sections, s)
		}
		if err := sections.Err(); err != nil {
			return err
		}
		articles, err := tx.Query(ctx, `SELECT id::text, section_id::text, title, cover_image_url, content_markdown, read_time, excerpt,
is_featured, is_active, display_order, published_at, created_at, updated_at
FROM library_articles ORDER BY display_order, title, id`)
		if err != nil {
			return err
		}
		for articles.Next() {
			var a LibraryArticle
			var published *time.Time
			var created, updated time.Time
			if err := articles.Scan(&a.ID, &a.SectionID, &a.Title, &a.CoverImageURL, &a.ContentMarkdown, &a.ReadTime, &a.Excerpt,
				&a.IsFeatured, &a.IsActive, &a.DisplayOrder, &published, &created, &updated); err != nil {
				return err
			}
			a.PublishedAt, a.CreatedAt, a.UpdatedAt = libraryISOPtr(published), libraryISO(created), libraryISO(updated)
			page.Articles = append(page.Articles, a)
		}
		return articles.Err()
	})
	return page, err
}

var libraryCoverTypes = map[string]string{"image/png": "png", "image/jpeg": "jpg", "image/webp": "webp"}

func libraryUpload(ctx context.Context, d *Deps, call Call) (*LibraryUploadResult, error) {
	if d.R2 == nil {
		return nil, Disabled("library upload")
	}
	if call.Form == nil || len(call.Form.File["file"]) == 0 {
		return &LibraryUploadResult{Error: "No file provided"}, nil
	}
	header := call.Form.File["file"][0]
	contentType, _, _ := mime.ParseMediaType(header.Header.Get("Content-Type"))
	extension, ok := libraryCoverTypes[contentType]
	if !ok {
		return &LibraryUploadResult{Error: "Unsupported content type; allowed: image/png, image/jpeg, image/webp"}, nil
	}
	if header.Size <= 0 {
		return &LibraryUploadResult{Error: "Uploaded file is empty"}, nil
	}
	if header.Size > libraryUploadMaxBytes {
		return &LibraryUploadResult{Error: "File exceeds 5MB limit"}, nil
	}
	src, err := header.Open()
	if err != nil {
		return nil, err
	}
	defer src.Close()
	body, err := io.ReadAll(io.LimitReader(src, libraryUploadMaxBytes+1))
	if err != nil {
		return nil, err
	}
	id, err := newObjectID()
	if err != nil {
		return nil, err
	}
	key := d.R2.objectKey("library/" + id + "." + extension)

	ctx, cancel := context.WithTimeout(ctx, libraryUploadTimeout)
	defer cancel()
	request, err := d.R2.objectRequest(key, body, contentType)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request.WithContext(ctx))
	if err != nil {
		return &LibraryUploadResult{Error: "Upload request failed"}, nil
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return &LibraryUploadResult{Error: fmt.Sprintf("Upload failed (%d)", response.StatusCode)}, nil
	}
	return &LibraryUploadResult{URL: d.R2.publicURL(key)}, nil
}

func uuidArg(call Call, i int) (pgtype.UUID, error) {
	var raw string
	if err := call.Arg(i, &raw); err != nil {
		return pgtype.UUID{}, err
	}
	return parseUUID(raw)
}

func parseUUID(raw string) (pgtype.UUID, error) {
	var id pgtype.UUID
	if err := id.Scan(raw); err != nil {
		return id, huma.Error400BadRequest("invalid id")
	}
	return id, nil
}

func registerLibrary(m *Mux) {
	Page(m, "/library", loadLibrary)

	done := &LibraryResult{Success: true}

	Action(m, libraryModule, "addLibrarySection", func(ctx context.Context, d *Deps, call Call) (*LibraryResult, error) {
		title := strings.TrimSpace(call.FormValue("title"))
		if title == "" {
			return &LibraryResult{Error: "Title is required"}, nil
		}
		return done, d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO library_sections (title, display_order, is_active) VALUES ($1, $2, $3)`,
				title, libraryInt(call.FormValue("displayOrder")), call.FormValue("isActive") == "on")
			return err
		})
	})

	Action(m, libraryModule, "updateLibrarySection", func(ctx context.Context, d *Deps, call Call) (*LibraryResult, error) {
		id, err := uuidArg(call, 0)
		if err != nil {
			return nil, err
		}
		title := strings.TrimSpace(call.FormValue("title"))
		if title == "" {
			return &LibraryResult{Error: "Title is required"}, nil
		}
		return done, d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE library_sections SET title = $2, display_order = $3, is_active = $4, updated_at = now() WHERE id = $1`,
				id, title, libraryInt(call.FormValue("displayOrder")), call.FormValue("isActive") == "on")
			return err
		})
	})

	Action(m, libraryModule, "deleteLibrarySection", func(ctx context.Context, d *Deps, call Call) (*LibraryResult, error) {
		id, err := uuidArg(call, 0)
		if err != nil {
			return nil, err
		}
		err = d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM library_sections WHERE id = $1`, id)
			return err
		})
		var pgErr *pgconn.PgError
		switch {
		case err == nil:
			return done, nil
		case errors.As(err, &pgErr) && pgErr.Code == libraryForeignKeyCode:
			return &LibraryResult{Error: "Section has articles. Delete or move them first."}, nil
		default:
			return &LibraryResult{Error: "Failed to delete section"}, nil
		}
	})

	Action(m, libraryModule, "addLibraryArticle", func(ctx context.Context, d *Deps, call Call) (*LibraryResult, error) {
		v := readLibraryArticle(call)
		if invalid := v.invalid(); invalid != "" {
			return &LibraryResult{Error: invalid}, nil
		}
		section, err := parseUUID(v.sectionID)
		if err != nil {
			return nil, err
		}
		return done, d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO library_articles (section_id, title, cover_image_url, content_markdown, read_time, excerpt,
is_featured, is_active, display_order, published_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, CASE WHEN $8 THEN now() END)`,
				section, v.title, v.coverImageURL, v.contentMarkdown, v.readTime, v.excerpt, v.isFeatured, v.isActive, v.displayOrder)
			return err
		})
	})

	Action(m, libraryModule, "updateLibraryArticle", func(ctx context.Context, d *Deps, call Call) (*LibraryResult, error) {
		id, err := uuidArg(call, 0)
		if err != nil {
			return nil, err
		}
		v := readLibraryArticle(call)
		if invalid := v.invalid(); invalid != "" {
			return &LibraryResult{Error: invalid}, nil
		}
		section, err := parseUUID(v.sectionID)
		if err != nil {
			return nil, err
		}
		return done, d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE library_articles SET section_id = $2, title = $3, cover_image_url = $4, content_markdown = $5,
read_time = $6, excerpt = $7, is_featured = $8, is_active = $9, display_order = $10,
published_at = CASE WHEN $9 AND published_at IS NULL THEN now() ELSE published_at END, updated_at = now()
WHERE id = $1`,
				id, section, v.title, v.coverImageURL, v.contentMarkdown, v.readTime, v.excerpt, v.isFeatured, v.isActive, v.displayOrder)
			return err
		})
	})

	Action(m, libraryModule, "deleteLibraryArticle", func(ctx context.Context, d *Deps, call Call) (*LibraryResult, error) {
		id, err := uuidArg(call, 0)
		if err != nil {
			return nil, err
		}
		return done, d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM library_articles WHERE id = $1`, id)
			return err
		})
	})

	Action(m, libraryModule, "toggleLibraryArticleActive", func(ctx context.Context, d *Deps, call Call) (*LibraryResult, error) {
		id, err := uuidArg(call, 0)
		if err != nil {
			return nil, err
		}
		var active bool
		if err := call.Arg(1, &active); err != nil {
			return nil, err
		}
		return done, d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE library_articles SET is_active = $2,
published_at = CASE WHEN $2 AND published_at IS NULL THEN now() ELSE published_at END, updated_at = now()
WHERE id = $1`, id, active)
			return err
		})
	})

	Action(m, libraryModule, "uploadLibraryCover", libraryUpload)
}

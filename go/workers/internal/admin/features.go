package admin

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	featuresModule      = "features/actions"
	featureDetailModule = "features/[featureId]/feature-detail-actions"
)

var (
	featuresApps         = []string{"telegram_miniapp", "website", "mobile", "extension"}
	featuresStatuses     = []string{"missing", "planned", "in_progress", "implemented", "live"}
	featuresEvidenceType = []string{"path", "branch", "pr", "linear", "commit", "doc"}
)

type FeaturesPage struct {
	Features []FeatureRow `json:"features" nullable:"false"`
}

type FeatureDetailPage struct {
	Feature        FeatureRow       `json:"feature"`
	AvailableFlags []FeatureFlagRow `json:"availableFlags" nullable:"false"`
}

type FeatureRow struct {
	ID          string                `json:"id"`
	Key         string                `json:"key"`
	Title       string                `json:"title"`
	Description string                `json:"description"`
	Owner       *string               `json:"owner"`
	Notes       *string               `json:"notes"`
	CreatedAt   time.Time             `json:"createdAt"`
	UpdatedAt   time.Time             `json:"updatedAt"`
	AppStatuses []FeatureAppStatusRow `json:"appStatuses" nullable:"false"`
	FlagLinks   []FeatureFlagLinkRow  `json:"flagLinks" nullable:"false"`
}

type FeatureAppStatusRow struct {
	ID         string               `json:"id"`
	FeatureID  string               `json:"featureId"`
	App        string               `json:"app" enum:"telegram_miniapp,website,mobile,extension"`
	Status     string               `json:"status" enum:"missing,planned,in_progress,implemented,live"`
	StatusNote *string              `json:"statusNote"`
	CreatedAt  time.Time            `json:"createdAt"`
	UpdatedAt  time.Time            `json:"updatedAt"`
	Evidence   []FeatureEvidenceRow `json:"evidence" nullable:"false"`
}

type FeatureEvidenceRow struct {
	ID                 string    `json:"id"`
	FeatureAppStatusID string    `json:"featureAppStatusId"`
	Type               string    `json:"type" enum:"path,branch,pr,linear,commit,doc"`
	Label              string    `json:"label"`
	Value              string    `json:"value"`
	Note               *string   `json:"note"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

type FeatureFlagLinkRow struct {
	ID        string         `json:"id"`
	FeatureID string         `json:"featureId"`
	FlagID    string         `json:"flagId"`
	CreatedAt time.Time      `json:"createdAt"`
	Flag      FeatureFlagRow `json:"flag"`
}

type FeatureFlagRow struct {
	ID                 string    `json:"id"`
	Key                string    `json:"key"`
	Description        string    `json:"description"`
	Enabled            bool      `json:"enabled"`
	Audience           string    `json:"audience" enum:"all,public,team"`
	TargetEnvironments []string  `json:"targetEnvironments" enum:"development,preview,production" nullable:"false"`
	Notes              *string   `json:"notes"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

const featuresFlagColumns = `f.id::text, f.key, f.description, f.enabled, f.audience, f.target_environments, f.notes,
f.created_at AT TIME ZONE 'UTC', f.updated_at AT TIME ZONE 'UTC'`

func featuresText(call Call, key string) string {
	return strings.TrimSpace(call.FormValue(key))
}

func featuresOptional(call Call, key string) *string {
	if v := featuresText(call, key); v != "" {
		return &v
	}
	return nil
}

func featuresLoad(ctx context.Context, tx pgx.Tx, only pgtype.UUID) ([]FeatureRow, error) {
	features := []FeatureRow{}
	index := map[string]int{}
	rows, err := tx.Query(ctx, `SELECT id::text, key, title, description, owner, notes,
created_at AT TIME ZONE 'UTC', updated_at AT TIME ZONE 'UTC'
FROM feature_registry WHERE $1::uuid IS NULL OR id = $1 ORDER BY title, id`, only)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var f FeatureRow
		if err := rows.Scan(&f.ID, &f.Key, &f.Title, &f.Description, &f.Owner, &f.Notes, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		f.AppStatuses, f.FlagLinks = []FeatureAppStatusRow{}, []FeatureFlagLinkRow{}
		index[f.ID] = len(features)
		features = append(features, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	statusAt := map[string][2]int{}
	rows, err = tx.Query(ctx, `SELECT id::text, feature_id::text, app, status, status_note,
created_at AT TIME ZONE 'UTC', updated_at AT TIME ZONE 'UTC'
FROM feature_app_statuses WHERE $1::uuid IS NULL OR feature_id = $1 ORDER BY feature_id, app`, only)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var s FeatureAppStatusRow
		if err := rows.Scan(&s.ID, &s.FeatureID, &s.App, &s.Status, &s.StatusNote, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, err
		}
		s.Evidence = []FeatureEvidenceRow{}
		if i, ok := index[s.FeatureID]; ok {
			statusAt[s.ID] = [2]int{i, len(features[i].AppStatuses)}
			features[i].AppStatuses = append(features[i].AppStatuses, s)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = tx.Query(ctx, `SELECT e.id::text, e.feature_app_status_id::text, e.type, e.label, e.value, e.note,
e.created_at AT TIME ZONE 'UTC', e.updated_at AT TIME ZONE 'UTC'
FROM feature_evidence e JOIN feature_app_statuses s ON s.id = e.feature_app_status_id
WHERE $1::uuid IS NULL OR s.feature_id = $1 ORDER BY e.created_at DESC, e.id`, only)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e FeatureEvidenceRow
		if err := rows.Scan(&e.ID, &e.FeatureAppStatusID, &e.Type, &e.Label, &e.Value, &e.Note, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		if at, ok := statusAt[e.FeatureAppStatusID]; ok {
			status := &features[at[0]].AppStatuses[at[1]]
			status.Evidence = append(status.Evidence, e)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = tx.Query(ctx, `SELECT l.id::text, l.feature_id::text, l.flag_id::text, l.created_at AT TIME ZONE 'UTC', `+featuresFlagColumns+`
FROM feature_flag_links l JOIN runtime_flags f ON f.id = l.flag_id
WHERE $1::uuid IS NULL OR l.feature_id = $1 ORDER BY l.created_at, l.id`, only)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l FeatureFlagLinkRow
		f := &l.Flag
		if err := rows.Scan(&l.ID, &l.FeatureID, &l.FlagID, &l.CreatedAt,
			&f.ID, &f.Key, &f.Description, &f.Enabled, &f.Audience, &f.TargetEnvironments, &f.Notes, &f.CreatedAt, &f.UpdatedAt); err != nil {
			return nil, err
		}
		if i, ok := index[l.FeatureID]; ok {
			features[i].FlagLinks = append(features[i].FlagLinks, l)
		}
	}
	return features, rows.Err()
}

func loadFeatures(ctx context.Context, d *Deps, _ PageRequest) (FeaturesPage, error) {
	page := FeaturesPage{}
	err := d.AppRead(ctx, func(tx pgx.Tx) (err error) {
		page.Features, err = featuresLoad(ctx, tx, pgtype.UUID{})
		return err
	})
	return page, err
}

func loadFeatureDetail(ctx context.Context, d *Deps, req PageRequest) (FeatureDetailPage, error) {
	id, err := parseUUID(req.Params["featureId"])
	if err != nil {
		return FeatureDetailPage{}, huma.Error404NotFound("feature not found")
	}
	page := FeatureDetailPage{}
	err = d.AppRead(ctx, func(tx pgx.Tx) error {
		features, err := featuresLoad(ctx, tx, id)
		if err != nil {
			return err
		}
		if len(features) == 0 {
			return huma.Error404NotFound("feature not found")
		}
		page.Feature = features[0]
		rows, err := tx.Query(ctx, `SELECT id::text, key, description, enabled, audience, target_environments, notes,
created_at AT TIME ZONE 'UTC', updated_at AT TIME ZONE 'UTC'
FROM runtime_flags ORDER BY key`)
		if err != nil {
			return err
		}
		page.AvailableFlags, err = pgx.CollectRows(rows, pgx.RowToStructByPos[FeatureFlagRow])
		return err
	})
	return page, err
}

func registerFeatures(m *Mux) {
	Page(m, "/features", loadFeatures)
	Page(m, "/features/{featureId}", loadFeatureDetail)

	Action(m, featuresModule, "getFeaturesForMatrix", func(ctx context.Context, d *Deps, _ Call) ([]FeatureRow, error) {
		page, err := loadFeatures(ctx, d, PageRequest{})
		return page.Features, err
	})

	Action(m, featuresModule, "createFeature", func(ctx context.Context, d *Deps, call Call) (*struct{}, error) {
		title, key, description := featuresText(call, "title"), featuresText(call, "key"), featuresText(call, "description")
		if title == "" || key == "" || description == "" {
			return nil, nil
		}
		return nil, d.AppWrite(ctx, func(tx pgx.Tx) error {
			var id string
			err := tx.QueryRow(ctx, `INSERT INTO feature_registry (key, title, description, owner, notes)
VALUES ($1, $2, $3, $4, $5) ON CONFLICT (key) DO NOTHING RETURNING id::text`,
				key, title, description, featuresOptional(call, "owner"), featuresOptional(call, "notes")).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO feature_app_statuses (feature_id, app, status)
SELECT $1::uuid, app, 'missing' FROM unnest($2::text[]) AS app`, id, featuresApps)
			return err
		})
	})

	Action(m, featureDetailModule, "updateFeatureStatus", func(ctx context.Context, d *Deps, call Call) (*struct{}, error) {
		statusID, err := uuidArg(call, 0)
		if err != nil {
			return nil, err
		}
		status := featuresText(call, "status")
		if featuresText(call, "featureId") == "" || !slices.Contains(featuresStatuses, status) {
			return nil, nil
		}
		return nil, d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE feature_app_statuses SET status = $2, status_note = $3, updated_at = now() WHERE id = $1`,
				statusID, status, featuresOptional(call, "statusNote"))
			return err
		})
	})

	Action(m, featureDetailModule, "addFeatureEvidence", func(ctx context.Context, d *Deps, call Call) (*struct{}, error) {
		evidenceType, label, value := featuresText(call, "type"), featuresText(call, "label"), featuresText(call, "value")
		statusRaw := featuresText(call, "featureAppStatusId")
		if featuresText(call, "featureId") == "" || statusRaw == "" || !slices.Contains(featuresEvidenceType, evidenceType) || label == "" || value == "" {
			return nil, nil
		}
		statusID, err := parseUUID(statusRaw)
		if err != nil {
			return nil, err
		}
		return nil, d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO feature_evidence (feature_app_status_id, type, label, value, note) VALUES ($1, $2, $3, $4, $5)`,
				statusID, evidenceType, label, value, featuresOptional(call, "note"))
			return err
		})
	})

	Action(m, featureDetailModule, "linkFeatureFlag", func(ctx context.Context, d *Deps, call Call) (*struct{}, error) {
		featureRaw, flagRaw := featuresText(call, "featureId"), featuresText(call, "flagId")
		if featureRaw == "" || flagRaw == "" {
			return nil, nil
		}
		feature, err := parseUUID(featureRaw)
		if err != nil {
			return nil, err
		}
		flag, err := parseUUID(flagRaw)
		if err != nil {
			return nil, err
		}
		return nil, d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO feature_flag_links (feature_id, flag_id) VALUES ($1, $2)
ON CONFLICT (feature_id, flag_id) DO NOTHING`, feature, flag)
			return err
		})
	})

	Action(m, featureDetailModule, "unlinkFeatureFlag", func(ctx context.Context, d *Deps, call Call) (*struct{}, error) {
		linkID, err := uuidArg(call, 0)
		if err != nil {
			return nil, err
		}
		return nil, d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM feature_flag_links WHERE id = $1`, linkID)
			return err
		})
	})
}

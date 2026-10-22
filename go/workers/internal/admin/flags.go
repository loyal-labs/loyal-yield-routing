package admin

import (
	"context"
	"slices"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const flagsModule = "flags/actions"

var (
	flagAudiences    = []string{"all", "public", "team"}
	flagEnvironments = []string{"development", "preview", "production"}
)

type FlagsPage struct {
	Flags             []FlagRow       `json:"flags" nullable:"false"`
	AvailableFeatures []FeatureOption `json:"availableFeatures" nullable:"false"`
}

type FlagRow struct {
	ID                 string          `json:"id"`
	Key                string          `json:"key"`
	Description        string          `json:"description"`
	Enabled            bool            `json:"enabled"`
	Audience           string          `json:"audience" enum:"all,public,team"`
	TargetEnvironments []string        `json:"targetEnvironments" enum:"development,preview,production" nullable:"false"`
	Notes              *string         `json:"notes"`
	LinkedFeatures     []LinkedFeature `json:"linkedFeatures" nullable:"false"`
}

type LinkedFeature struct {
	LinkID string `json:"linkId"`
	ID     string `json:"id"`
	Title  string `json:"title"`
	Key    string `json:"key"`
}

type FeatureOption struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Key   string `json:"key"`
}

type flagValues struct {
	key, description, audience string
	environments               []string
	notes                      *string
	enabled                    bool
}

func readFlagValues(call Call) (flagValues, bool) {
	v := flagValues{
		key:         strings.TrimSpace(call.FormValue("key")),
		description: strings.TrimSpace(call.FormValue("description")),
		audience:    strings.TrimSpace(call.FormValue("audience")),
		enabled:     call.FormValue("enabled") == "on",
	}
	if notes := strings.TrimSpace(call.FormValue("notes")); notes != "" {
		v.notes = &notes
	}
	if call.Form != nil {
		for _, raw := range call.Form.Value["targetEnvironments"] {
			env := strings.TrimSpace(raw)
			if slices.Contains(flagEnvironments, env) && !slices.Contains(v.environments, env) {
				v.environments = append(v.environments, env)
			}
		}
	}
	if len(v.environments) == 0 {
		v.environments = flagEnvironments
	}
	valid := v.key != "" && v.description != "" && slices.Contains(flagAudiences, v.audience)
	return v, valid
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

func loadFlags(ctx context.Context, d *Deps, _ PageRequest) (FlagsPage, error) {
	page := FlagsPage{Flags: []FlagRow{}, AvailableFeatures: []FeatureOption{}}
	err := d.AppRead(ctx, func(tx pgx.Tx) error {
		features, err := tx.Query(ctx, `SELECT id::text, title, key FROM feature_registry ORDER BY title`)
		if err != nil {
			return err
		}
		if page.AvailableFeatures, err = pgx.CollectRows(features, pgx.RowToStructByPos[FeatureOption]); err != nil {
			return err
		}
		flags, err := tx.Query(ctx, `SELECT id::text, key, description, enabled, audience, target_environments, notes
FROM runtime_flags ORDER BY key`)
		if err != nil {
			return err
		}
		byID := map[string]int{}
		for flags.Next() {
			var f FlagRow
			if err := flags.Scan(&f.ID, &f.Key, &f.Description, &f.Enabled, &f.Audience, &f.TargetEnvironments, &f.Notes); err != nil {
				return err
			}
			f.LinkedFeatures = []LinkedFeature{}
			byID[f.ID] = len(page.Flags)
			page.Flags = append(page.Flags, f)
		}
		if err := flags.Err(); err != nil {
			return err
		}
		links, err := tx.Query(ctx, `SELECT l.flag_id::text, l.id::text, f.id::text, f.title, f.key
FROM feature_flag_links l JOIN feature_registry f ON f.id = l.feature_id ORDER BY l.created_at, l.id`)
		if err != nil {
			return err
		}
		for links.Next() {
			var flagID string
			var link LinkedFeature
			if err := links.Scan(&flagID, &link.LinkID, &link.ID, &link.Title, &link.Key); err != nil {
				return err
			}
			if i, ok := byID[flagID]; ok {
				page.Flags[i].LinkedFeatures = append(page.Flags[i].LinkedFeatures, link)
			}
		}
		return links.Err()
	})
	return page, err
}

func registerFlags(m *Mux) {
	Page(m, "/flags", loadFlags)

	Action(m, flagsModule, "createRuntimeFlag", func(ctx context.Context, d *Deps, call Call) (*struct{}, error) {
		v, valid := readFlagValues(call)
		if !valid {
			return nil, nil
		}
		return nil, d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO runtime_flags (key, description, enabled, audience, target_environments, notes)
VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (key) DO NOTHING`,
				v.key, v.description, v.enabled, v.audience, v.environments, v.notes)
			return err
		})
	})

	Action(m, flagsModule, "updateRuntimeFlag", func(ctx context.Context, d *Deps, call Call) (*struct{}, error) {
		id, err := uuidArg(call, 0)
		if err != nil {
			return nil, err
		}
		v, valid := readFlagValues(call)
		if !valid {
			return nil, nil
		}
		return nil, d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE runtime_flags
SET key = $2, description = $3, enabled = $4, audience = $5, target_environments = $6, notes = $7, updated_at = now()
WHERE id = $1 AND NOT EXISTS (SELECT 1 FROM runtime_flags other WHERE other.key = $2 AND other.id <> $1)`,
				id, v.key, v.description, v.enabled, v.audience, v.environments, v.notes)
			return err
		})
	})

	Action(m, flagsModule, "toggleRuntimeFlag", func(ctx context.Context, d *Deps, call Call) (*struct{}, error) {
		id, err := uuidArg(call, 0)
		if err != nil {
			return nil, err
		}
		var enabled bool
		if err := call.Arg(1, &enabled); err != nil {
			return nil, err
		}
		return nil, d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE runtime_flags SET enabled = $2, updated_at = now() WHERE id = $1`, id, enabled)
			return err
		})
	})

	Action(m, flagsModule, "deleteRuntimeFlag", func(ctx context.Context, d *Deps, call Call) (*struct{}, error) {
		id, err := uuidArg(call, 0)
		if err != nil {
			return nil, err
		}
		return nil, d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM runtime_flags WHERE id = $1`, id)
			return err
		})
	})

	Action(m, flagsModule, "linkFlagToFeature", func(ctx context.Context, d *Deps, call Call) (*struct{}, error) {
		flagID, featureID := strings.TrimSpace(call.FormValue("flagId")), strings.TrimSpace(call.FormValue("featureId"))
		if flagID == "" || featureID == "" {
			return nil, nil
		}
		flag, err := parseUUID(flagID)
		if err != nil {
			return nil, err
		}
		feature, err := parseUUID(featureID)
		if err != nil {
			return nil, err
		}
		return nil, d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO feature_flag_links (flag_id, feature_id) VALUES ($1, $2)
ON CONFLICT (feature_id, flag_id) DO NOTHING`, flag, feature)
			return err
		})
	})

	Action(m, flagsModule, "unlinkFlagFromFeature", func(ctx context.Context, d *Deps, call Call) (*struct{}, error) {
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

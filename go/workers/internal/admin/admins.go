package admin

import (
	"context"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

const adminsModule = "admins/actions"

var adminsDigits = regexp.MustCompile(`^\d+$`)

type AdminsPage struct {
	Admins []AdminRow `json:"admins" nullable:"false"`
}

type AdminRow struct {
	ID          string    `json:"id"`
	TelegramID  string    `json:"telegramId"`
	Username    *string   `json:"username"`
	DisplayName string    `json:"displayName"`
	AddedAt     time.Time `json:"addedAt"`
	AddedBy     *string   `json:"addedBy"`
	Notes       *string   `json:"notes"`
}

type AdminActionResult struct {
	Error   string `json:"error,omitempty"`
	Success bool   `json:"success,omitempty"`
}

type adminsFields struct {
	telegramID               int64
	displayName              string
	username, addedBy, notes *string
	problem                  string
	overflow                 bool
}

func adminsOptional(call Call, key string) *string {
	if v := call.FormValue(key); v != "" {
		return &v
	}
	return nil
}

func adminsReadForm(call Call) adminsFields {
	telegramID := call.FormValue("telegramId")
	f := adminsFields{
		displayName: call.FormValue("displayName"),
		username:    adminsOptional(call, "username"),
		addedBy:     adminsOptional(call, "addedBy"),
		notes:       adminsOptional(call, "notes"),
	}
	switch {
	case telegramID == "" || f.displayName == "":
		f.problem = "Telegram ID and Display Name are required"
	case !adminsDigits.MatchString(telegramID):
		f.problem = "Telegram ID must be a valid integer"
	default:
		n, err := strconv.ParseInt(telegramID, 10, 64)
		f.telegramID, f.overflow = n, err != nil
	}
	return f
}

func adminsWriteResult(err error, failure string) AdminActionResult {
	switch {
	case err == nil:
		return AdminActionResult{Success: true}
	case appCrudIsUnique(err):
		return AdminActionResult{Error: "An admin with this Telegram ID already exists"}
	default:
		return AdminActionResult{Error: failure}
	}
}

func loadAdmins(ctx context.Context, d *Deps, _ PageRequest) (AdminsPage, error) {
	page := AdminsPage{}
	err := d.AppRead(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text, telegram_id::text, username, display_name, added_at AT TIME ZONE 'UTC', added_by, notes
FROM admins ORDER BY added_at DESC, id`)
		if err != nil {
			return err
		}
		page.Admins, err = pgx.CollectRows(rows, pgx.RowToStructByPos[AdminRow])
		return err
	})
	return page, err
}

func registerAdmins(m *Mux) {
	Page(m, "/admins", loadAdmins)

	Action(m, adminsModule, "addAdmin", func(ctx context.Context, d *Deps, call Call) (AdminActionResult, error) {
		f := adminsReadForm(call)
		if f.problem != "" {
			return AdminActionResult{Error: f.problem}, nil
		}
		if f.overflow {
			return AdminActionResult{Error: "Failed to add admin"}, nil
		}
		err := d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO admins (telegram_id, display_name, username, added_by, notes)
VALUES ($1, $2, $3, $4, $5)`, f.telegramID, f.displayName, f.username, f.addedBy, f.notes)
			return err
		})
		return adminsWriteResult(err, "Failed to add admin"), nil
	})

	Action(m, adminsModule, "updateAdmin", func(ctx context.Context, d *Deps, call Call) (AdminActionResult, error) {
		id, err := uuidArg(call, 0)
		if err != nil {
			return AdminActionResult{}, err
		}
		f := adminsReadForm(call)
		if f.problem != "" {
			return AdminActionResult{Error: f.problem}, nil
		}
		if f.overflow {
			return AdminActionResult{Error: "Failed to update admin"}, nil
		}
		err = d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE admins SET telegram_id = $2, display_name = $3, username = $4, added_by = $5, notes = $6
WHERE id = $1`, id, f.telegramID, f.displayName, f.username, f.addedBy, f.notes)
			return err
		})
		return adminsWriteResult(err, "Failed to update admin"), nil
	})

	Action(m, adminsModule, "deleteAdmin", func(ctx context.Context, d *Deps, call Call) (AdminActionResult, error) {
		id, err := uuidArg(call, 0)
		if err != nil {
			return AdminActionResult{}, err
		}
		err = d.AppWrite(ctx, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM admins WHERE id = $1`, id)
			return err
		})
		return AdminActionResult{Success: true}, err
	})
}

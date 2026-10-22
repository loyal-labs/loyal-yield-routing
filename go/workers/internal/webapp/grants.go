package webapp

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Grant struct{ Table, Privilege string }

func (g Grant) String() string { return g.Privilege + " on " + g.Table }

var ContentGrants = []Grant{
	{"summaries", "SELECT"},
	{"communities", "SELECT"},
	{"trusted_dapps", "SELECT"},
	{"library_sections", "SELECT"},
	{"library_articles", "SELECT"},
}

var StatsGrants = []Grant{
	{"public.loyal_stats_snapshots", "SELECT"},
}

var EarnMaxGrants = []Grant{
	{"earn_max_invite_redemptions", "SELECT"},
	{"earn_max_invite_redemptions", "INSERT"},
}

var PushGrants = []Grant{
	{"push_tokens", "SELECT"},
	{"push_tokens", "INSERT"},
	{"push_tokens", "UPDATE"},
}

var YieldGrants = []Grant{
	{"loyal_yield.multiply_route_states", "SELECT"},
}

// MissingGrants lists the grants the connected role lacks; a table that does not exist counts as missing.
func MissingGrants(ctx context.Context, pool *pgxpool.Pool, want []Grant) ([]Grant, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	batch := &pgx.Batch{}
	for _, g := range want {
		batch.Queue("SELECT COALESCE(has_table_privilege(current_user, to_regclass($1), $2), false)", g.Table, g.Privilege)
	}
	results := pool.SendBatch(ctx, batch)
	defer results.Close()
	var missing []Grant
	for _, g := range want {
		var granted bool
		if err := results.QueryRow().Scan(&granted); err != nil {
			return nil, fmt.Errorf("check %s: %w", g, err)
		}
		if !granted {
			missing = append(missing, g)
		}
	}
	return missing, nil
}

package webapp

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type relationKind int

const (
	tableRelation relationKind = iota
	sequenceRelation
)

var privilegeChecks = [...]string{
	tableRelation:    "SELECT COALESCE(has_table_privilege(current_user, to_regclass($1), $2), false)",
	sequenceRelation: "SELECT COALESCE(has_sequence_privilege(current_user, to_regclass($1), $2), false)",
}

type Grant struct {
	kind                relationKind
	relation, privilege string
}

func TableGrant(table, privilege string) Grant {
	return Grant{kind: tableRelation, relation: table, privilege: privilege}
}

func SequenceGrant(sequence, privilege string) Grant {
	return Grant{kind: sequenceRelation, relation: sequence, privilege: privilege}
}

func (g Grant) String() string { return g.privilege + " on " + g.relation }

var ContentGrants = []Grant{
	TableGrant("summaries", "SELECT"),
	TableGrant("communities", "SELECT"),
	TableGrant("trusted_dapps", "SELECT"),
	TableGrant("library_sections", "SELECT"),
	TableGrant("library_articles", "SELECT"),
}

var StatsGrants = []Grant{
	TableGrant("public.loyal_stats_snapshots", "SELECT"),
}

var EarnMaxGrants = []Grant{
	TableGrant("earn_max_invite_redemptions", "SELECT"),
	TableGrant("earn_max_invite_redemptions", "INSERT"),
}

var PushGrants = []Grant{
	TableGrant("push_tokens", "SELECT"),
	TableGrant("push_tokens", "INSERT"),
	TableGrant("push_tokens", "UPDATE"),
}

var YieldGrants = []Grant{
	TableGrant("loyal_yield.multiply_route_states", "SELECT"),
}

func MissingGrants(ctx context.Context, pool *pgxpool.Pool, want []Grant) ([]Grant, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	batch := &pgx.Batch{}
	for _, g := range want {
		batch.Queue(privilegeChecks[g.kind], g.relation, g.privilege)
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

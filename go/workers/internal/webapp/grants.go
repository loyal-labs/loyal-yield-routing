package webapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type relationKind int

const (
	tableRelation relationKind = iota
	sequenceRelation
)

var ErrRelationUnresolved = errors.New("relation does not resolve")

var privilegeChecks = [...]string{
	tableRelation:    "SELECT to_regclass($1) IS NOT NULL, has_table_privilege(current_user, to_regclass($1), $2)",
	sequenceRelation: "SELECT to_regclass($1) IS NOT NULL, has_sequence_privilege(current_user, to_regclass($1), $2)",
}

func publicUnlessQualified(relation string) string {
	if strings.Contains(relation, ".") {
		return relation
	}
	return "public." + relation
}

type Grant struct {
	kind                relationKind
	relation, privilege string
}

func TableGrant(table, privilege string) Grant {
	return Grant{kind: tableRelation, relation: publicUnlessQualified(table), privilege: privilege}
}

func SequenceGrant(sequence, privilege string) Grant {
	return Grant{kind: sequenceRelation, relation: publicUnlessQualified(sequence), privilege: privilege}
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

var PushSendGrants = []Grant{
	TableGrant("push_tokens", "SELECT"),
	TableGrant("push_tokens", "DELETE"),
	TableGrant("push_notification_sends", "SELECT"),
	TableGrant("push_notification_sends", "INSERT"),
	TableGrant("push_notification_sends", "UPDATE"),
	TableGrant("push_notification_tickets", "INSERT"),
}

var YieldGrants = []Grant{
	TableGrant("loyal_yield.multiply_operations", "SELECT"),
	TableGrant("loyal_yield.multiply_route_states", "SELECT"),
	TableGrant("loyal_yield.multiply_route_current_apy_days", "SELECT"),
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
		var resolved bool
		var granted *bool
		if err := results.QueryRow().Scan(&resolved, &granted); err != nil {
			return nil, fmt.Errorf("check %s: %w", g, err)
		}
		if !resolved {
			return nil, fmt.Errorf("check %s: %w", g, ErrRelationUnresolved)
		}
		if !*granted {
			missing = append(missing, g)
		}
	}
	return missing, nil
}

var grantRetryDelay = 250 * time.Millisecond

const grantRetryCap = 2 * time.Second

func GrantsHeld(ctx context.Context, pool *pgxpool.Pool, family string, want []Grant) bool {
	var failure error
	for delay := grantRetryDelay; ; delay = min(2*delay, grantRetryCap) {
		missing, err := MissingGrants(ctx, pool, want)
		if err == nil {
			if len(missing) > 0 {
				slog.Warn(family+" not served by Go: role lacks grants", "missing", missing)
				return false
			}
			return true
		}
		if ctx.Err() == nil {
			failure = err
		}
		select {
		case <-ctx.Done():
			slog.Error(family+" not served by Go: grant check failed", "error", failure)
			return false
		case <-time.After(delay):
		}
	}
}

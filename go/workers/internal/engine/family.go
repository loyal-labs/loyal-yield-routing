package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type Family string

const (
	FamilyObserver    Family = "observer"
	FamilyAutodeposit Family = "autodeposit"
	FamilyFleet       Family = "fleet"
	FamilyMultiply    Family = "multiply"
	FamilyLookup      Family = "lookup"
	FamilyBackyard    Family = "backyard"
)

var ErrFamilyHeld = errors.New("family is held by another process")

// HoldFamily takes the family's session advisory lock on a dedicated
// connection and keeps it until ctx ends or the connection drops. The DSN must
// be direct, not a transaction pooler: a pooler hands the session to strangers.
// done is closed when the hold is lost; the caller must stop writing then.
func HoldFamily(ctx context.Context, directDSN string, family Family) (done <-chan struct{}, err error) {
	conn, err := pgx.Connect(ctx, directDSN)
	if err != nil {
		return nil, err
	}
	var held bool
	if err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended('loyal-family:'||$1, 0))`, string(family)).Scan(&held); err != nil {
		_ = conn.Close(context.Background())
		return nil, err
	}
	if !held {
		_ = conn.Close(context.Background())
		return nil, fmt.Errorf("%s: %w", family, ErrFamilyHeld)
	}
	lost := make(chan struct{})
	go func() {
		defer close(lost)
		defer conn.Close(context.Background())
		_ = conn.PgConn().WaitForNotification(ctx)
	}()
	return lost, nil
}

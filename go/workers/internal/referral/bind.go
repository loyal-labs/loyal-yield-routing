package referral

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type Source string

const (
	Link            Source = "link"
	Typed           Source = "typed"
	InstallReferrer Source = "install_referrer"
)

var ErrUnknownSource = errors.New("referral: source is link, typed or install_referrer")

func ParseSource(raw string) (Source, error) {
	switch source := Source(raw); source {
	case Link, Typed, InstallReferrer:
		return source, nil
	}
	return "", ErrUnknownSource
}

type Result string

const (
	Bound         Result = "bound"
	UnknownCode   Result = "unknown_code"
	OwnCode       Result = "own_code"
	AlreadyBound  Result = "already_bound"
	ReferrerNewer Result = "referrer_newer"
	WindowClosed  Result = "window_closed"
)

func (s *Store) Bind(ctx context.Context, caller User, code Code, source Source) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	var referrer string
	err := s.yield.QueryRow(ctx, `SELECT app_user_id::text FROM loyal_yield.referral_codes WHERE code = $1`, string(code)).Scan(&referrer)
	if errors.Is(err, pgx.ErrNoRows) {
		return UnknownCode, nil
	}
	if err != nil {
		return "", err
	}
	if referrer == caller.ID {
		return OwnCode, nil
	}
	if result, found, err := s.existing(ctx, caller, referrer); found || err != nil {
		return result, err
	}
	var referrerSignedUpAt time.Time
	err = s.app.QueryRow(ctx, `SELECT created_at FROM app_users WHERE id = $1`, referrer).Scan(&referrerSignedUpAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return UnknownCode, nil
	}
	if err != nil {
		return "", err
	}
	if referrerSignedUpAt.After(caller.SignedUpAt) {
		return ReferrerNewer, nil
	}
	if time.Now().After(caller.ClaimClosesAt()) {
		return WindowClosed, nil
	}
	tag, err := s.yield.Exec(ctx, `INSERT INTO loyal_yield.referral_bindings (referee_app_user_id, referee_wallet, referrer_app_user_id, source, referee_signed_up_at)
VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`, caller.ID, caller.Wallet, referrer, string(source), caller.SignedUpAt)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 1 {
		return Bound, nil
	}
	result, found, err := s.existing(ctx, caller, referrer)
	if err == nil && !found {
		err = fmt.Errorf("referral: binding for %s conflicted but is not visible", caller.ID)
	}
	return result, err
}

func (s *Store) existing(ctx context.Context, caller User, referrer string) (Result, bool, error) {
	var bound string
	err := s.yield.QueryRow(ctx, `SELECT referrer_app_user_id::text FROM loyal_yield.referral_bindings WHERE referee_app_user_id = $1 OR referee_wallet = $2 LIMIT 1`,
		caller.ID, caller.Wallet).Scan(&bound)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, err
	case bound == referrer:
		return Bound, true, nil
	}
	return AlreadyBound, true, nil
}

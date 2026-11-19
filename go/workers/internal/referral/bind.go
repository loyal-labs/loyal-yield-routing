package referral

import (
	"context"
	"errors"
	"slices"
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
	var referrerWallet string
	err = s.app.QueryRow(ctx, `SELECT subject_address FROM app_users WHERE id = $1 AND provider = 'solana'`, referrer).Scan(&referrerWallet)
	if errors.Is(err, pgx.ErrNoRows) {
		return UnknownCode, nil
	}
	if err != nil {
		return "", err
	}
	for _, wallet := range []string{caller.Wallet, referrerWallet} {
		if err := s.observe(ctx, wallet); err != nil {
			return "", err
		}
	}
	claimant, err := knownIdentity(ctx, s.yield, caller.Wallet)
	if err != nil {
		return "", err
	}
	inviter, err := knownIdentity(ctx, s.yield, referrerWallet)
	if err != nil {
		return "", err
	}
	if slices.Contains(claimant.Wallets, referrerWallet) {
		return OwnCode, nil
	}
	firstSignup, _, err := s.members(ctx, claimant)
	if err != nil {
		return "", err
	}
	inviterSignup, inviterUsers, err := s.members(ctx, inviter)
	if err != nil {
		return "", err
	}

	tx, err := s.yield.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if err := lockIdentities(ctx, tx, claimant, inviter); err != nil {
		return "", err
	}
	for _, old := range []struct {
		wallet   string
		identity Identity
	}{{caller.Wallet, claimant}, {referrerWallet, inviter}} {
		current, err := knownIdentity(ctx, tx, old.wallet)
		if err != nil {
			return "", err
		}
		if old.identity.Subject != current.Subject || !slices.Equal(old.identity.Wallets, current.Wallets) {
			return "", errors.New("referral: verified wallet links changed during claim; retry")
		}
	}

	var count int
	var sameInviter bool
	err = tx.QueryRow(ctx, `SELECT count(*), COALESCE(bool_and(referrer_app_user_id::text = ANY($4::text[])), false)
FROM loyal_yield.referral_bindings WHERE referee_app_user_id = $1 OR referee_wallet = ANY($2::text[])
OR referee_privy_subject = NULLIF($3,'')`, caller.ID, claimant.Wallets, claimant.Subject, inviterUsers).Scan(&count, &sameInviter)
	if err != nil {
		return "", err
	}
	switch {
	case count > 1:
		return "", ErrIdentityConflict
	case count == 1 && sameInviter:
		return Bound, nil
	case count == 1:
		return AlreadyBound, nil
	case inviterSignup.After(firstSignup):
		return ReferrerNewer, nil
	case time.Now().After(firstSignup.Add(ClaimWindow)):
		return WindowClosed, nil
	}
	_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.referral_bindings
(referee_app_user_id, referee_wallet, referrer_app_user_id, source, referee_signed_up_at, referee_privy_subject)
VALUES ($1,$2,$3,$4,$5,NULLIF($6,''))`, caller.ID, caller.Wallet, referrer, string(source), firstSignup, claimant.Subject)
	if err != nil {
		return "", err
	}
	return Bound, tx.Commit(ctx)
}

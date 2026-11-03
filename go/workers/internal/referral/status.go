package referral

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"
)

type FriendStatus string

const (
	Joined    FriendStatus = "joined"
	Deposited FriendStatus = "deposited"
	Withdrawn FriendStatus = "withdrawn"
)

type Friend struct {
	Label          string
	Status         FriendStatus
	DepositedMicro int64
	BoundAt        time.Time
}

func status(positions int, principalMicro int64) FriendStatus {
	switch {
	case positions == 0:
		return Joined
	case principalMicro > 0:
		return Deposited
	}
	return Withdrawn
}

type boundFriend struct {
	id, wallet string
	Friend
}

func (s *Store) Friends(ctx context.Context, referrer string) ([]Friend, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	rows, err := s.yield.Query(ctx, `WITH earn_max AS (
  SELECT wallet, count(*) FILTER (WHERE kind = 'deposit') AS deposits, sum(delta) - least(0, min(running)) AS principal
  FROM (SELECT f.wallet, f.kind, d.delta, sum(d.delta) OVER (PARTITION BY f.wallet ORDER BY f.slot, f.transaction_index, f.kind = 'deposit') AS running
        FROM loyal_yield.earn_max_cash_flows f
        JOIN loyal_yield.referral_bindings b ON b.referee_wallet = f.wallet AND b.referrer_app_user_id = $1
        CROSS JOIN LATERAL (SELECT CASE f.kind WHEN 'deposit' THEN f.amount_raw ELSE -f.amount_raw END AS delta) d) flow
  GROUP BY wallet
)
SELECT b.referee_app_user_id::text, b.referee_wallet, b.bound_at,
       (count(p.id) + COALESCE(m.deposits, 0))::int, (COALESCE(sum(p.principal_amount_raw), 0) + COALESCE(m.principal, 0))::bigint
FROM loyal_yield.referral_bindings b
LEFT JOIN loyal_yield.user_yield_positions p ON p.wallet_address = b.referee_wallet
LEFT JOIN earn_max m ON m.wallet = b.referee_wallet
WHERE b.referrer_app_user_id = $1
GROUP BY b.referee_app_user_id, b.referee_wallet, b.bound_at, m.deposits, m.principal
ORDER BY b.bound_at DESC, b.referee_app_user_id`, referrer)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bound []boundFriend
	var ids []string
	for rows.Next() {
		var f boundFriend
		var positions int
		if err := rows.Scan(&f.id, &f.wallet, &f.BoundAt, &positions, &f.DepositedMicro); err != nil {
			return nil, err
		}
		f.Status = status(positions, f.DepositedMicro)
		bound = append(bound, f)
		ids = append(ids, f.id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	emails, err := s.emails(ctx, ids)
	if err != nil {
		return nil, err
	}
	friends := make([]Friend, len(bound))
	for i, f := range bound {
		f.Label = maskedLabel(emails[f.id], f.wallet)
		friends[i] = f.Friend
	}
	return friends, nil
}

func (s *Store) emails(ctx context.Context, ids []string) (map[string]string, error) {
	emails := map[string]string{}
	if len(ids) == 0 {
		return emails, nil
	}
	rows, err := s.app.Query(ctx, `SELECT id::text, email FROM app_users WHERE id = ANY($1::uuid[]) AND email IS NOT NULL`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, email string
		if err := rows.Scan(&id, &email); err != nil {
			return nil, err
		}
		emails[id] = email
	}
	return emails, rows.Err()
}

const minLocalForInitial = 3

func maskedLabel(email, wallet string) string {
	local, domain, ok := strings.Cut(email, "@")
	if ok && local != "" && domain != "" {
		if utf8.RuneCountInString(local) < minLocalForInitial {
			return "…@" + domain
		}
		initial, _ := utf8.DecodeRuneInString(local)
		return string(initial) + "…@" + domain
	}
	if len(wallet) <= 8 {
		return wallet
	}
	return wallet[:4] + "…" + wallet[len(wallet)-4:]
}

package admin

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	smartAccountsLamportsPerSOL = 1_000_000_000
	smartAccountsNativeSOLMint  = "So11111111111111111111111111111111111111112"
	smartAccountsJupiterSearch  = "https://lite-api.jup.ag/tokens/v2/search"
	smartAccountsSolanaEnv      = "mainnet"
)

type SmartAccountsPage struct {
	CreationPoints   []SmartAccountCreationPoint   `json:"creationPoints" nullable:"false"`
	Registrations    []SmartAccountRegistrationRow `json:"registrations" nullable:"false"`
	SolPriceUsd      *float64                      `json:"solPriceUsd"`
	SpendPoints      []SmartAccountSpendPoint      `json:"spendPoints" nullable:"false"`
	TotalAccounts    int64                         `json:"totalAccounts"`
	TotalCreated30d  int64                         `json:"totalCreated30d"`
	TotalSpentSol30d float64                       `json:"totalSpentSol30d"`
	TotalSpentUsd30d *float64                      `json:"totalSpentUsd30d"`
}

type SmartAccountCreationPoint struct {
	Count int64  `json:"count"`
	Date  string `json:"date"`
}

type SmartAccountSpendPoint struct {
	Amount float64  `json:"amount"`
	Date   string   `json:"date"`
	Usd    *float64 `json:"usd"`
}

type SmartAccountRegistrationRow struct {
	ID                   string  `json:"id"`
	RegisteredAt         string  `json:"registeredAt"`
	SolanaEnv            string  `json:"solanaEnv"`
	SponsorshipSignature *string `json:"sponsorshipSignature"`
	UserAddress          string  `json:"userAddress"`
	VaultAddress         *string `json:"vaultAddress"`
}

func smartAccountsRound(v float64, digits int) float64 {
	rounded, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', digits, 64), 64)
	return rounded
}

func smartAccountsSolPrice(ctx context.Context) *float64 {
	var tokens []struct {
		ID       string `json:"id"`
		UsdPrice any    `json:"usdPrice"`
	}
	query := url.Values{"query": {smartAccountsNativeSOLMint}}.Encode()
	if err := readsFetchJSON(ctx, 8*time.Second, 1<<20, http.MethodGet, smartAccountsJupiterSearch+"?"+query,
		map[string]string{"Content-Type": "application/json"}, nil, &tokens); err != nil {
		return nil
	}
	for _, token := range tokens {
		if token.ID != smartAccountsNativeSOLMint {
			continue
		}
		if price, ok := journeysNumber(token.UsdPrice); ok && price > 0 {
			return &price
		}
		return nil
	}
	return nil
}

func loadSmartAccounts(ctx context.Context, d *Deps, _ PageRequest) (SmartAccountsPage, error) {
	now := time.Now().UTC()
	end := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	start := end.AddDate(0, 0, -30)

	price := make(chan *float64, 1)
	go func() { price <- smartAccountsSolPrice(ctx) }()

	createdByDay := map[string]int64{}
	spentLamportsByDay := map[string]float64{}
	page := SmartAccountsPage{CreationPoints: []SmartAccountCreationPoint{}, SpendPoints: []SmartAccountSpendPoint{}, Registrations: []SmartAccountRegistrationRow{}}

	err := d.AppRead(ctx, func(tx pgx.Tx) error {
		creations, err := tx.Query(ctx, `SELECT count(*), to_char((date_trunc('day', created_at AT TIME ZONE 'UTC'))::date, 'YYYY-MM-DD') AS day
FROM app_user_smart_accounts
WHERE solana_env = $1 AND state = 'ready' AND created_at >= $2 AND created_at < $3
GROUP BY to_char((date_trunc('day', created_at AT TIME ZONE 'UTC'))::date, 'YYYY-MM-DD')`,
			smartAccountsSolanaEnv, start, end)
		if err != nil {
			return err
		}
		for creations.Next() {
			var count int64
			var day string
			if err := creations.Scan(&count, &day); err != nil {
				return err
			}
			createdByDay[day] = count
		}
		if err := creations.Err(); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM app_user_smart_accounts WHERE solana_env = $1 AND state = 'ready'`,
			smartAccountsSolanaEnv).Scan(&page.TotalAccounts); err != nil {
			return err
		}
		spend, err := tx.Query(ctx, `SELECT to_char((date_trunc('day', occurred_at AT TIME ZONE 'UTC'))::date, 'YYYY-MM-DD') AS day, sum(spent_lamports)
FROM app_smart_account_sponsorship_transactions
WHERE solana_env = $1 AND occurred_at >= $2 AND occurred_at < $3
GROUP BY to_char((date_trunc('day', occurred_at AT TIME ZONE 'UTC'))::date, 'YYYY-MM-DD')`,
			smartAccountsSolanaEnv, start, end)
		if err != nil {
			return err
		}
		for spend.Next() {
			var day string
			var total pgtype.Numeric
			if err := spend.Scan(&day, &total); err != nil {
				return err
			}
			if f, err := total.Float64Value(); err == nil && f.Valid {
				spentLamportsByDay[day] = f.Float64
			}
		}
		if err := spend.Err(); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT a.id::text, a.created_at, a.solana_env,
  coalesce(s.signature, a.creation_signature),
  coalesce(s.user_address, a.subject_address),
  coalesce(
    s.smart_account_address,
    (select c.smart_account_address from app_wallet_auth_completions c
      where c.user_id = a.user_id and c.solana_env = a.solana_env and c.state = 'completed' and c.smart_account_address is not null
      order by c.completed_at desc nulls last limit 1),
    a.user_smart_account_address
  )
FROM (
  SELECT a.id, a.user_id, a.created_at, a.solana_env, a.settings_pda, a.creation_signature,
    u.subject_address, u.smart_account_address AS user_smart_account_address
  FROM app_user_smart_accounts a
  INNER JOIN app_users u ON u.id = a.user_id
  WHERE a.solana_env = $1 AND a.state = 'ready'
  ORDER BY a.created_at DESC
  LIMIT 100
) a
LEFT JOIN LATERAL (
  select t.signature, t.user_address, t.smart_account_address from app_smart_account_sponsorship_transactions t
  where t.solana_env = a.solana_env and t.settings_pda = a.settings_pda
  order by t.occurred_at desc limit 1
) s ON true
ORDER BY a.created_at DESC`, smartAccountsSolanaEnv)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r SmartAccountRegistrationRow
			var registered time.Time
			if err := rows.Scan(&r.ID, &registered, &r.SolanaEnv, &r.SponsorshipSignature, &r.UserAddress, &r.VaultAddress); err != nil {
				return err
			}
			r.RegisteredAt = readsISO(registered)
			page.Registrations = append(page.Registrations, r)
		}
		return rows.Err()
	})
	if err != nil {
		return page, err
	}

	page.SolPriceUsd = <-price
	var spentLamports float64
	for i := 0; i < 30; i++ {
		day := start.AddDate(0, 0, i).Format("2006-01-02")
		created := createdByDay[day]
		lamports := spentLamportsByDay[day]
		sol := lamports / smartAccountsLamportsPerSOL
		page.TotalCreated30d += created
		spentLamports += lamports
		page.CreationPoints = append(page.CreationPoints, SmartAccountCreationPoint{Count: created, Date: day})
		point := SmartAccountSpendPoint{Amount: smartAccountsRound(sol, 6), Date: day}
		if page.SolPriceUsd != nil {
			usd := smartAccountsRound(sol*(*page.SolPriceUsd), 2)
			point.Usd = &usd
		}
		page.SpendPoints = append(page.SpendPoints, point)
	}
	totalSol := spentLamports / smartAccountsLamportsPerSOL
	page.TotalSpentSol30d = smartAccountsRound(totalSol, 6)
	if page.SolPriceUsd != nil {
		usd := smartAccountsRound(totalSol*(*page.SolPriceUsd), 2)
		page.TotalSpentUsd30d = &usd
	}
	return page, nil
}

func registerSmartAccounts(m *Mux) {
	Page(m, "/smart-accounts", loadSmartAccounts)
}

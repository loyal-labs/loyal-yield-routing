package admin

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
	"github.com/solana-foundation/solana-go/v2/rpc/jsonrpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

const (
	solanaRPCEnv = "LOYAL_ADMIN_SOLANA_RPC_URL"

	lamportsPerSOL                      = 1_000_000_000
	policyMinimumLamports               = 50_000_000
	sponsorshipCriticalRunwayHours      = 48
	sponsorshipLowRunwayHours           = 168
	mainnetAppEnv                       = "mainnet"
	mainnetYieldCluster                 = "mainnet-beta"
	settingsAuthoritySignaturePageLimit = 1_000
	settingsAuthorityMaxSignaturePages  = 10
	settingsAuthorityTransactionWorkers = 16
	fundingTimeout                      = 90 * time.Second
	day                                 = 24 * time.Hour
)

type earnFundingRole string

const (
	roleSponsorship       earnFundingRole = "sponsorship"
	rolePolicy            earnFundingRole = "policy"
	roleDeployment        earnFundingRole = "deployment"
	roleRouteFeePayer     earnFundingRole = "route_fee_payer"
	roleSettingsAuthority earnFundingRole = "settings_authority"
)

var earnDatabaseSpendTrackedRoles = map[earnFundingRole]bool{
	roleSponsorship: true, rolePolicy: true, roleDeployment: true, roleRouteFeePayer: true,
}

var earnSpendTrackedRoles = map[earnFundingRole]bool{
	roleSponsorship: true, rolePolicy: true, roleDeployment: true, roleRouteFeePayer: true, roleSettingsAuthority: true,
}

var earnOperationalWalletSpendGroups = map[string]bool{
	"gasless_store":                     true,
	"gasless_top_up_to_0_01_sol":        true,
	"gasless_verify_telegram_init_data": true,
	"lookup_table_close":                true,
	"lookup_table_create":               true,
	"lookup_table_deactivate":           true,
	"lookup_table_extend":               true,
	"lookup_table_rollover":             true,
	"lookup_table_verify":               true,
	"smart_account_sponsorship":         true,
	"yield_route_fee":                   true,
}

type EarnFundingWalletRole struct {
	Key              string  `json:"key"`
	Label            string  `json:"label"`
	ObservedCount    *int    `json:"observedCount"`
	LatestObservedAt *string `json:"latestObservedAt"`
}

type EarnFundingWallet struct {
	Address             string                  `json:"address"`
	BalanceLamports     *string                 `json:"balanceLamports"`
	BalanceObservedAt   *string                 `json:"balanceObservedAt"`
	BalanceSlot         *uint64                 `json:"balanceSlot"`
	BalanceError        *string                 `json:"balanceError"`
	ConfiguredAddresses []string                `json:"configuredAddresses" nullable:"false"`
	ObservedAddresses   []string                `json:"observedAddresses" nullable:"false"`
	Mismatch            bool                    `json:"mismatch"`
	Roles               []EarnFundingWalletRole `json:"roles" nullable:"false"`
	Status              string                  `json:"status"`
	StatusDetail        string                  `json:"statusDetail"`
	MinimumLamports     *string                 `json:"minimumLamports"`
	Spend24hLamports    *string                 `json:"spend24hLamports"`
	Spend7dLamports     *string                 `json:"spend7dLamports"`
	RunwayHours         *float64                `json:"runwayHours"`
}

type EarnFundingMissingRole struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Reason string `json:"reason"`
}

type EarnFundingSpendEvent struct {
	Address          string  `json:"address"`
	AmountBasis      string  `json:"amountBasis"`
	Group            string  `json:"group"`
	Lamports         string  `json:"lamports"`
	OccurredAt       string  `json:"occurredAt"`
	Signature        *string `json:"signature"`
	TransactionCount int     `json:"transactionCount"`
}

type EarnFundingData struct {
	Wallets           []EarnFundingWallet      `json:"wallets" nullable:"false"`
	MissingRoles      []EarnFundingMissingRole `json:"missingRoles" nullable:"false"`
	SpendEvents       []EarnFundingSpendEvent  `json:"spendEvents" nullable:"false"`
	SpendSourceErrors []string                 `json:"spendSourceErrors" nullable:"false"`
	SpendWindow       SafeReserveApyWindow     `json:"spendWindow"`
	SourceErrors      []string                 `json:"sourceErrors" nullable:"false"`
	ObservedAt        string                   `json:"observedAt"`
}

type earnIdentityObservation struct {
	address          string
	count            int
	latestObservedAt *string
}

type earnWalletSpend struct{ spend24h, spend7d string }

type earnSpendSource struct {
	events []EarnFundingSpendEvent
	failed bool
	spend  map[string]earnWalletSpend
}

type earnSpendWindow struct{ startedAt, endedAt time.Time }

func earnNormalizeAddress(value string) string { return strings.TrimSpace(value) }

func earnUniqueAddresses(values ...string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range values {
		if a := earnNormalizeAddress(v); a != "" && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}

func earnEnvOr(names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

func earnTimePtrISO(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := earnISO(*t)
	return &s
}

func earnSpendLamports(text *string) *big.Int {
	zero := new(big.Int)
	if text == nil {
		return zero
	}
	trimmed := strings.TrimSpace(*text)
	if !regexp.MustCompile(`^-?\d+$`).MatchString(trimmed) {
		return zero
	}
	n, _ := new(big.Int).SetString(trimmed, 10)
	if n.Sign() > 0 {
		return n
	}
	return zero
}

func earnNewSpendEvent(address string, basis string, group string, lamports *string, occurredAt *string, signature *string, count int) (EarnFundingSpendEvent, bool) {
	addr := earnNormalizeAddress(address)
	amount := earnSpendLamports(lamports)
	if addr == "" || occurredAt == nil || amount.Sign() == 0 || !earnOperationalWalletSpendGroups[group] {
		return EarnFundingSpendEvent{}, false
	}
	if basis != "compiled_fee" {
		basis = "confirmed_payer_outflow"
	}
	var sig *string
	if signature != nil {
		if s := earnNormalizeAddress(*signature); s != "" {
			sig = &s
		}
	}
	if count == 0 {
		count = 1
	}
	return EarnFundingSpendEvent{
		Address: addr, AmountBasis: basis, Group: group, Lamports: amount.String(),
		OccurredAt: *occurredAt, Signature: sig, TransactionCount: count,
	}, true
}

func (d *Deps) earnAppIdentityObservations(ctx context.Context) (sponsorship, deployment []earnIdentityObservation, sourceError string) {
	read := func(tx pgx.Tx, table string) ([]earnIdentityObservation, error) {
		rows, err := tx.Query(ctx, fmt.Sprintf(`
			SELECT payer_address, count(*), max(occurred_at)
			FROM %s
			WHERE solana_env = $1
			GROUP BY payer_address
			ORDER BY max(occurred_at) DESC
			LIMIT 1`, table), mainnetAppEnv)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []earnIdentityObservation
		for rows.Next() {
			var address string
			var count int
			var latest *time.Time
			if err := rows.Scan(&address, &count, &latest); err != nil {
				return nil, err
			}
			if a := earnNormalizeAddress(address); a != "" {
				out = append(out, earnIdentityObservation{a, count, earnTimePtrISO(latest)})
			}
		}
		return out, rows.Err()
	}
	err := d.AppRead(ctx, func(tx pgx.Tx) (err error) {
		if sponsorship, err = read(tx, "app_smart_account_sponsorship_transactions"); err != nil {
			return err
		}
		deployment, err = read(tx, "gasless_claim_transactions")
		return err
	})
	if err != nil {
		return nil, nil, "App database identity observations unavailable"
	}
	return sponsorship, deployment, ""
}

func (d *Deps) earnYieldIdentityObservations(ctx context.Context, settingsAuthorities []string) (policy, routeFeePayer, settingsAuthority []earnIdentityObservation, sourceError string) {
	read := func(tx pgx.Tx, query string, args ...any) ([]earnIdentityObservation, error) {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []earnIdentityObservation
		for rows.Next() {
			var address *string
			var count string
			var latest *time.Time
			if err := rows.Scan(&address, &count, &latest); err != nil {
				return nil, err
			}
			if address == nil || earnNormalizeAddress(*address) == "" {
				continue
			}
			n, _ := strconv.Atoi(count)
			out = append(out, earnIdentityObservation{earnNormalizeAddress(*address), n, earnTimePtrISO(latest)})
		}
		return out, rows.Err()
	}
	err := d.YieldRead(ctx, func(tx pgx.Tx) (err error) {
		if policy, err = read(tx, `
			SELECT
			  signer.address AS address,
			  COUNT(*)::text AS active_policy_count,
			  MAX(last_seen_at) AS latest_seen_at
			FROM loyal_yield.route_policies
			CROSS JOIN LATERAL unnest(delegated_signers) AS signer(address)
			WHERE active = true
			GROUP BY signer.address
			ORDER BY COUNT(*) DESC, MAX(last_seen_at) DESC`); err != nil {
			return err
		}
		if routeFeePayer, err = read(tx, `
			SELECT
			  fee_payer AS address,
			  COUNT(*)::text AS active_shard_count,
			  MAX(updated_at) AS latest_seen_at
			FROM loyal_yield.route_fee_payer_shard_status
			WHERE cluster = $1
			  AND enabled = true
			GROUP BY fee_payer
			ORDER BY COUNT(*) DESC, MAX(updated_at) DESC`, mainnetYieldCluster); err != nil {
			return err
		}
		if len(settingsAuthorities) == 0 {
			return nil
		}
		settingsAuthority, err = read(tx, `
			SELECT
			  authority AS address,
			  COUNT(*)::text AS active_policy_count,
			  MAX(last_seen_at) AS latest_seen_at
			FROM loyal_yield.route_policies
			WHERE active = true
			  AND authority = ANY($1::text[])
			GROUP BY authority
			ORDER BY COUNT(*) DESC, MAX(last_seen_at) DESC`, settingsAuthorities)
		return err
	})
	if err != nil {
		return nil, nil, nil, "Yield database identity observations unavailable"
	}
	return policy, routeFeePayer, settingsAuthority, ""
}

type earnAppSpendRow struct {
	address   string
	lamports  string
	occurred  time.Time
	signature *string
	txType    string
	count     int
}

func (d *Deps) earnAppSpend(ctx context.Context, table string, typed bool, group func(txType string) string, addresses []string, window earnSpendWindow) earnSpendSource {
	recentCutoff := window.endedAt.Add(-day)
	sevenDayCutoff := window.endedAt.Add(-7 * day)
	typeColumn, groupType := "''::text", ""
	if typed {
		typeColumn, groupType = "transaction_type", ", transaction_type"
	}
	failed := earnSpendSource{failed: true, spend: map[string]earnWalletSpend{}}
	var rows []earnAppSpendRow
	summary := map[string]earnWalletSpend{}
	err := d.AppRead(ctx, func(tx pgx.Tx) error {
		recent, err := tx.Query(ctx, fmt.Sprintf(`
			SELECT payer_address, spent_lamports::text, occurred_at, signature, %s
			FROM %s
			WHERE solana_env = $1 AND payer_address = ANY($2::text[]) AND occurred_at >= $3::timestamptz`, typeColumn, table),
			mainnetAppEnv, addresses, recentCutoff)
		if err != nil {
			return err
		}
		for recent.Next() {
			row := earnAppSpendRow{count: 1}
			if err := recent.Scan(&row.address, &row.lamports, &row.occurred, &row.signature, &row.txType); err != nil {
				recent.Close()
				return err
			}
			rows = append(rows, row)
		}
		recent.Close()
		if err := recent.Err(); err != nil {
			return err
		}
		historical, err := tx.Query(ctx, fmt.Sprintf(`
			SELECT payer_address, SUM(spent_lamports)::text, date_trunc('day', occurred_at), %s, COUNT(*)
			FROM %s
			WHERE solana_env = $1 AND payer_address = ANY($2::text[])
			  AND occurred_at >= $3::timestamptz AND occurred_at < $4::timestamptz
			GROUP BY payer_address%s, date_trunc('day', occurred_at)`, typeColumn, table, groupType),
			mainnetAppEnv, addresses, window.startedAt, recentCutoff)
		if err != nil {
			return err
		}
		for historical.Next() {
			var row earnAppSpendRow
			if err := historical.Scan(&row.address, &row.lamports, &row.occurred, &row.txType, &row.count); err != nil {
				historical.Close()
				return err
			}
			rows = append(rows, row)
		}
		historical.Close()
		if err := historical.Err(); err != nil {
			return err
		}
		totals, err := tx.Query(ctx, fmt.Sprintf(`
			SELECT payer_address,
			  COALESCE(SUM(spent_lamports) FILTER (WHERE occurred_at >= $4::timestamptz), 0)::text,
			  COALESCE(SUM(spent_lamports) FILTER (WHERE occurred_at >= $5::timestamptz), 0)::text
			FROM %s
			WHERE solana_env = $1 AND payer_address = ANY($2::text[]) AND occurred_at >= $3::timestamptz
			GROUP BY payer_address`, table),
			mainnetAppEnv, addresses, window.startedAt, recentCutoff, sevenDayCutoff)
		if err != nil {
			return err
		}
		defer totals.Close()
		for totals.Next() {
			var address string
			var s earnWalletSpend
			if err := totals.Scan(&address, &s.spend24h, &s.spend7d); err != nil {
				return err
			}
			summary[address] = s
		}
		return totals.Err()
	})
	if err != nil {
		return failed
	}
	events := []EarnFundingSpendEvent{}
	for _, row := range rows {
		occurred := earnISO(row.occurred)
		lamports := row.lamports
		if event, ok := earnNewSpendEvent(row.address, "confirmed_payer_outflow", group(row.txType), &lamports, &occurred, row.signature, row.count); ok {
			events = append(events, event)
		}
	}
	return earnSpendSource{events: events, spend: summary}
}

type earnYieldSpendRow struct {
	Address          string          `json:"address"`
	AmountBasis      string          `json:"amount_basis"`
	GroupKey         string          `json:"group_key"`
	Lamports         earnDecimalText `json:"lamports"`
	OccurredAt       *string         `json:"occurred_at"`
	Signature        *string         `json:"signature"`
	TransactionCount earnDecimalText `json:"transaction_count"`
}

type earnYieldSpendSummary struct {
	Address          string `json:"address"`
	Spend24hLamports string `json:"spend24h_lamports"`
	Spend7dLamports  string `json:"spend7d_lamports"`
}

const earnYieldSpendQuery = `
        WITH route_source_rows AS MATERIALIZED (
          SELECT fee_payer AS address, compiled_fee_lamports AS lamports,
            confirmed_at AS occurred_at, transaction_signature AS signature
          FROM loyal_yield.signed_route_submissions
          WHERE cluster = $1 AND confirmed_at IS NOT NULL
            AND fee_payer = ANY($2::text[]) AND confirmed_at >= $3::timestamptz
        ),
        route_event_rows AS (
          SELECT address, 'compiled_fee' AS amount_basis,
            'yield_route_fee' AS group_key, lamports::text, occurred_at,
            signature, 1::text AS transaction_count
          FROM route_source_rows
          WHERE occurred_at >= $4::timestamptz
          UNION ALL
          SELECT address, 'compiled_fee', 'yield_route_fee',
            SUM(lamports)::text, date_trunc('day', occurred_at), NULL,
            COUNT(*)::text
          FROM route_source_rows
          WHERE occurred_at < $4::timestamptz
          GROUP BY address, date_trunc('day', occurred_at)
        ),
        route_summary_rows AS (
          SELECT address,
            COALESCE(SUM(lamports) FILTER (
              WHERE occurred_at >= $4::timestamptz
            ), 0)::text AS spend24h_lamports,
            COALESCE(SUM(lamports) FILTER (
              WHERE occurred_at >= $5::timestamptz
            ), 0)::text AS spend7d_lamports
          FROM route_source_rows
          GROUP BY address
        ),
        lookup_source_rows AS MATERIALIZED (
          SELECT family.payer AS address,
            'lookup_table_' || operation.operation_kind AS group_key,
            GREATEST(COALESCE(operation.actual_fee_lamports, 0) + COALESCE(operation.actual_rent_lamports, 0) - COALESCE(operation.reclaimed_rent_lamports, 0), 0) AS lamports,
            operation.confirmed_at AS occurred_at,
            operation.transaction_signature AS signature
          FROM loyal_yield.lookup_table_operations AS operation
          JOIN loyal_yield.lookup_table_families AS family ON family.id = operation.family_id
          WHERE family.cluster = $1 AND operation.confirmed_at IS NOT NULL
            AND family.payer = ANY($2::text[]) AND operation.confirmed_at >= $3::timestamptz
        ),
        lookup_event_rows AS (
          SELECT address, 'confirmed_payer_outflow' AS amount_basis,
            group_key, lamports::text, occurred_at, signature,
            1::text AS transaction_count
          FROM lookup_source_rows
          WHERE occurred_at >= $4::timestamptz
          UNION ALL
          SELECT address, 'confirmed_payer_outflow', group_key,
            SUM(lamports)::text, date_trunc('day', occurred_at), NULL,
            COUNT(*)::text
          FROM lookup_source_rows
          WHERE occurred_at < $4::timestamptz
          GROUP BY address, group_key, date_trunc('day', occurred_at)
        ),
        lookup_summary_rows AS (
          SELECT address,
            COALESCE(SUM(lamports) FILTER (
              WHERE occurred_at >= $4::timestamptz
            ), 0)::text AS spend24h_lamports,
            COALESCE(SUM(lamports) FILTER (
              WHERE occurred_at >= $5::timestamptz
            ), 0)::text AS spend7d_lamports
          FROM lookup_source_rows
          GROUP BY address
        )
        SELECT
          COALESCE((
            SELECT json_agg(route_event_rows ORDER BY occurred_at, signature)
            FROM route_event_rows
          ), '[]'::json) AS route_rows,
          COALESCE((
            SELECT json_agg(lookup_event_rows ORDER BY occurred_at, signature)
            FROM lookup_event_rows
          ), '[]'::json) AS lookup_rows,
          COALESCE((
            SELECT json_agg(route_summary_rows ORDER BY address)
            FROM route_summary_rows
          ), '[]'::json) AS route_summary_rows,
          COALESCE((
            SELECT json_agg(lookup_summary_rows ORDER BY address)
            FROM lookup_summary_rows
          ), '[]'::json) AS lookup_summary_rows
      `

func (d *Deps) earnYieldSpend(ctx context.Context, addresses []string, window earnSpendWindow) earnSpendSource {
	failed := earnSpendSource{failed: true, spend: map[string]earnWalletSpend{}}
	var routeRows, lookupRows, routeSummary, lookupSummary []byte
	err := d.YieldRead(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, earnYieldSpendQuery, mainnetYieldCluster, addresses, window.startedAt, window.endedAt.Add(-day), window.endedAt.Add(-7*day)).
			Scan(&routeRows, &lookupRows, &routeSummary, &lookupSummary)
	})
	if err != nil {
		return failed
	}
	var routes, lookups []earnYieldSpendRow
	var routeTotals, lookupTotals []earnYieldSpendSummary
	for _, column := range []struct {
		raw  []byte
		into any
	}{{routeRows, &routes}, {lookupRows, &lookups}, {routeSummary, &routeTotals}, {lookupSummary, &lookupTotals}} {
		if earnDecodeColumn(column.raw, column.into) != nil {
			return failed
		}
	}
	type pair struct{ d24, d7 *big.Int }
	sums := map[string]pair{}
	order := []string{}
	for _, row := range append(routeTotals, lookupTotals...) {
		cur, ok := sums[row.Address]
		if !ok {
			cur = pair{new(big.Int), new(big.Int)}
			order = append(order, row.Address)
		}
		cur.d24.Add(cur.d24, earnDecimalText(row.Spend24hLamports).big())
		cur.d7.Add(cur.d7, earnDecimalText(row.Spend7dLamports).big())
		sums[row.Address] = cur
	}
	spend := map[string]earnWalletSpend{}
	for _, address := range order {
		spend[address] = earnWalletSpend{sums[address].d24.String(), sums[address].d7.String()}
	}
	events := []EarnFundingSpendEvent{}
	for _, row := range append(routes, lookups...) {
		lamports := string(row.Lamports)
		if event, ok := earnNewSpendEvent(row.Address, row.AmountBasis, row.GroupKey, &lamports, earnIsoText(row.OccurredAt), row.Signature, row.TransactionCount.count()); ok {
			events = append(events, event)
		}
	}
	return earnSpendSource{events: events, spend: spend}
}

func (d *Deps) earnWalletSpend(ctx context.Context, addresses []string, window earnSpendWindow) (events []EarnFundingSpendEvent, spend map[string]earnWalletSpend, sourceErrors []string) {
	sourceErrors = []string{}
	if len(addresses) == 0 {
		return []EarnFundingSpendEvent{}, map[string]earnWalletSpend{}, sourceErrors
	}
	var sponsorship, gasless, yield earnSpendSource
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		sponsorship = d.earnAppSpend(ctx, "app_smart_account_sponsorship_transactions", false, func(string) string { return "smart_account_sponsorship" }, addresses, window)
	}()
	go func() {
		defer wg.Done()
		gasless = d.earnAppSpend(ctx, "gasless_claim_transactions", true, func(t string) string { return "gasless_" + t }, addresses, window)
	}()
	go func() {
		defer wg.Done()
		yield = d.earnYieldSpend(ctx, addresses, window)
	}()
	wg.Wait()

	sources := []struct {
		error  string
		result earnSpendSource
	}{
		{"Smart-account sponsorship spend history unavailable", sponsorship},
		{"Gasless deployment spend history unavailable", gasless},
		{"Yield execution spend history unavailable", yield},
	}
	for _, s := range sources {
		if s.result.failed {
			sourceErrors = append(sourceErrors, s.error)
		}
	}
	if len(sourceErrors) > 0 {
		return []EarnFundingSpendEvent{}, map[string]earnWalletSpend{}, sourceErrors
	}
	events = []EarnFundingSpendEvent{}
	for _, s := range sources {
		events = append(events, s.result.events...)
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].OccurredAt < events[j].OccurredAt })

	type pair struct{ d24, d7 *big.Int }
	totals := map[string]pair{}
	for _, a := range addresses {
		totals[a] = pair{new(big.Int), new(big.Int)}
	}
	for _, s := range sources {
		for address, v := range s.result.spend {
			cur, ok := totals[address]
			if !ok {
				cur = pair{new(big.Int), new(big.Int)}
			}
			cur.d24.Add(cur.d24, earnDecimalText(v.spend24h).big())
			cur.d7.Add(cur.d7, earnDecimalText(v.spend7d).big())
			totals[address] = cur
		}
	}
	spend = map[string]earnWalletSpend{}
	for address, t := range totals {
		spend[address] = earnWalletSpend{t.d24.String(), t.d7.String()}
	}
	return events, spend, sourceErrors
}

func earnSolanaRPCClient(url string) *rpc.Client {
	return rpc.NewWithCustomRPCClient(jsonrpc.NewClientWithOpts(url, &jsonrpc.RPCClientOpts{
		HTTPClient: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}))
}

func earnFinalizedPayerOutflow(result *rpc.GetTransactionResult, payer solana.PublicKey) (uint64, error) {
	if result.Meta == nil || result.Transaction == nil {
		return 0, errors.New("payer balance data unavailable")
	}
	tx, err := result.Transaction.GetTransaction()
	if err != nil || len(tx.Message.AccountKeys) == 0 {
		return 0, errors.New("transaction message unavailable")
	}
	if tx.Message.AccountKeys[0] != payer {
		return 0, nil
	}
	pre, post := result.Meta.PreBalances, result.Meta.PostBalances
	if len(pre) == 0 || len(post) == 0 {
		return 0, errors.New("payer balance data unavailable")
	}
	if post[0] >= pre[0] {
		return 0, nil
	}
	return pre[0] - post[0], nil
}

func earnSettingsAuthoritySpend(ctx context.Context, url string, addresses []string, window earnSpendWindow) (map[string]earnWalletSpend, string) {
	spend := map[string]earnWalletSpend{}
	if len(addresses) == 0 {
		return spend, ""
	}
	const failure = "Settings authority finalized spend history unavailable"
	if url == "" {
		return spend, failure
	}
	client := earnSolanaRPCClient(url)
	cutoff24h := window.endedAt.Add(-day).UnixMilli()
	startedMs, endedMs := window.startedAt.UnixMilli(), window.endedAt.UnixMilli()
	for _, address := range addresses {
		key, err := solana.PublicKeyFromBase58(address)
		if err != nil {
			return map[string]earnWalletSpend{}, failure
		}
		var signatures []solana.Signature
		var before solana.Signature
		reached := false
		for page := 0; page < settingsAuthorityMaxSignaturePages; page++ {
			limit := settingsAuthoritySignaturePageLimit
			rows, err := client.GetSignaturesForAddressWithOpts(ctx, key, &rpc.GetSignaturesForAddressOpts{Limit: &limit, Before: before, Commitment: rpc.CommitmentFinalized})
			if err != nil {
				return map[string]earnWalletSpend{}, failure
			}
			for _, row := range rows {
				if row.BlockTime != nil {
					if at := int64(*row.BlockTime) * 1000; at < startedMs || at > endedMs {
						continue
					}
				}
				signatures = append(signatures, row.Signature)
			}
			var oldest *int64
			for i := len(rows) - 1; i >= 0; i-- {
				if rows[i].BlockTime != nil {
					t := int64(*rows[i].BlockTime)
					oldest = &t
					break
				}
			}
			if len(rows) < limit || (oldest != nil && *oldest*1000 < startedMs) || len(rows) == 0 {
				reached = true
				break
			}
			before = rows[len(rows)-1].Signature
		}
		if !reached {
			return map[string]earnWalletSpend{}, failure
		}

		version := uint64(1)
		var mu sync.Mutex
		var firstErr error
		var d24, d7 uint64
		jobs := make(chan solana.Signature)
		var wg sync.WaitGroup
		for w := 0; w < settingsAuthorityTransactionWorkers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for sig := range jobs {
					result, err := client.GetTransaction(ctx, sig, &rpc.GetTransactionOpts{Encoding: solana.EncodingBase64, Commitment: rpc.CommitmentFinalized, MaxSupportedTransactionVersion: &version})
					var outflow uint64
					var at int64
					if err == nil && (result.BlockTime == nil) {
						err = errors.New("block time unavailable")
					}
					if err == nil {
						at = int64(*result.BlockTime) * 1000
						if at >= startedMs && at <= endedMs {
							outflow, err = earnFinalizedPayerOutflow(result, key)
						} else {
							continue
						}
					}
					mu.Lock()
					if err != nil && firstErr == nil {
						firstErr = err
					} else if err == nil {
						d7 += outflow
						if at >= cutoff24h {
							d24 += outflow
						}
					}
					mu.Unlock()
				}
			}()
		}
		for _, sig := range signatures {
			jobs <- sig
		}
		close(jobs)
		wg.Wait()
		if firstErr != nil {
			return map[string]earnWalletSpend{}, failure
		}
		spend[address] = earnWalletSpend{strconv.FormatUint(d24, 10), strconv.FormatUint(d7, 10)}
	}
	return spend, ""
}

type earnBalanceRead struct {
	balances map[string]string
	slot     *uint64
	observed string
	err      string
}

func earnReadBalances(ctx context.Context, url string, addresses []string) earnBalanceRead {
	out := earnBalanceRead{balances: map[string]string{}, observed: earnISO(time.Now())}
	if len(addresses) == 0 {
		return out
	}
	if url == "" {
		out.err = "Solana RPC is not configured (" + solanaRPCEnv + ")"
		return out
	}
	keys := make([]solana.PublicKey, len(addresses))
	for i, address := range addresses {
		key, err := solana.PublicKeyFromBase58(address)
		if err != nil {
			out.err = "Solana RPC balance read failed"
			return out
		}
		keys[i] = key
	}
	client, err := chain.New(url, 30*time.Second)
	if err != nil {
		out.err = "Solana RPC balance read failed"
		return out
	}
	slot, accounts, err := client.Accounts(ctx, keys, rpc.CommitmentConfirmed, 0)
	if err != nil {
		out.err = "Solana RPC balance read failed"
		return out
	}
	out.slot = &slot
	for i, account := range accounts {
		if account != nil {
			out.balances[addresses[i]] = strconv.FormatUint(account.Lamports, 10)
		}
	}
	return out
}

type earnRoleDefinition struct {
	key        earnFundingRole
	label      string
	configured []string
	observed   []earnIdentityObservation
}

func (r earnRoleDefinition) addresses() []string {
	all := append([]string{}, r.configured...)
	for _, o := range r.observed {
		all = append(all, o.address)
	}
	return earnUniqueAddresses(all...)
}

func earnCalculateRunwayHours(balance, spend7d *big.Int) *float64 {
	if balance == nil || spend7d == nil || spend7d.Sign() <= 0 {
		return nil
	}
	b, _ := new(big.Float).SetInt(balance).Float64()
	s, _ := new(big.Float).SetInt(spend7d).Float64()
	hours := b / (s / (7 * 24))
	return &hours
}

func earnCalculateStatus(balance, minimum *big.Int, roles []earnFundingRole, runway *float64, spendAvailable, earnSpendTracked bool) (string, string) {
	if balance == nil {
		return "unknown", "Balance unavailable from confirmed Solana RPC"
	}
	if minimum != nil && balance.Cmp(minimum) < 0 {
		m, _ := new(big.Float).SetInt(minimum).Float64()
		return "critical", fmt.Sprintf("Below %s SOL safety floor", strconv.FormatFloat(m/lamportsPerSOL, 'f', -1, 64))
	}
	sponsorship := false
	for _, r := range roles {
		sponsorship = sponsorship || r == roleSponsorship
	}
	if sponsorship && runway != nil {
		if *runway < sponsorshipCriticalRunwayHours {
			return "critical", fmt.Sprintf("About %.1f hours of sponsorship runway", *runway)
		}
		if *runway < sponsorshipLowRunwayHours {
			return "low", fmt.Sprintf("About %.1f days of sponsorship runway", *runway/24)
		}
	}
	if sponsorship && runway == nil {
		return "low", "Balance available; recent sponsorship spend is unavailable"
	}
	if !earnSpendTracked {
		return "unknown", "Balance is above the safety floor, but spend is not tracked for this wallet so runway cannot be assessed"
	}
	if !spendAvailable {
		return "low", "Balance available; recent spend history is unavailable"
	}
	return "healthy", "Balance is above the configured operating thresholds"
}

type earnWalletAccumulator struct {
	configured []string
	observed   []string
	roles      []EarnFundingWalletRole
	roleKeys   []earnFundingRole
}

func earnAppendUnique(list []string, values ...string) []string {
	for _, v := range values {
		found := false
		for _, e := range list {
			found = found || e == v
		}
		if !found {
			list = append(list, v)
		}
	}
	return list
}

func earnCreateWallets(definitions []earnRoleDefinition, balances earnBalanceRead, spend map[string]earnWalletSpend) ([]EarnFundingWallet, []EarnFundingMissingRole) {
	order := []string{}
	byAddress := map[string]*earnWalletAccumulator{}
	missing := []EarnFundingMissingRole{}
	for _, definition := range definitions {
		addresses := definition.addresses()
		if len(addresses) == 0 {
			reason := "No usable public address was resolved"
			if len(definition.observed) == 0 {
				reason = "No public address was configured or observed"
			}
			missing = append(missing, EarnFundingMissingRole{string(definition.key), definition.label, reason})
			continue
		}
		for _, address := range addresses {
			acc, ok := byAddress[address]
			if !ok {
				acc = &earnWalletAccumulator{}
				byAddress[address] = acc
				order = append(order, address)
			}
			acc.configured = earnAppendUnique(acc.configured, definition.configured...)
			for _, o := range definition.observed {
				acc.observed = earnAppendUnique(acc.observed, o.address)
			}
			role := EarnFundingWalletRole{Key: string(definition.key), Label: definition.label}
			for _, o := range definition.observed {
				if o.address == address {
					count := o.count
					role.ObservedCount, role.LatestObservedAt = &count, o.latestObservedAt
					break
				}
			}
			acc.roles = append(acc.roles, role)
			acc.roleKeys = append(acc.roleKeys, definition.key)
		}
	}

	wallets := make([]EarnFundingWallet, 0, len(order))
	for _, address := range order {
		acc := byAddress[address]
		var balance *big.Int
		var balanceText *string
		if text, ok := balances.balances[address]; ok {
			balance, _ = new(big.Int).SetString(text, 10)
			balanceText = &text
		}
		s, hasSpend := spend[address]
		var spend24h, spend7d *string
		var spend7dInt *big.Int
		if hasSpend {
			spend24h, spend7d = &s.spend24h, &s.spend7d
			spend7dInt = earnDecimalText(s.spend7d).big()
		}
		runway := earnCalculateRunwayHours(balance, spend7dInt)
		var minimum *big.Int
		var minimumText *string
		for _, key := range acc.roleKeys {
			if key == rolePolicy || key == roleDeployment || key == roleRouteFeePayer || key == roleSettingsAuthority {
				minimum = big.NewInt(policyMinimumLamports)
				m := minimum.String()
				minimumText = &m
			}
		}
		tracked := false
		for _, key := range acc.roleKeys {
			tracked = tracked || earnSpendTracked(key)
		}
		status, detail := earnCalculateStatus(balance, minimum, acc.roleKeys, runway, hasSpend, tracked)
		mismatch := false
		for _, key := range acc.roleKeys {
			for _, definition := range definitions {
				if definition.key != key || len(definition.configured) == 0 {
					continue
				}
				if len(definition.observed) == 0 {
					mismatch = true
				}
				for _, configured := range definition.configured {
					seen := false
					for _, o := range definition.observed {
						seen = seen || o.address == configured
					}
					mismatch = mismatch || !seen
				}
			}
		}
		var balanceErr *string
		if balance == nil {
			message := "Account not found at the confirmed RPC slot"
			if balances.err != "" {
				message = balances.err
			}
			balanceErr = &message
		}
		var observedAt *string
		var slot *uint64
		if balances.err == "" {
			observedAt, slot = &balances.observed, balances.slot
		}
		wallets = append(wallets, EarnFundingWallet{
			Address: address, BalanceLamports: balanceText, BalanceObservedAt: observedAt, BalanceSlot: slot,
			BalanceError: balanceErr, ConfiguredAddresses: earnNonNil(acc.configured), ObservedAddresses: earnNonNil(acc.observed),
			Mismatch: mismatch, Roles: acc.roles, Status: status, StatusDetail: detail, MinimumLamports: minimumText,
			Spend24hLamports: spend24h, Spend7dLamports: spend7d, RunwayHours: runway,
		})
	}
	return wallets, missing
}

func earnSpendTracked(key earnFundingRole) bool { return earnSpendTrackedRoles[key] }

func earnNonNil(list []string) []string {
	if list == nil {
		return []string{}
	}
	return list
}

func (d *Deps) EarnFundingData(ctx context.Context) (EarnFundingData, error) {
	ctx, cancel := context.WithTimeout(ctx, fundingTimeout)
	defer cancel()
	now := time.Now()
	window := earnSpendWindow{endedAt: now}
	year := now.UTC().Year()
	if now.UTC().Month() < time.July {
		year--
	}
	window.startedAt = time.Date(year, time.July, 1, 0, 0, 0, 0, time.UTC)

	rpcURL := strings.TrimSpace(os.Getenv(solanaRPCEnv))
	configuredSettings := earnEnvOr("EARN_SETTINGS_AUTHORITY_PUBLIC_KEY")
	settingsAddresses := earnUniqueAddresses(configuredSettings)

	var settingsSpend map[string]earnWalletSpend
	var settingsErr string
	var settingsWG sync.WaitGroup
	settingsWG.Add(1)
	go func() {
		defer settingsWG.Done()
		settingsSpend, settingsErr = earnSettingsAuthoritySpend(ctx, rpcURL, settingsAddresses, earnSpendWindow{startedAt: now.Add(-7 * day), endedAt: now})
	}()

	var sponsorship, deployment, policy, routeFeePayer, settingsAuthority []earnIdentityObservation
	var appErr, yieldErr string
	var identityWG sync.WaitGroup
	identityWG.Add(2)
	go func() {
		defer identityWG.Done()
		sponsorship, deployment, appErr = d.earnAppIdentityObservations(ctx)
	}()
	go func() {
		defer identityWG.Done()
		policy, routeFeePayer, settingsAuthority, yieldErr = d.earnYieldIdentityObservations(ctx, settingsAddresses)
	}()
	identityWG.Wait()

	sourceErrors := []string{}
	for _, e := range []string{appErr, yieldErr} {
		if e != "" {
			sourceErrors = append(sourceErrors, e)
		}
	}
	definitions := []earnRoleDefinition{
		{roleSponsorship, "Smart-account sponsorship", earnUniqueAddresses(earnEnvOr("SMART_ACCOUNT_SPONSOR_PUBLIC_KEY")), sponsorship},
		{rolePolicy, "Earn policy / route execution", earnUniqueAddresses(earnEnvOr("EARN_POLICY_SIGNER_PUBLIC_KEY", "EARN_YIELD_ROUTER_PUBLIC_KEY")), policy},
		{roleDeployment, "Gasless deployment payer", earnUniqueAddresses(earnEnvOr("DEPLOYMENT_PUBLIC_KEY")), deployment},
		{roleSettingsAuthority, "Earn settings authority", settingsAddresses, settingsAuthority},
	}
	for _, observation := range routeFeePayer {
		definitions = append(definitions, earnRoleDefinition{roleRouteFeePayer, "Route fee-payer shard", []string{}, []earnIdentityObservation{observation}})
	}
	var all, databaseTracked []string
	for _, definition := range definitions {
		all = append(all, definition.addresses()...)
		if earnDatabaseSpendTrackedRoles[definition.key] {
			databaseTracked = append(databaseTracked, definition.addresses()...)
		}
	}
	all, databaseTracked = earnUniqueAddresses(all...), earnUniqueAddresses(databaseTracked...)

	var balances earnBalanceRead
	var events []EarnFundingSpendEvent
	var spend map[string]earnWalletSpend
	var spendErrors []string
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		balances = earnReadBalances(ctx, rpcURL, all)
	}()
	go func() {
		defer wg.Done()
		events, spend, spendErrors = d.earnWalletSpend(ctx, databaseTracked, window)
	}()
	wg.Wait()
	settingsWG.Wait()

	if balances.err != "" {
		sourceErrors = append(sourceErrors, balances.err)
	}
	sourceErrors = append(sourceErrors, spendErrors...)
	if settingsErr != "" {
		sourceErrors = append(sourceErrors, settingsErr)
	}
	merged := map[string]earnWalletSpend{}
	for address, v := range spend {
		merged[address] = v
	}
	for address, v := range settingsSpend {
		merged[address] = v
	}

	wallets, missing := earnCreateWallets(definitions, balances, merged)
	return EarnFundingData{
		Wallets: wallets, MissingRoles: missing, SpendEvents: events, SpendSourceErrors: spendErrors,
		SpendWindow:  SafeReserveApyWindow{EndedAt: earnISO(window.endedAt), StartedAt: earnISO(window.startedAt)},
		SourceErrors: sourceErrors, ObservedAt: balances.observed,
	}, nil
}

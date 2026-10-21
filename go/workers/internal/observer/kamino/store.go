package kamino

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/backyard"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

func earnMaxObservationTargets() []Target {
	rows := [][3]string{
		{"47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8", "6ZxkBSJEqsXA3Kdm2PDAzHLUdPTPUK93Lf4bAezec1UQ", "5Y8NV33Vv7WbnLfq3zBcKSdYPrk7g2KoiQoe7M2tcxp5"},
		{"47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8", "AYL4LMc4ZCVyq3Z7XPJGWDM4H9PiWjqXAAuuHBEGVR2Z", "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"},
		{"47tfyEG9SsdEnUm9cw5kY9BXngQGqu3LBoop9j5uTAv8", "3yDc9ARvtPLhYxZLgucZGuBtZ9bHshBvXTwHxGe3nhmC", "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA"},
		{"CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", "BUTND9T7Ux4KR8RAEgd4WoZwnP7xA279oA1y3iPVcvSh", "3b8X44fLF9ooXaUm3hhSgjpmVs6rZZ3pPoGnGahc3Uu7"},
		{"CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", "9GJ9GBRwCp4pHmWrQ43L5xpc9Vykg7jnfwcFGN8FoHYu", "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"},
		{"CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", "3ZUAwhEtK8XWfK4fy98z4yoptm4GeyeAu21L11HPXaZ5", "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"},
		{"CqAoLuqWtavaVE8deBjMKe8ZfSt9ghR6Vb8nfsyabyHA", "7SzMWArC8WAenndXFmRyfvcvrNPodqUFkmPrmmoRZvn4", "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA"},
		{"6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y", "AwCyCPZYJSZ93xcVKNK7jR8e1BHzJXq1D4bReNuh9woY", "AvZZF1YaZDziPY2RCK4oJrRVrbN3mTD9NL24hPeaZeUj"},
		{"6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y", "Atj6UREVWa7WxbF2EMKNyfmYUY1U1txughe2gjhcPDCo", "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"},
		{"6WEGfej9B9wjxRs6t4BYpb9iCXd8CpTpJ8fVSNzHCC5y", "92qeAka3ZzCGPfJriDXrE7tiNqfATVCAM6ZjjctR3TrS", "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo"},
	}
	targets := make([]Target, 0, len(rows))
	for _, row := range rows {
		market, mint := row[0], row[2]
		targets = append(targets, Target{Reserve: row[1], Market: &market, LiquidityMint: &mint})
	}
	return targets
}

func supplementalObservationTargets() []Target {
	resolved := map[string]bool{}
	for _, market := range policySupportedMarkets {
		resolved[market.market] = true
	}
	var targets []Target
	for _, r := range backyard.ObservationReserves() {
		if resolved[r.Market] {
			continue
		}
		market, name, mint := r.Market, r.MarketName, r.LiquidityMint
		targets = append(targets, Target{Reserve: r.Reserve, Market: &market, MarketName: &name, LiquidityMint: &mint})
	}
	return targets
}

type Store struct {
	pool   *pgxpool.Pool
	schema string
}

func NewStore(pool *pgxpool.Pool, schema string) *Store {
	if schema == "" {
		schema = "kamino"
	}
	return &Store{pool: pool, schema: schema}
}

func (s *Store) LoadTargets(ctx context.Context) ([]Target, error) {
	query := fmt.Sprintf(`SELECT reserve,market,market_name,symbol,liquidity_mint FROM %s.supported_reserves WHERE active=true ORDER BY market,liquidity_mint,reserve`, s.schema)
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("load Kamino targets: %w", err)
	}
	defer rows.Close()
	var targets []Target
	seen := make(map[string]struct{})
	for rows.Next() {
		var target Target
		var market, marketName, symbol, mint *string
		if err := rows.Scan(&target.Reserve, &market, &marketName, &symbol, &mint); err != nil {
			return nil, err
		}
		target.Market = market
		target.MarketName = marketName
		target.Symbol = symbol
		target.LiquidityMint = mint
		targets = append(targets, target)
		seen[target.Reserve] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, target := range earnMaxObservationTargets() {
		if _, exists := seen[target.Reserve]; !exists {
			targets = append(targets, target)
		}
	}
	return targets, nil
}

type Record struct {
	Target                                             Target
	Snapshot                                           Snapshot
	Diff                                               *Diff
	DiffSummary, Source, SourceCommitment, AccountHash string
	RawBase64                                          *string
	ReceivedAt, DecodedAt                              time.Time
	ReceiveToDecodeMS                                  int64
}
type InsertOutcome struct {
	EventID  int64
	Inserted bool
}

func (s *Store) Insert(ctx context.Context, record Record) (InsertOutcome, error) {
	if record.SourceCommitment != "confirmed" {
		return InsertOutcome{}, fmt.Errorf("kamino persistence requires confirmed commitment")
	}
	targetJSON, _ := json.Marshal(record.Target)
	snapshotJSON, _ := json.Marshal(record.Snapshot)
	diffJSON := []byte(`{}`)
	changedFields := []string{}
	diffChanged := false
	if record.Diff != nil {
		diffJSON, _ = json.Marshal(record.Diff)
		changedFields = record.Diff.ChangedFields
		diffChanged = record.Diff.Changed
	}
	recordJSON, _ := json.Marshal(map[string]any{"kind": "reserve_update", "source": record.Source, "observed_at": record.Snapshot.ObservedAt, "slot": record.Snapshot.Slot, "target": record.Target, "snapshot": record.Snapshot, "diff_summary": record.DiffSummary, "diff": record.Diff, "raw_account_data_base64": record.RawBase64, "source_commitment": record.SourceCommitment, "account_data_hash": record.AccountHash, "received_at": record.ReceivedAt, "decoded_at": record.DecodedAt, "receive_to_decode_ms": record.ReceiveToDecodeMS})
	provenance := ""
	if strings.HasPrefix(record.Source, "http_") {
		provenance = ":http"
	}
	dedupe := fmt.Sprintf("v2:%s%s:%s:%d:%s", record.SourceCommitment, provenance, record.Snapshot.Reserve, record.Snapshot.Slot, record.AccountHash)
	sequence := s.schema + ".reserve_update_event_id_seq"
	query := fmt.Sprintf(`WITH inserted_dedupe AS (
		INSERT INTO %s.reserve_update_dedupe(dedupe_key,event_id,reserve,slot,account_data_hash)
		VALUES($1,nextval('%s'::regclass),$2,$3,$4) ON CONFLICT(dedupe_key) DO NOTHING RETURNING event_id)
		INSERT INTO %s.reserve_updates(event_id,observed_at,slot,kind,source,reserve,market,market_name,symbol,liquidity_mint,mint_decimals,reserve_last_update_slot,reserve_last_update_stale,reserve_price_status,available_amount,borrowed_amount,borrowed_amount_sf,total_supply_amount,market_price_usd,market_price_last_updated_ts,cumulative_borrow_rate_bsf,total_supply_usd_estimate,total_borrow_usd_estimate,utilization,borrow_apr,supply_apr,borrow_apy,supply_apy,protocol_take_rate_pct,host_fixed_interest_rate_bps,diff_changed,changed_fields,diff_summary,diff,target,snapshot,record,raw_account_data_base64,api_supply_apy,api_borrow_apy,api_total_supply_usd,api_total_borrow_usd,source_commitment,account_data_hash,received_at,decoded_at,receive_to_decode_ms,decode_to_insert_ms)
		SELECT event_id,$5,$3,'reserve_update',$6,$2,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40,$41,$42,$43,$4,$44,$45,$46,GREATEST(0,EXTRACT(MILLISECONDS FROM now()-$45)::bigint) FROM inserted_dedupe RETURNING event_id`, s.schema, sequence, s.schema)
	cumulative := fmt.Sprintf("%d:%d:%d:%d", record.Snapshot.CumulativeBorrowRateBSF[0], record.Snapshot.CumulativeBorrowRateBSF[1], record.Snapshot.CumulativeBorrowRateBSF[2], record.Snapshot.CumulativeBorrowRateBSF[3])
	args := []any{dedupe, record.Snapshot.Reserve, int64(record.Snapshot.Slot), record.AccountHash, record.Snapshot.ObservedAt, record.Source, record.Snapshot.Market, record.Target.MarketName, coalesce(record.Snapshot.Symbol, record.Target.Symbol), record.Snapshot.LiquidityMint, int32(record.Snapshot.MintDecimals), int64(record.Snapshot.ReserveLastUpdateSlot), record.Snapshot.ReserveLastUpdateStale, int16(record.Snapshot.ReservePriceStatus), record.Snapshot.AvailableAmount, record.Snapshot.BorrowedAmount, record.Snapshot.BorrowedAmountSF, record.Snapshot.TotalSupplyAmount, record.Snapshot.MarketPriceUSD, int64(record.Snapshot.MarketPriceLastUpdatedTS), cumulative, record.Snapshot.TotalSupplyUSDEstimate, record.Snapshot.TotalBorrowUSDEstimate, record.Snapshot.Utilization, record.Snapshot.BorrowAPR, record.Snapshot.SupplyAPR, record.Snapshot.BorrowAPY, record.Snapshot.SupplyAPY, int16(record.Snapshot.ProtocolTakeRatePct), int32(record.Snapshot.HostFixedInterestRateBPS), diffChanged, changedFields, record.DiffSummary, diffJSON, targetJSON, snapshotJSON, recordJSON, record.RawBase64, record.Target.APISupplyAPY, record.Target.APIBorrowAPY, record.Target.APITotalSupplyUSD, record.Target.APITotalBorrowUSD, record.SourceCommitment, record.ReceivedAt, record.DecodedAt, record.ReceiveToDecodeMS}
	var outcome InsertOutcome
	err := db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, query, args...).Scan(&outcome.EventID)
		if err == pgx.ErrNoRows {
			return nil
		} else if err != nil {
			return fmt.Errorf("insert Kamino reserve update: %w", err)
		}
		outcome.Inserted = true
		_, err = tx.Exec(ctx, fmt.Sprintf(`INSERT INTO %[1]s.reserve_current_states AS current(reserve,state_event_id,account_data_hash,state_slot,state_observed_at,state_source)
VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(reserve) DO UPDATE SET state_event_id=EXCLUDED.state_event_id,account_data_hash=EXCLUDED.account_data_hash,state_slot=EXCLUDED.state_slot,state_observed_at=EXCLUDED.state_observed_at,state_source=EXCLUDED.state_source,updated_at=now()
WHERE (EXCLUDED.state_slot,EXCLUDED.state_event_id)>(current.state_slot,current.state_event_id)`, s.schema), record.Snapshot.Reserve, outcome.EventID, record.AccountHash, int64(record.Snapshot.Slot), record.Snapshot.ObservedAt, record.Source)
		if err != nil {
			return fmt.Errorf("advance Kamino current state: %w", err)
		}
		return nil
	})
	if err != nil {
		return InsertOutcome{}, err
	}
	return outcome, nil
}

func coalesce(first, second *string) *string {
	if first != nil {
		return first
	}
	return second
}

// supportedReserveCatalogLockKey is loyal-kamino-data timescale.rs
// SUPPORTED_RESERVE_CATALOG_LOCK_KEY, shared with the Rust explicit sync.
const supportedReserveCatalogLockKey int64 = 5_499_540_200_513_620

func (s *Store) RefreshSupportedReserves(ctx context.Context, records []SupportedReserveRecord) error {
	if len(records) == 0 {
		return fmt.Errorf("supported reserve catalog response must not be empty")
	}
	incoming, err := supportedReserveIdentities(records)
	if err != nil {
		return fmt.Errorf("supported reserve catalog response contains duplicate decoding identities")
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, supportedReserveCatalogLockKey); err != nil {
			return fmt.Errorf("acquire supported reserve catalog publication lock: %w", err)
		}
		rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT market,liquidity_mint,reserve FROM %s.supported_reserves WHERE active=TRUE ORDER BY market,liquidity_mint,reserve`, s.schema))
		if err != nil {
			return fmt.Errorf("load active supported reserve identities: %w", err)
		}
		var committed []SupportedReserveRecord
		for rows.Next() {
			var record SupportedReserveRecord
			if err := rows.Scan(&record.Market, &record.LiquidityMint, &record.Reserve); err != nil {
				rows.Close()
				return err
			}
			committed = append(committed, record)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		current, err := supportedReserveIdentities(committed)
		if err != nil {
			return fmt.Errorf("committed supported reserve catalog contains duplicate decoding identities")
		}
		if len(current) == 0 {
			return fmt.Errorf("normal supported reserve refresh requires a bootstrapped catalog; run explicit sync first")
		}
		removed, added := 0, 0
		for identity := range current {
			if _, ok := incoming[identity]; !ok {
				removed++
			}
		}
		for identity := range incoming {
			if _, ok := current[identity]; !ok {
				added++
			}
		}
		if removed != 0 || added != 0 {
			return fmt.Errorf("normal supported reserve refresh rejected decoding topology change; removed_identity_count=%d added_identity_count=%d; run explicit sync and restart", removed, added)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s.supported_reserves SET active=FALSE,updated_at=now() WHERE active=TRUE`, s.schema)); err != nil {
			return fmt.Errorf("deactivate existing supported reserves: %w", err)
		}
		upsert := fmt.Sprintf(`INSERT INTO %s.supported_reserves(market,liquidity_mint,reserve,market_name,symbol,risk_baskets,source,active,fetched_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,'kamino-api',TRUE,now(),now())
ON CONFLICT(market,liquidity_mint) DO UPDATE SET reserve=EXCLUDED.reserve,market_name=EXCLUDED.market_name,symbol=EXCLUDED.symbol,risk_baskets=EXCLUDED.risk_baskets,source=EXCLUDED.source,active=TRUE,fetched_at=EXCLUDED.fetched_at,updated_at=EXCLUDED.updated_at`, s.schema)
		for _, record := range records {
			if _, err := tx.Exec(ctx, upsert, record.Market, record.LiquidityMint, record.Reserve, record.MarketName, record.Symbol, record.RiskBaskets); err != nil {
				return fmt.Errorf("upsert supported reserve market %s mint %s: %w", record.Market, record.LiquidityMint, err)
			}
		}
		return nil
	})
}

func supportedReserveIdentities(records []SupportedReserveRecord) (map[[3]string]struct{}, error) {
	pairs := make(map[[2]string]struct{}, len(records))
	identities := make(map[[3]string]struct{}, len(records))
	for _, record := range records {
		pairs[[2]string{record.Market, record.LiquidityMint}] = struct{}{}
		identities[[3]string{record.Market, record.LiquidityMint, record.Reserve}] = struct{}{}
	}
	if len(pairs) != len(records) || len(identities) != len(records) {
		return nil, fmt.Errorf("duplicate decoding identity")
	}
	return identities, nil
}

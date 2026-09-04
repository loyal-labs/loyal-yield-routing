package kamino

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const floorLockSeed int64 = 5_499_540_200_513_621

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
	EventID                                              int64
	Inserted, CurrentStateAdmitted, VerificationAdmitted bool
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return InsertOutcome{}, err
	}
	defer tx.Rollback(ctx)
	cumulative := fmt.Sprintf("%d:%d:%d:%d", record.Snapshot.CumulativeBorrowRateBSF[0], record.Snapshot.CumulativeBorrowRateBSF[1], record.Snapshot.CumulativeBorrowRateBSF[2], record.Snapshot.CumulativeBorrowRateBSF[3])
	args := []any{dedupe, record.Snapshot.Reserve, int64(record.Snapshot.Slot), record.AccountHash, record.Snapshot.ObservedAt, record.Source, record.Snapshot.Market, record.Target.MarketName, coalesce(record.Snapshot.Symbol, record.Target.Symbol), record.Snapshot.LiquidityMint, int32(record.Snapshot.MintDecimals), int64(record.Snapshot.ReserveLastUpdateSlot), record.Snapshot.ReserveLastUpdateStale, int16(record.Snapshot.ReservePriceStatus), record.Snapshot.AvailableAmount, record.Snapshot.BorrowedAmount, record.Snapshot.BorrowedAmountSF, record.Snapshot.TotalSupplyAmount, record.Snapshot.MarketPriceUSD, int64(record.Snapshot.MarketPriceLastUpdatedTS), cumulative, record.Snapshot.TotalSupplyUSDEstimate, record.Snapshot.TotalBorrowUSDEstimate, record.Snapshot.Utilization, record.Snapshot.BorrowAPR, record.Snapshot.SupplyAPR, record.Snapshot.BorrowAPY, record.Snapshot.SupplyAPY, int16(record.Snapshot.ProtocolTakeRatePct), int32(record.Snapshot.HostFixedInterestRateBPS), diffChanged, changedFields, record.DiffSummary, diffJSON, targetJSON, snapshotJSON, recordJSON, record.RawBase64, record.Target.APISupplyAPY, record.Target.APIBorrowAPY, record.Target.APITotalSupplyUSD, record.Target.APITotalBorrowUSD, record.SourceCommitment, record.ReceivedAt, record.DecodedAt, record.ReceiveToDecodeMS}
	var outcome InsertOutcome
	err = tx.QueryRow(ctx, query, args...).Scan(&outcome.EventID)
	if err == pgx.ErrNoRows {
		outcome.Inserted = false
		if err = tx.QueryRow(ctx, fmt.Sprintf(`SELECT event_id FROM %s.reserve_update_dedupe WHERE dedupe_key=$1`, s.schema), dedupe).Scan(&outcome.EventID); err != nil {
			return InsertOutcome{}, err
		}
	} else if err != nil {
		return InsertOutcome{}, fmt.Errorf("insert Kamino reserve update: %w", err)
	} else {
		outcome.Inserted = true
	}
	if strings.HasPrefix(record.Source, "http_") {
		result, err := tx.Exec(ctx, s.upsertCurrentSQL(), record.Snapshot.Reserve, outcome.EventID, record.AccountHash, int64(record.Snapshot.Slot), record.Snapshot.ObservedAt, record.Source)
		if err != nil {
			return InsertOutcome{}, fmt.Errorf("advance Kamino current state: %w", err)
		}
		outcome.CurrentStateAdmitted = result.RowsAffected() > 0
		if _, err = tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %[1]s.reserve_confirmed_verifications verification USING %[1]s.reserve_current_states state WHERE state.reserve=$1 AND verification.reserve=state.reserve AND (verification.state_event_id<>state.state_event_id OR verification.account_data_hash<>state.account_data_hash)`, s.schema), record.Snapshot.Reserve); err != nil {
			return InsertOutcome{}, err
		}
		result, err = tx.Exec(ctx, s.upsertVerificationSQL(), record.Snapshot.Reserve, outcome.EventID, record.AccountHash, int64(record.Snapshot.Slot), record.ReceivedAt, record.SourceCommitment, record.Source)
		if err != nil {
			return InsertOutcome{}, fmt.Errorf("advance Kamino verification: %w", err)
		}
		outcome.VerificationAdmitted = result.RowsAffected() > 0
	} else {
		if _, err = tx.Exec(ctx, s.advanceFloorSQL(), record.Snapshot.Reserve, int64(record.Snapshot.Slot), record.AccountHash, true, record.Source, int16(1), record.Snapshot.ObservedAt); err != nil {
			return InsertOutcome{}, fmt.Errorf("advance Kamino stream floor: %w", err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return InsertOutcome{}, err
	}
	return outcome, nil
}

func (s *Store) RecordMalformed(ctx context.Context, reserve string, slot uint64, observedAt time.Time) error {
	_, err := s.pool.Exec(ctx, s.advanceFloorSQL(), reserve, int64(slot), nil, false, "laserstream_grpc", int16(1), observedAt)
	return err
}
func coalesce(first, second *string) *string {
	if first != nil {
		return first
	}
	return second
}
func (s *Store) tolerance() string {
	return fmt.Sprintf(`%s.confirmed_verification_slot_tolerance()`, s.schema)
}
func (s *Store) upsertCurrentSQL() string {
	return fmt.Sprintf(`INSERT INTO %[1]s.reserve_current_states AS current(reserve,state_event_id,account_data_hash,state_slot,state_observed_at,state_source)
SELECT state.reserve,state.event_id,state.account_data_hash,state.slot,state.observed_at,state.source FROM %[1]s.reserve_updates state LEFT JOIN %[1]s.reserve_confirmed_observation_floors floor ON floor.reserve=state.reserve LEFT JOIN %[1]s.reserve_confirmed_verifications verification ON verification.reserve=state.reserve WHERE state.reserve=$1 AND state.event_id=$2 AND state.account_data_hash=$3 AND state.slot=$4 AND state.observed_at=$5 AND state.source IN('http_snapshot','http_confirmed_refresh') AND $6 IN('http_snapshot','http_confirmed_refresh') AND (floor.reserve IS NULL OR state.slot>floor.floor_slot OR (floor.state_valid AND floor.account_data_hash=state.account_data_hash) OR (floor.state_valid AND floor.floor_slot>state.slot AND floor.floor_slot-state.slot<=%[2]s)) AND (verification.reserve IS NULL OR state.slot>verification.verified_slot OR verification.account_data_hash=state.account_data_hash) ON CONFLICT(reserve) DO UPDATE SET state_event_id=EXCLUDED.state_event_id,account_data_hash=EXCLUDED.account_data_hash,state_slot=EXCLUDED.state_slot,state_observed_at=EXCLUDED.state_observed_at,state_source=EXCLUDED.state_source,updated_at=now() WHERE (EXCLUDED.state_slot,EXCLUDED.state_event_id)>(current.state_slot,current.state_event_id)`, s.schema, s.tolerance())
}
func (s *Store) upsertVerificationSQL() string {
	return fmt.Sprintf(`INSERT INTO %[1]s.reserve_confirmed_verifications AS current(reserve,state_event_id,account_data_hash,verified_slot,verified_at,commitment,verification_source) SELECT $1,$2,$3,$4,$5,$6,$7 FROM %[1]s.reserve_current_states state LEFT JOIN %[1]s.reserve_confirmed_observation_floors floor ON floor.reserve=state.reserve WHERE state.reserve=$1 AND state.state_event_id=$2 AND state.account_data_hash=$3 AND state.state_slot<=$4 AND state.state_source IN('http_snapshot','http_confirmed_refresh') AND $7 IN('http_snapshot','http_confirmed_refresh') AND (floor.reserve IS NULL OR $4>floor.floor_slot OR (floor.state_valid AND floor.account_data_hash=state.account_data_hash) OR (floor.state_valid AND floor.floor_slot>$4 AND floor.floor_slot-$4<=%[2]s)) ON CONFLICT(reserve) DO UPDATE SET state_event_id=EXCLUDED.state_event_id,account_data_hash=EXCLUDED.account_data_hash,verified_slot=EXCLUDED.verified_slot,verified_at=EXCLUDED.verified_at,commitment=EXCLUDED.commitment,verification_source=EXCLUDED.verification_source,updated_at=now() WHERE (EXCLUDED.verified_slot,EXCLUDED.state_event_id)>(current.verified_slot,current.state_event_id)`, s.schema, s.tolerance())
}
func (s *Store) advanceFloorSQL() string {
	return fmt.Sprintf(`WITH observation_lock AS MATERIALIZED(SELECT pg_advisory_xact_lock(hashtextextended($1,%d))), advanced AS(INSERT INTO %[2]s.reserve_confirmed_observation_floors AS current(reserve,floor_slot,account_data_hash,state_valid,source,source_rank,observed_at) SELECT $1,$2,$3,$4,$5,$6,$7 FROM observation_lock ON CONFLICT(reserve) DO UPDATE SET floor_slot=CASE WHEN EXCLUDED.floor_slot>current.floor_slot THEN EXCLUDED.floor_slot ELSE current.floor_slot END,account_data_hash=CASE WHEN EXCLUDED.floor_slot>current.floor_slot THEN EXCLUDED.account_data_hash WHEN EXCLUDED.source_rank>current.source_rank THEN EXCLUDED.account_data_hash WHEN current.state_valid AND EXCLUDED.state_valid AND current.account_data_hash=EXCLUDED.account_data_hash THEN current.account_data_hash ELSE NULL END,state_valid=CASE WHEN EXCLUDED.floor_slot>current.floor_slot THEN EXCLUDED.state_valid WHEN EXCLUDED.source_rank>current.source_rank THEN EXCLUDED.state_valid ELSE current.state_valid AND EXCLUDED.state_valid AND current.account_data_hash=EXCLUDED.account_data_hash END,source=CASE WHEN EXCLUDED.floor_slot>current.floor_slot OR EXCLUDED.source_rank>=current.source_rank THEN EXCLUDED.source ELSE current.source END,source_rank=CASE WHEN EXCLUDED.floor_slot>current.floor_slot THEN EXCLUDED.source_rank ELSE GREATEST(current.source_rank,EXCLUDED.source_rank) END,observation_id=EXCLUDED.observation_id,observed_at=GREATEST(current.observed_at,EXCLUDED.observed_at),updated_at=now() WHERE EXCLUDED.floor_slot>current.floor_slot OR (EXCLUDED.floor_slot=current.floor_slot AND EXCLUDED.source_rank>=current.source_rank) RETURNING reserve,floor_slot,account_data_hash,state_valid) DELETE FROM %[2]s.reserve_confirmed_verifications verification USING advanced floor,%[2]s.reserve_current_states state WHERE verification.reserve=floor.reserve AND state.reserve=floor.reserve AND verification.state_event_id=state.state_event_id AND verification.account_data_hash=state.account_data_hash AND verification.verified_slot<=floor.floor_slot AND (NOT floor.state_valid OR verification.verified_slot=floor.floor_slot OR floor.floor_slot-verification.verified_slot>%[3]s) AND (NOT floor.state_valid OR floor.account_data_hash<>state.account_data_hash)`, floorLockSeed, s.schema, s.tolerance())
}

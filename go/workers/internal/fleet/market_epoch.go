package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	marketEpochFingerprintDomain        = "loyal-yield-market-epoch-envelope-v4"
	maximumSupportedCatalogAge          = 300 * time.Second
	maximumReserveEconomicSlotLag int64 = 1_500
	reserveEconomicMillisPerSlot  int64 = 250
	minimumUsableEpochLifetime          = 60 * time.Second
	minimumReserveSupplyUSDMicros int64 = 100_000_000_000
	stablecoinPriceUSDMicros      int64 = 1_000_000
	stablecoinDecimals            uint8 = 6
)

type SupportedReserveCatalogRow struct {
	Market        string    `json:"market"`
	LiquidityMint string    `json:"liquidityMint"`
	Reserve       string    `json:"reserve"`
	MarketName    *string   `json:"marketName"`
	Symbol        *string   `json:"symbol"`
	RiskBaskets   []string  `json:"riskBaskets"`
	Source        string    `json:"source"`
	FetchedAt     time.Time `json:"fetchedAt"`
}

// SupportedReserveStateRow is a reserve's latest confirmed row from the
// observer's stream (or its startup seed) in kamino.reserve_updates.
type SupportedReserveStateRow struct {
	StateEventID             int64     `json:"stateEventId"`
	AccountDataHash          string    `json:"accountDataHash"`
	StateObservedAt          time.Time `json:"stateObservedAt"`
	StateSlot                int64     `json:"stateSlot"`
	Source                   string    `json:"source"`
	SourceCommitment         string    `json:"sourceCommitment"`
	Reserve                  string    `json:"reserve"`
	Market                   *string   `json:"market"`
	MarketName               *string   `json:"marketName"`
	LiquidityMint            string    `json:"liquidityMint"`
	Symbol                   *string   `json:"symbol"`
	MintDecimals             int32     `json:"mintDecimals"`
	ReserveLastUpdateSlot    int64     `json:"reserveLastUpdateSlot"`
	ReserveLastUpdateStale   bool      `json:"reserveLastUpdateStale"`
	ReservePriceStatus       int16     `json:"reservePriceStatus"`
	AvailableAmount          float64   `json:"availableAmount"`
	BorrowedAmount           float64   `json:"borrowedAmount"`
	TotalSupplyAmount        float64   `json:"totalSupplyAmount"`
	MarketPriceUSD           float64   `json:"marketPriceUsd"`
	MarketPriceLastUpdatedTS int64     `json:"marketPriceLastUpdatedTs"`
	Utilization              float64   `json:"utilization"`
	BorrowAPY                float64   `json:"borrowApy"`
	SupplyAPY                float64   `json:"supplyApy"`
	// DepositLimit is the reserve config's deposit_limit as decimal text;
	// nil when the observation did not decode it.
	DepositLimit          *string `json:"depositLimit"`
	AvailableAmountBits   uint64  `json:"availableAmountBits,omitempty"`
	BorrowedAmountBits    uint64  `json:"borrowedAmountBits,omitempty"`
	TotalSupplyAmountBits uint64  `json:"totalSupplyAmountBits,omitempty"`
	MarketPriceUSDBits    uint64  `json:"marketPriceUsdBits,omitempty"`
	UtilizationBits       uint64  `json:"utilizationBits,omitempty"`
	BorrowAPYBits         uint64  `json:"borrowApyBits,omitempty"`
	SupplyAPYBits         uint64  `json:"supplyApyBits,omitempty"`
}

type SupportedReserveMarketSnapshot struct {
	CapturedAt    time.Time                    `json:"capturedAt"`
	Catalog       []SupportedReserveCatalogRow `json:"catalog"`
	ReserveStates []SupportedReserveStateRow   `json:"reserveStates"`
}

// MarketEvidenceStore reads the observer-owned catalog and each reserve's latest
// confirmed stream row. It is intentionally read-only: direct RPC observations
// must not invent monitor state_event_id values.
type MarketEvidenceStore struct {
	pool         *pgxpool.Pool
	schema       string
	enabledMints []string
}

// NewMarketEvidenceStoreFromPool borrows the runtime's read-only data pool.
func NewMarketEvidenceStoreFromPool(pool *pgxpool.Pool, schema string) (*MarketEvidenceStore, error) {
	if pool == nil || !validSQLIdentifier(schema) {
		return nil, fmt.Errorf("market evidence requires scoped pool and valid schema")
	}
	return &MarketEvidenceStore{pool: pool, schema: schema, enabledMints: append([]string(nil), earnStableMints...)}, nil
}

func (s *MarketEvidenceStore) SetEnabledMints(mints []string) error {
	if len(mints) == 0 {
		mints = earnStableMints
	}
	seen := map[string]bool{}
	for _, mint := range mints {
		if !isEarnStableMint(mint) || seen[mint] {
			return fmt.Errorf("enabled stable mints must be unique members of the Earn registry")
		}
		seen[mint] = true
	}
	s.enabledMints = append([]string(nil), mints...)
	return nil
}

func validSQLIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for index, r := range value {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || index > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func (s *MarketEvidenceStore) LoadSnapshot(ctx context.Context, enabledMints, riskBaskets []string) (SupportedReserveMarketSnapshot, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return SupportedReserveMarketSnapshot{}, err
	}
	defer tx.Rollback(ctx)
	var result SupportedReserveMarketSnapshot
	if err := tx.QueryRow(ctx, "SELECT transaction_timestamp()::timestamptz").Scan(&result.CapturedAt); err != nil {
		return result, err
	}
	catalogSQL := fmt.Sprintf(`
SELECT market, liquidity_mint, reserve, market_name, symbol, risk_baskets, source, fetched_at
FROM %s.supported_reserves
WHERE active=true AND liquidity_mint=ANY($1)
  AND (cardinality($2::text[])=0 OR EXISTS (SELECT 1 FROM unnest($2::text[]) basket WHERE basket=ANY(risk_baskets)))
ORDER BY liquidity_mint, market, reserve, fetched_at`, s.schema)
	rows, err := tx.Query(ctx, catalogSQL, enabledMints, riskBaskets)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var row SupportedReserveCatalogRow
		if err := rows.Scan(&row.Market, &row.LiquidityMint, &row.Reserve, &row.MarketName, &row.Symbol, &row.RiskBaskets, &row.Source, &row.FetchedAt); err != nil {
			rows.Close()
			return result, err
		}
		result.Catalog = append(result.Catalog, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()
	// reserve_current_states names each reserve's newest stored row; its
	// (reserve, observed_at, event_id) key reaches that row, never history.
	stateSQL := fmt.Sprintf(`
SELECT state.event_id, state.account_data_hash, state.observed_at, state.slot, state.source,
       state.source_commitment, state.reserve, state.market, state.market_name,
       state.liquidity_mint, state.symbol, state.mint_decimals, state.reserve_last_update_slot,
       state.reserve_last_update_stale, state.reserve_price_status, state.available_amount, state.borrowed_amount,
       state.total_supply_amount, state.market_price_usd, state.market_price_last_updated_ts, state.utilization,
       state.borrow_apy, state.supply_apy, NULLIF(state.snapshot->>'deposit_limit','')::NUMERIC(20,0)::text
FROM %[1]s.reserve_current_states current_state
JOIN %[1]s.reserve_updates state ON state.reserve=current_state.reserve AND state.observed_at=current_state.state_observed_at AND state.event_id=current_state.state_event_id
WHERE current_state.reserve IN (SELECT reserve FROM %[1]s.supported_reserves WHERE active AND liquidity_mint=ANY($1))`, s.schema)
	rows, err = tx.Query(ctx, stateSQL, enabledMints)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var row SupportedReserveStateRow
		if err := rows.Scan(&row.StateEventID, &row.AccountDataHash, &row.StateObservedAt, &row.StateSlot,
			&row.Source, &row.SourceCommitment, &row.Reserve, &row.Market, &row.MarketName, &row.LiquidityMint, &row.Symbol, &row.MintDecimals,
			&row.ReserveLastUpdateSlot, &row.ReserveLastUpdateStale, &row.ReservePriceStatus,
			&row.AvailableAmount, &row.BorrowedAmount, &row.TotalSupplyAmount, &row.MarketPriceUSD,
			&row.MarketPriceLastUpdatedTS, &row.Utilization, &row.BorrowAPY, &row.SupplyAPY, &row.DepositLimit); err != nil {
			return result, err
		}
		row.captureFloatBits()
		result.ReserveStates = append(result.ReserveStates, row)
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if err := tx.Commit(ctx); err != nil {
		return result, err
	}
	return result, nil
}

func (s *MarketEvidenceStore) LoadImmutableMarketEpoch(ctx context.Context) (ImmutableMarketEpoch, error) {
	// Rust builds one frontier for the complete code-owned Earn stablecoin
	// registry. Incomplete mints retain blockers while complete mints remain
	// routable, so the planner can safely cover same- and cross-mint fleets.
	enabledMints := append([]string(nil), s.enabledMints...)
	snapshot, err := s.LoadSnapshot(ctx, enabledMints, []string{"safe"})
	if err != nil {
		return ImmutableMarketEpoch{}, fmt.Errorf("load durable market evidence: %w", err)
	}
	return BuildImmutableMarketEpoch(snapshot, enabledMints)
}

func BuildImmutableMarketEpoch(snapshot SupportedReserveMarketSnapshot, enabledMints []string) (ImmutableMarketEpoch, error) {
	if snapshot.CapturedAt.IsZero() {
		return ImmutableMarketEpoch{}, fmt.Errorf("market snapshot captured_at is required")
	}
	enabledMints = sortedUnique(enabledMints)
	if len(enabledMints) == 0 {
		return ImmutableMarketEpoch{}, fmt.Errorf("at least one enabled mint is required")
	}
	catalog := append([]SupportedReserveCatalogRow(nil), snapshot.Catalog...)
	sort.Slice(catalog, func(i, j int) bool {
		left, right := catalog[i], catalog[j]
		if left.LiquidityMint != right.LiquidityMint {
			return left.LiquidityMint < right.LiquidityMint
		}
		if left.Market != right.Market {
			return left.Market < right.Market
		}
		if left.Reserve != right.Reserve {
			return left.Reserve < right.Reserve
		}
		return left.FetchedAt.Before(right.FetchedAt)
	})
	catalogFingerprint := catalogFingerprint(catalog)
	identityCounts := make(map[string]int)
	for _, row := range catalog {
		identityCounts[row.Reserve]++
	}
	statesByIdentity := make(map[string][]SupportedReserveStateRow)
	statesByReserve := make(map[string]bool)
	// The stream frontier is the newest of these rows: every stream update to
	// any of the reserves moves it, and the stream would have delivered any
	// later change, so each latest row is current up to the frontier.
	var frontier SupportedReserveStateRow
	for _, row := range snapshot.ReserveStates {
		row.restoreFloatBits()
		if row.StateSlot > frontier.StateSlot {
			frontier = row
		}
		statesByReserve[row.Reserve] = true
		if row.Market != nil {
			statesByIdentity[marketIdentity(row.Reserve, *row.Market, row.LiquidityMint)] = append(statesByIdentity[marketIdentity(row.Reserve, *row.Market, row.LiquidityMint)], row)
		}
	}

	publicationMinimum := snapshot.CapturedAt.Add(minimumUsableEpochLifetime)
	var reserves []MarketEpochReserve
	var coverages []MarketMintCoverage
	var routableCatalogExpiries, routableEpochExpiries []time.Time
	for _, mint := range enabledMints {
		var mintCatalog []SupportedReserveCatalogRow
		for _, row := range catalog {
			if row.LiquidityMint == mint {
				mintCatalog = append(mintCatalog, row)
			}
		}
		coverage := MarketMintCoverage{Mint: mint, CatalogReserveCount: len(mintCatalog), Blockers: []MarketMintBlocker{}}
		mintWideBlocked := false
		if len(mintCatalog) == 0 {
			mintWideBlocked = true
			coverage.Blockers = append(coverage.Blockers, blocker("missing_catalog", nil, "active safe catalog has no reserve for enabled mint "+mint))
		}
		if !isEarnStableMint(mint) {
			mintWideBlocked = true
			coverage.Blockers = append(coverage.Blockers, blocker("missing_stable_valuation", nil, "enabled mint "+mint+" has no code-owned stable valuation"))
		}
		var candidates []MarketEpochReserve
		var catalogExpiries, economicExpiries []time.Time
		for _, catalogRow := range mintCatalog {
			reserveID := stringPointer(catalogRow.Reserve)
			var hard []MarketMintBlocker
			catalogExpiry := catalogRow.FetchedAt.Add(maximumSupportedCatalogAge)
			if catalogRow.Source != "kamino-api" {
				hard = append(hard, blocker("catalog_source_mismatch", reserveID, "catalog source is "+catalogRow.Source))
			}
			if catalogRow.FetchedAt.After(snapshot.CapturedAt) {
				hard = append(hard, blocker("catalog_fetched_in_future", reserveID, "catalog fetched_at "+rustDisplayTime(catalogRow.FetchedAt)+" is in the future"))
			} else if !catalogExpiry.After(snapshot.CapturedAt) {
				hard = append(hard, blocker("catalog_stale", reserveID, "catalog expired at "+rustDisplayTime(catalogExpiry)))
			} else if !catalogExpiry.After(publicationMinimum) {
				hard = append(hard, blocker("catalog_insufficient_lifetime", reserveID, fmt.Sprintf("catalog expires at %s; remaining lifetime is below %d seconds", rustDisplayTime(catalogExpiry), int64(minimumUsableEpochLifetime/time.Second))))
			}
			if identityCounts[catalogRow.Reserve] != 1 {
				hard = append(hard, blocker("duplicate_catalog_reserve_identity", reserveID, "reserve identity appears more than once in the active safe enabled catalog"))
			}
			matches := statesByIdentity[marketIdentity(catalogRow.Reserve, catalogRow.Market, catalogRow.LiquidityMint)]
			if len(matches) == 0 {
				code := "missing_reserve_state"
				if statesByReserve[catalogRow.Reserve] {
					code = "reserve_state_identity_mismatch"
				}
				coverage.Blockers = append(coverage.Blockers, append(hard, blocker(code, reserveID, "catalog identity has no exact latest confirmed row in reserve_updates"))...)
				continue
			}
			if len(matches) != 1 {
				coverage.Blockers = append(coverage.Blockers, append(hard, blocker("duplicate_reserve_state_identity", reserveID, fmt.Sprintf("exact reserve identity returned %d rows", len(matches))))...)
				continue
			}
			coverage.VerifiedReserveCount++
			exact := matches[0]
			refreshable := []MarketMintBlocker{}
			economicExpiry, economicTarget := validateReserveState(exact, catalogRow, frontier.StateSlot, frontier.StateObservedAt, publicationMinimum, &hard, &refreshable)
			coverage.Blockers = append(coverage.Blockers, refreshable...)
			if len(hard) != 0 {
				coverage.Blockers = append(coverage.Blockers, hard...)
				continue
			}
			totalSupplyUSD := int64(math.Round(exact.TotalSupplyAmount))
			targetEligible := economicTarget && totalSupplyUSD > minimumReserveSupplyUSDMicros && exact.SupplyAPY >= 0 && exact.SupplyAPY < .5
			catalogExpiries = append(catalogExpiries, catalogExpiry)
			if economicTarget {
				economicExpiries = append(economicExpiries, economicExpiry)
			}
			candidates = append(candidates, MarketEpochReserve{
				StateEventID: exact.StateEventID, AccountDataHash: exact.AccountDataHash,
				StateObservedAt: exact.StateObservedAt.UTC(), StateSlot: exact.StateSlot,
				VerificationCommitment: exact.SourceCommitment, Reserve: exact.Reserve, Market: exact.Market,
				LiquidityMint: exact.LiquidityMint, MintDecimals: stablecoinDecimals,
				MarketPriceUSDMicros: stablecoinPriceUSDMicros, ReserveLastUpdateSlot: exact.ReserveLastUpdateSlot,
				EconomicSlotLag: frontier.StateSlot - exact.ReserveLastUpdateSlot, EconomicExpiresAt: economicExpiry.UTC(),
				ReserveLastUpdateStale: exact.ReserveLastUpdateStale, ReservePriceStatus: exact.ReservePriceStatus,
				MarketPriceLastUpdatedTS: exact.MarketPriceLastUpdatedTS,
				AvailableAmountRaw:       canonicalFloat(exact.AvailableAmount), BorrowedAmountRaw: canonicalFloat(exact.BorrowedAmount),
				TotalSupplyAmountRaw: canonicalFloat(exact.TotalSupplyAmount), DepositLimitRaw: exact.DepositLimit,
				UtilizationPPM: int64(math.Round(exact.Utilization * 1_000_000)),
				BorrowAPYBPS:   int64(math.Round(exact.BorrowAPY * 10_000)), ObservedAt: exact.StateObservedAt.UTC(), Slot: exact.StateSlot,
				SupplyAPYBPS: int64(math.Round(exact.SupplyAPY * 10_000)), TotalSupplyUSDMicros: totalSupplyUSD,
				TargetEligible: targetEligible,
			})
		}
		for _, reserve := range candidates {
			if reserve.TargetEligible {
				coverage.EligibleTargetReserveCount++
			}
		}
		if coverage.EligibleTargetReserveCount == 0 {
			mintWideBlocked = true
			coverage.Blockers = append(coverage.Blockers, blocker("no_eligible_target", nil, "admissible catalog subset contains no reserve inside target safety bounds"))
		}
		sortBlockers(coverage.Blockers)
		coverage.Blockers = deduplicateBlockers(coverage.Blockers)
		coverage.Complete = !mintWideBlocked && len(mintCatalog) != 0
		catalogMinimum, hasCatalog := minimumTime(catalogExpiries)
		mintExpiry, hasMintExpiry := minimumTime(append(catalogExpiries, economicExpiries...))
		if coverage.Complete && hasMintExpiry {
			value := mintExpiry.UTC()
			coverage.ExpiresAt = &value
			reserves = append(reserves, candidates...)
			routableEpochExpiries = append(routableEpochExpiries, value)
		}
		if coverage.Complete && hasCatalog {
			routableCatalogExpiries = append(routableCatalogExpiries, catalogMinimum)
		}
		coverages = append(coverages, coverage)
	}
	sort.Slice(reserves, func(i, j int) bool {
		if reserves[i].LiquidityMint != reserves[j].LiquidityMint {
			return reserves[i].LiquidityMint < reserves[j].LiquidityMint
		}
		if reserves[i].Reserve != reserves[j].Reserve {
			return reserves[i].Reserve < reserves[j].Reserve
		}
		return pointerValue(reserves[i].Market) < pointerValue(reserves[j].Market)
	})
	sort.Slice(coverages, func(i, j int) bool { return coverages[i].Mint < coverages[j].Mint })
	catalogExpiresAt := snapshot.CapturedAt
	if value, ok := minimumTime(routableCatalogExpiries); ok {
		catalogExpiresAt = value
	}
	expiresAt := snapshot.CapturedAt
	if value, ok := minimumTime(routableEpochExpiries); ok {
		expiresAt = value
	}
	fingerprint := epochFingerprint(reserves, enabledMints, catalogFingerprint, coverages)
	epoch := ImmutableMarketEpoch{
		OptimizerEpochID: positiveEpochID(fingerprint), Fingerprint: fingerprint, CatalogFingerprint: catalogFingerprint,
		CapturedAt: snapshot.CapturedAt.UTC(), ExpiresAt: expiresAt.UTC(), CatalogExpiresAt: catalogExpiresAt.UTC(),
		CatalogReserveCount: len(catalog), MintCoverage: coverages, Reserves: reserves,
		StreamFrontierSlot: frontier.StateSlot,
	}
	for _, reserve := range reserves {
		setTimeBounds(&epoch.OldestMarketObservedAt, &epoch.NewestMarketObservedAt, reserve.ObservedAt)
		setIntBounds(&epoch.MinimumMarketSlot, &epoch.MaximumMarketSlot, reserve.Slot)
	}
	return epoch, nil
}

// validateReserveState returns the reserve's economic expiry and whether its
// economics may serve as a target; a reserve that may not has no expiry. The economic lag runs from the reserve's
// last refresh to the stream frontier, up to which its row is current.
func validateReserveState(exact SupportedReserveStateRow, catalog SupportedReserveCatalogRow, frontierSlot int64, frontierObservedAt, publicationMinimum time.Time, hard, refreshable *[]MarketMintBlocker) (time.Time, bool) {
	reserve := stringPointer(catalog.Reserve)
	// http_confirmed_refresh rows come from the retired verifying observer.
	if exact.Source != "laserstream_grpc" && exact.Source != "http_snapshot" && exact.Source != "http_confirmed_refresh" {
		*hard = append(*hard, blocker("source_mismatch", reserve, "reserve state source is "+exact.Source))
	}
	if exact.SourceCommitment != "confirmed" {
		*hard = append(*hard, blocker("commitment_mismatch", reserve, "reserve state commitment is "+exact.SourceCommitment))
	}
	if exact.StateEventID <= 0 || len(exact.AccountDataHash) != 64 || !isHex(exact.AccountDataHash) || exact.StateSlot < 0 || frontierSlot < exact.StateSlot {
		*hard = append(*hard, blocker("invalid_state_identity", reserve, fmt.Sprintf("invalid event/hash/state/frontier coordinates event=%d state_slot=%d frontier_slot=%d", exact.StateEventID, exact.StateSlot, frontierSlot)))
	}
	if exact.MintDecimals != int32(stablecoinDecimals) {
		*hard = append(*hard, blocker("mint_decimals_mismatch", reserve, fmt.Sprintf("reserve mint decimals %d differ from code-owned %d", exact.MintDecimals, stablecoinDecimals)))
	}
	if exact.ReserveLastUpdateStale {
		*refreshable = append(*refreshable, blocker("explicit_stale_economics", reserve, "reserve last_update.stale is set; admitted as refresh-before-withdraw source only"))
	}
	var expiry time.Time
	if exact.ReserveLastUpdateSlot < 0 || exact.ReserveLastUpdateSlot > exact.StateSlot {
		*hard = append(*hard, blocker("invalid_economic_slot_order", reserve, fmt.Sprintf("last_update_slot=%d state_slot=%d frontier_slot=%d", exact.ReserveLastUpdateSlot, exact.StateSlot, frontierSlot)))
	} else {
		lag := frontierSlot - exact.ReserveLastUpdateSlot
		if lag > maximumReserveEconomicSlotLag {
			*refreshable = append(*refreshable, blocker("economic_slot_lag_exceeded", reserve, fmt.Sprintf("economic slot lag %d exceeds %d; admitted as refresh-before-withdraw source only", lag, maximumReserveEconomicSlotLag)))
		} else {
			lifetime := frontierObservedAt.UTC().Add(time.Duration((maximumReserveEconomicSlotLag-lag)*reserveEconomicMillisPerSlot) * time.Millisecond)
			if !lifetime.After(publicationMinimum) {
				*refreshable = append(*refreshable, blocker("economic_insufficient_lifetime", reserve, fmt.Sprintf("economic evidence expires at %s; remaining lifetime is below %d seconds; admitted as refresh-before-withdraw source only", rustDisplayTime(lifetime), int64(minimumUsableEpochLifetime/time.Second))))
			} else if !exact.ReserveLastUpdateStale {
				expiry = lifetime
			}
		}
	}
	if !finite(exact.AvailableAmount) || exact.AvailableAmount < 0 || !finite(exact.BorrowedAmount) || exact.BorrowedAmount < 0 || !finite(exact.TotalSupplyAmount) || exact.TotalSupplyAmount <= 0 || !finite(exact.MarketPriceUSD) || !finite(exact.Utilization) || exact.Utilization < 0 || exact.Utilization > 1.000001 || !finite(exact.BorrowAPY) || exact.BorrowAPY < 0 || !finite(exact.SupplyAPY) || exact.SupplyAPY < 0 || exact.SupplyAPY >= .5 {
		*hard = append(*hard, blocker("invalid_economic_fields", reserve, fmt.Sprintf("invalid available=%s borrowed=%s total_supply=%s utilization=%s borrow_apy=%s supply_apy=%s", canonicalFloat(exact.AvailableAmount), canonicalFloat(exact.BorrowedAmount), canonicalFloat(exact.TotalSupplyAmount), canonicalFloat(exact.Utilization), canonicalFloat(exact.BorrowAPY), canonicalFloat(exact.SupplyAPY))))
	}
	if exact.DepositLimit != nil {
		if _, ok := parseDepositLimit(*exact.DepositLimit); !ok {
			*hard = append(*hard, blocker("invalid_economic_fields", reserve, "invalid deposit_limit="+*exact.DepositLimit))
		}
	}
	return expiry, !expiry.IsZero()
}

func (r *SupportedReserveStateRow) captureFloatBits() {
	r.AvailableAmountBits = math.Float64bits(r.AvailableAmount)
	r.BorrowedAmountBits = math.Float64bits(r.BorrowedAmount)
	r.TotalSupplyAmountBits = math.Float64bits(r.TotalSupplyAmount)
	r.MarketPriceUSDBits = math.Float64bits(r.MarketPriceUSD)
	r.UtilizationBits = math.Float64bits(r.Utilization)
	r.BorrowAPYBits = math.Float64bits(r.BorrowAPY)
	r.SupplyAPYBits = math.Float64bits(r.SupplyAPY)
}

func (r *SupportedReserveStateRow) restoreFloatBits() {
	if r.AvailableAmountBits != 0 {
		r.AvailableAmount = math.Float64frombits(r.AvailableAmountBits)
	}
	if r.BorrowedAmountBits != 0 {
		r.BorrowedAmount = math.Float64frombits(r.BorrowedAmountBits)
	}
	if r.TotalSupplyAmountBits != 0 {
		r.TotalSupplyAmount = math.Float64frombits(r.TotalSupplyAmountBits)
	}
	if r.MarketPriceUSDBits != 0 {
		r.MarketPriceUSD = math.Float64frombits(r.MarketPriceUSDBits)
	}
	if r.UtilizationBits != 0 {
		r.Utilization = math.Float64frombits(r.UtilizationBits)
	}
	if r.BorrowAPYBits != 0 {
		r.BorrowAPY = math.Float64frombits(r.BorrowAPYBits)
	}
	if r.SupplyAPYBits != 0 {
		r.SupplyAPY = math.Float64frombits(r.SupplyAPYBits)
	}
}

// parseDepositLimit reads a KLend deposit_limit: a canonical u64 decimal.
func parseDepositLimit(text string) (uint64, bool) {
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil || strconv.FormatUint(value, 10) != text {
		return 0, false
	}
	return value, true
}

// parseAvailableLiquidity reads the epoch's available_amount, a KLend u64
// stored as a float. Only an integral value float64 holds exactly is evidence;
// anything else is unknown, not rounded.
func parseAvailableLiquidity(text string) (int64, bool) {
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || !finite(value) || value < 0 || value > 1<<53 || value != math.Trunc(value) {
		return 0, false
	}
	return int64(value), true
}

func catalogFingerprint(rows []SupportedReserveCatalogRow) string {
	h := sha256.New()
	for _, row := range rows {
		hashPart(h, []byte(row.Market))
		hashPart(h, []byte(row.LiquidityMint))
		hashPart(h, []byte(row.Reserve))
		hashPart(h, []byte(pointerValue(row.MarketName)))
		hashPart(h, []byte(pointerValue(row.Symbol)))
		baskets := append([]string(nil), row.RiskBaskets...)
		sort.Strings(baskets)
		for _, basket := range baskets {
			hashPart(h, []byte(basket))
		}
		hashPart(h, []byte(row.Source))
		hashPart(h, littleEndianInt64(row.FetchedAt.UnixMicro()))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func epochFingerprint(reserves []MarketEpochReserve, enabledMints []string, catalog string, coverages []MarketMintCoverage) string {
	h := sha256.New()
	hashPart(h, []byte(marketEpochFingerprintDomain))
	hashPart(h, []byte(catalog))
	for _, mint := range enabledMints {
		hashPart(h, []byte(mint))
	}
	for _, coverage := range coverages {
		hashPart(h, []byte(coverage.Mint))
		hashPart(h, littleEndianUint64(uint64(coverage.CatalogReserveCount)))
		hashPart(h, littleEndianUint64(uint64(coverage.VerifiedReserveCount)))
		hashPart(h, littleEndianUint64(uint64(coverage.EligibleTargetReserveCount)))
		hashPart(h, []byte{boolByte(coverage.Complete)})
		expiry := int64(0)
		if coverage.ExpiresAt != nil {
			expiry = coverage.ExpiresAt.UnixMicro()
		}
		hashPart(h, littleEndianInt64(expiry))
		for _, item := range coverage.Blockers {
			hashPart(h, []byte{blockerRank(item.Code)})
			hashPart(h, []byte(pointerValue(item.Reserve)))
			hashPart(h, []byte(item.Detail))
		}
	}
	for _, reserve := range reserves {
		hashPart(h, littleEndianInt64(reserve.StateEventID))
		hashPart(h, []byte(reserve.AccountDataHash))
		hashPart(h, littleEndianInt64(reserve.StateObservedAt.UnixMicro()))
		hashPart(h, littleEndianInt64(reserve.StateSlot))
		hashPart(h, []byte(reserve.VerificationCommitment))
		hashPart(h, []byte(reserve.Reserve))
		hashPart(h, []byte(pointerValue(reserve.Market)))
		hashPart(h, []byte(reserve.LiquidityMint))
		hashPart(h, []byte{reserve.MintDecimals})
		hashPart(h, littleEndianInt64(reserve.MarketPriceUSDMicros))
		hashPart(h, littleEndianInt64(reserve.ReserveLastUpdateSlot))
		hashPart(h, littleEndianInt64(reserve.EconomicSlotLag))
		hashPart(h, littleEndianInt64(reserve.EconomicExpiresAt.UnixMicro()))
		hashPart(h, []byte{boolByte(reserve.ReserveLastUpdateStale)})
		hashPart(h, littleEndianInt16(reserve.ReservePriceStatus))
		hashPart(h, littleEndianInt64(reserve.MarketPriceLastUpdatedTS))
		hashPart(h, []byte(reserve.AvailableAmountRaw))
		hashPart(h, []byte(reserve.BorrowedAmountRaw))
		hashPart(h, []byte(reserve.TotalSupplyAmountRaw))
		// An absent limit hashes as the empty part; a present one is at
		// least one digit, so the two never collide.
		hashPart(h, []byte(pointerValue(reserve.DepositLimitRaw)))
		hashPart(h, littleEndianInt64(reserve.UtilizationPPM))
		hashPart(h, littleEndianInt64(reserve.BorrowAPYBPS))
		hashPart(h, littleEndianInt64(reserve.ObservedAt.UnixMicro()))
		hashPart(h, littleEndianInt64(reserve.Slot))
		hashPart(h, littleEndianInt64(reserve.SupplyAPYBPS))
		hashPart(h, littleEndianInt64(reserve.TotalSupplyUSDMicros))
		hashPart(h, []byte{boolByte(reserve.TargetEligible)})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashPart(h interface{ Write([]byte) (int, error) }, value []byte) {
	_, _ = h.Write(littleEndianUint64(uint64(len(value))))
	_, _ = h.Write(value)
}
func littleEndianUint64(value uint64) []byte {
	result := make([]byte, 8)
	binary.LittleEndian.PutUint64(result, value)
	return result
}
func littleEndianInt64(value int64) []byte { return littleEndianUint64(uint64(value)) }
func littleEndianInt16(value int16) []byte {
	result := make([]byte, 2)
	binary.LittleEndian.PutUint16(result, uint16(value))
	return result
}
func positiveEpochID(fingerprint string) int64 {
	value := uint64(0xcbf29ce484222325)
	for _, b := range []byte(fingerprint) {
		value ^= uint64(b)
		value *= 0x100000001b3
	}
	value &= math.MaxInt64
	if value == 0 {
		return 1
	}
	return int64(value)
}
func canonicalFloat(value float64) string {
	base := strconv.FormatFloat(value, 'f', -1, 64)
	if value < 0 || strings.ContainsAny(base, "eE") {
		return base
	}
	exact := new(big.Rat).SetFloat64(value)
	best, bestDistance := base, decimalDistance(base, exact)
	for _, delta := range []int64{-1, 1} {
		candidate, ok := adjacentFixedDecimal(base, delta)
		if !ok {
			continue
		}
		parsed, err := strconv.ParseFloat(candidate, 64)
		if err != nil || math.Float64bits(parsed) != math.Float64bits(value) {
			continue
		}
		distance := decimalDistance(candidate, exact)
		comparison := distance.Cmp(bestDistance)
		if comparison < 0 || comparison == 0 && candidate > best {
			best, bestDistance = candidate, distance
		}
	}
	return best
}

func decimalDistance(value string, exact *big.Rat) *big.Rat {
	candidate := new(big.Rat)
	if _, ok := candidate.SetString(value); !ok {
		return new(big.Rat).SetInt64(math.MaxInt64)
	}
	return new(big.Rat).Abs(new(big.Rat).Sub(candidate, exact))
}

func adjacentFixedDecimal(value string, delta int64) (string, bool) {
	parts := strings.Split(value, ".")
	decimals := 0
	digits := value
	if len(parts) == 2 {
		decimals = len(parts[1])
		digits = parts[0] + parts[1]
	} else if len(parts) != 1 {
		return "", false
	}
	integer := new(big.Int)
	if _, ok := integer.SetString(digits, 10); !ok {
		return "", false
	}
	integer.Add(integer, big.NewInt(delta))
	if integer.Sign() < 0 {
		return "", false
	}
	result := integer.String()
	if decimals == 0 {
		return result, true
	}
	for len(result) <= decimals {
		result = "0" + result
	}
	return result[:len(result)-decimals] + "." + result[len(result)-decimals:], true
}
func rustDisplayTime(value time.Time) string {
	value = value.UTC()
	layout := "2006-01-02 15:04:05 UTC"
	if value.Nanosecond() != 0 {
		switch {
		case value.Nanosecond()%1_000_000 == 0:
			layout = "2006-01-02 15:04:05.000 UTC"
		case value.Nanosecond()%1_000 == 0:
			layout = "2006-01-02 15:04:05.000000 UTC"
		default:
			layout = "2006-01-02 15:04:05.000000000 UTC"
		}
	}
	return value.Format(layout)
}
func marketIdentity(reserve, market, mint string) string {
	return reserve + "\x00" + market + "\x00" + mint
}
func stringPointer(value string) *string { result := value; return &result }
func pointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
func blocker(code string, reserve *string, detail string) MarketMintBlocker {
	return MarketMintBlocker{Code: code, Reserve: reserve, Detail: detail}
}
func boolByte(value bool) byte {
	if value {
		return 1
	}
	return 0
}
func isHex(value string) bool { _, err := hex.DecodeString(value); return err == nil }
func sortedUnique(values []string) []string {
	seen := map[string]bool{}
	var result []string
	for _, v := range values {
		if !seen[v] {
			seen[v] = true
			result = append(result, v)
		}
	}
	sort.Strings(result)
	return result
}
func minimumTime(values []time.Time) (time.Time, bool) {
	if len(values) == 0 {
		return time.Time{}, false
	}
	result := values[0]
	for _, v := range values[1:] {
		if v.Before(result) {
			result = v
		}
	}
	return result, true
}
func setTimeBounds(minimum, maximum **time.Time, value time.Time) {
	value = value.UTC()
	if *minimum == nil || value.Before(**minimum) {
		v := value
		*minimum = &v
	}
	if *maximum == nil || value.After(**maximum) {
		v := value
		*maximum = &v
	}
}
func setIntBounds(minimum, maximum **int64, value int64) {
	if *minimum == nil || value < **minimum {
		v := value
		*minimum = &v
	}
	if *maximum == nil || value > **maximum {
		v := value
		*maximum = &v
	}
}
func sortBlockers(values []MarketMintBlocker) {
	sort.Slice(values, func(i, j int) bool {
		if values[i].Code != values[j].Code {
			return blockerRank(values[i].Code) < blockerRank(values[j].Code)
		}
		if pointerValue(values[i].Reserve) != pointerValue(values[j].Reserve) {
			return pointerValue(values[i].Reserve) < pointerValue(values[j].Reserve)
		}
		return values[i].Detail < values[j].Detail
	})
}
func deduplicateBlockers(values []MarketMintBlocker) []MarketMintBlocker {
	result := values[:0]
	var previous string
	for _, v := range values {
		key := v.Code + "\x00" + pointerValue(v.Reserve) + "\x00" + v.Detail
		if len(result) == 0 || key != previous {
			result = append(result, v)
			previous = key
		}
	}
	return result
}
func blockerRank(code string) byte {
	for index, value := range []string{"missing_catalog", "catalog_source_mismatch", "catalog_fetched_in_future", "catalog_stale", "catalog_insufficient_lifetime", "duplicate_catalog_reserve_identity", "duplicate_reserve_state_identity", "missing_reserve_state", "reserve_state_identity_mismatch", "source_mismatch", "commitment_mismatch", "invalid_state_identity", "missing_stable_valuation", "mint_decimals_mismatch", "explicit_stale_economics", "invalid_economic_slot_order", "economic_slot_lag_exceeded", "economic_insufficient_lifetime", "invalid_economic_fields", "no_eligible_target"} {
		if code == value {
			return byte(index)
		}
	}
	panic("unknown market blocker code: " + code)
}

// VerifyDirectObservation binds the monitor-owned state identity to the direct
// RPC bytes used by this process. A monitor lag or a changed account fails
// closed and waits for a newer confirmed row.
func (e ImmutableMarketEpoch) DurableEvidence() ImmutableMarketEpoch {
	result := e
	if e.NewestMarketObservedAt != nil {
		result.CapturedAt = e.NewestMarketObservedAt.UTC()
	}
	return result
}

// RoutableReserveAddresses validates the epoch's per-mint admission boundary.
// Complete mint coverages contribute their admissible reserve subset; blocked
// mints remain fingerprinted evidence but must contribute no RPC/planning
// addresses. The global catalog count intentionally includes both classes.
func (e ImmutableMarketEpoch) RoutableReserveAddresses() ([]string, error) {
	coverages := make(map[string]MarketMintCoverage, len(e.MintCoverage))
	catalogCount := 0
	for _, coverage := range e.MintCoverage {
		if coverage.Mint == "" || coverage.CatalogReserveCount < 0 || coverage.VerifiedReserveCount < 0 || coverage.EligibleTargetReserveCount < 0 {
			return nil, fmt.Errorf("immutable market epoch has invalid mint coverage")
		}
		if _, duplicate := coverages[coverage.Mint]; duplicate {
			return nil, fmt.Errorf("immutable market epoch has duplicate coverage for mint %s", coverage.Mint)
		}
		coverages[coverage.Mint] = coverage
		catalogCount += coverage.CatalogReserveCount
	}
	if catalogCount != e.CatalogReserveCount {
		return nil, fmt.Errorf("immutable market epoch catalog count disagrees with per-mint coverage")
	}
	addresses := make([]string, 0, len(e.Reserves))
	reserveCounts := make(map[string]int, len(coverages))
	eligibleCounts := make(map[string]int, len(coverages))
	seenAddresses := make(map[string]bool, len(e.Reserves))
	for _, reserve := range e.Reserves {
		coverage, exists := coverages[reserve.LiquidityMint]
		if !exists || !coverage.Complete {
			return nil, fmt.Errorf("reserve %s belongs to blocked or unknown mint %s", reserve.Reserve, reserve.LiquidityMint)
		}
		if reserve.Reserve == "" || seenAddresses[reserve.Reserve] {
			return nil, fmt.Errorf("immutable market epoch has empty or duplicate reserve identity")
		}
		seenAddresses[reserve.Reserve] = true
		reserveCounts[reserve.LiquidityMint]++
		if reserve.TargetEligible {
			eligibleCounts[reserve.LiquidityMint]++
		}
		addresses = append(addresses, reserve.Reserve)
	}
	for _, coverage := range e.MintCoverage {
		count := reserveCounts[coverage.Mint]
		if !coverage.Complete {
			if count != 0 {
				return nil, fmt.Errorf("blocked mint %s contributed routable reserves", coverage.Mint)
			}
			continue
		}
		if coverage.CatalogReserveCount == 0 || coverage.ExpiresAt == nil || count == 0 || count > coverage.VerifiedReserveCount || coverage.VerifiedReserveCount > coverage.CatalogReserveCount {
			return nil, fmt.Errorf("complete mint %s has inconsistent reserve coverage", coverage.Mint)
		}
		if eligibleCounts[coverage.Mint] != coverage.EligibleTargetReserveCount || coverage.EligibleTargetReserveCount == 0 {
			return nil, fmt.Errorf("complete mint %s has inconsistent target coverage", coverage.Mint)
		}
	}
	return addresses, nil
}

func (e ImmutableMarketEpoch) Validate() error {
	if e.Fingerprint == "" || e.CatalogFingerprint == "" || e.OptimizerEpochID != positiveEpochID(e.Fingerprint) || len(e.Reserves) == 0 || len(e.MintCoverage) == 0 {
		return fmt.Errorf("immutable market epoch identity or frontier is incomplete")
	}
	if epochFingerprint(e.Reserves, sortedUnique(coverageMints(e.MintCoverage)), e.CatalogFingerprint, e.MintCoverage) != e.Fingerprint {
		return fmt.Errorf("immutable market epoch fingerprint disagrees with typed evidence")
	}
	if _, err := e.RoutableReserveAddresses(); err != nil {
		return err
	}
	if !e.OptimizerEnvelopeExpiresAt().After(e.CapturedAt) || e.MaximumMarketSlot == nil || *e.MaximumMarketSlot < 0 {
		return fmt.Errorf("immutable market epoch has no usable slot or lifetime")
	}
	return nil
}

func coverageMints(coverages []MarketMintCoverage) []string {
	result := make([]string, 0, len(coverages))
	for _, coverage := range coverages {
		result = append(result, coverage.Mint)
	}
	return result
}

// planDirectReads plans each reserve whose direct read differs from its
// stream row, at or after the row's slot, on the direct read: the chain at a
// known slot. The stream row still lists the catalog and stays the published
// epoch evidence, target eligibility included (sameMintTargets).
func planDirectReads(snapshot, direct MarketSnapshot) {
	for address, reserve := range direct.Reserves {
		if held, ok := snapshot.Reserves[address]; ok && reserve.Slot >= held.Slot && !strings.EqualFold(held.DataHash, reserve.DataHash) {
			reserve.ObservedAt = direct.ObservedAt
			snapshot.Reserves[address] = reserve
		}
	}
}

// DirectObservationHashMismatch distinguishes a changing chain observation
// from invalid identity, slot, or expiry evidence. It does not establish which
// observation is newer: a hash difference alone cannot prove ordering.
type DirectObservationHashMismatch struct {
	Reserve string
}

func (e *DirectObservationHashMismatch) Error() string {
	return fmt.Sprintf("reserve %s direct account hash differs from durable confirmed evidence", e.Reserve)
}

func (e ImmutableMarketEpoch) VerifyDirectObservation(snapshot MarketSnapshot, required ...string) error {
	// Each reserve's latest row is current only while the observer's stream
	// is live; a frontier that trails the chain means the rows may be stale.
	if behind := snapshot.Slot - e.StreamFrontierSlot; behind > maximumReserveEconomicSlotLag {
		return fmt.Errorf("Kamino stream frontier slot %d is %d slots behind confirmed slot %d", e.StreamFrontierSlot, behind, snapshot.Slot)
	}
	var mismatch *DirectObservationHashMismatch
	for _, address := range required {
		evidence, ok := e.Reserve(address)
		if !ok {
			return fmt.Errorf("reserve %s is absent from complete immutable epoch", address)
		}
		direct, ok := snapshot.Reserves[address]
		if !ok {
			return fmt.Errorf("reserve %s is absent from direct snapshot", address)
		}
		if evidence.Market == nil || *evidence.Market != direct.Market || evidence.LiquidityMint != direct.Mint || evidence.StateSlot > snapshot.Slot || evidence.StateSlot > direct.Slot {
			return fmt.Errorf("reserve %s direct identity or slot does not cover durable evidence", address)
		}
		if !strings.EqualFold(evidence.AccountDataHash, direct.DataHash) && mismatch == nil {
			mismatch = &DirectObservationHashMismatch{Reserve: address}
		}
		if snapshot.ObservedAt.After(e.OptimizerEnvelopeExpiresAt()) {
			return fmt.Errorf("immutable market epoch expired before direct observation")
		}
	}
	// Validate every required reserve before classifying a mismatch as harmless
	// for read-only shadow planning. Never hide a later identity/slot failure.
	if mismatch != nil {
		return mismatch
	}
	return nil
}

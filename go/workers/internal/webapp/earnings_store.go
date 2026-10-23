package webapp

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Postgres readers and writers behind the earnings read-service dependencies,
// ported from yield-deposit-repository.server.ts and the snapshot store in
// earnings-read-service.server.ts. All tables live in the loyal_yield schema.

const earningsSchema = "loyal_yield"

// loadEarningsSnapshot ports loadEarnEarningsSnapshot (read-service:665): one
// scoped row, private payload keys split back out, checkpoints older than
// version 1 dropped.
func loadEarningsSnapshot(ctx context.Context, pool *pgxpool.Pool, input earningsReadInput, timezone string) (*earningsSnapshotRecord, error) {
	const q = `SELECT payload, generated_at
FROM ` + earningsSchema + `.earn_earnings_snapshots
WHERE cluster = $1 AND wallet_address = $2 AND settings = $3 AND vault_index = $4 AND timezone = $5`
	var raw []byte
	var generatedAt time.Time
	err := pool.QueryRow(ctx, q,
		input.cluster, input.walletAddress, input.settings, input.vaultIndex, timezone,
	).Scan(&raw, &generatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	var stored storedEarningsSnapshot
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	record := &earningsSnapshotRecord{
		generatedAt:    msTime(generatedAt),
		payload:        stored.EarningsRangeSet,
		sourceRevision: stored.SourceRevision,
	}
	if stored.Checkpoint != nil && stored.Checkpoint.Version == 1 {
		record.checkpoint = stored.Checkpoint
	}
	return record, nil
}

// marshalEarningsPayload writes the stored payload the way JSON.stringify
// spread it in saveSnapshot: response fields in struct order, then the private
// keys only when present.
func marshalEarningsPayload(payload EarningsRangeSet, sourceRevision *string, checkpoint *earningsCheckpointWire) (string, error) {
	stored := storedEarningsSnapshot{
		EarningsRangeSet: payload,
		SourceRevision:   sourceRevision,
		Checkpoint:       checkpoint,
	}
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(stored); err != nil {
		return "", err
	}
	return string(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))), nil
}

// saveEarningsSnapshot ports saveSnapshot (read-service:712). The upsert is
// idempotent per scope and last-write-wins only forward in time: an older
// generated_at can never overwrite a newer row, so a double run or a retry
// after a crash leaves the newest payload intact.
func saveEarningsSnapshot(ctx context.Context, pool *pgxpool.Pool, args earningsSaveArgs) error {
	generatedAt := mustParseISO(args.payload.GeneratedAt)
	payloadJSON, err := marshalEarningsPayload(args.payload, args.sourceRevision, args.checkpoint)
	if err != nil {
		return err
	}
	const q = `INSERT INTO ` + earningsSchema + `.earn_earnings_snapshots
  (cluster, wallet_address, settings, vault_index, timezone,
   history_revision, principal_amount_raw, payload, generated_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)
ON CONFLICT (cluster, wallet_address, settings, vault_index, timezone) DO UPDATE SET
  cluster = EXCLUDED.cluster,
  wallet_address = EXCLUDED.wallet_address,
  settings = EXCLUDED.settings,
  vault_index = EXCLUDED.vault_index,
  timezone = EXCLUDED.timezone,
  history_revision = EXCLUDED.history_revision,
  principal_amount_raw = EXCLUDED.principal_amount_raw,
  payload = EXCLUDED.payload,
  generated_at = EXCLUDED.generated_at,
  updated_at = EXCLUDED.updated_at
WHERE ` + earningsSchema + `.earn_earnings_snapshots.generated_at <= EXCLUDED.generated_at`
	tag, err := pool.Exec(ctx, q,
		args.input.cluster, args.input.walletAddress, args.input.settings, args.input.vaultIndex, args.timezone,
		args.payload.HistoryRevision, args.principal, payloadJSON, generatedAt,
	)
	if err != nil {
		return err
	}
	_ = tag
	return nil
}

// loadEarningsSourceRevision ports getEarnEarningsSourceRevision
// (read-service:693): all position rows for the wallet scope in id order.
func loadEarningsSourceRevision(ctx context.Context, pool *pgxpool.Pool, input earningsReadInput) (string, error) {
	const q = `SELECT id, status, principal_amount_raw, last_holding_event_id, updated_at
FROM ` + earningsSchema + `.user_yield_positions
WHERE wallet_address = $1 AND settings = $2 AND vault_index = $3
ORDER BY id ASC`
	rows, err := pool.Query(ctx, q, input.walletAddress, input.settings, input.vaultIndex)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var revisionRows []earningsSourceRevisionRow
	for rows.Next() {
		var row earningsSourceRevisionRow
		var lastEvent *int64
		var updatedAt time.Time
		if err := rows.Scan(&row.ID, &row.Status, &row.Principal, &lastEvent, &updatedAt); err != nil {
			return "", err
		}
		row.LastEvent = lastEvent
		row.UpdatedAt = isoMS(msTime(updatedAt))
		revisionRows = append(revisionRows, row)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if revisionRows == nil {
		revisionRows = []earningsSourceRevisionRow{}
	}
	return getEarnEarningsSourceRevision(revisionRows), nil
}

// loadEarningsHistoryFingerprint ports loadEarningsHistoryFingerprint
// (earnings-checkpoint.server.ts:63): md5 over the per-day revision chain
// strictly before `before`.
func loadEarningsHistoryFingerprint(ctx context.Context, pool *pgxpool.Pool, input earningsReadInput, before time.Time) (string, error) {
	const q = `SELECT md5(coalesce(string_agg(history_day::text || ':' || revision::text, ',' ORDER BY history_day), ''))
FROM ` + earningsSchema + `.earn_history_day_revisions
WHERE settings = $1 AND vault_index = $2
  AND history_day < ($3::timestamptz AT TIME ZONE 'UTC')::date`
	var fingerprint string
	if err := pool.QueryRow(ctx, q, input.settings, input.vaultIndex, before).Scan(&fingerprint); err != nil {
		return "", err
	}
	return fingerprint, nil
}

// ---- Ledger events (findYieldPositionEvents, repository:3559) ----

type earningsSourceRow struct {
	id            int64
	confirmedSlot int64
	amountRaw     int64
	sourceType    *string
	confirmedAt   time.Time
	liquidityMint string
}

type earningsHoldingRef struct {
	id               int64
	positionID       int64
	eventType        string
	principalDelta   *int64
	sourceDeposit    *int64
	sourceWithdrawal *int64
}

// loadEarningsLedgerEvents replays deposits, withdrawals and their holding
// records into the sorted ledger the calculator consumes. Ambiguous holding
// links (two records for one cash event, or a record whose kind contradicts
// its source) refuse the ledger rather than guess a principal path.
func loadEarningsLedgerEvents(ctx context.Context, pool *pgxpool.Pool, input earningsReadInput) ([]earningsEvent, error) {
	const depositsQ = `SELECT id, confirmed_slot, principal_amount_raw, confirmed_at, liquidity_mint
FROM ` + earningsSchema + `.user_yield_position_deposits
WHERE settings = $1 AND vault_index = $2 AND wallet_address = $3
ORDER BY confirmed_at ASC`
	const withdrawalsQ = `SELECT id, confirmed_slot, withdrawn_amount_raw, source_type, confirmed_at, liquidity_mint
FROM ` + earningsSchema + `.user_yield_position_withdrawals
WHERE settings = $1 AND vault_index = $2 AND wallet_address = $3
ORDER BY confirmed_at ASC`

	deposits, err := scanEarningsSourceRows(ctx, pool, depositsQ, input, false)
	if err != nil {
		return nil, err
	}
	withdrawals, err := scanEarningsSourceRows(ctx, pool, withdrawalsQ, input, true)
	if err != nil {
		return nil, err
	}

	depositsBySource, withdrawalsBySource := map[int64][]earningsHoldingRef{}, map[int64][]earningsHoldingRef{}
	appendRef := func(index map[int64][]earningsHoldingRef, id *int64, ref earningsHoldingRef) {
		if id != nil {
			index[*id] = append(index[*id], ref)
		}
	}
	scanHoldings := func(byDeposit bool) error {
		var q string
		if byDeposit {
			q = `SELECT id, position_id, event_type, principal_delta_raw, source_deposit_id, source_withdrawal_id
FROM ` + earningsSchema + `.user_yield_position_holding_events
WHERE source_deposit_id = ANY($1::bigint[])`
		} else {
			q = `SELECT id, position_id, event_type, principal_delta_raw, source_deposit_id, source_withdrawal_id
FROM ` + earningsSchema + `.user_yield_position_holding_events
WHERE source_withdrawal_id = ANY($1::bigint[])`
		}
		ids := make([]int64, 0, 64)
		if byDeposit {
			for _, row := range deposits {
				ids = append(ids, row.id)
			}
		} else {
			for _, row := range withdrawals {
				ids = append(ids, row.id)
			}
		}
		if len(ids) == 0 {
			return nil
		}
		rows, err := pool.Query(ctx, q, ids)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ref earningsHoldingRef
			if err := rows.Scan(&ref.id, &ref.positionID, &ref.eventType, &ref.principalDelta, &ref.sourceDeposit, &ref.sourceWithdrawal); err != nil {
				return err
			}
			if byDeposit {
				appendRef(depositsBySource, ref.sourceDeposit, ref)
			} else {
				appendRef(withdrawalsBySource, ref.sourceWithdrawal, ref)
			}
		}
		return rows.Err()
	}
	if err := scanHoldings(true); err != nil {
		return nil, err
	}
	if err := scanHoldings(false); err != nil {
		return nil, err
	}

	metadata := func(matches []earningsHoldingRef, eventType string, sourceType *string) (holdingEventID, positionID *int64, initializesPosition *bool, principalDeltaRaw *int64, err error) {
		if len(matches) == 0 {
			return
		}
		if len(matches) != 1 {
			return nil, nil, nil, nil, &earningsHistoryError{detail: "principal_history_position_ambiguous"}
		}
		holding := matches[0]
		isDeposit := holding.eventType == "deposit_initialized" || holding.eventType == "deposit_top_up"
		isWithdrawal := holding.eventType == "withdrawal_full" || holding.eventType == "withdrawal_partial"
		if (eventType == "deposit") != isDeposit || (eventType == "withdrawal" && !isWithdrawal) {
			return nil, nil, nil, nil, &earningsHistoryError{detail: "principal_history_position_ambiguous"}
		}
		if holding.sourceDeposit != nil && holding.sourceWithdrawal != nil {
			return nil, nil, nil, nil, &earningsHistoryError{detail: "principal_history_position_ambiguous"}
		}
		initializes := holding.eventType == "deposit_initialized"
		var delta *int64
		if sourceType == nil || *sourceType != "chain_snapshot" {
			delta = holding.principalDelta
		}
		return &holding.id, &holding.positionID, &initializes, delta, nil
	}

	const settlementsQ = `SELECT h.id, h.position_id, h.observed_at, h.observed_slot, h.liquidity_mint
FROM ` + earningsSchema + `.user_yield_position_holding_events h
JOIN ` + earningsSchema + `.user_yield_positions p ON p.id = h.position_id
WHERE p.settings = $1 AND p.wallet_address = $2 AND p.vault_index = $3
  AND h.event_type = 'snapshot_reconciled'
  AND h.amount_raw = 0
  AND h.principal_delta_raw IS NOT NULL
  AND h.source_deposit_id IS NULL
  AND h.source_withdrawal_id IS NULL
  AND (h.source_signature IS NOT NULL OR EXISTS (
    SELECT 1 FROM ` + earningsSchema + `.vault_position_snapshots s
    WHERE s.id = h.source_snapshot_id
      AND s.context->>'publication_scope' = 'complete_product_vault'
      AND COALESCE(s.context->>'idle_vault_liquidity_amount_raw', '0') = '0'
      AND NOT EXISTS (SELECT 1 FROM ` + earningsSchema + `.vault_position_snapshot_positions p2
                      WHERE p2.snapshot_id = s.id AND p2.amount_raw > 0)
  ))`
	settlementRows, err := pool.Query(ctx, settlementsQ, input.settings, input.walletAddress, input.vaultIndex)
	if err != nil {
		return nil, err
	}
	events := make([]earningsEvent, 0, len(deposits)+len(withdrawals)+8)
	for settlementRows.Next() {
		var id, positionID int64
		var confirmedAt time.Time
		var confirmedSlot int64
		var liquidityMint string
		if err := settlementRows.Scan(&id, &positionID, &confirmedAt, &confirmedSlot, &liquidityMint); err != nil {
			settlementRows.Close()
			return nil, err
		}
		events = append(events, earningsEvent{
			confirmedAt:         msTime(confirmedAt),
			confirmedSlot:       confirmedSlot,
			liquidityMint:       liquidityMint,
			holdingEventID:      &id,
			positionID:          &positionID,
			initializesPosition: earningsBool(false),
			settlesPosition:     earningsBool(true),
			eventType:           "withdrawal",
		})
	}
	if err := settlementRows.Err(); err != nil {
		settlementRows.Close()
		return nil, err
	}
	settlementRows.Close()

	for _, row := range deposits {
		holdingID, positionID, initializes, delta, err := metadata(depositsBySource[row.id], "deposit", nil)
		if err != nil {
			return nil, err
		}
		events = append(events, earningsEvent{
			amountRaw:           row.amountRaw,
			confirmedAt:         msTime(row.confirmedAt),
			confirmedSlot:       row.confirmedSlot,
			liquidityMint:       row.liquidityMint,
			holdingEventID:      holdingID,
			positionID:          positionID,
			initializesPosition: initializes,
			principalDeltaRaw:   delta,
			eventType:           "deposit",
		})
	}
	for _, row := range withdrawals {
		holdingID, positionID, initializes, delta, err := metadata(withdrawalsBySource[row.id], "withdrawal", row.sourceType)
		if err != nil {
			return nil, err
		}
		events = append(events, earningsEvent{
			amountRaw:           row.amountRaw,
			confirmedAt:         msTime(row.confirmedAt),
			confirmedSlot:       row.confirmedSlot,
			liquidityMint:       row.liquidityMint,
			holdingEventID:      holdingID,
			positionID:          positionID,
			initializesPosition: initializes,
			principalDeltaRaw:   delta,
			eventType:           "withdrawal",
		})
	}
	return sortEarningsEvents(events)
}

func scanEarningsSourceRows(ctx context.Context, pool *pgxpool.Pool, q string, input earningsReadInput, withdrawals bool) ([]earningsSourceRow, error) {
	rows, err := pool.Query(ctx, q, input.settings, input.vaultIndex, input.walletAddress)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []earningsSourceRow
	for rows.Next() {
		var row earningsSourceRow
		if withdrawals {
			if err := rows.Scan(&row.id, &row.confirmedSlot, &row.amountRaw, &row.sourceType, &row.confirmedAt, &row.liquidityMint); err != nil {
				return nil, err
			}
		} else {
			if err := rows.Scan(&row.id, &row.confirmedSlot, &row.amountRaw, &row.confirmedAt, &row.liquidityMint); err != nil {
				return nil, err
			}
		}
		row.confirmedAt = msTime(row.confirmedAt)
		out = append(out, row)
	}
	return out, rows.Err()
}

// ---- Positions and vault exposure snapshots ----

type earningsVaultRef struct {
	id          int64
	vaultPubkey string
}

// loadEarningsPositions ports findActiveYieldPositionsForVault
// (repository:2705): active rows for the wallet scope, newest-updated first.
func loadEarningsPositions(ctx context.Context, pool *pgxpool.Pool, input earningsReadInput) ([]earningsPosition, error) {
	const q = `SELECT id, initial_reserve, initial_market, initial_liquidity_mint, principal_amount_raw
FROM ` + earningsSchema + `.user_yield_positions
WHERE settings = $1 AND vault_index = $2 AND wallet_address = $3 AND status = 'active'
ORDER BY updated_at DESC, id DESC`
	rows, err := pool.Query(ctx, q, input.settings, input.vaultIndex, input.walletAddress)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var positions []earningsPosition
	for rows.Next() {
		var position earningsPosition
		if err := rows.Scan(&position.id, &position.initialReserve, &position.initialMarket, &position.initialLiquidityMint, &position.principalAmountRaw); err != nil {
			return nil, err
		}
		positions = append(positions, position)
	}
	return positions, rows.Err()
}

type earningsSnapshotRow struct {
	id           int64
	observedAt   time.Time
	observedSlot int64
}

// loadCompleteEarningsVaultSnapshots ports
// findCompleteYieldVaultExposureSnapshots (repository:2725): the vault's
// complete-product snapshots with kamino position rows plus idle balances on
// the latest snapshot only.
func loadCompleteEarningsVaultSnapshots(ctx context.Context, pool *pgxpool.Pool, input earningsReadInput, observedSince *time.Time) ([]earningsPortfolioSnapshot, error) {
	positions, err := loadEarningsPositions(ctx, pool, input)
	if err != nil {
		return nil, err
	}
	if len(positions) == 0 {
		return nil, nil
	}
	var vaultRef earningsVaultRef
	err = pool.QueryRow(ctx,
		`SELECT v.id, v.vault_pubkey
FROM `+earningsSchema+`.managed_vaults v
JOIN `+earningsSchema+`.user_yield_positions p
  ON p.vault_pubkey = v.vault_pubkey AND p.settings = v.settings AND p.vault_index = v.vault_index
WHERE v.settings = $1 AND v.vault_index = $2 AND p.status = 'active'
ORDER BY p.updated_at DESC, p.id DESC
LIMIT 1`,
		input.settings, input.vaultIndex,
	).Scan(&vaultRef.id, &vaultRef.vaultPubkey)
	// TS keys the vault on the first active position's vaultPubkey; the join
	// above keeps that pin (newest-updated active position first).
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}

	snapshotQuery := `SELECT id, observed_at, observed_slot
FROM ` + earningsSchema + `.vault_position_snapshots
WHERE vault_id = $1
  AND context->>'publication_scope' = 'complete_product_vault'`
	args := []any{vaultRef.id}
	if observedSince != nil {
		snapshotQuery += ` AND observed_at >= $2::timestamptz`
		args = append(args, *observedSince)
	}
	snapshotQuery += ` ORDER BY observed_at ASC, observed_slot ASC, id ASC`
	rows, err := pool.Query(ctx, snapshotQuery, args...)
	if err != nil {
		return nil, err
	}
	var snapshotRows []earningsSnapshotRow
	for rows.Next() {
		var row earningsSnapshotRow
		if err := rows.Scan(&row.id, &row.observedAt, &row.observedSlot); err != nil {
			rows.Close()
			return nil, err
		}
		row.observedAt = msTime(row.observedAt)
		snapshotRows = append(snapshotRows, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(snapshotRows) == 0 {
		return nil, nil
	}
	// TS re-sorts by (observedSlot, id) after the SQL fetch.
	sort.SliceStable(snapshotRows, func(i, j int) bool {
		if snapshotRows[i].observedSlot != snapshotRows[j].observedSlot {
			return snapshotRows[i].observedSlot < snapshotRows[j].observedSlot
		}
		return snapshotRows[i].id < snapshotRows[j].id
	})

	positionsBySnapshot := map[int64][]earningsPortfolioExposure{}
	ids := make([]int64, 0, len(snapshotRows))
	for _, row := range snapshotRows {
		ids = append(ids, row.id)
	}
	positionRows, err := pool.Query(ctx,
		`SELECT snapshot_id, amount_raw, liquidity_mint, reserve
FROM `+earningsSchema+`.vault_position_snapshot_positions
WHERE snapshot_id = ANY($1::bigint[]) AND has_value = true`, ids)
	if err != nil {
		return nil, err
	}
	for positionRows.Next() {
		var snapshotID int64
		var exposure earningsPortfolioExposure
		var reserve string
		if err := positionRows.Scan(&snapshotID, &exposure.amountRaw, &exposure.liquidityMint, &reserve); err != nil {
			positionRows.Close()
			return nil, err
		}
		if exposure.amountRaw <= 0 {
			continue
		}
		exposure.kind = "kamino"
		exposure.reserve = &reserve
		exposure.sourceID = "reserve:" + reserve
		positionsBySnapshot[snapshotID] = append(positionsBySnapshot[snapshotID], exposure)
	}
	if err := positionRows.Err(); err != nil {
		positionRows.Close()
		return nil, err
	}
	positionRows.Close()

	type idleRow struct {
		observedSlot int64
		amountRaw    int64
		mint         string
		tokenAccount string
	}
	var idleRows []idleRow
	idleRowsScan, err := pool.Query(ctx,
		`SELECT observed_slot, amount_raw, mint, token_account
FROM `+earningsSchema+`.vault_idle_token_balances_current
WHERE vault_id = $1`, vaultRef.id)
	if err != nil {
		return nil, err
	}
	for idleRowsScan.Next() {
		var row idleRow
		if err := idleRowsScan.Scan(&row.observedSlot, &row.amountRaw, &row.mint, &row.tokenAccount); err != nil {
			idleRowsScan.Close()
			return nil, err
		}
		idleRows = append(idleRows, row)
	}
	if err := idleRowsScan.Err(); err != nil {
		idleRowsScan.Close()
		return nil, err
	}
	idleRowsScan.Close()

	latest := snapshotRows[len(snapshotRows)-1]
	out := make([]earningsPortfolioSnapshot, 0, len(snapshotRows))
	for _, row := range snapshotRows {
		snapshot := earningsPortfolioSnapshot{
			exposures:    positionsBySnapshot[row.id],
			observedAt:   row.observedAt,
			observedSlot: row.observedSlot,
		}
		if snapshot.exposures == nil {
			snapshot.exposures = []earningsPortfolioExposure{}
		}
		if row.id == latest.id {
			for _, idle := range idleRows {
				if idle.observedSlot == row.observedSlot && idle.amountRaw > 0 {
					snapshot.exposures = append(snapshot.exposures, earningsPortfolioExposure{
						amountRaw:     idle.amountRaw,
						kind:          "idle",
						liquidityMint: idle.mint,
						sourceID:      "idle:" + idle.tokenAccount,
					})
				}
			}
		}
		out = append(out, snapshot)
	}
	return out, nil
}

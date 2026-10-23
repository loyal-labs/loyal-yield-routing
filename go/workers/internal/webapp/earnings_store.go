package webapp

import (
	"bytes"
	"context"
	"encoding/json"
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

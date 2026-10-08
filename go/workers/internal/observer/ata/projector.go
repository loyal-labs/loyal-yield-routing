package ata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ProjectorConfig bounds the single-threaded capture-to-Yield projection loop.
// Stream must be production or staging; each stream needs its own Yield event
// namespace because the retained capture sequences start independently at one.
type ProjectorConfig struct {
	Stream       string
	Cluster      string
	BatchLimit   int
	PollInterval time.Duration
	IOTimeout    time.Duration
	OnHealth     func(bool)
	OnError      func(error)
}

// Projector borrows both pools. It never owns signing or RPC capabilities.
// Capture must publish event IDs in commit order. Sequence gaps are legitimate;
// a retained producer cutover requires draining in-flight capture writes before
// accepting the scalar cursor as a complete frontier.
type Projector struct {
	capture      *pgxpool.Pool
	yield        *pgxpool.Pool
	schema       string
	cluster      string
	consumer     string
	batchLimit   int
	pollInterval time.Duration
	ioTimeout    time.Duration
	onHealth     func(bool)
	onError      func(error)
}

func NewProjector(capture, yield *pgxpool.Pool, config ProjectorConfig) (*Projector, error) {
	if capture == nil || yield == nil {
		return nil, errors.New("ATA projector requires capture and Yield pools")
	}
	var schema string
	switch config.Stream {
	case "production":
		schema = "loyal_prod"
	case "staging":
		schema = "loyal_staging"
	default:
		return nil, fmt.Errorf("unsupported ATA capture stream %q", config.Stream)
	}
	if config.Cluster != "mainnet-beta" && config.Cluster != "devnet" {
		return nil, fmt.Errorf("unsupported ATA projection cluster %q", config.Cluster)
	}
	if config.BatchLimit < 1 || config.BatchLimit > 10000 {
		return nil, errors.New("ATA projection batch limit must be between 1 and 10000")
	}
	if config.PollInterval <= 0 || config.PollInterval > time.Minute || config.IOTimeout <= 0 || config.IOTimeout > time.Minute {
		return nil, errors.New("ATA projection poll interval and IO timeout must be positive and at most one minute")
	}
	return &Projector{capture: capture, yield: yield, schema: schema, cluster: config.Cluster,
		consumer:   "balance_sweep_ata_projector:" + config.Stream,
		batchLimit: config.BatchLimit, pollInterval: config.PollInterval, ioTimeout: config.IOTimeout,
		onHealth: config.OnHealth, onError: config.OnError}, nil
}

type ProjectionOutcome struct {
	PreviousEventID int64
	LastEventID     int64
	InsertedEvents  int
	FetchedEvents   int
}

// RequireSchema only checks the retained columns. Fixture creation and
// production migrations are owned by the runtime operator, never this lane.
func (p *Projector) RequireSchema(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.ioTimeout)
	defer cancel()
	if _, err := p.capture.Exec(ctx, fmt.Sprintf(`SELECT event_id,target_id,cluster,wallet,wallet_usdc_ata,vault_pubkey,vault_usdc_ata,
amount_raw,owner,mint,slot,observed_at,source,source_commitment,txn_signature,account_data_hash,
raw_account_data_base64,raw_evidence,received_at FROM %s.balance_sweep_wallet_ata_observations WHERE false`, p.schema)); err != nil {
		return fmt.Errorf("ATA capture schema unavailable: %w", err)
	}
	for _, query := range []string{
		`SELECT id,cluster,wallet,wallet_token_ata,vault_pubkey,vault_token_ata,token_mint FROM loyal_yield.balance_sweep_targets WHERE false`,
		`SELECT consumer_name,last_event_id,updated_at FROM loyal_yield.projection_offsets WHERE false`,
		`SELECT event_id,target_id,wallet,wallet_usdc_ata,wallet_token_ata,mint,previous_amount_raw,amount_raw,delta_amount_raw,
observed_slot,observed_at,source,source_commitment,txn_signature,account_data_hash,raw_evidence FROM loyal_yield.balance_sweep_wallet_balance_events WHERE false`,
		`SELECT target_id,wallet,wallet_usdc_ata,wallet_token_ata,mint,amount_raw,owner,observed_slot,observed_at,source,
source_commitment,txn_signature,account_data_hash,raw_evidence,updated_at FROM loyal_yield.balance_sweep_wallet_balances_current WHERE false`,
	} {
		if _, err := p.yield.Exec(ctx, query); err != nil {
			return fmt.Errorf("ATA destination schema unavailable: %w", err)
		}
	}
	return nil
}

// Tick reads capture before opening the destination transaction. A competing
// retained/Go projector may advance the shared offset meanwhile; only events
// beyond the locked destination offset are then applied. Events, current
// balances and offset commit together. A lost commit response is retried by ID.
func (p *Projector) Tick(ctx context.Context) (ProjectionOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, p.ioTimeout)
	defer cancel()
	after, err := p.readOffset(ctx)
	if err != nil {
		return ProjectionOutcome{}, err
	}
	events, err := p.readCapture(ctx, after)
	if err != nil {
		return ProjectionOutcome{}, err
	}
	if len(events) == 0 {
		return ProjectionOutcome{PreviousEventID: after, LastEventID: after}, nil
	}
	return p.applyBatch(ctx, after, events)
}

// Run reports ordinary failures without canceling the independently committing
// capture lane. Failed batches retain their checkpoint and retry with bounded
// backoff; persistent evidence conflicts remain unhealthy and are never skipped.
// Callbacks run synchronously outside transactions and must return promptly.
// Constructor and RequireSchema errors remain runtime startup failures.
func (p *Projector) Run(ctx context.Context) error {
	backoff := 250 * time.Millisecond
	for {
		outcome, err := p.Tick(ctx)
		delay := p.pollInterval
		if err != nil {
			if p.onHealth != nil {
				p.onHealth(false)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if p.onError != nil {
				p.onError(err)
			}
			delay = backoff
			backoff = min(backoff*2, 30*time.Second)
		} else {
			backoff = 250 * time.Millisecond
			if p.onHealth != nil {
				p.onHealth(true)
			}
			if outcome.FetchedEvents == p.batchLimit {
				continue
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

type projectionEvent struct {
	id, targetID, amountRaw, slot int64
	cluster, wallet, walletATA    string
	vault, vaultATA, mint         string
	owner, signature              *string
	observedAt, receivedAt        time.Time
	source, commitment, hash      string
	rawAccountData                string
	evidence                      json.RawMessage
}

func (p *Projector) readOffset(ctx context.Context) (int64, error) {
	var offset int64
	err := p.yield.QueryRow(ctx, `SELECT last_event_id FROM loyal_yield.projection_offsets WHERE consumer_name=$1`, p.consumer).Scan(&offset)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read ATA projection offset: %w", err)
	}
	if offset < 0 {
		return 0, errors.New("negative ATA projection offset")
	}
	return offset, nil
}

func (p *Projector) readCapture(ctx context.Context, after int64) ([]projectionEvent, error) {
	rows, err := p.capture.Query(ctx, fmt.Sprintf(`SELECT event_id,target_id,cluster,wallet,wallet_usdc_ata,vault_pubkey,vault_usdc_ata,
amount_raw,owner,mint,slot,observed_at,source,source_commitment,txn_signature,account_data_hash,
raw_account_data_base64,raw_evidence,received_at FROM %s.balance_sweep_wallet_ata_observations
WHERE event_id > $1 ORDER BY event_id ASC LIMIT $2`, p.schema), after, p.batchLimit)
	if err != nil {
		return nil, fmt.Errorf("read captured ATA observations: %w", err)
	}
	defer rows.Close()
	events := make([]projectionEvent, 0, p.batchLimit)
	previous := after
	for rows.Next() {
		var event projectionEvent
		if err := rows.Scan(&event.id, &event.targetID, &event.cluster, &event.wallet, &event.walletATA, &event.vault, &event.vaultATA,
			&event.amountRaw, &event.owner, &event.mint, &event.slot, &event.observedAt, &event.source, &event.commitment, &event.signature,
			&event.hash, &event.rawAccountData, &event.evidence, &event.receivedAt); err != nil {
			return nil, fmt.Errorf("decode captured ATA observation: %w", err)
		}
		if err := event.validate(previous); err != nil {
			return nil, err
		}
		event.evidence, err = projectionEvidence(event.evidence, event.rawAccountData)
		if err != nil {
			return nil, fmt.Errorf("ATA event %d evidence: %w", event.id, err)
		}
		events = append(events, event)
		previous = event.id
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read captured ATA observations: %w", err)
	}
	return events, nil
}

func (event projectionEvent) validate(previous int64) error {
	if event.cluster != "mainnet-beta" && event.cluster != "devnet" {
		return fmt.Errorf("ATA event %d has unknown captured cluster %q", event.id, event.cluster)
	}
	if event.id <= previous || event.targetID <= 0 || event.amountRaw < 0 || event.slot <= 0 || event.observedAt.IsZero() || event.receivedAt.IsZero() {
		return fmt.Errorf("ATA event %d has invalid identity, amount or observation clock", event.id)
	}
	if event.cluster == "" || event.wallet == "" || event.walletATA == "" || event.vault == "" || event.vaultATA == "" || event.mint == "" || event.source == "" || event.commitment == "" || event.hash == "" {
		return fmt.Errorf("ATA event %d omits captured account identity or provenance", event.id)
	}
	return nil
}

// Match the retained mapper exactly without decoding JSON numbers as float64.
// Only explicitly captured observations are patches; omitted accounts are never
// inferred to have zero balance or removed from the destination projection.
func projectionEvidence(raw json.RawMessage, accountData string) (json.RawMessage, error) {
	if !json.Valid(raw) {
		return nil, errors.New("invalid JSON evidence")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		object = map[string]json.RawMessage{"raw_evidence": raw}
	}
	encoded, err := json.Marshal(accountData)
	if err != nil {
		return nil, err
	}
	object["raw_account_data_base64"] = encoded
	return json.Marshal(object)
}

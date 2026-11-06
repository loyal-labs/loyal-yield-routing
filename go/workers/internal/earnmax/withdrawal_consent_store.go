package earnmax

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

type WithdrawalConsentDB interface {
	Begin(context.Context) (pgx.Tx, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Only app routes receive this writer. Workers read intent and own separate
// reservations/settlements; replacing consent never releases their signed wires.
type WithdrawalConsentStore struct{ db WithdrawalConsentDB }

func NewWithdrawalConsentStore(db WithdrawalConsentDB) *WithdrawalConsentStore {
	return &WithdrawalConsentStore{db: db}
}

func (s *WithdrawalConsentStore) Latest(ctx context.Context, authority string, g WithdrawalGeneration) (*WithdrawalConsent, error) {
	var raw []byte
	err := s.db.QueryRow(ctx, `SELECT payload FROM loyal_yield.earn_max_withdrawal_consents WHERE authority=$1 AND request_signature=$2 AND request_instruction=$3 ORDER BY version DESC LIMIT 1`, authority, g.RequestSignature, g.RequestInstruction).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var consent WithdrawalConsent
	if err := json.Unmarshal(raw, &consent); err != nil {
		return nil, err
	}
	return &consent, nil
}

// Accept is called only after authenticated owner/signature/current-generation
// verification. A generation lock serializes immutable version insertion.
func (s *WithdrawalConsentStore) Accept(ctx context.Context, c WithdrawalConsent) (string, error) {
	message, err := c.CanonicalMessage()
	if err != nil {
		return "", err
	}
	id, err := c.Digest()
	if err != nil {
		return "", err
	}
	version, err := canonicalUint(c.Version)
	if err != nil || version == 0 || version > uint64(^uint64(0)>>1) {
		return "", errors.New("invalid consent version")
	}
	expires, err := canonicalUint(c.ExpiresAt)
	if err != nil || expires > uint64(^uint64(0)>>1) {
		return "", errors.New("invalid consent expiry")
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(c.OwnerSignature)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, c.WithdrawalGeneration.Key(c.Authority)); err != nil {
		return "", err
	}
	var existing []byte
	err = tx.QueryRow(ctx, `SELECT canonical_message FROM loyal_yield.earn_max_withdrawal_consents WHERE consent_id=$1`, id).Scan(&existing)
	if err == nil {
		if !bytes.Equal(existing, message) {
			return "", errors.New("consent digest conflict")
		}
		return id, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	var priorMessage []byte
	var latest int64
	err = tx.QueryRow(ctx, `SELECT version,canonical_message FROM loyal_yield.earn_max_withdrawal_consents WHERE authority=$1 AND request_signature=$2 AND request_instruction=$3 ORDER BY version DESC LIMIT 1`, c.Authority, c.RequestSignature, c.RequestInstruction).Scan(&latest, &priorMessage)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if uint64(latest) == version && bytes.Equal(priorMessage, message) {
		return id, tx.Commit(ctx)
	}
	if uint64(latest)+1 != version {
		return "", errors.New("consent version conflict: refresh and sign a new consent")
	}
	_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.earn_max_withdrawal_consents (consent_id,authority,vault,request_signature,request_instruction,version,owner,nonce,expires_at,canonical_message,owner_signature,payload) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, id, c.Authority, c.Vault, c.RequestSignature, c.RequestInstruction, int64(version), c.Owner, c.Nonce, time.Unix(int64(expires), 0).UTC(), message, signature, payload)
	if err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

func ConsentExpiry(c WithdrawalConsent) time.Time {
	n, _ := strconv.ParseInt(c.ExpiresAt, 10, 64)
	return time.Unix(n, 0).UTC()
}

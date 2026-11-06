package earnmax

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/releasecontrol"
)

// ErrWithdrawalClaimWireUnqualified refuses a claim when the server has no
// chain client to run the protected-claim qualifier against.
var ErrWithdrawalClaimWireUnqualified = errors.New("protected withdrawal claim wire is not independently qualified")
var ErrWithdrawalClaimPending = errors.New("withdrawal_claim_pending")
var ErrWithdrawalFeeBudget = errors.New(string(WithdrawalFeeBudget))

type WithdrawalAttemptStore struct {
	db    WithdrawalConsentDB
	chain *chain.Client
}

func NewWithdrawalAttemptStore(db WithdrawalConsentDB, clients ...*chain.Client) *WithdrawalAttemptStore {
	s := &WithdrawalAttemptStore{db: db}
	if len(clients) == 1 {
		s.chain = clients[0]
	}
	return s
}

type validatedWithdrawalAttempt struct {
	wire                 []byte
	signature, blockhash string
	height, fee          uint64
}

func validateWithdrawalAttemptWire(c WithdrawalConsent, wire []byte, signature, blockhash string, height, fee uint64) (validatedWithdrawalAttempt, error) {
	if len(wire) == 0 || len(wire) > 1232 || height == 0 || fee == 0 {
		return validatedWithdrawalAttempt{}, errors.New("invalid withdrawal claim attempt")
	}
	tx, err := solana.TransactionFromBytes(wire)
	if err != nil {
		return validatedWithdrawalAttempt{}, err
	}
	canonical, err := tx.MarshalBinary()
	if err != nil || !bytes.Equal(canonical, wire) {
		return validatedWithdrawalAttempt{}, errors.New("noncanonical withdrawal wire")
	}
	if len(tx.Signatures) != 1 || len(tx.Message.AccountKeys) == 0 || tx.Message.Header.NumRequiredSignatures != 1 || tx.Message.AccountKeys[0].String() != c.Owner || tx.Signatures[0].String() != signature || tx.Message.RecentBlockhash.String() != blockhash || tx.VerifySignatures() != nil {
		return validatedWithdrawalAttempt{}, errors.New("withdrawal wire signature or owner mismatch")
	}
	return validatedWithdrawalAttempt{append([]byte(nil), wire...), signature, blockhash, height, fee}, nil
}

// qualifyWithdrawalClaim must verify the complete protected claim instruction
// sequence, current receipt/seed and the actual blockhash validity bound. A
// wallet assertion, a successful simulation or a generic Squads instruction is
// insufficient evidence. No unsigned/client-provided bypass is accepted.
func (s *WithdrawalAttemptStore) qualifyWithdrawalClaim(ctx context.Context, c WithdrawalConsent, a *validatedWithdrawalAttempt) error {
	return qualifyProtectedWithdrawalClaim(ctx, s.chain, c, a)
}

func (s *WithdrawalAttemptStore) ExistingExact(ctx context.Context, c WithdrawalConsent, wire []byte) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, errors.New("withdrawal attempt store unavailable")
	}
	tx, err := solana.TransactionFromBytes(wire)
	if err != nil {
		return "", false, err
	}
	if len(tx.Signatures) != 1 {
		return "", false, errors.New("withdrawal attempt requires one owner signature")
	}
	if _, err := validateWithdrawalAttemptWire(c, wire, tx.Signatures[0].String(), tx.Message.RecentBlockhash.String(), 1, 1); err != nil {
		return "", false, err
	}
	digest := sha256.Sum256(wire)
	id := hex.EncodeToString(digest[:])
	consentID, err := c.Digest()
	if err != nil {
		return "", false, err
	}
	var prior []byte
	var priorConsent, state string
	err = s.db.QueryRow(ctx, `SELECT signed_wire,consent_id,state FROM loyal_yield.earn_max_withdrawal_claim_attempts WHERE attempt_id=$1`, id).Scan(&prior, &priorConsent, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !bytes.Equal(prior, wire) || priorConsent != consentID {
		return "", true, errors.New("withdrawal attempt identity conflict")
	}
	if state == "released" {
		return "", true, errors.New("withdrawal attempt blockhash expired")
	}
	return id, true, nil
}

func (s *WithdrawalAttemptStore) Reserve(ctx context.Context, c WithdrawalConsent, wire []byte, signature, blockhash string, height, fee uint64) (string, error) {
	a, err := validateWithdrawalAttemptWire(c, wire, signature, blockhash, height, fee)
	if err != nil {
		return "", err
	}
	idBytes := sha256.Sum256(wire)
	id := hex.EncodeToString(idBytes[:])
	consentID, err := c.Digest()
	if err != nil {
		return "", err
	}
	var priorWire []byte
	var priorConsent, state string
	err = s.db.QueryRow(ctx, `SELECT signed_wire,consent_id,state FROM loyal_yield.earn_max_withdrawal_claim_attempts WHERE attempt_id=$1`, id).Scan(&priorWire, &priorConsent, &state)
	if err == nil {
		if !bytes.Equal(priorWire, wire) || priorConsent != consentID {
			return "", errors.New("withdrawal attempt identity conflict")
		}
		if state == "released" {
			return "", errors.New("withdrawal attempt blockhash expired")
		}
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if err := s.CheckNAVPublication(ctx); err != nil {
		return "", err
	}
	if _, err := retryTransient(ctx, func() (struct{}, error) { return struct{}{}, s.qualifyWithdrawalClaim(ctx, c, &a) }); err != nil {
		return "", err
	}
	return s.reserveValidated(ctx, c, a)
}

func (s *WithdrawalAttemptStore) PendingAttempts(ctx context.Context, after string, limit int) ([]string, error) {
	if limit < 1 || limit > 32 {
		return nil, errors.New("withdrawal attempt batch must be between one and 32")
	}
	var body []byte
	if err := s.db.QueryRow(ctx, `SELECT COALESCE(json_agg(attempt_id ORDER BY attempt_id),'[]'::json) FROM (SELECT attempt_id FROM loyal_yield.earn_max_withdrawal_claim_attempts WHERE state='reserved' AND attempt_id>$1 ORDER BY attempt_id LIMIT $2) pending`, after, limit).Scan(&body); err != nil {
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal(body, &ids); err != nil {
		return nil, err
	}
	return ids, nil
}

func (s *WithdrawalAttemptStore) reserveValidated(ctx context.Context, c WithdrawalConsent, a validatedWithdrawalAttempt) (string, error) {
	owner, err := solana.PublicKeyFromBase58(c.Owner)
	if err != nil {
		return "", err
	}
	settings, err := solana.PublicKeyFromBase58(c.Settings)
	if err != nil {
		return "", err
	}
	if err := c.Validate(owner, settings, time.Now()); err != nil {
		return "", err
	}
	idBytes := sha256.Sum256(a.wire)
	id := hex.EncodeToString(idBytes[:])
	consentID, err := c.Digest()
	if err != nil {
		return "", err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, c.WithdrawalGeneration.Key(c.Authority)); err != nil {
		return "", err
	}
	var existing []byte
	var priorConsent string
	err = tx.QueryRow(ctx, `SELECT signed_wire,consent_id FROM loyal_yield.earn_max_withdrawal_claim_attempts WHERE attempt_id=$1`, id).Scan(&existing, &priorConsent)
	if err == nil {
		if !bytes.Equal(existing, a.wire) || priorConsent != consentID {
			return "", errors.New("withdrawal attempt identity conflict")
		}
		return id, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	var latest string
	if err := tx.QueryRow(ctx, `SELECT consent_id FROM loyal_yield.earn_max_withdrawal_consents WHERE authority=$1 AND request_signature=$2 AND request_instruction=$3 ORDER BY version DESC LIMIT 1`, c.Authority, c.RequestSignature, c.RequestInstruction).Scan(&latest); err != nil {
		return "", err
	}
	if latest != consentID {
		return "", errors.New("withdrawal consent changed")
	}
	var pending bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.earn_max_withdrawal_claim_attempts WHERE authority=$1 AND request_signature=$2 AND request_instruction=$3 AND state='reserved')`, c.Authority, c.RequestSignature, c.RequestInstruction).Scan(&pending); err != nil {
		return "", err
	}
	if pending {
		return "", ErrWithdrawalClaimPending
	}
	var spent, reserved string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(CASE WHEN state='settled' THEN actual_fee_lamports ELSE 0 END),0)::text,COALESCE(sum(CASE WHEN state='reserved' THEN reserved_fee_lamports ELSE 0 END),0)::text FROM loyal_yield.earn_max_withdrawal_claim_attempts WHERE authority=$1 AND request_signature=$2 AND request_instruction=$3`, c.Authority, c.RequestSignature, c.RequestInstruction).Scan(&spent, &reserved); err != nil {
		return "", err
	}
	consumed, err := strconv.ParseUint(spent, 10, 64)
	if err != nil {
		return "", err
	}
	held, err := strconv.ParseUint(reserved, 10, 64)
	if err != nil {
		return "", err
	}
	// Payable is checked atomically by the guarded claim itself; this ledger
	// owns the independent network-fee limit across all consent versions.
	budget, err := canonicalUint(c.MaxUserFeeLamports)
	if err != nil {
		return "", err
	}
	for _, amount := range []uint64{consumed, held, a.fee} {
		if amount > budget {
			return "", ErrWithdrawalFeeBudget
		}
		budget -= amount
	}
	reference, err := canonicalUint(c.ReferenceUSDC)
	if err != nil {
		return "", err
	}
	admission, err := releasecontrol.Check(ctx, tx, c.Vault, releasecontrol.ClaimAttempt, []string{c.Authority}, &reference)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.earn_max_withdrawal_claim_attempts(attempt_id,consent_id,authority,request_signature,request_instruction,signed_wire,transaction_signature,blockhash,last_valid_block_height,reserved_fee_lamports,release_control_vault,release_control_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, id, consentID, c.Authority, c.RequestSignature, c.RequestInstruction, a.wire, a.signature, a.blockhash, strconv.FormatUint(a.height, 10), strconv.FormatUint(a.fee, 10), admission.Vault, admission.Version)
	if err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}

func (s *WithdrawalAttemptStore) Reconcile(ctx context.Context, id string) error {
	if s.chain == nil {
		return errors.New("withdrawal attempt chain verifier unavailable")
	}
	var c WithdrawalGeneration
	var authority, signature, height, state string
	var wire []byte
	if err := s.db.QueryRow(ctx, `SELECT authority,request_signature,request_instruction,signed_wire,transaction_signature,last_valid_block_height::text,state FROM loyal_yield.earn_max_withdrawal_claim_attempts WHERE attempt_id=$1`, id).Scan(&authority, &c.RequestSignature, &c.RequestInstruction, &wire, &signature, &height, &state); err != nil {
		return err
	}
	if state != "reserved" {
		return nil
	}
	lastValid, err := strconv.ParseUint(height, 10, 64)
	if err != nil {
		return err
	}
	out, err := chain.Observe(ctx, s.chain, chain.Attempt{Wire: wire, Signature: signature, LastValidBlockHeight: lastValid, Required: chain.Finalized})
	if err != nil {
		return err
	}
	var fee, slot uint64
	if out.Kind != chain.Expired {
		if out.Kind != chain.Landed && out.Kind != chain.Failed {
			return nil
		}
		sig, err := solana.SignatureFromBase58(signature)
		if err != nil {
			return err
		}
		receipt, err := s.chain.Execution(ctx, sig, rpc.CommitmentFinalized)
		if err != nil {
			return err
		}
		if !bytes.Equal(receipt.Wire, wire) || receipt.Slot == 0 {
			return errors.New("withdrawal attempt receipt wire mismatch")
		}
		fee, slot = receipt.Fee, receipt.Slot
	} else {
		tx, err := solana.TransactionFromBytes(wire)
		if err != nil {
			return err
		}
		valid, _, err := s.chain.BlockhashValid(ctx, tx.Message.RecentBlockhash, rpc.CommitmentFinalized, out.ContextSlot)
		if err != nil {
			return err
		}
		if valid {
			return nil
		}
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, c.Key(authority)); err != nil {
		return err
	}
	if out.Kind == chain.Expired {
		_, err = tx.Exec(ctx, `UPDATE loyal_yield.earn_max_withdrawal_claim_attempts SET state='released' WHERE attempt_id=$1 AND state='reserved'`, id)
	} else {
		_, err = tx.Exec(ctx, `UPDATE loyal_yield.earn_max_withdrawal_claim_attempts SET state='settled',actual_fee_lamports=$2,finalized_slot=$3 WHERE attempt_id=$1 AND state='reserved'`, id, strconv.FormatUint(fee, 10), slot)
	}
	if err != nil {
		return fmt.Errorf("settle withdrawal attempt: %w", err)
	}
	return tx.Commit(ctx)
}

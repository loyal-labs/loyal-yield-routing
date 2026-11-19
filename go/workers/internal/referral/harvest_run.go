package referral

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

type HarvestReport struct {
	Status        string `json:"status"`
	Reason        string `json:"reason,omitempty"`
	VerifiedSlot  uint64 `json:"verifiedSlot,omitempty"`
	EligibleLPRaw string `json:"eligibleLpRaw,omitempty"`
	Signature     string `json:"signature,omitempty"`
}

type harvestInventory struct {
	HarvestReport
	state feeState
}

// A repeatable snapshot keeps the cursor, unharvested assessments, friends'
// certified weights and fractional carry tied to the same observer commit.
func (s *Store) harvestInventory(ctx context.Context) (out harvestInventory, err error) {
	tx, err := s.yield.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	var launch, slot, checked uint64
	var raw []byte
	var hold *string
	err = tx.QueryRow(ctx, `SELECT p.launch_slot, coalesce(c.verified_slot,0), coalesce(c.checked_slot,0), c.state, c.hold_reason
FROM loyal_yield.referral_earn_max_program p LEFT JOIN loyal_yield.referral_earn_max_fee_cursor c USING(singleton) WHERE p.singleton`).Scan(&launch, &slot, &checked, &raw, &hold)
	if errors.Is(err, pgx.ErrNoRows) {
		out.Status = "not_launched"
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.VerifiedSlot = slot
	if slot == 0 {
		out.Status = "opening_checkpoint_required"
		return out, nil
	}
	if hold != nil || checked != slot || slot < launch {
		out.Status = "held"
		out.Reason = "fee observer has an unresolved checkpoint"
		if hold != nil {
			out.Reason = *hold
		}
		return out, nil
	}
	if err = json.Unmarshal(raw, &out.state); err != nil {
		return out, err
	}
	if out.state.Head == "" || out.state.Accrued < out.state.OpeningReserve {
		return out, errors.New("invalid anchored fee inventory")
	}
	var outstanding string
	if err = tx.QueryRow(ctx, `SELECT coalesce(sum(fee_lp_raw),0)::text FROM loyal_yield.referral_earn_max_fee_assessments WHERE harvest_signature IS NULL`).Scan(&outstanding); err != nil {
		return out, err
	}
	if outstanding != strconv.FormatUint(out.state.Accrued-out.state.OpeningReserve, 10) {
		return out, errors.New("unharvested assessments disagree with the anchored fee inventory")
	}
	rows, err := tx.Query(ctx, `SELECT w.referee_app_user_id::text,coalesce(r.remainder,'0'),a.fee_lp_raw::text,w.shares_raw::text,a.effective_supply_raw::text
FROM loyal_yield.referral_earn_max_fee_assessments a
JOIN loyal_yield.referral_earn_max_fee_weights w ON w.assessment_signature=a.signature
JOIN loyal_yield.referral_bindings b USING(referee_app_user_id, referrer_app_user_id)
LEFT JOIN loyal_yield.referral_earn_max_fee_remainders r USING(referee_app_user_id)
WHERE a.harvest_signature IS NULL AND NOT w.realized AND w.hold_reason IS NULL AND w.shares_raw>0
AND NOT EXISTS(SELECT FROM loyal_yield.referral_attribution_conflicts c WHERE c.referee_app_user_id=w.referee_app_user_id)
ORDER BY w.referee_app_user_id,a.slot,a.transaction_index`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	var prior, carry string
	var eligible uint64
	for rows.Next() {
		var friend, remainder, fee, shares, supply string
		if err = rows.Scan(&friend, &remainder, &fee, &shares, &supply); err != nil {
			return out, err
		}
		if friend != prior {
			prior, carry = friend, remainder
		}
		q, err := strconv.ParseUint(fee, 10, 64)
		if err != nil {
			return out, err
		}
		w, err := strconv.ParseUint(shares, 10, 64)
		if err != nil {
			return out, err
		}
		d, err := strconv.ParseUint(supply, 10, 64)
		if err != nil {
			return out, err
		}
		whole, next, err := earnedFeeShares(carry, q, w, d)
		if err != nil {
			return out, err
		}
		if !whole.IsUint64() || ^uint64(0)-eligible < whole.Uint64() {
			return out, errors.New("referral fee amount overflow")
		}
		eligible += whole.Uint64()
		carry = next
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	out.EligibleLPRaw = strconv.FormatUint(eligible, 10)
	out.Status = "eligible"
	if eligible == 0 {
		out.Status = "no_eligible_fees"
	}
	return out, nil
}

const harvestAttemptFile = "harvest-attempt.json"

type harvestAttempt struct {
	Signature            solana.Signature
	Blockhash            solana.Hash
	LastValidBlockHeight uint64
	QuoteSlot            uint64
	Wire                 []byte
}

func (a harvestAttempt) validate(payer solana.PublicKey) error {
	if a.LastValidBlockHeight == 0 || a.QuoteSlot == 0 || a.Signature.IsZero() || a.Blockhash.IsZero() {
		return errors.New("incomplete persisted harvest attempt")
	}
	if err := ValidateHarvestWire(a.Wire, payer, a.Blockhash); err != nil {
		return err
	}
	tx, err := solana.TransactionFromBytes(a.Wire)
	if err != nil || len(tx.Signatures) != 1 || tx.Signatures[0] != a.Signature {
		return errors.New("persisted harvest signature differs from signed transaction")
	}
	return nil
}

// Lock the directory inode; replacing or removing the journal never releases
// the lock. Both the rename and the directory entry must survive power loss.
func lockHarvestDir(path string) (*os.File, error) {
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := dir.Stat()
	if err == nil && (!st.IsDir() || st.Mode().Perm()&0077 != 0) {
		err = errors.New("harvest state requires a private directory")
	}
	if err == nil {
		err = syscall.Flock(int(dir.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err != nil {
		dir.Close()
		return nil, err
	}
	return dir, nil
}

func saveHarvest(dir *os.File, a harvestAttempt) error {
	f, err := os.CreateTemp(dir.Name(), ".harvest-attempt-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = json.NewEncoder(f).Encode(a); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), filepath.Join(dir.Name(), harvestAttemptFile)); err != nil {
		return err
	}
	return dir.Sync()
}

func clearHarvest(dir *os.File) error {
	if dir == nil {
		return nil
	} // preview
	if err := os.Remove(filepath.Join(dir.Name(), harvestAttemptFile)); err != nil {
		return err
	}
	return dir.Sync()
}

// RunHarvest is a single scheduler tick. Only the independent finalized fee
// observer creates fee lots. An RPC send response never establishes a harvest.
func (s *Store) RunHarvest(ctx context.Context, c *chain.Client, payer solana.PublicKey, signer solana.PrivateKey, stateDir string, send bool) (HarvestReport, error) {
	var report HarvestReport
	var dir *os.File
	var err error
	if send {
		if len(signer) != ed25519.PrivateKeySize || signer.PublicKey() != payer {
			return report, errors.New("harvest fee payer does not match signing key")
		}
		if dir, err = lockHarvestDir(stateDir); errors.Is(err, syscall.EWOULDBLOCK) {
			return HarvestReport{Status: "busy"}, nil
		}
		if err != nil {
			return report, err
		}
		defer dir.Close()
	}
	if stateDir != "" {
		raw, err := os.ReadFile(filepath.Join(stateDir, harvestAttemptFile))
		if err == nil {
			var a harvestAttempt
			if err = json.Unmarshal(raw, &a); err != nil {
				return report, err
			}
			if err = a.validate(payer); err != nil {
				return report, err
			}
			return s.reconcileHarvest(ctx, c, a, dir)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return report, err
		}
	}
	inventory, err := s.harvestInventory(ctx)
	report = inventory.HarvestReport
	if err != nil || report.Status != "eligible" {
		return report, err
	}
	q, err := ReadHarvestQuote(ctx, c, payer, report.VerifiedSlot)
	if err != nil {
		return report, err
	}
	// Fees assessed since the last observer tick can accumulate alongside the
	// eligible inventory. A harvest in that interval consumes the older fees,
	// even if subsequent accrual restored the exact quoted amount.
	var events []feeEvent
	if q.Slot > report.VerifiedSlot {
		events, _, _, err = feeRange(ctx, c, report.VerifiedSlot, q.Slot, inventory.state.Head)
		if err != nil {
			return report, err
		}
	}
	available, err := harvestInventoryAvailable(inventory.state, q.state, events)
	if err != nil {
		return report, err
	}
	if !available {
		report.Status = "awaiting_fee_observer"
		return report, nil
	}
	w, err := q.Build(ctx, c)
	if err != nil {
		return report, err
	}
	if !send {
		if err = w.SimulateUnsigned(ctx, c); err != nil {
			return report, err
		}
		report.Status = "simulated"
		return report, nil
	}
	sig, err := signer.Sign(w.Message)
	if err != nil {
		return report, err
	}
	wire, err := w.Signed(sig)
	if err != nil {
		return report, err
	}
	a := harvestAttempt{Signature: sig, Blockhash: w.Blockhash, LastValidBlockHeight: w.LastValidBlockHeight, QuoteSlot: q.Slot, Wire: wire}
	if err = a.validate(payer); err != nil {
		return report, err
	}
	if err = w.Simulate(ctx, c, wire); err != nil {
		return report, err
	}
	if err = saveHarvest(dir, a); err != nil {
		return report, err
	}
	report.Signature = sig.String()
	report.Status = "submitted"
	if err = c.SendWire(ctx, wire, false); err != nil {
		report.Status = "submission_unknown"
		return report, fmt.Errorf("harvest send outcome unknown; persisted signature retained: %w", err)
	}
	return report, nil
}

func harvestInventoryAvailable(anchored, live feeState, events []feeEvent) (bool, error) {
	for _, e := range events {
		if e.kind == "harvest" {
			return false, nil
		}
		if err := anchored.apply(e); err != nil {
			return false, err
		}
	}
	if anchored.Minted != live.Minted || anchored.Dead != live.Dead || anchored.Accrued != live.Accrued {
		return false, errors.New("new finalized activity does not reconcile to the harvest quote")
	}
	return true, nil
}

type harvestCheckpoint struct {
	Verified    uint64
	ReceiptSlot uint64
	Held        bool
}

type harvestStatusRPC interface {
	chain.LandChain
	FinalizedBlockHeightAt(context.Context, uint64) (uint64, error)
	BlockhashValid(context.Context, solana.Hash, rpc.CommitmentType, uint64) (bool, uint64, error)
}

func (s *Store) reconcileHarvest(ctx context.Context, c *chain.Client, a harvestAttempt, dir *os.File) (HarvestReport, error) {
	var db harvestCheckpoint
	err := s.yield.QueryRow(ctx, `SELECT coalesce((SELECT verified_slot FROM loyal_yield.referral_earn_max_fee_cursor WHERE singleton),0),
coalesce((SELECT slot FROM loyal_yield.referral_earn_max_fee_harvests WHERE signature=$1),0),
coalesce((SELECT hold_reason IS NOT NULL OR verified_slot<>checked_slot FROM loyal_yield.referral_earn_max_fee_cursor WHERE singleton),true)`, a.Signature.String()).Scan(&db.Verified, &db.ReceiptSlot, &db.Held)
	if err != nil {
		return HarvestReport{Signature: a.Signature.String()}, err
	}
	out, err := harvestDisposition(ctx, c, a, db)
	if err == nil && (out.Status == "reconciled" || out.Status == "expired") {
		err = clearHarvest(dir)
	}
	return out, err
}

func harvestDisposition(ctx context.Context, c harvestStatusRPC, a harvestAttempt, db harvestCheckpoint) (HarvestReport, error) {
	out := HarvestReport{Signature: a.Signature.String(), VerifiedSlot: db.Verified}
	if db.ReceiptSlot != 0 {
		if db.ReceiptSlot < a.QuoteSlot || db.ReceiptSlot > db.Verified {
			return out, errors.New("harvest receipt outside verified interval")
		}
		out.Status = "reconciled"
		return out, nil
	}
	outcome, err := chain.Observe(ctx, c, chain.Attempt{Signature: out.Signature, LastValidBlockHeight: a.LastValidBlockHeight, Required: chain.Finalized})
	if err != nil {
		return out, err
	}
	out.Status = "pending_finality"
	switch outcome.Kind {
	case chain.Landed:
		out.Status = "awaiting_fee_observer"
		if db.Held || db.Verified >= outcome.Slot {
			out.Status = "held"
			out.Reason = "finalized harvest lacks a verified treasury receipt in the fee observer"
		}
	case chain.Failed:
		// Confirmed errors may be on a fork. Keep the record in both cases.
		if outcome.Commitment == chain.Finalized {
			out.Status = "held"
			out.Reason = "finalized harvest failed: " + outcome.Err
		}
	case chain.Expired:
		// Prove the observer has covered every possible landing slot. Absence
		// from the RPC alone cannot release an attempt after an RPC history gap.
		out.Status = "awaiting_fee_observer"
		if db.Held {
			out.Status = "held"
			out.Reason = "fee observer cannot verify the expired harvest"
			return out, nil
		}
		if db.Verified < a.QuoteSlot {
			return out, nil
		}
		height, err := c.FinalizedBlockHeightAt(ctx, db.Verified)
		if err != nil {
			return out, err
		}
		if height <= a.LastValidBlockHeight {
			return out, nil
		}
		valid, _, err := c.BlockhashValid(ctx, a.Blockhash, rpc.CommitmentFinalized, db.Verified)
		if err != nil {
			return out, err
		}
		if valid {
			return out, errors.New("recorded harvest expiry conflicts with live blockhash")
		}
		out.Status = "expired"
	}
	return out, nil
}

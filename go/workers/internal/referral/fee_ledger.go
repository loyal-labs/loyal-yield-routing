package referral

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/solana-foundation/solana-go/v2"
)

type FeeReport struct {
	DryRun              bool   `json:"dryRun"`
	OpeningIneligibleLP string `json:"openingIneligibleLpRaw,omitempty"`
	AdminAccruedLP      string `json:"adminAccruedLpRaw,omitempty"`
	VaultHistoryMarker  string `json:"vaultHistoryMarker,omitempty"`
	Status              string `json:"status"`
	VerifiedSlot        uint64 `json:"verifiedSlot,omitempty"`
	CheckedSlot         uint64 `json:"checkedSlot,omitempty"`
	Reason              string `json:"reason,omitempty"`
	Assessments         int    `json:"assessments"`
	Harvests            int    `json:"harvests"`
	Lots                int    `json:"lots"`
	HeldWeights         int    `json:"heldWeights"`
	RetriedWeights      int    `json:"retriedWeights"`
	BackfilledLots      int    `json:"backfilledLots"`
}

type assessedFee struct {
	FeePoint
	Amount, Supply uint64
}

func (s *Store) CollectFees(ctx context.Context, c *chain.Client, anchor, dry bool) (FeeReport, error) {
	out := FeeReport{DryRun: dry}
	var launch uint64
	var began time.Time
	err := s.yield.QueryRow(ctx, "SELECT launch_slot,launched_at FROM loyal_yield.referral_earn_max_program WHERE singleton").Scan(&launch, &began)
	if errors.Is(err, pgx.ErrNoRows) {
		out.Status = "not_launched"
		return out, nil
	}
	if err != nil {
		return out, err
	}
	var cursor uint64
	var raw []byte
	err = s.yield.QueryRow(ctx, "SELECT verified_slot,state FROM loyal_yield.referral_earn_max_fee_cursor WHERE singleton").Scan(&cursor, &raw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if cursor == 0 && !anchor {
		out.Status = "opening_checkpoint_required"
		return out, nil
	}
	if cursor != 0 && anchor {
		return out, errors.New("fee accounting already has an opening checkpoint")
	}
	out.VerifiedSlot = cursor
	if !dry && cursor > 0 {
		retries, lots, err := s.retryFeeWeights(ctx)
		if err != nil {
			return out, err
		}
		out.RetriedWeights, out.BackfilledLots = retries, lots
	}
	target, observed, err := feeSnapshot(ctx, c, max(cursor, launch))
	out.CheckedSlot = target
	if err != nil {
		return s.holdFees(ctx, out, dry, err)
	}
	out.AdminAccruedLP = strconv.FormatUint(observed.Accrued, 10)
	at, err := c.BlockTime(ctx, target)
	if err != nil {
		return s.holdFees(ctx, out, dry, err)
	}
	if target < launch || at.Before(began) {
		return s.holdFees(ctx, out, dry, errors.New("checkpoint precedes program launch"))
	}
	if cursor == 0 {
		observed.Head, err = feeOpeningMarker(ctx, c, target)
		if err != nil {
			return s.holdFees(ctx, out, dry, err)
		}
		observed.OpeningReserve = observed.Accrued
		if !dry {
			body, err := json.Marshal(observed)
			if err != nil {
				return out, err
			}
			_, err = s.yield.Exec(ctx, `INSERT INTO loyal_yield.referral_earn_max_fee_cursor(verified_slot,state,checked_slot) VALUES($1,$2,$1)`, target, body)
			if err != nil {
				return out, err
			}
		}
		out.OpeningIneligibleLP = strconv.FormatUint(observed.OpeningReserve, 10)
		out.VaultHistoryMarker = observed.Head
		out.Status = "anchored"
		if dry {
			out.Status = "anchor_preview"
		}
		out.VerifiedSlot = target
		return out, nil
	}
	var previous feeState
	if err := json.Unmarshal(raw, &previous); err != nil {
		return out, err
	}
	events, blocks, newHead, err := feeRange(ctx, c, cursor, target, previous.Head)
	if err != nil {
		return s.holdFees(ctx, out, dry, err)
	}
	model := previous
	for _, e := range events {
		if err := model.apply(e); err != nil {
			return s.holdFees(ctx, out, dry, fmt.Errorf("slot %d tx %d: %w", e.Slot, e.Index, err))
		}
	}
	observed.OpeningReserve = model.OpeningReserve
	model.Head, observed.Head = newHead, newHead
	if observed != model {
		return s.holdFees(ctx, out, dry, errors.New("finalized vault and LP mint do not reconcile to ordered receipts"))
	}
	err = s.applyFeeBatch(ctx, cursor, target, previous, observed, events, blocks, dry, &out)
	if err != nil {
		return s.holdFees(ctx, out, dry, err)
	}
	out.Status = "verified"
	out.VaultHistoryMarker = observed.Head
	if out.HeldWeights > 0 {
		out.Status = "verified_with_held_friends"
	}
	out.VerifiedSlot = target
	return out, nil
}

// The cursor continues to describe the last fully reconciled inventory.
func (s *Store) holdFees(ctx context.Context, out FeeReport, dry bool, cause error) (FeeReport, error) {
	out.Status = "held"
	out.Reason = cause.Error()
	out.Assessments = 0
	out.Harvests = 0
	out.Lots = 0
	if !dry && out.VerifiedSlot > 0 && out.CheckedSlot >= out.VerifiedSlot {
		_, err := s.yield.Exec(ctx, `UPDATE loyal_yield.referral_earn_max_fee_cursor SET checked_slot=$2,hold_reason=$3
WHERE singleton AND verified_slot=$1 AND checked_slot<=$2`, out.VerifiedSlot, out.CheckedSlot, out.Reason)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

func (s *Store) applyFeeBatch(ctx context.Context, from, to uint64, previous, observed feeState, events []feeEvent, blocks map[uint64][]solana.Signature, dry bool, out *FeeReport) error {
	tx, err := s.yield.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock_shared(hashtextextended('referral-identity-evidence', 0))"); err != nil {
		return err
	}
	var locked uint64
	lockSQL := "SELECT verified_slot FROM loyal_yield.referral_earn_max_fee_cursor WHERE singleton"
	if !dry {
		lockSQL += " FOR UPDATE"
	}
	if err := tx.QueryRow(ctx, lockSQL).Scan(&locked); err != nil {
		return err
	}
	if locked != from {
		return errors.New("fee cursor advanced concurrently; retry")
	}
	pending, err := unharvestedFees(ctx, tx)
	if err != nil {
		return err
	}
	reserve := previous.OpeningReserve
	treasury, err := feeTreasury()
	if err != nil {
		return err
	}
	for _, e := range events {
		if e.fee > 0 {
			a := assessedFee{FeePoint: e.FeePoint, Amount: e.fee, Supply: e.supplyBefore}
			if a.Supply == 0 {
				return errors.New("fee assessment has zero denominator")
			}
			held, err := recordFeeAssessment(ctx, tx, a, blocks[e.Slot], dry)
			if err != nil {
				return err
			}
			pending = append(pending, a)
			out.Assessments++
			out.HeldWeights += held
		}
		if e.kind != "harvest" {
			continue
		}
		total := reserve
		for _, a := range pending {
			if ^uint64(0)-total < a.Amount {
				return errors.New("unharvested inventory overflow")
			}
			total += a.Amount
		}
		if total != e.minted {
			return errors.New("actual mint does not reconcile with opening reserve and pending assessments")
		}
		if !dry {
			_, err := tx.Exec(ctx, `INSERT INTO loyal_yield.referral_earn_max_fee_harvests(signature,slot,transaction_index,treasury_lp_account,amount_lp_raw)
VALUES($1,$2,$3,$4,$5::text::numeric)`, e.Signature, e.Slot, e.Index, treasury.String(), strconv.FormatUint(e.minted, 10))
			if err != nil {
				return err
			}
			for _, a := range pending {
				n, held, err := realizeAssessment(ctx, tx, e, a, treasury)
				if err != nil {
					return err
				}
				out.Lots += n
				out.HeldWeights += held
			}
		}
		out.Harvests++
		reserve = 0
		pending = nil
	}
	// A corrupt/missing pending assessment must never be checkpointed even in
	// a batch without a harvest.
	sum := reserve
	for _, a := range pending {
		if ^uint64(0)-sum < a.Amount {
			return errors.New("pending amount overflow")
		}
		sum += a.Amount
	}
	if sum != observed.Accrued {
		return errors.New("outstanding assessments differ from current admin accumulator")
	}
	if dry {
		return nil
	}
	body, err := json.Marshal(observed)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE loyal_yield.referral_earn_max_fee_cursor SET verified_slot=$1,checked_slot=$1,state=$2,hold_reason=NULL WHERE singleton`, to, body)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func unharvestedFees(ctx context.Context, tx pgx.Tx) ([]assessedFee, error) {
	rows, err := tx.Query(ctx, `SELECT signature,slot,transaction_index,fee_lp_raw::text,effective_supply_raw::text,block_time
FROM loyal_yield.referral_earn_max_fee_assessments WHERE harvest_signature IS NULL ORDER BY slot,transaction_index`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pending []assessedFee
	for rows.Next() {
		var a assessedFee
		var amount, supply string
		if err = rows.Scan(&a.Signature, &a.Slot, &a.Index, &amount, &supply, &a.At); err != nil {
			return nil, err
		}
		if a.Amount, err = strconv.ParseUint(amount, 10, 64); err != nil {
			return nil, err
		}
		if a.Supply, err = strconv.ParseUint(supply, 10, 64); err != nil {
			return nil, err
		}
		pending = append(pending, a)
	}
	return pending, rows.Err()
}

func earnedFeeShares(old string, available, shares, supply uint64) (*big.Int, string, error) {
	carry, ok := new(big.Rat).SetString(old)
	if !ok || carry.Sign() < 0 || carry.Cmp(big.NewRat(1, 1)) >= 0 || supply == 0 || shares > supply {
		return nil, "", errors.New("invalid fractional referral inventory")
	}
	n := new(big.Int).Mul(new(big.Int).SetUint64(available), new(big.Int).SetUint64(shares))
	carry.Add(carry, new(big.Rat).SetFrac(n, new(big.Int).SetUint64(supply)))
	whole := new(big.Int).Quo(carry.Num(), carry.Denom())
	carry.Sub(carry, new(big.Rat).SetInt(whole))
	return whole, carry.RatString(), nil
}

func recordFeeAssessment(ctx context.Context, tx pgx.Tx, a assessedFee, block []solana.Signature, dry bool) (int, error) {
	keys := make([]string, len(block))
	for i, key := range block {
		keys[i] = key.String()
	}
	if !dry {
		_, err := tx.Exec(ctx, `INSERT INTO loyal_yield.referral_earn_max_fee_assessments
(signature,slot,transaction_index,effective_supply_raw,fee_lp_raw,block_time,block_signatures) VALUES($1,$2,$3,$4::text::numeric,$5::text::numeric,$6,$7)`,
			a.Signature, a.Slot, a.Index, strconv.FormatUint(a.Supply, 10), strconv.FormatUint(a.Amount, 10), a.At, keys)
		if err != nil {
			return 0, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT referee_app_user_id::text,referrer_app_user_id::text FROM loyal_yield.referral_bindings WHERE bound_at<=$1 ORDER BY referee_app_user_id`, a.At)
	if err != nil {
		return 0, err
	}
	ids, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (feeOwner, error) {
		var o feeOwner
		err := row.Scan(&o.Referee, &o.Referrer)
		return o, err
	})
	if err != nil {
		return 0, err
	}
	remaining, held := a.Supply, 0
	for _, id := range ids {
		o, err := feeOwnerForTx(ctx, tx, a.FeePoint, block, id.Referee)
		var shares, reason *string
		switch {
		case errors.Is(err, ErrFeeOwnershipUnproven):
			held++
			msg := err.Error()
			reason = &msg
		case err != nil:
			return 0, err
		case o.Referee != id.Referee || o.Referrer != id.Referrer || o.Shares > remaining:
			return 0, errors.New("overlapping share weights or changed binding")
		default:
			remaining -= o.Shares
			value := strconv.FormatUint(o.Shares, 10)
			shares = &value
		}
		if !dry {
			_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.referral_earn_max_fee_weights
(assessment_signature,referee_app_user_id,referrer_app_user_id,shares_raw,hold_reason) VALUES($1,$2,$3,$4::text::numeric,$5)`, a.Signature, id.Referee, id.Referrer, shares, reason)
			if err != nil {
				return 0, err
			}
		}
	}
	return held, nil
}

func realizeAssessment(ctx context.Context, tx pgx.Tx, h feeEvent, a assessedFee, treasury solana.PublicKey) (int, int, error) {
	tag, err := tx.Exec(ctx, "UPDATE loyal_yield.referral_earn_max_fee_assessments SET harvest_signature=$2 WHERE signature=$1 AND harvest_signature IS NULL", a.Signature, h.Signature)
	if err != nil {
		return 0, 0, err
	}
	if tag.RowsAffected() != 1 {
		return 0, 0, errors.New("assessment already collected")
	}
	rows, err := tx.Query(ctx, `SELECT referee_app_user_id::text,referrer_app_user_id::text,shares_raw::text
FROM loyal_yield.referral_earn_max_fee_weights WHERE assessment_signature=$1 AND shares_raw IS NOT NULL AND NOT realized ORDER BY referee_app_user_id`, a.Signature)
	if err != nil {
		return 0, 0, err
	}
	owners, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (feeOwner, error) {
		var o feeOwner
		var v string
		err := row.Scan(&o.Referee, &o.Referrer, &v)
		if err == nil {
			o.Shares, err = strconv.ParseUint(v, 10, 64)
		}
		return o, err
	})
	if err != nil {
		return 0, 0, err
	}
	n, held := 0, 0
	for _, o := range owners {
		lots, hold, err := realizeWeight(ctx, tx, h, a, treasury, o)
		if err != nil {
			return 0, 0, err
		}
		n += lots
		if hold {
			held++
		}
	}
	return n, held, nil
}

func realizeWeight(ctx context.Context, tx pgx.Tx, h feeEvent, a assessedFee, treasury solana.PublicKey, o feeOwner) (int, bool, error) {
	var inviter string
	var conflict bool
	if err := tx.QueryRow(ctx, `SELECT referrer_app_user_id::text,EXISTS(SELECT 1 FROM loyal_yield.referral_attribution_conflicts c WHERE c.referee_app_user_id=b.referee_app_user_id)
FROM loyal_yield.referral_bindings b WHERE referee_app_user_id=$1`, o.Referee).Scan(&inviter, &conflict); err != nil {
		return 0, false, err
	}
	if inviter != o.Referrer || conflict {
		_, err := tx.Exec(ctx, `UPDATE loyal_yield.referral_earn_max_fee_weights SET shares_raw=NULL,hold_reason='attribution changed before collection',last_attempt_at=clock_timestamp()
WHERE assessment_signature=$1 AND referee_app_user_id=$2 AND NOT realized`, a.Signature, o.Referee)
		return 0, true, err
	}
	n := 0
	if o.Shares > 0 {
		var old string
		err := tx.QueryRow(ctx, "SELECT remainder FROM loyal_yield.referral_earn_max_fee_remainders WHERE referee_app_user_id=$1", o.Referee).Scan(&old)
		if errors.Is(err, pgx.ErrNoRows) {
			old = "0"
		} else if err != nil {
			return 0, false, err
		}
		whole, fraction, err := earnedFeeShares(old, a.Amount, o.Shares, a.Supply)
		if err != nil {
			return 0, false, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.referral_earn_max_fee_remainders(referee_app_user_id,remainder) VALUES($1,$2)
ON CONFLICT(referee_app_user_id) DO UPDATE SET remainder=EXCLUDED.remainder`, o.Referee, fraction)
		if err != nil {
			return 0, false, err
		}
		if whole.Sign() > 0 {
			_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.referral_earn_max_fee_lots(referee_app_user_id,referrer_app_user_id,assessment_signature,assessment_slot,
harvest_signature,harvest_slot,vault,lp_mint,treasury_lp_account,amount_lp_raw) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::text::numeric)`,
				o.Referee, o.Referrer, a.Signature, a.Slot, h.Signature, h.Slot, earnmax.Vault.String(), earnmax.LPMint.String(), treasury.String(), whole.String())
			if err != nil {
				return 0, false, err
			}
			n++
		}
	}
	tag, err := tx.Exec(ctx, `UPDATE loyal_yield.referral_earn_max_fee_weights SET realized=true WHERE assessment_signature=$1 AND referee_app_user_id=$2 AND NOT realized AND shares_raw IS NOT NULL`, a.Signature, o.Referee)
	if err == nil && tag.RowsAffected() != 1 {
		return 0, false, errors.New("fee weight already realized")
	}
	return n, false, err
}

func (s *Store) retryFeeWeights(ctx context.Context) (int, int, error) {
	tx, err := s.yield.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock_shared(hashtextextended('referral-identity-evidence', 0))"); err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(ctx, "SELECT verified_slot FROM loyal_yield.referral_earn_max_fee_cursor WHERE singleton FOR UPDATE"); err != nil {
		return 0, 0, err
	}
	type retry struct {
		a                assessedFee
		referee, inviter string
		block            []string
		harvest          *string
		harvested        *uint64
	}
	rows, err := tx.Query(ctx, `SELECT a.signature,a.slot,a.transaction_index,a.block_time,a.fee_lp_raw::text,a.effective_supply_raw::text,a.block_signatures,
w.referee_app_user_id::text,w.referrer_app_user_id::text,h.signature,h.slot FROM loyal_yield.referral_earn_max_fee_weights w
JOIN loyal_yield.referral_earn_max_fee_assessments a ON a.signature=w.assessment_signature
LEFT JOIN loyal_yield.referral_earn_max_fee_harvests h ON h.signature=a.harvest_signature
WHERE w.hold_reason IS NOT NULL ORDER BY w.last_attempt_at,a.slot,a.transaction_index,w.referee_app_user_id LIMIT 16`)
	if err != nil {
		return 0, 0, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (retry, error) {
		var r retry
		var amt, sup string
		err := row.Scan(&r.a.Signature, &r.a.Slot, &r.a.Index, &r.a.At, &amt, &sup, &r.block, &r.referee, &r.inviter, &r.harvest, &r.harvested)
		if err == nil {
			r.a.Amount, err = strconv.ParseUint(amt, 10, 64)
		}
		if err == nil {
			r.a.Supply, err = strconv.ParseUint(sup, 10, 64)
		}
		return r, err
	})
	if err != nil {
		return 0, 0, err
	}
	treasury, err := feeTreasury()
	if err != nil {
		return 0, 0, err
	}
	n := 0
	for _, r := range items {
		block := make([]solana.Signature, len(r.block))
		for i, v := range r.block {
			block[i], err = solana.SignatureFromBase58(v)
			if err != nil {
				return 0, 0, err
			}
		}
		o, proofErr := feeOwnerForTx(ctx, tx, r.a.FeePoint, block, r.referee)
		if errors.Is(proofErr, ErrFeeOwnershipUnproven) {
			_, err = tx.Exec(ctx, `UPDATE loyal_yield.referral_earn_max_fee_weights SET hold_reason=$3,last_attempt_at=clock_timestamp() WHERE assessment_signature=$1 AND referee_app_user_id=$2 AND NOT realized`, r.a.Signature, r.referee, proofErr.Error())
			if err != nil {
				return 0, 0, err
			}
			continue
		}
		if proofErr != nil {
			return 0, 0, proofErr
		}
		if o.Referrer != r.inviter || o.Referee != r.referee {
			return 0, 0, errors.New("retry binding changed")
		}
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.referral_earn_max_fee_weights w SET shares_raw=$3::text::numeric,hold_reason=NULL,last_attempt_at=clock_timestamp()
WHERE assessment_signature=$1 AND referee_app_user_id=$2 AND NOT realized AND hold_reason IS NOT NULL
AND $3::text::numeric + (SELECT COALESCE(sum(shares_raw),0) FROM loyal_yield.referral_earn_max_fee_weights WHERE assessment_signature=$1) <= $4::text::numeric`, r.a.Signature, r.referee, strconv.FormatUint(o.Shares, 10), strconv.FormatUint(r.a.Supply, 10))
		if err != nil {
			return 0, 0, err
		}
		if tag.RowsAffected() != 1 {
			return 0, 0, errors.New("recovered participant weights exceed vault supply")
		}
		if r.harvest != nil && r.harvested != nil {
			count, _, err := realizeWeight(ctx, tx, feeEvent{FeePoint: FeePoint{Signature: *r.harvest, Slot: *r.harvested}}, r.a, treasury, o)
			if err != nil {
				return 0, 0, err
			}
			n += count
		}
	}
	return len(items), n, tx.Commit(ctx)
}

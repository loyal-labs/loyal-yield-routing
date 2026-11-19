package referral

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/solana-foundation/solana-go/v2"
)

type feeOwner struct {
	Referee, Referrer string
	Shares            uint64
}

type feeOwnership struct {
	owner            feeOwner
	start, throughAt time.Time
	expected         capitalReplay
	replay           capitalReplay
	lastSlot         uint64
	pastAssessment   bool
}

func feeBlockOrder(p FeePoint, block []solana.Signature) (map[string]int, []string, error) {
	if p.Slot == 0 || p.At.IsZero() || p.Index < 0 || p.Index >= len(block) || block[p.Index].String() != p.Signature {
		return nil, nil, errors.New("assessment missing at finalized block index")
	}
	order := make(map[string]int, len(block))
	keys := make([]string, len(block))
	for i, sig := range block {
		key := sig.String()
		if _, exists := order[key]; exists || sig.IsZero() {
			return nil, nil, errors.New("ambiguous finalized block signature list")
		}
		order[key], keys[i] = i, key
	}
	return order, keys, nil
}

func (o *feeOwnership) beforeAssessment() {
	if o.pastAssessment {
		return
	}
	o.pastAssessment = true
	// Replay holds mixed or unknown origins before certification. An active
	// account's free shares are therefore all fee-net qualifying deposits.
	if o.replay.Active.Sign() > 0 {
		o.owner.Shares = o.replay.Free
	}
}

func (o *feeOwnership) apply(p FeePoint, order map[string]int, slot uint64, sig string, m earnmax.CapitalMovement) error {
	if slot < o.lastSlot || (slot == o.lastSlot && slot != p.Slot) {
		return errors.New("capital transaction ordering unproved")
	}
	o.lastSlot = slot
	before := slot < p.Slot
	if slot == p.Slot {
		index, exists := order[sig]
		if !exists {
			return errors.New("capital transaction missing from finalized assessment block")
		}
		if index == p.Index {
			return errors.New("capital movement in the fee assessment transaction")
		}
		before = index < p.Index
	}
	if !before {
		o.beforeAssessment()
	} else if o.pastAssessment {
		return errors.New("capital transactions out of order across assessment")
	}
	if len(m.Events) == 0 {
		return errors.New("capital fact without movements")
	}
	for _, e := range m.Events {
		when := time.Unix(e.UnixTime, 0)
		if (before && when.After(p.At)) || (!before && when.Before(p.At)) {
			return errors.New("capital event time disagrees with assessment order")
		}
		if e.Kind == "deposit" && !when.Before(o.start) && !m.BoundWalletSigned {
			return errors.New("capital deposit owner unproved")
		}
	}
	return o.replay.apply(m, o.start)
}

func (o *feeOwnership) finish() (feeOwner, error) {
	o.beforeAssessment()
	if err := o.replay.accrue(o.throughAt.Unix()); err != nil {
		return feeOwner{}, err
	}
	a, b := &o.replay, &o.expected
	if a.At != b.At || a.LastEventAt != b.LastEventAt || a.Free != b.Free || a.Escrow != b.Escrow || a.Active.Cmp(&b.Active) != 0 || a.Reserved.Cmp(&b.Reserved) != 0 || a.Earned.Cmp(&b.Earned) != 0 {
		return feeOwner{}, errors.New("capital fact history disagrees with its certified checkpoint")
	}
	return o.owner, nil
}

// ErrFeeOwnershipUnproven holds this friend's fixed-denominator share for later proof.
var ErrFeeOwnershipUnproven = errors.New("referee share ownership is unproven")

func feeOwnerForTx(ctx context.Context, tx pgx.Tx, p FeePoint, block []solana.Signature, refereeID string) (feeOwner, error) {
	var empty feeOwner
	order, keys, err := feeBlockOrder(p, block)
	if err != nil {
		return empty, err
	}
	var launch uint64
	var began time.Time
	if err = tx.QueryRow(ctx, `SELECT launch_slot,launched_at FROM loyal_yield.referral_earn_max_program WHERE singleton`).Scan(&launch, &began); err != nil {
		return empty, fmt.Errorf("read referral program launch: %w", err)
	}
	if launch > p.Slot || began.After(p.At) {
		return empty, errors.New("fee assessment precedes referral launch")
	}
	var current feeOwnership
	var bound time.Time
	var conflict bool
	var expected, anchor []byte
	var throughAt *time.Time
	var authority *string
	var throughSlot, zeroSlot *uint64
	err = tx.QueryRow(ctx, `SELECT b.referee_app_user_id::text,b.referrer_app_user_id::text,b.bound_at,s.authority,
		CASE WHEN next.zero_slot IS NOT NULL THEN next.prior_through_slot ELSE s.earned_through_slot END,
		CASE WHEN next.zero_slot IS NOT NULL THEN next.prior_through_at ELSE s.earned_through_at END,
		CASE WHEN next.zero_slot IS NOT NULL THEN next.prior_replay ELSE s.replay END,
		prev.zero_slot,prev.zero_replay,
		EXISTS(SELECT FROM loyal_yield.referral_attribution_conflicts c WHERE c.referee_app_user_id=b.referee_app_user_id)
		FROM loyal_yield.referral_bindings b LEFT JOIN loyal_yield.referral_earn_max_capital_status s USING(referee_app_user_id)
		LEFT JOIN LATERAL (SELECT prior_through_slot,prior_through_at,prior_replay,zero_slot
		FROM loyal_yield.referral_earn_max_capital_resets r WHERE r.referee_app_user_id=b.referee_app_user_id AND r.authority=s.authority AND r.zero_slot >= $2
		ORDER BY r.zero_slot LIMIT 1) next ON true
		LEFT JOIN LATERAL (SELECT zero_slot,zero_replay FROM loyal_yield.referral_earn_max_capital_resets r
		WHERE r.referee_app_user_id=b.referee_app_user_id AND r.authority=s.authority AND r.zero_slot<$2 ORDER BY r.zero_slot DESC LIMIT 1) prev ON true
		WHERE b.referee_app_user_id=$1 AND b.bound_at<=$3`, refereeID, p.Slot, p.At).Scan(&current.owner.Referee, &current.owner.Referrer, &bound, &authority, &throughSlot, &throughAt, &expected, &zeroSlot, &anchor, &conflict)
	if errors.Is(err, pgx.ErrNoRows) {
		return empty, fmt.Errorf("%w: binding unavailable at assessment", ErrFeeOwnershipUnproven)
	}
	if err != nil {
		return empty, err
	}
	if conflict || throughAt == nil || authority == nil || throughSlot == nil || *throughSlot < p.Slot || throughAt.Before(p.At) {
		return current.owner, fmt.Errorf("%w: missing capital coverage or attribution conflict", ErrFeeOwnershipUnproven)
	}
	current.start = began
	if bound.After(began) {
		current.start = bound
	}
	current.throughAt = *throughAt
	from := launch - 1
	current.replay.At = current.start.Unix()
	if zeroSlot != nil {
		from = *zeroSlot
		if err = json.Unmarshal(anchor, &current.replay); err != nil || current.replay.Free != 0 || current.replay.Escrow != 0 || current.replay.Active.Sign() != 0 || current.replay.Reserved.Sign() != 0 {
			return current.owner, fmt.Errorf("%w: invalid zero checkpoint", ErrFeeOwnershipUnproven)
		}
	}
	if err = json.Unmarshal(expected, &current.expected); err != nil {
		return current.owner, fmt.Errorf("%w: malformed certified replay", ErrFeeOwnershipUnproven)
	}
	rows, err := tx.Query(ctx, `SELECT slot,signature,movement FROM loyal_yield.referral_earn_max_capital_facts
	WHERE referee_app_user_id=$1 AND authority=$2 AND slot>$3 AND slot<=$4
	ORDER BY slot,CASE WHEN slot=$5 THEN array_position($6::text[],signature) END`, refereeID, *authority, from, *throughSlot, p.Slot, keys)
	if err != nil {
		return empty, err
	}
	defer rows.Close()
	for rows.Next() {
		var slot uint64
		var sig string
		var body []byte
		if err = rows.Scan(&slot, &sig, &body); err != nil {
			return empty, err
		}
		var m earnmax.CapitalMovement
		if err = json.Unmarshal(body, &m); err != nil {
			return current.owner, fmt.Errorf("%w: malformed capital movement", ErrFeeOwnershipUnproven)
		}
		if err = current.apply(p, order, slot, sig, m); err != nil {
			return current.owner, fmt.Errorf("%w: %v", ErrFeeOwnershipUnproven, err)
		}
	}
	if err = rows.Err(); err != nil {
		return empty, err
	}
	owner, err := current.finish()
	if err != nil {
		return current.owner, fmt.Errorf("%w: %v", ErrFeeOwnershipUnproven, err)
	}
	return owner, nil
}

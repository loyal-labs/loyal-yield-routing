package referral

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

type settlementReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

const settlementFields = `SELECT settlement_id::text,lot_id,referee_app_user_id::text,referrer_app_user_id::text,state,wallet,settings,recipient_owner,recipient_ata,treasury_owner,treasury_ata,lp_mint,amount_lp_raw::text,quoted_micro_usdc::text,quote_asset_micro::text,quote_supply_raw::text,quote_slot,quote_at,nav_report_at,earned_through_slot,last_valid_height,unsigned_wire,signature,finalized_slot,signed_wire FROM loyal_yield.referral_earn_max_settlements `

func readSettlement(row pgx.Row) (Settlement, []byte, error) {
	var p Settlement
	var signed []byte
	var lp string
	err := row.Scan(&p.ID, &p.LotID, &p.Referee, &p.Inviter, &p.State, &p.Wallet, &p.Settings, &p.RecipientOwner, &p.RecipientATA, &p.TreasuryOwner, &p.TreasuryATA, &p.Mint, &lp, &p.MicroUSDC, &p.QuoteAssetMicro, &p.QuoteSupplyRaw, &p.QuoteSlot, &p.QuoteAt, &p.NAVReportAt, &p.EarnedThroughSlot, &p.LastValidHeight, &p.UnsignedWire, &p.Signature, &p.FinalizedSlot, &signed)
	if err == nil {
		p.LPRaw, err = strconv.ParseUint(lp, 10, 64)
	}
	return p, signed, err
}
func settlementByID(ctx context.Context, r settlementReader, id string) (Settlement, []byte, error) {
	return readSettlement(r.QueryRow(ctx, settlementFields+"WHERE settlement_id=$1", id))
}
func openSettlement(ctx context.Context, r settlementReader, referee string) (Settlement, error) {
	p, _, err := readSettlement(r.QueryRow(ctx, settlementFields+"WHERE referee_app_user_id=$1 AND state IN ('prepared','signed')", referee))
	return p, err
}

func lockSettlement(ctx context.Context, tx pgx.Tx, referee, treasury string) error {
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout=10000"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended('referral-identity-evidence',0))`); err != nil {
		return err
	}
	for _, key := range []string{"referral-settlement-referee:" + referee, "referral-settlement-treasury:" + treasury} {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, key); err != nil {
			return err
		}
	}
	return nil
}
func confirmedCaps(ctx context.Context, tx pgx.Tx, p Settlement) (*big.Int, *big.Int, uint64, error) {
	var owed, unused string
	var through uint64
	err := tx.QueryRow(ctx, `SELECT
  (c.earned_micro_usdc - (SELECT COALESCE(sum(quoted_micro_usdc),0) FROM loyal_yield.referral_earn_max_settlements WHERE referee_app_user_id=l.referee_app_user_id AND state='delivered'))::text,
  (l.amount_lp_raw - (SELECT COALESCE(sum(amount_lp_raw),0) FROM loyal_yield.referral_earn_max_settlements WHERE lot_id=l.lot_id AND state='delivered'))::text,
  c.earned_through_slot
 FROM loyal_yield.referral_earn_max_fee_lots l
 JOIN loyal_yield.referral_bindings b ON (b.referee_app_user_id,b.referrer_app_user_id)=(l.referee_app_user_id,l.referrer_app_user_id)
 JOIN loyal_yield.referral_earn_max_capital_status c ON c.referee_app_user_id=b.referee_app_user_id
 JOIN loyal_yield.referral_earn_max_program program ON program.singleton AND program.launch_slot<=l.harvest_slot
 WHERE l.lot_id=$1 AND l.referee_app_user_id=$2 AND l.referrer_app_user_id=$3 AND l.vault=$4 AND l.lp_mint=$5 AND l.treasury_lp_account=$6
 AND c.earned_micro_usdc IS NOT NULL AND c.earned_through_slot>=l.harvest_slot
 AND NOT EXISTS (SELECT 1 FROM loyal_yield.referral_attribution_conflicts x WHERE x.referee_app_user_id=b.referee_app_user_id)
 FOR SHARE OF c`, p.LotID, p.Referee, p.Inviter, earnmax.Vault.String(), earnmax.LPMint.String(), p.TreasuryATA).Scan(&owed, &unused, &through)
	if err != nil {
		return nil, nil, 0, err
	}
	due, ok := new(big.Int).SetString(owed, 10)
	if !ok {
		return nil, nil, 0, errors.New("invalid earned balance")
	}
	left, ok := new(big.Int).SetString(unused, 10)
	if !ok {
		return nil, nil, 0, errors.New("invalid harvested balance")
	}
	return due, left, through, nil
}
func custodyRemaining(ctx context.Context, tx pgx.Tx, source, id string, balance, observedSlot uint64) (*big.Int, error) {
	// Whole lots can contain carry from earlier harvests. Protect the whole
	// treasury's assessed ownership including fractional entitlements. An
	// unresolved owner temporarily reserves that assessment's full fee.
	// Round each assessed fraction upward at 10^-18 of one *raw* LP; only the
	// final sum is rounded to whole raw shares for the SPL balance check.
	var raw, required string
	var latest uint64
	err := tx.QueryRow(ctx, `WITH assessed AS (
	  SELECT a.fee_lp_raw AS fee, a.effective_supply_raw AS supply,
	    COALESCE(sum(w.shares_raw),0) AS shares,
	    COALESCE(bool_or(NOT w.realized),false) AS unresolved
	  FROM loyal_yield.referral_earn_max_fee_assessments a
	  JOIN loyal_yield.referral_earn_max_fee_harvests h ON h.signature=a.harvest_signature
	  LEFT JOIN loyal_yield.referral_earn_max_fee_weights w ON w.assessment_signature=a.signature
	  WHERE h.treasury_lp_account=$1
	  GROUP BY a.signature
	), backing AS (
	  SELECT COALESCE(sum(CASE WHEN unresolved THEN fee*1000000000000000000
	    ELSE div(fee*shares*1000000000000000000+supply-1,supply) END),0) AS scaled FROM assessed
	)
	SELECT COALESCE(sum(amount_lp_raw) FILTER (WHERE settlement_id<>$2 AND
	         (state IN ('prepared','signed') OR (state='delivered' AND finalized_slot>$3))),0)::text,
	       (greatest((SELECT div(scaled + 999999999999999999,1000000000000000000) FROM backing),
	         (SELECT COALESCE(sum(amount_lp_raw),0) FROM loyal_yield.referral_earn_max_fee_lots WHERE treasury_lp_account=$1)) -
	         COALESCE(sum(amount_lp_raw) FILTER(WHERE state='delivered' AND finalized_slot<=$3),0))::text,
	       (SELECT COALESCE(max(slot),0) FROM loyal_yield.referral_earn_max_fee_harvests WHERE treasury_lp_account=$1)
	FROM loyal_yield.referral_earn_max_settlements WHERE treasury_ata=$1`, source, id, observedSlot).Scan(&raw, &required, &latest)
	if err != nil {
		return nil, err
	}
	minimum, ok := new(big.Int).SetString(required, 10)
	if !ok || minimum.Sign() < 0 || latest > observedSlot || minimum.Cmp(new(big.Int).SetUint64(balance)) > 0 {
		return nil, errors.New("treasury snapshot does not cover all individually funded and unresolved fee shares")
	}
	held, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return nil, errors.New("invalid treasury reservation")
	}
	return new(big.Int).Sub(new(big.Int).SetUint64(balance), held), nil
}
func mainnetSettlement(ctx context.Context, c *chain.Client) error {
	genesis, err := c.GenesisHash(ctx)
	if err != nil {
		return err
	}
	if genesis.String() != "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d" {
		return errors.New("settlement requires Solana mainnet")
	}
	return nil
}

// PrepareSettlement returns the same live reservation for a retry. No signer or
// broadcast capability is used at any stage of the settlement operator.
func (s *Store) PrepareSettlement(ctx context.Context, c *chain.Client, lot int64) (Settlement, error) {
	p := Settlement{LotID: lot, ID: uuid.NewString(), Mint: earnmax.LPMint.String(), State: "prepared"}
	var harvest uint64
	err := s.yield.QueryRow(ctx, `SELECT referee_app_user_id::text,referrer_app_user_id::text,treasury_lp_account,harvest_slot FROM loyal_yield.referral_earn_max_fee_lots WHERE lot_id=$1 AND vault=$2 AND lp_mint=$3`, lot, earnmax.Vault.String(), p.Mint).Scan(&p.Referee, &p.Inviter, &p.TreasuryATA, &harvest)
	if err != nil {
		return Settlement{}, err
	}
	if open, err := openSettlement(ctx, s.yield, p.Referee); err == nil {
		if open.LotID != lot {
			return Settlement{}, errors.New("another lot has an open settlement for this friend")
		}
		return open, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Settlement{}, err
	}
	if err = mainnetSettlement(ctx, c); err != nil {
		return Settlement{}, err
	}
	q, err := s.settlementSnapshot(ctx, c, p.Inviter, p.TreasuryATA, harvest)
	if err != nil {
		return Settlement{}, err
	}
	hash, lastValid, slot, err := c.Blockhash(ctx, rpc.CommitmentFinalized, q.slot)
	if err != nil {
		return Settlement{}, err
	}
	p.LastValidHeight = lastValid
	p.QuoteSlot = q.slot
	p.QuoteAt = q.at
	p.NAVReportAt = q.reported
	p.QuoteAssetMicro = new(big.Int).SetUint64(q.asset).String()
	p.QuoteSupplyRaw = q.supply.String()
	p.Wallet = q.wallet.String()
	p.Settings = q.settings.String()
	p.RecipientOwner = q.recipient.String()
	p.RecipientATA = q.destination.String()
	p.TreasuryOwner = q.treasury.String()
	err = db.WithTx(ctx, s.yield, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := lockSettlement(ctx, tx, p.Referee, p.TreasuryATA); err != nil {
			return err
		}
		if open, err := openSettlement(ctx, tx, p.Referee); err == nil {
			if open.LotID != lot {
				return errors.New("another lot has an open settlement for this friend")
			}
			p = open
			return nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if !quoteCurrent(slot, time.Now().UTC(), p.QuoteSlot, p.QuoteAt) {
			return errors.New("finalized settlement quote is stale")
		}
		owed, left, through, err := confirmedCaps(ctx, tx, p)
		if err != nil {
			return err
		}
		p.EarnedThroughSlot = through
		if through > p.QuoteSlot {
			return errors.New("refresh settlement quote after capital checkpoint")
		}
		custody, err := custodyRemaining(ctx, tx, p.TreasuryATA, p.ID, q.balance, q.slot)
		if err != nil {
			return err
		}
		if custody.Cmp(left) < 0 {
			left = custody
		}
		lp, paid, err := settlementSize(owed, left, q.supply, q.asset)
		if err != nil {
			return err
		}
		p.LPRaw = lp
		p.MicroUSDC = paid.String()
		p.UnsignedWire, err = settlementWire(p, q, hash)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.referral_earn_max_settlements
   (settlement_id,lot_id,referee_app_user_id,referrer_app_user_id,state,wallet,settings,recipient_owner,recipient_ata,treasury_owner,treasury_ata,lp_mint,amount_lp_raw,quoted_micro_usdc,quote_asset_micro,quote_supply_raw,quote_slot,quote_at,nav_report_at,earned_through_slot,last_valid_height,unsigned_wire)
   VALUES($1,$2,$3,$4,'prepared',$5,$6,$7,$8,$9,$10,$11,$12::text::numeric,$13::text::numeric,$14::text::numeric,$15::text::numeric,$16,$17,$18,$19,$20,$21)`, p.ID, p.LotID, p.Referee, p.Inviter, p.Wallet, p.Settings, p.RecipientOwner, p.RecipientATA, p.TreasuryOwner, p.TreasuryATA, p.Mint, new(big.Int).SetUint64(p.LPRaw).String(), p.MicroUSDC, p.QuoteAssetMicro, p.QuoteSupplyRaw, p.QuoteSlot, p.QuoteAt, p.NAVReportAt, p.EarnedThroughSlot, p.LastValidHeight, p.UnsignedWire)
		return err
	})
	return p, err
}

func (s *Store) RegisterSettlement(ctx context.Context, c *chain.Client, id string, wire []byte) (Settlement, error) {
	p, prior, err := settlementByID(ctx, s.yield, id)
	if err != nil {
		return p, err
	}
	signature, err := verifySettlementWire(p.UnsignedWire, wire)
	if err != nil {
		return p, err
	}
	if len(prior) > 0 {
		if bytes.Equal(prior, wire) {
			return p, nil
		}
		return p, errors.New("different signed wire already registered")
	}
	if err = mainnetSettlement(ctx, c); err != nil {
		return p, err
	}
	q, err := s.settlementSnapshot(ctx, c, p.Inviter, p.TreasuryATA, p.QuoteSlot)
	if err != nil {
		return p, err
	}
	if q.wallet.String() != p.Wallet || q.settings.String() != p.Settings || q.recipient.String() != p.RecipientOwner || q.destination.String() != p.RecipientATA || q.treasury.String() != p.TreasuryOwner {
		return p, errors.New("settlement destination or treasury changed")
	}
	signedTx, err := solana.TransactionFromBytes(wire)
	if err != nil {
		return p, err
	}
	valid, nowSlot, err := c.BlockhashValid(ctx, signedTx.Message.RecentBlockhash, rpc.CommitmentFinalized, q.slot)
	if err != nil {
		return p, err
	}
	if !valid || !quoteCurrent(nowSlot, q.at, p.QuoteSlot, p.QuoteAt) {
		return p, errors.New("settlement blockhash or quote expired; funds remain reserved")
	}
	newRegistration := false
	err = db.WithTx(ctx, s.yield, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := lockSettlement(ctx, tx, p.Referee, p.TreasuryATA); err != nil {
			return err
		}
		current, old, err := settlementByID(ctx, tx, id)
		if err != nil {
			return err
		}
		if current.State != "prepared" {
			if bytes.Equal(old, wire) {
				p = current
				return nil
			}
			return errors.New("settlement already signed")
		}
		if !quoteCurrent(nowSlot, time.Now().UTC(), p.QuoteSlot, p.QuoteAt) {
			return errors.New("settlement quote expired; funds remain reserved")
		}
		owed, left, through, err := confirmedCaps(ctx, tx, p)
		if err != nil {
			return err
		}
		custody, err := custodyRemaining(ctx, tx, p.TreasuryATA, p.ID, q.balance, q.slot)
		if err != nil {
			return err
		}
		raw := new(big.Int).SetUint64(p.LPRaw)
		paid, ok := new(big.Int).SetString(p.MicroUSDC, 10)
		if !ok || through < p.EarnedThroughSlot || raw.Cmp(left) > 0 || raw.Cmp(custody) > 0 || paid.Cmp(owed) > 0 {
			return errors.New("earned, collected or custody cap changed")
		}
		_, err = tx.Exec(ctx, `UPDATE loyal_yield.referral_earn_max_settlements SET state='signed',signed_wire=$2,signature=$3 WHERE settlement_id=$1`, id, wire, signature.String())
		newRegistration = err == nil
		return err
	})
	if err != nil {
		return p, err
	}
	p, _, err = settlementByID(ctx, s.yield, id)
	p.NewRegistration = newRegistration && err == nil && p.State == "signed"
	return p, err
}

func (s *Store) ReconcileSettlement(ctx context.Context, c *chain.Client, id string) (Settlement, error) {
	p, wire, err := settlementByID(ctx, s.yield, id)
	if err != nil {
		return p, err
	}
	if p.State == "delivered" || p.State == "failed" || p.State == "expired" {
		return p, nil
	}
	if p.State != "signed" || p.Signature == nil {
		return p, errors.New("settlement has no registered signed wire")
	}
	if err = mainnetSettlement(ctx, c); err != nil {
		return p, err
	}
	sig, err := solana.SignatureFromBase58(*p.Signature)
	if err != nil {
		return p, err
	}
	ex, err := c.Execution(ctx, sig, rpc.CommitmentFinalized)
	if err != nil {
		return p, err
	}
	state, proof, err := settlementReceipt(p, wire, ex)
	if err != nil {
		return p, err
	}
	// A later attribution hold must not erase a transfer that already finalized.
	_, err = s.yield.Exec(ctx, `UPDATE loyal_yield.referral_earn_max_settlements SET state=$2,finalized_slot=$3,finalized_at=$4,receipt=$5 WHERE settlement_id=$1 AND state='signed' AND signature=$6`, id, state, ex.Slot, *ex.BlockTime, proof, *p.Signature)
	if err != nil {
		return p, err
	}
	p, _, err = settlementByID(ctx, s.yield, id)
	return p, err
}

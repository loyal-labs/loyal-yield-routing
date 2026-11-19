package referral

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"

	"github.com/solana-foundation/solana-go/v2/rpc"
)

const settlementQuoteSlots = 32
const settlementQuoteAge = 30 * time.Second

var settlementMemo = solana.MustPublicKeyFromBase58("MemoSq4gqABAXKb96qnH8TysNcWxMyWCqXgDLGmfcHr")

// LP settlement fixes USDC value at reservation. Both the historical quote and
// the later finalized delivery slot are retained, even when the LP price moves.
type Settlement struct {
	NewRegistration   bool      `json:"new_registration,omitempty"`
	ID                string    `json:"settlement_id"`
	LotID             int64     `json:"lot_id,string"`
	Referee           string    `json:"referee_app_user_id"`
	Inviter           string    `json:"referrer_app_user_id"`
	State             string    `json:"state"`
	Wallet            string    `json:"inviter_wallet"`
	Settings          string    `json:"settings"`
	RecipientOwner    string    `json:"recipient_owner"`
	RecipientATA      string    `json:"recipient_ata"`
	TreasuryOwner     string    `json:"treasury_owner"`
	TreasuryATA       string    `json:"treasury_ata"`
	Mint              string    `json:"lp_mint"`
	LPRaw             uint64    `json:"amount_lp_raw,string"`
	MicroUSDC         string    `json:"quoted_micro_usdc"`
	QuoteAssetMicro   string    `json:"quote_asset_micro"`
	QuoteSupplyRaw    string    `json:"quote_supply_raw"`
	QuoteSlot         uint64    `json:"quote_slot"`
	QuoteAt           time.Time `json:"quote_at"`
	NAVReportAt       time.Time `json:"nav_report_at"`
	EarnedThroughSlot uint64    `json:"earned_through_slot"`
	LastValidHeight   uint64    `json:"last_valid_block_height"`
	UnsignedWire      []byte    `json:"unsigned_wire"`
	Signature         *string   `json:"signature,omitempty"`
	FinalizedSlot     *uint64   `json:"finalized_slot,omitempty"`
}

type settlementSnapshot struct {
	wallet, settings, recipient, destination, treasury, source solana.PublicKey
	slot, asset, balance                                       uint64
	supply                                                     *big.Int
	decimals                                                   uint8
	at, reported                                               time.Time
}

func (s *Store) settlementSnapshot(ctx context.Context, c *chain.Client, inviter, source string, minSlot uint64) (settlementSnapshot, error) {
	var q settlementSnapshot
	var walletText string
	var settingsNames []string
	var shared bool
	err := db.WithTx(ctx, s.app, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout=10000"); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT u.subject_address, array_agg(DISTINCT s.settings_pda),
		  bool_or(EXISTS(SELECT 1 FROM app_user_smart_accounts x WHERE x.solana_env=s.solana_env AND x.settings_pda=s.settings_pda AND x.user_id<>s.user_id))
		  FROM app_users u JOIN app_user_smart_accounts s ON s.user_id=u.id
		  WHERE u.id=$1 AND u.provider='solana' AND s.solana_env='mainnet' AND s.state='ready' AND s.settings_pda IS NOT NULL
		  GROUP BY u.subject_address`, inviter).Scan(&walletText, &settingsNames, &shared)
	})
	if err != nil {
		return q, err
	}
	if shared || len(settingsNames) != 1 {
		return q, errors.New("inviter has no unambiguous ready Earn Max account")
	}
	if q.wallet, err = solana.PublicKeyFromBase58(walletText); err != nil {
		return q, err
	}
	if q.settings, err = solana.PublicKeyFromBase58(settingsNames[0]); err != nil {
		return q, err
	}
	if q.source, err = solana.PublicKeyFromBase58(source); err != nil {
		return q, err
	}
	auth, err := earnmax.AuthorityOf(q.settings)
	if err != nil {
		return q, err
	}
	q.recipient = auth.Key()
	if q.destination, err = spl.AssociatedTokenAddress(q.recipient, earnmax.LPMint, solana.TokenProgramID); err != nil {
		return q, err
	}
	if q.destination == q.source {
		return q, errors.New("treasury cannot be the payout destination")
	}
	keys := []solana.PublicKey{solana.SysVarClockPubkey, earnmax.Vault, earnmax.LPMint, q.settings, q.source, q.destination}
	slot, a, err := c.Accounts(ctx, keys, rpc.CommitmentFinalized, minSlot)
	if err != nil {
		return q, err
	}
	if len(a) != len(keys) || a[0] == nil || a[0].Owner != solana.SysVarPubkey || a[0].Executable || len(a[0].Data) != 40 || binary.LittleEndian.Uint64(a[0].Data) != slot {
		return q, errors.New("missing finalized settlement clock")
	}
	q.slot = slot
	q.at = time.Unix(int64(binary.LittleEndian.Uint64(a[0].Data[32:])), 0).UTC()
	vault, err := voltr.DecodeVault(a[1])
	if err != nil {
		return q, err
	}
	mint, err := spl.DecodeMint(a[2])
	if err != nil {
		return q, err
	}
	if vault.LPMint != earnmax.LPMint || vault.AssetMint != earnmax.AssetMint || mint.Program != solana.TokenProgramID || vault.TotalValueRaw == 0 {
		return q, errors.New("wrong vault, mint, or empty NAV")
	}
	q.asset = vault.TotalValueRaw
	q.decimals = mint.Decimals
	q.supply, err = vault.TotalLP(binary.LittleEndian.Uint64(a[2].Data[36:44]), q.at)
	if err != nil {
		return q, err
	}
	q.reported = time.Unix(int64(vault.LastLockedProfitReportUnix), 0).UTC()
	if q.supply.Sign() <= 0 || q.reported.After(q.at) || q.at.Sub(q.reported) > time.Hour {
		return q, errors.New("published NAV unavailable or older than the routine reporting interval")
	}
	cfg, err := squads.DecodeSettings(a[3])
	if err != nil {
		return q, err
	}
	settingsKey, bump, err := squads.SettingsAddress(cfg.Seed)
	if err != nil || settingsKey != q.settings || bump != cfg.Bump {
		return q, errors.New("invalid inviter settings derivation")
	}
	controls := false
	for _, member := range cfg.Signers {
		if member.Key == q.wallet && member.Permissions == squads.FullPermissions && cfg.Threshold == 1 {
			controls = true
		}
	}
	if !controls {
		return q, errors.New("inviter wallet does not control the destination settings")
	}
	src, err := spl.DecodeTokenAccount(a[4])
	if err != nil {
		return q, err
	}
	q.treasury = vault.Admin
	ata, err := spl.AssociatedTokenAddress(q.treasury, earnmax.LPMint, solana.TokenProgramID)
	if err != nil {
		return q, err
	}
	if !q.treasury.IsOnCurve() || ata != q.source || src.Program != solana.TokenProgramID || src.Mint != earnmax.LPMint || src.Owner != q.treasury || src.Frozen {
		return q, errors.New("harvested lot is not in the current direct-signing admin treasury LP ATA")
	}
	q.balance = src.Amount
	if a[5] != nil {
		dst, err := spl.DecodeTokenAccount(a[5])
		if err != nil {
			return q, err
		}
		if dst.Program != solana.TokenProgramID || dst.Mint != earnmax.LPMint || dst.Owner != q.recipient || dst.Frozen {
			return q, errors.New("invalid inviter receiving account")
		}
	}
	return q, nil
}

func quoteCurrent(slot uint64, at time.Time, planSlot uint64, planAt time.Time) bool {
	return slot >= planSlot && slot-planSlot <= settlementQuoteSlots && !at.Before(planAt) && at.Sub(planAt) <= settlementQuoteAge
}

// Reserve only whole shares whose fixed value fits the cumulative USDC cap.
// Charging the ceiling to whole micro-USDC avoids repeated sub-micro overdelivery.
func settlementSize(owed, available, supply *big.Int, value uint64) (uint64, *big.Int, error) {
	if owed.Sign() <= 0 || available.Sign() <= 0 || supply.Sign() <= 0 || value == 0 {
		return 0, nil, errors.New("no payable whole shares")
	}
	v := new(big.Int).SetUint64(value)
	lp := new(big.Int).Quo(new(big.Int).Mul(owed, supply), v)
	if lp.Cmp(available) > 0 {
		lp.Set(available)
	}
	if lp.Sign() <= 0 || !lp.IsUint64() {
		return 0, nil, errors.New("no payable whole shares within the confirmed caps")
	}
	num := new(big.Int).Mul(lp, v)
	paid := new(big.Int).Quo(num.Add(num, new(big.Int).Sub(supply, big.NewInt(1))), supply)
	return lp.Uint64(), paid, nil
}

func settlementWire(p Settlement, q settlementSnapshot, hash solana.Hash) ([]byte, error) {
	ix := spl.TransferChecked(solana.TokenProgramID, q.source, earnmax.LPMint, q.destination, q.treasury, p.LPRaw, q.decimals)
	memo := fmt.Sprintf("Loyal referral %s lot=%d quote=%d:%s/%s microUSDC=%s", p.ID, p.LotID, p.QuoteSlot, p.QuoteAssetMicro, p.QuoteSupplyRaw, p.MicroUSDC)
	ops := []solana.Instruction{spl.CreateIdempotentATA(q.treasury, q.recipient, earnmax.LPMint, solana.TokenProgramID), ix, solana.NewInstruction(settlementMemo, nil, []byte(memo))}
	tx, err := solana.NewTransaction(ops, hash, solana.TransactionPayer(q.treasury))
	if err != nil {
		return nil, err
	}
	wire, err := tx.MarshalBinary()
	if err != nil {
		return nil, err
	}
	if len(wire) > 1232 {
		return nil, errors.New("settlement transaction exceeds wire limit")
	}
	return wire, nil
}

// An operator may register only the exact signed version of the reserved message.
func verifySettlementWire(unsigned, signed []byte) (solana.Signature, error) {
	var none solana.Signature
	if len(signed) > 1232 {
		return none, errors.New("oversized signed settlement")
	}
	expected, err := solana.TransactionFromBytes(unsigned)
	if err != nil {
		return none, err
	}
	tx, err := solana.TransactionFromBytes(signed)
	if err != nil {
		return none, err
	}
	want, err := expected.Message.MarshalBinary()
	if err != nil {
		return none, err
	}
	got, err := tx.Message.MarshalBinary()
	if err != nil {
		return none, err
	}
	canonical, err := tx.MarshalBinary()
	if err != nil {
		return none, err
	}
	if len(tx.Signatures) != 1 || !bytes.Equal(signed, canonical) || !bytes.Equal(want, got) {
		return none, errors.New("signed wire differs from reserved settlement")
	}
	if err = tx.VerifySignatures(); err != nil {
		return none, err
	}
	return tx.Signatures[0], nil
}

func settlementReceipt(p Settlement, signed []byte, ex chain.Execution) (string, []byte, error) {
	if ex.Transaction == nil || len(ex.Transaction.Signatures) != 1 || p.Signature == nil || ex.Transaction.Signatures[0].String() != *p.Signature || !bytes.Equal(signed, ex.Wire) || ex.Slot <= p.QuoteSlot || ex.BlockTime == nil {
		return "", nil, errors.New("finalized receipt does not match persisted signed wire")
	}
	if ex.Err != nil {
		data, err := json.Marshal(map[string]any{"error": ex.Err})
		return "failed", data, err
	}
	src, err := solana.PublicKeyFromBase58(p.TreasuryATA)
	if err != nil {
		return "", nil, err
	}
	dst, err := solana.PublicKeyFromBase58(p.RecipientATA)
	if err != nil {
		return "", nil, err
	}
	sourcePre, ok1 := ex.Pre[src]
	sourcePost, ok2 := ex.Post[src]
	destPre, existed := ex.Pre[dst]
	destPost, ok3 := ex.Post[dst]
	matches := func(b chain.TokenBalance, owner string) bool {
		return b.Program == solana.TokenProgramID && b.Mint == earnmax.LPMint && b.Owner.String() == owner && b.Decimals == sourcePre.Decimals
	}
	if !ok1 || !ok2 || !ok3 || !matches(sourcePre, p.TreasuryOwner) || !matches(sourcePost, p.TreasuryOwner) || !matches(destPost, p.RecipientOwner) || (existed && !matches(destPre, p.RecipientOwner)) || sourcePre.Amount < sourcePost.Amount || sourcePre.Amount-sourcePost.Amount != p.LPRaw || destPost.Amount < destPre.Amount || destPost.Amount-destPre.Amount != p.LPRaw {
		return "", nil, errors.New("finalized transfer balances do not prove exact treasury debit and inviter credit")
	}
	data, err := json.Marshal(map[string]uint64{"source_before": sourcePre.Amount, "source_after": sourcePost.Amount, "destination_before": destPre.Amount, "destination_after": destPost.Amount})
	return "delivered", data, err
}

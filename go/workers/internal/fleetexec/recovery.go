package fleetexec

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	sdk "github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
)

// A signature-history response is not account-effect evidence. This proof is
// created only after an actual finalized account observation followed by an
// absent history check. Cross-mint requires its additional account-history and
// write-once receipt protocol and is deliberately excluded from this type.
type sameMintNoEffectProof struct {
	observedSlot, historySlot, observedHeight int64
	signature                                 string
}

type finalizedAccountReader interface {
	FinalizedAccounts(context.Context, []string, int64) (int64, []fleet.Account, error)
}

type sameMintRecovery struct {
	slotDuration time.Duration
	store        *Store
	accounts     finalizedAccountReader
	status       StatusClient
}

type noEffectBaseline struct {
	Vault, Source, Target, Mint            string
	SourceAmount, TargetAmount, IdleAmount int64
	Plan                                   struct {
		RouteKind       string `json:"route_kind"`
		SourceKind      string `json:"source_kind"`
		SourceSemantics string `json:"source_amount_semantics"`
		IdleAccount     string `json:"idle_token_account"`
	}
}

func (v *sameMintRecovery) inspect(ctx context.Context, record SubmissionRecord, floor int64) (*sameMintNoEffectProof, error) {
	if record.MovementLeg != LegRoute || floor <= 0 {
		return nil, errors.New("no-effect verifier requires same-mint route and a positive observation floor")
	}
	baseline, err := v.store.noEffectBaseline(ctx, record.OpportunityID)
	if err != nil {
		return nil, err
	}
	observed, err := observeSameMintNoEffect(ctx, v.accounts, baseline, floor)
	if err != nil {
		return nil, err
	}
	// Check history AFTER account readback, so the observation cannot hide a
	// signature that arrived between the earlier status check and readback.
	status, err := v.status.SignatureStatus(ctx, record.Signature)
	if err != nil {
		return nil, err
	}
	if status.Found || status.ContextSlot < observed || status.BlockHeight <= record.LastValidBlockHeight {
		return nil, errors.New("late signature, incomplete history frontier, or live blockhash prevents no-effect proof")
	}
	return &sameMintNoEffectProof{observed, status.ContextSlot, status.BlockHeight, record.Signature}, nil
}

func (s *Store) noEffectBaseline(ctx context.Context, id int64) (noEffectBaseline, error) {
	var b noEffectBaseline
	var raw json.RawMessage
	var source *string
	var snapshot *int64
	err := s.pool.QueryRow(ctx, `SELECT v.vault_pubkey, o.source_reserve, o.target_reserve,
 o.liquidity_mint, o.amount_raw, o.source_snapshot_id, o.execution_plan
 FROM loyal_yield.rebalance_opportunities o JOIN loyal_yield.managed_vaults v ON v.id=o.vault_id
 WHERE o.id=$1`, id).Scan(&b.Vault, &source, &b.Target, &b.Mint, &b.IdleAmount, &snapshot, &raw)
	if err != nil {
		return b, err
	}
	if err = json.Unmarshal(raw, &b.Plan); err != nil {
		return b, err
	}
	if b.Plan.RouteKind != "same_mint" {
		return b, errors.New("no-effect checker does not implement this protocol")
	}
	if b.Plan.SourceKind == "idle_vault_usdc" {
		return b, nil
	}
	if b.Plan.SourceKind != "reserve_position" || source == nil || snapshot == nil ||
		b.Plan.SourceSemantics != "kamino_obligation_collateral_deposited_amount" {
		return b, errors.New("missing typed collateral baseline")
	}
	b.Source = *source
	rows, err := s.pool.Query(ctx, `SELECT reserve, liquidity_mint, amount_raw, planning_metadata
 FROM loyal_yield.vault_position_snapshot_positions WHERE snapshot_id=$1 AND reserve IN ($2,$3)`, *snapshot, b.Source, b.Target)
	if err != nil {
		return b, err
	}
	defer rows.Close()
	sourceSeen, targetSeen := false, false
	for rows.Next() {
		var reserve, mint string
		var amount int64
		var metadata json.RawMessage
		if err = rows.Scan(&reserve, &mint, &amount, &metadata); err != nil {
			return b, err
		}
		var units struct {
			Semantics string `json:"amount_semantics"`
		}
		if err := json.Unmarshal(metadata, &units); err != nil || units.Semantics != "kamino_obligation_collateral_deposited_amount" {
			return b, errors.New("snapshot collateral units are unproven")
		}
		if mint != b.Mint || amount < 0 {
			return b, errors.New("snapshot mint or collateral amount mismatch")
		}
		switch reserve {
		case b.Source:
			if sourceSeen {
				return b, errors.New("duplicate source baseline")
			}
			sourceSeen = true
			b.SourceAmount = amount
		case b.Target:
			if targetSeen {
				return b, errors.New("duplicate target baseline")
			}
			targetSeen = true
			b.TargetAmount = amount
		}
	}
	if err = rows.Err(); err != nil {
		return b, err
	}
	if !sourceSeen || b.Source == b.Target || b.SourceAmount <= 0 {
		return b, errors.New("source collateral baseline missing or invalid")
	}
	// The Rust snapshot contract defines an absent target position as zero.
	return b, nil
}

// observeSameMintNoEffect compares obligation-deposited COLLATERAL, never
// reserve liquidity-vault deltas or present-day redeemable liquidity quotes.
// It mirrors inspect_expired_route's reserve_position / idle_vault_usdc arms.
func observeSameMintNoEffect(ctx context.Context, rpc finalizedAccountReader, b noEffectBaseline, floor int64) (int64, error) {
	if floor <= 0 || b.Vault == "" || b.Mint == "" {
		return 0, errors.New("incomplete no-effect identity")
	}
	if b.Plan.SourceKind == "idle_vault_usdc" {
		if b.Mint != fleet.USDCMint || b.IdleAmount < 0 || b.Plan.IdleAccount == "" {
			return 0, errors.New("invalid idle USDC baseline")
		}
		slot, accounts, err := rpc.FinalizedAccounts(ctx, []string{b.Plan.IdleAccount}, floor)
		if err != nil {
			return 0, err
		}
		if slot < floor || len(accounts) != 1 || accounts[0].Address != b.Plan.IdleAccount {
			return 0, errors.New("idle observation incomplete or too old")
		}
		amount, err := idleTokenAmount(accounts[0], b.Mint, b.Vault)
		if err != nil || amount != b.IdleAmount {
			return 0, errors.New("idle custody binding or balance changed")
		}
		return slot, nil
	}
	if b.Plan.SourceKind != "reserve_position" || b.Plan.SourceSemantics != "kamino_obligation_collateral_deposited_amount" || b.SourceAmount <= 0 || b.TargetAmount < 0 || b.Source == b.Target {
		return 0, errors.New("invalid collateral baseline")
	}
	slot, reserves, err := rpc.FinalizedAccounts(ctx, []string{b.Source, b.Target}, floor)
	if err != nil {
		return 0, err
	}
	if slot < floor || len(reserves) != 2 {
		return 0, errors.New("reserve observations incomplete or too old")
	}
	markets := make([]string, 2)
	obligations := make([]string, 2)
	for i, expected := range []string{b.Source, b.Target} {
		account := reserves[i]
		if account.Address != expected || account.Owner != fleet.KaminoProgram || account.Executable || account.Lamports == 0 || len(account.Data) != 8624 || !bytes.Equal(account.Data[:8], []byte{43, 242, 204, 202, 26, 247, 59, 127}) || binary.LittleEndian.Uint64(account.Data[8:16]) != 1 || sdk.PublicKeyFromBytes(account.Data[128:160]).String() != b.Mint {
			return 0, errors.New("reserve envelope, version or mint changed")
		}
		markets[i] = sdk.PublicKeyFromBytes(account.Data[32:64]).String()
		owner, e := sdk.PublicKeyFromBase58(b.Vault)
		if e != nil {
			return 0, e
		}
		market, e := sdk.PublicKeyFromBase58(markets[i])
		if e != nil {
			return 0, e
		}
		program := sdk.MustPublicKeyFromBase58(fleet.KaminoProgram)
		zero := sdk.PublicKey{}
		obligation, _, e := sdk.FindProgramAddress([][]byte{{0}, {0}, owner[:], market[:], zero[:], zero[:]}, program)
		if e != nil {
			return 0, e
		}
		obligations[i] = obligation.String()
	}
	keys := append([]string{b.Source, b.Target}, obligations...)
	observed, allAccounts, err := rpc.FinalizedAccounts(ctx, keys, slot)
	if err != nil {
		return 0, err
	}
	if observed < slot || len(allAccounts) != 4 {
		return 0, errors.New("obligation observations incomplete or too old")
	}
	for i, reserve := range []string{b.Source, b.Target} {
		if allAccounts[i].Address != reserve {
			return 0, errors.New("reserve identity changed in no-effect batch")
		}
		market, obligation, _, _, e := reservePostIdentity(allAccounts[i], b.Mint, b.Vault)
		if e != nil || market != markets[i] || obligation != obligations[i] {
			return 0, errors.New("reserve custody binding changed during no-effect observation")
		}
	}
	accounts := allAccounts[2:]
	for i, reserve := range []string{b.Source, b.Target} {
		if accounts[i].Address != obligations[i] {
			return 0, errors.New("obligation address mismatch")
		}
		amount := int64(0)
		a := accounts[i]
		absent := a.Lamports == 0 && len(a.Data) == 0 && a.Owner == "" && !a.Executable
		if !absent {
			var e error
			amount, e = obligationCollateral(a, markets[i], b.Vault, reserve)
			if e != nil {
				return 0, e
			}
		}
		expected := b.SourceAmount
		if i == 1 {
			expected = b.TargetAmount
		}
		if amount != expected {
			return 0, fmt.Errorf("obligation collateral changed for %s", reserve)
		}
	}
	return observed, nil
}

func obligationCollateral(a fleet.Account, market, owner, reserve string) (int64, error) {
	if a.Owner != fleet.KaminoProgram || a.Executable || a.Lamports == 0 || len(a.Data) != 3344 || !bytes.Equal(a.Data[:8], []byte{168, 206, 141, 106, 88, 76, 172, 167}) || sdk.PublicKeyFromBytes(a.Data[32:64]).String() != market || sdk.PublicKeyFromBytes(a.Data[64:96]).String() != owner {
		return 0, errors.New("obligation envelope or owner/market changed")
	}
	seen := map[string]bool{}
	var amount uint64
	for i := 0; i < 8; i++ {
		offset := 96 + i*136
		key := sdk.PublicKeyFromBytes(a.Data[offset : offset+32])
		if key.IsZero() {
			continue
		}
		name := key.String()
		if seen[name] {
			return 0, errors.New("duplicate obligation deposit")
		}
		seen[name] = true
		if name == reserve {
			amount = binary.LittleEndian.Uint64(a.Data[offset+32 : offset+40])
		}
	}
	if amount > math.MaxInt64 {
		return 0, errors.New("collateral amount exceeds BIGINT")
	}
	return int64(amount), nil
}

func idleTokenAmount(a fleet.Account, mint, owner string) (int64, error) {
	if a.Owner != "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA" || a.Executable || a.Lamports == 0 || len(a.Data) != 165 || a.Data[108] != 1 || sdk.PublicKeyFromBytes(a.Data[:32]).String() != mint || sdk.PublicKeyFromBytes(a.Data[32:64]).String() != owner {
		return 0, errors.New("idle token account envelope, mint, owner or state changed")
	}
	amount := binary.LittleEndian.Uint64(a.Data[64:72])
	if amount > math.MaxInt64 {
		return 0, errors.New("idle amount exceeds BIGINT")
	}
	return int64(amount), nil
}

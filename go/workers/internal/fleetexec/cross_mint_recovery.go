package fleetexec

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	sdk "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
	"math"
	"time"
)

type crossMintTokenAnchor struct {
	Mint         string `json:"mint"`
	TokenAccount string `json:"tokenAccount"`
	AmountRaw    int64  `json:"amountRaw"`
}
type crossMintPositionAnchor struct {
	Reserve                 string `json:"reserve"`
	Market                  string `json:"market"`
	Obligation              string `json:"obligation"`
	ObligationExists        bool   `json:"obligationExists"`
	CollateralRaw           int64  `json:"depositedCollateralAmountRaw"`
	MinimumDepositAmountRaw *int64 `json:"minimumDepositAmountRaw,omitempty"`
}
type crossMintAnchors struct {
	Debit    *crossMintTokenAnchor    `json:"debit"`
	Credit   *crossMintTokenAnchor    `json:"credit"`
	Position *crossMintPositionAnchor `json:"kaminoPosition,omitempty"`
}

// historyReader lists an address's transactions, newest first.
type historyReader interface {
	History(ctx context.Context, address sdk.PublicKey, limit int, before sdk.Signature, commitment rpc.CommitmentType, minContextSlot uint64) ([]chain.Signed, error)
}

// signatureReader is the signature status and finalized height a no-effect
// proof rechecks last.
type signatureReader interface {
	SignatureState(ctx context.Context, signature string) (chain.SignatureState, error)
	FinalizedBlockHeight(ctx context.Context) (height, slot uint64, err error)
}
type crossMintRecovery struct {
	store    *Store
	accounts fleet.AccountReader
	history  historyReader
	status   signatureReader
}
type crossMintNoEffectProof struct {
	observedSlot, historySlot, height, effectFloor, custodyAnchor int64
	signature, owner                                              string
	anchors                                                       json.RawMessage
	historyEvidence                                               json.RawMessage
	observedAt                                                    time.Time
}

func parseCrossMintAnchors(raw []byte) (crossMintAnchors, error) {
	var a crossMintAnchors
	if err := json.Unmarshal(raw, &a); err != nil {
		return a, err
	}
	if a.Debit == nil && a.Credit == nil {
		return a, errors.New("cross-mint no-effect proof has no token custody anchors")
	}
	seen := map[string]bool{}
	for _, anchor := range []*crossMintTokenAnchor{a.Debit, a.Credit} {
		if anchor == nil {
			continue
		}
		if anchor.AmountRaw < 0 || seen[anchor.TokenAccount] {
			return a, errors.New("invalid or duplicate cross-mint custody anchor")
		}
		seen[anchor.TokenAccount] = true
		if _, err := sdk.PublicKeyFromBase58(anchor.TokenAccount); err != nil {
			return a, err
		}
		if _, err := canonicalCustodyTokenProgram(anchor.Mint); err != nil {
			return a, err
		}
	}
	if p := a.Position; p != nil {
		if p.CollateralRaw < 0 || p.MinimumDepositAmountRaw != nil {
			return a, errors.New("position anchor conversion needs a source-backed minimum deposit decoder")
		}
		for _, key := range []string{p.Reserve, p.Market, p.Obligation} {
			if _, err := sdk.PublicKeyFromBase58(key); err != nil {
				return a, err
			}
		}
	}
	return a, nil
}

// Registry parity with loyal-actions stablecoins.rs, rather than trusting a
// signer-supplied program ID for a mint's token-account envelope.
func canonicalCustodyTokenProgram(mint string) (string, error) {
	switch mint {
	case fleet.USDCMint, "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB", "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA":
		return sdk.TokenProgramID.String(), nil
	case "CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH", "2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH", "2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo":
		return "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb", nil
	default:
		return "", errors.New("custody mint is outside canonical Earn registry")
	}
}
func custodyTokenAmount(a *chain.Account, mint, owner string) (int64, error) {
	program, err := canonicalCustodyTokenProgram(mint)
	if err != nil {
		return 0, err
	}
	if a == nil || a.Owner.String() != program || a.Executable || a.Lamports == 0 || len(a.Data) < 165 || a.Data[108] != 1 || sdk.PublicKeyFromBytes(a.Data[:32]).String() != mint || sdk.PublicKeyFromBytes(a.Data[32:64]).String() != owner {
		return 0, errors.New("custody token envelope, authority, mint or state differs")
	}
	if program == sdk.TokenProgramID.String() && len(a.Data) != 165 {
		return 0, errors.New("classic SPL custody account has wrong length")
	}
	// StateWithExtensions<Account>::unpack permits an unextended 165-byte base;
	// extended data must have AccountType::Account at 165 and a nonempty TLV
	// slice. The multisig length is explicitly excluded by the pinned SDK.
	if program != sdk.TokenProgramID.String() && len(a.Data) != 165 && (len(a.Data) <= 166 || len(a.Data) == 355 || a.Data[165] != 2) {
		return 0, errors.New("Token-2022 custody account type or length differs")
	}
	for _, offset := range []int{72, 109, 129} {
		if binary.LittleEndian.Uint32(a.Data[offset:offset+4]) > 1 {
			return 0, errors.New("token account optional authority/native tag invalid")
		}
	}
	raw := binary.LittleEndian.Uint64(a.Data[64:72])
	if raw > math.MaxInt64 {
		return 0, errors.New("custody balance exceeds BIGINT")
	}
	return int64(raw), nil
}

// Token custody requires a recognized signature AT the anchor slot. Empty or
// truncated history cannot prove attribution. Obligation history uses the
// source protocol's looser rule: no unrecognized signature since the anchor.
func verifyCustodyHistory(ctx context.Context, history historyReader, address string, anchor, floor int64, recognized map[string]bool, requireAnchor bool) ([]chain.Signed, error) {
	key, err := sdk.PublicKeyFromBase58(address)
	if err != nil {
		return nil, err
	}
	var before sdk.Signature
	observedAnchor := false
	observed := []chain.Signed{}
	seen := map[sdk.Signature]bool{}
	var previous int64 = math.MaxInt64
	for pageNo := 0; pageNo < 32; pageNo++ {
		page, err := history.History(ctx, key, 1000, before, rpc.CommitmentFinalized, uint64(max(floor, 0)))
		if err != nil {
			return nil, err
		}
		reachedOld := false
		for _, s := range page {
			slot := int64(s.Slot)
			if slot > previous || seen[s.Signature] {
				return nil, errors.New("address history is repeated or unordered")
			}
			previous = slot
			seen[s.Signature] = true
			if slot < anchor {
				reachedOld = true
				break
			}
			if !recognized[s.Signature.String()] {
				return nil, errors.New("custody history contains external signature")
			}
			observed = append(observed, s)
			observedAnchor = observedAnchor || slot == anchor
		}
		if reachedOld || len(page) < 1000 {
			if requireAnchor && !observedAnchor {
				return nil, errors.New("custody history anchor is unavailable")
			}
			return observed, nil
		}
		before = page[len(page)-1].Signature
	}
	return nil, errors.New("custody history exceeds bounded verification window")
}

func (v *crossMintRecovery) inspect(ctx context.Context, r SubmissionRecord, floor int64) (*crossMintNoEffectProof, error) {
	if r.MovementLeg == LegRoute || r.DecisionID == nil || floor <= 0 {
		return nil, errors.New("cross-mint recovery needs attached movement leg and effect floor")
	}
	anchors, err := parseCrossMintAnchors(r.ExpectedBalanceAnchors)
	if err != nil {
		return nil, err
	}
	var owner string
	var anchor *int64
	err = v.store.pool.QueryRow(ctx, `SELECT v.vault_pubkey,d.custody_reconciled_slot FROM loyal_yield.rebalance_decisions d JOIN loyal_yield.rebalance_opportunities o ON o.decision_id=d.id JOIN loyal_yield.managed_vaults v ON v.id=o.vault_id WHERE d.id=$1 AND o.id=$2 AND d.movement_route='cross_mint_jupiter' AND d.terminal_outcome IS NULL`, *r.DecisionID, r.OpportunityID).Scan(&owner, &anchor)
	if err != nil {
		return nil, err
	}
	if anchor == nil || *anchor < 0 {
		return nil, errors.New("movement lacks durable custody history anchor")
	}
	recognized := map[string]bool{}
	rows, err := v.store.pool.Query(ctx, `SELECT transaction_signature FROM loyal_yield.signed_route_submissions WHERE decision_id=$1 AND submission_state='reconciled' AND finalized_slot>=$2`, *r.DecisionID, *anchor)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var signature string
		if err := rows.Scan(&signature); err != nil {
			rows.Close()
			return nil, err
		}
		recognized[signature] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	tokenAnchors := []*crossMintTokenAnchor{}
	addresses := []string{}
	for _, a := range []*crossMintTokenAnchor{anchors.Debit, anchors.Credit} {
		if a != nil {
			tokenAnchors = append(tokenAnchors, a)
			addresses = append(addresses, a.TokenAccount)
		}
	}
	slot, accounts, err := fleet.ReadAccounts(ctx, v.accounts, addresses, rpc.CommitmentFinalized, floor)
	if err != nil {
		return nil, err
	}
	evidence := map[string]any{"custodyAnchorSlot": *anchor, "finalizedAccountSlot": slot, "accounts": map[string]any{}}
	history := evidence["accounts"].(map[string]any)
	for i, a := range tokenAnchors {
		amount, e := custodyTokenAmount(accounts[i], a.Mint, owner)
		if e != nil || amount != a.AmountRaw {
			return nil, errors.New("cross-mint token custody binding or amount changed")
		}
		statuses, e := verifyCustodyHistory(ctx, v.history, a.TokenAccount, *anchor, slot, recognized, true)
		if e != nil {
			return nil, e
		}
		history[a.TokenAccount] = statuses
	}
	if p := anchors.Position; p != nil {
		// The anchor may record a closed obligation (full source exit); the
		// reserve stays required by its envelope check below.
		observed, actual, e := fleet.ReadAccounts(ctx, v.accounts, []string{p.Reserve, p.Obligation}, rpc.CommitmentFinalized, slot)
		if e != nil {
			return nil, e
		}
		// The anchor market and obligation must be the actual PDA for this vault.
		if actual[0] == nil || len(actual[0].Data) != 8624 {
			return nil, errors.New("cross-mint reserve envelope missing")
		}
		mint := sdk.PublicKeyFromBytes(actual[0].Data[128:160]).String()
		market, obligation, _, _, e := reservePostIdentity(actual[0], mint, owner)
		if e != nil || market != p.Market || obligation != p.Obligation {
			return nil, errors.New("cross-mint position binding changed")
		}
		exists := actual[1] != nil
		amount := int64(0)
		if exists {
			amount, e = obligationCollateral(actual[1], p.Market, owner, p.Reserve)
			if e != nil {
				return nil, e
			}
		}
		if exists != p.ObligationExists || amount != p.CollateralRaw {
			return nil, errors.New("cross-mint obligation collateral changed")
		}
		statuses, e := verifyCustodyHistory(ctx, v.history, p.Obligation, *anchor, observed, recognized, false)
		if e != nil {
			return nil, e
		}
		history[p.Obligation] = statuses
		slot = observed
	}
	// Recheck signature history AFTER balances and account history. A late seen
	// signature (even processed with an error) vetoes no-effect publication.
	status, err := v.status.SignatureState(ctx, r.Signature)
	if err != nil {
		return nil, err
	}
	height, _, err := v.status.FinalizedBlockHeight(ctx)
	if err != nil {
		return nil, err
	}
	if status.Found || int64(status.ContextSlot) < slot || int64(height) <= r.LastValidBlockHeight {
		return nil, errors.New("late signature or incomplete history prevents cross-mint no-effect proof")
	}
	evidence["finalizedAccountSlot"] = slot
	rawEvidence, err := json.Marshal(evidence)
	if err != nil {
		return nil, err
	}
	return &crossMintNoEffectProof{observedSlot: slot, historySlot: int64(status.ContextSlot), height: int64(height), effectFloor: floor, custodyAnchor: *anchor, signature: r.Signature, owner: owner, anchors: bytes.Clone(r.ExpectedBalanceAnchors), historyEvidence: rawEvidence, observedAt: time.Now().UTC()}, nil
}

// Receipt and terminal leg transition share one transaction. A crash cannot
// leave a replaceable generation without its write-once no-effect receipt.
func (s *Store) expireCrossMint(ctx context.Context, lease SubmissionLease, p *crossMintNoEffectProof) error {
	r := lease.Submission
	if p == nil || r.DecisionID == nil || r.MovementLeg == LegRoute || p.signature != r.Signature || r.EffectCheckSlot == nil || r.ExpiryObservedBlockHeight == nil || p.effectFloor != *r.EffectCheckSlot || p.observedSlot < p.effectFloor || p.historySlot < p.observedSlot || p.height < *r.ExpiryObservedBlockHeight || *r.ExpiryObservedBlockHeight <= r.LastValidBlockHeight || !sameJSON(p.anchors, r.ExpectedBalanceAnchors) || time.Since(p.observedAt) > 30*time.Second {
		return errors.New("cross-mint expiry proof does not bind this custody observation")
	}
	// The receipt protocol binds the originally persisted expiry height, even
	// when finalized block height has advanced during history verification.
	height := *r.ExpiryObservedBlockHeight
	hash := sha256.New()
	hash.Write([]byte("cross-mint-no-effect-receipt-v1"))
	writeInt := func(n int64) { var raw [8]byte; binary.LittleEndian.PutUint64(raw[:], uint64(n)); hash.Write(raw[:]) }
	writeInt(r.ID)
	writeInt(*r.DecisionID)
	hash.Write([]byte(r.Signature))
	writeInt(height)
	writeInt(p.historySlot)
	writeInt(p.effectFloor)
	var anchorJSON any
	decoder := json.NewDecoder(bytes.NewReader(p.anchors))
	decoder.UseNumber()
	if err := decoder.Decode(&anchorJSON); err != nil {
		return err
	}
	canonical, err := json.Marshal(anchorJSON)
	if err != nil {
		return err
	}
	hash.Write(canonical)
	hash.Write(p.historyEvidence)
	writeInt(p.observedAt.UnixMicro())
	evidenceHash := hex.EncodeToString(hash.Sum(nil))
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, `SELECT s.id FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.rebalance_decisions d ON d.id=s.decision_id JOIN loyal_yield.rebalance_opportunities o ON o.id=s.opportunity_id JOIN loyal_yield.managed_vaults v ON v.id=o.vault_id WHERE s.id=$1 AND s.decision_id=$2 AND d.movement_route='cross_mint_jupiter' AND d.custody_reconciled_slot=$3 AND v.vault_pubkey=$4 AND s.expected_balance_anchors=$5 AND s.transaction_signature=$6 AND s.confirmation_lease_owner=$7 AND s.confirmation_fencing_token=$8 AND s.confirmation_lease_expires_at>clock_timestamp() AND s.submission_state IN ('expiry_check_pending','effect_ambiguous') AND s.expiry_observed_block_height=$9 AND s.effect_check_slot=$10 AND s.last_valid_block_height<$9 FOR UPDATE OF s,d FOR SHARE OF v`, r.ID, *r.DecisionID, p.custodyAnchor, p.owner, p.anchors, r.Signature, lease.Owner, lease.FencingToken, height, p.effectFloor).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleOwner
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO loyal_yield.cross_mint_no_effect_receipts(submission_id,decision_id,movement_leg,leg_generation,transaction_signature,observed_block_height,signature_history_checked_through_slot,effect_check_slot,expected_balance_anchors,observed_balance_anchors,signature_history_evidence,evidence_hash,observed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,$10,$11,$12)`, r.ID, *r.DecisionID, r.MovementLeg, r.LegGeneration, r.Signature, height, p.historySlot, p.effectFloor, p.anchors, p.historyEvidence, evidenceHash, p.observedAt); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions SET submission_state='expired',error_detail=$4,last_status_checked_at=clock_timestamp(),confirmation_lease_owner=NULL,confirmation_lease_expires_at=NULL,updated_at=clock_timestamp() WHERE id=$1 AND confirmation_lease_owner=$2 AND confirmation_fencing_token=$3 AND confirmation_lease_expires_at>clock_timestamp()`, r.ID, lease.Owner, lease.FencingToken, fmt.Sprintf("blockhash_expired_at_height_%d", height))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return nil
	})
}

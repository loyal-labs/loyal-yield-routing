package fleetexec

import (
	"bytes"
	"context"
	"encoding/binary"
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
	"math/big"
	"time"
)

// These values originate in the linked opportunity/decision, rather than a
// signer-supplied balance assertion. They are rechecked under the final fence.
type sameMintPostContract struct {
	vaultID, policyID                     int64
	vault, source, target, mint, settings string
	vaultIndex                            int16
	minimumSlot                           int64
	plan                                  json.RawMessage
	sourceKind                            string
	idleATA                               string
}
type observedPosition struct {
	reserve, market, mint, obligation, collateralMint string
	amount, redeemable, supplyAPY                     int64
	exists                                            bool
}

// Unexported: only finalized observation plus exact receipt creates a publishable
// proof. Current collateral is preserved, including a residual source deposit.
type sameMintPostProof struct {
	contract              sameMintPostContract
	slot                  int64
	positions             []observedPosition
	idleAmount            int64
	idleATA, tokenProgram string
	idleATAExists         bool
	observedAt            time.Time
	receipt               chain.Receipt
}

func (s *Store) loadPostContract(ctx context.Context, r SubmissionRecord) (sameMintPostContract, error) {
	var c sameMintPostContract
	if r.DecisionID == nil || r.ConfirmedSlot == nil || r.State != StateReconciliationPending || r.MovementLeg != LegRoute {
		return c, errors.New("same-mint post-state requires linked confirmed route")
	}
	var source *string
	var currentSlot int64
	var plan struct {
		RouteKind  string `json:"route_kind"`
		SourceKind string `json:"source_kind"`
		Semantics  string `json:"source_amount_semantics"`
		Settings   string `json:"settings"`
		VaultIndex int16  `json:"vault_index"`
		IdleATA    string `json:"idle_token_account"`
	}
	err := s.pool.QueryRow(ctx, `SELECT v.id,v.active_policy_id,v.vault_pubkey,v.settings,v.vault_index,o.source_reserve,o.target_reserve,o.liquidity_mint,o.execution_plan,
 GREATEST(COALESCE((SELECT max(observed_slot) FROM loyal_yield.vault_position_snapshots WHERE vault_id=v.id AND is_current),0),
 COALESCE((SELECT max(observed_slot) FROM loyal_yield.vault_reserve_positions_current WHERE vault_id=v.id),0),
 COALESCE((SELECT max(observed_slot) FROM loyal_yield.vault_idle_token_balances_current WHERE vault_id=v.id),0))
 FROM loyal_yield.rebalance_opportunities o JOIN loyal_yield.rebalance_decisions d ON d.id=o.decision_id
 JOIN loyal_yield.managed_vaults v ON v.id=o.vault_id JOIN loyal_yield.route_policies p ON p.id=v.active_policy_id
 WHERE o.id=$1 AND o.decision_id=$2 AND d.vault_id=v.id AND o.cluster=$3 AND v.active AND p.active
 AND d.movement_route<>'cross_mint_jupiter' AND d.status::text='confirming' AND d.signature=$4
 AND d.source_reserve IS NOT DISTINCT FROM o.source_reserve AND d.target_reserve=o.target_reserve AND d.liquidity_mint=o.liquidity_mint
 AND d.execution_plan=o.execution_plan`, r.OpportunityID, *r.DecisionID, r.Cluster, r.Signature).Scan(&c.vaultID, &c.policyID, &c.vault, &c.settings, &c.vaultIndex, &source, &c.target, &c.mint, &c.plan, &currentSlot)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal(c.plan, &plan); err != nil {
		return c, err
	}
	if plan.RouteKind != "same_mint" || plan.Settings != c.settings || plan.VaultIndex != c.vaultIndex {
		return c, errors.New("same-mint post-state vault identity differs from route plan")
	}
	c.sourceKind = plan.SourceKind
	if source != nil {
		c.source = *source
	}
	switch plan.SourceKind {
	case "reserve_position":
		if source == nil || c.source == c.target || plan.Semantics != "kamino_obligation_collateral_deposited_amount" {
			return c, errors.New("invalid route collateral contract")
		}
	case "idle_vault_usdc":
		if source != nil || c.mint != fleet.USDCMint || plan.IdleATA == "" {
			return c, errors.New("invalid idle route contract")
		}
		c.idleATA = plan.IdleATA
	default:
		return c, errors.New("unsupported same-mint source protocol")
	}
	if currentSlot == math.MaxInt64 {
		return c, errors.New("post-state observation frontier exceeds bound")
	}
	c.minimumSlot = currentSlot + 1
	if c.minimumSlot < *r.ConfirmedSlot {
		c.minimumSlot = *r.ConfirmedSlot
	}
	return c, nil
}

func (v *sameMintRecovery) reconcile(ctx context.Context, lease SubmissionLease, receipt chain.Receipt) error {
	r := lease.Submission
	if r.ConfirmedSlot == nil {
		return errors.New("post-state lacks confirmed slot")
	}
	if err := VerifyReceiptIdentity(receipt, r, *r.ConfirmedSlot); err != nil {
		return err
	}
	c, err := v.store.loadPostContract(ctx, r)
	if err != nil {
		return err
	}
	proof, err := observeSameMintPost(ctx, v.accounts, c, receipt, v.slotDuration)
	if err != nil {
		return err
	}
	return v.store.publishSameMintPost(ctx, lease, proof)
}

func reservePostIdentity(a *chain.Account, mint, owner string) (market, obligation, collateralMint, tokenProgram string, err error) {
	if a == nil || a.Owner.String() != fleet.KaminoProgram || a.Executable || a.Lamports == 0 || len(a.Data) != 8624 || !bytes.Equal(a.Data[:8], []byte{43, 242, 204, 202, 26, 247, 59, 127}) || binary.LittleEndian.Uint64(a.Data[8:16]) != 1 || sdk.PublicKeyFromBytes(a.Data[128:160]).String() != mint {
		return "", "", "", "", errors.New("post reserve envelope or mint differs")
	}
	market = sdk.PublicKeyFromBytes(a.Data[32:64]).String()
	collateralMint = sdk.PublicKeyFromBytes(a.Data[2560:2592]).String()
	tokenProgram = sdk.PublicKeyFromBytes(a.Data[408:440]).String()
	ownerKey, e := sdk.PublicKeyFromBase58(owner)
	if e != nil {
		err = e
		return
	}
	marketKey := sdk.PublicKeyFromBytes(a.Data[32:64])
	zero := sdk.PublicKey{}
	key, _, e := sdk.FindProgramAddress([][]byte{{0}, {0}, ownerKey[:], marketKey[:], zero[:], zero[:]}, sdk.MustPublicKeyFromBase58(fleet.KaminoProgram))
	err = e
	obligation = key.String()
	return
}
func associatedCustodyAccount(owner, mint, program string) (string, error) {
	o, err := sdk.PublicKeyFromBase58(owner)
	if err != nil {
		return "", err
	}
	m, err := sdk.PublicKeyFromBase58(mint)
	if err != nil {
		return "", err
	}
	p, err := sdk.PublicKeyFromBase58(program)
	if err != nil {
		return "", err
	}
	if program != "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA" && program != "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb" {
		return "", errors.New("unsupported liquidity token program")
	}
	a, _, err := sdk.FindProgramAddress([][]byte{o[:], p[:], m[:]}, sdk.MustPublicKeyFromBase58("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"))
	return a.String(), err
}

func observeSameMintPost(ctx context.Context, reader fleet.AccountReader, c sameMintPostContract, receipt chain.Receipt, slotDuration time.Duration) (*sameMintPostProof, error) {
	reserves := []string{c.target}
	if c.sourceKind == "reserve_position" {
		reserves = []string{c.source, c.target}
	}
	discoverySlot, discovery, err := fleet.ReadAccounts(ctx, reader, reserves, rpc.CommitmentFinalized, c.minimumSlot)
	if err != nil {
		return nil, err
	}
	positions := make([]observedPosition, len(reserves))
	obligations := make([]string, len(reserves))
	program := ""
	for i, a := range discovery {
		market, obligation, collateral, token, e := reservePostIdentity(a, c.mint, c.vault)
		if e != nil {
			return nil, e
		}
		if program != "" && program != token {
			return nil, errors.New("same-mint reserves disagree on token program")
		}
		program = token
		obligations[i] = obligation
		positions[i] = observedPosition{reserve: reserves[i], market: market, mint: c.mint, obligation: obligation, collateralMint: collateral}
	}
	ata, err := associatedCustodyAccount(c.vault, c.mint, program)
	if err != nil {
		return nil, err
	}
	if c.sourceKind == "idle_vault_usdc" && ata != c.idleATA {
		return nil, errors.New("idle custody ATA differs from signed plan")
	}
	// The second batch includes reserves again: obligation collateral and the
	// conversion fraction share one finalized bank observation. An earlier reserve
	// quote cannot be promoted to the later obligation slot. A full source exit
	// closes the source obligation inside the route (KLend withdraw), so null
	// accounts are returned rather than rejected; reserves stay required by
	// reservePostIdentity and the target must still hold funded collateral.
	keys := append(append(append([]string{}, reserves...), obligations...), ata)
	slot, accounts, err := fleet.ReadAccounts(ctx, reader, keys, rpc.CommitmentFinalized, discoverySlot)
	if err != nil {
		return nil, err
	}
	// Rust parity (decode_spl_token_account_amount(None)): a null liquidity ATA
	// is a zero balance. A present account must still be the vault's exact ATA.
	idleAccount := accounts[len(accounts)-1]
	idleExists := idleAccount != nil
	idle := int64(0)
	if idleExists {
		idle, err = custodyTokenAmount(idleAccount, c.mint, c.vault)
		if err != nil {
			return nil, err
		}
	}
	for i := range positions {
		p := &positions[i]
		a := accounts[i]
		market, obligation, collateral, token, e := reservePostIdentity(a, c.mint, c.vault)
		if e != nil {
			return nil, e
		}
		if market != p.market || obligation != p.obligation || collateral != p.collateralMint || token != program {
			return nil, errors.New("reserve identity changed within post observation")
		}
		state, e := fleet.DecodeKaminoReserve(a, fleet.ReserveIdentity{Address: p.reserve, Market: p.market, Mint: p.mint}, slot, slotDuration)
		if e != nil {
			return nil, e
		}
		p.supplyAPY = state.SupplyAPYBPS
		obligationAccount := accounts[len(reserves)+i]
		// Missing accounts are zero only when the finalized RPC returned a null
		// account. A malformed funded envelope is never interpreted as absent.
		p.exists = obligationAccount != nil
		if p.exists {
			p.amount, e = obligationCollateral(obligationAccount, p.market, c.vault, p.reserve)
			if e != nil {
				return nil, e
			}
		}
		p.redeemable, e = redeemableCollateral(a.Data, p.amount)
		if e != nil {
			return nil, e
		}
	}
	if positions[len(positions)-1].amount <= 0 {
		return nil, errors.New("post-state target collateral is not funded")
	}
	return &sameMintPostProof{contract: c, slot: slot, positions: positions, idleAmount: idle, idleATA: ata, idleATAExists: idleExists, tokenProgram: program, observedAt: time.Now().UTC(), receipt: receipt}, nil
}

// KLend Fraction is U68F60. total_supply is available + borrowed - all three
// fee pools. collateral_to_liquidity floors the wide product; it does not pass
// through float64 or assume that collateral and liquidity share raw units.
// Source: pinned klend 23b9f2b state/reserve.rs ReserveLiquidity::total_supply,
// ReserveCollateral::exchange_rate and CollateralExchangeRate conversion.
func redeemableCollateral(data []byte, collateral int64) (int64, error) {
	if len(data) != 8624 || collateral < 0 {
		return 0, errors.New("invalid collateral conversion input")
	}
	scaled := new(big.Int).Lsh(new(big.Int).SetUint64(binary.LittleEndian.Uint64(data[224:232])), 60)
	little := func(b []byte) *big.Int {
		rev := bytes.Clone(b)
		for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
			rev[i], rev[j] = rev[j], rev[i]
		}
		return new(big.Int).SetBytes(rev)
	}
	scaled.Add(scaled, little(data[232:248]))
	for _, offset := range []int{344, 360, 376} {
		scaled.Sub(scaled, little(data[offset:offset+16]))
	}
	if scaled.Sign() < 0 {
		return 0, errors.New("reserve fees exceed actual liquidity")
	}
	supply := binary.LittleEndian.Uint64(data[2592:2600])
	if supply == 0 || scaled.Sign() == 0 {
		return collateral, nil
	} // pinned SDK initial rate = 1
	numerator := new(big.Int).Mul(big.NewInt(collateral), scaled)
	denominator := new(big.Int).Lsh(new(big.Int).SetUint64(supply), 60)
	result := new(big.Int).Quo(numerator, denominator)
	if !result.IsInt64() || result.Sign() < 0 {
		return 0, errors.New("redeemable liquidity exceeds BIGINT")
	}
	return result.Int64(), nil
}

func (s *Store) publishSameMintPost(ctx context.Context, lease SubmissionLease, proof *sameMintPostProof) error {
	r := lease.Submission
	if proof == nil || r.ConfirmedSlot == nil || r.DecisionID == nil || proof.slot < *r.ConfirmedSlot || proof.slot < proof.contract.minimumSlot || proof.observedAt.After(time.Now()) || time.Since(proof.observedAt) > 30*time.Second {
		return errors.New("post-state proof missing or stale")
	}
	if err := VerifyReceiptIdentity(proof.receipt, r, *r.ConfirmedSlot); err != nil {
		return err
	}
	c := proof.contract
	contextJSON, err := json.Marshal(map[string]any{"publication_scope": "observed_subset", "finalized_signed_route": map[string]any{"submission_id": r.ID, "decision_id": *r.DecisionID, "finalized_slot": *r.ConfirmedSlot, "signature": r.Signature}})
	if err != nil {
		return err
	}
	return db.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, `SELECT s.id FROM loyal_yield.signed_route_submissions s JOIN loyal_yield.rebalance_opportunities o ON o.id=s.opportunity_id JOIN loyal_yield.rebalance_decisions d ON d.id=s.decision_id
   WHERE s.id=$1 AND s.opportunity_id=$2 AND s.decision_id=$3 AND s.transaction_signature=$4 AND s.confirmed_slot=$7
   AND s.submission_state='reconciliation_pending' AND s.confirmation_lease_owner=$5 AND s.confirmation_fencing_token=$6 AND s.confirmation_lease_expires_at>clock_timestamp()
   AND o.decision_id=d.id AND o.vault_id=$8 AND o.execution_plan=$9 AND d.execution_plan=$9 AND d.status::text='confirming' AND d.signature=s.transaction_signature
   FOR UPDATE OF s,d,o`, r.ID, r.OpportunityID, *r.DecisionID, r.Signature, lease.Owner, lease.FencingToken, *r.ConfirmedSlot, c.vaultID, c.plan).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleOwner
		}
		if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT v.id FROM loyal_yield.managed_vaults v JOIN loyal_yield.route_policies p ON p.id=v.active_policy_id WHERE v.id=$1 AND v.active_policy_id=$2 AND v.vault_pubkey=$3 AND v.settings=$4 AND v.vault_index=$5 AND v.active AND p.active FOR UPDATE OF v FOR SHARE OF p`, c.vaultID, c.policyID, c.vault, c.settings, c.vaultIndex).Scan(&id)
		if err != nil {
			return err
		}
		var currentSlot int64
		// All projectors serialize on the managed-vault row. Require a strictly
		// newer snapshot; equal-slot concurrent observations retry instead of
		// silently replacing a conflicting publication.
		err = tx.QueryRow(ctx, `SELECT GREATEST(COALESCE((SELECT max(observed_slot) FROM loyal_yield.vault_position_snapshots WHERE vault_id=$1 AND is_current),0),COALESCE((SELECT max(observed_slot) FROM loyal_yield.vault_reserve_positions_current WHERE vault_id=$1),0),COALESCE((SELECT max(observed_slot) FROM loyal_yield.vault_idle_token_balances_current WHERE vault_id=$1),0))`, c.vaultID).Scan(&currentSlot)
		if err != nil {
			return err
		}
		if currentSlot >= proof.slot {
			return errors.New("post-state publication was overtaken by current snapshot")
		}
		if _, err = tx.Exec(ctx, `UPDATE loyal_yield.vault_position_snapshots SET is_current=false WHERE vault_id=$1 AND is_current`, c.vaultID); err != nil {
			return err
		}
		var snapshot int64
		err = tx.QueryRow(ctx, `INSERT INTO loyal_yield.vault_position_snapshots(vault_id,policy_id,observed_slot,observed_at,chain_slot,is_current,context) VALUES($1,$2,$3,$4,$3,true,$5) RETURNING id`, c.vaultID, c.policyID, proof.slot, proof.observedAt, contextJSON).Scan(&snapshot)
		if err != nil {
			return err
		}
		for _, p := range proof.positions {
			metadata, e := json.Marshal(map[string]any{"amount_semantics": "kamino_obligation_collateral_deposited_amount", "source_collateral_amount_raw": fmt.Sprint(p.amount), "redeemable_source_liquidity_amount_raw": fmt.Sprint(p.redeemable), "redeemable_liquidity_amount_raw": fmt.Sprint(p.redeemable), "obligation": p.obligation, "obligation_exists": p.exists, "vault_liquidity_ata": proof.idleATA, "vault_liquidity_ata_exists": proof.idleATAExists, "idle_vault_liquidity_amount_raw": fmt.Sprint(proof.idleAmount), "vault_liquidity_amount_raw": fmt.Sprint(proof.idleAmount), "collateral_mint": p.collateralMint, "liquidity_token_program": proof.tokenProgram})
			if e != nil {
				return e
			}
			if p.amount > 0 {
				if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.vault_position_snapshot_positions(snapshot_id,reserve,market,liquidity_mint,amount_raw,supply_apy_bps,borrow_apy_bps,has_value,planning_metadata) VALUES($1,$2,$3,$4,$5,$6,NULL,true,$7)`, snapshot, p.reserve, p.market, p.mint, p.amount, p.supplyAPY, metadata); err != nil {
					return err
				}
			}
			_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.vault_reserve_positions_current(vault_id,reserve,market,liquidity_mint,amount_raw,has_value,supply_apy_bps,borrow_apy_bps,snapshot_id,observed_slot,observed_at,planning_metadata) VALUES($1,$2,$3,$4,$5::bigint,$5::bigint>0,$6,NULL,$7,$8,$9,$10)
   ON CONFLICT(vault_id,reserve) DO UPDATE SET market=EXCLUDED.market,liquidity_mint=EXCLUDED.liquidity_mint,amount_raw=EXCLUDED.amount_raw,has_value=EXCLUDED.has_value,supply_apy_bps=EXCLUDED.supply_apy_bps,borrow_apy_bps=EXCLUDED.borrow_apy_bps,snapshot_id=EXCLUDED.snapshot_id,observed_slot=EXCLUDED.observed_slot,observed_at=EXCLUDED.observed_at,planning_metadata=EXCLUDED.planning_metadata`, c.vaultID, p.reserve, p.market, p.mint, p.amount, p.supplyAPY, snapshot, proof.slot, proof.observedAt, metadata)
			if err != nil {
				return err
			}
		}
		if c.sourceKind == "idle_vault_usdc" {
			_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.vault_idle_token_balances_current(vault_id,mint,amount_raw,owner,token_account,observed_slot,observed_at,source_commitment,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,'finalized',clock_timestamp()) ON CONFLICT(vault_id,mint) DO UPDATE SET amount_raw=EXCLUDED.amount_raw,owner=EXCLUDED.owner,token_account=EXCLUDED.token_account,observed_slot=EXCLUDED.observed_slot,observed_at=EXCLUDED.observed_at,source_commitment=EXCLUDED.source_commitment,updated_at=clock_timestamp() WHERE loyal_yield.vault_idle_token_balances_current.observed_slot<EXCLUDED.observed_slot`, c.vaultID, c.mint, proof.idleAmount, c.vault, proof.idleATA, proof.slot, proof.observedAt)
			if err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, `UPDATE loyal_yield.rebalance_decisions SET status='confirmed',signature=$2,submitted_slot=COALESCE(submitted_slot,$3),confirmed_slot=$4,post_snapshot_id=$5,updated_at=clock_timestamp() WHERE id=$1`, *r.DecisionID, r.Signature, r.SubmittedSlot, *r.ConfirmedSlot, snapshot); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.signed_route_submissions SET submission_state='reconciled',finalized_slot=$4,finalized_at=COALESCE(finalized_at,clock_timestamp()),reconciled_slot=$5,reconciled_at=clock_timestamp(),confirmation_lease_owner=NULL,confirmation_lease_expires_at=NULL,error_detail=NULL,updated_at=clock_timestamp() WHERE id=$1 AND confirmation_lease_owner=$2 AND confirmation_fencing_token=$3 AND confirmation_lease_expires_at>clock_timestamp()`, r.ID, lease.Owner, lease.FencingToken, *r.ConfirmedSlot, proof.slot)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrStaleOwner
		}
		return nil
	})
}

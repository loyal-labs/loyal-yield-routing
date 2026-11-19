package referral

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math/big"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/earnmax"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

type CapitalReport struct {
	Coverage     string    `json:"coverage"`
	Reason       string    `json:"reason,omitempty"`
	Slot         uint64    `json:"asOfSlot,omitempty"`
	At           time.Time `json:"asOf"`
	Transactions int       `json:"verifiedMovements"`
	Earned       *string   `json:"earnedMicroUsdc,omitempty"`
	checkedReset bool
	lastReset    *uint64
	reset        *capitalReset
}

type capitalFact struct {
	sig      solana.Signature
	slot     uint64
	at       time.Time
	movement earnmax.CapitalMovement
}

func readCapitalFacts(ctx context.Context, c *chain.Client, authority earnmax.Authority, wallet solana.PublicKey, addresses []solana.PublicKey, from, to uint64, start time.Time) ([]capitalFact, string, error) {
	oldest, err := c.FirstAvailableBlock(ctx)
	if err != nil {
		return nil, "", err
	}
	if oldest > from+1 {
		return nil, "history_not_retained", nil
	}
	const pageSize = 64
	signatures := map[solana.Signature]uint64{}
	var gap string
	for _, address := range addresses {
		var cursor solana.Signature
		var lastSlot uint64
		for page := 0; page < 3; page++ {
			rows, err := c.History(ctx, address, pageSize, cursor, rpc.CommitmentFinalized, to)
			if err != nil {
				return nil, "", err
			}
			for _, row := range rows {
				if (lastSlot != 0 && row.Slot > lastSlot) || row.Signature == cursor {
					return nil, "inconsistent_history_page", nil
				}
				lastSlot = row.Slot
				if !row.Failed && row.Slot > from && row.Slot <= to {
					if prior, exists := signatures[row.Signature]; exists && prior != row.Slot {
						return nil, "inconsistent_transaction_slot", nil
					}
					signatures[row.Signature] = row.Slot
				}
			}
			if len(rows) < pageSize || lastSlot <= from {
				break
			}
			if page == 2 {
				gap = "history_page_limit"
			}
			cursor = rows[len(rows)-1].Signature
		}
	}
	sigs := make([]solana.Signature, 0, len(signatures))
	for s := range signatures {
		sigs = append(sigs, s)
	}
	// Bound batch size and total work per invocation; an uncovered interval never earns.
	if len(sigs) > 96 {
		return nil, "transaction_batch_limit", nil
	}
	var facts []capitalFact
	for len(sigs) > 0 {
		batch := sigs[:min(12, len(sigs))]
		sigs = sigs[len(batch):]
		executions, err := c.Executions(ctx, batch, rpc.CommitmentFinalized)
		if err != nil {
			return nil, "", err
		}
		for i, signature := range batch {
			e := executions[i]
			if e == nil || e.Slot != signatures[signature] {
				gap = "finalized_transaction_missing"
				continue
			}
			m, err := earnmax.CapitalMovementAt(*e, signature, authority)
			if err != nil {
				gap = "unverified_transaction"
				continue
			}
			if m != nil {
				m.BoundWalletSigned = e.Transaction.Message.IsSigner(wallet)
				for _, ev := range m.Events {
					if ev.Kind == "deposit" && !time.Unix(ev.UnixTime, 0).Before(start) && !m.BoundWalletSigned {
						gap = "historical_deposit_owner_unproved"
					}
				}
				facts = append(facts, capitalFact{signature, e.Slot, e.BlockTime.UTC(), *m})
			}
		}
	}
	sort.Slice(facts, func(i, j int) bool { return facts[i].slot < facts[j].slot })
	for i := 1; i < len(facts); i++ {
		if facts[i].slot == facts[i-1].slot {
			gap = "same_slot_order_unproved"
		}
	}
	return facts, gap, nil
}

func (r *CapitalReport) setClock(slot uint64, accounts []*chain.Account, launch, bound time.Time) error {
	if len(accounts) == 0 || accounts[0] == nil || len(accounts[0].Data) != 40 || binary.LittleEndian.Uint64(accounts[0].Data) != slot {
		return errors.New("missing finalized clock")
	}
	r.Slot = slot
	r.At = time.Unix(int64(binary.LittleEndian.Uint64(accounts[0].Data[32:])), 0).UTC()
	if r.At.Before(launch) || r.At.Before(bound) {
		return errors.New("finalized clock precedes eligibility")
	}
	return nil
}

// CollectCapital records finalized evidence for one already-bound referee. The
// operator can inspect an individual result without granting any signer access.
func (s *Store) CollectCapital(ctx context.Context, c *chain.Client, id string, dryRun bool) (CapitalReport, error) {
	var out CapitalReport
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var launch uint64
	var began, bound time.Time
	var walletName string
	var conflict bool
	err := s.yield.QueryRow(ctx, `SELECT launch_slot,launched_at FROM loyal_yield.referral_earn_max_program WHERE singleton`).Scan(&launch, &began)
	if errors.Is(err, pgx.ErrNoRows) {
		out.Coverage = "inactive"
		out.Reason = "program_not_launched"
		return out, nil
	}
	if err != nil {
		return out, err
	}
	if err = s.yield.QueryRow(ctx, `SELECT b.referee_wallet,b.bound_at,EXISTS(SELECT 1 FROM loyal_yield.referral_attribution_conflicts c WHERE c.referee_app_user_id=b.referee_app_user_id) FROM loyal_yield.referral_bindings b WHERE b.referee_app_user_id=$1`, id).Scan(&walletName, &bound, &conflict); err != nil {
		return out, err
	}
	wallet, err := solana.PublicKeyFromBase58(walletName)
	if err != nil {
		return out, err
	}
	var names []string
	err = s.app.QueryRow(ctx, `SELECT COALESCE(array_agg(DISTINCT s.settings_pda),'{}') FROM app_users u JOIN app_user_smart_accounts s ON s.user_id=u.id WHERE u.id=$1 AND u.subject_address=$2 AND u.provider='solana' AND s.solana_env='mainnet' AND s.state='ready' AND s.settings_pda IS NOT NULL AND NOT EXISTS (SELECT 1 FROM app_user_smart_accounts other WHERE other.solana_env=s.solana_env AND other.settings_pda=s.settings_pda AND other.user_id<>s.user_id)`, id, walletName).Scan(&names)
	if err != nil {
		return out, err
	}
	genesis, err := c.GenesisHash(ctx)
	if err != nil {
		return out, err
	}
	if genesis.String() != "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d" {
		return out, errors.New("referral evidence requires Solana mainnet")
	}
	var saved []byte
	var priorAuthority string
	var priorSlot *uint64
	var priorAt *time.Time
	out.checkedReset = true
	err = s.yield.QueryRow(ctx, `SELECT authority,earned_through_slot,earned_through_at,replay,
	(SELECT max(zero_slot) FROM loyal_yield.referral_earn_max_capital_resets WHERE referee_app_user_id=$1)
	FROM loyal_yield.referral_earn_max_capital_status WHERE referee_app_user_id=$1`, id).Scan(&priorAuthority, &priorSlot, &priorAt, &saved, &out.lastReset)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if len(names) != 1 {
		out.Coverage = "incomplete"
		out.Reason = "ready_settings_unproved"
		if priorAuthority == "" {
			return out, nil
		}
		slot, ac, err := c.Accounts(ctx, []solana.PublicKey{solana.SysVarClockPubkey}, rpc.CommitmentFinalized, launch)
		if err != nil {
			return out, err
		}
		if err = out.setClock(slot, ac, began, bound); err != nil {
			return out, err
		}
		if dryRun {
			return out, nil
		}
		return out, s.storeCapital(ctx, id, walletName, priorAuthority, out, nil, nil)
	}
	settings, err := solana.PublicKeyFromBase58(names[0])
	if err != nil {
		return out, err
	}
	authority, err := earnmax.AuthorityOf(settings)
	if err != nil {
		return out, err
	}
	user, err := voltr.UserAccounts(earnmax.Vault, earnmax.LPMint, authority.Key())
	if err != nil {
		return out, err
	}
	escrow, err := spl.AssociatedTokenAddress(user.Receipt, earnmax.LPMint, solana.TokenProgramID)
	if err != nil {
		return out, err
	}
	slot, ac, err := c.Accounts(ctx, []solana.PublicKey{solana.SysVarClockPubkey, settings, user.LPATA, escrow, user.Receipt}, rpc.CommitmentFinalized, launch)
	if err != nil {
		return out, err
	}
	if err := out.setClock(slot, ac, began, bound); err != nil {
		return out, err
	}
	start := began
	if bound.After(start) {
		start = bound
	}
	state := capitalReplay{At: start.Unix()}
	from := launch - 1
	if priorAuthority != "" && priorAuthority != authority.Key().String() {
		out.Coverage, out.Reason = "incomplete", "bound_authority_changed"
		if dryRun {
			return out, nil
		}
		return out, s.storeCapital(ctx, id, walletName, priorAuthority, out, nil, nil)
	}
	if priorSlot != nil {
		from = *priorSlot
		if err = json.Unmarshal(saved, &state); err != nil {
			return out, err
		}
		if from > slot {
			return out, errors.New("RPC behind previous finalized observation")
		}
	}
	out.Coverage = "incomplete"
	signer, err := squads.DecodeSettings(ac[1])
	if err != nil {
		out.Reason = "settings_evidence_invalid"
		if dryRun {
			return out, nil
		}
		return out, s.storeCapital(ctx, id, walletName, authority.Key().String(), out, nil, nil)
	}
	controls := false
	for _, v := range signer.Signers {
		if v.Key == wallet {
			controls = true
			break
		}
	}
	if conflict {
		out.Reason = "attribution_conflict"
	}
	if !controls {
		out.Reason = "settings_signer_mismatch"
	}
	balance := func(i int, owner solana.PublicKey) (uint64, error) {
		if ac[i] == nil {
			return 0, nil
		}
		tok, err := spl.DecodeTokenAccount(ac[i])
		if err != nil || ac[i].Owner != solana.TokenProgramID || tok.Mint != earnmax.LPMint || tok.Owner != owner {
			return 0, errors.New("invalid LP account identity")
		}
		return tok.Amount, nil
	}
	free, err := balance(2, authority.Key())
	if err != nil {
		return out, err
	}
	held, err := balance(3, user.Receipt)
	if err != nil {
		return out, err
	}
	if ac[4] != nil {
		receipt, err := voltr.DecodeWithdrawalReceipt(ac[4])
		if err != nil || receipt.Vault != earnmax.Vault || receipt.User != authority.Key() || receipt.LPEscrowedRaw != held {
			out.Reason = "withdrawal_receipt_mismatch"
		}
	} else if held != 0 {
		out.Reason = "escrow_without_receipt"
	}
	var facts []capitalFact
	if out.Reason == "" {
		facts, out.Reason, err = readCapitalFacts(ctx, c, authority, wallet, []solana.PublicKey{authority.Key(), user.LPATA, user.Receipt, escrow}, from, slot, start)
		if err != nil {
			return out, err
		}
	}
	out.Transactions = len(facts)
	earnedBefore := new(big.Rat).Set(&state.Earned)
	failed := -1
	if out.Reason == "" {
		for i, f := range facts {
			if err := state.apply(f.movement, start); err != nil {
				failed = i
				out.Reason = err.Error()
				break
			}
		}
	}
	if out.Reason == "" && (state.Free != free || state.Escrow != held) {
		out.Reason = "finalized_balances_disagree"
	}
	if resettableOrigin(out.Reason) && slot > from {
		reset, recovered := recoverCapital(facts, failed, free, held, slot, out.At, start, earnedBefore)
		if reset != nil && reset.slot > from && (priorAt == nil || !reset.at.Before(*priorAt)) {
			reset.reason, reset.priorSlot, reset.priorAt, reset.priorReplay = out.Reason, priorSlot, priorAt, saved
			out.reset = reset
			state = *recovered
			out.Reason = ""
		}
	}
	if out.Reason == "" {
		if err := state.accrue(out.At.Unix()); err != nil {
			out.Reason = err.Error()
		} else {
			out.Coverage = "complete"
			if out.lastReset != nil || out.reset != nil {
				out.Coverage, out.Reason = "incomplete", "historical_gap"
			}
			v := new(big.Int).Quo(state.Earned.Num(), state.Earned.Denom()).String()
			out.Earned = &v
		}
	}
	if dryRun {
		return out, nil
	}
	return out, s.storeCapital(ctx, id, walletName, authority.Key().String(), out, &state, facts)
}

func (s *Store) storeCapital(ctx context.Context, id, wallet, authority string, out CapitalReport, replay *capitalReplay, facts []capitalFact) error {
	tx, err := s.yield.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// A new identity observation may concern a formerly unknown linked wallet.
	// Read the conflict view after taking the shared identity writer lock.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended('referral-identity-evidence', 0))`); err != nil {
		return err
	}
	// Serialize saves for the same binding; a slow read cannot replace a newer checkpoint.
	var conflict bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM loyal_yield.referral_attribution_conflicts c WHERE c.referee_app_user_id=b.referee_app_user_id) FROM loyal_yield.referral_bindings b WHERE b.referee_app_user_id=$1 AND b.referee_wallet=$2 FOR UPDATE OF b`, id, wallet).Scan(&conflict); err != nil {
		return err
	}
	if conflict && out.Earned != nil {
		return errors.New("attribution changed while verifying the account")
	}
	if out.checkedReset && (out.Earned != nil || out.reset != nil) {
		var same bool
		err = tx.QueryRow(ctx, `SELECT max(zero_slot) IS NOT DISTINCT FROM $2::bigint FROM loyal_yield.referral_earn_max_capital_resets WHERE referee_app_user_id=$1`, id, out.lastReset).Scan(&same)
		if err != nil {
			return err
		}
		if !same {
			return errors.New("capital recovery advanced concurrently; retry")
		}
	}
	if r := out.reset; r != nil {
		var same bool
		err = tx.QueryRow(ctx, `SELECT (SELECT earned_through_slot FROM loyal_yield.referral_earn_max_capital_status WHERE referee_app_user_id=$1) IS NOT DISTINCT FROM $2::bigint`, id, r.priorSlot).Scan(&same)
		if err != nil {
			return err
		}
		if !same {
			return errors.New("capital checkpoint advanced before recovery; retry")
		}
		_, err = tx.Exec(ctx, `INSERT INTO loyal_yield.referral_earn_max_capital_resets(referee_app_user_id,authority,prior_through_slot,prior_through_at,prior_replay,zero_slot,zero_at,zero_replay,reason) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, id, authority, r.priorSlot, r.priorAt, r.priorReplay, r.slot, r.at, r.replay, r.reason)
		if err != nil {
			return err
		}
	}
	for _, f := range facts {
		body, err := json.Marshal(f.movement)
		if err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO loyal_yield.referral_earn_max_capital_facts(referee_app_user_id,authority,signature,slot,block_time,movement) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING`, id, authority, f.sig.String(), f.slot, f.at, body)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var equal bool
			err = tx.QueryRow(ctx, `SELECT slot=$4 AND block_time=$5 AND movement=$6::jsonb FROM loyal_yield.referral_earn_max_capital_facts WHERE referee_app_user_id=$1 AND authority=$2 AND signature=$3`, id, authority, f.sig.String(), f.slot, f.at, body).Scan(&equal)
			if err != nil {
				return err
			}
			if !equal {
				return errors.New("conflicting finalized referral evidence")
			}
		}
	}
	var state []byte
	var earnedAt *time.Time
	var earnedSlot *uint64
	if out.Earned != nil {
		state, err = json.Marshal(replay)
		if err != nil {
			return err
		}
		earnedAt = &out.At
		earnedSlot = &out.Slot
	}
	tag, err := tx.Exec(ctx, `INSERT INTO loyal_yield.referral_earn_max_capital_status AS old(referee_app_user_id,authority,as_of_slot,as_of_at,coverage,reason,earned_micro_usdc,earned_through_slot,earned_through_at,replay)
 VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7::text::numeric,$8,$9,$10)
 ON CONFLICT (referee_app_user_id) DO UPDATE SET
 as_of_slot=greatest(old.as_of_slot,excluded.as_of_slot),
 as_of_at=CASE WHEN excluded.as_of_slot>old.as_of_slot OR (excluded.as_of_slot=old.as_of_slot
 AND (old.coverage='incomplete' OR excluded.coverage='complete' OR excluded.reason='attribution_conflict')) THEN excluded.as_of_at ELSE old.as_of_at END,
 coverage=CASE WHEN excluded.as_of_slot>old.as_of_slot OR (excluded.as_of_slot=old.as_of_slot
 AND (old.coverage='incomplete' OR excluded.coverage='complete' OR excluded.reason='attribution_conflict')) THEN excluded.coverage ELSE old.coverage END,
 reason=CASE WHEN excluded.as_of_slot>old.as_of_slot OR (excluded.as_of_slot=old.as_of_slot
 AND (old.coverage='incomplete' OR excluded.coverage='complete' OR excluded.reason='attribution_conflict')) THEN excluded.reason ELSE old.reason END,
 earned_micro_usdc=CASE WHEN excluded.earned_through_slot>=COALESCE(old.earned_through_slot,0) THEN excluded.earned_micro_usdc ELSE old.earned_micro_usdc END,
 earned_through_slot=greatest(old.earned_through_slot,excluded.earned_through_slot),
 earned_through_at=CASE WHEN excluded.earned_through_slot>=COALESCE(old.earned_through_slot,0) THEN excluded.earned_through_at ELSE old.earned_through_at END,
 replay=CASE WHEN excluded.earned_through_slot>=COALESCE(old.earned_through_slot,0) THEN excluded.replay ELSE old.replay END
 WHERE old.authority=excluded.authority AND (old.earned_through_slot IS DISTINCT FROM excluded.earned_through_slot OR excluded.replay IS NULL OR old.replay=excluded.replay)`, id, authority, out.Slot, out.At, out.Coverage, out.Reason, out.Earned, earnedSlot, earnedAt, state)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("conflicting finalized referral checkpoint or account identity")
	}
	return tx.Commit(ctx)
}

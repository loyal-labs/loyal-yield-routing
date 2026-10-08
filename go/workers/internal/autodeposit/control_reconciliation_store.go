package autodeposit

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	WorkersDB "github.com/loyal-labs/loyal-yield-routing/go/workers/internal/db"
)

const controlTargetColumns = `id,setup_generation,policy_seed,settings,wallet,wallet_token_ata,vault_pubkey,COALESCE(vault_token_ata,vault_usdc_ata,''),token_mint,policy_account,subscription_authority,recurring_delegation,recurring_delegation_nonce,max_amount_per_period,start_timestamp,COALESCE(cluster,'')`

func scanControlTarget(row pgx.Row) (ControlTarget, error) {
	var t ControlTarget
	err := row.Scan(&t.TargetID, &t.SetupGeneration, &t.PolicySeed, &t.Settings, &t.Wallet, &t.WalletTokenATA, &t.Vault, &t.VaultTokenATA, &t.Mint, &t.Policy, &t.SubscriptionAuthority, &t.RecurringDelegation, &t.Nonce, &t.MaxAmountPerPeriod, &t.StartTimestamp, &t.Cluster)
	return t, err
}

func (s *Store) LoadControlTarget(ctx context.Context, targetID int64) (*ControlTarget, error) {
	t, err := scanControlTarget(s.pool.QueryRow(ctx, `SELECT `+controlTargetColumns+` FROM loyal_yield.balance_sweep_targets WHERE id=$1 AND cluster='mainnet-beta' AND subscription_authority IS NOT NULL AND recurring_delegation IS NOT NULL AND recurring_delegation_nonce IS NOT NULL AND max_amount_per_period>0`, targetID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// ApplyControlObservation ports the current Yield store's generation bootstrap
// (store.rs2136..2320), not the pre-0059 App's -targetID bootstrap. It fences
// both the outbox lease and the exact target generation/identity in one SQL
// transaction. The existing target trigger emits the configuration outbox.
func (s *Store) ApplyControlObservation(ctx context.Context, request ReconciliationRequest, owner string, o ControlObservation) error {
	if request.TargetID != o.Target.TargetID || o.ObservedSlot <= 0 || o.ObservedSlot < request.RequestedSlot || o.WalletBalanceRaw < 0 {
		return errors.New("control observation identity, balance or requested slot invalid")
	}
	if o.status() == "active" && !sha256HexPattern.MatchString(o.WalletAccountDataSHA256) {
		return errors.New("active control observation lacks decoded wallet account evidence")
	}
	return WorkersDB.WithTx(ctx, s.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		// Policy discovery already locks target before raising its request.
		// Keep that ordering so application cannot deadlock with discovery.
		target, err := scanControlTarget(tx.QueryRow(ctx, `SELECT `+controlTargetColumns+` FROM loyal_yield.balance_sweep_targets WHERE id=$1 FOR UPDATE`, request.TargetID))
		if err != nil {
			return err
		}
		var requestID int64
		if err := tx.QueryRow(ctx, `SELECT target_id FROM loyal_yield.autodeposit_reconciliation_requests WHERE target_id=$1 AND claim_owner=$2 AND claim_expires_at>now() FOR UPDATE`, request.TargetID, owner).Scan(&requestID); errors.Is(err, pgx.ErrNoRows) {
			return ErrOwnershipLost
		} else if err != nil {
			return err
		}
		if target.Cluster != mainnetCluster {
			return ErrChainNamespace
		}
		if !reflect.DeepEqual(target, o.Target) {
			return errors.New("control target generation or identity changed during observation")
		}
		var currentSlot int64
		var status string
		var bootstrap, floor *int64
		var now time.Time
		if err = tx.QueryRow(ctx, `SELECT chain_observation_slot,chain_status,bootstrap_generation,wallet_balance_floor_raw,now() FROM loyal_yield.balance_sweep_targets WHERE id=$1`, request.TargetID).Scan(&currentSlot, &status, &bootstrap, &floor, &now); err != nil {
			return err
		}
		if o.ObservedSlot >= currentSlot && status != "closed" {
			status = o.status()
			// App floor rebasing depends on this projection. A coherent decoded
			// snapshot may advance it; absent accounts never become zero balances.
			if sha256HexPattern.MatchString(o.WalletAccountDataSHA256) {
				var walletSlot, walletAmount int64
				var walletHash *string
				var walletCommitment string
				walletErr := tx.QueryRow(ctx, `SELECT observed_slot,amount_raw,account_data_hash,source_commitment FROM loyal_yield.balance_sweep_wallet_balances_current WHERE target_id=$1 AND mint=$2 FOR UPDATE`, target.TargetID, target.Mint).Scan(&walletSlot, &walletAmount, &walletHash, &walletCommitment)
				if walletErr != nil && !errors.Is(walletErr, pgx.ErrNoRows) {
					return walletErr
				}
				if walletSlot == o.ObservedSlot && (walletCommitment == "confirmed" || walletCommitment == "finalized") && (walletAmount != o.WalletBalanceRaw || walletHash != nil && *walletHash != o.WalletAccountDataSHA256) {
					return errors.New("control wallet proof contradicts same-slot projection")
				}
				if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.balance_sweep_wallet_balances_current(target_id,wallet,wallet_usdc_ata,wallet_token_ata,amount_raw,owner,mint,observed_slot,observed_at,source,source_commitment,account_data_hash,raw_evidence,updated_at)VALUES($1,$2,$3,$3,$4,$2,$5,$6,now(),'go_autodeposit_control_snapshot','confirmed',$7,jsonb_build_object('setupGeneration',$8::bigint),now())ON CONFLICT(target_id,mint)DO UPDATE SET wallet=EXCLUDED.wallet,wallet_usdc_ata=EXCLUDED.wallet_usdc_ata,wallet_token_ata=EXCLUDED.wallet_token_ata,amount_raw=EXCLUDED.amount_raw,owner=EXCLUDED.owner,observed_slot=EXCLUDED.observed_slot,observed_at=EXCLUDED.observed_at,source=EXCLUDED.source,source_commitment=EXCLUDED.source_commitment,account_data_hash=EXCLUDED.account_data_hash,raw_evidence=EXCLUDED.raw_evidence,updated_at=now()WHERE loyal_yield.balance_sweep_wallet_balances_current.observed_slot<EXCLUDED.observed_slot OR (loyal_yield.balance_sweep_wallet_balances_current.observed_slot=EXCLUDED.observed_slot AND loyal_yield.balance_sweep_wallet_balances_current.source_commitment<>'finalized')`, target.TargetID, target.Wallet, target.WalletTokenATA, o.WalletBalanceRaw, target.Mint, o.ObservedSlot, o.WalletAccountDataSHA256, target.SetupGeneration); err != nil {
					return err
				}
			}
			if _, err = tx.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET chain_status=$2,chain_observation_slot=$3,last_seen_at=now(),last_seen_slot=GREATEST(last_seen_slot,$3) WHERE id=$1`, request.TargetID, status, o.ObservedSlot); err != nil {
				return err
			}
			// The initial surplus is automatic work and needs the Earn route as
			// its destination, as every projected lot does: a lot scheduled on an
			// unrouted vault is never dispatched, skipped or closed. Bootstrap
			// waits for the route; the wallet change of the first Earn deposit
			// raises the next control observation. (Intentional correction of the
			// Yield store's bootstrap, which wrote lots on unrouted vaults.)
			var routed bool
			if err = tx.QueryRow(ctx, `SELECT `+targetRoutedSQL+` FROM loyal_yield.balance_sweep_targets AS target WHERE target.id=$1`, request.TargetID).Scan(&routed); err != nil {
				return err
			}
			if status == "active" && routed && (bootstrap == nil || *bootstrap != target.SetupGeneration) && floor != nil {
				if *floor < 0 {
					return errors.New("control target protection floor is negative")
				}
				if o.WalletBalanceRaw > *floor {
					var eventID int64
					inserted := false
					for attempt := 0; attempt < 16; attempt++ {
						err = tx.QueryRow(ctx, `INSERT INTO loyal_yield.balance_sweep_wallet_balance_events(event_id,target_id,wallet,wallet_usdc_ata,wallet_token_ata,mint,previous_amount_raw,amount_raw,delta_amount_raw,observed_slot,observed_at,source,source_commitment,raw_evidence,projected_at) VALUES(nextval('loyal_yield.autodeposit_bootstrap_event_id_seq'),$1,$2,$3,$3,$4,NULL,$5,NULL,$6,now(),'laserstream_autodeposit_activation','confirmed',jsonb_build_object('bootstrapGeneration',$7::bigint,'producer','go_control_reconciliation'),now()) ON CONFLICT(event_id) DO NOTHING RETURNING event_id`, target.TargetID, target.Wallet, target.WalletTokenATA, target.Mint, o.WalletBalanceRaw, o.ObservedSlot, target.SetupGeneration).Scan(&eventID)
						if err == nil {
							inserted = true
							break
						}
						if !errors.Is(err, pgx.ErrNoRows) {
							return err
						}
					}
					if !inserted {
						return errors.New("control bootstrap synthetic event range could not allocate an identity")
					}
					eligible := now.Add(time.Hour)
					if target.StartTimestamp != nil {
						start := time.Unix(*target.StartTimestamp, 0)
						if start.After(eligible) {
							eligible = start
						}
					}
					var slotID int64
					if err = tx.QueryRow(ctx, `INSERT INTO loyal_yield.balance_sweep_scheduled_slots(target_id,token_mint,eligible_after,status)VALUES($1,$2,$3,'scheduled')RETURNING id`, target.TargetID, target.Mint, eligible).Scan(&slotID); err != nil {
						return err
					}
					if _, err = tx.Exec(ctx, `INSERT INTO loyal_yield.balance_sweep_surplus_lots(target_id,scheduled_slot_id,source_event_id,original_amount_raw,remaining_amount_raw,classification,eligible_after,status,confidence,reason)VALUES($1,$2,$3,$4,$4,'initial_surplus',$5,'open','confirmed_snapshot','initial Autodeposit surplus observed by Go reconciliation')`, target.TargetID, slotID, eventID, o.WalletBalanceRaw-*floor, eligible); err != nil {
						return err
					}
				}
				if _, err = tx.Exec(ctx, `UPDATE loyal_yield.balance_sweep_targets SET bootstrap_generation=$2 WHERE id=$1`, target.TargetID, target.SetupGeneration); err != nil {
					return err
				}
			}
			if status == "closed" {
				if _, err = tx.Exec(ctx, `UPDATE loyal_yield.balance_sweep_scheduled_slots SET status='canceled',updated_at=now()WHERE target_id=$1 AND status IN('scheduled','requested')`, target.TargetID); err != nil {
					return err
				}
				if _, err = tx.Exec(ctx, `UPDATE loyal_yield.balance_sweep_surplus_lots SET status='suppressed',updated_at=now()WHERE target_id=$1 AND status='open' AND remaining_amount_raw>0`, target.TargetID); err != nil {
					return err
				}
			}
		}
		// A concurrently raised request survives through the high-water mark.
		tag, err := tx.Exec(ctx, `UPDATE loyal_yield.autodeposit_reconciliation_requests SET processed_slot=GREATEST(processed_slot,$3),requested_slot=GREATEST(requested_slot,$3),attempt_count=0,claim_owner=NULL,claim_expires_at=NULL,last_error=NULL,updated_at=now()WHERE target_id=$1 AND claim_owner=$2 AND claim_expires_at>now()`, request.TargetID, owner, o.ObservedSlot)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrOwnershipLost
		}
		return nil
	})
}

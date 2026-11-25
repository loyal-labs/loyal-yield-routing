package backyard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
)

// ActivatePerformanceFee seeds the Looping route once. Its first reconciled NAV
// report sets the economic baseline against the inherited Voltr high-water mark.
func ActivatePerformanceFee(ctx context.Context, out io.Writer, endpoint, dsn string, weeklyCap int64, apply bool) error {
	literal, err := PerformanceFeePolicy(looping.Name, weeklyCap)
	if err != nil {
		return err
	}
	client, err := chain.New(endpoint, 15*time.Second)
	if err != nil {
		return err
	}
	genesis, err := client.GenesisHash(ctx)
	if err != nil {
		return err
	}
	if genesis.String() != "5eykt4UsFv8P8NJdTREpY1vzqKqZKvdpKuc147dw2N9d" {
		return errors.New("Looping fee activation requires Solana mainnet")
	}
	keys := []solana.PublicKey{solanaKey(looping.VoltrVault), solanaKey(looping.lpMint), solanaKey(loopingTreasuryUSDC)}
	slot, accounts, err := client.Accounts(ctx, keys, rpc.CommitmentFinalized, 0)
	if err != nil {
		return err
	}
	if slot == 0 || slot > math.MaxInt64 || len(accounts) != len(keys) || accounts[0] == nil || accounts[1] == nil || accounts[2] == nil {
		return errors.New("missing finalized Looping vault, LP mint or USDC treasury")
	}
	book, err := decodeVoltrVaultBook(looping, confirmedAccount(looping.VoltrVault, accounts[0]))
	if err != nil {
		return err
	}
	if book.AdminPerformanceFeeBPS != 0 || book.ManagerPerformanceFeeBPS != 0 || book.ProtocolPerformanceFeeBPS != 0 ||
		book.AdminManagementFeeBPS != 0 || book.ManagerManagementFeeBPS != 0 || book.ProtocolManagementFeeBPS != 0 ||
		book.IssuanceFeeBPS != 0 || book.RedemptionFeeBPS != 0 {
		return errors.New("Looping's finalized Voltr fee configuration must be zero before activating the worker fee")
	}
	lp, err := decodeVoltrLPSupply(looping, confirmedAccount(looping.lpMint, accounts[1]))
	if err != nil {
		return err
	}
	_, supply, err := book.LPTotalsRaw(lp)
	if err != nil || supply == 0 {
		return errors.New("invalid effective Looping LP supply")
	}
	treasury, err := spl.DecodeTokenAccount(accounts[2])
	if err != nil || treasury.Program != solana.TokenProgramID || treasury.Mint != solanaKey(fleet.USDCMint) || treasury.Owner != solanaKey(backyardSettingsSigner) || treasury.Frozen {
		return errors.New("the fee treasury must be the confirmed wallet's initialized USDC account")
	}
	settings := solanaKey(backyardSettings)
	installed, err := squads.PoliciesAt(ctx, client, settings, rpc.CommitmentFinalized, slot)
	if err != nil {
		return err
	}
	policy, matches := squads.FindPolicy(installed, settings, solanaKey(backyardDelegate), literal)
	if matches != 1 {
		return fmt.Errorf("expected one finalized Looping payout policy with weekly cap %d, found %d", weeklyCap, matches)
	}
	checkpoint := performanceFeeState{ActivationSlot: int64(slot), WeeklyCapRaw: weeklyCap}
	status := "dry-run"
	if apply {
		database, err := openOperatorWriter(ctx, dsn, looping.Name)
		if err != nil {
			return err
		}
		defer database.Close()
		checkpoint, status, err = database.initializePerformanceFee(ctx, checkpoint)
		if err != nil {
			return err
		}
	}
	return json.NewEncoder(out).Encode(map[string]any{
		"status": status, "vault": looping.VoltrVault, "policy": policy.Account.String(), "treasury": loopingTreasuryUSDC,
		"finalizedSlot": slot, "highWaterMarkBits": book.HighWaterMarkBits.Big().String(), "accruedVoltrAdminLp": book.FeeAccumulatorAdminRaw,
		"effectiveLpSupply": supply, "checkpoint": checkpoint,
	})
}

func (d *Database) initializePerformanceFee(ctx context.Context, seed performanceFeeState) (performanceFeeState, string, error) {
	if d.vault != looping || !d.writer || !seed.valid() || seed.Version != 0 {
		return performanceFeeState{}, "", errors.New("a new checkpoint and the Looping writer lock are required")
	}
	status := "initialized"
	err := d.inTx(ctx, func(tx pgx.Tx) error {
		var existing json.RawMessage
		var journalReady bool
		err := tx.QueryRow(ctx, `SELECT state->'performanceFee', to_regclass('loyal_yield.backyard_looping_fee_finalized_version') IS NOT NULL
			FROM loyal_yield.multiply_route_states WHERE route_key=$1 FOR UPDATE`, looping.RouteKey).Scan(&existing, &journalReady)
		if err != nil {
			return fmt.Errorf("read Looping checkpoint: %w", err)
		}
		if !journalReady {
			return errors.New("apply the Looping performance fee migration before activating the checkpoint")
		}
		var canReconcileCapital bool
		if err := tx.QueryRow(ctx, `SELECT has_table_privilege(current_user, $1::text, 'SELECT')
			AND has_column_privilege(current_user, $1::text, 'applied_operation_id', 'UPDATE')`, resolutionTable).Scan(&canReconcileCapital); err != nil {
			return fmt.Errorf("check Looping fee worker access: %w", err)
		}
		if !canReconcileCapital {
			return errors.New("the Looping worker requires SELECT and UPDATE(applied_operation_id) on its fee capital resolutions")
		}
		if len(existing) > 0 {
			var current performanceFeeState
			if json.Unmarshal(existing, &current) != nil || !current.valid() || current.WeeklyCapRaw != seed.WeeklyCapRaw {
				return errors.New("the existing Looping fee checkpoint is invalid or has a different cap")
			}
			seed, status = current, "already-initialized"
			return nil
		}
		var busy, priorFee bool
		err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM loyal_yield.multiply_operations WHERE route_key=$1 AND status IN (`+nonterminalStatusSQL+`))
			OR EXISTS (SELECT FROM loyal_yield.backyard_manual_recovery_latches WHERE route_key=$1 AND cleared_at IS NULL),
			EXISTS (SELECT FROM loyal_yield.multiply_operations WHERE route_key=$1 AND
				(action='PAY_PERFORMANCE_FEE' OR expected_effects #> '{expectedEffects,performanceFee}' IS NOT NULL))`, looping.RouteKey).Scan(&busy, &priorFee)
		if err != nil {
			return err
		}
		if priorFee {
			return errors.New("existing fee journal requires recovery of the original Looping checkpoint")
		}
		if busy {
			return errors.New("Looping must finish its pending operation and clear any active recovery hold before activation")
		}
		_, err = tx.Exec(ctx, `UPDATE loyal_yield.multiply_route_states SET state=jsonb_set(state,'{performanceFee}',$2::jsonb),updated_at=clock_timestamp() WHERE route_key=$1`, looping.RouteKey, seed)
		return err
	})
	return seed, status, err
}

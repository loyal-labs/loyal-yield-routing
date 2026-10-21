package policy

import (
	"context"
	"fmt"
	"math"
	"math/big"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

// LoopAmounts are a KLend loop's op amounts in raw liquidity units:
// collateral deposited and withdrawn (math.MaxUint64 withdraws all KLend
// allows), debt borrowed and repaid (math.MaxUint64, or anything above the
// debt, repays exactly the debt: KLend caps a repayment at it).
type LoopAmounts struct {
	Deposit, Withdraw, Borrow, Repay uint64
}

const (
	LoopDeposit = iota
	LoopWithdraw
	LoopBorrow
	LoopRepay
	LoopInitialize
	LoopLegs
)

type loopLane struct {
	vault, market, collateralReserve, collateralMint, debtMint solana.PublicKey
	collateralCustody, debtCustody, obligation, metadata       solana.PublicKey
}

func newLoopLane(vault, market, collateralReserve, collateralMint, collateralProgram, debtMint, debtProgram solana.PublicKey) (loopLane, error) {
	l := loopLane{vault: vault, market: market, collateralReserve: collateralReserve, collateralMint: collateralMint, debtMint: debtMint}
	var err error
	if l.collateralCustody, err = spl.AssociatedTokenAddress(vault, collateralMint, collateralProgram); err != nil {
		return loopLane{}, err
	}
	if l.debtCustody, err = spl.AssociatedTokenAddress(vault, debtMint, debtProgram); err != nil {
		return loopLane{}, err
	}
	if l.obligation, err = kamino.ObligationAddress(1, 0, vault, market, collateralMint, debtMint); err != nil {
		return loopLane{}, err
	}
	l.metadata, err = kamino.UserMetadataAddress(vault)
	return l, err
}

func (l loopLane) constraints() [LoopLegs]squads.InstructionConstraintView {
	var out [LoopLegs]squads.InstructionConstraintView
	collateral := kamino.OwnCollateralAllowed(l.vault, squads.Pin(l.collateralReserve), squads.Pin(l.collateralCustody))
	debt := kamino.OwnDebtAllowed(l.vault, squads.Pin(l.market), squads.Pin(l.debtCustody))
	out[LoopDeposit] = kamino.DepositV2Allowed(collateral, squads.Unpinned)
	out[LoopWithdraw] = kamino.WithdrawV2Allowed(collateral, squads.Unpinned)
	out[LoopBorrow] = kamino.BorrowV2Allowed(debt, squads.Unpinned)
	out[LoopRepay] = kamino.RepayV2Allowed(debt, squads.Unpinned)
	out[LoopInitialize] = kamino.InitObligationAllowed(kamino.ObligationInitAllowed{Owner: squads.Pin(l.vault), FeePayer: squads.Pin(l.vault),
		Obligation: squads.Pin(l.obligation), LendingMarket: squads.Pin(l.market), Seed1: squads.Pin(l.collateralMint),
		Seed2: squads.Pin(l.debtMint), OwnerUserMetadata: squads.Pin(l.metadata)}, 1, 0)
	return out
}

func LoopConstraints(vault, market, collateralReserve, collateralMint, collateralProgram, debtMint, debtProgram solana.PublicKey) ([LoopLegs]squads.InstructionConstraintView, error) {
	lane, err := newLoopLane(vault, market, collateralReserve, collateralMint, collateralProgram, debtMint, debtProgram)
	if err != nil {
		return [LoopLegs]squads.InstructionConstraintView{}, err
	}
	return lane.constraints(), nil
}

func KLendLoop(c *chain.Client, settings solana.PublicKey, vaultIndex uint8, collateralReserve, debtReserve, payer solana.PublicKey, amounts LoopAmounts) Build {
	return func(ctx context.Context, minSlot uint64) (Product, error) {
		vault, _, err := squads.SmartAccountAddress(settings, vaultIndex)
		if err != nil {
			return Product{}, err
		}
		_, read, err := c.Accounts(ctx, []solana.PublicKey{collateralReserve, debtReserve}, rpc.CommitmentConfirmed, minSlot)
		if err != nil {
			return Product{}, err
		}
		collateral, err := kamino.DecodeReserve(read[0])
		if err != nil {
			return Product{}, fmt.Errorf("collateral reserve %s: %w", collateralReserve, err)
		}
		debt, err := kamino.DecodeReserve(read[1])
		if err != nil {
			return Product{}, fmt.Errorf("debt reserve %s: %w", debtReserve, err)
		}
		market := collateral.LendingMarket
		if debt.LendingMarket != market {
			return Product{}, fmt.Errorf("reserves %s and %s are in different markets", collateralReserve, debtReserve)
		}
		lane, err := newLoopLane(vault, market, collateralReserve, collateral.LiquidityMint, collateral.LiquidityTokenProgram,
			debt.LiquidityMint, debt.LiquidityTokenProgram)
		if err != nil {
			return Product{}, err
		}
		authority, err := kamino.LendingMarketAuthority(market)
		if err != nil {
			return Product{}, err
		}
		keys := []solana.PublicKey{lane.metadata, lane.obligation, vault}
		var collateralFarm, debtFarm solana.PublicKey
		if !collateral.FarmCollateral.IsZero() {
			if collateralFarm, err = kamino.ObligationFarmUserState(collateral.FarmCollateral, lane.obligation); err != nil {
				return Product{}, err
			}
			keys = append(keys, collateralFarm)
		}
		if !debt.FarmDebt.IsZero() {
			if debtFarm, err = kamino.ObligationFarmUserState(debt.FarmDebt, lane.obligation); err != nil {
				return Product{}, err
			}
			keys = append(keys, debtFarm)
		}
		_, state, err := c.Accounts(ctx, keys, rpc.CommitmentConfirmed, minSlot)
		if err != nil {
			return Product{}, err
		}
		if state[0] == nil {
			return Product{}, fmt.Errorf("vault %s has no KLend user metadata; create it with the klend product", vault)
		}
		farmExists := map[solana.PublicKey]bool{}
		for i, key := range keys[3:] {
			farmExists[key] = state[3+i] != nil
		}
		var topUp []solana.Instruction
		if state[1] == nil {
			rent, err := c.RentExempt(ctx, kamino.ObligationSize)
			if err != nil {
				return Product{}, err
			}
			keep, err := c.RentExempt(ctx, 0)
			if err != nil {
				return Product{}, err
			}
			var held uint64
			if state[2] != nil {
				held = state[2].Lamports
			}
			if held < rent+keep {
				topUp = []solana.Instruction{spl.SystemTransfer(payer, vault, rent+keep-held)}
			}
		}
		var obligation kamino.Obligation
		if state[1] != nil {
			if obligation, err = kamino.DecodeObligation(state[1]); err != nil {
				return Product{}, err
			}
		}
		var debtNow uint64
		for _, b := range obligation.Borrows {
			if b.Reserve == debtReserve {
				debtNow = ceilSF(b.BorrowedAmountSF)
			}
		}
		withdraw := amounts.Withdraw
		if withdraw != math.MaxUint64 {
			if withdraw, err = collateral.LiquidityToCollateral(withdraw); err != nil {
				return Product{}, err
			}
		}

		refresh := []solana.Instruction{}
		for _, r := range []struct {
			key     solana.PublicKey
			reserve kamino.Reserve
		}{{collateralReserve, collateral}, {debtReserve, debt}} {
			refresh = append(refresh, kamino.RefreshReserve(kamino.RefreshReserveAccounts{Reserve: r.key, LendingMarket: market,
				Pyth: r.reserve.PythPrice, SwitchboardPrice: r.reserve.SwitchboardPriceAggregator,
				SwitchboardTWAP: r.reserve.SwitchboardTWAPAggregator, Scope: r.reserve.ScopePriceFeed}))
		}
		refresh = append(refresh, kamino.RefreshObligation(market, lane.obligation,
			append(obligation.DepositReserves(), obligation.BorrowReserves()...)...))
		farmInit := func(farm, reserveFarm, reserve solana.PublicKey, mode uint8) []solana.Instruction {
			if farm.IsZero() || farmExists[farm] {
				return nil
			}
			return []solana.Instruction{kamino.InitObligationFarmsForReserve(kamino.ObligationFarmsInitAccounts{Payer: payer, Owner: vault,
				Obligation: lane.obligation, LendingMarketAuthority: authority, Reserve: reserve, ReserveFarmState: reserveFarm,
				ObligationFarmUserState: farm, LendingMarket: market}, mode)}
		}
		collateralAccounts := kamino.CollateralAccounts{Owner: vault, Obligation: lane.obligation, LendingMarket: market,
			LendingMarketAuthority: authority, Reserve: collateralReserve, LiquidityMint: collateral.LiquidityMint,
			LiquiditySupply: collateral.LiquiditySupply, CollateralMint: collateral.CollateralMint, CollateralSupply: collateral.CollateralSupply,
			UserLiquidity: lane.collateralCustody, LiquidityTokenProgram: collateral.LiquidityTokenProgram,
			ObligationFarmUserState: collateralFarm, ReserveFarmState: collateral.FarmCollateral}
		debtAccounts := kamino.LiquidityAccounts{Owner: vault, Obligation: lane.obligation, LendingMarket: market,
			LendingMarketAuthority: authority, Reserve: debtReserve, LiquidityMint: debt.LiquidityMint, LiquiditySupply: debt.LiquiditySupply,
			UserLiquidity: lane.debtCustody, TokenProgram: debt.LiquidityTokenProgram, FeeReceiver: debt.FeeReceiver,
			ObligationFarmUserState: debtFarm, ReserveFarmState: debt.FarmDebt}
		createCollateral := spl.CreateIdempotentATA(payer, vault, collateral.LiquidityMint, collateral.LiquidityTokenProgram)
		createDebt := spl.CreateIdempotentATA(payer, vault, debt.LiquidityMint, debt.LiquidityTokenProgram)
		before := func(setup ...solana.Instruction) []solana.Instruction {
			return append(append([]solana.Instruction(nil), setup...), refresh...)
		}

		constraints := lane.constraints()
		ops := make([]Op, LoopLegs)
		ops[LoopDeposit] = Op{Name: fmt.Sprintf("deposit %d (deposited collateral %d)", amounts.Deposit, obligation.Collateral(collateralReserve)),
			Inner:  kamino.DepositV2(collateralAccounts, amounts.Deposit),
			Before: before(append([]solana.Instruction{createCollateral}, farmInit(collateralFarm, collateral.FarmCollateral, collateralReserve, 0)...)...)}
		ops[LoopWithdraw] = Op{Name: fmt.Sprintf("withdraw %d collateral (deposited %d)", withdraw, obligation.Collateral(collateralReserve)),
			Inner: kamino.WithdrawV2(collateralAccounts, withdraw), Before: before(createCollateral)}
		ops[LoopBorrow] = Op{Name: fmt.Sprintf("borrow %d (debt now %d)", amounts.Borrow, debtNow),
			Inner:  kamino.BorrowV2(debtAccounts, amounts.Borrow),
			Before: before(append([]solana.Instruction{createDebt}, farmInit(debtFarm, debt.FarmDebt, debtReserve, 1)...)...)}
		ops[LoopRepay] = Op{Name: fmt.Sprintf("repay %d (debt now %d, before interest since the last refresh)", amounts.Repay, debtNow),
			Inner: kamino.RepayV2(debtAccounts, amounts.Repay), Before: before()}
		ops[LoopInitialize] = Op{Name: "init lane obligation " + lane.obligation.String(),
			Inner: kamino.InitObligation(kamino.ObligationInitAccounts{Owner: vault, FeePayer: vault, Obligation: lane.obligation,
				LendingMarket: market, Seed1: lane.collateralMint, Seed2: lane.debtMint, OwnerUserMetadata: lane.metadata}, 1, 0),
			Before: topUp, Done: state[1] != nil}
		for i := range ops {
			ops[i].Allowed = constraints[i]
		}
		return Product{Name: fmt.Sprintf("klend-loop:%s:%s", collateralReserve, debtReserve), VaultIndex: vaultIndex, Ops: ops}, nil
	}
}

func ceilSF(sf [16]byte) uint64 {
	value := kamino.U128(sf)
	value.Add(value, new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 60), big.NewInt(1)))
	value.Rsh(value, 60)
	if !value.IsUint64() {
		return math.MaxUint64
	}
	return value.Uint64()
}

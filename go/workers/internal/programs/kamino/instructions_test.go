package kamino

import (
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// admits is squads.Admits for a built instruction; these constraints pin
// keys only.
func admits(c squads.InstructionConstraintView, ix *solana.GenericInstruction) bool {
	inner := squads.Instruction{ProgramID: ix.ProgramID(), Data: ix.DataBytes}
	for _, meta := range ix.AccountValues {
		inner.Accounts = append(inner.Accounts, *meta)
	}
	return squads.Admits(c, inner, nil)
}

// A policy constraint built from an instruction's own account order, every
// slot pinned (the fixed ones by squads.Pinned), admits exactly that
// instruction, with and without a farm.
func TestAllowedAdmitsItsOwnInstruction(t *testing.T) {
	k := func(seed byte) solana.PublicKey { return solana.PublicKey{seed} }
	pin := squads.Pin
	pinned := func(a CollateralAccounts) CollateralAllowed {
		return CollateralAllowed{Owner: pin(a.Owner), Obligation: pin(a.Obligation), LendingMarket: pin(a.LendingMarket),
			LendingMarketAuthority: pin(a.LendingMarketAuthority), Reserve: pin(a.Reserve), LiquidityMint: pin(a.LiquidityMint),
			LiquiditySupply: pin(a.LiquiditySupply), CollateralMint: pin(a.CollateralMint), CollateralSupply: pin(a.CollateralSupply),
			UserLiquidity: pin(a.UserLiquidity), LiquidityTokenProgram: pin(a.LiquidityTokenProgram),
			ObligationFarmUserState: pin(a.ObligationFarmUserState), ReserveFarmState: pin(a.ReserveFarmState)}
	}
	farmed := CollateralAccounts{Owner: k(1), Obligation: k(2), LendingMarket: k(3), LendingMarketAuthority: k(4), Reserve: k(5),
		LiquidityMint: k(6), LiquiditySupply: k(7), CollateralMint: k(8), CollateralSupply: k(9), UserLiquidity: k(10),
		LiquidityTokenProgram: solana.TokenProgramID, ObligationFarmUserState: k(11), ReserveFarmState: k(12)}
	bare := farmed
	bare.ObligationFarmUserState, bare.ReserveFarmState = solana.PublicKey{}, solana.PublicKey{}
	debt := LiquidityAccounts{Owner: k(1), Obligation: k(2), LendingMarket: k(3), LendingMarketAuthority: k(4), Reserve: k(5),
		LiquidityMint: k(6), LiquiditySupply: k(7), UserLiquidity: k(10), TokenProgram: solana.TokenProgramID, FeeReceiver: k(14)}
	debtAllowed := LiquidityAllowed{Owner: pin(k(1)), Obligation: pin(k(2)), LendingMarket: pin(k(3)), LendingMarketAuthority: pin(k(4)),
		Reserve: pin(k(5)), LiquidityMint: pin(k(6)), LiquiditySupply: pin(k(7)), UserLiquidity: pin(k(10)),
		TokenProgram: pin(solana.TokenProgramID), FeeReceiver: pin(k(14)), ObligationFarmUserState: pin(solana.PublicKey{}),
		ReserveFarmState: pin(solana.PublicKey{})}
	obligation := ObligationInitAccounts{Owner: k(1), FeePayer: k(1), Obligation: k(2), LendingMarket: k(3), OwnerUserMetadata: k(13)}
	metadata := UserMetadataInitAccounts{Owner: k(1), FeePayer: k(1), UserMetadata: k(13)}

	cases := map[string]struct {
		allowed squads.InstructionConstraintView
		ix      *solana.GenericInstruction
	}{
		"deposit farmed":  {DepositV2Allowed(pinned(farmed), squads.Pinned), DepositV2(farmed, 7)},
		"deposit bare":    {DepositV2Allowed(pinned(bare), squads.Pinned), DepositV2(bare, 7)},
		"withdraw farmed": {WithdrawV2Allowed(pinned(farmed), squads.Pinned), WithdrawV2(farmed, 7)},
		"withdraw bare":   {WithdrawV2Allowed(pinned(bare), squads.Pinned), WithdrawV2(bare, 7)},
		"borrow":          {BorrowV2Allowed(debtAllowed, squads.Pinned), BorrowV2(debt, 7)},
		"repay":           {RepayV2Allowed(debtAllowed, squads.Pinned), RepayV2(debt, 7)},
		"init obligation": {InitObligationAllowed(ObligationInitAllowed{pin(k(1)), pin(k(1)), pin(k(2)), pin(k(3)), pin(solana.PublicKey{}), pin(solana.PublicKey{}), pin(k(13))}, 0, 0), InitObligation(obligation, 0, 0)},
		"init metadata":   {InitUserMetadataAllowed(UserMetadataInitAllowed{pin(k(1)), pin(k(1)), pin(k(13))}, solana.PublicKey{}), InitUserMetadata(metadata, solana.PublicKey{})},
	}
	for name, tc := range cases {
		if len(tc.allowed.AccountConstraints) != len(tc.ix.Accounts()) {
			t.Fatalf("%s: %d of %d account slots pinned", name, len(tc.allowed.AccountConstraints), len(tc.ix.Accounts()))
		}
		if !admits(tc.allowed, tc.ix) {
			t.Fatalf("%s: constraint does not admit its own instruction", name)
		}
	}
}

// A literal that forgets a slot cannot leave that account free.
func TestAllowedRefusesAnUnsetSlot(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("a deposit constraint with an unset user liquidity slot was built")
		}
	}()
	DepositV2Allowed(CollateralAllowed{Owner: squads.Pin(ProgramID)}, squads.Unpinned)
}

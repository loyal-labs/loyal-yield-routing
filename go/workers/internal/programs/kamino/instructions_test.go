package kamino

import (
	"bytes"
	"testing"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
)

// admits is Squads' ProgramInteraction rule for one constraint: same program,
// leading data equal, every constrained account slot holding an allowed key.
func admits(c squads.InstructionConstraintView, ix *solana.GenericInstruction) bool {
	data, _ := ix.Data()
	if c.ProgramID != ix.ProgramID() {
		return false
	}
	for _, d := range c.DataConstraints {
		if d.DataOffset != 0 || !bytes.HasPrefix(data, d.DataValue.Bytes) {
			return false
		}
	}
	accounts := ix.Accounts()
	for _, a := range c.AccountConstraints {
		if int(a.AccountIndex) >= len(accounts) || !containsKey(a.Pubkeys, accounts[a.AccountIndex].PublicKey) {
			return false
		}
	}
	return true
}

func containsKey(keys []solana.PublicKey, key solana.PublicKey) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}

// A policy constraint built from an instruction's own account order, with its
// accounts pinned, admits that instruction, with and without a farm.
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
	farmless := func(a CollateralAllowed) CollateralAllowed {
		a.ObligationFarmUserState, a.ReserveFarmState = pin(ProgramID), pin(ProgramID)
		return a
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
		TokenProgram: pin(solana.TokenProgramID), FeeReceiver: pin(k(14)), ObligationFarmUserState: pin(ProgramID),
		ReserveFarmState: pin(ProgramID)}
	obligation := ObligationInitAccounts{Owner: k(1), FeePayer: k(1), Obligation: k(2), LendingMarket: k(3), OwnerUserMetadata: k(13)}
	metadata := UserMetadataInitAccounts{Owner: k(1), FeePayer: k(1), UserMetadata: k(13)}

	cases := map[string]struct {
		allowed squads.InstructionConstraintView
		ix      *solana.GenericInstruction
	}{
		"deposit farmed":  {DepositV2Allowed(pinned(farmed)), DepositV2(farmed, 7)},
		"deposit bare":    {DepositV2Allowed(farmless(pinned(bare))), DepositV2(bare, 7)},
		"withdraw farmed": {WithdrawV2Allowed(pinned(farmed)), WithdrawV2(farmed, 7)},
		"withdraw bare":   {WithdrawV2Allowed(farmless(pinned(bare))), WithdrawV2(bare, 7)},
		"borrow":          {BorrowV2Allowed(debtAllowed), BorrowV2(debt, 7)},
		"repay":           {RepayV2Allowed(debtAllowed), RepayV2(debt, 7)},
		"init obligation": {InitObligationAllowed(ObligationInitAllowed{pin(k(1)), pin(k(1)), pin(k(2)), pin(k(3)), pin(solana.PublicKey{}), pin(solana.PublicKey{}), pin(k(13))}, 0, 0), InitObligation(obligation, 0, 0)},
		"init metadata":   {InitUserMetadataAllowed(UserMetadataInitAllowed{pin(k(1)), pin(k(1)), pin(k(13))}, solana.PublicKey{}), InitUserMetadata(metadata, solana.PublicKey{})},
	}
	for name, tc := range cases {
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
	DepositV2Allowed(CollateralAllowed{Owner: squads.Pin(ProgramID)})
}

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

// A policy constraint built from an instruction's own account order admits
// exactly that instruction, every slot pinned, with and without a farm, and
// refuses it once any pinned account changes.
func TestAllowedAdmitsItsOwnInstruction(t *testing.T) {
	k := func(seed byte) solana.PublicKey { return solana.PublicKey{seed} }
	one := func(key solana.PublicKey) []solana.PublicKey { return []solana.PublicKey{key} }
	pinned := func(a CollateralAccounts) CollateralAllowed {
		out := CollateralAllowed{Owner: one(a.Owner), Obligation: one(a.Obligation), LendingMarket: one(a.LendingMarket),
			LendingMarketAuthority: one(a.LendingMarketAuthority), Reserve: one(a.Reserve), LiquidityMint: one(a.LiquidityMint),
			LiquiditySupply: one(a.LiquiditySupply), CollateralMint: one(a.CollateralMint), CollateralSupply: one(a.CollateralSupply),
			UserLiquidity: one(a.UserLiquidity), LiquidityTokenProgram: one(a.LiquidityTokenProgram),
			ObligationFarmUserState: one(a.ObligationFarmUserState), ReserveFarmState: one(a.ReserveFarmState)}
		if a.ReserveFarmState.IsZero() {
			out.ObligationFarmUserState, out.ReserveFarmState = one(ProgramID), one(ProgramID)
		}
		return out
	}
	farmed := CollateralAccounts{Owner: k(1), Obligation: k(2), LendingMarket: k(3), LendingMarketAuthority: k(4), Reserve: k(5),
		LiquidityMint: k(6), LiquiditySupply: k(7), CollateralMint: k(8), CollateralSupply: k(9), UserLiquidity: k(10),
		LiquidityTokenProgram: solana.TokenProgramID, ObligationFarmUserState: k(11), ReserveFarmState: k(12)}
	bare := farmed
	bare.ObligationFarmUserState, bare.ReserveFarmState = solana.PublicKey{}, solana.PublicKey{}

	cases := map[string]struct {
		allowed squads.InstructionConstraintView
		ix      *solana.GenericInstruction
	}{
		"deposit farmed":  {DepositV2Allowed(pinned(farmed)), DepositV2(farmed, 7)},
		"deposit bare":    {DepositV2Allowed(pinned(bare)), DepositV2(bare, 7)},
		"withdraw farmed": {WithdrawV2Allowed(pinned(farmed)), WithdrawV2(farmed, 7)},
		"withdraw bare":   {WithdrawV2Allowed(pinned(bare)), WithdrawV2(bare, 7)},
		"init obligation": {InitObligationAllowed(ObligationInit[[]solana.PublicKey]{one(k(1)), one(k(1)), one(k(2)), one(k(3)), one(solana.PublicKey{}), one(solana.PublicKey{}), one(k(13))}, 0, 0), InitObligation(k(1), k(1), k(2), k(3), solana.PublicKey{}, solana.PublicKey{}, k(13), 0, 0)},
		"init metadata":   {InitUserMetadataAllowed(UserMetadataInit[[]solana.PublicKey]{one(k(1)), one(k(1)), one(k(13))}, solana.PublicKey{}), InitUserMetadata(k(1), k(1), k(13), solana.PublicKey{})},
		"init farm":       {InitObligationFarmsForReserveAllowed(ObligationFarmsInit[[]solana.PublicKey]{one(k(14)), one(k(1)), one(k(2)), one(k(4)), one(k(5)), one(k(12)), one(k(11)), one(k(3))}, 0), InitObligationFarmsForReserve(InitObligationFarmsAccounts{k(14), k(1), k(2), k(4), k(5), k(12), k(11), k(3)}, 0)},
	}
	for name, tc := range cases {
		if len(tc.allowed.AccountConstraints) != len(tc.ix.Accounts()) {
			t.Fatalf("%s: %d of %d account slots pinned", name, len(tc.allowed.AccountConstraints), len(tc.ix.Accounts()))
		}
		if !admits(tc.allowed, tc.ix) {
			t.Fatalf("%s: constraint does not admit its own instruction", name)
		}
		for slot := range tc.ix.Accounts() {
			moved := *tc.ix
			moved.AccountValues = append(solana.AccountMetaSlice(nil), tc.ix.AccountValues...)
			meta := *moved.AccountValues[slot]
			meta.PublicKey = k(99)
			moved.AccountValues[slot] = &meta
			if admits(tc.allowed, &moved) {
				t.Fatalf("%s: constraint admits slot %d moved to another account", name, slot)
			}
		}
	}
}

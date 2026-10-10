package backyard

import (
	"encoding/binary"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/spl"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
	"github.com/solana-foundation/solana-go/v2"
)

// The AUTO lane's legs, in the order of its one Squads policy: a leg's value
// is the constraint index it executes under.
const (
	autoDeposit = iota
	autoWithdraw
	autoBorrow
	autoRepay
	autoSwapToCollateral   // USDC or the debt into the collateral
	autoSwapFromCollateral // the collateral into USDC or the debt
	autoSwapDebtToUSDC
	autoInitialize
	autoLegs
)

// autoPolicy is the AUTO lane's policy. The KLend legs pin the vault, an
// obligation it owns, and the reserve (or market) and custody they move; the
// swaps pin the vault, their custody accounts and no platform fee; the
// initializer pins every account of the lane's one obligation. KLend and
// Jupiter check the rest themselves.
func autoPolicy(route RuntimeRoute) ([autoLegs]squads.InstructionConstraintView, error) {
	var out [autoLegs]squads.InstructionConstraintView
	keys, err := publicKeys([]string{route.Kamino.Vault, route.Kamino.Market, route.Kamino.Obligation, route.Kamino.CollateralReserve,
		route.Kamino.CollateralMint, route.Kamino.DebtMint, route.CollateralCustody, route.DebtCustody, bridgeSquadsATA})
	if err != nil {
		return out, err
	}
	vault, market, obligation, reserve, collateralMint, debtMint, collateral, debt, usdc := keys[0], keys[1], keys[2], keys[3], keys[4], keys[5], keys[6], keys[7], keys[8]
	metadata, err := kamino.UserMetadataAddress(vault)
	if err != nil {
		return out, err
	}
	pin, free := squads.Pin, squads.Any
	owned := kamino.OwnedObligation(vault)

	collateralLeg := kamino.CollateralAllowed{Owner: pin(vault), Obligation: owned, LendingMarket: free, LendingMarketAuthority: free,
		Reserve: pin(reserve), LiquidityMint: free, LiquiditySupply: free, CollateralMint: free, CollateralSupply: free,
		UserLiquidity: pin(collateral), LiquidityTokenProgram: free, ObligationFarmUserState: free, ReserveFarmState: free}
	debtLeg := kamino.LiquidityAllowed{Owner: pin(vault), Obligation: owned, LendingMarket: pin(market), LendingMarketAuthority: free,
		Reserve: free, LiquidityMint: free, LiquiditySupply: free, UserLiquidity: pin(debt), TokenProgram: free, FeeReceiver: free,
		ObligationFarmUserState: free, ReserveFarmState: free}
	swap := func(source, destination squads.Slot) squads.InstructionConstraintView {
		return jupiter.SharedAccountsRouteAllowed(jupiter.SharedRoute[squads.Slot]{TokenProgram: free, ProgramAuthority: free,
			User: pin(vault), Source: source, ProgramSource: free, ProgramDestination: free, Destination: destination,
			SourceMint: free, DestinationMint: free, PlatformFee: pin(jupiter.ProgramID), Token2022Program: free, EventAuthority: free},
			squads.Unpinned)
	}

	out[autoDeposit] = kamino.DepositV2Allowed(collateralLeg, squads.Unpinned)
	out[autoWithdraw] = kamino.WithdrawV2Allowed(collateralLeg, squads.Unpinned)
	out[autoBorrow] = kamino.BorrowV2Allowed(debtLeg, squads.Unpinned)
	out[autoRepay] = kamino.RepayV2Allowed(debtLeg, squads.Unpinned)
	out[autoSwapToCollateral] = swap(pin(usdc, debt), pin(collateral))
	out[autoSwapFromCollateral] = swap(pin(collateral), pin(usdc, debt))
	out[autoSwapDebtToUSDC] = swap(pin(debt), pin(usdc))
	out[autoInitialize] = kamino.InitObligationAllowed(kamino.ObligationInitAllowed{Owner: pin(vault), FeePayer: pin(vault),
		Obligation: pin(obligation), LendingMarket: pin(market), Seed1: pin(collateralMint), Seed2: pin(debtMint),
		OwnerUserMetadata: pin(metadata)}, 1, 0, squads.Pinned)
	return out, nil
}

// The capital and NAV bridge policies run two legs in one Squads payload:
// ArmReport arms the report ticket, then the Voltr leg consumes it. A leg's
// value is the constraint index it executes under.
const (
	bridgeArmLeg = iota
	bridgeCapitalLeg
	bridgeCapitalLegs
)

// The staging policy runs one leg: the vault's USDC custody to the strategy.
const (
	bridgeStageLeg = iota
	bridgeStageLegs
)

// The adaptor report both bridge legs carry, in Voltr's additional_args: the
// 57-byte ReportV1 (encodeBridgeReport), whose NAV after is at
// bridgeReportNAVOffset.
const (
	bridgeReportLen       = 57
	bridgeReportNAVOffset = 17
)

// bridgeLiteral is one bridge policy: its constraints in leg order and its
// spending limits.
type bridgeLiteral struct {
	constraints []squads.InstructionConstraintView
	limits      []squads.SpendingLimitView
}

// bridgePolicies are the Voltr bridge's four policies, by the action each
// authorizes. Every leg pins the vault, the strategy-two config and the
// report ticket it touches, bounds its amount by the per-leg cap (exactly
// zero for a NAV report) and its reported NAV by the vault's max; Voltr and
// the adaptor check the rest. Each policy also spends at most the daily USDC
// cap.
func bridgePolicies() map[Action]bridgeLiteral {
	pin, free := squads.Pin, squads.Any
	vault, settings, strategy, ticket := solanaKey(bridgeVault), solanaKey(bridgeSettings), solanaKey(bridgeStrategy), solanaKey(reportTicketPDA)
	usdc, custody, strategyATA := solanaKey(bridgeUSDC), solanaKey(bridgeSquadsATA), solanaKey(bridgeStrategyATA)
	limits := []squads.SpendingLimitView{{Mint: usdc, Period: 1 /* daily */, MaxPerPeriod: strategyTwoDailyAllocationCapRaw}}

	capital := func(offset uint64) []squads.DataConstraintView {
		return []squads.DataConstraintView{squads.DataU64(offset, squads.OpGreaterThan, 0), squads.DataU64(offset, squads.OpLessThanOrEqualTo, strategyTwoBridgeLegCapRaw)}
	}
	nothing := func(offset uint64) []squads.DataConstraintView {
		return []squads.DataConstraintView{squads.DataU64(offset, squads.OpEquals, 0)}
	}
	reportArgs := binary.LittleEndian.AppendUint32([]byte{1}, bridgeReportLen) // Some(ReportV1)
	reportArgs = append(reportArgs, 1)                                         // its version
	arm := func(operation byte, amount func(uint64) []squads.DataConstraintView) squads.InstructionConstraintView {
		data := append(amount(armAmountOffset),
			squads.DataU64(armReportArgsOffset+5+bridgeReportNAVOffset, squads.OpLessThanOrEqualTo, bridgeMaxNAV),
			squads.DataBytes(armReportArgsOffset, reportArgs))
		return armReportAllowed(armReport[squads.Slot]{Strategy: pin(strategy), Ticket: pin(ticket), Settings: free, Vault: free}, operation, data...)
	}
	move := func(allowed func(voltr.StrategyAllowed, []squads.Slot, ...squads.DataConstraintView) squads.InstructionConstraintView,
		adaptorInstruction []byte, amount func(uint64) []squads.DataConstraintView) squads.InstructionConstraintView {
		call := append(binary.LittleEndian.AppendUint32([]byte{1}, uint32(len(adaptorInstruction))), adaptorInstruction...) // Some(adaptor instruction)
		reportOffset := uint64(voltr.StrategyAdaptorCallOffset + len(call) + 5)
		data := append(amount(voltr.StrategyAmountOffset),
			squads.DataU64(reportOffset+bridgeReportNAVOffset, squads.OpLessThanOrEqualTo, bridgeMaxNAV),
			squads.DataBytes(voltr.StrategyAdaptorCallOffset, append(call, reportArgs...)))
		accounts := voltr.StrategyAllowed{Manager: pin(vault), Protocol: free, Vault: pin(solanaKey(bridgeVoltrVault)), Strategy: pin(strategy),
			AdaptorAddReceipt: free, StrategyInitReceipt: free, VaultAssetIdleAuth: free, VaultStrategyAuth: free, AssetMint: pin(usdc),
			LPMint: free, VaultAssetIdleATA: free, VaultStrategyAssetATA: pin(strategyATA), AssetTokenProgram: pin(solana.TokenProgramID),
			AdaptorProgram: pin(solanaKey(bridgeAdaptorProgram))}
		// The adaptor's remaining accounts: voltrStrategyInstruction's, then
		// the report ticket ticketedBridgeInstructions appends.
		return allowed(accounts, []squads.Slot{pin(settings), pin(vault), pin(custody), pin(ticket)}, data...)
	}
	twoLegs := func(armLeg, capitalLeg squads.InstructionConstraintView) bridgeLiteral {
		var legs [bridgeCapitalLegs]squads.InstructionConstraintView
		legs[bridgeArmLeg], legs[bridgeCapitalLeg] = armLeg, capitalLeg
		return bridgeLiteral{constraints: legs[:], limits: limits}
	}
	var stage [bridgeStageLegs]squads.InstructionConstraintView
	stage[bridgeStageLeg] = spl.TransferCheckedAllowed(solana.TokenProgramID, spl.TransferAllowed{Source: pin(custody), Mint: pin(usdc),
		Destination: pin(strategyATA), Authority: pin(vault)}, 6, capital(spl.TransferCheckedAmountOffset)...)

	return map[Action]bridgeLiteral{
		VoltrAllocateToSquads: twoLegs(arm(reportTicketDeposit, capital), move(voltr.DepositStrategyAllowed, adaptorDepositDiscriminator, capital)),
		ReportNAV:             twoLegs(arm(reportTicketDeposit, nothing), move(voltr.DepositStrategyAllowed, adaptorDepositDiscriminator, nothing)),
		VoltrRestoreIdle:      twoLegs(arm(reportTicketWithdraw, capital), move(voltr.WithdrawStrategyAllowed, adaptorWithdrawDiscriminator, capital)),
		StageSquadsToVoltr:    {constraints: stage[:], limits: limits},
	}
}

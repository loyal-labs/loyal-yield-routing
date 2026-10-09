package backyard

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// Unsigned simulation output is a prospective exit-cost input, never current
// custody, a signed build, or a committed position. No account overrides exist.
type phase3KaminoProjection struct {
	Slot          int64              `json:"slot"`
	MessageSHA256 string             `json:"messageSha256"`
	UnitsConsumed uint64             `json:"unitsConsumed"`
	Accounts      []ConfirmedAccount `json:"accounts"`
}

func depositProjectionAddresses(route RuntimeRoute) []string {
	addresses := []string{route.Kamino.Obligation, route.Kamino.CollateralReserve, route.CollateralCustody, route.CollateralLiquiditySupply, route.DebtCustody, budgetClockAddress}
	if selectorLane(route.Lane) || route.Lane == autoAUTOPYUSD.Lane {
		addresses = append(addresses, route.Kamino.Market)
	}
	return addresses
}

func simulateKaminoEntryProjection(ctx context.Context, c *chain.Client, r KaminoPrimeUSDCRequest, minimumSlot int64) (phase3KaminoProjection, error) {
	var projection phase3KaminoProjection
	_, leg, err := kaminoPrimeUSDCInstruction(r)
	if c == nil || err != nil || (leg != kaminoLegDeposit && leg != kaminoLegBorrow) || r.Action != OpenRouteStep || minimumSlot <= 0 {
		return projection, budgetHold("invalid_deposit_projection_request")
	}
	message, err := CompileKaminoMessage(r)
	if err != nil {
		return projection, err
	}
	if _, err = checkedUnsignedMessage(message); err != nil {
		return projection, err
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return projection, err
	}
	addresses := depositProjectionAddresses(route)
	if leg == kaminoLegBorrow {
		addresses = append(addresses, route.Kamino.DebtReserve, route.DebtLiquiditySupply, route.DebtFeeReceiver)
	} else if len(r.ObligationReserves) == 2 {
		addresses = append(addresses, route.Kamino.DebtReserve, route.DebtLiquiditySupply)
	}
	return simulatePhase3EntryProjection(ctx, c, message, addresses, minimumSlot)
}

func simulatePhase3EntryProjection(ctx context.Context, c *chain.Client, message []byte, addresses []string, minimumSlot int64) (phase3KaminoProjection, error) {
	var projection phase3KaminoProjection
	if c == nil || minimumSlot <= 0 || len(addresses) == 0 {
		return projection, budgetHold("invalid_deposit_projection_request")
	}
	_, err := checkedUnsignedMessage(message)
	if err != nil {
		return projection, err
	}
	keys, err := publicKeys(addresses)
	if err != nil {
		return projection, err
	}
	wire := append([]byte{1}, make([]byte, 64)...)
	wire = append(wire, message...)
	minimum := uint64(minimumSlot)
	simulated, err := c.Simulate(ctx, wire, rpc.SimulateTransactionOpts{Commitment: rpc.CommitmentConfirmed, MinContextSlot: &minimum, Accounts: &rpc.SimulateTransactionAccountsOpts{Encoding: solana.EncodingBase64, Addresses: keys}})
	var failure *chain.SimulationError
	if errors.As(err, &failure) {
		return projection, budgetHold("deposit_projection_failed")
	}
	if err != nil {
		return projection, budgetHold("deposit_projection_unavailable")
	}
	slot := int64(simulated.Slot)
	if slot-minimumSlot > observationLagSlots() || simulated.Units == 0 {
		return projection, budgetHold("deposit_projection_failed")
	}
	projection.Slot, projection.MessageSHA256, projection.UnitsConsumed = slot, sha256Bytes(message), simulated.Units
	for i, a := range simulated.Accounts {
		if a == nil || a.Executable {
			return projection, budgetHold("deposit_projection_incomplete")
		}
		projection.Accounts = append(projection.Accounts, ConfirmedAccount{Address: addresses[i], Owner: a.Owner.String(), Lamports: a.Lamports, Data: a.Data})
	}
	return projection, nil
}

// Validate against the original bounded transfer, independently decode the
// simulated position and include any undeployed/rounded collateral residue.
func validateDepositProjection(r KaminoPrimeUSDCRequest, effects ExpectedEffects, p phase3KaminoProjection) (uint64, uint64, uint64, error) {
	message, err := CompileKaminoMessage(r)
	if err != nil || p.MessageSHA256 != sha256Bytes(message) || effects.Deposit == nil {
		return 0, 0, 0, budgetHold("deposit_projection_identity_mismatch")
	}
	if _, err = MeasureExecutableDebit(r, effects); err != nil {
		return 0, 0, 0, err
	}
	route, err := runtimeRoute(r.RouteLane)
	if err != nil {
		return 0, 0, 0, err
	}
	var moved uint64
	for i, e := range effects.Accounts {
		a := accountAt(p.Accounts, e.Address)
		mint, _ := decodeBase58PublicKey(e.Mint)
		authority, _ := decodeBase58PublicKey(e.Authority)
		custody, err := DecodeTokenCustody(a.Owner, a.Data, mint, authority)
		if err != nil || a.Executable || a.Lamports == 0 || a.Owner != e.Owner {
			return 0, 0, 0, budgetHold("deposit_projection_custody_mismatch")
		}
		if i == 0 {
			if custody.Raw > e.BeforeRaw {
				return 0, 0, 0, budgetHold("deposit_projection_debit_mismatch")
			}
			moved = e.BeforeRaw - custody.Raw
			if moved < effects.Deposit.MinimumDebitRaw || moved > effects.Deposit.MaximumDebitRaw {
				return 0, 0, 0, budgetHold("deposit_projection_debit_mismatch")
			}
		} else if custody.Raw < e.BeforeRaw || custody.Raw-e.BeforeRaw != moved {
			return 0, 0, 0, budgetHold("deposit_projection_conservation_mismatch")
		}
	}
	obligation, err := decodeKaminoObligation(accountAt(p.Accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil || obligation.debtRaw != 0 || obligation.collateralDepositedRaw == 0 {
		return 0, 0, 0, budgetHold("deposit_projection_position_mismatch")
	}
	reserve, err := decodeKaminoReserve(accountAt(p.Accounts, route.Kamino.CollateralReserve), route.Kamino.CollateralMint, route.Kamino)
	if err != nil {
		return 0, 0, 0, err
	}
	clock := accountAt(p.Accounts, budgetClockAddress)
	if p.Slot <= 0 || len(clock.Data) != 40 || clock.Owner != "Sysvar1111111111111111111111111111111111111" || clock.Executable {
		return 0, 0, 0, budgetHold("deposit_projection_clock_mismatch")
	}
	clockSlot := binary.LittleEndian.Uint64(clock.Data[:8])
	if clockSlot < uint64(p.Slot) || clockSlot > uint64(p.Slot)+1 || reserve.refreshedSlot != int64(clockSlot) || obligation.refreshedSlot != int64(clockSlot) {
		return 0, 0, 0, budgetHold("deposit_projection_refresh_mismatch")
	}
	debt := accountAt(p.Accounts, route.DebtCustody)
	mint, _ := decodeBase58PublicKey(route.Kamino.DebtMint)
	authority, _ := decodeBase58PublicKey(bridgeVault)
	cash, err := DecodeTokenCustody(debt.Owner, debt.Data, mint, authority)
	if err != nil || debt.Lamports == 0 || debt.Executable || cash.Raw != 0 {
		return 0, 0, 0, budgetHold("deposit_projection_debt_custody_changed")
	}
	liquidity, err := reserve.redeemLiquidityRaw(obligation.collateralDepositedRaw)
	residue := effects.Accounts[0].BeforeRaw - moved
	if err != nil || liquidity == 0 || liquidity > math.MaxInt64 || residue > math.MaxInt64-liquidity {
		return 0, 0, 0, budgetHold("deposit_projection_return_overflow")
	}
	return obligation.collateralDepositedRaw, liquidity, residue, nil
}

// collateralRaw is the obligation's admitted collateral: zero for an initial
// deposit, the unchanged debt-free position for a plan B3 top-up deposit.
func validateInitialDepositPrestate(ctx context.Context, rpc *chain.Client, route RuntimeRoute, minimumSlot int64, collateralRaw uint64) (int64, error) {
	slot, accounts, err := confirmedAccounts(ctx, rpc, []string{route.Kamino.Obligation, route.DebtCustody}, minimumSlot)
	if err != nil {
		return 0, err
	}
	position, err := decodeKaminoObligation(accountAt(accounts, route.Kamino.Obligation), route.Kamino)
	if err != nil || position.hasPosition != (collateralRaw > 0) || position.debtRaw != 0 || position.collateralDepositedRaw != collateralRaw {
		return 0, budgetHold("deposit_prestate_changed")
	}
	debt := accountAt(accounts, route.DebtCustody)
	mint, _ := decodeBase58PublicKey(route.Kamino.DebtMint)
	authority, _ := decodeBase58PublicKey(bridgeVault)
	cash, err := DecodeTokenCustody(debt.Owner, debt.Data, mint, authority)
	if err != nil || cash.Raw != 0 || debt.Lamports == 0 || debt.Executable {
		return 0, budgetHold("deposit_prestate_changed")
	}
	return slot, nil
}

func observePhase3DepositAdmission(ctx context.Context, rpc *chain.Client, client *jupiterClient, manifest RouteManifest, observation Observation, decision Decision, evidence KaminoExecutionEvidence) (phase3BridgeAdmission, error) {
	if observation.Snapshot.PositionDebtRaw > 0 {
		return observePhase3RedepositAdmission(ctx, rpc, client, manifest, observation, decision, evidence)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	s, r := observation.Snapshot, evidence.Request
	// Plan B3: a top-up deposit joins a funded debt-free position; every
	// other initial deposit needs a flat obligation.
	topup := decision.Reason == topupDepositReason && s.HasPosition && s.PositionCollateralRaw > 0 && s.PositionCollateralValueRaw > 0 &&
		s.PositionDebtRaw == 0 && s.PositionDebtValueRaw == 0 && s.WithdrawalDemandRaw == 0 && !s.Unwind
	flat := decision.Reason != topupDepositReason && !s.HasPosition && s.PositionCollateralRaw == 0 && s.PositionCollateralValueRaw == 0 &&
		s.PositionDebtRaw == 0 && s.PositionDebtValueRaw == 0
	if rpc == nil || client == nil || !s.Fresh || s.Slot <= 0 || s.RouteKind != RouteKind || s.ManualReason != "" || s.Nonterminal != "" || s.HasAmbiguousSubmission || s.CutoverDrain ||
		s.RouteLane != s.StrategyKey || s.RouteLane != decision.StrategyKey || s.RouteLane != r.RouteLane || phase3BudgetFamilyForLane(s.RouteLane) == "" || (!topup && !flat) ||
		s.CollateralIdleRaw <= 0 || s.PrimeIdleRaw != s.CollateralIdleRaw || s.DebtIdleRaw != 0 || s.SquadsIdleRaw < 0 || s.VoltrIdleRaw < 0 || s.VoltrStrategyIdleRaw != 0 || decision.Action != OpenRouteStep || r.Action != decision.Action || decision.AmountRaw <= 0 || r.AmountRaw != uint64(decision.AmountRaw) || r.AmountRaw > uint64(s.CollateralIdleRaw) || evidence.ExpectedEffects.Deposit == nil {
		return phase3BridgeAdmission{}, budgetHold("complete_initial_deposit_return_unavailable")
	}
	route, err := runtimeRoute(s.RouteLane)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	current, err := observePhase3KnownBuildCost(ctx, rpc, r, evidence.ExpectedEffects)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	slot, err := validateInitialDepositPrestate(ctx, rpc, route, s.Slot, uint64(s.PositionCollateralRaw))
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	if evidence.ExpectedEffects.Accounts[0].BeforeRaw != uint64(s.CollateralIdleRaw) {
		return phase3BridgeAdmission{}, budgetHold("deposit_prestate_changed")
	}
	projection, err := simulateKaminoEntryProjection(ctx, rpc, r, max(slot, current.ObservationSlot))
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	receipts, liquidity, residue, err := validateDepositProjection(r, evidence.ExpectedEffects, projection)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	if receipts <= uint64(s.PositionCollateralRaw) {
		return phase3BridgeAdmission{}, budgetHold("deposit_projection_position_mismatch")
	}
	withdrawal, err := manifest.kaminoPacketForRoute(DeleverRouteStep, kaminoLegWithdraw, receipts, LatestBlockhash{Blockhash: r.RecentBlockhash, LastValidBlockHeight: r.LastValidBlockHeight}, r.RouteLane)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	withdrawal.ObligationReserves = []string{route.Kamino.CollateralReserve}
	_, policies, err := confirmedAccounts(ctx, rpc, []string{withdrawal.Policy}, projection.Slot)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	policy := accountAt(policies, withdrawal.Policy)
	if policy.Owner != squads.ProgramID.String() || policy.Executable || policy.Lamports == 0 || sha256Bytes(policy.Data) != withdrawal.PolicyAccountDataSHA256 {
		return phase3BridgeAdmission{}, budgetHold("deposit_withdrawal_policy_drift")
	}
	source, destination := kaminoLegCustodiesForRoute(kaminoLegWithdraw, route)
	withdrawalEffects, err := exactKaminoTokenEffects(projection.Accounts, source, destination, liquidity)
	if err != nil {
		return phase3BridgeAdmission{}, err
	}
	post := observation
	post.Snapshot.HasPosition = true
	post.Snapshot.PositionCollateralRaw = int64(receipts)
	post.Snapshot.CollateralIdleRaw, post.Snapshot.PrimeIdleRaw = int64(residue), int64(residue)
	tail, err := observePhase3WithdrawalAdmission(ctx, rpc, client, manifest, post, Decision{Action: DeleverRouteStep, StrategyKey: s.RouteLane, Reason: "withdrawal_withdraw_collateral"}, KaminoExecutionEvidence{withdrawal, withdrawalEffects})
	if err != nil {
		return tail, err
	}
	if len(tail.Exit) == 0 || tail.Exit[0].Action != ReportNAV {
		return tail, budgetHold("deposit_return_nav_unavailable")
	}
	encoded, err := jsonMarshalExpectedEffects(evidence.ExpectedEffects)
	if err != nil {
		return tail, err
	}
	input, err := encodePhase3BuildInput(r, encoded)
	if err != nil {
		return tail, err
	}
	plan := tail
	plan.Snapshot, plan.Decision, plan.Input, plan.CurrentCost = s, decision, input, current
	plan.DepositProjection, plan.PayoffWithdrawal = &projection, tail.Input
	plan.Exit = append([]phase3BridgeExitCost{{Action: ReportNAV, Cost: tail.Exit[0].Cost, Template: tail.Exit[0].Template}, {Action: DeleverRouteStep, Amount: receipts, Cost: tail.CurrentCost, Template: tail.Input}}, tail.Exit...)
	plan.ExitAfterMicros = 0
	for _, step := range plan.Exit {
		plan.ExitAfterMicros, err = budgetSum(plan.ExitAfterMicros, step.Cost.TotalMicros)
		if err != nil {
			return plan, err
		}
	}
	plan.ValidThroughSlot = min(current.ValidThroughSlot, tail.ValidThroughSlot)
	if projection.Slot > plan.ValidThroughSlot {
		return plan, budgetHold("stale_deposit_projection")
	}
	return plan, nil
}

func (d *Database) admitPhase3Deposit(ctx context.Context, rpc *chain.Client, client *jupiterClient, manifest RouteManifest, id string, o Observation, decision Decision, e KaminoExecutionEvidence) error {
	plan, err := observePhase3DepositAdmission(ctx, rpc, client, manifest, o, decision, e)
	if err != nil {
		return err
	}
	return d.persistPhase3ExitAdmission(ctx, rpc, id, o, decision, plan)
}

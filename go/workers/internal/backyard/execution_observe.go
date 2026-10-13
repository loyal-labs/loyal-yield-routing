package backyard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

const adaptorConfigLength = 472

var adaptorConfigDiscriminator = []byte{46, 154, 12, 115, 203, 165, 199, 235}

type observedAdaptorConfig struct{}

func decodeObservedAdaptorConfig(account ConfirmedAccount) (observedAdaptorConfig, error) {
	if account.Address != bridgeStrategy || account.Owner != bridgeAdaptorProgram || account.Executable ||
		account.Lamports == 0 || len(account.Data) != adaptorConfigLength ||
		!bytes.Equal(account.Data[:8], adaptorConfigDiscriminator) || account.Data[8] != 2 ||
		account.Data[9] != 0 || !allZero(account.Data[10:16]) {
		return observedAdaptorConfig{}, fmt.Errorf("adaptor config envelope or version drifted")
	}
	bindings := []string{
		voltr.ProgramID.String(), bridgeVoltrVault, bridgeStrategy, bridgeStrategyAuth,
		squads.ProgramID.String(), bridgeSettings, bridgeSettingsSigner, bridgeVault,
		bridgeUSDC, bridgeTokenProgram, bridgeSquadsATA, "",
	}
	for index, binding := range bindings {
		field := account.Data[16+index*32 : 48+index*32]
		if binding == "" {
			if !allZero(field) {
				return observedAdaptorConfig{}, fmt.Errorf("adaptor config reserved key is nonzero")
			}
			continue
		}
		if !sameKey(field, binding) {
			return observedAdaptorConfig{}, fmt.Errorf("adaptor config binding %d drifted", index)
		}
	}
	if binary.LittleEndian.Uint64(account.Data[400:408]) != bridgeMaxNAV ||
		binary.LittleEndian.Uint64(account.Data[408:416]) != 32 || !allZero(account.Data[416:472]) {
		return observedAdaptorConfig{}, fmt.Errorf("adaptor report bounds drifted")
	}
	return observedAdaptorConfig{}, nil
}

// Reuse the enriched, receipt-fenced bank owned by this Tick. The current slot
// is the view's; only the blockhash needs a new RPC read. Admission, signing
// simulation, durable authority binding and final send revalidation still run
// unchanged.
func prepareBridgeFromTickObservation(ctx context.Context, rpc *chain.Client, view *View, manifest RouteManifest, decision Decision, observation Observation) (Observation, BridgeExecutionEvidence, error) {
	batch := observation.routeBatch
	if rpc == nil || batch == nil || batch.Slot != observation.Snapshot.Slot || batch.ObservationID != observation.Snapshot.ObservationID || batch.ManifestSHA256 != manifest.SHA256 || observation.Validate() != nil {
		return Observation{}, BridgeExecutionEvidence{}, confirmedObservationUnavailable(fmt.Errorf("tick-local bridge observation is missing or stale"))
	}
	slot, err := view.slot(ctx)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	if slot-batch.Slot > min(observationLagSlots(), adaptorMaxReportAgeSlots) {
		return Observation{}, BridgeExecutionEvidence{}, confirmedObservationUnavailable(fmt.Errorf("tick-local bridge observation exceeded slot freshness"))
	}
	// The policies this build executes through: one read, at its slot.
	if observation.policies, err = observeInstalledPolicies(ctx, rpc, batch.Slot); err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	return prepareBridgeFromObservedAccounts(ctx, rpc, manifest, decision, observation, batch.Accounts)
}

func prepareBridgeFromObservedAccounts(ctx context.Context, rpc *chain.Client, manifest RouteManifest, decision Decision, observation Observation, accounts []ConfirmedAccount) (Observation, BridgeExecutionEvidence, error) {
	policy, err := observation.policies.account(policyKey{action: decision.Action})
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	route, err := runtimeRoute(decision.StrategyKey)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	ticketRequired := decision.Action != StageSquadsToVoltr
	if earnHeldLane(route.Lane) {
		// Reserve the entire bridge exit, including a report after staging:
		// the existing ticket must be present in this same observation;
		// admission cannot authorize setup.
		ticketRequired = true
	}
	var ticket observedReportTicket
	if ticketRequired {
		ticket, err = decodeObservedReportTicket(accountAt(accounts, reportTicketPDA))
		if err != nil {
			return Observation{}, BridgeExecutionEvidence{}, err
		}
		if ticket.Armed {
			return Observation{}, BridgeExecutionEvidence{}, fmt.Errorf("report ticket is already armed")
		}
	}
	custodies, err := decodeRouteNAVCustodiesForRoute(accounts, route)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	if uint64(observation.Snapshot.VoltrIdleRaw) != custodies.VoltrIdleRaw ||
		uint64(observation.Snapshot.VoltrStrategyIdleRaw) != custodies.StrategyUSDCraw ||
		uint64(observation.Snapshot.SquadsIdleRaw) != custodies.SquadsUSDCraw {
		return Observation{}, BridgeExecutionEvidence{}, fmt.Errorf("bridge custody changed inside confirmed construction snapshot")
	}
	effects, strategyAfter, squadsAfter, err := bridgeExpectedEffects(decision, custodies.VoltrIdleRaw, custodies.StrategyUSDCraw, custodies.SquadsUSDCraw)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	postCustodies := custodies
	postCustodies.StrategyUSDCraw = strategyAfter
	postCustodies.SquadsUSDCraw = squadsAfter
	switch decision.Action {
	case VoltrAllocateToSquads:
		postCustodies.VoltrIdleRaw -= uint64(decision.AmountRaw)
	case VoltrRestoreIdle:
		postCustodies.VoltrIdleRaw += uint64(decision.AmountRaw)
	}
	// The report values the whole book: every other lane the observation
	// valued beside the working lane's poststate.
	valued, err := navLanes(accounts, observation.Snapshot.heldLanes())
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	routes, err := navRoutes(route, valued)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	navAccounts, err := selectRouteNAVAccountsForRoutes(accounts, routes)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	nav, err := ComputeRouteNAVForRoutes(observation.Snapshot.Slot, navAccounts, manifest, &postCustodies, routes)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	if ticketRequired && nav.Report.Sequence <= ticket.LastConsumedSequence {
		return Observation{}, BridgeExecutionEvidence{}, fmt.Errorf("report ticket sequence is not fresh")
	}
	effects.Kind = "bridge"
	if decision.Action != StageSquadsToVoltr {
		effects.ReturnData = expectedAdaptorReturnData(nav.Report.NAVAfterRaw)
	}
	blockhash, err := latestBlockhash(ctx, rpc)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	return observation, BridgeExecutionEvidence{
		Request: BridgeBuildRequest{
			Action: decision.Action, AmountRaw: uint64(decision.AmountRaw),
			Report: nav.Report, Policy: policy,
			AdaptorConfig: bridgeStrategy, Settings: bridgeSettings,
			RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight,
		},
		ExpectedEffects: effects,
	}, nil
}

func expectedAdaptorReturnData(navAfterRaw uint64) *ExpectedReturnData {
	data := make([]byte, 8)
	binary.LittleEndian.PutUint64(data, navAfterRaw)
	return &ExpectedReturnData{ProgramID: bridgeAdaptorProgram, DataBase64: base64.StdEncoding.EncodeToString(data)}
}

func bridgeExpectedEffects(decision Decision, idle, strategy, squads uint64) (ExpectedEffects, uint64, uint64, error) {
	if decision.AmountRaw < 0 {
		return ExpectedEffects{}, 0, 0, fmt.Errorf("negative bridge amount")
	}
	amount := uint64(decision.AmountRaw)
	idleAfter, strategyAfter, squadsAfter := idle, strategy, squads
	switch decision.Action {
	case VoltrAllocateToSquads:
		// Voltr first moves idle USDC into its strategy ATA; the adaptor then
		// forwards that same amount to Squads. The coherent poststate therefore
		// consumes vault idle and increases Squads while strategy custody returns
		// to its starting balance.
		if idle < amount || squads > math.MaxUint64-amount {
			return ExpectedEffects{}, 0, 0, fmt.Errorf("allocation effects overflow or underflow")
		}
		idleAfter, squadsAfter = idle-amount, squads+amount
	case StageSquadsToVoltr:
		if squads < amount || strategy > math.MaxUint64-amount {
			return ExpectedEffects{}, 0, 0, fmt.Errorf("staging effects overflow or underflow")
		}
		squadsAfter, strategyAfter = squads-amount, strategy+amount
	case VoltrRestoreIdle:
		if strategy < amount || idle > math.MaxUint64-amount {
			return ExpectedEffects{}, 0, 0, fmt.Errorf("restore effects overflow or underflow")
		}
		strategyAfter, idleAfter = strategy-amount, idle+amount
	case ReportNAV:
		if amount != 0 {
			return ExpectedEffects{}, 0, 0, fmt.Errorf("NAV report cannot move capital")
		}
	default:
		return ExpectedEffects{}, 0, 0, fmt.Errorf("unsupported bridge action")
	}
	accounts := []ExpectedAccountEffect{
		{Address: bridgeIdleATA, Owner: bridgeTokenProgram, Mint: bridgeUSDC, Authority: bridgeIdleAuthority, BeforeRaw: idle, AfterRaw: idleAfter},
		{Address: bridgeStrategyATA, Owner: bridgeTokenProgram, Mint: bridgeUSDC, Authority: bridgeStrategyAuth, BeforeRaw: strategy, AfterRaw: strategyAfter},
		{Address: bridgeSquadsATA, Owner: bridgeTokenProgram, Mint: bridgeUSDC, Authority: bridgeVault, BeforeRaw: squads, AfterRaw: squadsAfter},
	}
	// Staging is a direct Squads-authorized SPL transfer. It does not invoke the
	// adaptor and its receipt therefore contains only the strategy and Squads
	// token accounts, not the unrelated unchanged vault-idle account.
	if decision.Action == StageSquadsToVoltr {
		accounts = accounts[1:]
	}
	return ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true, Accounts: accounts}, strategyAfter, squadsAfter, nil
}

// prepareKaminoFromTickObservation builds from the tick's own view batch, as
// the bridge does; it reads only the policies it executes through, at its slot.
func prepareKaminoFromTickObservation(ctx context.Context, rpc *chain.Client, view *View, manifest RouteManifest, decision Decision, observation Observation) (Observation, KaminoExecutionEvidence, error) {
	if rpc == nil || observation.routeBatch == nil || decision.Action != OpenRouteStep {
		return Observation{}, KaminoExecutionEvidence{}, fmt.Errorf("invalid Kamino evidence request")
	}
	prepareStart, accounts := time.Now(), observation.routeBatch.Accounts
	var err error
	if observation.policies, err = observeInstalledPolicies(ctx, rpc, observation.Snapshot.Slot); err != nil {
		return Observation{}, KaminoExecutionEvidence{}, err
	}
	route, err := runtimeRoute(decision.StrategyKey)
	if err != nil {
		return Observation{}, KaminoExecutionEvidence{}, err
	}
	position, err := observeKaminoFromFixedAccounts(observation.Snapshot.Slot, accounts, route.Kamino)
	if err != nil {
		return Observation{}, KaminoExecutionEvidence{}, err
	}
	leg, wireAmount, _, err := selectKaminoLeg(decision, position)
	if err != nil {
		return Observation{}, KaminoExecutionEvidence{}, err
	}
	blockhash, err := latestBlockhash(ctx, rpc)
	if err != nil {
		return Observation{}, KaminoExecutionEvidence{}, err
	}
	request, err := manifest.kaminoPacketForRoute(observation.policies, decision.Action, leg, wireAmount, blockhash, decision.StrategyKey)
	if err != nil {
		return Observation{}, KaminoExecutionEvidence{}, err
	}
	request.ObligationReserves = []string{}
	if position.CollateralDepositedRaw > 0 {
		request.ObligationReserves = append(request.ObligationReserves, route.Kamino.CollateralReserve)
	}
	if position.DebtRaw > 0 {
		if position.CollateralDepositedRaw == 0 {
			return Observation{}, KaminoExecutionEvidence{}, fmt.Errorf("Kamino obligation has debt without the pinned collateral reserve")
		}
		request.ObligationReserves = append(request.ObligationReserves, route.Kamino.DebtReserve)
	}
	effects, err := boundedKaminoDepositEffects(accounts, route, observation.Snapshot.Slot, wireAmount)
	if err != nil {
		return Observation{}, KaminoExecutionEvidence{}, err
	}
	observation.Snapshot.HasPosition = position.HasPosition
	logStage("prepare_kamino_evidence", prepareStart)
	return observation, KaminoExecutionEvidence{Request: request, ExpectedEffects: effects}, nil
}

func selectKaminoLeg(decision Decision, position KaminoPosition) (kaminoPrimeUSDCLeg, uint64, uint64, error) {
	switch decision.Action {
	case OpenRouteStep:
		if decision.AmountRaw <= 0 {
			return 0, 0, 0, fmt.Errorf("OPEN requires a positive exact amount")
		}
		if position.CollateralDepositedRaw == 0 && position.DebtRaw == 0 {
			return kaminoLegDeposit, uint64(decision.AmountRaw), uint64(decision.AmountRaw), nil
		}
		if position.CollateralDepositedRaw > 0 && position.DebtRaw == 0 && decision.Reason == topupDepositReason {
			return kaminoLegDeposit, uint64(decision.AmountRaw), uint64(decision.AmountRaw), nil
		}
		if position.CollateralDepositedRaw > 0 && position.DebtRaw > 0 && decision.Reason == topupDepositReason && earnActiveLane(decision.StrategyKey) {
			return kaminoLegDeposit, uint64(decision.AmountRaw), uint64(decision.AmountRaw), nil
		}
	}
	return 0, 0, 0, fmt.Errorf("Kamino position is not in a supported next-leg state")
}

func withdrawExcessAtLTV(position KaminoPosition, ltvBPS uint64) (uint64, uint64, error) {
	if ltvBPS == 0 || ltvBPS >= 10_000 {
		return 0, 0, fmt.Errorf("invalid repayment release LTV")
	}
	if position.CollateralDepositedRaw == 0 || position.RedeemablePrimeRaw == 0 || position.DebtRaw == 0 {
		return 0, 0, fmt.Errorf("position has no withdrawable repayment collateral")
	}
	excessPrime, err := withdrawableUnderlyingAtLTV(releaseValuesForPosition(position), ltvBPS)
	if err != nil {
		return 0, 0, err
	}
	receipt := new(big.Int).Mul(new(big.Int).SetUint64(excessPrime), new(big.Int).SetUint64(position.CollateralDepositedRaw))
	receipt.Quo(receipt, new(big.Int).SetUint64(position.RedeemablePrimeRaw))
	if !receipt.IsUint64() || receipt.Sign() <= 0 {
		return 0, 0, fmt.Errorf("withdrawable collateral rounds to zero")
	}
	prime := new(big.Int).Mul(receipt, new(big.Int).SetUint64(position.RedeemablePrimeRaw))
	prime.Quo(prime, new(big.Int).SetUint64(position.CollateralDepositedRaw))
	if !prime.IsUint64() || prime.Sign() <= 0 || prime.Uint64() > excessPrime {
		return 0, 0, fmt.Errorf("withdrawable PRIME amount is invalid")
	}
	return receipt.Uint64(), prime.Uint64(), nil
}

// Work in underlying token units before any receipt conversion. A forecast
// can use bounded scalar holdings without inventing an obligation or receipts.
func withdrawableUnderlyingAtLTV(values kaminoReleaseValues, ltvBPS uint64) (uint64, error) {
	if ltvBPS == 0 || ltvBPS >= 10_000 {
		return 0, fmt.Errorf("invalid repayment release LTV")
	}
	if values.CollateralRaw == 0 || values.DebtRaw == 0 {
		return 0, fmt.Errorf("position has no withdrawable repayment collateral")
	}
	debtValue, err := valueBetweenTokenRaw(values.DebtRaw, values.DebtDecimals, values.DebtDecimals, values.DebtPriceSF, values.DebtPriceSF, true)
	if err != nil {
		return 0, err
	}
	requiredDebtValue := new(big.Int).Mul(new(big.Int).SetUint64(debtValue), big.NewInt(10_000))
	requiredDebtValue.Add(requiredDebtValue, new(big.Int).SetUint64(ltvBPS-1))
	requiredDebtValue.Quo(requiredDebtValue, new(big.Int).SetUint64(ltvBPS))
	if !requiredDebtValue.IsUint64() {
		return 0, fmt.Errorf("required unwind collateral exceeds u64")
	}
	requiredCollateral, err := valueBetweenTokenRaw(requiredDebtValue.Uint64(), values.DebtDecimals, values.CollateralDecimals, values.DebtPriceSF, values.CollateralPriceSF, true)
	if err != nil {
		return 0, err
	}
	if requiredCollateral >= values.CollateralRaw {
		return 0, fmt.Errorf("no collateral excess is safely withdrawable at unwind LTV")
	}
	return values.CollateralRaw - requiredCollateral, nil
}

type kaminoCustodyBoundary struct {
	Address, Mint, Authority string
}

func kaminoLegCustodiesForRoute(leg kaminoPrimeUSDCLeg, route RuntimeRoute) (kaminoCustodyBoundary, kaminoCustodyBoundary) {
	collateralVault := kaminoCustodyBoundary{route.CollateralCustody, route.Kamino.CollateralMint, route.Kamino.Vault}
	collateralReserve := kaminoCustodyBoundary{route.CollateralLiquiditySupply, route.Kamino.CollateralMint, route.Kamino.MarketAuthority}
	stableVault := kaminoCustodyBoundary{route.DebtCustody, route.Kamino.DebtMint, route.Kamino.Vault}
	stableReserve := kaminoCustodyBoundary{route.DebtLiquiditySupply, route.Kamino.DebtMint, route.Kamino.MarketAuthority}
	switch leg {
	case kaminoLegDeposit:
		return collateralVault, collateralReserve
	case kaminoLegBorrow:
		return stableReserve, stableVault
	case kaminoLegRepay:
		return stableVault, stableReserve
	case kaminoLegWithdraw:
		return collateralReserve, collateralVault
	default:
		return kaminoCustodyBoundary{}, kaminoCustodyBoundary{}
	}
}

func exactKaminoTokenEffects(accounts []ConfirmedAccount, source, destination kaminoCustodyBoundary, amount uint64) (ExpectedEffects, error) {
	if amount == 0 {
		return ExpectedEffects{}, fmt.Errorf("Kamino effect amount is zero")
	}
	decode := func(boundary kaminoCustodyBoundary) (uint64, error) {
		account := accountAt(accounts, boundary.Address)
		mint, err := decodeBase58PublicKey(boundary.Mint)
		if err != nil {
			return 0, err
		}
		authority, err := decodeBase58PublicKey(boundary.Authority)
		if err != nil {
			return 0, err
		}
		custody, err := DecodeTokenCustody(account.Owner, account.Data, mint, authority)
		if err != nil || account.Executable || account.Lamports == 0 {
			return 0, fmt.Errorf("decode exact Kamino custody %s: %w", boundary.Address, err)
		}
		return custody.Raw, nil
	}
	sourceRaw, err := decode(source)
	if err != nil {
		return ExpectedEffects{}, err
	}
	destinationRaw, err := decode(destination)
	if err != nil {
		return ExpectedEffects{}, err
	}
	if sourceRaw < amount || destinationRaw > math.MaxUint64-amount {
		return ExpectedEffects{}, fmt.Errorf("Kamino custody effect overflows or underflows")
	}
	// Both accounts were decoded under their actual owner above. Preserve it:
	// PYUSD uses Token-2022, even when the bridge cash uses classic SPL Token.
	program := accountAt(accounts, source.Address).Owner
	if program != accountAt(accounts, destination.Address).Owner || source.Mint != destination.Mint {
		return ExpectedEffects{}, fmt.Errorf("Kamino transfer custody token programs or mints differ")
	}
	return ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true, Accounts: []ExpectedAccountEffect{
		{Address: source.Address, Owner: program, Mint: source.Mint, Authority: source.Authority, BeforeRaw: sourceRaw, AfterRaw: sourceRaw - amount},
		{Address: destination.Address, Owner: program, Mint: destination.Mint, Authority: destination.Authority, BeforeRaw: destinationRaw, AfterRaw: destinationRaw + amount},
	}}, nil
}

func boundedKaminoRepaymentEffects(accounts []ConfirmedAccount, source, destination kaminoCustodyBoundary, minimum, maximum uint64) (ExpectedEffects, error) {
	effects, err := exactKaminoTokenEffects(accounts, source, destination, maximum)
	if err != nil {
		return ExpectedEffects{}, err
	}
	effects.Kind = "kamino-repay"
	effects.Repayment = &ExpectedRepayment{MinimumDebitRaw: minimum, MaximumDebitRaw: maximum}
	if err := validateRepaymentEffects(effects); err != nil {
		return ExpectedEffects{}, err
	}
	return effects, nil
}

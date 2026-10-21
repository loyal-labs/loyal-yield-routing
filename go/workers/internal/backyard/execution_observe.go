package backyard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"slices"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/squads"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/voltr"
)

const adaptorConfigLength = 472

var adaptorConfigDiscriminator = []byte{46, 154, 12, 115, 203, 165, 199, 235}

type observedAdaptorConfig struct{}

func decodeObservedAdaptorConfig(v *Vault, account ConfirmedAccount) (observedAdaptorConfig, error) {
	if account.Address != v.Strategy || account.Owner != v.Adaptor || account.Executable ||
		account.Lamports == 0 || len(account.Data) != adaptorConfigLength ||
		!bytes.Equal(account.Data[:8], adaptorConfigDiscriminator) || account.Data[8] != 2 ||
		account.Data[9] != 0 || !allZero(account.Data[10:16]) {
		return observedAdaptorConfig{}, fmt.Errorf("adaptor config envelope or version drifted")
	}
	bindings := []string{
		voltr.ProgramID.String(), v.VoltrVault, v.Strategy, v.strategyAuth,
		squads.ProgramID.String(), backyardSettings, backyardSettingsSigner, v.squads,
		fleet.USDCMint, bridgeTokenProgram, v.cash, "",
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
		binary.LittleEndian.Uint64(account.Data[408:416]) != uint64(adaptorMaxReportAgeSlots) || !allZero(account.Data[416:472]) {
		return observedAdaptorConfig{}, fmt.Errorf("adaptor report bounds drifted")
	}
	return observedAdaptorConfig{}, nil
}

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
	if observation.policies, err = observeInstalledPolicies(ctx, rpc, manifest.vault, batch.Slot); err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	return prepareBridgeFromObservedAccounts(ctx, rpc, manifest, decision, observation, batch.Accounts)
}

func prepareBridgeFromObservedAccounts(ctx context.Context, rpc *chain.Client, manifest RouteManifest, decision Decision, observation Observation, accounts []ConfirmedAccount) (Observation, BridgeExecutionEvidence, error) {
	action, err := bridgeActionOf(decision.Action)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	policy, err := observation.policies.account(bridgePolicyKey(manifest.vault, decision.Action))
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	v := manifest.vault
	route, err := v.route(decision.StrategyKey)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	ticketRequired := !v.trustful() && (decision.Action != StageSquadsToVoltr || earnHeldLane(route.Lane))
	var ticket observedReportTicket
	if ticketRequired {
		ticket, err = decodeObservedReportTicket(v, accountAt(accounts, reportTicketPDA))
		if err != nil {
			return Observation{}, BridgeExecutionEvidence{}, err
		}
		if ticket.Armed {
			return Observation{}, BridgeExecutionEvidence{}, fmt.Errorf("report ticket is already armed")
		}
	}
	custodies, err := decodeRouteNAVCustodiesForRoute(v, accounts, route)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	if uint64(observation.Snapshot.VoltrIdleRaw) != custodies.VoltrIdleRaw ||
		uint64(observation.Snapshot.VoltrStrategyIdleRaw) != custodies.StrategyUSDCraw ||
		uint64(observation.Snapshot.SquadsIdleRaw) != custodies.SquadsUSDCraw {
		return Observation{}, BridgeExecutionEvidence{}, fmt.Errorf("bridge custody changed inside confirmed construction snapshot")
	}
	effects, idleAfter, strategyAfter, squadsAfter, err := bridgeExpectedEffects(v, decision, custodies.VoltrIdleRaw, custodies.StrategyUSDCraw, custodies.SquadsUSDCraw)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	postCustodies := custodies
	postCustodies.VoltrIdleRaw = idleAfter
	postCustodies.StrategyUSDCraw = strategyAfter
	postCustodies.SquadsUSDCraw = squadsAfter
	valued, err := navLanes(v, accounts, observation.Snapshot.heldLanes())
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	routes, err := navRoutes(v, route, valued)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	navAccounts, err := selectRouteNAVAccountsForRoutes(v, accounts, routes)
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
	// FSj27 returns the NAV each report-bearing leg arms; Trustful's
	// deposit_strategy returns its end value. A Trustful withdrawal returns
	// nothing.
	if decision.Action != StageSquadsToVoltr {
		effects.ReturnData = expectedAdaptorReturnData(v, nav.Report.NAVAfterRaw)
	}
	blockhash, err := latestBlockhash(ctx, rpc)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	return observation, BridgeExecutionEvidence{
		Request: BridgeBuildRequest{
			Action: action, AmountRaw: uint64(decision.AmountRaw),
			Report: nav.Report, Policy: policy,
			AdaptorConfig: v.Strategy, Settings: backyardSettings,
			RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight,
		},
		ExpectedEffects: effects,
	}, nil
}

func expectedAdaptorReturnData(v *Vault, navAfterRaw uint64) *ExpectedReturnData {
	data := make([]byte, 8)
	binary.LittleEndian.PutUint64(data, navAfterRaw)
	return &ExpectedReturnData{ProgramID: v.Adaptor, DataBase64: base64.StdEncoding.EncodeToString(data)}
}

func bridgeExpectedEffects(v *Vault, decision Decision, idle, strategy, squads uint64) (ExpectedEffects, uint64, uint64, uint64, error) {
	if decision.AmountRaw < 0 {
		return ExpectedEffects{}, 0, 0, 0, fmt.Errorf("negative bridge amount")
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
			return ExpectedEffects{}, 0, 0, 0, fmt.Errorf("allocation effects overflow or underflow")
		}
		idleAfter, squadsAfter = idle-amount, squads+amount
	case StageSquadsToVoltr:
		if v.trustful() {
			// Voltr's withdraw_strategy sweeps the holding account into idle
			// in the same transaction.
			if squads < amount || idle > math.MaxUint64-amount {
				return ExpectedEffects{}, 0, 0, 0, fmt.Errorf("withdrawal effects overflow or underflow")
			}
			squadsAfter, idleAfter = squads-amount, idle+amount
			break
		}
		if squads < amount || strategy > math.MaxUint64-amount {
			return ExpectedEffects{}, 0, 0, 0, fmt.Errorf("staging effects overflow or underflow")
		}
		squadsAfter, strategyAfter = squads-amount, strategy+amount
	case VoltrRestoreIdle:
		if strategy < amount || idle > math.MaxUint64-amount {
			return ExpectedEffects{}, 0, 0, 0, fmt.Errorf("restore effects overflow or underflow")
		}
		strategyAfter, idleAfter = strategy-amount, idle+amount
	case ReportNAV:
		if amount != 0 {
			return ExpectedEffects{}, 0, 0, 0, fmt.Errorf("NAV report cannot move capital")
		}
	default:
		return ExpectedEffects{}, 0, 0, 0, fmt.Errorf("unsupported bridge action")
	}
	accounts := []ExpectedAccountEffect{
		{Address: v.strategyATA, Owner: bridgeTokenProgram, Mint: fleet.USDCMint, Authority: v.strategyAuth, BeforeRaw: strategy, AfterRaw: strategyAfter},
		{Address: v.cash, Owner: bridgeTokenProgram, Mint: fleet.USDCMint, Authority: v.squads, BeforeRaw: squads, AfterRaw: squadsAfter},
	}
	invokesAdaptor := decision.Action != StageSquadsToVoltr || v.trustful()
	if invokesAdaptor {
		accounts = slices.Insert(accounts, 0, ExpectedAccountEffect{Address: v.idleATA, Owner: bridgeTokenProgram, Mint: fleet.USDCMint, Authority: v.idleAuth, BeforeRaw: idle, AfterRaw: idleAfter})
	}
	return ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true, Accounts: accounts}, idleAfter, strategyAfter, squadsAfter, nil
}

func prepareKaminoFromTickObservation(ctx context.Context, rpc *chain.Client, view *View, manifest RouteManifest, decision Decision, observation Observation) (Observation, KaminoExecutionEvidence, error) {
	if rpc == nil || observation.routeBatch == nil || decision.Action != OpenRouteStep {
		return Observation{}, KaminoExecutionEvidence{}, fmt.Errorf("invalid Kamino evidence request")
	}
	prepareStart, accounts := time.Now(), observation.routeBatch.Accounts
	var err error
	if observation.policies, err = observeInstalledPolicies(ctx, rpc, manifest.vault, observation.Snapshot.Slot); err != nil {
		return Observation{}, KaminoExecutionEvidence{}, err
	}
	route, err := manifest.route(decision.StrategyKey)
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
	if request.ObligationReserves, err = observedObligationTopology(accounts, route); err != nil {
		return Observation{}, KaminoExecutionEvidence{}, err
	}
	effects, err := boundedKaminoDepositEffects(accounts, route, observation.Snapshot.Slot, wireAmount)
	if err != nil {
		return Observation{}, KaminoExecutionEvidence{}, err
	}
	observation.Snapshot.HasPosition = position.HasPosition
	logStage("prepare_kamino_evidence", prepareStart)
	return observation, KaminoExecutionEvidence{Request: request, ExpectedEffects: effects}, nil
}

func selectKaminoLeg(decision Decision, position KaminoPosition) (kaminoLeg, uint64, uint64, error) {
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
	excessPrime, err := withdrawableUnderlyingAtLTV(position, ltvBPS)
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

func withdrawableUnderlyingAtLTV(position KaminoPosition, ltvBPS uint64) (uint64, error) {
	if ltvBPS == 0 || ltvBPS >= 10_000 {
		return 0, fmt.Errorf("invalid repayment release LTV")
	}
	if position.RedeemablePrimeRaw == 0 || position.DebtRaw == 0 {
		return 0, fmt.Errorf("position has no withdrawable repayment collateral")
	}
	debtValue, err := valueBetweenTokenRaw(position.DebtRaw, position.DebtDecimals, position.DebtDecimals, position.DebtPriceSF, position.DebtPriceSF, true)
	if err != nil {
		return 0, err
	}
	requiredDebtValue := new(big.Int).Mul(new(big.Int).SetUint64(debtValue), big.NewInt(10_000))
	requiredDebtValue.Add(requiredDebtValue, new(big.Int).SetUint64(ltvBPS-1))
	requiredDebtValue.Quo(requiredDebtValue, new(big.Int).SetUint64(ltvBPS))
	if !requiredDebtValue.IsUint64() {
		return 0, fmt.Errorf("required unwind collateral exceeds u64")
	}
	requiredCollateral, err := valueBetweenTokenRaw(requiredDebtValue.Uint64(), position.DebtDecimals, position.CollateralDecimals, position.DebtPriceSF, position.CollateralPriceSF, true)
	if err != nil {
		return 0, err
	}
	if requiredCollateral >= position.RedeemablePrimeRaw {
		return 0, fmt.Errorf("no collateral excess is safely withdrawable at unwind LTV")
	}
	return position.RedeemablePrimeRaw - requiredCollateral, nil
}

type kaminoCustodyBoundary struct {
	Address, Mint, Authority string
}

func kaminoLegCustodiesForRoute(leg kaminoLeg, route RuntimeRoute) (kaminoCustodyBoundary, kaminoCustodyBoundary) {
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

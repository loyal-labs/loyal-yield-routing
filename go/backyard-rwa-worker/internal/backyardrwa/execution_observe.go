package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
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
		bridgeVoltrProgram, bridgeVoltrVault, bridgeStrategy, bridgeStrategyAuth,
		bridgeSquadsProgram, bridgeSettings, bridgeSettingsSigner, bridgeVault,
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

func ObserveConfirmedBridgeExecutionEvidence(
	ctx context.Context,
	rpc *RPCClient,
	manifest RouteManifest,
	decision Decision,
	postMutationNAVRequired bool,
) (Observation, BridgeExecutionEvidence, error) {
	if rpc == nil {
		return Observation{}, BridgeExecutionEvidence{}, fmt.Errorf("RPC client is required")
	}
	policy, policyHash, err := manifest.bridgePolicy(decision.Action)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	for attempt := 0; attempt < maxConfirmedObservationAttempts; attempt++ {
		observation, accounts, err := observeConfirmedRouteSnapshotWithRPCAccounts(ctx, rpc, manifest)
		if err != nil {
			return Observation{}, BridgeExecutionEvidence{}, err
		}
		observation.Snapshot.PostMutationNAVRequired = postMutationNAVRequired
		if !decisionsEqual(Decide(observation.Snapshot), decision) {
			return Observation{}, BridgeExecutionEvidence{}, fmt.Errorf("actionable decision changed before construction")
		}
		route, err := runtimeRoute(decision.StrategyKey)
		if err != nil {
			return Observation{}, BridgeExecutionEvidence{}, err
		}
		ticketRequired := decision.Action != StageSquadsToVoltr
		policyAccount := accountAt(accounts, policy)
		if policyAccount.Owner != bridgeSquadsProgram || policyAccount.Executable ||
			policyAccount.Lamports == 0 || sha256Bytes(policyAccount.Data) != policyHash {
			return Observation{}, BridgeExecutionEvidence{}, fmt.Errorf("bridge policy bytes or owner drifted")
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
		navAccounts, err := selectRouteNAVAccountsForRoute(accounts, route)
		if err != nil {
			return Observation{}, BridgeExecutionEvidence{}, err
		}
		nav, err := ComputeRouteNAVForRoute(observation.Snapshot.Slot, navAccounts, manifest, &postCustodies, route)
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
		blockhash, err := rpc.LatestBlockhash(ctx)
		if err != nil {
			return Observation{}, BridgeExecutionEvidence{}, err
		}
		return observation, BridgeExecutionEvidence{
			Request: BridgeBuildRequest{
				Action: decision.Action, AmountRaw: uint64(decision.AmountRaw),
				Report:        nav.Report,
				AdaptorConfig: bridgeStrategy, Settings: bridgeSettings,
				RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight,
			},
			ExpectedEffects: effects,
		}, nil
	}
	return Observation{}, BridgeExecutionEvidence{}, confirmedObservationUnavailable(fmt.Errorf("confirmed bridge execution inputs did not align"))
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

func ObserveConfirmedKaminoExecutionEvidence(
	ctx context.Context,
	rpc *RPCClient,
	manifest RouteManifest,
	decision Decision,
) (Observation, KaminoExecutionEvidence, error) {
	if rpc == nil || (decision.Action != OpenPrimeUSDCStep && decision.Action != DeleverPrimeUSDCStep &&
		decision.Action != OpenRouteStep && decision.Action != DeleverRouteStep) {
		return Observation{}, KaminoExecutionEvidence{}, fmt.Errorf("invalid Kamino evidence request")
	}
	for attempt := 0; attempt < maxConfirmedObservationAttempts; attempt++ {
		observation, accounts, err := observeConfirmedRouteSnapshotWithRPCAccounts(ctx, rpc, manifest)
		if err != nil {
			return Observation{}, KaminoExecutionEvidence{}, err
		}
		route, err := runtimeRoute(decision.StrategyKey)
		if err != nil {
			return Observation{}, KaminoExecutionEvidence{}, err
		}
		position, err := observeKaminoFromFixedAccounts(ctx, rpc.GetMultipleAccounts, observation.Snapshot.Slot, accounts, route.Kamino)
		if err != nil {
			return Observation{}, KaminoExecutionEvidence{}, err
		}
		leg, wireAmount, effectAmount, err := selectKaminoLeg(decision, position)
		if err != nil {
			return Observation{}, KaminoExecutionEvidence{}, err
		}
		blockhash, err := rpc.LatestBlockhash(ctx)
		if err != nil {
			return Observation{}, KaminoExecutionEvidence{}, err
		}
		request, err := manifest.kaminoPacketForRoute(decision.Action, leg, wireAmount, blockhash, decision.StrategyKey)
		if err != nil {
			return Observation{}, KaminoExecutionEvidence{}, err
		}
		source, destination := kaminoLegCustodiesForRoute(leg, route)
		policy := accountAt(accounts, request.Policy)
		if policy.Owner != bridgeSquadsProgram || policy.Executable || policy.Lamports == 0 ||
			sha256Bytes(policy.Data) != request.PolicyAccountDataSHA256 {
			return Observation{}, KaminoExecutionEvidence{}, fmt.Errorf("PRIME/USDC policy bytes or owner drifted")
		}
		effects, err := exactKaminoTokenEffects(accounts, source, destination, effectAmount)
		if err != nil {
			return Observation{}, KaminoExecutionEvidence{}, err
		}
		observation.Snapshot.HasPosition = position.HasPosition
		return observation, KaminoExecutionEvidence{Request: request, ExpectedEffects: effects}, nil
	}
	return Observation{}, KaminoExecutionEvidence{}, confirmedObservationUnavailable(fmt.Errorf("confirmed bridge and Kamino construction reads did not align"))
}

func selectKaminoLeg(decision Decision, position KaminoPosition) (kaminoPrimeUSDCLeg, uint64, uint64, error) {
	action := decision.Action
	if action == OpenRouteStep {
		action = OpenPrimeUSDCStep
	}
	if action == DeleverRouteStep {
		action = DeleverPrimeUSDCStep
	}
	switch action {
	case OpenPrimeUSDCStep:
		if decision.AmountRaw <= 0 {
			return 0, 0, 0, fmt.Errorf("OPEN requires a positive exact amount")
		}
		if position.CollateralDepositedRaw == 0 && position.DebtRaw == 0 {
			return kaminoLegDeposit, uint64(decision.AmountRaw), uint64(decision.AmountRaw), nil
		}
		if position.CollateralDepositedRaw > 0 && position.DebtRaw == 0 {
			amount, err := position.targetLTVBorrowRaw()
			if err != nil {
				return 0, 0, 0, err
			}
			return kaminoLegBorrow, amount, amount, nil
		}
		if position.CollateralDepositedRaw > 0 && position.DebtRaw > 0 && decision.Reason == "single_loop_redeposit" {
			return kaminoLegDeposit, uint64(decision.AmountRaw), uint64(decision.AmountRaw), nil
		}
	case DeleverPrimeUSDCStep:
		if position.DebtRaw > 0 && decision.Reason == "withdrawal_release_repayment_collateral" {
			receiptRaw, primeRaw, err := withdrawExcessForRepayment(position)
			if err != nil {
				return 0, 0, 0, err
			}
			return kaminoLegWithdraw, receiptRaw, primeRaw, nil
		}
		if position.DebtRaw > 0 {
			amount := position.DebtRaw
			if decision.AmountRaw > 0 && uint64(decision.AmountRaw) < amount {
				amount = uint64(decision.AmountRaw)
			}
			return kaminoLegRepay, amount, amount, nil
		}
		if position.CollateralDepositedRaw > 0 && position.RedeemablePrimeRaw > 0 {
			receiptRaw := position.CollateralDepositedRaw
			if decision.Reason == "phase2_cutover_withdraw_collateral" && decision.AmountRaw > 0 && uint64(decision.AmountRaw) < receiptRaw {
				receiptRaw = uint64(decision.AmountRaw)
			}
			primeRaw := new(big.Int).Mul(new(big.Int).SetUint64(receiptRaw), new(big.Int).SetUint64(position.RedeemablePrimeRaw))
			primeRaw.Quo(primeRaw, new(big.Int).SetUint64(position.CollateralDepositedRaw))
			if !primeRaw.IsUint64() || primeRaw.Sign() <= 0 {
				return 0, 0, 0, fmt.Errorf("partial collateral withdrawal rounds to zero")
			}
			return kaminoLegWithdraw, receiptRaw, primeRaw.Uint64(), nil
		}
	}
	return 0, 0, 0, fmt.Errorf("PRIME/USDC position is not in a supported next-leg state")
}

const unwindLTVBPS uint64 = 4_500

func withdrawExcessForRepayment(position KaminoPosition) (uint64, uint64, error) {
	if position.CollateralDepositedRaw == 0 || position.RedeemablePrimeRaw == 0 || position.DebtRaw == 0 {
		return 0, 0, fmt.Errorf("position has no withdrawable repayment collateral")
	}
	debtValue, err := valueInDebtRaw(position.DebtRaw, position.DebtPriceSF, position.DebtPriceSF, true)
	if err != nil {
		return 0, 0, err
	}
	requiredDebtValue := new(big.Int).Mul(new(big.Int).SetUint64(debtValue), big.NewInt(10_000))
	requiredDebtValue.Add(requiredDebtValue, big.NewInt(int64(unwindLTVBPS-1)))
	requiredDebtValue.Quo(requiredDebtValue, big.NewInt(int64(unwindLTVBPS)))
	if !requiredDebtValue.IsUint64() {
		return 0, 0, fmt.Errorf("required unwind collateral exceeds u64")
	}
	requiredPrime, err := valueInDebtRaw(requiredDebtValue.Uint64(), position.DebtPriceSF, position.CollateralPriceSF, true)
	if err != nil {
		return 0, 0, err
	}
	if requiredPrime >= position.RedeemablePrimeRaw {
		return 0, 0, fmt.Errorf("no collateral excess is safely withdrawable at unwind LTV")
	}
	excessPrime := position.RedeemablePrimeRaw - requiredPrime
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

type kaminoCustodyBoundary struct {
	Address, Mint, Authority string
}

func kaminoLegCustodies(leg kaminoPrimeUSDCLeg) (kaminoCustodyBoundary, kaminoCustodyBoundary) {
	primeVault := kaminoCustodyBoundary{kaminoPrimeCustody, kaminoPrimeMint, bridgeVault}
	primeReserve := kaminoCustodyBoundary{kaminoPrimeLiquiditySupply, kaminoPrimeMint, kaminoPrimeMarketAuthority}
	usdcVault := kaminoCustodyBoundary{bridgeSquadsATA, bridgeUSDC, bridgeVault}
	usdcReserve := kaminoCustodyBoundary{kaminoUSDCLiquiditySupply, bridgeUSDC, kaminoPrimeMarketAuthority}
	switch leg {
	case kaminoLegDeposit:
		return primeVault, primeReserve
	case kaminoLegBorrow:
		return usdcReserve, usdcVault
	case kaminoLegRepay:
		return usdcVault, usdcReserve
	case kaminoLegWithdraw:
		return primeReserve, primeVault
	default:
		return kaminoCustodyBoundary{}, kaminoCustodyBoundary{}
	}
}

func kaminoLegCustodiesForRoute(leg kaminoPrimeUSDCLeg, route RuntimeRoute) (kaminoCustodyBoundary, kaminoCustodyBoundary) {
	if route.Lane == RouteID || route.Lane == "" {
		return kaminoLegCustodies(leg)
	}
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
	return ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Conserved: true, Accounts: []ExpectedAccountEffect{
		{Address: source.Address, Owner: bridgeTokenProgram, Mint: source.Mint, Authority: source.Authority, BeforeRaw: sourceRaw, AfterRaw: sourceRaw - amount},
		{Address: destination.Address, Owner: bridgeTokenProgram, Mint: destination.Mint, Authority: destination.Authority, BeforeRaw: destinationRaw, AfterRaw: destinationRaw + amount},
	}}, nil
}

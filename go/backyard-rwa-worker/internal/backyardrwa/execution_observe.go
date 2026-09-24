package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math"
	"math/big"
	"time"
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

func observeConfirmedBridgeExecutionEvidenceWithEnrichment(ctx context.Context, rpc *RPCClient, manifest RouteManifest, decision Decision, enrich func(context.Context, *Observation) error) (Observation, BridgeExecutionEvidence, error) {
	if rpc == nil || enrich == nil {
		return Observation{}, BridgeExecutionEvidence{}, fmt.Errorf("RPC client is required")
	}
	observation, accounts, err := observeConfirmedRouteSnapshotWithRPCAccountsAndEnrichment(ctx, rpc, manifest, enrich)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	return prepareBridgeFromObservedAccounts(ctx, rpc, manifest, decision, observation, accounts)
}

// Reuse the enriched, receipt-fenced bank owned by this Tick. Only the current
// slot and blockhash need new RPC reads; admission, signing simulation, durable
// authority binding and final send revalidation still run unchanged.
func prepareBridgeFromTickObservation(ctx context.Context, rpc *RPCClient, manifest RouteManifest, decision Decision, observation Observation) (Observation, BridgeExecutionEvidence, error) {
	batch := observation.routeBatch
	if rpc == nil || batch == nil || batch.Slot != observation.Snapshot.Slot || batch.ObservationID != observation.Snapshot.ObservationID || batch.ManifestSHA256 != manifest.SHA256 || observation.Validate() != nil || !freshAt(time.Now().UTC(), observation.ObservedAt, 30*time.Second) {
		return Observation{}, BridgeExecutionEvidence{}, confirmedObservationUnavailable(fmt.Errorf("tick-local bridge observation is missing or stale"))
	}
	slot, err := rpc.ConfirmedSlot(ctx)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	if slot < batch.Slot || slot-batch.Slot > budgetMaxObservationLagSlots {
		return Observation{}, BridgeExecutionEvidence{}, confirmedObservationUnavailable(fmt.Errorf("tick-local bridge observation exceeded slot freshness"))
	}
	return prepareBridgeFromObservedAccounts(ctx, rpc, manifest, decision, observation, batch.Accounts)
}

func prepareBridgeFromObservedAccounts(ctx context.Context, rpc *RPCClient, manifest RouteManifest, decision Decision, observation Observation, accounts []ConfirmedAccount) (Observation, BridgeExecutionEvidence, error) {
	policyPin, err := manifest.bridgePolicy(decision.Action)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	refreshedDecision := Decide(observation.Snapshot)
	if refreshedDecision.Action == HoldManualRecovery {
		// The refresh itself discovered a safety fault. Return the coherent
		// observation so Worker.Tick can durably record the hold and latch it;
		// treating this as ordinary drift would discard the stop.
		return observation, BridgeExecutionEvidence{}, nil
	}
	if !decisionsEqual(refreshedDecision, decision) {
		return Observation{}, BridgeExecutionEvidence{}, confirmedObservationUnavailable(fmt.Errorf("actionable decision changed before construction"))
	}
	route, err := runtimeRoute(decision.StrategyKey)
	if err != nil {
		return Observation{}, BridgeExecutionEvidence{}, err
	}
	ticketRequired := decision.Action != StageSquadsToVoltr
	if phase3BudgetFamilyForLane(route.Lane) != "" {
		// Reserve the entire bridge exit, including a report after staging.
		// Every required policy and the existing ticket must be present in
		// this same confirmed snapshot; admission cannot authorize setup.
		ticketRequired = true
		for _, action := range []Action{VoltrAllocateToSquads, StageSquadsToVoltr, VoltrRestoreIdle, ReportNAV} {
			binding, err := manifest.bridgePolicy(action)
			if err != nil {
				return Observation{}, BridgeExecutionEvidence{}, err
			}
			account := accountAt(accounts, binding.Account)
			if account.Owner != bridgeSquadsProgram || account.Executable || account.Lamports == 0 ||
				!maskedPolicyDigestMatches(account.Data, binding.MaskedByteRanges, binding.NormalizedDigest) {
				return Observation{}, BridgeExecutionEvidence{}, budgetHold("bridge_exit_policy_unavailable")
			}
		}
	}
	policyAccount := accountAt(accounts, policyPin.Account)
	if policyAccount.Owner != bridgeSquadsProgram || policyAccount.Executable ||
		policyAccount.Lamports == 0 || !maskedPolicyDigestMatches(policyAccount.Data, policyPin.MaskedByteRanges, policyPin.NormalizedDigest) {
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

func observeConfirmedKaminoExecutionEvidenceWithEnrichment(
	ctx context.Context,
	rpc *RPCClient,
	manifest RouteManifest,
	decision Decision,
	enrich func(context.Context, *Observation) error,
) (Observation, KaminoExecutionEvidence, error) {
	if rpc == nil || enrich == nil || (decision.Action != OpenPrimeUSDCStep && decision.Action != DeleverPrimeUSDCStep &&
		decision.Action != OpenRouteStep && decision.Action != DeleverRouteStep) {
		return Observation{}, KaminoExecutionEvidence{}, fmt.Errorf("invalid Kamino evidence request")
	}
	for attempt := 0; attempt < maxConfirmedObservationAttempts; attempt++ {
		observation, accounts, err := observeConfirmedRouteSnapshotWithRPCAccountsAndEnrichment(ctx, rpc, manifest, enrich)
		if err != nil {
			return Observation{}, KaminoExecutionEvidence{}, err
		}
		refreshedDecision := Decide(observation.Snapshot)
		if refreshedDecision.Action == HoldManualRecovery {
			// Do not attempt reserve decoding or packet construction after the
			// refresh has already found a durable safety stop. Worker.Tick receives
			// this coherent observation and persists it before returning.
			return observation, KaminoExecutionEvidence{}, nil
		}
		route, err := runtimeRoute(decision.StrategyKey)
		if err != nil {
			return Observation{}, KaminoExecutionEvidence{}, err
		}
		position, err := observeKaminoFromFixedAccounts(ctx, rpc.GetMultipleAccounts, observation.Snapshot.Slot, accounts, route.Kamino)
		if err != nil {
			return Observation{}, KaminoExecutionEvidence{}, err
		}
		repaymentRelease := position.DebtRaw > 0 && decision.Action == DeleverRouteStep && decision.Reason == "withdrawal_release_repayment_collateral" && positionReturnRoute(route.Lane)
		var leg kaminoPrimeUSDCLeg
		var wireAmount, effectAmount uint64
		// Release and full-payoff sizing read raw reserves (see the helpers).
		releaseAccounts := accounts
		if repaymentRelease {
			bound, raw, err := manifest.observeRawRepaymentRelease(ctx, rpc, route, observation.Snapshot.Slot, observation.Snapshot.PilotActive)
			if err != nil {
				return Observation{}, KaminoExecutionEvidence{}, err
			}
			leg, wireAmount, effectAmount, releaseAccounts = kaminoLegWithdraw, bound.ReceiptRaw, bound.LiquidityRaw, raw
		} else {
			leg, wireAmount, effectAmount, err = selectKaminoLeg(observation.Snapshot.PilotActive, decision, position)
			if err != nil {
				return Observation{}, KaminoExecutionEvidence{}, err
			}
		}
		if leg == kaminoLegBorrow {
			wireAmount, err = selectorBorrowAmount(observation.Snapshot, wireAmount)
			if err != nil {
				return Observation{}, KaminoExecutionEvidence{}, err
			}
			effectAmount = wireAmount
		}
		fullPayoff := leg == kaminoLegRepay && decision.Action == DeleverRouteStep && decision.AmountRaw > 0 && uint64(decision.AmountRaw) >= position.DebtRaw
		if fullPayoff {
			bound, raw, err := observeRawFullPayoff(ctx, rpc, route, observation.Snapshot.Slot)
			if err != nil {
				return Observation{}, KaminoExecutionEvidence{}, err
			}
			wireAmount, effectAmount, releaseAccounts = bound.UpperDebtRaw, bound.ObservedDebtRaw, raw
			if debtCashRaw(observation.Snapshot) < 0 || uint64(debtCashRaw(observation.Snapshot)) < wireAmount {
				return Observation{}, KaminoExecutionEvidence{}, budgetHold("full_payoff_cash_insufficient")
			}
		}
		blockhash, err := rpc.LatestBlockhash(ctx)
		if err != nil {
			return Observation{}, KaminoExecutionEvidence{}, err
		}
		request, err := manifest.kaminoPacketForRoute(decision.Action, leg, wireAmount, blockhash, decision.StrategyKey)
		if err != nil {
			return Observation{}, KaminoExecutionEvidence{}, err
		}
		request.ObligationReserves = []string{}
		request.FullPayoff = fullPayoff
		request.RepaymentRelease = repaymentRelease
		request.PilotRepaymentRelease = repaymentRelease && observation.Snapshot.PilotActive
		if repaymentRelease {
			request.ReleaseDebtIdleRaw = uint64(debtCashRaw(observation.Snapshot))
		}
		if position.CollateralDepositedRaw > 0 {
			request.ObligationReserves = append(request.ObligationReserves, route.Kamino.CollateralReserve)
		}
		if position.DebtRaw > 0 {
			if position.CollateralDepositedRaw == 0 {
				return Observation{}, KaminoExecutionEvidence{}, fmt.Errorf("Kamino obligation has debt without the pinned collateral reserve")
			}
			request.ObligationReserves = append(request.ObligationReserves, route.Kamino.DebtReserve)
		}
		source, destination := kaminoLegCustodiesForRoute(leg, route)
		policy := accountAt(accounts, request.Policy)
		if policy.Owner != bridgeSquadsProgram || policy.Executable || policy.Lamports == 0 ||
			sha256Bytes(policy.Data) != request.PolicyAccountDataSHA256 {
			return Observation{}, KaminoExecutionEvidence{}, fmt.Errorf("PRIME/USDC policy bytes or owner drifted")
		}
		var effects ExpectedEffects
		if leg == kaminoLegDeposit {
			effects, err = boundedKaminoDepositEffects(accounts, route, observation.Snapshot.Slot, wireAmount)
		} else if leg == kaminoLegBorrow {
			effects, err = kaminoBorrowEffects(accounts, route, wireAmount)
		} else if leg == kaminoLegRepay {
			effects, err = boundedKaminoRepaymentEffects(releaseAccounts, source, destination, effectAmount, wireAmount)
		} else {
			effects, err = exactKaminoTokenEffects(releaseAccounts, source, destination, effectAmount)
		}
		if err != nil {
			return Observation{}, KaminoExecutionEvidence{}, err
		}
		observation.Snapshot.HasPosition = position.HasPosition
		return observation, KaminoExecutionEvidence{Request: request, ExpectedEffects: effects}, nil
	}
	return Observation{}, KaminoExecutionEvidence{}, confirmedObservationUnavailable(fmt.Errorf("confirmed bridge and Kamino construction reads did not align"))
}

func selectKaminoLeg(pilotActive bool, decision Decision, position KaminoPosition) (kaminoPrimeUSDCLeg, uint64, uint64, error) {
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
			receiptRaw, primeRaw, err = capSelectedWithdrawalEffect(pilotActive, decision, position, receiptRaw, primeRaw)
			if err != nil {
				return 0, 0, 0, err
			}
			return kaminoLegWithdraw, receiptRaw, primeRaw, nil
		}
		if position.DebtRaw > 0 {
			amount := position.DebtRaw
			if decision.AmountRaw > 0 {
				amount = uint64(decision.AmountRaw)
			}
			// Keep the finite decision limit on the wire. KLend transfers only
			// min(request, refreshed debt), which may differ from this request.
			// This is not a forecast of interest or a full-payoff assertion.
			return kaminoLegRepay, amount, min(amount, position.DebtRaw), nil
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
			cappedReceipt, cappedPrime, err := capSelectedWithdrawalEffect(pilotActive, decision, position, receiptRaw, primeRaw.Uint64())
			if err != nil {
				return 0, 0, 0, err
			}
			return kaminoLegWithdraw, cappedReceipt, cappedPrime, nil
		}
	}
	return 0, 0, 0, fmt.Errorf("PRIME/USDC position is not in a supported next-leg state")
}

func capSelectedWithdrawalEffect(pilotActive bool, decision Decision, position KaminoPosition, receiptRaw, collateralRaw uint64) (uint64, uint64, error) {
	if pilotActive || decision.StrategyKey != SelectedRouteID || collateralRaw <= uint64(Phase2TransactionCapRaw) {
		return receiptRaw, collateralRaw, nil
	}
	if position.CollateralDepositedRaw == 0 || position.RedeemablePrimeRaw == 0 {
		return 0, 0, fmt.Errorf("selected withdrawal cap has no collateral exchange rate")
	}
	receipt := new(big.Int).Mul(new(big.Int).SetUint64(uint64(Phase2TransactionCapRaw)), new(big.Int).SetUint64(position.CollateralDepositedRaw))
	receipt.Quo(receipt, new(big.Int).SetUint64(position.RedeemablePrimeRaw))
	if !receipt.IsUint64() || receipt.Sign() <= 0 || receipt.Uint64() > receiptRaw {
		return 0, 0, fmt.Errorf("selected withdrawal cap produced an invalid receipt amount")
	}
	collateral := new(big.Int).Mul(receipt, new(big.Int).SetUint64(position.RedeemablePrimeRaw))
	collateral.Quo(collateral, new(big.Int).SetUint64(position.CollateralDepositedRaw))
	if !collateral.IsUint64() || collateral.Sign() <= 0 || collateral.Uint64() > uint64(Phase2TransactionCapRaw) {
		return 0, 0, fmt.Errorf("selected withdrawal effect exceeds the transaction cap")
	}
	return receipt.Uint64(), collateral.Uint64(), nil
}

const unwindLTVBPS uint64 = 4_500

func withdrawExcessForRepayment(position KaminoPosition) (uint64, uint64, error) {
	return withdrawExcessAtLTV(position, unwindLTVBPS)
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

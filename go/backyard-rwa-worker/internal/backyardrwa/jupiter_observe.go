package backyardrwa

import (
	"context"
	"fmt"
	"math"
)

func observeConfirmedJupiterExecutionEvidenceWithEnrichment(ctx context.Context, rpc *RPCClient, manifest RouteManifest, decision Decision, client *jupiterClient, enrich func(context.Context, *Observation) error) (Observation, JupiterExecutionEvidence, error) {
	if rpc == nil || client == nil || enrich == nil || decision.AmountRaw <= 0 {
		return Observation{}, JupiterExecutionEvidence{}, fmt.Errorf("invalid Jupiter evidence request")
	}
	binding, err := manifest.jupiterPolicyForRoute(decision.Action, decision.StrategyKey)
	if err != nil {
		return Observation{}, JupiterExecutionEvidence{}, err
	}
	for attempt := 0; attempt < maxConfirmedObservationAttempts; attempt++ {
		observation, accounts, err := observeConfirmedRouteSnapshotWithRPCAccountsAndEnrichment(ctx, rpc, manifest, enrich)
		if err != nil {
			return Observation{}, JupiterExecutionEvidence{}, err
		}
		refreshedDecision := Decide(observation.Snapshot)
		if refreshedDecision.Action == HoldManualRecovery {
			// Preserve the refreshed safety decision for Worker.Tick to journal
			// atomically with its route latch instead of discarding it as drift.
			return observation, JupiterExecutionEvidence{}, nil
		}
		if !decisionsEqual(refreshedDecision, decision) {
			return Observation{}, JupiterExecutionEvidence{}, confirmedObservationUnavailable(
				fmt.Errorf("actionable decision changed before Jupiter construction"),
			)
		}
		sourceMint, destinationMint, sourceATA, destinationATA, err := jupiterEdgeForRoute(decision.Action, decision.StrategyKey)
		if err != nil {
			return Observation{}, JupiterExecutionEvidence{}, err
		}
		sourceProgram, destinationProgram := bridgeTokenProgram, bridgeTokenProgram
		if catalogJupiterRoute(decision.StrategyKey) {
			edge, err := catalogJupiterBindingForRoute(decision.Action, decision.StrategyKey)
			if err != nil {
				return Observation{}, JupiterExecutionEvidence{}, err
			}
			sourceProgram, destinationProgram = edge.SourceTokenProgram, edge.DestinationTokenProgram
		}
		policy := accountAt(accounts, binding.Policy)
		if policy.Owner != bridgeSquadsProgram || policy.Executable || policy.Lamports == 0 || sha256Bytes(policy.Data) != binding.PolicyAccountDataSHA256 {
			return Observation{}, JupiterExecutionEvidence{}, fmt.Errorf("Jupiter policy bytes or owner drifted")
		}
		decode := func(address, mint, program string) (uint64, error) {
			mintKey, err := decodeBase58PublicKey(mint)
			if err != nil {
				return 0, err
			}
			authority, err := decodeBase58PublicKey(bridgeVault)
			if err != nil {
				return 0, err
			}
			account := accountAt(accounts, address)
			custody, err := DecodeTokenCustody(account.Owner, account.Data, mintKey, authority)
			if err != nil || account.Address != address || account.Owner != program || account.Executable || account.Lamports == 0 {
				return 0, fmt.Errorf("Jupiter custody %s drifted: %w", address, err)
			}
			return custody.Raw, nil
		}
		sourceRaw, err := decode(sourceATA, sourceMint, sourceProgram)
		if err != nil {
			return Observation{}, JupiterExecutionEvidence{}, err
		}
		destinationRaw, err := decode(destinationATA, destinationMint, destinationProgram)
		if err != nil {
			return Observation{}, JupiterExecutionEvidence{}, err
		}
		amount := uint64(decision.AmountRaw)
		if sourceRaw < amount {
			return Observation{}, JupiterExecutionEvidence{}, fmt.Errorf("Jupiter source custody is below exact input")
		}
		evidence, err := prepareJupiterQuoteEvidence(ctx, rpc, client, manifest, decision, sourceRaw, destinationRaw, observation.Snapshot.Slot)
		if err == nil && decision.Action == SwapStableToCollateralStep && phase3BudgetFamilyForLane(decision.StrategyKey) != "" {
			evidence.Request.EntryReturnReserved = true
		}
		if err == nil && decision.Action == SwapDebtToCollateralStep && catalogJupiterRoute(decision.StrategyKey) {
			evidence.Request.PositionReturnReserved = true
		}
		funding := (decision.Action == SwapCollateralToDebtStep && decision.Reason == "withdrawal_swap_repayment_buffer") ||
			(decision.Action == SwapUSDCToDebtStep && decision.Reason == "withdrawal_usdc_repayment_buffer")
		if err == nil && funding && observation.Snapshot.PositionDebtRaw > 0 && catalogJupiterRoute(decision.StrategyKey) {
			evidence.Request.FullPayoffFunding = true
		}
		return observation, evidence, err
	}
	return Observation{}, JupiterExecutionEvidence{}, confirmedObservationUnavailable(fmt.Errorf("confirmed Jupiter construction reads did not align"))
}

// Current execution calls this after checking actual custody/policy accounts.
// Exit costing may also quote prospective balances, but must never treat that
// estimate as current-state simulation or promote its wire to execution.
func prepareJupiterQuoteEvidence(ctx context.Context, rpc *RPCClient, client *jupiterClient, manifest RouteManifest, decision Decision, sourceRaw, destinationRaw uint64, slot int64) (JupiterExecutionEvidence, error) {
	binding, err := manifest.jupiterPolicyForRoute(decision.Action, decision.StrategyKey)
	if err != nil {
		return JupiterExecutionEvidence{}, err
	}
	sourceMint, destinationMint, sourceATA, destinationATA, err := jupiterEdgeForRoute(decision.Action, decision.StrategyKey)
	if err != nil {
		return JupiterExecutionEvidence{}, err
	}
	sourceProgram, destinationProgram := bridgeTokenProgram, bridgeTokenProgram
	if catalogJupiterRoute(decision.StrategyKey) {
		edge, err := catalogJupiterBindingForRoute(decision.Action, decision.StrategyKey)
		if err != nil {
			return JupiterExecutionEvidence{}, err
		}
		sourceProgram, destinationProgram = edge.SourceTokenProgram, edge.DestinationTokenProgram
	}
	if rpc == nil || client == nil || decision.AmountRaw <= 0 || uint64(decision.AmountRaw) > sourceRaw || slot <= 0 {
		return JupiterExecutionEvidence{}, fmt.Errorf("invalid Jupiter quote construction inputs")
	}
	amount := uint64(decision.AmountRaw)
	quote, instruction, err := client.freshSwapForRoute(ctx, decision.StrategyKey, decision.Action, amount)
	if err != nil {
		// Jupiter may temporarily return a different route dialect or account
		// shape as liquidity changes. That is a fail-closed observation miss,
		// not a fatal worker invariant: do not journal or sign it, and let the
		// serialized loop request a fresh quote on its next bounded tick.
		return JupiterExecutionEvidence{}, confirmedObservationUnavailable(err)
	}
	constraintIndex, err := binding.constraintIndex(instruction)
	if err != nil {
		return JupiterExecutionEvidence{}, confirmedObservationUnavailable(err)
	}
	out, minimum, err := validateJupiterQuoteForRoute(quote, decision.Action, amount, decision.StrategyKey)
	if err != nil || destinationRaw > math.MaxUint64-minimum {
		return JupiterExecutionEvidence{}, fmt.Errorf("Jupiter destination threshold overflows")
	}
	minimumAfter := destinationRaw + minimum
	blockhash, err := rpc.LatestBlockhash(ctx)
	if err != nil {
		return JupiterExecutionEvidence{}, err
	}
	request := JupiterSwapRequest{Action: decision.Action, AmountRaw: amount, QuotedOutputRaw: out, MinimumOutputRaw: minimum, Policy: binding.Policy, PolicyAccountDataSHA256: binding.PolicyAccountDataSHA256, PolicyConstraintIndex: constraintIndex, Instruction: instruction, RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight, RouteLane: decision.StrategyKey}
	request, err = prepareJupiterLookupTables(ctx, rpc, request, slot)
	if err != nil {
		return JupiterExecutionEvidence{}, confirmedObservationUnavailable(err)
	}
	return JupiterExecutionEvidence{
		Request: request,
		ExpectedEffects: ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "cross-mint-swap", Conserved: false, Accounts: []ExpectedAccountEffect{
			{Address: sourceATA, Owner: sourceProgram, Mint: sourceMint, Authority: bridgeVault, BeforeRaw: sourceRaw, AfterRaw: sourceRaw - amount},
			{Address: destinationATA, Owner: destinationProgram, Mint: destinationMint, Authority: bridgeVault, BeforeRaw: destinationRaw, AfterRaw: minimumAfter, MinimumAfterRaw: &minimumAfter},
		}},
	}, nil
}

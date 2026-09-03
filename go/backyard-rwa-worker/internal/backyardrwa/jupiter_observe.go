package backyardrwa

import (
	"context"
	"fmt"
	"math"
)

func ObserveConfirmedJupiterExecutionEvidence(ctx context.Context, rpc *RPCClient, manifest RouteManifest, decision Decision, client *jupiterClient) (Observation, JupiterExecutionEvidence, error) {
	if rpc == nil || client == nil || (decision.Action != SwapUSDCToPrimeStep && decision.Action != SwapPrimeToUSDCStep && decision.Action != SwapStableToCollateralStep && decision.Action != SwapCollateralToStableStep) || decision.AmountRaw <= 0 {
		return Observation{}, JupiterExecutionEvidence{}, fmt.Errorf("invalid Jupiter evidence request")
	}
	binding, err := manifest.jupiterPolicyForRoute(decision.Action, decision.StrategyKey)
	if err != nil {
		return Observation{}, JupiterExecutionEvidence{}, err
	}
	for attempt := 0; attempt < maxConfirmedObservationAttempts; attempt++ {
		observation, accounts, err := observeConfirmedRouteSnapshotWithRPCAccounts(ctx, rpc, manifest)
		if err != nil {
			return Observation{}, JupiterExecutionEvidence{}, err
		}
		if !decisionsEqual(Decide(observation.Snapshot), decision) {
			return Observation{}, JupiterExecutionEvidence{}, fmt.Errorf("actionable decision changed before Jupiter construction")
		}
		sourceMint, destinationMint, sourceATA, destinationATA, _ := jupiterEdgeForRoute(decision.Action, decision.StrategyKey)
		policy := accountAt(accounts, binding.Policy)
		if policy.Owner != bridgeSquadsProgram || policy.Executable || policy.Lamports == 0 || sha256Bytes(policy.Data) != binding.PolicyAccountDataSHA256 {
			return Observation{}, JupiterExecutionEvidence{}, fmt.Errorf("Jupiter policy bytes or owner drifted")
		}
		decode := func(address, mint string) (uint64, error) {
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
			if err != nil || account.Address != address || account.Owner != bridgeTokenProgram || account.Executable || account.Lamports == 0 {
				return 0, fmt.Errorf("Jupiter custody %s drifted: %w", address, err)
			}
			return custody.Raw, nil
		}
		sourceRaw, err := decode(sourceATA, sourceMint)
		if err != nil {
			return Observation{}, JupiterExecutionEvidence{}, err
		}
		destinationRaw, err := decode(destinationATA, destinationMint)
		if err != nil {
			return Observation{}, JupiterExecutionEvidence{}, err
		}
		amount := uint64(decision.AmountRaw)
		if sourceRaw < amount {
			return Observation{}, JupiterExecutionEvidence{}, fmt.Errorf("Jupiter source custody is below exact input")
		}
		quote, instruction, err := client.freshSwapForRoute(ctx, decision.StrategyKey, decision.Action, amount)
		if err != nil {
			return Observation{}, JupiterExecutionEvidence{}, err
		}
		constraintIndex, err := binding.constraintIndex(instruction)
		if err != nil {
			return Observation{}, JupiterExecutionEvidence{}, err
		}
		out, minimum, err := validateJupiterQuoteForRoute(quote, decision.Action, amount, decision.StrategyKey)
		if err != nil || destinationRaw > math.MaxUint64-minimum {
			return Observation{}, JupiterExecutionEvidence{}, fmt.Errorf("Jupiter destination threshold overflows")
		}
		minimumAfter := destinationRaw + minimum
		blockhash, err := rpc.LatestBlockhash(ctx)
		if err != nil {
			return Observation{}, JupiterExecutionEvidence{}, err
		}
		return observation, JupiterExecutionEvidence{
			Request: JupiterSwapRequest{Action: decision.Action, AmountRaw: amount, QuotedOutputRaw: out, MinimumOutputRaw: minimum, Policy: binding.Policy, PolicyAccountDataSHA256: binding.PolicyAccountDataSHA256, PolicyConstraintIndex: constraintIndex, Instruction: instruction, RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight, RouteLane: decision.StrategyKey},
			ExpectedEffects: ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "cross-mint-swap", Conserved: false, Accounts: []ExpectedAccountEffect{
				{Address: sourceATA, Owner: bridgeTokenProgram, Mint: sourceMint, Authority: bridgeVault, BeforeRaw: sourceRaw, AfterRaw: sourceRaw - amount},
				{Address: destinationATA, Owner: bridgeTokenProgram, Mint: destinationMint, Authority: bridgeVault, BeforeRaw: destinationRaw, AfterRaw: minimumAfter, MinimumAfterRaw: &minimumAfter},
			}},
		}, nil
	}
	return Observation{}, JupiterExecutionEvidence{}, confirmedObservationUnavailable(fmt.Errorf("confirmed Jupiter construction reads did not align"))
}

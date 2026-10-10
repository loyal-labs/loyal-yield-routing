package backyard

import (
	"context"
	"encoding/base64"
	"fmt"
	"math"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

func observeConfirmedJupiterExecutionEvidenceWithEnrichment(ctx context.Context, rpc *chain.Client, manifest RouteManifest, decision Decision, client *jupiter.Client, enrich func(context.Context, *Observation) error) (Observation, JupiterExecutionEvidence, error) {
	if rpc == nil || client == nil || enrich == nil || decision.AmountRaw <= 0 {
		return Observation{}, JupiterExecutionEvidence{}, fmt.Errorf("invalid Jupiter evidence request")
	}
	for attempt := 0; attempt < maxConfirmedObservationAttempts; attempt++ {
		prepareStart := time.Now()
		observation, accounts, err := observeConfirmedRouteSnapshotWithRPCAccountsAndEnrichment(ctx, rpc, manifest, enrich)
		logStage("prepare_jupiter_observe", prepareStart)
		if err != nil {
			return Observation{}, JupiterExecutionEvidence{}, err
		}
		refreshedDecision := Decide(observation.Snapshot)
		if refreshedDecision.Action == HoldManualRecovery {
			// Preserve the refreshed safety decision for Worker.Tick to journal
			// atomically with its route latch instead of discarding it as drift.
			return observation, JupiterExecutionEvidence{}, nil
		}
		// The policies this build executes through: one read, at its slot.
		if observation.policies, err = observeInstalledPolicies(ctx, rpc, observation.Snapshot.Slot); err != nil {
			return Observation{}, JupiterExecutionEvidence{}, err
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
		sourceProgram, destinationProgram, err := jupiterTokenPrograms(decision.Action, decision.StrategyKey)
		if err != nil {
			return Observation{}, JupiterExecutionEvidence{}, err
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
		// A partial-withdrawal debt-share swap carries the idle collateral;
		// the exact input is sized here from the prepared snapshot.
		quoteDecision := decision
		if wire, ok := partialWithdrawalWireAmount(observation.Snapshot, decision); ok {
			if wire <= 0 || uint64(wire) > sourceRaw {
				return Observation{}, JupiterExecutionEvidence{}, budgetHold("partial_withdrawal_swap_unavailable")
			}
			quoteDecision.AmountRaw = wire
		}
		evidence, err := prepareJupiterQuoteEvidence(ctx, rpc, client, manifest, observation.policies, quoteDecision, sourceRaw, destinationRaw, observation.Snapshot.Slot)
		logStage("prepare_jupiter_quote", prepareStart)
		if err == nil && decision.Action == SwapStableToCollateralStep && fundedLane(decision.StrategyKey) {
			evidence.Request.EntryReturnReserved = true
			evidence.Request.TopupReturnReserved = decision.Reason == topupSwapReason
		}
		if err == nil && decision.Action == SwapDebtToCollateralStep && positionReturnRoute(decision.StrategyKey) {
			evidence.Request.PositionReturnReserved = true
		}
		funding := (decision.Action == SwapCollateralToDebtStep && (decision.Reason == "withdrawal_swap_repayment_buffer" || decision.Reason == "hard_ltv_buffer_swap" || decision.Reason == leverageDownSwapReason)) ||
			(decision.Action == SwapUSDCToDebtStep && decision.Reason == "withdrawal_usdc_repayment_buffer")
		if err == nil && funding && observation.Snapshot.PositionDebtRaw > 0 && positionReturnRoute(decision.StrategyKey) {
			evidence.Request.FullPayoffFunding = true
		}
		return observation, evidence, err
	}
	return Observation{}, JupiterExecutionEvidence{}, confirmedObservationUnavailable(fmt.Errorf("confirmed Jupiter construction reads did not align"))
}

// jupiterTokenPrograms is the token program of the swap's source and
// destination custody: the AUTO lane's route identities, a catalog edge's
// assets, and the classic program for every USDC-only lane.
func jupiterTokenPrograms(action Action, lane string) (string, string, error) {
	switch {
	case lane == autoAUTOPYUSD.Lane:
		route, err := runtimeRoute(lane)
		if err != nil {
			return "", "", err
		}
		return autoTokenPrograms(route, action)
	case catalogJupiterRoute(lane):
		edges, leg, err := catalogEdge(action, lane)
		edge := edges[leg]
		return edge.from.program.String(), edge.to.program.String(), err
	default:
		return bridgeTokenProgram, bridgeTokenProgram, nil
	}
}

// Current execution calls this after checking actual custody/policy accounts.
// Exit costing may also quote prospective balances, but must never treat that
// estimate as current-state simulation or promote its wire to execution.
func prepareJupiterQuoteEvidence(ctx context.Context, rpc *chain.Client, client *jupiter.Client, manifest RouteManifest, policies installedPolicies, decision Decision, sourceRaw, destinationRaw uint64, slot int64) (JupiterExecutionEvidence, error) {
	sourceMint, destinationMint, sourceATA, destinationATA, err := jupiterEdgeForRoute(decision.Action, decision.StrategyKey)
	if err != nil {
		return JupiterExecutionEvidence{}, err
	}
	sourceProgram, destinationProgram, err := jupiterTokenPrograms(decision.Action, decision.StrategyKey)
	if err != nil {
		return JupiterExecutionEvidence{}, err
	}
	if rpc == nil || client == nil || decision.AmountRaw <= 0 || uint64(decision.AmountRaw) > sourceRaw || slot <= 0 {
		return JupiterExecutionEvidence{}, fmt.Errorf("invalid Jupiter quote construction inputs")
	}
	amount := uint64(decision.AmountRaw)
	// The blockhash does not depend on the quote: read both at once, and keep
	// the serial error order (the quote's checks first).
	var quote jupiter.Quote
	var instruction JupiterSwapInstruction
	var blockhash LatestBlockhash
	var blockhashErr error
	err = concurrentReads(ctx, func(ctx context.Context) (err error) {
		quote, instruction, err = freshSwapForRoute(ctx, client, decision.StrategyKey, decision.Action, amount)
		return err
	}, func(ctx context.Context) error {
		blockhash, blockhashErr = latestBlockhash(ctx, rpc)
		return nil
	})
	if err != nil {
		// Jupiter may temporarily return a different route dialect or account
		// shape as liquidity changes. That is a fail-closed observation miss,
		// not a fatal worker invariant: do not journal or sign it, and let the
		// serialized loop request a fresh quote on its next bounded tick.
		return JupiterExecutionEvidence{}, confirmedObservationUnavailable(err)
	}
	data, err := base64.StdEncoding.Strict().DecodeString(instruction.Data)
	if err != nil {
		return JupiterExecutionEvidence{}, confirmedObservationUnavailable(err)
	}
	key, _, err := jupiterPolicyLeg(decision.StrategyKey, decision.Action, data)
	if err != nil {
		return JupiterExecutionEvidence{}, confirmedObservationUnavailable(err)
	}
	policy, err := policies.account(key)
	if err != nil {
		return JupiterExecutionEvidence{}, err
	}
	out, minimum, err := validateJupiterQuoteForRoute(quote, decision.Action, amount, decision.StrategyKey)
	if err != nil || destinationRaw > math.MaxUint64-minimum {
		return JupiterExecutionEvidence{}, fmt.Errorf("Jupiter destination threshold overflows")
	}
	if decision.StrategyKey == autoAUTOPYUSD.Lane {
		// Retention keeps min(JSON threshold, enforceable wire floor): the
		// legacy AUTO wire cannot guarantee more than its quoted output scaled
		// by its own slippage, so funding is never sized off an advisory
		// threshold the program would not enforce. A normalized floor of zero
		// means the quote carries no enforceable output guarantee at all —
		// fail closed with a typed hold the caller can propagate, never a
		// retained zero minimum.
		floor, floorErr := jupiterInstructionWireFloor(instruction)
		if floorErr != nil {
			return JupiterExecutionEvidence{}, confirmedObservationUnavailable(floorErr)
		}
		if floor < minimum {
			minimum = floor
		}
		if minimum == 0 {
			return JupiterExecutionEvidence{}, budgetHold("jupiter_auto_wire_floor_zero")
		}
	}
	minimumAfter := destinationRaw + minimum
	if blockhashErr != nil {
		return JupiterExecutionEvidence{}, blockhashErr
	}
	request := JupiterSwapRequest{Action: decision.Action, AmountRaw: amount, QuotedOutputRaw: out, MinimumOutputRaw: minimum, Policy: policy, Instruction: instruction, RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight, RouteLane: decision.StrategyKey}
	request, err = manifest.prepareJupiterLookupTables(ctx, rpc, request, slot)
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

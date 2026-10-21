package backyard

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
)

func prepareJupiterFromTickObservation(ctx context.Context, rpc *chain.Client, manifest RouteManifest, decision Decision, client *jupiter.Client, observation Observation) (Observation, JupiterExecutionEvidence, error) {
	if rpc == nil || client == nil || observation.routeBatch == nil || decision.AmountRaw <= 0 {
		return Observation{}, JupiterExecutionEvidence{}, fmt.Errorf("invalid Jupiter evidence request")
	}
	prepareStart, accounts, v := time.Now(), observation.routeBatch.Accounts, manifest.vault
	var err error
	if observation.policies, err = observeInstalledPolicies(ctx, rpc, v, observation.Snapshot.Slot); err != nil {
		return Observation{}, JupiterExecutionEvidence{}, err
	}
	sourceMint, destinationMint, sourceATA, destinationATA, err := jupiterEdgeForRoute(v, decision.Action, decision.StrategyKey)
	if err != nil {
		return Observation{}, JupiterExecutionEvidence{}, err
	}
	sourceProgram, destinationProgram, err := jupiterTokenPrograms(v, decision.Action, decision.StrategyKey)
	if err != nil {
		return Observation{}, JupiterExecutionEvidence{}, err
	}
	decode := func(address, mint, program string) (uint64, error) {
		mintKey, err := decodeBase58PublicKey(mint)
		if err != nil {
			return 0, err
		}
		authority, err := decodeBase58PublicKey(v.squads)
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
	evidence, err := prepareJupiterQuoteEvidence(ctx, rpc, client, manifest, observation.policies, decision, sourceRaw, destinationRaw, observation.Snapshot.Slot)
	logStage("prepare_jupiter_quote", prepareStart)
	if err == nil && decision.Action == SwapStableToCollateralStep && earnHeldLane(decision.StrategyKey) {
		evidence.Request.EntryReturnReserved = true
		evidence.Request.TopupReturnReserved = decision.Reason == topupSwapReason
	}
	return observation, evidence, err
}

func jupiterTokenPrograms(v *Vault, action Action, lane string) (string, string, error) {
	switch {
	case catalogJupiterRoute(lane):
		edge, err := catalogConversion(v, action, lane)
		return edge.from.program.String(), edge.to.program.String(), err
	default:
		return bridgeTokenProgram, bridgeTokenProgram, nil
	}
}

// Current execution calls this after checking actual custody/policy accounts.
// Exit costing may also quote prospective balances, but must never treat that
// estimate as current-state simulation or promote its wire to execution.
func prepareJupiterQuoteEvidence(ctx context.Context, rpc *chain.Client, client *jupiter.Client, manifest RouteManifest, policies installedPolicies, decision Decision, sourceRaw, destinationRaw uint64, slot int64) (JupiterExecutionEvidence, error) {
	v := manifest.vault
	maxAccounts, err := jupiterSwapMaxAccounts(v, decision.StrategyKey, decision.Action)
	if err != nil {
		return JupiterExecutionEvidence{}, err
	}
	return prepareJupiterQuoteEvidenceAt(ctx, v, rpc, client, policies, decision, sourceRaw, destinationRaw, slot, maxAccounts)
}

func prepareJupiterQuoteEvidenceAt(ctx context.Context, v *Vault, rpc *chain.Client, client *jupiter.Client, policies installedPolicies, decision Decision, sourceRaw, destinationRaw uint64, slot int64, maxAccounts int) (JupiterExecutionEvidence, error) {
	sourceMint, destinationMint, sourceATA, destinationATA, err := jupiterEdgeForRoute(v, decision.Action, decision.StrategyKey)
	if err != nil {
		return JupiterExecutionEvidence{}, err
	}
	sourceProgram, destinationProgram, err := jupiterTokenPrograms(v, decision.Action, decision.StrategyKey)
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
		quote, instruction, err = freshSwapForRoute(ctx, v, client, decision.StrategyKey, decision.Action, amount, maxAccounts)
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
	key, _, err := jupiterPolicyLeg(v, decision.StrategyKey, decision.Action)
	if err != nil {
		return JupiterExecutionEvidence{}, confirmedObservationUnavailable(err)
	}
	policy, err := policies.account(key)
	if err != nil {
		return JupiterExecutionEvidence{}, err
	}
	out, minimum, err := validateJupiterQuoteForRoute(v, quote, decision.Action, amount, decision.StrategyKey)
	if err != nil || destinationRaw > math.MaxUint64-minimum {
		return JupiterExecutionEvidence{}, fmt.Errorf("Jupiter destination threshold overflows")
	}
	if autoCollateralLane(decision.StrategyKey) {
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
	return JupiterExecutionEvidence{
		Request: request,
		ExpectedEffects: ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "cross-mint-swap", Conserved: false, Accounts: []ExpectedAccountEffect{
			{Address: sourceATA, Owner: sourceProgram, Mint: sourceMint, Authority: v.squads, BeforeRaw: sourceRaw, AfterRaw: sourceRaw - amount},
			{Address: destinationATA, Owner: destinationProgram, Mint: destinationMint, Authority: v.squads, BeforeRaw: destinationRaw, AfterRaw: minimumAfter, MinimumAfterRaw: &minimumAfter},
		}},
	}, nil
}

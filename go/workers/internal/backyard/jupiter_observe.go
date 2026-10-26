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
	if route, routeErr := v.route(decision.StrategyKey); routeErr == nil {
		evidence.ExpectedEffects.Sale = collateralSale(accounts, route, decision.Action, amount, evidence.ExpectedEffects)
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

type prospectiveJupiterQuote struct {
	Request JupiterSwapRequest
}

func quoteProspectiveJupiter(ctx context.Context, rpc *chain.Client, client *jupiter.Client, manifest RouteManifest, policies installedPolicies, decision Decision, sourceRaw, destinationRaw uint64, slot int64) (prospectiveJupiterQuote, error) {
	evidence, err := prepareJupiterQuoteEvidence(ctx, rpc, client, manifest, policies, decision, sourceRaw, destinationRaw, slot)
	return prospectiveJupiterQuote{Request: evidence.Request}, err
}

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
	var instruction JupiterSwapInstruction
	var policy string
	var out, minimum uint64
	var blockhash LatestBlockhash
	err = concurrentReads(ctx, func(ctx context.Context) error {
		var quote jupiter.Quote
		var err error
		if quote, instruction, err = freshSwapForRoute(ctx, v, client, decision.StrategyKey, decision.Action, amount, maxAccounts); err != nil {
			return confirmedObservationUnavailable(err)
		}
		key, _, err := jupiterPolicyLeg(v, decision.StrategyKey, decision.Action)
		if err != nil {
			return confirmedObservationUnavailable(err)
		}
		if policy, err = policies.account(key); err != nil {
			return err
		}
		out, minimum, err = validateJupiterQuoteForRoute(v, quote, decision.Action, amount, decision.StrategyKey)
		if err != nil || destinationRaw > math.MaxUint64-minimum {
			return fmt.Errorf("Jupiter destination threshold overflows")
		}
		if autoCollateralLane(decision.StrategyKey) {
			floor, err := jupiterInstructionWireFloor(instruction)
			if err != nil {
				return confirmedObservationUnavailable(err)
			}
			minimum = min(minimum, floor)
			if minimum == 0 {
				return staleHold("jupiter_auto_wire_floor_zero")
			}
		}
		return nil
	}, func(ctx context.Context) (err error) {
		blockhash, err = latestBlockhash(ctx, rpc)
		return err
	})
	if err != nil {
		return JupiterExecutionEvidence{}, err
	}
	minimumAfter := destinationRaw + minimum
	request := JupiterSwapRequest{Action: decision.Action, AmountRaw: amount, QuotedOutputRaw: out, MinimumOutputRaw: minimum, Policy: policy, Instruction: instruction, RecentBlockhash: blockhash.Blockhash, LastValidBlockHeight: blockhash.LastValidBlockHeight, RouteLane: decision.StrategyKey}
	return JupiterExecutionEvidence{
		Request: request,
		ExpectedEffects: ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: effectsCrossMintSwap, Conserved: false, Accounts: []ExpectedAccountEffect{
			{Address: sourceATA, Owner: sourceProgram, Mint: sourceMint, Authority: v.squads, BeforeRaw: sourceRaw, AfterRaw: sourceRaw - amount},
			{Address: destinationATA, Owner: destinationProgram, Mint: destinationMint, Authority: v.squads, BeforeRaw: destinationRaw, AfterRaw: minimumAfter, MinimumAfterRaw: &minimumAfter},
		}},
	}, nil
}

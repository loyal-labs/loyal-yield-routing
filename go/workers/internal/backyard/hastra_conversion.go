package backyard

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/policy"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/hastra"
	"github.com/solana-foundation/solana-go/v2"
)

// hastraPriceLandingSeconds is how long before the stake price's own
// staleness limit a conversion stops being built or sent: a signed wire lives
// until its blockhash expires (about 150 blocks, roughly a minute), and a
// conversion that lands on a price past MaxStaleness reverts (PriceTooStale).
// Inside this window the conversion holds until Hastra's next price push.
const hastraPriceLandingSeconds = 120

func hastraLane(route RuntimeRoute) bool {
	return route.Kamino.CollateralMint == hastra.AUTOMint.String() && route.Kamino.DebtMint == hastra.WYLDSMint.String()
}

// hastraConversionLeg is the Hastra policy leg a lane converts action
// through, when Hastra is that conversion's venue: a Hastra lane's debt into
// collateral (stake deposit), collateral into debt (stake redeem) and USDC
// into debt (mint deposit). Its debt never converts into USDC (Hastra's
// redeem is an admin queue of 43 to 82 minutes), and its collateral and USDC
// convert through Jupiter's catalog edges.
func hastraConversionLeg(action Action, lane string) (byte, bool) {
	l, ok := findEarnLane(lane)
	if !ok || !hastraLane(l.route) {
		return 0, false
	}
	switch action {
	case SwapDebtToCollateralStep:
		return policy.HastraStakeDeposit, true
	case SwapCollateralToDebtStep:
		return policy.HastraStakeRedeem, true
	case SwapUSDCToDebtStep:
		return policy.HastraMintDeposit, true
	}
	return 0, false
}

type hastraEdge struct {
	leg                                              byte
	source, sourceMint, destination, destinationMint string
}

func hastraEdgeForRoute(v *Vault, action Action, lane string) (hastraEdge, error) {
	leg, ok := hastraConversionLeg(action, lane)
	if !ok {
		return hastraEdge{}, fmt.Errorf("lane %q converts %s through no Hastra edge", lane, action)
	}
	route, err := v.route(lane)
	if err != nil {
		return hastraEdge{}, err
	}
	debt := [2]string{route.DebtCustody, route.Kamino.DebtMint}
	collateral := [2]string{route.CollateralCustody, route.Kamino.CollateralMint}
	from, to := [2]string{v.cash, fleet.USDCMint}, debt // mint deposit
	switch leg {
	case policy.HastraStakeDeposit:
		from, to = debt, collateral
	case policy.HastraStakeRedeem:
		from, to = collateral, debt
	}
	return hastraEdge{leg: leg, source: from[0], sourceMint: from[1], destination: to[0], destinationMint: to[1]}, nil
}

type HastraConversionRequest struct {
	Action    Action
	AmountRaw uint64
	OutputRaw uint64
	// The stake price quoted (stake conversions only): Price wYLDS per AUTO
	// times PriceScale, verified for PriceTimestamp.
	Price                string `json:",omitempty"`
	PriceScale           uint64 `json:",omitempty"`
	PriceTimestamp       int64  `json:",omitempty"`
	Policy               string
	RecentBlockhash      string
	LastValidBlockHeight int64
	RouteLane            string
}

type HastraExecutionEvidence struct {
	Request         HastraConversionRequest
	ExpectedEffects ExpectedEffects
}

func (r HastraConversionRequest) stakePrice() (hastra.StakePrice, error) {
	price, ok := new(big.Int).SetString(r.Price, 10)
	if !ok {
		return hastra.StakePrice{}, fmt.Errorf("Hastra request carries no stake price")
	}
	return hastra.StakePrice{Price: price, Scale: r.PriceScale, Timestamp: r.PriceTimestamp}, nil
}

func hastraOutput(leg byte, price hastra.StakePrice, amount uint64) (uint64, error) {
	switch leg {
	case policy.HastraStakeDeposit:
		return price.AUTOForWYLDS(amount)
	case policy.HastraStakeRedeem:
		return price.WYLDSForAUTO(amount)
	case policy.HastraMintDeposit:
		return amount, nil
	}
	return 0, fmt.Errorf("unknown Hastra leg %d", leg)
}

func hastraInstruction(v *Vault, r HastraConversionRequest) (compiledInstruction, byte, error) {
	edge, err := hastraEdgeForRoute(v, r.Action, r.RouteLane)
	if err != nil {
		return compiledInstruction{}, 0, err
	}
	if r.AmountRaw == 0 || r.OutputRaw == 0 {
		return compiledInstruction{}, 0, fmt.Errorf("Hastra conversion of nothing")
	}
	var price hastra.StakePrice
	if edge.leg != policy.HastraMintDeposit {
		if price, err = r.stakePrice(); err != nil {
			return compiledInstruction{}, 0, err
		}
	} else if r.Price != "" || r.PriceScale != 0 || r.PriceTimestamp != 0 {
		return compiledInstruction{}, 0, fmt.Errorf("Hastra mint deposit carries a stake price")
	}
	if out, err := hastraOutput(edge.leg, price, r.AmountRaw); err != nil || out != r.OutputRaw {
		return compiledInstruction{}, 0, fmt.Errorf("Hastra output is not the program's at the quoted price: %w", err)
	}
	vault := kaminoKey(v.squads)
	stake, err := hastra.OwnStake(vault)
	if err != nil {
		return compiledInstruction{}, 0, err
	}
	mint, err := hastra.OwnMint(vault)
	if err != nil {
		return compiledInstruction{}, 0, err
	}
	var ix *solana.GenericInstruction
	switch edge.leg {
	case policy.HastraStakeDeposit:
		ix = hastra.StakeDeposit(stake, r.AmountRaw)
	case policy.HastraStakeRedeem:
		ix = hastra.StakeRedeem(stake, r.AmountRaw)
	default:
		ix = hastra.MintDeposit(mint, r.AmountRaw)
	}
	return kaminoCompiled(ix), edge.leg, nil
}

func compileHastraMessageForDelegate(v *Vault, request HastraConversionRequest, delegate publicKey) ([]byte, error) {
	if request.LastValidBlockHeight <= 0 {
		return nil, fmt.Errorf("invalid Hastra request")
	}
	blockhash, err := decodeKey(request.RecentBlockhash)
	if err != nil {
		return nil, err
	}
	outer, err := hastraPolicyExecute(v, request, delegate)
	if err != nil {
		return nil, err
	}
	message, err := compileV1Message(delegate, blockhash, []compiledInstruction{outer}, executionBudget(request.RouteLane, 1))
	if err != nil {
		return nil, err
	}
	return checkedUnsignedMessage(message)
}

func hastraPolicyExecute(v *Vault, request HastraConversionRequest, delegate publicKey) (compiledInstruction, error) {
	inner, leg, err := hastraInstruction(v, request)
	if err != nil {
		return compiledInstruction{}, err
	}
	policy, err := decodeKey(request.Policy)
	if err != nil || policy == (publicKey{}) {
		return compiledInstruction{}, fmt.Errorf("Hastra request names no installed policy")
	}
	return wrapSquadsPolicy(v, policy, delegate, []byte{leg}, inner)
}

func hastraPriceAt(ctx context.Context, view *View, minimumSlot int64) (hastra.StakePrice, int64, int64, error) {
	slot, accounts, err := view.read(ctx, []string{hastra.StakePriceConfig.String(), budgetClockAddress}, minimumSlot)
	if err != nil {
		return hastra.StakePrice{}, 0, 0, err
	}
	a := accounts[0]
	owner, err := decodeBase58PublicKey(a.Owner)
	if err != nil {
		return hastra.StakePrice{}, 0, 0, err
	}
	price, err := hastra.DecodeStakePrice(&chain.Account{Key: hastra.StakePriceConfig, Owner: solana.PublicKey(owner), Lamports: a.Lamports, Data: a.Data, Executable: a.Executable})
	if err != nil {
		return hastra.StakePrice{}, 0, 0, err
	}
	now := clockUnixTimestamp(accounts[1:])
	if now <= 0 {
		return hastra.StakePrice{}, 0, 0, budgetHold("invalid_valuation_clock")
	}
	return price, now, slot, nil
}

func landable(price hastra.StakePrice, now int64) bool {
	return now <= math.MaxInt64-hastraPriceLandingSeconds && price.Fresh(now+hastraPriceLandingSeconds)
}

func prepareHastraFromTickObservation(ctx context.Context, rpc *chain.Client, view *View, manifest RouteManifest, decision Decision, observation Observation) (Observation, HastraExecutionEvidence, error) {
	if rpc == nil || view == nil || observation.routeBatch == nil || decision.AmountRaw <= 0 {
		return Observation{}, HastraExecutionEvidence{}, fmt.Errorf("invalid Hastra evidence request")
	}
	prepareStart, accounts, v := time.Now(), observation.routeBatch.Accounts, manifest.vault
	var err error
	if observation.policies, err = observeInstalledPolicies(ctx, rpc, v, observation.Snapshot.Slot); err != nil {
		return Observation{}, HastraExecutionEvidence{}, err
	}
	edge, err := hastraEdgeForRoute(v, decision.Action, decision.StrategyKey)
	if err != nil {
		return Observation{}, HastraExecutionEvidence{}, err
	}
	authority, err := decodeBase58PublicKey(v.squads)
	if err != nil {
		return Observation{}, HastraExecutionEvidence{}, err
	}
	decode := func(address, mint string) (uint64, error) {
		mintKey, err := decodeBase58PublicKey(mint)
		if err != nil {
			return 0, err
		}
		account := accountAt(accounts, address)
		custody, err := DecodeTokenCustody(account.Owner, account.Data, mintKey, authority)
		if err != nil || account.Address != address || account.Owner != classicTokenProgram || account.Executable || account.Lamports == 0 {
			return 0, fmt.Errorf("Hastra custody %s drifted: %w", address, err)
		}
		return custody.Raw, nil
	}
	sourceRaw, err := decode(edge.source, edge.sourceMint)
	if err != nil {
		return Observation{}, HastraExecutionEvidence{}, err
	}
	destinationRaw, err := decode(edge.destination, edge.destinationMint)
	if err != nil {
		return Observation{}, HastraExecutionEvidence{}, err
	}
	evidence, err := prepareHastraQuoteEvidence(ctx, v, rpc, view, observation.policies, decision, sourceRaw, destinationRaw, observation.Snapshot.Slot)
	logStage("prepare_hastra_quote", prepareStart)
	if err != nil {
		return observation, evidence, err
	}
	return observation, evidence, nil
}

func prepareHastraQuoteEvidence(ctx context.Context, v *Vault, rpc *chain.Client, view *View, policies installedPolicies, decision Decision, sourceRaw, destinationRaw uint64, slot int64) (HastraExecutionEvidence, error) {
	edge, err := hastraEdgeForRoute(v, decision.Action, decision.StrategyKey)
	if err != nil {
		return HastraExecutionEvidence{}, err
	}
	if rpc == nil || view == nil || decision.AmountRaw <= 0 || uint64(decision.AmountRaw) > sourceRaw || slot <= 0 {
		return HastraExecutionEvidence{}, fmt.Errorf("invalid Hastra quote construction inputs")
	}
	amount := uint64(decision.AmountRaw)
	request := HastraConversionRequest{Action: decision.Action, AmountRaw: amount, RouteLane: decision.StrategyKey}
	var price hastra.StakePrice
	if edge.leg != policy.HastraMintDeposit {
		var now int64
		if price, now, _, err = hastraPriceAt(ctx, view, slot); err != nil {
			return HastraExecutionEvidence{}, confirmedObservationUnavailable(err)
		}
		if !landable(price, now) {
			return HastraExecutionEvidence{}, budgetHold("hastra_price_stale")
		}
		request.Price, request.PriceScale, request.PriceTimestamp = price.Price.String(), price.Scale, price.Timestamp
	}
	if request.OutputRaw, err = hastraOutput(edge.leg, price, amount); err != nil {
		return HastraExecutionEvidence{}, budgetHoldFrom(err, "hastra_conversion_rounds_to_zero")
	}
	if destinationRaw > math.MaxUint64-request.OutputRaw {
		return HastraExecutionEvidence{}, fmt.Errorf("Hastra destination overflows")
	}
	if request.Policy, err = policies.account(policyKey{hastra: hastraName()}); err != nil {
		return HastraExecutionEvidence{}, err
	}
	blockhash, err := latestBlockhash(ctx, rpc)
	if err != nil {
		return HastraExecutionEvidence{}, err
	}
	request.RecentBlockhash, request.LastValidBlockHeight = blockhash.Blockhash, blockhash.LastValidBlockHeight
	minimumAfter := destinationRaw + request.OutputRaw
	return HastraExecutionEvidence{
		Request: request,
		ExpectedEffects: ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "cross-mint-swap", Conserved: false, Accounts: []ExpectedAccountEffect{
			{Address: edge.source, Owner: classicTokenProgram, Mint: edge.sourceMint, Authority: v.squads, BeforeRaw: sourceRaw, AfterRaw: sourceRaw - amount},
			{Address: edge.destination, Owner: classicTokenProgram, Mint: edge.destinationMint, Authority: v.squads, BeforeRaw: destinationRaw, AfterRaw: minimumAfter, MinimumAfterRaw: &minimumAfter},
		}},
	}, nil
}

func conversionVenueHastra(decision Decision) bool {
	_, ok := hastraConversionLeg(decision.Action, decision.StrategyKey)
	return ok
}

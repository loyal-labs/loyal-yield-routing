package backyard

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/hastra"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/solana-foundation/solana-go/v2"
)

// hastraPriceLandingSeconds is how long before the stake price's own
// staleness limit a conversion stops being built or sent: a signed wire lives
// until its blockhash expires (about 150 blocks, roughly a minute), and a
// conversion that lands on a price past MaxStaleness reverts (PriceTooStale).
// Inside this window the conversion holds until Hastra's next price push.
const hastraPriceLandingSeconds = 120

// hastraLane reports a lane whose debt converts through Hastra: AUTO
// collateral with wYLDS debt. Keyed on the route's mints, not its name.
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
	route, err := runtimeRoute(lane)
	if err != nil || !hastraLane(route) {
		return 0, false
	}
	switch action {
	case SwapDebtToCollateralStep:
		return hastraStakeDepositLeg, true
	case SwapCollateralToDebtStep:
		return hastraStakeRedeemLeg, true
	case SwapUSDCToDebtStep:
		return hastraMintDepositLeg, true
	}
	return 0, false
}

// hastraEdge is one Hastra conversion's two vault custodies and mints; every
// Hastra account is a classic SPL Token account.
type hastraEdge struct {
	leg                                              byte
	source, sourceMint, destination, destinationMint string
}

func hastraEdgeForRoute(action Action, lane string) (hastraEdge, error) {
	leg, ok := hastraConversionLeg(action, lane)
	if !ok {
		return hastraEdge{}, fmt.Errorf("lane %q converts %s through no Hastra edge", lane, action)
	}
	route, err := runtimeRoute(lane)
	if err != nil {
		return hastraEdge{}, err
	}
	debt := [2]string{route.DebtCustody, route.Kamino.DebtMint}
	collateral := [2]string{route.CollateralCustody, route.Kamino.CollateralMint}
	from, to := [2]string{bridgeSquadsATA, bridgeUSDC}, debt // mint deposit
	switch leg {
	case hastraStakeDepositLeg:
		from, to = debt, collateral
	case hastraStakeRedeemLeg:
		from, to = collateral, debt
	}
	return hastraEdge{leg: leg, source: from[0], sourceMint: from[1], destination: to[0], destinationMint: to[1]}, nil
}

// HastraConversionRequest is one Hastra conversion of the vault's own
// custody, executed through the Hastra policy. A stake conversion's output is
// the program's formula at the stake price it reads when it executes; the
// request carries the price it was quoted at, and is signed only while that
// price is still the program's and lands before going stale
// (validateHastraPrice).
type HastraConversionRequest struct {
	Action    Action
	AmountRaw uint64
	// OutputRaw is what the conversion pays at the quoted price, with the
	// program's rounding: StakePrice.AUTOForWYLDS for a stake deposit,
	// WYLDSForAUTO for a redeem, the amount itself for a 1:1 mint deposit.
	OutputRaw uint64
	// The stake price quoted (stake conversions only): Price wYLDS per AUTO
	// times PriceScale, verified for PriceTimestamp.
	Price          string `json:",omitempty"`
	PriceScale     uint64 `json:",omitempty"`
	PriceTimestamp int64  `json:",omitempty"`
	// Policy is the installed account of the Hastra policy.
	Policy               string
	RecentBlockhash      string
	LastValidBlockHeight int64
	RouteLane            string
}

type HastraExecutionEvidence struct {
	Request         HastraConversionRequest
	ExpectedEffects ExpectedEffects
}

// stakePrice is the request's quoted price.
func (r HastraConversionRequest) stakePrice() (hastra.StakePrice, error) {
	price, ok := new(big.Int).SetString(r.Price, 10)
	if !ok {
		return hastra.StakePrice{}, fmt.Errorf("Hastra request carries no stake price")
	}
	return hastra.StakePrice{Price: price, Scale: r.PriceScale, Timestamp: r.PriceTimestamp}, nil
}

// hastraOutput is what leg pays for amount at price, by the program's
// rounding.
func hastraOutput(leg byte, price hastra.StakePrice, amount uint64) (uint64, error) {
	switch leg {
	case hastraStakeDepositLeg:
		return price.AUTOForWYLDS(amount)
	case hastraStakeRedeemLeg:
		return price.WYLDSForAUTO(amount)
	case hastraMintDepositLeg:
		return amount, nil
	}
	return 0, fmt.Errorf("unknown Hastra leg %d", leg)
}

// hastraInstruction is the request's Hastra instruction and its policy leg.
// The quote must be the program's own output at the carried price.
func hastraInstruction(r HastraConversionRequest) (compiledInstruction, byte, error) {
	edge, err := hastraEdgeForRoute(r.Action, r.RouteLane)
	if err != nil {
		return compiledInstruction{}, 0, err
	}
	if r.AmountRaw == 0 || r.OutputRaw == 0 {
		return compiledInstruction{}, 0, fmt.Errorf("Hastra conversion of nothing")
	}
	var price hastra.StakePrice
	if edge.leg != hastraMintDepositLeg {
		if price, err = r.stakePrice(); err != nil {
			return compiledInstruction{}, 0, err
		}
	} else if r.Price != "" || r.PriceScale != 0 || r.PriceTimestamp != 0 {
		return compiledInstruction{}, 0, fmt.Errorf("Hastra mint deposit carries a stake price")
	}
	if out, err := hastraOutput(edge.leg, price, r.AmountRaw); err != nil || out != r.OutputRaw {
		return compiledInstruction{}, 0, fmt.Errorf("Hastra output is not the program's at the quoted price: %w", err)
	}
	vault := kaminoKey(bridgeVault)
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
	case hastraStakeDepositLeg:
		ix = hastra.StakeDeposit(stake, r.AmountRaw)
	case hastraStakeRedeemLeg:
		ix = hastra.StakeRedeem(stake, r.AmountRaw)
	default:
		ix = hastra.MintDeposit(mint, r.AmountRaw)
	}
	return kaminoCompiled(ix), edge.leg, nil
}

func CompileHastraMessage(request HastraConversionRequest) ([]byte, error) {
	return compileHastraMessageForDelegate(request, mustKey(bridgeDelegate))
}

// compileHastraMessageForDelegate is the conversion's message: the
// canonical heap frame (the Hastra policy is as large as AUTO's KLend policy,
// which the deployed Squads ELF parses only under the 64 KiB heap) and the
// Squads execute of the one Hastra instruction at its policy leg.
func compileHastraMessageForDelegate(request HastraConversionRequest, delegate publicKey) ([]byte, error) {
	if request.LastValidBlockHeight <= 0 {
		return nil, fmt.Errorf("invalid Hastra request")
	}
	blockhash, err := decodeKey(request.RecentBlockhash)
	if err != nil {
		return nil, err
	}
	outer, err := hastraPolicyExecute(request, delegate)
	if err != nil {
		return nil, err
	}
	message, err := compileV1Message(delegate, blockhash, []compiledInstruction{outer}, executionBudget(request.RouteLane, 1))
	if err != nil {
		return nil, err
	}
	return checkedUnsignedMessage(message)
}

// hastraPolicyExecute is the request's conversion as the Squads execute of
// its Hastra instruction at the Hastra policy's leg.
func hastraPolicyExecute(request HastraConversionRequest, delegate publicKey) (compiledInstruction, error) {
	inner, leg, err := hastraInstruction(request)
	if err != nil {
		return compiledInstruction{}, err
	}
	policy, err := decodeKey(request.Policy)
	if err != nil || policy == (publicKey{}) {
		return compiledInstruction{}, fmt.Errorf("Hastra request names no installed policy")
	}
	return wrapSquadsPolicy(policy, delegate, []byte{leg}, inner)
}

// signHastraConversion signs the request's message as the pinned delegate.
func signHastraConversion(request HastraConversionRequest, executor ed25519.PrivateKey, expectedDelegate publicKey) (SignedTransaction, error) {
	message, err := compileHastraMessageForDelegate(request, expectedDelegate)
	if err != nil {
		return SignedTransaction{}, err
	}
	return signMessage(executor, expectedDelegate, message, request.RecentBlockhash, request.LastValidBlockHeight)
}

// hastraPriceAt reads the stake price and the chain clock at a view slot no
// older than minimumSlot.
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

// landable reports whether a conversion built at chain time now still lands
// on price: price stays inside its staleness limit for the wire's lifetime.
func landable(price hastra.StakePrice, now int64) bool {
	return now <= math.MaxInt64-hastraPriceLandingSeconds && price.Fresh(now+hastraPriceLandingSeconds)
}

// validateHastraPrice re-reads the stake price a stake conversion was
// quoted at, before it is signed (validateRequestPrestate): the program's
// price is still the quoted one (a newer push changes what the conversion
// pays) and stays fresh for the signed wire's whole lifetime
// (hastraPriceLandingSeconds), which is what makes the final send, which
// reads no chain, safe. Either change holds the conversion; it never errors.
// It returns the view slot of the read.
func validateHastraPrice(ctx context.Context, view *View, r HastraConversionRequest, slot int64) (int64, error) {
	leg, ok := hastraConversionLeg(r.Action, r.RouteLane)
	if !ok {
		return 0, budgetHold("hastra_conversion_edge_unregistered")
	}
	if leg == hastraMintDepositLeg {
		return slot, nil
	}
	price, now, observed, err := hastraPriceAt(ctx, view, slot)
	if err != nil {
		return 0, budgetHoldFrom(err, "hastra_price_unavailable")
	}
	quoted, err := r.stakePrice()
	if err != nil || price.Price.Cmp(quoted.Price) != 0 || price.Scale != quoted.Scale || price.Timestamp != quoted.Timestamp {
		return 0, budgetHold("hastra_price_moved")
	}
	if !landable(price, now) {
		return 0, budgetHold("hastra_price_stale")
	}
	return observed, nil
}

// prepareHastraFromTickObservation builds a Hastra conversion from the tick's
// own view batch, as the Jupiter path does: the vault's two custodies of the
// edge, the installed Hastra policy, the stake price and a blockhash.
func prepareHastraFromTickObservation(ctx context.Context, rpc *chain.Client, view *View, decision Decision, observation Observation) (Observation, HastraExecutionEvidence, error) {
	if rpc == nil || view == nil || observation.routeBatch == nil || decision.AmountRaw <= 0 {
		return Observation{}, HastraExecutionEvidence{}, fmt.Errorf("invalid Hastra evidence request")
	}
	prepareStart, accounts := time.Now(), observation.routeBatch.Accounts
	var err error
	if observation.policies, err = observeInstalledPolicies(ctx, rpc, observation.Snapshot.Slot); err != nil {
		return Observation{}, HastraExecutionEvidence{}, err
	}
	edge, err := hastraEdgeForRoute(decision.Action, decision.StrategyKey)
	if err != nil {
		return Observation{}, HastraExecutionEvidence{}, err
	}
	authority, err := decodeBase58PublicKey(bridgeVault)
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
	evidence, err := prepareHastraQuoteEvidence(ctx, rpc, view, observation.policies, decision, sourceRaw, destinationRaw, observation.Snapshot.Slot)
	logStage("prepare_hastra_quote", prepareStart)
	if err != nil {
		return observation, evidence, err
	}
	return observation, evidence, nil
}

// prepareHastraQuoteEvidence quotes decision's conversion of the source
// custody's sourceRaw at the stake price at slot, and its effects: the source
// debited exactly the amount and the destination credited at least the
// program's output at that price. A stake price that would go stale before
// the wire lands holds.
func prepareHastraQuoteEvidence(ctx context.Context, rpc *chain.Client, view *View, policies installedPolicies, decision Decision, sourceRaw, destinationRaw uint64, slot int64) (HastraExecutionEvidence, error) {
	edge, err := hastraEdgeForRoute(decision.Action, decision.StrategyKey)
	if err != nil {
		return HastraExecutionEvidence{}, err
	}
	if rpc == nil || view == nil || decision.AmountRaw <= 0 || uint64(decision.AmountRaw) > sourceRaw || slot <= 0 {
		return HastraExecutionEvidence{}, fmt.Errorf("invalid Hastra quote construction inputs")
	}
	amount := uint64(decision.AmountRaw)
	request := HastraConversionRequest{Action: decision.Action, AmountRaw: amount, RouteLane: decision.StrategyKey}
	var price hastra.StakePrice
	if edge.leg != hastraMintDepositLeg {
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
			{Address: edge.source, Owner: classicTokenProgram, Mint: edge.sourceMint, Authority: bridgeVault, BeforeRaw: sourceRaw, AfterRaw: sourceRaw - amount},
			{Address: edge.destination, Owner: classicTokenProgram, Mint: edge.destinationMint, Authority: bridgeVault, BeforeRaw: destinationRaw, AfterRaw: minimumAfter, MinimumAfterRaw: &minimumAfter},
		}},
	}, nil
}

// BuildSimulateAndPersistHastra is the Hastra counterpart of
// BuildSimulateAndPersistJupiter: the bound intent, the prestate (the quoted
// price still the program's), then sign, simulate and persist the wire.
func BuildSimulateAndPersistHastra(ctx context.Context, database *Database, rpc *chain.Client, view *View, operationID string, evidence HastraExecutionEvidence, credentials Credentials) error {
	if database == nil || rpc == nil || operationID == "" {
		return fmt.Errorf("Hastra runtime dependencies are required")
	}
	if _, _, err := hastraInstruction(evidence.Request); err != nil {
		return err
	}
	effects, err := jsonMarshalExpectedEffects(evidence.ExpectedEffects)
	if err != nil {
		return err
	}
	if _, err := DecodeExpectedEffects(effects); err != nil {
		return err
	}
	buildStart := time.Now()
	if err := database.requireBoundIntent(ctx, operationID, evidence.Request, effects); err != nil {
		return err
	}
	if err := validateBuildPrestate(ctx, rpc, view, evidence.Request, evidence.ExpectedEffects); err != nil {
		return err
	}
	logStage("hastra_build_prestate", buildStart)
	signer, err := credentials.signer()
	if err != nil {
		return err
	}
	signed, err := signHastraConversion(evidence.Request, signer, mustKey(bridgeDelegate))
	if err != nil {
		return err
	}
	if err := database.MarkBuilt(ctx, operationID, signed.messageSHA256, effects); err != nil {
		return err
	}
	simulation, err := simulateSigned(ctx, rpc, signed.signedWire)
	if err != nil {
		return err
	}
	logStage("hastra_build_simulate", buildStart)
	if err := database.MarkSimulated(ctx, operationID, simulation); err != nil {
		return err
	}
	build, err := signed.BuildResult(simulation.Slot)
	if err != nil {
		return err
	}
	return database.PersistSigned(ctx, operationID, build)
}

// conversionIntent is what the swap admissions read of a debt/collateral
// conversion, whichever venue converts it: its action, lane, exact input,
// the least output its venue guarantees, and the reservation it carries.
type conversionIntent struct {
	Action                                   Action
	RouteLane                                string
	AmountRaw, MinimumOutputRaw              uint64
	EntryReturnReserved, TopupReturnReserved bool
}

func (r JupiterSwapRequest) intent() conversionIntent {
	return conversionIntent{Action: r.Action, RouteLane: r.RouteLane, AmountRaw: r.AmountRaw, MinimumOutputRaw: r.MinimumOutputRaw,
		EntryReturnReserved: r.EntryReturnReserved, TopupReturnReserved: r.TopupReturnReserved}
}

func (r HastraConversionRequest) intent() conversionIntent {
	return conversionIntent{Action: r.Action, RouteLane: r.RouteLane, AmountRaw: r.AmountRaw, MinimumOutputRaw: r.OutputRaw}
}

// conversionVenueHastra reports a decided conversion that executes through
// Hastra rather than Jupiter.
func conversionVenueHastra(decision Decision) bool {
	_, ok := hastraConversionLeg(decision.Action, decision.StrategyKey)
	return ok
}

// conversionEvidence is one priced conversion of either venue: Request is a
// JupiterSwapRequest or a HastraConversionRequest. The exit and funding
// pricers carry it so a lane's funding leg is its own venue's.
type conversionEvidence struct {
	Request         any
	ExpectedEffects ExpectedEffects
}

func (c conversionEvidence) intent() conversionIntent {
	switch r := c.Request.(type) {
	case JupiterSwapRequest:
		return r.intent()
	case HastraConversionRequest:
		return r.intent()
	}
	return conversionIntent{}
}

// quotedOutputRaw is the conversion's quoted output: Jupiter's quote, or
// Hastra's output at the quoted price.
func (c conversionEvidence) quotedOutputRaw() uint64 {
	switch r := c.Request.(type) {
	case JupiterSwapRequest:
		return r.QuotedOutputRaw
	case HastraConversionRequest:
		return r.OutputRaw
	}
	return 0
}

// prepareConversionQuoteEvidence quotes decision's conversion on its venue:
// Hastra for a Hastra lane's debt conversions (hastraConversionLeg), Jupiter
// for every other.
func prepareConversionQuoteEvidence(ctx context.Context, rpc *chain.Client, view *View, client *jupiter.Client, manifest RouteManifest, policies installedPolicies, decision Decision, sourceRaw, destinationRaw uint64, slot int64) (conversionEvidence, error) {
	if conversionVenueHastra(decision) {
		e, err := prepareHastraQuoteEvidence(ctx, rpc, view, policies, decision, sourceRaw, destinationRaw, slot)
		return conversionEvidence{e.Request, e.ExpectedEffects}, err
	}
	e, err := prepareJupiterQuoteEvidence(ctx, rpc, client, manifest, policies, decision, sourceRaw, destinationRaw, slot)
	return conversionEvidence{e.Request, e.ExpectedEffects}, err
}

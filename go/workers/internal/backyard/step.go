package backyard

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"math"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// StepRequest is one leverage step of a lane as one transaction, from one
// valid position to the next with nothing idle in between:
//
//	up:   refreshes, Squads{borrow}, Squads{swap|stake}, refreshes, Squads{deposit}
//	down: refreshes, Squads{withdraw}, Squads{swap|redeem}, refreshes, Squads{repay}
//
// Each leg is a request its single-leg builder already accepts; the step only
// orders their instructions. KLend's check_refresh counts back from each
// top-level instruction, so each KLend execute carries its own refresh prefix
// directly in front of it. All three legs carry the step's one blockhash.
type StepRequest struct {
	// Open is the KLend borrow (up) or withdraw (down) leg; its
	// ObligationReserves is the obligation's topology before the step.
	Open KaminoPrimeUSDCRequest
	// The conversion of what Open released, exactly one of the two: debt
	// into collateral (up), collateral into debt (down).
	Swap   *JupiterSwapRequest      `json:",omitempty"`
	Hastra *HastraConversionRequest `json:",omitempty"`
	// Close is the KLend deposit (up) or repay (down) leg of held custody
	// plus the conversion's guaranteed output (stepCloseAmount). Its
	// topology is the obligation's after Open, which the step sets.
	Close KaminoPrimeUSDCRequest
}

type StepExecutionEvidence struct {
	Request         StepRequest
	ExpectedEffects ExpectedEffects
}

// conversion is the step's conversion request, whichever venue converts it.
func (r StepRequest) conversion() (any, error) {
	switch {
	case r.Swap != nil && r.Hastra == nil:
		return *r.Swap, nil
	case r.Hastra != nil && r.Swap == nil:
		return *r.Hastra, nil
	}
	return nil, fmt.Errorf("a step converts through exactly one venue")
}

// stepMessage is a compiled step and what it costs: the signed
// transaction's bytes (message and one signature) and its keys, against
// maxTransactionBytes and maxTransactionKeys.
type stepMessage struct {
	Message         []byte
	WireBytes, Keys int
}

// stepUp reports the step's direction from its Open leg.
func stepUp(open KaminoPrimeUSDCRequest, route RuntimeRoute) (bool, error) {
	_, leg, err := kaminoResolvedRouteInstruction(open, route)
	if err != nil {
		return false, err
	}
	switch leg {
	case kaminoLegBorrow:
		return true, nil
	case kaminoLegWithdraw:
		return false, nil
	}
	return false, fmt.Errorf("a step opens with a borrow or a withdrawal, not a %s", leg)
}

// buildStepMessage compiles step into one v1 message paid by delegate. It
// refuses more than maxTransactionKeys keys (step_over_key_limit); WireBytes
// is the signed size, which checkedUnsignedMessage bounds.
func buildStepMessage(step StepRequest, delegate publicKey) (stepMessage, error) {
	route, err := runtimeRoute(step.Open.RouteLane)
	if err != nil {
		return stepMessage{}, err
	}
	if step.Close.RouteLane != route.Lane {
		return stepMessage{}, fmt.Errorf("step legs name different lanes")
	}
	up, err := stepUp(step.Open, route)
	if err != nil {
		return stepMessage{}, err
	}
	conversion, closeLeg := SwapCollateralToDebtStep, kaminoLegRepay
	if up {
		conversion, closeLeg = SwapDebtToCollateralStep, kaminoLegDeposit
	}
	// After a borrow or a partial withdrawal the obligation holds both the
	// collateral deposit and the debt.
	step.Close.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	blockhash, err := decodeKey(step.Open.RecentBlockhash)
	if err != nil {
		return stepMessage{}, err
	}
	open, _, err := kaminoPolicyInstructions(step.Open, delegate, route)
	if err != nil {
		return stepMessage{}, err
	}
	convert, err := stepConversionExecute(step, route.Lane, conversion, delegate)
	if err != nil {
		return stepMessage{}, err
	}
	closing, leg, err := kaminoPolicyInstructions(step.Close, delegate, route)
	if err != nil {
		return stepMessage{}, err
	}
	if leg != closeLeg {
		return stepMessage{}, fmt.Errorf("a %s step closes with a %s, not a %s", map[bool]string{true: "up", false: "down"}[up], closeLeg, leg)
	}
	instructions := append(append(append([]compiledInstruction{}, open...), convert), closing...)
	keys := distinctKeys(delegate, instructions)
	if keys > maxTransactionKeys {
		return stepMessage{Keys: keys}, budgetHold("step_over_key_limit")
	}
	message, err := compileV1Message(delegate, blockhash, instructions, executionBudget(route.Lane, len(instructions)))
	if err != nil {
		return stepMessage{}, err
	}
	return stepMessage{Message: message, WireBytes: len(message) + ed25519.SignatureSize, Keys: keys}, nil
}

// stepSwapMaxAccounts is the route size a step's swap is quoted at: the key
// budget the step's legs leave (jupiterMaxAccounts), with the swap counted
// by its fixed accounts.
func stepSwapMaxAccounts(step StepRequest, action Action) (int, error) {
	route, err := runtimeRoute(step.Open.RouteLane)
	if err != nil {
		return 0, err
	}
	step.Close.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	delegate := mustKey(bridgeDelegate)
	open, _, err := kaminoPolicyInstructions(step.Open, delegate, route)
	if err != nil {
		return 0, err
	}
	closing, _, err := kaminoPolicyInstructions(step.Close, delegate, route)
	if err != nil {
		return 0, err
	}
	skeleton, err := jupiterSkeletonExecute(route.Lane, action)
	if err != nil {
		return 0, err
	}
	return jupiterMaxAccounts(append(append(open, skeleton), closing...)), nil
}

// compileStepMessageForDelegate is the step's signed message: every leg on
// the step's one blockhash, within the packet.
func compileStepMessageForDelegate(r StepRequest, delegate publicKey) ([]byte, error) {
	conversion, err := r.conversion()
	if err != nil {
		return nil, err
	}
	blockhash, height := r.Open.RecentBlockhash, r.Open.LastValidBlockHeight
	cb, ch := stepConversionBlockhash(conversion)
	if height <= 0 || r.Close.RecentBlockhash != blockhash || r.Close.LastValidBlockHeight != height || cb != blockhash || ch != height {
		return nil, fmt.Errorf("step legs do not share one blockhash")
	}
	step, err := buildStepMessage(r, delegate)
	if err != nil {
		return nil, err
	}
	return checkedUnsignedMessage(step.Message)
}

func stepConversionBlockhash(conversion any) (string, int64) {
	switch c := conversion.(type) {
	case JupiterSwapRequest:
		return c.RecentBlockhash, c.LastValidBlockHeight
	case HastraConversionRequest:
		return c.RecentBlockhash, c.LastValidBlockHeight
	}
	return "", 0
}

// stepConversionExecute is the conversion's Squads execute, through its own
// venue's producer, on lane in the step's direction.
func stepConversionExecute(step StepRequest, lane string, action Action, delegate publicKey) (compiledInstruction, error) {
	conversion, err := step.conversion()
	if err != nil {
		return compiledInstruction{}, err
	}
	switch r := conversion.(type) {
	case JupiterSwapRequest:
		if r.RouteLane != lane || r.Action != action {
			return compiledInstruction{}, fmt.Errorf("step swap is not %s on %s", action, lane)
		}
		return jupiterPolicyExecute(r, delegate)
	case HastraConversionRequest:
		if r.RouteLane != lane || r.Action != action {
			return compiledInstruction{}, fmt.Errorf("step conversion is not %s on %s", action, lane)
		}
		return hastraPolicyExecute(r, delegate)
	}
	return compiledInstruction{}, fmt.Errorf("step conversion %T is neither a Jupiter swap nor a Hastra conversion", conversion)
}

// stepConversionOutput is what the conversion is guaranteed to pay on
// chain: a Jupiter swap's wire floor (quoted out less its slippage, which
// the instruction enforces; a JSON threshold is advisory), or Hastra's
// output at its quoted price, which the conversion pays only while that price
// is the program's: at another price the close leg finds too little custody
// and the whole step fails.
func stepConversionOutput(conversion any) (uint64, error) {
	switch r := conversion.(type) {
	case JupiterSwapRequest:
		return jupiterInstructionWireFloor(r.Instruction)
	case HastraConversionRequest:
		return r.OutputRaw, nil
	}
	return 0, fmt.Errorf("step conversion %T is neither a Jupiter swap nor a Hastra conversion", conversion)
}

// stepCloseAmount is the Close leg's exact amount: the custody held before
// the step plus the conversion's guaranteed output. KLend deposits and repays
// at most that, so the residue is at most the conversion's slippage slack.
func stepCloseAmount(heldRaw uint64, conversion any) (uint64, error) {
	out, err := stepConversionOutput(conversion)
	if err != nil {
		return 0, err
	}
	if out == 0 || heldRaw > math.MaxUint64-out {
		return 0, fmt.Errorf("step close amount is zero or overflows")
	}
	return heldRaw + out, nil
}

// stepEffects bounds what the step does to the vault's two custodies of the
// lane, collateral then debt: each account's AfterRaw (and MinimumAfterRaw)
// is the least it can hold if the step lands. Up: the debt custody is
// untouched (the borrow is exactly what the conversion spends) and the
// collateral custody keeps at least held + guaranteed out - deposit, zero.
// Down: the collateral custody keeps at least held + withdrawn - converted,
// and the debt custody at least held + guaranteed out - repay, zero.
func stepEffects(up bool, collateral, debt ExpectedAccountEffect) ExpectedEffects {
	minimum := func(effect ExpectedAccountEffect) ExpectedAccountEffect {
		after := effect.AfterRaw
		effect.MinimumAfterRaw = &after
		return effect
	}
	accounts := []ExpectedAccountEffect{minimum(collateral), debt}
	if !up {
		accounts[1] = minimum(debt)
	}
	return ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: "atomic-step", Conserved: false, Accounts: accounts}
}

// BuildSimulateAndPersistStep is the step's build: the bound intent, the
// prestate, then sign, simulate and persist the one wire.
func BuildSimulateAndPersistStep(ctx context.Context, database *Database, rpc *chain.Client, view *View, operationID string, evidence StepExecutionEvidence, credentials Credentials) error {
	if database == nil || rpc == nil || operationID == "" {
		return fmt.Errorf("step runtime dependencies are required")
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
	signer, err := credentials.signer()
	if err != nil {
		return err
	}
	build, err := signStep(evidence.Request, signer)
	if err != nil {
		return err
	}
	if err := database.MarkBuilt(ctx, operationID, build.MessageSHA256, effects); err != nil {
		return err
	}
	simulation, err := simulateSigned(ctx, rpc, build.SignedWire)
	if err != nil {
		return err
	}
	logStage("step_build_simulate", buildStart)
	if err := database.MarkSimulated(ctx, operationID, simulation); err != nil {
		return err
	}
	if simulation.Slot <= 0 {
		return fmt.Errorf("exact signed step was not simulated")
	}
	build.SimulationSlot = simulation.Slot
	return database.PersistSigned(ctx, operationID, build)
}

// signStep signs the step's message as the pinned delegate.
func signStep(r StepRequest, executor ed25519.PrivateKey) (BuildResult, error) {
	delegate := mustKey(bridgeDelegate)
	if len(executor) != ed25519.PrivateKeySize || publicKeyFromBytes(executor.Public().(ed25519.PublicKey)) != delegate {
		return BuildResult{}, fmt.Errorf("executor is not the pinned Squads delegate")
	}
	message, err := compileStepMessageForDelegate(r, delegate)
	if err != nil {
		return BuildResult{}, err
	}
	signed, err := signMessage(executor, delegate, message, r.Open.RecentBlockhash, r.Open.LastValidBlockHeight)
	if err != nil {
		return BuildResult{}, err
	}
	return BuildResult{MessageSHA256: signed.messageSHA256, SignedWire: signed.signedWire, SignedWireSHA256: signed.signedWireSHA256,
		TransactionSignature: signed.transactionSignature, RecentBlockhash: signed.recentBlockhash, LastValidBlockHeight: signed.lastValidBlockHeight}, nil
}

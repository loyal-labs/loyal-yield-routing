package backyard

import (
	"crypto/ed25519"
	"fmt"
	"math"
)

// StepRequest is one leverage step of a lane as one transaction, from one
// valid position to the next with nothing idle in between:
//
//	up:   refreshes, Squads{borrow}, Squads{swap|stake}, refreshes, Squads{deposit}
//	down: refreshes, Squads{withdraw}, Squads{swap|redeem}, refreshes, Squads{repay}
//	move: refreshes, Squads{withdraw}, refreshes, Squads{deposit}
//	exit: refreshes, Squads{withdraw}, Squads{swap}
//
// A move converts nothing: its source lane's withdrawal deposits into the
// overflow lane that shares the source's collateral reserve and custody. An
// exit (an equity-zero exit's last step, beside no debt) closes nothing: its
// withdrawal converts to USDC in the Squads bridge account.
// Each leg is a request its single-leg builder already accepts; the step only
// orders their instructions. KLend's check_refresh counts back from each
// top-level instruction, so each KLend execute carries its own refresh prefix
// directly in front of it. All three legs carry the step's one blockhash.
type StepRequest struct {
	Open   KaminoPrimeUSDCRequest
	Swap   *JupiterSwapRequest      `json:",omitempty"`
	Hastra *HastraConversionRequest `json:",omitempty"`
	// Close is the KLend deposit (up, move) or repay (down) leg of held
	// custody plus the conversion's guaranteed output (stepCloseAmount), or
	// a move's redeemed withdrawal. A step sets its topology (the
	// obligation's after Open); a move's is the overflow obligation's own.
	// An exit has none.
	Close *KaminoPrimeUSDCRequest `json:",omitempty"`
}

type StepExecutionEvidence struct {
	Request         StepRequest
	ExpectedEffects ExpectedEffects
}

func (r StepRequest) move() bool { return r.Swap == nil && r.Hastra == nil }

func (r StepRequest) exit() bool { return r.Close == nil }

func (r StepRequest) conversion() (any, error) {
	switch {
	case r.Swap != nil && r.Hastra == nil:
		return *r.Swap, nil
	case r.Hastra != nil && r.Swap == nil:
		return *r.Hastra, nil
	}
	return nil, fmt.Errorf("a step converts through exactly one venue")
}

type stepMessage struct {
	Message         []byte
	WireBytes, Keys int
}

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

func buildStepMessage(v *Vault, step StepRequest, delegate publicKey) (stepMessage, error) {
	route, err := v.route(step.Open.RouteLane)
	if err != nil {
		return stepMessage{}, err
	}
	up, err := stepUp(step.Open, route)
	if err != nil {
		return stepMessage{}, err
	}
	conversion, closeLeg := SwapCollateralToDebtStep, kaminoLegRepay
	if up {
		conversion, closeLeg = SwapDebtToCollateralStep, kaminoLegDeposit
	}
	var closeRequest KaminoPrimeUSDCRequest
	closeRoute := route
	switch {
	case step.exit():
		if up || step.move() {
			return stepMessage{}, fmt.Errorf("an exit withdraws and converts to USDC")
		}
		conversion = SwapCollateralToStableStep
	case step.move():
		closeRequest = *step.Close
		if lane, ok := overflowLaneOf(route.Lane); !ok || closeRequest.RouteLane != lane {
			return stepMessage{}, fmt.Errorf("a move deposits only into its source's overflow lane")
		}
		if up {
			return stepMessage{}, fmt.Errorf("a move opens with a withdrawal")
		}
		if closeRoute, err = v.route(closeRequest.RouteLane); err != nil {
			return stepMessage{}, err
		}
		closeLeg = kaminoLegDeposit
	default:
		closeRequest = *step.Close
		if closeRequest.RouteLane != route.Lane {
			return stepMessage{}, fmt.Errorf("step legs name different lanes")
		}
		closeRequest.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
	}
	blockhash, err := decodeKey(step.Open.RecentBlockhash)
	if err != nil {
		return stepMessage{}, err
	}
	open, _, err := kaminoPolicyInstructions(v, step.Open, delegate, route)
	if err != nil {
		return stepMessage{}, err
	}
	instructions := append([]compiledInstruction{}, open...)
	if !step.move() {
		convert, err := stepConversionExecute(v, step, route.Lane, conversion, delegate)
		if err != nil {
			return stepMessage{}, err
		}
		instructions = append(instructions, convert)
	}
	if !step.exit() {
		closing, leg, err := kaminoPolicyInstructions(v, closeRequest, delegate, closeRoute)
		if err != nil {
			return stepMessage{}, err
		}
		if leg != closeLeg {
			return stepMessage{}, fmt.Errorf("a %s step closes with a %s, not a %s", map[bool]string{true: "up", false: "down"}[up], closeLeg, leg)
		}
		instructions = append(instructions, closing...)
	}
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

func stepSwapMaxAccounts(v *Vault, step StepRequest, action Action) (int, error) {
	route, err := v.route(step.Open.RouteLane)
	if err != nil {
		return 0, err
	}
	delegate := mustKey(bridgeDelegate)
	open, _, err := kaminoPolicyInstructions(v, step.Open, delegate, route)
	if err != nil {
		return 0, err
	}
	skeleton, err := jupiterSkeletonExecute(v, route.Lane, action)
	if err != nil {
		return 0, err
	}
	instructions := append(open, skeleton)
	if !step.exit() {
		closeRequest := *step.Close
		closeRequest.ObligationReserves = []string{route.Kamino.CollateralReserve, route.Kamino.DebtReserve}
		closing, _, err := kaminoPolicyInstructions(v, closeRequest, delegate, route)
		if err != nil {
			return 0, err
		}
		instructions = append(instructions, closing...)
	}
	return jupiterMaxAccounts(instructions), nil
}

func compileStepMessageForDelegate(v *Vault, r StepRequest, delegate publicKey) ([]byte, error) {
	blockhash, height := r.Open.RecentBlockhash, r.Open.LastValidBlockHeight
	cb, ch := blockhash, height
	if !r.move() {
		conversion, err := r.conversion()
		if err != nil {
			return nil, err
		}
		cb, ch = stepConversionBlockhash(conversion)
	}
	if height <= 0 || (!r.exit() && (r.Close.RecentBlockhash != blockhash || r.Close.LastValidBlockHeight != height)) || cb != blockhash || ch != height {
		return nil, fmt.Errorf("step legs do not share one blockhash")
	}
	step, err := buildStepMessage(v, r, delegate)
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

func stepConversionExecute(v *Vault, step StepRequest, lane string, action Action, delegate publicKey) (compiledInstruction, error) {
	conversion, err := step.conversion()
	if err != nil {
		return compiledInstruction{}, err
	}
	switch r := conversion.(type) {
	case JupiterSwapRequest:
		if r.RouteLane != lane || r.Action != action {
			return compiledInstruction{}, fmt.Errorf("step swap is not %s on %s", action, lane)
		}
		return jupiterPolicyExecute(v, r, delegate)
	case HastraConversionRequest:
		if r.RouteLane != lane || r.Action != action {
			return compiledInstruction{}, fmt.Errorf("step conversion is not %s on %s", action, lane)
		}
		return hastraPolicyExecute(v, r, delegate)
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
	return ExpectedEffects{Schema: "loyal-backyard-rwa-expected-effects/v1", Kind: effectsAtomicStep, Conserved: false, Accounts: accounts}
}

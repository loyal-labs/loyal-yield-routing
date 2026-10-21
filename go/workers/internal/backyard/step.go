package backyard

import (
	"crypto/ed25519"
	"fmt"
	"math"
)

type StepRequest struct {
	Open   KaminoLegRequest
	Swap   *JupiterSwapRequest      `json:",omitempty"`
	Hastra *HastraConversionRequest `json:",omitempty"`
	Close  *KaminoLegRequest        `json:",omitempty"`
}

type StepExecutionEvidence struct {
	Request         StepRequest
	ExpectedEffects ExpectedEffects
}

type stepShape uint8

const (
	shapeConvert stepShape = iota
	shapeMove
	shapeExit
)

func (r StepRequest) shape() (stepShape, error) {
	switch {
	case r.Swap != nil && r.Hastra != nil:
		return 0, fmt.Errorf("a step converts through exactly one venue")
	case r.Close == nil && r.Swap == nil && r.Hastra == nil:
		return 0, fmt.Errorf("an exit withdraws and converts to USDC")
	case r.Close == nil:
		return shapeExit, nil
	case r.Swap == nil && r.Hastra == nil:
		return shapeMove, nil
	}
	return shapeConvert, nil
}

func (r StepRequest) conversion() any {
	if r.Swap != nil {
		return *r.Swap
	}
	return *r.Hastra
}

func (r StepRequest) kind(route RuntimeRoute) (StepKind, error) {
	shape, err := r.shape()
	if err != nil {
		return notStep, err
	}
	up, err := opensWithBorrow(r.Open, route)
	if err != nil {
		return notStep, err
	}
	switch {
	case shape == shapeExit && up:
		return notStep, fmt.Errorf("an exit withdraws and converts to USDC")
	case shape == shapeExit:
		return stepExit, nil
	case shape == shapeMove && up:
		return notStep, fmt.Errorf("a move opens with a withdrawal")
	case shape == shapeMove:
		return stepMove, nil
	case up:
		return stepUp, nil
	}
	return stepDown, nil
}

type stepMessage struct {
	Message         []byte
	WireBytes, Keys int
}

func opensWithBorrow(open KaminoLegRequest, route RuntimeRoute) (bool, error) {
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
	kind, err := step.kind(route)
	if err != nil {
		return stepMessage{}, err
	}
	conversion, closeLeg := SwapCollateralToDebtStep, kaminoLegRepay
	var closeRequest KaminoLegRequest
	closeRoute := route
	switch kind {
	case stepExit:
		conversion = SwapCollateralToStableStep
	case stepMove:
		closeRequest = *step.Close
		if lane, ok := overflowLaneOf(route.Lane); !ok || closeRequest.RouteLane != lane {
			return stepMessage{}, fmt.Errorf("a move deposits only into its source's overflow lane")
		}
		if closeRoute, err = v.route(closeRequest.RouteLane); err != nil {
			return stepMessage{}, err
		}
		closeLeg = kaminoLegDeposit
	default:
		if kind == stepUp {
			conversion, closeLeg = SwapDebtToCollateralStep, kaminoLegDeposit
		}
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
	if kind != stepMove {
		convert, err := stepConversionExecute(v, step, route.Lane, conversion, delegate)
		if err != nil {
			return stepMessage{}, err
		}
		instructions = append(instructions, convert)
	}
	if kind != stepExit {
		closing, leg, err := kaminoPolicyInstructions(v, closeRequest, delegate, closeRoute)
		if err != nil {
			return stepMessage{}, err
		}
		if leg != closeLeg {
			return stepMessage{}, fmt.Errorf("a %s step closes with a %s, not a %s", map[bool]string{true: "up", false: "down"}[kind == stepUp], closeLeg, leg)
		}
		instructions = append(instructions, closing...)
	}
	keys := distinctKeys(delegate, instructions)
	if keys > maxTransactionKeys {
		return stepMessage{Keys: keys}, staleHold("step_over_key_limit")
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
	if step.Close != nil {
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
	shape, err := r.shape()
	if err != nil {
		return nil, err
	}
	blockhash, height := r.Open.RecentBlockhash, r.Open.LastValidBlockHeight
	cb, ch := blockhash, height
	if shape != shapeMove {
		cb, ch = stepConversionBlockhash(r.conversion())
	}
	if height <= 0 || (shape != shapeExit && (r.Close.RecentBlockhash != blockhash || r.Close.LastValidBlockHeight != height)) || cb != blockhash || ch != height {
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
	conversion := step.conversion()
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

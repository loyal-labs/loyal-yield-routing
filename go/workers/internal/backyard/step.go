package backyard

import (
	"fmt"
	"math"
)

// atomicStep is one leverage step of a lane as one transaction, from one
// valid position to the next with nothing idle in between:
//
//	up:   refreshes, Squads{borrow}, Squads{swap|stake}, refreshes, Squads{deposit}
//	down: refreshes, Squads{withdraw}, Squads{swap|redeem}, refreshes, Squads{repay}
//
// Each leg is a request its single-leg builder already accepts; the step only
// orders their instructions. KLend's check_refresh counts back from each
// top-level instruction, so each KLend execute carries its own refresh prefix
// directly in front of it.
type atomicStep struct {
	// Open is the KLend borrow (up) or withdraw (down) leg; its
	// ObligationReserves is the obligation's topology before the step.
	Open KaminoPrimeUSDCRequest
	// Conversion is a JupiterSwapRequest or a HastraConversionRequest of
	// what Open released: debt into collateral (up), collateral into debt
	// (down).
	Conversion any
	// Close is the KLend deposit (up) or repay (down) leg of held custody
	// plus the conversion's guaranteed output (stepCloseAmount). Its
	// topology is the obligation's after Open, which the step sets.
	Close KaminoPrimeUSDCRequest
	// Tables are the lookup tables the message may address accounts through:
	// the lane's and, for a swap, Jupiter's. A table holding none of the
	// step's accounts drops out of the message.
	Tables []LookupTableSnapshot
}

// stepMessage is a compiled step and what it costs on the wire.
type stepMessage struct {
	Message []byte
	// WireBytes is the signed transaction's size: one signature and the
	// message, against the solanaPacketBytes cap.
	WireBytes int
	// Accounts is every account the transaction locks, StaticAccounts those
	// the message carries as full keys.
	Accounts, StaticAccounts int
	// Tables are the lookup tables the message addresses accounts through.
	Tables []string
}

func (m stepMessage) Fits() bool { return m.WireBytes <= solanaPacketBytes }

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

// buildStepMessage compiles step into one v0 message paid by delegate. It
// returns the message whatever its size; WireBytes says whether it fits.
func buildStepMessage(step atomicStep, delegate publicKey) (stepMessage, error) {
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
	convert, err := stepConversionExecute(step.Conversion, route.Lane, conversion, delegate)
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
	for _, kaminoLeg := range [][]compiledInstruction{open, closing} {
		if err := validateKaminoRefreshSequence(kaminoLeg); err != nil {
			return stepMessage{}, err
		}
	}
	instructions := append(append(append([]compiledInstruction{}, open...), convert), closing...)
	if autoRoute(route.Lane) {
		// AUTO's KLend and Hastra policies parse only under the 64 KiB heap.
		// No compute-unit frame: the default meter of a nine-instruction
		// transaction is the 1.4M cap.
		instructions = withAutoExecutionHeap(instructions)
	}
	message, layout, err := compileV0(delegate, blockhash, instructions, step.Tables)
	if err != nil {
		return stepMessage{}, err
	}
	out := stepMessage{Message: message, WireBytes: 1 + 64 + len(message), StaticAccounts: layout.static, Accounts: layout.static + layout.loaded}
	for _, table := range layout.tables {
		out.Tables = append(out.Tables, encodeBase58(table[:]))
	}
	return out, nil
}

// stepConversionExecute is the conversion's Squads execute, through its own
// venue's producer, on lane in the step's direction.
func stepConversionExecute(conversion any, lane string, action Action, delegate publicKey) (compiledInstruction, error) {
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
// output at its quoted price.
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
// exactly, so the residue is at most the conversion's slippage slack.
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

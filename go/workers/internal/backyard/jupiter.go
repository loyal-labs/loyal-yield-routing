package backyard

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"math/big"
	"strconv"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/fleet"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/solana-foundation/solana-go/v2"
)

const (
	previousBackyardVault  = "AdwKLBQWKxNewpkjMFMz4NyKit7qXygGpjkqHBCWcriK"
	jupiterMaxSlippageBPS  = uint16(50)
	jupiterMaxRoutePlanLeg = 4
)

// jupiterMaxAccounts is the route size a swap is quoted at: the key budget a
// v1 transaction has left for the route's venue accounts once the rest of it
// (other, which names the swap's twelve fixed accounts) is counted. Jupiter
// treats maxAccounts as a hint; a route over the budget fails to compile.
func jupiterMaxAccounts(other []compiledInstruction) int {
	return max(maxTransactionKeys-distinctKeys(mustKey(backyardDelegate), other), 0)
}

func jupiterSwapMaxAccounts(v *Vault, lane string, action Action) (int, error) {
	skeleton, err := jupiterSkeletonExecute(v, lane, action)
	if err != nil {
		return 0, err
	}
	return jupiterMaxAccounts([]compiledInstruction{skeleton}), nil
}

func jupiterSkeletonExecute(v *Vault, lane string, action Action) (compiledInstruction, error) {
	sourceMint, destinationMint, sourceATA, destinationATA, err := jupiterEdgeForRoute(v, action, lane)
	if err != nil {
		return compiledInstruction{}, err
	}
	sourceProgram, destinationProgram, err := jupiterTokenPrograms(v, action, lane)
	if err != nil {
		return compiledInstruction{}, err
	}
	program := func(mint, tokenProgram, custody string) accountMeta {
		if mint == pyusdMint {
			return accountMeta{key: mustKey(custody), writable: true}
		}
		return accountMeta{key: publicKey(jupiter.AuthorityTokenAccounts(1, kaminoKey(mint), kaminoKey(tokenProgram))[0]), writable: true}
	}
	ro := func(key string) accountMeta { return accountMeta{key: mustKey(key)} }
	inner := compiledInstruction{program: publicKey(jupiter.ProgramID), data: jupiter.SharedAccountsRouteV2Discriminator[:], accounts: []accountMeta{
		{key: publicKey(jupiter.ProgramAuthority(0))}, {key: mustKey(v.squads), signer: true}, {key: mustKey(sourceATA), writable: true},
		program(sourceMint, sourceProgram, sourceATA), program(destinationMint, destinationProgram, destinationATA), {key: mustKey(destinationATA), writable: true},
		ro(sourceMint), ro(destinationMint), ro(sourceProgram), ro(destinationProgram), {key: publicKey(jupiter.EventAuthority)}, {key: publicKey(jupiter.ProgramID)}}}
	return wrapSquadsPolicy(v, publicKey(sha256.Sum256([]byte("jupiter swap policy stand-in"))), mustKey(backyardDelegate), []byte{0}, inner)
}

type JupiterSwapInstruction struct {
	ProgramID string                `json:"programId"`
	Accounts  []jupiter.AccountMeta `json:"accounts"`
	Data      string                `json:"data"`
}

type JupiterSwapRequest struct {
	Action               Action
	AmountRaw            uint64
	QuotedOutputRaw      uint64
	MinimumOutputRaw     uint64
	Policy               string
	Instruction          JupiterSwapInstruction
	RecentBlockhash      string
	LastValidBlockHeight int64
	RouteLane            string
	EntryReturnReserved  bool `json:"entryReturnReserved,omitempty"`
	// TopupReturnReserved marks an entry swap beside a funded debt-free
	// position (plan B3). It is set only for the journaled top-up reason and
	// always together with EntryReturnReserved.
	TopupReturnReserved bool `json:"topupReturnReserved,omitempty"`
}

type JupiterExecutionEvidence struct {
	Request         JupiterSwapRequest
	ExpectedEffects ExpectedEffects
}

func jupiterEdgeForRoute(v *Vault, action Action, lane string) (sourceMint, destinationMint, sourceATA, destinationATA string, err error) {
	if catalogJupiterRoute(lane) {
		edge, err := catalogConversion(v, action, lane)
		return edge.from.mint.String(), edge.to.mint.String(), edge.from.custody.String(), edge.to.custody.String(), err
	}
	route, err := v.route(lane)
	if err != nil {
		return "", "", "", "", err
	}
	switch {
	case !basicSwapAction(action):
		return "", "", "", "", fmt.Errorf("action %s is not an approved basic Jupiter edge", action)
	case action == SwapStableToCollateralStep || action == SwapDebtToCollateralStep:
		return fleet.USDCMint, route.Kamino.CollateralMint, v.cash, route.CollateralCustody, nil
	default:
		return route.Kamino.CollateralMint, fleet.USDCMint, route.CollateralCustody, v.cash, nil
	}
}

func basicSwapAction(action Action) bool {
	switch action {
	case SwapStableToCollateralStep, SwapDebtToCollateralStep, SwapCollateralToStableStep, SwapCollateralToDebtStep:
		return true
	}
	return false
}

func freshSwapForRoute(ctx context.Context, v *Vault, client *jupiter.Client, lane string, action Action, amount uint64, maxAccounts int) (jupiter.Quote, JupiterSwapInstruction, error) {
	if client == nil || amount == 0 || maxAccounts <= 0 {
		return jupiter.Quote{}, JupiterSwapInstruction{}, fmt.Errorf("invalid Jupiter quote request")
	}
	sourceMint, destinationMint, _, _, err := jupiterEdgeForRoute(v, action, lane)
	if err != nil {
		return jupiter.Quote{}, JupiterSwapInstruction{}, err
	}
	// Every swap policy admits shared_accounts_route_v2 only, which the API
	// returns for a V2 quote with shared accounts.
	request := jupiter.QuoteRequest{InputMint: sourceMint, OutputMint: destinationMint, Amount: amount, SlippageBPS: jupiterMaxSlippageBPS,
		MaxAccounts: maxAccounts, InstructionVersion: "V2"}
	if l, ok := findEarnLane(lane); ok {
		request.Dexes = l.jupiterDexes
	}
	quote, err := client.Quote(ctx, request)
	if err != nil {
		return jupiter.Quote{}, JupiterSwapInstruction{}, err
	}
	out, minimum, err := validateJupiterQuoteForRoute(v, quote, action, amount, lane)
	if err != nil {
		return jupiter.Quote{}, JupiterSwapInstruction{}, err
	}
	response, err := client.SwapInstructions(ctx, quote, solana.MustPublicKeyFromBase58(v.squads), true)
	if err != nil {
		return jupiter.Quote{}, JupiterSwapInstruction{}, err
	}
	if response.Companions() {
		return jupiter.Quote{}, JupiterSwapInstruction{}, fmt.Errorf("Jupiter route requires unapproved companion instructions")
	}
	swap := response.SwapInstruction
	instruction := JupiterSwapInstruction{ProgramID: swap.ProgramID, Accounts: swap.Accounts, Data: swap.Data}
	if _, err := validateJupiterInstructionForRoute(v, instruction, action, amount, out, minimum, lane); err != nil {
		return jupiter.Quote{}, JupiterSwapInstruction{}, err
	}
	return quote, instruction, nil
}

func validateJupiterQuoteForRoute(v *Vault, quote jupiter.Quote, action Action, amount uint64, lane string) (uint64, uint64, error) {
	source, destination, _, _, err := jupiterEdgeForRoute(v, action, lane)
	if err != nil {
		return 0, 0, err
	}
	out, err := strconv.ParseUint(quote.OutAmount, 10, 64)
	if err != nil || out == 0 {
		return 0, 0, fmt.Errorf("Jupiter quote has invalid output")
	}
	minimum, err := strconv.ParseUint(quote.OtherAmountThreshold, 10, 64)
	if err != nil || minimum == 0 || minimum > out {
		return 0, 0, fmt.Errorf("Jupiter quote has invalid threshold")
	}
	floor := out * uint64(10_000-jupiterMaxSlippageBPS) / 10_000
	if quote.InputMint != source || quote.OutputMint != destination || quote.InAmount != strconv.FormatUint(amount, 10) ||
		quote.SwapMode != "ExactIn" || quote.SlippageBPS > jupiterMaxSlippageBPS || minimum < floor ||
		len(quote.RoutePlan) == 0 || len(quote.RoutePlan) > jupiterMaxRoutePlanLeg || !jupiter.Null(quote.PlatformFee) {
		return 0, 0, fmt.Errorf("Jupiter quote identity or economics drifted")
	}
	return out, minimum, nil
}

func validateJupiterInstructionForRoute(v *Vault, value JupiterSwapInstruction, action Action, amount, out, minimum uint64, lane string) (compiledInstruction, error) {
	sourceMint, destinationMint, sourceATA, destinationATA, err := jupiterEdgeForRoute(v, action, lane)
	if err != nil {
		return compiledInstruction{}, err
	}
	if value.ProgramID != jupiter.ProgramID.String() || len(value.Accounts) > 64 {
		return compiledInstruction{}, fmt.Errorf("Jupiter program or account set drifted")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(value.Data)
	if err != nil {
		return compiledInstruction{}, fmt.Errorf("Jupiter instruction data is malformed")
	}
	args, err := jupiter.DecodeSharedAccountsRouteV2Args(data)
	if err != nil {
		return compiledInstruction{}, fmt.Errorf("unsupported Jupiter instruction dialect: %w", err)
	}
	if args.InAmount != amount || args.QuotedOutAmount != out || args.SlippageBPS > jupiterMaxSlippageBPS || minimum == 0 || minimum > out {
		return compiledInstruction{}, fmt.Errorf("Jupiter instruction economics drifted")
	}
	for _, expected := range []struct {
		index            int
		key              string
		signer, writable bool
	}{{1, v.squads, true, false}, {2, sourceATA, false, true}, {5, destinationATA, false, true}, {6, sourceMint, false, false}, {7, destinationMint, false, false}} {
		if expected.index >= len(value.Accounts) {
			return compiledInstruction{}, fmt.Errorf("Jupiter account boundary is absent")
		}
		got := value.Accounts[expected.index]
		if got.Pubkey != expected.key || got.IsSigner != expected.signer || got.IsWritable != expected.writable {
			return compiledInstruction{}, fmt.Errorf("Jupiter account boundary %d drifted", expected.index)
		}
	}
	accounts := make([]accountMeta, len(value.Accounts))
	for index, input := range value.Accounts {
		if input.Pubkey == previousBackyardVault || (input.IsSigner && input.Pubkey != v.squads) {
			return compiledInstruction{}, fmt.Errorf("Jupiter route crosses an unapproved authority")
		}
		key, err := decodeKey(input.Pubkey)
		if err != nil {
			return compiledInstruction{}, fmt.Errorf("invalid Jupiter account %d", index)
		}
		accounts[index] = accountMeta{key: key, signer: input.IsSigner, writable: input.IsWritable}
	}
	return compiledInstruction{program: publicKey(jupiter.ProgramID), accounts: accounts, data: data}, nil
}

// jupiterInstructionWireFloor is the output floor the wire itself enforces:
// Jupiter fails a swap that returns less than its quoted_out_amount less its
// slippage_bps, computed here in wide integer arithmetic. The instruction
// carries no other minimum, so a larger JSON threshold is advisory and must
// never be retained as a funding guarantee.
func jupiterInstructionWireFloor(instruction JupiterSwapInstruction) (uint64, error) {
	data, err := base64.StdEncoding.Strict().DecodeString(instruction.Data)
	if err != nil {
		return 0, fmt.Errorf("Jupiter instruction data is malformed")
	}
	args, err := jupiter.DecodeSharedAccountsRouteV2Args(data)
	if err != nil || args.SlippageBPS > jupiterMaxSlippageBPS {
		return 0, fmt.Errorf("Jupiter AUTO wire floor unavailable")
	}
	floor := new(big.Int).Mul(new(big.Int).SetUint64(args.QuotedOutAmount), big.NewInt(int64(10_000-args.SlippageBPS)))
	return floor.Div(floor, big.NewInt(10_000)).Uint64(), nil
}

func autoCollateralLane(lane string) bool {
	l, ok := findEarnLane(lane)
	return ok && l.route.Kamino.CollateralMint == autoAUTOPYUSDTemplate.Kamino.CollateralMint
}

func jupiterValidateAutoRetainedMinimum(request JupiterSwapRequest) error {
	if !autoCollateralLane(request.RouteLane) {
		return nil
	}
	floor, err := jupiterInstructionWireFloor(request.Instruction)
	if err != nil || request.MinimumOutputRaw > floor {
		return fmt.Errorf("Jupiter AUTO minimum exceeds enforceable wire floor")
	}
	return nil
}

func compileJupiterMessageForDelegate(v *Vault, request JupiterSwapRequest, delegate publicKey) ([]byte, error) {
	if request.AmountRaw == 0 || request.LastValidBlockHeight <= 0 {
		return nil, fmt.Errorf("invalid Jupiter request")
	}
	blockhash, err := decodeKey(request.RecentBlockhash)
	if err != nil {
		return nil, err
	}
	outer, err := jupiterPolicyExecute(v, request, delegate)
	if err != nil {
		return nil, err
	}
	message, err := compileV1Message(delegate, blockhash, []compiledInstruction{outer}, swapBudget())
	if err != nil {
		return nil, err
	}
	return checkedUnsignedMessage(message)
}

func jupiterPolicyExecute(v *Vault, request JupiterSwapRequest, delegate publicKey) (compiledInstruction, error) {
	inner, err := validateJupiterInstructionForRoute(v, request.Instruction, request.Action, request.AmountRaw, request.QuotedOutputRaw, request.MinimumOutputRaw, request.RouteLane)
	if err != nil {
		return compiledInstruction{}, err
	}
	if err := jupiterValidateAutoRetainedMinimum(request); err != nil {
		return compiledInstruction{}, err
	}
	_, index, err := jupiterPolicyLeg(v, request.RouteLane, request.Action)
	if err != nil {
		return compiledInstruction{}, err
	}
	policy, err := decodeKey(request.Policy)
	if err != nil || policy == (publicKey{}) {
		return compiledInstruction{}, fmt.Errorf("Jupiter request names no installed policy")
	}
	return wrapSquadsJupiterPolicy(v, policy, delegate, delegate, index, inner)
}

func wrapSquadsJupiterPolicy(v *Vault, policy, executor, expectedDelegate publicKey, constraintIndex byte, inner compiledInstruction) (compiledInstruction, error) {
	if executor != expectedDelegate || inner.program != publicKey(jupiter.ProgramID) {
		return compiledInstruction{}, fmt.Errorf("unrecognized Squads Jupiter policy or delegate")
	}
	return wrapSquadsPolicy(v, policy, executor, []byte{constraintIndex}, inner)
}

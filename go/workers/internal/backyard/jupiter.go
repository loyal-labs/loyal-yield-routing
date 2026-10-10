package backyard

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/jupiter"
	"github.com/solana-foundation/solana-go/v2"
)

const (
	previousBackyardVault  = "AdwKLBQWKxNewpkjMFMz4NyKit7qXygGpjkqHBCWcriK"
	jupiterMaxSlippageBPS  = uint16(50)
	jupiterMaxRoutePlanLeg = 4
)

// JupiterSwapInstruction is the swap instruction as journaled in the request.
type JupiterSwapInstruction struct {
	ProgramID string                `json:"programId"`
	Accounts  []jupiter.AccountMeta `json:"accounts"`
	Data      string                `json:"data"`
	// Quote metadata, not instruction bytes or authority. Tables must be read
	// from chain and may only encode accounts already present above.
	LookupTableAddresses []string `json:"lookupTableAddresses,omitempty"`
}

type JupiterSwapRequest struct {
	Action                  Action
	AmountRaw               uint64
	QuotedOutputRaw         uint64
	MinimumOutputRaw        uint64
	Policy                  string
	PolicyAccountDataSHA256 string
	PolicyConstraintIndex   byte
	Instruction             JupiterSwapInstruction
	RecentBlockhash         string
	LastValidBlockHeight    int64
	RouteLane               string
	LookupTables            []LookupTableSnapshot `json:",omitempty"`
	FullPayoffFunding       bool                  `json:"fullPayoffFunding,omitempty"`
	EntryReturnReserved     bool                  `json:"entryReturnReserved,omitempty"`
	PositionReturnReserved  bool                  `json:"positionReturnReserved,omitempty"`
	// TopupReturnReserved marks an entry swap beside a funded debt-free
	// position (plan B3). It is set only for the journaled top-up reason and
	// always together with EntryReturnReserved.
	TopupReturnReserved bool `json:"topupReturnReserved,omitempty"`
}

type JupiterExecutionEvidence struct {
	Request         JupiterSwapRequest
	ExpectedEffects ExpectedEffects
}

// productionJupiter is the swap/v1 client the process was started with, set
// once by Engine.Run or RunSelectorEvaluate.
var productionJupiter *jupiter.Client

func jupiterEdgeForRoute(action Action, lane string) (sourceMint, destinationMint, sourceATA, destinationATA string, err error) {
	if lane == "" {
		lane = RouteID
	}
	if lane == autoAUTOPYUSD.Lane {
		// AUTO edges resolve from the route's own reviewed identities; the
		// historical catalog entry is never consulted for the candidate lane.
		route, err := runtimeRoute(lane)
		if err != nil {
			return "", "", "", "", err
		}
		return autoSwapEdge(route, action)
	}
	if catalogJupiterRoute(lane) {
		b, err := catalogJupiterBindingForRoute(action, lane)
		return b.SourceMint, b.DestinationMint, b.SourceCustody, b.DestinationCustody, err
	}
	if lane != RouteID && lane != PhaseOneLaneID && lane != SelectedRouteID && lane != "OnRe/ONyc/USDC" {
		return "", "", "", "", fmt.Errorf("unregistered Jupiter lane")
	}
	if lane == PhaseOneLaneID || lane == SelectedRouteID || lane == "OnRe/ONyc/USDC" {
		route, err := runtimeRoute(lane)
		if err != nil {
			return "", "", "", "", err
		}
		switch action {
		case SwapStableToCollateralStep, SwapUSDCToPrimeStep, SwapDebtToCollateralStep:
			return bridgeUSDC, route.Kamino.CollateralMint, bridgeSquadsATA, route.CollateralCustody, nil
		case SwapCollateralToStableStep, SwapPrimeToUSDCStep, SwapCollateralToDebtStep:
			return route.Kamino.CollateralMint, bridgeUSDC, route.CollateralCustody, bridgeSquadsATA, nil
		default:
			return "", "", "", "", fmt.Errorf("action %s is not an approved basic Jupiter edge", action)
		}
	}
	switch action {
	case SwapUSDCToPrimeStep:
		return bridgeUSDC, kaminoPrimeMint, bridgeSquadsATA, kaminoPrimeCustody, nil
	case SwapPrimeToUSDCStep:
		return kaminoPrimeMint, bridgeUSDC, kaminoPrimeCustody, bridgeSquadsATA, nil
	default:
		return "", "", "", "", fmt.Errorf("action %s is not an approved Jupiter edge", action)
	}
}

func freshSwapForRoute(ctx context.Context, client *jupiter.Client, lane string, action Action, amount uint64) (jupiter.Quote, JupiterSwapInstruction, error) {
	if client == nil || amount == 0 {
		return jupiter.Quote{}, JupiterSwapInstruction{}, fmt.Errorf("invalid Jupiter quote request")
	}
	sourceMint, destinationMint, _, _, err := jupiterEdgeForRoute(action, lane)
	if err != nil {
		return jupiter.Quote{}, JupiterSwapInstruction{}, err
	}
	request := jupiter.QuoteRequest{InputMint: sourceMint, OutputMint: destinationMint, Amount: amount, SlippageBPS: jupiterMaxSlippageBPS, MaxAccounts: 32}
	useSharedAccounts := true
	if catalogJupiterRoute(lane) && lane != autoAUTOPYUSD.Lane {
		// AUTO's dialect is fixed by its reviewed policy shape (legacy
		// SharedAccountsRoute), not by the historical catalog entries.
		binding, err := catalogJupiterBindingForRoute(action, lane)
		if err != nil {
			return jupiter.Quote{}, JupiterSwapInstruction{}, err
		}
		if binding.fixedPrefixV2() {
			request.InstructionVersion = "V2"
		}
		useSharedAccounts = binding.DiscriminatorHex == hex.EncodeToString(jupiter.SharedAccountsRouteDiscriminator[:]) || binding.fixedPrefixV2()
	}
	if lane == SelectedRouteID {
		// The selected RWA representative was reviewed against Manifest. Keep
		// Jupiter's optimizer inside that one venue family instead of accepting
		// whichever venue happens to quote one raw unit better.
		request.Dexes = "Manifest"
	}
	quote, err := client.Quote(ctx, request)
	if err != nil {
		return jupiter.Quote{}, JupiterSwapInstruction{}, err
	}
	out, minimum, err := validateJupiterQuoteForRoute(quote, action, amount, lane)
	if err != nil {
		return jupiter.Quote{}, JupiterSwapInstruction{}, err
	}
	response, err := client.SwapInstructions(ctx, quote, solana.MustPublicKeyFromBase58(bridgeVault), useSharedAccounts)
	if err != nil {
		return jupiter.Quote{}, JupiterSwapInstruction{}, err
	}
	if response.Companions() {
		return jupiter.Quote{}, JupiterSwapInstruction{}, fmt.Errorf("Jupiter route requires unapproved companion instructions")
	}
	swap := response.SwapInstruction
	instruction := JupiterSwapInstruction{ProgramID: swap.ProgramID, Accounts: swap.Accounts, Data: swap.Data}
	if _, err := validateJupiterInstructionForRoute(instruction, action, amount, out, minimum, lane); err != nil {
		return jupiter.Quote{}, JupiterSwapInstruction{}, err
	}
	if lane == RouteID || lane == "" {
		if err := validateInstalledJupiterHeader(action, instruction); err != nil {
			return jupiter.Quote{}, JupiterSwapInstruction{}, err
		}
	}
	if acceptsJupiterLookupHints(lane, action) {
		instruction.LookupTableAddresses = response.AddressLookupTableAddresses
		if err := validateJupiterLookupCandidates(response.AddressLookupTableAddresses); err != nil {
			return jupiter.Quote{}, JupiterSwapInstruction{}, err
		}
	}
	return quote, instruction, nil
}

// The installed Phase 1 and selected Phase 2 policies accept only legacy 37-byte
// SharedAccountsRoute data with the amount at offset 18. The forward policy
// additionally selects one of its two exact route-plan-prefix constraints from
// the manifest after the fresh instruction is built.
func validateInstalledJupiterHeader(action Action, instruction JupiterSwapInstruction) error {
	data, err := base64.StdEncoding.Strict().DecodeString(instruction.Data)
	if err != nil || len(data) < 8 || [8]byte(data[:8]) != jupiter.SharedAccountsRouteDiscriminator {
		return fmt.Errorf("installed Phase 1 swap policy does not authorize this Jupiter dialect")
	}
	if action != SwapUSDCToPrimeStep && action != SwapPrimeToUSDCStep &&
		action != SwapStableToCollateralStep && action != SwapCollateralToStableStep || len(data) != 37 {
		return fmt.Errorf("fresh Jupiter header does not match the installed policy offsets")
	}
	return nil
}

func validateJupiterQuoteForRoute(quote jupiter.Quote, action Action, amount uint64, lane string) (uint64, uint64, error) {
	source, destination, _, _, err := jupiterEdgeForRoute(action, lane)
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

func validateJupiterInstructionForRoute(value JupiterSwapInstruction, action Action, amount, out, minimum uint64, lane string) (compiledInstruction, error) {
	if catalogJupiterRoute(lane) && lane != autoAUTOPYUSD.Lane {
		return validateCatalogJupiterInstruction(value, action, amount, out, minimum, lane)
	}
	sourceMint, destinationMint, sourceATA, destinationATA, err := jupiterEdgeForRoute(action, lane)
	if err != nil {
		return compiledInstruction{}, err
	}
	if value.ProgramID != jupiter.ProgramID.String() || len(value.Accounts) == 0 || len(value.Accounts) > 64 {
		return compiledInstruction{}, fmt.Errorf("Jupiter program or account set drifted")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(value.Data)
	if err != nil || len(data) < 28 {
		return compiledInstruction{}, fmt.Errorf("Jupiter instruction data is malformed")
	}
	legacy, v2 := [8]byte(data[:8]) == jupiter.SharedAccountsRouteDiscriminator, [8]byte(data[:8]) == jupiter.SharedAccountsRouteV2Discriminator
	autoLane := lane == autoAUTOPYUSD.Lane
	if autoLane && !legacy {
		return compiledInstruction{}, fmt.Errorf("AUTO policy requires legacy sharedAccountsRoute")
	}
	if selectorLane(lane) && !legacy {
		return compiledInstruction{}, fmt.Errorf("basic policy requires legacy sharedAccountsRoute")
	}
	if !legacy && !v2 {
		return compiledInstruction{}, fmt.Errorf("unsupported Jupiter instruction dialect")
	}
	type boundary struct {
		index            int
		key              string
		signer, writable bool
	}
	boundaries := []boundary{}
	if selectorLane(lane) || autoLane {
		// Both the basic swap families and the combined AUTO policy pin the
		// platform-fee account to the Jupiter program.
		boundaries = append(boundaries, boundary{9, jupiter.ProgramID.String(), false, false})
	}
	slippageOffset, feeOffset := len(data)-3, len(data)-1
	if legacy {
		boundaries = append(boundaries, []boundary{{2, bridgeVault, true, false}, {3, sourceATA, false, true}, {6, destinationATA, false, true}, {7, sourceMint, false, false}, {8, destinationMint, false, false}, {0, bridgeTokenProgram, false, false}}...)
	} else {
		boundaries = []boundary{{1, bridgeVault, true, false}, {2, sourceATA, false, true}, {5, destinationATA, false, true}, {6, sourceMint, false, false}, {7, destinationMint, false, false}, {8, bridgeTokenProgram, false, false}, {9, bridgeTokenProgram, false, false}}
		slippageOffset, feeOffset = 25, 27
	}
	for _, expected := range boundaries {
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
		if input.Pubkey == previousBackyardVault || (input.IsSigner && input.Pubkey != bridgeVault) {
			return compiledInstruction{}, fmt.Errorf("Jupiter route crosses an unapproved authority")
		}
		key, err := decodeKey(input.Pubkey)
		if err != nil {
			return compiledInstruction{}, fmt.Errorf("invalid Jupiter account %d", index)
		}
		accounts[index] = accountMeta{key: key, signer: input.IsSigner, writable: input.IsWritable}
	}
	if len(data) < 19 || readU64(data[len(data)-19:]) != amount || readU64(data[len(data)-11:]) != out ||
		int(slippageOffset)+2 > len(data) || uint16(data[slippageOffset])|uint16(data[slippageOffset+1])<<8 > jupiterMaxSlippageBPS ||
		feeOffset >= len(data) || data[feeOffset] != 0 || minimum == 0 || minimum > out {
		return compiledInstruction{}, fmt.Errorf("Jupiter instruction economics drifted")
	}
	return compiledInstruction{program: publicKey(jupiter.ProgramID), accounts: accounts, data: data}, nil
}

// jupiterInstructionWireFloor computes the exact output floor a legacy
// sharedAccountsRoute wire can enforce: the on-wire quoted output scaled by
// the on-wire slippage in wide integer arithmetic. The legacy dialect carries
// no explicit minimum-out argument, so any larger JSON threshold is advisory
// and must never be retained as a funding guarantee. Other dialects have no
// such floor.
func jupiterInstructionWireFloor(instruction JupiterSwapInstruction) (uint64, error) {
	data, err := base64.StdEncoding.Strict().DecodeString(instruction.Data)
	if err != nil {
		return 0, fmt.Errorf("Jupiter instruction data is malformed")
	}
	route, err := jupiter.DecodeSharedAccountsRoute(data)
	if err != nil || route.SlippageBPS > jupiterMaxSlippageBPS {
		return 0, fmt.Errorf("Jupiter AUTO wire floor unavailable")
	}
	floor := new(big.Int).Mul(new(big.Int).SetUint64(route.QuotedOutAmount), big.NewInt(int64(10_000-route.SlippageBPS)))
	return floor.Div(floor, big.NewInt(10_000)).Uint64(), nil
}

// jupiterValidateAutoRetainedMinimum rejects a retained AUTO request whose
// minimum output exceeds what its own wire can enforce, so caller-forged
// evidence quoting an advisory JSON threshold cannot bypass the producer's
// conservative retention. Other lanes keep their established semantics.
func jupiterValidateAutoRetainedMinimum(request JupiterSwapRequest) error {
	if request.RouteLane != autoAUTOPYUSD.Lane {
		return nil
	}
	floor, err := jupiterInstructionWireFloor(request.Instruction)
	if err != nil || request.MinimumOutputRaw > floor {
		return fmt.Errorf("Jupiter AUTO minimum exceeds enforceable wire floor")
	}
	return nil
}

type SignedJupiterTransaction struct {
	message, signedWire                                   []byte
	messageSHA256, signedWireSHA256, transactionSignature string
	recentBlockhash                                       string
	lastValidBlockHeight                                  int64
}

func BuildAndSignJupiterTransaction(request JupiterSwapRequest, executor ed25519.PrivateKey) (SignedJupiterTransaction, error) {
	return buildAndSignJupiterTransactionForDelegate(request, executor, mustKey(bridgeDelegate))
}

func CompileJupiterMessage(request JupiterSwapRequest) ([]byte, error) {
	return compileJupiterMessageForDelegate(request, mustKey(bridgeDelegate))
}

// The production wrapper supplies the embedded reviewed manifest exactly once;
// the manifest-aware helper below retains that value into AUTO policy
// validation so a candidate binding never depends on a reload inside the
// compiler or on request-supplied authorization.
func compileJupiterMessageForDelegate(request JupiterSwapRequest, delegate publicKey) ([]byte, error) {
	manifest, err := loadEmbeddedRouteManifest()
	if err != nil {
		return nil, err
	}
	return manifest.compileJupiterMessage(request, delegate)
}

func (m RouteManifest) compileJupiterMessage(request JupiterSwapRequest, delegate publicKey) ([]byte, error) {
	if request.AmountRaw == 0 || request.LastValidBlockHeight <= 0 || !validSHA256(request.PolicyAccountDataSHA256) {
		return nil, fmt.Errorf("invalid Jupiter request")
	}
	blockhash, err := decodeKey(request.RecentBlockhash)
	if err != nil {
		return nil, err
	}
	inner, err := validateJupiterInstructionForRoute(request.Instruction, request.Action, request.AmountRaw, request.QuotedOutputRaw, request.MinimumOutputRaw, request.RouteLane)
	if err != nil {
		return nil, err
	}
	if err := jupiterValidateAutoRetainedMinimum(request); err != nil {
		return nil, err
	}
	if request.RouteLane == autoAUTOPYUSD.Lane {
		// Retention point: the reviewed AUTO binding supplied on m decides,
		// never the request and never the catalog.
		if err := m.requireAutoJupiterBinding(request); err != nil {
			return nil, err
		}
	} else if catalogJupiterRoute(request.RouteLane) {
		b, err := catalogJupiterBindingForRoute(request.Action, request.RouteLane)
		if err != nil || request.Policy != b.Policy || request.PolicyAccountDataSHA256 != b.PolicySHA256 || request.PolicyConstraintIndex != b.ConstraintIndex {
			return nil, fmt.Errorf("Jupiter policy does not match catalog edge")
		}
	} else if request.RouteLane == PhaseOneLaneID || request.RouteLane == SelectedRouteID || request.RouteLane == "OnRe/ONyc/USDC" {
		_, _, sourceCustody, destinationCustody, err := jupiterEdgeForRoute(request.Action, request.RouteLane)
		if err != nil {
			return nil, err
		}
		binding, index, err := resolveBasicSwapPolicy(sourceCustody, destinationCustody)
		if err != nil || request.Policy != binding.Policy || request.PolicyConstraintIndex != index {
			return nil, fmt.Errorf("Jupiter policy does not match the basic swap family")
		}
	} else if request.RouteLane != PhaseOneLaneID && request.RouteLane != SelectedRouteID && request.RouteLane != "OnRe/ONyc/USDC" && err == nil {
		err = validateInstalledJupiterHeader(request.Action, request.Instruction)
		if err != nil {
			return nil, err
		}
	}
	policy, err := decodeKey(request.Policy)
	if err != nil || policy == (publicKey{}) {
		return nil, fmt.Errorf("invalid Jupiter policy binding")
	}
	outer, err := wrapSquadsJupiterPolicy(policy, delegate, delegate, request.PolicyConstraintIndex, inner)
	if err != nil {
		return nil, err
	}
	if err := validateJupiterLookupIdentities(request); err != nil {
		return nil, err
	}
	var message []byte
	if request.RouteLane == autoAUTOPYUSD.Lane {
		// AUTO arrives here only through requireAutoJupiterBinding above, so the
		// reviewed lane carries the canonical ComputeBudget heap frame plus the
		// canonical compute-unit frame ahead of its already-validated policy
		// outer — the live default CU meter ran out mid-swap. The v0 path keeps
		// lookup snapshots and gains both instructions directly; the legacy path
		// goes through the closed AUTO swap resource wrapper because
		// compileLegacyMessage still admits exactly one payload instruction.
		// The initializer and Kamino legs keep the heap-only wrappers.
		if len(request.LookupTables) > 0 {
			message, err = compileV0Message(delegate, blockhash, withAutoSwapExecutionResources([]compiledInstruction{outer}), request.LookupTables)
		} else {
			message, err = compileAutoSwapResourceLegacyMessage(delegate, blockhash, outer)
		}
	} else if len(request.LookupTables) > 0 {
		message, err = compileV0Message(delegate, blockhash, []compiledInstruction{outer}, request.LookupTables)
	} else {
		message, err = compileLegacyMessage(delegate, blockhash, []compiledInstruction{outer})
	}
	if err != nil {
		return nil, err
	}
	return checkedUnsignedMessage(message)
}

func buildAndSignJupiterTransactionForDelegate(request JupiterSwapRequest, executor ed25519.PrivateKey, expectedDelegate publicKey) (SignedJupiterTransaction, error) {
	if len(executor) != ed25519.PrivateKeySize || request.AmountRaw == 0 || request.LastValidBlockHeight <= 0 || !validSHA256(request.PolicyAccountDataSHA256) {
		return SignedJupiterTransaction{}, fmt.Errorf("invalid Jupiter signing material")
	}
	feePayer := publicKeyFromBytes(executor.Public().(ed25519.PublicKey))
	if feePayer != expectedDelegate {
		return SignedJupiterTransaction{}, fmt.Errorf("executor is not the pinned Squads delegate")
	}
	message, err := compileJupiterMessageForDelegate(request, expectedDelegate)
	if err != nil {
		return SignedJupiterTransaction{}, err
	}
	signature := ed25519.Sign(executor, message)
	wire := append(encodeShortVec(1), signature...)
	wire = append(wire, message...)
	if len(wire) > solanaPacketBytes {
		return SignedJupiterTransaction{}, fmt.Errorf("Jupiter packet is %d bytes, exceeds %d", len(wire), solanaPacketBytes)
	}
	messageHash, wireHash := sha256.Sum256(message), sha256.Sum256(wire)
	return SignedJupiterTransaction{message: message, signedWire: wire, messageSHA256: hex.EncodeToString(messageHash[:]), signedWireSHA256: hex.EncodeToString(wireHash[:]), transactionSignature: encodeBase58(signature), recentBlockhash: request.RecentBlockhash, lastValidBlockHeight: request.LastValidBlockHeight}, nil
}

func wrapSquadsJupiterPolicy(policy, executor, expectedDelegate publicKey, constraintIndex byte, inner compiledInstruction) (compiledInstruction, error) {
	if executor != expectedDelegate || inner.program != publicKey(jupiter.ProgramID) {
		return compiledInstruction{}, fmt.Errorf("unrecognized Squads Jupiter policy or delegate")
	}
	return wrapSquadsPolicy(policy, executor, []byte{constraintIndex}, inner)
}

func (s SignedJupiterTransaction) BuildResult(simulationSlot int64) (BuildResult, error) {
	if simulationSlot <= 0 || len(s.signedWire) == 0 {
		return BuildResult{}, fmt.Errorf("exact signed Jupiter transaction was not simulated")
	}
	return BuildResult{MessageSHA256: s.messageSHA256, SignedWire: append([]byte(nil), s.signedWire...), SignedWireSHA256: s.signedWireSHA256, TransactionSignature: s.transactionSignature, RecentBlockhash: s.recentBlockhash, LastValidBlockHeight: s.lastValidBlockHeight, SimulationSlot: simulationSlot}, nil
}

func BuildSimulateAndPersistJupiter(ctx context.Context, database *Database, rpc *chain.Client, operationID string, evidence JupiterExecutionEvidence, credentials Credentials) error {
	if database == nil || rpc == nil || operationID == "" {
		return fmt.Errorf("Jupiter runtime dependencies are required")
	}
	if _, err := validateJupiterInstructionForRoute(evidence.Request.Instruction, evidence.Request.Action, evidence.Request.AmountRaw, evidence.Request.QuotedOutputRaw, evidence.Request.MinimumOutputRaw, evidence.Request.RouteLane); err != nil {
		return err
	}
	if err := jupiterValidateAutoRetainedMinimum(evidence.Request); err != nil {
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
	if err := validateBuildPrestate(ctx, rpc, evidence.Request, evidence.ExpectedEffects); err != nil {
		return err
	}
	logStage("jupiter_build_prestate", buildStart)
	signer, err := credentials.signer()
	if err != nil {
		return err
	}
	signed, err := BuildAndSignJupiterTransaction(evidence.Request, signer)
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
	logStage("jupiter_build_simulate", buildStart)
	if err := database.MarkSimulated(ctx, operationID, simulation); err != nil {
		return err
	}
	build, err := signed.BuildResult(simulation.Slot)
	if err != nil {
		return err
	}
	return database.PersistSigned(ctx, operationID, build)
}

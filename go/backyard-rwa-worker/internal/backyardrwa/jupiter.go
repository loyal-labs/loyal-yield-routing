package backyardrwa

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	jupiterV6Program       = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"
	jupiterAPIBase         = "https://lite-api.jup.ag/swap/v1"
	previousBackyardVault  = "AdwKLBQWKxNewpkjMFMz4NyKit7qXygGpjkqHBCWcriK"
	jupiterMaxSlippageBPS  = uint16(50)
	jupiterMaxRoutePlanLeg = 4
	jupiterResponseBytes   = 2 << 20
)

var (
	jupiterSharedAccountsRoute   = []byte{0xc1, 0x20, 0x9b, 0x33, 0x41, 0xd6, 0x9c, 0x81}
	jupiterSharedAccountsRouteV2 = []byte{209, 152, 83, 147, 124, 254, 216, 233}
)

type JupiterInstructionAccount struct {
	Pubkey     string `json:"pubkey"`
	IsSigner   bool   `json:"isSigner"`
	IsWritable bool   `json:"isWritable"`
}

type JupiterSwapInstruction struct {
	ProgramID string                      `json:"programId"`
	Accounts  []JupiterInstructionAccount `json:"accounts"`
	Data      string                      `json:"data"`
}

type JupiterQuote struct {
	InputMint            string            `json:"inputMint"`
	OutputMint           string            `json:"outputMint"`
	InAmount             string            `json:"inAmount"`
	OutAmount            string            `json:"outAmount"`
	OtherAmountThreshold string            `json:"otherAmountThreshold"`
	SwapMode             string            `json:"swapMode"`
	SlippageBPS          uint16            `json:"slippageBps"`
	PlatformFee          json.RawMessage   `json:"platformFee"`
	RoutePlan            []json.RawMessage `json:"routePlan"`
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
}

type JupiterExecutionEvidence struct {
	Request         JupiterSwapRequest
	ExpectedEffects ExpectedEffects
}

type jupiterClient struct {
	base string
	http *http.Client
}

func newJupiterClient(base string, client *http.Client) (*jupiterClient, error) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || client == nil {
		return nil, fmt.Errorf("invalid Jupiter client")
	}
	return &jupiterClient{base: string(bytes.TrimRight([]byte(base), "/")), http: client}, nil
}

func productionJupiterClient() *jupiterClient {
	client, _ := newJupiterClient(jupiterAPIBase, &http.Client{Timeout: 20 * time.Second})
	return client
}

func jupiterEdge(action Action) (sourceMint, destinationMint, sourceATA, destinationATA string, err error) {
	switch action {
	case SwapUSDCToPrimeStep:
		return bridgeUSDC, kaminoPrimeMint, bridgeSquadsATA, kaminoPrimeCustody, nil
	case SwapPrimeToUSDCStep:
		return kaminoPrimeMint, bridgeUSDC, kaminoPrimeCustody, bridgeSquadsATA, nil
	default:
		return "", "", "", "", fmt.Errorf("action %s is not an approved Jupiter edge", action)
	}
}

func (c *jupiterClient) FreshSwap(ctx context.Context, action Action, amount uint64) (JupiterQuote, JupiterSwapInstruction, error) {
	if c == nil || amount == 0 {
		return JupiterQuote{}, JupiterSwapInstruction{}, fmt.Errorf("invalid Jupiter quote request")
	}
	sourceMint, destinationMint, _, _, err := jupiterEdge(action)
	if err != nil {
		return JupiterQuote{}, JupiterSwapInstruction{}, err
	}
	query := url.Values{
		"inputMint": {sourceMint}, "outputMint": {destinationMint},
		"amount": {strconv.FormatUint(amount, 10)}, "slippageBps": {strconv.Itoa(int(jupiterMaxSlippageBPS))},
		"swapMode": {"ExactIn"}, "maxAccounts": {"32"},
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/quote?"+query.Encode(), nil)
	if err != nil {
		return JupiterQuote{}, JupiterSwapInstruction{}, err
	}
	var quote JupiterQuote
	var quoteRaw json.RawMessage
	if quoteRaw, err = c.doJSON(request); err != nil {
		return JupiterQuote{}, JupiterSwapInstruction{}, err
	}
	if err := json.Unmarshal(quoteRaw, &quote); err != nil {
		return JupiterQuote{}, JupiterSwapInstruction{}, fmt.Errorf("decode Jupiter quote: %w", err)
	}
	out, minimum, err := validateJupiterQuote(quote, action, amount)
	if err != nil {
		return JupiterQuote{}, JupiterSwapInstruction{}, err
	}
	body, err := json.Marshal(map[string]any{
		"userPublicKey": bridgeVault, "quoteResponse": json.RawMessage(quoteRaw),
		"wrapAndUnwrapSol": false, "useSharedAccounts": true, "dynamicComputeUnitLimit": false,
	})
	if err != nil {
		return JupiterQuote{}, JupiterSwapInstruction{}, err
	}
	request, err = http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/swap-instructions", bytes.NewReader(body))
	if err != nil {
		return JupiterQuote{}, JupiterSwapInstruction{}, err
	}
	request.Header.Set("content-type", "application/json")
	responseRaw, err := c.doJSON(request)
	if err != nil {
		return JupiterQuote{}, JupiterSwapInstruction{}, err
	}
	var response struct {
		SetupInstructions      []json.RawMessage      `json:"setupInstructions"`
		OtherInstructions      []json.RawMessage      `json:"otherInstructions"`
		CleanupInstruction     json.RawMessage        `json:"cleanupInstruction"`
		TokenLedgerInstruction json.RawMessage        `json:"tokenLedgerInstruction"`
		SwapInstruction        JupiterSwapInstruction `json:"swapInstruction"`
	}
	if err := json.Unmarshal(responseRaw, &response); err != nil {
		return JupiterQuote{}, JupiterSwapInstruction{}, fmt.Errorf("decode Jupiter instructions: %w", err)
	}
	if len(response.SetupInstructions) != 0 || len(response.OtherInstructions) != 0 || !jsonNull(response.CleanupInstruction) || !jsonNull(response.TokenLedgerInstruction) {
		return JupiterQuote{}, JupiterSwapInstruction{}, fmt.Errorf("Jupiter route requires unapproved companion instructions")
	}
	if _, err := validateJupiterInstruction(response.SwapInstruction, action, amount, out, minimum); err != nil {
		return JupiterQuote{}, JupiterSwapInstruction{}, err
	}
	if err := validateInstalledJupiterHeader(action, response.SwapInstruction); err != nil {
		return JupiterQuote{}, JupiterSwapInstruction{}, err
	}
	return quote, response.SwapInstruction, nil
}

// Both installed Phase 1 policies accept only legacy 37-byte
// SharedAccountsRoute data with the amount at offset 18. The forward policy
// additionally selects one of its two exact route-plan-prefix constraints from
// the manifest after the fresh instruction is built.
func validateInstalledJupiterHeader(action Action, instruction JupiterSwapInstruction) error {
	data, err := base64.StdEncoding.Strict().DecodeString(instruction.Data)
	if err != nil || len(data) < 8 || !bytes.Equal(data[:8], jupiterSharedAccountsRoute) {
		return fmt.Errorf("installed Phase 1 swap policy does not authorize this Jupiter dialect")
	}
	if action != SwapUSDCToPrimeStep && action != SwapPrimeToUSDCStep || len(data) != 37 {
		return fmt.Errorf("fresh Jupiter header does not match the installed Phase 1 policy offsets")
	}
	return nil
}

func (c *jupiterClient) doJSON(request *http.Request) (json.RawMessage, error) {
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, jupiterResponseBytes+1))
	if err != nil || len(data) > jupiterResponseBytes {
		return nil, fmt.Errorf("Jupiter response exceeds bounded body")
	}
	if response.StatusCode != http.StatusOK || !json.Valid(data) {
		return nil, fmt.Errorf("Jupiter returned invalid HTTP %d response", response.StatusCode)
	}
	return data, nil
}

func jsonNull(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func validateJupiterQuote(quote JupiterQuote, action Action, amount uint64) (uint64, uint64, error) {
	source, destination, _, _, err := jupiterEdge(action)
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
		len(quote.RoutePlan) == 0 || len(quote.RoutePlan) > jupiterMaxRoutePlanLeg || !jsonNull(quote.PlatformFee) {
		return 0, 0, fmt.Errorf("Jupiter quote identity or economics drifted")
	}
	return out, minimum, nil
}

func validateJupiterInstruction(value JupiterSwapInstruction, action Action, amount, out, minimum uint64) (compiledInstruction, error) {
	sourceMint, destinationMint, sourceATA, destinationATA, err := jupiterEdge(action)
	if err != nil {
		return compiledInstruction{}, err
	}
	if value.ProgramID != jupiterV6Program || len(value.Accounts) == 0 || len(value.Accounts) > 64 {
		return compiledInstruction{}, fmt.Errorf("Jupiter program or account set drifted")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(value.Data)
	if err != nil || len(data) < 28 {
		return compiledInstruction{}, fmt.Errorf("Jupiter instruction data is malformed")
	}
	legacy, v2 := bytes.Equal(data[:8], jupiterSharedAccountsRoute), bytes.Equal(data[:8], jupiterSharedAccountsRouteV2)
	if !legacy && !v2 {
		return compiledInstruction{}, fmt.Errorf("unsupported Jupiter instruction dialect")
	}
	type boundary struct {
		index            int
		key              string
		signer, writable bool
	}
	boundaries := []boundary{}
	slippageOffset, feeOffset := len(data)-3, len(data)-1
	if legacy {
		boundaries = []boundary{{2, bridgeVault, true, false}, {3, sourceATA, false, true}, {6, destinationATA, false, true}, {7, sourceMint, false, false}, {8, destinationMint, false, false}, {0, bridgeTokenProgram, false, false}}
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
	return compiledInstruction{program: mustKey(jupiterV6Program), accounts: accounts, data: data}, nil
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

func buildAndSignJupiterTransactionForDelegate(request JupiterSwapRequest, executor ed25519.PrivateKey, expectedDelegate publicKey) (SignedJupiterTransaction, error) {
	if len(executor) != ed25519.PrivateKeySize || request.AmountRaw == 0 || request.LastValidBlockHeight <= 0 || !validSHA256(request.PolicyAccountDataSHA256) {
		return SignedJupiterTransaction{}, fmt.Errorf("invalid Jupiter signing material")
	}
	feePayer := publicKeyFromBytes(executor.Public().(ed25519.PublicKey))
	if feePayer != expectedDelegate {
		return SignedJupiterTransaction{}, fmt.Errorf("executor is not the pinned Squads delegate")
	}
	blockhash, err := decodeKey(request.RecentBlockhash)
	if err != nil {
		return SignedJupiterTransaction{}, fmt.Errorf("invalid confirmed blockhash")
	}
	inner, err := validateJupiterInstruction(request.Instruction, request.Action, request.AmountRaw, request.QuotedOutputRaw, request.MinimumOutputRaw)
	if err != nil {
		return SignedJupiterTransaction{}, err
	}
	if err := validateInstalledJupiterHeader(request.Action, request.Instruction); err != nil {
		return SignedJupiterTransaction{}, err
	}
	policy, err := decodeKey(request.Policy)
	if err != nil || policy == (publicKey{}) {
		return SignedJupiterTransaction{}, fmt.Errorf("invalid Jupiter policy binding")
	}
	outer, err := wrapSquadsJupiterPolicy(policy, feePayer, expectedDelegate, request.PolicyConstraintIndex, inner)
	if err != nil {
		return SignedJupiterTransaction{}, err
	}
	message, err := compileLegacyMessage(feePayer, blockhash, []compiledInstruction{outer})
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
	if executor != expectedDelegate || inner.program != mustKey(jupiterV6Program) || len(inner.accounts) > math.MaxUint8 || len(inner.data) > math.MaxUint16 {
		return compiledInstruction{}, fmt.Errorf("unrecognized Squads Jupiter policy or delegate")
	}
	transactionAccounts := make([]accountMeta, 0, len(inner.accounts)+1)
	indexes := make([]byte, 0, len(inner.accounts))
	for _, account := range inner.accounts {
		indexes = append(indexes, pushOrMergeMeta(&transactionAccounts, account))
	}
	programIndex := pushOrMergeMeta(&transactionAccounts, accountMeta{key: inner.program})
	for index := range transactionAccounts {
		transactionAccounts[index].signer = false
	}
	compiled := []byte{1, programIndex, byte(len(indexes))}
	compiled = append(compiled, indexes...)
	compiled = appendU16(compiled, uint16(len(inner.data)))
	compiled = append(compiled, inner.data...)
	data := append([]byte(nil), squadsExecuteSyncDiscriminator...)
	data = append(data, 0, 1, 1, 1, 1)
	data = appendU32(data, 1)
	data = append(data, constraintIndex, 1, 0)
	data = appendU32(data, uint32(len(compiled)))
	data = append(data, compiled...)
	accounts := []accountMeta{{key: policy, writable: true}, {key: mustKey(bridgeSquadsProgram)}, {key: executor, signer: true}}
	accounts = append(accounts, transactionAccounts...)
	return compiledInstruction{program: mustKey(bridgeSquadsProgram), accounts: accounts, data: data}, nil
}

func (s SignedJupiterTransaction) BuildResult(simulationSlot int64) (BuildResult, error) {
	if simulationSlot <= 0 || len(s.signedWire) == 0 {
		return BuildResult{}, fmt.Errorf("exact signed Jupiter transaction was not simulated")
	}
	return BuildResult{MessageSHA256: s.messageSHA256, SignedWire: append([]byte(nil), s.signedWire...), SignedWireSHA256: s.signedWireSHA256, TransactionSignature: s.transactionSignature, RecentBlockhash: s.recentBlockhash, LastValidBlockHeight: s.lastValidBlockHeight, SimulationSlot: simulationSlot}, nil
}

func BuildSimulateAndPersistJupiter(ctx context.Context, database *Database, rpc *RPCClient, operationID string, evidence JupiterExecutionEvidence) error {
	if database == nil || rpc == nil || operationID == "" {
		return fmt.Errorf("Jupiter runtime dependencies are required")
	}
	if _, err := validateJupiterInstruction(evidence.Request.Instruction, evidence.Request.Action, evidence.Request.AmountRaw, evidence.Request.QuotedOutputRaw, evidence.Request.MinimumOutputRaw); err != nil {
		return err
	}
	effects, err := jsonMarshalExpectedEffects(evidence.ExpectedEffects)
	if err != nil {
		return err
	}
	if _, err := DecodeExpectedEffects(effects); err != nil {
		return err
	}
	signer, err := loadPinnedPolicySigner()
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
	simulation, err := rpc.SimulateSignedTransaction(ctx, signed.signedWire)
	if err != nil {
		return err
	}
	if err := database.MarkSimulated(ctx, operationID, simulation); err != nil {
		return err
	}
	build, err := signed.BuildResult(simulation.Slot)
	if err != nil {
		return err
	}
	return database.PersistSigned(ctx, operationID, build)
}

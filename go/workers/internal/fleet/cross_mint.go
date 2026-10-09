package fleet

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"math/bits"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/squadspolicy"
	solana "github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
)

const (
	jupiterProgram     = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4"
	jupiterEvent       = "D8cy77BBepLMngZx6ZukaTff5hCt1HrWyKk3Hnd9oitf"
	alphaQProgram      = "ALPHAQmeA7bjrVuccPsYPiCvsi428SNwte66Srvs4pHA"
	tokenProgram       = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	token2022Program   = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	computeProgram     = "ComputeBudget111111111111111111111111111111"
	instructionsSysvar = "Sysvar1nstructions1111111111111111111111111"
	maxJupiterResponse = 2_000_000
)

var (
	jupiterRouteV2Discriminator  = []byte{187, 100, 250, 204, 49, 196, 175, 20}
	jupiterSharedV2Discriminator = []byte{209, 152, 83, 147, 124, 254, 216, 233}
)

type JupiterBuildClient struct {
	url, apiKey string
	client      *http.Client
}

func NewJupiterBuildClient(rawURL, apiKey string) (*JupiterBuildClient, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, errors.New("Jupiter build URL must be absolute HTTPS")
	}
	httpClient := &http.Client{Timeout: 15 * time.Second}
	httpClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" || req.URL.Host != u.Host {
			return errors.New("Jupiter build redirect escaped configured HTTPS origin")
		}
		if len(via) >= 3 {
			return errors.New("too many Jupiter build redirects")
		}
		return nil
	}
	return &JupiterBuildClient{url: rawURL, apiKey: strings.TrimSpace(apiKey), client: httpClient}, nil
}

func (c *JupiterBuildClient) fetch(ctx context.Context, inputMint, outputMint string, amount uint64, taker string, slippage uint16) ([]byte, error) {
	u, _ := url.Parse(c.url)
	q := u.Query()
	q.Set("inputMint", inputMint)
	q.Set("outputMint", outputMint)
	q.Set("amount", strconv.FormatUint(amount, 10))
	q.Set("taker", taker)
	q.Set("maxAccounts", "48")
	q.Set("slippageBps", strconv.Itoa(int(slippage)))
	q.Set("onlyDirectRoutes", "true")
	q.Set("dexes", "AlphaQ")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("x-api-key", c.apiKey)
	}
	res, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("Jupiter /build returned HTTP %s", res.Status)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxJupiterResponse+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxJupiterResponse {
		return nil, errors.New("Jupiter /build response exceeds 2 MB")
	}
	return body, nil
}

type rawJupiterBuild struct {
	InputMint                     string                  `json:"inputMint"`
	OutputMint                    string                  `json:"outputMint"`
	InAmount                      string                  `json:"inAmount"`
	OutAmount                     string                  `json:"outAmount"`
	OtherAmountThreshold          string                  `json:"otherAmountThreshold"`
	SwapMode                      string                  `json:"swapMode"`
	SlippageBPS                   uint16                  `json:"slippageBps"`
	PlatformFeeBPS                *uint16                 `json:"platformFeeBps"`
	RoutePlan                     []rawJupiterRoute       `json:"routePlan"`
	ComputeBudgetInstructions     []rawJupiterInstruction `json:"computeBudgetInstructions"`
	SetupInstructions             []rawJupiterInstruction `json:"setupInstructions"`
	SwapInstruction               rawJupiterInstruction   `json:"swapInstruction"`
	CleanupInstruction            *rawJupiterInstruction  `json:"cleanupInstruction"`
	OtherInstructions             []rawJupiterInstruction `json:"otherInstructions"`
	TipInstruction                *rawJupiterInstruction  `json:"tipInstruction"`
	AddressesByLookupTableAddress map[string][]string     `json:"addressesByLookupTableAddress"`
	BlockhashWithMetadata         rawJupiterBlockhash     `json:"blockhashWithMetadata"`
}
type rawJupiterRoute struct {
	SwapInfo rawJupiterSwapInfo `json:"swapInfo"`
	Percent  *float64           `json:"percent"`
	BPS      uint16             `json:"bps"`
}
type rawJupiterSwapInfo struct {
	AMMKey     string `json:"ammKey"`
	Label      string `json:"label"`
	InputMint  string `json:"inputMint"`
	OutputMint string `json:"outputMint"`
	InAmount   string `json:"inAmount"`
	OutAmount  string `json:"outAmount"`
}
type rawJupiterInstruction struct {
	ProgramID string              `json:"programId"`
	Accounts  []rawJupiterAccount `json:"accounts"`
	Data      string              `json:"data"`
}
type rawJupiterAccount struct {
	Pubkey     string `json:"pubkey"`
	IsSigner   bool   `json:"isSigner"`
	IsWritable bool   `json:"isWritable"`
}
type rawJupiterBlockhash struct {
	Blockhash            []byte          `json:"blockhash"`
	LastValidBlockHeight uint64          `json:"lastValidBlockHeight"`
	FetchedAt            json.RawMessage `json:"fetchedAt"`
}

type validatedJupiterBuild struct {
	Swap                               RouteInstruction
	Dialect                            string
	ConstraintIndex                    uint8
	Input, QuotedOutput, MinimumOutput uint64
	Slippage                           uint16
	UnitPrice                          uint64
	RouteSteps                         int
	Tables                             []LookupTable
	Blockhash                          string
	LastValidBlockHeight               uint64
	ObservedBlockHeight                uint64
	ResponseSHA256                     string
}

type crossMintPlan struct {
	Kind       string                  `json:"kind"`
	SourceMint string                  `json:"source_liquidity_mint"`
	TargetMint string                  `json:"target_liquidity_mint"`
	Amount     uint64                  `json:"amount_raw"`
	ValueLoss  uint16                  `json:"cross_mint_maximum_value_loss_bps"`
	Bindings   CrossMintPolicyBindings `json:"policy_bindings"`
}

func stableTokenProgram(mint string) (string, bool) {
	switch mint {
	case CashMint, USDGMint, PYUSDMint:
		return token2022Program, true
	case USDCMint, USDTMint, USDSMint:
		return tokenProgram, true
	}
	return "", false
}

func parseAmount(value string) (uint64, error) {
	if value == "" {
		return 0, errors.New("invalid amount")
	}
	return strconv.ParseUint(value, 10, 64)
}

func rawInstruction(raw rawJupiterInstruction, label string) (RouteInstruction, error) {
	if _, err := decodePublicKey(raw.ProgramID); err != nil {
		return RouteInstruction{}, fmt.Errorf("%s program: %w", label, err)
	}
	data, err := base64.StdEncoding.Strict().DecodeString(raw.Data)
	if err != nil {
		return RouteInstruction{}, fmt.Errorf("%s data: %w", label, err)
	}
	ix := RouteInstruction{Step: label, Program: raw.ProgramID, Data: data}
	for _, account := range raw.Accounts {
		if _, err := decodePublicKey(account.Pubkey); err != nil {
			return RouteInstruction{}, err
		}
		ix.Accounts = append(ix.Accounts, InstructionAccount{Address: account.Pubkey, Signer: account.IsSigner, Writable: account.IsWritable})
	}
	return ix, nil
}

func validateJupiterEnvelope(body []byte, expected crossMintPlan, vault string, maximumSlippage uint16, tables []LookupTable) (validatedJupiterBuild, error) {
	var raw rawJupiterBuild
	if err := json.Unmarshal(body, &raw); err != nil {
		return validatedJupiterBuild{}, fmt.Errorf("invalid Jupiter /build JSON: %w", err)
	}
	input, err := parseAmount(raw.InAmount)
	if err != nil {
		return validatedJupiterBuild{}, err
	}
	quoted, err := parseAmount(raw.OutAmount)
	if err != nil {
		return validatedJupiterBuild{}, err
	}
	minimum, err := parseAmount(raw.OtherAmountThreshold)
	if err != nil {
		return validatedJupiterBuild{}, err
	}
	if raw.InputMint != expected.SourceMint || raw.OutputMint != expected.TargetMint || raw.SwapMode != "ExactIn" || input != expected.Amount || quoted == 0 || minimum == 0 || minimum > quoted || raw.SlippageBPS > maximumSlippage || minimum != thresholdFor(quoted, raw.SlippageBPS) || raw.PlatformFeeBPS != nil && *raw.PlatformFeeBPS != 0 {
		return validatedJupiterBuild{}, errors.New("Jupiter quote envelope violates ExactIn contract")
	}
	if raw.CleanupInstruction != nil || raw.TipInstruction != nil || len(raw.OtherInstructions) != 0 {
		return validatedJupiterBuild{}, errors.New("Jupiter cleanup, tip, and other instructions are disabled")
	}
	if len(raw.ComputeBudgetInstructions) != 1 {
		return validatedJupiterBuild{}, errors.New("Jupiter must return exactly one compute-unit-price instruction")
	}
	budget, err := rawInstruction(raw.ComputeBudgetInstructions[0], "jupiter_compute_price")
	if err != nil {
		return validatedJupiterBuild{}, err
	}
	if budget.Program != computeProgram || len(budget.Accounts) != 0 || len(budget.Data) != 9 || budget.Data[0] != 3 {
		return validatedJupiterBuild{}, errors.New("invalid Jupiter compute-unit-price instruction")
	}
	unitPrice := binary.LittleEndian.Uint64(budget.Data[1:])
	if unitPrice > 10_000_000 {
		return validatedJupiterBuild{}, errors.New("Jupiter compute-unit price exceeds limit")
	}
	steps, err := validateJupiterRoutePlan(raw.RoutePlan, expected.SourceMint, expected.TargetMint, input, quoted)
	if err != nil {
		return validatedJupiterBuild{}, err
	}
	inputATA, err := deriveATA(vault, expected.SourceMint, mustStableProgram(expected.SourceMint))
	if err != nil {
		return validatedJupiterBuild{}, err
	}
	outputATA, err := deriveATA(vault, expected.TargetMint, mustStableProgram(expected.TargetMint))
	if err != nil {
		return validatedJupiterBuild{}, err
	}
	swap, err := rawInstruction(raw.SwapInstruction, "jupiter_exact_in")
	if err != nil {
		return validatedJupiterBuild{}, err
	}
	dialect, err := validateJupiterSwap(swap, raw, steps, vault, inputATA, outputATA)
	if err != nil {
		return validatedJupiterBuild{}, err
	}
	constraintIndex := uint8(0)
	if dialect == "shared_accounts_route_v2" {
		constraintIndex = 1
	}
	if len(raw.SetupInstructions) > 5 {
		return validatedJupiterBuild{}, errors.New("too many Jupiter setup instructions")
	}
	setups := make([]RouteInstruction, 0, len(raw.SetupInstructions))
	routeMints := map[string]bool{expected.SourceMint: true, expected.TargetMint: true}
	for _, step := range steps {
		routeMints[step.SwapInfo.InputMint] = true
		routeMints[step.SwapInfo.OutputMint] = true
	}
	setupATAs := map[string]bool{}
	for _, setupRaw := range raw.SetupInstructions {
		setup, e := rawInstruction(setupRaw, "jupiter_setup")
		if e != nil || e == nil && (!validateIdempotentATA(setup, vault) || !routeMints[setup.Accounts[3].Address] || setupATAs[setup.Accounts[1].Address]) {
			return validatedJupiterBuild{}, errors.New("unsupported Jupiter setup instruction")
		}
		setupATAs[setup.Accounts[1].Address] = true
		setups = append(setups, setup)
	}
	if len(tables) != len(raw.AddressesByLookupTableAddress) {
		return validatedJupiterBuild{}, errors.New("Jupiter lookup table count mismatch")
	}
	byAddress := map[string]LookupTable{}
	for _, table := range tables {
		byAddress[table.Address] = table
	}
	for address, listed := range raw.AddressesByLookupTableAddress {
		table, ok := byAddress[address]
		if !ok || !equalStrings(table.Addresses, listed) {
			return validatedJupiterBuild{}, errors.New("Jupiter lookup table membership mismatch")
		}
	}
	if len(raw.BlockhashWithMetadata.Blockhash) != 32 || raw.BlockhashWithMetadata.LastValidBlockHeight == 0 || !validJupiterFetchedAt(raw.BlockhashWithMetadata.FetchedAt) || bytes.Equal(raw.BlockhashWithMetadata.Blockhash, make([]byte, 32)) {
		return validatedJupiterBuild{}, errors.New("invalid Jupiter blockhash metadata")
	}
	all := append(computeBudgetInstructions(uint32(defaultComputeLimit), unitPrice), setups...)
	all = append(all, swap)
	unique := map[string]bool{vault: true}
	dataBytes := 0
	for _, ix := range all {
		unique[ix.Program] = true
		dataBytes += len(ix.Data)
		for _, a := range ix.Accounts {
			unique[a.Address] = true
		}
	}
	if len(all) > 8 || len(unique) > 64 || dataBytes > 1024 {
		return validatedJupiterBuild{}, errors.New("Jupiter build exceeds instruction, account, or data limits")
	}
	structure, _, e := compileV0Transaction(vault, encodeBase58(raw.BlockhashWithMetadata.Blockhash), all, tables, 1, defaultComputeLimit)
	if e != nil || structure.PacketBytes > SolanaPacketLimit {
		return validatedJupiterBuild{}, errors.New("Jupiter build exceeds packet limit")
	}
	h := sha256.Sum256(body)
	return validatedJupiterBuild{Swap: swap, Dialect: dialect, ConstraintIndex: constraintIndex, Input: input, QuotedOutput: quoted, MinimumOutput: minimum, Slippage: raw.SlippageBPS, UnitPrice: unitPrice, RouteSteps: len(steps), Tables: tables, Blockhash: encodeBase58(raw.BlockhashWithMetadata.Blockhash), LastValidBlockHeight: raw.BlockhashWithMetadata.LastValidBlockHeight, ResponseSHA256: hex.EncodeToString(h[:])}, nil
}

func validJupiterFetchedAt(raw json.RawMessage) bool {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text) != ""
	}
	var unix struct {
		Seconds uint64 `json:"secs_since_epoch"`
		Nanos   uint32 `json:"nanos_since_epoch"`
	}
	return json.Unmarshal(raw, &unix) == nil && unix.Seconds > 0 && unix.Nanos < 1_000_000_000
}

func mustStableProgram(mint string) string { program, _ := stableTokenProgram(mint); return program }
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validateJupiterRoutePlan(routes []rawJupiterRoute, inputMint, outputMint string, input, quoted uint64) ([]rawJupiterRoute, error) {
	if len(routes) < 1 || len(routes) > 2 {
		return nil, errors.New("Jupiter route must be one or two AlphaQ steps")
	}
	mint, amount := inputMint, input
	seenMint := map[string]bool{inputMint: true}
	seenAMM := map[string]bool{}
	for _, route := range routes {
		inAmount, e1 := parseAmount(route.SwapInfo.InAmount)
		outAmount, e2 := parseAmount(route.SwapInfo.OutAmount)
		if e1 != nil || e2 != nil || route.Percent == nil || *route.Percent != 100 || route.BPS != 10_000 || route.SwapInfo.Label != "AlphaQ" || route.SwapInfo.InputMint != mint || inAmount != amount || outAmount == 0 || !isEarnStableMint(route.SwapInfo.OutputMint) || route.SwapInfo.OutputMint == mint || seenMint[route.SwapInfo.OutputMint] || seenAMM[route.SwapInfo.AMMKey] {
			return nil, errors.New("invalid direct AlphaQ route plan")
		}
		if _, e := decodePublicKey(route.SwapInfo.AMMKey); e != nil {
			return nil, e
		}
		seenAMM[route.SwapInfo.AMMKey] = true
		seenMint[route.SwapInfo.OutputMint] = true
		mint = route.SwapInfo.OutputMint
		amount = outAmount
	}
	if mint != outputMint || amount != quoted {
		return nil, errors.New("Jupiter route plan output mismatch")
	}
	return routes, nil
}

func validateJupiterSwap(ix RouteInstruction, raw rawJupiterBuild, routes []rawJupiterRoute, vault, inputATA, outputATA string) (string, error) {
	if ix.Program != jupiterProgram {
		return "", errors.New("unexpected Jupiter swap program")
	}
	dialect, amountOffset, slipOffset, feeOffset, core := "", 0, 0, 0, 0
	if len(ix.Data) >= 8 && bytes.Equal(ix.Data[:8], jupiterRouteV2Discriminator) {
		dialect = "route_v2"
		amountOffset = 8
		slipOffset = 24
		feeOffset = 26
		core = 10
	} else if len(ix.Data) >= 8 && bytes.Equal(ix.Data[:8], jupiterSharedV2Discriminator) {
		dialect = "shared_accounts_route_v2"
		amountOffset = 9
		slipOffset = 25
		feeOffset = 27
		core = 12
	} else {
		return "", errors.New("unsupported Jupiter V2 dialect")
	}
	first := 9
	if len(routes) == 2 {
		first += 6
	}
	expectedLen := feeOffset + 1 + 4 + first
	if len(ix.Data) != expectedLen || binary.LittleEndian.Uint64(ix.Data[amountOffset:amountOffset+8]) != rawMustAmount(raw.InAmount) || binary.LittleEndian.Uint64(ix.Data[amountOffset+8:amountOffset+16]) != rawMustAmount(raw.OutAmount) || binary.LittleEndian.Uint16(ix.Data[slipOffset:slipOffset+2]) != raw.SlippageBPS || ix.Data[feeOffset] != 0 {
		return "", errors.New("invalid Jupiter ExactIn instruction data")
	}
	offset := feeOffset + 1
	if int(binary.BigEndian.Uint32(ix.Data[offset:offset+4])) != len(routes) {
		return "", errors.New("Jupiter route instruction step mismatch")
	}
	offset += 4
	directions := make([]bool, len(routes))
	for i, route := range routes {
		variant := uint32(ix.Data[offset])
		directionOffset := offset + 1
		step := 6
		if i == 0 {
			variant = binary.BigEndian.Uint32(ix.Data[offset : offset+4])
			directionOffset = offset + 4
			step = 9
		}
		if variant != 104 || ix.Data[directionOffset] > 1 || binary.LittleEndian.Uint16(ix.Data[directionOffset+1:directionOffset+3]) != route.BPS || ix.Data[directionOffset+3] != byte(i) || ix.Data[directionOffset+4] != byte(i+1) {
			return "", errors.New("Jupiter encoded AlphaQ route mismatch")
		}
		directions[i] = ix.Data[directionOffset] == 1
		offset += step
	}
	if len(ix.Accounts) < core {
		return "", errors.New("truncated Jupiter accounts")
	}
	inputProgram, _ := stableTokenProgram(raw.InputMint)
	outputProgram, _ := stableTokenProgram(raw.OutputMint)
	if dialect == "route_v2" {
		expected := []InstructionAccount{{vault, true, false}, {inputATA, false, true}, {outputATA, false, true}, {raw.InputMint, false, false}, {raw.OutputMint, false, false}, {inputProgram, false, false}, {outputProgram, false, false}, {jupiterProgram, false, false}, {jupiterEvent, false, false}, {jupiterProgram, false, false}}
		if !equalMetas(ix.Accounts[:core], expected) {
			return "", errors.New("invalid Jupiter route-v2 core accounts")
		}
	} else {
		fixed := map[int]InstructionAccount{1: {vault, true, false}, 2: {inputATA, false, true}, 5: {outputATA, false, true}, 6: {raw.InputMint, false, false}, 7: {raw.OutputMint, false, false}, 8: {inputProgram, false, false}, 9: {outputProgram, false, false}, 10: {jupiterEvent, false, false}, 11: {jupiterProgram, false, false}}
		for i, want := range fixed {
			if ix.Accounts[i] != want {
				return "", errors.New("invalid Jupiter shared-v2 core accounts")
			}
		}
		if ix.Accounts[0].Address == "11111111111111111111111111111111" || ix.Accounts[0].Signer || ix.Accounts[0].Writable || ix.Accounts[3].Address == "11111111111111111111111111111111" || ix.Accounts[4].Address == "11111111111111111111111111111111" || ix.Accounts[3].Signer || !ix.Accounts[3].Writable || ix.Accounts[4].Signer || !ix.Accounts[4].Writable {
			return "", errors.New("invalid Jupiter shared authority or route accounts")
		}
	}
	expectedCount := core
	for _, route := range routes {
		inProgram, _ := stableTokenProgram(route.SwapInfo.InputMint)
		outProgram, _ := stableTokenProgram(route.SwapInfo.OutputMint)
		if inProgram == token2022Program && outProgram == token2022Program {
			return "", errors.New("AlphaQ Token-2022 to Token-2022 route is unsupported")
		}
		expectedCount += 14
		if inProgram == token2022Program || outProgram == token2022Program {
			expectedCount += 2
		}
	}
	if len(ix.Accounts) != expectedCount {
		return "", errors.New("invalid AlphaQ residual account count")
	}
	protected := map[string]bool{vault: true, inputATA: true, outputATA: true, raw.InputMint: true, raw.OutputMint: true, tokenProgram: true, token2022Program: true, "11111111111111111111111111111111": true, "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL": true, computeProgram: true, jupiterProgram: true, jupiterEvent: true, alphaQProgram: true, instructionsSysvar: true}
	cursor := core
	source := inputATA
	if dialect == "shared_accounts_route_v2" {
		source = ix.Accounts[3].Address
	}
	for i, route := range routes {
		inProgram, _ := stableTokenProgram(route.SwapInfo.InputMint)
		outProgram, _ := stableTokenProgram(route.SwapInfo.OutputMint)
		segLen := 14
		var token2022Mint string
		if inProgram == token2022Program {
			segLen = 16
			token2022Mint = route.SwapInfo.InputMint
		}
		if outProgram == token2022Program {
			segLen = 16
			token2022Mint = route.SwapInfo.OutputMint
		}
		seg := ix.Accounts[cursor : cursor+segLen]
		fixed := map[int]InstructionAccount{0: {alphaQProgram, false, false}, 2: {route.SwapInfo.AMMKey, false, false}, 11: {tokenProgram, false, false}, 12: {instructionsSysvar, false, false}, segLen - 1: {jupiterProgram, false, false}}
		for n, want := range fixed {
			if seg[n] != want {
				return "", errors.New("invalid AlphaQ fixed residual account")
			}
		}
		src, dst := seg[5], seg[4]
		if directions[i] {
			src, dst = seg[4], seg[5]
		}
		expectedAuthority := vault
		if dialect == "shared_accounts_route_v2" && src.Address != inputATA {
			expectedAuthority = ix.Accounts[0].Address
		}
		if seg[1] != (InstructionAccount{expectedAuthority, false, false}) || src.Address != source || src.Signer || !src.Writable || dst.Address == "11111111111111111111111111111111" || dst.Address == source || dst.Signer || !dst.Writable {
			return "", errors.New("invalid AlphaQ route custody chain")
		}
		if i == len(routes)-1 {
			final := outputATA
			if dialect == "shared_accounts_route_v2" {
				final = ix.Accounts[4].Address
			}
			if dst.Address != final {
				return "", errors.New("AlphaQ destination mismatch")
			}
		}
		if protected[seg[3].Address] || protected[seg[6].Address] || protected[seg[7].Address] || seg[3].Address == seg[6].Address || seg[3].Address == seg[7].Address || seg[3].Signer || !seg[3].Writable || seg[6].Signer || !seg[6].Writable || seg[7].Signer || !seg[7].Writable || seg[6].Address == seg[7].Address || seg[8].Address != seg[6].Address || seg[9].Address != seg[7].Address || seg[10] != (InstructionAccount{seg[7].Address, false, true}) {
			return "", errors.New("invalid AlphaQ pool accounts")
		}
		if segLen == 16 && (seg[13] != (InstructionAccount{token2022Mint, false, false}) || seg[14] != (InstructionAccount{token2022Program, false, false})) {
			return "", errors.New("invalid AlphaQ Token-2022 residual accounts")
		}
		source = dst.Address
		cursor += segLen
	}
	return dialect, nil
}

func rawMustAmount(v string) uint64 { n, _ := parseAmount(v); return n }
func equalMetas(a, b []InstructionAccount) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func validateIdempotentATA(ix RouteInstruction, vault string) bool {
	if ix.Program != "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL" || len(ix.Accounts) != 6 || !bytes.Equal(ix.Data, []byte{1}) || ix.Accounts[0] != (InstructionAccount{vault, true, true}) || ix.Accounts[2] != (InstructionAccount{vault, false, false}) || ix.Accounts[4] != (InstructionAccount{"11111111111111111111111111111111", false, false}) {
		return false
	}
	mint := ix.Accounts[3].Address
	program, ok := stableTokenProgram(mint)
	if !ok || ix.Accounts[5] != (InstructionAccount{program, false, false}) {
		return false
	}
	ata, err := deriveATA(vault, mint, program)
	return err == nil && ix.Accounts[1] == (InstructionAccount{ata, false, true})
}

func deriveATA(owner, mint, program string) (string, error) {
	ownerKey, err := solana.PublicKeyFromBase58(owner)
	if err != nil {
		return "", err
	}
	mintKey, err := solana.PublicKeyFromBase58(mint)
	if err != nil {
		return "", err
	}
	programKey, err := solana.PublicKeyFromBase58(program)
	if err != nil {
		return "", err
	}
	ataProgram, _ := solana.PublicKeyFromBase58("ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL")
	address, _, err := solana.FindProgramAddress([][]byte{ownerKey[:], programKey[:], mintKey[:]}, ataProgram)
	if err != nil {
		return "", err
	}
	return address.String(), nil
}

func minimumProfitableCrossMintOutput(planJSON json.RawMessage, sourceAmount uint64, sourceAPY, targetAPY int64) (uint64, error) {
	var plan struct {
		Holding int64 `json:"holding_horizon_seconds"`
		Costs   struct {
			Kind    string `json:"kind"`
			Swap    int64  `json:"jupiter_swap_usd_micros"`
			Deposit int64  `json:"deposit_usd_micros"`
		} `json:"estimated_execution_costs"`
	}
	if json.Unmarshal(planJSON, &plan) != nil || plan.Holding <= 0 || plan.Costs.Kind != "cross_mint_jupiter" || plan.Costs.Swap < 0 || plan.Costs.Deposit < 0 {
		return 0, errors.New("cross-mint plan has invalid economics")
	}
	const year int64 = 365 * 24 * 60 * 60
	future := func(principal uint64, apy int64) (int64, bool) {
		if principal > uint64(math.MaxInt64) {
			return 0, false
		}
		if apy < 0 {
			return 0, false
		}
		gain := new(big.Int).Mul(new(big.Int).SetUint64(principal), big.NewInt(apy))
		gain.Mul(gain, big.NewInt(plan.Holding))
		gain.Quo(gain, big.NewInt(year*10_000))
		gain.Add(gain, new(big.Int).SetUint64(principal))
		if !gain.IsInt64() {
			return 0, false
		}
		return gain.Int64(), true
	}
	source, ok := future(sourceAmount, sourceAPY)
	if !ok {
		return 0, errors.New("source recovery economics overflow")
	}
	source -= plan.Costs.Deposit
	beats := func(amount uint64) bool {
		target, ok := future(amount, targetAPY)
		if !ok {
			return false
		}
		afterSwap := target - plan.Costs.Swap
		if afterSwap < math.MinInt64+plan.Costs.Deposit {
			return false
		}
		return afterSwap-plan.Costs.Deposit > source
	}
	if !beats(sourceAmount) {
		return 0, errors.New("swap economics do not beat source recovery at zero value loss")
	}
	low, high := uint64(1), sourceAmount
	for low < high {
		mid := low + (high-low)/2
		if beats(mid) {
			high = mid
		} else {
			low = mid + 1
		}
	}
	return low, nil
}

func thresholdFor(amount uint64, bps uint16) uint64 {
	if bps > 10_000 {
		return 0
	}
	hi, lo := bits.Mul64(amount, uint64(10_000-bps))
	q, rem := bits.Div64(hi, lo, 10_000)
	if rem > 0 {
		q++
	}
	return q
}

func minimumEconomicOutput(amount uint64, bps uint16) (uint64, error) {
	if amount == 0 || bps == 0 || bps > 1000 {
		return 0, errors.New("invalid value-loss cap or amount")
	}
	hi, lo := bits.Mul64(amount, uint64(10_000-bps))
	q, rem := bits.Div64(hi, lo, 10_000)
	if rem > 0 {
		q++
	}
	return q, nil
}

func decodeLookupTable(account *chain.Account, observedSlot int64) (LookupTable, error) {
	if account == nil {
		return LookupTable{}, errors.New("lookup table is absent")
	}
	if account.Owner.String() != altProgram || account.Executable || account.Lamports == 0 || len(account.Data) < 56 || (len(account.Data)-56)%32 != 0 || binary.LittleEndian.Uint32(account.Data[:4]) != 1 || binary.LittleEndian.Uint64(account.Data[4:12]) != ^uint64(0) || observedSlot <= 0 {
		return LookupTable{}, fmt.Errorf("lookup table %s is invalid or deactivated", account.Key)
	}
	lastExtended := binary.LittleEndian.Uint64(account.Data[12:20])
	if lastExtended >= uint64(observedSlot) && len(account.Data) > 56 {
		return LookupTable{}, fmt.Errorf("lookup table %s is not warmed at finalized slot", account.Key)
	}
	t := LookupTable{Address: account.Key.String(), Active: true, UsableAfterSlot: int64(lastExtended) + 1, LastVerifiedSlot: observedSlot}
	for i := 56; i < len(account.Data); i += 32 {
		t.Addresses = append(t.Addresses, encodeBase58(account.Data[i:i+32]))
	}
	return t, nil
}

func (r *Revalidator) loadFinalizedJupiterTables(ctx context.Context, listed map[string][]string, minimum int64) ([]LookupTable, error) {
	if len(listed) > 4 {
		return nil, errors.New("too many Jupiter lookup tables")
	}
	addresses := make([]string, 0, len(listed))
	total := 0
	for a, v := range listed {
		total += len(v)
		if len(v) == 0 || len(v) > 256 || total > 1024 {
			return nil, errors.New("invalid Jupiter lookup table declaration")
		}
		addresses = append(addresses, a)
	}
	sort.Strings(addresses)
	if len(addresses) == 0 {
		return nil, nil
	}
	observedSlot, accounts, err := ReadAccounts(ctx, r.rpc, addresses, rpc.CommitmentFinalized, minimum)
	if err != nil {
		return nil, err
	}
	tables := make([]LookupTable, len(accounts))
	for i, a := range accounts {
		tables[i], err = decodeLookupTable(a, observedSlot)
		if err != nil {
			return nil, err
		}
		if !equalStrings(tables[i].Addresses, listed[addresses[i]]) {
			return nil, errors.New("Jupiter lookup table declaration differs from finalized chain")
		}
	}
	return tables, nil
}

func validateToken2022Extensions(data []byte, accountType byte) error {
	if len(data) <= 165 {
		return errors.New("Token-2022 account omits account type")
	}
	if data[165] != accountType {
		return errors.New("Token-2022 account has wrong account type")
	}
	for offset := 166; offset < len(data); {
		if len(data)-offset < 4 {
			if bytes.Equal(data[offset:], make([]byte, len(data)-offset)) {
				return nil
			}
			return errors.New("truncated Token-2022 extension")
		}
		kind := binary.LittleEndian.Uint16(data[offset:])
		size := int(binary.LittleEndian.Uint16(data[offset+2:]))
		offset += 4
		if size > len(data)-offset {
			return errors.New("truncated Token-2022 extension data")
		}
		value := data[offset : offset+size]
		offset += size
		if kind == 0 {
			if !bytes.Equal(value, make([]byte, len(value))) {
				return errors.New("nonzero Token-2022 padding")
			}
			continue
		}
		if accountType == 1 {
			switch kind {
			case 3, 4, 12, 16, 18, 19:
			case 6:
				if len(value) != 1 || value[0] != 1 {
					return errors.New("Token-2022 default account state is not initialized")
				}
			case 1:
				if len(value) != 108 || binary.LittleEndian.Uint64(value[80:88]) != 0 || binary.LittleEndian.Uint16(value[88:90]) != 0 || binary.LittleEndian.Uint64(value[98:106]) != 0 || binary.LittleEndian.Uint16(value[106:108]) != 0 {
					return errors.New("Token-2022 mint has a nonzero transfer fee")
				}
			case 14:
				if len(value) != 64 || !bytes.Equal(value[32:], make([]byte, 32)) {
					return errors.New("Token-2022 mint has an active transfer hook")
				}
			default:
				return errors.New("Token-2022 mint has an unsupported extension")
			}
		} else {
			switch kind {
			case 7:
			case 2:
				if len(value) != 8 || binary.LittleEndian.Uint64(value) != 0 {
					return errors.New("Token-2022 account has withheld transfer fees")
				}
			case 15:
				if len(value) != 1 || value[0] != 0 {
					return errors.New("Token-2022 account is inside a transfer hook")
				}
			default:
				return errors.New("Token-2022 token account has an unsupported extension")
			}
		}
	}
	return nil
}

func validateStableAccount(account *chain.Account, mint, owner string) error {
	program, ok := stableTokenProgram(mint)
	if !ok || account == nil || account.Owner.String() != program {
		return errors.New("stable token account has wrong canonical program")
	}
	if len(account.Data) < 165 || encodeBase58(account.Data[:32]) != mint || encodeBase58(account.Data[32:64]) != owner || account.Data[108] != 1 {
		return errors.New("stable token account binding or state is invalid")
	}
	if program == token2022Program {
		return validateToken2022Extensions(account.Data, 2)
	}
	if len(account.Data) != 165 {
		return errors.New("classic token account has extensions")
	}
	return nil
}
func validateStableMint(account *chain.Account, mint string) error {
	program, ok := stableTokenProgram(mint)
	if !ok || account == nil || account.Key.String() != mint || account.Owner.String() != program || len(account.Data) < 82 || account.Data[44] != 6 || account.Data[45] != 1 {
		return errors.New("stable mint binding, decimals, or state is invalid")
	}
	if program == token2022Program {
		return validateToken2022Extensions(account.Data, 1)
	}
	if len(account.Data) != 82 {
		return errors.New("classic mint has extensions")
	}
	return nil
}

type swapSpendingLimit struct {
	Mint    string
	Start   int64
	Daily   bool
	Maximum uint64
	Exact   bool
}

func decodeStrictSwapPolicy(data []byte) (DecodedSquadsPolicy, []string, []swapSpendingLimit, error) {
	policy, err := DecodeSquadsPolicy(data)
	if err != nil {
		return policy, nil, nil, err
	}
	c := wireCursor{b: data}
	c.skip(8 + 32 + 8 + 1 + 8 + 8)
	n := c.u32()
	for i := uint32(0); i < n; i++ {
		c.skip(32)
		if c.u8() != 7 {
			return policy, nil, nil, errors.New("swap policy signer does not have full permissions")
		}
	}
	if c.u16() != 1 {
		return policy, nil, nil, errors.New("swap policy threshold is not one")
	}
	c.skip(4)
	if c.u8() != 3 {
		return policy, nil, nil, errors.New("swap policy payload is not ProgramInteraction")
	}
	c.skip(1)
	start := c
	legacyPayload, end, legacyErr := squadspolicy.DecodeConstraints(c.b, c.i, policy.AccountIndex, false, 128, 256)
	if legacyErr == nil && len(legacyPayload.Constraints) == 0 {
		legacyErr = errors.New("invalid legacy policy constraint count")
	}
	if legacyErr == nil {
		c.i = end
	}
	compact := false
	var table []string
	if legacyErr != nil {
		compact = true
		c = start
		count := int(c.u8())
		if count > 240 {
			return policy, nil, nil, errors.New("compact swap policy table exceeds limit")
		}
		table = make([]string, count)
		for i := range table {
			table[i] = encodeBase58(c.take(32))
		}
		c = start
		compactPayload, compactEnd, decodeErr := squadspolicy.DecodeConstraints(c.b, c.i, policy.AccountIndex, true, 128, 256)
		err = decodeErr
		if err == nil && len(compactPayload.Constraints) == 0 {
			err = errors.New("invalid compact policy constraint count")
		}
		if err != nil {
			return policy, nil, nil, err
		}
		c.i = compactEnd
	}
	if c.u8() != 0 || c.u8() != 0 {
		return policy, nil, nil, errors.New("swap policy hooks are not allowed")
	}
	count := int(c.u32())
	if compact {
		c.i -= 4
		count = int(c.u8())
	}
	if count != 3 {
		return policy, nil, nil, errors.New("swap policy must have three source spending limits")
	}
	limits := make([]swapSpendingLimit, 0, 3)
	readOptionI64 := func() (bool, error) {
		tag := c.u8()
		if tag == 0 {
			return false, nil
		}
		if tag == 1 {
			c.skip(8)
			return true, nil
		}
		return false, errors.New("invalid spending expiration option")
	}
	for i := 0; i < count; i++ {
		var mint string
		if compact {
			idx := int(c.u8())
			if idx >= len(table) {
				return policy, nil, nil, errors.New("spending mint index is invalid")
			}
			mint = table[idx]
		} else {
			mint = encodeBase58(c.take(32))
		}
		started := int64(binary.LittleEndian.Uint64(c.take(8)))
		expiration, e := readOptionI64()
		if e != nil || expiration {
			return policy, nil, nil, errors.New("swap spending limit must not expire")
		}
		daily := c.u8() == 1
		exact := true
		maximum := uint64(0)
		if compact {
			maximum = binary.LittleEndian.Uint64(c.take(8))
		} else {
			accumulate := c.u8()
			maximum = binary.LittleEndian.Uint64(c.take(8))
			maxUse := binary.LittleEndian.Uint64(c.take(8))
			enforce := c.u8()
			remaining := binary.LittleEndian.Uint64(c.take(8))
			lastReset := int64(binary.LittleEndian.Uint64(c.take(8)))
			exact = accumulate == 0 && maxUse == 0 && enforce == 0 && remaining <= maximum && lastReset >= started
		}
		limits = append(limits, swapSpendingLimit{mint, started, daily, maximum, exact})
	}
	if c.err != nil {
		return policy, nil, nil, c.err
	}
	policyStart := int64(binary.LittleEndian.Uint64(c.take(8)))
	if c.u8() != 0 {
		return policy, nil, nil, errors.New("swap policy expiration is not allowed")
	}
	c.skip(32)
	if c.err != nil || policyStart < 0 {
		return policy, nil, nil, errors.New("swap policy trailing state is invalid")
	}
	return policy, table, limits, nil
}

func validateCrossMintSwapPolicy(policy DecodedSquadsPolicy, table []string, limits []swapSpendingLimit, binding CrossMintPolicyBindings, swap RouteInstruction, dialect string) error {
	derived, bump, err := derivePolicyAccount(binding.Settings, policy.PolicySeed)
	if err != nil || derived != binding.Swap.PolicyAccount || bump != policy.Bump || policy.Settings != binding.Settings {
		return errors.New("finalized swap policy PDA/settings identity differs from durable binding")
	}
	if len(policy.DelegatedSigners) != 1 || policy.DelegatedSigners[0] != binding.DelegatedSigner || len(policy.SignerPermissions) != 1 || policy.SignerPermissions[0] != 7 || policy.TimeLock != 0 || policy.StaleTransactionIndex > policy.TransactionIndex || policy.AccountIndex != binding.VaultIndex || len(policy.Constraints) != 2 {
		return errors.New("finalized swap policy does not have the canonical signer/index/two-dialect shape")
	}
	expectedMints := []string{USDCMint, USDTMint, USDSMint}
	expectedShard := "classic"
	program := tokenProgram
	if binding.Swap.SourceShard == "token_2022" {
		expectedMints = []string{CashMint, USDGMint, PYUSDMint}
		expectedShard = "token_2022"
		program = token2022Program
	}
	if binding.Swap.SourceShard != expectedShard || !contains(expectedMints, swapSourceMint(swap)) || len(limits) != 3 {
		return errors.New("finalized swap policy source shard is invalid")
	}
	seen := map[string]bool{}
	for _, limit := range limits {
		if !contains(expectedMints, limit.Mint) || seen[limit.Mint] || limit.Start < 0 || !limit.Daily || limit.Maximum != binding.Swap.DailySourceMintSpendingCap || !limit.Exact {
			return errors.New("finalized swap policy spending limits differ from durable binding")
		}
		seen[limit.Mint] = true
	}
	if fingerprintCrossMintManifest(binding, expectedMints, program) != binding.Swap.ManifestFingerprint {
		return errors.New("finalized swap policy manifest fingerprint differs from durable binding")
	}
	if len(table) > 0 {
		referenced := map[string]bool{}
		for _, constraint := range policy.Constraints {
			referenced[constraint.ProgramID.String()] = true
			for _, a := range constraint.AccountConstraints {
				for _, k := range a.Pubkeys {
					referenced[k.String()] = true
				}
				if a.Owner != nil {
					referenced[a.Owner.String()] = true
				}
			}
		}
		for _, limit := range limits {
			referenced[limit.Mint] = true
		}
		if len(referenced) != len(table) {
			return errors.New("compact swap policy pubkey table is not tight")
		}
		for _, key := range table {
			if !referenced[key] {
				return errors.New("compact swap policy pubkey table has an unused key")
			}
		}
	}
	atas := map[string]bool{}
	for _, mint := range earnStableMints {
		ata, err := deriveATA(binding.VaultPubkey, mint, mustStableProgram(mint))
		if err != nil {
			return err
		}
		atas[ata] = true
	}

	for i, constraint := range policy.Constraints {
		if constraint.ProgramID.String() != jupiterProgram {
			return errors.New("finalized swap policy authorizes a non-Jupiter program")
		}
		expectedDisc := jupiterRouteV2Discriminator
		offset := uint64(24)
		if i == 1 {
			expectedDisc = jupiterSharedV2Discriminator
			offset = 25
		}
		if len(constraint.AccountConstraints) != 2 || len(constraint.DataConstraints) != 3 || constraint.AccountConstraints[0].AccountIndex != map[bool]uint8{true: 1, false: 0}[i == 1] || len(constraint.AccountConstraints[0].Pubkeys) != 1 || constraint.AccountConstraints[0].Pubkeys[0].String() != binding.VaultPubkey || constraint.AccountConstraints[0].Owner != nil || len(constraint.AccountConstraints[1].Pubkeys) != 6 || constraint.AccountConstraints[1].AccountIndex != map[bool]uint8{true: 5, false: 2}[i == 1] || !exactKeySet(constraint.AccountConstraints[1].Pubkeys, atas) || constraint.AccountConstraints[1].Owner != nil || constraint.DataConstraints[0].DataOffset != 0 || constraint.DataConstraints[0].DataValue.Kind != 5 || constraint.DataConstraints[0].Operator != 0 || !bytes.Equal(constraint.DataConstraints[0].DataValue.Bytes, expectedDisc) || constraint.DataConstraints[1].DataOffset != offset || constraint.DataConstraints[1].DataValue.Kind != 1 || constraint.DataConstraints[1].Operator != 5 || constraint.DataConstraints[1].DataValue.U16 != binding.Swap.MaxSlippageBPS || constraint.DataConstraints[2].DataOffset != offset+2 || constraint.DataConstraints[2].DataValue.Kind != 0 || constraint.DataConstraints[2].Operator != 0 || constraint.DataConstraints[2].DataValue.U8 != 0 {
			return errors.New("finalized swap policy constraints differ from canonical manifest")
		}
	}
	selected := 0
	if dialect == "shared_accounts_route_v2" {
		selected = 1
	}
	if !policyConstraintMatches(policy.Constraints[selected], swap) {
		return errors.New("fresh Jupiter build does not satisfy finalized swap policy")
	}
	return nil
}

func swapSourceMint(ix RouteInstruction) string {
	if len(ix.Data) >= 8 && bytes.Equal(ix.Data[:8], jupiterRouteV2Discriminator) && len(ix.Accounts) > 3 {
		return ix.Accounts[3].Address
	}
	if len(ix.Accounts) > 6 {
		return ix.Accounts[6].Address
	}
	return ""
}

func exactKeySet(values []solana.PublicKey, expected map[string]bool) bool {
	if len(values) != len(expected) {
		return false
	}
	seen := map[string]bool{}
	for _, key := range values {
		v := key.String()
		if !expected[v] || seen[v] {
			return false
		}
		seen[v] = true
	}
	return true
}
func derivePolicyAccount(settings string, seed uint64) (string, uint8, error) {
	settingsKey, err := solana.PublicKeyFromBase58(settings)
	if err != nil {
		return "", 0, err
	}
	program, _ := solana.PublicKeyFromBase58(SquadsProgram)
	var raw [8]byte
	binary.LittleEndian.PutUint64(raw[:], seed)
	key, bump, err := solana.FindProgramAddress([][]byte{[]byte("smart_account"), []byte("policy"), settingsKey[:], raw[:]}, program)
	if err != nil {
		return "", 0, err
	}
	return key.String(), bump, nil
}

func fingerprintCrossMintManifest(binding CrossMintPolicyBindings, mints []string, program string) string {
	return CrossMintManifestFingerprint(binding.Settings, binding.VaultPubkey, binding.DelegatedSigner, binding.VaultIndex, binding.Swap.MaxSlippageBPS, binding.Swap.DailySourceMintSpendingCap, mints, program)
}

// CrossMintManifestFingerprint is generalized_cross_mint_manifest_fingerprint,
// shared by the observer projection and the route revalidator.
func CrossMintManifestFingerprint(settings, vault, delegate string, vaultIndex uint8, maxSlippageBPS uint16, dailyCap uint64, mints []string, program string) string {
	h := sha256.New()
	field := func(v string) { h.Write([]byte(v)); h.Write([]byte{0}) }
	field("canonical_stables_v1")
	field(settings)
	field(vault)
	field(delegate)
	field(strconv.Itoa(int(vaultIndex)))
	field(strconv.Itoa(int(maxSlippageBPS)))
	for _, mint := range mints {
		field(mint)
		field(program)
		var cap [8]byte
		binary.LittleEndian.PutUint64(cap[:], dailyCap)
		h.Write(cap[:])
	}
	for _, mint := range earnStableMints {
		field(mint)
		field(mustStableProgram(mint))
	}
	field("route_v2")
	h.Write([]byte{0})
	field("shared_accounts_route_v2")
	h.Write([]byte{1})
	return hex.EncodeToString(h.Sum(nil))
}

func (r *Revalidator) cycleCrossMint(ctx context.Context, lease RevalidationLease) error {
	ctx, cancel := context.WithDeadline(ctx, lease.ExpiresAt.Add(-5*time.Second))
	defer cancel()
	store, ok := r.store.(crossMintPreflightStore)
	if !ok {
		return errors.New("cross-mint source requires actual movement control checks")
	}
	generation, err := store.CheckCrossMintPreflightLease(ctx, lease)
	if err != nil {
		return err
	}
	out, err := r.prepareCrossMintPreflight(ctx, lease, false)
	if err != nil {
		return err
	}
	current, err := store.CheckCrossMintPreflightLease(ctx, lease)
	if err != nil || current != generation {
		if err == nil {
			err = errors.New("cross-mint control generation changed during source preflight")
		}
		return err
	}
	if out.WaitingALT {
		return r.store.CommitRevalidation(ctx, lease, RevalidationCommit{CrossMintControlGeneration: &generation, Disposition: "waiting_alt", Preparation: &out.PreflightPreparation, MissingAddresses: out.MissingAddresses, ExpectedEpochFingerprint: lease.OptimizerEpochKey, ExpectedOpportunityKey: lease.IdempotencyKey})
	}
	return r.store.CommitRevalidation(ctx, lease, RevalidationCommit{CrossMintControlGeneration: &generation, Disposition: "ready", Preparation: &out.PreflightPreparation, ConflictKeys: out.PreflightPreparation.Transaction.WritableAccounts, ExpectedEpochFingerprint: lease.OptimizerEpochKey, ExpectedOpportunityKey: lease.IdempotencyKey, FreshEconomics: true, ObservedSourceAPYBPS: out.SourceAPYBPS, ObservedTargetAPYBPS: out.TargetAPYBPS, TargetObservedSupplyUSDMicros: out.TargetObservedSupplyUSDMicros, TargetObservedSlot: out.ObservedSlot})
}

func (r *Revalidator) prepareCrossMintPreflight(ctx context.Context, lease RevalidationLease, activation bool) (out CrossMintActivationPreparation, err error) {
	if !r.crossMintEnabled || r.jupiter == nil {
		return out, errors.New("cross-mint revalidation is disabled")
	}
	var plan crossMintPlan
	if err := json.Unmarshal(lease.ExecutionPlan, &plan); err != nil || plan.Kind != "cross_mint_jupiter" || plan.SourceMint != lease.SourceLiquidityMint || plan.TargetMint != lease.TargetLiquidityMint || plan.Amount != lease.LiquidityAmountRaw || plan.SourceMint == plan.TargetMint {
		return out, errors.New("canonical cross-mint execution plan is invalid")
	}
	b := plan.Bindings
	if b.VaultPubkey != lease.VaultPubkey || b.VaultIndex != lease.VaultIndex || b.DelegatedSigner != r.signer || b.Settings == "" || b.Withdraw.PolicyAccount != lease.PolicyAccount || b.Withdraw.SourceCommitment != "finalized" || b.Swap.SourceCommitment != "finalized" || b.Deposit.SourceCommitment != "finalized" || b.Swap.MaxSlippageBPS == 0 || b.Swap.MaxSlippageBPS > 10_000 || b.Swap.DailySourceMintSpendingCap < plan.Amount {
		return out, errors.New("cross-mint policy bindings are invalid")
	}
	if plan.ValueLoss == 0 || plan.ValueLoss > 1_000 {
		return out, errors.New("cross-mint plan value-loss cap is invalid")
	}
	valueLoss := plan.ValueLoss
	if valueLoss > r.crossMintMaxValueLossBPS {
		valueLoss = r.crossMintMaxValueLossBPS
	}
	effectiveSlippage := b.Swap.MaxSlippageBPS
	if r.crossMintMaxSlippageBPS < effectiveSlippage {
		effectiveSlippage = r.crossMintMaxSlippageBPS
	}
	minimumOutput, err := minimumEconomicOutput(plan.Amount, valueLoss)
	if err != nil {
		return out, err
	}
	q := crossMintActivationRequest(lease)
	minimumSlot := int64(max(b.Withdraw.ObservedSlot, b.Swap.ObservedSlot, b.Deposit.ObservedSlot))
	bank, err := r.loadCrossMintRouteBank(ctx, q, plan, nil, minimumSlot)
	if err != nil {
		return out, err
	}
	if bank.targetEconomics.LastUpdateStale || bank.targetEconomics.EconomicLifetimeMillis <= 0 || bank.targetEconomics.TotalSupplyUSDMicros <= minimumReserveSupplyUSDMicros || bank.targetEconomics.SupplyAPYBPS < 0 || bank.targetEconomics.SupplyAPYBPS >= 5000 {
		return out, errors.New("finalized cross-mint target is no longer eligible")
	}
	profitableOutput, err := minimumProfitableCrossMintOutput(lease.ExecutionPlan, plan.Amount, bank.sourceEconomics.SupplyAPYBPS, bank.targetEconomics.SupplyAPYBPS)
	if err != nil {
		return out, err
	}
	if profitableOutput > minimumOutput {
		minimumOutput = profitableOutput
	}
	body, err := r.jupiter.fetch(ctx, plan.SourceMint, plan.TargetMint, plan.Amount, lease.VaultPubkey, effectiveSlippage)
	if err != nil {
		return out, err
	}
	var envelope rawJupiterBuild
	if err = json.Unmarshal(body, &envelope); err != nil {
		return out, err
	}
	if threshold, e := parseAmount(envelope.OtherAmountThreshold); e != nil || threshold < minimumOutput {
		quoted, e := parseAmount(envelope.OutAmount)
		if e != nil || quoted < minimumOutput {
			return out, errors.New("fresh Jupiter quote is below economic minimum")
		}
		hi, lo := bits.Mul64(quoted-minimumOutput, 10_000)
		available64, _ := bits.Div64(hi, lo, quoted)
		available := uint16(available64)
		tight := effectiveSlippage
		if available < tight {
			tight = available
		}
		if tight <= 1 {
			return out, errors.New("fresh Jupiter quote leaves no safe slippage budget")
		}
		body, err = r.jupiter.fetch(ctx, plan.SourceMint, plan.TargetMint, plan.Amount, lease.VaultPubkey, tight-1)
		if err != nil {
			return out, err
		}
		if err = json.Unmarshal(body, &envelope); err != nil {
			return out, err
		}
	}
	minimumSlot = bank.slot
	jupiterTables, err := r.loadFinalizedJupiterTables(ctx, envelope.AddressesByLookupTableAddress, minimumSlot)
	if err != nil {
		return out, err
	}
	validated, err := validateJupiterEnvelope(body, plan, lease.VaultPubkey, effectiveSlippage, jupiterTables)
	if err != nil {
		return out, err
	}
	if validated.MinimumOutput < minimumOutput {
		return out, errors.New("signed Jupiter minimum output exceeds maximum value loss")
	}
	blockHeight, _, err := r.rpc.FinalizedBlockHeight(ctx)
	if err != nil {
		return out, err
	}
	if blockHeight > validated.LastValidBlockHeight {
		return out, errors.New("Jupiter certification blockhash expired")
	}
	validated.ObservedBlockHeight = blockHeight
	var additionalMints []string
	for _, mint := range earnStableMints {
		if mint == plan.SourceMint || mint == plan.TargetMint {
			continue
		}
		ata, e := deriveATA(lease.VaultPubkey, mint, mustStableProgram(mint))
		if e != nil {
			return out, e
		}
		for _, meta := range validated.Swap.Accounts {
			if meta.Address == ata {
				additionalMints = append(additionalMints, mint)
				break
			}
		}
	}
	if len(additionalMints) > 0 {
		fresh, e := r.loadCrossMintRouteBank(ctx, q, plan, additionalMints, bank.slot)
		if e != nil {
			return out, e
		}
		if !sameCrossMintCoreBank(bank, fresh) {
			return out, errors.New("prewithdraw finalized accounts changed during quote resolution")
		}
		bank = fresh
		for _, mint := range additionalMints {
			ata, _ := deriveATA(lease.VaultPubkey, mint, mustStableProgram(mint))
			if e := validateVaultTokenAccount(bank.at(ata), mint, lease.VaultPubkey); e != nil {
				return out, e
			}
		}
	}
	source, target, slot := bank.source, bank.target, bank.slot
	collateral := bank.sourceCollateral
	if collateral <= 1 || lease.SourceCollateralRaw != collateral-1 {
		return out, errors.New("finalized collateral anchor differs from opportunity")
	}
	accounts := []chain.Account{bank.accounts[lease.SourceReserve], bank.accounts[lease.TargetReserve], bank.accounts[plan.SourceMint], bank.accounts[plan.TargetMint], bank.accounts[source.Position.VaultLiquidityATA], bank.accounts[target.Position.VaultLiquidityATA], bank.accounts[b.Withdraw.PolicyAccount], bank.accounts[b.Swap.PolicyAccount], bank.accounts[b.Deposit.PolicyAccount]}
	for _, i := range []int{6, 7, 8} {
		if accounts[i].Owner.String() != SquadsProgram || accounts[i].Executable || accounts[i].Lamports == 0 {
			return out, errors.New("finalized prewithdraw policy account is not funded Squads state")
		}
	}
	route, err := BuildCrossMintLegs(KaminoSameMintRouteRequest{Vault: lease.VaultPubkey, Source: source.Position, Target: target.Position, WithdrawCollateralAmount: lease.SourceCollateralRaw, DepositLiquidityAmount: validated.MinimumOutput})
	if err != nil {
		return out, err
	}
	if len(route.Protected) != 2 {
		return out, errors.New("KLend cross-mint route did not return withdraw and deposit")
	}
	for i, policyIndex := range []int{6, 8} {
		policy, err := DecodeSquadsPolicy(accounts[policyIndex].Data)
		if err != nil {
			return out, err
		}
		binding := b.Withdraw
		if i == 1 {
			binding = b.Deposit
		}
		derived, bump, e := derivePolicyAccount(b.Settings, policy.PolicySeed)
		if e != nil || derived != binding.PolicyAccount || bump != policy.Bump || policy.Settings != b.Settings || policy.AccountIndex != lease.VaultIndex || len(policy.DelegatedSigners) != 1 || len(policy.SignerPermissions) != 1 || policy.SignerPermissions[0] != 7 || policy.TimeLock != 0 || policy.StaleTransactionIndex > policy.TransactionIndex || policy.DelegatedSigners[0] != r.signer || binding.ConstraintIndex >= uint8(len(policy.Constraints)) || !policyConstraintMatches(policy.Constraints[binding.ConstraintIndex], route.Protected[i]) {
			return out, errors.New("finalized Earn policy does not authorize exact KLend instruction")
		}
		if i == 0 {
			recovery, e := BuildIdleDeposit(KaminoIdleDepositRequest{Vault: lease.VaultPubkey, Target: source.Position, DepositLiquidityAmount: plan.Amount})
			if e != nil {
				return out, e
			}
			if len(policy.Constraints) < 2 || !policyConstraintMatches(policy.Constraints[1], recovery.Protected[0]) {
				return out, errors.New("finalized source policy does not authorize immutable recovery index 1")
			}
		}
	}
	swapPolicy, swapTable, spendingLimits, err := decodeStrictSwapPolicy(accounts[7].Data)
	if err != nil {
		return out, err
	}
	if err = validateCrossMintSwapPolicy(swapPolicy, swapTable, spendingLimits, b, validated.Swap, validated.Dialect); err != nil {
		return out, err
	}
	withdrawWrapped, err := wrapSquadsPolicy(b.Withdraw.PolicyAccount, r.signer, lease.VaultIndex, []uint8{b.Withdraw.ConstraintIndex}, []RouteInstruction{route.Protected[0]})
	if err != nil {
		return out, err
	}
	swapWrapped, err := wrapSquadsPolicy(b.Swap.PolicyAccount, r.signer, lease.VaultIndex, []uint8{validated.ConstraintIndex}, []RouteInstruction{validated.Swap})
	if err != nil {
		return out, err
	}
	firstObligation := -1
	for i, ix := range route.Public {
		if ix.Step == "kamino_refresh_obligation" {
			firstObligation = i
			break
		}
	}
	if firstObligation < 0 {
		return out, errors.New("cross-mint withdrawal lacks obligation refresh")
	}
	managedInstructions := append([]RouteInstruction{}, route.Public[:firstObligation+1]...)
	managedInstructions = append(managedInstructions, withdrawWrapped)
	required := requiredLookupTableAddresses(managedInstructions)
	externalAddresses := map[string]bool{}
	for _, table := range jupiterTables {
		for _, address := range table.Addresses {
			externalAddresses[address] = true
		}
	}
	filtered := required[:0]
	for _, address := range required {
		if !externalAddresses[address] {
			filtered = append(filtered, address)
		}
	}
	required = filtered
	var managed []LookupTable
	// Finalized Jupiter tables can already cover every withdrawal address.
	// The managed-table query intentionally rejects an empty requirements set.
	if len(required) > 0 {
		managed, err = r.store.LoadReusableLookupTables(ctx, lease.Cluster, lease.VaultID, slot, required)
		if err != nil {
			return out, err
		}
		managed, err = r.verifyFinalizedLookupTables(ctx, managed, slot)
		if err != nil {
			return out, err
		}
	}
	instructions := append([]RouteInstruction{}, computeBudgetInstructions(uint32(r.computeLimit), validated.UnitPrice)...)
	instructions = append(instructions, route.Public[:firstObligation+1]...)
	instructions = append(instructions, withdrawWrapped, swapWrapped)
	tables, err := combineCrossMintLookupTables(managed, jupiterTables)
	if err != nil {
		return out, err
	}
	manifestInput := KaminoSameMintRouteRequest{Vault: lease.VaultPubkey, Source: source.Position, Target: target.Position}
	manifest, err := BuildCrossMintPreflightALTManifest(manifestInput, b.Settings, b.Withdraw.PolicyAccount, b.Swap.PolicyAccount, r.signer, instructions, validated.Swap)
	if err != nil {
		return out, err
	}
	manifest, err = r.bindFinalizedCrossMintALTManifest(ctx, manifest, jupiterTables, slot)
	if err != nil {
		return out, err
	}
	requirementsFingerprint, err := ALTManifestRequirementsFingerprint(&manifest)
	if err != nil {
		return out, err
	}
	preview, allStatic, err := compileV0Transaction(r.signer, validated.Blockhash, instructions, tables, 1, r.computeLimit)
	if err != nil {
		return out, err
	}
	requiredSet := map[string]bool{}
	for _, address := range required {
		requiredSet[address] = true
	}
	missing := []string{}
	for _, address := range allStatic {
		if requiredSet[address] {
			missing = append(missing, address)
		}
	}
	if len(missing) > 0 {
		out.WaitingALT, out.MissingAddresses = true, canonicalStrings(missing)
		out.PreflightPreparation = crossMintMissingALTPreparation(q, instructions, missing)
		out.PreflightPreparation.RequirementsFingerprint = requirementsFingerprint
		out.PreflightPreparation.Manifest = &manifest
		out.SharedAddresses, out.VaultAddresses = manifest.SharedAddresses, manifest.VaultAddresses
		return out, nil
	}
	if preview.PacketBytes > SolanaPacketLimit {
		return out, errors.New("prewithdraw verifier exceeds Solana packet size")
	}
	sim, err := SimulateExact(ctx, r.rpc, preview.UnsignedWire, rpc.CommitmentFinalized, slot)
	if err != nil || !sim.Succeeded || sim.UnitsConsumed > r.computeLimit {
		if err != nil {
			return out, fmt.Errorf("finalized cross-mint preflight simulation: %w", err)
		}
		return out, errors.New("finalized cross-mint preflight simulation failed")
	}
	fee, err := r.rpc.Fee(ctx, preview.Message, rpc.CommitmentFinalized)
	if err != nil {
		return out, err
	}
	if fee > uint64(lease.FeeCapLamports) {
		return out, errors.New("cross-mint preflight fee exceeds opportunity cap")
	}
	preview.FeeLamports = fee
	cert, err := crossMintSourceCertificate(lease, plan, bank, validated, swapPolicy, valueLoss, preview, sim, jupiterTables)
	if err != nil {
		return out, err
	}
	certRaw, err := json.Marshal(cert)
	if err != nil {
		return out, err
	}
	routeHash := sha256.Sum256(preview.Message)
	out.Certificate = cert
	out.PreflightPreparation = RoutePreparation{RouteFingerprint: hex.EncodeToString(routeHash[:]), RequirementsFingerprint: requirementsFingerprint, Transaction: preview, Simulation: sim, ExecutionPlan: certRaw}
	out.PreflightPreparation.Manifest = &manifest
	if err := preserveCanonicalPlan(lease.ExecutionPlan, &out.PreflightPreparation, "cross_mint_preflight"); err != nil {
		return out, err
	}
	out.ObservedAt, out.ObservedSlot = bank.observedAt, bank.slot
	out.SourceAPYBPS, out.TargetAPYBPS = bank.sourceEconomics.SupplyAPYBPS, bank.targetEconomics.SupplyAPYBPS
	out.TargetObservedSupplyUSDMicros = bank.targetEconomics.TotalSupplyUSDMicros
	out.SharedAddresses, out.VaultAddresses = manifest.SharedAddresses, manifest.VaultAddresses
	if activation {
		withdrawalTables, e := r.store.LoadReusableLookupTables(ctx, lease.Cluster, lease.VaultID, bank.slot, requiredLookupTableAddresses(managedInstructions))
		if e != nil {
			return out, e
		}
		withdrawalTables, e = r.verifyFinalizedLookupTables(ctx, withdrawalTables, bank.slot)
		if e != nil {
			return out, e
		}
		initial, e := r.finishCrossMintInitialWithdrawal(ctx, lease, q, bank, managedInstructions, withdrawalTables)
		if e != nil {
			return out, e
		}
		out.InitialWithdrawalPreparation = initial
		if initial.WaitingALT {
			out.WaitingALT, out.MissingAddresses = true, initial.MissingAddresses
			out.PreflightPreparation = initial.Preparation
			out.Certificate = CrossMintPreflightCertificate{}
			out.SharedAddresses, out.VaultAddresses = initial.SharedAddresses, initial.VaultAddresses
			return out, nil
		}
	}
	finalBank, e := r.loadCrossMintRouteBank(ctx, q, plan, additionalMints, bank.slot)
	if e != nil {
		return out, e
	}
	if !sameCrossMintPreparationBank(bank, finalBank) {
		return out, errors.New("prewithdraw finalized account bank changed during exact verifier simulation")
	}
	return out, nil
}

func tableNames(tables []LookupTable) []string {
	out := make([]string, len(tables))
	for i, t := range tables {
		out[i] = t.Address
	}
	sort.Strings(out)
	return out
}

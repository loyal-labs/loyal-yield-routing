package jupiter

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/solana-foundation/solana-go/v2"
)

// The swap/v1 API bases. Lite is keyless and rate limited (it answered 503 for
// a few minutes on 2026-10-09). Keyed serves the same API to requests carrying
// an API key, and also answers keyless requests under a smaller rate limit.
const (
	LiteBase  = "https://lite-api.jup.ag/swap/v1"
	KeyedBase = "https://api.jup.ag/swap/v1"
)

const maxResponseBytes = 2 << 20

// Client calls one swap/v1 base. It sends each request once: no retries, no
// caching and no fallback to another host.
type Client struct {
	base, apiKey string
	http         *http.Client
}

// NewClient binds an absolute base URL. A non-empty apiKey is sent as
// x-api-key on every request.
func NewClient(base, apiKey string, client *http.Client) (*Client, error) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || client == nil {
		return nil, errors.New("invalid Jupiter client")
	}
	return &Client{base: strings.TrimRight(base, "/"), apiKey: apiKey, http: client}, nil
}

// QuoteRequest asks for an ExactIn route.
type QuoteRequest struct {
	InputMint, OutputMint string
	Amount                uint64
	SlippageBPS           uint16
	MaxAccounts           int
	// InstructionVersion "V2" asks for the v2 route dialects; empty leaves
	// Jupiter's default.
	InstructionVersion string
	// Dexes restricts the route to these venues; empty allows any.
	Dexes string
}

// Quote is the part of a quote the workers read. Raw is the response as
// received, which SwapInstructions posts back unchanged.
type Quote struct {
	Raw                  json.RawMessage   `json:"-"`
	InputMint            string            `json:"inputMint"`
	OutputMint           string            `json:"outputMint"`
	InAmount             string            `json:"inAmount"`
	OutAmount            string            `json:"outAmount"`
	OtherAmountThreshold string            `json:"otherAmountThreshold"`
	SwapMode             string            `json:"swapMode"`
	SlippageBPS          uint16            `json:"slippageBps"`
	ContextSlot          uint64            `json:"contextSlot"`
	PlatformFee          json.RawMessage   `json:"platformFee"`
	RoutePlan            []json.RawMessage `json:"routePlan"`
}

// SwapInstructions is the swap-instructions response.
type SwapInstructions struct {
	SwapInstruction             Instruction       `json:"swapInstruction"`
	SetupInstructions           []json.RawMessage `json:"setupInstructions"`
	OtherInstructions           []json.RawMessage `json:"otherInstructions"`
	CleanupInstruction          json.RawMessage   `json:"cleanupInstruction"`
	TokenLedgerInstruction      json.RawMessage   `json:"tokenLedgerInstruction"`
	AddressLookupTableAddresses []string          `json:"addressLookupTableAddresses"`
}

// Instruction is one instruction as the API encodes it.
type Instruction struct {
	ProgramID string        `json:"programId"`
	Accounts  []AccountMeta `json:"accounts"`
	Data      string        `json:"data"` // base64
}

// AccountMeta is one instruction account as the API encodes it.
type AccountMeta struct {
	Pubkey     string `json:"pubkey"`
	IsSigner   bool   `json:"isSigner"`
	IsWritable bool   `json:"isWritable"`
}

// Decode is the instruction as the chain takes it.
func (ix Instruction) Decode() (*solana.GenericInstruction, error) {
	program, err := solana.PublicKeyFromBase58(ix.ProgramID)
	if err != nil {
		return nil, fmt.Errorf("Jupiter instruction program: %w", err)
	}
	data, err := base64.StdEncoding.DecodeString(ix.Data)
	if err != nil {
		return nil, fmt.Errorf("Jupiter instruction data: %w", err)
	}
	metas := make(solana.AccountMetaSlice, len(ix.Accounts))
	for i, a := range ix.Accounts {
		key, err := solana.PublicKeyFromBase58(a.Pubkey)
		if err != nil {
			return nil, fmt.Errorf("Jupiter instruction account %d: %w", i, err)
		}
		metas[i] = &solana.AccountMeta{PublicKey: key, IsSigner: a.IsSigner, IsWritable: a.IsWritable}
	}
	return solana.NewInstruction(program, metas, data), nil
}

// Null reports whether an optional JSON value is absent or null.
func Null(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// Companions reports whether the response asks for any instruction besides
// the swap.
func (s SwapInstructions) Companions() bool {
	return len(s.SetupInstructions) != 0 || len(s.OtherInstructions) != 0 || !Null(s.CleanupInstruction) || !Null(s.TokenLedgerInstruction)
}

// Quote fetches an ExactIn quote.
func (c *Client) Quote(ctx context.Context, request QuoteRequest) (Quote, error) {
	query := url.Values{
		"inputMint": {request.InputMint}, "outputMint": {request.OutputMint},
		"amount": {strconv.FormatUint(request.Amount, 10)}, "slippageBps": {strconv.Itoa(int(request.SlippageBPS))},
		"swapMode": {"ExactIn"}, "maxAccounts": {strconv.Itoa(request.MaxAccounts)},
	}
	if request.InstructionVersion != "" {
		query.Set("instructionVersion", request.InstructionVersion)
	}
	if request.Dexes != "" {
		query.Set("dexes", request.Dexes)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/quote?"+query.Encode(), nil)
	if err != nil {
		return Quote{}, err
	}
	raw, err := c.do(httpRequest)
	if err != nil {
		return Quote{}, err
	}
	var quote Quote
	if err := json.Unmarshal(raw, &quote); err != nil {
		return Quote{}, fmt.Errorf("decode Jupiter quote: %w", err)
	}
	quote.Raw = raw
	return quote, nil
}

// SwapInstructions fetches the instructions for quote with user as the
// swap authority, without SOL wrapping or a dynamic compute unit limit.
func (c *Client) SwapInstructions(ctx context.Context, quote Quote, user solana.PublicKey, useSharedAccounts bool) (SwapInstructions, error) {
	body, err := json.Marshal(map[string]any{
		"userPublicKey": user.String(), "quoteResponse": quote.Raw,
		"wrapAndUnwrapSol": false, "useSharedAccounts": useSharedAccounts, "dynamicComputeUnitLimit": false,
	})
	if err != nil {
		return SwapInstructions{}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/swap-instructions", bytes.NewReader(body))
	if err != nil {
		return SwapInstructions{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	raw, err := c.do(httpRequest)
	if err != nil {
		return SwapInstructions{}, err
	}
	var response SwapInstructions
	if err := json.Unmarshal(raw, &response); err != nil {
		return SwapInstructions{}, fmt.Errorf("decode Jupiter instructions: %w", err)
	}
	return response, nil
}

func (c *Client) do(request *http.Request) (json.RawMessage, error) {
	if c.apiKey != "" {
		request.Header.Set("x-api-key", c.apiKey)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return nil, errors.New("Jupiter response exceeds bounded body")
	}
	if response.StatusCode != http.StatusOK || !json.Valid(data) {
		return nil, fmt.Errorf("Jupiter returned invalid HTTP %d response", response.StatusCode)
	}
	return data, nil
}

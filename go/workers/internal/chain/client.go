// Package chain is the workers' one Solana JSON-RPC client. It reads, sends
// and reports what the cluster says; it never retries. A rate limit comes back
// as ErrRateLimited and the caller's loop decides when to try again. Errors
// never carry the endpoint URL, which can hold a provider key.
package chain

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	bin "github.com/gagliardetto/binary"
	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
	"github.com/solana-foundation/solana-go/v2/rpc/jsonrpc"
)

// ErrUnavailable is an endpoint that could not answer: transport failure,
// timeout, 5xx, rate limit or a lagging node. It says nothing about the chain.
var ErrUnavailable = errors.New("rpc unavailable")

var ErrRateLimited = fmt.Errorf("%w: rate limited", ErrUnavailable)

var ErrBehind = fmt.Errorf("%w: node behind the requested slot", ErrUnavailable)

const (
	rateLimitedCode = -32429
	behindCode      = -32016

	internalErrorCode           = -32603
	blockNotAvailableCode       = -32004
	nodeUnhealthyCode           = -32005
	blockStatusNotAvailableCode = -32014
)

var unavailableCodes = map[int]bool{internalErrorCode: true, blockNotAvailableCode: true, nodeUnhealthyCode: true, blockStatusNotAvailableCode: true}

const getMultipleAccountsLimit = 100

type Client struct {
	rpc      *rpc.Client
	slotRate slotRate
}

type slotRate struct {
	mu        sync.Mutex
	perSecond float64
	err       error
	at        time.Time
	reading   chan struct{}
}

const SlotSamplePeriod = 60 * time.Second

func New(endpoint string, timeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		return nil, errors.New("chain client requires a timeout")
	}
	if parsed, err := url.Parse(endpoint); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("chain client requires an http or https endpoint")
	}
	return &Client{rpc: rpc.NewWithCustomRPCClient(jsonrpc.NewClientWithOpts(endpoint, &jsonrpc.RPCClientOpts{HTTPClient: keyedURLClient(timeout)}))}, nil
}

func keyedURLClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

var (
	urlPattern    = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://\S+`)
	secretPattern = regexp.MustCompile(`(?i)(password|api[-_]?key|token|secret)=\S+`)
)

const maxScrubbed = 500

// Scrub masks URLs and credential values in text that may echo an RPC endpoint and bounds it for logs and storage.
func Scrub(message string) string {
	message = urlPattern.ReplaceAllString(message, "<url>")
	message = secretPattern.ReplaceAllString(message, "$1=<redacted>")
	if len(message) > maxScrubbed {
		message = strings.ToValidUTF8(message[:maxScrubbed], "")
	}
	return message
}

func failed(method string, err error) error {
	var rpcErr *jsonrpc.RPCError
	var httpErr *jsonrpc.HTTPError
	var urlErr *url.Error
	switch {
	case errors.As(err, &rpcErr) && rpcErr.Code == rateLimitedCode,
		errors.As(err, &httpErr) && httpErr.Code == http.StatusTooManyRequests:
		return fmt.Errorf("%s: %w", method, ErrRateLimited)
	case errors.As(err, &rpcErr) && rpcErr.Code == behindCode:
		return fmt.Errorf("%s: %w", method, ErrBehind)
	case errors.As(err, &rpcErr) && unavailableCodes[rpcErr.Code]:
		return fmt.Errorf("%s: %w: rpc error %d: %s", method, ErrUnavailable, rpcErr.Code, Scrub(rpcErr.Message))
	case errors.As(err, &rpcErr):
		return fmt.Errorf("%s: rpc error %d: %s", method, rpcErr.Code, Scrub(rpcErr.Message))
	case errors.As(err, &httpErr):
		return fmt.Errorf("%s: %w: rpc status %d", method, ErrUnavailable, httpErr.Code)
	case errors.As(err, &urlErr):
		return fmt.Errorf("%s: %w: %w", method, ErrUnavailable, urlErr.Err)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%s: %w: %w", method, ErrUnavailable, context.DeadlineExceeded)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("%s: %w", method, context.Canceled)
	}
	return fmt.Errorf("%s: invalid rpc response", method)
}

type Account struct {
	Key        solana.PublicKey
	Owner      solana.PublicKey
	Lamports   uint64
	Data       []byte
	Executable bool
}

// Accounts reads keys at commitment, in calls of 100. Every call is pinned to
// at least the first call's context slot, so no account is older than Slot.
// An absent account is nil; whether that is allowed is the caller's decision.
func (c *Client) Accounts(ctx context.Context, keys []solana.PublicKey, commitment rpc.CommitmentType, minContextSlot uint64) (slot uint64, accounts []*Account, err error) {
	if len(keys) == 0 {
		return 0, nil, errors.New("getMultipleAccounts: no keys")
	}
	accounts = make([]*Account, 0, len(keys))
	for start := 0; start < len(keys); start += getMultipleAccountsLimit {
		batch := keys[start:min(start+getMultipleAccountsLimit, len(keys))]
		out, err := c.rpc.GetMultipleAccountsWithOpts(ctx, batch, &rpc.GetMultipleAccountsOpts{Encoding: solana.EncodingBase64, Commitment: commitment, MinContextSlot: atLeast(minContextSlot)})
		if err != nil {
			return 0, nil, failed("getMultipleAccounts", err)
		}
		if len(out.Value) != len(batch) || out.Context.Slot == 0 || out.Context.Slot < minContextSlot {
			return 0, nil, errors.New("getMultipleAccounts: response does not match the request")
		}
		if start == 0 {
			slot, minContextSlot = out.Context.Slot, out.Context.Slot
		}
		for i, value := range out.Value {
			if value == nil {
				accounts = append(accounts, nil)
				continue
			}
			accounts = append(accounts, &Account{Key: batch[i], Owner: value.Owner, Lamports: value.Lamports, Data: value.Data.GetBinary(), Executable: value.Executable})
		}
	}
	return slot, accounts, nil
}

func (c *Client) Slot(ctx context.Context, commitment rpc.CommitmentType) (uint64, error) {
	slot, err := c.rpc.GetSlot(ctx, commitment)
	if err != nil {
		return 0, failed("getSlot", err)
	}
	if slot == 0 {
		return 0, errors.New("getSlot: zero slot")
	}
	return slot, nil
}

func (c *Client) Blockhash(ctx context.Context, commitment rpc.CommitmentType, minContextSlot uint64) (hash solana.Hash, lastValid, slot uint64, err error) {
	out, err := c.rpc.GetLatestBlockhashWithOpts(ctx, &rpc.GetLatestBlockhashOpts{Commitment: commitment, MinContextSlot: atLeast(minContextSlot)})
	if err != nil {
		return solana.Hash{}, 0, 0, failed("getLatestBlockhash", err)
	}
	if out.Value == nil || out.Value.Blockhash.IsZero() || out.Value.LastValidBlockHeight == 0 || out.Context.Slot == 0 {
		return solana.Hash{}, 0, 0, errors.New("getLatestBlockhash: empty blockhash")
	}
	return out.Value.Blockhash, out.Value.LastValidBlockHeight, out.Context.Slot, nil
}

// MessageFee is what the cluster charges for the compiled message: 5000 lamports
// per signature, precompile signatures included, plus ceil(unit price × unit
// limit / 1e6) from its ComputeBudget instructions, or a v1 message's
// config priority fee.
func MessageFee(message []byte) (uint64, error) {
	var m solana.Message
	decoder := bin.NewBinDecoder(message)
	if err := m.UnmarshalWithDecoder(decoder); err != nil || decoder.HasRemaining() {
		return 0, fmt.Errorf("message fee: invalid compiled message: %v", err)
	}
	signatures, limit, price, limited := uint64(m.Header.NumRequiredSignatures), uint64(0), uint64(0), false
	if m.GetVersion() == solana.MessageVersionV1 {
		// A v1 message carries its priority fee as a lamports total in its
		// config; its ComputeBudget instructions are no-ops.
		fee := signatures * 5000
		if p := m.TransactionConfig.PriorityFee; p != nil {
			fee += *p
		}
		return fee, nil
	}
	for _, ix := range m.Instructions {
		if int(ix.ProgramIDIndex) >= len(m.AccountKeys) || len(ix.Data) == 0 {
			continue
		}
		switch program := m.AccountKeys[ix.ProgramIDIndex]; {
		case program == solana.ComputeBudget && ix.Data[0] == 2 && len(ix.Data) == 5:
			limit, limited = min(uint64(binary.LittleEndian.Uint32(ix.Data[1:])), 1_400_000), true
		case program == solana.ComputeBudget && ix.Data[0] == 3 && len(ix.Data) == 9:
			price = binary.LittleEndian.Uint64(ix.Data[1:])
		case program == ed25519Program || program == solana.Secp256k1ProgramID || program == secp256r1Program:
			signatures += uint64(ix.Data[0])
		}
	}
	if price > 0 && !limited {
		return 0, errors.New("message fee: a compute unit price needs an explicit unit limit")
	}
	hi, micro := bits.Mul64(price, limit)
	micro, carry := bits.Add64(micro, 999_999, 0)
	if hi != 0 || carry != 0 {
		return 0, errors.New("message fee: prioritization fee overflows")
	}
	return signatures*5000 + micro/1_000_000, nil
}

var ed25519Program, secp256r1Program = solana.MustPublicKeyFromBase58("Ed25519SigVerify111111111111111111111111111"), solana.MustPublicKeyFromBase58("Secp256r1SigVerify1111111111111111111111111")

func (c *Client) GenesisHash(ctx context.Context) (solana.Hash, error) {
	hash, err := c.rpc.GetGenesisHash(ctx)
	if err != nil {
		return solana.Hash{}, failed("getGenesisHash", err)
	}
	if hash.IsZero() {
		return solana.Hash{}, errors.New("getGenesisHash: empty hash")
	}
	return hash, nil
}

func (c *Client) RentExempt(ctx context.Context, size uint64) (uint64, error) {
	lamports, err := c.rpc.GetMinimumBalanceForRentExemption(ctx, size, rpc.CommitmentConfirmed)
	if err != nil {
		return 0, failed("getMinimumBalanceForRentExemption", err)
	}
	if lamports == 0 {
		return 0, errors.New("getMinimumBalanceForRentExemption: zero")
	}
	return lamports, nil
}

type SimulationError struct {
	Slot uint64
	Err  any
	Logs []string
}

func (e *SimulationError) Error() string {
	tail := e.Logs
	if len(tail) > 8 {
		tail = tail[len(tail)-8:]
	}
	return fmt.Sprintf("simulation failed at slot %d: %v %q", e.Slot, e.Err, tail)
}

type Simulated struct {
	Slot     uint64
	Units    uint64
	Logs     []string
	Accounts []*Account
}

// Simulate runs wire with opts; signature checks, blockhash replacement and
// the minimum slot are the caller's to choose. A transaction failure is a
// *SimulationError.
func (c *Client) Simulate(ctx context.Context, wire []byte, opts rpc.SimulateTransactionOpts) (Simulated, error) {
	out, err := c.rpc.SimulateRawTransactionWithOpts(ctx, wire, &opts)
	if err != nil {
		return Simulated{}, failed("simulateTransaction", err)
	}
	if out.Value == nil || out.Context.Slot == 0 || opts.MinContextSlot != nil && out.Context.Slot < *opts.MinContextSlot {
		return Simulated{}, errors.New("simulateTransaction: empty or stale result")
	}
	if out.Value.Err != nil {
		return Simulated{}, &SimulationError{Slot: out.Context.Slot, Err: out.Value.Err, Logs: out.Value.Logs}
	}
	simulated := Simulated{Slot: out.Context.Slot, Logs: out.Value.Logs}
	if out.Value.UnitsConsumed != nil {
		simulated.Units = *out.Value.UnitsConsumed
	}
	if opts.Accounts == nil {
		return simulated, nil
	}
	if len(out.Value.Accounts) != len(opts.Accounts.Addresses) {
		return Simulated{}, errors.New("simulateTransaction: captured accounts do not match the request")
	}
	simulated.Accounts = make([]*Account, len(out.Value.Accounts))
	for i, value := range out.Value.Accounts {
		if value != nil {
			simulated.Accounts[i] = &Account{Key: opts.Accounts.Addresses[i], Owner: value.Owner, Lamports: value.Lamports, Data: value.Data.GetBinary(), Executable: value.Executable}
		}
	}
	return simulated, nil
}

type TokenBalance struct {
	Mint     solana.PublicKey
	Owner    solana.PublicKey
	Program  solana.PublicKey
	Amount   uint64
	Decimals uint8
}

// Receipt is a landed transaction as the cluster recorded it. Err is the
// transaction's chain error, nil when it succeeded. Pre and Post hold the
// token accounts the cluster reported on each side, keyed by address; an
// account absent from one side did not exist there as a token account.
type Receipt struct {
	Slot uint64
	Err  any
	Fee  uint64
	Wire []byte
	// Keys are the static keys, then the loaded writable, then the loaded
	// readonly addresses; the lamport vectors follow the same order.
	Keys                           []solana.PublicKey
	LoadedWritable, LoadedReadonly []solana.PublicKey
	PreLamports, PostLamports      []uint64
	Pre                            map[solana.PublicKey]TokenBalance
	Post                           map[solana.PublicKey]TokenBalance
	Logs                           []string
	ReturnProgram                  solana.PublicKey
	ReturnData                     []byte
}

func (c *Client) Receipt(ctx context.Context, signature solana.Signature, commitment rpc.CommitmentType) (Receipt, error) {
	read, err := c.Execution(ctx, signature, commitment)
	return read.Receipt, err
}

type Signed struct {
	Signature solana.Signature
	Slot      uint64
	Failed    bool
	BlockTime *time.Time
}

// History lists up to limit of address's transactions at commitment, newest
// first, starting below before (the zero signature starts at the head), from
// a node at or past minContextSlot.
func (c *Client) History(ctx context.Context, address solana.PublicKey, limit int, before solana.Signature, commitment rpc.CommitmentType, minContextSlot uint64) ([]Signed, error) {
	out, err := c.rpc.GetSignaturesForAddressWithOpts(ctx, address, &rpc.GetSignaturesForAddressOpts{Limit: &limit, Before: before, Commitment: commitment, MinContextSlot: atLeast(minContextSlot)})
	if err != nil {
		return nil, failed("getSignaturesForAddress", err)
	}
	if len(out) > limit {
		return nil, errors.New("getSignaturesForAddress: more signatures than requested")
	}
	history := make([]Signed, 0, len(out))
	for _, row := range out {
		if row == nil || row.Signature.IsZero() || row.Slot == 0 {
			return nil, errors.New("getSignaturesForAddress: incomplete signature")
		}
		signed := Signed{Signature: row.Signature, Slot: row.Slot, Failed: row.Err != nil}
		if row.BlockTime != nil {
			at := row.BlockTime.Time().UTC()
			signed.BlockTime = &at
		}
		history = append(history, signed)
	}
	return history, nil
}

var ErrNotFound = errors.New("not found")

// SendWire submits the exact bytes once; maxRetries 0 keeps the node from
// rebroadcasting on its own. Land decides when to send again.
func (c *Client) SendWire(ctx context.Context, wire []byte, skipPreflight bool) error {
	none := uint(0)
	if _, err := c.rpc.SendRawTransactionWithOpts(ctx, wire, rpc.TransactionOpts{Encoding: solana.EncodingBase64, SkipPreflight: skipPreflight, PreflightCommitment: rpc.CommitmentConfirmed, MaxRetries: &none}); err != nil {
		return failed("sendTransaction", err)
	}
	return nil
}

func (c *Client) FinalizedBlockHeight(ctx context.Context) (height, slot uint64, err error) {
	out, err := c.rpc.GetEpochInfo(ctx, rpc.CommitmentFinalized)
	if err != nil {
		return 0, 0, failed("getEpochInfo", err)
	}
	if out == nil || out.BlockHeight == 0 || out.AbsoluteSlot == 0 {
		return 0, 0, errors.New("getEpochInfo: zero height or slot")
	}
	return out.BlockHeight, out.AbsoluteSlot, nil
}

func (c *Client) SignatureState(ctx context.Context, signature string) (SignatureState, error) {
	sig, err := solana.SignatureFromBase58(signature)
	if err != nil {
		return SignatureState{}, fmt.Errorf("getSignatureStatuses: %w", err)
	}
	out, err := c.rpc.GetSignatureStatuses(ctx, true, sig)
	if err != nil {
		return SignatureState{}, failed("getSignatureStatuses", err)
	}
	if len(out.Value) != 1 || out.Context.Slot == 0 {
		return SignatureState{}, errors.New("getSignatureStatuses: invalid shape or context")
	}
	state := SignatureState{ContextSlot: out.Context.Slot}
	status := out.Value[0]
	if status == nil {
		return state, nil
	}
	state.Found, state.Slot = true, status.Slot
	switch status.ConfirmationStatus {
	case rpc.ConfirmationStatusProcessed:
		state.Commitment = Processed
	case rpc.ConfirmationStatusConfirmed:
		state.Commitment = Confirmed
	case rpc.ConfirmationStatusFinalized:
		state.Commitment = Finalized
	default:
		return SignatureState{}, errors.New("getSignatureStatuses: unknown commitment")
	}
	if status.Err != nil {
		text, err := json.Marshal(status.Err)
		if err != nil {
			return SignatureState{}, errors.New("getSignatureStatuses: invalid error")
		}
		state.Err = string(text)
	}
	return state, nil
}

func tokenBalances(keys []solana.PublicKey, rows []rpc.TokenBalance) (map[solana.PublicKey]TokenBalance, error) {
	balances := make(map[solana.PublicKey]TokenBalance, len(rows))
	for _, row := range rows {
		if int(row.AccountIndex) >= len(keys) || row.Owner == nil || row.ProgramId == nil || row.UiTokenAmount == nil {
			return nil, errors.New("getTransaction: incomplete token balance")
		}
		amount, err := strconv.ParseUint(row.UiTokenAmount.Amount, 10, 64)
		if err != nil {
			return nil, errors.New("getTransaction: invalid token amount")
		}
		key := keys[row.AccountIndex]
		if _, dup := balances[key]; dup {
			return nil, errors.New("getTransaction: duplicate token balance")
		}
		balances[key] = TokenBalance{Mint: row.Mint, Owner: *row.Owner, Program: *row.ProgramId, Amount: amount, Decimals: row.UiTokenAmount.Decimals}
	}
	return balances, nil
}

func (c *Client) ProgramAccounts(ctx context.Context, program solana.PublicKey, filters []rpc.RPCFilter, commitment rpc.CommitmentType, minContextSlot uint64) (slot uint64, accounts []Account, err error) {
	out, err := c.rpc.GetProgramAccountsWithContext(ctx, program, &rpc.GetProgramAccountsOpts{Encoding: solana.EncodingBase64, Commitment: commitment, Filters: filters, MinContextSlot: atLeast(minContextSlot)})
	if err != nil {
		return 0, nil, failed("getProgramAccounts", err)
	}
	if out == nil || out.Context.Slot == 0 {
		return 0, nil, errors.New("getProgramAccounts: response has no context")
	}
	accounts = make([]Account, 0, len(out.Value))
	for _, value := range out.Value {
		if value == nil || value.Account == nil || value.Account.Data == nil {
			return 0, nil, errors.New("getProgramAccounts: incomplete account")
		}
		accounts = append(accounts, Account{Key: value.Pubkey, Owner: value.Account.Owner, Lamports: value.Account.Lamports, Data: value.Account.Data.GetBinary(), Executable: value.Account.Executable})
	}
	return out.Context.Slot, accounts, nil
}

type MicroLamports uint64

// PriorityFees are the per-compute-unit prices that recent landed
// transactions writing any of writable paid, one per slot.
func (c *Client) PriorityFees(ctx context.Context, writable []solana.PublicKey) ([]MicroLamports, error) {
	out, err := c.rpc.GetRecentPrioritizationFees(ctx, writable)
	if err != nil {
		return nil, failed("getRecentPrioritizationFees", err)
	}
	fees := make([]MicroLamports, len(out))
	for i, row := range out {
		fees[i] = MicroLamports(row.PrioritizationFee)
	}
	return fees, nil
}

func atLeast(slot uint64) *uint64 {
	if slot == 0 {
		return nil
	}
	return &slot
}

func (c *Client) FinalizedBlockHeightAt(ctx context.Context, slot uint64) (uint64, error) {
	rewards, version := false, uint64(anyVersion)
	out, err := c.rpc.GetBlockWithOpts(ctx, slot, &rpc.GetBlockOpts{TransactionDetails: rpc.TransactionDetailsNone, Rewards: &rewards, Commitment: rpc.CommitmentFinalized, MaxSupportedTransactionVersion: &version})
	if errors.Is(err, rpc.ErrNotConfirmed) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, failed("getBlock", err)
	}
	if out.BlockHeight == nil || *out.BlockHeight == 0 {
		return 0, errors.New("getBlock: no block height")
	}
	return *out.BlockHeight, nil
}

func (c *Client) SlotsPerSecond(ctx context.Context) (float64, error) {
	r := &c.slotRate
	for {
		r.mu.Lock()
		if !r.at.IsZero() && time.Since(r.at) < SlotSamplePeriod {
			defer r.mu.Unlock()
			return r.perSecond, r.err
		}
		if wait := r.reading; wait != nil {
			r.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		done := make(chan struct{})
		r.reading = done
		r.mu.Unlock()
		perSecond, err := c.measureSlotRate(ctx)
		r.mu.Lock()
		if ctx.Err() == nil {
			r.perSecond, r.err, r.at = perSecond, err, time.Now()
		}
		r.reading = nil
		r.mu.Unlock()
		close(done)
		return perSecond, err
	}
}

func (c *Client) measureSlotRate(ctx context.Context) (float64, error) {
	slots, seconds, err := c.SlotSamples(ctx, 5)
	if err != nil {
		return 0, err
	}
	if slots == 0 || seconds == 0 {
		return 0, errors.New("getRecentPerformanceSamples: no usable sample")
	}
	return float64(slots) / float64(seconds), nil
}

func (c *Client) SlotSamples(ctx context.Context, n uint) (slots, seconds uint64, err error) {
	out, err := c.rpc.GetRecentPerformanceSamples(ctx, &n)
	if err != nil {
		return 0, 0, failed("getRecentPerformanceSamples", err)
	}
	for _, sample := range out {
		if sample != nil && sample.NumSlots > 0 && sample.SamplePeriodSecs > 0 {
			slots, seconds = slots+sample.NumSlots, seconds+uint64(sample.SamplePeriodSecs)
		}
	}
	return slots, seconds, nil
}

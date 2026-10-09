// Package chain is the workers' one Solana JSON-RPC client. It reads, sends
// and reports what the cluster says; it never retries. A rate limit comes back
// as ErrRateLimited and the caller's loop decides when to try again. Errors
// never carry the endpoint URL, which can hold a provider key.
package chain

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/solana-foundation/solana-go/v2"
	"github.com/solana-foundation/solana-go/v2/rpc"
	"github.com/solana-foundation/solana-go/v2/rpc/jsonrpc"
)

// ErrUnavailable is an endpoint that could not answer: transport failure,
// timeout, 5xx, rate limit or a lagging node. It says nothing about the chain.
var ErrUnavailable = errors.New("rpc unavailable")

// ErrRateLimited is the endpoint refusing for rate: HTTP 429 with a plain
// body, or the JSON-RPC error providers send with it.
var ErrRateLimited = fmt.Errorf("%w: rate limited", ErrUnavailable)

// ErrBehind is a node that has not reached the requested minContextSlot yet.
var ErrBehind = fmt.Errorf("%w: node behind the requested slot", ErrUnavailable)

const (
	rateLimitedCode = -32429
	behindCode      = -32016
)

// maxAccountsPerCall is getMultipleAccounts' limit.
const maxAccountsPerCall = 100

type Client struct {
	rpc *rpc.Client
}

func New(endpoint string, timeout time.Duration) (*Client, error) {
	if endpoint == "" || timeout <= 0 {
		return nil, errors.New("chain client requires an endpoint and a timeout")
	}
	// The URL can carry a provider key; never forward it to a redirect target.
	httpClient := &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Client{rpc: rpc.NewWithCustomRPCClient(jsonrpc.NewClientWithOpts(endpoint, &jsonrpc.RPCClientOpts{HTTPClient: httpClient}))}, nil
}

// failed names the method and keeps only what the endpoint said: the JSON-RPC
// code and message, the HTTP status or the transport cause. solana-go's own
// messages for the last two carry the request URL, which can hold a key.
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
	case errors.As(err, &rpcErr):
		return fmt.Errorf("%s: rpc error %d: %s", method, rpcErr.Code, rpcErr.Message)
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

// Account is one account as the cluster holds it.
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
	for start := 0; start < len(keys); start += maxAccountsPerCall {
		batch := keys[start:min(start+maxAccountsPerCall, len(keys))]
		opts := &rpc.GetMultipleAccountsOpts{Encoding: solana.EncodingBase64, Commitment: commitment}
		if minContextSlot > 0 {
			opts.MinContextSlot = &minContextSlot
		}
		out, err := c.rpc.GetMultipleAccountsWithOpts(ctx, batch, opts)
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

// Blockhash is a blockhash at commitment, the last block height it is valid
// at and the slot it was read at.
func (c *Client) Blockhash(ctx context.Context, commitment rpc.CommitmentType) (hash solana.Hash, lastValid, slot uint64, err error) {
	out, err := c.rpc.GetLatestBlockhash(ctx, commitment)
	if err != nil {
		return solana.Hash{}, 0, 0, failed("getLatestBlockhash", err)
	}
	if out.Value == nil || out.Value.Blockhash.IsZero() || out.Value.LastValidBlockHeight == 0 || out.Context.Slot == 0 {
		return solana.Hash{}, 0, 0, errors.New("getLatestBlockhash: empty blockhash")
	}
	return out.Value.Blockhash, out.Value.LastValidBlockHeight, out.Context.Slot, nil
}

// Fee is what the cluster charges for message (the compiled message bytes,
// not the signed wire) at commitment.
func (c *Client) Fee(ctx context.Context, message []byte, commitment rpc.CommitmentType) (uint64, error) {
	out, err := c.rpc.GetFeeForMessage(ctx, base64.StdEncoding.EncodeToString(message), commitment)
	if err != nil {
		return 0, failed("getFeeForMessage", err)
	}
	if out == nil || out.Value == nil {
		return 0, errors.New("getFeeForMessage: no fee for this blockhash")
	}
	return *out.Value, nil
}

// GenesisHash identifies the cluster.
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

// RentExempt is the minimum balance an account of size bytes needs.
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

// SimulationError is a simulation the cluster ran and the transaction failed.
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

// Simulated is a simulation the cluster ran and the transaction succeeded.
type Simulated struct {
	Slot  uint64
	Units uint64
	Logs  []string
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
	return simulated, nil
}

// TokenBalance is one token account's balance before or after a transaction.
type TokenBalance struct {
	Mint    solana.PublicKey
	Owner   solana.PublicKey
	Program solana.PublicKey
	Amount  uint64
}

// Receipt is a landed transaction as the cluster recorded it. Err is the
// transaction's chain error, nil when it succeeded. Pre and Post hold the
// token accounts the cluster reported on each side, keyed by address; an
// account absent from one side did not exist there as a token account.
type Receipt struct {
	Slot uint64
	Err  any
	Fee  uint64
	// Wire is the transaction exactly as the cluster stored it.
	Wire []byte
	// Keys are the static keys, then the loaded writable, then the loaded
	// readonly addresses; the lamport vectors follow the same order.
	Keys                           []solana.PublicKey
	LoadedWritable, LoadedReadonly []solana.PublicKey
	PreLamports, PostLamports      []uint64
	Pre                            map[solana.PublicKey]TokenBalance
	Post                           map[solana.PublicKey]TokenBalance
	Logs                           []string
}

// Receipt reads signature's transaction at commitment (confirmed or
// finalized). ErrNotFound means the cluster has no record of it there.
func (c *Client) Receipt(ctx context.Context, signature solana.Signature, commitment rpc.CommitmentType) (Receipt, error) {
	version := uint64(0)
	out, err := c.rpc.GetTransaction(ctx, signature, &rpc.GetTransactionOpts{Encoding: solana.EncodingBase64, Commitment: commitment, MaxSupportedTransactionVersion: &version})
	if errors.Is(err, rpc.ErrNotFound) {
		return Receipt{}, ErrNotFound
	}
	if err != nil {
		return Receipt{}, failed("getTransaction", err)
	}
	if out.Slot == 0 || out.Meta == nil || out.Transaction == nil {
		return Receipt{}, errors.New("getTransaction: incomplete receipt")
	}
	tx, err := out.Transaction.GetTransaction()
	if err != nil {
		return Receipt{}, fmt.Errorf("getTransaction: decode: %w", err)
	}
	loaded := out.Meta.LoadedAddresses
	keys := append(append(append([]solana.PublicKey(nil), tx.Message.AccountKeys...), loaded.Writable...), loaded.ReadOnly...)
	receipt := Receipt{
		Slot: out.Slot, Err: out.Meta.Err, Fee: out.Meta.Fee, Wire: out.Transaction.GetBinary(), Logs: out.Meta.LogMessages,
		Keys: keys, LoadedWritable: loaded.Writable, LoadedReadonly: loaded.ReadOnly,
		PreLamports: out.Meta.PreBalances, PostLamports: out.Meta.PostBalances,
	}
	if receipt.Pre, err = tokenBalances(keys, out.Meta.PreTokenBalances); err != nil {
		return Receipt{}, err
	}
	if receipt.Post, err = tokenBalances(keys, out.Meta.PostTokenBalances); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

// Signed is one transaction in an address's confirmed history.
type Signed struct {
	Signature solana.Signature
	Slot      uint64
	Failed    bool
}

// History lists up to limit of address's confirmed transactions, newest
// first, starting below before (the zero signature starts at the head).
func (c *Client) History(ctx context.Context, address solana.PublicKey, limit int, before solana.Signature) ([]Signed, error) {
	out, err := c.rpc.GetSignaturesForAddressWithOpts(ctx, address, &rpc.GetSignaturesForAddressOpts{Limit: &limit, Before: before, Commitment: rpc.CommitmentConfirmed})
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
		history = append(history, Signed{Signature: row.Signature, Slot: row.Slot, Failed: row.Err != nil})
	}
	return history, nil
}

// ErrNotFound is a signature or account the cluster has no record of.
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

// FinalizedBlockHeight is the finalized block height and its slot, read
// together from one node.
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

// SignatureState reads one signature's status with full history.
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
		balances[key] = TokenBalance{Mint: row.Mint, Owner: *row.Owner, Program: *row.ProgramId, Amount: amount}
	}
	return balances, nil
}

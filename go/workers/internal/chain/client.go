// Package chain is the workers' one Solana JSON-RPC client. It reads, sends
// and reports what the cluster says; it never retries. A rate limit comes back
// as ErrRateLimited and the caller's loop decides when to try again. Errors
// never carry the endpoint URL, which can hold a provider key.
package chain

import (
	"context"
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

// ErrRateLimited is the endpoint refusing for rate: HTTP 429 with a plain
// body, or the JSON-RPC error providers send with it.
var ErrRateLimited = errors.New("rpc rate limited")

const rateLimitedCode = -32429

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
// error, the HTTP status or the transport cause. solana-go's own messages for
// the last two include the request URL.
func failed(method string, err error) error {
	var rpcErr *jsonrpc.RPCError
	var httpErr *jsonrpc.HTTPError
	var urlErr *url.Error
	switch {
	case errors.As(err, &rpcErr) && rpcErr.Code == rateLimitedCode,
		errors.As(err, &httpErr) && httpErr.Code == http.StatusTooManyRequests:
		return fmt.Errorf("%s: %w", method, ErrRateLimited)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded), errors.As(err, &rpcErr):
		return fmt.Errorf("%s: %w", method, err)
	case errors.As(err, &httpErr):
		return fmt.Errorf("%s: rpc status %d", method, httpErr.Code)
	case errors.As(err, &urlErr):
		return fmt.Errorf("%s: %w", method, urlErr.Err)
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

// Blockhash is a confirmed blockhash and the last block height it is valid at.
func (c *Client) Blockhash(ctx context.Context) (solana.Hash, uint64, error) {
	out, err := c.rpc.GetLatestBlockhash(ctx, rpc.CommitmentConfirmed)
	if err != nil {
		return solana.Hash{}, 0, failed("getLatestBlockhash", err)
	}
	if out.Value == nil || out.Value.Blockhash.IsZero() || out.Value.LastValidBlockHeight == 0 {
		return solana.Hash{}, 0, errors.New("getLatestBlockhash: empty blockhash")
	}
	return out.Value.Blockhash, out.Value.LastValidBlockHeight, nil
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

// Simulate runs the exact signed bytes with signature checks and the wire's
// own blockhash. A transaction failure is a *SimulationError.
func (c *Client) Simulate(ctx context.Context, wire []byte) (units uint64, logs []string, err error) {
	out, err := c.rpc.SimulateRawTransactionWithOpts(ctx, wire, &rpc.SimulateTransactionOpts{SigVerify: true, Commitment: rpc.CommitmentConfirmed})
	if err != nil {
		return 0, nil, failed("simulateTransaction", err)
	}
	if out.Value == nil || out.Context.Slot == 0 {
		return 0, nil, errors.New("simulateTransaction: empty result")
	}
	if out.Value.Err != nil {
		return 0, nil, &SimulationError{Slot: out.Context.Slot, Err: out.Value.Err, Logs: out.Value.Logs}
	}
	if out.Value.UnitsConsumed != nil {
		units = *out.Value.UnitsConsumed
	}
	return units, out.Value.Logs, nil
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
	Keys []solana.PublicKey
	Pre  map[solana.PublicKey]TokenBalance
	Post map[solana.PublicKey]TokenBalance
	Logs []string
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
	keys := append(append(append([]solana.PublicKey(nil), tx.Message.AccountKeys...), out.Meta.LoadedAddresses.Writable...), out.Meta.LoadedAddresses.ReadOnly...)
	receipt := Receipt{Slot: out.Slot, Err: out.Meta.Err, Fee: out.Meta.Fee, Keys: keys, Logs: out.Meta.LogMessages}
	if receipt.Pre, err = tokenBalances(keys, out.Meta.PreTokenBalances); err != nil {
		return Receipt{}, err
	}
	if receipt.Post, err = tokenBalances(keys, out.Meta.PostTokenBalances); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

// ErrNotFound is a signature or account the cluster has no record of.
var ErrNotFound = errors.New("not found")

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

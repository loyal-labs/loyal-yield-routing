package fleetexec

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	solanasdk "github.com/gagliardetto/solana-go"
	"io"
	"net/http"
	"strconv"
	"time"
)

// SignatureStatus is the chain's durable outcome for one signature. Found is
// false when the signature is absent from history.
type SignatureStatus struct {
	Found       bool
	Slot        int64
	Confirmed   bool
	Finalized   bool
	Err         string
	BlockHeight int64
	// ContextSlot is the RPC signature-history frontier, not an observation
	// of account or obligation effects. It cannot prove funds stayed unchanged.
	ContextSlot int64
}

// StatusClient answers protocol-specific recovery questions for exact
// signatures. It never retries internally.
type StatusClient interface {
	// SignatureStatus resolves one exact signature's durable outcome.
	SignatureStatus(ctx context.Context, signature string) (SignatureStatus, error)
	// FinalizedTransaction returns the finalized receipt bytes for one exact
	// signature, or nil when history does not yet contain it.
	FinalizedTransaction(ctx context.Context, signature string) (*TransactionReceipt, error)
}

// TransactionReceipt is the exact chain receipt evidence used for
// reconciliation: identity plus the token balance effects.
type TransactionReceipt struct {
	Slot              int64
	Signature         string
	MessageB64        string
	SignedTransaction []byte
	Err               string
	// accountAddresses is the durable message index->address evidence
	// persisted with the signed wire; reconciliation refuses to interpret
	// balance indices without it.
	accountAddresses []string
	TokenDeltas      []TokenDelta
}

// WithAccountAddresses binds the durable index->address evidence so the
// receipt's balance indices resolve to exact accounts.
func (r *TransactionReceipt) WithAccountAddresses(addresses []string) (*TransactionReceipt, error) {
	if len(addresses) == 0 {
		return nil, fmt.Errorf("empty account index evidence")
	}
	if len(r.accountAddresses) != len(addresses) {
		return nil, fmt.Errorf("receipt account list differs from durable account index evidence")
	}
	for i := range addresses {
		if r.accountAddresses[i] != addresses[i] {
			return nil, fmt.Errorf("receipt account %d differs from durable evidence", i)
		}
	}
	out := *r
	out.accountAddresses = append([]string(nil), addresses...)
	return &out, nil
}

// TokenDelta is one anchored pre->post token balance movement from the
// receipt metadata.
type TokenDelta struct {
	Account  string `json:"account"`
	Mint     string `json:"mint"`
	Decimals uint8  `json:"decimals"`
	// PreRaw and PostRaw are exact raw balances; a missing pre balance is
	// unknown, not zero, and is rejected during verification.
	PreRaw  *uint64 `json:"pre_raw"`
	PostRaw *uint64 `json:"post_raw"`
	// These are separate pre/post receipt assertions. Missing metadata remains
	// unknown; cross-mint custody cannot infer it from the current account.
	PreOwner    string `json:"-"`
	PostOwner   string `json:"-"`
	PreProgram  string `json:"-"`
	PostProgram string `json:"-"`
}

// RPCAdapter is the production Solana JSON-RPC dependency with a per-call
// deadline and no retry behavior.
type RPCAdapter struct {
	url      string
	client   *http.Client
	deadline time.Duration
}

// NewRPCAdapter builds the production adapter against one RPC endpoint.
func NewRPCAdapter(url string, deadline time.Duration) (*RPCAdapter, error) {
	if url == "" || deadline <= 0 {
		return nil, fmt.Errorf("incomplete RPC adapter configuration")
	}
	return &RPCAdapter{
		url:      url,
		client:   &http.Client{Timeout: deadline + 5*time.Second},
		deadline: deadline,
	}, nil
}

type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int64         `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

// rpcCallDiagnostics contains only bounded metadata, never provider text or IO.
type rpcCallDiagnostics struct {
	class      string
	httpStatus int
	rpcCode    int
}

func (a *RPCAdapter) call(ctx context.Context, result interface{}, method string, params ...interface{}) error {
	_, err := a.callWithDiagnostics(ctx, result, method, params...)
	return err
}

// Keep the original error for non-lookup callers. Lookup discards it after
// classification; returning metadata separately prevents unsafe cause wrapping.
func (a *RPCAdapter) callWithDiagnostics(ctx context.Context, result interface{}, method string, params ...interface{}) (d rpcCallDiagnostics, err error) {
	callCtx, cancel := context.WithTimeout(ctx, a.deadline)
	defer cancel()
	d.class = "request"
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: time.Now().UnixNano(), Method: method, Params: params})
	if err != nil {
		return d, err
	}
	request, err := http.NewRequestWithContext(callCtx, http.MethodPost, a.url, bytes.NewReader(body))
	if err != nil {
		return d, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		d.class = "transport"
		return d, fmt.Errorf("%s: %w", method, err)
	}
	defer response.Body.Close()
	d.httpStatus = response.StatusCode
	if response.StatusCode != http.StatusOK {
		d.class = "http"
		return d, fmt.Errorf("%s: rpc status %d", method, response.StatusCode)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	d.class = "decode"
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&envelope); err != nil {
		return d, fmt.Errorf("%s: decode: %w", method, err)
	}
	if envelope.Error != nil {
		d.class, d.rpcCode = "json_rpc", envelope.Error.Code
		return d, envelope.Error
	}
	if result == nil {
		return d, nil
	}
	d.class = "result_decode"
	return d, json.Unmarshal(envelope.Result, result)
}

type rpcSignatureStatus struct {
	Slot               int64           `json:"slot"`
	ConfirmationStatus string          `json:"confirmationStatus"`
	Err                json.RawMessage `json:"err"`
}

func (a *RPCAdapter) SignatureStatus(ctx context.Context, signature string) (SignatureStatus, error) {
	var envelope struct {
		Context struct {
			Slot int64 `json:"slot"`
		} `json:"context"`
		Value []*rpcSignatureStatus `json:"value"`
	}
	if err := a.call(ctx, &envelope, "getSignatureStatuses", []string{signature}, map[string]interface{}{"searchTransactionHistory": true}); err != nil {
		return SignatureStatus{}, err
	}
	if len(envelope.Value) != 1 || envelope.Context.Slot <= 0 {
		return SignatureStatus{}, fmt.Errorf("signature history response has invalid shape or context")
	}
	var height int64
	if err := a.call(ctx, &height, "getBlockHeight", map[string]interface{}{"commitment": "finalized"}); err != nil {
		return SignatureStatus{}, err
	}
	if height <= 0 {
		return SignatureStatus{}, fmt.Errorf("missing finalized block height")
	}
	out := SignatureStatus{ContextSlot: envelope.Context.Slot, BlockHeight: height}
	status := envelope.Value[0]
	if status == nil {
		return out, nil
	}
	if status.Slot <= 0 {
		return SignatureStatus{}, fmt.Errorf("found signature has no slot")
	}
	out.Found, out.Slot = true, status.Slot
	switch status.ConfirmationStatus {
	case "processed":
	case "confirmed":
		out.Confirmed = true
	case "finalized":
		out.Confirmed, out.Finalized = true, true
	default:
		return SignatureStatus{}, fmt.Errorf("signature commitment is missing or unknown")
	}
	if len(status.Err) > 0 && string(status.Err) != "null" {
		out.Err = string(status.Err)
	}
	return out, nil
}

type rpcTokenBalance struct {
	AccountIndex  int    `json:"accountIndex"`
	Mint          string `json:"mint"`
	Owner         string `json:"owner"`
	ProgramID     string `json:"programId"`
	UITokenAmount struct {
		Amount   string `json:"amount"`
		Decimals uint8  `json:"decimals"`
	} `json:"uiTokenAmount"`
}

// FinalizedTransaction fetches the finalized receipt for one exact signature.
// A nil receipt means finalized history does not contain the signature yet.
func (a *RPCAdapter) FinalizedTransaction(ctx context.Context, signature string) (*TransactionReceipt, error) {
	var raw json.RawMessage
	if err := a.call(ctx, &raw, "getTransaction", signature, map[string]interface{}{
		"encoding":                       "base64",
		"commitment":                     "finalized",
		"maxSupportedTransactionVersion": 0,
	}); err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var decoded struct {
		Slot        int64             `json:"slot"`
		Transaction []json.RawMessage `json:"transaction"`
		Meta        *struct {
			Err    json.RawMessage                        `json:"err"`
			Pre    []rpcTokenBalance                      `json:"preTokenBalances"`
			Post   []rpcTokenBalance                      `json:"postTokenBalances"`
			Loaded *struct{ Writable, Readonly []string } `json:"loadedAddresses"`
		} `json:"meta"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, fmt.Errorf("decode finalized transaction: %w", err)
	}
	if decoded.Slot <= 0 || len(decoded.Transaction) != 2 || decoded.Meta == nil {
		return nil, fmt.Errorf("finalized receipt has missing slot, transaction or metadata")
	}
	var wireB64, encoding string
	if err := json.Unmarshal(decoded.Transaction[0], &wireB64); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(decoded.Transaction[1], &encoding); err != nil || encoding != "base64" {
		return nil, fmt.Errorf("unsupported receipt encoding")
	}
	wire, err := base64.StdEncoding.DecodeString(wireB64)
	if err != nil || len(wire) > SolanaPacketLimit {
		return nil, fmt.Errorf("invalid receipt transaction bytes")
	}
	transaction, err := solanasdk.TransactionFromBytes(wire)
	if err != nil {
		return nil, fmt.Errorf("decode receipt wire: %w", err)
	}
	canonical, err := transaction.MarshalBinary()
	if err != nil || !bytes.Equal(canonical, wire) {
		return nil, fmt.Errorf("noncanonical receipt wire")
	}
	if len(transaction.Signatures) == 0 || transaction.Signatures[0].String() != signature {
		return nil, fmt.Errorf("receipt signature differs from requested identity")
	}
	if err := transaction.VerifySignatures(); err != nil {
		return nil, fmt.Errorf("receipt signature invalid: %w", err)
	}
	message, err := transaction.Message.MarshalBinary()
	if err != nil {
		return nil, err
	}
	receipt := &TransactionReceipt{Slot: decoded.Slot, Signature: signature, MessageB64: base64.StdEncoding.EncodeToString(message), SignedTransaction: bytes.Clone(wire)}
	for _, key := range transaction.Message.AccountKeys {
		receipt.accountAddresses = append(receipt.accountAddresses, key.String())
	}
	writable, readonly := 0, 0
	for _, lookup := range transaction.Message.AddressTableLookups {
		writable += len(lookup.WritableIndexes)
		readonly += len(lookup.ReadonlyIndexes)
	}
	if writable+readonly > 0 && decoded.Meta.Loaded == nil {
		return nil, fmt.Errorf("versioned receipt omits loaded address evidence")
	}
	if decoded.Meta.Loaded != nil {
		if len(decoded.Meta.Loaded.Writable) != writable || len(decoded.Meta.Loaded.Readonly) != readonly {
			return nil, fmt.Errorf("loaded address count differs from receipt message")
		}
		for _, key := range append(append([]string(nil), decoded.Meta.Loaded.Writable...), decoded.Meta.Loaded.Readonly...) {
			if _, err := solanasdk.PublicKeyFromBase58(key); err != nil {
				return nil, fmt.Errorf("invalid loaded receipt address")
			}
			receipt.accountAddresses = append(receipt.accountAddresses, key)
		}
	}
	if len(decoded.Meta.Err) > 0 && string(decoded.Meta.Err) != "null" {
		receipt.Err = string(decoded.Meta.Err)
	}
	receipt.TokenDeltas, err = receiptDeltas(receipt.accountAddresses, decoded.Meta.Pre, decoded.Meta.Post)
	if err != nil {
		return nil, err
	}

	return receipt, nil
}

// receiptDeltas reduces pre/post token balances into exact per-account deltas
// keyed by the durable account index evidence. Balances without a pre value
// stay unknown and are preserved; unknown is never zero.
func receiptDeltas(accountAddresses []string, pre, post []rpcTokenBalance) ([]TokenDelta, error) {
	type key struct {
		index int
		mint  string
	}
	byKey := make(map[key]*TokenDelta)
	add := func(entries []rpcTokenBalance, isPre bool) error {
		for _, entry := range entries {
			if entry.AccountIndex < 0 || entry.AccountIndex >= len(accountAddresses) {
				return fmt.Errorf("token balance account index %d exceeds message keys", entry.AccountIndex)
			}
			k := key{entry.AccountIndex, entry.Mint}
			delta := byKey[k]
			if delta == nil {
				delta = &TokenDelta{Account: accountAddresses[entry.AccountIndex], Mint: entry.Mint, Decimals: entry.UITokenAmount.Decimals}
				byKey[k] = delta
			}
			if delta.Decimals != entry.UITokenAmount.Decimals {
				return fmt.Errorf("decimals changed for account %s", delta.Account)
			}
			raw, err := strconv.ParseUint(entry.UITokenAmount.Amount, 10, 64)
			if err != nil {
				return fmt.Errorf("decode token amount %q: %w", entry.UITokenAmount.Amount, err)
			}
			if isPre && delta.PreRaw != nil || !isPre && delta.PostRaw != nil {
				return fmt.Errorf("duplicate token balance for account %s", delta.Account)
			}
			if isPre {
				preRaw := raw
				delta.PreRaw = &preRaw
				delta.PreOwner, delta.PreProgram = entry.Owner, entry.ProgramID
			} else {
				postRaw := raw
				delta.PostRaw = &postRaw
				delta.PostOwner, delta.PostProgram = entry.Owner, entry.ProgramID
			}
		}
		return nil
	}
	if err := add(pre, true); err != nil {
		return nil, err
	}
	if err := add(post, false); err != nil {
		return nil, err
	}
	out := make([]TokenDelta, 0, len(byKey))
	for _, delta := range byKey {
		out = append(out, *delta)
	}
	return out, nil
}

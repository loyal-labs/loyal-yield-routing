package fleetexec

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	sdk "github.com/gagliardetto/solana-go"
	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/chain"
)

// LookupRPC uses one endpoint with bounded calls and no hidden retries. Query
// credentials are supported but neither transport errors nor provider bodies
// are rendered; redirecting a credential-bearing POST is forbidden.
// The embedded LandRPC is the shared send path.
type LookupRPC struct {
	adapter *RPCAdapter
	*chain.Client
}

func NewLookupRPC(endpoint string, deadline time.Duration) (*LookupRPC, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Fragment != "" {
		return nil, errors.New("invalid lookup RPC endpoint")
	}
	a, err := NewRPCAdapter(endpoint, deadline)
	if err != nil {
		return nil, err
	}
	a.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	land, err := chain.New(endpoint, deadline)
	if err != nil {
		return nil, err
	}
	return &LookupRPC{adapter: a, Client: land}, nil
}
func (r *LookupRPC) call(ctx context.Context, out any, method string, params ...any) error {
	if err := r.adapter.call(ctx, out, method, params...); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("lookup RPC %s failed", method)
	}
	return nil
}
func (r *LookupRPC) SignatureStatus(ctx context.Context, sig string) (SignatureStatus, error) {
	out, err := r.adapter.SignatureStatus(ctx, sig)
	if err != nil {
		if ctx.Err() != nil {
			return SignatureStatus{}, ctx.Err()
		}
		return SignatureStatus{}, errors.New("lookup signature history unavailable")
	}
	return out, nil
}

type lookupRPCAccount struct {
	Owner      string            `json:"owner"`
	Lamports   *uint64           `json:"lamports"`
	Executable *bool             `json:"executable"`
	Data       []json.RawMessage `json:"data"`
}

func lookupAccountBytes(a *lookupRPCAccount) ([]byte, error) {
	if a == nil || a.Lamports == nil || a.Executable == nil || *a.Executable || len(a.Data) != 2 {
		return nil, errors.New("incomplete lookup account evidence")
	}
	var encoded, encoding string
	if json.Unmarshal(a.Data[0], &encoded) != nil || json.Unmarshal(a.Data[1], &encoding) != nil || encoding != "base64" {
		return nil, errors.New("lookup account encoding is not base64")
	}
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, errors.New("invalid lookup account data")
	}
	return b, nil
}
func decodeLookupSnapshot(address string, slot int64, a *lookupRPCAccount, hashes *lookupRPCAccount) (LookupSnapshot, error) {
	out := LookupSnapshot{Address: address, Slot: slot}
	if slot <= 0 {
		return out, errors.New("lookup account finalized context is missing")
	}
	hd, err := lookupAccountBytes(hashes)
	if err != nil {
		return out, err
	}
	if hashes.Owner != "Sysvar1111111111111111111111111111111111111" || len(hd) < 8 {
		return out, errors.New("lookup SlotHashes owner/data mismatch")
	}
	count := binary.LittleEndian.Uint64(hd[:8])
	if count == 0 || count > 512 || (uint64(len(hd)) != 8+40*count && len(hd) != 8+40*512) {
		return out, errors.New("lookup SlotHashes is incomplete")
	}
	for _, padding := range hd[8+40*count:] {
		if padding != 0 {
			return out, errors.New("lookup SlotHashes padding is noncanonical")
		}
	}
	for i := uint64(0); i < count; i++ {
		s := binary.LittleEndian.Uint64(hd[8+40*i:])
		if s > uint64(slot) || (i > 0 && s >= out.SlotHashes[i-1]) {
			return out, errors.New("lookup SlotHashes ordering/frontier mismatch")
		}
		out.SlotHashes = append(out.SlotHashes, s)
	}
	if a == nil {
		out.Absent = true
		return out, nil
	}
	d, err := lookupAccountBytes(a)
	if err != nil {
		return out, err
	}
	if a.Owner != lookupProgram || len(d) < 56 || (len(d)-56)%32 != 0 || (len(d)-56)/32 > 256 || binary.LittleEndian.Uint32(d[:4]) != 1 || d[21] > 1 {
		return out, errors.New("lookup table owner/state/length mismatch")
	}
	out.Owner, out.Lamports, out.Data = a.Owner, *a.Lamports, d
	out.DeactivationSlot = binary.LittleEndian.Uint64(d[4:12])
	out.LastExtendedSlot = binary.LittleEndian.Uint64(d[12:20])
	out.LastExtendedStartIndex = d[20]
	if out.LastExtendedSlot > uint64(slot) || (out.DeactivationSlot != ^uint64(0) && out.DeactivationSlot > uint64(slot)) {
		return out, errors.New("lookup table metadata is newer than finalized context")
	}
	if d[21] == 1 {
		out.Authority = sdk.PublicKeyFromBytes(d[22:54]).String()
	}
	for offset := 56; offset < len(d); offset += 32 {
		out.Addresses = append(out.Addresses, sdk.PublicKeyFromBytes(d[offset:offset+32]).String())
	}
	if int(out.LastExtendedStartIndex) > len(out.Addresses) {
		return out, errors.New("lookup extension start exceeds membership")
	}
	return out, nil
}
func (r *LookupRPC) LookupSnapshot(ctx context.Context, address string, minSlot int64) (LookupSnapshot, error) {
	if _, err := sdk.PublicKeyFromBase58(address); err != nil || minSlot < 0 {
		return LookupSnapshot{}, errors.New("invalid lookup readback request")
	}
	var result struct {
		Context struct {
			Slot int64 `json:"slot"`
		} `json:"context"`
		Value []*lookupRPCAccount `json:"value"`
	}
	config := map[string]any{"commitment": "finalized", "encoding": "base64"}
	if minSlot > 0 {
		config["minContextSlot"] = minSlot
	}
	if err := r.call(ctx, &result, "getMultipleAccounts", []string{address, sdk.SysVarSlotHashesPubkey.String()}, config); err != nil {
		return LookupSnapshot{}, err
	}
	if len(result.Value) != 2 || result.Context.Slot < minSlot {
		return LookupSnapshot{}, errors.New("lookup readback is missing or below receipt frontier")
	}
	return decodeLookupSnapshot(address, result.Context.Slot, result.Value[0], result.Value[1])
}
func (r *LookupRPC) LookupFinalizedReceipt(ctx context.Context, sig string) (*LookupReceipt, error) {
	var result *struct {
		Slot        int64             `json:"slot"`
		Transaction []json.RawMessage `json:"transaction"`
		Meta        *struct {
			Err  json.RawMessage `json:"err"`
			Fee  *uint64         `json:"fee"`
			Pre  []uint64        `json:"preBalances"`
			Post []uint64        `json:"postBalances"`
		} `json:"meta"`
	}
	if err := r.call(ctx, &result, "getTransaction", sig, map[string]any{"commitment": "finalized", "encoding": "base64", "maxSupportedTransactionVersion": 0}); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, nil
	}
	if result.Slot <= 0 || result.Meta == nil || result.Meta.Fee == nil || len(result.Meta.Err) == 0 || len(result.Transaction) != 2 {
		return nil, errors.New("lookup receipt has incomplete finalized metadata")
	}
	var data, encoding string
	if json.Unmarshal(result.Transaction[0], &data) != nil || json.Unmarshal(result.Transaction[1], &encoding) != nil || encoding != "base64" {
		return nil, errors.New("lookup receipt lacks exact packet")
	}
	wire, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, errors.New("invalid lookup receipt packet")
	}
	tx, err := sdk.TransactionFromBytes(wire)
	if err != nil || tx.Message.IsVersioned() || len(tx.Signatures) != 1 || tx.Signatures[0].String() != sig {
		return nil, errors.New("lookup receipt packet signature/version mismatch")
	}
	out := &LookupReceipt{Signature: sig, Slot: result.Slot, Wire: wire, FeeLamports: *result.Meta.Fee, PreLamports: result.Meta.Pre, PostLamports: result.Meta.Post}
	for _, key := range tx.Message.AccountKeys {
		out.Addresses = append(out.Addresses, key.String())
	}
	if len(out.PreLamports) != len(out.Addresses) || len(out.PostLamports) != len(out.Addresses) {
		return nil, errors.New("lookup receipt balance indices incomplete")
	}
	if string(result.Meta.Err) != "null" {
		out.Err = string(result.Meta.Err)
	}
	return out, nil
}
func (r *LookupRPC) LookupBlockhash(ctx context.Context) (string, int64, int64, error) {
	var out struct {
		Context struct {
			Slot int64 `json:"slot"`
		} `json:"context"`
		Value struct {
			Blockhash string `json:"blockhash"`
			Height    int64  `json:"lastValidBlockHeight"`
		} `json:"value"`
	}
	if err := r.call(ctx, &out, "getLatestBlockhash", map[string]any{"commitment": "finalized"}); err != nil {
		return "", 0, 0, err
	}
	if _, err := sdk.HashFromBase58(out.Value.Blockhash); err != nil || out.Value.Height <= 0 || out.Context.Slot <= 0 {
		return "", 0, 0, errors.New("lookup blockhash/expiry missing")
	}
	return out.Value.Blockhash, out.Value.Height, out.Context.Slot, nil
}
func (r *LookupRPC) LookupFee(ctx context.Context, message []byte) (uint64, error) {
	var out struct {
		Value *uint64 `json:"value"`
	}
	if err := r.call(ctx, &out, "getFeeForMessage", base64.StdEncoding.EncodeToString(message), map[string]any{"commitment": "finalized"}); err != nil {
		return 0, err
	}
	if out.Value == nil {
		return 0, errors.New("lookup fee estimate unavailable")
	}
	return *out.Value, nil
}
func (r *LookupRPC) LookupRent(ctx context.Context, length int) (uint64, error) {
	if length < 56 || length > 56+32*256 {
		return 0, errors.New("invalid lookup rent size")
	}
	var out uint64
	err := r.call(ctx, &out, "getMinimumBalanceForRentExemption", length, map[string]any{"commitment": "finalized"})
	return out, err
}
func (r *LookupRPC) LookupBalance(ctx context.Context, key string) (uint64, error) {
	var out struct {
		Value *uint64 `json:"value"`
	}
	if err := r.call(ctx, &out, "getBalance", key, map[string]any{"commitment": "finalized"}); err != nil {
		return 0, err
	}
	if out.Value == nil {
		return 0, errors.New("lookup payer balance unavailable")
	}
	return *out.Value, nil
}
func (r *LookupRPC) SimulateLookup(ctx context.Context, wire []byte) error {
	var out struct {
		Value *struct {
			Err json.RawMessage `json:"err"`
		} `json:"value"`
	}
	if err := r.call(ctx, &out, "simulateTransaction", base64.StdEncoding.EncodeToString(wire), map[string]any{"commitment": "finalized", "encoding": "base64", "sigVerify": false, "replaceRecentBlockhash": true}); err != nil {
		return err
	}
	if out.Value == nil || string(out.Value.Err) != "null" {
		return errors.New("lookup unsigned simulation failed or incomplete")
	}
	return nil
}

var _ LookupChain = (*LookupRPC)(nil)

package autodeposit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/gagliardetto/solana-go"
)

// ArtifactRPC exposes only bounded confirmed history/receipt reads. It never
// submits packets, and error messages never include provider URLs or bodies.
type ArtifactRPC struct {
	endpoint string
	client   *http.Client
}

func NewArtifactRPC(endpoint string) (*ArtifactRPC, error) {
	u, e := url.Parse(endpoint)
	if e != nil || u.User != nil || u.Fragment != "" || u.Host == "" {
		return nil, errors.New("artifact RPC endpoint invalid")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if u.Scheme != "https" && !(u.Scheme == "http" && (host == "localhost" || ip != nil && ip.IsLoopback())) {
		return nil, errors.New("artifact RPC requires HTTPS or loopback")
	}
	// Existing RPC providers authenticate HTTPS requests through query values.
	// Keep the endpoint private; transport/envelope errors never render it.
	return &ArtifactRPC{endpoint: endpoint, client: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return errors.New("artifact RPC redirects forbidden") }}}, nil
}
func (r *ArtifactRPC) read(ctx context.Context, method string, params any, out any) error {
	body, e := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params"`
	}{"2.0", 1, method, params})
	if e != nil {
		return e
	}
	request, e := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if e != nil {
		return errors.New("artifact RPC request invalid")
	}
	request.Header.Set("Content-Type", "application/json")
	response, e := r.client.Do(request)
	if e != nil {
		return errors.New("artifact RPC transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("artifact RPC HTTP response failed")
	}
	data, e := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if e != nil || len(data) > 2*1024*1024 {
		return errors.New("artifact RPC response exceeds bound")
	}
	var envelope struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Error   json.RawMessage `json:"error"`
		Result  json.RawMessage `json:"result"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.JSONRPC != "2.0" || envelope.ID != 1 || len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return errors.New("artifact RPC envelope invalid")
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return ErrArtifactCreationProofPending
	}
	if json.Unmarshal(envelope.Result, out) != nil {
		return errors.New("artifact RPC result invalid")
	}
	return nil
}
func (r *ArtifactRPC) ArtifactHistory(ctx context.Context, address string, limit int) ([]ArtifactHistoryEntry, error) {
	return r.ArtifactHistoryPage(ctx, address, limit, "")
}

func (r *ArtifactRPC) ArtifactHistoryPage(ctx context.Context, address string, limit int, before string) ([]ArtifactHistoryEntry, error) {
	if _, e := solana.PublicKeyFromBase58(address); e != nil {
		return nil, e
	}
	if limit < 1 || limit > 32 {
		return nil, errors.New("artifact history limit invalid")
	}
	options := map[string]any{"limit": limit, "commitment": "confirmed"}
	if before != "" {
		if _, err := solana.SignatureFromBase58(before); err != nil {
			return nil, errors.New("artifact history cursor invalid")
		}
		options["before"] = before
	}
	var rows []struct {
		Signature string          `json:"signature"`
		Slot      int64           `json:"slot"`
		Err       json.RawMessage `json:"err"`
	}
	if e := r.read(ctx, "getSignaturesForAddress", []any{address, options}, &rows); e != nil {
		return nil, e
	}
	if len(rows) > limit {
		return nil, errors.New("artifact history exceeds requested bound")
	}
	result := make([]ArtifactHistoryEntry, 0, len(rows))
	for _, row := range rows {
		if _, e := solana.SignatureFromBase58(row.Signature); e != nil || row.Slot <= 0 {
			return nil, errors.New("artifact history identity invalid")
		}
		result = append(result, ArtifactHistoryEntry{Signature: row.Signature, Slot: row.Slot, Failed: len(row.Err) > 0 && string(row.Err) != "null"})
	}
	return result, nil
}
func (r *ArtifactRPC) ArtifactReceipt(ctx context.Context, signature string) (ArtifactReceipt, error) {
	var receipt ArtifactReceipt
	if _, e := solana.SignatureFromBase58(signature); e != nil {
		return receipt, e
	}
	var result struct {
		Slot        int64             `json:"slot"`
		Transaction []json.RawMessage `json:"transaction"`
		Meta        *struct {
			Err    json.RawMessage                       `json:"err"`
			Pre    []uint64                              `json:"preBalances"`
			Post   []uint64                              `json:"postBalances"`
			Loaded struct{ Writable, Readonly []string } `json:"loadedAddresses"`
			Inner  []struct {
				Index        int                          `json:"index"`
				Instructions []solana.CompiledInstruction `json:"instructions"`
			} `json:"innerInstructions"`
		} `json:"meta"`
	}
	if e := r.read(ctx, "getTransaction", []any{signature, map[string]any{"commitment": "confirmed", "encoding": "base64", "maxSupportedTransactionVersion": 0}}, &result); e != nil {
		return receipt, e
	}
	if result.Slot <= 0 || result.Meta == nil || len(result.Meta.Err) == 0 || string(result.Meta.Err) != "null" || len(result.Transaction) != 2 {
		return receipt, ErrArtifactCreationProofPending
	}
	var encoded, encoding string
	if json.Unmarshal(result.Transaction[0], &encoded) != nil || json.Unmarshal(result.Transaction[1], &encoding) != nil || encoding != "base64" || len(encoded) > 1644 {
		return receipt, errors.New("artifact receipt encoding invalid")
	}
	wire, e := base64.StdEncoding.DecodeString(encoded)
	if e != nil || len(wire) == 0 || len(wire) > solanaPacketBytes {
		return receipt, errors.New("artifact receipt packet invalid")
	}
	receipt = ArtifactReceipt{Signature: signature, Slot: result.Slot, Wire: wire, PreLamports: result.Meta.Pre, PostLamports: result.Meta.Post, LoadedWritable: result.Meta.Loaded.Writable, LoadedReadonly: result.Meta.Loaded.Readonly}
	tx, e := solana.TransactionFromBytes(wire)
	if e != nil {
		return receipt, errors.New("artifact receipt transaction invalid")
	}
	for _, group := range result.Meta.Inner {
		if group.Index < 0 || group.Index >= len(tx.Message.Instructions) {
			return receipt, errors.New("artifact receipt inner index invalid")
		}
		receipt.InnerInstructions = append(receipt.InnerInstructions, group.Instructions...)
	}
	return receipt, nil
}

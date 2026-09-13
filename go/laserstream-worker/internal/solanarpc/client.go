package solanarpc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

type Client struct {
	url       string
	http      *http.Client
	requestID atomic.Uint64
}

func New(url string, timeout time.Duration) *Client {
	return &Client{url: url, http: &http.Client{Timeout: timeout}}
}

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    any    `json:"data,omitempty"`
	} `json:"error,omitempty"`
}

func (c *Client) call(ctx context.Context, method string, params any, target any) error {
	id := c.requestID.Add(1)
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return fmt.Errorf("encode %s request: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call Solana %s: %w", method, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("read Solana %s response: %w", method, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("solana %s returned HTTP %d", method, resp.StatusCode)
	}
	var envelope rpcEnvelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("decode Solana %s response: %w", method, err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("solana %s RPC %d: %s", method, envelope.Error.Code, envelope.Error.Message)
	}
	if len(envelope.Result) == 0 || bytes.Equal(envelope.Result, []byte("null")) {
		return errors.New("solana " + method + " returned null")
	}
	if err := json.Unmarshal(envelope.Result, target); err != nil {
		return fmt.Errorf("decode Solana %s result: %w", method, err)
	}
	return nil
}

func (c *Client) Slot(ctx context.Context, commitment string) (uint64, error) {
	var slot uint64
	err := c.call(ctx, "getSlot", []any{map[string]any{"commitment": commitment}}, &slot)
	return slot, err
}

type Account struct {
	Lamports   uint64
	Owner      string
	Data       []byte
	Executable bool
	RentEpoch  uint64
}

type AccountsResponse struct {
	Slot     uint64
	Accounts []*Account
}

func (c *Client) MultipleAccounts(ctx context.Context, addresses []string, commitment string, minimumSlot *uint64) (AccountsResponse, error) {
	config := map[string]any{"encoding": "base64", "commitment": commitment}
	if minimumSlot != nil {
		config["minContextSlot"] = *minimumSlot
	}
	var result struct {
		Context struct {
			Slot uint64 `json:"slot"`
		} `json:"context"`
		Value []json.RawMessage `json:"value"`
	}
	if err := c.call(ctx, "getMultipleAccounts", []any{addresses, config}, &result); err != nil {
		return AccountsResponse{}, err
	}
	if len(result.Value) != len(addresses) {
		return AccountsResponse{}, fmt.Errorf("getMultipleAccounts returned %d accounts for %d addresses", len(result.Value), len(addresses))
	}
	response := AccountsResponse{Slot: result.Context.Slot, Accounts: make([]*Account, len(result.Value))}
	for index, raw := range result.Value {
		if bytes.Equal(raw, []byte("null")) {
			continue
		}
		var value struct {
			Lamports   uint64            `json:"lamports"`
			Owner      string            `json:"owner"`
			Data       []json.RawMessage `json:"data"`
			Executable bool              `json:"executable"`
			RentEpoch  uint64            `json:"rentEpoch"`
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return AccountsResponse{}, fmt.Errorf("decode account %s: %w", addresses[index], err)
		}
		if len(value.Data) < 1 {
			return AccountsResponse{}, fmt.Errorf("account %s omitted base64 data", addresses[index])
		}
		var encoded string
		if err := json.Unmarshal(value.Data[0], &encoded); err != nil {
			return AccountsResponse{}, fmt.Errorf("decode account %s base64 field: %w", addresses[index], err)
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return AccountsResponse{}, fmt.Errorf("decode account %s data: %w", addresses[index], err)
		}
		response.Accounts[index] = &Account{Lamports: value.Lamports, Owner: value.Owner, Data: data, Executable: value.Executable, RentEpoch: value.RentEpoch}
	}
	return response, nil
}

type SignatureStatus struct {
	Signature string          `json:"signature"`
	Slot      uint64          `json:"slot"`
	Err       json.RawMessage `json:"err"`
}

func (c *Client) SignaturesForAddress(ctx context.Context, address, commitment, before string, limit int) ([]SignatureStatus, error) {
	config := map[string]any{"commitment": commitment, "limit": limit}
	if before != "" {
		config["before"] = before
	}
	var result []SignatureStatus
	if err := c.call(ctx, "getSignaturesForAddress", []any{address, config}, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *Client) Transaction(ctx context.Context, signature string, commitment string) (json.RawMessage, error) {
	var result json.RawMessage
	err := c.call(ctx, "getTransaction", []any{signature, map[string]any{"commitment": commitment, "encoding": "json", "maxSupportedTransactionVersion": 0}}, &result)
	return result, err
}

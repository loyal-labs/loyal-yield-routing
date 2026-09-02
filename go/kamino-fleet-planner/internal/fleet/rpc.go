package fleet

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type Account struct {
	Address    string
	Owner      string
	Lamports   uint64
	Executable bool
	Data       []byte
}

type RPCClient struct {
	url    string
	client *http.Client
}

func NewRPCClient(rpcURL string) *RPCClient {
	return &RPCClient{url: rpcURL, client: &http.Client{Timeout: 15 * time.Second}}
}

func (c *RPCClient) call(ctx context.Context, method string, params []any, output any) error {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		request.Header.Set("content-type", "application/json")
		response, err := c.client.Do(request)
		if err == nil {
			var envelope struct {
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			err = json.NewDecoder(response.Body).Decode(&envelope)
			response.Body.Close()
			if response.StatusCode != http.StatusOK {
				err = fmt.Errorf("RPC %s returned HTTP %d", method, response.StatusCode)
			}
			if err == nil && len(envelope.Error) > 0 && string(envelope.Error) != "null" {
				err = fmt.Errorf("RPC %s failed", method)
			}
			if err == nil {
				err = json.Unmarshal(envelope.Result, output)
			}
		}
		if err == nil {
			return nil
		}
		last = err
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
			}
		}
	}
	return last
}

func (c *RPCClient) ConfirmedSlot(ctx context.Context) (int64, error) {
	var slot int64
	if err := c.call(ctx, "getSlot", []any{map[string]string{"commitment": "confirmed"}}, &slot); err != nil {
		return 0, err
	}
	if slot <= 0 {
		return 0, fmt.Errorf("confirmed slot is unavailable")
	}
	return slot, nil
}

func (c *RPCClient) ConfirmedAccounts(ctx context.Context, addresses []string, minimumSlot int64) (int64, []Account, error) {
	if len(addresses) == 0 || minimumSlot <= 0 {
		return 0, nil, fmt.Errorf("addresses and minimum slot are required")
	}
	var result struct {
		Context struct {
			Slot int64 `json:"slot"`
		} `json:"context"`
		Value []*struct {
			Owner      string          `json:"owner"`
			Lamports   uint64          `json:"lamports"`
			Executable bool            `json:"executable"`
			Data       json.RawMessage `json:"data"`
		} `json:"value"`
	}
	if err := c.call(ctx, "getMultipleAccounts", []any{addresses, map[string]any{"commitment": "confirmed", "encoding": "base64", "minContextSlot": minimumSlot}}, &result); err != nil {
		return 0, nil, err
	}
	if result.Context.Slot < minimumSlot || len(result.Value) != len(addresses) {
		return 0, nil, fmt.Errorf("incoherent confirmed account response")
	}
	accounts := make([]Account, len(addresses))
	for index, value := range result.Value {
		if value == nil || value.Owner == "" || value.Lamports == 0 {
			return 0, nil, fmt.Errorf("required account %s is absent", addresses[index])
		}
		var encoded []string
		if err := json.Unmarshal(value.Data, &encoded); err != nil || len(encoded) != 2 || encoded[1] != "base64" {
			return 0, nil, fmt.Errorf("account %s has invalid encoding", addresses[index])
		}
		data, err := base64.StdEncoding.DecodeString(encoded[0])
		if err != nil {
			return 0, nil, fmt.Errorf("decode account %s: %w", addresses[index], err)
		}
		accounts[index] = Account{Address: addresses[index], Owner: value.Owner, Lamports: value.Lamports, Executable: value.Executable, Data: data}
	}
	return result.Context.Slot, accounts, nil
}

package backyardrwa

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type RPCClient struct {
	url    string
	client *http.Client
}

type ConfirmedAccount struct {
	Address    string
	Owner      string
	Lamports   uint64
	Data       []byte
	Executable bool
}

type LatestBlockhash struct {
	Blockhash            string
	LastValidBlockHeight int64
}

func NewRPCClient(rpcURL string) (*RPCClient, error) {
	config := RuntimeConfig{DatabaseURL: "validation-only", RPCURL: rpcURL, RouteKey: "validation-only"}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &RPCClient{url: rpcURL, client: &http.Client{Timeout: 15 * time.Second}}, nil
}

func (c *RPCClient) call(ctx context.Context, method string, params []any, output any) error {
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("content-type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("RPC %s returned HTTP %d", method, response.StatusCode)
	}
	envelope := struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}{}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return err
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return fmt.Errorf("RPC %s failed", method)
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return fmt.Errorf("RPC %s returned no result", method)
	}
	return json.Unmarshal(envelope.Result, output)
}

func (c *RPCClient) ConfirmedSlot(ctx context.Context) (int64, error) {
	var slot int64
	if err := c.call(ctx, "getSlot", []any{map[string]string{"commitment": "confirmed"}}, &slot); err != nil {
		return 0, err
	}
	if slot <= 0 {
		return 0, fmt.Errorf("confirmed slot unavailable")
	}
	return slot, nil
}

// GetMultipleAccounts reads one coherent confirmed account set. The returned
// context slot is the only slot callers may use for the resulting Snapshot.
func (c *RPCClient) GetMultipleAccounts(
	ctx context.Context,
	addresses []string,
	minContextSlot int64,
) (int64, []ConfirmedAccount, error) {
	if len(addresses) == 0 || minContextSlot <= 0 {
		return 0, nil, fmt.Errorf("account addresses and minContextSlot are required")
	}
	var result struct {
		Context struct {
			Slot int64 `json:"slot"`
		} `json:"context"`
		Value []*struct {
			Owner      string          `json:"owner"`
			Lamports   uint64          `json:"lamports"`
			Data       json.RawMessage `json:"data"`
			Executable bool            `json:"executable"`
		} `json:"value"`
	}
	err := c.call(ctx, "getMultipleAccounts", []any{addresses, map[string]any{
		"commitment": "confirmed", "encoding": "base64", "minContextSlot": minContextSlot,
	}}, &result)
	if err != nil {
		return 0, nil, err
	}
	if result.Context.Slot < minContextSlot || len(result.Value) != len(addresses) {
		return 0, nil, fmt.Errorf("incoherent confirmed account response")
	}
	accounts := make([]ConfirmedAccount, len(addresses))
	for index, value := range result.Value {
		if value == nil || value.Owner == "" {
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
		accounts[index] = ConfirmedAccount{
			Address: addresses[index], Owner: value.Owner, Lamports: value.Lamports,
			Data: data, Executable: value.Executable,
		}
	}
	return result.Context.Slot, accounts, nil
}

func (c *RPCClient) LatestBlockhash(ctx context.Context) (LatestBlockhash, error) {
	var result struct {
		Context struct {
			Slot int64 `json:"slot"`
		} `json:"context"`
		Value struct {
			Blockhash            string `json:"blockhash"`
			LastValidBlockHeight int64  `json:"lastValidBlockHeight"`
		} `json:"value"`
	}
	if err := c.call(ctx, "getLatestBlockhash", []any{map[string]string{"commitment": "confirmed"}}, &result); err != nil {
		return LatestBlockhash{}, err
	}
	if result.Context.Slot <= 0 || result.Value.Blockhash == "" || result.Value.LastValidBlockHeight <= 0 {
		return LatestBlockhash{}, fmt.Errorf("invalid confirmed blockhash response")
	}
	return LatestBlockhash{Blockhash: result.Value.Blockhash, LastValidBlockHeight: result.Value.LastValidBlockHeight}, nil
}

// SimulateSignedTransaction verifies the exact signed bytes that would later
// be persisted. It never replaces the blockhash and requires signature checks.
func (c *RPCClient) SimulateSignedTransaction(ctx context.Context, signedWire []byte) (SimulationResult, error) {
	if len(signedWire) == 0 {
		return SimulationResult{}, fmt.Errorf("signed transaction wire is required")
	}
	var result struct {
		Context struct {
			Slot int64 `json:"slot"`
		} `json:"context"`
		Value struct {
			Err           json.RawMessage `json:"err"`
			Logs          []string        `json:"logs"`
			UnitsConsumed uint64          `json:"unitsConsumed"`
		} `json:"value"`
	}
	encoded := base64.StdEncoding.EncodeToString(signedWire)
	err := c.call(ctx, "simulateTransaction", []any{encoded, map[string]any{
		"commitment": "confirmed", "encoding": "base64", "sigVerify": true,
		"replaceRecentBlockhash": false,
	}}, &result)
	if err != nil {
		return SimulationResult{}, err
	}
	if result.Context.Slot <= 0 || (len(result.Value.Err) > 0 && string(result.Value.Err) != "null") {
		return SimulationResult{}, fmt.Errorf("signed transaction simulation failed")
	}
	return SimulationResult{Slot: result.Context.Slot, UnitsConsumed: result.Value.UnitsConsumed, Logs: append([]string(nil), result.Value.Logs...)}, nil
}

// SendSignedTransactionOnce submits exactly the persisted wire with RPC retries
// disabled. Callers must durably record broadcast_intent before invoking it.
func (c *RPCClient) SendSignedTransactionOnce(ctx context.Context, signedWire []byte, expectedSignature string) (string, error) {
	if len(signedWire) == 0 || expectedSignature == "" {
		return "", fmt.Errorf("persisted signed wire and signature are required")
	}
	var signature string
	encoded := base64.StdEncoding.EncodeToString(signedWire)
	err := c.call(ctx, "sendTransaction", []any{encoded, map[string]any{
		"encoding": "base64", "preflightCommitment": "confirmed", "skipPreflight": false, "maxRetries": 0,
	}}, &signature)
	if err != nil {
		return "", err
	}
	if signature != expectedSignature {
		return "", fmt.Errorf("RPC returned a different transaction signature")
	}
	return signature, nil
}

func (c *RPCClient) SignatureStatus(ctx context.Context, signature string) (SignatureObservation, error) {
	if signature == "" {
		return SignatureObservation{}, fmt.Errorf("transaction signature is required")
	}
	var result struct {
		Value []*struct {
			Slot               int64           `json:"slot"`
			Err                json.RawMessage `json:"err"`
			ConfirmationStatus string          `json:"confirmationStatus"`
		} `json:"value"`
	}
	if err := c.call(ctx, "getSignatureStatuses", []any{[]string{signature}, map[string]bool{"searchTransactionHistory": true}}, &result); err != nil {
		return SignatureObservation{}, err
	}
	if len(result.Value) != 1 || result.Value[0] == nil {
		return SignatureObservation{Found: false}, nil
	}
	status := result.Value[0]
	failed := len(status.Err) > 0 && string(status.Err) != "null"
	confirmed := !failed && status.Slot > 0 && (status.ConfirmationStatus == "confirmed" || status.ConfirmationStatus == "finalized")
	return SignatureObservation{Found: true, Confirmed: confirmed, ConfirmationSlot: status.Slot, Failed: failed}, nil
}

func (c *RPCClient) ConfirmedBlockHeight(ctx context.Context) (int64, error) {
	var height int64
	if err := c.call(ctx, "getBlockHeight", []any{map[string]string{"commitment": "confirmed"}}, &height); err != nil {
		return 0, err
	}
	if height <= 0 {
		return 0, fmt.Errorf("confirmed block height unavailable")
	}
	return height, nil
}

func ConfirmedSlot(ctx context.Context, rpcURL string) (int64, error) {
	client, err := NewRPCClient(rpcURL)
	if err != nil {
		return 0, err
	}
	return client.ConfirmedSlot(ctx)
}

package solana

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
	"time"
)

// LandRPC is the JSON-RPC client every family lands through.
type LandRPC struct {
	url    string
	client *http.Client
}

func NewLandRPC(url string, timeout time.Duration) (*LandRPC, error) {
	if url == "" || timeout <= 0 {
		return nil, errors.New("land RPC requires url and timeout")
	}
	// The URL can carry a provider key; never forward it to a redirect target.
	client := &http.Client{Timeout: timeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &LandRPC{url: url, client: client}, nil
}

func (r *LandRPC) call(ctx context.Context, out any, method string, params ...any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		// The URL can carry a provider key; keep only the cause.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return fmt.Errorf("%s: %w", method, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: rpc status %d", method, response.StatusCode)
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("%s: decode: %w", method, err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("%s: rpc error %d: %s", method, envelope.Error.Code, envelope.Error.Message)
	}
	return json.Unmarshal(envelope.Result, out)
}

func (r *LandRPC) SendWire(ctx context.Context, wire []byte, skipPreflight bool) error {
	var signature string
	return r.call(ctx, &signature, "sendTransaction", base64.StdEncoding.EncodeToString(wire), map[string]any{
		"encoding": "base64", "skipPreflight": skipPreflight, "preflightCommitment": "confirmed", "maxRetries": 0,
	})
}

func (r *LandRPC) FinalizedBlockHeight(ctx context.Context) (uint64, uint64, error) {
	var info struct {
		AbsoluteSlot uint64 `json:"absoluteSlot"`
		BlockHeight  uint64 `json:"blockHeight"`
	}
	if err := r.call(ctx, &info, "getEpochInfo", map[string]any{"commitment": "finalized"}); err != nil {
		return 0, 0, err
	}
	if info.BlockHeight == 0 || info.AbsoluteSlot == 0 {
		return 0, 0, errors.New("getEpochInfo: zero height or slot")
	}
	return info.BlockHeight, info.AbsoluteSlot, nil
}

func (r *LandRPC) SignatureState(ctx context.Context, signature string) (SignatureState, error) {
	var envelope struct {
		Context struct {
			Slot uint64 `json:"slot"`
		} `json:"context"`
		Value []*struct {
			Slot               uint64          `json:"slot"`
			ConfirmationStatus string          `json:"confirmationStatus"`
			Err                json.RawMessage `json:"err"`
		} `json:"value"`
	}
	if err := r.call(ctx, &envelope, "getSignatureStatuses", []string{signature}, map[string]any{"searchTransactionHistory": true}); err != nil {
		return SignatureState{}, err
	}
	if len(envelope.Value) != 1 || envelope.Context.Slot == 0 {
		return SignatureState{}, errors.New("getSignatureStatuses: invalid shape or context")
	}
	state := SignatureState{ContextSlot: envelope.Context.Slot}
	status := envelope.Value[0]
	if status == nil {
		return state, nil
	}
	state.Found, state.Slot = true, status.Slot
	switch status.ConfirmationStatus {
	case "processed":
		state.Commitment = Processed
	case "confirmed":
		state.Commitment = Confirmed
	case "finalized":
		state.Commitment = Finalized
	default:
		return SignatureState{}, errors.New("getSignatureStatuses: unknown commitment")
	}
	if len(status.Err) > 0 && string(status.Err) != "null" {
		state.Err = string(status.Err)
	}
	return state, nil
}

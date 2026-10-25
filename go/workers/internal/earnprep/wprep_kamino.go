package earnprep

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/solana-foundation/solana-go/v2"

	"github.com/loyal-labs/loyal-yield-routing/go/workers/internal/programs/kamino"
)

const KaminoWithdrawInstructionsURL = "https://api.kamino.finance/ktx/klend/withdraw-instructions"

type KaminoWithdrawBundle struct {
	KaminoInstructionBundle
	LookupTables []solana.PublicKey
}

// KaminoWithdrawInstructions ports fetchKaminoWithdrawInstruction (CL:2742-2791).
func KaminoWithdrawInstructions(ctx context.Context, client *http.Client, endpoint string, vault, market, reserve solana.PublicKey, liquidityAmountRaw uint64) (KaminoWithdrawBundle, error) {
	body, err := json.Marshal(kaminoDepositRequestBody{
		Vault:   vault.String(),
		Market:  market.String(),
		Reserve: reserve.String(),
		Amount:  FormatRawTokenAmountForAPI(liquidityAmountRaw, KaminoDepositUSDCDecimals),
	})
	if err != nil {
		return KaminoWithdrawBundle{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return KaminoWithdrawBundle{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return KaminoWithdrawBundle{}, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return KaminoWithdrawBundle{}, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return KaminoWithdrawBundle{}, &KaminoUpstreamError{
			Status:  response.StatusCode,
			Message: fmt.Sprintf("Kamino withdraw instruction request failed with status %d: %s", response.StatusCode, payload),
		}
	}
	bundle, err := DecodeKaminoDepositBundle(payload, kamino.ProgramID, kamino.WithdrawV2Discriminator)
	if err != nil {
		return KaminoWithdrawBundle{}, err
	}
	return KaminoWithdrawBundle{KaminoInstructionBundle: bundle, LookupTables: kaminoLookupTableKeys(payload)}, nil
}

func kaminoLookupTableKeys(payload []byte) []solana.PublicKey {
	var wire struct {
		LutsByAddress json.RawMessage `json:"lutsByAddress"`
	}
	if json.Unmarshal(payload, &wire) != nil || len(wire.LutsByAddress) == 0 {
		return nil
	}
	var candidates []json.RawMessage
	if json.Unmarshal(wire.LutsByAddress, &candidates) != nil {
		candidates = objectKeys(wire.LutsByAddress)
	}
	var keys []solana.PublicKey
	seen := map[solana.PublicKey]bool{}
	for _, raw := range candidates {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			continue
		}
		key, err := solana.PublicKeyFromBase58(text)
		if err != nil || seen[key] {
			continue
		}
		seen[key] = true
		keys = append(keys, key)
	}
	return keys
}

func objectKeys(raw json.RawMessage) []json.RawMessage {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil
	}
	var keys []json.RawMessage
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return keys
		}
		quoted, _ := json.Marshal(token)
		keys = append(keys, quoted)
		var skip json.RawMessage
		if decoder.Decode(&skip) != nil {
			return keys
		}
	}
	return keys
}
